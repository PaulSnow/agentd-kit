package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requestsHarness is coordHarness with the CI watcher on and widgets'
// policy merging widget-bot's requests into main as a fast-forward; the
// forge lists one open request, !2, which no session delivered, by
// widget-bot, green on 5eed0101 at the target's tip: due to merge once
// its coordinator, c, approves it.
func requestsHarness(t *testing.T) (*harness, *fakeForgeAPI) {
	t.Helper()
	h := coordHarness(t)
	h.cfg.Poll.CI = true
	api := &fakeForgeAPI{gitlab: map[string]string{}}
	h.sup.api = api
	setMerge(t, h, func(m *MergeRules) {})
	api.set(func() {
		api.gitlab[ciProject+"/merge_requests|state=opened"] = `[{"iid":2}]`
		api.gitlab[ciProject+"/repository/branches/main"] = `{"name":"main","commit":{"id":"` + mergeTip + `"}}`
		api.gitlab[mergePolicyPath] = fakeNotFound
		api.gitlab["="+ciProject] = `{"id":7,"merge_method":"ff"}`
		api.gitlab[ciProject+"/repository/merge_base"] = `{"id":"` + mergeTip + `"}`
		api.gitlab[ciProject+"/pipelines|ref=main"] = `[{"id":40,"status":"success","sha":"badbeef1234"}]`
		api.gitlab[ciProject+"/pipelines/40/jobs"] = `[]`
	})
	outsideMR(api, outsideMRState{head: "5eed0101", pipeline: 60, status: "success"})
	return h, api
}

// outsideMRState is what the forge says of request !2.
type outsideMRState struct {
	head, status, author string
	pipeline             int
	jobs                 []string
	unresolved           bool
}

// outsideMR sets request !2, its pipeline and the pipeline's jobs; a job
// is "name:status", and the first job's log fails a test.
func outsideMR(api *fakeForgeAPI, s outsideMRState) {
	if s.author == "" {
		s.author = "widget-bot"
	}
	id := strconv.Itoa(s.pipeline)
	api.set(func() {
		api.gitlab[ciProject+"/merge_requests/2"] = `{"iid":2,"state":"opened","title":"Fix the squeak","description":"","web_url":"https://gitlab.example.com/example-org/widgets/-/merge_requests/2","sha":"` + s.head +
			`","target_branch":"main","author":{"username":"` + s.author + `"},"blocking_discussions_resolved":` + strconv.FormatBool(!s.unresolved) +
			`,"head_pipeline":{"id":` + id + `,"status":"` + s.status + `","sha":"` + s.head + `"}}`
		api.gitlab[ciProject+"/pipelines/"+id+"/jobs"] = gitlabJobs(s.pipeline*10, s.jobs)
		api.gitlab[ciProject+"/merge_requests/2/discussions"] = `[]`
		api.gitlab[ciProject+"/jobs/"+strconv.Itoa(s.pipeline*10+1)+"/trace"] = "2026-10-22T00:00:02.000Z --- FAIL: TestSqueak (0.01s)\n2026-10-22T00:00:02.100Z FAIL\n"
	})
}

// approve is c's approve call; it returns the reply and whether it was
// refused.
func (h *harness) approve(ref, sha string) (map[string]any, bool) {
	h.t.Helper()
	text, refused := callOne(h.t, h.mcpServerFor("c"), ToolApprove, map[string]any{"request": ref, "sha": sha})
	var reply map[string]any
	if err := json.Unmarshal([]byte(text), &reply); err != nil {
		h.t.Fatalf("reply %q: %v", text, err)
	}
	return reply, refused
}

