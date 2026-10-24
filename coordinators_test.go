package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// promptLine and summaryLine are a fake transcript's user prompt and an
// end-of-turn summary, written at at.
func promptLine(uuid, text string, at time.Time) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "uuid": uuid, "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": text}})
	return string(b) + "\n"
}

func summaryLine(uuid, id, text string, at time.Time, context int64) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": uuid, "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"id": id, "stop_reason": "end_turn", "content": []map[string]any{{"type": "text", "text": text}},
			"usage": map[string]int64{"input_tokens": 1, "cache_creation_input_tokens": context, "cache_read_input_tokens": 0, "output_tokens": 10}}})
	return string(b) + "\n"
}

// coordHarness is a live harness with sessions a and b in widgets, d in
// readonly, and c, widgets' coordinator, all alive, transcripts read.
func coordHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, false, "a", "b", "d")
	h.cfg.TranscriptDir = filepath.Join(h.dir, "projects")
	d := h.cfg.Sessions["d"]
	d.Repository = "readonly"
	h.cfg.Sessions["d"] = d
	w := h.cfg.Repositories["widgets"]
	w.Coordinator = &Coordinator{Session: "c", Workdir: filepath.Join(h.dir, "c")}
	h.cfg.Repositories["widgets"] = w
	if err := os.MkdirAll(filepath.Join(h.dir, "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c", "d"} {
		h.alive(n, "screen of "+n)
	}
	h.restart(false)
	return h
}

func (h *harness) transcriptOf(name, id, text string) {
	h.t.Helper()
	appendFile(h.t, filepath.Join(TranscriptDir(h.cfg.TranscriptDir, h.cfg.Sessions[name].Cwd), id+".jsonl"), text)
}

func (h *harness) wakes() []*ActPayload {
	var out []*ActPayload
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActWake {
			out = append(out, e.Act)
		}
	}
	return out
}

func (h *harness) pack(t *testing.T) Pack {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, "c", PackFile))
	if err != nil {
		t.Fatal(err)
	}
	var p Pack
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("%s: %v", PackFile, err)
	}
	return p
}

// A transcript's prompts and end-of-turn summaries become turn events,
// screened and bounded, once each: a replayed agentd reading the same
// transcript from its start logs none again. A tool result is no prompt.
func TestTurnsFromTranscriptOnceAcrossRestarts(t *testing.T) {
	h := coordHarness(t)
	h.cfg.ConfidentialPatterns = []string{`hunter2`}
	h.restart(false)
	at := h.clock.Now()
	h.transcriptOf("a", "t1", promptLine("u1", "Build the widget.", at)+
		`{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`+"\n"+
		`{"type":"user","uuid":"u3","isSidechain":true,"message":{"role":"user","content":"a subagent's prompt"}}`+"\n"+
		summaryLine("u4", "m1", "Built it; the check passes.", at, 100)+
		promptLine("u5", "the password is hunter2", at)+
		promptLine("u6", strings.Repeat("long ", 200), at))
	h.ticks(3, time.Minute)
	turns := func() []TurnPayload {
		var out []TurnPayload
		for _, e := range h.events() {
			if e.Kind == KindTurn && e.Session == "a" {
				out = append(out, *e.Turn)
			}
		}
		return out
	}
	got := turns()
	if len(got) != 4 {
		t.Fatalf("turns = %+v, want 4", got)
	}
	if got[0].Role != TurnPrompt || got[0].Text != "Build the widget." || got[1].Role != TurnSummary || got[1].Text != "Built it; the check passes." {
		t.Fatalf("turns = %+v", got[:2])
	}
	if got[2].Text != "" || got[2].Withheld != 1 {
		t.Fatalf("confidential prompt = %+v", got[2])
	}
	if len(got[3].Text) > turnText+len("…") || !strings.HasPrefix(got[3].Text, "long long") {
		t.Fatalf("long prompt kept %d bytes", len(got[3].Text))
	}
	h.restart(false)
	h.ticks(3, time.Minute)
	if n := len(turns()); n != 4 {
		t.Fatalf("after a restart of agentd: %d turns", n)
	}
	h.transcriptOf("a", "t1", promptLine("u7", "Now the gadget.", h.clock.Now()))
	h.ticks(2, time.Minute)
	if got := turns(); len(got) != 5 || got[4].Text != "Now the gadget." {
		t.Fatalf("after a new prompt: %+v", got)
	}
	if tr := h.sup.Tracks()["a"]; tr.TurnCount != 5 || len(tr.Turns) != 5 {
		t.Fatalf("track: %d turns, %d kept", tr.TurnCount, len(tr.Turns))
	}
}

