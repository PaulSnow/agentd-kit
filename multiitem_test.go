package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// issueWork is a thread agent's work on issue n of widgets.
func issueWork(n int, title string) map[string]any {
	return map[string]any{"title": title, "brief": fmt.Sprintf("Fix #%d.", n), "issue": n, "done_when": map[string]any{"kind": "forge", "ref": fmt.Sprintf("#%d", n), "state": "closed"}}
}

// worksAgentOut is what a fake thread agent prints when it reads several
// items in the owner's message: works, how many were asked and which,
// and those it does not open, with why.
func worksAgentOut(asked int, askedFor []string, notOpened []map[string]any, works ...map[string]any) string {
	a := map[string]any{"answer": fmt.Sprintf("%d items for agentd to design.", len(works)), "hand_to": "", "works": works, "asked": asked}
	if askedFor != nil {
		a["asked_for"] = askedFor
	}
	if notOpened != nil {
		a["not_opened"] = notOpened
	}
	out, _ := json.Marshal(a)
	res, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "structured_output": json.RawMessage(out),
		"usage": map[string]any{"input_tokens": 12, "output_tokens": 300}, "total_cost_usd": 0.01})
	return string(res) + "\n"
}

// askMany is the owner asking widgets' console for one work item per
// issue, the fake thread agent's replies set beforehand; it is the
// instruction and its items, oldest first.
func (h *harness) askMany(text string) (*Instruction, []*WorkItem) {
	h.t.Helper()
	if err := reply(h, "widgets", text); err != nil {
		h.t.Fatal(err)
	}
	h.ticks(6, time.Minute)
	in := h.instruction(h.t)
	var items []*WorkItem
	for _, id := range h.sup.st.sortedWork() {
		if it := h.sup.st.Work[id]; it.Instruction == in.ID {
			items = append(items, it)
		}
	}
	return h.sup.st.Instructions[in.ID], items
}

// One thread agent's reply to a message asking for one work item per
// issue opens an item for each, with its own issue and id, the first the
// one a single item has; the message says how many of how many were
// opened, and each item's state.
func TestOneReplyOpensAnItemPerIssue(t *testing.T) {
	dir := fakeClaude(t)
	h := designerHarness(t, false)
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 0, designOut("Fix the widget", false, 0))
	claudeAnswers(t, dir, 1, worksAgentOut(3, []string{"#101", "#102", "#103"}, nil,
		issueWork(101, "Widget squeaks"), issueWork(102, "Widget wobbles"), issueWork(103, "Widget fades")))
	in, items := h.askMany("One work item per issue for #101, #102, #103.")
	if len(items) != 3 {
		t.Fatalf("%d items: %+v", len(items), items)
	}
	ids, issues := map[string]bool{}, []int{}
	for _, it := range items {
		ids[it.ID] = true
		issues = append(issues, it.Issue)
		if !it.Designer {
			t.Fatalf("item not designed: %+v", it)
		}
	}
	sort.Ints(issues)
	if len(ids) != 3 || fmt.Sprint(issues) != "[101 102 103]" || !ids[workID(in.ID)] {
		t.Fatalf("ids %v, issues %v, first %s", ids, issues, workID(in.ID))
	}
	if calls := claudeCalls(t, dir); calls != 4 {
		t.Fatalf("claude was called %d times: one thread agent and three designers", calls)
	}
	if !strings.Contains(in.Waiting, "agentd opened 3 of 3 items asked") {
		t.Fatalf("waiting = %q", in.Waiting)
	}
	for _, it := range items {
		if !strings.Contains(in.Waiting, it.ID) {
			t.Fatalf("waiting lacks %s: %q", it.ID, in.Waiting)
		}
	}
	if !strings.Contains(in.Waiting, "waits on your go, change or decline") {
		t.Fatalf("waiting lacks the items' state: %q", in.Waiting)
	}
	// Declining one leaves the message waiting on the others.
	if err := h.respondDesign(items[1].ID, DesignDecline, ""); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	if in = h.sup.st.Instructions[in.ID]; !in.Replied.IsZero() || !strings.Contains(in.Waiting, items[1].ID+" is declined") {
		t.Fatalf("after one decline: %+v", in)
	}
}

