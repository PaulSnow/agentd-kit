package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// roundsHarness is the MCP harness with a reviewer, rv, and a judge, jd,
// on widgets#7 beside its worker s1.
func roundsHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h, head := mcpHarness(t)
	for name, role := range map[string]string{"rv": RoleReviewer, "jd": RoleJudge} {
		s := h.cfg.Sessions["s1"]
		s.Workdir, s.Cwd, s.Role = filepath.Join(h.dir, name), filepath.Join(h.dir, name, "wt"), role
		h.cfg.Sessions[name] = s
	}
	return h, head
}

// finding is a review_findings finding on A1, at spin.go.
func finding(title string) map[string]any {
	return map[string]any{"title": title, "severity": "medium", "items": []any{"A1"}, "paths": []any{"spin.go"},
		"consequence": "A widget may not spin.", "suggestion": "Test it."}
}

// review makes the reviewer's call and returns its reply decoded.
func review(t *testing.T, h *harness, findings ...map[string]any) (map[string]any, bool) {
	t.Helper()
	list := []any{}
	for _, f := range findings {
		list = append(list, f)
	}
	text, refused := callOne(t, h.mcpServerFor("rv"), ToolReviewFindings, map[string]any{"findings": list})
	var reply map[string]any
	if err := json.Unmarshal([]byte(text), &reply); err != nil {
		t.Fatalf("reply %q: %v", text, err)
	}
	return reply, refused
}

// judge disposes each id with disp.
func judge(t *testing.T, h *harness, disp string, ids ...string) (string, bool) {
	t.Helper()
	var ds []any
	for _, id := range ids {
		ds = append(ds, map[string]any{"finding_id": id, "disposition": disp, "reason": "The spin test covers it."})
	}
	return callOne(t, h.mcpServerFor("jd"), ToolJudgeDispositions, map[string]any{"dispositions": ds,
		"plan": []any{map[string]any{"id": "P1", "step": "Add the spin test."}}, "lessons": []any{"Test spinning first."}})
}

func roundsOf(t *testing.T, h *harness) *Case {
	t.Helper()
	c := h.logState().Cases[widgetsCase]
	if c == nil {
		t.Fatal("no case")
	}
	return c
}

// A finding keeps its id in a second round: named by it, or given again
// with its paths and a title like its own.
func TestRoundsFindingIDSurvivesSecondRound(t *testing.T) {
	h, _ := roundsHarness(t)
	reply, refused := review(t, h, finding("Spin has no test"), finding("Stop is not idempotent"))
	if refused || reply["round"] != 1.0 || !reflect.DeepEqual(reply["ids"], []any{"F1", "F2"}) {
		t.Fatalf("round 1 %v", reply)
	}
	if text, refused := judge(t, h, DispositionAdopt, "F1", "F2"); refused {
		t.Fatalf("judge %s", text)
	}
	again := finding("Spin still has no test")
	again["id"] = "F1"
	reply, refused = review(t, h, again, finding("Stop is not idempotent at all"), finding("Gears grind"))
	if refused || reply["round"] != 2.0 || !reflect.DeepEqual(reply["ids"], []any{"F1", "F2", "F3"}) || !reflect.DeepEqual(reply["new"], []any{"F3"}) {
		t.Fatalf("round 2 %v", reply)
	}
	c := roundsOf(t, h)
	if len(c.Rounds) != 2 || c.Rounds[1].Findings[0].ID != "F1" || c.Rounds[1].Findings[0].Title != "Spin still has no test" || c.RoundStop != nil {
		t.Fatalf("rounds %+v", c.Rounds)
	}
	// An id the case does not have is refused, as is one given twice.
	bad := finding("Made up")
	bad["id"] = "F9"
	if _, refused := judge(t, h, DispositionAdopt, "F1", "F2", "F3"); refused {
		t.Fatal("judge round 2 refused")
	}
	if reply, refused := review(t, h, bad); !refused || !strings.Contains(reply["refused"].(string), "F9") {
		t.Fatalf("unknown id %v", reply)
	}
	if reply, refused := review(t, h, again, again); !refused || !strings.Contains(reply["refused"].(string), "twice") {
		t.Fatalf("twice %v", reply)
	}
}

