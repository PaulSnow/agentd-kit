package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The issue index (#69 A). Per repository, Go lists every open issue and
// request on the forge at poll.every and logs one index event for each
// that is new, changed or no longer open, so the pages read the forge
// only through the log. Each issue carries a sentence saying where it
// stands, logged by what knows: the status check for an issue a session
// works, the request loop for a request and its issue, and, for an idle
// issue, a small model call made once per change on the forge. An idle
// issue the first listing found is back-filled once, at most backfillCap
// per repository each poll interval, spread over it, backfillAtOnce at a
// time. The newest sentence stands, with its writer.

// Index and sentence event kinds.
const (
	KindIndex    = "index"
	KindSentence = "sentence"
)

// Sentence writers.
const (
	WriterStatus = "status" // the status check of the session working the issue
	WriterLoop   = "loop"   // the request loop, from the request's place
	WriterIdle   = "idle"   // the idle-issue job, once per change on the forge, and the back-fill
)

// Index limits: the pages one listing reads, the labels kept, a title's
// length, the idle issues one pass writes, and the idle job's input.
const (
	indexPages     = 20
	indexPerPage   = 100
	indexLabels    = 12
	indexTitle     = 200
	sentenceMax    = 240
	idleBatch      = 5
	idleBody       = 2000
	idleComments   = 5
	idleRetryAfter = time.Hour
	idleTries      = 3
	backfillCap    = 300
	backfillAtOnce = 10
)

// BackfillPoll is the shortest wait between loop passes while a back-fill
// has calls left to start, so they start a few at a time.
const BackfillPoll = 30 * time.Second

// IndexPayload is one open issue or request as the forge lists it, or one
// that left the open list (Gone). First marks the repository's first
// listing: its issues are known, not changed.
type IndexPayload struct {
	Repository string    `json:"repository"`
	Ref        string    `json:"ref"` // #N or !N
	Title      string    `json:"title,omitempty"`
	State      string    `json:"state,omitempty"`
	Labels     []string  `json:"labels,omitempty"`
	Milestone  string    `json:"milestone,omitempty"`
	Author     string    `json:"author,omitempty"`
	Branch     string    `json:"branch,omitempty"` // a request's source branch
	Draft      bool      `json:"draft,omitempty"`
	Opened     time.Time `json:"opened,omitempty"`
	Changed    time.Time `json:"changed,omitempty"`
	Gone       bool      `json:"gone,omitempty"`
	First      bool      `json:"first,omitempty"`
	Withheld   int       `json:"withheld,omitempty"`
}

func (p *IndexPayload) check() error {
	if p.Repository == "" || !issueRefPattern.MatchString(p.Ref) && !mrRefPattern.MatchString(p.Ref) {
		return fmt.Errorf("an index event names its repository and an issue or request")
	}
	return nil
}

// same says whether two listings say the same of an issue.
func (p IndexPayload) same(q IndexPayload) bool {
	return p.Title == q.Title && p.State == q.State && strings.Join(p.Labels, "\n") == strings.Join(q.Labels, "\n") &&
		p.Milestone == q.Milestone && p.Author == q.Author && p.Branch == q.Branch && p.Draft == q.Draft &&
		p.Opened.Equal(q.Opened) && p.Changed.Equal(q.Changed) && p.Gone == q.Gone && p.Withheld == q.Withheld
}

// SentencePayload is where an issue or request stands, in one sentence,
// as its writer put it; For is the forge change an idle sentence was
// written for, and Failed an idle call that failed for it.
type SentencePayload struct {
	Repository string    `json:"repository"`
	Ref        string    `json:"ref"`
	Text       string    `json:"text,omitempty"`
	Writer     string    `json:"writer"`
	Session    string    `json:"session,omitempty"`
	For        time.Time `json:"for,omitempty"`
	Failed     string    `json:"failed,omitempty"`
	Usage      *JobUsage `json:"usage,omitempty"`
	Model      string    `json:"model,omitempty"`
	Withheld   int       `json:"withheld,omitempty"`
	// Hash is the text's hash as written, kept when the text is
	// withheld, so the same text is not logged again.
	Hash string `json:"hash,omitempty"`
}

func (p *SentencePayload) check() error {
	switch {
	case p.Repository == "" || !issueRefPattern.MatchString(p.Ref) && !mrRefPattern.MatchString(p.Ref):
		return fmt.Errorf("a sentence names its repository and an issue or request")
	case p.Writer != WriterStatus && p.Writer != WriterLoop && p.Writer != WriterIdle:
		return fmt.Errorf("a sentence's writer is %s, %s or %s", WriterStatus, WriterLoop, WriterIdle)
	case p.Text == "" && p.Failed == "":
		return fmt.Errorf("a sentence carries its text, or why the idle job wrote none")
	}
	return nil
}

// IndexIssue is an open issue or request as the index holds it: the
// latest listing, when it was first and last logged, and the change on
// the forge the repository's first listing found it at (Quiet), which no
// idle sentence is written for.
type IndexIssue struct {
	IndexPayload
	Seen  time.Time
	At    time.Time
	Quiet time.Time
}

// Number is the issue's or request's number.
func (i *IndexIssue) Number() int { n, _ := strconv.Atoi(i.Ref[1:]); return n }

// IndexSentence is the latest sentence of a ref, and when it was logged;
// IdleFor and IdleFailed the latest idle attempt, IdleFailed zero once
// one worked; IdleWithheld whether it was withheld; IdleTries the idle
// calls that failed for IdleFor's change; Tried the hash each writer last
// wrote, withheld or not.
type IndexSentence struct {
	SentencePayload
	At           time.Time
	IdleFor      time.Time
	IdleFailed   time.Time
	IdleWithheld bool
	IdleTries    int
	Tried        map[string]string
}

// textHash is a sentence's hash, as Tried keeps it.
func textHash(text string) string { return shortHash(HashPane(text)) }

// sentenceKey keys a ref's sentence.
func sentenceKey(repo, ref string) string { return repo + " " + ref }

func (st *LogState) applyIndex(e Event) {
	p := *e.Index
	if st.Index == nil {
		st.Index = map[string]map[string]*IndexIssue{}
	}
	m := st.Index[p.Repository]
	if m == nil {
		m = map[string]*IndexIssue{}
		st.Index[p.Repository] = m
	}
	if st.IndexRead == nil {
		st.IndexRead = map[string]time.Time{}
	}
	st.IndexRead[p.Repository] = e.Time
	prev := m[p.Ref]
	if p.Gone {
		// What is kept of it goes with it, so the fold does not grow
		// with every issue ever closed.
		delete(m, p.Ref)
		delete(st.Steps, sentenceKey(p.Repository, p.Ref))
		delete(st.Sentences, sentenceKey(p.Repository, p.Ref))
		return
	}
	i := &IndexIssue{IndexPayload: p, Seen: e.Time, At: e.Time}
	switch {
	case prev != nil:
		i.Seen, i.Quiet = prev.Seen, prev.Quiet
		if prev.Title != p.Title && p.Title != "" {
			st.noteStep(p.Repository, p.Ref, e.Time, "Retitled on the forge: "+p.Title)
		}
	case !p.First && !p.Opened.IsZero():
		st.noteStep(p.Repository, p.Ref, p.Opened, fmt.Sprintf("Opened on the forge by %s.", p.Author))
	}
	if p.First {
		i.Quiet = p.Changed
	}
	m[p.Ref] = i
}

func (st *LogState) applySentence(e Event) {
	p := *e.Sentence
	if st.Sentences == nil {
		st.Sentences = map[string]*IndexSentence{}
	}
	k := sentenceKey(p.Repository, p.Ref)
	prev := st.Sentences[k]
	s := &IndexSentence{}
	if prev != nil {
		c := *prev
		s = &c
	}
	s.Tried = copyMap(s.Tried)
	if s.Tried == nil {
		s.Tried = map[string]string{}
	}
	if p.Hash != "" {
		s.Tried[p.Writer] = p.Hash
	}
	if p.Writer == WriterIdle {
		tries := 0
		if p.Failed != "" {
			tries = 1
			if !s.IdleFailed.IsZero() && s.IdleFor.Equal(p.For) {
				tries = s.IdleTries + 1
			}
		}
		s.IdleFor, s.IdleFailed, s.IdleWithheld, s.IdleTries = p.For, time.Time{}, p.Withheld > 0, tries
		if p.Failed != "" {
			s.IdleFailed = e.Time
		}
	}
	if p.Text != "" {
		if prev == nil || prev.Text != p.Text {
			st.noteStep(p.Repository, p.Ref, e.Time, p.Text)
		}
		s.SentencePayload, s.At = p, e.Time
	}
	st.Sentences[k] = s
}

// listOpen lists repo's open issues and requests, a page at a time, with
// their fields: GitHub's issues list holds both, its pulls list gives a
// request's branch; GitLab lists them apart.
func listOpen(api ForgeAPI, r Repository) ([]IndexPayload, bool, error) {
	truncated := false
	type label struct {
		Name string `json:"name"`
	}
	var out []IndexPayload
	page := func(get func(n int) (APIResponse, error), what string, each func([]byte) (int, error)) error {
		for n := 1; n <= indexPages; n++ {
			resp, err := get(n)
			if err != nil {
				return err
			}
			if resp.Status != 200 {
				return statusError(what, resp)
			}
			got, err := each(resp.Body)
			if err != nil {
				return fmt.Errorf("%s: %w", what, err)
			}
			if got < indexPerPage {
				return nil
			}
		}
		truncated = true
		return nil
	}
	if r.Forge == "gitlab" {
		p := url.PathEscape(r.Project)
		err := page(func(n int) (APIResponse, error) {
			return api.GitLab(fmt.Sprintf("/projects/%s/issues?state=opened&per_page=%d&page=%d", p, indexPerPage, n))
		}, "GitLab issues", func(b []byte) (int, error) {
			var is []struct {
				IID       int                     `json:"iid"`
				Title     string                  `json:"title"`
				State     string                  `json:"state"`
				Labels    []string                `json:"labels"`
				Milestone *struct{ Title string } `json:"milestone"`
				Author    struct{ Username string }
				CreatedAt time.Time `json:"created_at"`
				UpdatedAt time.Time `json:"updated_at"`
			}
			if err := json.Unmarshal(b, &is); err != nil {
				return 0, err
			}
			for _, i := range is {
				q := IndexPayload{Ref: "#" + strconv.Itoa(i.IID), Title: i.Title, State: "open", Labels: i.Labels, Author: i.Author.Username, Opened: i.CreatedAt, Changed: i.UpdatedAt}
				if i.Milestone != nil {
					q.Milestone = i.Milestone.Title
				}
				out = append(out, q)
			}
			return len(is), nil
		})
		if err != nil {
			return nil, false, err
		}
		err = page(func(n int) (APIResponse, error) {
			return api.GitLab(fmt.Sprintf("/projects/%s/merge_requests?state=opened&per_page=%d&page=%d", p, indexPerPage, n))
		}, "GitLab merge requests", func(b []byte) (int, error) {
			var ms []struct {
				IID          int                     `json:"iid"`
				Title        string                  `json:"title"`
				Labels       []string                `json:"labels"`
				Milestone    *struct{ Title string } `json:"milestone"`
				Author       struct{ Username string }
				SourceBranch string    `json:"source_branch"`
				Draft        bool      `json:"draft"`
				CreatedAt    time.Time `json:"created_at"`
				UpdatedAt    time.Time `json:"updated_at"`
			}
			if err := json.Unmarshal(b, &ms); err != nil {
				return 0, err
			}
			for _, m := range ms {
				q := IndexPayload{Ref: "!" + strconv.Itoa(m.IID), Title: m.Title, State: "open", Labels: m.Labels, Author: m.Author.Username,
					Branch: m.SourceBranch, Draft: m.Draft, Opened: m.CreatedAt, Changed: m.UpdatedAt}
				if m.Milestone != nil {
					q.Milestone = m.Milestone.Title
				}
				out = append(out, q)
			}
			return len(ms), nil
		})
		if err != nil {
			return nil, false, err
		}
	} else {
		err := page(func(n int) (APIResponse, error) {
			return api.GitHub(fmt.Sprintf("/repos/%s/issues?state=open&per_page=%d&page=%d", r.Project, indexPerPage, n), nil)
		}, "GitHub issues", func(b []byte) (int, error) {
			var is []struct {
				Number      int                     `json:"number"`
				Title       string                  `json:"title"`
				Labels      []label                 `json:"labels"`
				Milestone   *struct{ Title string } `json:"milestone"`
				User        struct{ Login string }  `json:"user"`
				Draft       bool                    `json:"draft"`
				CreatedAt   time.Time               `json:"created_at"`
				UpdatedAt   time.Time               `json:"updated_at"`
				PullRequest *struct{}               `json:"pull_request"`
			}
			if err := json.Unmarshal(b, &is); err != nil {
				return 0, err
			}
			for _, i := range is {
				q := IndexPayload{Ref: "#" + strconv.Itoa(i.Number), Title: i.Title, State: "open", Author: i.User.Login, Opened: i.CreatedAt, Changed: i.UpdatedAt}
				if i.PullRequest != nil {
					q.Ref, q.Draft = "!"+strconv.Itoa(i.Number), i.Draft
				}
				for _, l := range i.Labels {
					q.Labels = append(q.Labels, l.Name)
				}
				if i.Milestone != nil {
					q.Milestone = i.Milestone.Title
				}
				out = append(out, q)
			}
			return len(is), nil
		})
		if err != nil {
			return nil, false, err
		}
		branches := map[string]string{}
		err = page(func(n int) (APIResponse, error) {
			return api.GitHub(fmt.Sprintf("/repos/%s/pulls?state=open&per_page=%d&page=%d", r.Project, indexPerPage, n), nil)
		}, "GitHub pulls", func(b []byte) (int, error) {
			var ps []struct {
				Number int                  `json:"number"`
				Head   struct{ Ref string } `json:"head"`
			}
			if err := json.Unmarshal(b, &ps); err != nil {
				return 0, err
			}
			for _, p := range ps {
				branches["!"+strconv.Itoa(p.Number)] = p.Head.Ref
			}
			return len(ps), nil
		})
		if err != nil {
			return nil, false, err
		}
		for i := range out {
			out[i].Branch = branches[out[i].Ref]
		}
	}
	for i := range out {
		out[i].Repository = "" // set by the caller
		out[i].Title = cutUTF8(out[i].Title, indexTitle)
		if len(out[i].Labels) > indexLabels {
			out[i].Labels = out[i].Labels[:indexLabels]
		}
		sort.Strings(out[i].Labels)
	}
	return out, truncated, nil
}

// indexChanges is what a listing changes in the index known: each issue
// new or changed, and each known one the listing no longer holds, gone,
// unless the listing was cut short at indexPages. A first listing marks
// every issue First.
func indexChanges(repo string, list []IndexPayload, known map[string]IndexPayload, first, truncated bool) []IndexPayload {
	var out []IndexPayload
	listed := map[string]bool{}
	for _, p := range list {
		p.Repository = repo
		listed[p.Ref] = true
		if k, ok := known[p.Ref]; ok && k.same(p) {
			continue
		}
		p.First = first
		out = append(out, p)
	}
	for _, ref := range sortedKeys(known) {
		if !listed[ref] && !truncated {
			out = append(out, IndexPayload{Repository: repo, Ref: ref, Gone: true})
		}
	}
	return out
}

// indexRun is when each repository's index was last read, on the loop,
// and the error last told for each, which its workers set.
type indexRun struct {
	last   map[string]time.Time
	mu     sync.Mutex
	failed map[string]string
	listed map[string]bool // listed since agentd started, events or not
	// fill is each repository's back-fill run under way: when it started
	// and how many calls it started.
	fill map[string]*backfillRun
}

// backfillRun is one poll interval's back-fill of a repository; more says
// whether its last pass left due calls unstarted.
type backfillRun struct {
	start time.Time
	taken int
	more  bool
}

// backfillWait is how long the loop may wait before a back-fill under way
// has a call to start, BackfillPoll at the least; ok false when none has.
func (s *Supervisor) backfillWait(now time.Time) (time.Duration, bool) {
	if s.idx == nil {
		return 0, false
	}
	every := s.cfg.Poll.Every.Duration
	var wait time.Duration
	ok := false
	for _, run := range s.idx.fill {
		if !run.more {
			continue
		}
		d := run.start.Add(time.Duration(int64(every) * int64(run.taken) / backfillCap)).Sub(now)
		if !ok || d < wait {
			wait, ok = d, true
		}
	}
	return max(wait, BackfillPoll), ok
}

// firstListing says whether repo was never listed, by the log or since
// agentd started, and marks it listed.
func (r *indexRun) firstListing(repo string, read bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	first := !read && !r.listed[repo]
	r.listed[repo] = true
	return first
}

// failedOnce records repo's listing error, "" when it worked, and says
// whether it is new and so to be told.
func (r *indexRun) failedOnce(repo, msg string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.failed[repo]
	r.failed[repo] = msg
	return msg != "" && msg != prev
}

// indexed says whether repo's issues can be listed.
func indexed(r Repository) bool {
	return (r.Forge == "github" || r.Forge == "gitlab") && r.Project != ""
}

// readIndexes lists each forge repository's open issues at poll.every, in
// a worker, and starts the idle-issue sentences that are due. A shadow
// run reads nothing: the live run's index is the log's.
func (s *Supervisor) readIndexes(now time.Time) {
	if s.shadow || s.api == nil || !s.cfg.Poll.Index {
		return
	}
	if s.idx == nil {
		s.idx = &indexRun{last: map[string]time.Time{}, failed: map[string]string{}, listed: map[string]bool{}, fill: map[string]*backfillRun{}}
	}
	for _, repo := range sortedKeys(s.cfg.Repositories) {
		repo, r := repo, s.cfg.Repositories[repo]
		if !indexed(r) || s.halt != nil {
			continue
		}
		if s.cfg.Jobs.Sentences.Enabled {
			for _, ref := range idleDue(s.cfg, s.st, repo, now) {
				ref, i := ref, *s.st.Index[repo][ref]
				s.spawnShared("idle:"+repo+ref, func(w *Supervisor) { w.idleSentence(repo, r, i) })
			}
			s.backfill(repo, r, now)
		}
		if last := s.idx.last[repo]; last.IsZero() || !now.Before(last.Add(s.cfg.Poll.Every.Duration)) {
			s.idx.last[repo] = now
			known := map[string]IndexPayload{}
			for ref, i := range s.st.Index[repo] {
				known[ref] = i.IndexPayload
			}
			_, read := s.st.IndexRead[repo]
			s.spawnShared("index:"+repo, func(w *Supervisor) { w.readIndex(repo, r, known, read) })
		}
	}
}

// readIndex lists repo's open issues and logs what changed; a failed
// listing is logged once, until it works again.
func (s *Supervisor) readIndex(repo string, r Repository, known map[string]IndexPayload, read bool) {
	list, truncated, err := listOpen(s.api.At(r.Host), r)
	msg := ""
	if err != nil {
		msg = oneLine(err.Error(), 300)
	}
	if s.idx.failedOnce(repo, msg) {
		s.emit("", Event{Kind: KindError, Error: &ErrorPayload{Step: "listing " + repo + "'s open issues", Error: msg}})
	}
	if err != nil {
		return
	}
	first := s.idx.firstListing(repo, read)
	for k := range list {
		s.screenIndex(&list[k])
	}
	for _, p := range indexChanges(repo, list, known, first, truncated) {
		p := p
		s.emit("", Event{Kind: KindIndex, Index: &p})
	}
}

// screenIndex blanks a listed issue's words when any matches a
// confidential pattern, before it is compared with the index, so a
// withheld issue is logged once.
func (s *Supervisor) screenIndex(p *IndexPayload) {
	if i := s.confidentialMatch(append([]string{p.Title, p.Branch, p.Milestone, p.Author}, p.Labels...)...); i > 0 {
		p.Title, p.Branch, p.Milestone, p.Author, p.Labels, p.Withheld = "", "", "", "", nil, i
	}
}

// workingOn are repo's issues being worked: one a session agentd knows
// works, or an open work item names.
func workingOn(cfg *Config, st *LogState, repo string) map[string]bool {
	out := map[string]bool{}
	c := withRuntime(cfg, st.Runtime)
	for _, name := range sortedKeys(c.Sessions) {
		if s := c.Sessions[name]; s.Repository == repo && s.Issue > 0 {
			out["#"+strconv.Itoa(s.Issue)] = true
		}
	}
	for _, w := range st.Work {
		if w.Repository == repo && w.Issue > 0 && !workTerminal(w.State) {
			out["#"+strconv.Itoa(w.Issue)] = true
		}
	}
	return out
}

// workTerminal says whether a work item's state is an end.
func workTerminal(state string) bool { return state == WorkDone || state == WorkDeclined }

// idleDue are repo's idle issues whose sentence is due, oldest change
// first, at most idleBatch: an open issue no session or item works,
// changed on the forge since the repository's first listing, since its
// last idle sentence and since the status check or the loop last wrote
// its sentence, for whose change no idle call is resting (idleResting).
func idleDue(cfg *Config, st *LogState, repo string, now time.Time) []string {
	busy := workingOn(cfg, st, repo)
	var out []*IndexIssue
	for ref, i := range st.Index[repo] {
		if ref[0] != '#' || busy[ref] || i.Changed.IsZero() || !i.Changed.After(i.Quiet) {
			continue
		}
		if s := st.Sentences[sentenceKey(repo, ref)]; s != nil {
			if s.Text != "" && s.Writer != WriterIdle && !s.At.Before(i.Changed) {
				continue // written since by what works on it
			}
			if !s.IdleFor.Before(i.Changed) && (s.IdleFailed.IsZero() || idleResting(s, now)) {
				continue // written, withheld or failed for this change
			}
		}
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Changed.Before(out[b].Changed) })
	var refs []string
	for _, i := range out {
		if len(refs) == idleBatch {
			break
		}
		refs = append(refs, i.Ref)
	}
	return refs
}