// A pack holds only its repository's sessions and items, never the
// coordinator's own turns or an owner-only item, stays under its byte
// limit, and counts what it left out.
func TestPackHoldsOnlyItsRepositoryUnderLimit(t *testing.T) {
	h := coordHarness(t)
	h.cfg.Policies.Coordinator.PackBytes = 2048
	at := h.clock.Now()
	var text string
	for i := 0; i < 30; i++ {
		text += promptLine("a"+string(rune('a'+i)), "prompt number "+strings.Repeat("x", 60), at)
	}
	h.transcriptOf("a", "t1", text)
	h.transcriptOf("d", "t2", promptLine("d1", "readonly work", at))
	h.transcriptOf("c", "t3", promptLine("c1", "coordinator's own", at))
	h.ticks(2, time.Minute)
	for _, it := range []ItemPayload{
		{ID: "gitlab:example-org/widgets#4", Kind: "issue", Repository: "gitlab/example-org/widgets", Title: "Widget squeaks"},
		{ID: "gitlab:example-org/readonly#5", Kind: "issue", Repository: "gitlab/example-org/readonly", Title: "Elsewhere"},
		{ID: "mail:x", Kind: "mail", Repository: "gitlab/example-org/widgets", Title: "Private", OwnerOnly: true},
	} {
		it := it
		h.sup.emit("", Event{Kind: KindItem, Mode: ModeShared, Item: &it})
	}
	p, b := h.sup.buildPack("widgets", nil, h.clock.Now())
	if len(b) > 2048 {
		t.Fatalf("pack is %d bytes, over 2048", len(b))
	}
	names := []string{}
	for _, s := range p.Sessions {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "a,b" {
		t.Fatalf("pack sessions = %v", names)
	}
	if len(p.Items) != 1 || p.Items[0].ID != "gitlab:example-org/widgets#4" {
		t.Fatalf("pack items = %+v", p.Items)
	}
	if p.Summarized.Turns == 0 || len(p.Sessions[0].Turns)+p.Summarized.Turns != 30 {
		t.Fatalf("turns shown %d, summarized %d, want 30 in all", len(p.Sessions[0].Turns), p.Summarized.Turns)
	}
	if bytes.Contains(b, []byte("coordinator's own")) || bytes.Contains(b, []byte("readonly work")) {
		t.Fatalf("pack holds another's turns: %s", b)
	}
}

// Nothing due wakes no one; a finished session, a session waiting on a
// decision, a new item and an owner answer each wake the coordinator once,
// with the reason recorded, and none while it is still on a wake.
func TestWakeOnlyWhenDue(t *testing.T) {
	h := coordHarness(t)
	h.ticks(5, time.Minute)
	if n := len(h.wakes()); n != 0 || h.tmux.count("type", "c") != 0 {
		t.Fatalf("woke with nothing due: %d wakes", n)
	}
	h.status("a", "# a\n\nDONE\n")
	h.ticks(3, time.Minute)
	w := h.wakes()
	if len(w) != 1 || w[0].Reason != "finished: a" || h.tmux.count("type", "c") != 1 {
		t.Fatalf("wakes = %+v, typed %d", w, h.tmux.count("type", "c"))
	}
	if typed := h.tmux.calls[len(h.tmux.calls)-1]; typed.op == "type" && typed.args[0] != h.cfg.Policies.Coordinator.Pointer {
		t.Fatalf("typed %q", typed.args[0])
	}
	if p := h.pack(t); len(p.Due) != 1 || p.Due[0] != "finished: a" || p.Repository != "widgets" {
		t.Fatalf("pack = %+v", p)
	}
	// Still on its wake: a new item waits.
	it := ItemPayload{ID: "gitlab:example-org/widgets#4", Kind: "issue", Repository: "gitlab/example-org/widgets", Title: "Widget squeaks"}
	h.sup.emit("", Event{Kind: KindItem, Mode: ModeShared, Item: &it})
	h.ticks(3, time.Minute)
	if n := len(h.wakes()); n != 1 {
		t.Fatalf("woke during a wake: %d", n)
	}
	h.transcriptOf("c", "t3", summaryLine("c1", "m1", "Routed.", h.clock.Now(), 100))
	h.ticks(2, time.Minute)
	if w := h.wakes(); len(w) != 2 || w[1].Reason != "item: gitlab:example-org/widgets#4" {
		t.Fatalf("wakes = %+v", w)
	}
	h.transcriptOf("c", "t3", summaryLine("c2", "m2", "Noted.", h.clock.Now(), 100))
	writeQuestions(t, h, "b", chooseColour)
	h.ticks(2, time.Minute)
	if w := h.wakes(); len(w) != 3 || w[2].Reason != "waiting: b" {
		t.Fatalf("wakes = %+v", w)
	}
	h.transcriptOf("c", "t3", summaryLine("c3", "m3", "Asked.", h.clock.Now(), 100))
	if err := answerAs(h, "b", AnswerPayload{ID: "Q1", Choice: "b"}); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	if w := h.wakes(); len(w) != 4 || w[3].Reason != "answer: b/Q1" {
		t.Fatalf("wakes = %+v", w)
	}
	h.transcriptOf("c", "t3", summaryLine("c4", "m4", "Done.", h.clock.Now(), 100))
	h.ticks(5, time.Minute)
	h.restart(false)
	h.ticks(3, time.Minute)
	if n := len(h.wakes()); n != 4 {
		t.Fatalf("woke again for what was handled: %d", n)
	}
	// A coordinator is never nudged, however long it is quiet.
	h.ticks(3, time.Hour)
	if n := h.count(KindAction, ActNudge, "c"); n != 0 {
		t.Fatalf("coordinator nudged %d times", n)
	}
}

// A coordinator over the threshold is rotated once it has handled a wake,
// never during one, and its fresh session is pointed at the pack.
func TestCoordinatorRotatedAfterWakeNotDuring(t *testing.T) {
	h := coordHarness(t)
	id := launchCoordinator(t, h) // restarted from the pack
	if f := launchFacts(t, h, "c"); f.Pack != filepath.Join(h.dir, "c", PackFile) || f.Repository != "widgets" {
		t.Fatalf("restart facts = %+v", f)
	}
	h.pack(t) // written before the launch
	h.ticks(2, time.Minute)
	h.transcriptOf("c", id, promptLine("c0", "start", h.clock.Now())+usageLine("m0", 10, 150000, 0, 5, false))
	h.status("a", "# a\n\nDONE\n")
	h.ticks(3, time.Minute)
	if n := len(h.wakes()); n != 1 {
		t.Fatalf("wakes = %d", n)
	}
	if n := h.count(KindAction, ActRotate, "c"); n != 0 {
		t.Fatalf("rotated during a wake: %d", n)
	}
	h.transcriptOf("c", id, summaryLine("c1", "m1", "Handled.", h.clock.Now(), 150000))
	h.ticks(2, time.Minute)
	if n := h.count(KindAction, ActRotate, "c"); n != 1 {
		t.Fatalf("rotations = %d, want 1", n)
	}
	if f := launchFacts(t, h, "c"); f.Why != "rotation" || f.Pack == "" {
		t.Fatalf("rotation facts = %+v", f)
	}
	h.ticks(3, time.Minute)
	if n := h.count(KindAction, ActRotate, "c"); n != 1 {
		t.Fatalf("rotated twice for one wake: %d", n)
	}
}

// launchFacts is the facts of the latest launch of name, out of its
// command line.
func launchFacts(t *testing.T, h *harness, name string) restartFacts {
	t.Helper()
	var line string
	for _, e := range h.events() {
		if e.Kind == KindAction && (e.Act.Action == ActRestart || e.Act.Action == ActRotate) && e.Session == name {
			line = e.Act.Command
		}
	}
	i, j := strings.Index(line, `{"why"`), strings.LastIndex(line, "}")
	if i < 0 || j < i {
		t.Fatalf("no facts in the command: %s", line)
	}
	var f restartFacts
	if err := json.Unmarshal([]byte(strings.ReplaceAll(line[i:j+1], `'\''`, `'`)), &f); err != nil {
		t.Fatalf("facts: %v: %s", err, line[i:j+1])
	}
	return f
}

// launchCoordinator has c found gone and restarted, so it runs under a
// transcript agentd launched, and returns that transcript's id.
func launchCoordinator(t *testing.T, h *harness) string {
	t.Helper()
	h.tmux.alive["c"] = false
	h.ticks(1, time.Minute)
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActRestart && e.Session == "c" {
			return e.Act.Transcript
		}
	}
	t.Fatal("c was not restarted")
	return ""
}

