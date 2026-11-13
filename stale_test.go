package main

import (
	"strings"
	"testing"
	"time"
)

// statusRow is session name's row of agentd status over the harness's
// log, read at now.
func (h *harness) statusRow(name string, now time.Time) SessionStatus {
	h.t.Helper()
	rows, _ := SummarizeAt(h.cfg, h.events(), ModeLive, now)
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	h.t.Fatalf("no status row for %s", name)
	return SessionStatus{}
}

// A runtime session observed working and then ended reads gone, with the
// time it was last seen, three days later: in status and in the report
// the console is built from. It never reads "screen changed recently".
// An ended reviewer's or judge's track is kept the same way (applySession);
// a work session stands for them, as a reviewer with nothing to review is
// ended at once.
func TestEndedRuntimeSessionShowsGone(t *testing.T) {
	h := sessionsHarness(t)
	name := h.startSession("fix/spin", KindWork, 7)
	h.ticks(2, time.Minute)
	h.alive(name, "working")
	h.ticks(1, time.Minute)
	h.alive(name, "working more")
	h.ticks(1, time.Minute)
	if st := h.state(name); st != Working {
		t.Fatalf("state = %v, want working", st)
	}
	seen := h.sup.Tracks()[name].LastSeen
	h.sup.emit(name, Event{Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: newIntentID(), Action: SessionEnd, Repository: "widgets", By: "agentd"}})
	h.ticks(2, time.Minute)
	later := h.clock.Now().Add(72 * time.Hour)

	r := h.statusRow(name, later)
	if r.State != Gone || !strings.Contains(r.Reason, "gone since "+seen.UTC().Format(time.RFC3339)) || strings.Contains(r.Reason, "screen changed recently") {
		t.Fatalf("status row = %s: %s", r.State, r.Reason)
	}
	st := NewLogState(h.cfg, ModeLive).Fold(h.events())
	rep := ReportFrom(withRuntime(h.cfg, st.Runtime), st, "", later)
	for _, rr := range rep.Repositories {
		for _, c := range rr.Cases {
			if c.Session == name && c.State != string(Gone) {
				t.Fatalf("report case = %+v", c)
			}
		}
	}
}

// A supervised session not observed for over two heartbeats reads stale
// since it was last seen, and so does one whose run has stopped; the end
// of a --once run's pass does not make it stale.
func TestUnobservedSessionShowsStale(t *testing.T) {
	h := newHarness(t, false, "blocked")
	h.status("blocked", blocked)
	h.alive("blocked", "static")
	h.ticks(3, time.Minute)
	if st := h.state("blocked"); st != BlockedOnOwner {
		t.Fatalf("state = %v", st)
	}
	seen := h.sup.Tracks()["blocked"].LastSeen
	want := "stale since " + seen.UTC().Format(time.RFC3339) + "; last seen blocked-on-owner"

	if r := h.statusRow("blocked", seen.Add(observeHeartbeat)); r.State != BlockedOnOwner {
		t.Fatalf("fresh row = %s: %s", r.State, r.Reason)
	}
	if r := h.statusRow("blocked", seen.Add(2*observeHeartbeat+time.Minute)); r.State != Stale || !strings.HasPrefix(r.Reason, want) {
		t.Fatalf("unobserved row = %s: %s", r.State, r.Reason)
	}

	h.sup.emit("", Event{Kind: KindStop, Stop: &StopPayload{Reason: "once"}})
	if r := h.statusRow("blocked", seen.Add(time.Minute)); r.State != BlockedOnOwner {
		t.Fatalf("after a --once pass: %s: %s", r.State, r.Reason)
	}
	h.sup.emit("", Event{Kind: KindStop, Stop: &StopPayload{Reason: "interrupt"}})
	if r := h.statusRow("blocked", seen.Add(time.Minute)); r.State != Stale || !strings.HasPrefix(r.Reason, want) {
		t.Fatalf("stopped run: %s: %s", r.State, r.Reason)
	}
}

// A quiet session agentd still observes, whose observation does not
// change, logs one again each heartbeat and never reads stale.
func TestQuietSessionHeartbeat(t *testing.T) {
	h := newHarness(t, false, "blocked")
	h.status("blocked", blocked)
	h.alive("blocked", "static")
	h.ticks(3*12, 5*time.Minute) // three hours
	if n := h.count(KindObserve, "", "blocked"); n < 6 {
		t.Fatalf("%d observe events in three hours, want one at least each %v", n, observeHeartbeat)
	}
	if r := h.statusRow("blocked", h.clock.Now()); r.State != BlockedOnOwner {
		t.Fatalf("quiet row = %s: %s", r.State, r.Reason)
	}
}
