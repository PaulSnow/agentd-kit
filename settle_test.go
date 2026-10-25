package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// idleScreen is a session that ended its turn: its summary, then the empty
// input prompt in its box. runningScreen is one whose turn still runs.
const (
	idleScreen = "⏺ Merged #21; STATUS.md names the next step.\n\n" +
		"╭──────────────────────────────────────────╮\n" +
		"│ >                                        │\n" +
		"╰──────────────────────────────────────────╯\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"
	runningScreen = "⏺ Bash(go test ./...)\n" +
		"✻ Testing… (3m · ↓ 2k tokens · esc to interrupt)\n\n" +
		"╭──────────────────────────────────────────╮\n" +
		"│ >                                        │\n" +
		"╰──────────────────────────────────────────╯\n"
	workLeft = "# s1\n\n| Item | State |\n|---|---|\n| K #20 poll | coding |\n\n## Questions\n\nnone\n"
	moreWork = "# s1\n\n| Item | State |\n|---|---|\n| K #20 poll | testing |\n\n## Questions\n\nnone\n"
)

// endedHarness is a live harness whose session s1 has work left in its
// STATUS.md, its transcript read, and screen.
func endedHarness(t *testing.T, screen string) *harness {
	t.Helper()
	h := newHarness(t, false, "s1")
	h.cfg.TranscriptDir = filepath.Join(h.dir, "projects")
	h.status("s1", workLeft)
	h.alive("s1", screen)
	h.restart(false)
	return h
}

// typed is what was typed into a session, in order.
func (h *harness) typed(name string) []string {
	var out []string
	for _, c := range h.tmux.calls {
		if c.op == "type" && c.name == name {
			out = append(out, c.args[0])
		}
	}
	return out
}

