package main

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Live facts (#62). A thread agent is given, read the moment it starts,
// each running session's brief heading, its STATUS.md "Now" and when that
// changed, and the repository's issues and requests updated in the last
// day, from one forge list shared by threads for a minute; so it never
// answers from a card the curator has not caught up with. What changes,
// a session's brief or Now, its start or finish, an issue's opening or
// closing, is logged as a fact: a history record on the session's and
// the issue's topics, which calls their curators.

// KindFact is a change agentd saw in a session's files or run, or in an
// issue's state.
const KindFact = "fact"

// Fact changes.
const (
	FactStarted  = "started"  // the session runs
	FactFinished = "finished" // its STATUS.md is DONE
	FactStopped  = "stopped"  // it failed, stopped, or is parked
	FactBrief    = "brief"    // its BRIEF.md's first heading changed
	FactNow      = "now"      // its STATUS.md's Now changed
	FactOpened   = "opened"   // an issue or request opened, or reopened
	FactClosed   = "closed"   // one closed or merged
	FactSeen     = "seen"     // first seen, long open or closed: no record, only what the next change is told from
)

var factChanges = []string{FactStarted, FactFinished, FactStopped, FactBrief, FactNow, FactOpened, FactClosed, FactSeen}

// FactPayload is one change: a session's, or an issue's or request's.
type FactPayload struct {
	Repository string     `json:"repository"`
	Change     string     `json:"change"`
	Session    string     `json:"session,omitempty"`
	Brief      string     `json:"brief,omitempty"` // the brief's first heading
	Now        string     `json:"now,omitempty"`   // STATUS.md's Now, cut
	Hash       string     `json:"hash,omitempty"`  // of what changed, as it is now
	Ref        string     `json:"ref,omitempty"`
	Title      string     `json:"title,omitempty"`
	State      string     `json:"state,omitempty"`
	Updated    *time.Time `json:"updated,omitempty"`
}

func (p *FactPayload) check() error {
	if p.Repository == "" || !contains(factChanges, p.Change) || (p.Session == "") == (p.Ref == "") {
		return fmt.Errorf("a fact names its repository, a change, and a session or a reference")
	}
	return nil
}

// factKey is what a fact is told from in LogState.Facts.
func factKey(p *FactPayload) string {
	switch {
	case p.Ref != "":
		return p.Repository + p.Ref
	case p.Change == FactBrief || p.Change == FactNow:
		return "session:" + p.Session + "|" + p.Change
	}
	return "session:" + p.Session + "|run"
}

// factValue is what LogState.Facts keeps of a fact.
func factValue(p *FactPayload) string {
	switch {
	case p.Ref != "":
		return p.State
	case p.Change == FactBrief || p.Change == FactNow:
		return p.Hash
	}
	return p.Change
}

func (st *LogState) applyFact(e Event) {
	if st.Facts == nil {
		st.Facts = map[string]string{}
	}
	st.Facts[factKey(e.Fact)] = factValue(e.Fact)
}

// factRecord is the history record a fact makes, or nil for one only seen.
func factRecord(e Event) *HistRecord {
	p := e.Fact
	var text, title string
	var topics []string
	switch p.Change {
	case FactSeen:
		return nil
	case FactOpened, FactClosed:
		title = p.Ref + " " + p.Change + ": " + p.Title
		text = fmt.Sprintf("%s is %s: %s", p.Ref, p.State, p.Title)
		topics = []string{p.Ref}
	case FactBrief:
		text = "session " + p.Session + "'s brief is now: " + p.Brief
	case FactNow:
		text = "session " + p.Session + "'s STATUS.md Now: " + p.Now
	default:
		text = "session " + p.Session + " " + p.Change
		if p.Brief != "" {
			text += "; its brief: " + p.Brief
		}
	}
	key := strings.Join([]string{p.Repository, p.Session, p.Ref, p.Change, p.Hash, p.State}, "|")
	if p.Updated != nil {
		key += "|" + p.Updated.UTC().Format(time.RFC3339Nano)
	} else {
		key += "|" + e.Time.UTC().Format(time.RFC3339Nano)
	}
	session := p.Session
	return &HistRecord{ID: histID(HistFact, key), Kind: HistFact, At: e.Time, Session: session, Title: title, Text: text,
		Topics: topicsOf(session, topics, title, text), Source: "fact:" + p.Change, repo: p.Repository}
}

// liveNow is the most of a STATUS.md's Now a thread agent is given.
const liveNow = 600

var nowHeading = regexp.MustCompile(`(?i)^(#+)\s*Now\b`)

// statusNow is the Now section of STATUS.md text, or, with none, the
// file's top; cut to liveNow bytes.
func statusNow(text string) string {
	lines := strings.Split(text, "\n")
	var out []string
	level := 0
	for _, l := range lines {
		if level > 0 {
			if m := anyHeading.FindStringSubmatch(l); m != nil && len(m[1]) <= level {
				break
			}
			out = append(out, l)
		} else if m := nowHeading.FindStringSubmatch(l); m != nil {
			level = len(m[1])
		}
	}
	if level == 0 {
		out = lines
	}
	return cutUTF8(strings.TrimSpace(strings.Join(out, "\n")), liveNow)
}

// briefHeading is the first heading of BRIEF.md text, or its first line.
func briefHeading(text string) string {
	first := ""
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if anyHeading.MatchString(t) {
			return cutUTF8(strings.TrimSpace(strings.TrimLeft(t, "#")), 200)
		}
		if first == "" {
			first = t
		}
	}
	return cutUTF8(first, 200)
}

