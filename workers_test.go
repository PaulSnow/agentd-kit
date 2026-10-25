package main

import (
	"bytes"
	"context"
	"reflect"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a log buffer the loop, its workers and the test may use
// at once.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.b.Bytes()...)
}

// waitFor polls cond until it holds, failing the test after a few seconds
// of real time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A gate that takes a long time runs in a worker: the pass goes on, and a
// session that is gone is restarted while the gate still runs.
func TestSlowGateDoesNotDelayARestart(t *testing.T) {
	h := newHarness(t, false, "d", "gone")
	doneWithDelivery(t, h, "d")
	h.status("gone", working)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	h.runner.onGate = func() { <-release }
	passed := make(chan error, 1)
	go func() { passed <- h.sup.Pass() }()
	select {
	case err := <-passed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pass waited for the gate")
	}
	waitFor(t, "the gate to start", func() bool { return h.runner.gateCount() == 1 })
	if h.tmux.count("new-session", "gone") != 1 {
		t.Fatal("the restart waited for the gate")
	}
	if n := len(h.forge.pushes); n != 0 {
		t.Fatalf("pushed %d times before the gate ended", n)
	}
	close(release)
	if err := h.sup.Wait(); err != nil {
		t.Fatal(err)
	}
	if len(h.forge.pushes) != 1 || h.sup.Tracks()["d"].Delivered["d:"+h.dir+"/d/wt"] == "" {
		t.Fatalf("after the gate: pushes %v, delivered %v", h.forge.pushes, h.sup.Tracks()["d"].Delivered)
	}
	// A second pass while nothing runs starts no second delivery.
	h.ticks(2, 5*time.Minute)
	if len(h.forge.pushes) != 1 || len(h.runner.gates) != 1 {
		t.Fatalf("pushes %v, gates %d", h.forge.pushes, len(h.runner.gates))
	}
}

// A job still running is not started again by the next pass.
func TestRunningJobIsNotStartedTwice(t *testing.T) {
	h := newHarness(t, false, "d")
	doneWithDelivery(t, h, "d")
	release := make(chan struct{})
	h.runner.onGate = func() { <-release }
	h.sup.Pass() // returns at once: TestSlowGateDoesNotDelayARestart
	waitFor(t, "the gate to start", func() bool { return h.runner.gateCount() == 1 })
	h.clock.advance(5 * time.Minute)
	h.sup.Pass()
	h.clock.advance(5 * time.Minute)
	h.sup.Pass()
	close(release)
	h.sup.Wait()
	if h.runner.gateCount() != 1 || len(h.forge.pushes) != 1 {
		t.Fatalf("gates %d, pushes %v", h.runner.gateCount(), h.forge.pushes)
	}
}

// What a worker appended is in the log before the loop applies it, so a
// crash of agentd in between loses nothing: replay applies it.
func TestWorkerOutcomeSurvivesACrash(t *testing.T) {
	h := newHarness(t, false, "d", "ask")
	doneWithDelivery(t, h, "d")
	h.status("ask", blocked)
	h.alive("ask", "waiting")
	h.sup.Pass()
	h.sup.work.jobs.Wait() // the jobs ended, and the loop never drained them
	if h.sup.Tracks()["d"].Delivered["d:"+h.dir+"/d/wt"] != "" {
		t.Fatal("setup: the loop applied the worker's events without a drain")
	}
	h.restart(false)
	tr := h.sup.Tracks()
	if tr["d"].Delivered["d:"+h.dir+"/d/wt"] == "" || !tr["ask"].Posted["questions:ask:1"] {
		t.Fatalf("after replay: delivered %v, posted %v", tr["d"].Delivered, tr["ask"].Posted)
	}
	h.ticks(3, 5*time.Minute)
	if len(h.forge.pushes) != 1 || len(h.runner.calls) != 1 {
		t.Fatalf("after the restart: pushes %v, escalations %d", h.forge.pushes, len(h.runner.calls))
	}
}

