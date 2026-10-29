package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

// wideFinding is a review finding that widens the scope, at path.
func wideFinding(title, path string) map[string]any {
	return map[string]any{"title": title, "severity": "low", "widens_scope": true, "items": []any{}, "paths": []any{path},
		"consequence": "None for spinning.", "suggestion": "File it."}
}

// deferBoth runs !1 through a review with two findings that widen the
// scope, F1 and F2, and a judge that raises a decision for each and
// defers both, then writes DONE.
func deferBoth(t *testing.T, h *harness) string {
	t.Helper()
	h.ticks(5, 5*time.Minute)
	if text, refused := h.callAs(judgmentName("!1", KindReviewer), ToolReviewFindings, map[string]any{"findings": []any{
		wideFinding("Add a widget catalogue", "catalogue.go"), wideFinding("Log every spin", "log.go")}}); refused {
		t.Fatalf("review refused: %s", text)
	}
	h.ticks(3, 5*time.Minute)
	judge := judgmentName("!1", KindJudge)
	if _, ok := h.sup.st.Runtime[judge]; !ok {
		t.Fatalf("no judge started: %v", h.loopSteps("!1"))
	}
	for _, id := range []string{"F1", "F2"} {
		if text, refused := h.callAs(judge, ToolRaiseDecision, deferDecision(id)); refused {
			t.Fatalf("raise for %s refused: %s", id, text)
		}
	}
	if text, refused := h.callAs(judge, ToolJudgeDispositions, deferBothArgs()); refused {
		t.Fatalf("judge refused: %s", text)
	}
	h.sup.track(judge).State = Done
	h.ticks(3, 5*time.Minute)
	return judge
}

func deferBothArgs() map[string]any {
	return map[string]any{"dispositions": []any{
		map[string]any{"finding_id": "F1", "disposition": DispositionDefer, "reason": "It widens the scope; the owner decides."},
		map[string]any{"finding_id": "F2", "disposition": DispositionDefer, "reason": "It widens the scope; the owner decides."}},
		"plan": []any{}}
}

// deferredCard is the request card of !1 for the decision of a deferred
// finding, "d:…", or fails.
func (h *harness) deferredCard() repoNeed {
	h.t.Helper()
	for _, n := range requestNeeds(h.sup.cfg, h.sup.st, "widgets", h.clock.Now()) {
		if n.Request && strings.HasPrefix(n.Entry, entryDecision) {
			return n
		}
	}
	h.t.Fatalf("no d: card for !1: %+v; place %+v", requestNeeds(h.sup.cfg, h.sup.st, "widgets", h.clock.Now()), h.place("!1"))
	return repoNeed{}
}

// answerCard answers !1's deferred card through the page, with choice
// or, when choice is "", in the owner's own words.
func (h *harness) answerCard(entry, choice, text string) error {
	src := newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a")
	src.now = h.clock.Now
	return src.respond("widgets", entry, "", text, &AnswerPayload{Choice: choice}, nil)
}

// countSteps counts ref's loop steps of a kind.
func (h *harness) countSteps(ref, step string) int {
	n := 0
	for _, s := range h.loopSteps(ref) {
		if s == step {
			n++
		}
	}
	return n
}

