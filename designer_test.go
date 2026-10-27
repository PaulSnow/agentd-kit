package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// designerHarness is sessionsHarness with the design step on, the
// models set so the designer runs the strongest, and widgets' design
// policy auto.
func designerHarness(t *testing.T, auto bool) *harness {
	t.Helper()
	h := sessionsHarness(t)
	h.cfg.Jobs.Designer.Off = false
	h.cfg.Policies.Models = ModelsPolicy{Work: "sonnet", Reviewer: "sonnet", Judge: "opus"}
	w := h.cfg.Repositories["widgets"]
	w.Policy.Design = DesignRules{Auto: auto, SpendLimitUSD: 5}
	h.cfg.Repositories["widgets"] = w
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	return h
}

// designOut is what a fake designer prints: a brief with design and
// acceptance, a question, done_when and a rating.
func designOut(step string, deploys bool, spend float64) string {
	out, _ := json.Marshal(map[string]any{
		"brief": map[string]any{
			"exists":       []string{"paint.go: Paint(color) paints one wall"},
			"missing":      []string{"a way to paint every wall"},
			"design":       []string{step, "agentd-style test in paint_test.go"},
			"where":        "widgets, a branch cut from main",
			"out_of_scope": []string{"the fence"},
			"acceptance":   []string{"every wall of the shed is red", "./bin/check passes"},
		},
		"questions": []string{"Which red?"},
		"done_when": map[string]any{"kind": "file", "path": "SHED.md", "contains": "red"},
		"rating":    map[string]any{"production": false, "deployment": deploys, "spend_usd": spend, "why": "a change to the repository alone"},
	})
	res, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "structured_output": json.RawMessage(out),
		"usage": map[string]any{"input_tokens": 4000, "output_tokens": 900}, "total_cost_usd": 0.25})
	return string(res) + "\n"
}

// askDesigned is the owner asking widgets' console for work no session
// covers, which the fake thread agent reads as work: the item Go opens to
// be designed.
func (h *harness) askDesigned(dir, text string) *WorkItem {
	h.t.Helper()
	claudeAnswers(h.t, dir, 1, agentWork("No session covers it: agentd designs it first.", "Paint the shed", "Paint the shed red."))
	if err := reply(h, "widgets", text); err != nil {
		h.t.Fatal(err)
	}
	h.ticks(3, time.Minute)
	in := h.instruction(h.t)
	it := h.sup.st.Work[workID(in.ID)]
	if it == nil {
		h.t.Fatalf("no item was opened: instruction = %+v", in)
	}
	return it
}

// respondDesign is the owner's answer on an item's design entry, through
// the page.
func (h *harness) respondDesign(id, choice, text string) error {
	src := newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a")
	src.now = h.clock.Now
	var a *AnswerPayload
	if choice != "" {
		a = &AnswerPayload{Choice: choice}
	}
	return src.respond("widgets", entryDesign+id, "", text, a, nil)
}

