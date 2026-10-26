package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requestsHarness is sessionsHarness with the CI watcher on and widgets'
// policy merging widget-bot's requests into main as a fast-forward; the
// forge lists one open request, !2, which no session delivered, by
// widget-bot, green on 5eed0101 at the target's tip: due to merge once
// the loop approves it.
func requestsHarness(t *testing.T) (*harness, *fakeForgeAPI) {
	t.Helper()
	h := sessionsHarness(t)
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
	// The request's target, main when "", whether it is a draft, and
	// when it was last updated, a day before the harness's clock when "".
	target, updated string
	draft           bool
}

// outsideMR sets request !2, its pipeline and the pipeline's jobs; a job
// is "name:status", and the first job's log fails a test.
func outsideMR(api *fakeForgeAPI, s outsideMRState) {
	if s.author == "" {
		s.author = "widget-bot"
	}
	if s.target == "" {
		s.target = "main"
	}
	if s.updated == "" {
		s.updated = "2026-10-21T00:00:00.000Z"
	}
	id := strconv.Itoa(s.pipeline)
	api.set(func() {
		api.gitlab[ciProject+"/merge_requests/2"] = `{"iid":2,"state":"opened","title":"Fix the squeak","description":"","web_url":"https://gitlab.example.com/example-org/widgets/-/merge_requests/2","sha":"` + s.head +
			`","target_branch":"` + s.target + `","draft":` + strconv.FormatBool(s.draft) + `,"updated_at":"` + s.updated + `","author":{"username":"` + s.author + `"},"blocking_discussions_resolved":` + strconv.FormatBool(!s.unresolved) +
			`,"head_pipeline":{"id":` + id + `,"status":"` + s.status + `","sha":"` + s.head + `"}}`
		api.gitlab[ciProject+"/pipelines/"+id+"/jobs"] = gitlabJobs(s.pipeline*10, s.jobs)
		api.gitlab[ciProject+"/merge_requests/2/discussions"] = `[]`
		api.gitlab[ciProject+"/jobs/"+strconv.Itoa(s.pipeline*10+1)+"/trace"] = "2026-10-22T00:00:02.000Z --- FAIL: TestSqueak (0.01s)\n2026-10-22T00:00:02.100Z FAIL\n"
	})
}

// approve is the loop's approval of widgets' ref at sha, as it records
// one when the request's review converged there.
func (h *harness) approve(ref, sha string) {
	h.t.Helper()
	h.sup.emit("", Event{Kind: KindLoop, Loop: &LoopPayload{Repository: "widgets", Ref: ref, Head: sha, Step: LoopApprove}})
}

// A request no session delivered is watched on its repository's requests
// track; its own red pipeline is recorded, with the failing job, and no
// session is relaunched to fix it.
func TestOutsideRedPipelineRecorded(t *testing.T) {
	h, api := requestsHarness(t)
	outsideMR(api, outsideMRState{head: "5eed0101", pipeline: 60, status: "failed", jobs: []string{"test:failed", "lint:success"}})
	h.ticks(3, 5*time.Minute)
	r := h.sup.st.CI[ciKey("widgets", "!2")]
	if r.Session != requestsTrack("widgets") || r.State != CIFailed || r.Verdict != CIOwn || r.Author != "widget-bot" || r.Run != "60" {
		t.Fatalf("record = %+v", r)
	}
	if !reflect.DeepEqual(r.Jobs, []string{"test"}) || !strings.Contains(r.Excerpt, "FAIL: TestSqueak") {
		t.Fatalf("jobs %v, excerpt %q", r.Jobs, r.Excerpt)
	}
	if n := h.count(KindAction, ActCIFix, ""); n != 0 {
		t.Fatalf("ci fixes = %d", n)
	}
	// A forge error and a new thread in between change nothing else.
	api.set(func() { api.gitlabErr = errors.New("connection refused") })
	h.ticks(2, 5*time.Minute)
	api.set(func() { api.gitlabErr = nil })
	outsideMR(api, outsideMRState{head: "5eed0101", pipeline: 60, status: "failed", jobs: []string{"test:failed", "lint:success"}, unresolved: true})
	h.ticks(3, 5*time.Minute)
	if r := h.sup.st.CI[ciKey("widgets", "!2")]; r.State != CIFailed || !r.Unresolved {
		t.Fatalf("record = %+v", r)
	}
	sameAfterReplay(t, h, "after the red request")
}

