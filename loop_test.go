package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// loopHarness is mergeHarness with a sessions entry for widgets, so
// agentd runs its loop: d works widgets#7, whose goal is recorded, and
// delivers !1, green on c0ffee.
func loopHarness(t *testing.T) (*harness, *fakeForgeAPI) {
	t.Helper()
	h, api := mergeHarness(t)
	d := h.cfg.Sessions["d"]
	d.Issue = 7
	h.cfg.Sessions["d"] = d
	clone := filepath.Join(h.dir, "clone")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	w := h.cfg.Repositories["widgets"]
	w.Sessions = &SessionsPolicy{Root: h.dir, Clone: clone, Target: "main"}
	h.cfg.Repositories["widgets"] = w
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	if err := RecordGoal(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), widgetsCase, widgetsBody); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	h.sup.api = api
	return h, api
}

// judgmentName is the reviewer or judge the loop starts for request ref.
func judgmentName(ref, kind string) string {
	return runtimeName("widgets", "request-"+ref[1:], kind)
}

// callAs makes a started session's tool call, through the server agentd
// mcp runs for it, and returns the reply.
func (h *harness) callAs(name, tool string, args map[string]any) (string, bool) {
	h.t.Helper()
	srv, err := NewMCPServer(h.sup.cfg, name, NewLogWriter(h.buf), func() ([]Event, error) { return h.events(), nil }, h.clock.Now)
	if err != nil {
		h.t.Fatal(err)
	}
	return callOne(h.t, srv, tool, args)
}

// loopSteps is the loop events' steps for ref, oldest first.
func (h *harness) loopSteps(ref string) []string {
	var out []string
	for _, e := range h.events() {
		if e.Kind == KindLoop && e.Loop.Ref == ref {
			out = append(out, e.Loop.Step)
		}
	}
	return out
}

// place is where ref stands, as agentd status shows it.
func (h *harness) place(ref string) LoopView {
	h.t.Helper()
	for _, l := range loopViews(h.sup.cfg, h.sup.st) {
		if l.Ref == ref {
			return l
		}
	}
	h.t.Fatalf("no loop view of %s: %+v", ref, loopViews(h.sup.cfg, h.sup.st))
	return LoopView{}
}

// A delivered request goes review, judge, fix round, review again and
// merge, with no coordinator and no human step: agentd starts and ends
// each judgment session, relaunches the work session with the round in
// its facts, and merges once the rounds converge.
func TestLoopReviewJudgeFixMerge(t *testing.T) {
	h, api := loopHarness(t)
	reviewer, judge := judgmentName("!1", KindReviewer), judgmentName("!1", KindJudge)
	h.ticks(5, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merged before any review: %+v", h.forge.merges)
	}
	if p := h.place("!1"); p.Phase != PlaceReviewing {
		t.Fatalf("place = %+v", p)
	}
	if _, ok := h.sup.st.Runtime[reviewer]; !ok {
		t.Fatalf("no reviewer started: %v", h.sup.st.Runtime)
	}
	if n := h.tmux.count("new-session", reviewer); n != 1 {
		t.Fatalf("reviewer launched %d times", n)
	}
	if cmd := h.lastLaunch(reviewer); !strings.Contains(cmd, `"review":{"request":"!1"`) || !strings.Contains(cmd, `"head":"c0ffee"`) {
		t.Fatalf("reviewer facts lack the request: %s", cmd)
	}
	if text, refused := h.callAs(reviewer, ToolReviewFindings, map[string]any{"findings": []any{finding("Spin is untested")}}); refused {
		t.Fatalf("review refused: %s", text)
	}
	h.ticks(3, 5*time.Minute)
	if _, ok := h.sup.st.Runtime[reviewer]; ok {
		t.Fatal("the reviewer was not ended once its round was recorded")
	}
	if _, ok := h.sup.st.Runtime[judge]; !ok {
		t.Fatalf("no judge started: %v", h.loopSteps("!1"))
	}
	if p := h.place("!1"); p.Phase != PlaceJudging {
		t.Fatalf("place = %+v", p)
	}
	if text, refused := h.callAs(judge, ToolJudgeDispositions, map[string]any{"dispositions": []any{
		map[string]any{"finding_id": "F1", "disposition": DispositionAdopt, "reason": "A spin test is cheap."}},
		"plan": []any{map[string]any{"id": "P1", "step": "Add the spin test."}}}); refused {
		t.Fatalf("judge refused: %s", text)
	}
	h.ticks(3, 5*time.Minute)
	if _, ok := h.sup.st.Runtime[judge]; ok {
		t.Fatal("the judge was not ended once its round was disposed")
	}
	if n := h.count(KindAction, ActFixRound, "d"); n != 1 {
		t.Fatalf("fix rounds = %d, want 1; steps %v", n, h.loopSteps("!1"))
	}
	if cmd := h.lastLaunch("d"); !strings.Contains(cmd, `"why":"fix-round"`) || !strings.Contains(cmd, "Add the spin test.") || !strings.Contains(cmd, FixRoundPointer) {
		t.Fatalf("fix round facts: %s", cmd)
	}
	if b, _ := os.ReadFile(filepath.Join(h.dir, "d", "STATUS.md")); ParseStatus(string(b)).Done {
		t.Fatalf("STATUS.md kept its DONE: %q", b)
	}
	if p := h.place("!1"); p.Phase != PlaceFixing {
		t.Fatalf("place = %+v", p)
	}
	// The fix: a new commit, delivered, green.
	h.git.head = "c0ffe2"
	h.status("d", "# Fixed\n\nDONE\n")
	api.pipelines("c0ffe2", "success", nil, "success", nil)
	h.ticks(12, 5*time.Minute)
	if _, ok := h.sup.st.Runtime[reviewer]; !ok {
		t.Fatalf("no second review: steps %v, place %+v", h.loopSteps("!1"), h.place("!1"))
	}
	if text, refused := h.callAs(reviewer, ToolReviewFindings, map[string]any{"findings": []any{}}); refused {
		t.Fatalf("second review refused: %s", text)
	}
	h.ticks(6, 5*time.Minute)
	if len(h.forge.merges) != 1 || h.forge.merges[0].ref != "!1" || h.forge.merges[0].head != "c0ffe2" {
		t.Fatalf("merges = %+v; skips %v; place %+v", h.forge.merges, h.mergeSkips(), h.place("!1"))
	}
	if got := strings.Join(h.loopSteps("!1"), ","); got != "review,judge,fix,review" {
		t.Fatalf("steps = %s", got)
	}
	if _, ok := h.sup.st.Runtime[reviewer]; ok {
		t.Fatal("the reviewer of the converged round still runs")
	}
}

