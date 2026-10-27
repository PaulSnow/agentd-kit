package main

import (
	"strings"
	"testing"
	"time"
)

// A start at sessions.max ends a finished session agentd started, the
// oldest first, and tells the console which and why; a finished one
// whose request is still open is not ended, and with every place held by
// one at work the start is refused, as before.
func TestCappedStartEndsFinishedSession(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	w1 := h.startSession("issue-1", KindWork, 1)
	h.clock.advance(time.Minute)
	w2 := h.startSession("issue-2", KindWork, 2)
	h.ticks(1, time.Minute)
	h.alive(w1, "finished\n"+idleScreen)
	h.alive(w2, "finished\n"+idleScreen)
	h.finish(w1)
	h.finish(w2)
	h.ticks(2, time.Minute)
	if h.sup.st.Tracks[w1].State != Done || h.sup.st.Tracks[w2].State != Done {
		t.Fatalf("states: %s %s", h.sup.st.Tracks[w1].State, h.sup.st.Tracks[w2].State)
	}

	// Neither is finished while its delivery has no outcome: what it
	// wrote is not delivered yet.
	if why := h.sup.finishedFor(w2); why != "" {
		t.Fatalf("a session whose delivery is pending is finished: %s", why)
	}
	for _, w := range []string{w1, w2} {
		tr := h.sup.st.Tracks[w]
		if tr.Delivered == nil {
			tr.Delivered = map[string]string{}
		}
		tr.Delivered[w+":"+h.sup.cfg.Sessions[w].Cwd] = "no MR.md: nothing to deliver"
	}
	// w1, the older, delivered a request still open: it may get a fix
	// round, so w2 is the one ended.
	h.sup.st.Tracks[w1].Deliveries = []DeliveryRecord{{Ref: "!3", Repository: "widgets"}}
	if why := h.sup.finishedFor(w1); why != "" {
		t.Fatalf("a session with an open request is finished: %s", why)
	}
	claudeAnswers(t, dir, 1, agentWork("agentd starts a session for it.", "Paint it", "Paint the shed red."))
	if err := reply(h, "widgets", "Paint the shed red."); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	in := h.instruction(t)
	if _, ok := h.sup.st.Runtime[w2]; ok || in.Failed != "" || !in.Started || h.sup.st.Runtime[in.Session].Owner != in.ID {
		t.Fatalf("instruction = %+v, runtime %v", in, h.sup.st.Runtime)
	}
	if _, ok := h.sup.st.Runtime[w1]; !ok {
		t.Fatal("w1, whose request is open, was ended")
	}
	var notice string
	for _, e := range h.events() {
		if e.Kind == KindMessage && e.Message.From == "agentd" && strings.Contains(e.Message.Text, "to make room") {
			notice = e.Message.Text
		}
		if e.Kind == KindSession && e.Session == w2 && e.Runtime.Action == SessionEnd && !strings.Contains(e.Runtime.Why, "wrote DONE") {
			t.Fatalf("the end's why: %+v", e.Runtime)
		}
	}
	if !strings.Contains(notice, "agentd ended "+w2) || !strings.Contains(notice, "wrote DONE, with no request delivered") || !strings.Contains(notice, in.Session) {
		t.Fatalf("notice = %q", notice)
	}

	// The request merges: w1 is finished now.
	h.sup.st.CI[ciKey("widgets", "!3")] = CIRecord{CIPayload: CIPayload{State: CIMerged}}
	if why := h.sup.finishedFor(w1); !strings.Contains(why, "merged or closed (!3)") {
		t.Fatalf("why = %q", why)
	}
	// One stopped with nothing delivered waits on the owner's clear.
	h.sup.st.Tracks[w1].State, h.sup.st.Tracks[w1].Deliveries = Stopped, nil
	if why := h.sup.finishedFor(w1); why != "" {
		t.Fatalf("a stopped session with nothing delivered is finished: %s", why)
	}
	// One at work is never ended: the new session is working.
	h.alive(in.Session, "painting")
	h.ticks(1, time.Minute)
	if why := h.sup.finishedFor(in.Session); why != "" {
		t.Fatalf("a working session is finished: %s", why)
	}
}
