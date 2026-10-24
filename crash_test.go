package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// restarts counts the times a session's command was started again.
func restarts(h *harness, name string) int {
	return h.tmux.count("new-session", name) + h.tmux.count("respawn", name)
}

// A crash that repeats with the same cause, the same exit status and last
// screen, parks the session for the owner after the second crash: one
// escalation, no more restarts. Being seen alive again does not clear the
// park; only agentd clear does.
func TestRepeatedCrashCauseParks(t *testing.T) {
	h := newHarness(t, false, "mon")
	h.status("mon", working)
	h.alive("mon", "working")
	h.tmux.crash["mon"], h.tmux.remain["mon"], h.tmux.exit["mon"] = true, true, 1
	h.ticks(1, 5*time.Minute)
	h.tmux.alive["mon"] = false // it dies; its restarts crash with the same screen
	h.ticks(12*12, 5*time.Minute)

	if got := restarts(h, "mon"); got != 2 {
		t.Fatalf("restarts = %d, want 2: the first crash restarts, the repeat parks", got)
	}
	if h.state("mon") != CrashLooping || len(h.runner.calls) != 1 || !strings.Contains(h.runner.calls[0][4], "agentd clear mon") {
		t.Fatalf("state = %s, escalations = %v", h.state("mon"), h.runner.calls)
	}
	exits := 0
	for _, e := range h.events() {
		if e.Kind == KindCrash && e.Session == "mon" {
			if c := e.Crash; c.Exit != nil && *c.Exit == 1 && c.ScreenHash != "" {
				exits++
			}
		}
	}
	if exits != 2 {
		t.Fatalf("crash events with exit status and screen = %d, want 2", exits)
	}

	// The owner starts it by hand: still parked, never typed into or restarted.
	h.tmux.dead["mon"] = false
	h.alive("mon", "static")
	h.tmux.crash["mon"] = false
	h.ticks(24, 5*time.Minute)
	h.restart(false)
	h.ticks(3, 5*time.Minute)
	if h.state("mon") != CrashLooping || typed(h, "mon") != 0 || len(h.runner.calls) != 1 {
		t.Fatalf("seen alive: state = %s, typed %d, escalations %d", h.state("mon"), typed(h, "mon"), len(h.runner.calls))
	}

	// agentd clear lifts the park, while agentd runs.
	if err := ClearSession(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "mon"); err != nil {
		t.Fatal(err)
	}
	h.tmux.alive["mon"] = false
	h.ticks(2, 5*time.Minute)
	if got := restarts(h, "mon"); got != 3 || h.state("mon") == CrashLooping {
		t.Fatalf("after clear: restarts = %d, state = %s", got, h.state("mon"))
	}
	if err := ClearSession(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "nope"); err == nil {
		t.Fatal("clear accepted an unconfigured session")
	}
}

// Crashes with different causes park after max_crashes, however far apart.
func TestCrashesParkAfterNRegardlessOfWindow(t *testing.T) {
	h := newHarness(t, false, "mon")
	h.status("mon", working)
	n := 0
	h.tmux.deadScreen["mon"] = func() string { n++; return fmt.Sprintf("error number %d", n) }
	h.tmux.crash["mon"], h.tmux.remain["mon"], h.tmux.exit["mon"] = true, true, 2
	for i := 0; i < 6; i++ {
		h.ticks(1, 3*24*time.Hour) // days apart, far outside any window
	}
	if got := restarts(h, "mon"); got != 3 {
		t.Fatalf("restarts = %d, want 3", got)
	}
	if h.state("mon") != CrashLooping || len(h.runner.calls) != 1 {
		t.Fatalf("state = %s, escalations = %v", h.state("mon"), h.runner.calls)
	}
	h.restart(false)
	h.ticks(3, 3*24*time.Hour)
	if got := restarts(h, "mon"); got != 3 || len(h.runner.calls) != 1 {
		t.Fatalf("after replay: restarts = %d, escalations = %d", got, len(h.runner.calls))
	}
}

// A session gone with open Questions is waiting on the owner: it is parked
// and its Questions escalated, never restarted.
func TestDeadSessionWithQuestionsIsParked(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", blocked)
	h.ticks(12, 5*time.Minute)
	if got := restarts(h, "s1"); got != 0 {
		t.Fatalf("restarts = %d of a session waiting on the owner", got)
	}
	if h.state("s1") != BlockedOnOwner || len(h.runner.calls) != 1 || !strings.Contains(h.runner.calls[0][4], "Which version gates the fields?") {
		t.Fatalf("state = %s, escalations = %v", h.state("s1"), h.runner.calls)
	}
}

// The default restart starts a fresh context told to read the brief and
// the status file, never --continue.
func TestDefaultRestartIsFresh(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.ticks(1, 5*time.Minute)
	if restarts(h, "s1") != 1 {
		t.Fatal("not restarted")
	}
	for _, c := range h.tmux.calls {
		if c.op != "new-session" {
			continue
		}
		cmd := c.args[1]
		if strings.Contains(cmd, "--continue") || !strings.Contains(cmd, h.dir+"/s1/BRIEF.md") || !strings.Contains(cmd, h.dir+"/s1/STATUS.md") {
			t.Fatalf("restart command = %q", cmd)
		}
	}
}

// The default restart command carries the brief and status paths as data:
// a workdir whose name holds quotes or $( ) reaches Claude's prompt intact
// and runs nothing.
func TestRestartPromptPathsAreQuoted(t *testing.T) {
	h := newHarness(t, false, "s1")
	weird := filepath.Join(h.dir, `we"ird $(touch pwned) 'dir`)
	if err := os.MkdirAll(weird, 0o755); err != nil {
		t.Fatal(err)
	}
	s := h.cfg.Sessions["s1"]
	s.Workdir = weird
	h.cfg.Sessions["s1"] = s
	h.status("s1", working)
	writeFile(t, filepath.Join(weird, "STATUS.md"), working)
	h.ticks(1, 5*time.Minute)
	var line string
	for _, c := range h.tmux.calls {
		if c.op == "new-session" {
			line = c.args[1]
		}
	}
	bin, home := t.TempDir(), t.TempDir()
	got := filepath.Join(h.dir, "prompt")
	writeFile(t, filepath.Join(bin, "claude"), "#!/bin/sh\nprintf '%s' \"$5\" > '"+got+"'\n")
	if err := os.Chmod(filepath.Join(bin, "claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", line)
	cmd.Dir = h.dir
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restart command failed: %v: %s", err, out)
	}
	b, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if f := promptFacts(t, string(b)); f.Brief != filepath.Join(weird, "BRIEF.md") || f.Progress != filepath.Join(weird, "STATUS.md") || f.Why != "restart" {
		t.Fatalf("prompt = %q", b)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "pwned")); err == nil {
		t.Fatal("the workdir's name ran as a command")
	}
}
