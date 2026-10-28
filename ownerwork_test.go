package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// agentWork is what a fake thread agent prints when it asks for work.
func agentWork(answer, title, brief string) string {
	out, _ := json.Marshal(map[string]any{"answer": answer, "hand_to": "", "work": map[string]any{"title": title, "brief": brief, "done_when": map[string]any{"kind": "owner"}}})
	res, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "structured_output": json.RawMessage(out),
		"usage": map[string]any{"input_tokens": 12, "output_tokens": 300}, "total_cost_usd": 0.01})
	return string(res) + "\n"
}

// routeOf is the route steps of instruction id.
func (h *harness) routeOf(id string) []RoutePayload {
	var out []RoutePayload
	for _, e := range h.events() {
		if e.Kind == KindRoute && e.Route.Instruction == id {
			out = append(out, *e.Route)
		}
	}
	return out
}

// finish makes started session name finished at the harness's time.
func (h *harness) finish(name string) {
	h.t.Helper()
	h.status(name, "# s\n\n## Now\n- Step 1a arrived on the target.\n\nDONE\n")
	at := h.clock.Now()
	if err := os.Chtimes(filepath.Join(h.dir, name, "STATUS.md"), at, at); err != nil {
		h.t.Fatal(err)
	}
}

// The owner's message asks for work no running session covers: the
// agent names it as work, and Go starts a work session of the repository
// for it, REPO-owner-SLUG-HEX, briefed with the owner's words and the
// agent's brief and facts; the route says it started it, the page links
// it, and the session's first post is threaded under the owner's message.
func TestOwnerWorkStartsSession(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 1, agentWork("No session covers it: agentd starts one to deploy the update.", "Deploy the update", "Read the deploy notes and ship step 1a."))
	if err := UIInstruction(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "widgets", "", "Make step 1a arrive.", stage(t, upload{name: "notes.txt", body: []byte("the deploy notes")})); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, time.Minute)
	in := h.instruction(t)
	name, branch := ownerWorkName("widgets", "Deploy the update", in.ID)
	if !strings.HasPrefix(name, "widgets-owner-deploy-the-update-") || len(name) != len("widgets-owner-deploy-the-update-")+6 {
		t.Fatalf("name = %q", name)
	}
	if in.Session != name || !in.Started || in.Failed != "" {
		t.Fatalf("instruction = %+v", in)
	}
	steps := h.routeOf(in.ID)
	if last := steps[len(steps)-1]; last.Step != RouteTo || last.Reason != "started "+name || !last.Started {
		t.Fatalf("route = %+v", steps)
	}
	p := h.sup.st.Runtime[name]
	if p.Owner != in.ID || p.Kind != KindWork || p.Branch != branch {
		t.Fatalf("start = %+v", p)
	}
	brief, err := os.ReadFile(filepath.Join(h.dir, name, "BRIEF.md"))
	if err != nil || !strings.HasPrefix(string(brief), briefOwner) {
		t.Fatalf("brief = %q, %v", brief, err)
	}
	for _, want := range []string{"Make step 1a arrive.", "Read the deploy notes and ship step 1a.", `"facts_at"`, `"running"`} {
		if !strings.Contains(string(brief), want) {
			t.Fatalf("the brief lacks %q:\n%s", want, brief)
		}
	}
	// Its files are copied into its workdir, and the message is handed
	// over once, at its launch.
	if b, err := os.ReadFile(filepath.Join(h.dir, name, workdirAttachments, in.Attachments[0].Hash+".txt")); err != nil || string(b) != "the deploy notes" {
		t.Fatalf("attachment = %q, %v", b, err)
	}
	h.alive(name, "deploying\n"+idleScreen)
	h.ticks(2, time.Minute)
	if in = h.sup.st.Instructions[in.ID]; in.Delivered.IsZero() || h.count(KindAction, ActInstruct, name) != 0 {
		t.Fatalf("delivered %v, instructed %d times", in.Delivered, h.count(KindAction, ActInstruct, name))
	}
	if docs := readInstructions(t, filepath.Join(h.dir, name)); len(docs) != 1 || docs[0].Text != "Make step 1a arrive." {
		t.Fatalf("instructions.json = %+v", docs)
	}
	linked := false
	for _, s := range instructionSteps(h.sup.st, in) {
		linked = linked || s.Session == name
	}
	if !linked {
		t.Fatalf("the steps do not link %s: %+v", name, instructionSteps(h.sup.st, in))
	}
	// Its first post goes under the owner's message.
	if err := PostMessage(h.sup.cfg, NewLogWriter(h.buf), h.tmux, h.clock.Now(), MessagePost{Console: "widgets", Kind: "info", From: name, Text: "Reading the deploy notes."}); err != nil {
		t.Fatal(err)
	}
	src := newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a")
	v, err := (&UI{sources: []*uiSource{src}}).consoleView("widgets", h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	threaded := false
	for _, e := range v.Entries {
		for _, r := range e.Responses {
			threaded = threaded || e.Entry == entryInstruction+in.ID && r.Text == "Reading the deploy notes."
		}
	}
	if !threaded {
		t.Fatalf("the session's first post is not under the owner's message: %+v", v.Entries)
	}
}