// The owner's work is designed before any session starts: the designer
// runs on the strongest model, given the owner's words; its brief is
// posted as the design entry and the item waits, designed, with no
// session; the owner's words alone are a change, which the designer
// takes for a second round with the design before; on go, Go starts the
// work session with the design as its BRIEF.md and the owner's message,
// and a reviewer of its branch is given the design as its standard.
func TestDesignerBriefPostsAsDecisionAndStartsOnGo(t *testing.T) {
	dir := fakeClaude(t)
	h := designerHarness(t, false)
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 2, designOut("Add PaintAll to paint.go, calling Paint for each wall", false, 0))
	it := h.askDesigned(dir, "Paint the shed red, all of it.")
	h.ticks(2, time.Minute)
	it = h.item(it.ID)
	if it.State != WorkDesigned || it.Design == nil || it.Round != 1 || len(h.sup.st.Runtime) != 0 {
		t.Fatalf("item = %+v, runtime %v", it, h.sup.st.Runtime)
	}
	if it.DoneWhen.Kind != DoneFile || it.DesignCost == nil || it.DesignCost.CostUSD != 0.25 {
		t.Fatalf("done_when %+v, cost %+v", it.DoneWhen, it.DesignCost)
	}
	args := readFake(t, dir, "args.2")
	if !strings.Contains(args, "--model\nopus\n") || !strings.Contains(args, "agentd") {
		t.Fatalf("the designer's call: %s", args)
	}
	if in := readFake(t, dir, "in.2"); !strings.Contains(in, "Paint the shed red, all of it.") || !strings.Contains(in, `"agent_reading"`) {
		t.Fatalf("the designer's input: %s", in)
	}
	in := h.sup.st.Instructions[it.Instruction]
	if in.Session != "" || !strings.Contains(in.Waiting, "waits on your go") {
		t.Fatalf("instruction = %+v", in)
	}

	// The design is an open entry of widgets' console, the brief shown.
	src := newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a")
	v, err := (&UI{sources: []*uiSource{src}}).consoleView("widgets", h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	var entry *uiEntry
	for i, e := range v.Entries {
		if e.Entry == entryDesign+it.ID {
			entry = &v.Entries[i]
		}
	}
	if entry == nil || !entry.Open || entry.Decision == nil {
		t.Fatalf("no open design entry: %+v", v.Entries)
	}
	for _, want := range []string{"> Paint the shed red, all of it.", "## Design", "1. Add PaintAll", "## Acceptance", "Which red?", "SHED.md contains", "$0.25"} {
		if !strings.Contains(entry.Decision.Context, want) {
			t.Fatalf("the design lacks %q:\n%s", want, entry.Decision.Context)
		}
	}
	if got := decisionOptions(entry.Decision.Decision); strings.Join(got, ",") != "go,change,decline" {
		t.Fatalf("options = %v", got)
	}
	// The item's own entry takes no done or re-assign while designed.
	if err := recordInstructionRe(h.cfg, NewLogWriter(h.buf), h.clock.Now(), []string{"widgets"}, "", "", "ui", "", &EntryRe{ID: entryWork + it.ID, Kind: "work"}, &EntryAnswer{Choice: WorkAnswerDone}, nil); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, time.Minute)
	if it = h.item(it.ID); it.State != WorkDesigned {
		t.Fatalf("done on a designed item changed it: %+v", it)
	}

	// Words alone are a change: round 2, given the design before.
	claudeAnswers(t, dir, 3, designOut("Add PaintAll to paint.go, in signal red", false, 0))
	if err := h.respondDesign(it.ID, "", "Use signal red."); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, time.Minute)
	if it = h.item(it.ID); it.State != WorkDesigned || it.Round != 2 || !strings.Contains(it.Design.Brief.Design[0], "signal red") || it.DesignCost.CostUSD != 0.5 {
		t.Fatalf("after the change: item = %+v", it)
	}
	if in := readFake(t, dir, "in.3"); !strings.Contains(in, `"owner_change":"Use signal red."`) || !strings.Contains(in, `"previous_design"`) {
		t.Fatalf("round 2's input: %s", in)
	}

	// Go: the work session starts, briefed with the design.
	if err := h.respondDesign(it.ID, DesignGo, ""); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, time.Minute)
	it = h.item(it.ID)
	in = h.sup.st.Instructions[it.Instruction]
	if it.ApprovedBy != "owner" || it.Session == "" || !it.Started || in.Session != it.Session || !in.Started {
		t.Fatalf("after go: item = %+v, instruction = %+v", it, in)
	}
	name := it.Session
	if p := h.sup.st.Runtime[name]; p.Item != it.ID || p.Owner != in.ID {
		t.Fatalf("start = %+v", p)
	}
	brief, err := os.ReadFile(filepath.Join(h.dir, name, "BRIEF.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# " + it.ID + ": Paint the shed", "in signal red", "## Questions\n\n- Which red?", "## How agentd tracks it", `"ID done: EVIDENCE"`} {
		if !strings.Contains(string(brief), want) {
			t.Fatalf("BRIEF.md lacks %q:\n%s", want, brief)
		}
	}
	if !strings.HasPrefix(string(brief), "# "+it.ID) {
		t.Fatalf("BRIEF.md does not open with the design:\n%s", brief)
	}
	if calls := claudeCalls(t, dir); calls != 3 {
		t.Fatalf("claude was called %d times", calls)
	}

	// Its reviewer reviews against the design.
	branch := h.sup.st.Runtime[name].Branch
	rev := h.startSession(branch, KindReviewer, 0)
	h.ticks(2, time.Minute)
	rb, err := os.ReadFile(filepath.Join(h.dir, rev, "BRIEF.md"))
	if err != nil || !strings.Contains(string(rb), `"standard":"# `+it.ID) {
		t.Fatalf("the reviewer's brief: %s, %v", rb, err)
	}
}

