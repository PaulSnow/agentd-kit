package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// decisionArgs is a raise_decision call of type typ asking q, with the
// whole decision form; extra sets or replaces arguments.
func decisionArgs(typ, q string, extra map[string]any) map[string]any {
	a := map[string]any{"type": typ, "question": q, "context": "The widget is grey today.", "why": "The style guide changed.",
		"if_wrong": "A second release; easy to undo.", "recommendation": "Take the first.", "blocking": true}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

func options(labels ...string) []any {
	var out []any
	for i, l := range labels {
		out = append(out, map[string]any{"key": string(rune('a' + i)), "label": l, "consequence": "Then " + l + "."})
	}
	return out
}

// raise makes one raise_decision call as s1 and returns the reply.
func raise(t *testing.T, h *harness, args map[string]any) (map[string]any, bool) {
	t.Helper()
	text, refused := callOne(t, h.mcpServerFor("s1"), ToolRaiseDecision, args)
	var reply map[string]any
	if err := json.Unmarshal([]byte(text), &reply); err != nil {
		t.Fatalf("reply %q: %v", text, err)
	}
	return reply, refused
}

func mustRaise(t *testing.T, h *harness, args map[string]any) string {
	t.Helper()
	reply, refused := raise(t, h, args)
	if refused {
		t.Fatalf("refused: %v", reply)
	}
	return reply["id"].(string)
}

// answersJSON is answers.json's answers, each as the compact JSON the
// session reads, by id.
func answersJSON(t *testing.T, h *harness, name string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, name, AnswersFile))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Answers []json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, a := range doc.Answers {
		var id struct{ ID string }
		json.Unmarshal(a, &id)
		out[id.ID] = string(a)
	}
	return out
}

// Each type raised through the server is answered on its own terms, and
// the session reads the exact JSON #3 names for it, with the note beside
// it. agentd numbers the decisions D1 on.
func TestRaisedDecisionTypes(t *testing.T) {
	h, _ := mcpHarness(t)
	h.alive("s1", "waiting")
	h.status("s1", working)
	raised := []map[string]any{
		decisionArgs(DecisionYesNo, "Should the widget spin faster?", nil),
		decisionArgs(DecisionChooseOne, "Which colour should the widget be?", map[string]any{"options": options("Blue", "Green")}),
		decisionArgs(DecisionChooseMany, "Which platforms should the widget ship on?", map[string]any{"options": options("Linux", "Windows", "Mac")}),
		decisionArgs(DecisionNumber, "How many spins per second?", map[string]any{"limits": map[string]any{"min": 1, "max": 10}}),
		decisionArgs(DecisionDate, "When should the widget ship?", map[string]any{"limits": map[string]any{"min": "2026-10-25", "max": "2026-12-31"}}),
		decisionArgs(DecisionText, "What should the widget's tagline say?", nil),
	}
	for i, a := range raised {
		if id := mustRaise(t, h, a); id != "D"+string(rune('1'+i)) {
			t.Fatalf("raise %d: id %s", i, id)
		}
	}
	h.ticks(2, 5*time.Minute)
	if h.state("s1") != BlockedOnOwner {
		t.Fatalf("state %s", h.state("s1"))
	}
	yes := true
	for _, a := range []AnswerPayload{
		{ID: "D1", Answer: &yes},
		{ID: "D2", Choice: "b", Note: "the guide wins"},
		{ID: "D3", Choices: []string{"a", "c"}},
		{ID: "D4", Value: json.RawMessage(`4`)},
		{ID: "D5", Value: json.RawMessage(`"2026-11-01"`)},
		{ID: "D6", Text: "It spins."},
	} {
		if err := answerAs(h, "s1", a); err != nil {
			t.Fatalf("%s: %v", a.ID, err)
		}
	}
	h.ticks(2, 5*time.Minute)
	want := map[string]string{
		"D1": `{"id":"D1","answer":true}`,
		"D2": `{"id":"D2","choice":"b","note":"the guide wins"}`,
		"D3": `{"id":"D3","choices":["a","c"]}`,
		"D4": `{"id":"D4","value":4}`,
		"D5": `{"id":"D5","value":"2026-11-01"}`,
		"D6": `{"id":"D6","text":"It spins."}`,
	}
	got := answersJSON(t, h, "s1")
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: %s, want %s", id, got[id], w)
		}
	}
	if h.state("s1") == BlockedOnOwner {
		t.Fatalf("answered: state %s", h.state("s1"))
	}
	// The case record holds the decisions; a replay rebuilds them.
	h.restart(false)
	tr := h.sup.Tracks()["s1"]
	if len(tr.Raised) != 6 || tr.waits(tr.Raised[2]) {
		t.Fatalf("replay: %+v", tr.Raised)
	}
	if c := h.logState().Cases[widgetsCase]; c == nil || len(c.Decisions) != 6 || c.Decisions[0].Session != "s1" {
		t.Fatalf("case decisions: %+v", c)
	}
}

