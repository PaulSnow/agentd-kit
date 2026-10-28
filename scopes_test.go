package main

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// scoped records what the owner wrote in scope's console box, as the
// page's form does.
func scoped(h *harness, scope, to, text string, now bool) error {
	c, err := ParseScope(h.sup.cfg, scope)
	if err != nil {
		return err
	}
	return ScopeInstruction(h.sup.cfg, NewLogWriter(h.buf), h.clock.Now(), c, to, text, now, nil)
}

// A message at an issue goes to the issue's session at its next turn:
// not while it works, but once its turn ends; with now it is typed at
// once.
func TestIssueScopeWaitsForTheNextTurn(t *testing.T) {
	h := sessionsHarness(t)
	h.cfg.Policies.Nudge.Enabled = true // a still screen is a turn ended
	w1 := h.startSession("issue-1", KindWork, 1)
	h.ticks(2, time.Minute)
	for i := 0; i < 3; i++ {
		h.alive(w1, "w1 works "+itoa(i))
		h.ticks(1, time.Minute)
	}
	if err := scoped(h, "widgets#1", w1, "Use the small cache.", false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		h.alive(w1, "w1 still works "+itoa(i))
		h.ticks(1, time.Minute)
	}
	if in := h.instruction(t); in.Session != w1 || !in.Delivered.IsZero() || in.Scope != "widgets#1" {
		t.Fatalf("typed while it works: %+v", in)
	}
	h.alive(w1, "w1 waits at its prompt")
	h.ticks(int(h.cfg.Policies.Nudge.Stall.Duration/time.Minute)+3, time.Minute)
	if in := h.instruction(t); in.Delivered.IsZero() {
		t.Fatalf("not typed at its next turn: %+v", in)
	}

	h2 := sessionsHarness(t)
	w2 := h2.startSession("issue-2", KindWork, 2)
	h2.ticks(2, time.Minute)
	for i := 0; i < 2; i++ {
		h2.alive(w2, "w2 works "+itoa(i))
		h2.ticks(1, time.Minute)
	}
	if err := scoped(h2, "widgets#2", w2, "Stop and push what you have.", true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		h2.alive(w2, "w2 works on "+itoa(i))
		h2.ticks(1, time.Minute)
	}
	if in := h2.instruction(t); in.Delivered.IsZero() || !in.Now {
		t.Fatalf("now was not typed at once: %+v", in)
	}
}

// What the owner writes at a repository never reaches a running session,
// even when the agent hands it over; work it names #N is that issue's.
func TestRepoScopeNeverTypesIntoASession(t *testing.T) {
	dir := fakeClaude(t)
	h := sessionsHarness(t)
	h.cfg.Jobs.Designer.Off = true
	w1 := h.startSession("issue-1", KindWork, 1)
	h.ticks(2, time.Minute)
	h.alive(w1, "w1 works")
	h.ticks(1, time.Minute)
	claudeAnswers(t, dir, 1, agentOut("Handed to "+w1+".", w1))
	if err := scoped(h, "widgets", "", "Fix the squeak first.", false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		h.alive(w1, "w1 works "+itoa(i))
		h.ticks(1, time.Minute)
	}
	in := h.instruction(t)
	if in.Session == w1 || in.AgentLast == nil || in.AgentLast.HandTo != "" || h.count(KindAction, ActInstruct, w1) != 0 {
		t.Fatalf("a repository's message reached %s: %+v", w1, in)
	}

	h2 := sessionsHarness(t)
	h2.cfg.Jobs.Designer.Off = true
	claudeAnswers(t, dir, 0, agentWork("Go starts a session for it.", "Fix the crash", "Fix it."))
	if err := scoped(h2, "widgets", "", "Please fix the crash in #7.", false); err != nil {
		t.Fatal(err)
	}
	h2.ticks(4, time.Minute)
	st := h2.logState()
	var got *WorkItem
	for _, w := range st.Work {
		got = w
	}
	if got == nil || got.Issue != 7 {
		t.Fatalf("work item %+v", got)
	}
}

// A question at home that names no repository is answered by the home
// thread, given every repository's level and today's tokens, and its
// answer is the home conversation's.
func TestHomeScopeAnswered(t *testing.T) {
	dir := fakeClaude(t)
	claudeAnswers(t, dir, 0, claudeResult(`{"answer":"widgets has nothing open that needs you."}`))
	h := sessionsHarness(t)
	if err := scoped(h, "home", "", "Anything need me today?", false); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	in := h.instruction(t)
	if in.Scope != ScopeHome || len(in.Repositories) != 0 || in.AgentLast == nil || !strings.Contains(in.AgentLast.Answer, "nothing open") {
		t.Fatalf("home question: %+v %+v", in, in.AgentLast)
	}
	if x := readFake(t, dir, "in.1"); !strings.Contains(x, `"name":"widgets"`) || !strings.Contains(x, `"today_tokens"`) || !strings.Contains(x, "Anything need me today?") {
		t.Fatalf("home input %s", x)
	}
	if c := h.logState().scopeConversation(ScopeHome, 5); len(c) != 1 || c[0].State != "answered" {
		t.Fatalf("conversation %+v", c)
	}
	// A message at home naming a repository is that repository's, and
	// still hands nothing to a session.
	h2 := sessionsHarness(t)
	if err := scoped(h2, "home", "", "How is widgets doing?", false); err != nil {
		t.Fatal(err)
	}
	if in := h2.instruction(t); len(in.Repositories) != 1 || in.Repositories[0] != "widgets" || in.Scope != ScopeHome || scopeHandsOver(in.Scope) {
		t.Fatalf("named: %+v", in)
	}
}

