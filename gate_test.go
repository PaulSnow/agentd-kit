package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func doneWithDelivery(t *testing.T, h *harness, name string) {
	t.Helper()
	mr := h.dir + "/mr.md"
	writeFile(t, mr, "The MR description.\n")
	h.status(name, "# Finished\n\nDONE\n")
	h.deliver(name, Deliver{Repository: "widgets", Worktree: h.dir + "/" + name + "/wt", Target: "main", MRFile: mr})
}

// Delivery runs the repository's gate in the worktree first and pushes only
// when it passes; the gate's command, commit, exit status and duration are
// recorded.
func TestDeliveryRunsTheGateFirst(t *testing.T) {
	h := newHarness(t, false, "d")
	doneWithDelivery(t, h, "d")
	h.ticks(2, 5*time.Minute)
	want := []gateRun{{h.dir + "/d/wt", []string{"./bin/check"}, 10 * time.Minute}}
	if !reflect.DeepEqual(h.runner.gates, want) {
		t.Fatalf("gates = %+v, want %+v", h.runner.gates, want)
	}
	if len(h.forge.pushes) != 1 {
		t.Fatalf("pushes = %v", h.forge.pushes)
	}
	var gate *Event
	for _, e := range h.events() {
		if e.Kind == KindAction && actionOf(e) == ActGate {
			e := e
			gate = &e
		}
	}
	if gate == nil || gate.Act.SHA != "c0ffee" || len(gate.Act.Gate) == 0 {
		t.Fatalf("gate event = %+v", gate)
	}
	if gate.Act.Exit == nil || *gate.Act.Exit != 0 {
		t.Fatalf("gate event exit = %v", gate.Act.Exit)
	}
	if gate.Act.DurationS == nil {
		t.Fatalf("gate event lacks its duration: %+v", gate.Act)
	}
}

// A failing gate stops the push; it is retried on the backoff schedule and
// given up, with one escalation, after MaxRestarts failures on that commit.
// A new commit gets fresh tries.
func TestFailingGateIsNotPushed(t *testing.T) {
	h := newHarness(t, false, "d")
	doneWithDelivery(t, h, "d")
	h.runner.gateExit = 1
	h.ticks(24, 5*time.Minute)
	if len(h.forge.pushes) != 0 || len(h.forge.opens) != 0 {
		t.Fatalf("pushed %v after a failing gate", h.forge.pushes)
	}
	if got := h.count(KindError, ActGate, "d"); got != 3 {
		t.Fatalf("failed gates = %d, want 3", got)
	}
	if len(h.runner.calls) != 1 || !strings.Contains(h.runner.calls[0][4], "gate") {
		t.Fatalf("escalations = %v", h.runner.calls)
	}
	h.git.head, h.runner.gateExit = "beef", 0
	h.ticks(1, 5*time.Minute)
	if len(h.forge.pushedSHAs) != 1 || h.forge.pushedSHAs[0] != "beef" {
		t.Fatalf("pushes = %v after a new commit passed the gate", h.forge.pushedSHAs)
	}
}

// The gate runs with a scrubbed environment: PATH, HOME, LANG, TMPDIR and
// the repository's allow-list, never an agent socket, a token or Claude's
// settings; an allow-list naming one of those is refused at load.
func TestGateEnvironmentIsScrubbed(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("GITLAB_TOKEN", "glpat-secret")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-secret")
	t.Setenv("GOFLAGS", "-mod=readonly")
	t.Setenv("EDITOR", "vi")
	h := newHarness(t, false, "d")
	r := h.cfg.Repositories["widgets"]
	r.GateEnv = []string{"GOFLAGS"}
	h.cfg.Repositories["widgets"] = r
	doneWithDelivery(t, h, "d")
	h.ticks(1, 5*time.Minute)
	env := strings.Join(h.runner.gateEnv, "\n")
	for _, want := range []string{"PATH=", "HOME=", "GOFLAGS=-mod=readonly"} {
		if !strings.Contains(env, want) {
			t.Fatalf("gate environment lacks %s:\n%s", want, env)
		}
	}
	for _, bad := range []string{"SSH_AUTH_SOCK", "GITLAB_TOKEN", "CLAUDE_", "EDITOR"} {
		if strings.Contains(env, bad) {
			t.Fatalf("gate environment carries %s:\n%s", bad, env)
		}
	}
	for _, name := range []string{"SSH_AUTH_SOCK", "GITLAB_TOKEN", "CLAUDE_CONFIG_DIR"} {
		path := filepath.Join(t.TempDir(), "c.json")
		writeFile(t, path, `{"repositories": {"r": {"forge": "gitlab", "project": "a/b", "gate": ["make"], "gate_env": ["`+name+`"]}}}`)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("gate_env %s was accepted", name)
		}
	}
}

