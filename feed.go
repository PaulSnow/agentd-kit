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
// console receives, through the escalation command if there is one, and
// in the UI in any case, a plain line for each
// thing the page shows of that repository: a decision waiting on the
// owner, a settled CI result, a delivery, a merge, a new item. The lines
// are listed from the folded state, each under a key that stays the same
// however often it is listed, and each is posted once: feed events record
// it, so a restart does not post it again. Escalations already go to the
// same console with the same text, and are not fed again. The owner's
// reply in a console comes back through agentd console reply as an owner
// record, as the page's POST does, and wakes the loop.

// FeedPolicy turns the feed on. It is off by default. TurnLength is how
// much of a session's turn its console is shown (turns.go), in bytes.
type FeedPolicy struct {
	Enabled    bool `json:"enabled"`
	TurnLength int  `json:"turn_length"`
}

// Feed phases.
const (
	FeedBaseline = "baseline" // the entries there when the feed began for its repositories: never posted
	FeedPaused   = "paused"   // the feed was off, or the repositories it names left the configuration: they begin again with a baseline
	FeedIntent   = "intent"   // a post is about to be made
	FeedPosted   = "posted"
	FeedFailed   = "failed"
	FeedGaveUp   = "gave-up"
	FeedWould    = "would" // shadow mode: the post it would make
	// FeedImported is a line of the old console's history, imported once
	// by the owner (#41): the fold ignores it, and the UI shows it.
	FeedImported = "imported"
)

// FeedPayload is one step of a feed post: its entry's key, the repository
// and console, and the text posted, or the confidential pattern it matched
// in its place. A baseline names the repositories it begins the feed for
// and every key of theirs there then.
type FeedPayload struct {
	Phase        string   `json:"phase"`
	Key          string   `json:"key,omitempty"`
	Keys         []string `json:"keys,omitempty"`
	Repositories []string `json:"repositories,omitempty"`
	Repository   string   `json:"repository,omitempty"`
	Console      string   `json:"console,omitempty"`
	Text         string   `json:"text,omitempty"`
	Withheld     int      `json:"withheld,omitempty"`
	Error        string   `json:"error,omitempty"`
	// An imported line: when it was written, by whom, its kind when the
	// outside console program's line gave one, and the session it was
	// typed to when posted with --to.
	At   *time.Time `json:"at,omitempty"`
	From string     `json:"from,omitempty"`
	Kind string     `json:"kind,omitempty"`
	To   string     `json:"to,omitempty"`
}

// FeedState is what the feed events fold to: the repositories the feed
// has begun for since it was last off, the keys posted or settled, and
// each key's failed tries.
type FeedState struct {
	Begun    map[string]bool
	Fed      map[string]bool
	Failures map[string][]time.Time
}

// FeedPerPass is how many lines one pass posts at most; the rest wait for
// the next, so a burst of new items does not hold the console's command
// for long.
const FeedPerPass = 10

// FeedTextLimit is how long a posted line may be, in bytes. A line is
// screened whole before it is cut.
const FeedTextLimit = 1000

