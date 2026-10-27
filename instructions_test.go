package main

import (
	"strings"
	"testing"
	"time"
)

// instrHarness is repoHarness: a and b work widgets, d readonly.
func instrHarness(t *testing.T) *harness {
	t.Helper()
	return repoHarness(t)
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
// its text, and marked withheld.
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
}