// A round that brings no new findings stops the loop: converged when it
// brings none at all, repeating, a park for the owner, when they all came
// back. A stopped loop takes no more reviews.
func TestRoundsStopWithoutNewFindings(t *testing.T) {
	h, _ := roundsHarness(t)
	review(t, h, finding("Spin has no test"))
	// The next round waits for the judge.
	if reply, refused := review(t, h, finding("Gears grind")); !refused || !strings.Contains(reply["refused"].(string), "dispositions") {
		t.Fatalf("before the judge %v", reply)
	}
	judge(t, h, DispositionAdopt, "F1")
	reply, refused := review(t, h, finding("Spin has no test"))
	if refused || reply["stopped"] != RoundsRepeating {
		t.Fatalf("repeat %v", reply)
	}
	c := roundsOf(t, h)
	if c.RoundStop == nil || c.RoundStop.Reason != RoundsRepeating || c.RoundStop.Round != 2 || len(c.RoundStop.Lessons) < 2 ||
		!strings.Contains(strings.Join(c.RoundStop.Lessons, " "), "F1") {
		t.Fatalf("stop %+v", c.RoundStop)
	}
	if cv := caseByID(t, h, widgetsCase); cv.State != CaseNeedsYou || cv.Rounds == nil || cv.Rounds.Stop == nil {
		t.Fatalf("case %+v", cv)
	}
	if reply, refused := review(t, h); !refused || !strings.Contains(reply["refused"].(string), "only the owner") {
		t.Fatalf("after the stop %v", reply)
	}
	// The repeated finding is still the judge's to dispose.
	if text, refused := judge(t, h, DispositionDecline, "F1"); refused {
		t.Fatalf("judge after the stop %s", text)
	}

	// A round with no findings at all converges, and needs no judge.
	h2, _ := roundsHarness(t)
	if reply, refused := review(t, h2); refused || reply["stopped"] != RoundsConverged {
		t.Fatalf("empty round %v", reply)
	}
	if cv := caseByID(t, h2, widgetsCase); cv.State == CaseNeedsYou || cv.Rounds.Stop.Reason != RoundsConverged {
		t.Fatalf("converged case %+v", cv)
	}
}

// The backstop stops a loop that keeps finding new things; only the
// owner's record resets it.
func TestRoundsBackstop(t *testing.T) {
	h, _ := roundsHarness(t)
	h.cfg.Policies.Rounds.Max = 2
	review(t, h, finding("Spin has no test"))
	judge(t, h, DispositionAdopt, "F1")
	reply, _ := review(t, h, finding("Gears grind"))
	if reply["stopped"] != RoundsBackstop {
		t.Fatalf("round 2 %v", reply)
	}
	judge(t, h, DispositionAdopt, "F2")
	if reply, refused := review(t, h, finding("Axles squeak")); !refused || !strings.Contains(reply["refused"].(string), "backstop") {
		t.Fatalf("round 3 %v", reply)
	}
	// Neither a session nor a run can write the owner's record.
	for _, mode := range []string{ModeShared, ModeLive} {
		e := Event{V: EventVersion, Time: h.clock.Now(), Session: "jd", Kind: KindCase, Mode: mode, Case: &CasePayload{Case: widgetsCase, Rounds: &CaseRounds{Max: 5}}}
		if err := e.check(); err == nil {
			t.Fatalf("a %s rounds record validated", mode)
		}
	}
	if err := RecordRounds(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), widgetsCase, 0); err == nil {
		t.Fatal("a backstop of 0 recorded")
	}
	if err := RecordRounds(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), widgetsCase, 1); err != nil {
		t.Fatal(err)
	}
	c := roundsOf(t, h)
	if c.RoundStop != nil || c.RoundMax != 1 || c.RoundBase != 2 {
		t.Fatalf("after the reset %+v", c)
	}
	reply, refused := review(t, h, finding("Axles squeak"))
	if refused || reply["round"] != 3.0 || reply["stopped"] != RoundsBackstop {
		t.Fatalf("round 3 after the reset %v", reply)
	}
}

