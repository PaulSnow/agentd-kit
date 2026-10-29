package main

import (
	"bytes"
	"encoding/json"
	"net/http"
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
	for _, l := range loopViews(h.sup.cfg, h.sup.st, h.clock.Now()) {
		if l.Ref == ref {
			return l
		}
	}
	h.t.Fatalf("no loop view of %s: %+v", ref, loopViews(h.sup.cfg, h.sup.st, h.clock.Now()))
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
	h.sup.endFinished(h.clock.Now())
	if _, ok := h.sup.st.Runtime[name]; !ok {
		t.Fatal("ended while its request is open")
	}
	h.sup.st.CI[ciKey("widgets", "!5")] = CIRecord{CIPayload: CIPayload{Repository: "widgets", Ref: "!5", State: CIMerged}}
	h.sup.endFinished(h.clock.Now())
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
	live := loopViews(h.sup.cfg, h.sup.st, h.clock.Now())
	h.restart(false)
	h.sup.api = api
	h.sup.refreshRuntime()
	if got := loopViews(h.sup.cfg, h.sup.st, h.clock.Now()); !reflect.DeepEqual(got, live) {
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

// A request enters the loop only when it is no draft, into one of
// merge.targets, by one of merge.authors or, under review.others
// review-only, updated within review.others_max_age, and not excluded or
// reserved (#55). One outside is still watched for CI, and agentd status
// lists it once with why.
func TestLoopSkipsRequestsOutsideIt(t *testing.T) {
	for _, c := range []struct {
		name, why string
		mr        outsideMRState
		policy    func(p *RepoPolicy)
	}{
		{"draft", LoopOutDraft, outsideMRState{draft: true}, nil},
		{"target", LoopOutTarget, outsideMRState{target: "release"}, nil},
		{"author", LoopOutAuthor, outsideMRState{author: "someone-else"}, nil},
		{"stale", LoopOutStale, outsideMRState{author: "someone-else", updated: "2026-10-01T00:00:00Z"},
			func(p *RepoPolicy) { p.Review.Others = ReviewOnly }},
		{"shorter max age", LoopOutStale, outsideMRState{author: "someone-else", updated: "2026-10-19T00:00:00Z"},
			func(p *RepoPolicy) { p.Review.Others, p.Review.OthersMaxAge = ReviewOnly, Duration{48 * time.Hour} }},
		{"excluded", LoopOutExcluded, outsideMRState{}, func(p *RepoPolicy) { p.Merge.Exclude.Numbers = []int{2} }},
		{"reserved", LoopOutExcluded, outsideMRState{}, func(p *RepoPolicy) { p.Scope.Reserved.Numbers = []int{2} }},
		{"recent other author", "", outsideMRState{author: "someone-else"}, func(p *RepoPolicy) { p.Review.Others = ReviewOnly }},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, api := loopHarness(t)
			if c.policy != nil {
				w := h.sup.cfg.Repositories["widgets"]
				c.policy(&w.Policy)
				h.sup.cfg.Repositories["widgets"] = w
			}
			api.set(func() { api.gitlab[ciProject+"/merge_requests|state=opened"] = `[{"iid":1},{"iid":2}]` })
			c.mr.head, c.mr.pipeline, c.mr.status = "5eed0101", 60, "success"
			outsideMR(api, c.mr)
			h.ticks(3, 5*time.Minute)
			if _, ok := h.sup.st.CI[ciKey("widgets", "!2")]; !ok {
				t.Fatal("!2 is not watched for CI")
			}
			reviewed := contains(h.loopSteps("!2"), LoopReview)
			if c.why == "" {
				if !reviewed {
					t.Fatalf("!2 not reviewed; place %+v", h.place("!2"))
				}
				return
			}
			if reviewed {
				t.Fatalf("!2 reviewed though %s; steps %v", c.why, h.loopSteps("!2"))
			}
			if p := h.place("!2"); p.Phase != PlaceOutside || p.Reason != c.why {
				t.Fatalf("place = %+v, want %s", p, c.why)
			}
			if !contains(h.loopSteps("!1"), LoopReview) {
				t.Fatalf("!1 not reviewed: steps %v", h.loopSteps("!1"))
			}
			rows, meta := SummarizeState(h.sup.cfg, h.sup.st)
			var b bytes.Buffer
			PrintStatus(&b, rows, meta, h.clock.Now())
			if line := "loop widgets !2: " + PlaceOutside + " (" + c.why + ")"; strings.Count(b.String(), line) != 1 {
				t.Fatalf("status has not %q once:\n%s", line, b.String())
			}
		})
	}
}

