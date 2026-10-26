package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// settled sets the coordinator policy's settle window and restarts the
// harness under it.
func settled(h *harness, d time.Duration) {
	h.cfg.Policies.Coordinator.Settle = Duration{d}
	h.restart(false)
}

// arrive logs a forge item for widgets.
func (h *harness) arrive(id, kind, author string) {
	it := ItemPayload{ID: "gitlab/example-org/widgets" + id, Kind: kind, Repository: "gitlab/example-org/widgets", Title: "Widget " + id,
		SourceTime: h.clock.Now(), Author: author}
	h.sup.emit("", Event{Kind: KindItem, Mode: ModeShared, Item: &it})
}

// A request's thread moving after its coordinator approved it is the
// approval's echo: it does not wake the coordinator. Another thread
// moving does (#49).
func TestCoordinatorApprovalDoesNotWakeIt(t *testing.T) {
	h, _ := requestsHarness(t)
	h.ticks(3, 5*time.Minute)
	if _, refused := h.approve("!2", "5eed0101"); refused {
		t.Fatal("approve refused")
	}
	h.arrive("!2@"+h.clock.Now().UTC().Format(time.RFC3339), ItemDiscussion, "")
	h.ticks(3, time.Minute)
	if w := h.wakes(); len(w) != 0 {
		t.Fatalf("woke for its own approval: %+v", w)
	}
	h.arrive("#9", ItemIssue, "someone")
	h.ticks(2, time.Minute)
	if w := h.wakes(); len(w) != 1 || w[0].Reason != "item: gitlab/example-org/widgets#9" {
		t.Fatalf("wakes = %+v", w)
	}
}

// An item the coordinator's own forge login wrote does not wake it, nor
// does a thread it was handling moving while it handled it: its own
// comment. The same thread moving later does (#49).
func TestOwnCommentItemDoesNotWake(t *testing.T) {
	h := coordHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Coordinator.Logins = []string{"widget-coord"}
	h.cfg.Repositories["widgets"] = w
	h.restart(false)
	h.arrive("#5", ItemIssue, "widget-coord")
	h.ticks(3, time.Minute)
	if w := h.wakes(); len(w) != 0 {
		t.Fatalf("woke for its own issue: %+v", w)
	}
	h.arrive("#4", ItemIssue, "someone")
	h.ticks(2, time.Minute)
	if w := h.wakes(); len(w) != 1 || w[0].Reason != "item: gitlab/example-org/widgets#4" {
		t.Fatalf("wakes = %+v", w)
	}
	// It comments on #4 while it handles the wake.
	h.ticks(1, time.Minute)
	h.arrive("#4@"+h.clock.Now().UTC().Format(time.RFC3339), ItemDiscussion, "")
	h.ticks(1, time.Minute)
	h.transcriptOf("c", "t1", summaryLine("c1", "m1", "Commented on #4.", h.clock.Now(), 100))
	h.ticks(3, time.Minute)
	if w := h.wakes(); len(w) != 1 {
		t.Fatalf("woke for its own comment: %+v", w)
	}
	h.ticks(1, 10*time.Minute)
	later := "#4@" + h.clock.Now().UTC().Format(time.RFC3339)
	h.arrive(later, ItemDiscussion, "")
	h.ticks(2, time.Minute)
	if w := h.wakes(); len(w) != 2 || w[1].Reason != "item: gitlab/example-org/widgets"+later {
		t.Fatalf("wakes = %+v", w)
	}
}

// An owner instruction wakes the coordinator at once, through the settle
// window, and carries what was settling with it (#49).
func TestOwnerInstructionAlwaysWakes(t *testing.T) {
	h := instrHarness(t)
	settled(h, 2*time.Minute)
	h.ticks(2, time.Minute)
	h.arrive("#4", ItemIssue, "someone")
	h.ticks(1, time.Minute)
	if w := h.wakes(); len(w) != 0 {
		t.Fatalf("woke inside the settle window: %+v", w)
	}
	if err := reply(h, "widgets", "Put the squeak fix first."); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, time.Second)
	w := h.wakes()
	if len(w) != 1 || !strings.Contains(w[0].Reason, "item: gitlab/example-org/widgets#4") || !strings.Contains(w[0].Reason, "instruction: ") {
		t.Fatalf("wakes = %+v", w)
	}
}

// Two items a minute apart, inside the settle window, wake the
// coordinator once, with both (#49).
func TestTwoItemsAMinuteApartWakeOnce(t *testing.T) {
	h := coordHarness(t)
	settled(h, 2*time.Minute)
	h.ticks(2, time.Minute)
	h.arrive("#4", ItemIssue, "someone")
	h.ticks(1, time.Minute)
	h.arrive("#6", ItemIssue, "someone")
	h.ticks(4, time.Minute)
	w := h.wakes()
	if len(w) != 1 || w[0].Reason != "item: gitlab/example-org/widgets#4; item: gitlab/example-org/widgets#6" {
		t.Fatalf("wakes = %+v", w)
	}
}

// agentd status shows each of a coordinator's latest wakes with its
// reason and what it spent, so an empty wake shows (#49).
func TestStatusShowsSpendPerWake(t *testing.T) {
	h := coordHarness(t)
	id := launchCoordinator(t, h)
	h.transcriptOf("c", id, usageLine("m0", 1000, 0, 0, 0, false))
	h.ticks(2, time.Minute)
	h.status("a", "# a\n\nDONE\n")
	h.ticks(2, time.Minute)
	h.transcriptOf("c", id, summaryLine("c1", "m1", "Handled.", h.clock.Now(), 0)) // spend 1+50 more
	h.ticks(1, time.Minute)
	h.status("b", "# b\n\nDONE\n")
	h.ticks(2, time.Minute)
	h.transcriptOf("c", id, usageLine("m2", 149, 0, 0, 0, false))
	h.ticks(2, time.Minute)
	rows, meta := Summarize(h.cfg, h.events(), ModeLive)
	var c SessionStatus
	for _, r := range rows {
		if r.Name == "c" {
			c = r
		}
	}
	if len(c.WakeLog) != 2 || c.WakeLog[0].Reason != "finished: a" || c.WakeLog[0].Spend != 51 ||
		c.WakeLog[1].Reason != "finished: b" || c.WakeLog[1].Spend != 149 {
		t.Fatalf("wake log = %+v", c.WakeLog)
	}
	var out bytes.Buffer
	PrintStatus(&out, rows, meta, h.clock.Now())
	if !strings.Contains(out.String(), "latest wakes: finished: a 51, finished: b 149") {
		t.Fatalf("status:\n%s", out.String())
	}
}
