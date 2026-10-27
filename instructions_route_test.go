package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// instruction is the instruction the log folds to, as the page reads it.
func (h *harness) instruction(t *testing.T) *Instruction {
	t.Helper()
	st := h.logState()
	if len(st.Instructions) != 1 {
		t.Fatalf("%d instructions", len(st.Instructions))
	}
	for _, in := range st.Instructions {
		return in
	}
	return nil
}

// readInstructions reads instructions.json in the workdir.
func readInstructions(t *testing.T, workdir string) []InstructionDoc {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(workdir, InstructionsFile))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Instructions []InstructionDoc `json:"instructions"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Instructions
}

// toolLines are a fake transcript's tool call and its result, written at
// at: the command, and the exit code it failed with, or none.
func toolLines(uuid, id, command string, exit int, at time.Time) string {
	use, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": uuid + "a", "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"id": "m-" + id, "stop_reason": "tool_use", "content": []map[string]any{{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": command + "\nsecond line"}}}}})
	result := map[string]any{"type": "tool_result", "tool_use_id": id, "content": "ok"}
	if exit != 0 {
		result["is_error"], result["content"] = true, fmt.Sprintf("Exit code %d\nFAIL", exit)
	}
	res, _ := json.Marshal(map[string]any{"type": "user", "uuid": uuid + "b", "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": []map[string]any{result}}})
	return string(use) + "\n" + string(res) + "\n"
}

// A message in a session's own box is typed into it at once, mid-turn
// too: instructions.json is written and the fixed pointer typed, never the
// owner's words. The session's next prompt is it taking the message, the
// tools it runs then are threaded under it as they are read, with the
// failed one marked, and so is the reply it ends that turn with. A
// restart of agentd delivers nothing again.
func TestInstructionToWorkingSessionAtOnce(t *testing.T) {
	h := turnsHarness(t)
	for i := 0; i < 3; i++ {
		h.alive("b", fmt.Sprintf("b works, screen %d", i))
		h.ticks(1, time.Minute)
	}
	if h.state("b") != Working {
		t.Fatalf("b is %s", h.state("b"))
	}
	const text = "Put the squeak fix ahead of the docs."
	if err := UIInstruction(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "widgets", "b", text); err != nil {
		t.Fatal(err)
	}
	if err := UIInstruction(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "widgets", "d", text); err == nil {
		t.Fatal("an instruction to another repository's session was taken")
	}
	h.alive("b", "b works, screen 4")
	h.ticks(1, time.Minute)
	in := h.instruction(t)
	if in.Session != "b" || in.To != "b" || in.Started || in.Delivered.IsZero() || in.Failed != "" {
		t.Fatalf("instruction = %+v", in)
	}
	docs := readInstructions(t, h.cfg.Sessions["b"].Workdir)
	if len(docs) != 1 || docs[0].Text != text || !docs[0].New || docs[0].ID != in.ID {
		t.Fatalf("instructions.json = %+v", docs)
	}
	if got := h.typed("b"); len(got) != 1 || got[0] != h.cfg.Policies.Instructions.Pointer {
		t.Fatalf("typed into b: %q", got)
	}
	if n := len(h.typed("a")); n != 0 {
		t.Fatalf("typed into a %d times", n)
	}
	if !h.sup.st.chatInFlight(h.clock.Now()) || h.sup.passWait() != ChatPoll {
		t.Fatalf("pass wait %v while the message is unanswered", h.sup.passWait())
	}
	at := h.clock.Now()
	h.transcriptOf("b", "t1", promptLine("u1", h.cfg.Policies.Instructions.Pointer, at)+toolLines("u2", "c1", "go test ./...", 1, at))
	h.ticks(2, time.Minute)
	in = h.instruction(t)
	if in.Taken.IsZero() || len(in.Activity) != 1 || in.Activity[0].Text != "Bash: go test ./..." || !in.Activity[0].Failed || in.Activity[0].Exit == nil || *in.Activity[0].Exit != 1 || !in.Replied.IsZero() {
		t.Fatalf("instruction while it works = %+v", in)
	}
	h.transcriptOf("b", "t1", toolLines("u3", "c2", "./bin/check", 0, at)+summaryLine("u4", "m1", "The squeak fix now goes first; the docs follow.", at, 100))
	h.ticks(2, time.Minute)
	in = h.instruction(t)
	if len(in.Activity) != 2 || in.Activity[1].Failed || in.Reply != "The squeak fix now goes first; the docs follow." {
		t.Fatalf("instruction after the turn = %+v", in)
	}
	if h.sup.st.chatInFlight(h.clock.Now()) || h.sup.passWait() != h.cfg.PollInterval.Duration {
		t.Fatal("still in flight after the reply")
	}
	h.restart(false)
	h.ticks(3, time.Minute)
	if n := len(h.typed("b")); n != 1 {
		t.Fatalf("typed %d times after a restart of agentd", n)
	}
}

// launchOf is the transcript the last restart of name launched, and its
// command.
func (h *harness) launchOf(name string) (transcript string, act *ActPayload) {
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Session == name && e.Act.Action == ActRestart {
			transcript, act = e.Act.Transcript, e.Act
		}
	}
	return transcript, act
}

// A message in the repository's box goes to its chat session, which
// agentd starts for it beside the work sessions, outside sessions.max,
// with a brief and a launch that hand it the message. The next message
// is typed into the same session; a chat session is never nudged, and
// is ended once the owner is quiet for ChatIdle.
func TestChatSessionStartedAndKeptWarm(t *testing.T) {
	h := sessionsHarness(t)
	h.startSession("issue-1", KindWork, 1)
	h.startSession("issue-2", KindWork, 2)
	h.ticks(3, time.Minute)
	if err := reply(h, "widgets", "What is left on the squeak?"); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, time.Minute)
	name := runtimeName("widgets", chatBranch, KindChat)
	in := h.instruction(t)
	if in.Session != name || !in.Started || in.Delivered.IsZero() {
		t.Fatalf("instruction = %+v", in)
	}
	sess := h.sup.cfg.Sessions[name]
	if sess.Kind != KindChat || len(sess.Deliver) != 0 || len(sessionsOf(h.sup.st.Runtime, "widgets")) != 2 {
		t.Fatalf("chat session = %+v; counted %v", sess, sessionsOf(h.sup.st.Runtime, "widgets"))
	}
	brief, err := os.ReadFile(filepath.Join(sess.Workdir, "BRIEF.md"))
	if err != nil || !strings.HasPrefix(string(brief), briefChat) {
		t.Fatalf("BRIEF.md = %q, %v", brief, err)
	}
	if docs := readInstructions(t, sess.Workdir); len(docs) != 1 || docs[0].Text != "What is left on the squeak?" {
		t.Fatalf("instructions.json = %+v", docs)
	}
	transcript, act := h.launchOf(name)
	if transcript == "" || !strings.Contains(act.Command, h.cfg.Policies.Instructions.Pointer) || strings.Join(act.Instructions, ",") != in.ID {
		t.Fatalf("launch = %+v", act)
	}
	at := h.clock.Now()
	file := filepath.Join(TranscriptDir(h.cfg.TranscriptDir, sess.Cwd), transcript+".jsonl")
	appendFile(t, file, promptLine("u1", "launch prompt", at)+toolLines("u2", "c1", "cat STATUS.md", 0, at)+summaryLine("u3", "m1", "Two tests are left.", at, 100))
	h.ticks(2, time.Minute)
	if in := h.instruction(t); in.Reply != "Two tests are left." || len(in.Activity) != 1 {
		t.Fatalf("instruction = %+v", in)
	}
	h.alive(name, "chat waits")
	h.ticks(12, time.Minute)
	if n := len(h.typed(name)); n != 0 {
		t.Fatalf("an idle chat session was typed into %d times", n)
	}
	if err := reply(h, "widgets", "And the docs?"); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, time.Minute)
	if got := h.typed(name); len(got) != 1 || got[0] != h.cfg.Policies.Instructions.Pointer || h.count(KindAction, ActRestart, name) != 1 {
		t.Fatalf("second message: typed %q, %d launches", got, h.count(KindAction, ActRestart, name))
	}
	appendFile(t, file, promptLine("u4", h.cfg.Policies.Instructions.Pointer, h.clock.Now())+summaryLine("u5", "m2", "The docs wait.", h.clock.Now(), 100))
	h.ticks(2, time.Minute)
	h.clock.advance(ChatIdle)
	h.ticks(2, time.Minute)
	if _, ok := h.sup.st.Runtime[name]; ok {
		t.Fatal("a quiet chat session was not ended")
	}
}

// What cannot be delivered says so: a withheld text, and a console with
// no repository and no session addressed. An instruction recorded before
// #56 is not routed at all.
func TestInstructionFailuresShown(t *testing.T) {
	h := turnsHarness(t)
	h.cfg.ConfidentialPatterns = []string{`(?i)secret-project`}
	h.restart(false)
	h.ticks(3, time.Minute)
	if err := reply(h, "widgets", "Move secret-project work first."); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	if in := h.instruction(t); in.Failed == "" || !strings.Contains(in.Failed, "confidential") || in.Session != "" {
		t.Fatalf("instruction = %+v", in)
	}
	if len(h.typed("a"))+len(h.typed("b")) != 0 {
		t.Fatal("a withheld instruction was typed")
	}
	h2 := turnsHarness(t)
	h2.ticks(1, time.Minute)
	old := &InstructionPayload{ID: "I00000001", Repositories: []string{"widgets"}, Text: "Old words.", From: "ui"}
	if err := NewLogWriter(h2.buf).Append(Event{V: EventVersion, Time: h2.clock.Now(), Kind: KindInstruction, Mode: ModeOwner, Instruction: old}); err != nil {
		t.Fatal(err)
	}
	h2.ticks(3, time.Minute)
	if in := h2.instruction(t); in.Session != "" || in.Waiting != "" || in.Failed != "" || h2.count(KindRoute, "", "") != 0 {
		t.Fatalf("an instruction from before #56 was routed: %+v", in)
	}
}

// Shadow mode routes, and records the delivery it would make, but types
// and starts nothing.
func TestShadowInstructionOnlyRecords(t *testing.T) {
	h := turnsHarness(t)
	h.restart(true)
	h.ticks(3, time.Minute)
	if err := UIInstruction(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "widgets", "a", "Hold the release."); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, time.Minute)
	if n := len(h.typed("a")); n != 0 {
		t.Fatalf("shadow typed %d times", n)
	}
	st := NewLogState(h.cfg, ModeShadow).Fold(h.events())
	for _, in := range st.Instructions {
		if in.Session != "a" || in.Delivered.IsZero() {
			t.Fatalf("shadow instruction = %+v", in)
		}
	}
	if _, err := os.Stat(filepath.Join(h.cfg.Sessions["a"].Workdir, InstructionsFile)); err == nil {
		t.Fatal("shadow wrote instructions.json")
	}
}

// The console shows a message's steps and the tools its session runs as
// they are read, refreshing every few seconds until the answer is
// threaded under it, in full; a session's page has its own message box,
// whose message goes to that session.
func TestConsoleShowsTurnLive(t *testing.T) {
	h := turnsHarness(t)
	h.ticks(3, time.Minute)
	u, _ := testUI(t, UIConfig{}, newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a"))
	u.now = h.clock.Now
	page := owner(t, u, "/session/night/b")
	if !strings.Contains(page, `<input type="hidden" name="session" value="b">`) {
		t.Fatalf("no message box on b's page:\n%s", page)
	}
	if w := ownerPost(u, "/reply", url.Values{"src": {"night"}, "repository": {"widgets"}, "session": {"b"}, "text": {"Rebase on main."}, "back": {"/console/widgets"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("message: %d %s", w.Code, w.Body.String())
	}
	h.ticks(1, time.Minute)
	if in := h.instruction(t); in.To != "b" || in.Delivered.IsZero() {
		t.Fatalf("instruction = %+v", in)
	}
	long := strings.Repeat("All rebased and the check passes. ", 20)
	at := h.clock.Now()
	h.transcriptOf("b", "t1", promptLine("u1", h.cfg.Policies.Instructions.Pointer, at)+toolLines("u2", "c1", "git rebase origin/main", 1, at))
	h.ticks(1, time.Minute)
	page = owner(t, u, "/console/widgets")
	for _, want := range []string{"Rebase on main.", "delivered to b", "b started on it", "<code>Bash: git rebase origin/main</code>", "failed, exit 1", "waiting for b&#39;s reply", "data-live"} {
		if !strings.Contains(page, want) {
			t.Fatalf("console lacks %q:\n%s", want, page)
		}
	}
	if strings.Index(page, `id="reply"`) > strings.Index(page, `id="messages"`) {
		t.Fatal("the reply box is below the history")
	}
	h.transcriptOf("b", "t1", summaryLine("u3", "m1", long, at, 100))
	h.ticks(1, time.Minute)
	page = owner(t, u, "/console/widgets")
	if in := h.instruction(t); in.Reply != strings.TrimSpace(long) {
		t.Fatalf("the reply is cut: %d bytes of %d", len(in.Reply), len(strings.TrimSpace(long)))
	}
	if !strings.Contains(page, "b replied") || strings.Contains(page, "data-live") || !strings.Contains(page, "msg-reply thread") {
		t.Fatalf("console after the reply:\n%s", page)
	}
}

// How long agentd itself holds up a chat turn: an owner record wakes
// the loop within WakeCheck, and the pass it wakes routes and types the
// message; while it is unanswered the loop passes every ChatPoll, and
// each pass logs what the session's transcript gained, for the page to
// show at its next refresh. The model's own time comes on top.
func TestChatLatency(t *testing.T) {
	h := turnsHarness(t)
	h.ticks(3, time.Minute)
	if WakeCheck > 2*time.Second || ChatPoll > 5*time.Second {
		t.Fatalf("wake check %s, chat poll %s", WakeCheck, ChatPoll)
	}
	if err := UIInstruction(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "widgets", "a", "Is the squeak fixed?"); err != nil {
		t.Fatal(err)
	}
	h.sup.Pass()
	h.sup.Wait()
	if in := h.instruction(t); in.Delivered.IsZero() || len(h.typed("a")) != 1 || h.sup.passWait() != ChatPoll {
		t.Fatalf("after one pass: %+v, typed %q, next pass in %s", in, h.typed("a"), h.sup.passWait())
	}
	at := h.clock.Now()
	h.transcriptOf("a", "t1", promptLine("u1", h.cfg.Policies.Instructions.Pointer, at)+toolLines("u2", "c1", "go test ./...", 0, at))
	h.sup.Pass()
	h.sup.Wait()
	if in := h.instruction(t); len(in.Activity) != 1 {
		t.Fatalf("after one more pass: %+v", in)
	}
	h.transcriptOf("a", "t1", summaryLine("u3", "m1", "Yes; the test passes.", at, 100))
	h.sup.Pass()
	h.sup.Wait()
	if in := h.instruction(t); in.Reply != "Yes; the test passes." || h.sup.passWait() != h.cfg.PollInterval.Duration {
		t.Fatalf("after the answer's pass: %+v", in)
	}
}

// A message addressed to a session goes to it or nowhere: to a finished
// session it fails, and no chat session is started in its place; a
// reviewer takes none. A session that is gone before it answers is
// launched again with the message, and what it ends that turn with is the
// answer. Tools are logged only while a message waits on its session, and
// the log's fold of the messages agrees with the running one.
func TestAddressedMessageEdges(t *testing.T) {
	h := sessionsHarness(t)
	rev := h.startSession("request-4", KindReviewer, 0)
	if err := UIInstruction(h.sup.cfg, NewLogWriter(h.buf), h.clock.Now(), "widgets", rev, "Look again."); err == nil || !strings.Contains(err.Error(), "reviewer") {
		t.Fatalf("a reviewer took a message: %v", err)
	}
	h.status("a", "# Status\n\nDONE\n")
	h.ticks(3, time.Minute)
	if err := UIInstruction(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "widgets", "a", "One more thing."); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	in := h.instruction(t)
	if _, chat := h.sup.st.Runtime[runtimeName("widgets", chatBranch, KindChat)]; in.Failed == "" || in.Session != "" || chat {
		t.Fatalf("a message to a finished session: %+v, started %v", in, sortedKeys(h.sup.st.Runtime))
	}
	at := h.clock.Now()
	h.transcriptOf("b", "t1", toolLines("u1", "c1", "ls", 0, at))
	h.ticks(2, time.Minute)
	if n := h.count(KindTurn, "", "b"); n != 0 {
		t.Fatalf("%d turns logged with no message open", n)
	}
	if err := UIInstruction(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "widgets", "b", "Rebase on main."); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, time.Minute)
	if len(h.typed("b")) != 1 {
		t.Fatalf("typed into b %q", h.typed("b"))
	}
	// b takes it, runs a tool, and is gone before it answers.
	at = h.clock.Now()
	h.transcriptOf("b", "t1", promptLine("u2", h.cfg.Policies.Instructions.Pointer, at)+toolLines("u3", "c2", "git rebase origin/main", 1, at))
	h.ticks(1, time.Minute)
	h.tmux.alive["b"] = false
	h.ticks(3, time.Minute)
	_, act := h.launchOf("b")
	if act == nil || len(act.Instructions) != 1 || !strings.Contains(act.Command, `"why":"instructed"`) {
		t.Fatalf("restart = %+v", act)
	}
	var b *Instruction
	for _, x := range h.logState().Instructions {
		if x.To == "b" {
			b = x
		}
	}
	if b.Delivered.IsZero() || !b.Taken.IsZero() || len(b.Activity) != 0 || b.Reply != "" {
		t.Fatalf("after the restart: %+v", b)
	}
	if docs := readInstructions(t, h.cfg.Sessions["b"].Workdir); len(docs) != 1 || !docs[0].New {
		t.Fatalf("instructions.json = %+v", docs)
	}
	transcript, _ := h.launchOf("b")
	at = h.clock.Now()
	h.transcriptOf("b", transcript, promptLine("u4", "launch prompt", at)+summaryLine("u5", "m1", "Rebased.", at, 100))
	h.ticks(2, time.Minute)
	live := h.sup.st.Instructions[b.ID]
	if live.Reply != "Rebased." {
		t.Fatalf("after the new session's turn: %+v", live)
	}
	folded := h.logState().Instructions[b.ID]
	if folded.Session != live.Session || !folded.Delivered.Equal(live.Delivered) || !folded.Taken.Equal(live.Taken) || folded.Reply != live.Reply || len(folded.Activity) != len(live.Activity) {
		t.Fatalf("the log folds to %+v, the run holds %+v", folded, live)
	}
}

// A message its session never answers within ChatIdle is let go: a later
// turn is not taken as its answer, nor is it handed over again.
func TestUnansweredMessageLetGo(t *testing.T) {
	h := turnsHarness(t)
	h.ticks(3, time.Minute)
	if err := UIInstruction(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "widgets", "b", "Rebase on main."); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, time.Minute)
	h.clock.advance(ChatIdle)
	at := h.clock.Now()
	h.transcriptOf("b", "t1", promptLine("u1", "something else", at)+summaryLine("u2", "m1", "Done with something else.", at, 100))
	h.ticks(2, time.Minute)
	if in := h.instruction(t); in.Delivered.IsZero() || !in.Taken.IsZero() || in.Reply != "" || h.sup.st.chatInFlight(h.clock.Now()) {
		t.Fatalf("an old message took a later turn: %+v", in)
	}
	if ids := h.sup.st.freshInstructions("b", true, h.clock.Now()); len(ids) != 0 {
		t.Fatalf("an old message would be handed over again: %v", ids)
	}
}
