package main

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ciHarness is a harness whose session d is DONE with a delivery to
// widgets, a GitLab project, and whose CI is watched through a fake API:
// the request agentd opens is !1, with head c0ffee, onto main.
func ciHarness(t *testing.T) (*harness, *fakeForgeAPI) {
	t.Helper()
	h := newHarness(t, false, "d")
	doneWithDelivery(t, h, "d")
	h.cfg.Poll.CI = true
	api := &fakeForgeAPI{gitlab: map[string]string{}}
	h.sup.api = api
	return h, api
}

const ciProject = "/projects/example-org%2Fwidgets"

// pipelines sets the request's pipeline for head and its jobs, and the
// target's latest pipeline and its jobs; a job is "name:status".
func (f *fakeForgeAPI) pipelines(head, state string, jobs []string, target string, targetJobs []string) {
	f.set(func() {
		f.gitlab[ciProject+"/merge_requests/1"] = `{"iid":1,"state":"opened","title":"Add the feature","description":"The MR description.","web_url":"https://gitlab.example.com/example-org/widgets/-/merge_requests/1","sha":"` + head +
			`","target_branch":"main","head_pipeline":{"id":50,"status":"` + state + `","sha":"` + head + `","web_url":"https://gitlab.example.com/example-org/widgets/-/pipelines/50"}}`
		f.gitlab[ciProject+"/pipelines/50/jobs"] = gitlabJobs(500, jobs)
		f.gitlab[ciProject+"/pipelines|ref=main"] = `[{"id":40,"status":"` + target + `","sha":"badbeef1234","web_url":"https://gitlab.example.com/example-org/widgets/-/pipelines/40"}]`
		f.gitlab[ciProject+"/pipelines/40/jobs"] = gitlabJobs(400, targetJobs)
		f.gitlab[ciProject+"/jobs/501/trace"] = "2026-10-22T00:00:01.000Z $ go test ./...\n2026-10-22T00:00:02.000Z --- FAIL: TestWidgets (0.01s)\n2026-10-22T00:00:02.100Z FAIL\n"
	})
}