// lastLaunch is the command of the session's latest launch.
func (h *harness) lastLaunch(name string) string {
	cmd := ""
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Session == name && e.Act.Command != "" {
			cmd = e.Act.Command
		}
	}
	return cmd
}

// An open request no session delivered is reviewed too, in its own case;
// a converged review approves it at the reviewed head, and it merges.
func TestLoopReviewsOutsideRequest(t *testing.T) {
	h, api := loopHarness(t)
	outsideOpen(api)
	h.ticks(3, 5*time.Minute)
	reviewer := judgmentName("!2", KindReviewer)
	sess, ok := h.sup.cfg.Sessions[reviewer]
	if !ok || sess.Request != "!2" || h.sup.cfg.caseOf(sess) != "gitlab/example-org/widgets!2" {
		t.Fatalf("reviewer = %+v, %v; steps %v", sess, ok, h.loopSteps("!2"))
	}
	if text, refused := h.callAs(reviewer, ToolReviewFindings, map[string]any{"findings": []any{}}); refused {
		t.Fatalf("review refused: %s", text)
	}
	h.ticks(3, 5*time.Minute)
	if got := h.sup.st.approvedAt("widgets", "!2"); got != "5eed0101" {
		t.Fatalf("approved at %q; steps %v; place %+v", got, h.loopSteps("!2"), h.place("!2"))
	}
	merged := false
	for _, m := range h.forge.merges {
		merged = merged || m.ref == "!2" && m.head == "5eed0101"
	}
	if !merged {
		t.Fatalf("merges = %+v; skips %v", h.forge.merges, h.mergeSkips())
	}
}

// outsideOpen lists !2, which no session delivered, open and green.
func outsideOpen(api *fakeForgeAPI) {
	api.set(func() { api.gitlab[ciProject+"/merge_requests|state=opened"] = `[{"iid":1},{"iid":2}]` })
	outsideMR(api, outsideMRState{head: "5eed0101", pipeline: 60, status: "success"})
}