// A draft enters the loop on the pass after it leaves draft.
func TestLoopDraftEntersWhenReady(t *testing.T) {
	h, api := loopHarness(t)
	api.set(func() { api.gitlab[ciProject+"/merge_requests|state=opened"] = `[{"iid":1},{"iid":2}]` })
	mr := outsideMRState{head: "5eed0101", pipeline: 60, status: "success", draft: true}
	outsideMR(api, mr)
	h.ticks(3, 5*time.Minute)
	if steps := h.loopSteps("!2"); len(steps) > 0 {
		t.Fatalf("draft took steps %v", steps)
	}
	mr.draft = false
	outsideMR(api, mr)
	h.ticks(3, 5*time.Minute)
	if !contains(h.loopSteps("!2"), LoopReview) {
		t.Fatalf("!2 not reviewed after leaving draft; place %+v", h.place("!2"))
	}
}

// A request a work session delivered is filtered the same way: one that
// turns draft leaves the loop, and its reviewer is ended.
func TestLoopFiltersDeliveredRequest(t *testing.T) {
	h, api := loopHarness(t)
	h.ticks(3, 5*time.Minute)
	reviewer := judgmentName("!1", KindReviewer)
	if _, ok := h.sup.st.Runtime[reviewer]; !ok {
		t.Fatalf("no reviewer of !1; steps %v", h.loopSteps("!1"))
	}
	api.set(func() {
		k := ciProject + "/merge_requests/1"
		api.gitlab[k] = strings.Replace(api.gitlab[k], `"target_branch":"main"`, `"draft":true,"target_branch":"main"`, 1)
	})
	h.ticks(3, 5*time.Minute)
	if p := h.place("!1"); p.Phase != PlaceOutside || p.Reason != LoopOutDraft {
		t.Fatalf("place = %+v", p)
	}
	if _, ok := h.sup.st.Runtime[reviewer]; ok {
		t.Fatal("the reviewer of a request that left the loop still runs")
	}
	if steps := h.loopSteps("!1"); len(steps) != 1 {
		t.Fatalf("steps after leaving the loop %v", steps)
	}
}

