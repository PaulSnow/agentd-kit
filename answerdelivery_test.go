package main

import (
	"strings"
	"testing"
	"time"
)

// blockedItem is an item of the owner's held by session a, whose
// STATUS.md asks the owner under Questions: a is blocked on the owner,
// and so is the item.
func (h *harness) blockedItem() *WorkItem {
	h.t.Helper()
	h.alive("a", "a works")
	it := h.askWork("Write the release notes.", ThreadWork{Title: "Release notes", Brief: "Write them.", DoneWhen: EndCheck{Kind: DoneOwner}}, "a")
	h.status("a", "# Status\n\n## Now\n- "+it.ID+": writing\n\n## Questions\n- "+it.ID+" blocked: Short form or long form?\n")
	h.alive("a", "a asks\n"+idleScreen)
	h.ticks(2, time.Minute)
	if st := h.state("a"); st != BlockedOnOwner {
		h.t.Fatalf("a is %s, not blocked on the owner", st)
	}
	if it = h.item(it.ID); it.State != WorkBlocked || it.Session != "a" {
		h.t.Fatalf("item = %+v", it)
	}
	return it
}

// respondWork is the owner's words on the item's entry, as the page
// records them: the instruction they are.
func (h *harness) respondWork(it *WorkItem, text string) *Instruction {
	h.t.Helper()
	before := map[string]bool{}
	for id := range h.logState().Instructions {
		before[id] = true
	}
	re := &EntryRe{ID: entryWork + it.ID, Kind: "work", At: it.Opened}
	if err := recordInstructionRe(h.cfg, NewLogWriter(h.buf), h.clock.Now(), []string{"widgets"}, "", "", "ui", text, re, nil, nil); err != nil {
		h.t.Fatal(err)
	}
	return h.newInstruction(before)
}

// newInstruction is the one instruction the log holds that before did not.
func (h *harness) newInstruction(before map[string]bool) *Instruction {
	h.t.Helper()
	for id, in := range h.logState().Instructions {
		if !before[id] {
			return in
		}
	}
	h.t.Fatal("no instruction was recorded")
	return nil
}

// answerLine is the fixed line that says the owner answered on entry.
func answerLine(entry, id string) string {
	return "The owner answered on " + entry + " (instruction " + id + "): read " + InstructionsFile + " now, act on it, and answer in the text you end your turn with."
}

// instructActs are the instruct acts taken into session name.
func (h *harness) instructActs(name string) []*ActPayload {
	var out []*ActPayload
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Session == name && e.Act.Action == ActInstruct {
			out = append(out, e.Act)
		}
	}
	return out
}

// The owner's words on an item's entry reach its session blocked on the
// owner within one pass: instructions.json holds them and a fixed line
// naming the entry and the instruction is typed, never the words; the
// session's next prompt is it taking them.
func TestBlockedItemSessionGetsAnswerTyped(t *testing.T) {
	h := sessionsHarness(t)
	it := h.blockedItem()
	before := len(h.typed("a"))
	const text = "Use the short form."
	in := h.respondWork(it, text)
	h.ticks(1, time.Minute)
	typed := h.typed("a")[before:]
	if len(typed) != 1 || typed[0] != answerLine(entryWork+it.ID, in.ID) || strings.Contains(typed[0], text) {
		t.Fatalf("typed into a: %q", typed)
	}
	acts := h.instructActs("a")
	if len(acts) == 0 || !contains(acts[len(acts)-1].Instructions, in.ID) {
		t.Fatalf("no instruct act names %s: %+v", in.ID, acts)
	}
	docs := readInstructions(t, h.sup.cfg.Sessions["a"].Workdir)
	if last := docs[len(docs)-1]; last.ID != in.ID || last.Text != text || !last.New {
		t.Fatalf("instructions.json = %+v", docs)
	}
	in = h.logState().Instructions[in.ID]
	if in.Session != "a" || in.Delivered.IsZero() || in.Failed != "" {
		t.Fatalf("instruction = %+v", in)
	}
	h.transcriptOf("a", "t1", promptLine("u1", typed[0], h.clock.Now()))
	h.ticks(2, time.Minute)
	if in = h.logState().Instructions[in.ID]; in.Taken.IsZero() {
		t.Fatalf("the session's prompt did not take it: %+v", in)
	}
}