// A green request no session delivered merges only once the loop
// approves it, at the approved head, by the policy's method.
func TestOutsideApprovedGreenMergesAtApprovedHead(t *testing.T) {
	h, _ := requestsHarness(t)
	h.ticks(3, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merged unapproved: %+v", h.forge.merges)
	}
	if s := h.mergeSkips(); len(s) != 1 || !strings.Contains(s[0], "not converged at 5eed0101") {
		t.Fatalf("skips = %q", s)
	}
	h.approve("!2", "5eed0101")
	h.ticks(3, 5*time.Minute)
	want := []mergedRequest{{ref: "!2", target: "main", head: "5eed0101", method: MergeFastForward}}
	if !reflect.DeepEqual(h.forge.merges, want) {
		t.Fatalf("merges = %+v, want %+v (skips %q)", h.forge.merges, want, h.mergeSkips())
	}
	if got := h.sup.Tracks()[requestsTrack("widgets")].Merges[ciKey("widgets", "!2")]; got != "5eed0101" {
		t.Fatalf("merges recorded = %v", h.sup.Tracks()[requestsTrack("widgets")].Merges)
	}
	sameAfterReplay(t, h, "after the outside merge")
}

// A push after the approval holds the merge until the new head is
// approved.
func TestOutsidePushAfterApprovalNeedsNewApproval(t *testing.T) {
	h, api := requestsHarness(t)
	h.ticks(2, 5*time.Minute)
	h.approve("!2", "5eed0101")
	outsideMR(api, outsideMRState{head: "5eed0202", pipeline: 61, status: "success"})
	h.ticks(3, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merged a head never approved: %+v", h.forge.merges)
	}
	h.approve("!2", "5eed0202")
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
	if r := h.sup.st.CI[ciKey("widgets", "!2")]; !r.Unresolved || h.sup.st.approvedAt("widgets", "!2") != "5eed0101" {
		t.Fatalf("record = %+v", r)
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
	// The answer's text is withheld from the log; the fact is kept.
	h.cfg.ConfidentialPatterns = []string{"(?i)rules configuration"}
	h.restart(false)
	h.sup.api = api
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
	marked := 0
	for _, e := range h.events() {
		if e.Kind == KindError && e.Error.Act != nil && e.Error.Act.Action == ActRetest && e.Error.EmptyPipeline && !strings.Contains(e.Error.Error, "would have been empty") {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("withheld retest errors marked empty = %d", marked)
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

// A request by an author the policy does not list is watched, approved
// or not, and never merged.
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
	if r := h.sup.st.CI[ciKey("widgets", "!2")]; r.Author != "someone-else" {
		t.Fatalf("record = %+v", r)
	}
}

// An approval event of an earlier version, by a coordinator, is read and
// counts for nothing (#53).
func TestOldApprovalIgnored(t *testing.T) {
	h, _ := requestsHarness(t)
	h.ticks(2, 5*time.Minute)
	h.sup.emit("c", Event{Kind: KindApproval, Mode: ModeShared, Approval: &ApprovalPayload{Repository: "widgets", Ref: "!2", SHA: "5eed0101"}})
	h.ticks(2, 5*time.Minute)
	if got := h.sup.st.approvedAt("widgets", "!2"); got != "" || len(h.forge.merges) != 0 {
		t.Fatalf("approved at %q, merges %+v", got, h.forge.merges)
	}
}

// An earlier version's coordinator wakes, one with its outcome and one a
// crash cut short, fold to nothing: no pending action, none cut short.
func TestOldWakesReplayed(t *testing.T) {
	cfg := DefaultConfig()
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	wake := func(kind, intent string) Event {
		return Event{V: EventVersion, Time: at, Session: "c", Kind: kind, Mode: ModeLive, Act: &ActPayload{Action: ActWake, Intent: intent, Attempt: 1, Reason: "finished: a"}}
	}
	events := []Event{wake(KindIntent, "i1"), wake(KindAction, "i1"), wake(KindIntent, "i2"),
		{V: EventVersion, Time: at, Session: "c", Kind: KindCoordinator, Mode: ModeShared, Coordinator: &CoordinatorPayload{Repository: "widgets"}}}
	for _, e := range events {
		if err := e.check(); err != nil {
			t.Fatalf("%+v: %v", e, err)
		}
	}
	st := NewLogState(&cfg, ModeLive).Fold(events)
	if c := st.CutShort(); len(c) != 0 {
		t.Fatalf("cut short = %+v", c)
	}
	if tr := st.Tracks["c"]; tr != nil && len(tr.CutShort) != 0 {
		t.Fatalf("track cut short = %v", tr.CutShort)
	}
}

// The keys the coordinator's removal took away are refused at load, each
// naming what replaced it, and a repository's file that still has one is
// told what replaced it (#53).
func TestRemovedCoordinatorKeysRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	for _, c := range []string{
		`{"policies": {"coordinator": {"pack_bytes": 2048}}}`,
		`{"policies": {"models": {"coordinator": "opus"}}}`,
		`{"repositories": {"widgets": {"forge": "gitlab", "project": "example-org/widgets", "coordinator": {"session": "c", "workdir": "/srv/c"}}}}`,
		`{"repositories": {"widgets": {"forge": "gitlab", "project": "example-org/widgets", "policy": {"sessions": {"coordinator_model": "opus"}}}}}`,
	} {
		writeFile(t, path, c)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "removed in #53") {
			t.Errorf("%s: %v", c, err)
		}
	}
	if n := removedNote([]string{"merge.method", "sessions.coordinator_model"}); !strings.Contains(n, "review.judge_model") {
		t.Errorf("note = %q", n)
	}
	if n := removedNote([]string{"merge.method"}); n != "" {
		t.Errorf("note for another key = %q", n)
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
	h.sup.mergeRequest(requestsTrack("widgets"), h.sup.track(requestsTrack("widgets")), ciWatched{Session: requestsTrack("widgets"), Repository: "widgets", Ref: "!2"}, r, true, true)
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
	h := sessionsHarness(t)
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
	if r := h.sup.st.CI[ciKey("widgets", "#2")]; r.Session != requestsTrack("widgets") || r.State != CISuccess || r.Base != mergeTip || r.Run != "21" {
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

// GitLab's answer that a pipeline would be empty is kept in a failed
// retest's error from whichever stream carried it (#46).
func TestRetestErrorKeepsEmptyAnswer(t *testing.T) {
	const body = `{"message":{"base":["The resulting pipeline would have been empty. Review the rules configuration for the relevant jobs."]}}`
	if err := retestError("HTTP 400 Bad Request", body); !emptyPipelineAnswer(err.Error()) {
		t.Fatalf("error = %v", err)
	}
	if err := retestError("HTTP 400", `{"message":{"base":["No stages / jobs for this pipeline."]}}`); !emptyPipelineAnswer(err.Error()) {
		t.Fatalf("older answer: error = %v", err)
	}
	if err := retestError("HTTP 400", `{"message":{"base":["Pipeline will not run for the selected trigger. The rules configuration prevented any jobs from being added to the pipeline."]}}`); !emptyPipelineAnswer(err.Error()) {
		t.Fatalf("rules answer: error = %v", err)
	}
	if err := retestError("HTTP 500", `{"message":"oops"}`); err.Error() != "glab: HTTP 500" {
		t.Fatalf("error = %v", err)
	}
	r := Repository{Forge: "gitlab"}
	r.Policy.Merge.Enabled = true
	a := AllowedForge{Forge: &fakeForge{retestErr: errors.New("glab: The resulting pipeline would have been empty. (HTTP 400)")}, Repos: map[string]Repository{"widgets": r}}
	if err := a.RetestAllowed("widgets", "!2"); !errors.Is(err, errEmptyPipeline) {
		t.Fatalf("error = %v", err)
	}
}