// sessionFiles is what a session's brief and STATUS.md say now, read as
// regular files only, and when STATUS.md last changed.
type sessionFiles struct {
	Brief, Now string
	StatusAt   time.Time
	Done       bool
}

func readSessionFiles(workdir string) sessionFiles {
	var f sessionFiles
	if workdir == "" {
		return f
	}
	if b, err := readRegular(filepath.Join(workdir, "BRIEF.md"), statusLimit); err == nil {
		f.Brief = briefHeading(string(b))
	}
	path := filepath.Join(workdir, "STATUS.md")
	if b, err := readRegular(path, statusLimit); err == nil {
		f.Now, f.Done = statusNow(string(b)), ParseStatus(string(b)).Done
		if fi, err := os.Stat(path); err == nil {
			f.StatusAt = fi.ModTime().UTC()
		}
	}
	return f
}

// runOf is a session's run as a fact tells it: started, finished,
// stopped, or "" before agentd has seen it.
func (s *Supervisor) runOf(name string) string {
	t := s.st.Tracks[name]
	switch {
	case t == nil || t.State == "":
		return ""
	case t.State == Done:
		return FactFinished
	case t.Parked || t.State == Stopped || t.State == CrashLooping || t.State == Failed:
		return FactStopped
	}
	return FactStarted
}

// noteSessions logs, for each session of a repository, what changed in
// its run, its brief's heading and its STATUS.md's Now since the last
// fact; the brief and Now only while it runs. A shadow run logs none: the
// live run does.
func (s *Supervisor) noteSessions() {
	if s.shadow {
		return
	}
	for _, name := range s.cfg.SessionNames() {
		sess := s.cfg.Sessions[name]
		if sess.Repository == "" || sess.Role != "" || sess.Kind == KindChat || s.halt != nil {
			continue
		}
		run := s.runOf(name)
		if run == "" {
			continue
		}
		f := readSessionFiles(sess.Workdir)
		if s.confidentialMatch(f.Brief) > 0 {
			f.Brief = ""
		}
		if s.confidentialMatch(f.Now) > 0 {
			f.Now = ""
		}
		note := func(p FactPayload) {
			p.Repository, p.Session = sess.Repository, name
			if v, ok := s.st.Facts[factKey(&p)]; !ok || v != factValue(&p) {
				s.emit("", Event{Kind: KindFact, Fact: &p})
			}
		}
		if _, ok := s.st.Facts["session:"+name+"|run"]; ok || run == FactStarted {
			note(FactPayload{Change: run, Brief: f.Brief})
		}
		if run == FactStarted {
			note(FactPayload{Change: FactBrief, Brief: f.Brief, Hash: shortHash(HashPane(f.Brief))})
			note(FactPayload{Change: FactNow, Now: f.Now, Hash: shortHash(HashPane(f.Now))})
		}
	}
}

