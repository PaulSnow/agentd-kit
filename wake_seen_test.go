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

// arriveAt logs a discussion of widgets' thread that moved at.
func (h *harness) arriveAt(thread string, at time.Time) string {
	id := thread + "@" + at.UTC().Format(time.RFC3339)
	it := ItemPayload{ID: "gitlab/example-org/widgets" + id, Kind: ItemDiscussion, Repository: "gitlab/example-org/widgets", Title: "Widget " + thread, SourceTime: at}
	h.sup.emit("", Event{Kind: KindItem, Mode: ModeShared, Item: &it})
	return "item: gitlab/example-org/widgets" + id
}

// agentd's merge of a request, and a session the coordinator started or
// ended for an issue, are its acts: their threads moving then do not wake
// it. The same threads moving later do (#49).
func TestCoordinatorActsDoNotWakeIt(t *testing.T) {
	h := sessionsHarness(t)
	h.ticks(2, time.Minute)
	h.sup.emit("d", Event{Kind: KindAction, Act: &ActPayload{Action: ActMerge, Key: mergeKey("widgets", "!3"), Repository: "widgets", Ref: "!3"}})
	h.arriveAt("!3", h.clock.Now().Add(20*time.Second))
	h.ticks(2, time.Minute)
	reply, refused := h.startSession(map[string]any{"branch": "fix/spin", "issue": 7})
	if refused {
		t.Fatalf("start = %v", reply)
	}
	h.arriveAt("#7", h.clock.Now())
	h.ticks(3, time.Minute)
	if _, refused := callOne(t, h.mcpServerFor("c"), ToolEndSession, map[string]any{"name": reply["name"]}); refused {
		t.Fatal("end refused")
	}
	h.arriveAt("#7", h.clock.Now().Add(-10*time.Second))
	h.ticks(3, time.Minute)
	if w := h.wakes(); len(w) != 0 {
		t.Fatalf("woke for its own acts: %+v", w)
	}
	h.ticks(1, 10*time.Minute)
	merged, issue := h.arriveAt("!3", h.clock.Now()), h.arriveAt("#7", h.clock.Now())
	h.ticks(2, time.Minute)
	if w := h.wakes(); len(w) != 1 || w[0].Reason != merged+"; "+issue {
		t.Fatalf("wakes = %+v", w)
	}
}

// An outsider's move on a request well before the coordinator approved it
// is not its echo: it wakes the coordinator (#49).
func TestMoveBeforeApprovalWakes(t *testing.T) {
	h, _ := requestsHarness(t)
	h.ticks(3, 5*time.Minute)
	if _, refused := h.approve("!2", "5eed0101"); refused {
		t.Fatal("approve refused")
	}
	before := h.arriveAt("!2", h.clock.Now().Add(-90*time.Second))
	h.ticks(2, time.Minute)
	if w := h.wakes(); len(w) != 1 || w[0].Reason != before {
		t.Fatalf("wakes = %+v", w)
	}
}

// A wake's turn that leaves no summary keeps its threads quiet only until
// the stall after the wake: a later move on them wakes the coordinator
// (#49).
func TestTurnWithoutSummaryEndsAtStall(t *testing.T) {
	h := coordHarness(t)
	h.arrive("#4", ItemIssue, "someone")
	h.ticks(2, time.Minute)
	if w := h.wakes(); len(w) != 1 {
		t.Fatalf("wakes = %+v", w)
	}
	h.ticks(5, 10*time.Minute)
	later := h.arriveAt("#4", h.clock.Now())
	h.ticks(2, time.Minute)
	if w := h.wakes(); len(w) != 2 || w[1].Reason != later {
		t.Fatalf("wakes = %+v", w)
	}
}

// Logins match as the forge has them, whatever their case (#49).
func TestOwnLoginMatchesAnyCase(t *testing.T) {
	h := coordHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Coordinator.Logins = []string{"Widget-Coord"}
	h.cfg.Repositories["widgets"] = w
	h.restart(false)
	h.arrive("#5", ItemIssue, "widget-coord")
	h.ticks(3, time.Minute)
	if w := h.wakes(); len(w) != 0 {
		t.Fatalf("woke for its own issue: %+v", w)
	}
}

// A pointer the transcript dates a little before its wake still begins
// the wake's turn: its summary ends it, though a later prompt follows
// (#49).
func TestPointerDatedBeforeWakeBeginsItsTurn(t *testing.T) {
	h := coordHarness(t)
	h.arrive("#4", ItemIssue, "someone")
	h.ticks(2, time.Minute)
	w := h.wakes()
	if len(w) != 1 {
		t.Fatalf("wakes = %+v", w)
	}
	at := h.lastWakeAt(t)
	h.transcriptOf("c", "t1", promptLine("p1", h.cfg.Policies.Coordinator.Pointer, at.Add(-time.Second))+
		summaryLine("c1", "m1", "Handled #4.", at.Add(2*time.Minute), 100)+
		promptLine("p2", "Something else.", at.Add(3*time.Minute))+summaryLine("c2", "m2", "Done.", at.Add(9*time.Minute), 100))
	h.ticks(1, 10*time.Minute)
	later := h.arriveAt("#4", h.clock.Now())
	h.ticks(2, time.Minute)
	if w := h.wakes(); len(w) != 2 || w[1].Reason != later {
		t.Fatalf("wakes = %+v", w)
	}
}

// lastWakeAt is the time of c's latest wake action.
func (h *harness) lastWakeAt(t *testing.T) time.Time {
	t.Helper()
	var at time.Time
	for _, e := range h.events() {
		if e.Kind == KindAction && actionOf(e) == ActWake {
			at = e.Time
		}
	}
	if at.IsZero() {
		t.Fatal("no wake")
	}
	return at
}
