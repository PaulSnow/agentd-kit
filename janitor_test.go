package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// janitorHarness is widgets with issues #4 and #5 and request !9 listed,
// close, rebase and issue allowed, and the janitor due from midnight.
func janitorHarness(t *testing.T, shadow bool) (*harness, *fakeForgeAPI, string) {
	t.Helper()
	dir := fakeClaude(t)
	h, api := indexHarness(t)
	h.cfg.Jobs.Janitor = JanitorConfig{Enabled: true, At: "00:00", Repositories: []string{"widgets"}}
	api.gitlab["widgets/merge_requests/9"] = `{"state":"opened","updated_at":"2026-10-04T00:00:00Z"}`
	h.allow(api, "close", "rebase", "issue")
	// The index is the live run's.
	h.cfg.Jobs.Janitor.Enabled = false
	h.ticks(2, time.Minute)
	h.cfg.Jobs.Janitor.Enabled = true
	if shadow {
		// A shadow run reads its own index events.
		for _, e := range h.events() {
			if e.Kind == KindIndex {
				e.Mode = ModeShadow
				h.appendEvent(e)
			}
		}
		h.restart(true)
		h.sup.api = api
	}
	return h, api, dir
}

type jp map[string]any

func janitorOut(props ...jp) string {
	if props == nil {
		props = []jp{}
	}
	b, _ := json.Marshal(map[string]any{"summary": "One stale request.", "proposals": props})
	return jobOut(string(b))
}

func jobOut(structured string) string {
	return `{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":` + structured + `,"usage":{"input_tokens":100,"output_tokens":20}}`
}

func closeNine() jp {
	return jp{"action": JanitorCloseRequest, "target": "!9", "reason": "Superseded.", "cites": []string{"!9"}}
}

func (h *harness) proposals() []*JanitorRecord {
	var out []*JanitorRecord
	for _, id := range sortedKeys(h.sup.st.Janitor) {
		out = append(out, h.sup.st.Janitor[id])
	}
	return out
}

func (h *harness) janitorRuns() []*JanitorPayload {
	var out []*JanitorPayload
	for _, e := range h.events() {
		if e.Janitor != nil && e.Janitor.Step == JanitorRan {
			out = append(out, e.Janitor)
		}
	}
	return out
}

func (h *harness) oneProposal() *JanitorRecord {
	h.t.Helper()
	ps := h.proposals()
	if len(ps) != 1 {
		h.t.Fatalf("proposals = %+v, runs %+v", ps, h.janitorRuns())
	}
	return ps[0]
}