// At sessions.max no session is started: the route says so and names the
// sessions holding the places, and the entry's next step offers to end
// one, which the owner's choice does, starting the session after.
func TestOwnerWorkAtMaxNamesHolders(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	w1 := h.startSession("issue-1", KindWork, 1)
	w2 := h.startSession("issue-2", KindWork, 2)
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 1, agentWork("agentd starts a session for it.", "Paint it", "Paint the shed red."))
	if err := reply(h, "widgets", "Paint the shed red."); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	in := h.instruction(t)
	if in.Session != "" || !strings.Contains(in.Failed, "its max") || !strings.Contains(in.Failed, w1) || !strings.Contains(in.Failed, w2) {
		t.Fatalf("instruction = %+v", in)
	}
	d := nextDecision(h.sup.cfg, h.sup.st, in, h.clock.Now())
	if d == nil || !contains(decisionOptions(*d), nextEnd+w1) || contains(decisionOptions(*d), nextStart) {
		t.Fatalf("next step = %+v", d)
	}
	src := newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a")
	src.now = h.clock.Now
	if err := src.respond("widgets", entryNext+in.ID, "", "", &AnswerPayload{Choice: nextStart}, nil); err == nil {
		t.Fatal("a step not offered was taken")
	}
	if err := src.respond("widgets", entryNext+in.ID, "", "", &AnswerPayload{Choice: nextEnd + w1}, nil); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	in = h.sup.st.Instructions[in.ID]
	if _, ok := h.sup.st.Runtime[w1]; ok || in.Failed != "" || !in.Started || h.sup.st.Runtime[in.Session].Owner != in.ID {
		t.Fatalf("after ending %s: instruction = %+v", w1, in)
	}
	if src.catchUp(); len(src.nextEntries(h.sup.cfg, "widgets", func(...string) bool { return false }, h.clock.Now())) != 0 {
		t.Fatal("the next step is still offered")
	}
}

// A session that finished and whose tmux session still runs may be handed
// work: the agent is told it is alive, and Go takes the DONE off its
// STATUS.md and types the message as into a running one.
func TestHandToDoneAliveReopens(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	w1 := h.startSession("issue-1", KindWork, 1)
	h.ticks(2, time.Minute)
	h.alive(w1, "w1 works")
	h.ticks(1, time.Minute)
	h.finish(w1)
	h.alive(w1, "w1 is done")
	h.ticks(1, time.Minute)
	if st := h.state(w1); st != Done {
		t.Fatalf("state = %v", st)
	}
	claudeAnswers(t, dir, 1, agentOut("Handed to "+w1+", which did step 1a.", w1))
	if err := reply(h, "widgets", "Ship step 1b too."); err != nil {
		t.Fatal(err)
	}
	// Mid-turn, it is not reopened: the pointer would go into the turn.
	h.alive(w1, "w1 writes\n"+runningScreen)
	h.ticks(2, time.Minute)
	if h.count(KindAction, ActReopen, w1) != 0 || !h.sup.st.Instructions[h.instruction(t).ID].Delivered.IsZero() {
		t.Fatal("reopened mid-turn")
	}
	for i := 0; i < 3; i++ {
		h.alive(w1, "w1 pane "+itoa(i)+"\n"+idleScreen)
		h.ticks(1, time.Minute)
	}
	x := agentInput(t, dir, 1)
	if len(x.Finished) != 1 || x.Finished[0].Name != w1 || !x.Finished[0].Alive || strings.Contains(x.Finished[0].Status, "DONE") || !strings.Contains(x.Finished[0].Status, "Step 1a arrived") {
		t.Fatalf("finished = %+v", x.Finished)
	}
	in := h.instruction(t)
	if in.Session != w1 || in.Delivered.IsZero() || in.Failed != "" {
		t.Fatalf("instruction = %+v", in)
	}
	b, _ := os.ReadFile(filepath.Join(h.dir, w1, "STATUS.md"))
	if ParseStatus(string(b)).Done || !strings.Contains(string(b), "Reopened by agentd") {
		t.Fatalf("STATUS.md = %q", b)
	}
	if h.count(KindAction, ActReopen, w1) != 1 || h.count(KindAction, ActInstruct, w1) != 1 {
		t.Fatalf("reopened %d, instructed %d times", h.count(KindAction, ActReopen, w1), h.count(KindAction, ActInstruct, w1))
	}
	if docs := readInstructions(t, h.sup.cfg.Sessions[w1].Workdir); len(docs) != 1 || docs[0].Text != "Ship step 1b too." {
		t.Fatalf("instructions.json = %+v", docs)
	}
}

