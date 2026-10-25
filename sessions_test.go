package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sessionsHarness is coordHarness with widgets' coordinator c allowed to
// start sessions: each in the harness's directory, from a clone, cut from
// main.
func sessionsHarness(t *testing.T) *harness {
	t.Helper()
	h := coordHarness(t)
	clone := filepath.Join(h.dir, "clone")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	w := h.cfg.Repositories["widgets"]
	w.Sessions = &SessionsPolicy{Root: h.dir, Clone: clone, Target: "main"}
	w.Policy.Sessions.Max = 2
	h.cfg.Repositories["widgets"] = w
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	return h
}

// startSession is c's start_session call; it returns the reply.
func (h *harness) startSession(args map[string]any) (map[string]any, bool) {
	h.t.Helper()
	text, refused := callOne(h.t, h.mcpServerFor("c"), ToolStartSession, args)
	var reply map[string]any
	if err := json.Unmarshal([]byte(text), &reply); err != nil {
		h.t.Fatalf("reply %q: %v", text, err)
	}
	return reply, refused
}

func (h *harness) sessionEvents() []RuntimePayload {
	var out []RuntimePayload
	for _, e := range h.events() {
		if e.Kind == KindSession {
			out = append(out, *e.Runtime)
		}
	}
	return out
}

// A coordinator's start_session is a shared event; agentd then makes the
// session's workdir, brief and worktree, launches it under the restart
// command, and supervises it: its DONE wakes the coordinator.
func TestCoordinatorStartsSupervisedSession(t *testing.T) {
	h := sessionsHarness(t)
	reply, refused := h.startSession(map[string]any{"branch": "fix/spin", "issue": 7})
	name, _ := reply["name"].(string)
	if refused || reply["started"] != true || name != runtimeName("widgets", "fix/spin", KindWork) {
		t.Fatalf("start = %v", reply)
	}
	if ev := h.sessionEvents(); len(ev) != 1 || ev[0].By != "c" || ev[0].Branch != "fix/spin" || ev[0].Issue != 7 {
		t.Fatalf("session events = %+v", ev)
	}
	h.ticks(2, time.Minute)
	if n := h.count(KindAction, ActProvision, name); n != 1 {
		t.Fatalf("provisioned %d times", n)
	}
	if g := h.git.worktrees; len(g) != 1 || g[0] != filepath.Join(h.dir, "clone")+" "+filepath.Join(h.dir, name, "work")+" fix/spin main" {
		t.Fatalf("worktrees = %q", g)
	}
	brief, err := os.ReadFile(filepath.Join(h.dir, name, "BRIEF.md"))
	if err != nil || !strings.HasPrefix(string(brief), briefInstruction) || !strings.Contains(string(brief), `"branch":"fix/spin"`) || !strings.Contains(string(brief), `"issue":7`) {
		t.Fatalf("brief = %q, %v", brief, err)
	}
	if n := h.tmux.count("new-session", name); n != 1 {
		t.Fatalf("launched %d times", n)
	}
	sess := h.sup.cfg.Sessions[name]
	if !sess.Runtime || sess.Issue != 7 || sess.Cwd != filepath.Join(h.dir, name, "work") || len(sess.Deliver) != 1 || sess.Deliver[0].Target != "main" || !sess.Deliver[0].Draft {
		t.Fatalf("session = %+v", sess)
	}
	if _, ok := h.cfg.Sessions[name]; ok {
		t.Fatal("the started session was added to the configuration itself")
	}
	h.alive(name, "working on spin")
	h.ticks(2, time.Minute)
	h.status(name, "# s\n\nDONE\n")
	h.ticks(3, time.Minute)
	if w := h.wakes(); len(w) != 1 || w[0].Reason != "finished: "+name {
		t.Fatalf("wakes = %+v", w)
	}
}