// A second coordinator for one repository is refused: in the config, and
// from the log, where another run's claim holds until the owner releases
// it.
func TestSecondCoordinatorRefused(t *testing.T) {
	h := coordHarness(t)
	r := h.cfg.Repositories["readonly"]
	r.Coordinator = &Coordinator{Session: "c", Workdir: filepath.Join(h.dir, "c")}
	h.cfg.Repositories["readonly"] = r
	if err := h.cfg.finish(h.dir); err == nil || !strings.Contains(err.Error(), "coordinates") {
		t.Fatalf("one session coordinating two repositories: %v", err)
	}
	r.Coordinator = &Coordinator{Session: "a", Workdir: filepath.Join(h.dir, "a")}
	h.cfg.Repositories["readonly"] = r
	if err := h.cfg.finish(h.dir); err == nil || !strings.Contains(err.Error(), "sessions.a") {
		t.Fatalf("a coordinator also configured as a session: %v", err)
	}

	h = coordHarness(t)
	claim := Event{V: EventVersion, Time: h.clock.Now(), Session: "elsewhere", Kind: KindCoordinator, Mode: ModeShared,
		Coordinator: &CoordinatorPayload{Repository: "widgets"}}
	if err := NewLogWriter(h.buf).Append(claim); err != nil {
		t.Fatal(err)
	}
	h.status("a", "# a\n\nDONE\n")
	h.ticks(3, time.Minute)
	if n := len(h.wakes()); n != 0 {
		t.Fatalf("a second coordinator woke: %d", n)
	}
	refused := false
	for _, e := range h.events() {
		refused = refused || e.Kind == KindSkip && e.Session == "c" && strings.Contains(skipReason(e), "elsewhere")
	}
	if !refused {
		t.Fatal("the refusal was not logged")
	}
	if err := ReleaseCoordinator(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "widgets"); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	if w := h.wakes(); len(w) != 1 || w[0].Reason != "finished: a" {
		t.Fatalf("after the release: %+v", w)
	}
	if got := NewLogState(h.cfg, ModeLive).Fold(h.events()).Coordinators["widgets"]; got != "c" {
		t.Fatalf("claim = %q", got)
	}
	if err := ReleaseCoordinator(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "nowhere"); err == nil {
		t.Fatal("released an unknown repository")
	}
}