// The tree must be the one the gate passed: a HEAD that moved, or a tree
// that became dirty, during the gate is not pushed; the next pass gates
// the new commit and pushes that.
func TestTreeChangedDuringGateIsNotPushed(t *testing.T) {
	h := newHarness(t, false, "moved")
	doneWithDelivery(t, h, "moved")
	h.runner.onGate = func() { h.git.head, h.runner.onGate = "beef", nil }
	h.ticks(1, 5*time.Minute)
	if len(h.forge.pushes) != 0 {
		t.Fatalf("pushed %v after HEAD moved under the gate", h.forge.pushedSHAs)
	}
	h.ticks(1, 5*time.Minute)
	if !reflect.DeepEqual(h.forge.pushedSHAs, []string{"beef"}) || len(h.runner.gates) != 2 {
		t.Fatalf("pushed %v after %d gates, want the new commit once gated", h.forge.pushedSHAs, len(h.runner.gates))
	}

	h = newHarness(t, false, "dirty")
	doneWithDelivery(t, h, "dirty")
	h.runner.onGate = func() { h.git.clean = false }
	h.ticks(3, 5*time.Minute)
	if len(h.forge.pushes) != 0 {
		t.Fatalf("pushed %v after the tree became dirty under the gate", h.forge.pushes)
	}
}

// Shadow mode logs the gate it would run and runs none.
func TestShadowDoesNotRunGates(t *testing.T) {
	h := newHarness(t, true, "d")
	doneWithDelivery(t, h, "d")
	h.ticks(3, 5*time.Minute)
	if len(h.runner.gates) != 0 || h.count(KindWould, ActGate, "d") != 1 {
		t.Fatalf("gates run = %d, would gate = %d", len(h.runner.gates), h.count(KindWould, ActGate, "d"))
	}
}

// A repository that allows push without a gate is refused at load.
func TestPushWithoutGateIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	writeFile(t, path, `{"repositories": {"r": {"forge": "gitlab", "project": "a/b", "allow": ["push", "open"]}}}`)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "gate") {
		t.Fatalf("push without a gate: %v", err)
	}
	writeFile(t, path, `{"repositories": {"r": {"forge": "gitlab", "project": "a/b", "allow": ["push", "open"], "gate": ["make", "check"], "gate_timeout": "20m"}}}`)
	if _, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	}
}

// The real gate runs in the worktree and reports the exit status.
func TestExecGate(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bin", "check"), "#!/bin/sh\npwd > ran\nexit 3\n")
	if err := os.Chmod(filepath.Join(dir, "bin", "check"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "bin", "loud"), "#!/bin/sh\nhead -c 300000 /dev/zero | tr '\\0' x\necho END\nenv\n")
	if err := os.Chmod(filepath.Join(dir, "bin", "loud"), 0o755); err != nil {
		t.Fatal(err)
	}
	exit, _, err := ExecRunner{}.Gate(dir, []string{"./bin/check"}, []string{"PATH=" + os.Getenv("PATH")}, 10*time.Second)
	if err != nil || exit != 3 {
		t.Fatalf("gate = %d, %v", exit, err)
	}
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	_, out, err := ExecRunner{}.Gate(dir, []string{"./bin/loud"}, []string{"PATH=" + os.Getenv("PATH"), "ONLY=this"}, 10*time.Second)
	if err != nil || len(out) > 64*1024 || !strings.Contains(out, "END") || !strings.Contains(out, "ONLY=this") || strings.Contains(out, "SSH_AUTH_SOCK") {
		t.Fatalf("output: %d bytes, %v; env shown: %v", len(out), err, strings.Contains(out, "SSH_AUTH_SOCK"))
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ran")); strings.TrimSpace(string(b)) != dir {
		t.Fatalf("gate ran in %q", b)
	}
	if _, _, err := (ExecRunner{}).Gate(dir, []string{"sleep", "30"}, []string{"PATH=" + os.Getenv("PATH")}, 100*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("hung gate: %v", err)
	}
}