// A judge's defer needs its decision, and a round with a deferred finding
// whose decision is unanswered waits for the owner, read from the log:
// a restart neither reviews it again nor counts it converged (!1280).
func TestLoopDeferredFindingWaitsAcrossRestart(t *testing.T) {
	h, api := loopHarness(t)
	h.ticks(5, 5*time.Minute)
	if text, refused := h.callAs(judgmentName("!1", KindReviewer), ToolReviewFindings, map[string]any{"findings": []any{
		wideFinding("Add a widget catalogue", "catalogue.go"), wideFinding("Log every spin", "log.go")}}); refused {
		t.Fatalf("review refused: %s", text)
	}
	h.ticks(3, 5*time.Minute)
	judge := judgmentName("!1", KindJudge)
	long := deferDecision("F1")
	long["context"] = strings.Repeat("x", maxDecisionField+1)
	if text, refused := h.callAs(judge, ToolRaiseDecision, long); !refused || !strings.Contains(text, "1000 characters") {
		t.Fatalf("a long context was raised: %s", text)
	}
	if text, refused := h.callAs(judge, ToolJudgeDispositions, deferBothArgs()); !refused || !strings.Contains(text, "raise_decision") {
		t.Fatalf("a defer with no decision was accepted: %s", text)
	}
	for _, id := range []string{"F1", "F2"} {
		if text, refused := h.callAs(judge, ToolRaiseDecision, deferDecision(id)); refused {
			t.Fatalf("raise for %s refused: %s", id, text)
		}
	}
	if text, refused := h.callAs(judge, ToolJudgeDispositions, deferBothArgs()); refused {
		t.Fatalf("judge refused: %s", text)
	}
	h.sup.track(judge).State = Done
	h.ticks(3, 5*time.Minute)
	check := func(when string) {
		t.Helper()
		if p := h.place("!1"); p.Phase != PlaceOwner || !strings.Contains(p.Reason, "finding F1 of round 1 is deferred to you (entry d:") {
			t.Fatalf("%s: place = %+v", when, p)
		}
		if n, a := h.countSteps("!1", LoopReview), h.countSteps("!1", LoopApprove); n != 1 || a != 0 {
			t.Fatalf("%s: steps %v", when, h.loopSteps("!1"))
		}
		if c := h.deferredCard(); !strings.Contains(c.Entry, judge) || c.Decision == nil || c.Decision.Finding != "F1" || len(c.Options) != 3 {
			t.Fatalf("%s: card %+v", when, c)
		}
	}
	check("before the restart")
	h.restart(false)
	h.sup.api = api
	h.sup.refreshRuntime()
	h.ticks(3, 5*time.Minute)
	check("after the restart")
	if len(h.forge.merges) != 0 {
		t.Fatalf("merged with findings deferred: %+v", h.forge.merges)
	}
}

// The log of !1280 as the old build wrote it: a judge that deferred two
// findings with no decision raised. Its round waits for the owner before
// and after a restart, and is neither reviewed again nor approved.
func TestLoopReplayed1280Waits(t *testing.T) {
	h, api := loopHarness(t)
	h.ticks(5, 5*time.Minute)
	if text, refused := h.callAs(judgmentName("!1", KindReviewer), ToolReviewFindings, map[string]any{"findings": []any{
		wideFinding("Add a widget catalogue", "catalogue.go"), wideFinding("Log every spin", "log.go")}}); refused {
		t.Fatalf("review refused: %s", text)
	}
	h.ticks(3, 5*time.Minute)
	judge := judgmentName("!1", KindJudge)
	NewLogWriter(h.buf).Append(Event{V: EventVersion, Time: h.clock.Now(), Session: judge, Kind: KindCase, Mode: ModeShared,
		Case: &CasePayload{Case: widgetsCase, Call: &CaseCall{Tool: ToolJudgeDispositions, Hash: "old1280", Dispositions: &CaseDispositions{Round: 1, Dispositions: []Disposition{
			{FindingID: "F1", Disposition: DispositionDefer, Reason: "Out of scope."}, {FindingID: "F2", Disposition: DispositionDefer, Reason: "Out of scope."}}}}}})
	h.restart(false)
	h.sup.api = api
	h.sup.refreshRuntime()
	check := func(when string) {
		t.Helper()
		if p := h.place("!1"); p.Phase != PlaceOwner || !strings.Contains(p.Reason, "finding F1 of round 1 is deferred to you") || !strings.Contains(p.Reason, "no decision") {
			t.Fatalf("%s: place = %+v", when, p)
		}
		if n, a := h.countSteps("!1", LoopReview), h.countSteps("!1", LoopApprove); n != 1 || a != 0 {
			t.Fatalf("%s: steps %v", when, h.loopSteps("!1"))
		}
	}
	h.ticks(3, 5*time.Minute)
	check("before the restart")
	h.restart(false)
	h.sup.api = api
	h.sup.refreshRuntime()
	h.ticks(3, 5*time.Minute)
	check("after the restart")
}