// LiveIssue is an issue or request as the forge's list has it now.
type LiveIssue struct {
	Ref     string    `json:"ref"`
	Title   string    `json:"title"`
	State   string    `json:"state"`
	Updated time.Time `json:"updated"`
	created time.Time
	closed  time.Time
}

// Bounds of the live issue list.
const (
	liveIssueAge     = time.Minute      // how long one list serves every thread
	liveIssueSpan    = 24 * time.Hour   // how far back it lists
	liveIssueRefresh = 5 * time.Minute  // how often the curators have it read, idle
	liveIssueMax     = 30               // the most a thread agent is given
	liveIssueFew     = 5                // the fewest kept before the thread is cut
	liveIssueTitle   = 160              // bytes of a title
	liveIssueCall    = "issues_updated" // its name in errors
)

// liveIssues is each repository's latest list, shared by the loop and its
// workers: a list is read by one at a time.
type liveIssues struct {
	mu  sync.Mutex
	at  map[string]time.Time
	is  map[string][]LiveIssue
	err map[string]error
}

// recent is repo's issues and requests updated within liveIssueSpan of
// now, newest first, and when they were read: the list read within
// liveIssueAge, or one forge list now.
func (l *liveIssues) recent(api ForgeAPI, repo string, r Repository, now time.Time) ([]LiveIssue, time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if at, ok := l.at[repo]; ok && now.Sub(at) < liveIssueAge && !now.Before(at) {
		return l.is[repo], at, l.err[repo]
	}
	if api == nil || r.Project == "" {
		return nil, time.Time{}, fmt.Errorf("%s: no forge to list %s's issues from", liveIssueCall, repo)
	}
	// A failed list is kept as long as one read, so a forge that fails is
	// not asked again by every thread.
	list, err := listRecent(api.At(r.Host), r, now.Add(-liveIssueSpan))
	if err != nil {
		list, err = nil, fmt.Errorf("%s: %v", liveIssueCall, oneLine(err.Error(), 200))
	}
	if l.at == nil {
		l.at, l.is, l.err = map[string]time.Time{}, map[string][]LiveIssue{}, map[string]error{}
	}
	l.at[repo], l.is[repo], l.err[repo] = now, list, err
	return list, now, err
}

// age is how long ago repo's list was read, or a day for one never read.
func (l *liveIssues) age(repo string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if at, ok := l.at[repo]; ok {
		return now.Sub(at)
	}
	return liveIssueSpan
}