// idleResting says whether s's last idle call failed and is not to be
// tried again yet for the change it was for: a withheld sentence is not
// tried again until the issue changes, as it would only be withheld
// again; a failed one waits idleRetryAfter, then twice that, and so on,
// and after idleTries tries is not tried again until the issue changes.
func idleResting(s *IndexSentence, now time.Time) bool {
	if s.IdleFailed.IsZero() {
		return false
	}
	if s.IdleWithheld || idleGaveUp(s) {
		return true
	}
	return s.IdleFailed.Add(idleRetryAfter << max(s.IdleTries-1, 0)).After(now)
}

// idleGaveUp says whether s's idle calls failed idleTries times for one
// change.
func idleGaveUp(s *IndexSentence) bool {
	return !s.IdleFailed.IsZero() && s.IdleTries >= idleTries
}

// backfillable says whether i is one the back-fill writes for: an open
// issue no session or item works, not changed on the forge since the first
// listing (a change is idleDue's), with no sentence, and whose idle
// sentence was not withheld, which writing it again would only repeat,
// and whose idle calls did not fail idleTries times.
func backfillable(st *LogState, repo string, i *IndexIssue, busy map[string]bool) bool {
	if i.Ref[0] != '#' || busy[i.Ref] || i.Changed.After(i.Quiet) {
		return false
	}
	s := st.Sentences[sentenceKey(repo, i.Ref)]
	return s == nil || s.Text == "" && !s.IdleWithheld && !idleGaveUp(s)
}

