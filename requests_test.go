package main

import (
	"encoding/json"
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