// An answer in the owner's own words is delivered but leaves the case
// blocked; the session re-raises with options drawn from it, which
// supersedes the old decision. A question the same as an open one is
// refused.
func TestOtherLeavesCaseBlocked(t *testing.T) {
	h, _ := mcpHarness(t)
	h.alive("s1", "waiting")
	h.status("s1", working)
	q := "Which colour should the widget be?"
	id := mustRaise(t, h, decisionArgs(DecisionChooseOne, q, map[string]any{"options": options("Blue", "Green")}))
	if _, refused := raise(t, h, decisionArgs(DecisionChooseOne, " which colour should the widget be? ", map[string]any{"options": options("Red", "Green")})); !refused {
		t.Fatal("a duplicate of an open decision was accepted")
	}
	h.ticks(2, 5*time.Minute)
	if err := answerAs(h, "s1", AnswerPayload{ID: id, Other: "Neither; use the brand red."}); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, 5*time.Minute)
	if got := answersJSON(t, h, "s1")[id]; got != `{"id":"D1","other":"Neither; use the brand red."}` || h.state("s1") != BlockedOnOwner {
		t.Fatalf("other: %s, state %s", got, h.state("s1"))
	}
	if _, refused := raise(t, h, decisionArgs(DecisionChooseOne, q, map[string]any{"options": options("Red", "Blue"), "supersedes": "D9"})); !refused {
		t.Fatal("superseding an unknown decision was accepted")
	}
	next := mustRaise(t, h, decisionArgs(DecisionChooseOne, q, map[string]any{"options": options("Brand red", "Blue"), "supersedes": id}))
	if next != "D2" {
		t.Fatalf("re-raised as %s", next)
	}
	tr := h.logState().Tracks["s1"]
	if _, open := tr.decision(id); open {
		t.Fatal("the superseded decision is still open")
	}
	if err := answerAs(h, "s1", AnswerPayload{ID: id, Choice: "a"}); err == nil {
		t.Fatal("an answer to a superseded decision was recorded")
	}
	h.ticks(2, 5*time.Minute)
	if h.state("s1") != BlockedOnOwner {
		t.Fatalf("re-raised: state %s", h.state("s1"))
	}
	if err := answerAs(h, "s1", AnswerPayload{ID: next, Choice: "a"}); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, 5*time.Minute)
	if h.state("s1") == BlockedOnOwner {
		t.Fatalf("answered: state %s", h.state("s1"))
	}
}

