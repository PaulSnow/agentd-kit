package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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

// The dialog as Claude Code draws it now: the same menu without numbers
// (#43), under the link to its security guide.
func unnumberedTrustScreen(dir string) string {
	return "Accessing workspace:\n\n" + dir + "\n\nQuick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder first.\n\n" +
		"Claude Code'll be able to read, edit, and execute files here.\n\nSecurity guide\n\n" +
		"❯ No, exit\n  Yes, I trust this folder\n\nEnter to confirm · Esc to cancel"
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
	for _, keys := range []string{`["Down", "Enter"]`, `["2"]`, `["Up"]`, `["Escape"]`, `["j"]`, `["C-c"]`} {
		writeFile(t, path, `{"policies": {"trust_prompt": {"enabled": true, "pattern": "trust this folder", "keys": `+keys+`}}}`)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "trust_prompt.keys") {
			t.Errorf("%s: %v", keys, err)
		}
	}
	writeFile(t, path, `{"policies": {"trust_prompt": {"enabled": true, "pattern": "trust this folder", "keys": ["C-m"]}}}`)
	if _, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	}
}

// A session relaunched after the arrow, before the confirm, draws the
// same prompt on the same screen: it is answered afresh.
func TestTrustAnsweredAgainAfterRelaunch(t *testing.T) {
	tr := &Track{TrustHash: "h1"}
	tr.Ready = true
	tr.launched("", time.Now())
	if tr.TrustHash != "" || tr.Ready {
		t.Fatalf("trust hash or readiness kept over a launch: %q, %v", tr.TrustHash, tr.Ready)
	}
}

// The menu without numbers is answered by its accepting option too: the
// arrow to it, then Enter once it shows selected, never Enter on "No,
// exit" (#43).
func TestTrustUnnumberedDialogAnswered(t *testing.T) {
	h := newHarness(t, false, "s1", "boxed")
	for _, n := range []string{"s1", "boxed"} {
		h.status(n, working)
	}
	h.alive("s1", unnumberedTrustScreen(h.cfg.Sessions["s1"].Cwd))
	h.alive("boxed", "╭──────╮\n│ Is this a project you created or one you trust? │\n│ │\n│ Security guide │\n│ │\n│ ❯ No, exit │\n│   Yes, I trust this folder │\n│ │\n│ Enter to confirm · Esc to cancel │\n╰──────╯")
	h.ticks(3, 5*time.Minute)
	for _, n := range []string{"s1", "boxed"} {
		if sent := keysSent(h, n); len(sent) != 2 || strings.Join(sent[0], " ") != "Down" || strings.Join(sent[1], " ") != "Enter" {
			t.Fatalf("%s: keys %v", n, sent)
		}
		if h.tmux.chosen[n] != 2 || !h.tmux.alive[n] {
			t.Fatalf("%s: chosen %d, alive %v", n, h.tmux.chosen[n], h.tmux.alive[n])
		}
		noEnterOnReject(t, h, n)
	}
}

// An unnumbered menu agentd cannot read safely gets no keys: two options
// that say Yes, or no cursor.
func TestTrustUnnumberedUnclearMenu(t *testing.T) {
	re := regexp.MustCompile(DefaultConfig().Policies.Trust.Pattern)
	for _, screen := range []string{
		"Is this a project you created or one you trust?\n\n❯ Yes, proceed\n  Yes, I trust this folder\n  No, exit\n\nEnter to confirm · Esc to cancel",
		"Is this a project you created or one you trust?\n\n  No, exit\n  Yes, I trust this folder\n\nEnter to confirm · Esc to cancel",
	} {
		if keys, _ := trustKeys(trustMenu(screen, re, ""), []string{"Enter"}); keys != nil {
			t.Fatalf("keys %v for %q", keys, screen)
		}
	}
}