// The gate that runs is the target branch's: a session that changed the
// gate's files, committed or not, is refused delivery with a clear reason
// and one escalation, and its gate is not run. A later commit with the
// gate as the target has it is delivered.
func TestChangedGateIsRefused(t *testing.T) {
	h := newHarness(t, false, "d")
	r := h.cfg.Repositories["widgets"]
	r.GatePaths = []string{"bin/lib"}
	h.cfg.Repositories["widgets"] = r
	doneWithDelivery(t, h, "d")
	h.git.gateChanged = []string{"bin/check"}
	h.ticks(4, 5*time.Minute)
	if len(h.runner.gates) != 0 || len(h.forge.pushes) != 0 {
		t.Fatalf("ran %d gates, pushed %v with a changed gate", len(h.runner.gates), h.forge.pushes)
	}
	if !reflect.DeepEqual(h.git.gatePaths, []string{"bin/check", "bin/lib"}) {
		t.Fatalf("asked about %v", h.git.gatePaths)
	}
	var reason string
	for _, e := range h.events() {
		if e.Kind == KindSkip && strings.Contains(e.Skip.Key, ":gate-changed:") {
			reason = e.Skip.Reason
		}
	}
	if !strings.Contains(reason, "bin/check") || !strings.Contains(reason, "target branch") {
		t.Fatalf("skip reason %q", reason)
	}
	if len(h.runner.calls) != 1 || !strings.Contains(h.runner.calls[0][4], "bin/check") {
		t.Fatalf("escalations = %v", h.runner.calls)
	}
	h.git.gateChanged, h.git.head = nil, "beef"
	h.ticks(1, 5*time.Minute)
	if len(h.forge.pushedSHAs) != 1 || h.forge.pushedSHAs[0] != "beef" {
		t.Fatalf("pushes = %v once the gate is the target's", h.forge.pushedSHAs)
	}
}

// The gate runs with HOME a fresh empty directory, removed after it, and
// the gate event records which credential paths stay reachable.
func TestGateRunsWithScratchHome(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".netrc"), "machine example.org\n")
	t.Setenv("HOME", home)
	h := newHarness(t, false, "d")
	doneWithDelivery(t, h, "d")
	var scratch string
	var entries int
	h.runner.onGate = func() {
		for _, kv := range h.runner.gateEnv {
			if strings.HasPrefix(kv, "HOME=") {
				scratch = strings.TrimPrefix(kv, "HOME=")
			}
		}
		es, err := os.ReadDir(scratch)
		if err != nil {
			t.Errorf("the gate's HOME: %v", err)
		}
		entries = len(es)
	}
	h.ticks(1, 5*time.Minute)
	if scratch == "" || scratch == home || entries != 0 {
		t.Fatalf("the gate's HOME is %q with %d entries", scratch, entries)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("the gate's HOME was left behind: %v", err)
	}
	var gate *ActPayload
	for _, e := range h.events() {
		if e.Kind == KindAction && actionOf(e) == ActGate {
			gate = e.Act
		}
	}
	if gate == nil || !reflect.DeepEqual(gate.Reachable, []string{"~/.netrc", "~/.ssh"}) {
		t.Fatalf("gate event %+v", gate)
	}
	path := filepath.Join(t.TempDir(), "c.json")
	writeFile(t, path, `{"repositories": {"r": {"forge": "gitlab", "project": "a/b", "gate": ["make"], "gate_env": ["HOME"]}}}`)
	if _, err := LoadConfig(path); err == nil {
		t.Error("gate_env HOME was accepted")
	}
}

