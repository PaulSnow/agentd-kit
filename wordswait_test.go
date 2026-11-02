package main

import (
	"strings"
	"testing"
	"time"
)

// unassignedItem is an open item of the owner's that no session holds:
// every place of widgets is held by a session at work, so agentd starts
// none for it. It returns the item and the sessions holding the places.
func (h *harness) unassignedItem() (*WorkItem, []string) {
	h.t.Helper()
	w1 := h.startSession("issue-1", KindWork, 1)
	w2 := h.startSession("issue-2", KindWork, 2)
	h.alive(w1, "working")
	h.alive(w2, "working")
	it := h.askWork("Write the release notes.", ThreadWork{Title: "Release notes", Brief: "Write them.", DoneWhen: EndCheck{Kind: DoneOwner}}, "")
	h.ticks(2, time.Minute)
	if it = h.item(it.ID); it.State != WorkOpen || it.Session != "" {
		h.t.Fatalf("item = %+v", it)
	}
	return it, []string{w1, w2}
}

// The owner's words alone on an open item no session holds yet wait for
// the session the item is given to, and the owner is told so on the
// item; that session gets them whole, byte for byte in instructions.json
// and quoted under the item in items.json.
func TestWordsOnUnassignedItemWaitForItsSession(t *testing.T) {
	h := sessionsHarness(t)
	it, held := h.unassignedItem()
	text := longAnswer(2000)
	in := h.respondWork(it, text)
	h.ticks(2, time.Minute)
	in = h.sup.st.Instructions[in.ID]
	if in.Failed != "" || in.Session != "" || !strings.Contains(in.Waiting, "your words wait for the session "+it.ID+" is given to") {
		t.Fatalf("instruction = failed %q, session %q, waiting %q", in.Failed, in.Session, in.Waiting)
	}
	told := false
	for _, n := range h.noticesAbout(it.ID) {
		if strings.Contains(n, in.ID) && strings.Contains(n, "wait") {
			told = true
		}
		if strings.Contains(n, "F1-PART-ALPHA") {
			t.Fatalf("the notice quotes the owner's words: %q", n)
		}
	}
	if !told {
		t.Fatalf("the owner was not told: %q", h.noticesAbout(it.ID))
	}

	// A place frees: agentd starts a session for the item, and the words
	// go to it.
	if err := h.endSession(held[0]); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	it = h.item(it.ID)
	name := it.Session
	if name == "" {
		t.Fatalf("the item was given to no session: %+v", it)
	}
	h.alive(name, "launched\n"+idleScreen)
	h.ticks(3, time.Minute)
	if in = h.sup.st.Instructions[in.ID]; in.Session != name || in.Delivered.IsZero() || in.Failed != "" {
		t.Fatalf("instruction = session %q, delivered %v, failed %q, waiting %q", in.Session, in.Delivered, in.Failed, in.Waiting)
	}
	workdir := h.sup.cfg.Sessions[name].Workdir
	whole := false
	for _, d := range readInstructions(t, workdir) {
		if d.ID == in.ID && d.Text == text {
			whole = true
		}
	}
	if !whole {
		t.Fatalf("instructions.json of %s does not hold %s whole: %+v", name, in.ID, readInstructions(t, workdir))
	}
	if doc := itemsDocOf(t, workdir, it.ID); !strings.Contains(doc, in.ID) || !strings.Contains(doc, "F1-PART-ALPHA") || !strings.Contains(doc, "F2-PART-OMEGA") || !strings.Contains(doc, "F2-END-ZETA") {
		t.Fatalf("items.json of %s does not carry the words under %s: %s", name, it.ID, doc)
	}
}

// The owner's words alone on a done or a declined item fail, with a
// reason naming the item and what it is, and reach no session.
func TestWordsOnClosedItemFail(t *testing.T) {
	for _, step := range []string{WorkStepDone, WorkStepDeclined} {
		t.Run(step, func(t *testing.T) {
			h := sessionsHarness(t)
			h.alive("a", "a works")
			it := h.askWork("Write the release notes.", ThreadWork{Title: "Release notes", Brief: "Write them.", DoneWhen: EndCheck{Kind: DoneOwner}}, "a")
			h.sup.workEmit(WorkPayload{ID: it.ID, Step: step, By: "owner"})
			if it = h.item(it.ID); !it.terminal() {
				t.Fatalf("item = %+v", it)
			}
			typed := h.typed("a")
			in := h.respondWork(it, "One more thing: add the date.")
			h.ticks(2, time.Minute)
			in = h.sup.st.Instructions[in.ID]
			if want := it.ID + " is " + it.State + " already; your words went to no session"; in.Failed != want || in.Waiting != "" || in.Session != "" {
				t.Fatalf("instruction = failed %q (want %q), waiting %q, session %q", in.Failed, want, in.Waiting, in.Session)
			}
			if got := h.typed("a"); strings.Contains(strings.Join(got[len(typed):], "\n"), in.ID) {
				t.Fatalf("the words were typed: %q", got)
			}
		})
	}
}
