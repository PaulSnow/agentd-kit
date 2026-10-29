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
func (h *harness) askWork(text string, w ThreadWork, handTo string) *WorkItem {
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

func deployWork(page string) ThreadWork {
	return ThreadWork{Title: "Deploy v3.38.2", Brief: "Deploy v3.38.2 to production, step 1b.", DoneWhen: EndCheck{Kind: DoneURL, URL: page, Contains: "3.38.2"}}
}

// The owner's instruction is a work item: the session Go starts for it
// (#65) is assigned it and typed its id, its items.json holding the item
// and the owner's words; the session naming it acknowledges it, and only
// the check of done_when closes it. A claim the check refutes is posted back to the session and
// the owner, and closes nothing.
func TestWorkItemClosedOnlyByItsCheck(t *testing.T) {
	h := sessionsHarness(t)
	page := &versionPage{version: "3.38.1"}
	url := page.serve(t)
	it := h.askWork("Deploy v3.38.2 to production now.", deployWork(url), "")
	in := h.sup.st.Instructions[it.Instruction]
	if it.State != WorkAssigned || it.Started || it.Session == "" || it.Session != in.Session || !in.Started {
		t.Fatalf("item = %+v", it)
	}
	name := it.Session
	h.ticks(1, time.Minute)
	h.alive(name, "launched\n"+idleScreen)
	h.ticks(2, time.Minute)
	if typed := h.typed(name); len(typed) != 1 || !strings.HasPrefix(typed[0], itemPointerHead+it.ID) {
		t.Fatalf("typed = %q", typed)
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
	h.ticks(3, time.Minute)
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
	told := 0
	for _, l := range h.typed(name) {
		if strings.HasPrefix(l, itemRefutedHead) {
			told++
		}
	}
	if told != 1 {
		t.Fatalf("told %d times: %q", told, h.typed(name))
	}

	// The deploy lands: the next check closes it, with its evidence.
	page.set("3.38.2")
	h.ticks(6, time.Minute)
	if it = h.item(it.ID); it.State != WorkDone || it.DoneBy != "agentd" || !strings.Contains(it.Evidence, `contains "3.38.2"`) || !strings.Contains(it.Evidence, "checked") {
		t.Fatalf("item = %+v", it)
	}
	steps := strings.Join(h.workSteps(it.ID), " ")
	if steps != "open assign check typed ack working claim check escalate told check" {
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
	// The session started anew is briefed with the item and the owner's
	// words.
	h.ticks(1, time.Minute)
	brief, err := os.ReadFile(filepath.Join(h.dir, it.Session, "BRIEF.md"))
	if err != nil || !strings.HasPrefix(string(brief), briefItem) || !strings.Contains(string(brief), it.ID) || !strings.Contains(string(brief), "Deploy v3.38.2.") || !strings.Contains(string(brief), "WAITING for go 1b") {
		t.Fatalf("brief = %q, %v", brief, err)
	}
}

// An item handed to a running session is typed into it with its id; one
// the session does not name within ack_within is typed again once, then
// given to a session started for it.
func TestWorkItemUnacknowledgedRetypedThenReassigned(t *testing.T) {
	h := sessionsHarness(t)
	h.alive("a", "a works")
	it := h.askWork("Bump the version to 3.38.2.", ThreadWork{Title: "Bump the version", Brief: "Bump it.", DoneWhen: EndCheck{Kind: DoneOwner}}, "a")
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
	it := h.askWork("Go 1b.", ThreadWork{Title: "Go 1b", Brief: "Deploy 1b.", DoneWhen: EndCheck{Kind: DoneOwner}}, "laptop-deploy")
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
	it := h.askWork("Tag v3.38.2.", ThreadWork{Title: "Tag it", Brief: "Tag v3.38.2.", DoneWhen: EndCheck{Kind: DoneForge, Ref: "v3.38.2", State: "tagged"}}, "")
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
	it := h.askWork("Write the release notes.", ThreadWork{Title: "Release notes", Brief: "Write them.", DoneWhen: EndCheck{Kind: DoneOwner}}, "a")
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
	if docs := readInstructions(t, h.sup.cfg.Sessions["a"].Workdir); len(docs) != 2 || docs[1].Text != "Use the short form." {
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
	it := h.askWork("Make the logo bigger.", ThreadWork{Title: "Bigger logo", Brief: "Make it bigger.", DoneWhen: EndCheck{Kind: DoneOwner}}, "")
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
	it := h.askWork("Deploy v3.38.2 to production now.", ThreadWork{Title: "Deploy v3.38.2", Brief: "Deploy it.", DoneWhen: EndCheck{Kind: DoneForge, Ref: "v3.38.2", State: "tagged"}}, "")
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
	if it == nil || it.Session != "a" || it.DoneWhen.Ref != "v3.38.2" || in.Session != "a" {
		t.Fatalf("instruction = %+v; item = %+v", in, it)
	}
	if typed := h.typed("a"); len(typed) != 2 || !strings.Contains(typed[1], it.ID) {
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

// A new item never goes to a session that wrote DONE or is stopped, even
// handed to it: it goes to a running session on the same issue, or to a
// session started for it (#68).
func TestNewItemNeverGoesToFinishedSession(t *testing.T) {
	for _, c := range []string{"done", "stopped", "status DONE"} {
		t.Run(c, func(t *testing.T) {
			h := sessionsHarness(t)
			w1 := h.startSession("issue-1", KindWork, 1)
			h.ticks(2, time.Minute)
			h.alive(w1, "w1 works")
			h.ticks(1, time.Minute)
			switch c {
			case "done":
				h.finish(w1)
				h.alive(w1, "w1 is done")
				h.ticks(1, time.Minute)
				if st := h.state(w1); st != Done {
					t.Fatalf("state = %v", st)
				}
			case "stopped":
				tr := h.sup.track(w1)
				tr.State, tr.Stopped = Stopped, "its spend cap"
			case "status DONE":
				// DONE written, the track not yet seen it.
				h.finish(w1)
			}
			it := h.askWork("Ship the follow-up.", ThreadWork{Title: "Ship the follow-up", Brief: "Ship it.", DoneWhen: EndCheck{Kind: DoneOwner}}, w1)
			h.ticks(1, time.Minute)
			it = h.item(it.ID)
			for _, step := range h.workItemAssigns(it.ID) {
				if step == w1 {
					t.Fatalf("the item went to %s, which is %s: item = %+v", w1, c, it)
				}
			}
			if it.Session == "" || !it.Started {
				t.Fatalf("no session started for it: item = %+v", it)
			}
			// Its message goes with it: w1 is not reopened, nor sent it.
			h.alive(w1, "w1 pane\n"+idleScreen)
			h.ticks(2, time.Minute)
			if in := h.sup.st.Instructions[it.Instruction]; in.Session != it.Session || h.count(KindAction, ActReopen, w1) != 0 || h.count(KindAction, ActInstruct, w1) != 0 {
				t.Fatalf("the message went to %s (%+v); reopened %d, instructed %d", in.Session, h.routeOf(in.ID), h.count(KindAction, ActReopen, w1), h.count(KindAction, ActInstruct, w1))
			}
		})
	}
	// A running session on the item's issue takes it.
	h := sessionsHarness(t)
	w1 := h.startSession("issue-1", KindWork, 1)
	w2 := h.startSession("issue-1-b", KindWork, 1)
	h.ticks(2, time.Minute)
	h.alive(w1, "w1 works")
	h.alive(w2, "w2 works")
	h.ticks(1, time.Minute)
	h.finish(w1)
	h.alive(w1, "w1 is done")
	h.ticks(1, time.Minute)
	it := h.askWork("Ship the follow-up.", ThreadWork{Title: "Ship the follow-up", Brief: "Ship it.", Issue: 1, DoneWhen: EndCheck{Kind: DoneOwner}}, w1)
	if it = h.item(it.ID); it.Session != w2 || it.Started {
		t.Fatalf("not given to %s, on the same issue: item = %+v", w2, it)
	}
	// Its DONE, never naming the item, is its own issue's, not a claim.
	h.status(w2, "# s\n\nissue 1 part b shipped\n\nDONE\n")
	h.alive(w2, "w2 is done")
	h.ticks(3, time.Minute)
	if steps := h.workSteps(it.ID); contains(steps, WorkStepClaim) {
		t.Fatalf("claimed: steps %v", steps)
	}
	if it = h.item(it.ID); len(it.Tried) == 0 || it.Tried[0] != w2 || !strings.Contains(it.Notes[0].Text, "without ever naming the item") {
		t.Fatalf("not given back: item = %+v", it)
	}
}

// workItemAssigns are the sessions item id was assigned to, in order.
func (h *harness) workItemAssigns(id string) []string {
	var out []string
	for _, e := range h.events() {
		if e.Kind == KindWorkItem && e.Work.ID == id && e.Work.Step == WorkStepAssign {
			out = append(out, e.Work.Session)
		}
	}
	return out
}

// A DONE that is not this assignment's claims nothing: one written
// before the item was assigned, or one whose only done line names another
// item; the item is given back (#68).
func TestClaimRefusedWhenDoneOlderOrForOtherItem(t *testing.T) {
	for _, c := range []string{"older", "other"} {
		t.Run(c, func(t *testing.T) {
			h := sessionsHarness(t)
			h.alive("a", "a works")
			it := h.askWork("Bump the version to 3.38.2.", ThreadWork{Title: "Bump the version", Brief: "Bump it.", DoneWhen: EndCheck{Kind: DoneOwner}}, "a")
			if it.Session != "a" {
				t.Fatalf("item = %+v", it)
			}
			// The message and the item are typed into a.
			h.alive("a", "a works\n"+idleScreen)
			h.ticks(2, time.Minute)
			if in := h.sup.st.Instructions[it.Instruction]; in.Delivered.IsZero() {
				t.Fatalf("not delivered: %+v", in)
			}
			path := filepath.Join(h.dir, "a", "STATUS.md")
			switch c {
			case "older":
				h.status("a", "# Status\n\nall of it shipped\n\nDONE\n")
				at := it.Assigned.Add(-time.Hour)
				if err := os.Chtimes(path, at, at); err != nil {
					t.Fatal(err)
				}
			case "other":
				h.status("a", "# Status\n\nWdeadbeef done: the other one shipped\n\nDONE\n")
				at := it.Assigned.Add(time.Minute)
				if err := os.Chtimes(path, at, at); err != nil {
					t.Fatal(err)
				}
			}
			h.alive("a", "a is done")
			h.ticks(3, time.Minute)
			for _, step := range h.workSteps(it.ID) {
				if step == WorkStepClaim {
					t.Fatalf("claimed: steps %v", h.workSteps(it.ID))
				}
			}
			it = h.item(it.ID)
			want := "DONE predates the assignment"
			if c == "other" {
				want = "DONE names Wdeadbeef, not this item"
			}
			if len(it.Tried) == 0 || it.Tried[0] != "a" || len(it.Notes) == 0 || !strings.Contains(it.Notes[0].Text, want) {
				t.Fatalf("not reopened with %q: item = %+v", want, it)
			}
		})
	}
}

// Work the thread agent hands to a finished session whose tmux runs is
// not handed over: Go starts a session for it, and the finished one is
// not reopened (#68).
func TestWorkHandedToFinishedSessionStartsOne(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	w1 := h.startSession("issue-1", KindWork, 1)
	h.ticks(2, time.Minute)
	h.alive(w1, "w1 works")
	h.ticks(1, time.Minute)
	h.finish(w1)
	h.alive(w1, "w1 is done\n"+idleScreen)
	h.ticks(1, time.Minute)
	out, _ := json.Marshal(map[string]any{"answer": "Handed to " + w1 + ".", "hand_to": w1, "work": map[string]any{"title": "Ship step 1b", "brief": "Ship it.", "done_when": map[string]any{"kind": "owner"}}})
	res, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "structured_output": json.RawMessage(out), "usage": map[string]any{"input_tokens": 12, "output_tokens": 300}, "total_cost_usd": 0.01})
	claudeAnswers(t, dir, 0, string(res)+"\n")
	if err := reply(h, "widgets", "Ship step 1b too."); err != nil {
		t.Fatal(err)
	}
	h.ticks(4, time.Minute)
	in := h.instruction(t)
	it := h.sup.st.Work[workID(in.ID)]
	if it == nil || it.Session == "" || it.Session == w1 || !in.Started || in.Session != it.Session || in.Failed != "" {
		t.Fatalf("item = %+v, instruction = %+v", it, in)
	}
	if n := h.count(KindAction, ActReopen, w1); n != 0 {
		t.Fatalf("%s reopened %d times", w1, n)
	}
	if told := h.messages("The thread agent named " + w1 + ", which has finished and takes no new work; agentd opened " + it.ID); len(told) != 1 {
		t.Fatalf("told %q", told)
	}
}

// At the repository's max, an item handed to a finished session waits,
// saying so: the finished session is not reopened with its message, nor
// given the item; the owner's next step offers no finished session
// either (#68).
func TestWorkMessageAtMaxNotReopened(t *testing.T) {
	h := sessionsHarness(t)
	w1 := h.startSession("issue-1", KindWork, 1)
	w2 := h.startSession("issue-2", KindWork, 2)
	h.ticks(2, time.Minute)
	h.alive(w1, "w1 works")
	h.alive(w2, "w2 works")
	h.ticks(1, time.Minute)
	h.finish(w1)
	h.alive(w1, "w1 is done")
	h.ticks(1, time.Minute)
	it := h.askWork("Ship the follow-up.", ThreadWork{Title: "Ship the follow-up", Brief: "Ship it.", DoneWhen: EndCheck{Kind: DoneOwner}}, w1)
	h.alive(w1, "w1 pane\n"+idleScreen)
	h.ticks(3, time.Minute)
	it = h.item(it.ID)
	in := h.sup.st.Instructions[it.Instruction]
	if n := h.count(KindAction, ActReopen, w1) + h.count(KindAction, ActInstruct, w1); n != 0 || contains(h.workItemAssigns(it.ID), w1) {
		t.Fatalf("%s reopened or instructed %d times; assigns %v", w1, n, h.workItemAssigns(it.ID))
	}
	if !strings.Contains(in.Waiting, it.ID) {
		t.Fatalf("instruction = %+v", in)
	}
	h.sup.emit("", Event{Kind: KindRoute, Route: &RoutePayload{Instruction: in.ID, Step: RouteFail, Reason: "a failure"}})
	if d := nextDecision(h.sup.cfg, h.sup.st, h.sup.st.Instructions[in.ID], h.clock.Now()); d != nil && contains(decisionOptions(*d), nextSend+w1) {
		t.Fatalf("next step offers %s: %+v", w1, d)
	}
}

// An item never waits on a session Go cannot type into (#70): a new one
// passes it by for a started session, and one it holds that agentd will
// not bring back is given back, the note saying why. A session gone that
// the restart policy brings back keeps the item it took.
func TestItemReassignedWhenTmuxGone(t *testing.T) {
	// gone makes w1 blocked on the owner with its tmux session gone.
	gone := func(h *harness, w1, status string) {
		t.Helper()
		h.status(w1, status)
		h.tmux.alive[w1] = false
		h.ticks(1, time.Minute)
		if st := h.state(w1); st != BlockedOnOwner {
			t.Fatalf("state = %v", st)
		}
	}
	work := ThreadWork{Title: "Ship the follow-up", Brief: "Ship it.", DoneWhen: EndCheck{Kind: DoneOwner}}
	t.Run("new item", func(t *testing.T) {
		h := sessionsHarness(t)
		w1 := h.startSession("issue-1", KindWork, 1)
		h.ticks(2, time.Minute)
		h.alive(w1, "w1 works")
		h.ticks(1, time.Minute)
		gone(h, w1, "# s\n\n## Questions\n- Which version?\n")
		it := h.askWork("Ship the follow-up.", work, w1)
		h.ticks(1, time.Minute)
		it = h.item(it.ID)
		if contains(h.workItemAssigns(it.ID), w1) {
			t.Fatalf("the item went to %s, whose tmux session is gone: item = %+v", w1, it)
		}
		if it.Session == "" || it.Session == w1 || !it.Started {
			t.Fatalf("no session started for it: item = %+v", it)
		}
	})
	for _, c := range []string{"typed", "acknowledged"} {
		t.Run("held "+c, func(t *testing.T) {
			h := sessionsHarness(t)
			w1 := h.startSession("issue-1", KindWork, 1)
			h.ticks(2, time.Minute)
			h.alive(w1, "w1 works")
			h.ticks(1, time.Minute)
			it := h.askWork("Ship the follow-up.", work, w1)
			h.alive(w1, "w1 works on")
			h.ticks(1, time.Minute)
			if it = h.item(it.ID); it.Session != w1 {
				t.Fatalf("not given to %s: item = %+v", w1, it)
			}
			status := "# s\n\n## Questions\n- Which version?\n"
			if c == "acknowledged" {
				status = "# s\n\n## Now\n- " + it.ID + ": shipping\n\n## Questions\n- Which version?\n"
				h.status(w1, status)
				h.ticks(1, time.Minute)
				if st := h.item(it.ID).State; st == WorkAssigned {
					t.Fatalf("not acknowledged: %s", st)
				}
			}
			gone(h, w1, status)
			h.ticks(1, time.Minute)
			it = h.item(it.ID)
			if !contains(it.Tried, w1) || len(it.Notes) == 0 {
				t.Fatalf("not given back: item = %+v", it)
			}
			note := it.Notes[0].Text
			if !strings.Contains(note, w1+" cannot be typed into") || !strings.Contains(note, "blocked on the owner") || !strings.Contains(note, "not restart") {
				t.Fatalf("note = %q", note)
			}
			if it.Session == "" || it.Session == w1 || !it.Started {
				t.Fatalf("no session started for it: item = %+v", it)
			}
		})
	}
	t.Run("restarted", func(t *testing.T) {
		// Gone with no Questions: the restart brings it back, and the item
		// it took stays with it.
		h := sessionsHarness(t)
		w1 := h.startSession("issue-1", KindWork, 1)
		h.ticks(2, time.Minute)
		h.alive(w1, "w1 works")
		h.ticks(1, time.Minute)
		it := h.askWork("Ship the follow-up.", work, w1)
		h.status(w1, "# s\n\n## Now\n- "+it.ID+": shipping\n")
		h.alive(w1, "w1 works on")
		h.ticks(1, time.Minute)
		h.tmux.alive[w1] = false
		h.ticks(2, time.Minute)
		if it = h.item(it.ID); it.Session != w1 || len(it.Tried) != 0 {
			t.Fatalf("taken off %s, which the restart brings back: item = %+v", w1, it)
		}
	})
	t.Run("takesItem", func(t *testing.T) {
		h := sessionsHarness(t)
		if !h.sup.takesItem("a") {
			t.Fatal("a, running, refused")
		}
		h.tmux.alive["b"] = false // configured, never seen, no tmux
		if h.sup.takesItem("b") {
			t.Fatal("b, with no tmux session, takes an item")
		}
		for _, c := range []string{"parked", "stopped", "crash-looping", "blocked"} {
			tr := h.sup.track("a")
			*tr = Track{}
			switch c {
			case "parked":
				tr.Parked = true
			case "stopped":
				tr.State, tr.Stopped = Stopped, "its spend cap"
			case "crash-looping":
				tr.State = CrashLooping
			case "blocked":
				tr.State, tr.LastObs = BlockedOnOwner, &ObservePayload{State: string(BlockedOnOwner)}
			}
			if h.sup.takesItem("a") {
				t.Fatalf("a, %s, takes an item", c)
			}
		}
	})
}

// issueSeven is widgets' issue #7 as the forge answers it alone.
const issueSeven = "/projects/example-org%2Fwidgets/issues/7"

// umbrellaWork is work whose done_when is widgets' #7 closed, and the
// forge that answers #7 opened.
func (h *harness) umbrellaWork() (*WorkItem, *fakeForgeAPI) {
	h.t.Helper()
	it := h.askWork("Fix the child of #7.", ThreadWork{Title: "Fix the child", Brief: "Fix it.", DoneWhen: EndCheck{Kind: DoneForge, Ref: "#7", State: "closed"}}, "")
	api := &fakeForgeAPI{gitlab: map[string]string{"=" + issueSeven: `{"state":"opened"}`}}
	h.sup.api = api
	return it, api
}

// answerOf is agentd's answer to the owner's response text, "" for none.
func (h *harness) answerOf(text string) string {
	h.t.Helper()
	in := h.instructionText(h.t, text)
	for _, e := range h.events() {
		if e.Kind == KindRoute && e.Route.Step == RouteAnswered && e.Route.Instruction == in.ID && e.Route.Agent != nil {
			return e.Route.Agent.Answer
		}
	}
	return ""
}

// othersFor are the sessions other than name that hold item it.
func (h *harness) othersFor(it *WorkItem, name string) []string {
	var out []string
	for n, p := range h.sup.st.Runtime {
		if n != name && (p.Item == it.ID || p.Owner == it.Instruction) {
			out = append(out, n)
		}
	}
	return out
}

// A session that finishes with a done line its check refutes is not
// given back: its claim goes to the owner on the item, once, with the
// line and what the check saw, and no other session is started for it
// (W0f446a9c ran three in 40 seconds). The owner's not-done then gives
// it to a new session.
func TestDoneLineFailingCheckAsksOwner(t *testing.T) {
	h := sessionsHarness(t)
	it, _ := h.umbrellaWork()
	name := it.Session
	h.ticks(1, time.Minute)
	h.alive(name, "fixing")
	h.status(name, "# Status\n\n## Now\n"+it.ID+": fixing\n")
	h.ticks(2, time.Minute)
	h.status(name, "# Status\n\n"+it.ID+" done: the child is fixed; #7 stays open as an umbrella\n\nDONE\n")
	h.alive(name, "all done")
	h.ticks(10, time.Minute)
	it = h.item(it.ID)
	if it.State == WorkDone || it.State == WorkOpen || it.Session != name || len(it.Tried) != 0 {
		t.Fatalf("item = %+v", it)
	}
	if others := h.othersFor(it, name); len(others) != 0 {
		t.Fatalf("sessions started for the item again: %v", others)
	}
	ns := h.noticesAbout(it.ID)
	if len(ns) != 1 || !strings.Contains(ns[0], "the child is fixed; #7 stays open as an umbrella") || !strings.Contains(ns[0], "the check says: it is opened") {
		t.Fatalf("notices = %q", ns)
	}
	if opts := strings.Join(it.workOptions(), ","); opts != "done,not-done,reassign,decline" {
		t.Fatalf("options = %s", opts)
	}
	ld, ok := workDecision(it)
	if !ok || !strings.Contains(ld.CameBack, "the child is fixed") || !strings.Contains(ld.Checked, "it is opened") {
		t.Fatalf("decision = %+v, %v", ld, ok)
	}

	// Not done: the finished session's item goes to a new one.
	re := &EntryRe{ID: entryWork + it.ID, Kind: "work", At: it.Opened}
	if err := recordInstructionRe(h.cfg, NewLogWriter(h.buf), h.clock.Now(), []string{"widgets"}, "", "", "ui", "the child is not fixed", re, &EntryAnswer{Choice: WorkAnswerNotDone}, nil); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, time.Minute)
	if a := h.answerOf("the child is not fixed"); !strings.Contains(a, "open again") {
		t.Fatalf("answer = %q", a)
	}
	if it = h.item(it.ID); len(it.Tried) != 1 || it.Tried[0] != name || it.Session == name {
		t.Fatalf("after not done: item = %+v", it)
	}
}

// A done line whose check finds the forge not yet closed is checked again
// each poll_interval while the owner's card waits, one call for the
// issue alone each time, so a close the forge shows late (We0ce4f4f)
// closes the item, with no other session and no card left.
func TestStaleForgeClosedAfterClaim(t *testing.T) {
	h := sessionsHarness(t)
	it, api := h.umbrellaWork()
	name := it.Session
	h.ticks(1, time.Minute)
	h.alive(name, "fixing")
	h.status(name, "# Status\n\n"+it.ID+" done: closed #7\n\nDONE\n")
	h.ticks(4, time.Minute)
	if it = h.item(it.ID); it.State == WorkDone || it.Refuted == "" || len(it.Tried) != 0 {
		t.Fatalf("item = %+v", it)
	}
	api.set(func() { api.gitlab["="+issueSeven] = `{"state":"closed"}` })
	h.ticks(2, h.cfg.PollInterval.Duration+time.Minute)
	it = h.item(it.ID)
	if it.State != WorkDone || it.DoneBy != "agentd" || !strings.Contains(it.Evidence, "it is closed") || len(it.Tried) != 0 {
		t.Fatalf("item = %+v", it)
	}
	if others := h.othersFor(it, name); len(others) != 0 {
		t.Fatalf("sessions started for the item again: %v", others)
	}
	if _, ok := workDecision(it); ok {
		t.Fatal("the owner's card is still up on a closed item")
	}
	checks, asks := 0, 0
	for _, e := range h.events() {
		if e.Kind == KindWorkItem && e.Work.ID == it.ID && e.Work.Step == WorkStepCheck {
			checks++
		}
	}
	for _, c := range api.callList() {
		if strings.Contains(c, "/issues") && strings.Contains(c, "widgets") {
			if c != "gitlab "+issueSeven {
				t.Fatalf("a check read more than the issue: %q", c)
			}
			asks++
		}
	}
	if checks < 2 || asks < checks {
		t.Fatalf("%d checks logged, %d calls for #7: %q", checks, asks, api.callList())
	}
}

// The owner's done closes an item whatever its state, its check unmet:
// assigned, or worked on.
func TestOwnerDoneAcceptedOnAssignedItem(t *testing.T) {
	for _, working := range []bool{false, true} {
		h := sessionsHarness(t)
		it, _ := h.umbrellaWork()
		if working {
			h.ticks(1, time.Minute)
			h.alive(it.Session, "fixing")
			h.status(it.Session, "# Status\n\n## Now\n"+it.ID+": fixing\n")
			h.ticks(2, time.Minute)
			if it = h.item(it.ID); it.State != WorkWorking {
				t.Fatalf("item = %+v", it)
			}
		} else if it.State != WorkAssigned {
			t.Fatalf("item = %+v", it)
		}
		re := &EntryRe{ID: entryWork + it.ID, Kind: "work", At: it.Opened}
		if err := recordInstructionRe(h.cfg, NewLogWriter(h.buf), h.clock.Now(), []string{"widgets"}, "", "", "ui", "it stays open, an umbrella", re, &EntryAnswer{Choice: WorkAnswerDone}, nil); err != nil {
			t.Fatal(err)
		}
		h.ticks(2, time.Minute)
		if it = h.item(it.ID); it.State != WorkDone || it.DoneBy != "owner" || !strings.Contains(it.Evidence, "it stays open, an umbrella") {
			t.Fatalf("working %v: item = %+v", working, it)
		}
		if a := h.answerOf("it stays open, an umbrella"); a != it.ID+" is done, as you said." {
			t.Fatalf("answer = %q", a)
		}
	}
}