// The judge disposes every finding of the latest round exactly once, with
// a reason, and cannot adopt one that widens the scope.
func TestRoundsJudgeChecks(t *testing.T) {
	h, _ := roundsHarness(t)
	wide := map[string]any{"title": "Add a widget catalogue", "severity": "low", "widens_scope": true, "paths": []any{"catalogue.go"},
		"consequence": "None for spinning.", "suggestion": "File it."}
	noItems := finding("Spin has no test")
	noItems["items"] = []any{}
	if reply, refused := review(t, h, noItems); !refused || !strings.Contains(reply["refused"].(string), "widens_scope") {
		t.Fatalf("no items %v", reply)
	}
	review(t, h, finding("Spin has no test"), wide)
	jd := h.mcpServerFor("jd")
	call := func(ds []any, plan []any) (string, bool) {
		return callOne(t, jd, ToolJudgeDispositions, map[string]any{"dispositions": ds, "plan": plan, "lessons": []any{}})
	}
	d := func(id, disp, reason string) map[string]any {
		return map[string]any{"finding_id": id, "disposition": disp, "reason": reason}
	}
	for name, c := range map[string]struct {
		ds, plan []any
		want     string
	}{
		"missing":  {[]any{d("F1", "adopt", "Yes.")}, nil, "F2"},
		"twice":    {[]any{d("F1", "adopt", "Yes."), d("F1", "decline", "No."), d("F2", "defer", "Later.")}, nil, "twice"},
		"unknown":  {[]any{d("F1", "adopt", "Yes."), d("F2", "defer", "Later."), d("F7", "adopt", "Yes.")}, nil, "F7"},
		"reason":   {[]any{d("F1", "adopt", " "), d("F2", "defer", "Later.")}, nil, "reason"},
		"kind":     {[]any{d("F1", "accept", "Yes."), d("F2", "defer", "Later.")}, nil, "adopt, decline or defer"},
		"widening": {[]any{d("F1", "adopt", "Yes."), d("F2", "adopt", "Nice.")}, nil, "raise_decision"},
		"plan ids": {[]any{d("F1", "adopt", "Yes."), d("F2", "defer", "Later.")},
			[]any{map[string]any{"id": "P1", "step": "a"}, map[string]any{"id": "P1", "step": "b"}}, "P1"},
	} {
		text, refused := call(c.ds, c.plan)
		if !refused || !strings.Contains(text, c.want) {
			t.Errorf("%s: %s", name, text)
		}
	}
	if text, refused := call([]any{d("F1", "adopt", "Yes."), d("F2", "defer", "Out of scope; asked the owner.")}, []any{}); refused {
		t.Fatalf("valid %s", text)
	}
	if text, refused := call([]any{d("F1", "adopt", "Yes."), d("F2", "defer", "Again.")}, []any{}); !refused || !strings.Contains(text, "disposed") {
		t.Fatalf("twice per round %s", text)
	}
	c := roundsOf(t, h)
	if r := c.Rounds[0]; len(r.Dispositions) != 2 || r.Dispositions[1].Disposition != DispositionDefer || c.Disposition("F2") == nil {
		t.Fatalf("round %+v", r)
	}
}

