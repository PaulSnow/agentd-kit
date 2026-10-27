package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// listAPI is a forge whose GitHub issue list is body, whatever its since;
// it counts the lists.
type listAPI struct {
	mu    sync.Mutex
	body  string
	lists int
}

func (a *listAPI) GitHub(path string, _ map[string]string) (APIResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !strings.HasPrefix(path, "/repos/example-org/widgets/issues?state=all&sort=updated&direction=desc&since=") {
		return APIResponse{Status: http.StatusNotFound, Header: http.Header{}}, nil
	}
	a.lists++
	return APIResponse{Status: http.StatusOK, Header: http.Header{}, Body: []byte(a.body)}, nil
}
func (a *listAPI) GitLab(string) (APIResponse, error) {
	return APIResponse{Status: http.StatusNotFound}, nil
}
func (a *listAPI) At(string) ForgeAPI { return a }

func (a *listAPI) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lists
}

// githubWidgets makes the harness's widgets a GitHub repository listed by
// a, the supervisor restarted on it.
func githubWidgets(h *harness, a *listAPI) {
	w := h.cfg.Repositories["widgets"]
	w.Forge, w.Host = "github", ""
	h.cfg.Repositories["widgets"] = w
	h.restart(false)
	h.sup.api = a
}

// The owner asks whether file attachments are being added, a minute after
// the issue for them was filed, while a session whose card still tells its
// last job builds it (#62). The agent is given, read as it starts, the
// issue from one forge list and what the session's STATUS.md says now, and
// told to say how fresh each source is and never to assert an absence. A
// second thread within a minute is given the same list, with no call.
func TestThreadAgentGivenFreshFacts(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	api := &listAPI{}
	githubWidgets(h, api)
	w1 := h.startSession("issue-60", KindWork, 60)
	h.ticks(2, time.Minute)
	work := h.sup.cfg.Sessions[w1].Workdir
	writeFile(t, filepath.Join(work, "BRIEF.md"), "# #61: attach screenshots, PDFs and files to a message\n\nBuild #61.\n")
	writeFile(t, filepath.Join(work, "STATUS.md"), "# #61 attachments\n\n## Now\n- Building #61: the upload handler and its size limit.\n\n## Questions\nnone\n")
	filed := h.clock.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	api.body = fmt.Sprintf(`[{"number":61,"title":"Console: attach screenshots, PDFs and files to a message","state":"open","created_at":%q,"updated_at":%q},`+
		`{"number":60,"title":"Issues carry their own context","state":"closed","created_at":"2026-10-01T00:00:00Z","updated_at":%q,"closed_at":%q,"pull_request":{"merged_at":%q}}]`,
		filed, filed, filed, filed, filed)
	claudeAnswers(t, dir, 0, agentOut("Yes: #61 was filed a minute ago, and "+w1+" is building it.", ""))
	if err := reply(h, "widgets", "Are we adding support for attaching files?"); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, time.Second)
	x := agentInput(t, dir, 1)
	var issue *LiveIssue
	for i := range x.Issues {
		if x.Issues[i].Ref == "#61" {
			issue = &x.Issues[i]
		}
	}
	if issue == nil || issue.State != "open" || !strings.Contains(issue.Title, "attach") || x.IssuesAt == nil || x.IssuesError != "" {
		t.Fatalf("the agent is not given the issue filed a minute ago: %+v", x)
	}
	if len(x.Issues) != 2 || x.Issues[1].Ref != "!60" || x.Issues[1].State != "merged" {
		t.Fatalf("a merged request is not listed as one: %+v", x.Issues)
	}
	var running *threadRunning
	for i := range x.Running {
		if x.Running[i].Name == w1 {
			running = &x.Running[i]
		}
	}
	if running == nil || !strings.Contains(running.Now, "Building #61") || !strings.HasPrefix(running.Brief, "#61: attach") || running.StatusAt == nil {
		t.Fatalf("the agent is not given what the session's files say now: %+v", x.Running)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args.1"))
	for _, want := range []string{"Never state that something does not exist", "how fresh each was", "trust the live facts", `"missing-from-state"`} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("the agent's instructions lack %q", want)
		}
	}
	if api.count() != 1 {
		t.Fatalf("%d lists for one thread", api.count())
	}
	// Another thread within the minute: the same list, no second call.
	if err := reply(h, "widgets", "And who builds it?"); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, time.Second)
	if x := agentInput(t, dir, 2); len(x.Issues) != 2 || api.count() != 1 {
		t.Fatalf("the second thread's list: %d issues, %d calls", len(x.Issues), api.count())
	}
	// The issue's opening and the session's brief and Now are facts, on
	// the issue's topic and the session's, which call their curators; a
	// Now once in nowGap.
	h.clock.advance(nowGap)
	h.ticks(1, time.Second)
	hist := NewContextStore(h.cfg)
	for _, e := range h.events() {
		hist.Apply(e)
	}
	var opened, now bool
	for _, r := range hist.History("widgets").OnTopic("#61") {
		opened = opened || r.Kind == HistFact && strings.Contains(r.Title, "#61 opened")
		now = now || r.Kind == HistFact && strings.Contains(r.Text, "STATUS.md Now: - Building #61")
	}
	if !opened || !now || !contains(curatedKinds, HistFact) {
		t.Fatalf("#61's topic lacks its opening or the session's Now: opened %v, now %v", opened, now)
	}
	merged := false
	for _, r := range hist.History("widgets").OnTopic("!60") {
		merged = merged || r.Kind == HistFact && r.Text == "!60 is merged: Issues carry their own context"
	}
	if !merged {
		t.Fatal("a request merged within the day is no fact on its topic")
	}
}