// A question of a type wider than it needs is refused, and the refusal
// names the type to use; so is one missing its decision form.
func TestOverWideDecisionRefused(t *testing.T) {
	h, _ := mcpHarness(t)
	for name, c := range map[string]struct {
		args map[string]any
		want string
	}{
		"text naming options":   {decisionArgs(DecisionText, "Should it be (a) blue or (b) green?", nil), "choose_one"},
		"text of yes or no":     {decisionArgs(DecisionText, "Ship it today, yes or no?", nil), "yes_no"},
		"choose_one yes and no": {decisionArgs(DecisionChooseOne, "Ship it today?", map[string]any{"options": options("Yes", "No")}), "yes_no"},
		"choose_many of one":    {decisionArgs(DecisionChooseMany, "Which platforms?", map[string]any{"options": options("Linux")}), "yes_no"},
		"number with options":   {decisionArgs(DecisionNumber, "How many?", map[string]any{"options": options("One", "Two")}), "choose_one"},
		"no if_wrong":           {decisionArgs(DecisionYesNo, "Ship it?", map[string]any{"if_wrong": ""}), "if_wrong"},
		"unknown type":          {decisionArgs("maybe", "Ship it?", nil), "yes_no"},
		"bad limits":            {decisionArgs(DecisionNumber, "How many?", map[string]any{"limits": map[string]any{"min": 5, "max": 1}}), "min"},
		"default out of range":  {decisionArgs(DecisionNumber, "How many?", map[string]any{"limits": map[string]any{"min": 1, "max": 5}, "default": "9"}), "default"},
		"bad deadline":          {decisionArgs(DecisionYesNo, "Ship it?", map[string]any{"deadline": "soon"}), "deadline"},
	} {
		reply, refused := raise(t, h, c.args)
		if !refused || !strings.Contains(reply["refused"].(string), c.want) {
			t.Errorf("%s: %v, want a refusal naming %s", name, reply, c.want)
		}
	}
	if tr := h.logState().Tracks["s1"]; tr != nil && len(tr.Raised) != 0 {
		t.Fatalf("raised %+v", tr.Raised)
	}
}

// An answer is checked against its question: a key it lists, a value in
// its limits, and an answer, not only a note.
func TestOutOfRangeAnswerRefused(t *testing.T) {
	h, _ := mcpHarness(t)
	n := mustRaise(t, h, decisionArgs(DecisionNumber, "How many spins per second?", map[string]any{"limits": map[string]any{"min": 1, "max": 5}}))
	m := mustRaise(t, h, decisionArgs(DecisionChooseMany, "Which platforms?", map[string]any{"options": options("Linux", "Windows", "Mac"), "limits": map[string]any{"max": 2}}))
	d := mustRaise(t, h, decisionArgs(DecisionDate, "When should it ship?", map[string]any{"limits": map[string]any{"min": "2026-10-25", "max": "2026-12-31"}}))
	for name, a := range map[string]AnswerPayload{
		"over the max":       {ID: n, Value: json.RawMessage(`9`)},
		"not a number":       {ID: n, Value: json.RawMessage(`"four"`)},
		"unknown key":        {ID: m, Choices: []string{"a", "z"}},
		"a key twice":        {ID: m, Choices: []string{"a", "a"}},
		"too many":           {ID: m, Choices: []string{"a", "b", "c"}},
		"none chosen":        {ID: m, Choices: []string{}},
		"before the min":     {ID: d, Value: json.RawMessage(`"2026-01-01"`)},
		"not a date":         {ID: d, Value: json.RawMessage(`"next week"`)},
		"only a note":        {ID: n, Note: "later"},
		"value and other":    {ID: n, Value: json.RawMessage(`3`), Other: "or more"},
		"choice on a number": {ID: n, Choice: "a"},
	} {
		if err := answerAs(h, "s1", a); err == nil {
			t.Errorf("%s: recorded", name)
		}
	}
	for _, a := range []AnswerPayload{{ID: n, Value: json.RawMessage(`5`)}, {ID: m, Choices: []string{"c", "a"}}, {ID: d, Value: json.RawMessage(`"2026-12-31"`)}} {
		if err := answerAs(h, "s1", a); err != nil {
			t.Errorf("%s: %v", a.ID, err)
		}
	}
}