// Status shows each coordinator's wakes and its tokens per wake.
func TestStatusShowsWakes(t *testing.T) {
	h := coordHarness(t)
	id := launchCoordinator(t, h)
	h.transcriptOf("c", id, usageLine("m0", 1000, 0, 0, 0, false))
	h.ticks(2, time.Minute)
	h.status("a", "# a\n\nDONE\n")
	h.ticks(2, time.Minute)
	h.transcriptOf("c", id, summaryLine("c1", "m1", "Handled.", h.clock.Now(), 0)) // spend 1+50 more
	h.status("b", "# b\n\nDONE\n")
	h.ticks(2, time.Minute)
	h.transcriptOf("c", id, usageLine("m2", 149, 0, 0, 0, false))
	h.ticks(2, time.Minute)
	rows, meta := Summarize(h.cfg, h.events(), ModeLive)
	var c SessionStatus
	for _, r := range rows {
		if r.Name == "c" {
			c = r
		}
	}
	if c.Wakes != 2 || c.WakeTokens != (51+149)/2 || c.Role != RoleCoordinator {
		t.Fatalf("status of c = %+v", c)
	}
	var out bytes.Buffer
	PrintStatus(&out, rows, meta, h.clock.Now())
	if !strings.Contains(out.String(), "coordinator of widgets: wakes 2, 100 tokens per wake") {
		t.Fatalf("status:\n%s", out.String())
	}
	// The page shows them too, from the report.
	rep := BuildReport(h.cfg, h.events(), ModeLive, "host-a", h.clock.Now())
	out.Reset()
	if err := pageTemplate.Execute(&out, buildView(PageData{Report: rep}, parseViewQuery(nil), h.clock.Now())); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "coordinator: 2 wakes, 100 tokens per wake") {
		t.Fatalf("page:\n%s", out.String())
	}
}

