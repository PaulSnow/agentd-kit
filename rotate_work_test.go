package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// doneStatus is a work session's progress at the end of its round.
const doneStatus = "# fix/spin\n\n| Item | State |\n|---|---|\n| K #8 spin | fixed |\n\n## Questions\n\nnone\n\nDONE\n"

// workHarness is a live harness with one work session a coordinator
// started, launched under a transcript agentd named: its name and the
// transcript's id.
func workHarness(t *testing.T) (*harness, string, string) {
	t.Helper()
	h := sessionsHarness(t)
	reply, _ := h.startSession(map[string]any{"branch": "fix/spin"})
	name := reply["name"].(string)
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
	h.ticks(3, time.Minute)
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

// An instruction a coordinator posts after the rotation is typed into the
// fresh session, and its transcript is the one read from then on (#48).
func TestInstructionAfterRotationReachesNewSession(t *testing.T) {
	h, name, id := workHarness(t)
	h.status(name, doneStatus)
	h.alive(name, idleScreen)
	at := h.clock.Now()
	h.workTranscript(name, id, promptLine("u1", "Fix the spin test.", at)+summaryLine("u2", "m1", "Fixed; DONE.", at, 150000))
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
// each adding about 1k tokens to a 20k start.
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