// A session whose first screens showed no trust prompt, and that has not
// been seen ready since its launch, still gets its trust prompt answered
// when it shows: it is starting, not waiting at a dialog (#43).
func TestTrustAnsweredBeforeReady(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.alive("s1", "Claude Code is loading")
	h.ticks(3, 5*time.Minute)
	h.alive("s1", unnumberedTrustScreen(h.cfg.Sessions["s1"].Cwd))
	h.ticks(3, 5*time.Minute)
	if sent := keysSent(h, "s1"); len(sent) != 2 || strings.Join(sent[1], " ") != "Enter" || h.tmux.chosen["s1"] != 2 {
		t.Fatalf("keys %v, chosen %d", sent, h.tmux.chosen["s1"])
	}
	noEnterOnReject(t, h, "s1")
	if n := h.tmux.count("type", "s1"); n != 0 {
		t.Fatalf("%d lines typed into a session not yet ready", n)
	}
}

// No line is typed, and no key but the trust keys sent, into a screen that
// shows the folder-trust prompt, whatever state the session was read in:
// a line would answer the menu (#43).
func TestNothingTypedIntoTrustPrompt(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.alive("s1", "static")
	h.ticks(1, 5*time.Minute)
	tr := h.sup.Tracks()["s1"]
	h.tmux.panes["s1"] = unnumberedTrustScreen(h.cfg.Sessions["s1"].Cwd)
	for _, act := range []string{ActNudge, ActResume, ActAnswer, ActWake, ActDecline} {
		err := h.sup.typeInto("s1", act, &ActPayload{Message: "go on"}, tr, StillHash(h.tmux.panes["s1"]), func() error {
			return h.tmux.Type("s1", "go on")
		})
		if err != errTrustPrompt {
			t.Fatalf("%s into a trust prompt = %v, want errTrustPrompt", act, err)
		}
	}
	if got := typed(h, "s1"); got != 0 {
		t.Fatalf("typed %d times into a trust prompt", got)
	}
	// Trust answering off with no pattern: a blank line is not the
	// question.
	h.sup.cfg.Policies.Trust.Enabled, h.sup.cfg.Policies.Trust.Pattern = false, ""
	h.sup.trustRe = regexp.MustCompile(`^(?:)$`)
	h.tmux.panes["s1"] = "static\n\n❯ "
	if err := h.sup.typeInto("s1", ActNudge, &ActPayload{Message: "go on"}, tr, StillHash(h.tmux.panes["s1"]), func() error {
		return h.tmux.Type("s1", "go on")
	}); err != nil || typed(h, "s1") != 1 {
		t.Fatalf("typeInto with no trust pattern = %v, typed %d", err, typed(h, "s1"))
	}
}

// A session seen ready since its launch is past its trust prompt: a trust
// menu it shows later is a dialog and gets no keys, and readiness is read
// back from the log after a restart of agentd (#43).
func TestTrustMenuAfterReadyIsDialog(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.alive("s1", "❯ ")
	h.ticks(2, 5*time.Minute)
	if !h.sup.Tracks()["s1"].Ready {
		t.Fatal("not ready at its input prompt")
	}
	h.restart(false)
	if !h.sup.Tracks()["s1"].Ready {
		t.Fatal("readiness lost over a restart")
	}
	h.tmux.panes["s1"] = unnumberedTrustScreen(h.cfg.Sessions["s1"].Cwd)
	h.ticks(3, 5*time.Minute)
	if sent := keysSent(h, "s1"); len(sent) != 0 {
		t.Fatalf("keys %v sent to a menu shown after the session was ready", sent)
	}
	if st := h.sup.Tracks()["s1"].State; st != BlockedOnOwner {
		t.Fatalf("state %s, want %s", st, BlockedOnOwner)
	}
}

// Only the visible screen counts: a trust question scrolled into the
// history does not stop a nudge (#43).
func TestTrustQuestionInHistoryDoesNotStopTyping(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.alive("s1", "static")
	h.ticks(1, 5*time.Minute)
	tr := h.sup.Tracks()["s1"]
	h.tmux.history["s1"] = unnumberedTrustScreen(h.cfg.Sessions["s1"].Cwd)
	err := h.sup.typeInto("s1", ActNudge, &ActPayload{Message: "go on"}, tr, StillHash(h.tmux.history["s1"]+"\n"+h.tmux.panes["s1"]), func() error {
		return h.tmux.Type("s1", "go on")
	})
	if err != nil || typed(h, "s1") != 1 {
		t.Fatalf("typeInto = %v, typed %d", err, typed(h, "s1"))
	}
}