// A wake an agentd crash cut short is not outstanding, and a typed wake
// the coordinator's transcript never moved after counts as handled once
// the nudge stall has passed, so neither silences the coordinator.
func TestCutShortOrUntakenWakeDoesNotStall(t *testing.T) {
	h := coordHarness(t)
	h.ticks(2, time.Minute)
	intent := Event{V: EventVersion, Time: h.clock.Now(), Session: "c", Kind: KindIntent, Mode: ModeLive,
		Act: &ActPayload{Action: ActWake, Intent: "cut", Key: "wake:widgets:1", Reason: "finished: a"}}
	if err := NewLogWriter(h.buf).Append(intent); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	h.sup.surfaceCutShort()
	h.clock.advance(time.Minute)
	h.status("b", "# b\n\nDONE\n")
	h.ticks(3, time.Minute)
	if w := h.wakes(); len(w) != 1 || w[0].Reason != "finished: b" {
		t.Fatalf("after a cut-short wake: %+v", w)
	}
	// The pointer was typed, but the transcript never moved after it.
	it := ItemPayload{ID: "gitlab:example-org/widgets#4", Kind: "issue", Repository: "gitlab/example-org/widgets", Title: "Widget squeaks"}
	h.sup.emit("", Event{Kind: KindItem, Mode: ModeShared, Item: &it})
	h.ticks(3, time.Minute)
	if n := len(h.wakes()); n != 1 {
		t.Fatalf("woke within the stall: %d", n)
	}
	h.ticks(2, h.cfg.Policies.Nudge.Stall.Duration)
	if w := h.wakes(); len(w) != 2 || w[1].Reason != "item: "+it.ID {
		t.Fatalf("after the stall: %+v", w)
	}
}

// The reasons a wake is due for count against the byte limit too: a busy
// repository's first pack keeps a few and counts the rest.
func TestPackCountsDueOverLimit(t *testing.T) {
	h := coordHarness(t)
	var due []string
	for i := 0; i < 600; i++ {
		due = append(due, fmt.Sprintf("item: gitlab:example-org/widgets#%d", i))
	}
	p, b := h.sup.buildPack("widgets", due, h.clock.Now())
	if len(b) > h.cfg.Policies.Coordinator.PackBytes || p.Summarized.Due == 0 || len(p.Due)+p.Summarized.Due != 600 {
		t.Fatalf("pack %d bytes, due %d shown, %d summarized", len(b), len(p.Due), p.Summarized.Due)
	}
}