// An approved close closes the request once, through the console's entry,
// and the owner's answer is answered with the outcome.
func TestJanitorApprovedCloseOnce(t *testing.T) {
	h, _, dir := janitorHarness(t, false)
	claudeAnswers(t, dir, 0, janitorOut(closeNine()))
	h.ticks(3, time.Minute)
	if n := claudeCalls(t, dir); n != 1 {
		t.Fatalf("%d calls in a day", n)
	}
	r := h.oneProposal()
	if !r.open() || len(h.forge.closed) != 0 {
		t.Fatalf("acted without an answer: %+v %v", r, h.forge.closed)
	}
	if ms := h.messages("Janitor"); len(ms) != 1 || !strings.Contains(ms[0], entryJanitor+r.ID) {
		t.Fatalf("messages = %q", ms)
	}
	es := BuildDigest(h.cfg, h.sup.st, nil, h.clock.Now())
	if len(es) != 1 || es[0].Kind != DigestJanitor || es[0].Entry != entryJanitor+r.ID || es[0].Since.IsZero() {
		t.Fatalf("digest = %+v", es)
	}
	src := newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a")
	v, err := (&UI{sources: []*uiSource{src}}).consoleView("widgets", h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	shown := false
	for _, e := range v.Entries {
		shown = shown || e.Entry == entryJanitor+r.ID && e.Open && e.Decision != nil
	}
	if !shown {
		t.Fatalf("no open entry: %+v", v.Entries)
	}
	if err := h.replyEntry(entryJanitor+r.ID, "burn"); err == nil {
		t.Fatal("an answer not offered was taken")
	}
	if err := h.replyEntry(entryJanitor+r.ID, JanitorApprove); err != nil {
		t.Fatal(err)
	}
	// A second click before the pass is told it is settled.
	h.clock.advance(time.Second)
	if err := h.replyEntry(entryJanitor+r.ID, JanitorApprove); err != nil {
		t.Fatal(err)
	}
	h.ticks(4, time.Minute)
	if len(h.forge.closed) != 1 || h.forge.closed[0] != "!9" {
		t.Fatalf("closed = %v; %+v", h.forge.closed, h.oneProposal())
	}
	if r := h.oneProposal(); r.End != JanitorDone || r.Outcome != "closed !9" {
		t.Fatalf("proposal = %+v", r)
	}
	if h.count(KindAction, ActClose, "") != 1 {
		t.Fatal("the close is not one logged action")
	}
	for _, in := range h.sup.st.Instructions {
		if in.Replied.IsZero() || in.Session != "" {
			t.Fatalf("not answered by Go: %+v", in)
		}
	}
	if err := h.replyEntry(entryJanitor+r.ID, JanitorApprove); err == nil {
		t.Fatal("a settled proposal took an answer")
	}
	h.restart(false)
	h.ticks(2, time.Minute)
	if len(h.forge.closed) != 1 {
		t.Fatalf("closed again after a restart: %v", h.forge.closed)
	}
}

// What Go refuses: no citation, a record not given, a target not cited or
// of another kind, an issue no request merged for, a session not finished,
// a follow-up that names a session.
func TestJanitorRefusals(t *testing.T) {
	h, _, dir := janitorHarness(t, false)
	h.cfg.Sessions["nightly-worker"] = Session{}
	claudeAnswers(t, dir, 0, janitorOut(
		jp{"action": JanitorCloseRequest, "target": "!9", "reason": "x", "cites": []string{}},
		jp{"action": JanitorCloseRequest, "target": "!9", "reason": "x", "cites": []string{"!9", "#77"}},
		jp{"action": JanitorCloseRequest, "target": "!8", "reason": "x", "cites": []string{"!9"}},
		jp{"action": JanitorCloseIssue, "target": "!9", "reason": "x", "cites": []string{"!9"}},
		jp{"action": JanitorFollowUp, "target": "!9", "reason": "x", "title": "T", "body": "ask nightly-worker", "cites": []string{"!9"}},
		jp{"action": JanitorAskOwner, "target": "!9", "reason": "x", "cites": []string{"!9"}},
		jp{"action": "merge_request", "target": "!9", "reason": "x", "cites": []string{"!9"}},
	))
	h.ticks(1, time.Minute)
	if len(h.proposals()) != 0 {
		t.Fatalf("taken: %+v", h.oneProposal())
	}
	rs := h.janitorRuns()
	if len(rs) != 1 || len(rs[0].Refused) != 7 {
		t.Fatalf("runs = %+v", rs)
	}
	for i, want := range []string{"cites no record", "not given", "no record it was given", "target is a request", "names a session", "asks nothing", "does not allow"} {
		if !strings.Contains(rs[0].Refused[i], want) {
			t.Errorf("refusal %d = %q, want %q", i, rs[0].Refused[i], want)
		}
	}
}

// A rejected proposal is not made again while its records stand, however
// old they get, and is once the request changed.
func TestJanitorRejectedNotRepeated(t *testing.T) {
	h, api, dir := janitorHarness(t, false)
	claudeAnswers(t, dir, 0, janitorOut(closeNine()))
	h.ticks(1, time.Minute)
	r := h.oneProposal()
	if err := h.replyEntry(entryJanitor+r.ID, JanitorReject); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	if r := h.oneProposal(); r.End != JanitorDeclined {
		t.Fatalf("proposal = %+v", r)
	}
	h.clock.advance(24 * time.Hour)
	h.ticks(2, time.Minute)
	h.oneProposal()
	if n := claudeCalls(t, dir); n != 2 {
		t.Fatalf("%d calls in two days", n)
	}
	if in := readFake(t, dir, "in.2"); !strings.Contains(in, `"standing":["close_request !9"]`) {
		t.Fatalf("input = %s", in)
	}
	api.gitlab["widgets/merge_requests?state=opened"] = strings.Replace(api.gitlab["widgets/merge_requests?state=opened"], `"updated_at":"2026-10-04`, `"updated_at":"2026-10-23`, 1)
	h.cfg.Jobs.Janitor.Enabled = false
	h.ticks(3, time.Minute) // the listing
	h.cfg.Jobs.Janitor.Enabled = true
	h.clock.advance(24 * time.Hour)
	h.ticks(2, time.Minute)
	if len(h.proposals()) != 2 || len(h.forge.closed) != 0 {
		t.Fatalf("not made again after a change: %+v", h.proposals())
	}
}

// An open proposal whose records changed is withdrawn, and one approved
// after its target changed on the forge, or left it, is not carried out.
func TestJanitorChangedTarget(t *testing.T) {
	for name, body := range map[string]string{"changed": `{"state":"opened","updated_at":"2026-10-20T00:00:00Z"}`, "merged": `{"state":"merged","updated_at":"2026-10-04T00:00:00Z"}`} {
		t.Run(name, func(t *testing.T) {
			h, api, dir := janitorHarness(t, false)
			claudeAnswers(t, dir, 0, janitorOut(closeNine()))
			h.ticks(1, time.Minute)
			api.gitlab["widgets/merge_requests/9"] = body
			if err := h.replyEntry(entryJanitor+h.oneProposal().ID, JanitorApprove); err != nil {
				t.Fatal(err)
			}
			h.ticks(3, time.Minute)
			if r := h.oneProposal(); r.End != JanitorFailed || len(h.forge.closed) != 0 {
				t.Fatalf("proposal = %+v, closed %v", r, h.forge.closed)
			}
		})
	}
	h, api, dir := janitorHarness(t, false)
	claudeAnswers(t, dir, 0, janitorOut())
	claudeAnswers(t, dir, 1, janitorOut(closeNine()))
	h.ticks(1, time.Minute)
	id := h.oneProposal().ID
	api.gitlab["widgets/merge_requests?state=opened"] = `[]`
	h.ticks(3, time.Minute)
	h.clock.advance(24 * time.Hour)
	h.ticks(2, time.Minute)
	if r := h.oneProposal(); r.ID != id || r.End != janitorWithdrawn {
		t.Fatalf("proposal = %+v", r)
	}
	if err := h.replyEntry(entryJanitor+id, JanitorApprove); err == nil {
		t.Fatal("a withdrawn proposal took an answer")
	}
}

// Over the budget no call is made, and the console says so once a day.
func TestJanitorBudget(t *testing.T) {
	h, _, dir := janitorHarness(t, false)
	h.cfg.Jobs.Janitor.BudgetTokens = 100
	h.ticks(3, time.Minute)
	if n := claudeCalls(t, dir); n != 0 {
		t.Fatalf("%d calls over the budget", n)
	}
	if ms := h.messages("token budget"); len(ms) != 1 || !strings.Contains(ms[0], "100") {
		t.Fatalf("messages = %q", ms)
	}
}

// A failed call is tried again an hour later, three times in a day, and
// its tokens are charged.
func TestJanitorFailureRetried(t *testing.T) {
	h, _, dir := janitorHarness(t, false)
	claudeAnswers(t, dir, 0, jobOut(`{"nonsense":1}`))
	h.ticks(2, time.Minute)
	if n := claudeCalls(t, dir); n != 1 {
		t.Fatalf("%d calls at once", n)
	}
	for range [5]int{} {
		h.clock.advance(time.Hour)
		h.ticks(1, time.Minute)
	}
	if n := claudeCalls(t, dir); n != janitorTries {
		t.Fatalf("%d calls in a day", n)
	}
	if h.sup.st.JanitorRuns["widgets"].spent(h.clock.Now()) == 0 {
		t.Fatal("no spend kept")
	}
	charged := 0
	for _, s := range h.sup.st.Ledger.Spends {
		if s.Kind == SpendJanitor {
			charged++
		}
	}
	if charged != janitorTries {
		t.Fatalf("%d spends charged", charged)
	}
}

// A shadow run asks and logs its proposals, and posts and does nothing.
func TestJanitorShadow(t *testing.T) {
	h, _, dir := janitorHarness(t, true)
	claudeAnswers(t, dir, 0, janitorOut(closeNine()))
	before := len(h.messages("Janitor"))
	h.ticks(3, time.Minute)
	if r := h.oneProposal(); !r.open() {
		t.Fatalf("proposal = %+v", r)
	}
	if len(h.messages("Janitor")) != before || len(h.forge.closed) != 0 {
		t.Fatal("a shadow run posted or acted")
	}
}

// Where delivery.allow lacks the write, the action is neither offered nor
// taken; approved while it was allowed and not now, it is not done.
func TestJanitorNeedsDeliveryAllow(t *testing.T) {
	h, api, dir := janitorHarness(t, false)
	claudeAnswers(t, dir, 0, janitorOut(closeNine()))
	h.ticks(1, time.Minute)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Delivery.Allow = []string{"push", "open"}
	h.cfg.Repositories["widgets"] = w
	h.restart(false)
	h.sup.api = api
	if err := h.replyEntry(entryJanitor+h.oneProposal().ID, JanitorApprove); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, time.Minute)
	if r := h.oneProposal(); r.End != JanitorFailed || len(h.forge.closed) != 0 {
		t.Fatalf("proposal = %+v, closed %v", r, h.forge.closed)
	}
	if got := strings.Join(janitorActions(h.cfg, "widgets"), " "); got != "ask_owner end_session" {
		t.Fatalf("actions = %s", got)
	}
	h.clock.advance(24 * time.Hour)
	h.ticks(1, time.Minute)
	h.oneProposal()
}