// listRecent is one forge call: GitHub's issues (its requests among them)
// updated since; GitLab's issues, its requests being the merge requests
// list's, which the poll reads.
func listRecent(api ForgeAPI, r Repository, since time.Time) ([]LiveIssue, error) {
	var out []LiveIssue
	stamp := since.UTC().Format(time.RFC3339)
	if r.Forge == "gitlab" {
		var is []struct {
			IID       int        `json:"iid"`
			Title     string     `json:"title"`
			State     string     `json:"state"`
			UpdatedAt time.Time  `json:"updated_at"`
			CreatedAt time.Time  `json:"created_at"`
			ClosedAt  *time.Time `json:"closed_at"`
		}
		resp, err := api.GitLab(fmt.Sprintf("/projects/%s/issues?updated_after=%s&order_by=updated_at&sort=desc&per_page=100", url.PathEscape(r.Project), stamp))
		if err := getJSON(resp, err, "GitLab issues", &is); err != nil {
			return nil, err
		}
		for _, i := range is {
			li := LiveIssue{Ref: "#" + strconv.Itoa(i.IID), Title: i.Title, State: i.State, Updated: i.UpdatedAt, created: i.CreatedAt}
			if i.ClosedAt != nil {
				li.closed = *i.ClosedAt
			}
			out = append(out, li)
		}
	} else {
		var is []struct {
			Number      int        `json:"number"`
			Title       string     `json:"title"`
			State       string     `json:"state"`
			UpdatedAt   time.Time  `json:"updated_at"`
			CreatedAt   time.Time  `json:"created_at"`
			ClosedAt    *time.Time `json:"closed_at"`
			PullRequest *struct {
				MergedAt *time.Time `json:"merged_at"`
			} `json:"pull_request"`
		}
		resp, err := api.GitHub(fmt.Sprintf("/repos/%s/issues?state=all&sort=updated&direction=desc&since=%s&per_page=100", r.Project, stamp), nil)
		if err := getJSON(resp, err, "GitHub issues", &is); err != nil {
			return nil, err
		}
		for _, i := range is {
			li := LiveIssue{Ref: "#" + strconv.Itoa(i.Number), Title: i.Title, State: i.State, Updated: i.UpdatedAt, created: i.CreatedAt}
			if i.PullRequest != nil {
				li.Ref = "!" + strconv.Itoa(i.Number)
				if i.PullRequest.MergedAt != nil {
					li.State = "merged"
				}
			}
			if i.ClosedAt != nil {
				li.closed = *i.ClosedAt
			}
			out = append(out, li)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// issueFacts are the facts a list read at now makes, against the states
// known: an issue opened, reopened or closed; one first seen that opened
// or closed within the list's span is that change, any other only seen.
func issueFacts(repo string, list []LiveIssue, known map[string]string, now time.Time) []FactPayload {
	var out []FactPayload
	for _, li := range list {
		updated := li.Updated
		p := FactPayload{Repository: repo, Ref: li.Ref, Title: li.Title, State: li.State, Updated: &updated}
		open := li.State == "open" || li.State == "opened"
		was, ok := known[repo+li.Ref]
		switch {
		case ok && was == li.State:
			continue
		case ok && open:
			p.Change = FactOpened
		case ok:
			p.Change = FactClosed
		case open && now.Sub(li.created) <= liveIssueSpan:
			p.Change = FactOpened
		case !open && !li.closed.IsZero() && now.Sub(li.closed) <= liveIssueSpan:
			p.Change = FactClosed
		default:
			p.Change = FactSeen
		}
		out = append(out, p)
	}
	return out
}

// knownIssues copies the issue states the log has, for a worker.
func (st *LogState) knownIssues() map[string]string {
	out := map[string]string{}
	for k, v := range st.Facts {
		if !strings.HasPrefix(k, "session:") {
			out[k] = v
		}
	}
	return out
}

// readIssues reads repo's live list, from the cache or the forge, and
// logs the facts it makes; the titles are screened, and one that matches
// is left out of what is returned and logged.
func (w *Supervisor) readIssues(repo string, known map[string]string) ([]LiveIssue, time.Time, error) {
	now := w.clock.Now()
	list, at, err := w.live.recent(w.api, repo, w.cfg.Repositories[repo], now)
	if err != nil {
		return nil, at, err
	}
	var kept []LiveIssue
	for _, li := range list {
		if w.confidentialMatch(li.Title) > 0 {
			li.Title = ""
		}
		li.Title = cutUTF8(li.Title, liveIssueTitle)
		kept = append(kept, li)
	}
	if !w.shadow {
		for _, p := range issueFacts(repo, kept, known, now) {
			p := p
			w.emit("", Event{Kind: KindFact, Fact: &p})
		}
	}
	return kept, at, nil
}

// noteFacts logs what changed in the sessions, and, for the curators,
// has each repository's issues listed when no thread did so within
// liveIssueRefresh.
func (s *Supervisor) noteFacts(now time.Time) {
	s.noteSessions()
	if s.shadow || s.api == nil || !s.cfg.Jobs.Curator.Enabled {
		return
	}
	for _, repo := range sortedKeys(s.cfg.Repositories) {
		if s.cfg.Repositories[repo].Project == "" || s.live.age(repo, now) < liveIssueRefresh {
			continue
		}
		repo, known := repo, s.st.knownIssues()
		s.spawnShared("issues:"+repo, func(w *Supervisor) {
			w.readIssues(repo, known)
		})
	}
}