// A request no session delivered is watched for its coordinator; its own
// red pipeline is due for the coordinator, once, and the pack names the
// failing job with the tail of its log.
func TestOutsideRedPipelineWakesCoordinator(t *testing.T) {
	h, api := requestsHarness(t)
	outsideMR(api, outsideMRState{head: "5eed0101", pipeline: 60, status: "failed", jobs: []string{"test:failed", "lint:success"}})
	h.ticks(3, 5*time.Minute)
	r := h.sup.st.CI[ciKey("widgets", "!2")]
	if r.Session != "c" || r.State != CIFailed || r.Verdict != CIOwn || r.Author != "widget-bot" || r.Run != "60" {
		t.Fatalf("record = %+v", r)
	}
	w := h.wakes()
	if len(w) != 1 || w[0].Reason != "ci: !2 failed at 5eed0101" {
		t.Fatalf("wakes = %+v", w)
	}
	p := h.pack(t)
	if len(p.Requests) != 1 || p.Requests[0].Ref != "!2" || !reflect.DeepEqual(p.Requests[0].Jobs, []string{"test"}) ||
		!strings.Contains(p.Requests[0].Excerpt, "FAIL: TestSqueak") || p.Requests[0].Approved {
		t.Fatalf("pack requests = %+v", p.Requests)
	}
	// The coordinator is never restarted to fix it: it starts a session.
	if n := h.count(KindAction, ActCIFix, "c"); n != 0 {
		t.Fatalf("ci fixes on the coordinator = %d", n)
	}
	h.transcriptOf("c", "t1", summaryLine("c1", "m1", "Started a fix.", h.clock.Now(), 100))
	h.ticks(3, 5*time.Minute)
	// The same failure, though a forge error and a new thread changed
	// its record in between, is not due again.
	api.set(func() { api.gitlabErr = errors.New("connection refused") })
	h.ticks(2, 5*time.Minute)
	api.set(func() { api.gitlabErr = nil })
	outsideMR(api, outsideMRState{head: "5eed0101", pipeline: 60, status: "failed", jobs: []string{"test:failed", "lint:success"}, unresolved: true})
	h.ticks(3, 5*time.Minute)
	if n := len(h.wakes()); n != 1 {
		t.Fatalf("woke again for the same failure: %d", n)
	}
	sameAfterReplay(t, h, "after the red request")
}

// A green request no session delivered merges only once its coordinator
// approves it, at the approved head, by the policy's method.
func TestOutsideApprovedGreenMergesAtApprovedHead(t *testing.T) {
	h, _ := requestsHarness(t)
	h.ticks(3, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merged unapproved: %+v", h.forge.merges)
	}
	if s := h.mergeSkips(); len(s) != 1 || !strings.Contains(s[0], "approved it at 5eed0101") {
		t.Fatalf("skips = %q", s)
	}
	if reply, refused := h.approve("!2", "5eed0101"); refused || reply["sha"] != "5eed0101" {
		t.Fatalf("approve = %v, %v", reply, refused)
	}
	h.ticks(3, 5*time.Minute)
	want := []mergedRequest{{ref: "!2", target: "main", head: "5eed0101", method: MergeFastForward}}
	if !reflect.DeepEqual(h.forge.merges, want) {
		t.Fatalf("merges = %+v, want %+v (skips %q)", h.forge.merges, want, h.mergeSkips())
	}
	if got := h.sup.Tracks()["c"].Merges[ciKey("widgets", "!2")]; got != "5eed0101" {
		t.Fatalf("merges recorded = %v", h.sup.Tracks()["c"].Merges)
	}
	sameAfterReplay(t, h, "after the outside merge")
}

// A push after the approval holds the merge until the new head is
// approved; the old head cannot be approved again.
func TestOutsidePushAfterApprovalNeedsNewApproval(t *testing.T) {
	h, api := requestsHarness(t)
	h.ticks(2, 5*time.Minute)
	if _, refused := h.approve("!2", "5eed0101"); refused {
		t.Fatal("approve refused")
	}
	outsideMR(api, outsideMRState{head: "5eed0202", pipeline: 61, status: "success"})
	h.ticks(3, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merged a head never approved: %+v", h.forge.merges)
	}
	if reply, refused := h.approve("!2", "5eed0101"); !refused || !strings.Contains(reply["refused"].(string), "head is 5eed0202") {
		t.Fatalf("approve of the old head = %v, %v", reply, refused)
	}
	if _, refused := h.approve("!2", "5eed0202"); refused {
		t.Fatal("approve of the new head refused")
	}
	h.ticks(3, 5*time.Minute)
	if len(h.forge.merges) != 1 || h.forge.merges[0].head != "5eed0202" {
		t.Fatalf("merges = %+v (skips %q)", h.forge.merges, h.mergeSkips())
	}
}

