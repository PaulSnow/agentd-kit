package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// answerAs records the owner's answer as the page does: checked against
// the state of the run's mode folded from the log so far.
func answerAs(h *harness, session string, a AnswerPayload) error {
	st := NewLogState(h.cfg, mode(h.sup.shadow))
	st.Fold(h.events())
	return RecordAnswer(NewLogWriter(h.buf), st, h.clock.Now(), session, a)
}

func readAnswers(t *testing.T, h *harness, name string) (answersDoc, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, name, AnswersFile))
	if os.IsNotExist(err) {
		return answersDoc{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var doc answersDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%s: %v", AnswersFile, err)
	}
	return doc, true
}

func writeQuestions(t *testing.T, h *harness, name, doc string) {
	t.Helper()
	writeFile(t, filepath.Join(h.dir, name, QuestionsFile), doc)
}

func statusID(text string) string {
	return statusDecision + shortHash(HashPane(ParseStatus(text).Questions))
}

const chooseColour = `{"decisions":[{"id":"D1","type":"choose_one","question":"Which colour should the widget be?",` +
	`"context":"The widget is grey today.","why":"The style guide changed.","if_wrong":"A second release; easy to undo.",` +
	`"recommendation":"b","options":[{"key":"a","label":"Blue","consequence":"Matches the old logo."},` +
	`{"key":"b","label":"Green","consequence":"Matches the new guide."}],"blocking":true}]}`

// An answer to STATUS.md's Questions round-trips: the owner's answer is an
// owner event, agentd writes it to answers.json and types the fixed
// pointer line once, never the answer, and the session is no longer
// blocked on those questions. A replay neither writes nor types again.
func TestAnswerRoundTrips(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.alive("s1", "waiting")
	h.status("s1", question(1))
	h.ticks(2, 5*time.Minute)
	if h.state("s1") != BlockedOnOwner {
		t.Fatalf("state %s", h.state("s1"))
	}
	id := statusID(question(1))
	if err := answerAs(h, "s1", AnswerPayload{ID: id, Text: "Version 3 gates them.", Note: "ask again if unsure"}); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, 5*time.Minute)
	doc, ok := readAnswers(t, h, "s1")
	if !ok || len(doc.Answers) != 1 || doc.Answers[0].ID != id || doc.Answers[0].Text != "Version 3 gates them." || doc.Answers[0].Note != "ask again if unsure" {
		t.Fatalf("answers.json = %+v", doc)
	}
	pointer := h.cfg.Policies.Answers.Pointer
	var types []string
	for _, c := range h.tmux.calls {
		if c.op == "type" {
			types = append(types, c.args[0])
		}
	}
	if len(types) != 1 || types[0] != pointer {
		t.Fatalf("typed %q, want the pointer once", types)
	}
	if h.state("s1") == BlockedOnOwner || h.count(KindAction, ActAnswer, "s1") != 1 {
		t.Fatalf("state %s, %d answer actions", h.state("s1"), h.count(KindAction, ActAnswer, "s1"))
	}
	h.restart(false)
	h.ticks(3, 5*time.Minute)
	if n := h.tmux.count("type", "s1"); n != 1 {
		t.Fatalf("typed %d times after a replay", n)
	}
	if !h.sup.Tracks()["s1"].Answers[id].Delivered {
		t.Fatal("replay lost the delivery")
	}
	// Reworded questions are new: they block again until answered.
	h.status("s1", question(2))
	h.ticks(2, 5*time.Minute)
	if h.state("s1") != BlockedOnOwner {
		t.Fatalf("reworded questions: state %s", h.state("s1"))
	}
}

// Shadow mode records the answer and the delivery it would make, once,
// and writes and types nothing.
func TestShadowAnswerOnlyRecords(t *testing.T) {
	h := newHarness(t, true, "s1")
	h.alive("s1", "waiting")
	h.status("s1", question(1))
	h.ticks(2, 5*time.Minute)
	if err := answerAs(h, "s1", AnswerPayload{ID: statusID(question(1)), Text: "Yes."}); err != nil {
		t.Fatal(err)
	}
	h.ticks(4, 5*time.Minute)
	if _, ok := readAnswers(t, h, "s1"); ok || typed(h, "s1") != 0 {
		t.Fatalf("shadow wrote or typed: %d typed", typed(h, "s1"))
	}
	if n := h.count(KindWould, ActAnswer, "s1"); n != 1 {
		t.Fatalf("%d would-be answers, want 1", n)
	}
}