// The owner's fix on a deferred finding's decision is a fix round for
// the work session, as an adopted finding is, and only the new head is
// reviewed (!1281); an answer in the owner's own words leaves the round
// waiting.
func TestLoopDeferredAnswerFix(t *testing.T) {
	h, api := loopHarness(t)
	deferBoth(t, h)
	c := h.deferredCard()
	if err := h.answerCard(c.Entry, "", "Hmm, not sure."); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, 5*time.Minute)
	if p := h.place("!1"); p.Phase != PlaceOwner || !strings.Contains(p.Reason, "finding F1 of round 1 is deferred to you") {
		t.Fatalf("own words: place = %+v", p)
	}
	if err := h.answerCard(h.deferredCard().Entry, FindingFix, "extend the test in this MR"); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, 5*time.Minute)
	if p := h.place("!1"); p.Phase != PlaceOwner || !strings.Contains(p.Reason, "finding F2 of round 1 is deferred to you") {
		t.Fatalf("one answered: place = %+v", p)
	}
	if err := h.answerCard(h.deferredCard().Entry, FindingMerge, ""); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, 5*time.Minute)
	if n := h.count(KindAction, ActFixRound, "d"); n != 1 {
		t.Fatalf("fix rounds = %d; steps %v; place %+v", n, h.loopSteps("!1"), h.place("!1"))
	}
	if cmd := h.lastLaunch("d"); !strings.Contains(cmd, `"owner":"fix"`) || !strings.Contains(cmd, "Add a widget catalogue") {
		t.Fatalf("fix round facts: %s", cmd)
	}
	h.ticks(3, 5*time.Minute)
	if got := strings.Join(h.loopSteps("!1"), ","); got != "review,judge,fix" {
		t.Fatalf("reviewed before the fix was delivered: %s", got)
	}
	if p := h.place("!1"); p.Phase != PlaceFixing {
		t.Fatalf("place = %+v", p)
	}
	h.git.head = "c0ffe2"
	h.status("d", "# Fixed\n\nDONE\n")
	api.pipelines("c0ffe2", "success", nil, "success", nil)
	h.ticks(12, 5*time.Minute)
	reviewer := judgmentName("!1", KindReviewer)
	if _, ok := h.sup.st.Runtime[reviewer]; !ok || !strings.Contains(h.lastLaunch(reviewer), `"head":"c0ffe2"`) {
		t.Fatalf("no review of the new head: steps %v, place %+v", h.loopSteps("!1"), h.place("!1"))
	}
	if got := strings.Join(h.loopSteps("!1"), ","); got != "review,judge,fix,review" {
		t.Fatalf("steps = %s", got)
	}
}

// The owner's merge on every deferred finding converges the round at the
// head reviewed, with no further review; an issue answer converges too
// and, where delivery.allow lacks issue, tells the owner the issue to
// file.
func TestLoopDeferredAnswerMerge(t *testing.T) {
	h, _ := loopHarness(t)
	deferBoth(t, h)
	for _, k := range []string{FindingMerge, FindingIssue} {
		if err := h.answerCard(h.deferredCard().Entry, k, ""); err != nil {
			t.Fatal(err)
		}
		h.ticks(1, 5*time.Minute)
	}
	h.ticks(6, 5*time.Minute)
	if len(h.forge.merges) != 1 || h.forge.merges[0].ref != "!1" || h.forge.merges[0].head != "c0ffee" {
		t.Fatalf("merges = %+v; skips %v; place %+v", h.forge.merges, h.mergeSkips(), h.place("!1"))
	}
	if got := strings.Join(h.loopSteps("!1"), ","); got != "review,judge" {
		t.Fatalf("steps = %s", got)
	}
	if len(h.forge.issues) != 0 {
		t.Fatalf("opened an issue delivery.allow does not permit: %v", h.forge.issues)
	}
	told := 0
	for _, e := range h.events() {
		if e.Kind == KindMessage && strings.Contains(e.Message.Text, "F2") && strings.Contains(e.Message.Text, "Log every spin") && strings.Contains(e.Message.Text, "log.go") {
			told++
		}
	}
	if told != 1 {
		t.Fatalf("the owner was told the issue to file %d times", told)
	}
}

// An issue answer opens the follow-up issue where delivery.allow permits
// it, naming the request, the finding and its paths, once.
func TestLoopDeferredAnswerIssueOpens(t *testing.T) {
	h, api := loopHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Delivery.Allow = append(w.Policy.Delivery.Allow, ActIssue)
	h.cfg.Repositories["widgets"] = w
	h.restart(false)
	h.sup.api = api
	deferBoth(t, h)
	for _, k := range []string{FindingIssue, FindingMerge} {
		if err := h.answerCard(h.deferredCard().Entry, k, ""); err != nil {
			t.Fatal(err)
		}
		h.ticks(1, 5*time.Minute)
	}
	h.ticks(4, 5*time.Minute)
	h.restart(false)
	h.sup.api = api
	h.ticks(4, 5*time.Minute)
	if len(h.forge.issues) != 1 || !strings.Contains(h.forge.issues[0], "!1") || !strings.Contains(h.forge.issues[0], "F1") || !strings.Contains(h.forge.issues[0], "catalogue.go") {
		t.Fatalf("issues = %q", h.forge.issues)
	}
	if len(h.forge.merges) != 1 {
		t.Fatalf("merges = %+v; place %+v", h.forge.merges, h.place("!1"))
	}
}