// A session's run, brief and Now are facts when they change, and only
// then; a shadow run logs none.
func TestSessionFacts(t *testing.T) {
	h := sessionsHarness(t)
	w1 := h.startSession("issue-7", KindWork, 7)
	h.ticks(2, time.Minute)
	work := h.sup.cfg.Sessions[w1].Workdir
	writeFile(t, filepath.Join(work, "STATUS.md"), "# spin\n\n## Now\nwriting the test\n\n## Questions\nnone\n")
	h.ticks(5, time.Minute)
	// Rewritten twice within nowGap: its latest is the one fact.
	writeFile(t, filepath.Join(work, "STATUS.md"), "# spin\n\n## Now\nwriting it\n\n## Questions\nnone\n")
	h.ticks(1, time.Minute)
	writeFile(t, filepath.Join(work, "STATUS.md"), "# spin\n\n## Now\nwriting the code\n\n## Questions\nnone\n")
	h.ticks(5, time.Minute)
	var changes, nows []string
	for _, e := range h.events() {
		if e.Fact != nil && e.Fact.Session == w1 {
			changes = append(changes, e.Fact.Change)
			if e.Fact.Change == FactNow {
				nows = append(nows, e.Fact.Now)
			}
		}
	}
	// The first Now is the template's STATUS.md, written at the start.
	if got := strings.Join(changes, ","); got != "started,brief,now,now,now" || nows[1] != "writing the test" || nows[2] != "writing the code" {
		t.Fatalf("facts %s, Nows %q", got, nows)
	}
	// It finishes: a fact; its Now is no longer noted.
	writeFile(t, filepath.Join(work, "STATUS.md"), "# spin\n\n## Now\nmerged\n\nDONE\n")
	h.ticks(3, time.Minute)
	var last []string
	for _, e := range h.events() {
		if e.Fact != nil && e.Fact.Session == w1 {
			last = append(last, e.Fact.Change)
		}
	}
	if got := strings.Join(last[5:], ","); got != "finished" {
		t.Fatalf("after DONE, facts %s", got)
	}
	// Another, ended by the owner while it runs: it stopped.
	w2 := h.startSession("issue-8", KindWork, 8)
	h.ticks(2, time.Minute)
	if err := h.endSession(w2); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, time.Minute)
	var ended []string
	for _, e := range h.events() {
		if e.Fact != nil && e.Fact.Session == w2 && (e.Fact.Change == FactStarted || e.Fact.Change == FactStopped) {
			ended = append(ended, e.Fact.Change)
		}
	}
	if got := strings.Join(ended, ","); got != "started,stopped" {
		t.Fatalf("an ended session's facts: %s", got)
	}
	h.restart(true)
	writeFile(t, filepath.Join(work, "STATUS.md"), "# spin\n\n## Now\nshadowed\n")
	n := len(h.events())
	h.ticks(2, time.Minute)
	for _, e := range h.events()[n:] {
		if e.Fact != nil {
			t.Fatalf("a shadow run logged a fact: %+v", e.Fact)
		}
	}
}

// STATUS.md's Now is its Now section, to the next heading of its level,
// cut; with none, the file's top.
func TestStatusNow(t *testing.T) {
	if got := statusNow("# t\n\n## Now\n- a\n### detail\n- b\n## Next\n- c\n"); got != "- a\n### detail\n- b" {
		t.Fatalf("Now = %q", got)
	}
	if got := statusNow("# t\nworking\n"); got != "# t\nworking" {
		t.Fatalf("no Now = %q", got)
	}
	if got := statusNow("## Now\n" + strings.Repeat("x", 2000)); len(got) > liveNow+len("…") {
		t.Fatalf("Now is %d bytes", len(got))
	}
	if got := briefHeading("\n# agentd-kit #62: fresh facts\n\nbody"); got != "agentd-kit #62: fresh facts" {
		t.Fatalf("heading = %q", got)
	}
}

// An issue's state is a fact when it changes; one first seen is a fact
// only when it opened or closed within the list's span.
func TestIssueFacts(t *testing.T) {
	now := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	list := []LiveIssue{
		{Ref: "#1", State: "open", created: now.Add(-time.Minute)},
		{Ref: "#2", State: "open", created: now.Add(-72 * time.Hour)},
		{Ref: "#3", State: "closed", created: now.Add(-72 * time.Hour), closed: now.Add(-time.Hour)},
		{Ref: "#4", State: "closed"},
		{Ref: "#5", State: "open"},
		{Ref: "#6", State: "open"},
	}
	known := map[string]string{"widgets#4": "open", "widgets#5": "closed", "widgets#6": "open"}
	var got []string
	for _, p := range issueFacts("widgets", list, known, now) {
		got = append(got, p.Ref+":"+p.Change)
	}
	if s := strings.Join(got, ","); s != "#1:opened,#2:seen,#3:closed,#4:closed,#5:opened" {
		t.Fatalf("facts %s", s)
	}
}

// An input over its bound drops issues, then the thread's records, and
// last cuts the sessions' Now.
func TestThreadInputFitCutsNowLast(t *testing.T) {
	x := threadInput{Message: threadRecord{Text: "q"}, Running: []threadRunning{{Name: "w", Now: strings.Repeat("n", 600)}},
		Earlier: []threadRecord{{Text: strings.Repeat("e", 300)}}, Issues: []LiveIssue{{Ref: "#1", Title: strings.Repeat("t", 150)}}}
	b, err := x.fit(400)
	if err != nil || len(b) > 400 || strings.Contains(string(b), `"earlier"`) || strings.Contains(string(b), `"issues"`) || !strings.Contains(string(b), `"now":"nnn`) {
		t.Fatalf("fit: %v %s", err, b)
	}
	if len(x.Running[0].Now) != 600 {
		t.Fatal("fit cut the caller's input")
	}
}