// A stop between the start and the end is reported, never done again.
func TestJanitorCutShort(t *testing.T) {
	h, api, dir := janitorHarness(t, false)
	claudeAnswers(t, dir, 0, janitorOut(closeNine()))
	h.ticks(1, time.Minute)
	r := h.oneProposal()
	if err := h.replyEntry(entryJanitor+r.ID, JanitorApprove); err != nil {
		t.Fatal(err)
	}
	in := h.instruction(t)
	h.sup.emit("", Event{Kind: KindJanitor, Janitor: &JanitorPayload{Repository: "widgets", Step: JanitorStarted, ID: r.ID, Instruction: in.ID}})
	h.clock.advance(time.Minute)
	h.restart(false)
	h.sup.api = api
	h.sup.emit("", Event{Kind: KindStart, Start: &StartPayload{}})
	h.ticks(3, time.Minute)
	if r := h.oneProposal(); r.End != JanitorFailed || !strings.Contains(r.Outcome, "may or may not") || len(h.forge.closed) != 0 {
		t.Fatalf("proposal = %+v, closed %v", r, h.forge.closed)
	}
}

// An approved follow-up opens its issue, a rebase asks the forge, and a
// question takes the owner's words.
func TestJanitorOtherActions(t *testing.T) {
	h, _, dir := janitorHarness(t, false)
	claudeAnswers(t, dir, 0, janitorOut(
		jp{"action": JanitorFollowUp, "target": "!9", "reason": "Left over.", "title": "Cache the page", "body": "The request leaves it out.", "cites": []string{"!9"}},
		jp{"action": JanitorRebaseRequest, "target": "!9", "reason": "Stale.", "cites": []string{"!9"}},
		jp{"action": JanitorAskOwner, "target": "!9", "reason": "Yours.", "body": "Is it wanted?", "cites": []string{"!9"}}))
	h.ticks(1, time.Minute)
	if len(h.proposals()) != 3 {
		t.Fatalf("runs = %+v", h.janitorRuns())
	}
	for _, r := range h.proposals() {
		h.clock.advance(time.Second)
		if err := h.replyEntry(entryJanitor+r.ID, JanitorApprove); err != nil {
			t.Fatal(err)
		}
	}
	h.ticks(4, time.Minute)
	for _, r := range h.proposals() {
		if r.End != JanitorDone {
			t.Errorf("proposal = %+v", r)
		}
	}
	if len(h.forge.rebased) != 1 || len(h.forge.issues) != 1 || !strings.HasPrefix(h.forge.issues[0], "Cache the page\n") {
		t.Fatalf("rebased %v, issues %q", h.forge.rebased, h.forge.issues)
	}
}

