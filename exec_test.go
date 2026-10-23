package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shim puts an executable script named name first on PATH for the test.
func shim(t *testing.T, name, script string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// quick fails the test when f takes longer than a hung command would be
// allowed to, and returns f's error.
func quick(t *testing.T, what string, f func() error) error {
	t.Helper()
	start := time.Now()
	err := f()
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("%s took %s; the timeout did not kill it", what, took)
	}
	return err
}

// A hung tmux is killed at its timeout and reported as an error, never as
// a gone session: a gone session would be restarted.
func TestHungTmuxTimesOut(t *testing.T) {
	shim(t, "tmux", "exec sleep 30")
	tm := ExecTmux{Timeout: 100 * time.Millisecond}
	var alive bool
	err := quick(t, "has-session", func() (err error) { alive, err = tm.HasSession("s1"); return })
	if err == nil || alive || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("has-session = %v, %v; want a timeout error", alive, err)
	}
	err = quick(t, "capture-pane", func() error { _, err := tm.CapturePane("s1", 40); return err })
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("capture-pane error = %v", err)
	}
}

// git, glab, gh and the escalation command are killed at their timeout too,
// even when the hung process has a child holding its output open.
func TestHungForgeCommandsTimeOut(t *testing.T) {
	for _, name := range []string{"git", "glab", "gh", "post"} {
		shim(t, name, "sleep 30; echo late")
	}
	g := ExecGit{Timeout: 100 * time.Millisecond}
	f := ExecForge{Timeout: 100 * time.Millisecond}
	r := ExecRunner{Timeout: 100 * time.Millisecond}
	checks := map[string]func() error{
		"fetch": func() error { return g.Fetch(t.TempDir()) },
		"push":  func() error { return f.Push(t.TempDir(), "feature") },
		"glab find": func() error {
			_, _, err := f.Find(Repository{Forge: "gitlab", Project: "a/b"}, "feature", "main")
			return err
		},
		"gh find": func() error {
			_, _, err := f.Find(Repository{Forge: "github", Project: "a/b"}, "feature", "main")
			return err
		},
		"glab open": func() error {
			_, err := f.Open(Repository{Forge: "gitlab", Project: "a/b"}, MergeRequest{}, t.TempDir())
			return err
		},
		"gh open": func() error {
			_, err := f.Open(Repository{Forge: "github", Project: "a/b"}, MergeRequest{}, t.TempDir())
			return err
		},
		"escalation": func() error { return r.Run([]string{"post", "infra", "hello"}) },
	}
	for what, check := range checks {
		if err := quick(t, what, check); err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Errorf("%s error = %v, want a timeout", what, err)
		}
	}
}

// git never stops to ask for credentials: prompts are off in its environment.
func TestGitNeverPrompts(t *testing.T) {
	shim(t, "git", `echo "$GIT_TERMINAL_PROMPT $GCM_INTERACTIVE [$GIT_ASKPASS] [$SSH_ASKPASS]"`)
	t.Setenv("GIT_ASKPASS", "/usr/bin/ksshaskpass")
	got, err := ExecGit{Timeout: 10 * time.Second}.Branch(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got != "0 never [] []" {
		t.Fatalf("git environment = %q", got)
	}
}

// The timeouts default and are validated.
func TestTimeoutsConfig(t *testing.T) {
	d := DefaultConfig().Timeouts
	if d.Tmux.Duration != 10*time.Second || d.Forge.Duration != 2*time.Minute || d.Escalate.Duration != 2*time.Minute {
		t.Fatalf("default timeouts = %+v", d)
	}
	path := filepath.Join(t.TempDir(), "c.json")
	writeFile(t, path, `{"timeouts": {"tmux": "0s"}}`)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("a zero timeout was accepted")
	}
}
