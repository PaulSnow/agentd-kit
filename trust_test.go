package main

import (
	"encoding/json"
	"os"
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

// noEnterOnReject fails when Enter reached the menu while an option
// other than one saying Yes was selected, as the fake tmux saw it.
func noEnterOnReject(t *testing.T, h *harness, name string) {
	t.Helper()
	h.tmux.mu.Lock()
	defer h.tmux.mu.Unlock()
	for _, opt := range h.tmux.enterOn[name] {
		if !strings.Contains(opt, "Yes") {
			t.Fatalf("Enter sent with %q selected", opt)
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
		noEnterOnReject(t, h, n)
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

// Claude Code's screen as it prints the new dialog: the question opens a
// paragraph on one line, the path sits under "Accessing workspace:", and a
// menu an earlier dialog left higher on the screen is not read as its own.
func TestTrustRealScreenAnswered(t *testing.T) {
	h := newHarness(t, false, "s1", "ascii")
	for _, n := range []string{"s1", "ascii"} {
		h.status(n, working)
	}
	cwd := h.cfg.Sessions["s1"].Cwd
	h.alive("s1", "╭──────╮\n│ ❯ 1. Yes, proceed │\n│   2. No, exit │\n╰──────╯\n"+
		"│ Accessing workspace:\n│\n│ "+cwd+"\n│\n"+
		"│ Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first.\n│\n"+
		"│ ❯ 1. No, exit\n│   2. Yes, I trust this folder\n│\n│ Enter to confirm · Esc to cancel")
	h.alive("ascii", "Is this a project you created or one you trust?\n\n> 1. No, exit\n  2. Yes, I trust this folder\n\nEnter to confirm · Esc to cancel")
	h.ticks(3, 5*time.Minute)
	if sent := keysSent(h, "s1"); len(sent) != 2 || strings.Join(sent[0], " ") != "Down" || strings.Join(sent[1], " ") != "Enter" {
		t.Fatalf("s1: keys %v", sent)
	}
	noEnterOnReject(t, h, "s1")
	if !h.tmux.alive["s1"] {
		t.Fatal("s1 exited")
	}
	if sent := keysSent(h, "ascii"); len(sent) != 2 || strings.Join(sent[0], " ") != "Down" || h.tmux.chosen["ascii"] != 2 {
		t.Fatalf("ascii: keys %v", sent)
	}
	noEnterOnReject(t, h, "ascii")
}

// A configured directory reached through a symbolic link is the
// directory tmux reports the pane in, resolved.
func TestTrustSymlinkedDirectory(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	real := h.cfg.Sessions["s1"].Cwd
	link := filepath.Join(h.dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	sess := h.cfg.Sessions["s1"]
	sess.Cwd = link
	h.cfg.Sessions["s1"] = sess
	h.alive("s1", newTrustScreen(real))
	h.ticks(3, 5*time.Minute)
	if sent := keysSent(h, "s1"); len(sent) != 2 {
		t.Fatalf("keys %v", sent)
	}
}

// A prompt agentd will not answer, and keys that did not take, go to the
// owner once each, rather than leaving the session starting in silence.
func TestTrustUnansweredIsEscalated(t *testing.T) {
	h := newHarness(t, false, "nocursor", "sticky")
	for _, n := range []string{"nocursor", "sticky"} {
		h.status(n, working)
	}
	h.alive("nocursor", "Is this a project you created or one you trust?\n\n  1. No, exit\n  2. Yes, I trust this folder\n\nEnter to confirm · Esc to cancel")
	h.alive("sticky", newTrustScreen(h.cfg.Sessions["sticky"].Cwd))
	h.tmux.sticky["sticky"] = true
	h.ticks(12, 5*time.Minute)
	for _, n := range []string{"nocursor", "sticky"} {
		said := 0
		for _, c := range h.runner.calls {
			if line := strings.Join(c, " "); strings.Contains(line, n+" ") && strings.Contains(line, "folder-trust") {
				said++
			}
		}
		if said != 1 {
			t.Fatalf("%s: %d escalations: %v", n, said, h.runner.calls)
		}
	}
}

// A directory under one Claude Code trusts is trusted.
func TestTrustParentTrusted(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, ".claude.json")
	writeFile(t, state, `{"projects": {"`+dir+`": {"hasTrustDialogAccepted": true}, "`+dir+`/b": {"hasTrustDialogAccepted": false}}}`)
	if trusted, known := claudeTrusts(state, filepath.Join(dir, "a", "wt")); !trusted || !known {
		t.Fatalf("under a trusted parent: %v %v", trusted, known)
	}
	writeFile(t, state, `{"projects": {"`+dir+`/b": {"hasTrustDialogAccepted": false}}}`)
	if trusted, known := claudeTrusts(state, filepath.Join(dir, "b")); trusted || !known {
		t.Fatalf("untrusted: %v %v", trusted, known)
	}
	if _, known := claudeTrusts(filepath.Join(dir, "none.json"), dir); known {
		t.Fatal("a missing file is known")
	}
}

// Keys that move the cursor or pick an option are refused: they would
// act on top of the option agentd selected.
func TestTrustKeysOnlyConfirm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agentd.json")
	for _, keys := range []string{`["Down", "Enter"]`, `["2"]`, `["Up"]`} {
		writeFile(t, path, `{"policies": {"trust_prompt": {"enabled": true, "pattern": "trust this folder", "keys": `+keys+`}}}`)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "trust_prompt.keys") {
			t.Errorf("%s: %v", keys, err)
		}
	}
	writeFile(t, path, `{"policies": {"trust_prompt": {"enabled": true, "pattern": "trust this folder", "keys": ["Enter"]}}}`)
	if _, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	}
}