func TestScreenShowsIdlePrompt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		screen string
		want   bool
	}{
		{"boxed empty prompt", idleScreen, true},
		{"bare chevron with a suggestion", "done.\n\n❯ Try \"fix lint errors\"\n", true},
		{"turn running", runningScreen, false},
		{"dialog cursor", "Do you want to proceed?\n❯ 1. Yes\n  2. No\n", false},
		{"quoted prompt high in the scrollback", "> Build the widget\n" + strings.Repeat("output line\n", 10), false},
		{"nothing", "", false},
	} {
		if got := idlePrompt(tc.screen); got != tc.want {
			t.Errorf("%s: idlePrompt = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A session that ended its turn at the prompt, with work left, is idle at
// once and continued after settle, not after the stall: the line is the
// continue message and the facts its STATUS.md gives, and it counts as a
// nudge. Ending its turn again after real work, it is continued again.
func TestEndedTurnContinuedAfterSettle(t *testing.T) {
	h := endedHarness(t, idleScreen)
	at := h.clock.Now()
	h.transcriptOf("s1", "t1", promptLine("u1", "Build the widget.", at)+summaryLine("u2", "m1", "Merged #21.", at, 100))
	h.ticks(3, time.Minute)
	if h.state("s1") != Idle {
		t.Fatalf("state = %s, want idle once the turn ended", h.state("s1"))
	}
	if n := len(h.typed("s1")); n != 0 {
		t.Fatalf("typed %d lines within settle", n)
	}
	h.ticks(5, time.Minute) // past the 6-minute settle
	typed := h.typed("s1")
	if len(typed) != 1 {
		t.Fatalf("typed %v, want one continue line after settle", typed)
	}
	msg := h.cfg.Policies.Nudge.ContinueMessage + " "
	if !strings.HasPrefix(typed[0], msg) {
		t.Fatalf("continue line = %q", typed[0])
	}
	var f restartFacts
	if err := json.Unmarshal([]byte(strings.TrimPrefix(typed[0], msg)), &f); err != nil {
		t.Fatalf("facts: %v", err)
	}
	if f.Why != "continue" || f.Item != "K #20 poll" || f.State != "coding" || !strings.HasSuffix(f.Progress, "STATUS.md") {
		t.Fatalf("facts = %+v", f)
	}
	if tr := h.sup.Tracks()["s1"]; tr.Nudges != 1 || tr.NudgeCount != 1 || tr.Continues != 1 {
		t.Fatalf("nudges %d, count %d, continues %d", tr.Nudges, tr.NudgeCount, tr.Continues)
	}
	// The same ended turn is not continued twice: it waits for the stall.
	h.ticks(20, time.Minute)
	if n := len(h.typed("s1")); n != 1 {
		t.Fatalf("typed %d lines for one ended turn", n)
	}

	// It worked, wrote STATUS.md, and ended another turn.
	h.tmux.panes["s1"] = runningScreen
	h.ticks(1, time.Minute)
	later := h.clock.Now()
	h.status("s1", moreWork)
	h.transcriptOf("s1", "t1", promptLine("u3", typed[0], later)+summaryLine("u4", "m2", "Merged #22.", later, 100))
	h.tmux.panes["s1"] = idleScreen + "\n"
	h.ticks(8, time.Minute)
	if n := len(h.typed("s1")); n != 2 {
		t.Fatalf("typed %d lines, want a second continue for the second ended turn", n)
	}
	// Replayed, the continues stand.
	h.restart(false)
	if tr := h.sup.Tracks()["s1"]; tr.Continues != 1 || tr.ContinueStatus != ParseStatus(moreWork).Hash || tr.NudgeCount != 2 {
		t.Fatalf("after replay: continues %d for %q, nudges %d", tr.Continues, tr.ContinueStatus, tr.NudgeCount)
	}
}

// A session that ends its turn again and again without changing STATUS.md
// is continued nudge.max times, then goes to the owner once.
func TestEndedTurnContinuedAtMostMax(t *testing.T) {
	h := endedHarness(t, idleScreen)
	for i := 0; i < 5; i++ {
		at := h.clock.Now()
		h.transcriptOf("s1", "t1", promptLine("p"+string(rune('a'+i)), "go on", at)+summaryLine("s"+string(rune('a'+i)), "m", "Nothing to do.", at, 100))
		h.tmux.panes["s1"] = runningScreen
		h.ticks(1, time.Minute)
		h.tmux.panes["s1"] = idleScreen + strings.Repeat("\n", i)
		h.ticks(8, time.Minute)
	}
	if n := len(h.typed("s1")); n != 3 {
		t.Fatalf("typed %d lines, want nudge.max = 3", n)
	}
	if len(h.runner.calls) != 1 || !strings.Contains(h.runner.calls[0][4], "STATUS.md unchanged") {
		t.Fatalf("escalations = %v", h.runner.calls)
	}
}

// An ended turn is not continued when STATUS.md is DONE, asks Questions, or
// a blocking decision waits on the owner.
func TestEndedTurnNotContinuedWhenFinishedOrBlocked(t *testing.T) {
	for _, tc := range []struct {
		name, status, questions string
		want                    State
	}{
		{"done", "# s1\n\nAll finished.\n\nDONE\n", "", Done},
		{"questions", "# s1\n\n## Questions\n\nWhich colour?\n", "", BlockedOnOwner},
		{"blocking decision", workLeft, chooseColour, BlockedOnOwner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := endedHarness(t, idleScreen)
			h.status("s1", tc.status)
			if tc.questions != "" {
				writeQuestions(t, h, "s1", tc.questions)
			}
			at := h.clock.Now()
			h.transcriptOf("s1", "t1", promptLine("u1", "Build the widget.", at)+summaryLine("u2", "m1", "Over to you.", at, 100))
			h.ticks(30, time.Minute)
			if h.state("s1") != tc.want {
				t.Fatalf("state = %s, want %s", h.state("s1"), tc.want)
			}
			if typed := h.typed("s1"); len(typed) != 0 {
				t.Fatalf("typed %v", typed)
			}
		})
	}
}

// A turn still running is working, and is not nudged before the stall.
func TestRunningTurnNotNudgedBeforeStall(t *testing.T) {
	h := endedHarness(t, runningScreen)
	at := h.clock.Now()
	h.transcriptOf("s1", "t1", summaryLine("u0", "m0", "Earlier turn.", at, 100)+promptLine("u1", "Build the widget.", at))
	h.ticks(39, time.Minute)
	if h.state("s1") != Working {
		t.Fatalf("state = %s, want working", h.state("s1"))
	}
	if typed := h.typed("s1"); len(typed) != 0 {
		t.Fatalf("typed %v before the stall", typed)
	}
	h.ticks(3, time.Minute)
	if typed := h.typed("s1"); len(typed) != 1 || typed[0] != h.cfg.Policies.Nudge.Message {
		t.Fatalf("typed %v, want the stall nudge", typed)
	}
}

// A session over its context threshold that ended its turn after a
// checkpoint is rotated, not continued, even with settle shorter than the
// rotation's grace.
func TestRotationAtCheckpointBeforeContinue(t *testing.T) {
	h, id := contextHarness(t, false)
	h.cfg.Policies.Nudge.Settle = Duration{time.Minute}
	h.status("s", workLeft)
	h.alive("s", idleScreen)
	h.git.merge, h.git.mergeSubject = "m1", "Merge #21: contexts"
	at := h.clock.Now()
	h.transcript(id, promptLine("u1", "Build the widget.", at)+summaryLine("u2", "m1", "Merged #21.", at, 150000))
	h.ticks(12, time.Minute)
	if n := h.count(KindAction, ActRotate, "s"); n != 1 {
		t.Fatalf("rotations = %d, want 1", n)
	}
	if typed := h.typed("s"); len(typed) != 0 {
		t.Fatalf("typed %v instead of rotating", typed)
	}
}