// An error from git, the forge CLIs or the escalation command that matches
// a confidential pattern never reaches the log.
func TestConfidentialErrorIsWithheld(t *testing.T) {
	h := newHarness(t, false, "d")
	h.cfg.ConfidentialPatterns = []string{`(?i)project\s+nightingale`}
	h.restart(false)
	doneWithDelivery(t, h, "d")
	h.forge.pushErr = errors.New("remote: Project Nightingale is archived")
	h.ticks(2, 5*time.Minute)
	if strings.Contains(h.buf.String(), "Nightingale") {
		t.Fatalf("the log carries the confidential error:\n%s", h.buf.String())
	}
	if h.count(KindError, ActPush, "d") == 0 || !strings.Contains(h.buf.String(), "withheld") {
		t.Fatalf("the error was not logged as withheld:\n%s", h.buf.String())
	}
}

// A gate none of whose paths the target branch has is pinned by nothing,
// and is refused until gate_paths names its files.
func TestUnpinnedGateIsRefused(t *testing.T) {
	h := newHarness(t, false, "d")
	doneWithDelivery(t, h, "d")
	h.git.gateUnpinned = true
	h.ticks(3, 5*time.Minute)
	if len(h.runner.gates) != 0 || len(h.forge.pushes) != 0 {
		t.Fatalf("ran %d gates, pushed %v with an unpinned gate", len(h.runner.gates), h.forge.pushes)
	}
	var reason string
	for _, e := range h.events() {
		if e.Kind == KindSkip && strings.Contains(e.Skip.Key, ":gate-unpinned") {
			reason = e.Skip.Reason
		}
	}
	if !strings.Contains(reason, "gate_paths") {
		t.Fatalf("skip reason %q", reason)
	}
}

// A gate file changed while the gate ran, and still changed when it ends,
// is refused: the gate files are checked again after it.
func TestGateChangedDuringItsRunIsNotPushed(t *testing.T) {
	h := newHarness(t, false, "d")
	doneWithDelivery(t, h, "d")
	h.git.onGateFiles = func(n int) {
		if n == 2 {
			h.git.gateChanged = []string{"bin/check"}
		}
	}
	h.ticks(1, 5*time.Minute)
	if len(h.runner.gates) != 1 || len(h.forge.pushes) != 0 || h.git.gateChecks != 2 {
		t.Fatalf("ran %d gates, checked %d times, pushed %v", len(h.runner.gates), h.git.gateChecks, h.forge.pushes)
	}
}

// What a gate leaves read-only in its HOME, as Go's module cache is, is
// removed with it; the accounts' files and the agent socket are recorded
// as reachable.
func TestGateHomeIsRemovedAndAccountsAreReachable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sock := filepath.Join(t.TempDir(), "agent.sock")
	writeFile(t, sock, "")
	t.Setenv("SSH_AUTH_SOCK", sock)
	token := filepath.Join(home, "tokens", "a")
	writeFile(t, token, "x")
	h := newHarness(t, false, "d")
	h.cfg.Accounts = map[string]Account{"a": {TokenFile: token}}
	doneWithDelivery(t, h, "d")
	var scratch string
	h.runner.onGate = func() {
		for _, kv := range h.runner.gateEnv {
			if strings.HasPrefix(kv, "HOME=") {
				scratch = strings.TrimPrefix(kv, "HOME=")
			}
		}
		mod := filepath.Join(scratch, "go", "pkg", "mod")
		writeFile(t, filepath.Join(mod, "m", "f.go"), "package m\n")
		for _, d := range []string{filepath.Join(mod, "m"), mod} {
			if err := os.Chmod(d, 0o555); err != nil {
				t.Error(err)
			}
		}
	}
	h.ticks(1, 5*time.Minute)
	if _, err := os.Stat(scratch); scratch == "" || !os.IsNotExist(err) {
		t.Fatalf("the gate's HOME %q was left behind: %v", scratch, err)
	}
	for _, e := range h.events() {
		if e.Kind == KindAction && actionOf(e) == ActGate {
			if !reflect.DeepEqual(e.Act.Reachable, []string{"account a token_file", "$SSH_AUTH_SOCK"}) {
				t.Fatalf("reachable %v", e.Act.Reachable)
			}
			return
		}
	}
	t.Fatal("no gate event")
}

