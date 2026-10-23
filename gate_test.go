package main

import (
	"os"
	"path/filepath"
	"reflect"
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
	want := []gateRun{{h.dir + "/d/wt", []string{"./bin/check"}, 2 * time.Minute}}
	if !reflect.DeepEqual(h.runner.gates, want) {
		t.Fatalf("gates = %+v, want %+v", h.runner.gates, want)
	}
	if len(h.forge.pushes) != 1 {
		t.Fatalf("pushes = %v", h.forge.pushes)
	}
	var gate *Event
	for _, e := range h.events() {
		if e.Kind == KindAction && detailString(e, "action") == ActGate {
			e := e
			gate = &e
		}
	}
	if gate == nil || detailString(*gate, "sha") != "c0ffee" || gate.Details["command"] == nil {
		t.Fatalf("gate event = %+v", gate)
	}
	if n, ok := detailInt(*gate, "exit"); !ok || n != 0 {
		t.Fatalf("gate event exit = %v", gate.Details["exit"])
	}
	if _, ok := gate.Details["duration_s"]; !ok {
		t.Fatalf("gate event lacks its duration: %+v", gate)
	}
}

// A failing gate stops the push; it is retried on the backoff schedule and
// given up, with one escalation, after MaxRestarts failures.
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
	exit, err := ExecRunner{}.Gate(dir, []string{"./bin/check"}, 10*time.Second)
	if err != nil || exit != 3 {
		t.Fatalf("gate = %d, %v", exit, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ran")); strings.TrimSpace(string(b)) != dir {
		t.Fatalf("gate ran in %q", b)
	}
	if _, err := (ExecRunner{}).Gate(dir, []string{"sleep", "30"}, 100*time.Millisecond); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("hung gate: %v", err)
	}
}