// backfillDue are repo's backfillable issues, the latest changed first,
// whose last idle call is not resting (idleResting).
func backfillDue(cfg *Config, st *LogState, repo string, now time.Time) []string {
	busy := workingOn(cfg, st, repo)
	var out []*IndexIssue
	for ref, i := range st.Index[repo] {
		if !backfillable(st, repo, i, busy) {
			continue
		}
		if s := st.Sentences[sentenceKey(repo, ref)]; s != nil && idleResting(s, now) {
			continue
		}
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].Changed.Equal(out[b].Changed) {
			return out[a].Changed.After(out[b].Changed)
		}
		return out[a].Number() < out[b].Number()
	})
	refs := make([]string, len(out))
	for k, i := range out {
		refs[k] = i.Ref
	}
	return refs
}

// backfill starts the idle sentences of repo's issues that have none, on
// the sentences job's model: at most backfillCap a poll interval, spread
// evenly over it, so the k-th call of a run starts k/backfillCap of the
// way through it, and at most backfillAtOnce under way; the loop comes
// back for the next (backfillWait). A loop pass that comes once the
// interval is over starts what the run has left, and the next pass starts
// a new run. None starts while a listing of the repository from an
// earlier pass is under way.
func (s *Supervisor) backfill(repo string, r Repository, now time.Time) {
	if s.work.inFlight["index:"+repo] {
		return // what it would write for may be changing
	}
	due := backfillDue(s.cfg, s.st, repo, now)
	run := s.idx.fill[repo]
	if run == nil {
		if len(due) == 0 {
			return
		}
		run = &backfillRun{start: now}
		s.idx.fill[repo] = run
	}
	every := s.cfg.Poll.Every.Duration
	allowed, over := backfillCap, !now.Before(run.start.Add(every))
	if !over {
		allowed = min(backfillCap, int(int64(now.Sub(run.start))*backfillCap/int64(every))+1)
	}
	busy := 0
	for key := range s.work.inFlight {
		if strings.HasPrefix(key, "idle:"+repo+"#") {
			busy++
		}
	}
	run.more = false
	for _, ref := range due {
		if s.work.inFlight["idle:"+repo+ref] {
			continue
		}
		if run.taken >= allowed || busy >= backfillAtOnce {
			run.more = true
			break
		}
		i := *s.st.Index[repo][ref]
		if s.spawnShared("idle:"+repo+ref, func(w *Supervisor) { w.idleSentence(repo, r, i) }) {
			run.taken++
			busy++
		}
	}
	if over {
		delete(s.idx.fill, repo)
	}
}