// A reviewer stopped by its budget fails its step only: the request waits
// for the owner, told once, while the repository's other request is
// reviewed and merged; the owner's retry starts the review again.
func TestLoopFailedJudgmentStopsOnlyItself(t *testing.T) {
	h, api := loopHarness(t)
	outsideOpen(api)
	h.ticks(3, 5*time.Minute)
	stuck := judgmentName("!1", KindReviewer)
	if _, ok := h.sup.st.Runtime[stuck]; !ok {
		t.Fatalf("no reviewer of !1: %v", h.sup.st.Runtime)
	}
	// The reviewer of !1 reaches its spend cap.
	tr := h.sup.track(stuck)
	tr.State, tr.Stopped = Stopped, "its spend cap"
	h.ticks(1, 5*time.Minute)
	if p := h.place("!1"); p.Phase != PlaceOwner || !strings.Contains(p.Reason, "was stopped: its spend cap") || !strings.Contains(p.Reason, "agentd loop retry widgets !1") {
		t.Fatalf("place = %+v", p)
	}
	if _, ok := h.sup.st.Runtime[stuck]; ok {
		t.Fatal("the failed reviewer was not ended")
	}
	h.ticks(3, 5*time.Minute)
	if n := h.count(KindAction, ActEscalate, requestsTrack("widgets")); n != 1 {
		t.Fatalf("escalations = %d, want 1", n)
	}
	// The other request goes on.
	other := judgmentName("!2", KindReviewer)
	if text, refused := h.callAs(other, ToolReviewFindings, map[string]any{"findings": []any{}}); refused {
		t.Fatalf("review of !2 refused: %s", text)
	}
	h.ticks(3, 5*time.Minute)
	if len(h.forge.merges) != 1 || h.forge.merges[0].ref != "!2" {
		t.Fatalf("merges = %+v", h.forge.merges)
	}
	// The owner's retry reviews !1 again.
	if err := RetryLoop(h.cfg, NewLogWriter(h.buf), h.events(), h.clock.Now(), "widgets", "!1"); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, 5*time.Minute)
	if _, ok := h.sup.st.Runtime[stuck]; !ok {
		t.Fatalf("no review after the retry: steps %v, place %+v", h.loopSteps("!1"), h.place("!1"))
	}
	if err := RetryLoop(h.cfg, NewLogWriter(h.buf), h.events(), h.clock.Now(), "widgets", "!1"); err == nil {
		t.Fatal("a retry with no failed step was recorded")
	}
}

// agentd status shows each request's place in the loop with its reason.
func TestLoopPlaceOnStatus(t *testing.T) {
	h, _ := loopHarness(t)
	h.ticks(3, 5*time.Minute)
	rows, meta := SummarizeState(h.sup.cfg, h.sup.st)
	var b bytes.Buffer
	PrintStatus(&b, rows, meta, h.clock.Now())
	if !strings.Contains(b.String(), "loop widgets !1: reviewing (round 1 at c0ffee)") {
		t.Fatalf("status:\n%s", b.String())
	}
	out, err := json.Marshal(meta["loops"])
	if err != nil || !strings.Contains(string(out), `"phase":"reviewing"`) {
		t.Fatalf("loops = %s, %v", out, err)
	}
}

// Shadow mode records the steps it would take and starts nothing.
func TestLoopShadowStartsNothing(t *testing.T) {
	h, api := loopHarness(t)
	outsideOpen(api)
	h.restart(true)
	h.sup.api = api
	h.ticks(4, 5*time.Minute)
	if len(h.sup.st.Runtime) != 0 || h.count(KindSession, "", "") != 0 {
		t.Fatalf("shadow started sessions: %v", h.sup.st.Runtime)
	}
	if got := h.loopSteps("!2"); len(got) != 1 || got[0] != LoopReview {
		t.Fatalf("shadow steps = %v", got)
	}
}