// A reply that covers fewer items than asked is handed back to the thread
// agent, given what was opened; its next reply opens the rest, or says
// why one is not opened, which the message then shows.
func TestFewerItemsHandedBack(t *testing.T) {
	dir := fakeClaude(t)
	h := designerHarness(t, false)
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 0, designOut("Fix the widget", false, 0))
	claudeAnswers(t, dir, 1, worksAgentOut(3, []string{"#101", "#102", "#103"}, nil, issueWork(101, "Widget squeaks")))
	claudeAnswers(t, dir, 2, worksAgentOut(3, []string{"#101", "#102", "#103"}, []map[string]any{{"what": "#103", "why": "already fixed by !7"}},
		issueWork(102, "Widget wobbles")))
	in, items := h.askMany("One work item per issue for #101, #102, #103.")
	if len(items) != 2 || items[0].Issue != 101 || items[1].Issue != 102 || items[0].ID != workID(in.ID) {
		t.Fatalf("items = %+v", items)
	}
	back := readFake(t, dir, "in.2")
	if !strings.Contains(back, `"handed_back"`) || !strings.Contains(back, "Widget squeaks") {
		t.Fatalf("the hand-back's input: %s", back)
	}
	if calls := claudeCalls(t, dir); calls != 4 {
		t.Fatalf("claude was called %d times: two thread agents and two designers", calls)
	}
	for _, want := range []string{"agentd opened 2 of 3 items asked", "#103", "already fixed by !7"} {
		if !strings.Contains(in.Waiting, want) {
			t.Fatalf("waiting lacks %q: %q", want, in.Waiting)
		}
	}
}

// A thread agent that keeps covering fewer items than asked is handed
// the message back threadHandBacks times, no more; then what it opened
// stands, and the message names what was not opened and says a new
// message can ask for it.
func TestHandBacksEnd(t *testing.T) {
	dir := fakeClaude(t)
	h := designerHarness(t, false)
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 0, designOut("Fix the widget", false, 0))
	short := worksAgentOut(3, []string{"#101", "#102", "#103"}, nil, issueWork(101, "Widget squeaks"), issueWork(102, "Widget wobbles"))
	for n := 1; n <= 1+threadHandBacks; n++ {
		claudeAnswers(t, dir, n, short)
	}
	in, items := h.askMany("One work item per issue for #101, #102, #103.")
	if len(items) != 2 {
		t.Fatalf("items = %+v", items)
	}
	if calls := claudeCalls(t, dir); calls != 1+threadHandBacks+2 {
		t.Fatalf("claude was called %d times", calls)
	}
	for _, want := range []string{"agentd opened 2 of 3 items asked", "not opened: #103", "a new message can ask for"} {
		if !strings.Contains(in.Waiting, want) {
			t.Fatalf("waiting lacks %q: %q", want, in.Waiting)
		}
	}
}

// A reply with one work still opens the one item the instruction always
// had, and the message reads as it did.
func TestOneWorkOneItem(t *testing.T) {
	dir := fakeClaude(t)
	h := designerHarness(t, false)
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 0, designOut("Fix the widget", false, 0))
	claudeAnswers(t, dir, 1, workAgentOut("agentd designs it.", "", issueWork(101, "Widget squeaks")))
	in, items := h.askMany("Fix #101.")
	if len(items) != 1 || items[0].ID != workID(in.ID) || strings.Contains(in.Waiting, "of 1") {
		t.Fatalf("items %+v, waiting %q", items, in.Waiting)
	}
}

// workIDOf is workID for the first item, and distinct for the others.
func TestWorkIDOf(t *testing.T) {
	if workIDOf("I1", 0) != workID("I1") || workIDOf("I1", 1) == workID("I1") || workIDOf("I1", 1) == workIDOf("I1", 2) {
		t.Fatal("workIDOf")
	}
}

// With the design step off, the first item takes the session started
// for the message, and each other item a session of its own, never the
// first's.
func TestItemsPerIssueUndesigned(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 1, worksAgentOut(2, nil, nil, issueWork(101, "Widget squeaks"), issueWork(102, "Widget wobbles")))
	in, items := h.askMany("One work item per issue for #101 and #102.")
	if len(items) != 2 || items[0].Designer || items[1].Designer {
		t.Fatalf("items = %+v", items)
	}
	if items[0].Session == "" || items[0].Session != in.Session || items[1].Session == "" || items[1].Session == in.Session {
		t.Fatalf("sessions: message %q, items %q %q", in.Session, items[0].Session, items[1].Session)
	}
	if p := h.sup.st.Runtime[items[1].Session]; p.Issue != 102 || p.Item != items[1].ID {
		t.Fatalf("the second item's session: %+v", p)
	}
}