// The default is applied only once a deadline the owner set passes: the
// agent's own deadline is a suggestion, and a question with no owner
// deadline waits however long it goes unanswered.
func TestDefaultOnlyAfterOwnerDeadline(t *testing.T) {
	h, _ := mcpHarness(t)
	h.alive("s1", "waiting")
	h.status("s1", working)
	suggested := h.clock.Now().Add(time.Hour).Format(time.RFC3339)
	id := mustRaise(t, h, decisionArgs(DecisionChooseOne, "Which colour should the widget be?",
		map[string]any{"options": options("Blue", "Green"), "default": "a", "deadline": suggested}))
	other := mustRaise(t, h, decisionArgs(DecisionNumber, "How many spins per second?", map[string]any{"default": "3"}))
	h.ticks(6, time.Hour)
	tr := h.sup.Tracks()["s1"]
	if len(tr.Answers) != 0 || h.state("s1") != BlockedOnOwner {
		t.Fatalf("no owner deadline: answers %+v, state %s", tr.Answers, h.state("s1"))
	}
	st := NewLogState(h.cfg, ModeLive).Fold(h.events())
	if err := RecordDeadline(NewLogWriter(h.buf), st, h.clock.Now(), "s1", id, h.clock.Now().Add(-time.Minute)); err == nil {
		t.Fatal("a deadline in the past was recorded")
	}
	if err := RecordDeadline(NewLogWriter(h.buf), st, h.clock.Now(), "s1", id, h.clock.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, 30*time.Minute)
	if _, ok := h.sup.Tracks()["s1"].Answers[id]; ok {
		t.Fatal("the default was applied before the owner's deadline")
	}
	h.ticks(3, 30*time.Minute)
	got := answersJSON(t, h, "s1")
	if got[id] != `{"id":"D1","choice":"a","note":"`+defaultNote+`"}` || got[other] != "" {
		t.Fatalf("answers %v", got)
	}
	if a := h.sup.Tracks()["s1"].Answers[id]; !a.ByDefault || !a.Delivered {
		t.Fatalf("answer %+v", a)
	}
	// D2 has no owner deadline: still open, still blocking.
	if h.state("s1") != BlockedOnOwner {
		t.Fatalf("state %s", h.state("s1"))
	}
	// A question with no default takes no deadline.
	text := mustRaise(t, h, decisionArgs(DecisionText, "What should the tagline say?", nil))
	st = NewLogState(h.cfg, ModeLive).Fold(h.events())
	if err := RecordDeadline(NewLogWriter(h.buf), st, h.clock.Now(), "s1", text, h.clock.Now().Add(time.Hour)); err == nil {
		t.Fatal("a deadline on a question with no default was recorded")
	}
}

