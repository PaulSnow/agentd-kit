package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// longAnswer is an owner's answer of n characters in two numbered parts
// on their own lines, each with a marker, the second's near the end.
func longAnswer(n int) string {
	p1 := "1. F1-PART-ALPHA: rewrap the doc comment on the test so it says which shapes it covers"
	p2 := "2. F2-PART-OMEGA: drop the one-validator case from the cases table"
	end := " F2-END-ZETA."
	pad := func(s string, k int) string {
		for utf8.RuneCountInString(s) < k {
			s += " and keep the wording short"
		}
		return string([]rune(s)[:k])
	}
	half := n / 2
	return pad(p1, half-1) + "\n" + pad(p2, n-half-utf8.RuneCountInString(end)) + end
}

// itemsDocOf is item id as items.json in workdir holds it.
func itemsDocOf(t *testing.T, workdir, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(workdir, ItemsFile))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Items []itemDoc }
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, d := range doc.Items {
		if d.ID == id {
			out, _ := json.Marshal(d)
			var s strings.Builder
			for _, n := range d.Notes {
				s.WriteString(n.Text + "\n")
			}
			return string(out) + "\n" + s.String()
		}
	}
	t.Fatalf("items.json holds no %s: %s", id, b)
	return ""
}

// An owner's answer of 2,000 characters in two numbered parts, sent on an
// item's entry with words alone, with not done or with re-assign, reaches
// the session that works the item whole: instructions.json holds the text
// byte for byte, newlines kept; items.json quotes both parts of a choice's
// words; and the line typed names the instruction and its length, never
// the owner's words (#77).
func TestLongAnswerOnItemReachesSessionWhole(t *testing.T) {
	text := longAnswer(2000)
	if n := utf8.RuneCountInString(text); n != 2000 || !strings.Contains(text, "\n2. F2-PART-OMEGA") {
		t.Fatalf("the answer has %d characters: %q", n, text)
	}
	respond := func(h *harness, it *WorkItem, choice string) *Instruction {
		t.Helper()
		before := map[string]bool{}
		for id := range h.logState().Instructions {
			before[id] = true
		}
		var a *EntryAnswer
		if choice != "" {
			a = &EntryAnswer{Choice: choice}
		}
		re := &EntryRe{ID: entryWork + it.ID, Kind: "work", At: it.Opened}
		if err := recordInstructionRe(h.cfg, NewLogWriter(h.buf), h.clock.Now(), []string{"widgets"}, "", "", "ui", text, re, a, nil); err != nil {
			t.Fatal(err)
		}
		return h.newInstruction(before)
	}
	// arrived checks what session name holds of instruction in.
	arrived := func(h *harness, name string, in *Instruction, it *WorkItem, items bool) {
		t.Helper()
		workdir := h.sup.cfg.Sessions[name].Workdir
		var got *InstructionDoc
		for _, d := range readInstructions(t, workdir) {
			if d.ID == in.ID {
				d := d
				got = &d
			}
		}
		if got == nil || got.Text != text {
			t.Fatalf("instructions.json of %s does not hold %s whole: %+v", name, in.ID, got)
		}
		var line string
		for _, l := range h.typed(name) {
			if strings.Contains(l, in.ID) {
				line = l
			}
		}
		if !strings.Contains(line, "2000 characters") || strings.Contains(line, "F1-PART-ALPHA") || strings.Contains(line, "F2-PART-OMEGA") || strings.Contains(line, "\n") {
			t.Fatalf("typed into %s: %q (all: %q)", name, line, h.typed(name))
		}
		if items {
			if doc := itemsDocOf(t, workdir, it.ID); !strings.Contains(doc, "F1-PART-ALPHA") || !strings.Contains(doc, "F2-PART-OMEGA") || !strings.Contains(doc, "F2-END-ZETA") {
				t.Fatalf("items.json of %s does not quote both parts: %s", name, doc)
			}
		}
	}

	t.Run("words alone", func(t *testing.T) {
		h := sessionsHarness(t)
		it := h.blockedItem()
		in := respond(h, it, "")
		h.ticks(1, time.Minute)
		arrived(h, "a", in, it, false)
	})

	t.Run("not done", func(t *testing.T) {
		h := sessionsHarness(t)
		it := h.askWork("Make the logo bigger.", ThreadWork{Title: "Bigger logo", Brief: "Make it bigger.", DoneWhen: EndCheck{Kind: DoneOwner}}, "")
		name := it.Session
		h.alive(name, "resizing")
		h.status(name, "# Status\n\n"+it.ID+" done: the logo is 2x\n")
		h.ticks(3, time.Minute)
		if it = h.item(it.ID); it.Claim == "" {
			t.Fatalf("item = %+v", it)
		}
		in := respond(h, it, WorkAnswerNotDone)
		h.alive(name, "resizing still")
		h.ticks(3, time.Minute)
		if it = h.item(it.ID); it.State != WorkWorking || it.Session != name {
			t.Fatalf("item = %+v", it)
		}
		arrived(h, name, in, it, true)
	})

	t.Run("reassign", func(t *testing.T) {
		h := sessionsHarness(t)
		h.alive("a", "a works")
		it := h.askWork("Write the release notes.", ThreadWork{Title: "Release notes", Brief: "Write them.", DoneWhen: EndCheck{Kind: DoneOwner}}, "a")
		if it.Session != "a" {
			t.Fatalf("item = %+v", it)
		}
		in := respond(h, it, WorkAnswerReassign)
		h.ticks(2, time.Minute)
		it = h.item(it.ID)
		name := it.Session
		if name == "" || name == "a" {
			t.Fatalf("not given to another session: %+v", it)
		}
		h.alive(name, "launched\n"+idleScreen)
		h.ticks(3, time.Minute)
		arrived(h, name, in, it, true)
		for _, d := range readInstructions(t, h.sup.cfg.Sessions["a"].Workdir) {
			if d.ID == in.ID {
				t.Fatalf("the words went to a, which the item was taken from")
			}
		}
	})
}

// The generic line deliverInstructions types states how many characters
// the new instructions hold, and restartOf still takes a prompt that
// starts with it for instructed.
func TestInstructionsPointerStatesLength(t *testing.T) {
	pol := DefaultConfig().Policies
	line := instructionsLine(pol.Instructions.Pointer, []*Instruction{{InstructionPayload: InstructionPayload{Text: "héllo"}}, {InstructionPayload: InstructionPayload{Text: "1.\n2."}}})
	if !strings.HasPrefix(line, pol.Instructions.Pointer) || !strings.Contains(line, "2 new, 10 characters") || strings.Contains(line, "héllo") {
		t.Fatalf("line = %q", line)
	}
	if _, why := restartOf(pol, line); why != "instructed" {
		t.Fatalf("restartOf(%q) = %q", line, why)
	}
	if l := labelPrompt(pol, "prompt", line); l.Link != "Your message delivered" {
		t.Fatalf("label = %+v", l)
	}
	if l := answeredLine(entryWork+"W1", &Instruction{InstructionPayload: InstructionPayload{ID: "I1", Text: strings.Repeat("é", 2000)}}); !strings.Contains(l, "(instruction I1, 2000 characters)") {
		t.Fatalf("answered line = %q", l)
	}
}