// One session per branch and kind: a second start is refused and not
// logged, while a reviewer on the same branch is its own session. Another
// repository, a target, a branch name git would read otherwise, an account
// the repository does not allow, and a start over max are refused too.
func TestStartSessionRefusals(t *testing.T) {
	h := sessionsHarness(t)
	if _, refused := h.startSession(map[string]any{"branch": "fix-spin"}); refused {
		t.Fatal("first start refused")
	}
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"branch": "fix-spin"}, "already has a work session"},
		{map[string]any{"branch": "other", "repository": "readonly"}, `repository "readonly" is not widgets`},
		{map[string]any{"branch": "main"}, "is a target"},
		{map[string]any{"branch": "../x"}, "not a plain branch"},
		{map[string]any{"branch": "-x"}, "not a plain branch"},
		{map[string]any{"branch": "refs/heads/x"}, "not a plain branch"},
		{map[string]any{"branch": "x", "kind": "reviewer"}, "needs the issue"},
		{map[string]any{"branch": "x", "kind": "boss"}, "is not work, reviewer or judge"},
		{map[string]any{"branch": "x", "account": "other"}, `account "other" is not one`},
		{map[string]any{"branch": "x", "case": "widgets#7"}, "not the tool's"},
	} {
		reply, refused := h.startSession(c.args)
		if r, _ := reply["refused"].(string); !refused || !strings.Contains(r, c.want) {
			t.Errorf("%v: reply %v, want refusal %q", c.args, reply, c.want)
		}
	}
	if reply, refused := h.startSession(map[string]any{"branch": "fix-spin", "kind": "reviewer", "issue": 7}); refused || reply["name"] != runtimeName("widgets", "fix-spin", KindReviewer) {
		t.Fatalf("reviewer start = %v", reply)
	}
	if reply, refused := h.startSession(map[string]any{"branch": "third"}); !refused || !strings.Contains(reply["refused"].(string), "its max") {
		t.Fatalf("start over max = %v", reply)
	}
	if n := len(h.sessionEvents()); n != 2 {
		t.Fatalf("%d session events, want the two starts", n)
	}
}

// A replay rebuilds the started sessions from the log: a restarted agentd
// supervises them without provisioning or launching them again, and an
// ended session is stopped once and left out, its files kept.
func TestReplayRebuildsStartedSessions(t *testing.T) {
	h := sessionsHarness(t)
	reply, _ := h.startSession(map[string]any{"branch": "fix-spin"})
	name := reply["name"].(string)
	h.ticks(2, time.Minute)
	h.alive(name, "working")
	h.restart(false)
	if s, ok := h.sup.cfg.Sessions[name]; !ok || !s.Runtime {
		t.Fatalf("replay lost %s", name)
	}
	h.ticks(3, time.Minute)
	if p, n := h.count(KindAction, ActProvision, name), h.tmux.count("new-session", name); p != 1 || n != 1 {
		t.Fatalf("after replay: provisioned %d, launched %d", p, n)
	}
	rows, _ := Summarize(h.cfg, h.events(), ModeLive)
	found := false
	for _, r := range rows {
		found = found || r.Name == name
	}
	if !found {
		t.Fatalf("status lacks %s: %+v", name, rows)
	}
	text, refused := callOne(t, h.mcpServerFor("c"), ToolEndSession, map[string]any{"name": name})
	if refused || !strings.Contains(text, `"ended":true`) {
		t.Fatalf("end = %s", text)
	}
	h.ticks(2, time.Minute)
	if n, s := h.count(KindAction, ActEnd, name), h.tmux.count("stop", name); n != 1 || s != 1 {
		t.Fatalf("ended %d, stopped %d", n, s)
	}
	if _, ok := h.sup.cfg.Sessions[name]; ok {
		t.Fatal("an ended session is still supervised")
	}
	if _, err := os.Stat(filepath.Join(h.dir, name, "BRIEF.md")); err != nil {
		t.Fatalf("its files went: %v", err)
	}
	h.restart(false)
	h.ticks(2, time.Minute)
	if _, ok := h.sup.cfg.Sessions[name]; ok || h.tmux.count("stop", name) != 1 {
		t.Fatal("replay brought an ended session back")
	}
	if _, refused := callOne(t, h.mcpServerFor("c"), ToolEndSession, map[string]any{"name": name}); !refused {
		t.Fatal("ended twice")
	}
}

