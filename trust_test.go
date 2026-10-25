package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The folder-trust dialog as Claude Code words it now, for directory dir:
// "No, exit" first and selected.
func newTrustScreen(dir string) string {
	return "Accessing workspace:\n\n" + dir + "\n\nQuick safety check: Is this a project you created or one you trust?\n\n" +
		"❯ 1. No, exit\n  2. Yes, I trust this folder\n\nEnter to confirm · Esc to cancel"
}

// The older wording: "Yes, proceed" first and selected.
const oldTrustScreen = "Do you trust the files in this folder?\n\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to exit"

// noEnterOnReject fails when Enter was sent in a send made while the
// screen had a rejecting option selected: each send's screen is the one
// recorded with its action.
func noEnterOnReject(t *testing.T, h *harness, name string) {
	t.Helper()
	for _, e := range h.events() {
		if e.Kind != KindAction || actionOf(e) != ActTrust || e.Session != name {
			continue
		}
		for _, k := range e.Act.Keys {
			if k == "Enter" && e.Act.Reason != "" && strings.Contains(e.Act.Reason, "No") {
				t.Fatalf("Enter sent with a rejecting option selected: %+v", e.Act)
			}
		}
	}
}

// The new dialog is answered by its accepting option: the arrow to it
// first, Enter only once the screen shows it selected; the action records
// the directory.
func TestTrustNewDialogAnsweredByAcceptingOption(t *testing.T) {
	h := newHarness(t, false, "s1")
	cwd := h.cfg.Sessions["s1"].Cwd
	h.status("s1", working)
	h.alive("s1", newTrustScreen(cwd))
	h.ticks(3, 5*time.Minute)
	sent := keysSent(h, "s1")
	if len(sent) != 2 || strings.Join(sent[0], " ") != "Down" || strings.Join(sent[1], " ") != "Enter" {
		t.Fatalf("keys %v", sent)
	}
	if h.tmux.chosen["s1"] != 2 || !h.tmux.alive["s1"] {
		t.Fatalf("chosen %d, alive %v", h.tmux.chosen["s1"], h.tmux.alive["s1"])
	}
	n := 0
	for _, e := range h.events() {
		if e.Kind == KindAction && actionOf(e) == ActTrust {
			n++
			if e.Act.Cwd != cwd {
				t.Fatalf("trust action %+v", e.Act)
			}
		}
	}
	if n != 2 {
		t.Fatalf("%d trust actions", n)
	}
	noEnterOnReject(t, h, "s1")
}

// The older wording, its accepting option already selected, gets Enter.
func TestTrustOldDialogAnswered(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.alive("s1", oldTrustScreen)
	h.ticks(3, 5*time.Minute)
	if sent := keysSent(h, "s1"); len(sent) != 1 || strings.Join(sent[0], " ") != "Enter" || h.tmux.chosen["s1"] != 1 {
		t.Fatalf("keys %v, chosen %d", sent, h.tmux.chosen["s1"])
	}
}

// A dialog for a directory other than the one agentd launches the session
// in gets no keys, and the owner hears once: the pane is elsewhere, or the
// dialog names another directory.
func TestTrustRefusedForAnotherDirectory(t *testing.T) {
	h := newHarness(t, false, "moved", "named")
	for _, n := range []string{"moved", "named"} {
		h.status(n, working)
	}
	h.tmux.paths["moved"] = filepath.Join(h.dir, "elsewhere")
	h.alive("moved", newTrustScreen(filepath.Join(h.dir, "elsewhere")))
	h.alive("named", newTrustScreen(filepath.Join(h.dir, "other-repo")))
	h.ticks(6, 5*time.Minute)
	for _, n := range []string{"moved", "named"} {
		if sent := keysSent(h, n); len(sent) != 0 {
			t.Fatalf("%s: keys %v", n, sent)
		}
		said := 0
		for _, c := range h.runner.calls {
			if strings.Contains(strings.Join(c, " "), n) && strings.Contains(strings.Join(c, " "), "trust") {
				said++
			}
		}
		if said != 1 {
			t.Fatalf("%s: %d escalations, want 1: %v", n, said, h.runner.calls)
		}
	}
}

