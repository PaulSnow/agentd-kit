package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// doneStatus is a work session's progress at the end of its round.
const doneStatus = "# fix/spin\n\n| Item | State |\n|---|---|\n| K #8 spin | fixed |\n\n## Questions\n\nnone\n\nDONE\n"

// workHarness is a live harness with one work session agentd
// started, launched under a transcript agentd named: its name and the
// transcript's id.
func workHarness(t *testing.T) (*harness, string, string) {
	t.Helper()
	h := sessionsHarness(t)
	name := h.startSession("fix/spin", KindWork, 0)
	h.ticks(2, time.Minute) // provisioned, then found gone and launched
	id := ""
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Session == name && e.Act.Transcript != "" {
			id = e.Act.Transcript
		}
	}
	if id == "" {
		t.Fatal("the launch named no transcript")
	}
	return h, name, id
}

// workTranscript appends text to the work session's transcript id.
func (h *harness) workTranscript(name, id, text string) {
	h.t.Helper()
	appendFile(h.t, filepath.Join(TranscriptDir(h.cfg.TranscriptDir, filepath.Join(h.dir, name, "work")), id+".jsonl"), text)
}

// delivered records the work session's branch delivered, with the event
// mark-delivered writes.
func (h *harness) delivered(name string) {
	h.t.Helper()
	wt := filepath.Join(h.dir, name, "work")
	if err := h.sup.log.Append(Event{V: EventVersion, Time: h.clock.Now(), Session: name, Kind: KindAction, Mode: ModeOwner,
		Act: &ActPayload{Action: ActOpen, Delivered: name + ":" + wt, Ref: "!1", By: "mark-delivered"}}); err != nil {
		h.t.Fatal(err)
	}
}

// rotations are the work session's rotate actions.
func (h *harness) rotations(name string) []*ActPayload {
	var out []*ActPayload
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Session == name && e.Act.Action == ActRotate {
			out = append(out, e.Act)
		}
	}
	return out
}

// replacedFacts is the facts document of the last command the pane of
// name was replaced with.
func (h *harness) replacedFacts(name string) restartFacts {
	h.t.Helper()
	var cmd string
	for _, c := range h.tmux.calls {
		if c.op == "replace" && c.name == name {
			cmd = c.args[1]
		}
	}
	i, j := strings.Index(cmd, `{"why"`), strings.LastIndex(cmd, "}")
	if i < 0 || j < i {
		h.t.Fatalf("no facts in the prompt: %s", cmd)
	}
	var f restartFacts
	if err := json.Unmarshal([]byte(strings.ReplaceAll(cmd[i:j+1], `'\''`, `'`)), &f); err != nil {
		h.t.Fatalf("facts: %v: %s", err, cmd[i:j+1])
	}
	return f
}

// A work session that reports DONE over the threshold is rotated once, at
// the end of its turn, with a prompt carrying its progress, its branch and
// the instruction it was last given (#48).
func TestDoneWorkSessionRotated(t *testing.T) {
	h, name, id := workHarness(t)
	h.status(name, doneStatus)
	h.alive(name, idleScreen)
	at := h.clock.Now()
	h.workTranscript(name, id, promptLine("u1", "Fix the spin test; CI fails on it.", at)+summaryLine("u2", "m1", "Fixed; DONE.", at, 150000))
	// Not while its branch is being delivered.
	h.ticks(3, time.Minute)
	if n := len(h.rotations(name)); n != 0 {
		t.Fatalf("rotated before the delivery was recorded: %d", n)
	}
	h.delivered(name)
	h.ticks(2, time.Minute)
	rots := h.rotations(name)
	if len(rots) != 1 || rots[0].Context <= h.cfg.Policies.Context.Threshold || rots[0].Transcript == id {
		t.Fatalf("rotations = %+v, want one", rots)
	}
	f := h.replacedFacts(name)
	if f.Why != "rotation" || f.Item != "K #8 spin" || f.State != "fixed" || f.Branch != "fix/spin" || f.Instruction != "Fix the spin test; CI fails on it." {
		t.Fatalf("facts = %+v", f)
	}
	if typed := h.typedBy(h.tmux, name); len(typed) != 0 {
		t.Fatalf("typed %q into a session being rotated", typed)
	}
	// The fresh session ends its first turn small: no second rotation,
	// and none on a replay either.
	h.alive(name, idleScreen)
	later := h.clock.Now()
	h.workTranscript(name, rots[0].Transcript, promptLine("u3", DefaultRestartPrompt+" {}", later)+summaryLine("u4", "m2", "Still DONE.", later, 20000))
	h.ticks(3, time.Minute)
	h.restart(false)
	h.ticks(2, time.Minute)
	if n := len(h.rotations(name)); n != 1 {
		t.Fatalf("rotations = %d after the fresh session's turn, want 1", n)
	}
}

