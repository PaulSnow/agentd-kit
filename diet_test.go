package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// The session diet (#87): a hard context cap, a brief size cap, and no
// designer for work the loop already describes.

// A work session whose context reaches the hard cap mid-turn is cut once
// for its transcript, with why "cap" and its progress in the fresh facts;
// the cut is a console line and agentd status's last cut.
func TestHardCapCutsWorkSessionMidTurn(t *testing.T) {
	h, name, id := workHarness(t)
	h.status(name, workLeft)
	h.alive(name, runningScreen)
	h.workTranscript(name, id, promptLine("u1", "Fix the flake.", h.clock.Now())+usageLine("m1", 1, 150000, 0, 5, false))
	h.ticks(4, time.Minute)
	rots := h.rotations(name)
	if len(rots) != 1 {
		t.Fatalf("rotations = %d, want 1", len(rots))
	}
	if a := rots[0]; a.HardCap != 150000 || a.Reason != "context 150001 over hard cap 150000" || a.Checkpoint != "cap:"+id {
		t.Fatalf("rotate = %+v", a)
	}
	f := h.replacedFacts(name)
	if f.Why != "cap" || f.Item != "K #20 poll" || f.Instruction != "Fix the flake." {
		t.Fatalf("facts = %+v", f)
	}
	// The fresh transcript is measured from scratch; the old one is not
	// cut again.
	h.ticks(4, time.Minute)
	if n := len(h.rotations(name)); n != 1 {
		t.Fatalf("rotations = %d after the cut, want 1", n)
	}
	want := name + " was cut mid-turn at 150k tokens (cap 150k); a fresh context resumes from STATUS.md."
	found := 0
	for _, e := range h.sup.feedEntries() {
		if strings.HasPrefix(e.Key, "cut:"+name+":context:") && e.Text == want {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("console lines for the cut: %d, want 1: %+v", found, h.sup.feedEntries())
	}
	if r := h.statusRow(name, h.clock.Now()); r.LastCut != want {
		t.Fatalf("last cut = %q", r.LastCut)
	}
}

// Under the hard cap a turn that still runs is not cut.
func TestHardCapNotReachedNotCut(t *testing.T) {
	h, name, id := workHarness(t)
	h.status(name, workLeft)
	h.alive(name, runningScreen)
	h.workTranscript(name, id, promptLine("u1", "Fix the flake.", h.clock.Now())+usageLine("m1", 1, 149998, 0, 5, false))
	h.ticks(4, time.Minute)
	if n := len(h.rotations(name)); n != 0 {
		t.Fatalf("cut under the hard cap: %d", n)
	}
}

// A DONE session whose delivery is not recorded is not cut, even over the
// hard cap.
func TestHardCapSparesUndeliveredDone(t *testing.T) {
	h, name, id := workHarness(t)
	h.status(name, doneStatus)
	h.alive(name, idleScreen)
	at := h.clock.Now()
	h.workTranscript(name, id, promptLine("u1", "Fix the spin test.", at)+summaryLine("u2", "m1", "Fixed; DONE.", at, 160000))
	h.ticks(4, time.Minute)
	if n := len(h.rotations(name)); n != 0 {
		t.Fatalf("cut an undelivered DONE session: %d", n)
	}
}

// A work session that is DONE with its delivery recorded has nothing to
// resume: over the hard cap, mid-turn as the transcript reads, it is not
// cut or relaunched, and no cut is pending for it (#87).
func TestDoneSessionNotCut(t *testing.T) {
	h, name, id := workHarness(t)
	h.status(name, doneStatus)
	h.alive(name, idleScreen)
	h.delivered(name)
	h.workTranscript(name, id, promptLine("u1", "Fix the spin test.", h.clock.Now())+usageLine("m1", 1, 200000, 0, 5, false))
	h.ticks(4, time.Minute)
	if rots := h.rotations(name); len(rots) != 0 {
		t.Fatalf("cut a DONE, delivered session: %+v", rots)
	}
	for _, c := range h.tmux.calls {
		if c.op == "replace" && c.name == name {
			t.Fatalf("relaunched a DONE session: %+v", c)
		}
	}
	tr := h.sup.Tracks()[name]
	if tr.State != Done || len(tr.Cuts) != 0 {
		t.Fatalf("state %s, cuts %+v", tr.State, tr.Cuts)
	}
	if u, _ := tr.current(); u.Context != 200001 {
		t.Fatalf("context = %d, want 200001", u.Context)
	}
	sess := h.sup.cfg.Sessions[name]
	ev := Evidence{Alive: true, Status: StatusEvidence{Exists: true, Done: true}}
	if h.sup.rotationPending(name, sess, tr, ev, Done, h.clock.Now()) {
		t.Fatal("a cut is pending for a DONE session")
	}
}

// A Working work session just over the hard cap is still cut, once, with
// the cap prompt.
func TestWorkingSessionCut(t *testing.T) {
	h, name, id := workHarness(t)
	h.status(name, workLeft)
	h.alive(name, runningScreen)
	h.workTranscript(name, id, promptLine("u1", "Fix the flake.", h.clock.Now())+usageLine("m1", 1, 150000, 0, 1, false))
	h.ticks(4, time.Minute)
	rots := h.rotations(name)
	if len(rots) != 1 || rots[0].Context != 150001 || rots[0].HardCap != 150000 || rots[0].Checkpoint != "cap:"+id {
		t.Fatalf("rotations = %+v, want one cut at 150001", rots)
	}
	if f := h.replacedFacts(name); f.Why != "cap" {
		t.Fatalf("facts = %+v, want why cap", f)
	}
	h.ticks(4, time.Minute)
	if n := len(h.rotations(name)); n != 1 {
		t.Fatalf("rotations = %d after the cut, want 1", n)
	}
}

// Only a Working or Idle work session is cut: one blocked on the owner,
// stopped, parked, failed, starting, usage-limited or DONE over the cap is
// not, and no cut is pending for it (#87).
func TestHardCapOnlyWorkingOrIdle(t *testing.T) {
	h, name, _ := workHarness(t)
	sess := h.sup.cfg.Sessions[name]
	now := h.clock.Now()
	for _, c := range []struct {
		why   string
		state State
		set   func(*Track, *Evidence)
		cut   bool
	}{
		{"working", Working, nil, true},
		{"idle", Idle, nil, true},
		{"blocked on the owner", BlockedOnOwner, nil, false},
		{"stopped", Stopped, func(t *Track, _ *Evidence) { t.Stopped = "budget reached" }, false},
		{"stopped, still read idle", Idle, func(t *Track, _ *Evidence) { t.Stopped = "budget reached" }, false},
		{"parked", CrashLooping, func(t *Track, _ *Evidence) { t.Parked = true }, false},
		{"parked, still read idle", Idle, func(t *Track, _ *Evidence) { t.Parked = true }, false},
		{"failed", Failed, nil, false},
		{"starting", Starting, nil, false},
		{"usage-limited", UsageLimited, func(t *Track, _ *Evidence) { t.Limited = true }, false},
		{"done", Done, func(_ *Track, ev *Evidence) { ev.Status.Done = true }, false},
		{"DONE in STATUS.md, read idle", Idle, func(_ *Track, ev *Evidence) { ev.Status.Done = true }, false},
	} {
		tr := newTrack()
		tr.Transcript, tr.Measured, tr.Launched = "launched", "launched", now.Add(-time.Hour)
		tr.Usage = map[string]UsagePayload{"launched": {Context: 200000}}
		ev := Evidence{Alive: true}
		if c.set != nil {
			c.set(tr, &ev)
		}
		if got := capEligible(sess, tr, ev, c.state); got != c.cut {
			t.Errorf("%s: capEligible = %v, want %v", c.why, got, c.cut)
		}
		if got := h.sup.rotationPending(name, sess, tr, ev, c.state, now); got != c.cut {
			t.Errorf("%s: rotationPending = %v, want %v", c.why, got, c.cut)
		}
		if !c.cut && h.sup.rotate(name, sess, tr, ev, c.state, now) {
			t.Errorf("%s: cut", c.why)
		}
	}
	if n := len(h.rotations(name)); n != 0 {
		t.Fatalf("rotations = %d, want 0", n)
	}
}

// Live mode never cuts on a guessed transcript, and a coordinator or chat
// session is never cut.
func TestHardCapSparesGuessAndCoordinator(t *testing.T) {
	h, _, _ := workHarness(t)
	over := func(kind string, guessed bool) bool {
		tr := newTrack()
		tr.Transcript, tr.Measured = "launched", "launched"
		if guessed {
			tr.Transcript, tr.Measured = "", "newest"
		}
		tr.Usage = map[string]UsagePayload{tr.Measured: {Context: 200000, Guessed: guessed}}
		return h.sup.overCap(Session{Kind: kind}, tr)
	}
	if !over(KindWork, false) {
		t.Fatal("a launched work session over the cap is not over it")
	}
	if over(KindWork, true) {
		t.Fatal("live mode cuts on a guess")
	}
	if over("", false) || over(KindChat, false) {
		t.Fatal("a coordinator or chat session is cut")
	}
}

// A hard cap at or below the threshold is refused at load; 0 is off.
func TestHardCapAtOrBelowThresholdRefused(t *testing.T) {
	for _, c := range []struct {
		cap int64
		ok  bool
	}{{120000, false}, {100000, false}, {-1, false}, {0, true}, {120001, true}} {
		cfg := DefaultConfig()
		cfg.Policies.Context.HardCap = c.cap
		err := cfg.finish(t.TempDir())
		if got := err == nil || !strings.Contains(err.Error(), "hard_cap"); got != c.ok {
			t.Errorf("hard_cap %d: %v", c.cap, err)
		}
	}
	if cfg := DefaultConfig(); cfg.Policies.Context.HardCap != 150000 || cfg.Policies.Brief.MaxBytes != 32768 {
		t.Fatalf("defaults = %+v, %+v", cfg.Policies.Context, cfg.Policies.Brief)
	}
}

// An oversize owner_work is cut under max_bytes: the head, the message,
// the agent's brief and the branch facts stay, the oldest records go
// first, and the marker names what was dropped.
func TestFitBriefCutsOversizeOwnerWork(t *testing.T) {
	head := "# W1: the design\n\nDo the thing as designed."
	var earlier []any
	for i := 0; i < 40; i++ {
		earlier = append(earlier, map[string]any{"text": strings.Repeat("e", 1000), "n": i})
	}
	var finished []any
	for i := 0; i < 10; i++ {
		finished = append(finished, map[string]any{"name": "old", "summary": strings.Repeat("f", 1000)})
	}
	facts := map[string]any{
		"branch": "owner/w1", "worktree": "/w/work", "repository": "widgets", "issue": 9,
		"owner_work": map[string]any{"title": "W1", "brief": "the agent's brief", "message": map[string]any{"text": "the owner's words"},
			"earlier": earlier, "finished": finished},
	}
	text, cut := fitBrief(head, facts, 16384)
	if cut == nil {
		t.Fatal("not cut")
	}
	body, marker, _ := strings.Cut(string(text), "[agentd cut this brief")
	if len(body) > 16384 || cut.Kept != len(body) || cut.Bytes <= 40000 {
		t.Fatalf("kept %d bytes (cut %+v)", len(body), cut)
	}
	for _, s := range []string{head, `"branch":"owner/w1"`, `"worktree":"/w/work"`, `"issue":9`, "the agent's brief", "the owner's words"} {
		if !strings.Contains(body, s) {
			t.Fatalf("dropped %q", s)
		}
	}
	if strings.Contains(body, `"n":0,`) || !strings.Contains(body, `"n":39`) {
		t.Fatal("earlier records not dropped oldest first")
	}
	if !strings.Contains(marker, "dropped finished, earlier[0..") || strings.Count(string(text), "\n[agentd cut") != 1 {
		t.Fatalf("marker = %q", marker)
	}
	if small, cut := fitBrief(head, map[string]any{"branch": "b"}, 16384); cut != nil || !strings.HasPrefix(string(small), head) {
		t.Fatalf("a brief that fits was cut: %+v", cut)
	}
}

// An assembled brief over the cap is cut at the end, its marker naming
// the cut.
func TestCapBriefCutsAtEnd(t *testing.T) {
	text, cut := capBrief([]byte(strings.Repeat("x", 5000)), 1000)
	if cut == nil || cut.Bytes != 5000 || cut.Kept > 1000 {
		t.Fatalf("cut = %+v", cut)
	}
	body, _, ok := strings.Cut(string(text), "[agentd cut this brief from 5000")
	if !ok || len(body) > 1000 {
		t.Fatalf("text = %q", text)
	}
	if same, cut := capBrief([]byte("short"), 1000); cut != nil || string(same) != "short" {
		t.Fatal("a brief that fits was cut")
	}
}

// A brief cut at provision is a console line naming the session, the
// sizes and what was dropped.
func TestBriefCutIsConsoleLine(t *testing.T) {
	h, name, _ := workHarness(t)
	tr := h.sup.track(name)
	tr.Cuts = append(tr.Cuts, SessionCut{At: h.clock.Now(), Action: ActProvision, Brief: &BriefCut{Bytes: 61000, Kept: 32000, Dropped: []string{"finished", "earlier[0..7]"}}})
	want := name + "'s brief was cut from 61k to 32k bytes at its provision: dropped finished, earlier[0..7]."
	for _, e := range h.sup.feedEntries() {
		if strings.HasPrefix(e.Key, "cut:"+name+":brief:") && e.Text == want {
			return
		}
	}
	t.Fatalf("no console line %q", want)
}

// The provision of a session whose facts are over the cap records the
// cut on its action, folded into the session's cuts.
func TestProvisionRecordsBriefCut(t *testing.T) {
	h := sessionsHarness(t)
	h.cfg.Policies.Brief.MaxBytes = 600
	name := h.startSession("fix/spin", KindWork, 0)
	h.ticks(2, time.Minute)
	var cut *BriefCut
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Session == name && e.Act.Action == ActProvision && e.Act.BriefCut != nil {
			cut = e.Act.BriefCut
		}
	}
	if cut == nil {
		t.Skip("the harness's start facts fit 600 bytes")
	}
	if tr := h.sup.track(name); len(tr.Cuts) != 1 || tr.Cuts[0].Brief == nil {
		t.Fatalf("cuts = %+v", tr.Cuts)
	}
}