// Only a finished session may be ended, checked again at the approval.
func TestJanitorEndSession(t *testing.T) {
	run := &janitorRun{in: janitorInput{Actions: []string{JanitorEndSession}}, screen: func(...string) int { return 0 }}
	p := JanitorProposal{Action: JanitorEndSession, Target: "s:w", Reason: "x", Cites: map[string]string{"s:w": "h"}}
	if why := run.refuses(p, janitorRecord{ID: "s:w", Kind: janitorSession}, true, nil); why != "the session is not finished" {
		t.Fatalf("why = %q", why)
	}
	if why := run.refuses(p, janitorRecord{ID: "s:w", Kind: janitorSession, Endable: true}, true, nil); why != "" {
		t.Fatalf("why = %q", why)
	}
	h, _, _ := janitorHarness(t, false)
	h.sup.janitorHere(JanitorRecord{JanitorProposal: JanitorProposal{ID: "J1", Action: JanitorEndSession, Target: "s:gone"}, Repository: "widgets"}, &Instruction{})
	for _, e := range h.events() {
		if e.Kind == KindSession {
			t.Fatalf("ended %+v", e)
		}
	}
}

// Reserved and excluded numbers are no records, an issue is one only once
// a request of it closed, and a record's age is no change.
func TestJanitorRecords(t *testing.T) {
	h, _, _ := janitorHarness(t, false)
	now := h.clock.Now()
	rs := h.sup.janitorRecords("widgets", now)
	if len(rs) != 1 || rs[0].ID != "!9" || rs[0].Age != 18 {
		t.Fatalf("records = %+v", rs)
	}
	if later := h.sup.janitorRecords("widgets", now.Add(48*time.Hour)); later[0].hash() != rs[0].hash() || later[0].Age == rs[0].Age {
		t.Fatalf("a day passing changed the record")
	}
	w := h.cfg.Repositories["widgets"]
	w.Policy.Scope.Reserved.Numbers = []int{9}
	h.cfg.Repositories["widgets"] = w
	if rs := h.sup.janitorRecords("widgets", now); len(rs) != 0 {
		t.Fatalf("records = %+v", rs)
	}
	if anyMerged([]string{"!3 merged things (closed without merging)"}) || !anyMerged([]string{"!3 x (merged)"}) {
		t.Fatal("merged misread")
	}
}