// A shadow run claims no coordinator: the claim is a shared fact that
// would hold against the live run.
func TestShadowClaimsNoCoordinator(t *testing.T) {
	h := coordHarness(t)
	h.restart(true)
	h.status("a", "# a\n\nDONE\n")
	h.ticks(3, time.Minute)
	if n := h.count(KindCoordinator, "", ""); n != 0 {
		t.Fatalf("a shadow run logged %d claims", n)
	}
	if n := h.count(KindWould, ActWake, "c"); n != 1 {
		t.Fatalf("would-be wakes = %d", n)
	}
	// A would-be wake is taken as the coordinator's business until the
	// stall, as live mode would wait for it.
	h.status("b", "# b\n\nDONE\n")
	h.ticks(3, time.Minute)
	if n := h.count(KindWould, ActWake, "c"); n != 1 {
		t.Fatalf("would-be wakes during a would-be wake = %d", n)
	}
	h.restart(false)
	h.ticks(2, time.Minute)
	if n := h.count(KindCoordinator, "", "c"); n != 1 {
		t.Fatalf("live claims = %d", n)
	}
}

// Two runs that claim one repository in the same pass settle it by the
// log's order: the claim written first holds, in memory as on replay.
func TestRaceForClaimSettledByLogOrder(t *testing.T) {
	h := coordHarness(t)
	rival := NewLogWriter(h.buf)
	once := false
	h.tmux.onPane = func(name string) {
		if name == "c" && !once {
			once = true
			rival.Append(Event{V: EventVersion, Time: h.clock.Now(), Session: "elsewhere", Kind: KindCoordinator, Mode: ModeShared,
				Coordinator: &CoordinatorPayload{Repository: "widgets"}})
		}
	}
	h.status("a", "# a\n\nDONE\n")
	h.ticks(3, time.Minute)
	if got := h.sup.st.Coordinators["widgets"]; got != "elsewhere" {
		t.Fatalf("holder in memory = %q", got)
	}
	if got := NewLogState(h.cfg, ModeLive).Fold(h.events()).Coordinators["widgets"]; got != "elsewhere" {
		t.Fatalf("holder on replay = %q", got)
	}
	if n := len(h.wakes()); n != 0 {
		t.Fatalf("the losing coordinator woke: %d", n)
	}
}

// A confidential term across the cut is still found: turns are screened
// whole, then cut.
func TestTurnScreenedBeforeCut(t *testing.T) {
	h := coordHarness(t)
	h.cfg.ConfidentialPatterns = []string{`hunter2`}
	h.restart(false)
	h.transcriptOf("a", "t1", promptLine("u1", strings.Repeat("x", turnText-3)+"hunter2 more", h.clock.Now()))
	h.ticks(2, time.Minute)
	for _, e := range h.events() {
		if e.Kind == KindTurn && (e.Turn.Withheld != 1 || e.Turn.Text != "") {
			t.Fatalf("turn = %+v", e.Turn)
		}
	}
	if h.count(KindTurn, "", "a") != 1 {
		t.Fatal("no turn logged")
	}
}

// A coordinator's errors name the field the owner wrote.
func TestCoordinatorErrorsNameTheirField(t *testing.T) {
	h := coordHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Coordinator = &Coordinator{Session: "c2", Workdir: filepath.Join(h.dir, "missing"), Account: "nosuch"}
	h.cfg.Repositories["widgets"] = w
	err := h.cfg.finish(h.dir)
	if err == nil || !strings.Contains(err.Error(), "repositories.widgets.coordinator.account") ||
		!strings.Contains(err.Error(), "repositories.widgets.coordinator.workdir") || strings.Contains(err.Error(), "sessions.c2") {
		t.Fatalf("errors: %v", err)
	}
}