// BackfillProgress is how far the sentences of a repository's idle issues
// are written: Done of Of.
type BackfillProgress struct {
	Done int `json:"done"`
	Of   int `json:"of"`
}

// backfillProgress is repo's progress while the back-fill has issues left:
// its open issues no session or item works, but for those whose sentence
// was withheld, and of those the ones with a sentence; nil when the
// sentences job is off or no issue is backfillable, so an issue opened
// since, which idleDue writes for, does not show it.
func backfillProgress(cfg *Config, st *LogState, repo string) *BackfillProgress {
	if !cfg.Jobs.Sentences.Enabled {
		return nil
	}
	busy := workingOn(cfg, st, repo)
	var p BackfillProgress
	left := false
	for ref, i := range st.Index[repo] {
		if ref[0] != '#' || busy[ref] {
			continue
		}
		if s := st.Sentences[sentenceKey(repo, ref)]; s != nil && s.Text == "" && (s.IdleWithheld || idleGaveUp(s)) {
			continue
		}
		p.Of++
		if st.sentenceOf(repo, ref) != nil {
			p.Done++
		}
		left = left || backfillable(st, repo, i, busy)
	}
	if !left {
		return nil
	}
	return &p
}

// String is the progress in words.
func (p *BackfillProgress) String() string {
	return fmt.Sprintf("sentences being written, %d of %d done", p.Done, p.Of)
}