// Gate paths are literal: wildcards and pathspec magic are refused.
func TestGatePathsAreLiteral(t *testing.T) {
	for _, p := range []string{"bin/*", ":(glob)bin", "bin/ch?ck", "[b]in"} {
		path := filepath.Join(t.TempDir(), "c.json")
		writeFile(t, path, `{"repositories": {"r": {"forge": "gitlab", "project": "a/b", "gate": ["make"], "gate_paths": [`+strconv.Quote(p)+`]}}}`)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("gate_paths %q was accepted", p)
		}
	}
}

// What the gate-file check, a gate or the escalation command reports is
// screened like any other error.
func TestConfidentialGateErrorIsWithheld(t *testing.T) {
	h := newHarness(t, false, "d")
	h.cfg.ConfidentialPatterns = []string{`(?i)project\s+nightingale`}
	h.restart(false)
	doneWithDelivery(t, h, "d")
	h.git.gateErr = errors.New("fatal: could not read from Project Nightingale")
	h.ticks(2, 5*time.Minute)
	if strings.Contains(h.buf.String(), "Nightingale") || !strings.Contains(h.buf.String(), "withheld") {
		t.Fatalf("the gate-file error was not withheld:\n%s", h.buf.String())
	}
}

// GateFiles names the gate paths whose content at the head, or on disk,
// is not the target branch's as the configured URL has it, whatever the
// session did to its index, its refs or its git config.
func TestExecGateFiles(t *testing.T) {
	root := t.TempDir()
	remote, dir := filepath.Join(root, "remote.git"), filepath.Join(root, "wt")
	run := func(args ...string) string {
		t.Helper()
		out, err := gitOut(10*time.Second, dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOut(10*time.Second, root, "init", "-q", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	run("config", "user.email", "agent@example.org")
	run("config", "user.name", "agent")
	good := "#!/bin/sh\ngo test ./...\n"
	writeFile(t, filepath.Join(dir, "bin", "check"), good)
	writeFile(t, filepath.Join(dir, "lib", "a.sh"), "true\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n")
	writeFile(t, filepath.Join(dir, ".gitignore"), "*.tmp\n")
	run("add", "bin/check", "lib/a.sh", "main.go", ".gitignore")
	run("commit", "-q", "-m", "base")
	run("push", "-q", remote, "HEAD:refs/heads/main")
	base := run("rev-parse", "HEAD")
	g := ExecGit{Timeout: 10 * time.Second}
	paths := []string{"bin/check", "lib", "bin/absent"}
	check := func(want ...string) {
		t.Helper()
		got, pinned, err := g.GateFiles(dir, remote, "main", run("rev-parse", "HEAD"), paths)
		if err != nil || pinned != 2 || strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("changed %v, pinned %d, %v; want %v", got, pinned, err, want)
		}
	}
	check()
	writeFile(t, filepath.Join(dir, "main.go"), "package main // changed\n")
	run("commit", "-q", "-am", "work")
	check()
	evil := "#!/bin/sh\nexit 0\n"
	writeFile(t, filepath.Join(dir, "bin", "check"), evil)
	check("bin/check")
	run("commit", "-q", "-am", "pass always")
	check("bin/check")
	// A replace ref that makes the head's tree look like the target's.
	writeFile(t, filepath.Join(dir, "bin", "check"), good)
	run("replace", run("rev-parse", "HEAD^{tree}"), run("rev-parse", base+"^{tree}"))
	check("bin/check")
	run("replace", "-d", run("rev-parse", "HEAD^{tree}"))
	run("checkout", "-q", base, "--", "bin/check")
	run("commit", "-q", "-m", "restore the gate")
	check()
	// Index flags that hide an edit from git status.
	for _, flag := range []string{"--skip-worktree", "--assume-unchanged"} {
		run("update-index", flag, "bin/check")
		writeFile(t, filepath.Join(dir, "bin", "check"), evil)
		check("bin/check")
		writeFile(t, filepath.Join(dir, "bin", "check"), good)
		run("update-index", "--no"+strings.TrimPrefix(flag, "-"), "bin/check")
	}
	// A file mode change, an ignored file and a file the target lacks.
	if err := os.Chmod(filepath.Join(dir, "lib", "a.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	check("lib")
	if err := os.Chmod(filepath.Join(dir, "lib", "a.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "lib", "b.tmp"), "exit 0\n")
	writeFile(t, filepath.Join(dir, "bin", "absent"), "exit 0\n")
	check("lib", "bin/absent")
	os.Remove(filepath.Join(dir, "lib", "b.tmp"))
	os.Remove(filepath.Join(dir, "bin", "absent"))
	// A directory swapped for a symbolic link to the same files.
	copyDir := filepath.Join(root, "lib-copy")
	writeFile(t, filepath.Join(copyDir, "a.sh"), "true\n")
	if err := os.Rename(filepath.Join(dir, "lib"), filepath.Join(root, "lib-orig")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(copyDir, filepath.Join(dir, "lib")); err != nil {
		t.Fatal(err)
	}
	check("lib")
	os.Remove(filepath.Join(dir, "lib"))
	if err := os.Rename(filepath.Join(root, "lib-orig"), filepath.Join(dir, "lib")); err != nil {
		t.Fatal(err)
	}
	check()
	// origin/main pointed at the session's own gate, and a fetch refspec
	// that never moves it, change nothing: the URL answers for the target.
	run("config", "remote.origin.url", remote)
	run("config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/x/*")
	writeFile(t, filepath.Join(dir, "bin", "check"), evil)
	run("commit", "-q", "-am", "evil base")
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	check("bin/check")
	// A remote named after the configured URL would answer for it.
	run("config", "remote."+remote+".url", dir)
	if _, _, err := g.GateFiles(dir, remote, "main", run("rev-parse", "HEAD"), paths); err == nil || !strings.Contains(err.Error(), "names a remote") {
		t.Fatalf("a remote named after the URL gave %v", err)
	}
	if err := (ExecForge{Timeout: 10 * time.Second}).Push(dir, remote, run("rev-parse", "HEAD"), "x"); err == nil || !strings.Contains(err.Error(), "names a remote") {
		t.Fatalf("push through a remote named after the URL gave %v", err)
	}
	run("config", "--remove-section", "remote."+remote)
	// A file-system monitor, or a hook, named in the worktree's config
	// never runs.
	marker := filepath.Join(root, "monitor-ran")
	writeFile(t, filepath.Join(root, "monitor.sh"), "#!/bin/sh\ntouch "+marker+"\n")
	if err := os.Chmod(filepath.Join(root, "monitor.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	run("config", "core.fsmonitor", filepath.Join(root, "monitor.sh"))
	run("config", "core.hooksPath", root)
	if err := os.Symlink(filepath.Join(root, "monitor.sh"), filepath.Join(root, "post-index-change")); err != nil {
		t.Fatal(err)
	}
	check("bin/check")
	if _, err := g.Clean(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the worktree's fsmonitor ran")
	}
	// A target commit this clone has not fetched is an error, not a pass.
	other := filepath.Join(root, "other")
	if _, err := gitOut(10*time.Second, root, "clone", "-q", remote, other); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(other, "c"), "x\n")
	for _, args := range [][]string{{"-c", "user.email=a@example.org", "-c", "user.name=a", "add", "c"},
		{"-c", "user.email=a@example.org", "-c", "user.name=a", "commit", "-q", "-m", "new"}, {"push", "-q", "origin", "HEAD:main"}} {
		if _, err := gitOut(10*time.Second, other, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := g.GateFiles(dir, remote, "main", run("rev-parse", "HEAD"), paths); err == nil || !strings.Contains(err.Error(), "not been fetched") {
		t.Fatalf("an unfetched target gave %v", err)
	}
}