// deferOne runs ref, a request no session delivered, through a review
// with one finding that widens the scope and a judge that raises its
// decision and defers it, then writes DONE.
func deferOne(t *testing.T, h *harness, ref string) {
	t.Helper()
	h.ticks(3, 5*time.Minute)
	if text, refused := h.callAs(judgmentName(ref, KindReviewer), ToolReviewFindings, map[string]any{"findings": []any{
		wideFinding("Add a widget catalogue", "catalogue.go")}}); refused {
		t.Fatalf("review refused: %s; steps %v", text, h.loopSteps(ref))
	}
	h.ticks(3, 5*time.Minute)
	judge := judgmentName(ref, KindJudge)
	if text, refused := h.callAs(judge, ToolRaiseDecision, deferDecision("F1")); refused {
		t.Fatalf("raise refused: %s; steps %v", text, h.loopSteps(ref))
	}
	if text, refused := h.callAs(judge, ToolJudgeDispositions, map[string]any{"dispositions": []any{
		map[string]any{"finding_id": "F1", "disposition": DispositionDefer, "reason": "It widens the scope; the owner decides."}}, "plan": []any{}}); refused {
		t.Fatalf("judge refused: %s", text)
	}
	h.sup.track(judge).State = Done
	h.ticks(3, 5*time.Minute)
}

// fixContinued answers !2's deferred finding fix, and checks the fix
// round starts on name, which continues !2, with the round in its facts;
// then name pushes a fix to !2's branch and writes DONE, and the new head
// is reviewed.
func fixContinued(t *testing.T, h *harness, api *fakeForgeAPI, name string) {
	t.Helper()
	deferOne(t, h, "!2")
	if err := h.answerCard(h.deferredCard().Entry, FindingFix, ""); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, 5*time.Minute)
	if n := h.count(KindAction, ActFixRound, name); n != 1 {
		t.Fatalf("fix rounds = %d; steps %v; place %+v", n, h.loopSteps("!2"), h.place("!2"))
	}
	if cmd := h.lastLaunch(name); !strings.Contains(cmd, `"owner":"fix"`) || !strings.Contains(cmd, "Add a widget catalogue") {
		t.Fatalf("fix round facts: %s", cmd)
	}
	if p := h.place("!2"); p.Phase != PlaceFixing {
		t.Fatalf("place = %+v", p)
	}
	outsideMR(api, outsideMRState{head: "5eed0102", pipeline: 61, status: "success"})
	h.statusOf(name, "# Fixed\n\nDONE\n")
	h.ticks(6, 5*time.Minute)
	reviewer := judgmentName("!2", KindReviewer)
	if _, ok := h.sup.st.Runtime[reviewer]; !ok || !strings.Contains(h.lastLaunch(reviewer), `"head":"5eed0102"`) {
		t.Fatalf("no review of the new head: steps %v, place %+v", h.loopSteps("!2"), h.place("!2"))
	}
	if got := strings.Join(h.loopSteps("!2"), ","); got != "review,judge,fix,review" {
		t.Fatalf("steps = %s", got)
	}
}

