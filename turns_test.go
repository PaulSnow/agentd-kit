package main

import (
	"encoding/json"
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
	h := turnsHarness(t)
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
	// Each post follows its turn event in the log.
	var order []string
	for _, e := range h.events() {
		if e.Kind == KindTurn && e.Session == "a" {
			order = append(order, "turn")
		} else if e.Kind == KindMessage {
			order = append(order, "post")
		}
	}
	if strings.Join(order, ",") != "turn,post,turn,post" {
		t.Fatalf("log order = %v", order)
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
	h := turnsHarness(t)
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
	h := turnsHarness(t)
	h.cfg.ConfidentialPatterns = []string{`hunter2`}
	h.restart(false)
	h.transcriptOf("a", "t1", promptLine("u1", strings.Repeat("x", turnText+10)+" hunter2", h.clock.Now()))
	h.ticks(2, time.Minute)
	if got := h.turnMessages(); len(got) != 1 || got[0].Text != "" || got[0].Withheld != 1 || got[0].From != "a" {
		t.Fatalf("turns posted = %+v", got)
	}
	// A session whose name matches is withheld too.
	h.cfg.ConfidentialPatterns = []string{`hunter2`, `^b$`}
	h.restart(false)
	h.transcriptOf("b", "t2", promptLine("u1", "Plain words.", h.clock.Now()))
	h.ticks(2, time.Minute)
	if got := h.turnMessages(); len(got) != 2 || got[1].Text != "" || got[1].Withheld != 2 {
		t.Fatalf("turns posted = %+v", got)
	}
}

// A session's turns are all posted unless a rule says otherwise, and a
// shadow run posts none.
func TestTurnsAllByDefaultShadowPostsNone(t *testing.T) {
	h := turnsHarness(t)
	if h.cfg.TurnsOf("a") != TurnsAll || h.cfg.TurnsOf("d") != TurnsAll {
		t.Fatalf("defaults: a %s, d %s", h.cfg.TurnsOf("a"), h.cfg.TurnsOf("d"))
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
	h := turnsHarness(t)
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
	h := turnsHarness(t)
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
	li := strings.LastIndex(body[:max(i, 0)], `<li class="entry msg msg-reply"`)
	if i < 0 || li < 0 || !strings.Contains(body[li:i], `<a href="/session/night/w17">w17</a> <span class="badge">reply</span>`) {
		t.Fatalf("the turn is not shown with its kind and session:\n%s", body)
	}
	// One box, newest last (#59): the owner's earlier instruction is
	// there, and the oldest turn kept.
	if strings.Contains(body, "Painted coat 0<") || !strings.Contains(body, "Painted coat 1<") || strings.Index(body, "Ship it on Friday") > strings.Index(body, "Painted coat 1<") {
		t.Fatalf("the turns are not kept apart:\n%s", body)
	}
}

// promptLine and summaryLine are a fake transcript's user prompt and an
// end-of-turn summary, written at at.
func promptLine(uuid, text string, at time.Time) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "uuid": uuid, "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": text}})
	return string(b) + "\n"
}

func summaryLine(uuid, id, text string, at time.Time, context int64) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": uuid, "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"id": id, "stop_reason": "end_turn", "content": []map[string]any{{"type": "text", "text": text}},
			"usage": map[string]int64{"input_tokens": 1, "cache_creation_input_tokens": context, "cache_read_input_tokens": 0, "output_tokens": 10}}})
	return string(b) + "\n"
}

// turnsHarness is a live harness with sessions a and b in widgets and d
// in readonly, all alive, transcripts read.
func turnsHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, false, "a", "b", "d")
	h.cfg.TranscriptDir = filepath.Join(h.dir, "projects")
	d := h.cfg.Sessions["d"]
	d.Repository = "readonly"
	h.cfg.Sessions["d"] = d
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "d"} {
		h.alive(n, "screen of "+n)
	}
	h.restart(false)
	return h
}

func (h *harness) transcriptOf(name, id, text string) {
	h.t.Helper()
	appendFile(h.t, filepath.Join(TranscriptDir(h.cfg.TranscriptDir, h.cfg.Sessions[name].Cwd), id+".jsonl"), text)
}

// A transcript's prompts and end-of-turn summaries become turn events,
// screened and bounded, once each: a replayed agentd reading the same
// transcript from its start logs none again. A tool result is no prompt.
func TestTurnsFromTranscriptOnceAcrossRestarts(t *testing.T) {
	h := turnsHarness(t)
	h.cfg.ConfidentialPatterns = []string{`hunter2`}
	h.restart(false)
	at := h.clock.Now()
	h.transcriptOf("a", "t1", promptLine("u1", "Build the widget.", at)+
		`{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`+"\n"+
		`{"type":"user","uuid":"u3","isSidechain":true,"message":{"role":"user","content":"a subagent's prompt"}}`+"\n"+
		summaryLine("u4", "m1", "Built it; the check passes.", at, 100)+
		promptLine("u5", "the password is hunter2", at)+
		promptLine("u6", strings.Repeat("long ", 200), at))
	h.ticks(3, time.Minute)
	turns := func() []TurnPayload {
		var out []TurnPayload
		for _, e := range h.events() {
			if e.Kind == KindTurn && e.Session == "a" {
				out = append(out, *e.Turn)
			}
		}
		return out
	}
	got := turns()
	if len(got) != 4 {
		t.Fatalf("turns = %+v, want 4", got)
	}
	if got[0].Role != TurnPrompt || got[0].Text != "Build the widget." || got[1].Role != TurnSummary || got[1].Text != "Built it; the check passes." {
		t.Fatalf("turns = %+v", got[:2])
	}
	if got[2].Text != "" || got[2].Withheld != 1 {
		t.Fatalf("confidential prompt = %+v", got[2])
	}
	if len(got[3].Text) > turnText+len("…") || !strings.HasPrefix(got[3].Text, "long long") {
		t.Fatalf("long prompt kept %d bytes", len(got[3].Text))
	}
	h.restart(false)
	h.ticks(3, time.Minute)
	if n := len(turns()); n != 4 {
		t.Fatalf("after a restart of agentd: %d turns", n)
	}
	h.transcriptOf("a", "t1", promptLine("u7", "Now the gadget.", h.clock.Now()))
	h.ticks(2, time.Minute)
	if got := turns(); len(got) != 5 || got[4].Text != "Now the gadget." {
		t.Fatalf("after a new prompt: %+v", got)
	}
	if tr := h.sup.Tracks()["a"]; tr.TurnCount != 5 || len(tr.Turns) != 5 {
		t.Fatalf("track: %d turns, %d kept", tr.TurnCount, len(tr.Turns))
	}
}

// A confidential term across the cut is still found: turns are screened
// whole, then cut.
func TestTurnScreenedBeforeCut(t *testing.T) {
	h := turnsHarness(t)
	h.cfg.ConfidentialPatterns = []string{`hunter2`}
	h.restart(false)
	h.transcriptOf("a", "t1", promptLine("u1", strings.Repeat("x", turnText-3)+"hunter2 more", h.clock.Now()))
	h.ticks(2, time.Minute)
	for _, e := range h.events() {
		if e.Kind == KindTurn && (e.Turn.Withheld != 1 || e.Turn.Text != "") {
			t.Fatalf("turn = %+v", e.Turn)
		}
	}
	if h.count(KindTurn, "", "a") != 1 {
		t.Fatal("no turn logged")
	}
}