// The most proposals of a run, oldest target first.
func TestJanitorMaxProposals(t *testing.T) {
	old, young := janitorRecord{ID: "!1", Kind: janitorRequest}, janitorRecord{ID: "!2", Kind: janitorRequest}
	run := &janitorRun{repo: "widgets", in: janitorInput{Actions: []string{JanitorCloseRequest}, Max: 1, Records: []janitorRecord{old, young}}, screen: func(...string) int { return 0 }}
	p := &JanitorPayload{}
	raw := `{"summary":"s","proposals":[{"action":"close_request","target":"!2","reason":"r","cites":["!2"]},{"action":"close_request","target":"!1","reason":"r","cites":["!1"]}]}`
	if err := run.check(json.RawMessage(raw), p, time.Now()); err != nil || len(p.Proposals) != 1 || p.Proposals[0].Target != "!1" || len(p.Refused) != 1 {
		t.Fatalf("%v %+v", err, p)
	}
}

func TestJanitorConfig(t *testing.T) {
	for _, j := range []JanitorConfig{{At: "25:00"}, {BudgetTokens: -1}, {Repositories: []string{"nope"}}} {
		cfg := DefaultConfig()
		cfg.Jobs.Janitor = j
		var errs []error
		if cfg.checkJanitor(&errs); len(errs) != 1 {
			t.Errorf("%+v: %v", j, errs)
		}
	}
}