// An open discussion holds an approved, green request.
func TestOutsideOpenThreadHoldsMerge(t *testing.T) {
	h, api := requestsHarness(t)
	outsideMR(api, outsideMRState{head: "5eed0101", pipeline: 60, status: "success", unresolved: true})
	h.ticks(2, 5*time.Minute)
	h.approve("!2", "5eed0101")
	h.ticks(3, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merged with an open thread: %+v", h.forge.merges)
	}
	if s := h.mergeSkips(); len(s) == 0 || !strings.Contains(s[len(s)-1], "open discussion") {
		t.Fatalf("skips = %q", s)
	}
	if p, _ := h.sup.buildPack("widgets", nil, h.clock.Now(), true); len(p.Requests) != 1 || !p.Requests[0].Unresolved || !p.Requests[0].Approved {
		t.Fatalf("pack requests = %+v", p.Requests)
	}
	outsideMR(api, outsideMRState{head: "5eed0101", pipeline: 60, status: "success"})
	h.ticks(3, 5*time.Minute)
	if len(h.forge.merges) != 1 {
		t.Fatalf("merges after the thread resolved = %+v (skips %q)", h.forge.merges, h.mergeSkips())
	}
}

// With retest_on_base_move, a request green on a base the target has
// since moved from is retested first, once, and merges when the new run
// is green on the new tip.
func TestOutsideMovedBaseRetestedBeforeMerge(t *testing.T) {
	h, api := requestsHarness(t)
	repo := h.cfg.Repositories["widgets"]
	repo.Policy.CI.RetestOnBaseMove = true
	h.cfg.Repositories["widgets"] = repo
	h.ticks(2, 5*time.Minute)
	if r := h.sup.st.CI[ciKey("widgets", "!2")]; r.Base != mergeTip {
		t.Fatalf("base = %q", r.Base)
	}
	h.approve("!2", "5eed0101")
	const moved = "abad1dea"
	api.set(func() {
		api.gitlab[ciProject+"/repository/branches/main"] = `{"name":"main","commit":{"id":"` + moved + `"}}`
		api.gitlab[ciProject+"/repository/merge_base"] = `{"id":"` + moved + `"}`
	})
	h.ticks(3, 5*time.Minute)
	if len(h.forge.merges) != 0 || !reflect.DeepEqual(h.forge.retests, []string{"!2"}) {
		t.Fatalf("merges %+v, retests %v", h.forge.merges, h.forge.retests)
	}
	// The new pipeline runs, then passes on the new tip.
	outsideMR(api, outsideMRState{head: "5eed0101", pipeline: 62, status: "running"})
	h.ticks(2, 5*time.Minute)
	outsideMR(api, outsideMRState{head: "5eed0101", pipeline: 62, status: "success"})
	h.ticks(3, 5*time.Minute)
	if r := h.sup.st.CI[ciKey("widgets", "!2")]; r.Base != moved {
		t.Fatalf("base = %q, want %s", r.Base, moved)
	}
	if len(h.forge.merges) != 1 || len(h.forge.retests) != 1 {
		t.Fatalf("merges %+v, retests %v (skips %q)", h.forge.merges, h.forge.retests, h.mergeSkips())
	}
	sameAfterReplay(t, h, "after the retest")
}