// What another process logs during the pass a wake is made in, at the
// wake's own time, is due at the next wake, not lost to it.
func TestDueLoggedDuringAWakesPassIsNotLost(t *testing.T) {
	h := coordHarness(t)
	h.ticks(2, time.Minute)
	other := NewLogWriter(h.buf)
	it := ItemPayload{ID: "gitlab:example-org/widgets#4", Kind: "issue", Repository: "gitlab/example-org/widgets", Title: "Widget squeaks"}
	once := false
	h.tmux.onPane = func(name string) {
		if name == "c" && !once {
			once = true
			other.Append(Event{V: EventVersion, Time: h.clock.Now(), Kind: KindItem, Mode: ModeShared, Item: &it})
		}
	}
	h.status("a", "# a\n\nDONE\n")
	h.ticks(2, time.Minute)
	if w := h.wakes(); len(w) != 1 || w[0].Reason != "finished: a" {
		t.Fatalf("wakes = %+v", w)
	}
	h.transcriptOf("c", "t3", summaryLine("c1", "m1", "Handled.", h.clock.Now(), 100))
	h.ticks(2, time.Minute)
	if w := h.wakes(); len(w) != 2 || w[1].Reason != "item: "+it.ID {
		t.Fatalf("wakes = %+v", w)
	}
	h.transcriptOf("c", "t3", summaryLine("c2", "m2", "Handled.", h.clock.Now(), 100))
	h.restart(false)
	h.ticks(3, time.Minute)
	if n := len(h.wakes()); n != 2 {
		t.Fatalf("woke again after a replay: %d", n)
	}
}

// A turn that ends other than with end_turn, an interruption, counts as
// handled once the screen has been still for the stall.
func TestInterruptedWakeCountsAsHandled(t *testing.T) {
	h := coordHarness(t)
	h.status("a", "# a\n\nDONE\n")
	h.ticks(2, time.Minute)
	h.transcriptOf("c", "t3", promptLine("c1", "[Request interrupted by user]", h.clock.Now()))
	h.status("b", "# b\n\nDONE\n")
	h.ticks(3, time.Minute)
	if n := len(h.wakes()); n != 1 {
		t.Fatalf("woke within the stall: %d", n)
	}
	h.ticks(2, h.cfg.Policies.Nudge.Stall.Duration)
	if w := h.wakes(); len(w) != 2 || w[1].Reason != "finished: b" {
		t.Fatalf("wakes = %+v", w)
	}
}

// A coordinator whose screen moves between the read and the typing is
// logged once for the wake, not once a pass.
func TestMovingScreenSkipsAWakeOnce(t *testing.T) {
	h := coordHarness(t)
	n := 0
	h.tmux.onPane = func(name string) {
		if name == "c" {
			n++
			h.tmux.panes["c"] = fmt.Sprintf("thinking %d", n)
		}
	}
	h.status("a", "# a\n\nDONE\n")
	h.ticks(5, time.Minute)
	skips := 0
	for _, e := range h.events() {
		if e.Kind == KindSkip && e.Session == "c" && strings.HasPrefix(e.Skip.Key, "screen-changed:") {
			skips++
		}
	}
	if skips != 1 || len(h.wakes()) != 0 {
		t.Fatalf("skips %d, wakes %d", skips, len(h.wakes()))
	}
}

// Trimming drops the oldest turn of the pack, whichever session it is.
func TestPackDropsTheOldestTurn(t *testing.T) {
	h := coordHarness(t)
	old, recent := h.clock.Now(), h.clock.Now().Add(time.Hour)
	var a, b string
	h.cfg.Policies.Coordinator.Turns = 5
	for i := 0; i < 5; i++ {
		if i < 4 {
			a += promptLine(fmt.Sprint("a", i), "older work "+strings.Repeat("o", 40), old)
		}
		b += promptLine(fmt.Sprint("b", i), "newer work "+strings.Repeat("n", 40), recent)
	}
	h.transcriptOf("a", "t1", a)
	h.transcriptOf("b", "t2", b)
	h.ticks(2, time.Minute)
	_, full := h.sup.buildPack("widgets", nil, h.clock.Now())
	h.cfg.Policies.Coordinator.PackBytes = len(full) - 1
	p, _ := h.sup.buildPack("widgets", nil, h.clock.Now())
	if len(p.Sessions[0].Turns) != 3 || len(p.Sessions[1].Turns) != 5 || p.Summarized.Turns != 1 {
		t.Fatalf("a shows %d, b %d, summarized %d", len(p.Sessions[0].Turns), len(p.Sessions[1].Turns), p.Summarized.Turns)
	}
}