// The owner ends any started session; a name nobody started is an error.
// A failed worktree is tried again on the backoff, not launched meanwhile.
func TestOwnerEndsAndProvisionRetries(t *testing.T) {
	h := sessionsHarness(t)
	h.git.worktreeErr = errors.New("fetch failed")
	reply, _ := h.startSession(map[string]any{"branch": "fix-spin"})
	name := reply["name"].(string)
	h.ticks(1, time.Minute)
	if h.tmux.count("new-session", name) != 0 || h.count(KindError, "", name) == 0 {
		t.Fatal("a session without its worktree was launched, or its failure not logged")
	}
	h.git.worktreeErr = nil
	h.ticks(3, time.Minute)
	if h.tmux.count("new-session", name) != 1 {
		t.Fatal("not launched once its worktree was made")
	}
	if err := EndSession(h.cfg, NewLogWriter(h.buf), h.events(), h.clock.Now(), "nobody"); err == nil {
		t.Fatal("ended a session nobody started")
	}
	if err := EndSession(h.cfg, NewLogWriter(h.buf), h.events(), h.clock.Now(), name); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, time.Minute)
	if h.count(KindAction, ActEnd, name) != 1 {
		t.Fatal("the owner's end did not stop it")
	}
}

// A coordinator gets start_session and end_session only when its
// repository has a sessions entry, and approve always (#36); that entry
// needs a coordinator.
func TestCoordinatorServerNeedsSessionsEntry(t *testing.T) {
	h := coordHarness(t)
	rs := session(t, h.mcpServerFor("c"), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if len(rs) != 1 || len(rs[0].Result.Tools) != 1 || rs[0].Result.Tools[0].Name != ToolApprove {
		t.Fatalf("tools without a sessions entry = %+v", rs)
	}
	if got := mcpLaunchConfig(h.cfg, "c", h.cfg.Sessions["c"]); !strings.Contains(got, `"--session","c"`) {
		t.Fatalf("launch config = %s", got)
	}
	h = sessionsHarness(t)
	if got := mcpLaunchConfig(h.cfg, "c", h.cfg.Sessions["c"]); !strings.Contains(got, `"--session","c"`) {
		t.Fatalf("launch config = %s", got)
	}
	var f restartFacts
	if err := json.Unmarshal(h.sup.packFacts("c", h.cfg.Sessions["c"], h.sup.track("c"), "restart"), &f); err != nil || strings.Join(f.Tools, ",") != "start_session,end_session,approve" {
		t.Fatalf("facts tools = %v, %v", f.Tools, err)
	}
	rs = session(t, h.mcpServerFor("c"), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if len(rs) != 1 || len(rs[0].Result.Tools) != 3 || rs[0].Result.Tools[0].Name != ToolStartSession {
		t.Fatalf("tools = %+v", rs)
	}
	cfg := DefaultConfig()
	cfg.Repositories = map[string]Repository{"widgets": {Forge: "gitlab", Project: "example-org/widgets", Sessions: &SessionsPolicy{Root: h.dir, Clone: h.dir, Target: "main"}}}
	if err := cfg.finish(h.dir); err == nil || !strings.Contains(err.Error(), "needs the repository's coordinator") {
		t.Fatalf("finish = %v", err)
	}
}

// AddWorktree checks out a branch origin has, and cuts a new one from
// origin/target.
func TestExecGitAddWorktree(t *testing.T) {
	dir := t.TempDir()
	origin, clone := filepath.Join(dir, "origin"), filepath.Join(dir, "clone")
	gitRun(t, dir, "init", "-q", "--bare", origin)
	seed := filepath.Join(dir, "seed")
	gitRun(t, dir, "clone", "-q", origin, seed)
	writeFile(t, filepath.Join(seed, "a.txt"), "a\n")
	gitRun(t, seed, "add", "a.txt")
	gitRun(t, seed, "commit", "-q", "-m", "a")
	gitRun(t, seed, "push", "-q", "origin", "HEAD:refs/heads/main")
	gitRun(t, seed, "push", "-q", "origin", "HEAD:refs/heads/old")
	gitRun(t, dir, "clone", "-q", origin, clone)
	g := ExecGit{Timeout: time.Minute}
	for _, b := range []string{"old", "new/one"} {
		wt := filepath.Join(dir, "wt-"+strings.ReplaceAll(b, "/", "-"))
		if err := g.AddWorktree(clone, wt, b, "main", false); err != nil {
			t.Fatal(err)
		}
		if got, err := g.Branch(wt); err != nil || got != b {
			t.Fatalf("branch = %q, %v", got, err)
		}
	}
}

// A branch started again after its end is a new session: a fresh Track,
// a new brief and, where the last one finished, a fresh STATUS.md, so it
// is launched and not taken for DONE. The coordinator's server, launched
// while the first ran, starts it again.
func TestStartAgainAfterEnd(t *testing.T) {
	h := sessionsHarness(t)
	reply, _ := h.startSession(map[string]any{"branch": "fix-spin", "issue": 7})
	name := reply["name"].(string)
	h.ticks(2, time.Minute)
	h.alive(name, "working")
	h.ticks(1, time.Minute)
	h.status(name, "# s\n\nDONE\n")
	h.ticks(2, time.Minute)
	srv, err := NewMCPServer(projected(h.cfg, h.events()), "c", NewLogWriter(h.buf), func() ([]Event, error) { return h.events(), nil }, h.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, refused := callOne(t, srv, ToolEndSession, map[string]any{"name": name}); refused {
		t.Fatal("end refused")
	}
	h.ticks(1, time.Minute)
	if text, refused := callOne(t, srv, ToolStartSession, map[string]any{"branch": "fix-spin", "issue": 8}); refused {
		t.Fatalf("start again = %s", text)
	}
	h.ticks(3, time.Minute)
	if n := h.tmux.count("new-session", name); n != 2 {
		t.Fatalf("launched %d times, want twice", n)
	}
	brief, _ := os.ReadFile(filepath.Join(h.dir, name, "BRIEF.md"))
	status, _ := os.ReadFile(filepath.Join(h.dir, name, "STATUS.md"))
	if !strings.Contains(string(brief), `"issue":8`) || ParseStatus(string(status)).Done {
		t.Fatalf("brief %q, status %q", brief, status)
	}
	if tr := h.sup.track(name); tr.Parked || len(tr.Crashes) != 0 || tr.State == Done {
		t.Fatalf("track carried over: %+v", tr)
	}
}

// Two starts past max both appended: the fold holds the first only.
func TestFoldHoldsMax(t *testing.T) {
	h := sessionsHarness(t)
	for i, b := range []string{"a", "b", "c"} {
		h.sup.emit(runtimeName("widgets", b, KindWork), Event{Kind: KindSession, Mode: ModeShared,
			Runtime: &RuntimePayload{ID: string(rune('x' + i)), Action: SessionStart, Repository: "widgets", Kind: KindWork, Branch: b, Max: 2, By: "c"}})
	}
	if rt := projectedRuntime(h.cfg, h.events()); len(rt) != 2 {
		t.Fatalf("running = %v, want max 2", rt)
	}
}

// A started session's decision is answered from the console, not taken
// for an instruction.
func TestConsoleAnswersStartedSession(t *testing.T) {
	h := sessionsHarness(t)
	reply, _ := h.startSession(map[string]any{"branch": "fix-spin"})
	name := reply["name"].(string)
	st := NewLogState(h.cfg, ModeLive).Fold(h.events())
	if !replyForm(st, withRuntime(h.cfg, st.Runtime), []string{name, "D1", "yes"}) {
		t.Fatal("not an answer form")
	}
	if err := ConsoleReply(h.cfg, NewLogWriter(h.buf), st, h.clock.Now(), "widgets", name+" D1 yes"); err == nil {
		t.Fatal("an answer to a decision never raised was taken")
	}
	if h.count(KindInstruction, "", "") != 0 {
		t.Fatal("the answer became an instruction")
	}
}

// A reviewer's worktree is detached at the branch its worker has out.
func TestExecGitReviewerBesideWorker(t *testing.T) {
	dir := t.TempDir()
	origin, clone := filepath.Join(dir, "origin"), filepath.Join(dir, "clone")
	gitRun(t, dir, "init", "-q", "--bare", origin)
	seed := filepath.Join(dir, "seed")
	gitRun(t, dir, "clone", "-q", origin, seed)
	writeFile(t, filepath.Join(seed, "a.txt"), "a\n")
	gitRun(t, seed, "add", "a.txt")
	gitRun(t, seed, "commit", "-q", "-m", "a")
	gitRun(t, seed, "push", "-q", "origin", "HEAD:refs/heads/main")
	gitRun(t, dir, "clone", "-q", origin, clone)
	g := ExecGit{Timeout: time.Minute}
	work, review := filepath.Join(dir, "work"), filepath.Join(dir, "review")
	if err := g.AddWorktree(clone, work, "fix", "main", false); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(work, "b.txt"), "b\n")
	gitRun(t, work, "add", "b.txt")
	gitRun(t, work, "commit", "-q", "-m", "b")
	if err := g.AddWorktree(clone, review, "fix", "main", true); err != nil {
		t.Fatal(err)
	}
	if a, b := gitRun(t, work, "rev-parse", "HEAD"), gitRun(t, review, "rev-parse", "HEAD"); a != b {
		t.Fatalf("reviewer at %s, worker at %s", b, a)
	}
}

// An end and a new start of the name in one pass: the old tmux session is
// stopped first, and only then is the new one provisioned and launched.
// A reviewer's worktree is detached.
func TestEndAndStartInOnePass(t *testing.T) {
	h := sessionsHarness(t)
	reply, _ := h.startSession(map[string]any{"branch": "fix-spin", "kind": "reviewer", "issue": 7})
	name := reply["name"].(string)
	h.ticks(2, time.Minute)
	if g := h.git.worktrees; len(g) != 1 || !strings.HasSuffix(g[0], " detached") {
		t.Fatalf("worktrees = %q", g)
	}
	h.alive(name, "reviewing")
	h.ticks(1, time.Minute)
	srv := h.mcpServerFor("c")
	callOne(t, srv, ToolEndSession, map[string]any{"name": name})
	if text, refused := callOne(t, srv, ToolStartSession, map[string]any{"branch": "fix-spin", "kind": "reviewer", "issue": 8}); refused {
		t.Fatalf("start again = %s", text)
	}
	h.ticks(3, time.Minute)
	if s, n := h.tmux.count("stop", name), h.tmux.count("new-session", name); s != 1 || n != 2 {
		t.Fatalf("stopped %d, launched %d", s, n)
	}
	var stop, launch int
	for i, c := range h.tmux.calls {
		if c.name == name && c.op == "stop" {
			stop = i
		}
		if c.name == name && c.op == "new-session" {
			launch = i
		}
	}
	if stop > launch {
		t.Fatal("launched before the old session was stopped")
	}
}

// An end whose stop keeps failing goes to the repository's console, once.
func TestEndThatKeepsFailingEscalates(t *testing.T) {
	h := sessionsHarness(t)
	reply, _ := h.startSession(map[string]any{"branch": "fix-spin"})
	name := reply["name"].(string)
	h.ticks(2, time.Minute)
	h.alive(name, "working")
	h.ticks(1, time.Minute)
	h.tmux.stopErr = map[string]error{name: errors.New("tmux hung")}
	if err := EndSession(h.cfg, NewLogWriter(h.buf), h.events(), h.clock.Now(), name); err != nil {
		t.Fatal(err)
	}
	h.ticks(40, 10*time.Minute)
	var posts []*ActPayload
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActEscalate && e.Session == name {
			posts = append(posts, e.Act)
		}
	}
	if len(posts) != 1 || posts[0].Console != "widgets" || !strings.Contains(posts[0].Message, "could not stop") {
		t.Fatalf("escalations = %d, first %+v", len(posts), *posts[0])
	}
}

// A start made under max 2 holds after max is lowered to 1.
func TestStartHeldByItsOwnMax(t *testing.T) {
	h := sessionsHarness(t)
	h.startSession(map[string]any{"branch": "a"})
	h.startSession(map[string]any{"branch": "b"})
	w := h.cfg.Repositories["widgets"]
	w.Policy.Sessions.Max = 1
	h.cfg.Repositories["widgets"] = w
	if rt := projectedRuntime(h.cfg, h.events()); len(rt) != 2 {
		t.Fatalf("running = %v", rt)
	}
}