// Each role is offered its own tools, and a call to another's is a
// protocol error that reaches no case.
func TestRoundsToolsByRole(t *testing.T) {
	h, _ := roundsHarness(t)
	names := func(name string) []string {
		rs := session(t, h.mcpServerFor(name), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		var out []string
		for _, tool := range rs[0].Result.Tools {
			out = append(out, tool.Name)
		}
		return out
	}
	for name, want := range map[string][]string{
		"s1": {ToolReportProgress, ToolRaiseDecision, ToolRequestStep},
		"rv": {ToolReviewFindings},
		"jd": {ToolJudgeDispositions, ToolRaiseDecision},
	} {
		if got := names(name); !reflect.DeepEqual(got, append(want, contextTools...)) {
			t.Errorf("%s: %v", name, got)
		}
	}
	before := len(h.events())
	rs := session(t, h.mcpServerFor("s1"), toolCall(1, ToolReviewFindings, map[string]any{"findings": []any{}}))
	if rs[0].Error == nil || len(h.events()) != before {
		t.Fatalf("worker's review %+v", rs[0])
	}
	rs = session(t, h.mcpServerFor("rv"), toolCall(1, ToolReportProgress, map[string]any{}))
	if rs[0].Error == nil {
		t.Fatalf("reviewer's report %+v", rs[0])
	}
}

// A close is refused while a finding lacks its disposition, and needs a
// demo line or N/A with its reason; the close record marks each item and
// keeps the lessons.
func TestRoundsCloseRecord(t *testing.T) {
	h, _ := roundsHarness(t)
	review(t, h, finding("Spin has no test"))
	closeIt := func(text string) error {
		return RecordRuling(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), widgetsCase, RulingClose, text, "owner")
	}
	if err := closeIt("Run widgets spin."); err == nil || !strings.Contains(err.Error(), "F1") {
		t.Fatalf("close before the judge: %v", err)
	}
	judge(t, h, DispositionAdopt, "F1")
	for _, text := range []string{"", "N/A", "N/A:  "} {
		if err := closeIt(text); err == nil {
			t.Fatalf("close %q accepted", text)
		}
	}
	if err := closeIt("N/A: a library change with no screen."); err != nil {
		t.Fatal(err)
	}
	c := roundsOf(t, h)
	r := c.Rulings[len(c.Rulings)-1].Close
	if !c.Closed || r == nil || r.NA != "a library change with no screen." || r.Demo != "" || len(r.Items) != 2 ||
		r.Items[0] != (CloseItem{ID: "A1", By: GoalOwner}) || !contains(r.Lessons, "Test spinning first.") {
		t.Fatalf("close record %+v", r)
	}
	// A close carries its record; one without is not the owner's close.
	e := Event{V: EventVersion, Time: h.clock.Now(), Kind: KindCase, Mode: ModeOwner, Case: &CasePayload{Case: widgetsCase, Ruling: &CaseRuling{Kind: RulingClose, Close: &CloseRecord{}}}}
	if err := e.check(); err == nil {
		t.Fatal("a close with neither demo nor N/A validated")
	}
}

// A reviewer's facts list the findings by id with their dispositions and
// the plan, never the worker's own account.
func TestRoundsReviewerFactsLeaveOutTheAccount(t *testing.T) {
	h, head := roundsHarness(t)
	h.cfg.Policies.Restart.Command = []string{"claude", "--mcp-config={mcp}", "{prompt}"}
	if err := RecordRuling(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), widgetsCase, RulingRestriction, "Do not touch the gears.", "owner"); err != nil {
		t.Fatal(err)
	}
	if text, refused := callOne(t, h.mcpServerFor("s1"), ToolReportProgress, validReport(head)); refused {
		t.Fatalf("report %s", text)
	}
	review(t, h, finding("Spin has no test"))
	judge(t, h, DispositionAdopt, "F1")
	h.restart(false)
	_, facts := h.sup.launchCommand("rv", h.cfg.Sessions["rv"], newTrack(), "id-1", "restart")
	var f restartFacts
	if err := json.Unmarshal(facts, &f); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Tools, []string{ToolReviewFindings}) || f.Rounds == nil || len(f.Rounds.Findings) != 1 ||
		f.Rounds.Findings[0].ID != "F1" || f.Rounds.Findings[0].Disposition != DispositionAdopt || len(f.Rounds.Plan) != 1 {
		t.Fatalf("facts %s", facts)
	}
	for _, said := range []string{"Spin works", "Stopping is next", "Write the stop test", "A test per item"} {
		if strings.Contains(string(facts), said) {
			t.Fatalf("the reviewer's facts carry the worker's account %q: %s", said, facts)
		}
	}
}