// GitLab answers a retest of a project whose CI rules make no
// merge-request pipelines with an empty pipeline. agentd asks once, then
// escalates once, saying so and what would unstick the request, instead
// of retrying and stopping silently (#46).
func TestEmptyRetestEscalatesOnce(t *testing.T) {
	h, api := requestsHarness(t)
	repo := h.cfg.Repositories["widgets"]
	repo.Policy.CI.RetestOnBaseMove = true
	h.cfg.Repositories["widgets"] = repo
	h.ticks(2, 5*time.Minute)
	h.approve("!2", "5eed0101")
	h.forge.retestErr = fmt.Errorf("glab: %w", fmt.Errorf("%s (HTTP 400)", "The resulting pipeline would have been empty. Review the rules configuration for the relevant jobs."))
	const moved = "abad1dea"
	api.set(func() {
		api.gitlab[ciProject+"/repository/branches/main"] = `{"name":"main","commit":{"id":"` + moved + `"}}`
		api.gitlab[ciProject+"/repository/merge_base"] = `{"id":"` + moved + `"}`
	})
	h.ticks(8, 5*time.Minute)
	if len(h.forge.merges) != 0 || len(h.forge.retests) != 1 {
		t.Fatalf("merges %+v, retests %v", h.forge.merges, h.forge.retests)
	}
	var esc []string
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActEscalate {
			esc = append(esc, e.Act.Message)
		}
	}
	if len(esc) != 1 || !strings.Contains(esc[0], "no merge-request pipelines") || !strings.Contains(esc[0], "merge main into its branch") {
		t.Fatalf("escalations = %q", esc)
	}
	if s := h.mergeSkips(); len(s) == 0 || !strings.Contains(s[len(s)-1], "would be empty") {
		t.Fatalf("skips = %q", s)
	}
	sameAfterReplay(t, h, "after the empty retest")
	h.restart(false)
	h.sup.api = api
	h.ticks(3, 5*time.Minute)
	if len(h.forge.retests) != 1 {
		t.Fatalf("retests after restart = %v", h.forge.retests)
	}
	// The target moves again: the forge is asked once more, in case its
	// rules changed, and the owner is not told twice about the same head.
	api.set(func() {
		api.gitlab[ciProject+"/repository/branches/main"] = `{"name":"main","commit":{"id":"abad1deb"}}`
		api.gitlab[ciProject+"/repository/merge_base"] = `{"id":"abad1deb"}`
	})
	h.ticks(6, 5*time.Minute)
	if len(h.forge.retests) != 2 || h.count(KindAction, ActEscalate, "") != 1 {
		t.Fatalf("retests %v, escalations %d, skips %q", h.forge.retests, h.count(KindAction, ActEscalate, ""), h.mergeSkips())
	}
}

// A request by an author the policy does not list is watched and in the
// pack, approved or not, and never merged.
func TestOutsideOtherAuthorNeverMerged(t *testing.T) {
	h, api := requestsHarness(t)
	outsideMR(api, outsideMRState{head: "5eed0101", pipeline: 60, status: "success", author: "someone-else"})
	h.ticks(2, 5*time.Minute)
	h.approve("!2", "5eed0101")
	h.ticks(4, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merged another author's request: %+v", h.forge.merges)
	}
	s := h.mergeSkips()
	if len(s) != 2 || !strings.Contains(s[1], "author someone-else") {
		t.Fatalf("skips = %q", s)
	}
	if p, _ := h.sup.buildPack("widgets", nil, h.clock.Now(), true); len(p.Requests) != 1 || p.Requests[0].Author != "someone-else" {
		t.Fatalf("pack requests = %+v", p.Requests)
	}
}

// approve takes only a recorded request of the coordinator's own
// repository at its head, whole or by a prefix of 7 or more.
func TestApproveRefusals(t *testing.T) {
	h, _ := requestsHarness(t)
	if reply, refused := h.approve("!2", "5eed0101"); !refused || !strings.Contains(reply["refused"].(string), "no record") {
		t.Fatalf("approve before a record = %v", reply)
	}
	h.ticks(2, 5*time.Minute)
	for _, c := range [][2]string{{"2", "5eed0101"}, {"!2", "5eed"}, {"!2", "5EED0101"}, {"!9", "5eed0101"}, {"!2", "5eed01011"}} {
		if reply, refused := h.approve(c[0], c[1]); !refused {
			t.Errorf("approve %v = %v", c, reply)
		}
	}
	if n := len(h.sup.st.Approvals); n != 0 {
		t.Fatalf("approvals logged by refused calls: %d", n)
	}
	// An approval only the repository's coordinator logged counts.
	h.sup.emit("a", Event{Kind: KindApproval, Mode: ModeShared, Approval: &ApprovalPayload{Repository: "widgets", Ref: "!2", SHA: "5eed0101"}})
	h.ticks(1, 5*time.Minute)
	if n := len(h.sup.st.Approvals); n != 0 {
		t.Fatalf("a worker's approval counted: %v", h.sup.st.Approvals)
	}
}