// The round's issue-answer item starts its session with no designer run,
// its facts carrying the finding, and its open says why.
func TestLoopIssueItemSkipsDesigner(t *testing.T) {
	h, _ := loopHarness(t)
	deferBoth(t, h)
	for _, e := range h.deferredEntries(widgetsCase) {
		h.answerAsks(e, FindingIssue, "", "!1", false)
		h.ticks(2, 5*time.Minute)
	}
	h.ticks(2, 5*time.Minute)
	if len(h.sup.st.Work) != 1 {
		t.Fatalf("items opened: %d: %+v", len(h.sup.st.Work), h.sup.st.Work)
	}
	for _, it := range h.sup.st.Work {
		if it.Designer || it.State == WorkDesigning || it.State == WorkDesigned {
			t.Fatalf("item designed: %+v", it)
		}
		in := h.sup.st.Instructions[it.Instruction]
		if in == nil || in.Session == "" {
			t.Fatalf("no session started for %s: %+v", it.ID, in)
		}
		b, err := os.ReadFile(ownerWorkPath(h.cfg, "widgets", in.ID))
		if err != nil {
			t.Fatal(err)
		}
		var doc ownerWorkDoc
		if err := json.Unmarshal(b, &doc); err != nil || doc.Finding == nil || doc.Finding.Request != "!1" || doc.Finding.Title == "" {
			t.Fatalf("facts' finding = %+v (%v)", doc.Finding, err)
		}
	}
	notes := 0
	for _, e := range h.events() {
		if e.Kind == KindWorkItem && e.Work.Step == WorkStepOpen && strings.HasPrefix(e.Work.Note, "no designer: the owner answered issue on finding") {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("open notes saying why = %d", notes)
	}
}

// Work on an issue agentd opened as a finding's follow-up is described by
// that finding; work on any other issue is not.
func TestFollowUpIssueWorkSkipsDesigner(t *testing.T) {
	h, _ := loopHarness(t)
	h.sup.st.IssuesOpened = map[string]OpenedIssue{
		"finding:" + widgetsCase + ":F1":              {Number: 42},
		"finding:gitlab/example-org/widgets-two#3:F1": {Number: 43},
		"issue:w1": {Number: 44},
	}
	h.sup.st.Cases[widgetsCase] = &Case{Rounds: []RoundRecord{{N: 1, Findings: []Finding{{ID: "F1", Title: "Add a catalogue", Consequence: "no catalogue", Paths: []string{"a.go"}}}}}}
	in := &Instruction{}
	in.ID, in.Repositories = "i1", []string{"widgets"}
	why, f := h.sup.loopDescribes(in, ThreadWork{Title: "follow-up", Issue: 42})
	if !strings.Contains(why, "#42 is agentd's follow-up issue of finding F1") || f == nil || f.Title != "Add a catalogue" || f.Request != widgetsCase || f.Paths[0] != "a.go" {
		t.Fatalf("why %q, finding %+v", why, f)
	}
	for _, n := range []int{43, 44, 45, 0} {
		if why, _ := h.sup.loopDescribes(in, ThreadWork{Title: "other", Issue: n}); why != "" {
			t.Fatalf("#%d: %q", n, why)
		}
	}
}
