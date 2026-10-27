package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// versionPage is a page that says the version it is set to, as a deployed
// service's --version does.
type versionPage struct {
	mu      sync.Mutex
	version string
}

func (p *versionPage) set(v string) { p.mu.Lock(); p.version = v; p.mu.Unlock() }

func (p *versionPage) serve(t *testing.T) string {
	itemLoopback = true
	t.Cleanup(func() { itemLoopback = false })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		w.Write([]byte("widgets " + p.version + "\n"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/version"
}

// askWork is the owner asking for work on widgets' console and a thread
// agent reading it as w, handed to handTo when set: the item Go opens.
func (h *harness) askWork(text string, w WorkRequest, handTo string) *WorkItem {
	h.t.Helper()
	if err := reply(h, "widgets", text); err != nil {
		h.t.Fatal(err)
	}
	h.sup.ownerRecords()
	in := h.instruction(h.t)
	h.sup.emit("", Event{Kind: KindRoute, Route: &RoutePayload{Instruction: in.ID, Step: RouteAgent}})
	h.sup.emit("", Event{Kind: KindRoute, Route: &RoutePayload{Instruction: in.ID, Step: RouteAnswered,
		Agent: &AgentPayload{Answer: "Go holds it as a work item until it is done.", HandTo: handTo, Work: &w}}})
	h.ticks(1, time.Minute)
	it := h.sup.st.Work[workID(in.ID)]
	if it == nil {
		h.t.Fatal("no work item was opened")
	}
	return it
}

// item is the item id as the supervisor holds it now.
func (h *harness) item(id string) *WorkItem {
	h.t.Helper()
	it := h.sup.st.Work[id]
	if it == nil {
		h.t.Fatalf("no item %s", id)
	}
	return it
}

// workSteps are the steps of item id, in order.
func (h *harness) workSteps(id string) []string {
	var out []string
	for _, e := range h.events() {
		if e.Kind == KindWorkItem && e.Work.ID == id {
			out = append(out, e.Work.Step)
		}
	}
	return out
}

// noticesAbout are agentd's console lines about item id.
func (h *harness) noticesAbout(id string) []string {
	var out []string
	for _, e := range h.events() {
		if e.Kind == KindMessage && e.Message.From == "agentd" && strings.Contains(e.Message.Text, id) {
			out = append(out, e.Message.Text)
		}
	}
	return out
}

func deployWork(page string) WorkRequest {
	return WorkRequest{Title: "Deploy v3.38.2", Brief: "Deploy v3.38.2 to production, step 1b.", DoneWhen: EndCheck{Kind: DoneURL, URL: page, Contains: "3.38.2"}}
}

// The owner's instruction is a work item: Go starts a session for it,
// whose brief and items.json hold the item and the owner's words; the
// session naming it acknowledges it, and only the check of done_when
// closes it. A claim the check refutes is posted back to the session and
// the owner, and closes nothing.
func TestWorkItemClosedOnlyByItsCheck(t *testing.T) {
	h := sessionsHarness(t)
	page := &versionPage{version: "3.38.1"}
	url := page.serve(t)
	it := h.askWork("Deploy v3.38.2 to production now.", deployWork(url), "")
	if it.State != WorkAssigned || !it.Started || !strings.Contains(it.Session, "widgets-owner-deploy-v3-38-2-"+strings.ToLower(it.ID)) {
		t.Fatalf("item = %+v", it)
	}
	name := it.Session
	h.ticks(1, time.Minute)
	brief, err := os.ReadFile(filepath.Join(h.dir, name, "BRIEF.md"))
	if err != nil || !strings.HasPrefix(string(brief), briefItem) || !strings.Contains(string(brief), it.ID) || !strings.Contains(string(brief), "Deploy v3.38.2 to production now.") {
		t.Fatalf("brief = %q, %v", brief, err)
	}
	b, err := os.ReadFile(filepath.Join(h.dir, name, ItemsFile))
	var doc struct{ Items []itemDoc }
	if err != nil || json.Unmarshal(b, &doc) != nil || len(doc.Items) != 1 || doc.Items[0].DoneWhen.URL != url || doc.Items[0].Owner != "Deploy v3.38.2 to production now." {
		t.Fatalf("items.json = %s, %v", b, err)
	}

	h.alive(name, "working on the deploy")
	h.status(name, "# Status\n\n## Now\n"+it.ID+": deploying 1b\n")
	h.ticks(2, time.Minute)
	if it = h.item(it.ID); it.State != WorkWorking || it.Acked.IsZero() {
		t.Fatalf("item = %+v", it)
	}

	// A claim the page refutes.
	h.status(name, "# Status\n\n## Now\n"+it.ID+" done: deployed v3.38.2\n")
	h.alive(name, "deployed, I think")
	h.ticks(2, time.Minute)
	if it = h.item(it.ID); it.State == WorkDone || it.Refuted != "deployed v3.38.2" || it.Check == nil || it.Check.OK || !it.Check.Claim {
		t.Fatalf("a refuted claim: item = %+v, check %+v", it, it.Check)
	}
	if ns := h.noticesAbout(it.ID); len(ns) != 1 || !strings.Contains(ns[0], "does not contain") {
		t.Fatalf("notices = %q", ns)
	}
	if typed := h.typed(name); len(typed) == 0 || !strings.HasPrefix(typed[len(typed)-1], "agentd checked work item "+it.ID) {
		t.Fatalf("typed = %q", typed)
	}
	// Told once, however many polls find it still not done; an unchanged
	// result is not logged again.
	for i := 0; i < 30; i++ {
		h.alive(name, "deploying "+itoa(i))
		h.ticks(1, time.Minute)
	}
	if n := len(h.typed(name)); n != 1 {
		t.Fatalf("typed %d lines: %q", n, h.typed(name))
	}

	// The deploy lands: the next check closes it, with its evidence.
	page.set("3.38.2")
	h.ticks(6, time.Minute)
	if it = h.item(it.ID); it.State != WorkDone || it.DoneBy != "agentd" || !strings.Contains(it.Evidence, `contains "3.38.2"`) || !strings.Contains(it.Evidence, "checked") {
		t.Fatalf("item = %+v", it)
	}
	steps := strings.Join(h.workSteps(it.ID), " ")
	if steps != "open assign check ack working claim check escalate told check" {
		t.Fatalf("steps = %s", steps)
	}
}

// A session that writes DONE with its item not done gives it back: the
// item is open again with the session's STATUS.md as a note, and Go
// assigns it to a new session.
func TestWorkItemReopenedWhenSessionFinishesEarly(t *testing.T) {
	h := sessionsHarness(t)
	page := &versionPage{version: "3.38.1"}
	it := h.askWork("Deploy v3.38.2.", deployWork(page.serve(t)), "")
	first := it.Session
	h.ticks(1, time.Minute)
	h.alive(first, "done with 1a")
	h.status(first, "# Status\n\n"+it.ID+": step 1a deployed.\nWAITING for go 1b\n\nDONE\n")
	h.ticks(4, time.Minute)
	it = h.item(it.ID)
	if len(it.Tried) != 1 || it.Tried[0] != first || len(it.Notes) == 0 || !strings.Contains(it.Notes[0].Text, "WAITING for go 1b") {
		t.Fatalf("item = %+v", it)
	}
	if it.Session == "" || it.Session == first || it.State != WorkAssigned || !it.Started {
		t.Fatalf("not re-assigned: item = %+v", it)
	}
	if _, ok := h.sup.st.Runtime[first]; ok {
		t.Fatalf("%s, started for the item and taken off it, still holds a place", first)
	}
}

// An item handed to a running session is typed into it with its id; one
// the session does not name within ack_within is typed again once, then
// given to a session started for it.
func TestWorkItemUnacknowledgedRetypedThenReassigned(t *testing.T) {
	h := sessionsHarness(t)
	h.alive("a", "a works")
	it := h.askWork("Bump the version to 3.38.2.", WorkRequest{Title: "Bump the version", Brief: "Bump it.", DoneWhen: EndCheck{Kind: DoneOwner}}, "a")
	if it.Session != "a" || it.Started {
		t.Fatalf("item = %+v", it)
	}
	pointer := "agentd assigned you work item " + it.ID
	count := func() int {
		n := 0
		for _, l := range h.typed("a") {
			if strings.HasPrefix(l, pointer) {
				n++
			}
		}
		return n
	}
	for i := 0; i < 9; i++ {
		// The screen keeps the line agentd typed, which acknowledges
		// nothing.
		h.alive("a", "> "+pointer+": read it in items.json\na works "+itoa(i))
		h.ticks(1, time.Minute)
	}
	if st := h.item(it.ID).State; st != WorkAssigned {
		t.Fatalf("agentd's own line acknowledged it: %s", st)
	}
	if n := count(); n != 1 {
		t.Fatalf("typed %d times within ack_within", n)
	}
	for i := 0; i < 3; i++ {
		h.alive("a", "a works on "+itoa(i))
		h.ticks(1, time.Minute)
	}
	if n := count(); n != 2 || h.item(it.ID).Session != "a" {
		t.Fatalf("typed %d times; item = %+v", n, h.item(it.ID))
	}
	for i := 0; i < 11; i++ {
		h.alive("a", "a still works "+itoa(i))
		h.ticks(1, time.Minute)
	}
	it = h.item(it.ID)
	if n := count(); n != 2 || len(it.Tried) != 1 || it.Tried[0] != "a" || !it.Started || it.Session == "a" {
		t.Fatalf("typed %d times; item = %+v", n, it)
	}
}

// Go never assigns an item to a session it does not run: the agent's
// hand-over to one is passed by, and a session is started for it.
func TestWorkItemNeverToUnreachableSession(t *testing.T) {
	h := sessionsHarness(t)
	h.alive("laptop-deploy", "❯ ")
	it := h.askWork("Go 1b.", WorkRequest{Title: "Go 1b", Brief: "Deploy 1b.", DoneWhen: EndCheck{Kind: DoneOwner}}, "laptop-deploy")
	if it.Session == "laptop-deploy" || !it.Started {
		t.Fatalf("item = %+v", it)
	}
	if n := h.tmux.count("type", "laptop-deploy"); n != 0 {
		t.Fatalf("typed into laptop-deploy %d times", n)
	}
}

// An item with no progress for items.escalate_after is told to the
// owner once, at the deadline.
func TestWorkItemEscalatedOnceAtDeadline(t *testing.T) {
	h := sessionsHarness(t)
	it := h.askWork("Tag v3.38.2.", WorkRequest{Title: "Tag it", Brief: "Tag v3.38.2.", DoneWhen: EndCheck{Kind: DoneForge, Ref: "v3.38.2", State: "tagged"}}, "")
	name := it.Session
	h.alive(name, "tagging")
	h.status(name, "# Status\n\n"+it.ID+": tagging\n")
	h.ticks(3, time.Minute)
	if it = h.item(it.ID); it.State != WorkWorking {
		t.Fatalf("item = %+v", it)
	}
	// STATUS.md changes no more from here.
	at := h.clock.Now().Add(-24 * time.Hour)
	os.Chtimes(filepath.Join(h.dir, name, "STATUS.md"), at, at)
	stall := func() int {
		n := 0
		for _, s := range h.noticesAbout(it.ID) {
			if strings.Contains(s, "no progress") {
				n++
			}
		}
		return n
	}
	deadline := it.Progress.Add(DefaultItemEscalateAfter)
	for h.clock.Now().Before(deadline.Add(-5 * time.Minute)) {
		h.alive(name, "tagging "+h.clock.Now().String())
		h.ticks(1, 5*time.Minute)
	}
	if n := stall(); n != 0 {
		t.Fatalf("escalated %d times before the deadline", n)
	}
	for i := 0; i < 6; i++ {
		h.alive(name, "tagging still "+itoa(i))
		h.ticks(1, 5*time.Minute)
	}
	if n := stall(); n != 1 {
		t.Fatalf("escalated %d times after the deadline", n)
	}
}

// The owner answers on an item's entry: done with a word closes it, by
// the owner; re-assign takes its session off it; decline drops it. Words
// alone go to its session.
func TestWorkItemOwnerAnswers(t *testing.T) {
	h := sessionsHarness(t)
	h.alive("a", "a works")
	it := h.askWork("Write the release notes.", WorkRequest{Title: "Release notes", Brief: "Write them.", DoneWhen: EndCheck{Kind: DoneOwner}}, "a")
	respond := func(choice, text string) {
		t.Helper()
		var a *EntryAnswer
		if choice != "" {
			a = &EntryAnswer{Choice: choice}
		}
		re := &EntryRe{ID: entryWork + it.ID, Kind: "work", At: it.Opened}
		if err := recordInstructionRe(h.cfg, NewLogWriter(h.buf), h.clock.Now(), []string{"widgets"}, "", "", "ui", text, re, a, nil); err != nil {
			t.Fatal(err)
		}
		h.alive("a", "a works "+choice+text)
		h.ticks(2, time.Minute)
	}
	respond("", "Use the short form.")
	if docs := readInstructions(t, h.sup.cfg.Sessions["a"].Workdir); len(docs) != 1 || docs[0].Text != "Use the short form." {
		t.Fatalf("the owner's words did not reach a: %+v", docs)
	}
	respond(WorkAnswerReassign, "a is busy")
	if it = h.item(it.ID); len(it.Tried) != 1 || it.Session == "a" || it.Reassigns != 1 {
		t.Fatalf("item = %+v", it)
	}
	respond(WorkAnswerDone, "I wrote them myself")
	if it = h.item(it.ID); it.State != WorkDone || it.DoneBy != "owner" || !strings.Contains(it.Evidence, "I wrote them myself") {
		t.Fatalf("item = %+v", it)
	}
	respond(WorkAnswerDecline, "")
	if it = h.item(it.ID); it.State != WorkDone {
		t.Fatalf("a closed item changed: %+v", it)
	}
}

// A session's claim on an item only the owner can judge waits on the
// owner: not done keeps it with its session, and the same claim is not
// asked again.
func TestWorkItemOwnerJudgesClaim(t *testing.T) {
	h := sessionsHarness(t)
	it := h.askWork("Make the logo bigger.", WorkRequest{Title: "Bigger logo", Brief: "Make it bigger.", DoneWhen: EndCheck{Kind: DoneOwner}}, "")
	name := it.Session
	h.alive(name, "resizing")
	h.status(name, "# Status\n\n"+it.ID+" done: the logo is 2x\n")
	h.ticks(3, time.Minute)
	if it = h.item(it.ID); it.State == WorkDone || it.Claim != "the logo is 2x" {
		t.Fatalf("item = %+v", it)
	}
	if ns := h.noticesAbout(it.ID); len(ns) != 1 || !strings.Contains(ns[0], "Only you can say") {
		t.Fatalf("notices = %q", ns)
	}
	if opts := strings.Join(it.workOptions(), ","); opts != "done,not-done,reassign,decline" {
		t.Fatalf("options = %s", opts)
	}
	re := &EntryRe{ID: entryWork + it.ID, Kind: "work", At: it.Opened}
	if err := recordInstructionRe(h.cfg, NewLogWriter(h.buf), h.clock.Now(), []string{"widgets"}, "", "", "ui", "it is still small", re, &EntryAnswer{Choice: WorkAnswerNotDone}, nil); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, time.Minute)
	if it = h.item(it.ID); it.State != WorkWorking || it.Session != name || it.Claim != "" {
		t.Fatalf("item = %+v", it)
	}
	if ns := h.noticesAbout(it.ID); len(ns) != 1 {
		t.Fatalf("the same claim was asked again: %q", ns)
	}
}

// done_when is checked as written: a page's body, a forge state, a file.
func TestEndCheckKinds(t *testing.T) {
	for _, d := range []EndCheck{
		{Kind: DoneURL, URL: "file:///etc/passwd", Contains: "x"},
		{Kind: DoneURL, URL: "https://example.com/v"},
		{Kind: DoneForge, Ref: "#4", State: "merged"},
		{Kind: DoneForge, Ref: "../x", State: "tagged"},
		{Kind: DoneFile, Path: "../secrets", Contains: "x"},
		{Kind: DoneFile, Path: "/etc/hosts", Contains: "x"},
		{Kind: "vibes"},
	} {
		if d.check() == nil {
			t.Errorf("%+v was accepted", d)
		}
	}
	// A url check reads no service of this machine's, nor the cloud's
	// metadata.
	for _, u := range []string{"http://169.254.169.254/latest/meta-data", "http://127.0.0.1:1/version"} {
		if ok, seen := checkURL(u, "x"); ok || !strings.Contains(seen, "dials no address") {
			t.Errorf("%s: %v %s", u, ok, seen)
		}
	}
	h := sessionsHarness(t)
	os.WriteFile(filepath.Join(h.dir, "clone", "VERSION"), []byte("3.38.2\n"), 0o644)
	if ok, seen := h.sup.runCheck(EndCheck{Kind: DoneFile, Path: "VERSION", Contains: "3.38.2"}, Repository{}, filepath.Join(h.dir, "clone"), ""); !ok || !strings.Contains(seen, "VERSION contains") {
		t.Fatalf("file check: %v %s", ok, seen)
	}
	h.sup.api = &fakeForgeAPI{gitlab: map[string]string{
		"=/projects/example-org%2Fwidgets/merge_requests/4": `{"state":"merged"}`,
		"=/projects/example-org%2Fwidgets/issues/7":         `{"state":"opened"}`,
	}}
	repo := h.cfg.Repositories["widgets"]
	if ok, seen := h.sup.runCheck(EndCheck{Kind: DoneForge, Ref: "!4", State: "merged"}, repo, "", ""); !ok {
		t.Fatalf("merged: %s", seen)
	}
	if ok, seen := h.sup.runCheck(EndCheck{Kind: DoneForge, Ref: "#7", State: "closed"}, repo, "", ""); ok || !strings.Contains(seen, "it is opened") {
		t.Fatalf("closed: %v %s", ok, seen)
	}
}

// Open items are the first thing on the repository's console, each with
// the owner's words, state, session and done_when, and answers the owner
// gives on the page settle them.
func TestWorkItemOnThePage(t *testing.T) {
	h := sessionsHarness(t)
	it := h.askWork("Deploy v3.38.2 to production now.", WorkRequest{Title: "Deploy v3.38.2", Brief: "Deploy it.", DoneWhen: EndCheck{Kind: DoneForge, Ref: "v3.38.2", State: "tagged"}}, "")
	u := harnessUI(t, h)
	body := owner(t, u, "/console/widgets")
	items, box := strings.Index(body, `class="work-items"`), strings.Index(body, `id="box"`)
	if items < 0 || box < items {
		t.Fatalf("the items are not first on the console:\n%s", body)
	}
	for _, want := range []string{it.ID, "Deploy v3.38.2 to production now.", it.Session, "tag v3.38.2 exists", `value="` + entryWork + it.ID + `"`, `name="choice" value="decline"`} {
		if !strings.Contains(body[items:box], want) {
			t.Fatalf("the item lacks %q:\n%s", want, body[items:box])
		}
	}
	form := url.Values{"src": {"main"}, "repository": {"widgets"}, "entry": {entryWork + it.ID}, "choice": {WorkAnswerDecline}, "text": {"not this week"}}
	if w := ownerPost(u, "/respond", form); w.Code != http.StatusSeeOther {
		t.Fatalf("respond: %d %s", w.Code, w.Body.String())
	}
	h.ticks(2, time.Minute)
	if it = h.item(it.ID); it.State != WorkDeclined || it.DoneBy != "owner" {
		t.Fatalf("item = %+v", it)
	}
	if body := owner(t, u, "/console/widgets"); strings.Contains(body, `class="work-items"`) {
		t.Fatal("a declined item is still shown as open")
	}
	var out strings.Builder
	PrintWork(&out, WorkViews(h.cfg, h.sup.st, "", true, h.clock.Now(), func(...string) bool { return false }))
	if !strings.Contains(out.String(), it.ID) || !strings.Contains(out.String(), "declined") {
		t.Fatalf("agentd items --work --all:\n%s", out.String())
	}
}

// workAgentOut is what a fake thread agent prints when it reads work in
// the owner's message.
func workAgentOut(answer, handTo string, work map[string]any) string {
	out, _ := json.Marshal(map[string]any{"answer": answer, "hand_to": handTo, "work": work})
	res, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "structured_output": json.RawMessage(out),
		"usage": map[string]any{"input_tokens": 12, "output_tokens": 300}, "total_cost_usd": 0.01})
	return string(res) + "\n"
}