// ci.required requires green CI of a delivered request whatever
// merge.require names.
func TestCIRequiredAddsCIToRequire(t *testing.T) {
	h, api := mergeHarness(t)
	setMerge(t, h, func(m *MergeRules) { m.Require = []string{RequireGate} })
	repo := h.cfg.Repositories["widgets"]
	repo.Policy.CI.Required = true
	h.cfg.Repositories["widgets"] = repo
	api.pipelines("c0ffee", "canceled", nil, "success", nil)
	h.ticks(4, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merged without green CI: %+v", h.forge.merges)
	}
	if s := h.mergeSkips(); len(s) != 1 || !strings.Contains(s[0], "not green") {
		t.Fatalf("skips = %q", s)
	}
}

// On GitLab a resolvable thread not resolved is an open discussion where
// the project does not require threads resolved; one opened after the
// watcher's round is read again just before the merge.
func TestOutsideUnresolvedThreadReadAtMerge(t *testing.T) {
	h, api := requestsHarness(t)
	h.ticks(2, 5*time.Minute)
	h.approve("!2", "5eed0101")
	api.set(func() {
		api.gitlab[ciProject+"/merge_requests/2/discussions"] = `[{"notes":[{"resolvable":false},{"resolvable":true,"resolved":false}]}]`
	})
	r := h.sup.st.CI[ciKey("widgets", "!2")]
	if r.Unresolved {
		t.Fatal("recorded unresolved before the thread")
	}
	h.sup.mergeRequest("c", h.sup.track("c"), ciWatched{Session: "c", Repository: "widgets", Ref: "!2"}, r, true, true)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merged with an open thread: %+v", h.forge.merges)
	}
	if s := h.mergeSkips(); len(s) == 0 || !strings.Contains(s[len(s)-1], "open discussion") {
		t.Fatalf("skips = %q", s)
	}
	h.ticks(1, 5*time.Minute)
	if r := h.sup.st.CI[ciKey("widgets", "!2")]; !r.Unresolved {
		t.Fatalf("record = %+v", r)
	}
}

// A run's base is the tip when the run was first seen, running, and a
// forge error in between keeps it: a target that moved while the run ran
// is retested.
func TestOutsideBaseIsTipWhenRunFirstSeen(t *testing.T) {
	h, api := requestsHarness(t)
	repo := h.cfg.Repositories["widgets"]
	repo.Policy.CI.RetestOnBaseMove = true
	h.cfg.Repositories["widgets"] = repo
	outsideMR(api, outsideMRState{head: "5eed0101", pipeline: 60, status: "running"})
	h.ticks(2, 5*time.Minute)
	const moved = "abad1dea"
	api.set(func() {
		api.gitlab[ciProject+"/repository/branches/main"] = `{"name":"main","commit":{"id":"` + moved + `"}}`
		api.gitlab[ciProject+"/repository/merge_base"] = `{"id":"` + moved + `"}`
		api.gitlabErr = errors.New("connection refused")
	})
	h.ticks(1, 5*time.Minute)
	if r := h.sup.st.CI[ciKey("widgets", "!2")]; r.State != CIUnknown || r.Base != mergeTip {
		t.Fatalf("record = %+v", r)
	}
	api.set(func() { api.gitlabErr = nil })
	outsideMR(api, outsideMRState{head: "5eed0101", pipeline: 60, status: "success"})
	h.ticks(1, 5*time.Minute)
	if r := h.sup.st.CI[ciKey("widgets", "!2")]; r.State != CISuccess || r.Base != mergeTip {
		t.Fatalf("record = %+v", r)
	}
	h.approve("!2", "5eed0101")
	h.ticks(2, 5*time.Minute)
	if len(h.forge.merges) != 0 || len(h.forge.retests) != 1 {
		t.Fatalf("merges %+v, retests %v", h.forge.merges, h.forge.retests)
	}
}