// agentd starts the top queued case, in a repository the week's priorities
// list, as a work session on its own branch: not a reserved one, not one
// a session works, and one a pass.
func TestLoopStartsNextQueuedCase(t *testing.T) {
	h, _ := loopHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Scope.Reserved.Numbers = []int{9}
	h.cfg.Repositories["widgets"] = w
	h.cfg.Tracking.Priorities = filepath.Join(h.dir, "priorities.json")
	writeFile(t, h.cfg.Tracking.Priorities, `{"week":"2026-W43","repositories":[{"repository":"gitlab/example-org/widgets","priority":1}]}`)
	log := NewLogWriter(h.buf)
	for _, n := range []string{"7", "9", "10", "11"} {
		log.Append(Event{V: EventVersion, Time: h.clock.Now(), Kind: KindItem, Mode: ModeShared, Item: &ItemPayload{ID: "gitlab/example-org/widgets#" + n,
			Kind: ItemIssue, Repository: "gitlab/example-org/widgets", Title: "Widgets " + n}})
	}
	h.restart(false)
	h.ticks(1, 5*time.Minute)
	var started []RuntimePayload
	for _, p := range h.sessionEvents() {
		if p.Kind == KindWork {
			started = append(started, p)
		}
	}
	if len(started) != 1 || started[0].Issue != 10 || started[0].Branch != "issue-10" || started[0].By != "agentd" {
		t.Fatalf("started = %+v", started)
	}
	h.ticks(1, 5*time.Minute)
	if n := len(h.sessionEvents()); n != 2 {
		t.Fatalf("session events = %+v", h.sessionEvents())
	}
	if p := h.sessionEvents()[1]; p.Issue != 11 {
		t.Fatalf("second start = %+v", p)
	}
	// With three places, two work sessions leave the last for the
	// judgment sessions their requests need.
	w = h.cfg.Repositories["widgets"]
	w.Policy.Sessions.Max = 3
	h.cfg.Repositories["widgets"] = w
	log.Append(Event{V: EventVersion, Time: h.clock.Now(), Kind: KindItem, Mode: ModeShared, Item: &ItemPayload{ID: "gitlab/example-org/widgets#12",
		Kind: ItemIssue, Repository: "gitlab/example-org/widgets", Title: "Widgets 12"}})
	h.ticks(2, 5*time.Minute)
	for _, p := range h.sessionEvents() {
		if p.Issue == 12 {
			t.Fatalf("started #12 into the judgment sessions' place: %+v", h.sessionEvents())
		}
	}
}

// A work session agentd started is ended once every request it delivered
// is merged or closed, so its place goes to the next case.
func TestLoopEndsFinishedWorkSession(t *testing.T) {
	h, _ := loopHarness(t)
	name := runtimeName("widgets", "issue-10", KindWork)
	if why := h.sup.startRuntime("widgets", name, RuntimePayload{Kind: KindWork, Branch: "issue-10", Issue: 10}); why != "" {
		t.Fatal(why)
	}
	tr := h.sup.track(name)
	tr.Deliveries = []DeliveryRecord{{Ref: "!5", Repository: "widgets"}}
	h.sup.st.CI[ciKey("widgets", "!5")] = CIRecord{CIPayload: CIPayload{Repository: "widgets", Ref: "!5", State: CIPending}}
	h.sup.endFinished()
	if _, ok := h.sup.st.Runtime[name]; !ok {
		t.Fatal("ended while its request is open")
	}
	h.sup.st.CI[ciKey("widgets", "!5")] = CIRecord{CIPayload: CIPayload{Repository: "widgets", Ref: "!5", State: CIMerged}}
	h.sup.endFinished()
	if _, ok := h.sup.st.Runtime[name]; ok {
		t.Fatal("not ended after its request merged")
	}
}

// reviewAndAdopt runs !1 through a review with one finding and a judge
// that adopts it, to the fix round.
func reviewAndAdopt(t *testing.T, h *harness) {
	t.Helper()
	h.ticks(5, 5*time.Minute)
	if text, refused := h.callAs(judgmentName("!1", KindReviewer), ToolReviewFindings, map[string]any{"findings": []any{finding("Spin is untested")}}); refused {
		t.Fatalf("review refused: %s", text)
	}
	h.ticks(3, 5*time.Minute)
	if text, refused := h.callAs(judgmentName("!1", KindJudge), ToolJudgeDispositions, map[string]any{"dispositions": []any{
		map[string]any{"finding_id": "F1", "disposition": DispositionAdopt, "reason": "A spin test is cheap."}}}); refused {
		t.Fatalf("judge refused: %s", text)
	}
	h.ticks(3, 5*time.Minute)
	if n := h.count(KindAction, ActFixRound, "d"); n != 1 {
		t.Fatalf("fix rounds = %d; steps %v", n, h.loopSteps("!1"))
	}
}

// A fix round that delivers no new commit waits for the owner, and a
// replay of the log puts every request where the live run has it.
func TestLoopFixRoundWithoutCommitWaitsAndReplayAgrees(t *testing.T) {
	h, api := loopHarness(t)
	reviewAndAdopt(t, h)
	h.status("d", "# Nothing to change\n\nDONE\n")
	h.ticks(12, 5*time.Minute)
	if p := h.place("!1"); p.Phase != PlaceOwner || !strings.Contains(p.Reason, "delivered no new commit") {
		t.Fatalf("place = %+v", p)
	}
	live := loopViews(h.sup.cfg, h.sup.st)
	h.restart(false)
	h.sup.api = api
	h.sup.refreshRuntime()
	if got := loopViews(h.sup.cfg, h.sup.st); !reflect.DeepEqual(got, live) {
		t.Fatalf("replayed %+v, live %+v", got, live)
	}
}