// A work session that ends a fix round with work left is rotated too: the
// end of any turn over the threshold is its checkpoint (#48).
func TestWorkSessionRotatedAtEndOfFixRound(t *testing.T) {
	h, name, id := workHarness(t)
	h.status(name, workLeft)
	h.alive(name, idleScreen)
	at := h.clock.Now()
	h.workTranscript(name, id, promptLine("u1", "Review round 2: fix F3.", at)+summaryLine("u2", "m1", "F3 fixed.", at, 150000))
	h.ticks(3, time.Minute)
	if n := len(h.rotations(name)); n != 1 {
		t.Fatalf("rotations = %d, want 1", n)
	}
	if f := h.replacedFacts(name); f.Instruction != "Review round 2: fix F3." {
		t.Fatalf("facts = %+v", f)
	}
}

// Under the threshold a DONE work session is not rotated, nor is one over
// it whose turn still runs (#48).
func TestWorkSessionNotRotatedUnderThresholdOrBusy(t *testing.T) {
	h, name, id := workHarness(t)
	h.status(name, doneStatus)
	h.alive(name, idleScreen)
	at := h.clock.Now()
	h.workTranscript(name, id, promptLine("u1", "Fix the spin test.", at)+summaryLine("u2", "m1", "Fixed; DONE.", at, 50000))
	h.ticks(4, time.Minute)
	if n := len(h.rotations(name)); n != 0 {
		t.Fatalf("rotated under the threshold: %d", n)
	}
	// A new instruction is taken, its turn running over the threshold.
	h.status(name, workLeft)
	h.alive(name, runningScreen)
	h.workTranscript(name, id, promptLine("u3", "Also fix the flake.", h.clock.Now())+usageLine("m2", 10, 150000, 0, 5, false))
	h.ticks(4, time.Minute)
	if n := len(h.rotations(name)); n != 0 {
		t.Fatalf("rotated mid-turn: %d", n)
	}
}

// An instruction posted after the rotation is typed into the
// fresh session's pane, and the fresh transcript is the one read from then
// on (#48). The fake pane keeps whatever is typed; a line lost while the
// fresh Claude starts is not modelled.
func TestInstructionAfterRotationReachesNewSession(t *testing.T) {
	h, name, id := workHarness(t)
	h.status(name, doneStatus)
	h.alive(name, idleScreen)
	at := h.clock.Now()
	h.workTranscript(name, id, promptLine("u1", "Fix the spin test.", at)+summaryLine("u2", "m1", "Fixed; DONE.", at, 150000))
	h.delivered(name)
	h.ticks(3, time.Minute)
	rots := h.rotations(name)
	if len(rots) != 1 {
		t.Fatalf("rotations = %d, want 1", len(rots))
	}
	h.alive(name, idleScreen)
	if err := PostMessage(h.cfg, NewLogWriter(h.buf), h.tmux, h.clock.Now(), MessagePost{Console: "widgets", Text: "Rebase on main.", To: name}); err != nil {
		t.Fatal(err)
	}
	replaced, typedAt := -1, -1
	for i, c := range h.tmux.calls {
		if c.name == name && c.op == "replace" {
			replaced = i
		}
		if c.name == name && c.op == "type" && c.args[0] == "Rebase on main." {
			typedAt = i
		}
	}
	if replaced < 0 || typedAt < replaced {
		t.Fatalf("replaced at %d, typed at %d", replaced, typedAt)
	}
	h.workTranscript(name, rots[0].Transcript, promptLine("u5", "Rebase on main.", h.clock.Now()))
	h.ticks(1, time.Minute)
	tr := h.sup.track(name)
	if tr.Transcript != rots[0].Transcript || tr.lastTurn().Text != "Rebase on main." {
		t.Fatalf("transcript %q, last turn %+v", tr.Transcript, tr.lastTurn())
	}
}

// A line typed into the work session between the read and the rotation
// moves its screen: the rotation waits, and the turn it started is not cut
// off (#48).
func TestWorkRotationWaitsForMovingScreen(t *testing.T) {
	h, name, id := workHarness(t)
	h.status(name, doneStatus)
	h.alive(name, idleScreen)
	at := h.clock.Now()
	h.workTranscript(name, id, promptLine("u1", "Fix the spin test.", at)+summaryLine("u2", "m1", "Fixed; DONE.", at, 150000))
	h.delivered(name)
	// A pass reads the pane once to observe it; the line lands before the
	// pass reads it again to act.
	var last time.Time
	h.tmux.onPane = func(n string) {
		if n != name {
			return
		}
		if now := h.clock.Now(); now.Equal(last) {
			h.tmux.panes[name] = runningScreen
		} else {
			last = now
		}
	}
	h.ticks(3, time.Minute)
	if n := len(h.rotations(name)); n != 0 || h.tmux.count("replace", name) != 0 {
		t.Fatalf("rotated over a moving screen: %d", n)
	}
}

