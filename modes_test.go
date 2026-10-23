package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// Every event carries the mode of the run that wrote it.
func TestEveryEventCarriesItsMode(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		h := newHarness(t, shadow, "s1")
		h.status("s1", working)
		h.alive("s1", "static")
		h.sup.Run(contextWithCancel(t), true)
		h.ticks(12, 5*time.Minute)
		want := "live"
		if shadow {
			want = "shadow"
		}
		for _, e := range h.events() {
			if got := detailString(e, "mode"); got != want {
				t.Fatalf("shadow=%v: event %+v has mode %q, want %q", shadow, e, got, want)
			}
		}
	}
}

// A live run ignores what a shadow run logged: the shadow's would-be
// restarts, escalations and delivery do not use up the live run's.
func TestLiveReplayIgnoresShadowEvents(t *testing.T) {
	h := newHarness(t, true, "mon", "d")
	h.status("mon", working)
	h.tmux.crash["mon"] = true
	mr := h.dir + "/mr.md"
	writeFile(t, mr, "The MR description.\n")
	h.status("d", "# Finished\n\nDONE\n")
	h.deliver("d", Deliver{Repository: "widgets", Worktree: h.dir + "/d/wt", Target: "main", MRFile: mr})
	h.ticks(12*12, 5*time.Minute)
	if h.count(KindWould, ActRestart, "mon") == 0 || h.count(KindWould, ActOpen, "d") != 1 {
		t.Fatal("the shadow run did not log its would-be actions")
	}
	h.restart(false)
	h.ticks(3, 5*time.Minute)
	if got := h.tmux.count("new-session", "mon"); got == 0 {
		t.Fatal("the live run took the shadow's would-be restarts as its own")
	}
	if len(h.forge.opens) != 1 {
		t.Fatalf("opens = %d, want the live run to deliver once", len(h.forge.opens))
	}
}

// A shadow run ignores what a live run did: its own nudge allowance starts
// fresh rather than from the live run's count.
func TestShadowReplayUsesOnlyShadowEvents(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.alive("s1", "static")
	h.ticks(19, 5*time.Minute)
	if got := h.tmux.count("type", "s1"); got != 2 {
		t.Fatalf("live nudges = %d, want 2", got)
	}
	h.restart(true)
	h.ticks(5*12, 5*time.Minute)
	if got := h.count(KindWould, ActNudge, "s1"); got != 3 {
		t.Fatalf("would nudge = %d, want 3 counted from the shadow's own events", got)
	}
}

// Shadow mode fetches, since a fetch writes nothing to the remote, so its
// decisions use current refs; the fetch is logged as an action.
func TestShadowFetches(t *testing.T) {
	h := newHarness(t, true, "d")
	mr := h.dir + "/mr.md"
	writeFile(t, mr, "The MR description.\n")
	h.status("d", "# Finished\n\nDONE\n")
	h.deliver("d", Deliver{Repository: "widgets", Worktree: h.dir + "/d/wt", Target: "main", MRFile: mr})
	h.ticks(3, 5*time.Minute)
	if h.git.fetches != 1 || h.count(KindAction, ActFetch, "d") != 1 || h.count(KindWould, ActFetch, "d") != 0 {
		t.Fatalf("fetches = %d, fetch actions = %d", h.git.fetches, h.count(KindAction, ActFetch, "d"))
	}
	if len(h.forge.pushes) != 0 || len(h.forge.opens) != 0 {
		t.Fatal("shadow pushed or opened")
	}
}

// oldFormat rewrites the harness's log as the previous version wrote it:
// only start events carry a mode.
func oldFormat(t *testing.T, h *harness) {
	t.Helper()
	var out bytes.Buffer
	for _, e := range h.events() {
		if e.Kind != KindStart {
			delete(e.Details, "mode")
		}
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(append(b, '\n'))
	}
	h.buf.Reset()
	h.buf.Write(out.Bytes())
}

// A log written before events carried a mode replays as before: an untagged
// event belongs to the mode of the start before it, and an untagged
// mark-delivered record is the owner's. Nothing is escalated or delivered
// again, in either mode.
func TestOldFormatLogReplays(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		h := newHarness(t, shadow, "s1", "d", "m")
		h.status("s1", blocked)
		h.alive("s1", "waiting")
		doneWithDelivery(t, h, "d")
		doneWithDelivery(t, h, "m")
		h.git.branches = map[string]string{h.dir + "/m/wt": "other"}
		h.sup.Run(contextWithCancel(t), true)
		h.ticks(3, 5*time.Minute)
		h.buf.WriteString(`{"time":"2026-10-22T00:20:00Z","session":"m","kind":"action","details":{"action":"open","delivered":"m:` + h.dir + `/m/wt","ref":"!7","by":"mark-delivered"}}` + "\n")
		posts, opens := h.count(kindFor(shadow), ActEscalate, ""), h.count(kindFor(shadow), ActOpen, "")
		oldFormat(t, h)
		h.restart(shadow)
		h.ticks(6, 5*time.Minute)
		if got := h.count(kindFor(shadow), ActEscalate, ""); got != posts {
			t.Fatalf("shadow=%v: escalations = %d after replaying the old log, want %d", shadow, got, posts)
		}
		if got := h.count(kindFor(shadow), ActOpen, ""); got != opens {
			t.Fatalf("shadow=%v: opens = %d after replaying the old log, want %d", shadow, got, opens)
		}
		if got := h.sup.Tracks()["m"].Delivered["m:"+h.dir+"/m/wt"]; got != "!7" {
			t.Fatalf("shadow=%v: the untagged mark-delivered record was not read: %q", shadow, got)
		}
	}
}

func kindFor(shadow bool) string {
	if shadow {
		return KindWould
	}
	return KindAction
}

// A restart shadow mode only would have made starts nothing, so the session
// still gone at the next pass has not crashed: shadow logs no crash and
// never parks it, before or after a replay.
func TestShadowRestartsAreNotCrashes(t *testing.T) {
	h := newHarness(t, true, "gone")
	h.status("gone", working)
	h.ticks(12*12, 5*time.Minute)
	h.restart(true)
	h.ticks(12, 5*time.Minute)
	if got := h.count(KindCrash, "", "gone"); got != 0 {
		t.Fatalf("crash events = %d in shadow", got)
	}
	if h.state("gone") == CrashLooping || h.count(KindWould, ActRestart, "gone") < 4 {
		t.Fatalf("state = %s, would restarts = %d", h.state("gone"), h.count(KindWould, ActRestart, "gone"))
	}
}