// A finding with its disposition is evidence a report can cite; one the
// case does not have, or not yet disposed, is not.
func TestRoundsFindingEvidence(t *testing.T) {
	h, head := roundsHarness(t)
	report := func(id string) (string, bool) {
		r := validReport(head)
		r["acknowledged"] = []any{}
		r["items"].([]any)[0].(map[string]any)["evidence"] = []any{map[string]any{"kind": "finding", "id": id}}
		return callOne(t, h.mcpServerFor("s1"), ToolReportProgress, r)
	}
	review(t, h, finding("Spin has no test"))
	if text, refused := report("F1"); !refused || !strings.Contains(text, "disposition") {
		t.Fatalf("undisposed %s", text)
	}
	judge(t, h, DispositionAdopt, "F1")
	if text, refused := report("F1"); refused {
		t.Fatalf("disposed %s", text)
	}
	if text, refused := report("F4"); !refused || !strings.Contains(text, "F4") {
		t.Fatalf("unknown %s", text)
	}
}

// A role is read from the file: reviewer or judge, and no other.
func TestRoundsRoleFromConfig(t *testing.T) {
	dir := t.TempDir()
	for role, ok := range map[string]bool{RoleReviewer: true, RoleJudge: true, "coordinator": false, "boss": false} {
		cfg := DefaultConfig()
		cfg.Policies.Escalate.Command = []string{"route", "{message}"}
		cfg.Repositories = map[string]Repository{"widgets": {Forge: "gitlab", Host: "gitlab.example.com", Project: "example-org/widgets"}}
		cfg.Sessions = map[string]Session{"s1": {Workdir: dir, Cwd: dir, Repository: "widgets", Issue: 7, Role: role}}
		if err := cfg.finish(dir); (err == nil) != ok {
			t.Errorf("role %s: %v", role, err)
		}
	}
	var s Session
	if err := json.Unmarshal([]byte(`{"workdir":"w","role":"reviewer"}`), &s); err != nil || s.Role != RoleReviewer {
		t.Fatalf("session %+v: %v", s, err)
	}
}

