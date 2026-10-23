package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// failingWriter fails every write whose line contains failOn.
type failingWriter struct {
	bytes.Buffer
	failOn string
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.failOn != "" && strings.Contains(string(p), w.failOn) {
		return 0, errors.New("disk full")
	}
	return w.Buffer.Write(p)
}

// Every action is recorded as an intent before it is taken; when that
// record cannot be written the action is not taken, nothing else is done
// for the rest of the pass, and the pass returns the error.
func TestFailedIntentStopsThePass(t *testing.T) {
	h := newHarness(t, false, "a", "b")
	h.status("a", working)
	h.status("b", working)
	w := &failingWriter{failOn: `"kind":"intent"`}
	h.sup = NewSupervisor(h.cfg, NewLogWriter(w), h.clock, h.tmux, h.git, h.forge, h.runner, false)
	err := h.sup.Pass()
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("pass error = %v, want the append failure", err)
	}
	if h.tmux.mutations != 0 {
		t.Fatalf("tmux mutations = %d after the intent could not be recorded", h.tmux.mutations)
	}
	if got := h.tmux.count("has-session", "b"); got != 0 {
		t.Fatalf("session b observed %d times after the pass stopped", got)
	}
}

// When the outcome of an action cannot be recorded, the action has happened
// but nothing further is done in that pass.
func TestFailedOutcomeStopsThePass(t *testing.T) {
	h := newHarness(t, false, "a", "b")
	h.status("a", working)
	h.status("b", working)
	w := &failingWriter{failOn: `"kind":"action"`}
	h.sup = NewSupervisor(h.cfg, NewLogWriter(w), h.clock, h.tmux, h.git, h.forge, h.runner, false)
	if err := h.sup.Pass(); err == nil {
		t.Fatal("pass returned no error")
	}
	if got := h.tmux.count("new-session", ""); got != 1 {
		t.Fatalf("restarts = %d, want only the one whose outcome failed to record", got)
	}
	if !strings.Contains(w.String(), `"kind":"intent"`) {
		t.Fatal("no intent recorded before the action")
	}
	// The next pass tries again, and stops again at the first failed append.
	w.failOn = ""
	h.clock.advance(5 * time.Minute)
	if err := h.sup.Pass(); err != nil {
		t.Fatalf("pass with a working log: %v", err)
	}
}
