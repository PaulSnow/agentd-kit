package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// screen renders a recorded screen from testdata/screens with its spinner
// verb, elapsed-time counter and token count filled in for tick i, as a
// running Claude Code session would show them a moment apart.
func screen(t *testing.T, file string, i int) string {
	t.Helper()
	b, err := os.ReadFile("testdata/screens/" + file)
	if err != nil {
		t.Fatal(err)
	}
	verbs := []string{"Reviewing", "Pondering", "Cogitating", "Musing"}
	elapsed := fmt.Sprintf("%dm %ds", 4+i/60, i%60)
	if i%3 == 2 {
		elapsed = fmt.Sprintf("%dh %dm %ds", 1, i%60, i%7)
	}
	return strings.NewReplacer("{verb}", verbs[i%len(verbs)], "{elapsed}", elapsed,
		"{tokens}", fmt.Sprintf("%d.%dk", 8+i, i%10)).Replace(string(b))
}

// keysSent is every key list sent to name, in order.
func keysSent(h *harness, name string) [][]string {
	h.tmux.mu.Lock()
	defer h.tmux.mu.Unlock()
	var out [][]string
	for _, c := range h.tmux.calls {
		if c.op == "keys" && c.name == name {
			out = append(out, c.args)
		}
	}
	return out
}

// A screen that differs only in its spinner, its elapsed-time counter and
// its token count hashes the same; one whose text changed does not.
func TestStillHashIgnoresCounters(t *testing.T) {
	for i := 1; i < 200; i++ {
		if a, b := StillHash(screen(t, "subagent-working.txt", 0)), StillHash(screen(t, "subagent-working.txt", i)); a != b {
			t.Fatalf("tick %d: hashes differ for a screen whose counters alone moved:\n%s\n---\n%s",
				i, stillText(screen(t, "subagent-working.txt", 0)), stillText(screen(t, "subagent-working.txt", i)))
		}
	}
	moved := strings.Replace(screen(t, "subagent-working.txt", 3), "+14 more tool uses", "+15 more tool uses", 1)
	if StillHash(screen(t, "subagent-working.txt", 3)) == StillHash(moved) {
		t.Fatal("a screen whose text changed hashes as still")
	}
}

// A session whose screen moves only by its counters is still: after the
// stall time it is idle, and nudged, as a session showing a fixed screen
// would be.
func TestScreenDifferingOnlyInCounterIsStill(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.alive("s1", screen(t, "subagent-working.txt", 0))
	for i := 1; i <= 10; i++ { // 50 minutes, the counter moving every pass
		h.tmux.panes["s1"] = screen(t, "subagent-working.txt", i)
		h.ticks(1, 5*time.Minute)
	}
	if h.state("s1") != Idle {
		t.Fatalf("state = %s after 50 minutes of a moving counter alone, want idle", h.state("s1"))
	}
	if h.count(KindAction, ActNudge, "s1") == 0 {
		t.Fatal("a session still but for its counter was never nudged")
	}
}

// A subagent's permission dialog under a running counter is detected: the
// session is blocked on the owner, escalated once however its counter
// moves, and never typed into while the decline policy is off.
func TestSubagentDialogUnderCounterIsDetected(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.alive("s1", "starting")
	h.ticks(1, 5*time.Minute)
	for i := 0; i < 60; i++ { // five hours
		h.tmux.panes["s1"] = screen(t, "subagent-dialog.txt", i)
		h.ticks(1, 5*time.Minute)
		if h.state("s1") != BlockedOnOwner {
			t.Fatalf("tick %d: state = %s, want blocked-on-owner", i, h.state("s1"))
		}
	}
	if got := typed(h, "s1"); got != 0 {
		t.Fatalf("typed %d times into a dialog", got)
	}
	if len(h.runner.calls) != 1 || !strings.Contains(h.runner.calls[0][4], "dialog") {
		t.Fatalf("escalations = %v, want one naming the dialog", h.runner.calls)
	}
}

// The question line and numbered options are a dialog without any
// configured pattern; the same question quoted in output, with no options
// after it, is not.
func TestDialogRecognisedByItsShape(t *testing.T) {
	for name, c := range map[string]struct {
		pane string
		want bool
	}{
		"subagent": {screen(t, "subagent-dialog.txt", 5), true},
		"bare":     {"Do you want to create notes.md?\n❯ 1. Yes\n  2. No\n", true},
		"quoted":   {"> Do you want to proceed?\nthe test prints the prompt text\n", false},
		"list":     {"Steps:\n1. Build\n2. Test\n", false},
		"prose":    {"⏺ Which approach do you prefer?\n\n  1. Rewrite\n  2. Patch\n", false},
		"nocursor": {"Which approach do you prefer?\n  1. Rewrite\n  2. Patch\n", false},
		"working":  {screen(t, "subagent-working.txt", 5), false},
	} {
		if got := dialogShape(c.pane); got != c.want {
			t.Errorf("%s: dialogShape = %v, want %v", name, got, c.want)
		}
	}
	h := newHarness(t, false, "s1")
	h.cfg.Policies.Dialog.Patterns = nil
	h.restart(false)
	h.status("s1", working)
	h.alive("s1", screen(t, "subagent-dialog.txt", 0))
	h.ticks(2, 5*time.Minute)
	if h.state("s1") != BlockedOnOwner {
		t.Fatalf("with no patterns, state = %s, want blocked-on-owner", h.state("s1"))
	}
}

