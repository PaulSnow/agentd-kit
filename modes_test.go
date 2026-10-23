package main

import (
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