// A retest that keeps failing is tried with the restart policy's backoff
// and then given up, with a skip.
func TestOutsideFailingRetestGivesUp(t *testing.T) {
	h, api := requestsHarness(t)
	repo := h.cfg.Repositories["widgets"]
	repo.Policy.CI.RetestOnBaseMove = true
	h.cfg.Repositories["widgets"] = repo
	h.forge.retestErr = errors.New("403 Forbidden")
	h.ticks(2, 5*time.Minute)
	h.approve("!2", "5eed0101")
	api.set(func() {
		api.gitlab[ciProject+"/repository/branches/main"] = `{"name":"main","commit":{"id":"abad1dea"}}`
	})
	h.ticks(12, time.Hour)
	if n, max := len(h.forge.retests), h.cfg.Policies.Restart.MaxRestarts; n != max {
		t.Fatalf("retests = %d, want %d", n, max)
	}
	if s := h.mergeSkips(); len(s) == 0 || !strings.Contains(s[len(s)-1], "not retrying") {
		t.Fatalf("skips = %q", s)
	}
	if len(h.forge.merges) != 0 {
		t.Fatalf("merges = %+v", h.forge.merges)
	}
}

// GitHub cannot test a pull request's merge result again: one green on a
// base the target moved from is skipped with that reason, not merged.
func TestOutsideGitHubMovedBaseSkipped(t *testing.T) {
	h := coordHarness(t)
	h.cfg.Poll.CI = true
	repo := h.cfg.Repositories["widgets"]
	repo.Forge = "github"
	repo.Policy.CI.RetestOnBaseMove = true
	h.cfg.Repositories["widgets"] = repo
	setMerge(t, h, func(m *MergeRules) {})
	const base = "/repos/example-org/widgets"
	api := &fakeForgeAPI{github: map[string]string{
		base + "/pulls?state=open&per_page=100":            `[{"number":2}]`,
		base + "/pulls/2":                                  `{"state":"open","mergeable_state":"clean","user":{"login":"widget-bot"},"head":{"sha":"5eed0101"},"base":{"ref":"main"}}`,
		base + "/pulls/2/reviews?per_page=100":             `[]`,
		base + "/commits/5eed0101/check-runs?per_page=100": `{"check_runs":[{"id":21,"name":"test","status":"completed","conclusion":"success"}]}`,
		base + "/git/ref/heads/main":                       `{"object":{"sha":"` + mergeTip + `"}}`,
		base + "/compare/" + mergeTip + "...5eed0101":      `{"status":"ahead","behind_by":0}`,
		base + "/compare/abad1dea...5eed0101":              `{"status":"diverged","behind_by":1}`,
	}}
	h.sup.api = api
	h.ticks(2, 5*time.Minute)
	if r := h.sup.st.CI[ciKey("widgets", "#2")]; r.Session != "c" || r.State != CISuccess || r.Base != mergeTip || r.Run != "21" {
		t.Fatalf("record = %+v", r)
	}
	h.approve("#2", "5eed0101")
	api.set(func() { api.github[base+"/git/ref/heads/main"] = `{"object":{"sha":"abad1dea"}}` })
	h.ticks(3, 5*time.Minute)
	if len(h.forge.merges) != 0 || len(h.forge.retests) != 0 {
		t.Fatalf("merges %+v, retests %v", h.forge.merges, h.forge.retests)
	}
	if s := h.mergeSkips(); len(s) == 0 || !strings.Contains(s[len(s)-1], "GitHub has no call") {
		t.Fatalf("skips = %q", s)
	}
}

// A merged-results pipeline counts for the head only while its commit is
// GitLab's merge of that head, and its first parent is the base it ran
// on; one left from an earlier head, its new push not run, is not green.
func TestMergedResultsPipelineJudgedByItsCommit(t *testing.T) {
	api := &fakeForgeAPI{gitlab: map[string]string{
		ciProject + "/merge_requests/2": `{"iid":2,"state":"opened","sha":"5eed0202","target_branch":"main","author":{"username":"widget-bot"},` +
			`"head_pipeline":{"id":70,"status":"success","sha":"3e3e3e3e","ref":"refs/merge-requests/2/merge"}}`,
		ciProject + "/merge_requests/2/discussions": `[]`,
		ciProject + "/repository/commits/3e3e3e3e":  `{"id":"3e3e3e3e","parent_ids":["` + mergeTip + `","5eed0101"]}`,
	}}
	c, err := gitlabCI(api, "example-org/widgets", "!2")
	if err != nil || c.state != CIPending {
		t.Fatalf("pipeline of the earlier head: %+v, %v", c, err)
	}
	api.gitlab[ciProject+"/repository/commits/3e3e3e3e"] = `{"id":"3e3e3e3e","parent_ids":["` + mergeTip + `","5eed0202"]}`
	c, err = gitlabCI(api, "example-org/widgets", "!2")
	if err != nil || c.state != CISuccess || c.base != mergeTip || c.run != "70" {
		t.Fatalf("pipeline of the head: %+v, %v", c, err)
	}
}