// With the decline policy on, a dialog still showing after its time is
// declined once with Escape, never approved, and the escalation says when
// it will be; a dialog that goes before its time is not declined.
func TestDialogDeclinedAfterPolicy(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.cfg.Policies.Dialog.DeclineAfter = Duration{20 * time.Minute}
	h.restart(false)
	h.status("s1", working)
	h.alive("s1", "starting")
	h.ticks(1, 5*time.Minute)
	for i := 0; i < 4; i++ { // shown 0..15 minutes
		h.tmux.panes["s1"] = screen(t, "subagent-dialog.txt", i)
		h.ticks(1, 5*time.Minute)
	}
	if got := keysSent(h, "s1"); len(got) != 0 {
		t.Fatalf("keys %v before the decline time", got)
	}
	if len(h.runner.calls) != 1 || !strings.Contains(h.runner.calls[0][4], "decline") {
		t.Fatalf("escalations = %v, want one saying it will be declined", h.runner.calls)
	}
	for i := 4; i < 12; i++ { // 20 minutes on, and the dialog still shows
		h.tmux.panes["s1"] = screen(t, "subagent-dialog.txt", i)
		h.ticks(1, 5*time.Minute)
	}
	got := keysSent(h, "s1")
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != "Escape" {
		t.Fatalf("keys = %v, want one Escape", got)
	}
	if h.count(KindAction, ActDecline, "s1") != 1 || h.tmux.count("type", "s1") != 0 {
		t.Fatalf("declines = %d, typed = %d", h.count(KindAction, ActDecline, "s1"), h.tmux.count("type", "s1"))
	}
	if len(h.runner.calls) != 2 || !strings.Contains(h.runner.calls[1][4], "still shows") {
		t.Fatalf("escalations = %v, want a second saying a dialog still shows after the decline", h.runner.calls)
	}
	sameAfterReplay(t, h, "after a decline")
	h.restart(false) // the decline is not repeated across a restart of agentd
	h.ticks(2, 5*time.Minute)
	if got := keysSent(h, "s1"); len(got) != 1 {
		t.Fatalf("keys = %v after a restart, want the one decline", got)
	}

	// A new episode that ends before its time is not declined.
	h.tmux.panes["s1"] = "busy"
	h.ticks(1, 5*time.Minute)
	h.tmux.panes["s1"] = screen(t, "subagent-dialog.txt", 1)
	h.ticks(3, 5*time.Minute)
	h.tmux.panes["s1"] = "busy again"
	h.ticks(3, 5*time.Minute)
	if got := keysSent(h, "s1"); len(got) != 1 {
		t.Fatalf("keys = %v, want no decline for a dialog that went in time", got)
	}
}

// In shadow mode a decline is only a would event.
func TestDialogDeclineShadow(t *testing.T) {
	h := newHarness(t, true, "s1")
	h.cfg.Policies.Dialog.DeclineAfter = Duration{10 * time.Minute}
	h.restart(true)
	h.status("s1", working)
	h.alive("s1", screen(t, "subagent-dialog.txt", 0))
	for i := 1; i < 8; i++ {
		h.tmux.panes["s1"] = screen(t, "subagent-dialog.txt", i)
		h.ticks(1, 5*time.Minute)
	}
	if got := keysSent(h, "s1"); len(got) != 0 {
		t.Fatalf("shadow sent keys %v", got)
	}
	if h.count(KindWould, ActDecline, "s1") != 1 {
		t.Fatalf("would-declines = %d, want 1", h.count(KindWould, ActDecline, "s1"))
	}
}