func gitlabJobs(first int, jobs []string) string {
	var parts []string
	for i, j := range jobs {
		name, status, _ := strings.Cut(j, ":")
		parts = append(parts, `{"id":`+strconv.Itoa(first+1+i)+`,"name":"`+name+`","status":"`+status+`"}`)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// ciEvents is the request's ci events, oldest first.
func (h *harness) ciEvents() []CIPayload {
	var out []CIPayload
	for _, e := range h.events() {
		if e.Kind == KindCI {
			out = append(out, *e.CI)
		}
	}
	return out
}

// A failure that is the branch's own starts one fix session, through the
// restart policy's command, handed the failing job's log excerpt in its
// pack; later polls of the same failure start no other.
func TestCIBranchFailureStartsOneFix(t *testing.T) {
	h, api := ciHarness(t)
	api.pipelines("c0ffee", "failed", []string{"test:failed", "lint:success"}, "success", []string{"test:success", "lint:success"})
	h.ticks(6, 5*time.Minute)
	if len(h.forge.opens) != 1 {
		t.Fatalf("opens = %d, want 1", len(h.forge.opens))
	}
	if n := h.count(KindAction, ActCIFix, "d"); n != 1 {
		t.Fatalf("ci-fix actions = %d, want 1", n)
	}
	var fix *ActPayload
	for _, e := range h.events() {
		if e.Kind == KindAction && actionOf(e) == ActCIFix {
			fix = e.Act
		}
	}
	if fix.Ref != "!1" || fix.Head != "c0ffee" || fix.Cause == "" || !strings.Contains(fix.Command, `"why":"ci-fix"`) ||
		!strings.Contains(fix.Command, "--- FAIL: TestWidgets") || strings.Contains(fix.Command, "2026-10-22T00:00:02") {
		t.Fatalf("fix = %+v", fix)
	}
	if !strings.Contains(fix.Command, "Fix it on the same branch") {
		t.Fatalf("fix command lacks the CI pointer: %s", fix.Command)
	}
	evs := h.ciEvents()
	if len(evs) == 0 {
		t.Fatal("no ci events")
	}
	last := evs[len(evs)-1]
	if last.State != CIFailed || last.Verdict != CIOwn || len(last.Jobs) != 1 || last.Jobs[0] != "test" || last.TargetState != CISuccess {
		t.Fatalf("ci = %+v", last)
	}
	// One event for the failure, however often it is polled.
	n := 0
	for _, e := range evs {
		if e.State == CIFailed {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("failed ci events = %d, want 1: %+v", n, evs)
	}
	// The fix cleared the delivery: it waits for the fix session.
	if tr := h.sup.Tracks()["d"]; tr.Delivered["d:"+h.dir+"/d/wt"] != "" {
		t.Fatalf("delivery still recorded: %v", tr.Delivered)
	}
}

// When the target branch is itself red with the same job, the request
// waits: no fix session.
func TestCIRedTargetWaits(t *testing.T) {
	h, api := ciHarness(t)
	api.pipelines("c0ffee", "failed", []string{"test:failed"}, "failed", []string{"test:failed"})
	h.ticks(6, 5*time.Minute)
	if n := h.count(KindAction, ActCIFix, ""); n != 0 {
		t.Fatalf("ci-fix actions = %d, want 0", n)
	}
	evs := h.ciEvents()
	if last := evs[len(evs)-1]; last.Verdict != CITargetRed || last.TargetState != CIFailed {
		t.Fatalf("ci = %+v", last)
	}
	// The target turns green: the request's failure is now its own.
	api.pipelines("c0ffee", "failed", []string{"test:failed"}, "success", []string{"test:success"})
	h.ticks(3, 5*time.Minute)
	if n := h.count(KindAction, ActCIFix, ""); n != 1 {
		t.Fatalf("ci-fix actions after the target turned green = %d, want 1", n)
	}
}

// A request that fixes the red target is not parked waiting for it: it
// passes a job the target fails, so what it fails is its own.
func TestCITargetFixIsNotParked(t *testing.T) {
	h, api := ciHarness(t)
	api.pipelines("c0ffee", "failed", []string{"test:failed", "lint:success"}, "failed", []string{"test:failed", "lint:failed"})
	h.ticks(4, 5*time.Minute)
	if n := h.count(KindAction, ActCIFix, "d"); n != 1 {
		t.Fatalf("ci-fix actions = %d, want 1", n)
	}
}

// A request whose text names the target's red commit is its fix too.
func TestCIRequestNamingTheRedCommitIsTheFix(t *testing.T) {
	h, api := ciHarness(t)
	api.pipelines("c0ffee", "failed", []string{"test:failed"}, "failed", []string{"test:failed"})
	api.set(func() {
		api.gitlab[ciProject+"/merge_requests/1"] = strings.Replace(api.gitlab[ciProject+"/merge_requests/1"], "The MR description.", "Fixes the build broken at badbeef1.", 1)
	})
	h.ticks(4, 5*time.Minute)
	if n := h.count(KindAction, ActCIFix, "d"); n != 1 {
		t.Fatalf("ci-fix actions = %d, want 1", n)
	}
}

// The fix session's new commit goes to the same request once it has
// finished; the same failure again becomes a decision for the owner, not
// a second fix session.
func TestCISameFailureTwiceIsADecision(t *testing.T) {
	h, api := ciHarness(t)
	api.pipelines("c0ffee", "failed", []string{"test:failed"}, "success", []string{"test:success"})
	h.ticks(4, 5*time.Minute)
	if n := h.count(KindAction, ActCIFix, "d"); n != 1 {
		t.Fatalf("ci-fix actions = %d, want 1", n)
	}
	pushes := len(h.forge.pushes)
	// The fix session commits and its screen goes still.
	h.git.head = "c0ffee2"
	h.alive("d", "fixed\n")
	h.ticks(8, 5*time.Minute)
	if len(h.forge.pushes) != pushes+1 || h.forge.pushedSHAs[len(h.forge.pushedSHAs)-1] != "c0ffee2" {
		t.Fatalf("pushes = %v, want the fix pushed once", h.forge.pushedSHAs)
	}
	if len(h.forge.opens) != 1 {
		t.Fatalf("opens = %d: the fix went to a new request", len(h.forge.opens))
	}
	// The new head fails the same way.
	api.pipelines("c0ffee2", "failed", []string{"test:failed"}, "success", []string{"test:success"})
	h.ticks(6, 5*time.Minute)
	if n := h.count(KindAction, ActCIFix, "d"); n != 1 {
		t.Fatalf("ci-fix actions = %d, want still 1", n)
	}
	tr := h.sup.Tracks()["d"]
	var d *Decision
	for _, o := range tr.Raised {
		if o.ID == "C1" {
			o := o
			d = &o
		}
	}
	if d == nil || d.Type != DecisionChooseOne || len(d.Options) != 2 || !strings.Contains(d.Question, "!1") {
		t.Fatalf("raised = %+v", tr.Raised)
	}
	if n := h.count(KindAction, ActRaise, "d"); n != 1 {
		t.Fatalf("raise actions = %d, want 1", n)
	}
	// The owner says retry: one more fix session.
	if err := RecordAnswer(h.sup.log, h.sup.st, h.clock.Now(), "d", AnswerPayload{ID: "C1", Asked: decisionHash(*d), Choice: "retry"}); err != nil {
		t.Fatal(err)
	}
	h.ticks(4, 5*time.Minute)
	if n := h.count(KindAction, ActCIFix, "d"); n != 2 {
		t.Fatalf("ci-fix actions after retry = %d, want 2", n)
	}
}

// A forge that cannot be reached is unknown, never green, and starts
// nothing.
func TestCIUnreachableForgeIsUnknown(t *testing.T) {
	h, api := ciHarness(t)
	api.set(func() { api.gitlabErr = errors.New("glab api: connection refused") })
	h.ticks(4, 5*time.Minute)
	evs := h.ciEvents()
	if len(evs) != 1 || evs[0].State != CIUnknown || evs[0].Error == "" || evs[0].Ref != "!1" {
		t.Fatalf("ci events = %+v", evs)
	}
	if n := h.count(KindAction, ActCIFix, ""); n != 0 {
		t.Fatalf("ci-fix actions = %d", n)
	}
	if r := h.sup.st.CI[ciKey("widgets", "!1")]; r.State != CIUnknown {
		t.Fatalf("ci record = %+v", r)
	}
}

// Status, the report the page is built from, the page and the case show
// each request's CI.
func TestCIShownOnStatusReportPageAndCase(t *testing.T) {
	h, api := ciHarness(t)
	s := h.cfg.Sessions["d"]
	s.Issue = 7
	h.cfg.Sessions["d"] = s
	api.pipelines("c0ffee", "failed", []string{"test:failed"}, "failed", []string{"test:failed"})
	h.ticks(3, 5*time.Minute)
	var out bytes.Buffer
	rows, meta := SummarizeState(h.cfg, h.sup.st)
	PrintStatus(&out, rows, meta, h.clock.now)
	if !strings.Contains(out.String(), "ci widgets !1 (d): failed (target-red: test)") {
		t.Fatalf("status:\n%s", out.String())
	}
	rep := ReportFrom(h.cfg, h.sup.st, "host-a", h.clock.now)
	var got *ReportDelivery
	for _, rr := range rep.Repositories {
		for _, d := range rr.Deliveries {
			d := d
			got = &d
		}
	}
	if got == nil || got.CI != CIFailed || got.CIVerdict != CITargetRed || len(got.CIJobs) != 1 {
		t.Fatalf("report delivery = %+v", got)
	}
	data := testPageData()
	data.Report = rep
	p, err := NewPage(PageConfig{Address: pageAddr, Whois: []string{"whois", "{addr}"}, Owner: "owner-a"},
		&fakeWhois{logins: map[string]string{"192.0.2.20": "owner-a"}}, func() (PageData, error) { return data, nil },
		func([]string, string) error { return nil }, func(string, AnswerPayload) error { return nil }, func() time.Time { return pageAt })
	if err != nil {
		t.Fatal(err)
	}
	if body := get(p, "GET", "/", pageAddr, "192.0.2.20:51000").Body.String(); !strings.Contains(body, "CI failed (target-red) test") {
		t.Fatalf("page lacks the request's CI:\n%s", body)
	}
	var cv *CaseView
	for _, c := range CasesFrom(h.cfg, h.sup.st) {
		if c.Issue == 7 {
			c := c
			cv = &c
		}
	}
	if cv == nil || len(cv.CI) != 1 || cv.CI[0].Ref != "!1" || cv.CI[0].State != CIFailed {
		t.Fatalf("case = %+v", cv)
	}
	sameAfterReplay(t, h, "after the ci events")
}

// GitHub: a pull request's head check runs, the base's, and the failing
// job's log.
func TestCIGitHubPullRequest(t *testing.T) {
	api := &fakeForgeAPI{github: map[string]string{
		"/repos/example-org/gadgets/pulls/4":                                    `{"state":"open","merged":false,"title":"Gadgets","body":"","html_url":"https://github.com/example-org/gadgets/pull/4","head":{"sha":"abc1234def"},"base":{"ref":"main"}}`,
		"/repos/example-org/gadgets/commits/abc1234def/check-runs?per_page=100": `{"check_runs":[{"id":9,"name":"test","status":"completed","conclusion":"failure"},{"id":10,"name":"lint","status":"completed","conclusion":"success"}]}`,
		"/repos/example-org/gadgets/commits/main/check-runs?per_page=100":       `{"check_runs":[{"id":7,"name":"test","status":"completed","conclusion":"success","head_sha":"0123456789"}]}`,
		"/repos/example-org/gadgets/actions/jobs/9/logs":                        "2026-10-22T00:00:01.0000000Z FAIL gadgets\n",
	}}
	c, err := githubCI(api, "example-org/gadgets", "#4")
	if err != nil {
		t.Fatal(err)
	}
	if c.state != CIFailed || c.head != "abc1234def" || c.target != "main" || len(c.failed) != 1 || c.failed[0] != "test" || c.failedIDs[0] != "9" {
		t.Fatalf("check = %+v", c)
	}
	tgt, err := c.targetCI()
	if err != nil || tgt.state != CISuccess || ciVerdict(c, tgt) != CIOwn {
		t.Fatalf("target = %+v, %v", tgt, err)
	}
	log, err := c.jobLog("9")
	if err != nil || ciExcerpt(log) != "FAIL gadgets" {
		t.Fatalf("log = %q, %v", log, err)
	}
	api.github["/repos/example-org/gadgets/pulls/4"] = `{"state":"closed","merged":true}`
	if c, err := githubCI(api, "example-org/gadgets", "#4"); err != nil || c.state != CIMerged {
		t.Fatalf("merged = %+v, %v", c, err)
	}
}

// Shadow mode records the fix it would start, once, and starts nothing.
func TestCIShadowWouldFixOnce(t *testing.T) {
	h := newHarness(t, true, "d")
	doneWithDelivery(t, h, "d")
	h.cfg.Poll.CI = true
	api := &fakeForgeAPI{gitlab: map[string]string{}}
	h.sup.api = api
	// Shadow mode opens nothing: the owner's mark-delivered names the request.
	if err := MarkDelivered(h.cfg, h.sup.log, h.clock.Now(), "d", h.dir+"/d/wt", "!1"); err != nil {
		t.Fatal(err)
	}
	api.pipelines("c0ffee", "failed", []string{"test:failed"}, "success", []string{"test:success"})
	h.ticks(6, 5*time.Minute)
	if n := h.count(KindWould, ActCIFix, "d"); n != 1 {
		t.Fatalf("would ci-fix = %d, want 1", n)
	}
	if n := h.tmux.count("new-session", "d") + h.tmux.count("replace", "d"); n != 0 {
		t.Fatalf("shadow mode started %d sessions", n)
	}
}

// questions.json may not take the ids agentd gives its own decisions.
func TestCIDecisionIDsAreReserved(t *testing.T) {
	err := checkDecisions([]Decision{{ID: "C1", Type: DecisionText, Question: "Which colour?"}})
	if err == nil || !strings.Contains(err.Error(), "C, D or R") {
		t.Fatalf("C1 = %v", err)
	}
}
