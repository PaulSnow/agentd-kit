package main

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// trackView is what a Track holds that the log decides: everything but
// the pane memory a pass keeps for itself (the hash a nudge's echo is
// absorbed into, and the last observation, kept to log only changes).
func trackView(t *Track) Track {
	c := *t
	c.Hash, c.LastObs = "", nil
	return c
}

// Replaying the log gives the state the live path built from the same
// events, session by session, over a run that crashes, nudges, escalates
// and updates Questions, meets a dialog and a usage limit, fails and passes
// a gate, gives an escalation up and delivers.
func TestReplayEqualsTheLiveState(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		h := newHarness(t, shadow, "mon", "idle", "ask", "dlg", "lim", "d", "gate")
		h.status("mon", working)
		n := 0
		h.tmux.deadScreen["mon"] = func() string { n++; return fmt.Sprintf("error %d", n%2) }
		h.tmux.crash["mon"], h.tmux.remain["mon"] = true, true
		h.status("idle", working)
		h.alive("idle", "static")
		h.status("ask", blocked)
		h.alive("ask", "waiting")
		h.status("dlg", working)
		h.alive("dlg", "Do you want to proceed?\n❯ 1. Yes")
		h.status("lim", working)
		h.alive("lim", "Claude usage limit reached. Your limit will reset at 5pm")
		doneWithDelivery(t, h, "d")
		doneWithDelivery(t, h, "gate")
		h.git.branches = map[string]string{h.dir + "/gate/wt": "feature"}
		h.runner.failing = 2
		h.sup.Run(contextWithCancel(t), true)
		for i := 0; i < 30; i++ {
			switch i {
			case 6:
				h.status("ask", blocked+"2. And which release?\n")
			case 10:
				// The limit clears while the pane cannot take the resume
				// line, so it is still owed when agentd restarts.
				h.tmux.panes["lim"], h.tmux.commands["lim"] = "ready", "bash"
			case 12:
				h.tmux.commands["lim"] = ""
			case 14:
				h.status("ask", working)
			}
			h.ticks(1, 7*time.Minute)
			sameAfterReplay(t, h, fmt.Sprintf("shadow=%v pass %d", shadow, i))
		}
	}
}

// sameAfterReplay fails the test when a replay of the log so far differs
// from the live state.
func sameAfterReplay(t *testing.T, h *harness, when string) {
	t.Helper()
	live := h.sup.Tracks()
	replayed := NewLogState(h.cfg, h.sup.mode()).Fold(h.events()).Tracks
	if len(live) != len(replayed) {
		t.Fatalf("%s: %d live tracks, %d replayed", when, len(live), len(replayed))
	}
	for name, lt := range live {
		rt, ok := replayed[name]
		if !ok {
			t.Fatalf("%s: %s missing from the replay", when, name)
		}
		if a, b := trackView(lt), trackView(rt); !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: %s differs after replay:\nlive     %+v\nreplayed %+v", when, name, a, b)
		}
	}
}

// An intent whose outcome never reached the log, because agentd died
// between the two, is shown at the next start, in the log and by status,
// and an escalation cut short this way is never posted again.
func TestCutShortIntentIsShownAndNotRepeated(t *testing.T) {
	h := newHarness(t, false, "ask")
	h.status("ask", blocked)
	h.alive("ask", "waiting")
	// The escalation is posted, but its outcome cannot be written: the
	// process is as good as dead after it.
	w := &failingWriter{failOn: `"kind":"action"`}
	h.sup = NewSupervisor(h.cfg, NewLogWriter(w), h.clock, h.tmux, h.git, h.forge, h.runner, false)
	if err := h.sup.Pass(); err == nil {
		t.Fatal("the pass went on after the outcome could not be recorded")
	}
	if len(h.runner.calls) != 1 {
		t.Fatalf("posted %d times, want the one cut short", len(h.runner.calls))
	}
	h.buf = &bytes.Buffer{}
	h.buf.Write(w.Bytes())
	rows, _ := Summarize(h.cfg, h.events(), ModeLive)
	if len(rows) != 1 || len(rows[0].CutShort) != 1 || !strings.Contains(rows[0].CutShort[0], "escalate") {
		t.Fatalf("status before the restart = %+v", rows)
	}
	h.restart(false)
	h.sup.Run(contextWithCancel(t), true)
	h.ticks(12, 5*time.Minute)
	if len(h.runner.calls) != 1 {
		t.Fatalf("posted %d times after the restart, want 1", len(h.runner.calls))
	}
	if h.count(KindWarning, ActEscalate, "ask") != 1 {
		t.Fatalf("want one warning naming the cut-short escalation:\n%s", h.buf.String())
	}
	var out bytes.Buffer
	rows, meta := Summarize(h.cfg, h.events(), ModeLive)
	PrintStatus(&out, rows, meta, h.clock.now)
	if !strings.Contains(out.String(), "cut short: escalate questions:ask:1") {
		t.Fatalf("status does not show the cut-short escalation:\n%s", out.String())
	}
}

// status lists every configured session, one with no events yet too, and
// shows why a delivery waits and which escalations were given up on.
func TestStatusShowsSilentSessionsAndBlockers(t *testing.T) {
	h := newHarness(t, false, "quiet", "d", "ask")
	h.status("d", "# Finished\n\nDONE\n")
	h.deliver("d", Deliver{Repository: "widgets", Worktree: h.dir + "/d/wt", Target: "main", MRFile: h.dir + "/missing.md"})
	h.status("ask", blocked)
	h.alive("ask", "waiting")
	h.runner.failing = 100
	h.ticks(12*12, 5*time.Minute)
	var events []Event
	for _, e := range h.events() {
		if e.Session != "quiet" {
			events = append(events, e)
		}
	}
	rows, meta := Summarize(h.cfg, events, ModeLive)
	byName := map[string]SessionStatus{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if _, ok := byName["quiet"]; !ok || len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	if b := byName["d"].Blocked; len(b) != 1 || !strings.Contains(b[0], "merge-request text is missing") {
		t.Fatalf("d blocked = %v", b)
	}
	if g := byName["ask"].GaveUp; len(g) != 1 || g[0] != "questions:ask:1" {
		t.Fatalf("ask gave up = %v", g)
	}
	var out bytes.Buffer
	PrintStatus(&out, rows, meta, h.clock.now)
	for _, want := range []string{"quiet", "delivery: DONE but the merge-request text is missing", "gave up escalating questions:ask:1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
}

// status shows one mode at a time: the latest run's by default, so a
// shadow run's would-be restarts are not counted as a live run's.
func TestStatusKeepsModesApart(t *testing.T) {
	h := newHarness(t, true, "gone")
	h.status("gone", working)
	h.sup.Run(contextWithCancel(t), true)
	h.ticks(3, 5*time.Minute)
	events := h.events()
	if m := statusMode(events); m != ModeShadow {
		t.Fatalf("default mode = %s", m)
	}
	shadow, _ := Summarize(h.cfg, events, ModeShadow)
	live, _ := Summarize(h.cfg, events, ModeLive)
	if shadow[0].Restarts == 0 || live[0].Restarts != 0 || live[0].State != "" {
		t.Fatalf("shadow %+v, live %+v", shadow[0], live[0])
	}
}