// After the go, the owner's words on the design entry reach the running
// session doing it the same way, blocked on the owner or not.
func TestDesignWordsAfterGoReachRunningSession(t *testing.T) {
	dir := fakeClaude(t)
	h := designerHarness(t, false)
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 2, designOut("Add PaintAll to paint.go", false, 0))
	it := h.askDesigned(dir, "Paint the shed red, all of it.")
	h.ticks(2, time.Minute)
	if err := h.respondDesign(it.ID, DesignGo, ""); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, time.Minute)
	it = h.item(it.ID)
	name := it.Session
	if name == "" {
		t.Fatalf("no session after go: %+v", it)
	}
	h.status(name, "# Status\n\n## Now\n- "+it.ID+": painting\n\n## Questions\n- "+it.ID+" blocked: Which red?\n")
	h.alive(name, "launched\n"+idleScreen)
	h.ticks(3, time.Minute)
	if st := h.state(name); st != BlockedOnOwner {
		t.Fatalf("%s is %s", name, st)
	}
	before := len(h.typed(name))
	ids := map[string]bool{}
	for id := range h.logState().Instructions {
		ids[id] = true
	}
	if err := h.respondDesign(it.ID, "", "Signal red."); err != nil {
		t.Fatal(err)
	}
	in := h.newInstruction(ids)
	h.ticks(1, time.Minute)
	if typed := h.typed(name)[before:]; len(typed) != 1 || typed[0] != answerLine(entryDesign+it.ID, in.ID) {
		t.Fatalf("typed into %s: %q", name, typed)
	}
	docs := readInstructions(t, h.sup.cfg.Sessions[name].Workdir)
	if last := docs[len(docs)-1]; last.ID != in.ID || last.Text != "Signal red." {
		t.Fatalf("instructions.json = %+v", docs)
	}
}

// An answer that cannot be typed into the item's session tells the owner
// at once, on the item: the instruction, its entry, why, and what the
// owner can do; the instruction fails with the reason.
func TestDeadPaneAnswerEscalatesItem(t *testing.T) {
	for _, c := range []string{"tmux gone", "not claude"} {
		t.Run(c, func(t *testing.T) {
			h := sessionsHarness(t)
			it := h.blockedItem()
			if c == "tmux gone" {
				h.tmux.alive["a"] = false
			} else {
				h.tmux.commands["a"] = "bash"
			}
			before := len(h.typed("a"))
			in := h.respondWork(it, "Use the short form.")
			h.ticks(1, time.Minute)
			if typed := h.typed("a")[before:]; len(typed) != 0 {
				t.Fatalf("typed into a: %q", typed)
			}
			var note string
			for _, e := range h.events() {
				if e.Kind == KindWorkItem && e.Work.ID == it.ID && e.Work.Step == WorkStepEscalate && strings.HasPrefix(e.Work.Key, "undelivered:") {
					note = e.Work.Note
				}
			}
			for _, want := range []string{it.ID, in.ID, entryWork + it.ID, "could not be typed into a", "Reopen the session", "reassign", "by hand"} {
				if !strings.Contains(note, want) {
					t.Fatalf("escalation lacks %q: %q", want, note)
				}
			}
			if in = h.logState().Instructions[in.ID]; in.Failed == "" || !strings.Contains(note, in.Failed) {
				t.Fatalf("instruction = %+v, note %q", in, note)
			}
			if ns := h.noticesAbout(it.ID); len(ns) == 0 || !strings.Contains(ns[len(ns)-1], in.ID) {
				t.Fatalf("notices = %q", ns)
			}
		})
	}
}

// Delivering the owner's answer settles STATUS.md's Questions as they
// stand: the session is no longer blocked on the owner, and no second
// line is typed for them; Questions the session rewrites block it again.
func TestBlockedEndsWhenAnswerDelivered(t *testing.T) {
	h := sessionsHarness(t)
	it := h.blockedItem()
	in := h.respondWork(it, "Use the short form.")
	h.ticks(1, time.Minute)
	if in = h.logState().Instructions[in.ID]; in.Delivered.IsZero() {
		t.Fatalf("not delivered: %+v", in)
	}
	before := len(h.typed("a"))
	h.alive("a", "a writes on\n"+idleScreen)
	h.ticks(2, time.Minute)
	if st := h.state("a"); st == BlockedOnOwner {
		t.Fatalf("a is still blocked on the owner: %s", h.sup.Tracks()["a"].LastObs.Reason)
	}
	if o := h.sup.Tracks()["a"].LastObs; o != nil && strings.Contains(o.Reason, "STATUS.md has open Questions") {
		t.Fatalf("reason = %q", o.Reason)
	}
	if typed := h.typed("a")[before:]; len(typed) != 0 {
		t.Fatalf("typed into a after the answer: %q", typed)
	}
	h.status("a", "# Status\n\n## Now\n- "+it.ID+": writing\n\n## Questions\n- "+it.ID+" blocked: Which headings?\n")
	h.alive("a", "a asks again\n"+idleScreen)
	h.ticks(2, time.Minute)
	if st := h.state("a"); st != BlockedOnOwner {
		t.Fatalf("new Questions: a is %s", st)
	}
}