// Two reviewers checked against the same state: the call appended second
// is not the next round, is not folded, and is told so; the close still
// waits for every finding the fold has.
func TestRoundsTwoReviewersRace(t *testing.T) {
	h, _ := roundsHarness(t)
	s := h.cfg.Sessions["rv"]
	s.Workdir = filepath.Join(h.dir, "rv2")
	h.cfg.Sessions["rv2"] = s
	stale := h.events()
	reads := 0
	srv, err := NewMCPServer(h.cfg, "rv2", NewLogWriter(h.buf), func() ([]Event, error) {
		if reads++; reads == 1 {
			return stale, nil
		}
		return h.events(), nil
	}, h.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	review(t, h, finding("Spin has no test"))
	text, refused := callOne(t, srv, ToolReviewFindings, map[string]any{"findings": []any{finding("Gears grind")}})
	if !refused || !strings.Contains(text, "another call") {
		t.Fatalf("second reviewer %s", text)
	}
	c := roundsOf(t, h)
	if len(c.Rounds) != 1 || len(c.findings()) != 1 || c.findings()[0].Title != "Spin has no test" {
		t.Fatalf("rounds %+v", c.Rounds)
	}
}

// Text matching a confidential pattern is withheld, a finding given again
// still keeps its id, and a refused call logs no value it was given.
func TestRoundsScreening(t *testing.T) {
	h, _ := roundsHarness(t)
	h.cfg.ConfidentialPatterns = []string{"(?i)gizmo"}
	secret := finding("The gizmo leaks")
	reply, _ := review(t, h, secret)
	judge(t, h, DispositionAdopt, "F1")
	c := roundsOf(t, h)
	if f := c.Rounds[0].Findings[0]; !strings.HasPrefix(f.Title, "(withheld") {
		t.Fatalf("finding %+v", f)
	}
	if reply, _ = review(t, h, secret, finding("Gears grind")); !reflect.DeepEqual(reply["ids"], []any{"F1", "F2"}) {
		t.Fatalf("again %v", reply)
	}
	judge(t, h, DispositionAdopt, "F1", "F2")
	bad := finding("Made up")
	bad["id"] = "F77-gizmo"
	if reply, refused := review(t, h, bad); !refused || !strings.Contains(reply["refused"].(string), "F77-gizmo") {
		t.Fatalf("refused %v", reply)
	}
	for _, e := range h.events() {
		if e.Case != nil && e.Case.Call != nil && strings.Contains(strings.ToLower(e.Case.Call.Refused+compact(e.Case.Call.Findings)), "gizmo") {
			t.Fatalf("logged %+v", e.Case.Call)
		}
	}
	// Other paths and an unlike title are a new finding.
	other := finding("Gears grind")
	other["paths"] = []any{"gears.go"}
	if reply, _ := review(t, h, other); !reflect.DeepEqual(reply["new"], []any{"F3"}) {
		t.Fatalf("other paths %v", reply)
	}
}

// A finding that comes back in three rounds in a row parks the case even
// while each round brings something new.
func TestRoundsFindingThatKeepsComingBack(t *testing.T) {
	h, _ := roundsHarness(t)
	h.cfg.Policies.Rounds.Max = 5
	for i, extra := range []string{"Gears grind", "Axles squeak", "Bolts rattle"} {
		reply, _ := review(t, h, finding("Spin has no test"), finding(extra))
		var ids []string
		for _, id := range reply["ids"].([]any) {
			ids = append(ids, id.(string))
		}
		if i < 2 && reply["stopped"] != nil || i == 2 && reply["stopped"] != RoundsRepeating {
			t.Fatalf("round %d %v", i+1, reply)
		}
		judge(t, h, DispositionAdopt, ids...)
	}
}

// A close logged before closes had records still replays; a close's N/A
// is read in any case; the owner's rounds record takes a configured case
// the log does not have yet.
func TestRoundsOldCloseAndOwnerRecords(t *testing.T) {
	h, _ := roundsHarness(t)
	NewLogWriter(h.buf).Append(Event{V: EventVersion, Time: h.clock.Now(), Kind: KindCase, Mode: ModeOwner, Case: &CasePayload{Case: widgetsCase, Ruling: &CaseRuling{Kind: RulingClose}}})
	if c := roundsOf(t, h); !c.Closed {
		t.Fatal("an old close did not replay")
	}
	h2 := caseHarness(t)
	if err := RecordRounds(NewLogWriter(h2.buf), h2.cfg, h2.logState(), h2.clock.Now(), widgetsCase, 4); err != nil {
		t.Fatal(err)
	}
	if err := RecordRounds(NewLogWriter(h2.buf), h2.cfg, h2.logState(), h2.clock.Now(), "gitlab/example-org/widgets#99", 4); err == nil {
		t.Fatal("an unknown case took a rounds record")
	}
	if err := RecordRuling(NewLogWriter(h2.buf), h2.cfg, h2.logState(), h2.clock.Now(), widgetsCase, RulingClose, "n/a: nothing to see", "owner"); err != nil {
		t.Fatal(err)
	}
	c := h2.logState().Cases[widgetsCase]
	if r := c.Rulings[len(c.Rulings)-1].Close; r.NA != "nothing to see" || r.Demo != "" {
		t.Fatalf("close %+v", r)
	}
}

// A judge's facts keep the rulings without asking it to acknowledge them.
func TestRoundsJudgeFacts(t *testing.T) {
	h, _ := roundsHarness(t)
	h.cfg.Policies.Restart.Command = []string{"claude", "--mcp-config={mcp}", "{prompt}"}
	if err := RecordRuling(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), widgetsCase, RulingRestriction, "Do not touch the gears.", "owner"); err != nil {
		t.Fatal(err)
	}
	review(t, h, finding("Spin has no test"))
	h.restart(false)
	_, facts := h.sup.launchCommand("jd", h.cfg.Sessions["jd"], newTrack(), "id-1", "restart")
	var f restartFacts
	if err := json.Unmarshal(facts, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Acknowledge) != 0 || len(f.Rulings) != 1 || f.Rounds == nil || f.Rounds.Findings[0].Disposition != "" {
		t.Fatalf("facts %s", facts)
	}
}

