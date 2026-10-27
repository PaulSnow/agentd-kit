package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// agentOut is what a fake thread agent prints: its tool calls, then its
// result with answer, hand_to and what it cost.
func agentOut(answer, handTo string, tools ...string) string {
	var b strings.Builder
	for i, t := range tools {
		line, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]any{
			{"type": "tool_use", "id": "c" + string(rune('a'+i)), "name": "Read", "input": map[string]any{"file_path": t}}}}})
		b.Write(append(line, '\n'))
	}
	out, _ := json.Marshal(map[string]any{"answer": answer, "hand_to": handTo})
	res, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "structured_output": json.RawMessage(out),
		"usage": map[string]any{"input_tokens": 12, "cache_creation_input_tokens": 2000, "cache_read_input_tokens": 9000, "output_tokens": 300}, "total_cost_usd": 0.01})
	b.Write(append(res, '\n'))
	return b.String()
}

// agentInput is what the fake was given on call n.
func agentInput(t *testing.T, dir string, n int) threadInput {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "in."+itoa(n)))
	if err != nil {
		t.Fatal(err)
	}
	var x threadInput
	if err := json.Unmarshal(b, &x); err != nil {
		t.Fatalf("call %d's input: %v\n%s", n, err, b)
	}
	return x
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// A message in the repository's box goes to a thread agent started for
// it alone (#59): no chat session is started. The agent reads in the
// clone with the read-only tools and the context tools (#60), is given
// the message and the running sessions, and its answer, its tools and
// what it was given and cost are logged and threaded under the message.
// A follow-up in the thread starts a new agent with the thread so far.
func TestThreadAgentAnswers(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	w1 := h.startSession("issue-1", KindWork, 1)
	h.ticks(2, time.Minute)
	claudeAnswers(t, dir, 1, agentOut("Two tests are left on the squeak, both in squeak_test.go.", "", "squeak_test.go"))
	if err := reply(h, "widgets", "What is left on the squeak?"); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	in := h.instruction(t)
	if !in.Agent || in.Session != "" || in.Reply != "Two tests are left on the squeak, both in squeak_test.go." || len(in.Activity) != 1 || in.Activity[0].Text != "Read: squeak_test.go" {
		t.Fatalf("instruction = %+v", in)
	}
	if _, chat := h.sup.st.Runtime[runtimeName("widgets", chatBranch, KindChat)]; chat || claudeCalls(t, dir) != 1 {
		t.Fatalf("a chat session was started, or %d calls", claudeCalls(t, dir))
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args.1"))
	for _, want := range []string{"stream-json", "--tools\nRead,Grep,Glob\n", "mcp__agentd__state", `"--repository","widgets"`, `"--as","thread:` + in.ID + `"`} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("the agent's arguments lack %q:\n%s", want, args)
		}
	}
	x := agentInput(t, dir, 1)
	running := map[string]bool{}
	for _, r := range x.Running {
		running[r.Name] = true
	}
	if x.Message.Text != "What is left on the squeak?" || len(x.Earlier) != 0 || !running[w1] || x.Worktree != filepath.Join(h.dir, "clone") {
		t.Fatalf("the agent's input = %+v", x)
	}
	var answered *AgentPayload
	for _, e := range h.events() {
		if e.Kind == KindRoute && e.Route.Step == RouteAnswered {
			answered = e.Route.Agent
		}
	}
	if answered == nil || answered.Usage == nil || answered.Usage.InputTokens != 12 || answered.Usage.CacheReadTokens != 9000 || answered.InputBytes == 0 {
		t.Fatalf("the agent's tokens are not logged: %+v", answered)
	}
	// A follow-up to the agent's answer: a new agent, given the thread.
	claudeAnswers(t, dir, 2, agentOut("The docs wait on the squeak.", ""))
	src := newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a")
	if err := src.respond("widgets", entryInstruction+in.ID, "", "And the docs?", nil); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	x = agentInput(t, dir, 2)
	if len(x.Earlier) != 2 || x.Earlier[0].Text != "What is left on the squeak?" || x.Earlier[1].From != "agent" || x.Thread != entryInstruction+in.ID || x.Message.Text != "And the docs?" {
		t.Fatalf("the follow-up's input = %+v", x)
	}
}

// Twenty responses in twenty threads never make one agent's context
// larger than its own thread: no record of another thread is given.
func TestThreadAgentContextIsItsThread(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 0, agentOut("Noted.", ""))
	first := 0
	for i := 0; i < 20; i++ {
		if err := reply(h, "widgets", "Question "+itoa(i)+": how is the paint?"); err != nil {
			t.Fatal(err)
		}
		h.ticks(1, time.Minute)
		x := agentInput(t, dir, i+1)
		b, _ := json.Marshal(x)
		if first == 0 {
			first = len(b)
		}
		if len(x.Earlier) != 0 || len(b) > first+16 {
			t.Fatalf("response %d's input holds %d earlier records, %d bytes against %d", i, len(x.Earlier), len(b), first)
		}
	}
}

// The agent hands work that belongs to a running session over: the
// owner's record goes to that session, with the entry, as any other. A
// hand-over to no running session is refused; the agent is started
// once more, and the response then fails, saying why.
func TestThreadAgentHandsOverOrFails(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	w1 := h.startSession("issue-1", KindWork, 1)
	h.ticks(2, time.Minute)
	h.alive(w1, "w1 works")
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 1, agentOut("Handed to "+w1+", which works on the squeak.", w1))
	if err := reply(h, "widgets", "Fix the squeak first."); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		h.alive(w1, "w1 works "+itoa(i))
		h.ticks(1, time.Minute)
	}
	in := h.instruction(t)
	if in.Session != w1 || in.Delivered.IsZero() || in.AgentLast == nil || in.AgentLast.HandTo != w1 {
		t.Fatalf("instruction = %+v", in)
	}
	if docs := readInstructions(t, h.sup.cfg.Sessions[w1].Workdir); len(docs) != 1 || docs[0].Text != "Fix the squeak first." {
		t.Fatalf("instructions.json = %+v", docs)
	}

	h2 := sessionsHarness(t)
	claudeAnswers(t, dir, 0, agentOut("Handed over.", "nobody"))
	before := claudeCalls(t, dir)
	if err := reply(h2, "widgets", "Paint it red."); err != nil {
		t.Fatal(err)
	}
	h2.ticks(4, time.Minute)
	in = h2.instruction(t)
	if n := claudeCalls(t, dir) - before; n != agentRuns || in.Failed == "" || !strings.Contains(in.Failed, "no running session") {
		t.Fatalf("%d runs; instruction = %+v", n, in)
	}
}

// A chat session an older agentd started is ended once nothing of the
// owner's is open on it: the console keeps no long-lived session.
func TestOldChatSessionEnded(t *testing.T) {
	h := sessionsHarness(t)
	name := runtimeName("widgets", chatBranch, KindChat)
	if why := h.sup.startRuntime("widgets", name, RuntimePayload{Kind: KindChat, Branch: chatBranch}); why != "" {
		t.Fatal(why)
	}
	h.sup.refreshRuntime()
	h.ticks(2, time.Minute)
	if _, ok := h.sup.st.Runtime[name]; ok {
		t.Fatal("the old chat session was not ended")
	}
}
