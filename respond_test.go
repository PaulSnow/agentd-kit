package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// yesNo is a decision that does not block, so its session works on and
// takes the owner's messages while it waits.
func yesNo(id, question string) string {
	return fmt.Sprintf(`{"id":%q,"type":"yes_no","question":%q,"context":"The paint is dry.","why":"It is ready.",`+
		`"if_wrong":"Undo it.","recommendation":"yes","blocking":false}`, id, question)
}

// respondFixture is three working sessions of widgets with five open
// entries between them, raised in this order: a's Q1, b's Q1, c's Q1,
// a's Q2, and b's post. It returns the UI over the harness's log and the
// entry ids, in that order.
func respondFixture(t *testing.T) (*harness, *UI, []string) {
	t.Helper()
	h := newHarness(t, false, "a", "b", "c")
	for i := 0; i < 2; i++ {
		for _, n := range []string{"a", "b", "c"} {
			h.alive(n, fmt.Sprintf("%s works, screen %d", n, i))
		}
		h.ticks(1, time.Minute)
	}
	raise := func(name string, ds ...string) {
		doc := `{"decisions":[`
		for i, d := range ds {
			if i > 0 {
				doc += ","
			}
			doc += d
		}
		writeQuestions(t, h, name, doc+"]}")
	}
	work := func(i int) {
		for _, n := range []string{"a", "b", "c"} {
			h.alive(n, fmt.Sprintf("%s works, screen %d", n, i))
		}
		h.ticks(1, time.Minute)
	}
	raise("a", yesNo("Q1", "Ship the blue widgets?"))
	work(2)
	raise("b", yesNo("Q1", "Delete the old paint branch?"))
	work(3)
	raise("c", yesNo("Q1", "Merge the follow-up now?"))
	work(4)
	raise("a", yesNo("Q1", "Ship the blue widgets?"), yesNo("Q2", "Rename the gadget repository?"))
	work(5)
	if err := PostMessage(h.cfg, NewLogWriter(h.buf), h.tmux, h.clock.Now(), MessagePost{Console: "widgets", From: "b", Text: "I paused the second coat: the primer is still wet."}); err != nil {
		t.Fatal(err)
	}
	var post string
	for _, e := range h.events() {
		if e.Kind == KindMessage && e.Message.From == "b" {
			post = entryKey(entryMessage, e.Message.ID, e.Time, e.Message.Text)
		}
	}
	for _, n := range []string{"a", "b", "c"} {
		if h.state(n) != Working {
			t.Fatalf("%s is %s", n, h.state(n))
		}
	}
	u, _ := testUI(t, UIConfig{}, newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a"))
	u.now = h.clock.Now
	return h, u, []string{decisionEntry("a", "Q1"), decisionEntry("b", "Q1"), decisionEntry("c", "Q1"), decisionEntry("a", "Q2"), post}
}

// A response is bound to the entry it was given on (#59): with five
// entries open from three sessions, "yes" to the third settles that
// decision only, and a comment on the fifth goes to the session that
// wrote it. Each session receives only its own records, each carrying
// the entry Go filled in from the log.
func TestResponseBoundToItsEntry(t *testing.T) {
	h, u, entries := respondFixture(t)
	respond := func(form url.Values) {
		t.Helper()
		form.Set("src", "night")
		form.Set("repository", "widgets")
		form.Set("back", "/console/widgets")
		if w := ownerPost(u, "/respond", form); w.Code != http.StatusSeeOther {
			t.Fatalf("respond %v: %d %s", form, w.Code, w.Body.String())
		}
	}
	respond(url.Values{"entry": {entries[2]}, "answer": {"yes"}, "text": {"but only after CI is green"}})
	respond(url.Values{"entry": {entries[4]}, "text": {"Explain: why is it still wet?"}})
	// An entry the console does not hold, and an answer to what is no
	// decision, are refused, and record nothing.
	for _, form := range []url.Values{{"entry": {"m:nothing"}, "text": {"yes"}}, {"entry": {entries[4]}, "answer": {"yes"}}} {
		form.Set("src", "night")
		form.Set("repository", "widgets")
		if w := ownerPost(u, "/respond", form); w.Code != http.StatusBadRequest {
			t.Fatalf("%v: %d", form, w.Code)
		}
	}
	for i := 6; i < 8; i++ {
		for _, n := range []string{"a", "b", "c"} {
			h.alive(n, fmt.Sprintf("%s works, screen %d", n, i))
		}
		h.ticks(1, time.Minute)
	}
	tr := h.sup.Tracks()
	if a, ok := tr["c"].answer("Q1"); !ok || a.Answer == nil || !*a.Answer || a.Note != "but only after CI is green" {
		t.Fatalf("c's Q1 answer: %+v, %v", a, ok)
	}
	for _, open := range []struct{ session, id string }{{"a", "Q1"}, {"b", "Q1"}, {"a", "Q2"}} {
		if _, ok := tr[open.session].answer(open.id); ok {
			t.Fatalf("%s's %s was answered too", open.session, open.id)
		}
	}
	if _, err := os.Stat(filepath.Join(h.cfg.Sessions["a"].Workdir, InstructionsFile)); !os.IsNotExist(err) {
		t.Fatalf("a was handed instructions: %v", err)
	}
	record := func(name string) map[string]any {
		t.Helper()
		docs := readInstructions(t, h.cfg.Sessions[name].Workdir)
		if len(docs) != 1 {
			t.Fatalf("%s holds %d records", name, len(docs))
		}
		b, _ := json.Marshal(docs[0])
		var m map[string]any
		json.Unmarshal(b, &m)
		delete(m, "id")
		delete(m, "at")
		if re, ok := m["re"].(map[string]any); ok {
			delete(re, "at")
		}
		return m
	}
	c, _ := json.Marshal(record("c"))
	if want := `{"answer":{"answer":true},"from":"ui","new":true,"re":{"decision":"Q1","id":"` + entries[2] +
		`","kind":"decision","options":["yes","no"],"question":"Merge the follow-up now?","session":"c","text":"The paint is dry."},"text":"but only after CI is green"}`; string(c) != want {
		t.Fatalf("c receives\n%s\nwant\n%s", c, want)
	}
	b, _ := json.Marshal(record("b"))
	if want := `{"from":"ui","new":true,"re":{"id":"` + entries[4] + `","kind":"info","session":"b","text":"I paused the second coat: the primer is still wet."},"text":"Explain: why is it still wet?"}`; string(b) != want {
		t.Fatalf("b receives\n%s\nwant\n%s", b, want)
	}
	if got := h.typed("c"); len(got) == 0 || got[len(got)-1] != h.cfg.Policies.Instructions.Pointer {
		t.Fatalf("typed into c: %q", got)
	}
}

// The box's JSON (#59 §2) holds each entry as the page renders it, newest
// last: a response threaded under the entry it answers with the session's
// reply under it, an answered decision with its answer, and only the
// decisions still open listed as open.
func TestBoxThreadsResponsesUnderTheirEntries(t *testing.T) {
	h, u, entries := respondFixture(t)
	for _, form := range []url.Values{{"entry": {entries[2]}, "answer": {"yes"}}, {"entry": {entries[4]}, "quick": {"Explain"}, "text": {"why is it still wet?"}}} {
		form.Set("src", "night")
		form.Set("repository", "widgets")
		if w := ownerPost(u, "/respond", form); w.Code != http.StatusSeeOther {
			t.Fatalf("respond %v: %d %s", form, w.Code, w.Body.String())
		}
	}
	for i := 6; i < 8; i++ {
		for _, n := range []string{"a", "b", "c"} {
			h.alive(n, fmt.Sprintf("%s works, screen %d", n, i))
		}
		h.ticks(1, time.Minute)
	}
	at := h.clock.Now()
	for _, tn := range []TurnPayload{
		{Transcript: "t9", Line: 1, Role: TurnPrompt, Text: h.cfg.Policies.Instructions.Pointer, At: at},
		{Transcript: "t9", Line: 2, Role: TurnSummary, Text: "The primer needs four hours; it was laid at noon.", At: at},
	} {
		tn := tn
		if err := h.sup.log.Append(Event{V: EventVersion, Time: at, Session: "b", Kind: KindTurn, Mode: ModeLive, Turn: &tn}); err != nil {
			t.Fatal(err)
		}
	}
	w := get(u, "GET", "/console/widgets/entries", pageAddr, "192.0.2.20:51000")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("entries: %d %s", w.Code, w.Body.String())
	}
	var doc uiEntriesDoc
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	html := map[string]string{}
	var order []string
	for _, e := range doc.Entries {
		if e.Hash == "" || e.ID == "" {
			t.Fatalf("entry %+v", e)
		}
		var entry string
		if i := strings.Index(e.HTML, `data-entry="`); i >= 0 {
			entry = e.HTML[i+len(`data-entry="`):]
			entry = entry[:strings.Index(entry, `"`)]
		}
		html[entry] = e.HTML
		order = append(order, entry)
	}
	if fmt.Sprint(order) != fmt.Sprint(entries) {
		t.Fatalf("entries in order %q, want %q", order, entries)
	}
	post := html[entries[4]]
	reply, response := strings.Index(post, "The primer needs four hours"), strings.Index(post, "Explain. why is it still wet?")
	if response < 0 || reply < response || !strings.Contains(post, `class="responses"`) {
		t.Fatalf("the response and its reply are not threaded under the post:\n%s", post)
	}
	if !strings.Contains(html[entries[2]], "Answered: <strong>yes</strong>") || strings.Contains(html[entries[2]], `name="answer"`) {
		t.Fatalf("the answered decision:\n%s", html[entries[2]])
	}
	if !strings.Contains(html[entries[0]], `<button type="submit" name="answer" value="yes">Yes</button>`) {
		t.Fatalf("an open decision offers no options:\n%s", html[entries[0]])
	}
	if len(doc.Open) != 3 || !strings.Contains(doc.Chips, `id="chips"`) {
		t.Fatalf("open %q, chips %q", doc.Open, doc.Chips)
	}
}