// A hand-over to a finished session whose tmux session is gone drops no
// message: Go opens its item from it, starts a session for it and tells
// the owner on the console (#68). A message that failed otherwise still
// offers the next step: end a session to start one for it, or send it to
// a session that takes messages.
func TestHandToDoneGoneOpensItem(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	w1 := h.startSession("issue-1", KindWork, 1)
	h.ticks(2, time.Minute)
	h.alive(w1, "w1 works")
	h.ticks(1, time.Minute)
	h.finish(w1)
	h.tmux.alive[w1] = false
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 0, agentOut("Handed to "+w1+".", w1))
	before := claudeCalls(t, dir)
	if err := reply(h, "widgets", "Ship step 1b too."); err != nil {
		t.Fatal(err)
	}
	h.ticks(4, time.Minute)
	in := h.instruction(t)
	if n := claudeCalls(t, dir) - before; n != 1 || in.Failed != "" {
		t.Fatalf("%d runs; instruction = %+v", n, in)
	}
	if x := agentInput(t, dir, before+1); len(x.Finished) != 1 || x.Finished[0].Alive {
		t.Fatalf("finished = %+v", x.Finished)
	}
	h.handToTold(in, w1, false)

	// Another message fails: its entry offers the next step.
	if err := reply(h, "widgets", "Tag it too."); err != nil {
		t.Fatal(err)
	}
	h.sup.ownerRecords()
	var other *Instruction
	for _, x := range h.sup.st.Instructions {
		if x.Text == "Tag it too." {
			other = x
		}
	}
	h.sup.emit("", Event{Kind: KindRoute, Route: &RoutePayload{Instruction: other.ID, Step: RouteFail, Reason: "agentd could not start a session for it"}})
	// The repository is at its max: a session is ended to start one.
	d := nextDecision(h.sup.cfg, h.sup.st, other, h.clock.Now())
	if d == nil || !contains(decisionOptions(*d), nextEnd+w1) || !contains(decisionOptions(*d), nextSend+"a") || contains(decisionOptions(*d), nextSend+w1) {
		t.Fatalf("next step = %+v", d)
	}
	src := newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a")
	src.now = h.clock.Now
	v, err := (&UI{sources: []*uiSource{src}}).consoleView("widgets", h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	shown := false
	for _, e := range v.Entries {
		shown = shown || e.Entry == entryNext+other.ID && e.Open && e.Decision != nil
	}
	if !shown {
		t.Fatalf("the page offers no next step: %+v", v.Entries)
	}
	// The owner sends it to a: it goes there as a new message.
	if err := src.respond("widgets", entryNext+other.ID, "", "", &AnswerPayload{Choice: nextSend + "a"}, nil); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	if other = h.sup.st.Instructions[other.ID]; other.Session != "a" || other.Failed != "" {
		t.Fatalf("after the next step: %+v", other)
	}
}

// An instruction whose thread agent hands it to a stopped session, with
// no work named, is not lost: Go opens a work item from the
// instruction's words, starts a session for it, or has it designed when
// design is on, and says so once on the repository's console (#68).
func TestHandToStoppedSessionOpensItemAndTellsOwner(t *testing.T) {
	for _, design := range []bool{false, true} {
		t.Run(map[bool]string{false: "start", true: "design"}[design], func(t *testing.T) {
			dir := fakeClaude(t)
			h := sessionsHarness(t)
			if design {
				h = designerHarness(t, false)
			}
			w1 := h.startSession("issue-1", KindWork, 1)
			h.ticks(2, time.Minute)
			h.alive(w1, "w1 works")
			h.ticks(1, time.Minute)
			tr := h.sup.track(w1)
			tr.State, tr.Stopped = Stopped, "its spend cap"
			h.tmux.alive[w1] = false
			before := claudeCalls(t, dir)
			claudeAnswers(t, dir, before+1, agentOut("Handed to "+w1+", which knows the step.", w1))
			claudeAnswers(t, dir, before+2, designOut("Ship step 1b of the plan", false, 0))
			if err := reply(h, "widgets", "Ship step 1b of the plan too."); err != nil {
				t.Fatal(err)
			}
			h.ticks(5, time.Minute)
			in := h.instruction(t)
			if in.Failed != "" {
				t.Fatalf("instruction = %+v", in)
			}
			h.handToTold(in, w1, design)
		})
	}
}