// A parked session is never typed into, whatever its screen shows, nor by
// any action that asks to type: the safeguard refuses it before the pane.
func TestParkedSessionNeverTyped(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.cfg.Policies.Dialog.DeclineAfter = Duration{5 * time.Minute}
	h.restart(false)
	h.status("s1", working)
	h.alive("s1", "static")
	h.ticks(1, 5*time.Minute)
	tr := h.sup.Tracks()["s1"]
	tr.Crashes, tr.Parked = []Crash{{Exit: 1, Screen: "abc"}, {Exit: 1, Screen: "abc"}}, true
	h.ticks(12, 5*time.Minute)
	h.tmux.panes["s1"] = screen(t, "subagent-dialog.txt", 0)
	h.ticks(4, 5*time.Minute)
	err := h.sup.typeInto("s1", ActNudge, &ActPayload{Message: "go on"}, tr, StillHash(h.tmux.panes["s1"]), func() error {
		return h.tmux.Type("s1", "go on")
	})
	if err != errParked {
		t.Fatalf("typeInto a parked session = %v, want errParked", err)
	}
	if got := typed(h, "s1"); got != 0 {
		t.Fatalf("typed %d times into a parked session", got)
	}
}

// A decline cut short by a crash of agentd may have sent Escape: it counts
// as sent, so the dialog is not declined twice.
func TestCutShortDeclineIsNotRepeated(t *testing.T) {
	cfg := DefaultConfig()
	st := NewLogState(&cfg, ModeLive)
	act := &ActPayload{Action: ActDecline, Intent: "i1", Keys: []string{"Escape"}, Episode: 1}
	for _, e := range []Event{
		{V: EventVersion, Time: pageAt, Kind: KindObserve, Mode: ModeLive, Session: "s1", Observe: &ObservePayload{State: string(BlockedOnOwner), Alive: true, Dialog: true}},
		{V: EventVersion, Time: pageAt, Kind: KindWarning, Mode: ModeLive, Session: "s1", Warning: &WarningPayload{Warning: "cut short", Intent: act}},
	} {
		st.apply(e)
	}
	if d := st.Tracks["s1"].Dialog; d.Declined != 1 || d.Episode != 1 {
		t.Fatalf("dialog = %+v, want episode 1 declined", d)
	}
}

// Words a pattern matches in a session's output block it and escalate, but
// are never declined: only a dialog's shape is.
func TestPatternAloneIsNotDeclined(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.cfg.Policies.Dialog.DeclineAfter = Duration{10 * time.Minute}
	h.restart(false)
	h.status("s1", working)
	h.alive("s1", "the test prints: Do you want to proceed")
	h.ticks(8, 5*time.Minute)
	if h.state("s1") != BlockedOnOwner {
		t.Fatalf("state = %s, want blocked-on-owner", h.state("s1"))
	}
	if got := keysSent(h, "s1"); len(got) != 0 {
		t.Fatalf("keys %v for a pattern without a dialog's shape", got)
	}
	if len(h.runner.calls) != 1 || strings.Contains(h.runner.calls[0][4], "decline") {
		t.Fatalf("escalations = %v, want one that promises no decline", h.runner.calls)
	}
	// A real dialog replacing the words, with no pass between them, is
	// timed from its own first showing.
	h.tmux.panes["s1"] = screen(t, "subagent-dialog.txt", 0)
	h.ticks(1, 5*time.Minute)
	if got := keysSent(h, "s1"); len(got) != 0 {
		t.Fatalf("keys %v at the dialog's first pass", got)
	}
	h.ticks(3, 5*time.Minute)
	if got := keysSent(h, "s1"); len(got) != 1 {
		t.Fatalf("keys = %v ten minutes after the dialog showed, want one Escape", got)
	}
}

// A dialog the escalation gap kept quiet is not declined: the owner is
// told of it first.
func TestUnescalatedDialogIsNotDeclined(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.cfg.Policies.Dialog.DeclineAfter = Duration{10 * time.Minute}
	h.restart(false)
	h.status("s1", working)
	h.alive("s1", screen(t, "subagent-dialog.txt", 0))
	h.ticks(1, 5*time.Minute) // escalated at 0m
	h.tmux.panes["s1"] = "busy"
	h.ticks(1, 5*time.Minute)
	h.tmux.panes["s1"] = screen(t, "subagent-dialog.txt", 1)
	h.ticks(4, 5*time.Minute) // 10m..25m: inside the gap, never escalated
	if got := keysSent(h, "s1"); len(got) != 0 {
		t.Fatalf("keys %v for a dialog the owner was not told of", got)
	}
	h.ticks(3, 5*time.Minute) // 30m: escalated, then declined
	if len(h.runner.calls) < 2 || len(keysSent(h, "s1")) != 1 {
		t.Fatalf("escalations %d, keys %v; want the second escalation and then the decline", len(h.runner.calls), keysSent(h, "s1"))
	}
}

// The decline time may not be negative.
func TestDialogDeclineAfterValidated(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Policies.Dialog.DeclineAfter = Duration{-time.Minute}
	if err := cfg.finish(t.TempDir()); err == nil || !strings.Contains(err.Error(), "decline_after") {
		t.Fatalf("err = %v, want one naming decline_after", err)
	}
}
