package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