// The thread agent's work opens the item: the owner's message is
// answered, not handed over, and the item, with the agent's done_when,
// goes to the session it named. Work whose done_when Go cannot check as
// written is refused.
func TestThreadAgentWorkOpensItem(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	h.alive("a", "a works")
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 1, workAgentOut("a takes it; Go holds it until the tag exists.", "a", map[string]any{
		"title": "Tag v3.38.2", "brief": "Tag the release.", "done_when": map[string]any{"kind": "forge", "ref": "v3.38.2", "state": "tagged"}}))
	if err := reply(h, "widgets", "Tag v3.38.2 today."); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		h.alive("a", "a works "+itoa(i))
		h.ticks(1, time.Minute)
	}
	in := h.instruction(t)
	it := h.sup.st.Work[workID(in.ID)]
	if it == nil || it.Session != "a" || it.DoneWhen.Ref != "v3.38.2" || in.Session != "" || in.Replied.IsZero() {
		t.Fatalf("instruction = %+v; item = %+v", in, it)
	}
	if typed := h.typed("a"); len(typed) != 1 || !strings.Contains(typed[0], it.ID) {
		t.Fatalf("typed into a: %q", typed)
	}
	x := agentInput(t, dir, 1)
	if len(x.Running) == 0 {
		t.Fatalf("the agent was given no running sessions: %+v", x)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args.1"))
	if !strings.Contains(string(args), "done_when") || !strings.Contains(string(args), "Go supervises and can reach") {
		t.Fatal("the agent was not told of work items or which sessions it can reach")
	}

	h2 := sessionsHarness(t)
	claudeAnswers(t, dir, 0, workAgentOut("Done.", "", map[string]any{"title": "Read it", "brief": "Read it.", "done_when": map[string]any{"kind": "file", "path": "../../etc/passwd", "contains": "root"}}))
	if err := reply(h2, "widgets", "Read the file."); err != nil {
		t.Fatal(err)
	}
	h2.ticks(4, time.Minute)
	if in := h2.instruction(t); in.Failed == "" || !strings.Contains(in.Failed, "done_when file") || len(h2.sup.st.Work) != 0 {
		t.Fatalf("instruction = %+v", in)
	}
}