const idleSystem = `You write one sentence, at most 200 characters, saying where an open issue stands for its owner: what it asks and what last happened on it. Use only the facts given. No names of people beyond the author's login, no paths, no host names. Answer with the JSON the schema asks for.`

func idleSchema() []byte {
	return []byte(`{"type":"object","properties":{"sentence":{"type":"string","maxLength":240}},"required":["sentence"],"additionalProperties":false}`)
}

// idleInput is an idle issue as its sentence's call is given it.
type idleInput struct {
	Ref       string   `json:"ref"`
	Title     string   `json:"title"`
	Labels    []string `json:"labels,omitempty"`
	Milestone string   `json:"milestone,omitempty"`
	Author    string   `json:"author,omitempty"`
	Opened    string   `json:"opened"`
	Changed   string   `json:"changed"`
	Body      string   `json:"body,omitempty"`
	Comments  []string `json:"latest_comments,omitempty"`
}

// idleSentence writes an idle issue's sentence with one small model call,
// from its fields, body and latest comments, screened; it is logged for
// the change on the forge it was written for, or why it failed.
func (s *Supervisor) idleSentence(repo string, r Repository, i IndexIssue) {
	job := jobFromConfig(s.cfg)
	if m := s.cfg.Jobs.Sentences.Model; m != "" {
		job.Model = m
	}
	p := SentencePayload{Repository: repo, Ref: i.Ref, Writer: WriterIdle, For: i.Changed, Model: job.Model}
	in := idleInput{Ref: i.Ref, Title: i.Title, Labels: i.Labels, Milestone: i.Milestone, Author: i.Author,
		Opened: i.Opened.UTC().Format(time.RFC3339), Changed: i.Changed.UTC().Format(time.RFC3339)}
	in.Body, in.Comments = readIssueBrief(s.api.At(r.Host), r, i.Number())
	if s.confidentialMatch(in.Body) > 0 {
		in.Body = ""
	}
	var kept []string
	for _, c := range in.Comments {
		if s.confidentialMatch(c) == 0 {
			kept = append(kept, c)
		}
	}
	in.Comments = kept
	input, _ := json.Marshal(in)
	raw, usage, err := job.Run(s.runCtx, idleSystem, idleSchema(), input)
	p.Usage = &usage
	if err == nil {
		var a struct {
			Sentence string `json:"sentence"`
		}
		if err = json.Unmarshal(raw, &a); err == nil {
			p.Text = cutUTF8(oneLine(a.Sentence, sentenceMax), sentenceMax)
			p.Hash = textHash(p.Text)
			if p.Text == "" {
				err = fmt.Errorf("the answer's sentence is empty")
			}
		}
	}
	if err != nil {
		p.Text, p.Failed = "", oneLine(err.Error(), 300)
	}
	s.emit("", Event{Kind: KindSentence, Sentence: &p})
}

