package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func question(i int) string {
	return fmt.Sprintf("# Blocked\n\n## Questions\n1. Which version gates the fields? (wording %d)\n", i)
}

// A session that rewords its open Questions five times gets one escalation
// and four updates, each naming the session and saying the questions
// changed, one per change, never repeated after agentd restarts.
func TestRewordedQuestionsPostUpdatesNotEscalations(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.alive("s1", "waiting")
	for i := 0; i < 5; i++ {
		h.status("s1", question(i))
		h.ticks(3, 5*time.Minute)
	}
	if got := h.count(KindAction, ActEscalate, "s1"); got != 1 {
		t.Fatalf("escalations = %d, want 1", got)
	}
	if got := h.count(KindAction, ActUpdate, "s1"); got != 4 {
		t.Fatalf("updates = %d, want 4", got)
	}
	if got := len(h.runner.calls); got != 5 {
		t.Fatalf("posts = %d, want 5", got)
	}
	for _, c := range h.runner.calls[1:] {
		msg := c[4]
		if !strings.HasPrefix(msg, "s1") || !strings.Contains(msg, "changed") || c[1] != "widgets" {
			t.Fatalf("update does not name the session and the change: %v", c)
		}
	}
	if !strings.Contains(h.runner.calls[4][4], "wording 4") {
		t.Fatalf("last update lacks the new text: %q", h.runner.calls[4][4])
	}

	// Replay after a restart posts nothing again.
	h.restart(false)
	h.ticks(12, 5*time.Minute)
	if got := len(h.runner.calls); got != 5 {
		t.Fatalf("posts = %d after replay, want still 5", got)
	}
	// A change made while agentd was down is one update.
	h.status("s1", question(5))
	h.restart(false)
	h.ticks(6, 5*time.Minute)
	if got := h.count(KindAction, ActUpdate, "s1"); got != 5 {
		t.Fatalf("updates = %d after a change across a restart, want 5", got)
	}
}

// Questions that empty close the escalation; a later non-empty Questions
// section is a new escalation. DONE closes it too.
func TestEmptiedQuestionsReopenAsNewEscalation(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.alive("s1", "waiting")
	h.status("s1", question(0))
	h.ticks(3, 5*time.Minute)
	h.status("s1", "# Blocked\n\n## Questions\nnone\n")
	h.ticks(3, 5*time.Minute)
	h.status("s1", question(0)) // the same text again is a new episode
	h.ticks(3, 5*time.Minute)
	if got := h.count(KindAction, ActEscalate, "s1"); got != 2 {
		t.Fatalf("escalations = %d, want 2", got)
	}
	if got := h.count(KindAction, ActUpdate, "s1"); got != 0 {
		t.Fatalf("updates = %d, want 0", got)
	}

	h.status("s1", question(0)+"\nDONE\n")
	h.ticks(3, 5*time.Minute)
	h.status("s1", question(1))
	h.restart(false) // the episode count comes back from the log
	h.ticks(3, 5*time.Minute)
	if got := h.count(KindAction, ActEscalate, "s1"); got != 3 {
		t.Fatalf("escalations = %d after DONE then reopen, want 3", got)
	}
	h.restart(false)
	h.ticks(3, 5*time.Minute)
	if got := len(h.runner.calls); got != 3 {
		t.Fatalf("posts = %d after replay, want 3", got)
	}
}

// An update whose text matches a confidential pattern is replaced by a
// generic one, and the log never holds the text.
func TestConfidentialUpdateIsWithheld(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.cfg.ConfidentialPatterns = []string{`(?i)project\s+nightingale`}
	h.restart(false)
	h.alive("s1", "waiting")
	h.status("s1", question(0))
	h.ticks(2, 5*time.Minute)
	h.status("s1", "# Blocked\n\n## Questions\n1. Does Project Nightingale ship first?\n")
	h.ticks(2, 5*time.Minute)
	if got := h.count(KindAction, ActUpdate, "s1"); got != 1 {
		t.Fatalf("updates = %d, want 1", got)
	}
	msg := h.runner.calls[1][4]
	if strings.Contains(strings.ToLower(msg), "nightingale") || !strings.Contains(msg, "withheld") || !strings.HasPrefix(msg, "s1") {
		t.Fatalf("update leaked or was not withheld: %q", msg)
	}
	if strings.Contains(strings.ToLower(h.buf.String()), "nightingale") {
		t.Fatal("the event log carries the confidential text")
	}
}

// Shadow mode logs would versions of the escalation and the updates, posts
// nothing, and keeps the same counts across a restart.
func TestShadowQuestionUpdates(t *testing.T) {
	h := newHarness(t, true, "s1")
	h.alive("s1", "waiting")
	for i := 0; i < 5; i++ {
		h.status("s1", question(i))
		h.ticks(3, 5*time.Minute)
	}
	h.restart(true)
	h.ticks(6, 5*time.Minute)
	if len(h.runner.calls) != 0 || h.count(KindAction, "", "") != 0 {
		t.Fatalf("shadow acted: %v", h.runner.calls)
	}
	if got := h.count(KindWould, ActEscalate, "s1"); got != 1 {
		t.Fatalf("would escalate = %d, want 1", got)
	}
	if got := h.count(KindWould, ActUpdate, "s1"); got != 4 {
		t.Fatalf("would update = %d, want 4", got)
	}
}
