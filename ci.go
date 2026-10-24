package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The CI watcher (#25, #1 "What remains", item 1). Every request a
// session's deliveries name is watched, in a worker beside the forge poll,
// through the same read-only API: the pipeline of its exact head commit,
// and when that fails, the target branch's latest pipeline. A failure is
// the branch's own unless the target is red with the same jobs and the
// request is not the target's fix; on its own failure the delivering
// session is relaunched to fix it, handed the failing job's log excerpt,
// within the restart policy's bounds. The same failure after a fix, or
// the attempts used up, is a decision for the owner. A forge that cannot
// be asked is unknown, never green.

// DefaultCIEvery is how often each watched request is asked about when
// poll.ci_every is unset.
const DefaultCIEvery = 5 * time.Minute

// sourceCI is the CI watcher's entry among the poll worker's due times.
const sourceCI = "ci"

// CI states of a request's pipeline or the target's, and the verdicts on
// a failure.
const (
	CIPending = "pending"
	CISuccess = "success"
	CIFailed  = "failed"
	CINone    = "none" // no pipeline, or one that was cancelled or skipped
	CIUnknown = "unknown"
	CIMerged  = "merged"
	CIClosed  = "closed"

	CIOwn           = "own"            // the branch's own failure
	CITargetRed     = "target-red"     // the target is red with the same jobs: wait for it
	CITargetPending = "target-pending" // the target's latest pipeline has not settled: wait for it
)

// CI decision answers.
const (
	ciRetry = "retry"
	ciLeave = "leave"
)

// ciExcerptLines and ciExcerptBytes bound the failing job's log excerpt:
// its last lines, as the fix session's pack and the event carry it.
const (
	ciExcerptLines = 40
	ciExcerptBytes = 3000
)

// CIPolicy is how a fix session hears why it was started: Pointer, typed
// before its facts.
type CIPolicy struct {
	Pointer string `json:"pointer"`
}

// CIPayload is a change in a request's CI: its pipeline's state on Head,
// the target's when it was asked, the verdict on a failure, the failing
// jobs, the cause (a hash of the jobs and the excerpt, clocks and counters
// stripped) and the first failing job's log excerpt, or the error that
// left it unknown. Delivered is the delivery key that named the request.
type CIPayload struct {
	Repository  string   `json:"repository"`
	Ref         string   `json:"ref"`
	URL         string   `json:"url,omitempty"`
	Delivered   string   `json:"delivered,omitempty"`
	Head        string   `json:"head,omitempty"`
	Target      string   `json:"target,omitempty"`
	State       string   `json:"state"`
	TargetState string   `json:"target_state,omitempty"`
	Verdict     string   `json:"verdict,omitempty"`
	Jobs        []string `json:"jobs,omitempty"`
	Cause       string   `json:"cause,omitempty"`
	Excerpt     string   `json:"excerpt,omitempty"`
	Withheld    int      `json:"withheld,omitempty"` // the confidential pattern the excerpt matched
	Error       string   `json:"error,omitempty"`
}

// CIRecord is a request's latest CI, as the state holds it: which session
// delivered it, and since when it has been so.
type CIRecord struct {
	CIPayload
	Session string
	At      time.Time
}

// CIFixRecord is one fix session started for a request's failure, or
// one shadow mode would have started: the failing head and its cause,
// and the decision whose retry it took.
type CIFixRecord struct {
	Ref        string
	Repository string
	Delivered  string
	Head       string
	Cause      string
	Decision   string
	Transcript string
	Taken      bool
	At         time.Time
	// Pushed is the head the delivery after the fix recorded: the fix's
	// commit, or the failing head again when the fix changed nothing; ""
	// for the owner's mark-delivered. Redelivered says that delivery was
	// recorded, and Seen that the watcher has since recorded the head it
	// delivered, or any change for the owner's.
	Pushed      string
	Redelivered bool
	Seen        bool
}

// CIFact is what a fix session's pack says of the failure.
type CIFact struct {
	Request string   `json:"request"`
	URL     string   `json:"url,omitempty"`
	Head    string   `json:"head"`
	Target  string   `json:"target,omitempty"`
	Jobs    []string `json:"jobs"`
	Excerpt string   `json:"excerpt,omitempty"`
}

func ciKey(repo, ref string) string { return repo + " " + ref }

// same says whether two payloads say the same of a request.
func (p CIPayload) same(q CIPayload) bool {
	return p.State == q.State && p.Head == q.Head && p.TargetState == q.TargetState && p.Verdict == q.Verdict &&
		p.Cause == q.Cause && p.Error == q.Error && p.URL == q.URL && p.Delivered == q.Delivered
}

// applyCI folds a request's CI.
func (st *LogState) applyCI(e Event) {
	if st.CI == nil {
		st.CI = map[string]CIRecord{}
	}
	st.CI[ciKey(e.CI.Repository, e.CI.Ref)] = CIRecord{CIPayload: *e.CI, Session: e.Session, At: e.Time}
	if t := st.Tracks[e.Session]; t != nil {
		st.ciSeenBy(t, e.CI.Repository, e.CI.Ref, true)
	}
}

// ciSeen ends the wait of the session's latest fix when the record of its
// request already shows the head its delivery recorded.
func (st *LogState) ciSeen(t *Track) {
	if n := len(t.CIFixes); n > 0 {
		st.ciSeenBy(t, t.CIFixes[n-1].Repository, t.CIFixes[n-1].Ref, false)
	}
}

// ciSeenBy ends the wait for the head a request's latest fix delivered:
// at the first record of that head, or for the owner's mark-delivered,
// which names no head, at the next record (changed says one was just
// made). Any head after it is followed as it comes.
func (st *LogState) ciSeenBy(t *Track, repo, ref string, changed bool) {
	for i := len(t.CIFixes) - 1; i >= 0; i-- {
		f := t.CIFixes[i]
		if f.Repository != repo || f.Ref != ref || !f.Taken {
			continue
		}
		r, ok := st.CI[ciKey(repo, ref)]
		if f.Redelivered && !f.Seen && ok && (f.Pushed == "" && changed || f.Pushed != "" && f.Pushed == r.Head) {
			fixes := append([]CIFixRecord(nil), t.CIFixes...)
			fixes[i].Seen = true
			t.CIFixes = fixes
		}
		return
	}
}

// ciWatched is a request to ask about: whose delivery named it.
type ciWatched struct {
	Session    string
	Repository string
	Ref        string
	Delivered  string
}

// watchedRequests is every request a session's deliveries name, in a
// configured repository, the latest delivery of each, less those the
// forge said were merged or closed. The owner's mark-delivered names no
// repository: the session's deliver entry for the worktree does.
func (s *Supervisor) watchedRequests() []ciWatched {
	seen := map[string]int{}
	var out []ciWatched
	for _, name := range s.cfg.SessionNames() {
		t := s.st.Tracks[name]
		if t == nil {
			continue
		}
		sess := s.cfg.Sessions[name]
		for _, d := range t.Deliveries {
			repo := d.Repository
			if repo == "" {
				for _, dl := range sess.Deliver {
					if name+":"+dl.Worktree == d.Delivered {
						repo = dl.Repository
					}
				}
			}
			if _, ok := s.cfg.Repositories[repo]; !ok || !requestRef.MatchString(d.Ref) {
				continue
			}
			key := ciKey(repo, d.Ref)
			if r, ok := s.st.CI[key]; ok && (r.State == CIMerged || r.State == CIClosed) {
				continue
			}
			w := ciWatched{Session: name, Repository: repo, Ref: d.Ref, Delivered: d.Delivered}
			if i, ok := seen[key]; ok {
				out[i] = w
				continue
			}
			seen[key] = len(out)
			out = append(out, w)
		}
	}
	return out
}

// requestRef is a request's reference: !12 on GitLab, #12 on GitHub.
var requestRef = regexp.MustCompile(`^[!#][0-9]+$`)

// watchCI starts the watcher's round, in a worker, when it is due: every
// poll.ci_every while poll.ci is set and the forges' APIs are there.
func (s *Supervisor) watchCI(now time.Time) {
	if s.api == nil || !s.cfg.Poll.CI {
		return
	}
	s.work.mu.Lock()
	next := s.work.nextPoll[sourceCI]
	s.work.mu.Unlock()
	if now.Before(next) {
		return
	}
	watched := s.watchedRequests()
	if len(watched) == 0 {
		return
	}
	prev := copyMap(s.st.CI)
	s.spawnShared("poll:"+sourceCI, func(w *Supervisor) {
		w.work.mu.Lock()
		w.work.nextPoll[sourceCI] = now.Add(w.cfg.Poll.CIEvery.Duration)
		w.work.mu.Unlock()
		for _, r := range watched {
			p := w.askCI(r, prev[ciKey(r.Repository, r.Ref)])
			if old, ok := prev[ciKey(r.Repository, r.Ref)]; ok && old.Session == r.Session && old.same(p) {
				continue
			}
			w.emit(r.Session, Event{Kind: KindCI, CI: &p})
		}
	})
}

// askCI asks the forge about one request. The log excerpt is fetched only
// for a failure the last record does not already hold.
func (s *Supervisor) askCI(r ciWatched, prev CIRecord) CIPayload {
	repo := s.cfg.Repositories[r.Repository]
	p := CIPayload{Repository: r.Repository, Ref: r.Ref, Delivered: r.Delivered}
	api := s.api.At(repo.Host)
	var c ciCheck
	var err error
	if repo.Forge == "github" {
		c, err = githubCI(api, repo.Project, r.Ref)
	} else {
		c, err = gitlabCI(api, repo.Project, r.Ref)
	}
	if err != nil {
		p.State, p.Error = CIUnknown, s.withheld(err.Error())
		return p
	}
	p.URL, p.Head, p.Target, p.State = c.url, c.head, c.target, c.state
	if c.state != CIFailed {
		return p
	}
	p.Jobs = c.failed
	tgt, err := c.targetCI()
	if err != nil {
		p.State, p.Error = CIUnknown, s.withheld(err.Error())
		return p
	}
	p.TargetState = tgt.state
	p.Verdict = ciVerdict(c, tgt)
	if p.Verdict != CIOwn {
		return p
	}
	if prev.Head == p.Head && prev.Cause != "" && strings.Join(prev.Jobs, "\n") == strings.Join(p.Jobs, "\n") {
		p.Cause, p.Excerpt, p.Withheld = prev.Cause, prev.Excerpt, prev.Withheld
		return p
	}
	// A job whose log cannot be read (another CI app's check, on
	// GitHub) leaves the failure known, its excerpt empty.
	log := ""
	for _, id := range c.failedIDs {
		if l, err := c.jobLog(id); err == nil {
			log = l
			break
		}
	}
	excerpt := ciExcerpt(log)
	p.Cause = ciCause(p.Jobs, excerpt)
	if i := s.confidentialMatch(excerpt); i > 0 {
		p.Withheld = i
	} else {
		p.Excerpt = excerpt
	}
	return p
}

// ciVerdict says whose a failure is. It is the branch's own unless the
// target is red, every job failing on the request fails on the target
// too, no job failing on the target passes on the request, and the
// request's text names neither the target's red commit nor its pipeline:
// a request that passes what the target fails, or says it fixes it, is
// the target's fix, and is never left waiting for itself.
func ciVerdict(c, tgt ciCheck) string {
	if tgt.state == CIPending {
		return CITargetPending
	}
	if tgt.state != CIFailed {
		return CIOwn
	}
	red := map[string]bool{}
	for _, j := range tgt.failed {
		red[j] = true
	}
	for _, j := range c.failed {
		if !red[j] {
			return CIOwn
		}
	}
	for _, j := range c.passed {
		if red[j] {
			return CIOwn
		}
	}
	text := strings.ToLower(c.text)
	if len(tgt.head) >= 7 && strings.Contains(text, strings.ToLower(tgt.head[:7])) || tgt.url != "" && strings.Contains(text, strings.ToLower(tgt.url)) {
		return CIOwn
	}
	return CITargetRed
}

// ciCheck is what the forge says of one pipeline: its state, its failing
// and passing jobs, and for a request its head, target, page and text,
// with how to ask about its target and a job's log.
type ciCheck struct {
	url, head, target, text string
	state                   string
	failed, passed          []string
	failedIDs               []string
	targetCI                func() (ciCheck, error)
	jobLog                  func(id string) (string, error)
}

// ciGet asks one API path and decodes its JSON reply.
func ciGet(call func() (APIResponse, error), what string, v any) error {
	resp, err := call()
	if err != nil {
		return err
	}
	if resp.Status != http.StatusOK {
		return statusError(what, resp)
	}
	if v == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Body, v); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// ciText asks one API path for text: a job's log.
func ciText(call func() (APIResponse, error), what string) (string, error) {
	resp, err := call()
	if err != nil {
		return "", err
	}
	if resp.Status != http.StatusOK {
		return "", statusError(what, resp)
	}
	return string(resp.Body), nil
}

// gitlabCI asks GitLab about merge request !n: its head pipeline and that
// pipeline's jobs.
func gitlabCI(api ForgeAPI, project, ref string) (ciCheck, error) {
	base := "/projects/" + url.PathEscape(project)
	get := func(path string, v any) error {
		return ciGet(func() (APIResponse, error) { return api.GitLab(path) }, "glab api "+path, v)
	}
	var mr struct {
		State        string `json:"state"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		WebURL       string `json:"web_url"`
		SHA          string `json:"sha"`
		TargetBranch string `json:"target_branch"`
		HeadPipeline *struct {
			ID     int    `json:"id"`
			Status string `json:"status"`
			SHA    string `json:"sha"`
			Ref    string `json:"ref"`
		} `json:"head_pipeline"`
	}
	if err := get(base+"/merge_requests/"+ref[1:], &mr); err != nil {
		return ciCheck{}, err
	}
	c := ciCheck{url: mr.WebURL, head: mr.SHA, target: mr.TargetBranch, text: mr.Title + "\n" + mr.Description}
	switch mr.State {
	case "merged":
		c.state = CIMerged
		return c, nil
	case "closed":
		c.state = CIClosed
		return c, nil
	}
	jobs := func(c *ciCheck, pipeline int) error {
		var js []struct {
			ID           int    `json:"id"`
			Name         string `json:"name"`
			Status       string `json:"status"`
			AllowFailure bool   `json:"allow_failure"`
		}
		if err := get(fmt.Sprintf("%s/pipelines/%d/jobs?per_page=100", base, pipeline), &js); err != nil {
			return err
		}
		for _, j := range js {
			switch {
			case j.Status == "failed" && !j.AllowFailure:
				c.failed = append(c.failed, j.Name)
				c.failedIDs = append(c.failedIDs, strconv.Itoa(j.ID))
			case j.Status == "success":
				c.passed = append(c.passed, j.Name)
			}
		}
		c.failed, c.failedIDs = sortedJobs(c.failed, c.failedIDs)
		return nil
	}
	p := mr.HeadPipeline
	// A merged-results or merge-train pipeline runs on GitLab's merge of
	// the head, under the request's own ref.
	merged := p != nil && strings.HasPrefix(p.Ref, "refs/merge-requests/")
	if p == nil || p.SHA != "" && p.SHA != mr.SHA && !merged {
		// No pipeline has run on the head commit yet.
		c.state = CIPending
		if p == nil {
			c.state = CINone
		}
		return c, nil
	}
	c.state = gitlabState(p.Status)
	if c.state == CIFailed {
		if err := jobs(&c, p.ID); err != nil {
			return ciCheck{}, err
		}
		if len(c.failed) == 0 {
			c.state = CISuccess // only jobs allowed to fail failed
		}
	}
	c.targetCI = func() (ciCheck, error) {
		var ps []struct {
			ID     int    `json:"id"`
			Status string `json:"status"`
			SHA    string `json:"sha"`
			WebURL string `json:"web_url"`
		}
		// The latest finished pipeline: one still running says nothing yet
		// of the red one before it.
		if err := get(fmt.Sprintf("%s/pipelines?ref=%s&scope=finished&per_page=1", base, url.QueryEscape(mr.TargetBranch)), &ps); err != nil {
			return ciCheck{}, err
		}
		if len(ps) == 0 {
			return ciCheck{state: CINone}, nil
		}
		t := ciCheck{head: ps[0].SHA, url: ps[0].WebURL, state: gitlabState(ps[0].Status)}
		if t.state == CIFailed {
			if err := jobs(&t, ps[0].ID); err != nil {
				return ciCheck{}, err
			}
		}
		return t, nil
	}
	c.jobLog = func(id string) (string, error) {
		path := base + "/jobs/" + id + "/trace"
		return ciText(func() (APIResponse, error) { return api.GitLab(path) }, "glab api "+path)
	}
	return c, nil
}

// gitlabState is a GitLab pipeline status as a CI state.
func gitlabState(status string) string {
	switch status {
	case "success":
		return CISuccess
	case "failed":
		return CIFailed
	case "canceled", "skipped", "manual":
		return CINone
	}
	return CIPending
}

// githubCI asks GitHub about pull request #n: its head commit's check
// runs.
func githubCI(api ForgeAPI, project, ref string) (ciCheck, error) {
	base := "/repos/" + project
	get := func(path string, v any) error {
		return ciGet(func() (APIResponse, error) { return api.GitHub(path, nil) }, "gh api "+path, v)
	}
	var pr struct {
		State   string `json:"state"`
		Merged  bool   `json:"merged"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := get(base+"/pulls/"+ref[1:], &pr); err != nil {
		return ciCheck{}, err
	}
	c := ciCheck{url: pr.HTMLURL, head: pr.Head.SHA, target: pr.Base.Ref, text: pr.Title + "\n" + pr.Body}
	switch {
	case pr.Merged:
		c.state = CIMerged
		return c, nil
	case pr.State == "closed":
		c.state = CIClosed
		return c, nil
	}
	// settled judges completed runs only, as the target is: a run still
	// going says nothing yet of the red one before it.
	checks := func(c *ciCheck, commit string, settled bool) error {
		var cr struct {
			CheckRuns []struct {
				ID         int64  `json:"id"`
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				HeadSHA    string `json:"head_sha"`
				HTMLURL    string `json:"html_url"`
			} `json:"check_runs"`
		}
		if err := get(base+"/commits/"+url.PathEscape(commit)+"/check-runs?per_page=100", &cr); err != nil {
			return err
		}
		running := false
		for _, r := range cr.CheckRuns {
			if c.head == "" {
				c.head = r.HeadSHA
			}
			switch {
			case r.Status != "completed":
				running = !settled
			case r.Conclusion == "failure" || r.Conclusion == "timed_out" || r.Conclusion == "startup_failure":
				c.failed = append(c.failed, r.Name)
				c.failedIDs = append(c.failedIDs, strconv.FormatInt(r.ID, 10))
				if c.url == "" {
					c.url = r.HTMLURL
				}
			case r.Conclusion == "success" || r.Conclusion == "neutral" || r.Conclusion == "skipped":
				c.passed = append(c.passed, r.Name)
			}
		}
		c.failed, c.failedIDs = sortedJobs(c.failed, c.failedIDs)
		switch {
		case len(c.failed) > 0:
			c.state = CIFailed
		case running:
			c.state = CIPending
		case len(c.passed) > 0:
			c.state = CISuccess
		default:
			c.state = CINone
		}
		return nil
	}
	if err := checks(&c, pr.Head.SHA, false); err != nil {
		return ciCheck{}, err
	}
	c.targetCI = func() (ciCheck, error) {
		var t ciCheck
		if err := checks(&t, pr.Base.Ref, true); err != nil {
			return ciCheck{}, err
		}
		return t, nil
	}
	c.jobLog = func(id string) (string, error) {
		path := base + "/actions/jobs/" + id + "/logs"
		return ciText(func() (APIResponse, error) { return api.GitHub(path, nil) }, "gh api "+path)
	}
	return c, nil
}

// sortedJobs sorts failing jobs by name, keeping each id with its job, so
// the same failure is the same list however the forge orders it.
func sortedJobs(names, ids []string) ([]string, []string) {
	idx := make([]int, len(names))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return names[idx[a]] < names[idx[b]] })
	n, d := make([]string, len(names)), make([]string, len(ids))
	for i, j := range idx {
		n[i], d[i] = names[j], ids[j]
	}
	return n, d
}