// handToTold checks that in, which its agent handed to the session that
// does not run, has an open item from its words, a session started for
// it, or its design under way, and one console message saying so.
func (h *harness) handToTold(in *Instruction, gone string, design bool) {
	h.t.Helper()
	it := h.sup.st.Work[workID(in.ID)]
	if it == nil || it.terminal() || !strings.Contains(in.Text, it.Title) {
		h.t.Fatalf("no open item from the instruction: %+v", it)
	}
	if design {
		if !it.Designer || it.Session != "" {
			h.t.Fatalf("not designed: item = %+v", it)
		}
	} else if it.Session == "" || it.Session == gone || in.Session != it.Session || !in.Started {
		h.t.Fatalf("no session started for it: item = %+v, instruction = %+v", it, in)
	}
	var told []*MessagePayload
	for _, e := range h.events() {
		if e.Kind == KindMessage && e.Message.Console == "widgets" && strings.Contains(e.Message.Text, gone) {
			told = append(told, e.Message)
		}
	}
	if len(told) != 1 || !strings.Contains(told[0].Text, it.ID) || !strings.Contains(told[0].Text, "not running") || told[0].Kind != "info" {
		h.t.Fatalf("console messages naming %s: %+v", gone, told)
	}
}

// A card older than the newest STATUS.md it covers is marked stale in the
// agent's input; one newer is not.
func TestStaleCardMarked(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	w1 := h.startSession("issue-1", KindWork, 1)
	h.ticks(2, time.Minute)
	h.alive(w1, "w1 works")
	h.ticks(1, time.Minute)
	h.finish(w1)
	h.ticks(1, time.Minute)
	h.sup.cards = NewContextStore(h.sup.cfg)
	old, fresh := h.clock.Now().Add(-48*time.Hour), h.clock.Now().Add(time.Hour)
	h.sup.cards.Apply(Event{Kind: KindCard, Card: &CardPayload{Repository: "widgets", Topic: "#1", Card: &Card{Topic: "#1", Kind: CardIssue, Status: "step 1a has not arrived", Updated: old}}})
	h.sup.cards.Apply(Event{Kind: KindCard, Card: &CardPayload{Repository: "widgets", Topic: "#2", Card: &Card{Topic: "#2", Kind: CardIssue, Status: "open", Updated: fresh}}})
	claudeAnswers(t, dir, 0, agentOut("Step 1a arrived, by "+w1+"'s STATUS.md.", ""))
	if err := reply(h, "widgets", "Did step 1a arrive? See #1 and #2."); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	x := agentInput(t, dir, 1)
	cards := map[string]threadCard{}
	for _, c := range x.Cards {
		cards[c.Topic] = c
	}
	if c, ok := cards["#1"]; !ok || !strings.Contains(c.Stale, w1+"'s STATUS.md") {
		t.Fatalf("#1's card = %+v", x.Cards)
	}
	if c, ok := cards["#2"]; !ok || c.Stale != "" {
		t.Fatalf("#2's card = %+v", x.Cards)
	}
	if !strings.Contains(threadSystem, "STATUS.md outranks a card") {
		t.Fatal("the agent is not told STATUS.md outranks a card")
	}
}

// A failure older than the next steps' span offers none: the history of
// failures is not a list of open questions.
func TestOldFailureOffersNoNextStep(t *testing.T) {
	h := sessionsHarness(t)
	in := &Instruction{InstructionPayload: InstructionPayload{ID: "I1", Repositories: []string{"widgets"}, Route: true}, Failed: "it failed", FailedAt: h.clock.Now()}
	if nextDecision(h.sup.cfg, h.sup.st, in, h.clock.Now().Add(time.Hour)) == nil {
		t.Fatal("a fresh failure offers no next step")
	}
	if nextDecision(h.sup.cfg, h.sup.st, in, h.clock.Now().Add(nextSpan+time.Hour)) != nil {
		t.Fatal("an old failure offers a next step")
	}
}

// A hand-over to a name that matches a confidential pattern, or runs over
// lines, is logged and told as "a session", one line (#68).
func TestHandToDroppedNameScreened(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	h.cfg.ConfidentialPatterns = []string{`(?i)secret-project`}
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	claudeAnswers(t, dir, 0, agentOut("Handed over.", "secret-project\nsession"))
	if err := reply(h, "widgets", "Paint it red."); err != nil {
		t.Fatal(err)
	}
	h.ticks(4, time.Minute)
	in := h.instruction(t)
	if in.Failed != "" || in.AgentLast == nil || in.AgentLast.HandToDropped != "a session" {
		t.Fatalf("instruction = %+v, agent = %+v", in, in.AgentLast)
	}
	if told := h.messages("The thread agent named a session, which is not running"); len(told) != 1 {
		t.Fatalf("told %q", told)
	}
	for _, e := range h.events() {
		if b, _ := json.Marshal(e); strings.Contains(string(b), "secret-project") && e.Kind != KindInstruction {
			t.Fatalf("logged: %s", b)
		}
	}
}