// Enter alone is never sent while a rejecting option is selected: not
// when the arrow did not take, not when the menu has no accepting option,
// not when the cursor cannot be seen; with the older wording's No
// selected, only after the arrow took.
func TestTrustNeverEnterOnRejectingOption(t *testing.T) {
	h := newHarness(t, false, "sticky", "noyes", "nocursor", "oldno")
	for _, n := range []string{"sticky", "noyes", "nocursor", "oldno"} {
		h.status(n, working)
	}
	cwd := func(n string) string { return h.cfg.Sessions[n].Cwd }
	h.alive("sticky", newTrustScreen(cwd("sticky")))
	h.tmux.sticky["sticky"] = true
	h.alive("noyes", "Is this a project you created or one you trust?\n\n❯ 1. No, exit\n  2. Cancel\n\nEnter to confirm · Esc to cancel")
	h.alive("nocursor", "Is this a project you created or one you trust?\n\n  1. No, exit\n  2. Yes, I trust this folder\n\nEnter to confirm · Esc to cancel")
	h.alive("oldno", "Do you trust the files in this folder?\n\n  1. Yes, proceed\n❯ 2. No, exit\n\nEnter to confirm · Esc to exit")
	h.ticks(8, 5*time.Minute)
	for _, n := range []string{"sticky", "noyes", "nocursor"} {
		for _, keys := range keysSent(h, n) {
			for _, k := range keys {
				if k == "Enter" {
					t.Fatalf("%s: Enter sent: %v", n, keysSent(h, n))
				}
			}
		}
		if !h.tmux.alive[n] {
			t.Fatalf("%s: the session exited", n)
		}
	}
	if sent := keysSent(h, "sticky"); len(sent) != 1 || strings.Join(sent[0], " ") != "Down" {
		t.Fatalf("sticky: %v", sent)
	}
	// Up to Yes, then Enter once the screen shows Yes selected.
	if sent := keysSent(h, "oldno"); len(sent) != 2 || strings.Join(sent[0], " ") != "Up" || strings.Join(sent[1], " ") != "Enter" || h.tmux.chosen["oldno"] != 1 {
		t.Fatalf("oldno: %v, chosen %d", sent, h.tmux.chosen["oldno"])
	}
	noEnterOnReject(t, h, "oldno")
	for _, n := range []string{"noyes", "nocursor"} {
		if sent := keysSent(h, n); len(sent) != 0 {
			t.Fatalf("%s: %v", n, sent)
		}
	}
}

// Before a launch, agentd reads Claude Code's state for the session's
// login and tells the owner once when the directory is not trusted yet;
// the launch goes on. A trusted directory, or no state file, says nothing.
func TestTrustCheckedBeforeLaunch(t *testing.T) {
	h := newHarness(t, false, "fresh", "known")
	for _, n := range []string{"fresh", "known"} {
		h.status(n, working)
	}
	state := map[string]any{"projects": map[string]any{
		h.cfg.Sessions["known"].Cwd: map[string]any{"hasTrustDialogAccepted": true},
		h.cfg.Sessions["fresh"].Cwd: map[string]any{"hasTrustDialogAccepted": false},
	}}
	b, _ := json.Marshal(state)
	writeFile(t, filepath.Join(h.dir, ".claude.json"), string(b))
	h.ticks(3, 5*time.Minute)
	said := map[string]int{}
	for _, c := range h.runner.calls {
		line := strings.Join(c, " ")
		for _, n := range []string{"fresh", "known"} {
			if strings.Contains(line, h.cfg.Sessions[n].Cwd) && strings.Contains(line, "trust") {
				said[n]++
			}
		}
	}
	if said["fresh"] != 1 || said["known"] != 0 {
		t.Fatalf("escalations %v: %v", said, h.runner.calls)
	}
	if h.tmux.count("new-session", "fresh") == 0 && h.tmux.count("replace", "fresh") == 0 {
		t.Fatalf("fresh was not launched: %v", h.tmux.calls)
	}
	// No state file: nothing to say.
	h2 := newHarness(t, false, "s1")
	h2.status("s1", working)
	h2.ticks(3, 5*time.Minute)
	for _, c := range h2.runner.calls {
		if strings.Contains(strings.Join(c, " "), "trust") {
			t.Fatalf("escalated without a state file: %v", c)
		}
	}
}