// Decisions raised in questions.json block until answered; the answer
// arrives in the JSON #3 names. An answer in the owner's own words is
// delivered but does not settle the decision. A replay rebuilds the open
// and answered decisions.
func TestDecisionsFile(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.alive("s1", "waiting")
	h.status("s1", working)
	writeQuestions(t, h, "s1", chooseColour)
	h.ticks(2, 5*time.Minute)
	if h.state("s1") != BlockedOnOwner || h.count(KindDecision, "", "s1") != 1 {
		t.Fatalf("state %s, %d decision events", h.state("s1"), h.count(KindDecision, "", "s1"))
	}
	if msg := h.runner.calls[0][4]; !strings.Contains(msg, "Which colour should the widget be?") {
		t.Fatalf("escalation %q", msg)
	}
	if err := answerAs(h, "s1", AnswerPayload{ID: "D1", Other: "Neither; use the brand red."}); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, 5*time.Minute)
	if doc, _ := readAnswers(t, h, "s1"); len(doc.Answers) != 1 || doc.Answers[0].Other == "" || h.state("s1") != BlockedOnOwner {
		t.Fatalf("other: %+v, state %s", doc, h.state("s1"))
	}
	if err := answerAs(h, "s1", AnswerPayload{ID: "D1", Choice: "b"}); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, 5*time.Minute)
	b, _ := os.ReadFile(filepath.Join(h.dir, "s1", AnswersFile))
	if string(b) != `{"answers":[{"id":"D1","choice":"b"}]}`+"\n" || h.state("s1") == BlockedOnOwner {
		t.Fatalf("answers.json %s, state %s", b, h.state("s1"))
	}
	before := h.sup.Tracks()["s1"]
	h.restart(false)
	after := h.sup.Tracks()["s1"]
	if len(after.Decisions) != 1 || after.Decisions[0].ID != "D1" || !after.settled("D1") || !after.Answers["D1"].Delivered ||
		after.DecisionsHash != before.DecisionsHash {
		t.Fatalf("replay: %+v", after)
	}
	if n := h.tmux.count("type", "s1"); n != 2 {
		t.Fatalf("typed %d times, want 2", n)
	}
}

// A refused questions.json is logged once, with why, and the decisions
// read before it stand.
func TestRefusedDecisionsFile(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.alive("s1", "waiting")
	h.status("s1", working)
	writeQuestions(t, h, "s1", chooseColour)
	h.ticks(1, 5*time.Minute)
	writeQuestions(t, h, "s1", `{"decisions":[{"id":"D2","type":"text","question":"Pick a or b?"}]}`)
	h.ticks(3, 5*time.Minute)
	var refused []string
	for _, e := range h.events() {
		if e.Kind == KindDecision && e.Decision.Refused != "" {
			refused = append(refused, e.Decision.Refused)
		}
	}
	if len(refused) != 1 || !strings.Contains(refused[0], "D2") {
		t.Fatalf("refusals %q", refused)
	}
	if ds := h.sup.Tracks()["s1"].Decisions; len(ds) != 1 || ds[0].ID != "D1" {
		t.Fatalf("decisions %+v", ds)
	}
}

