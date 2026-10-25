package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// instrHarness is coordHarness with readonly's coordinator e as well.
func instrHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, false, "a", "b", "d")
	h.cfg.TranscriptDir = filepath.Join(h.dir, "projects")
	d := h.cfg.Sessions["d"]
	d.Repository = "readonly"
	h.cfg.Sessions["d"] = d
	for repo, co := range map[string]string{"widgets": "c", "readonly": "e"} {
		r := h.cfg.Repositories[repo]
		r.Coordinator = &Coordinator{Session: co, Workdir: filepath.Join(h.dir, co)}
		h.cfg.Repositories[repo] = r
		if err := os.MkdirAll(filepath.Join(h.dir, co), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		h.alive(n, "screen of "+n)
	}
	h.restart(false)
	return h
}

// reply is the owner's reply in console, as agentd console reply records it.
func reply(h *harness, console, text string) error {
	st := NewLogState(h.cfg, mode(h.sup.shadow))
	st.Fold(h.events())
	return ConsoleReply(h.cfg, NewLogWriter(h.buf), st, h.clock.Now(), console, text)
}

func instructions(h *harness) []*InstructionPayload {
	var out []*InstructionPayload
	for _, e := range h.events() {
		if e.Kind == KindInstruction {
			out = append(out, e.Instruction)
		}
	}
	return out
}

// A free-text reply is recorded once, wakes its repository's coordinator
// once, and stays in that coordinator's pack until a wake that carried it
// was handled; another repository's coordinator never sees it.
func TestInstructionWakesItsCoordinatorOnce(t *testing.T) {
	h := instrHarness(t)
	h.ticks(2, time.Minute)
	const text = "Put the squeak fix ahead of the docs."
	for i := 0; i < 2; i++ {
		if err := reply(h, "widgets", text); err != nil {
			t.Fatal(err)
		}
	}
	ins := instructions(h)
	if len(ins) != 1 || ins[0].Text != text || ins[0].From != "console:widgets" || strings.Join(ins[0].Repositories, ",") != "widgets" || !strings.HasPrefix(ins[0].ID, "I") {
		t.Fatalf("instructions = %+v", ins)
	}
	h.ticks(3, time.Minute)
	w := h.wakes()
	if len(w) != 1 || w[0].Reason != "instruction: "+ins[0].ID {
		t.Fatalf("wakes = %+v", w)
	}
	if h.tmux.count("type", "e") != 0 {
		t.Fatal("readonly's coordinator was woken")
	}
	p := h.pack(t)
	if len(p.OwnerInstructions) != 1 || p.OwnerInstructions[0].Text != text || p.OwnerInstructions[0].ID != ins[0].ID {
		t.Fatalf("pack instructions = %+v", p.OwnerInstructions)
	}
	if other, _ := h.sup.buildPack("readonly", nil, h.clock.Now(), false); len(other.OwnerInstructions) != 0 {
		t.Fatalf("readonly's pack shows %+v", other.OwnerInstructions)
	}
	// Still on its wake: no second wake, and a launch's pack still shows it.
	h.ticks(3, time.Minute)
	if n := len(h.wakes()); n != 1 {
		t.Fatalf("%d wakes", n)
	}
	if p, _ := h.sup.buildPack("widgets", nil, h.clock.Now(), false); len(p.OwnerInstructions) != 1 {
		t.Fatalf("unhandled instruction left the pack: %+v", p.OwnerInstructions)
	}
	// Handled: the next wake's pack no longer carries it, and a restart
	// of agentd wakes nothing for it.
	h.transcriptOf("c", "t3", summaryLine("c1", "m1", "Reordered.", h.clock.Now(), 100))
	h.status("a", "# a\n\nDONE\n")
	h.ticks(3, time.Minute)
	if w := h.wakes(); len(w) != 2 || w[1].Reason != "finished: a" {
		t.Fatalf("wakes = %+v", w)
	}
	if p := h.pack(t); len(p.OwnerInstructions) != 0 {
		t.Fatalf("handled instruction still in the pack: %+v", p.OwnerInstructions)
	}
	h.transcriptOf("c", "t3", summaryLine("c2", "m2", "Noted.", h.clock.Now(), 100))
	h.restart(false)
	h.ticks(3, time.Minute)
	if n := len(h.wakes()); n != 2 {
		t.Fatalf("woke again after a restart: %d", n)
	}
	// The same words later, once handled, are a new instruction.
	if err := reply(h, "widgets", text); err != nil {
		t.Fatal(err)
	}
	if n := len(instructions(h)); n != 2 {
		t.Fatalf("%d instructions", n)
	}
}