// Shadow mode records the default it would apply and applies nothing.
func TestShadowDefaultOnlyRecords(t *testing.T) {
	h, _ := mcpHarness(t)
	h.restart(true)
	h.alive("s1", "waiting")
	h.status("s1", working)
	id := mustRaise(t, h, decisionArgs(DecisionYesNo, "Should the widget spin faster?", map[string]any{"default": "no"}))
	st := NewLogState(h.cfg, ModeShadow).Fold(h.events())
	if err := RecordDeadline(NewLogWriter(h.buf), st, h.clock.Now(), "s1", id, h.clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	h.ticks(4, time.Hour)
	if _, ok := readAnswers(t, h, "s1"); ok {
		t.Fatal("shadow wrote answers.json")
	}
	if a := NewLogState(h.cfg, ModeShadow).Fold(h.events()).Tracks["s1"].Answers[id]; !a.ByDefault {
		t.Fatalf("the shadow recorded no default: %+v", a)
	}
	if live := NewLogState(h.cfg, ModeLive).Fold(h.events()).Tracks["s1"]; live != nil && len(live.Answers) != 0 {
		t.Fatalf("the live run sees the shadow's default: %+v", live.Answers)
	}
}

// questions.json may not take the ids agentd numbers decisions and
// directives with.
func TestQuestionsFileReservedIDs(t *testing.T) {
	for _, id := range []string{"D1", "R2", "D10"} {
		ds := []Decision{{ID: id, Type: DecisionYesNo, Question: "q", Context: "c", Why: "w", IfWrong: "i", Recommendation: "r"}}
		if err := checkDecisions(ds); err == nil {
			t.Errorf("%s accepted", id)
		}
	}
}

// The page shows the decision form above each type's controls, the
// agent's suggested deadline, and a deadline field for a question with a
// default.
func TestPageShowsDecisionTypes(t *testing.T) {
	h, _ := mcpHarness(t)
	mustRaise(t, h, decisionArgs(DecisionChooseMany, "Which platforms should it ship on?", map[string]any{"options": options("Linux", "Mac"), "default": "a,b",
		"deadline": "2026-11-01T12:00:00Z"}))
	mustRaise(t, h, decisionArgs(DecisionNumber, "How many spins per second?", map[string]any{"limits": map[string]any{"min": 1, "max": 5}}))
	mustRaise(t, h, decisionArgs(DecisionDate, "When should it ship?", nil))
	body := renderPageFor(t, h)
	for _, want := range []string{`type="checkbox" name="choices" value="a"`, `type="number" name="value" min="1" max="5"`, `type="date" name="value"`,
		`action="/deadline"`, "suggests 2026-11-01", "If wrong: A second release"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Index(body, "If wrong: A second release") > strings.Index(body, `name="choices"`) {
		t.Error("the controls come before the decision form")
	}
}

// renderPageFor is the page as the owner sees it, with the harness's
// decisions.
func renderPageFor(t *testing.T, h *harness) string {
	t.Helper()
	data := testPageData()
	rep := Report{Repositories: []ReportRepo{{Name: "widgets", Cases: []ReportCase{{Session: "s1"}}}}}
	data.Decisions = DecisionsFrom(h.cfg, h.logState(), rep)
	p, err := NewPage(PageConfig{Address: pageAddr, Whois: []string{"whois", "{addr}"}, Owner: "owner-a"},
		&fakeWhois{logins: map[string]string{"192.0.2.20": "owner-a"}}, func() (PageData, error) { return data, nil },
		func([]string, string) error { return nil }, func(string, AnswerPayload) error { return nil }, func() time.Time { return pageAt })
	if err != nil {
		t.Fatal(err)
	}
	w := get(p, "GET", "/", pageAddr, "192.0.2.20:51000")
	if w.Code != 200 {
		t.Fatalf("page: %d", w.Code)
	}
	return w.Body.String()
}

// The page's forms carry a choose_many's keys and a number's or a date's
// value as the answer JSON, and the owner's deadline to the log; a
// deadline it cannot read is refused.
func TestPageAnswerTypesAndDeadline(t *testing.T) {
	p, _, marks := testPageMarks(t, PageConfig{})
	var set []string
	p.deadline = func(session, id string, at time.Time) error {
		set = append(set, session+" "+id+" "+at.UTC().Format(time.RFC3339))
		return nil
	}
	origin := "http://" + pageAddr
	for _, form := range []url.Values{
		{"session": {"w17"}, "id": {"D3"}, "choices": {"a", "c"}},
		{"session": {"w17"}, "id": {"D4"}, "value": {"4"}},
		{"session": {"w17"}, "id": {"D5"}, "value": {"2026-11-01"}},
	} {
		if w := postTo(p, "/answer", origin, p.token, form, "192.0.2.20:51000"); w.Code != http.StatusSeeOther {
			t.Fatalf("%v: %d %s", form, w.Code, w.Body.String())
		}
	}
	var got []string
	for _, a := range marks.answers {
		b, _ := json.Marshal(AnswerPayload{ID: a.ID, Choices: a.Choices, Value: a.Value})
		got = append(got, string(b))
	}
	if want := []string{`{"id":"D3","choices":["a","c"]}`, `{"id":"D4","value":4}`, `{"id":"D5","value":"2026-11-01"}`}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("answers %v", got)
	}
	if w := postTo(p, "/deadline", origin, p.token, url.Values{"session": {"w17"}, "id": {"D1"}, "deadline": {"2026-11-01T12:00:00Z"}}, "192.0.2.20:51000"); w.Code != http.StatusSeeOther {
		t.Fatalf("deadline: %d %s", w.Code, w.Body.String())
	}
	if w := postTo(p, "/deadline", origin, p.token, url.Values{"session": {"w17"}, "id": {"D1"}, "deadline": {"soon"}}, "192.0.2.20:51000"); w.Code != http.StatusBadRequest {
		t.Fatalf("unreadable deadline: %d", w.Code)
	}
	if w := postTo(p, "/deadline", "", p.token, url.Values{"session": {"w17"}, "id": {"D1"}, "deadline": {"2026-11-01T12:00:00Z"}}, "192.0.2.20:51000"); w.Code != http.StatusForbidden {
		t.Fatalf("no origin: %d", w.Code)
	}
	if len(set) != 1 || set[0] != "w17 D1 2026-11-01T12:00:00Z" {
		t.Fatalf("deadlines %v", set)
	}
}

// Review round 1: a default logged after an owner's answer to the same
// question does not replace it; ordinary numbered sentences are not taken
// for options; a refusal logs none of the session's values; a decision
// already settled cannot be superseded.
func TestDecisionReview1(t *testing.T) {
	d := Decision{ID: "D1", Type: DecisionYesNo, Question: "q", Default: "yes"}
	no, yes := false, true
	cfg := DefaultConfig()
	st := NewLogState(&cfg, ModeLive)
	st.Fold([]Event{
		{V: EventVersion, Time: pageAt, Kind: KindAnswer, Mode: ModeOwner, Session: "s1", Answer: &AnswerPayload{ID: "D1", Answer: &no, Asked: decisionHash(d)}},
		{V: EventVersion, Time: pageAt.Add(time.Second), Kind: KindAnswer, Mode: ModeLive, Session: "s1", Answer: &AnswerPayload{ID: "D1", Answer: &yes, Asked: decisionHash(d), ByDefault: true, Note: defaultNote}},
	})
	if a := st.Tracks["s1"].Answers["D1"]; a.ByDefault || *a.Answer {
		t.Fatalf("the default replaced the owner's answer: %+v", a)
	}

	for _, q := range []string{"The retry count is 3. The timeout is 5. What should the log line say?", "Phase 1. shipped, phase 2. is pending; what is phase 3 called?",
		"See section 4) and appendix b); what title fits?"} {
		if err := checkDecision(Decision{ID: "D1", Type: DecisionText, Question: q, Context: "c", Why: "w", IfWrong: "i", Recommendation: "r"}); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}

	h, _ := mcpHarness(t)
	if _, refused := raise(t, h, decisionArgs("secret-type-value", "Ship it?", nil)); !refused {
		t.Fatal("unknown type accepted")
	}
	if strings.Contains(h.buf.String(), "secret-type-value") {
		t.Fatal("the refusal logged the session's value")
	}
	id := mustRaise(t, h, decisionArgs(DecisionChooseOne, "Which colour?", map[string]any{"options": options("Blue", "Green")}))
	if err := answerAs(h, "s1", AnswerPayload{ID: id, Choice: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, refused := raise(t, h, decisionArgs(DecisionYesNo, "Ship it?", map[string]any{"supersedes": id})); !refused {
		t.Fatal("a settled decision was superseded")
	}
}

// Review round 2: a withheld decision keeps neither its default nor its
// limits; markers followed by punctuation still make a list; limits are
// bounded.
func TestDecisionReview2(t *testing.T) {
	w := withholdDecision(Decision{ID: "D1", Type: DecisionNumber, Default: "4", Limits: &DecisionLimits{Min: json.RawMessage(`1`)}})
	if w.Default != "" || w.Limits != nil || !w.Withheld {
		t.Fatalf("withheld %+v", w)
	}
	for _, q := range []string{"Which: (a), (b), or (c)?", "Use (a): rebase or (b): merge?", "Pick (a) (b)"} {
		if err := checkDecision(Decision{ID: "D1", Type: DecisionText, Question: q, Context: "c", Why: "w", IfWrong: "i", Recommendation: "r"}); err == nil {
			t.Errorf("%q accepted as text", q)
		}
	}
	long := json.RawMessage("0." + strings.Repeat("0", 60) + "1")
	if err := checkDecision(Decision{ID: "D1", Type: DecisionNumber, Question: "q", Context: "c", Why: "w", IfWrong: "i", Recommendation: "r",
		Limits: &DecisionLimits{Min: long}}); err == nil {
		t.Fatal("a 63-character limit was accepted")
	}
}