// A base whose tip could not be asked when the run was first seen is
// never filled in later: the run is retested before it merges.
func TestOutsideUnknownBaseRetested(t *testing.T) {
	h, api := requestsHarness(t)
	repo := h.cfg.Repositories["widgets"]
	repo.Policy.CI.RetestOnBaseMove = true
	h.cfg.Repositories["widgets"] = repo
	api.set(func() { api.gitlab[ciProject+"/repository/branches/main"] = fakeNotFound })
	h.ticks(2, 5*time.Minute)
	api.set(func() {
		api.gitlab[ciProject+"/repository/branches/main"] = `{"name":"main","commit":{"id":"` + mergeTip + `"}}`
	})
	h.ticks(1, 5*time.Minute)
	if r := h.sup.st.CI[ciKey("widgets", "!2")]; r.Base != baseUnknown {
		t.Fatalf("base = %q", r.Base)
	}
	h.approve("!2", "5eed0101")
	h.ticks(3, 5*time.Minute)
	if len(h.forge.merges) != 0 || len(h.forge.retests) != 1 {
		t.Fatalf("merges %+v, retests %v", h.forge.merges, h.forge.retests)
	}
	// No new run comes: the wait is said once.
	if s := h.mergeSkips(); len(s) == 0 || !strings.Contains(s[len(s)-1], "waiting for the new pipeline") {
		t.Fatalf("skips = %q", s)
	}
}

// The same failure in a new run, after a green one, is due again.
func TestOutsideRepeatFailureInNewRunIsDue(t *testing.T) {
	h, api := requestsHarness(t)
	red := func(run int) {
		outsideMR(api, outsideMRState{head: "5eed0101", pipeline: run, status: "failed", jobs: []string{"test:failed"}})
	}
	red(60)
	h.ticks(3, 5*time.Minute)
	h.transcriptOf("c", "t1", summaryLine("c1", "m1", "Looked.", h.clock.Now(), 100))
	outsideMR(api, outsideMRState{head: "5eed0101", pipeline: 61, status: "success"})
	h.ticks(2, 5*time.Minute)
	red(62)
	h.ticks(3, 5*time.Minute)
	if w := h.wakes(); len(w) != 2 || w[1].Reason != "ci: !2 failed at 5eed0101" {
		t.Fatalf("wakes = %+v", w)
	}
}

// GitLab's answer that a pipeline would be empty is kept in a failed
// retest's error from whichever stream carried it (#46).
func TestRetestErrorKeepsEmptyAnswer(t *testing.T) {
	const body = `{"message":{"base":["The resulting pipeline would have been empty. Review the rules configuration for the relevant jobs."]}}`
	if err := retestError("HTTP 400 Bad Request", body); !strings.Contains(err.Error(), emptyPipelineAnswer) {
		t.Fatalf("error = %v", err)
	}
	if err := retestError("HTTP 500", `{"message":"oops"}`); err.Error() != "glab: HTTP 500" {
		t.Fatalf("error = %v", err)
	}
	r := Repository{Forge: "gitlab"}
	r.Policy.Merge.Enabled = true
	a := AllowedForge{Forge: &fakeForge{retestErr: errors.New("glab: The resulting pipeline would have been empty. (HTTP 400)")}, Repos: map[string]Repository{"widgets": r}}
	if err := a.RetestAllowed("widgets", "!2"); !errors.Is(err, errEmptyPipeline) || !strings.HasPrefix(err.Error(), errEmptyPipeline.Error()) {
		t.Fatalf("error = %v", err)
	}
}