// A judge that crash-loops fails its step; the owner's retry starts a
// judge again. One that asked the owner waits for the answer even when
// it wrote DONE.
func TestLoopJudgeCrashAndOwnerQuestion(t *testing.T) {
	h, _ := loopHarness(t)
	h.ticks(5, 5*time.Minute)
	if text, refused := h.callAs(judgmentName("!1", KindReviewer), ToolReviewFindings, map[string]any{"findings": []any{finding("Spin is untested")}}); refused {
		t.Fatalf("review refused: %s", text)
	}
	h.ticks(3, 5*time.Minute)
	judge := judgmentName("!1", KindJudge)
	tr := h.sup.track(judge)
	tr.State, tr.Questions.Open = Done, true
	if p := h.place("!1"); p.Phase != PlaceOwner || !strings.Contains(p.Reason, "asks the owner") {
		t.Fatalf("place = %+v", p)
	}
	tr.Questions.Open, tr.State, tr.Parked, tr.Crashes = false, CrashLooping, true, []Crash{{Exit: 1}}
	h.ticks(1, 5*time.Minute)
	if p := h.place("!1"); p.Phase != PlaceOwner || !strings.Contains(p.Reason, "the judge of round 1 crash-loops") {
		t.Fatalf("place = %+v", p)
	}
	if err := RetryLoop(h.cfg, NewLogWriter(h.buf), h.events(), h.clock.Now(), "widgets", "!1"); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, 5*time.Minute)
	if got := strings.Join(h.loopSteps("!1"), ","); got != "review,judge,failed,retry,judge" {
		t.Fatalf("steps = %s", got)
	}
	// A second failure of the same round is told to the owner again.
	tr = h.sup.track(judge)
	tr.State, tr.Stopped = Stopped, "its spend cap"
	h.ticks(3, 5*time.Minute)
	if n := h.count(KindAction, ActEscalate, requestsTrack("widgets")); n != 2 {
		t.Fatalf("escalations = %d, want 2", n)
	}
}

// A head that moves after the rounds converged, as a CI fix moves it, is
// not merged unreviewed: the request waits for the owner's reset.
func TestLoopHeadMovedAfterConvergence(t *testing.T) {
	h, api := loopHarness(t)
	api.pipelines("c0ffee", "running", nil, "success", nil)
	h.ticks(5, 5*time.Minute)
	if text, refused := h.callAs(judgmentName("!1", KindReviewer), ToolReviewFindings, map[string]any{"findings": []any{}}); refused {
		t.Fatalf("review refused: %s", text)
	}
	h.ticks(2, 5*time.Minute)
	api.pipelines("c0ffe3", "success", nil, "success", nil)
	h.ticks(4, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merged an unreviewed head: %+v", h.forge.merges)
	}
	if p := h.place("!1"); p.Phase != PlaceOwner || !strings.Contains(p.Reason, "head moved to c0ffe3") || !strings.Contains(p.Reason, "agentd cases rounds") {
		t.Fatalf("place = %+v", p)
	}
}

// A reviewer that asked the owner waits for the answer, DONE or not; one
// whose request left the loop, merged by hand, is ended.
func TestLoopReviewerQuestionAndStrandedJudgment(t *testing.T) {
	h, api := loopHarness(t)
	h.ticks(5, 5*time.Minute)
	reviewer := judgmentName("!1", KindReviewer)
	tr := h.sup.track(reviewer)
	tr.State, tr.Questions.Open = Done, true
	if p := h.place("!1"); p.Phase != PlaceOwner || !strings.Contains(p.Reason, "the reviewer of round 1 asks the owner") {
		t.Fatalf("place = %+v", p)
	}
	api.set(func() {
		api.gitlab[ciProject+"/merge_requests/1"] = strings.Replace(api.gitlab[ciProject+"/merge_requests/1"], `"state":"opened"`, `"state":"merged"`, 1)
	})
	h.ticks(3, 5*time.Minute)
	if _, ok := h.sup.st.Runtime[reviewer]; ok {
		t.Fatalf("the reviewer of a merged request still runs; ci %+v", h.sup.st.CI[ciKey("widgets", "!1")])
	}
}

// A looped repository needs room for a work session and its reviewer.
func TestLoopNeedsTwoPlaces(t *testing.T) {
	h, _ := loopHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Sessions.Max = 1
	h.cfg.Repositories["widgets"] = w
	if err := h.cfg.finish(h.dir); err == nil || !strings.Contains(err.Error(), "2 or more") {
		t.Fatalf("finish = %v", err)
	}
}