// Over a run of fix rounds, rotating at each round's end over the
// threshold keeps the context the rounds' calls carry near the threshold,
// where without rotation it grows the whole run (#48). The round sizes are
// the measured ones: 588 calls over the largest session, 30 calls a round,
// each adding about 1k tokens to a 20k start. The averages are modelled
// from those sizes; only where the rotations fall comes from agentd.
func TestWorkRotationKeepsContextSmall(t *testing.T) {
	const rounds, calls, base, step = 20, 30, 20000, 1000
	h, name, id := workHarness(t)
	h.status(name, workLeft)
	var with, without int64
	since := int64(0) // calls since the last fresh context
	for r := 0; r < rounds; r++ {
		h.alive(name, idleScreen)
		at := h.clock.Now()
		text := promptLine(fmt.Sprintf("p%d", r), fmt.Sprintf("Round %d: fix the findings.", r), at)
		for c := 0; c < calls; c++ {
			since++
			ctx := int64(base) + since*step
			with += ctx
			without += int64(base) + int64(r*calls+c+1)*step
			if c < calls-1 {
				text += usageLine(fmt.Sprintf("r%dc%d", r, c), 1, ctx, 0, 5, false)
			} else {
				text += summaryLine(fmt.Sprintf("s%d", r), fmt.Sprintf("r%dc%d", r, c), "Round done.", at, ctx)
			}
		}
		h.workTranscript(name, id, text)
		h.ticks(2, time.Minute)
		if rots := h.rotations(name); len(rots) > 0 && rots[len(rots)-1].Transcript != id {
			id, since = rots[len(rots)-1].Transcript, 0
		}
	}
	n := int64(rounds * calls)
	t.Logf("average context: %d without rotation, %d rotated at round ends over %d; %d rotations",
		without/n, with/n, h.cfg.Policies.Context.Threshold, len(h.rotations(name)))
	if with*10 > without*6 {
		t.Fatalf("average context %d rotated, %d not: under 60%% expected", with/n, without/n)
	}
}

// A nudge of agentd's own ending in "…" is not taken for a prefix of the
// owner's instruction (#48).
func TestLastInstructionOwnLineEndingInEllipsis(t *testing.T) {
	h, name, _ := workHarness(t)
	h.sup.cfg.Policies.Nudge.Message = "Carry on…"
	tr := h.sup.track(name)
	tr.Turns = append(tr.Turns, TurnRecord{Role: TurnPrompt, Text: "Carry on with the spin fix."}, TurnRecord{Role: TurnPrompt, Text: "Carry on…"})
	if got := h.sup.lastInstruction(tr); got != "Carry on with the spin fix." {
		t.Fatalf("instruction = %q", got)
	}
}

// A prompt typed into the pane after /clear is read (#58 follow-up):
// Claude Code goes on in a transcript agentd did not launch, recorded
// in testdata/prompts/cleared.jsonl. A transcript /clear began before the
// launch, one that /clear did not begin, and any where another session
// keeps its transcripts are not the session's.
func TestPromptsAfterClearAreRead(t *testing.T) {
	h, name, id := workHarness(t)
	h.alive(name, runningScreen)
	launched := h.clock.Now()
	h.workTranscript(name, id, promptLine("u1", "Fix the spin test.", launched))
	h.ticks(1, time.Minute)
	b, err := os.ReadFile("testdata/prompts/cleared.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	cleared := func(tid string, at time.Time) string {
		return strings.NewReplacer("{id}", tid, "{cwd}", filepath.Join(h.dir, name, "work"), "{at}", at.Format(time.RFC3339Nano)).Replace(string(b))
	}
	h.workTranscript(name, "c-early", cleared("c-early", launched.Add(-time.Hour)))
	h.workTranscript(name, "c-cleared", cleared("c-cleared", h.clock.Now()))
	h.workTranscript(name, "c-other", promptLine("o1", "Another session's prompt.", h.clock.Now()))
	h.workTranscript(name, "c-quoted", promptLine("q1", "Why does <command-name>/clear</command-name> begin a new transcript?", h.clock.Now()))
	h.ticks(2, time.Minute)
	got := map[string][]string{}
	for _, e := range h.events() {
		if e.Kind == KindTurn && e.Session == name {
			got[e.Turn.Transcript] = append(got[e.Turn.Transcript], e.Turn.Role+": "+e.Turn.Text)
		}
	}
	want := []string{"prompt: Read BRIEF.md (its last section is new) and STATUS.md, then paint the third coat as the brief says.", "summary: Third coat started."}
	if fmt.Sprint(got["c-cleared"]) != fmt.Sprint(want) || len(got["c-early"]) != 0 || len(got["c-other"]) != 0 || len(got["c-quoted"]) != 0 || len(got[id]) != 1 {
		t.Fatalf("turns by transcript: %q", got)
	}
	// It runs in the cleared transcript now: its context is that one's.
	if tr := h.sup.track(name); tr.Measured != "c-cleared" {
		t.Fatalf("measured %q, want the cleared transcript", tr.Measured)
	} else if u, ok := tr.current(); !ok || u.Transcript != "c-cleared" || !u.Cleared {
		t.Fatalf("current usage %+v, %v", u, ok)
	}
	if h.sup.cfg.sharesTranscripts(name) {
		t.Fatal("a session alone in its worktree shares its transcripts")
	}
	cfg := *h.sup.cfg
	cfg.Sessions = map[string]Session{name: h.sup.cfg.Sessions[name], "twin": h.sup.cfg.Sessions[name]}
	if !cfg.sharesTranscripts(name) {
		t.Fatal("a twin in the same worktree shares no transcripts")
	}
}
