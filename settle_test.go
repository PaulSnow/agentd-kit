package main

import (
	"encoding/json"
	"os"
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
		{"text typed at the prompt", "done.\n\n│ > a draft the owner typed │\n", false},
		{"nothing", "", false},
	} {
		if got := idlePrompt(tc.screen); got != tc.want {
			t.Errorf("%s: idlePrompt = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A summary from before the session was last launched is another Claude's
// turn, not this one's.
func TestTurnEndedOnlySinceLaunch(t *testing.T) {
	at := time.Date(2026, 10, 22, 1, 0, 0, 0, time.UTC)
	ev := Evidence{Prompt: true, Status: StatusEvidence{Exists: true}}
	tr := newTrack()
	tr.applyTurn(&TurnPayload{Transcript: "t1", Line: 1, Role: TurnSummary, Text: "Done here.", At: at})
	if !tr.turnEnded(ev) {
		t.Fatal("a summary at the prompt is no ended turn")
	}
	tr.launched("t2", at.Add(time.Minute))
	if tr.turnEnded(ev) {
		t.Fatal("the previous Claude's summary ended the new one's turn")
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
	h.ticks(20, time.Minute)
	if typed := h.typed("s1"); len(typed) != 2 || typed[1] != h.cfg.Policies.Nudge.Message {
		t.Fatalf("typed %v, want the stall nudge once the stall has passed", typed)
	}
	h.tmux.calls = nil

	// It worked, wrote STATUS.md, and ended another turn.
	h.tmux.panes["s1"] = runningScreen
	h.ticks(1, time.Minute)
	later := h.clock.Now()
	h.status("s1", moreWork)
	h.transcriptOf("s1", "t1", promptLine("u3", typed[0], later)+summaryLine("u4", "m2", "Merged #22.", later, 100))
	h.tmux.panes["s1"] = idleScreen + "\n"
	h.ticks(8, time.Minute)
	if n := len(h.typed("s1")); n != 1 {
		t.Fatalf("typed %d lines, want a second continue for the second ended turn", n)
	}
	// Replayed, the continues stand.
	h.restart(false)
	if tr := h.sup.Tracks()["s1"]; tr.Continues != 1 || tr.ContinueStatus == "" || tr.NudgeCount != 3 {
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
	if len(h.runner.calls) != 1 || !strings.Contains(h.runner.calls[0][4], "progress unchanged") {
		t.Fatalf("escalations = %v", h.runner.calls)
	}
	// Then it is left to the stall, like a turn that has not ended.
	h.ticks(40, time.Minute)
	if typed := h.typed("s1"); len(typed) != 4 || typed[3] != h.cfg.Policies.Nudge.Message {
		t.Fatalf("typed %d lines, want the stall nudge after the continues", len(typed))
	}
	// New progress in STATUS.md is new facts: the next ended turn is
	// continued again.
	h.status("s1", moreWork)
	at := h.clock.Now()
	h.transcriptOf("s1", "t1", promptLine("pz", "go on", at)+summaryLine("sz", "m", "Moved on.", at, 100))
	h.tmux.panes["s1"] = idleScreen + "\n\n"
	h.ticks(8, time.Minute)
	if typed := h.typed("s1"); len(typed) != 5 || !strings.HasPrefix(typed[4], h.cfg.Policies.Nudge.ContinueMessage) {
		t.Fatalf("typed %d lines, want a continue for the new facts", len(typed))
	}
}

// A continue answered within settle, the session back at the prompt at
// once, is answered: the nudge count and the idle clock start again, so a
// later hang gets its stall nudge.
func TestQuicklyAnsweredContinueIsProgress(t *testing.T) {
	h := endedHarness(t, idleScreen)
	at := h.clock.Now()
	h.transcriptOf("s1", "t1", promptLine("u1", "Build the widget.", at)+summaryLine("u2", "m1", "Merged #21.", at, 100))
	h.ticks(8, time.Minute)
	if tr := h.sup.Tracks()["s1"]; tr.Nudges != 1 {
		t.Fatalf("nudges = %d after the continue", tr.Nudges)
	}
	h.status("s1", moreWork)
	reply := h.clock.Now()
	h.transcriptOf("s1", "t1", promptLine("u3", "continue", reply)+summaryLine("u4", "m2", "Nothing more right now.", reply, 100))
	h.tmux.panes["s1"] = idleScreen + "\n"
	h.ticks(1, time.Minute)
	if tr := h.sup.Tracks()["s1"]; tr.Nudges != 0 || !tr.HashSince.Equal(reply) {
		t.Fatalf("nudges %d, idle since %s: the reply was taken for an echo", tr.Nudges, tr.HashSince)
	}
}

// A transcript whose last line does not end the turn, a tool call under
// the prompt, is no ended turn, whatever the last folded turn was.
func TestToolCallLastIsNoEndedTurn(t *testing.T) {
	h := endedHarness(t, idleScreen)
	at := h.clock.Now()
	h.transcriptOf("s1", "t1", promptLine("u1", "Build the widget.", at)+summaryLine("u2", "m1", "Merged #21.", at, 100)+
		`{"type":"assistant","timestamp":"`+at.Format(time.RFC3339Nano)+`","message":{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"Bash"}]}}`+"\n")
	h.ticks(20, time.Minute)
	if typed := h.typed("s1"); len(typed) != 0 {
		t.Fatalf("typed %v", typed)
	}
}

// No continue goes when nudging is off or nudge.max is 0, nor to a session
// with no STATUS.md to go on from, nor in shadow mode, which records the
// continue it would type.
func TestEndedTurnNotContinuedWhenNudgesOffOrNoStatus(t *testing.T) {
	for _, tc := range []string{"disabled", "max 0", "no status", "shadow"} {
		t.Run(tc, func(t *testing.T) {
			h := endedHarness(t, idleScreen)
			switch tc {
			case "disabled":
				h.cfg.Policies.Nudge.Enabled = false
			case "max 0":
				h.cfg.Policies.Nudge.Max = 0
			case "no status":
				if err := os.Remove(filepath.Join(h.dir, "s1", "STATUS.md")); err != nil {
					t.Fatal(err)
				}
			case "shadow":
				h.restart(true)
			}
			at := h.clock.Now()
			h.transcriptOf("s1", "t1", promptLine("u1", "Build the widget.", at)+summaryLine("u2", "m1", "Merged #21.", at, 100))
			h.ticks(20, time.Minute)
			if typed := h.typed("s1"); len(typed) != 0 {
				t.Fatalf("typed %v", typed)
			}
			want := 0
			if tc == "shadow" {
				want = 1
			}
			if n := h.count(KindWould, ActNudge, "s1"); n != want {
				t.Fatalf("would-be nudges = %d, want %d", n, want)
			}
			if tc == "disabled" && h.state("s1") != Working {
				t.Fatalf("state = %s with nudging off", h.state("s1"))
			}
		})
	}
}

// Facts that match a confidential pattern are not typed, and the log
// never holds the facts, only the message and their hash.
func TestContinueLineScreened(t *testing.T) {
	h := endedHarness(t, idleScreen)
	h.cfg.ConfidentialPatterns = []string{`#20 poll`}
	h.restart(false)
	at := h.clock.Now()
	h.transcriptOf("s1", "t1", promptLine("u1", "Build the widget.", at)+summaryLine("u2", "m1", "Merged #21.", at, 100))
	h.ticks(8, time.Minute)
	if typed := h.typed("s1"); len(typed) != 1 || typed[0] != h.cfg.Policies.Nudge.ContinueMessage {
		t.Fatalf("typed %v, want the message alone", typed)
	}
	if strings.Contains(h.buf.String(), "#20 poll") {
		t.Fatal("the log holds the confidential facts")
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

// A turn still running is working, and is not nudged before the stall:
// one after a prompt, and one whose screen runs a turn although the last
// folded turn is a summary.
func TestRunningTurnNotNudgedBeforeStall(t *testing.T) {
	for _, tc := range []string{"prompt last", "summary last"} {
		t.Run(tc, func(t *testing.T) {
			runningTurn(t, tc == "summary last")
		})
	}
}

func runningTurn(t *testing.T, summaryLast bool) {
	h := endedHarness(t, runningScreen)
	at := h.clock.Now()
	lines := summaryLine("u0", "m0", "Earlier turn.", at, 100) + promptLine("u1", "Build the widget.", at)
	if summaryLast {
		lines += summaryLine("u2", "m1", "Thinking out loud.", at, 100)
	}
	h.transcriptOf("s1", "t1", lines)
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

// A rotation that has given up holds no continue.
func TestGivenUpRotationDoesNotHoldContinue(t *testing.T) {
	h, id := contextHarness(t, false)
	h.cfg.Policies.Nudge.Settle = Duration{time.Minute}
	h.status("s", workLeft)
	h.alive("s", idleScreen)
	h.git.merge, h.git.mergeSubject = "m1", "Merge #21: contexts"
	tr := h.sup.Tracks()["s"]
	for i := 0; i < h.cfg.Policies.Restart.MaxRestarts; i++ {
		tr.Failures["rotate:s:m1"] = append(tr.Failures["rotate:s:m1"], h.clock.Now())
	}
	at := h.clock.Now()
	h.transcript(id, promptLine("u1", "Build the widget.", at)+summaryLine("u2", "m1", "Merged #21.", at, 150000))
	h.ticks(12, time.Minute)
	if n := h.count(KindAction, ActRotate, "s"); n != 0 {
		t.Fatalf("rotations = %d after giving up", n)
	}
	if typed := h.typed("s"); len(typed) != 1 || !strings.HasPrefix(typed[0], h.cfg.Policies.Nudge.ContinueMessage) {
		t.Fatalf("typed %v, want the continue", typed)
	}
}