// logStamp is the time a CI log line starts with, ansiEscape a terminal
// colour code, and logSection GitLab's section marker, with its clock.
// causeNoise is what differs between two runs of the same failure: commit
// hashes, clocks and other long numbers.
var (
	logStamp   = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z ?`)
	ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")
	logSection = regexp.MustCompile(`section_(?:start|end):\d+:[A-Za-z0-9_.-]*(?:\[[^\]]*\])?\r?`)
	causeNoise = regexp.MustCompile(`(?i)\b[0-9a-f]{7,40}\b|\d{4,}|\d+(?:\.\d+)?\s*(?:ms|s|m|h)\b`)
)

// ciCause is a failure's cause: its failing jobs and its excerpt, without
// what differs from one run of the same failure to the next.
func ciCause(jobs []string, excerpt string) string {
	return shortHash(StillHash(strings.Join(jobs, "\n") + "\n" + causeNoise.ReplaceAllString(excerpt, "#")))
}

// ciExcerpt is the end of a job's log: its last lines, without their
// timestamps or colour codes, within ciExcerptBytes.
func ciExcerpt(log string) string {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(log, "\r\n", "\n"), "\n"), "\n")
	if len(lines) > ciExcerptLines {
		lines = lines[len(lines)-ciExcerptLines:]
	}
	for i, l := range lines {
		lines[i] = logStamp.ReplaceAllString(logSection.ReplaceAllString(ansiEscape.ReplaceAllString(l, ""), ""), "")
	}
	out := strings.Join(lines, "\n")
	for len(out) > ciExcerptBytes {
		_, rest, ok := strings.Cut(out, "\n")
		if !ok {
			out = out[len(out)-ciExcerptBytes:]
			break
		}
		out = rest
	}
	return strings.ToValidUTF8(out, "")
}

// ciFixes is the fix sessions started for a request, oldest first.
func (t *Track) ciFixes(repo, ref string) []CIFixRecord {
	var out []CIFixRecord
	for _, f := range t.CIFixes {
		if f.Repository == repo && f.Ref == ref {
			out = append(out, f)
		}
	}
	return out
}

// notePushed records the head the delivery after a fix recorded.
func (t *Track) notePushed(key, head string) {
	for i := len(t.CIFixes) - 1; i >= 0; i-- {
		if f := &t.CIFixes[i]; f.Delivered == key && f.Taken {
			if !f.Redelivered {
				// Copied, so a worker's track never shares the record.
				fixes := append([]CIFixRecord(nil), t.CIFixes...)
				fixes[i].Pushed, fixes[i].Redelivered = head, true
				t.CIFixes = fixes
			}
			return
		}
	}
}

// ciStale says whether a request's record is of a head before the one a
// fix's delivery recorded since: until the watcher has recorded that head.
func (t *Track) ciStale(r CIRecord) bool {
	fixes := t.ciFixes(r.Repository, r.Ref)
	if len(fixes) == 0 {
		return false
	}
	f := fixes[len(fixes)-1]
	return f.Taken && !f.Seen
}

// hasRaised says whether an open raised decision has the id.
func (t *Track) hasRaised(id string) bool {
	for _, o := range t.Raised {
		if o.ID == id {
			return true
		}
	}
	return false
}

// pendingCIFix is the fix session a delivery waits for: the latest one
// taken for the delivery key, while the delivery is not recorded again.
func (t *Track) pendingCIFix(key string) (CIFixRecord, bool) {
	if t.Delivered[key] != "" {
		return CIFixRecord{}, false
	}
	for i := len(t.CIFixes) - 1; i >= 0; i-- {
		if f := t.CIFixes[i]; f.Delivered == key {
			return f, f.Taken
		}
	}
	return CIFixRecord{}, false
}

// ciFixFinished says whether a fix session has finished: gone, or its turn
// ended after it was launched, or its screen still for the stall.
func (s *Supervisor) ciFixFinished(t *Track, f CIFixRecord, ev Evidence, now time.Time) bool {
	if !ev.Alive {
		return true
	}
	stall := s.cfg.Policies.Nudge.Stall.Duration
	still := now.Sub(t.HashSince) >= stall && now.Sub(f.At) >= stall
	if tr := ev.Transcript; s.cfg.TranscriptDir != "" && tr.Found && tr.At.After(f.At) {
		return tr.Role == "assistant" && tr.StopReason == "end_turn" || still
	}
	return still
}

// fixCI starts a fix session for the session's request whose pipeline
// failed on its own, when one is due, and says whether it started one. A
// session is relaunched only when it has finished (DONE) or sits idle, so
// no work is cut short. The fixes per request are bounded by the restart
// policy's count in its window, and follow its backoff; the same cause as
// the last fix, or the count used up, raises a decision for the owner
// instead, and the owner's retry allows one more fix.
func (s *Supervisor) fixCI(name string, sess Session, t *Track, ev Evidence, state State, now time.Time) bool {
	if state != Done && state != Idle || t.Limited {
		return false
	}
	pol := s.cfg.Policies.Restart
	keys := make([]string, 0, len(s.st.CI))
	for k, r := range s.st.CI {
		if r.Session == name {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		r := s.st.CI[k]
		if r.State != CIFailed || r.Verdict != CIOwn || t.Delivered[r.Delivered] == "" {
			continue
		}
		fixes := t.ciFixes(r.Repository, r.Ref)
		var last *CIFixRecord
		since := ""
		if len(fixes) > 0 {
			last = &fixes[len(fixes)-1]
			since = last.Transcript
			switch {
			case !last.Taken && last.Head == r.Head && last.Cause == r.Cause:
				continue // shadow mode would have fixed this one
			case t.ciStale(r):
				// The record is of a head before the fix's delivery: the
				// watcher has not seen the head delivered since.
				continue
			}
		}
		n := 0
		for _, f := range fixes {
			if now.Sub(f.At) < pol.Window.Duration {
				n++
			}
		}
		why := ""
		switch {
		case last != nil && last.Cause == r.Cause:
			why = "fails again with the same cause after a fix session"
		case n >= pol.MaxRestarts:
			why = fmt.Sprintf("has used its %d fix sessions in %s", pol.MaxRestarts, pol.Window.Duration)
		}
		retry := ""
		if why != "" {
			// One decision per failure after each fix: a retry that meets
			// the same failure asks again.
			rkey := "ci-raise:" + k + ":" + r.Cause + ":" + since
			id := t.CIRaised[rkey]
			if id == "" {
				s.raiseCI(name, t, r, rkey, why)
				continue
			}
			a, ok := t.Answers[id]
			if !ok || a.Choice != ciRetry || last != nil && last.Decision == id {
				continue
			}
			retry = id
		} else if n > 0 && now.Sub(last.At) < backoff(n, pol) {
			continue
		}
		if !pol.Enabled {
			s.skipOnce(name, "ci-fix:"+name, "restart policy disabled; not starting a CI fix session", t, nil)
			return false
		}
		return s.startCIFix(name, sess, t, ev, r, len(fixes)+1, retry)
	}
	return false
}

// startCIFix relaunches the session with the failure in its pack.
func (s *Supervisor) startCIFix(name string, sess Session, t *Track, ev Evidence, r CIRecord, attempt int, retry string) bool {
	id := newSessionID()
	fact := &CIFact{Request: r.Ref, URL: r.URL, Head: r.Head, Target: r.Target, Jobs: r.Jobs, Excerpt: r.Excerpt}
	cmd, facts := s.launchCommandWith(name, sess, t, id, "ci-fix", fact)
	line, err := s.launchLine(sess, cmd)
	if err != nil {
		s.skipOnce(name, "account:"+name, "not starting a CI fix session: "+err.Error(), t, &SkipPayload{Account: sess.Account})
		return false
	}
	key := "ci-fix:" + name + ":" + r.Ref + ":" + r.Head + ":" + r.Cause
	err = s.act(name, ActCIFix, &ActPayload{Key: key, Attempt: attempt, Cwd: sess.Cwd, Respawn: ev.PaneDead, Account: sess.Account, Command: line, Transcript: id,
		Repository: r.Repository, Ref: r.Ref, URL: r.URL, Head: r.Head, Delivered: r.Delivered, Cause: r.Cause, Decision: retry}, func() error {
		switch {
		case ev.Alive:
			return s.tmux.Replace(name, sess.Cwd, line)
		case ev.PaneDead:
			return s.tmux.Respawn(name, sess.Cwd, line)
		}
		return s.tmux.NewSession(name, sess.Cwd, line, s.cfg.Policies.Restart.Width, s.cfg.Policies.Restart.Height)
	})
	if err != nil || s.halt != nil || s.shadow {
		return false
	}
	s.notePack(name, sess, facts)
	return true
}

// raiseCI raises the decision a repeated failure becomes, on the session
// that delivered the request: retry with one more fix session, or leave
// the request to the owner.
func (s *Supervisor) raiseCI(name string, t *Track, r CIRecord, key, why string) {
	id := fmt.Sprintf("C%d", len(t.CIRaised)+1)
	jobs := strings.Join(r.Jobs, ", ")
	d := Decision{
		ID:             id,
		Type:           DecisionChooseOne,
		Question:       cutUTF8(fmt.Sprintf("%s's request %s in %s %s (failing: %s). Start one more fix session?", name, r.Ref, r.Repository, why, jobs), maxQuestion),
		Context:        cutUTF8(fmt.Sprintf("Head %s, target %s. %s", shortHash(r.Head), r.Target, r.URL), maxQuestion),
		Why:            "agentd does not start a fix session for a failure a fix session already met, or past the restart policy's count.",
		IfWrong:        "Another fix session spends tokens on the same failure, or the request waits for the owner.",
		Recommendation: ciLeave,
		Options: []DecisionOption{
			{Key: ciRetry, Label: "Retry", Consequence: "agentd starts one more fix session with the failing job's log."},
			{Key: ciLeave, Label: "Leave it", Consequence: "agentd starts no fix session for this failure; the owner looks at the request."},
		},
	}
	s.act(name, ActRaise, &ActPayload{Key: key, Repository: r.Repository, Ref: r.Ref, Cause: r.Cause, Raised: &d}, func() error { return nil })
}