// The forge calls, as the CLIs are given them.
func TestJanitorForgeCalls(t *testing.T) {
	for _, forge := range []string{"gitlab", "github"} {
		dir := t.TempDir()
		t.Setenv("CALLS", dir+"/calls")
		shim(t, "glab", `echo "glab $*" >> "$CALLS"`)
		shim(t, "gh", `echo "gh $*" >> "$CALLS"`)
		f, r := ExecForge{Timeout: time.Minute}, Repository{Forge: forge, Project: "o/p"}
		for _, err := range []error{f.Close(r, "!3"), f.Close(r, "#4"), f.Rebase(r, "!3")} {
			if err != nil {
				t.Fatal(err)
			}
		}
		if f.Rebase(r, "#4") == nil || f.Close(r, "x") == nil {
			t.Fatal("a bad reference was taken")
		}
		want := "glab api --method PUT projects/o%2Fp/merge_requests/3 -f state_event=close\nglab api --method PUT projects/o%2Fp/issues/4 -f state_event=close\nglab api --method PUT projects/o%2Fp/merge_requests/3/rebase\n"
		if forge == "github" {
			want = "gh api --method PATCH repos/o/p/pulls/3 -f state=closed\ngh api --method PATCH repos/o/p/issues/4 -f state=closed\ngh api --method PUT repos/o/p/pulls/3/update-branch\n"
		}
		if got := readFake(t, dir, "calls"); got != want {
			t.Fatalf("%s: %s", forge, got)
		}
	}
}

// A target being worked takes a question alone.
func TestJanitorBusyTarget(t *testing.T) {
	run := &janitorRun{in: janitorInput{Actions: []string{JanitorCloseRequest, JanitorAskOwner}}, screen: func(...string) int { return 0 }}
	busy := janitorRecord{ID: "!1", Kind: janitorRequest, Busy: true}
	p := JanitorProposal{Action: JanitorCloseRequest, Target: "!1", Reason: "x", Body: "Is it?", Cites: map[string]string{"!1": "h"}}
	if why := run.refuses(p, busy, true, nil); why != "its target is being worked" {
		t.Fatalf("why = %q", why)
	}
	p.Action = JanitorAskOwner
	if why := run.refuses(p, busy, true, nil); why != "" {
		t.Fatalf("why = %q", why)
	}
}

// An approval that came from anywhere but the owner's page or command
// line, or with words withheld, does nothing, and the proposal stays open.
func TestJanitorAnswerOnlyOwners(t *testing.T) {
	h, _, dir := janitorHarness(t, false)
	claudeAnswers(t, dir, 0, janitorOut(closeNine()))
	h.ticks(1, time.Minute)
	id := h.oneProposal().ID
	for _, p := range []InstructionPayload{{From: "console"}, {From: "ui", Withheld: 1}} {
		p.ID, p.Route = "I"+p.From, true
		p.Re, p.Answer = &EntryRe{ID: entryJanitor + id}, &EntryAnswer{Choice: JanitorApprove}
		if !h.sup.routeJanitor(&Instruction{InstructionPayload: p}) {
			t.Fatal("not taken as the proposal's")
		}
		h.sup.Wait()
		if r := h.oneProposal(); !r.open() || len(h.forge.closed) != 0 {
			t.Fatalf("%+v: proposal = %+v, closed %v", p, r, h.forge.closed)
		}
	}
}

// A question that cites what already waits on the owner is not asked again.
func TestJanitorAsksNothingTwice(t *testing.T) {
	run := &janitorRun{in: janitorInput{Actions: []string{JanitorAskOwner}, Records: []janitorRecord{{ID: "!1", Kind: janitorRequest}, {ID: "l:!1", Kind: janitorDecision}}}, screen: func(...string) int { return 0 }}
	p := JanitorProposal{Action: JanitorAskOwner, Target: "!1", Reason: "x", Body: "Is it?", Cites: map[string]string{"!1": "h"}}
	if why := run.refuses(p, run.in.Records[0], true, nil); why != "" {
		t.Fatalf("why = %q", why)
	}
	p.Cites["l:!1"] = "h"
	if why := run.refuses(p, run.in.Records[0], true, nil); why != "it waits on the owner already" {
		t.Fatalf("why = %q", why)
	}
}