// readIssueBrief reads an issue's body and its latest comments, for its
// idle sentence; what cannot be read is left out.
func readIssueBrief(api ForgeAPI, r Repository, n int) (string, []string) {
	var body string
	var comments []string
	if r.Forge == "gitlab" {
		base := fmt.Sprintf("/projects/%s/issues/%d", url.PathEscape(r.Project), n)
		var i struct {
			Description string `json:"description"`
		}
		resp, err := api.GitLab(base)
		if getJSON(resp, err, "GitLab issue", &i) == nil {
			body = i.Description
		}
		var ns []struct {
			Body   string `json:"body"`
			System bool   `json:"system"`
		}
		resp, err = api.GitLab(fmt.Sprintf("%s/notes?sort=desc&order_by=created_at&per_page=%d", base, idleComments*2))
		if getJSON(resp, err, "GitLab notes", &ns) == nil {
			for k := len(ns) - 1; k >= 0; k-- {
				if !ns[k].System {
					comments = append(comments, ns[k].Body)
				}
			}
		}
	} else {
		base := fmt.Sprintf("/repos/%s/issues/%d", r.Project, n)
		var i struct {
			Body     string `json:"body"`
			Comments int    `json:"comments"`
		}
		resp, err := api.GitHub(base, nil)
		if getJSON(resp, err, "GitHub issue", &i) == nil {
			body = i.Body
		}
		if i.Comments > 0 {
			last := (i.Comments + threadComments - 1) / threadComments
			var cs []struct {
				Body string `json:"body"`
			}
			resp, err = api.GitHub(fmt.Sprintf("%s/comments?per_page=%d&page=%d", base, threadComments, last), nil)
			if getJSON(resp, err, "GitHub comments", &cs) == nil {
				for _, c := range cs {
					comments = append(comments, c.Body)
				}
			}
		}
	}
	if len(comments) > idleComments {
		comments = comments[len(comments)-idleComments:]
	}
	for k := range comments {
		comments[k] = cutUTF8(comments[k], idleBody/2)
	}
	return cutUTF8(body, idleBody), comments
}

// noteStatusSentence logs a status check's summary as the sentence of the
// issue the session works, when it says something other than said, the
// hash of what the status check last wrote there.
func (s *Supervisor) noteStatusSentence(name string, sess Session, r *StatusRecord, said string) {
	if r == nil || sess.Repository == "" || sess.Issue <= 0 || strings.TrimSpace(r.Summary) == "" {
		return
	}
	ref := "#" + strconv.Itoa(sess.Issue)
	text := cutUTF8(oneLine(r.Summary, sentenceMax), sentenceMax)
	if textHash(text) == said {
		return
	}
	s.emit("", Event{Kind: KindSentence, Sentence: &SentencePayload{Repository: sess.Repository, Ref: ref, Text: text, Writer: WriterStatus, Session: name, Hash: textHash(text)}})
}

// loopSentence is a request's place in its loop, in words.
func loopSentence(ref string, p loopPlace) string {
	text := ref + " is " + p.Phase
	if p.Reason != "" {
		text += ": " + p.Reason
	}
	return cutUTF8(oneLine(text, sentenceMax), sentenceMax)
}

// noteLoopSentence logs a request's place as its sentence, and as its
// issue's unless a session still running writes the issue's through its
// status check, when the place moved.
func (s *Supervisor) noteLoopSentence(q loopRequest, p loopPlace) {
	text := loopSentence(q.Ref, p)
	refs := []string{q.Ref}
	if issue := caseIssue(q.Case); issue != "" {
		prev := s.st.Sentences[sentenceKey(q.Repository, issue)]
		if _, running := s.cfg.Sessions[sessionOf(prev)]; prev == nil || prev.Writer != WriterStatus || !running {
			refs = append(refs, issue)
		}
	}
	h := textHash(text)
	for _, ref := range refs {
		if prev := s.st.Sentences[sentenceKey(q.Repository, ref)]; prev != nil && prev.Tried[WriterLoop] == h && (prev.Writer == WriterLoop || prev.Text == "") {
			continue
		}
		s.emit("", Event{Kind: KindSentence, Sentence: &SentencePayload{Repository: q.Repository, Ref: ref, Text: text, Writer: WriterLoop, Session: p.Session, Hash: h}})
	}
}

// sessionOf is the session a sentence names, "" for none.
func sessionOf(s *IndexSentence) string {
	if s == nil {
		return ""
	}
	return s.Session
}

// caseIssue is the #N of a case id repo#N, or "".
func caseIssue(id string) string {
	if i := strings.LastIndex(id, "#"); i >= 0 && issueRefPattern.MatchString(id[i:]) {
		return id[i:]
	}
	return ""
}

// IssueJoin is what belongs to one issue, by its number.
type IssueJoin struct {
	Work     []*WorkItem
	Sessions []string
	Requests []string // !N, oldest first
	Case     string
}

