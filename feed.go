package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The consoles' feed (#27, #1 "What remains", item 2). Each repository's
// console receives, through the escalation command, a plain line for each
// thing the page shows of that repository: a decision waiting on the
// owner, a settled CI result, a delivery, a merge, a new item. The lines
// are listed from the folded state, each under a key that stays the same
// however often it is listed, and each is posted once: feed events record
// it, so a restart does not post it again. Escalations already go to the
// same console with the same text, and are not fed again. The owner's
// reply in a console comes back through agentd console reply as an owner
// record, as the page's POST does, and wakes the loop.

// FeedPolicy turns the feed on. It is off by default.
type FeedPolicy struct {
	Enabled bool `json:"enabled"`
}

// Feed phases.
const (
	FeedBaseline = "baseline" // the entries there when the feed began: never posted
	FeedIntent   = "intent"   // a post is about to be made
	FeedPosted   = "posted"
	FeedFailed   = "failed"
	FeedGaveUp   = "gave-up"
	FeedWould    = "would" // shadow mode: the post it would make
)

// FeedPayload is one step of a feed post: its entry's key, the repository
// and console, and the text posted, or the confidential pattern it matched
// in its place. A baseline names every key there when the feed began.
type FeedPayload struct {
	Phase      string   `json:"phase"`
	Key        string   `json:"key,omitempty"`
	Keys       []string `json:"keys,omitempty"`
	Repository string   `json:"repository,omitempty"`
	Console    string   `json:"console,omitempty"`
	Text       string   `json:"text,omitempty"`
	Withheld   int      `json:"withheld,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// FeedState is what the feed events fold to: whether the baseline was
// taken, the keys posted or settled, and each key's failed tries.
type FeedState struct {
	Started  bool
	Fed      map[string]bool
	Failures map[string][]time.Time
}

// applyFeed folds a feed event. An intent counts as posted, so a post a
// crash of agentd cut short is never repeated; its failure undoes it.
func (st *LogState) applyFeed(e Event) {
	f := &st.Feed
	if f.Fed == nil {
		f.Fed, f.Failures = map[string]bool{}, map[string][]time.Time{}
	}
	p := e.Feed
	switch p.Phase {
	case FeedBaseline:
		f.Started = true
		for _, k := range p.Keys {
			f.Fed[k] = true
		}
	case FeedIntent, FeedPosted, FeedWould, FeedGaveUp:
		f.Fed[p.Key] = true
	case FeedFailed:
		delete(f.Fed, p.Key)
		f.Failures[p.Key] = append(f.Failures[p.Key], e.Time)
	}
}

// feedEntry is one line for a repository's console.
type feedEntry struct {
	Key, Repository, Text string
}

// feedEntries lists every line the state holds for the configured
// repositories, in a stable order.
func (s *Supervisor) feedEntries() []feedEntry {
	var out []feedEntry
	add := func(repo, key, text string) {
		if _, ok := s.cfg.Repositories[repo]; ok {
			out = append(out, feedEntry{Key: key, Repository: repo, Text: text})
		}
	}
	for _, name := range s.cfg.SessionNames() {
		t := s.st.Tracks[name]
		if t == nil {
			continue
		}
		sess := s.cfg.Sessions[name]
		for _, d := range t.open() {
			// A blocking decision is in the session's escalation, which
			// went to the same console: it is not fed again.
			if d.Blocking || !t.waits(d) {
				continue
			}
			text := fmt.Sprintf("%s asks %s (%s): %s", name, d.ID, d.Type, d.Question)
			if d.Recommendation != "" {
				text += " Recommended: " + d.Recommendation + "."
			}
			if keys := optionKeys(d); keys != "" {
				text += " Options: " + keys + "."
			}
			text += fmt.Sprintf(" Reply: %s %s <answer>", name, d.ID)
			add(sess.Repository, "decision:"+name+":"+d.ID+":"+decisionHash(d), text)
		}
		for _, d := range t.Deliveries {
			repo := s.deliveryRepository(name, d)
			if d.Ref == "" || d.Ref == "would-open" {
				continue
			}
			add(repo, "delivery:"+d.Delivered+":"+d.Ref, fmt.Sprintf("%s delivered %s to %s.", name, d.Ref, repo))
		}
		keys := make([]string, 0, len(t.Merges))
		for k := range t.Merges {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			repo, ref, _ := strings.Cut(k, " ")
			add(repo, "merge:"+k+":"+t.Merges[k], fmt.Sprintf("agentd merged %s in %s at %s, %s's delivery.", ref, repo, shortHash(t.Merges[k]), name))
		}
	}
	keys := make([]string, 0, len(s.st.CI))
	for k := range s.st.CI {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r := s.st.CI[k]
		switch r.State {
		case CISuccess, CIFailed, CIMerged, CIClosed:
		default:
			continue // not settled
		}
		text := fmt.Sprintf("%s in %s: CI %s on %s.", r.Ref, r.Repository, r.State, shortHash(r.Head))
		if r.State == CIFailed {
			text = fmt.Sprintf("%s in %s: CI failed on %s (%s; failing: %s).", r.Ref, r.Repository, shortHash(r.Head), r.Verdict, strings.Join(r.Jobs, ", "))
		}
		add(r.Repository, "ci:"+k+":"+r.Head+":"+r.State, text)
	}
	for _, it := range s.st.SortedItems(true) {
		repo := s.cfg.ItemRepository(it.Repository)
		text := fmt.Sprintf("New %s in %s: %s", strings.ReplaceAll(it.Kind, "_", " "), repo, it.Title)
		if it.Link != "" {
			text += " " + it.Link
		}
		add(repo, "item:"+it.ID, text+fmt.Sprintf(" (reply: seen %s)", it.ID))
	}
	return out
}

// optionKeys lists a choice decision's option keys.
func optionKeys(d Decision) string {
	var keys []string
	for _, o := range d.Options {
		keys = append(keys, o.Key)
	}
	return strings.Join(keys, ", ")
}

// deliveryRepository is the repository a delivery went to: as recorded,
// or for the owner's mark-delivered, the session's deliver entry for it.
func (s *Supervisor) deliveryRepository(name string, d DeliveryRecord) string {
	if d.Repository != "" {
		return d.Repository
	}
	for _, dl := range s.cfg.Sessions[name].Deliver {
		if name+":"+dl.Worktree == d.Delivered {
			return dl.Repository
		}
	}
	return ""
}

// ItemRepository is the configured repository an item's platform/path
// names, or "".
func (c *Config) ItemRepository(key string) string {
	names := make([]string, 0, len(c.Repositories))
	for name := range c.Repositories {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		r := c.Repositories[name]
		if key != "" && strings.EqualFold(key, repoKey(r.Forge, r.Project)) {
			return name
		}
	}
	return ""
}

// RepositoryConsole is the console a repository's feed goes to: its own,
// or its name.
func (c *Config) RepositoryConsole(repo string) string {
	if r := c.Repositories[repo]; r.Console != "" {
		return r.Console
	}
	return repo
}

// feedConsoles posts each line not yet posted, in a worker, one at a time.
// The first pass with the feed on records the lines already there as its
// baseline, and posts none of them.
func (s *Supervisor) feedConsoles(now time.Time) {
	if !s.cfg.Policies.Feed.Enabled {
		return
	}
	entries := s.feedEntries()
	f := s.st.Feed
	if !f.Started {
		keys := []string{}
		for _, e := range entries {
			keys = append(keys, e.Key)
		}
		s.emit("", Event{Kind: KindFeed, Feed: &FeedPayload{Phase: FeedBaseline, Keys: keys}})
		return
	}
	var due []feedEntry
	for _, e := range entries {
		n := len(f.Failures[e.Key])
		if f.Fed[e.Key] || n > 0 && now.Sub(f.Failures[e.Key][n-1]) < backoff(n, s.cfg.Policies.Restart) {
			continue
		}
		due = append(due, e)
	}
	if len(due) == 0 {
		return
	}
	failures := map[string]int{}
	for _, e := range due {
		failures[e.Key] = len(f.Failures[e.Key])
	}
	s.spawnShared("feed", func(w *Supervisor) {
		for _, e := range due {
			if w.halt != nil {
				return
			}
			w.feedPost(e, failures[e.Key])
		}
	})
}

// feedPost posts one line to its repository's console through the
// escalation command, screened first: a line that matches a confidential
// pattern is replaced by one naming only the repository, and the event
// records the pattern, not the text. failed is how often it failed before.
func (s *Supervisor) feedPost(e feedEntry, failed int) {
	console := s.cfg.RepositoryConsole(e.Repository)
	p := FeedPayload{Key: e.Key, Repository: e.Repository, Console: console, Text: e.Text}
	if i := s.confidentialMatch(e.Text); i > 0 {
		p.Text, p.Withheld = fmt.Sprintf("Something new in %s; the details were withheld because they matched a confidential pattern. Look at the page.", e.Repository), i
	}
	if s.shadow {
		p.Phase = FeedWould
		s.emit("", Event{Kind: KindFeed, Feed: &p})
		return
	}
	p.Phase = FeedIntent
	if s.emit("", Event{Kind: KindFeed, Feed: &p}); s.halt != nil {
		return
	}
	argv := make([]string, len(s.cfg.Policies.Escalate.Command))
	for i, part := range s.cfg.Policies.Escalate.Command {
		argv[i] = strings.NewReplacer("{console}", console, "{message}", p.Text).Replace(part)
	}
	err := s.runner.Run(argv)
	out := FeedPayload{Phase: FeedPosted, Key: e.Key, Repository: e.Repository, Console: console}
	if err != nil {
		out.Phase, out.Error = FeedFailed, s.withheld(err.Error())
		if failed+1 >= s.cfg.Policies.Restart.MaxRestarts {
			s.emit("", Event{Kind: KindFeed, Feed: &out})
			out.Phase = FeedGaveUp
		}
	}
	s.emit("", Event{Kind: KindFeed, Feed: &out})
}

// ConsoleReply applies the owner's reply in a console: "SESSION ID
// ANSWER" answers one of a session's decisions, read by the decision's
// type, and "seen ID..." marks items seen. It goes through RecordAnswer
// and MarkSeen, as the page's POST does, from console:NAME. A session or
// item whose repository does not feed that console cannot be answered or
// marked from it.
func ConsoleReply(cfg *Config, log *Log, st *LogState, now time.Time, console, reply string) error {
	f := strings.Fields(reply)
	from := "console:" + console
	if len(f) == 0 {
		return errors.New("an empty reply")
	}
	if strings.EqualFold(f[0], "seen") {
		var ids []string
		for _, id := range f[1:] {
			id = strings.Trim(id, ",")
			it, ok := st.Items[id]
			if !ok {
				return fmt.Errorf("no item %q", id)
			}
			if repo := cfg.ItemRepository(it.Repository); repo == "" || cfg.RepositoryConsole(repo) != console {
				return fmt.Errorf("item %s is not fed to console %q", id, console)
			}
			ids = append(ids, id)
		}
		return MarkSeen(log, now, ids, from)
	}
	if len(f) < 3 {
		return errors.New(`reply "SESSION ID ANSWER" or "seen ID..."`)
	}
	session, id := f[0], f[1]
	sess, ok := cfg.Sessions[session]
	if !ok {
		return fmt.Errorf("no session %q", session)
	}
	if sess.Repository == "" || cfg.RepositoryConsole(sess.Repository) != console {
		return fmt.Errorf("%s's decisions are not fed to console %q", session, console)
	}
	t := st.Tracks[session]
	if t == nil {
		return fmt.Errorf("no session %q", session)
	}
	d, ok := t.decision(id)
	if !ok {
		return fmt.Errorf("%s has no open decision %q", session, id)
	}
	// The answer is the rest of the reply as typed.
	rest := strings.TrimSpace(reply)
	for _, w := range f[:2] {
		rest = strings.TrimSpace(strings.TrimPrefix(rest, w))
	}
	a := AnswerPayload{ID: id, From: from}
	if o, ok := cutFold(rest, "other:"); ok {
		a.Other = strings.TrimSpace(o)
		return RecordAnswer(log, st, now, session, a)
	}
	switch d.Type {
	case DecisionYesNo:
		switch strings.ToLower(rest) {
		case "yes", "no":
			yes := strings.EqualFold(rest, "yes")
			a.Answer = &yes
		default:
			return errors.New("answer yes or no")
		}
	case DecisionChooseOne:
		a.Choice = rest
	case DecisionChooseMany:
		for _, k := range strings.Split(rest, ",") {
			if k = strings.TrimSpace(k); k != "" {
				a.Choices = append(a.Choices, k)
			}
		}
	case DecisionNumber, DecisionDate:
		if n, err := strconv.ParseFloat(rest, 64); err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) {
			a.Value = json.RawMessage(strconv.FormatFloat(n, 'f', -1, 64))
		} else {
			a.Value, _ = json.Marshal(rest)
		}
	default:
		a.Text = rest
	}
	return RecordAnswer(log, st, now, session, a)
}

// cutFold cuts prefix from s, ignoring case.
func cutFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}
