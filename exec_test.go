package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
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
			_, err := f.Find(Repository{Forge: "gitlab", Project: "a/b"}, "feature", "main")
			return err
		},
		"gh find": func() error {
			_, err := f.Find(Repository{Forge: "github", Project: "a/b"}, "feature", "main")
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

// The real clients list the open and merged requests on the branch, each
// with its head commit, and leave out closed ones.
func TestFindListsOpenAndMergedRequests(t *testing.T) {
	f := ExecForge{Timeout: 10 * time.Second}
	shim(t, "glab", `echo '[{"iid":3,"state":"closed","sha":"c","web_url":"u3"},{"iid":2,"state":"merged","sha":"m","web_url":"u2"},{"iid":1,"state":"opened","sha":"o","web_url":"u1"}]'`)
	got, err := f.Find(Repository{Forge: "gitlab", Project: "a/b"}, "feature", "main")
	want := []MergeResult{{Ref: "!2", URL: "u2", Head: "m", Merged: true}, {Ref: "!1", URL: "u1", Head: "o"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("gitlab: %+v %v", got, err)
	}
	shim(t, "gh", `echo '[{"number":3,"state":"CLOSED","headRefOid":"c","url":"u3"},{"number":2,"state":"MERGED","headRefOid":"m","url":"u2"},{"number":1,"state":"OPEN","headRefOid":"o","url":"u1"}]'`)
	got, err = f.Find(Repository{Forge: "github", Project: "a/b"}, "feature", "main")
	want = []MergeResult{{Ref: "#2", URL: "u2", Head: "m", Merged: true}, {Ref: "#1", URL: "u1", Head: "o"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("github: %+v %v", got, err)
	}
}

// gitRun runs real git for a test, with an identity.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// After the forge rebases a request before merging, or after the session
// rebases its branch onto a squash merge, the worktree's commits are in the
// target as patches with other hashes. They are not ahead, and the target
// contains the rebased request's head but not the local one.
func TestAheadDiscountsPatchesAlreadyInTarget(t *testing.T) {
	root := t.TempDir()
	origin, wt, other := filepath.Join(root, "origin.git"), filepath.Join(root, "wt"), filepath.Join(root, "other")
	gitRun(t, root, "init", "-q", "--bare", origin)
	gitRun(t, root, "clone", "-q", origin, wt)
	writeFile(t, filepath.Join(wt, "a"), "a\n")
	gitRun(t, wt, "add", "a")
	gitRun(t, wt, "commit", "-q", "-m", "base")
	gitRun(t, wt, "push", "-q", "origin", "main")
	gitRun(t, wt, "checkout", "-q", "-b", "feature")
	writeFile(t, filepath.Join(wt, "b"), "b\n")
	gitRun(t, wt, "add", "b")
	gitRun(t, wt, "commit", "-q", "-m", "feature one")
	writeFile(t, filepath.Join(wt, "c"), "c\n")
	gitRun(t, wt, "add", "c")
	gitRun(t, wt, "commit", "-q", "-m", "feature two")
	gitRun(t, wt, "push", "-q", "origin", "feature")
	local := gitRun(t, wt, "rev-parse", "HEAD")

	// The forge rebases feature onto a moved main and fast-forwards main to it.
	gitRun(t, root, "clone", "-q", origin, other)
	writeFile(t, filepath.Join(other, "d"), "d\n")
	gitRun(t, other, "add", "d")
	gitRun(t, other, "commit", "-q", "-m", "someone else")
	gitRun(t, other, "fetch", "-q", "origin", "feature")
	gitRun(t, other, "cherry-pick", "origin/main..origin/feature")
	rebased := gitRun(t, other, "rev-parse", "HEAD")
	gitRun(t, other, "push", "-q", "origin", "main")

	g := ExecGit{Timeout: 30 * time.Second}
	if err := g.Fetch(wt); err != nil {
		t.Fatal(err)
	}
	if n, err := g.Ahead(wt, "main"); err != nil || n != 0 {
		t.Fatalf("ahead after a rebase merge = %d, %v; want 0", n, err)
	}
	if in, err := g.Contains(wt, rebased, "main"); err != nil || !in {
		t.Fatalf("contains the rebased head = %v, %v", in, err)
	}
	if in, err := g.Contains(wt, local, "main"); err != nil || in {
		t.Fatalf("contains the local head = %v, %v", in, err)
	}
	if in, err := g.Contains(wt, "0123456789012345678901234567890123456789", "main"); err != nil || in {
		t.Fatalf("contains an unknown commit = %v, %v", in, err)
	}
	writeFile(t, filepath.Join(wt, "e"), "e\n")
	gitRun(t, wt, "add", "e")
	gitRun(t, wt, "commit", "-q", "-m", "new work")
	if n, err := g.Ahead(wt, "main"); err != nil || n != 1 {
		t.Fatalf("ahead with new work = %d, %v; want 1", n, err)
	}
}

// gone says whether pid has exited: no such process, or a zombie waiting
// for whoever adopted it to reap it.
func gone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

// A timeout kills the command's whole process group, not just the command:
// a child it started, here a sleep in the background, does not outlive it.
func TestTimeoutKillsTheProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	shim(t, "git", "sleep 4242 &\necho $! > "+pidFile+"\nwait")
	err := quick(t, "fetch", func() error { return ExecGit{Timeout: 300 * time.Millisecond}.Fetch(t.TempDir()) })
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("fetch error = %v, want a timeout", err)
	}
	b, rerr := os.ReadFile(pidFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(3 * time.Second)
	for !gone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("the background sleep %d outlived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
