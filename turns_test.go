package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// turnMessages are the turns a harness's log posted, in order.
func (h *harness) turnMessages() []*MessagePayload {
	var out []*MessagePayload
	for _, m := range messages(h.t, h.buf) {
		if m.Kind == MessagePrompt || m.Kind == MessageReply {
			out = append(out, m)
		}
	}
	return out
}

// A session's prompts and replies are posted to its console once each,
// with ids that stay the same: a restart of agentd reading the transcript
// from its start posts none again. A reply is cut at the turn length.
func TestTurnsPostedOnceAcrossRestart(t *testing.T) {
	h := coordHarness(t)
	h.cfg.Policies.Feed.TurnLength = 50
	h.restart(false)
	at := h.clock.Now()
	h.transcriptOf("a", "t1", promptLine("u1", "Build the widget.", at)+
		summaryLine("u2", "m1", "Built it.\nThe check passes. "+strings.Repeat("more ", 40), at, 100))
	h.ticks(3, time.Minute)
	got := h.turnMessages()
	if len(got) != 2 || got[0].Kind != MessagePrompt || got[0].Text != "Build the widget." || got[0].From != "a" || got[0].Console != "widgets" ||
		got[1].Kind != MessageReply || !strings.HasPrefix(got[1].Text, "Built it.\nThe check passes.") || len(got[1].Text) > 50+len("…") || got[0].ID == got[1].ID {
		t.Fatalf("turns posted = %+v", got)
	}
	// Turns read in one pass are each folded as themselves.
	if tr := h.sup.Tracks()["a"]; len(tr.Turns) != 2 || tr.Turns[0].Text != "Build the widget." || tr.Turns[1].Role != TurnSummary {
		t.Fatalf("track turns = %+v", tr.Turns)
	}
	ids := got[0].ID + got[1].ID
	h.restart(false)
	h.ticks(3, time.Minute)
	if got := h.turnMessages(); len(got) != 2 || got[0].ID+got[1].ID != ids {
		t.Fatalf("after a restart of agentd: %+v", got)
	}
	h.transcriptOf("a", "t1", promptLine("u3", "Now the gadget.", h.clock.Now()))
	h.ticks(2, time.Minute)
	if got := h.turnMessages(); len(got) != 3 || got[2].Text != "Now the gadget." {
		t.Fatalf("after a new prompt: %+v", got)
	}
}

// prompts posts a session's prompts only; a session's own rule overrides
// its repository's, and off posts nothing.
func TestTurnsPromptsOnly(t *testing.T) {
	h := coordHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Feed.Turns = TurnsPrompts
	h.cfg.Repositories["widgets"] = w
	b := h.cfg.Sessions["b"]
	b.Policy.Feed.Turns = TurnsAll
	h.cfg.Sessions["b"] = b
	d := h.cfg.Sessions["d"]
	d.Policy.Feed.Turns = TurnsOff
	h.cfg.Sessions["d"] = d
	h.restart(false)
	at := h.clock.Now()
	for _, n := range []string{"a", "b", "d"} {
		h.transcriptOf(n, "t-"+n, promptLine("u1", "Prompt of "+n, at)+summaryLine("u2", "m1", "Reply of "+n, at, 100))
	}
	h.ticks(3, time.Minute)
	var texts []string
	for _, m := range h.turnMessages() {
		texts = append(texts, m.Kind+":"+m.Text)
	}
	sort.Strings(texts) // the sessions are measured in workers, in any order
	if strings.Join(texts, ",") != "prompt:Prompt of a,prompt:Prompt of b,reply:Reply of b" {
		t.Fatalf("turns posted = %q", texts)
	}
}

// A turn that matches a confidential pattern, even past the length the
// turn event keeps, is posted without its text.
func TestTurnPostWithheld(t *testing.T) {
	h := coordHarness(t)
	h.cfg.ConfidentialPatterns = []string{`hunter2`}
	h.restart(false)
	h.transcriptOf("a", "t1", promptLine("u1", strings.Repeat("x", turnText+10)+" hunter2", h.clock.Now()))
	h.ticks(2, time.Minute)
	if got := h.turnMessages(); len(got) != 1 || got[0].Text != "" || got[0].Withheld != 1 || got[0].From != "a" {
		t.Fatalf("turns posted = %+v", got)
	}
}