// review.others_max_age defaults to 14 days, takes a day count, and a
// repository's own file may only shorten it.
func TestOthersMaxAgePolicy(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"14d"`), &d); err != nil || d.Duration != 14*24*time.Hour {
		t.Fatalf("14d = %v, %v", d.Duration, err)
	}
	h, _ := loopHarness(t)
	w := h.sup.cfg.Repositories["widgets"]
	if w.Policy.Review.OthersMaxAge.Duration != DefaultOthersMaxAge || DefaultOthersMaxAge != 14*24*time.Hour {
		t.Fatalf("default = %v", w.Policy.Review.OthersMaxAge)
	}
	eff := w.Policy
	if !narrowKey(&eff, "review.others_max_age", json.RawMessage(`"7d"`)) || eff.Review.OthersMaxAge.Duration != 7*24*time.Hour {
		t.Fatalf("7d not taken: %v", eff.Review.OthersMaxAge)
	}
	if narrowKey(&eff, "review.others_max_age", json.RawMessage(`"30d"`)) || narrowKey(&eff, "review.others_max_age", json.RawMessage(`"0s"`)) {
		t.Fatal("a longer or zero age was taken")
	}
	var errs []error
	w.Policy.Review.OthersMaxAge = Duration{-time.Hour}
	h.cfg.checkPolicy("widgets", &w, &errs)
	if len(errs) == 0 {
		t.Fatal("a negative age was accepted")
	}
}

// A request into a target is judged by that target's own file, as its
// merge is, never by another target's.
func TestLoopJudgesEachTargetByItsOwnFile(t *testing.T) {
	pol := RepoPolicy{Merge: MergeRules{Targets: []string{"main", "release"}, Authors: []string{"widget-bot", "gadget-bot"}}}
	cfg := &Config{Repositories: map[string]Repository{"widgets": {Policy: pol}}}
	st := &LogState{Policies: map[string]PolicyPayload{policyKey("widgets", "main"): {
		Repository: "widgets", Target: "main", File: json.RawMessage(`{"merge":{"targets":["main"],"authors":["widget-bot"]}}`)}}}
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	all, _ := effectivePolicy(cfg, st, "widgets")
	for _, c := range []struct{ target, author, want string }{
		{"release", "gadget-bot", ""},
		{"main", "gadget-bot", LoopOutAuthor},
		{"main", "widget-bot", ""},
	} {
		r := CIRecord{CIPayload: CIPayload{Head: "5eed0101", Target: c.target, Author: c.author}}
		if got := outsideLoop(all, func(target string) RepoPolicy { return effectiveFor(cfg, st, "widgets", target) }, r, true, "!2", now); got != c.want {
			t.Errorf("%s by %s: %q, want %q", c.target, c.author, got, c.want)
		}
	}
}

// A request by another author whose update the watcher has not recorded,
// as in a record written before #55, starts nothing and does not end its
// running reviewer; once recorded, it is judged by its age.
func TestLoopWaitsForAnUnknownUpdate(t *testing.T) {
	h, api := loopHarness(t)
	w := h.sup.cfg.Repositories["widgets"]
	w.Policy.Review.Others = ReviewOnly
	h.sup.cfg.Repositories["widgets"] = w
	api.set(func() { api.gitlab[ciProject+"/merge_requests|state=opened"] = `[{"iid":1},{"iid":2}]` })
	mr := outsideMRState{author: "someone-else", head: "5eed0101", pipeline: 60, status: "success", updated: "none"}
	outsideMR(api, mr)
	h.ticks(3, 5*time.Minute)
	if p := h.place("!2"); p.Phase != PlaceOutside || p.Reason != LoopOutUnknown {
		t.Fatalf("place = %+v", p)
	}
	if steps := h.loopSteps("!2"); len(steps) > 0 {
		t.Fatalf("steps %v", steps)
	}
	mr.updated = ""
	outsideMR(api, mr)
	h.ticks(3, 5*time.Minute)
	reviewer := judgmentName("!2", KindReviewer)
	if _, ok := h.sup.st.Runtime[reviewer]; !ok {
		t.Fatalf("no reviewer of !2; place %+v", h.place("!2"))
	}
	k := ciKey("widgets", "!2")
	r := h.sup.st.CI[k]
	r.Updated = ""
	h.sup.st.CI[k] = r
	h.sup.endFinished(h.clock.Now())
	if _, ok := h.sup.st.Runtime[reviewer]; !ok {
		t.Fatal("the reviewer was ended while the update is unknown")
	}
	if f := h.sup.reviewFact(reviewer); f == nil || f.Request != "!2" {
		t.Fatalf("fact while the update is unknown = %+v", f)
	}
}

// agentd status judges a request's age at the reader's clock, as the live
// loop does, though no event was written as it crossed
// review.others_max_age.
func TestStatusJudgesAgeAtTheReadersClock(t *testing.T) {
	h, api := loopHarness(t)
	w := h.sup.cfg.Repositories["widgets"]
	w.Policy.Review.Others = ReviewOnly
	h.sup.cfg.Repositories["widgets"] = w
	api.set(func() { api.gitlab[ciProject+"/merge_requests|state=opened"] = `[{"iid":1},{"iid":2}]` })
	outsideMR(api, outsideMRState{author: "someone-else", head: "5eed0101", pipeline: 60, status: "success"})
	h.ticks(3, 5*time.Minute)
	later := h.clock.Now().Add(DefaultOthersMaxAge)
	if got := loopViews(h.sup.cfg, h.sup.st, later); !containsView(got, "!2", LoopOutStale) {
		t.Fatalf("live at a later pass: %+v", got)
	}
	_, meta := SummarizeAt(h.sup.cfg, h.events(), ModeLive, later)
	if got, _ := meta["loops"].([]LoopView); !containsView(got, "!2", LoopOutStale) {
		t.Fatalf("status at a later clock: %+v", meta["loops"])
	}
}

// containsView says whether views put ref outside the loop for why.
func containsView(views []LoopView, ref, why string) bool {
	for _, v := range views {
		if v.Ref == ref && v.Phase == PlaceOutside && v.Reason == why {
			return true
		}
	}
	return false
}

// loopAtOwner is a loop whose request !1 waits on the owner: its
// reviewer was stopped by its spend cap, so its review step failed.
func loopAtOwner(t *testing.T) *harness {
	t.Helper()
	h, api := loopHarness(t)
	outsideOpen(api)
	h.ticks(3, 5*time.Minute)
	tr := h.sup.track(judgmentName("!1", KindReviewer))
	tr.State, tr.Stopped = Stopped, "its spend cap"
	h.ticks(1, 5*time.Minute)
	if p := h.place("!1"); p.Phase != PlaceOwner {
		t.Fatalf("place = %+v", p)
	}
	return h
}

// lastLoop is the last loop event of h's log.
func (h *harness) lastLoop() Event {
	h.t.Helper()
	es := h.events()
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Kind == KindLoop {
			return es[i]
		}
	}
	h.t.Fatal("no loop event")
	return Event{}
}

// A request the loop waits on the owner for is a decision card with
// retry the step, skip to judging and stop; each option, posted from the
// page, writes the event `agentd loop retry|skip|stop` writes.
func TestLoopOwnerCard(t *testing.T) {
	for _, step := range []string{LoopRetry, LoopSkip, LoopStop} {
		h := loopAtOwner(t)
		v := BuildRepoLevel(h.cfg, h.sup.st, "widgets", nil, h.clock.Now())
		var card *LevelDecision
		for k := range v.Needs {
			if v.Needs[k].Entry == entryLoop+"!1" {
				card = &v.Needs[k]
			}
		}
		if card == nil || len(card.Options) != 3 || card.Options[0].Key != LoopRetry || card.Options[1].Key != LoopSkip || card.Options[2].Key != LoopStop {
			t.Fatalf("the card: %+v in %+v", card, v.Needs)
		}
		src := newUISource("live", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a")
		u, _ := testUI(t, UIConfig{}, src)
		if body := owner(t, u, "/repo/widgets"); !strings.Contains(body, `value="l:!1"`) || !strings.Contains(body, `value="skip"`) {
			t.Fatalf("the page has no card:\n%s", body)
		}
		if w := multipartTo(u, "/respond", u.token, map[string]string{"src": "live", "repository": "widgets", "entry": "l:!1", "choice": step, "back": "/repo/widgets"}); w.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d %s", step, w.Code, w.Body.String())
		}
		page := h.lastLoop()
		c := loopAtOwner(t)
		if err := LoopStep(c.cfg, NewLogWriter(c.buf), c.events(), c.clock.Now(), "widgets", "!1", step); err != nil {
			t.Fatal(err)
		}
		cli := c.lastLoop()
		if page.Kind != cli.Kind || page.Mode != ModeOwner || cli.Mode != ModeOwner || *page.Loop != *cli.Loop || page.Loop.Step != step {
			t.Fatalf("%s: the page wrote %+v %+v, the CLI %+v %+v", step, page, page.Loop, cli, cli.Loop)
		}
	}
}

// Stop leaves the request alone until a retry; skip takes the failed
// review as clean, so the request goes on as converged; neither is
// taken twice.
func TestLoopStopAndSkip(t *testing.T) {
	h := loopAtOwner(t)
	step := func(s string) error {
		return LoopStep(h.cfg, NewLogWriter(h.buf), h.events(), h.clock.Now(), "widgets", "!1", s)
	}
	if err := step(LoopStop); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	h.ticks(3, 5*time.Minute)
	if p := h.place("!1"); p.Phase != PlaceOwner || !strings.Contains(p.Reason, "stopped") {
		t.Fatalf("stopped: %+v", p)
	}
	if _, ok := h.sup.st.Runtime[judgmentName("!1", KindReviewer)]; ok {
		t.Fatal("a reviewer runs on a stopped request")
	}
	if step(LoopStop) == nil || step(LoopSkip) == nil {
		t.Fatal("a stopped request was stopped or skipped again")
	}
	if err := step(LoopRetry); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	h.ticks(2, 5*time.Minute)
	if _, ok := h.sup.st.Runtime[judgmentName("!1", KindReviewer)]; !ok {
		t.Fatalf("no review after the retry: %+v", h.place("!1"))
	}

	s := loopAtOwner(t)
	if err := LoopStep(s.cfg, NewLogWriter(s.buf), s.events(), s.clock.Now(), "widgets", "!1", LoopSkip); err != nil {
		t.Fatal(err)
	}
	sapi := s.sup.api
	s.restart(false)
	s.sup.api = sapi
	if p := s.place("!1"); p.Phase == PlaceOwner || p.Phase == PlaceReviewing || !strings.Contains(p.Reason, "skipped") {
		t.Fatalf("skipped: %+v", p)
	}
	if LoopStep(s.cfg, NewLogWriter(s.buf), s.events(), s.clock.Now(), "widgets", "!1", LoopSkip) == nil {
		t.Fatal("a skip with no failed review was recorded")
	}
	s.ticks(3, 5*time.Minute)
	if !mergedRef(s, "!1") {
		t.Fatalf("the skipped request did not merge: %+v, merges %+v", s.place("!1"), s.forge.merges)
	}

	// A skipped request the owner then stops does not merge.
	x := loopAtOwner(t)
	for _, step := range []string{LoopSkip, LoopStop} {
		if err := LoopStep(x.cfg, NewLogWriter(x.buf), x.events(), x.clock.Now(), "widgets", "!1", step); err != nil {
			t.Fatal(err)
		}
	}
	api := x.sup.api
	x.restart(false)
	x.sup.api = api
	x.ticks(3, 5*time.Minute)
	if mergedRef(x, "!1") {
		t.Fatal("a stopped request merged")
	}
}

// mergedRef says whether h's forge merged ref.
func mergedRef(h *harness, ref string) bool {
	for _, m := range h.forge.merges {
		if m.ref == ref {
			return true
		}
	}
	return false
}

// loopAtMax is a loop whose request !1 has round 1's findings due for a
// judge while widgets, at sessions.max 2, runs two started work sessions
// the loop may not end for room: the first waits on the owner, with an
// open question in its STATUS.md; the second is idle past its nudges.
// It returns the two.
func loopAtMax(t *testing.T) (*harness, string, string) {
	t.Helper()
	h, _ := loopHarness(t)
	setMax := func(n int) {
		w := h.cfg.Repositories["widgets"]
		w.Policy.Sessions.Max = n
		h.cfg.Repositories["widgets"] = w
	}
	setMax(3)
	h.ticks(5, 5*time.Minute)
	reviewer := judgmentName("!1", KindReviewer)
	if _, ok := h.sup.st.Runtime[reviewer]; !ok {
		t.Fatalf("no reviewer started: %v", h.sup.st.Runtime)
	}
	asks := h.startSession("issue-10", KindWork, 10)
	idle := h.startSession("issue-11", KindWork, 11)
	h.ticks(1, time.Minute)
	h.alive(asks, "Which colour, red or blue?\n"+idleScreen)
	h.alive(idle, idleScreen)
	h.status(asks, "# Status\n\n## Questions\n- Q1. Red or blue?\n")
	h.status(idle, "# Status\n\nPainting.\n")
	setMax(2)
	if text, refused := h.callAs(reviewer, ToolReviewFindings, map[string]any{"findings": []any{finding("Spin is untested")}}); refused {
		t.Fatalf("review refused: %s", text)
	}
	h.ticks(1, time.Minute)
	h.sup.track(idle).Nudges = h.cfg.Policies.Nudge.Max
	h.ticks(2, time.Minute)
	if h.state(asks) != BlockedOnOwner {
		t.Fatalf("%s is %s", asks, h.state(asks))
	}
	return h, asks, idle
}

// loopSkipsOf are the skips the loop recorded for ref.
func (h *harness) loopSkipsOf(ref string) []string {
	var out []string
	for _, e := range h.events() {
		if e.Kind == KindSkip && strings.HasPrefix(e.Skip.Key, "loop:"+ciKey("widgets", ref)+":") {
			out = append(out, e.Skip.Reason)
		}
	}
	return out
}

// A judge due at sessions.max waits for a place, and says so where the
// owner looks: the place, the request's sentence and a card on the
// repository page name the max and each holder, which waits on the owner
// and which is idle past its nudges, and what the owner can do. One skip
// is recorded for the holders, not one a tick; the judge starts on the
// tick after the owner ends a holder, with one loop event.
func TestLoopStepWaitsForAPlace(t *testing.T) {
	h, asks, idle := loopAtMax(t)
	judge := judgmentName("!1", KindJudge)
	if _, ok := h.sup.st.Runtime[judge]; ok {
		t.Fatal("a judge started past the max")
	}
	p := h.place("!1")
	for _, want := range []string{"widgets runs 2 started sessions, its max 2", "the places are held by " + asks + ", " + idle,
		asks + " waits on you", idle + " is idle past its nudges", "agentd sessions end NAME", "raise sessions.max"} {
		if p.Phase != PlaceWaitPlace || !strings.Contains(p.Reason, want) {
			t.Fatalf("place = %+v, want %q", p, want)
		}
	}
	// The sentence counts the holders when naming them would not fit;
	// the card names them.
	if s := h.sup.st.Sentences[sentenceKey("widgets", "!1")]; s == nil || !strings.Contains(s.Text, "!1 is "+PlaceWaitPlace+": widgets runs 2 started sessions, its max 2 (1 waits on you, 1 is idle past its nudges); end one or raise sessions.max") {
		t.Fatalf("sentence = %+v", s)
	}
	rows, meta := SummarizeState(h.sup.cfg, h.sup.st)
	var b bytes.Buffer
	PrintStatus(&b, rows, meta, h.clock.Now())
	if !strings.Contains(b.String(), "loop widgets !1: "+PlaceWaitPlace+" ("+p.Reason+")") {
		t.Fatalf("status:\n%s", b.String())
	}
	var card *LevelDecision
	for _, n := range requestNeeds(h.cfg, h.sup.st, "widgets", h.clock.Now()) {
		if n.Entry == entryLoop+"!1" {
			d := n.LevelDecision
			card = &d
		}
	}
	if card == nil || !strings.Contains(card.Asked, p.Reason) || len(card.Options) != 2 ||
		card.Options[0].Key != nextEnd+asks || card.Options[1].Key != nextEnd+idle || !strings.Contains(card.Options[0].Label, "start the judge") {
		t.Fatalf("card = %+v", card)
	}
	src := newUISource("live", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a")
	u, _ := testUI(t, UIConfig{}, src)
	if body := owner(t, u, "/repo/widgets"); !strings.Contains(body, `value="l:!1"`) || !strings.Contains(body, `value="end:`+asks+`"`) {
		t.Fatalf("the page has no card:\n%s", body)
	}
	h.ticks(3, 5*time.Minute)
	if skips := h.loopSkipsOf("!1"); len(skips) != 1 || !strings.Contains(skips[0], asks) {
		t.Fatalf("skips = %q", skips)
	}

	// The owner ends the idle one from the card: the same end `agentd
	// sessions end` writes.
	if w := multipartTo(u, "/respond", u.token, map[string]string{"src": "live", "repository": "widgets", "entry": "l:!1", "choice": nextEnd + idle, "back": "/repo/widgets"}); w.Code != http.StatusSeeOther {
		t.Fatalf("end: %d %s", w.Code, w.Body.String())
	}
	var ends []Event
	for _, e := range h.events() {
		if e.Kind == KindSession && e.Runtime.Action == SessionEnd && e.Session == idle {
			ends = append(ends, e)
		}
	}
	if len(ends) != 1 || ends[0].Mode != ModeOwner || ends[0].Runtime.By != "owner" || ends[0].Runtime.Repository != "widgets" {
		t.Fatalf("ends = %+v", ends)
	}
	h.ticks(1, 5*time.Minute)
	if _, ok := h.sup.st.Runtime[judge]; !ok {
		t.Fatalf("no judge on the tick after a place freed: place %+v, steps %v", h.place("!1"), h.loopSteps("!1"))
	}
	h.ticks(3, 5*time.Minute)
	judged := 0
	for _, s := range h.loopSteps("!1") {
		if s == LoopJudge {
			judged++
		}
	}
	if judged != 1 {
		t.Fatalf("steps = %v", h.loopSteps("!1"))
	}
	if p := h.place("!1"); p.Phase != PlaceJudging {
		t.Fatalf("place = %+v", p)
	}
}