// statusOf writes the STATUS.md of a configured or started session.
func (h *harness) statusOf(name, text string) {
	h.t.Helper()
	sess, ok := withRuntime(h.sup.cfg, h.sup.st.Runtime).Sessions[name]
	if !ok {
		h.t.Fatalf("no session %s", name)
	}
	if err := os.WriteFile(filepath.Join(sess.Workdir, "STATUS.md"), []byte(text), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// A request the owner opened, which no session delivered but a work
// session continues on its source branch, takes the fix answer as a fix
// round on that session, and its new head is reviewed (!1269).
func TestLoopContinuedRequestFixRound(t *testing.T) {
	h, api := loopHarness(t)
	d := h.cfg.Sessions["d"]
	d.Branch = "fix-squeak"
	h.cfg.Sessions["d"] = d
	outsideOpen(api)
	h.sup.emit("", Event{Kind: KindIndex, Index: &IndexPayload{Repository: "widgets", Ref: "!2", Title: "Fix the squeak", State: "open", Branch: "fix-squeak"}})
	fixContinued(t, h, api, "d")
}

// A work session whose open item names the request continues it too.
func TestLoopContinuedRequestByFacts(t *testing.T) {
	h, api := loopHarness(t)
	outsideOpen(api)
	h.sup.emit("", Event{Kind: KindWorkItem, Work: &WorkPayload{ID: "W2", Step: WorkStepOpen, Repository: "widgets", Instruction: "I2",
		Work: &ThreadWork{Title: "Finish the squeak fix on !2", Brief: "Push the fix to !2's branch.", DoneWhen: EndCheck{Kind: DoneOwner}}}})
	h.sup.emit("", Event{Kind: KindWorkItem, Work: &WorkPayload{ID: "W2", Step: WorkStepAssign, Session: "d"}})
	h.status("d", "# Pushed\n\nW2 done: pushed to !2's branch\n\nDONE\n")
	h.ticks(1, 5*time.Minute)
	if its := h.sup.st.itemsOf("d"); len(its) != 1 {
		t.Fatalf("d's items: %+v", h.item("W2"))
	}
	fixContinued(t, h, api, "d")
}

// adoptOutside runs !2, which no session delivered nor continues, its
// goal recorded, through a review with one finding and a judge that
// adopts it.
func adoptOutside(t *testing.T, h *harness, api *fakeForgeAPI) {
	t.Helper()
	h.ticks(3, 5*time.Minute)
	if err := RecordGoal(NewLogWriter(h.buf), withRuntime(h.cfg, h.sup.st.Runtime), h.logState(), h.clock.Now(), "gitlab/example-org/widgets!2", widgetsBody); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	h.sup.api = api
	if text, refused := h.callAs(judgmentName("!2", KindReviewer), ToolReviewFindings, map[string]any{"findings": []any{finding("Spin is untested")}}); refused {
		t.Fatalf("review refused: %s", text)
	}
	h.ticks(3, 5*time.Minute)
	if text, refused := h.callAs(judgmentName("!2", KindJudge), ToolJudgeDispositions, map[string]any{"dispositions": []any{
		map[string]any{"finding_id": "F1", "disposition": DispositionAdopt, "reason": "A spin test is cheap."}}}); refused {
		t.Fatalf("judge refused: %s", text)
	}
	h.ticks(3, 5*time.Minute)
}

// An outside request's adopted findings wait for its author, and the
// head the author pushes next is reviewed.
func TestLoopOutsideFixHeadMovedReviewed(t *testing.T) {
	h, api := loopHarness(t)
	outsideOpen(api)
	adoptOutside(t, h, api)
	outsideMR(api, outsideMRState{head: "5eed0102", pipeline: 61, status: "success"})
	h.ticks(4, 5*time.Minute)
	reviewer := judgmentName("!2", KindReviewer)
	if _, ok := h.sup.st.Runtime[reviewer]; !ok || !strings.Contains(h.lastLaunch(reviewer), `"head":"5eed0102"`) {
		t.Fatalf("no review of the new head: steps %v, place %+v", h.loopSteps("!2"), h.place("!2"))
	}
	if got := strings.Join(h.loopSteps("!2"), ","); got != "review,judge,review" {
		t.Fatalf("steps = %s", got)
	}
	if n := h.count(KindAction, ActFixRound, ""); n != 0 {
		t.Fatalf("fix rounds = %d", n)
	}
}

// Until the author pushes, the request waits for them, and says the new
// head is reviewed.
func TestLoopOutsideFixHeadUnchangedWaits(t *testing.T) {
	h, api := loopHarness(t)
	outsideOpen(api)
	adoptOutside(t, h, api)
	h.ticks(4, 5*time.Minute)
	if p := h.place("!2"); p.Phase != PlaceOwner || !strings.Contains(p.Reason, "round 1 adopted findings; waiting for the author to push; the new head is reviewed") {
		t.Fatalf("place = %+v", p)
	}
	if n := h.countSteps("!2", LoopReview); n != 1 {
		t.Fatalf("reviews = %d; steps %v", n, h.loopSteps("!2"))
	}
}

// deferMany runs !1 through a review with a finding that widens the
// scope for each title, F1 on, and a judge that raises a decision for
// each and defers them all, then writes DONE.
func deferMany(t *testing.T, h *harness, titles ...string) {
	t.Helper()
	h.ticks(5, 5*time.Minute)
	var fs, ds []any
	for i, title := range titles {
		fs = append(fs, wideFinding(title, "f"+strconv.Itoa(i+1)+".go"))
		ds = append(ds, map[string]any{"finding_id": "F" + strconv.Itoa(i+1), "disposition": DispositionDefer, "reason": "It widens the scope; the owner decides."})
	}
	if text, refused := h.callAs(judgmentName("!1", KindReviewer), ToolReviewFindings, map[string]any{"findings": fs}); refused {
		t.Fatalf("review refused: %s", text)
	}
	h.ticks(3, 5*time.Minute)
	judge := judgmentName("!1", KindJudge)
	for i := range titles {
		if text, refused := h.callAs(judge, ToolRaiseDecision, deferDecision("F"+strconv.Itoa(i+1))); refused {
			t.Fatalf("raise for F%d refused: %s", i+1, text)
		}
	}
	if text, refused := h.callAs(judge, ToolJudgeDispositions, map[string]any{"dispositions": ds, "plan": []any{}}); refused {
		t.Fatalf("judge refused: %s", text)
	}
	h.sup.track(judge).State = Done
	h.ticks(3, 5*time.Minute)
}

// deferredEntries is the console entries of the decisions of the latest
// round of case id's deferred findings, F1 first.
func (h *harness) deferredEntries(id string) []string {
	h.t.Helper()
	c := h.sup.st.Cases[id]
	if c == nil || len(c.Rounds) == 0 {
		h.t.Fatalf("no rounds in %s", id)
	}
	var out []string
	for _, d := range c.deferred(h.sup.st, &c.Rounds[len(c.Rounds)-1]) {
		out = append(out, d.Entry)
	}
	return out
}

// answerAsks answers the decision at entry with choice and the owner's
// words through the page, and the thread agent then asks for work on
// ref for it, as it did for each answer on the night of #76. With
// replay, the agent's answer is written to the log, as the build before
// the fix wrote it, rather than emitted.
func (h *harness) answerAsks(entry, choice, words, ref string, replay bool) {
	h.t.Helper()
	if err := h.answerCard(entry, choice, words); err != nil {
		h.t.Fatal(err)
	}
	events, _, err := ReadEvents(bytes.NewReader(h.buf.Bytes()))
	if err != nil {
		h.t.Fatal(err)
	}
	id := ""
	for _, e := range events {
		if e.Kind == KindInstruction && e.Instruction.Re != nil && e.Instruction.Re.ID == entry {
			id = e.Instruction.ID
		}
	}
	if id == "" {
		h.t.Fatalf("no instruction on %s", entry)
	}
	w := ThreadWork{Title: "Fix the judge's finding on " + ref, Brief: "Answer " + choice + " on " + entry + ": do it on " + ref + ".",
		DoneWhen: EndCheck{Kind: DoneForge, Ref: ref, State: "merged"}}
	routes := []RoutePayload{{Instruction: id, Step: RouteAgent},
		{Instruction: id, Step: RouteAnswered, Agent: &AgentPayload{Answer: "Go holds it as a work item until it is done.", Work: &w}}}
	if replay {
		for i := range routes {
			h.appendEvent(Event{Kind: KindRoute, Mode: ModeLive, Route: &routes[i]})
		}
		return
	}
	h.sup.ownerRecords()
	for i := range routes {
		h.sup.emit("", Event{Kind: KindRoute, Route: &routes[i]})
	}
}

// allow adds actions to widgets' delivery.allow and restarts agentd.
func (h *harness) allow(api *fakeForgeAPI, actions ...string) {
	w := h.cfg.Repositories["widgets"]
	w.Policy.Delivery.Allow = append(w.Policy.Delivery.Allow, actions...)
	h.cfg.Repositories["widgets"] = w
	h.restart(false)
	h.sup.api = api
}

// The owner's fix on a delivered request's deferred finding, which the
// thread agent took for work, opens no item: the work session's fix round
// carries it, with the decision's question and the owner's words (#76).
func TestLoopAnswerFixDeliveredOpensNoItem(t *testing.T) {
	h, _ := loopHarness(t)
	deferBoth(t, h)
	es := h.deferredEntries(widgetsCase)
	h.answerAsks(es[0], FindingFix, "extend the test in this MR", "!1", false)
	h.ticks(2, 5*time.Minute)
	h.answerAsks(es[1], FindingMerge, "", "!1", false)
	h.ticks(4, 5*time.Minute)
	if len(h.sup.st.Work) != 0 {
		t.Fatalf("items opened: %+v", h.sup.st.Work)
	}
	if n := h.count(KindAction, ActFixRound, "d"); n != 1 {
		t.Fatalf("fix rounds = %d; steps %v; place %+v", n, h.loopSteps("!1"), h.place("!1"))
	}
	if cmd := h.lastLaunch("d"); !strings.Contains(cmd, `"question":"Fix F1 in this request?"`) || !strings.Contains(cmd, `"words":"extend the test in this MR"`) {
		t.Fatalf("fix round facts lack the decision's words: %s", cmd)
	}
}

// A request a work session continues takes the fix as that session's
// fix round, and opens no item either.
func TestLoopAnswerFixContinuedOpensNoItem(t *testing.T) {
	h, api := loopHarness(t)
	d := h.cfg.Sessions["d"]
	d.Branch = "fix-squeak"
	h.cfg.Sessions["d"] = d
	outsideOpen(api)
	h.sup.emit("", Event{Kind: KindIndex, Index: &IndexPayload{Repository: "widgets", Ref: "!2", Title: "Fix the squeak", State: "open", Branch: "fix-squeak"}})
	deferOne(t, h, "!2")
	h.answerAsks(h.deferredEntries("gitlab/example-org/widgets!2")[0], FindingFix, "do it here", "!2", false)
	h.ticks(4, 5*time.Minute)
	if len(h.sup.st.Work) != 0 {
		t.Fatalf("items opened: %+v", h.sup.st.Work)
	}
	if n := h.count(KindAction, ActFixRound, "d"); n != 1 {
		t.Fatalf("fix rounds = %d; steps %v; place %+v", n, h.loopSteps("!2"), h.place("!2"))
	}
}

// A merge answer opens no item: the loop converges and merges.
func TestLoopAnswerMergeOpensNoItem(t *testing.T) {
	h, _ := loopHarness(t)
	deferBoth(t, h)
	for _, e := range h.deferredEntries(widgetsCase) {
		h.answerAsks(e, FindingMerge, "", "!1", false)
		h.ticks(1, 5*time.Minute)
	}
	h.ticks(6, 5*time.Minute)
	if len(h.sup.st.Work) != 0 {
		t.Fatalf("items opened: %+v", h.sup.st.Work)
	}
	if len(h.forge.merges) != 1 {
		t.Fatalf("merges = %+v; place %+v", h.forge.merges, h.place("!1"))
	}
}

// Where delivery.allow has issue, an issue answer opens no item: agentd
// opens the follow-up once and names it on the request once.
func TestLoopAnswerIssueNoItemWithAllow(t *testing.T) {
	h, api := loopHarness(t)
	h.allow(api, ActIssue, ActComment)
	deferBoth(t, h)
	es := h.deferredEntries(widgetsCase)
	h.answerAsks(es[0], FindingIssue, "", "!1", false)
	h.ticks(1, 5*time.Minute)
	h.answerAsks(es[1], FindingMerge, "", "!1", false)
	h.ticks(4, 5*time.Minute)
	h.restart(false)
	h.sup.api = api
	h.ticks(4, 5*time.Minute)
	if len(h.sup.st.Work) != 0 {
		t.Fatalf("items opened: %+v", h.sup.st.Work)
	}
	if len(h.forge.issues) != 1 {
		t.Fatalf("issues = %q", h.forge.issues)
	}
	var notes []string
	for _, c := range h.forge.comments {
		if strings.HasPrefix(c, "!1 ") && strings.Contains(c, "#") && strings.Contains(c, "F1") {
			notes = append(notes, c)
		}
	}
	if len(notes) != 1 {
		t.Fatalf("comments naming the follow-up on !1: %q", h.forge.comments)
	}
}

// Without issue in delivery.allow, a round's issue answers open one item
// between them.
func TestLoopAnswerIssueOneItemWithoutAllow(t *testing.T) {
	h, _ := loopHarness(t)
	deferBoth(t, h)
	for _, e := range h.deferredEntries(widgetsCase) {
		h.answerAsks(e, FindingIssue, "", "!1", false)
		h.ticks(2, 5*time.Minute)
	}
	h.ticks(2, 5*time.Minute)
	if len(h.sup.st.Work) != 1 {
		t.Fatalf("items opened: %d: %+v", len(h.sup.st.Work), h.sup.st.Work)
	}
}

// A request no session delivered nor continues opens no item for a fix:
// agentd comments the decision on it to its author, once, where
// delivery.allow has comment, or tells the owner once where it lacks it,
// and waits for the push.
func TestLoopOutsideFixCommentsAuthor(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		h, api := loopHarness(t)
		if allowed {
			h.allow(api, ActComment)
		}
		outsideOpen(api)
		deferOne(t, h, "!2")
		h.answerAsks(h.deferredEntries("gitlab/example-org/widgets!2")[0], FindingFix, "please add the catalogue here", "!2", false)
		h.ticks(3, 5*time.Minute)
		h.restart(false)
		h.sup.api = api
		h.ticks(3, 5*time.Minute)
		if len(h.sup.st.Work) != 0 {
			t.Fatalf("allowed %v: items opened: %+v", allowed, h.sup.st.Work)
		}
		var asks []string
		for _, c := range h.forge.comments {
			if strings.HasPrefix(c, "!2 ") {
				asks = append(asks, c)
			}
		}
		told := 0
		for _, e := range h.events() {
			if e.Kind == KindMessage && strings.Contains(e.Message.Text, "!2") && strings.Contains(e.Message.Text, "please add the catalogue here") {
				told++
			}
		}
		switch {
		case allowed && (len(asks) != 1 || !strings.Contains(asks[0], "widget-bot") || !strings.Contains(asks[0], "Add a widget catalogue") || !strings.Contains(asks[0], "please add the catalogue here")):
			t.Fatalf("comments on !2: %q", h.forge.comments)
		case !allowed && (len(asks) != 0 || told != 1):
			t.Fatalf("without comment: comments %q, told %d", h.forge.comments, told)
		}
		if p := h.place("!2"); p.Phase != PlaceOwner || !strings.Contains(p.Reason, "waiting for the author to push") {
			t.Fatalf("place = %+v", p)
		}
	}
}

// Three answers in one batch, no pass between them, fold into one fix
// round whose facts list each decision, and open no item.
func TestLoopBatchAnswersOneFixRound(t *testing.T) {
	h, _ := loopHarness(t)
	deferMany(t, h, "Add a widget catalogue", "Log every spin", "Count the squeaks")
	for i, e := range h.deferredEntries(widgetsCase) {
		h.answerAsks(e, FindingFix, "words "+strconv.Itoa(i+1), "!1", false)
	}
	h.ticks(4, 5*time.Minute)
	if len(h.sup.st.Work) != 0 {
		t.Fatalf("items opened: %+v", h.sup.st.Work)
	}
	if n := h.count(KindAction, ActFixRound, "d"); n != 1 {
		t.Fatalf("fix rounds = %d; steps %v; place %+v", n, h.loopSteps("!1"), h.place("!1"))
	}
	cmd := h.lastLaunch("d")
	for _, id := range []string{"1", "2", "3"} {
		if !strings.Contains(cmd, `"question":"Fix F`+id+` in this request?"`) || !strings.Contains(cmd, `"words":"words `+id+`"`) {
			t.Fatalf("fix round facts lack decision F%s: %s", id, cmd)
		}
	}
}

// Tonight's log, as on !1295: three fix answers on one delivered request,
// each taken by the thread agent for work, read back after a restart,
// give one fix round and no item.
func TestLoopReplayedTonightOneFixRound(t *testing.T) {
	h, api := loopHarness(t)
	deferMany(t, h, "Add a widget catalogue", "Log every spin", "Count the squeaks")
	for _, e := range h.deferredEntries(widgetsCase) {
		h.answerAsks(e, FindingFix, "", "!1", true)
	}
	h.restart(false)
	h.sup.api = api
	h.ticks(6, 5*time.Minute)
	if len(h.sup.st.Work) != 0 {
		t.Fatalf("items opened: %+v", h.sup.st.Work)
	}
	if n := h.count(KindAction, ActFixRound, "d"); n != 1 {
		t.Fatalf("fix rounds = %d; steps %v; place %+v", n, h.loopSteps("!1"), h.place("!1"))
	}
}