// Words about a decision already answered leave its answer as it is and
// reach its session; another answer to it is refused. Words the owner
// adds to an answer go into it only when they may be logged and fit; a
// long or confidential comment still goes, screened, as the response.
// A session waiting on the owner takes a response too (#59 review).
func TestResponseEdges(t *testing.T) {
	h, u, entries := respondFixture(t)
	h.cfg.ConfidentialPatterns = []string{`hunter2`}
	u, _ = testUI(t, UIConfig{}, newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a"))
	u.now = h.clock.Now
	post := func(form url.Values) int {
		t.Helper()
		form.Set("src", "night")
		form.Set("repository", "widgets")
		return ownerPost(u, "/respond", form).Code
	}
	if c := post(url.Values{"entry": {entries[2]}, "answer": {"yes"}, "text": {strings.Repeat("Only after CI is green. ", 40)}}); c != http.StatusSeeOther {
		t.Fatalf("a yes with a long comment: %d", c)
	}
	if c := post(url.Values{"entry": {entries[2]}, "text": {"Thanks."}}); c != http.StatusSeeOther {
		t.Fatalf("words about an answered decision: %d", c)
	}
	if c := post(url.Values{"entry": {entries[2]}, "answer": {"no"}}); c != http.StatusBadRequest {
		t.Fatalf("a second answer: %d", c)
	}
	if c := post(url.Values{"entry": {entries[0]}, "answer": {"yes"}, "text": {"the password is hunter2"}}); c != http.StatusSeeOther {
		t.Fatalf("a yes with a confidential comment: %d", c)
	}
	st := h.logState()
	if a := st.Tracks["c"].Answers["Q1"]; a.Answer == nil || !*a.Answer || a.Note != "" || a.Other != "" {
		t.Fatalf("c's answer after the words about it: %+v", a)
	}
	if a := st.Tracks["a"].Answers["Q1"]; a.Answer == nil || !*a.Answer || a.Note != "" {
		t.Fatalf("a's answer: %+v", a)
	}
	if strings.Contains(h.buf.String(), "hunter2") {
		t.Fatal("the confidential comment was logged")
	}
	var words []string
	for _, in := range st.Instructions {
		if in.Re != nil && in.Re.ID == entries[2] && in.Answer == nil {
			words = append(words, in.Text)
		}
	}
	if fmt.Sprint(words) != "[Thanks.]" {
		t.Fatalf("words about c's answered decision: %q", words)
	}
	// A need: b is stopped by the owner, and a response to it is bound.
	if err := h.sup.log.Append(Event{V: EventVersion, Time: h.clock.Now(), Session: "b", Kind: KindTransition, Mode: ModeLive,
		Transition: &TransitionPayload{From: string(Working), To: string(UsageLimited), Reason: "usage limit until 14:00"}}); err != nil {
		t.Fatal(err)
	}
	if c := post(url.Values{"entry": {needEntry("b", "usage-limited")}, "quick": {"Go ahead"}}); c != http.StatusSeeOther {
		t.Fatalf("a response to a need: %d", c)
	}
	found := false
	for _, in := range h.logState().Instructions {
		found = found || in.Re != nil && in.Re.Kind == "need" && in.To == "b" && in.Text == "Go ahead"
	}
	if !found {
		t.Fatal("the response to the need is not bound to it")
	}
}

// The page and each poll carry the latest consoleWindow entries; older
// ones come before them on asking, until none is left. A session's
// status lines within minutes of each other are one entry, its latest,
// with the earlier ones folded under it; its progress reports are
// entries too; an instruction's tools are one line (#59 §4).
func TestConsoleWindowAndCompactEntries(t *testing.T) {
	h, u, _ := respondFixture(t)
	log := NewLogWriter(h.buf)
	at := h.clock.Now()
	for i := 0; i < 120; i++ {
		if err := log.Append(Event{V: EventVersion, Time: at.Add(time.Duration(i) * time.Hour), Kind: KindMessage, Mode: ModeShared,
			Message: &MessagePayload{ID: fmt.Sprintf("M%03d", i), Console: "widgets", Kind: MessageReply, From: "a", Text: fmt.Sprintf("Coat %03d done.", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	late := at.Add(200 * time.Hour)
	for i, text := range []string{"Sanding.", "Priming.", "Painting."} {
		if err := log.Append(Event{V: EventVersion, Time: late.Add(time.Duration(i) * time.Minute), Kind: KindMessage, Mode: ModeShared,
			Message: &MessagePayload{ID: fmt.Sprintf("S%d", i), Console: "widgets", Kind: "info", From: "b", Text: text}}); err != nil {
			t.Fatal(err)
		}
	}
	// Another session's line between b's leaves b's run whole.
	if err := log.Append(Event{V: EventVersion, Time: late.Add(30 * time.Second), Kind: KindMessage, Mode: ModeShared,
		Message: &MessagePayload{ID: "C0", Console: "widgets", Kind: "info", From: "c", Text: "Unrelated."}}); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Event{V: EventVersion, Time: late.Add(time.Hour), Session: "c", Kind: KindCase, Mode: ModeShared,
		Case: &CasePayload{Case: "gitlab/example-org/widgets#7", Call: &CaseCall{Tool: ToolReportProgress, Hash: "h1", Progress: &CaseProgress{Summary: "Two of three widgets painted.", NextAction: "Paint the third."}}}}); err != nil {
		t.Fatal(err)
	}
	u.now = func() time.Time { return late.Add(2 * time.Hour) }
	page := owner(t, u, "/console/widgets")
	if n := strings.Count(page, `<li class="entry `); n != consoleWindow || !strings.Contains(page, `class="older"`) || strings.Contains(page, "Coat 060 done.") || !strings.Contains(page, "Coat 119 done.") {
		t.Fatalf("the page holds %d entries:\n%s", n, page)
	}
	if !strings.Contains(page, "Painting.") || strings.Count(page, `<li class="entry msg msg-info"`) != 2 || !strings.Contains(page, "2 earlier") || strings.Index(page, "Sanding.") < strings.Index(page, "2 earlier") {
		t.Fatalf("the status lines are not one entry:\n%s", page)
	}
	if !strings.Contains(page, "Two of three widgets painted.") || !strings.Contains(page, "Next: Paint the third.") || !strings.Contains(page, `class="entry msg msg-progress"`) {
		t.Fatalf("the progress report is no entry:\n%s", page)
	}
	seen, before, rounds := map[string]bool{}, "", 0
	for {
		target := "/console/widgets/entries"
		if before != "" {
			target += "?before=" + before
		}
		var doc uiEntriesDoc
		if err := json.Unmarshal(get(u, "GET", target, pageAddr, "192.0.2.20:51000").Body.Bytes(), &doc); err != nil || len(doc.Entries) == 0 {
			t.Fatalf("%s: %v, %d entries", target, err, len(doc.Entries))
		}
		for _, e := range doc.Entries {
			if seen[e.ID] {
				t.Fatalf("%s repeats %s", target, e.ID)
			}
			seen[e.ID] = true
		}
		rounds++
		if !doc.More {
			break
		}
		before = doc.Entries[0].ID
	}
	if rounds != 3 || len(seen) != 120+2+1+5 {
		t.Fatalf("%d rounds, %d entries", rounds, len(seen))
	}
	// A poll naming what the page has gets those without their HTML.
	var doc, quiet uiEntriesDoc
	json.Unmarshal(get(u, "GET", "/console/widgets/entries", pageAddr, "192.0.2.20:51000").Body.Bytes(), &doc)
	var have []string
	for _, e := range doc.Entries[1:] {
		have = append(have, e.ID+"."+e.Hash)
	}
	json.Unmarshal(get(u, "GET", "/console/widgets/entries?have="+url.QueryEscape(strings.Join(have, ",")), pageAddr, "192.0.2.20:51000").Body.Bytes(), &quiet)
	if len(quiet.Entries) != consoleWindow || quiet.Entries[0].HTML == "" {
		t.Fatalf("a quiet poll: %d entries", len(quiet.Entries))
	}
	for _, e := range quiet.Entries[1:] {
		if e.HTML != "" {
			t.Fatalf("%s sent again", e.ID)
		}
	}
	// A window before an entry the console no longer holds is the one
	// before its time.
	es := []uiEntry{{DOMID: "e1"}, {DOMID: "e2"}, {DOMID: "e3"}}
	for i := range es {
		es[i].At = at.Add(time.Duration(i) * time.Hour)
	}
	if w, more := window(es, "gone", at.Add(90*time.Minute), 1); len(w) != 1 || w[0].DOMID != "e2" || !more {
		t.Fatalf("window before a gone entry: %+v, %v", w, more)
	}
	mv := uiMessageView{Activity: []uiStep{{Text: "Bash: go test"}, {Text: "Bash: ./bin/check", Failed: true}, {Text: "Read: a.go"}}}
	if got := mv.ToolsLine(); got != "3 tools, 1 failed" {
		t.Fatalf("tools line %q", got)
	}
}