// applyFeed folds a feed event. An intent counts as posted, so a post a
// crash of agentd cut short is never repeated; its failure undoes it.
func (st *LogState) applyFeed(e Event) {
	f := &st.Feed
	if f.Fed == nil {
		f.Begun, f.Fed, f.Failures = map[string]bool{}, map[string]bool{}, map[string][]time.Time{}
	}
	p := e.Feed
	switch p.Phase {
	case FeedPaused:
		if len(p.Repositories) == 0 {
			f.Begun = map[string]bool{}
		}
		for _, r := range p.Repositories {
			delete(f.Begun, r)
		}
	case FeedBaseline:
		for _, r := range p.Repositories {
			f.Begun[r] = true
		}
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

// feedEntry is one line for a repository's console: its text, and the
// reply to type, which a cut never shortens.
type feedEntry struct {
	Key, Repository, Text, Reply string
}

// feedEntries lists every line the state holds for the configured
// repositories, in a stable order.
func (s *Supervisor) feedEntries() []feedEntry {
	var out []feedEntry
	merged := map[string]bool{} // the requests agentd merged, by key and head
	add := func(repo, key, text, reply string) {
		if _, ok := s.cfg.Repositories[repo]; ok {
			out = append(out, feedEntry{Key: key, Repository: repo, Text: text, Reply: reply})
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
			// The reply names the decision as it was posted, so a late
			// reply cannot answer a question reworded since.
			add(sess.Repository, "decision:"+name+":"+d.ID+":"+decisionHash(d), text,
				fmt.Sprintf(" Reply: %s %s@%s <answer>", name, d.ID, decisionHash(d)))
		}
		for _, d := range t.Deliveries {
			repo := s.deliveryRepository(name, d)
			if d.Ref == "" || d.Ref == "would-open" {
				continue
			}
			add(repo, "delivery:"+d.Delivered+":"+d.Ref, fmt.Sprintf("%s delivered %s to %s.", name, d.Ref, repo), "")
		}
		// Only a merge made: one cut short may not have been.
		keys := make([]string, 0, len(t.Merged))
		for k := range t.Merged {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			repo, ref, _ := strings.Cut(k, " ")
			merged[k+" "+t.Merged[k]] = true
			add(repo, "merge:"+k+":"+t.Merged[k], fmt.Sprintf("agentd merged %s in %s at %s, %s's delivery.", ref, repo, shortHash(t.Merged[k]), name), "")
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
		if r.State == CIMerged && merged[k+" "+r.Head] {
			continue // its merge line says so
		}
		text := fmt.Sprintf("%s in %s: CI %s on %s.", r.Ref, r.Repository, r.State, shortHash(r.Head))
		if r.State == CIFailed {
			text = fmt.Sprintf("%s in %s: CI failed on %s (%s; failing: %s).", r.Ref, r.Repository, shortHash(r.Head), r.Verdict, strings.Join(r.Jobs, ", "))
		}
		add(r.Repository, "ci:"+k+":"+r.Head+":"+r.State, text, "")
	}
	for _, it := range s.st.SortedItems(true) {
		repo := s.cfg.ItemRepository(it.Repository)
		text := fmt.Sprintf("New %s in %s: %s", strings.ReplaceAll(it.Kind, "_", " "), repo, it.Title)
		if it.Link != "" {
			text += " " + it.Link
		}
		add(repo, "item:"+it.ID, text, fmt.Sprintf(" (reply: seen %s)", it.ID))
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

// feedConsoles posts each line not yet posted, in a worker, one at a time,
// at most FeedPerPass a pass. The first pass that feeds a repository,
// whether the feed was just turned on or the repository just configured,
// records its lines already there as its baseline, and posts none of
// them; so does the first after the feed was off, which is recorded once.
func (s *Supervisor) feedConsoles(now time.Time) {
	f := s.st.Feed
	if !s.cfg.Policies.Feed.Enabled {
		if len(f.Begun) > 0 {
			s.emit("", Event{Kind: KindFeed, Feed: &FeedPayload{Phase: FeedPaused}})
		}
		return
	}
	// A repository no longer configured is paused, so it begins again
	// with a baseline if it comes back.
	var gone []string
	for repo := range f.Begun {
		if _, ok := s.cfg.Repositories[repo]; !ok {
			gone = append(gone, repo)
		}
	}
	if len(gone) > 0 {
		sort.Strings(gone)
		s.emit("", Event{Kind: KindFeed, Feed: &FeedPayload{Phase: FeedPaused, Repositories: gone}})
	}
	entries := s.feedEntries()
	var begin []string
	for repo := range s.cfg.Repositories {
		if !f.Begun[repo] {
			begin = append(begin, repo)
		}
	}
	sort.Strings(begin)
	if len(begin) > 0 {
		p := &FeedPayload{Phase: FeedBaseline, Repositories: begin, Keys: []string{}}
		for _, e := range entries {
			if !f.Begun[e.Repository] && !f.Fed[e.Key] {
				p.Keys = append(p.Keys, e.Key)
			}
		}
		s.emit("", Event{Kind: KindFeed, Feed: p})
		return
	}
	var due []feedEntry
	for _, e := range entries {
		n := len(f.Failures[e.Key])
		if f.Fed[e.Key] || n > 0 && now.Sub(f.Failures[e.Key][n-1]) < backoff(n, s.cfg.Policies.Restart) {
			continue
		}
		if due = append(due, e); len(due) == FeedPerPass {
			break
		}
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
// escalation command, when there is one, screened first: a line that matches a confidential
// pattern is replaced by one naming only the repository, and the event
// records the pattern, not the text. failed is how often it failed before.
func (s *Supervisor) feedPost(e feedEntry, failed int) {
	console := s.cfg.RepositoryConsole(e.Repository)
	// The text is cut, never the reply, and the line with its mark of the
	// cut stays within FeedTextLimit.
	p := FeedPayload{Key: e.Key, Repository: e.Repository, Console: console,
		Text: cutUTF8(e.Text, max(0, FeedTextLimit-len(e.Reply)-len("…"))) + e.Reply}
	if i := s.confidentialMatch(e.Text, e.Reply); i > 0 {
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
	// Without a command the intent is the post: the UI shows it (#42).
	var err error
	if tmpl := s.cfg.Policies.Escalate.Command; len(tmpl) > 0 {
		argv := make([]string, len(tmpl))
		for i, part := range tmpl {
			argv[i] = strings.NewReplacer("{console}", console, "{message}", p.Text).Replace(part)
		}
		err = s.runner.Run(argv)
	}
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
// marked from it. Any other reply is an instruction (RecordInstruction):
// a "seen" naming no known item, or words that do not begin with a
// session and one of its decisions or a decision's id.
func ConsoleReply(cfg *Config, log *Log, st *LogState, now time.Time, console, reply string) error {
	cfg = withRuntime(cfg, st.Runtime) // a started session's decisions are answered too
	f := strings.Fields(reply)
	from := "console:" + console
	if len(f) == 0 {
		return errors.New("an empty reply")
	}
	if !replyForm(st, cfg, f) {
		return RecordInstruction(cfg, log, st, now, console, reply)
	}
	if strings.EqualFold(f[0], "seen") {
		if len(f) == 1 {
			return errors.New(`reply "SESSION ID ANSWER" or "seen ID..."`)
		}
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
	id, asked, versioned := strings.Cut(id, "@")
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
	// A fed decision's line names its version, and a reply must too; a
	// blocking one's escalation shows none.
	if !versioned && !d.Blocking {
		return fmt.Errorf("reply to %s's %s as its line says, %s@VERSION", session, id, id)
	}
	if versioned && asked != decisionHash(d) {
		return fmt.Errorf("%s's %s changed since that line was posted; reply to its new line", session, id)
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

// replyForm says whether a reply's words read as a seen mark (seen, then
// at least one known item) or an answer (a session, then one of its
// decisions or a word like a decision's id, then more); a reply that
// does is applied as one, or refused, and never taken as an instruction.
func replyForm(st *LogState, cfg *Config, f []string) bool {
	if strings.EqualFold(f[0], "seen") {
		if len(f) == 1 {
			return true // names nothing: refused
		}
		for _, id := range f[1:] {
			if _, ok := st.Items[strings.Trim(id, ",")]; ok {
				return true
			}
		}
		return false
	}
	if _, ok := cfg.Sessions[f[0]]; !ok || len(f) < 2 {
		return false
	}
	id, _, _ := strings.Cut(f[1], "@")
	if t := st.Tracks[f[0]]; t != nil {
		if _, ok := t.decision(id); ok {
			return true
		}
	}
	return decisionLike.MatchString(f[1])
}

// cutFold cuts prefix from s, ignoring case.
func cutFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}