// A reply is an answer or a seen mark when it reads as one, an error when
// it nearly does, and otherwise an instruction; a console that feeds no
// repository takes none.
func TestConsoleReplyKinds(t *testing.T) {
	h := instrHarness(t)
	h.ticks(1, time.Minute)
	it := ItemPayload{ID: "gitlab:example-org/widgets#4", Kind: "issue", Repository: "gitlab/example-org/widgets", Title: "Widget squeaks"}
	h.sup.emit("", Event{Kind: KindItem, Mode: ModeShared, Item: &it})
	for _, bad := range []string{"a D9 yes", "a Q1@0123abcd yes", "a D3", "a D3@abcd1234", "seen", "seen gitlab:example-org/widgets#4 gitlab:example-org/widgets#99"} {
		if err := reply(h, "widgets", bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	if err := reply(h, "nowhere", "Stop for the night."); err == nil {
		t.Error("a console that feeds no repository took an instruction")
	}
	if len(instructions(h)) != 0 {
		t.Fatalf("instructions = %+v", instructions(h))
	}
	if err := reply(h, "widgets", "seen gitlab:example-org/widgets#4"); err != nil || h.count(KindSeen, "", "") != 1 {
		t.Fatalf("seen mark: %v", err)
	}
	for _, text := range []string{"seen it, go ahead", "a: hold the release", "Hold everything."} {
		if err := reply(h, "widgets", text); err != nil {
			t.Fatalf("%q: %v", text, err)
		}
	}
	if n := len(instructions(h)); n != 3 {
		t.Fatalf("%d instructions: %+v", n, instructions(h))
	}
}

// An instruction that matches a confidential pattern is recorded without
// its text, and the pack says it was withheld.
func TestInstructionScreened(t *testing.T) {
	h := instrHarness(t)
	h.cfg.ConfidentialPatterns = []string{`(?i)secret-project`}
	h.restart(false)
	h.ticks(1, time.Minute)
	if err := reply(h, "widgets", "Move secret-project work first."); err != nil {
		t.Fatal(err)
	}
	ins := instructions(h)
	if len(ins) != 1 || ins[0].Text != "" || ins[0].Withheld != 1 {
		t.Fatalf("instructions = %+v", ins)
	}
	if strings.Contains(h.buf.String(), "secret-project") || ins[0].Hash != "" {
		t.Fatalf("the log holds the confidential text or its hash: %+v", ins[0])
	}
	h.ticks(3, time.Minute)
	if p := h.pack(t); len(p.OwnerInstructions) != 1 || !p.OwnerInstructions[0].Withheld {
		t.Fatalf("pack = %+v", p.OwnerInstructions)
	}
}

// An instruction a wake cut short covered is still due: the next wake
// carries it, and that wake's handling retires it.
func TestInstructionCutShortWakeLeavesItDue(t *testing.T) {
	h := instrHarness(t)
	h.ticks(2, time.Minute)
	if err := reply(h, "widgets", "Hold the release."); err != nil {
		t.Fatal(err)
	}
	var at time.Time
	for _, e := range h.events() {
		if e.Kind == KindInstruction {
			at = e.Time
		}
	}
	id := instructions(h)[0].ID
	key := instructionReason + id + "@" + at.UTC().Format(time.RFC3339Nano)
	intent := Event{V: EventVersion, Time: h.clock.Now(), Session: "c", Kind: KindIntent, Mode: ModeLive,
		Act: &ActPayload{Action: ActWake, Intent: "cut", Key: "wake:widgets:1", Reason: instructionReason + id, Covered: []string{key}}}
	if err := NewLogWriter(h.buf).Append(intent); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	h.sup.surfaceCutShort()
	h.clock.advance(time.Minute)
	h.ticks(3, time.Minute)
	if w := h.wakes(); len(w) != 1 || w[0].Reason != instructionReason+id {
		t.Fatalf("after a cut-short wake: %+v", w)
	}
	if p := h.pack(t); len(p.OwnerInstructions) != 1 {
		t.Fatalf("pack = %+v", p.OwnerInstructions)
	}
	h.transcriptOf("c", "t3", summaryLine("c1", "m1", "Held.", h.clock.Now(), 100))
	h.status("a", "# a\n\nDONE\n")
	h.ticks(3, time.Minute)
	if p := h.pack(t); len(h.wakes()) != 2 || len(p.OwnerInstructions) != 0 {
		t.Fatalf("wakes %+v, pack %+v", h.wakes(), p.OwnerInstructions)
	}
}

// A repository the console feeds that has no coordinator never handles an
// instruction, so it does not hold back the same words sent again once
// the coordinators have handled the first.
func TestInstructionRepeatedPastARepositoryWithoutCoordinator(t *testing.T) {
	h := instrHarness(t)
	r := h.cfg.Repositories["readonly"]
	r.Coordinator, r.Console = nil, "widgets"
	h.cfg.Repositories["readonly"] = r
	delete(h.cfg.Sessions, "e")
	h.restart(false)
	h.ticks(2, time.Minute)
	const text = "Hold the release."
	if err := reply(h, "widgets", text); err != nil {
		t.Fatal(err)
	}
	if ins := instructions(h); len(ins) != 1 || strings.Join(ins[0].Repositories, ",") != "readonly,widgets" {
		t.Fatalf("instructions = %+v", ins)
	}
	h.ticks(3, time.Minute)
	h.transcriptOf("c", "t3", summaryLine("c1", "m1", "Held.", h.clock.Now(), 100))
	h.status("a", "# a\n\nDONE\n")
	h.ticks(3, time.Minute)
	if n := len(h.wakes()); n != 2 {
		t.Fatalf("%d wakes", n)
	}
	if err := reply(h, "widgets", text); err != nil {
		t.Fatal(err)
	}
	if n := len(instructions(h)); n != 2 {
		t.Fatalf("%d instructions", n)
	}
}