// An owner record another process appends wakes the loop within the wake
// check, long before the next poll: here a clear is applied, and the
// parked session restarted, with the poll interval an hour away.
func TestOwnerRecordWakesTheLoop(t *testing.T) {
	h := newHarness(t, false, "mon")
	h.cfg.PollInterval = Duration{time.Hour}
	h.status("mon", working)
	h.tmux.crash["mon"] = true
	buf := &lockedBuffer{}
	h.sup = NewSupervisor(h.cfg, NewLogWriter(buf), h.clock, h.tmux, h.git, h.forge, h.runner, false)
	// Park the session first: it crashes on every start.
	parked := func() bool { tr := h.sup.Tracks()["mon"]; return tr != nil && tr.Parked }
	for i := 0; i < 12 && !parked(); i++ {
		h.sup.Pass()
		h.sup.Wait()
		h.clock.advance(time.Hour)
	}
	if !parked() {
		t.Fatal("setup: the session did not park")
	}
	h.tmux.crash["mon"] = false
	restarts := h.tmux.count("new-session", "mon")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.sup.Run(ctx, false) }()
	waitFor(t, "the loop to wait for the poll and the wake check", func() bool { return h.clock.waiting() == 2 })
	if err := ClearSession(h.cfg, NewLogWriter(buf), h.clock.Now(), "mon"); err != nil {
		t.Fatal(err)
	}
	// Time moves a second at a time; the wake check comes every
	// WakeCheck, the poll only after an hour.
	start := h.clock.Now()
	waitFor(t, "the cleared session to restart", func() bool {
		if h.tmux.count("new-session", "mon") > restarts {
			return true
		}
		h.clock.advance(time.Second)
		return false
	})
	if waited := h.clock.Now().Sub(start); waited > 2*WakeCheck {
		t.Fatalf("the restart came %s after the clear, want within %s", waited, 2*WakeCheck)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A job that ends at once, a delivery that is held say, does not wake the
// loop: it waits for the poll, rather than passing again and again.
func TestQuickJobsDoNotSpinTheLoop(t *testing.T) {
	h := newHarness(t, false, "d")
	h.cfg.PollInterval = Duration{time.Hour}
	doneWithDelivery(t, h, "d")
	s := h.cfg.Sessions["d"]
	s.Hold = "waiting for the owner's review"
	h.cfg.Sessions["d"] = s
	buf := &lockedBuffer{}
	h.sup = NewSupervisor(h.cfg, NewLogWriter(buf), h.clock, h.tmux, h.git, h.forge, h.runner, false)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.sup.Run(ctx, false) }()
	waitFor(t, "the loop to wait", func() bool { return h.clock.waiting() >= 2 })
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := h.tmux.count("has-session", "d"); n != 1 {
		t.Fatalf("%d passes with the clock stopped, want 1", n)
	}
}

// A dialog the owner answers while its escalation command still runs is
// over when that escalation settles: the next dialog is escalated, and the
// loop's state is the replay's.
func TestDialogAnsweredDuringItsEscalation(t *testing.T) {
	h := newHarness(t, false, "dlg")
	h.status("dlg", working)
	h.alive("dlg", "Do you want to proceed?\n❯ 1. Yes")
	release := make(chan struct{})
	h.runner.onRun = func() { <-release }
	h.sup.Pass() // the escalation starts, and waits
	waitFor(t, "the escalation to start", func() bool { return h.runner.attemptCount() == 1 })
	h.clock.advance(5 * time.Minute)
	h.tmux.setPane("dlg", "working again")
	h.sup.Pass() // the dialog is gone; the escalation is still running
	close(release)
	h.sup.Wait()
	sameAfterReplay(t, h, "after the late escalation")
	if h.sup.Tracks()["dlg"].Dialog.Open() {
		t.Fatal("the escalation settled a dialog that had gone away")
	}
	h.runner.onRun = nil
	h.clock.advance(31 * time.Minute)
	h.tmux.setPane("dlg", "Do you want to delete it?\n❯ 1. Yes")
	h.ticks(2, 5*time.Minute)
	if len(h.runner.calls) != 2 {
		t.Fatalf("escalations = %d, want the second dialog's too", len(h.runner.calls))
	}
	sameAfterReplay(t, h, "after the second dialog")
}

// clone copies every map, slice and pointer of a Track, so a worker never
// shares one with the loop. A field added to Track must be added to clone, and
// here.
func TestCloneCopiesEveryMapAndSlice(t *testing.T) {
	cloned := map[string]bool{"Restarts": true, "Usage": true, "Crashes": true, "Posted": true, "Delivered": true, "Skipped": true, "Failures": true,
		"DeliveredRefs": true, "Blocked": true, "GaveUp": true, "CutShort": true, "Filtered": true, "Deliveries": true, "LastObs": true, "Escalated": true,
		"Answers": true, "Decisions": true, "Raised": true, "Deadlines": true, "Gated": true, "Turns": true, "TurnLines": true, "Wakes": true, "WakeBase": true, "Woken": true, "CIFixes": true, "CIRaised": true, "Merges": true, "Merged": true}
	ty := reflect.TypeOf(Track{})
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		if k := f.Type.Kind(); (k == reflect.Map || k == reflect.Slice || k == reflect.Pointer) && !cloned[f.Name] {
			t.Errorf("Track.%s is a %s that clone does not copy", f.Name, k)
		}
	}
	tr := newTrack()
	tr.Posted["a"], tr.Delivered["a"], tr.Skipped["a"], tr.Blocked["a"], tr.Filtered["a"] = true, "x", true, "x", true
	tr.Failures["a"] = []time.Time{{}}
	c := tr.clone()
	c.Posted["b"], c.Delivered["b"], c.Skipped["b"], c.Blocked["b"], c.Filtered["b"] = true, "y", true, "y", true
	c.Failures["a"] = append(c.Failures["a"], time.Time{})
	if len(tr.Posted)+len(tr.Delivered)+len(tr.Skipped)+len(tr.Blocked)+len(tr.Filtered) != 5 || len(tr.Failures["a"]) != 1 {
		t.Fatalf("the clone shares the original's maps: %+v", tr)
	}
}