// branchNumber matches a number as a word of a branch name.
func branchNumber(n int) *regexp.Regexp {
	return regexp.MustCompile(`(^|[/_.-])` + strconv.Itoa(n) + `($|[/_.-])`)
}

// titleNumber matches #N in a title, as a whole reference.
func titleNumber(n int) *regexp.Regexp {
	return regexp.MustCompile(`#` + strconv.Itoa(n) + `\b`)
}

// joinIssue gathers what belongs to repo's issue n: the work items naming
// it, by ThreadWork.Issue or a forge done_when; the sessions working it,
// by Issue or by the item they serve; the open requests whose title says
// #n or whose branch holds n as a word; and its case.
func joinIssue(cfg *Config, st *LogState, repo string, n int) IssueJoin {
	var j IssueJoin
	ref := "#" + strconv.Itoa(n)
	items := map[string]bool{}
	for _, id := range sortedKeys(st.Work) {
		w := st.Work[id]
		if w.Repository == repo && (w.Issue == n || w.DoneWhen.Kind == "forge" && w.DoneWhen.Ref == ref) {
			j.Work = append(j.Work, w)
			items[w.ID] = true
		}
	}
	c := withRuntime(cfg, st.Runtime)
	for _, name := range sortedKeys(c.Sessions) {
		sess := c.Sessions[name]
		if sess.Repository != repo {
			continue
		}
		rt, started := st.Runtime[name]
		if sess.Issue == n || started && rt.Item != "" && items[rt.Item] {
			j.Sessions = append(j.Sessions, name)
		}
	}
	for _, w := range j.Work {
		if w.Session != "" && !w.terminal() && !contains(j.Sessions, w.Session) {
			j.Sessions = append(j.Sessions, w.Session)
		}
	}
	title, branch := titleNumber(n), branchNumber(n)
	for _, i := range st.Index[repo] {
		if i.Ref[0] == '!' && (title.MatchString(i.Title) || branch.MatchString(i.Branch)) {
			j.Requests = append(j.Requests, i.Ref)
		}
	}
	sort.Slice(j.Requests, func(a, b int) bool { return refNumber(j.Requests[a]) < refNumber(j.Requests[b]) })
	if _, ok := st.Cases[repo+ref]; ok {
		j.Case = repo + ref
	}
	return j
}

// refNumber is the number of #N or !N, 0 when it is neither.
func refNumber(ref string) int {
	if len(ref) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(ref[1:])
	return n
}

// IndexCounts are a repository's counts: open issues, those being worked
// (a live session or an open work item), open requests, and those that
// wait on the owner.
type IndexCounts struct {
	Open, Working, Requests, Waiting int
	// Loose are those of Waiting that no listed issue shows: a decision,
	// an item or a request whose issue is not listed, or that has none.
	Loose int
	Last  time.Time
	// Backfill is how far the idle issues' sentences are written, while
	// some are not.
	Backfill *BackfillProgress `json:"backfill,omitempty"`
}

// indexCounts counts repo's index; waiting says which issues wait on the
// owner.
func indexCounts(cfg *Config, st *LogState, repo string, waiting func(ref string) bool) IndexCounts {
	var c IndexCounts
	busy := workingOn(cfg, st, repo)
	for ref, i := range st.Index[repo] {
		if i.Changed.After(c.Last) {
			c.Last = i.Changed
		}
		if ref[0] == '!' {
			c.Requests++
			continue
		}
		c.Open++
		if busy[ref] {
			c.Working++
		}
		if waiting != nil && waiting(ref) {
			c.Waiting++
		}
	}
	c.Backfill = backfillProgress(cfg, st, repo)
	return c
}

// sentenceOf is the sentence a ref shows: its latest, or none.
func (st *LogState) sentenceOf(repo, ref string) *IndexSentence {
	if s := st.Sentences[sentenceKey(repo, ref)]; s != nil && s.Text != "" {
		return s
	}
	return nil
}

// ActIssue is Go opening an issue for work that names none, which a
// repository's delivery.allow must hold.
const ActIssue = "issue"

// issueBodyLine is the one line of an issue Go opens for work.
const issueBodyLine = "_Opened by agentd for the owner's work item %s, so the work has an issue to be tracked by. The owner's words and the design are on the console._"

// OpenIssue opens an issue on the forge, if the repository's delivery
// allows it, and returns its number.
func (a AllowedForge) OpenIssue(repo, title, body string) (int, error) {
	if err := a.allowed(repo, ActIssue); err != nil {
		return 0, err
	}
	return a.Forge.OpenIssue(a.Repos[repo], title, body)
}

// OpenIssue opens an issue through the forge's API and returns its number.
func (f ExecForge) OpenIssue(repo Repository, title, body string) (int, error) {
	var out []byte
	var err error
	var got struct {
		Number int `json:"number"`
		IID    int `json:"iid"`
	}
	switch repo.Forge {
	case "github":
		out, err = runTimed(f.Timeout, "", nil, true, "gh", glabArgs(repo, "--method", "POST", fmt.Sprintf("repos/%s/issues", repo.Project), "-f", "title="+title, "-f", "body="+body)...)
	case "gitlab":
		out, err = runTimed(f.Timeout, "", nil, true, "glab", glabArgs(repo, "--method", "POST", fmt.Sprintf("projects/%s/issues", url.PathEscape(repo.Project)), "-f", "title="+title, "-f", "description="+body)...)
	default:
		return 0, fmt.Errorf("unknown forge %q", repo.Forge)
	}
	if err != nil {
		return 0, fmt.Errorf("open an issue: %s", exitMessage(err))
	}
	if err := json.Unmarshal(out, &got); err != nil {
		return 0, fmt.Errorf("open an issue: the reply: %w", err)
	}
	if got.Number == 0 {
		got.Number = got.IID
	}
	if got.Number <= 0 {
		return 0, fmt.Errorf("open an issue: the reply names no number")
	}
	return got.Number, nil
}