// Scopes parse and read back; an issue's number is taken from words that
// name one issue only.
func TestScopes(t *testing.T) {
	h := newHarness(t, false)
	for in, want := range map[string]Scope{"home": {}, "": {}, "widgets": {Repository: "widgets"}, "widgets#12": {Repository: "widgets", Issue: 12}} {
		got, err := ParseScope(h.cfg, in)
		if err != nil || got != want || scopeOf(got.String()) != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
	}
	for _, bad := range []string{"nowhere", "widgets#x", "widgets#0"} {
		if _, err := ParseScope(h.cfg, bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	for text, want := range map[string]int{"fix #7": 7, "fix #7 and #7 again": 7, "fix #7 and #8": 0, "fix &#39; things": 0, "see !7": 0} {
		w := ThreadWork{}
		scopedIssue(&Instruction{InstructionPayload: InstructionPayload{Scope: "widgets", Text: text}}, &w)
		if w.Issue != want {
			t.Errorf("%q: %d", text, w.Issue)
		}
	}
	w := ThreadWork{}
	scopedIssue(&Instruction{InstructionPayload: InstructionPayload{Scope: "widgets#9", Text: "fix #7"}}, &w)
	if w.Issue != 9 {
		t.Errorf("issue scope: %d", w.Issue)
	}
	if namedRepository(h.cfg, "How is widgets doing?") != "widgets" || namedRepository(h.cfg, "widgets and readonly") != "" || namedRepository(h.cfg, "gadgets") != "" {
		t.Error("named repository")
	}
}

// The console box posts with its scope from every level: at home and at
// a repository nothing is addressed to a session; at an issue the
// message is addressed to its session, now only when asked; a typed
// option of the level's one open decision is that decision's answer.
func TestConsoleBoxPosts(t *testing.T) {
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{}, f.src)
	for _, page := range []string{"/", "/repo/widgets", "/repo/widgets/issue/4"} {
		if body := owner(t, u, page); !strings.Contains(body, `id="console-box"`) {
			t.Errorf("%s has no console box", page)
		}
	}
	post := func(scope, text string, now bool) {
		t.Helper()
		v := url.Values{"src": {"live"}, "scope": {scope}, "text": {text}, "back": {"/"}}
		if now {
			v.Set("now", "1")
		}
		if w := ownerPost(u, "/reply", v); w.Code != 303 {
			t.Fatalf("%s: %d %s", scope, w.Code, w.Body.String())
		}
	}
	post("home", "What needs me?", false)
	post("widgets", "Look at #6 next.", false)
	post("widgets#4", "Use the small cache.", false)
	post("widgets#4", "Push now.", true)
	post("widgets#5", "Decline", false)
	events, _, _ := f.log.Tail(0)
	var got []*InstructionPayload
	for _, e := range events {
		if e.Kind == KindInstruction {
			got = append(got, e.Instruction)
		}
	}
	if len(got) != 5 {
		t.Fatalf("%d instructions", len(got))
	}
	if g := got[0]; g.Scope != "home" || len(g.Repositories) != 0 || g.To != "" {
		t.Errorf("home: %+v", g)
	}
	if g := got[1]; g.Scope != "widgets" || g.Repositories[0] != "widgets" || g.To != "" || g.Now {
		t.Errorf("repository: %+v", g)
	}
	if g := got[2]; g.Scope != "widgets#4" || g.To != "w4" || g.Now {
		t.Errorf("issue: %+v", g)
	}
	if g := got[3]; g.To != "w4" || !g.Now {
		t.Errorf("issue, now: %+v", g)
	}
	if g := got[4]; g.Re == nil || g.Re.ID != "w:wa1" || g.Answer == nil || g.Answer.Choice != "decline" {
		t.Errorf("typed decision: %+v", g)
	}
	if body := owner(t, u, "/repo/widgets"); !strings.Contains(body, "Look at #6 next.") {
		t.Error("the repository's conversation does not show its message")
	}
	if body := owner(t, u, "/"); !strings.Contains(body, "What needs me?") || strings.Contains(body, "Look at #6 next.") {
		t.Error("the home conversation is not its own")
	}
	if w := ownerPost(u, "/reply", url.Values{"src": {"live"}, "scope": {"nowhere"}, "text": {"x"}}); w.Code != 400 {
		t.Errorf("an unknown scope: %d", w.Code)
	}
}