// A repository whose policy sets design.auto goes on without a go when
// the design is contained, telling the owner; one that deploys waits on
// the owner's go.
func TestDesignAutoProceedsWhenContained(t *testing.T) {
	dir := fakeClaude(t)
	h := designerHarness(t, true)
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 2, designOut("Add PaintAll", false, 1))
	it := h.askDesigned(dir, "Paint the shed red.")
	h.ticks(3, time.Minute)
	it = h.item(it.ID)
	if it.ApprovedBy != "agentd" || it.Session == "" || !strings.Contains(it.ApprovedNote, "design.auto") {
		t.Fatalf("a contained design did not go on: %+v", it)
	}
	if ns := h.noticesAbout(it.ID); len(ns) != 1 || !strings.Contains(ns[0], "without your go") {
		t.Fatalf("notices = %q", ns)
	}
	// The design is still posted, answered by agentd's go.
	src := newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a")
	src.catchUp()
	des := src.designEntries(h.sup.cfg, "widgets", func(...string) bool { return false })
	if len(des) != 1 || des[0].open || des[0].Answer == nil || des[0].Answer.Choice != DesignGo {
		t.Fatalf("design entries = %+v", des)
	}

	// One that deploys waits.
	h2 := designerHarness(t, true)
	dir2 := fakeClaude(t)
	h2.ticks(1, time.Minute)
	claudeAnswers(t, dir2, 2, designOut("Ship the paint", true, 0))
	it2 := h2.askDesigned(dir2, "Ship the paint.")
	h2.ticks(3, time.Minute)
	if it2 = h2.item(it2.ID); it2.State != WorkDesigned || it2.Session != "" || len(h2.sup.st.Runtime) != 0 {
		t.Fatalf("a deploying design went on: %+v", it2)
	}
	if ok, why := it2.Design.contained(DesignRules{Auto: true}); ok || why != "it deploys" {
		t.Fatalf("contained = %v, %q", ok, why)
	}
	if ok, _ := (&WorkDesign{Brief: &DesignBrief{}, Rating: &DesignRating{SpendUSD: 6}}).contained(DesignRules{Auto: true, SpendLimitUSD: 5}); ok {
		t.Fatal("a spend over the limit is contained")
	}
}

// A designer that fails is run again after designRetry; after
// designTries failures the owner is asked, with the thread agent's brief
// standing in; decline answers the owner's message and starts nothing.
func TestDesignerFailuresThenDecline(t *testing.T) {
	dir := fakeClaude(t)
	h := designerHarness(t, false)
	h.ticks(1, time.Minute)
	bad := claudeResult(`{"brief":{"exists":[],"missing":[],"design":[],"where":"","out_of_scope":[],"acceptance":[]},"questions":[],"done_when":{"kind":"owner"},"rating":{"production":false,"deployment":false,"spend_usd":0,"why":""}}`) + "\n"
	claudeAnswers(t, dir, 2, bad)
	claudeAnswers(t, dir, 3, bad)
	it := h.askDesigned(dir, "Paint the shed red.")
	h.ticks(5, time.Minute)
	if it = h.item(it.ID); it.State != WorkDesigned || it.DesignFails != designTries || it.Design != nil || !strings.Contains(it.DesignError, "no design") {
		t.Fatalf("after %d failures: %+v", designTries, it)
	}
	var fails []time.Time
	for _, e := range h.events() {
		if e.Kind == KindWorkItem && e.Work.Step == WorkStepDesign && e.Work.Design.Error != "" {
			fails = append(fails, e.Time)
		}
	}
	if len(fails) != designTries || fails[1].Sub(fails[0]) < designRetry {
		t.Fatalf("failures at %v", fails)
	}
	src := newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a")
	src.catchUp()
	des := src.designEntries(h.sup.cfg, "widgets", func(...string) bool { return false })
	if len(des) != 1 || !des[0].open || !strings.Contains(des[0].Decision.Context, "The designer failed") || !strings.Contains(des[0].Decision.Context, "Paint the shed red.") {
		t.Fatalf("design entries = %+v", des)
	}
	if err := h.respondDesign(it.ID, DesignDecline, "not now"); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	it = h.item(it.ID)
	in := h.sup.st.Instructions[it.Instruction]
	if it.State != WorkDeclined || in.Replied.IsZero() || !strings.Contains(in.Reply, "declined") || len(h.sup.st.Runtime) != 0 {
		t.Fatalf("after decline: item = %+v, instruction = %+v", it, in)
	}
	if calls := claudeCalls(t, dir); calls != 3 {
		t.Fatalf("claude was called %d times", calls)
	}
}

// The designer runs the configured model, else the strongest of
// policies.models, else opus.
func TestDesignerModel(t *testing.T) {
	cfg := &Config{}
	if m := designerJob(cfg).Model; m != DefaultDesignerModel {
		t.Fatalf("model = %q", m)
	}
	cfg.Policies.Models = ModelsPolicy{Work: "claude-sonnet-5-5", Reviewer: "claude-fable-5-1", Judge: "claude-opus-5-5"}
	if m := designerJob(cfg).Model; m != "claude-fable-5-1" {
		t.Fatalf("model = %q", m)
	}
	cfg.Jobs.Designer.Model = "haiku"
	if m := designerJob(cfg).Model; m != "haiku" {
		t.Fatalf("model = %q", m)
	}
}