// A session gone with open Questions is parked, not restarted; once the
// owner answers, it is restarted with a prompt naming answers.json, which
// is written first.
func TestDeadBlockedSessionRestartsWithAnswers(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", question(1))
	h.ticks(3, 5*time.Minute)
	if h.state("s1") != BlockedOnOwner || h.tmux.count("new-session", "s1") != 0 {
		t.Fatalf("state %s, %d starts", h.state("s1"), h.tmux.count("new-session", "s1"))
	}
	if err := answerAs(h, "s1", AnswerPayload{ID: statusID(question(1)), Text: "Use version 3."}); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, 5*time.Minute)
	var cmd string
	for _, c := range h.tmux.calls {
		if c.op == "new-session" {
			cmd = c.args[1]
		}
	}
	file := filepath.Join(h.dir, "s1", AnswersFile)
	if !strings.Contains(cmd, `"why":"answered"`) || !strings.Contains(cmd, file) {
		t.Fatalf("restart command %q", cmd)
	}
	if doc, ok := readAnswers(t, h, "s1"); !ok || doc.Answers[0].Text != "Use version 3." {
		t.Fatalf("answers.json %+v", doc)
	}
	if !h.sup.Tracks()["s1"].Answers[statusID(question(1))].Delivered || typed(h, "s1") != 0 {
		t.Fatal("the restart did not deliver the answer, or typed it")
	}
}

// An answer is checked against its decision: of its type, one of its
// options, one answer or other but not both; an unknown or closed
// decision records nothing.
func TestAnswerIsChecked(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.alive("s1", "waiting")
	h.status("s1", working)
	writeQuestions(t, h, "s1", chooseColour)
	h.ticks(1, 5*time.Minute)
	yes := true
	for name, a := range map[string]AnswerPayload{
		"unknown decision":  {ID: "D9", Choice: "a"},
		"not an option":     {ID: "D1", Choice: "c"},
		"wrong type":        {ID: "D1", Answer: &yes},
		"both":              {ID: "D1", Choice: "a", Other: "or not"},
		"none":              {ID: "D1", Note: "only a note"},
		"status, none open": {ID: statusDecision + "abc", Text: "x"},
		"long note":         {ID: "D1", Choice: "a", Note: strings.Repeat("n", maxNote+1)},
	} {
		if err := answerAs(h, "s1", a); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := answerAs(h, "nobody", AnswerPayload{ID: "D1", Choice: "a"}); err == nil {
		t.Error("an unknown session accepted")
	}
	if n := h.count(KindAnswer, "", ""); n != 0 {
		t.Fatalf("%d answers recorded", n)
	}
}

// What a session may raise is checked: plain unique ids, the decision form
// filled in, and the narrowest type.
func TestDecisionsAreChecked(t *testing.T) {
	var ok QuestionsDoc
	if err := json.Unmarshal([]byte(chooseColour), &ok); err != nil {
		t.Fatal(err)
	}
	if err := checkDecisions(ok.Decisions); err != nil {
		t.Fatal(err)
	}
	base := ok.Decisions[0]
	bad := map[string]func(d *Decision){
		"id":             func(d *Decision) { d.ID = "has space" },
		"status id":      func(d *Decision) { d.ID = statusDecision + "x" },
		"no question":    func(d *Decision) { d.Question = " " },
		"no if_wrong":    func(d *Decision) { d.IfWrong = "" },
		"long question":  func(d *Decision) { d.Question = strings.Repeat("q", maxQuestion+1) },
		"type":           func(d *Decision) { d.Type = "number" },
		"yes and no":     func(d *Decision) { d.Options[0].Label, d.Options[1].Label = "Yes", "No" },
		"one option":     func(d *Decision) { d.Options = d.Options[:1] },
		"key":            func(d *Decision) { d.Options[1].Key = "a" },
		"no consequence": func(d *Decision) { d.Options[0].Consequence = "" },
		"default":        func(d *Decision) { d.Default = "z" },
		"text options":   func(d *Decision) { d.Type = DecisionText },
		"yes_no options": func(d *Decision) { d.Type = DecisionYesNo },
	}
	for name, change := range bad {
		d := base
		d.Options = append([]DecisionOption(nil), base.Options...)
		change(&d)
		if checkDecisions([]Decision{d}) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if checkDecisions([]Decision{base, base}) == nil {
		t.Error("a duplicate id accepted")
	}
}

// The pointer line is one non-empty line.
func TestAnswersPointerIsChecked(t *testing.T) {
	for _, p := range []string{"", "two\nlines"} {
		cfg := DefaultConfig()
		cfg.Policies.Answers.Pointer = p
		if err := cfg.finish(t.TempDir()); err == nil || !strings.Contains(err.Error(), "answers.pointer") {
			t.Errorf("pointer %q: %v", p, err)
		}
	}
}