// staleServer is the named session's server whose first read is the log
// as it is now, and every later read the log as it is then: a call checked
// against a state that changed before it was appended.
func (h *harness) staleServer(name string) *MCPServer {
	h.t.Helper()
	stale, reads := h.events(), 0
	srv, err := NewMCPServer(h.cfg, name, NewLogWriter(h.buf), func() ([]Event, error) {
		if reads++; reads == 1 {
			return stale, nil
		}
		return h.events(), nil
	}, h.clock.Now)
	if err != nil {
		h.t.Fatal(err)
	}
	return srv
}

// What changed between a round call's check and its append wins: another
// round with the same words as an earlier one, the owner's reset, the
// owner's close. The call is not folded and is refused; a reopen resets
// the loop.
func TestRoundsChangedBeforeAppend(t *testing.T) {
	h, _ := roundsHarness(t)
	review(t, h, finding("Spin has no test"))
	judge(t, h, DispositionAdopt, "F1")
	late := h.staleServer("rv")
	review(t, h, finding("Gears grind"))
	if text, refused := callOne(t, late, ToolReviewFindings, map[string]any{"findings": []any{finding("Spin has no test")}}); !refused {
		t.Fatalf("same words as round 1 %s", text)
	}
	judge(t, h, DispositionAdopt, "F2")

	late = h.staleServer("rv")
	if err := RecordRounds(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), widgetsCase, 3); err != nil {
		t.Fatal(err)
	}
	if text, refused := callOne(t, late, ToolReviewFindings, map[string]any{"findings": []any{finding("Axles squeak")}}); !refused {
		t.Fatalf("across the reset %s", text)
	}
	if c := roundsOf(t, h); len(c.Rounds) != 2 || c.RoundStop != nil {
		t.Fatalf("after the reset %+v", c.Rounds)
	}

	late = h.staleServer("rv")
	if err := RecordRuling(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), widgetsCase, RulingClose, "N/A, nothing visible", "owner"); err != nil {
		t.Fatal(err)
	}
	if text, refused := callOne(t, late, ToolReviewFindings, map[string]any{"findings": []any{finding("Axles squeak")}}); !refused {
		t.Fatalf("across the close %s", text)
	}
	c := roundsOf(t, h)
	if len(c.undisposed()) != 0 || c.Rulings[len(c.Rulings)-1].Close.NA != "nothing visible" {
		t.Fatalf("after the close %+v", c)
	}
	if err := RecordRuling(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), widgetsCase, RulingReopen, "", "owner"); err != nil {
		t.Fatal(err)
	}
	if reply, _ := review(t, h); reply["stopped"] != RoundsConverged {
		t.Fatalf("after the reopen %v", reply)
	}
	if err := RecordRuling(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), widgetsCase, RulingClose, "N/A.", "owner"); err == nil {
		t.Fatal("N/A without a reason accepted")
	}
}