// A new set of holders records one skip more; the same set none.
func TestLoopWaitSkipPerHolders(t *testing.T) {
	h, _, idle := loopAtMax(t)
	h.ticks(2, 5*time.Minute)
	if n := len(h.loopSkipsOf("!1")); n != 1 {
		t.Fatalf("skips = %q", h.loopSkipsOf("!1"))
	}
	// The place goes to another work session before the loop sees it.
	h.sup.emit(idle, Event{Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: newIntentID(), Action: SessionEnd, Repository: "widgets", By: "agentd"}})
	h.sup.refreshRuntime()
	other := h.startSession("issue-12", KindWork, 12)
	h.alive(other, idleScreen)
	h.ticks(3, 5*time.Minute)
	if skips := h.loopSkipsOf("!1"); len(skips) != 2 || !strings.Contains(skips[1], other) {
		t.Fatalf("skips = %q", skips)
	}
}

// With sessions.yield_after set, a work session that holds no owner item
// and is idle past its nudges is ended for the waiting judge once the
// wait has lasted yield_after, never before; one console message names
// it, and the judge starts. One that waits on the owner, or holds an
// owner item, is never ended so, and the card says which and why.
func TestLoopYieldsAnIdleHolder(t *testing.T) {
	h, asks, idle := loopAtMax(t)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Sessions.YieldAfter = Duration{time.Hour}
	h.cfg.Repositories["widgets"] = w
	h.ticks(1, 5*time.Minute)
	var card *LevelDecision
	for _, n := range requestNeeds(h.cfg, h.sup.st, "widgets", h.clock.Now()) {
		if n.Entry == entryLoop+"!1" {
			d := n.LevelDecision
			card = &d
		}
	}
	if card == nil || !strings.Contains(card.Asked, "never ends "+asks+", which waits on you") || !strings.Contains(card.Asked, idle) {
		t.Fatalf("card = %+v", card)
	}
	h.ticks(6, 5*time.Minute) // 35 minutes in all
	if _, ok := h.sup.st.Runtime[idle]; !ok {
		t.Fatal("yielded before yield_after")
	}
	h.ticks(8, 5*time.Minute)
	if _, ok := h.sup.st.Runtime[idle]; ok {
		t.Fatalf("not yielded after yield_after: place %+v", h.place("!1"))
	}
	if _, ok := h.sup.st.Runtime[asks]; !ok {
		t.Fatal("the one that waits on the owner was ended")
	}
	if _, ok := h.sup.st.Runtime[judgmentName("!1", KindJudge)]; !ok {
		t.Fatalf("no judge after the yield: steps %v", h.loopSteps("!1"))
	}
	var notices []string
	for _, e := range h.events() {
		if e.Kind == KindMessage && e.Message.From == "agentd" && strings.Contains(e.Message.Text, "paused") {
			notices = append(notices, e.Message.Text)
		}
	}
	if len(notices) != 1 || !strings.Contains(notices[0], idle) || !strings.Contains(notices[0], "judge") || !strings.Contains(notices[0], "workdir and worktree stay") {
		t.Fatalf("notices = %q", notices)
	}
}