// A coordinator's turns are not posted unless its own rule says so, and a
// shadow run posts none.
func TestCoordinatorTurnsNotPostedByDefault(t *testing.T) {
	h := coordHarness(t)
	at := h.clock.Now()
	h.transcriptOf("c", "tc", promptLine("u1", "agentd: a judgment is due.", at)+summaryLine("u2", "m1", "Judged.", at, 100))
	h.ticks(3, time.Minute)
	if h.count(KindTurn, "", "c") != 2 || len(h.turnMessages()) != 0 {
		t.Fatalf("coordinator: %d turns, posted %+v", h.count(KindTurn, "", "c"), h.turnMessages())
	}
	if h.cfg.TurnsOf("c") != TurnsOff || h.cfg.TurnsOf("a") != TurnsAll {
		t.Fatalf("defaults: c %s, a %s", h.cfg.TurnsOf("c"), h.cfg.TurnsOf("a"))
	}
	w := h.cfg.Repositories["widgets"]
	w.Policy.Feed.Turns = TurnsAll
	h.cfg.Repositories["widgets"] = w
	h.restart(false)
	h.transcriptOf("c", "tc", promptLine("u3", "agentd: another judgment.", h.clock.Now()))
	h.ticks(2, time.Minute)
	if h.cfg.TurnsOf("c") != TurnsOff || len(h.turnMessages()) != 0 {
		t.Fatalf("the repository's rule reached its coordinator: %s, %+v", h.cfg.TurnsOf("c"), h.turnMessages())
	}
	w.Policy.Feed.Turns = TurnsPrompts
	w.Coordinator.Policy.Feed.Turns = TurnsAll
	h.cfg.Repositories["widgets"] = w
	delete(h.cfg.Sessions, "c")
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	if h.cfg.TurnsOf("c") != TurnsAll {
		t.Fatalf("a coordinator's own rule: %s", h.cfg.TurnsOf("c"))
	}

	s := newHarness(t, true, "a")
	s.cfg.TranscriptDir = filepath.Join(s.dir, "projects")
	s.restart(true)
	s.alive("a", "screen")
	s.transcriptOf("a", "t1", promptLine("u1", "Build it.", s.clock.Now()))
	s.ticks(2, time.Minute)
	if s.count(KindTurn, "", "a") != 1 || len(s.turnMessages()) != 0 {
		t.Fatalf("shadow: %d turns, posted %+v", s.count(KindTurn, "", "a"), s.turnMessages())
	}
}

// A turn read more than an hour after it was written, from a transcript
// agentd reads for the first time, is logged but not posted.
func TestTurnBacklogNotPosted(t *testing.T) {
	h := coordHarness(t)
	now := h.clock.Now()
	h.transcriptOf("a", "t1", promptLine("u1", "An old prompt.", now.Add(-2*time.Hour))+promptLine("u2", "A new prompt.", now))
	h.ticks(2, time.Minute)
	if got := h.turnMessages(); h.count(KindTurn, "", "a") != 2 || len(got) != 1 || got[0].Text != "A new prompt." {
		t.Fatalf("%d turns, posted %+v", h.count(KindTurn, "", "a"), got)
	}
}

// A turns rule other than off, prompts or all, and a turn length out of
// range, are refused at load.
func TestTurnsRulesChecked(t *testing.T) {
	h := coordHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Feed.Turns = "some"
	h.cfg.Repositories["widgets"] = w
	a := h.cfg.Sessions["a"]
	a.Policy.Feed.Turns = "All"
	h.cfg.Sessions["a"] = a
	h.cfg.Policies.Feed.TurnLength = turnRead + 1
	err := h.cfg.finish(h.dir)
	for _, want := range []string{`repositories.widgets.policy.feed.turns "some"`, `sessions.a.policy.feed.turns "All"`, "policies.feed.turn_length"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not name %s", err, want)
		}
	}
}

// A posted turn shows in its console with its kind, linked to its
// session, and turns, however many, push out none of the other messages.
func TestTurnShownInConsole(t *testing.T) {
	u, f := fixtureUI(t)
	log := NewLogWriter(f.nightBuf)
	for i := 0; i <= uiKeptMessages; i++ {
		if err := log.Append(Event{V: EventVersion, Time: pageAt.Add(-time.Minute), Kind: KindMessage, Mode: ModeShared,
			Message: &MessagePayload{ID: fmt.Sprintf("T%d", i), Console: "widgets-console", Kind: MessageReply, From: "w17", Text: fmt.Sprintf("Painted coat %d", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	body := owner(t, u, "/console/widgets")
	i := strings.Index(body, "Painted coat 200")
	li := strings.LastIndex(body[:max(i, 0)], `<li class="msg msg-reply">`)
	if i < 0 || li < 0 || !strings.Contains(body[li:i], `<a href="/session/night/w17">w17</a> <span class="badge">reply</span>`) {
		t.Fatalf("the turn is not shown with its kind and session:\n%s", body)
	}
	if strings.Contains(body, "Painted coat 0<") || !strings.Contains(body, "Painted coat 1<") || !strings.Contains(body, "Ship it on Friday") {
		t.Fatalf("the turns are not kept apart:\n%s", body)
	}
}