// OpenedIssue is what the log says of the issue Go opened for a work item:
// its number, or Unsure when an open was begun and its outcome is not
// known, a crash or an error between the forge's call and the record.
type OpenedIssue struct {
	Number int
	Unsure bool
}

// noteOpenedIssue folds Go's opening of an issue for a work item: the
// intent marks it unsure until the action names the number. An error
// leaves it unsure, as a forge that timed out may have opened it.
func (st *LogState) noteOpenedIssue(e Event) {
	a := e.Act
	switch {
	case e.Kind == KindError && e.Error != nil:
		a = e.Error.Act
	case e.Kind == KindWarning && e.Warning != nil:
		a = e.Warning.Intent
	}
	if a == nil || a.Action != ActIssue || a.Key == "" {
		return
	}
	if st.IssuesOpened == nil {
		st.IssuesOpened = map[string]OpenedIssue{}
	}
	switch e.Kind {
	case KindIntent:
		if st.IssuesOpened[a.Key].Number == 0 {
			st.IssuesOpened[a.Key] = OpenedIssue{Unsure: true}
		}
	case KindAction:
		if n := refNumber(a.Ref); n > 0 {
			st.IssuesOpened[a.Key] = OpenedIssue{Number: n}
		}
	}
}

// findOpenedIssue looks through repo's issues, open or closed, newest
// first, for the one whose body carries line, and returns its number, or
// 0 when none does; one it cannot look through to the end is an error, so
// the caller opens none.
func findOpenedIssue(api ForgeAPI, r Repository, line string) (int, error) {
	for n := 1; n <= indexPages; n++ {
		var resp APIResponse
		var err error
		if r.Forge == "gitlab" {
			resp, err = api.GitLab(fmt.Sprintf("/projects/%s/issues?state=all&order_by=created_at&sort=desc&per_page=%d&page=%d", url.PathEscape(r.Project), indexPerPage, n))
		} else {
			resp, err = api.GitHub(fmt.Sprintf("repos/%s/issues?state=all&sort=created&direction=desc&per_page=%d&page=%d", r.Project, indexPerPage, n), nil)
		}
		if err != nil {
			return 0, err
		}
		if resp.Status != 200 {
			return 0, statusError("issues", resp)
		}
		var is []struct {
			Number      int    `json:"number"`
			IID         int    `json:"iid"`
			Body        string `json:"body"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(resp.Body, &is); err != nil {
			return 0, fmt.Errorf("issues: %w", err)
		}
		for _, i := range is {
			if strings.Contains(i.Body+i.Description, line) {
				return max(i.Number, i.IID), nil
			}
		}
		if len(is) < indexPerPage {
			return 0, nil
		}
	}
	return 0, fmt.Errorf("more than %d issues to look through", indexPages*indexPerPage)
}

// issueForWork opens an issue for work w of item id in repo, when it names
// none, so every piece of work has one, and sets w.Issue to it; work
// whose done_when is an issue's closing names that issue. It runs on the
// loop, as a console thread's comment does, bounded by the forge timeout.
// The issue opened is folded from the log, so an item asking again gets
// the same number; an open whose outcome the log lacks, cut short or
// failed, is looked for on the forge by its body's line before another. Where the
// repository does not allow it, or the forge refuses, or the title matches
// a confidential pattern, it opens none and logs why. A shadow run logs
// what it would open.
func (s *Supervisor) issueForWork(id, repo string, w *ThreadWork) {
	if w.Issue <= 0 && w.DoneWhen.Kind == "forge" && issueRefPattern.MatchString(w.DoneWhen.Ref) {
		w.Issue = refNumber(w.DoneWhen.Ref) // its end names the issue
	}
	if o := s.st.IssuesOpened["issue:"+id]; w.Issue <= 0 && o.Number > 0 {
		w.Issue = o.Number // opened for this item before
	}
	if w.Issue > 0 || !indexed(s.cfg.Repositories[repo]) {
		return
	}
	skip := func(reason string) {
		s.emit("", Event{Kind: KindSkip, Skip: &SkipPayload{Key: "issue:" + id, Action: ActIssue, Repository: repo,
			Reason: fmt.Sprintf("no issue opened for work item %s: %s", id, reason)}})
	}
	if err := s.forge.allowed(repo, ActIssue); err != nil {
		skip(err.Error() + " (delivery.allow lacks issue)")
		return
	}
	title := cutUTF8(oneLine(w.Title, workTitle), workTitle)
	if strings.TrimSpace(title) == "" {
		skip("the work has no title")
		return
	}
	if i := s.confidentialMatch(title); i > 0 {
		skip(fmt.Sprintf("its title matches confidential pattern %d", i))
		return
	}
	body := fmt.Sprintf(issueBodyLine, id)
	a := &ActPayload{Repository: repo, Key: "issue:" + id, Message: title}
	if o := s.st.IssuesOpened[a.Key]; o.Unsure {
		// An open with no recorded outcome may have reached the forge: the
		// issue it opened is found by its body's line, never opened twice.
		if s.api == nil {
			skip("an earlier open has no recorded outcome and no forge API is set to look for it")
			return
		}
		n, err := findOpenedIssue(s.api.At(s.cfg.Repositories[repo].Host), s.cfg.Repositories[repo], body)
		if err != nil {
			skip("an earlier open has no recorded outcome and the forge's issues could not be read: " + err.Error())
			return
		}
		if n > 0 {
			a.Action, a.Ref, a.Intent = ActIssue, "#"+strconv.Itoa(n), newIntentID()
			kind := KindAction
			if s.shadow {
				kind = KindWould
			}
			s.emit("", Event{Kind: kind, Act: a})
			w.Issue = n
			return
		}
	}
	n := 0
	if err := s.act("", ActIssue, a, func() error {
		var err error
		n, err = s.forge.OpenIssue(repo, title, body)
		if err == nil {
			a.Ref = "#" + strconv.Itoa(n)
		}
		return err
	}); err == nil && n > 0 {
		w.Issue = n
	}
}