// An idle holder that holds an owner item is never yielded.
func TestLoopNeverYieldsAnItemsHolder(t *testing.T) {
	h, _, idle := loopAtMax(t)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Sessions.YieldAfter = Duration{time.Hour}
	h.cfg.Repositories["widgets"] = w
	if h.sup.st.Work == nil {
		h.sup.st.Work = map[string]*WorkItem{}
	}
	now := h.clock.Now()
	h.sup.st.Work["W1"] = &WorkItem{ID: "W1", Repository: "widgets", Session: idle, State: WorkAssigned, Opened: now, Progress: now, Assigned: now, Typed: now, Acked: now, Escalated: map[string]bool{}}
	h.status(idle, "# Status\n\n## Now\nW1: painting the shed.\n")
	h.ticks(1, 5*time.Minute)
	var card *LevelDecision
	for _, n := range requestNeeds(h.cfg, h.sup.st, "widgets", h.clock.Now()) {
		if n.Entry == entryLoop+"!1" {
			d := n.LevelDecision
			card = &d
		}
	}
	if card == nil || !strings.Contains(card.Asked, idle+", which holds an owner item") {
		t.Fatalf("card = %+v", card)
	}
	h.ticks(20, 5*time.Minute)
	if _, ok := h.sup.st.Runtime[idle]; !ok {
		t.Fatalf("a holder of an owner item was yielded: %+v", h.sup.st.Work["W1"])
	}
	if p := h.place("!1"); p.Phase != PlaceWaitPlace {
		t.Fatalf("place = %+v", p)
	}
}
