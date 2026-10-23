package main

import (
	"strings"
	"testing"
	"time"
)

const dialog = "Edit file src/main.go\nDo you want to make this edit to main.go?\n❯ 1. Yes\n  2. Yes, and don't ask again\n  3. No\n"

// typed counts every tmux write that types or sends keys to a session.
func typed(h *harness, name string) int {
	return h.tmux.count("type", name) + h.tmux.count("keys", name)
}

// A session stopped at a permission or confirmation dialog is blocked on
// the owner: it is never typed into or nudged, and one escalation names the
// dialog.
func TestDialogIsBlockedOnOwnerNotTyped(t *testing.T) {
	for name, pane := range map[string]string{
		"permission": dialog,
		"menu":       "Pick one\n❯ 1. Keep going\n  2. Stop\n",
		"confirm":    "Apply the migration?\nEnter to confirm · Esc to cancel\n",
	} {
		h := newHarness(t, false, "s1")
		h.status("s1", working)
		h.alive("s1", pane)
		h.ticks(10*12, 5*time.Minute)
		if got := typed(h, "s1"); got != 0 {
			t.Fatalf("%s: typed %d times into a dialog", name, got)
		}
		if h.state("s1") != BlockedOnOwner {
			t.Fatalf("%s: state = %s, want blocked-on-owner", name, h.state("s1"))
		}
		if len(h.runner.calls) != 1 || !strings.Contains(h.runner.calls[0][4], "dialog") {
			t.Fatalf("%s: escalations = %v, want one naming the dialog", name, h.runner.calls)
		}
	}
}

// A dialog that has scrolled off the visible screen does not block.
func TestDialogInScrollbackDoesNotBlock(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.alive("s1", "thinking")
	h.tmux.history["s1"] = dialog
	h.ticks(2, 5*time.Minute)
	if h.state("s1") == BlockedOnOwner {
		t.Fatal("a dialog in the scrollback blocked the session")
	}
}

// A static screen whose transcript last stopped on a tool call is waiting
// on the owner, most likely at a permission prompt: it is not nudged.
func TestStaticToolUseIsNotNudged(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.cfg.TranscriptDir = h.dir + "/transcripts"
	h.status("s1", working)
	h.alive("s1", "static")
	dir := TranscriptDir(h.cfg.TranscriptDir, h.cfg.Sessions["s1"].Cwd)
	writeFile(t, dir+"/abc.jsonl", `{"type":"assistant","timestamp":"`+h.clock.now.Format(time.RFC3339Nano)+`","message":{"stop_reason":"tool_use"}}`+"\n")
	h.ticks(10*12, 5*time.Minute)
	if got := typed(h, "s1"); got != 0 {
		t.Fatalf("nudged %d times while a tool call waited", got)
	}
	if h.state("s1") != BlockedOnOwner || len(h.runner.calls) != 1 {
		t.Fatalf("state = %s, escalations = %v", h.state("s1"), h.runner.calls)
	}
}

// The usage-limit resume message goes only to a session that can take it:
// not one that is DONE, has open Questions, or is at a dialog.
func TestResumeOnlyToATypableSession(t *testing.T) {
	cases := map[string]func(h *harness){
		"done":      func(h *harness) { h.status("s1", "# Finished\n\nDONE\n"); h.tmux.panes["s1"] = "> " },
		"questions": func(h *harness) { h.status("s1", blocked); h.tmux.panes["s1"] = "> " },
		"dialog":    func(h *harness) { h.tmux.panes["s1"] = dialog },
	}
	for name, after := range cases {
		h := newHarness(t, false, "s1")
		h.status("s1", working)
		h.alive("s1", "Limit reached, resets 3pm")
		h.ticks(2, 5*time.Minute)
		after(h)
		h.ticks(6, 5*time.Minute)
		if got := h.tmux.count("type", "s1"); got != 0 {
			t.Fatalf("%s: typed %d lines", name, got)
		}
	}
}

// Trust keys are sent at most once per screen: a prompt that stays on
// screen after the keys gets no second send.
func TestTrustKeysOncePerScreen(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.alive("s1", "Do you trust this folder?")
	h.tmux.sticky["s1"] = true
	h.ticks(12, 5*time.Minute)
	if got := h.tmux.count("keys", "s1"); got != 1 {
		t.Fatalf("key sends = %d, want 1 for one screen", got)
	}
	h.restart(false)
	h.ticks(3, 5*time.Minute)
	if got := h.tmux.count("keys", "s1"); got != 1 {
		t.Fatalf("key sends = %d after replay, want still 1", got)
	}
}

// The trust prompt is answered only when it is on the visible screen, and
// never when STATUS.md has open Questions.
func TestTrustOnlyOnScreenAndNotOverQuestions(t *testing.T) {
	h := newHarness(t, false, "scroll", "asks")
	h.status("scroll", working)
	h.alive("scroll", "working away")
	h.tmux.history["scroll"] = "Do you trust this folder?"
	h.status("asks", blocked)
	h.alive("asks", "Do you trust this folder?")
	h.ticks(3, 5*time.Minute)
	if got := h.tmux.count("keys", ""); got != 0 {
		t.Fatalf("key sends = %d", got)
	}
	if h.state("asks") != BlockedOnOwner {
		t.Fatalf("asks: state = %s, want blocked-on-owner", h.state("asks"))
	}
}

// Nothing is typed into a pane that is not running Claude, such as the
// shell left after Claude exited; one skip says why.
func TestNoTypingIntoAPaneNotRunningClaude(t *testing.T) {
	h := newHarness(t, false, "idle", "trust", "limit")
	for _, n := range []string{"idle", "trust", "limit"} {
		h.status(n, working)
		h.tmux.commands[n] = "bash"
	}
	h.alive("idle", "$ ")
	h.alive("trust", "Do you trust this folder?")
	h.alive("limit", "Limit reached, resets 3pm")
	h.ticks(1, 5*time.Minute)
	h.tmux.panes["limit"] = "$ "
	h.ticks(12, 5*time.Minute)
	for _, n := range []string{"idle", "trust", "limit"} {
		if got := typed(h, n); got != 0 {
			t.Fatalf("%s: typed %d times into bash", n, got)
		}
		skips := 0
		for _, e := range h.events() {
			if e.Kind == KindSkip && e.Session == n && strings.Contains(detailString(e, "reason"), "bash") {
				skips++
			}
		}
		if skips != 1 {
			t.Fatalf("%s: skips naming the command = %d, want 1", n, skips)
		}
	}
	h.cfg.Policies.PaneCommands = []string{"claude", "bash"}
	h.ticks(1, 5*time.Minute)
	if typed(h, "idle")+typed(h, "trust") == 0 {
		t.Fatal("a configured pane command was not allowed")
	}
}