// A config file may allow the janitor's writes.
func TestJanitorConfigFileAllows(t *testing.T) {
	path := t.TempDir() + "/c.json"
	writeFile(t, path, `{"repositories": {"r": {"forge": "gitlab", "project": "a/b", "policy": {"delivery": {"allow": ["close", "rebase", "issue"]}}}}, "jobs": {"janitor": {"enabled": true}}}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := janitorActions(cfg, "r"); len(got) != 6 {
		t.Fatalf("actions = %v", got)
	}
	if m := janitorJob(cfg).Model; m != DefaultJanitorModel {
		t.Fatalf("model = %s", m)
	}
}

// No run before the repository is listed: it would see nothing and settle
// the day.
func TestJanitorWaitsForListing(t *testing.T) {
	dir := fakeClaude(t)
	h, api := indexHarness(t)
	h.cfg.Jobs.Janitor = JanitorConfig{Enabled: true, At: "00:00", Repositories: []string{"widgets"}}
	h.allow(api, "close")
	claudeAnswers(t, dir, 0, janitorOut(closeNine()))
	h.sup.Pass()
	h.sup.Wait()
	if len(h.janitorRuns()) != 0 {
		t.Fatalf("ran unlisted: %+v", h.janitorRuns()[0])
	}
	h.clock.advance(time.Minute)
	h.ticks(3, time.Minute)
	h.oneProposal()
}

// The console page shows a proposal's approve and reject, and its form
// records the owner's answer on it.
func TestJanitorPage(t *testing.T) {
	u, f, _ := attachmentFixture(t)
	e := Event{V: EventVersion, Time: time.Date(2026, 10, 24, 6, 30, 0, 0, time.UTC), Kind: KindJanitor, Mode: ModeLive, Janitor: &JanitorPayload{Repository: "widgets", Step: JanitorRan, Day: "2026-10-24",
		Proposals: []JanitorProposal{{ID: "J1", Action: JanitorCloseRequest, Target: "!9", Name: "Paint <b>it</b>", Reason: "Superseded by !12.", Cites: map[string]string{"!9": "h"}}}}}
	if err := NewLogWriter(f.coordBuf).Append(e); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/console/widgets", "/repo/widgets"} {
		body := get(u, "GET", path, pageAddr, "192.0.2.20:51000").Body.String()
		for _, want := range []string{`value="approve"`, `value="reject"`, "j:J1", "Superseded by !12.", "Paint &lt;b&gt;it&lt;/b&gt;"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", path, want)
			}
		}
	}
	w := multipartTo(u, "/respond", u.token, map[string]string{"src": "coord", "repository": "widgets", "entry": "j:J1", "choice": JanitorReject, "back": "/console/widgets"})
	if w.Code != 303 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	n := 0
	for _, in := range coordState(f).Instructions {
		if in.Re != nil && in.Re.ID == "j:J1" && in.From == "ui" && in.Answer != nil && in.Answer.Choice == JanitorReject {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d answers recorded", n)
	}
}

// Approved, a finished session is ended once and its place freed; one put
// back to work since the proposal is not.
func TestJanitorEndsFinishedSession(t *testing.T) {
	for _, back := range []bool{false, true} {
		dir := fakeClaude(t)
		h := sessionsHarness(t)
		w := h.startSession("issue-1", KindWork, 1)
		h.ticks(1, time.Minute)
		h.alive(w, "finished\n"+idleScreen)
		h.finish(w)
		h.ticks(2, time.Minute)
		h.sup.st.Tracks[w].Delivered = map[string]string{w + ":" + h.sup.cfg.Sessions[w].Cwd: "no MR.md: nothing to deliver"}
		h.cfg.Jobs.Janitor = JanitorConfig{Enabled: true, At: "00:00", Repositories: []string{"widgets"}}
		h.sup.cfg.Jobs.Janitor = h.cfg.Jobs.Janitor
		claudeAnswers(t, dir, 0, janitorOut(jp{"action": JanitorEndSession, "target": "s:" + w, "reason": "All done.", "cites": []string{"s:" + w}}))
		h.ticks(1, time.Minute)
		r := h.oneProposal()
		if back {
			h.status(w, "# s\n\n## Now\n- at work again\n")
			h.ticks(1, time.Minute)
		}
		if err := h.replyEntry(entryJanitor+r.ID, JanitorApprove); err != nil {
			t.Fatal(err)
		}
		h.ticks(3, time.Minute)
		_, running := h.sup.st.Runtime[w]
		if r := h.oneProposal(); running != back || (r.End == JanitorDone) == back {
			t.Fatalf("back %v: running %v, proposal %+v", back, running, r)
		}
	}
}
