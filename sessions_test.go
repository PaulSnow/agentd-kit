package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repoHarness is a, b and d, with d in readonly, and the transcripts
// under the harness's directory.
func repoHarness(t *testing.T) *harness {
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

// sessionsHarness is repoHarness with a sessions entry for widgets, so
// agentd starts sessions there: each in the harness's directory, from a
// clone, cut from main.
func sessionsHarness(t *testing.T) *harness {
	t.Helper()
	h := repoHarness(t)
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

// startSession is agentd's start of a widgets session of kind on branch,
// as its loop makes one; it returns the name.
func (h *harness) startSession(branch, kind string, issue int) string {
	h.t.Helper()
	name := runtimeName("widgets", branch, kind)
	if why := h.sup.startRuntime("widgets", name, RuntimePayload{Kind: kind, Branch: branch, Issue: issue}); why != "" {
		h.t.Fatalf("start of %s refused: %s", name, why)
	}
	h.sup.refreshRuntime()
	return name
}

// endSession is the owner's agentd sessions end.
func (h *harness) endSession(name string) error {
	return EndSession(h.cfg, NewLogWriter(h.buf), h.events(), h.clock.Now(), name)
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

// A started session is a shared event; agentd then makes the session's
// workdir, brief and worktree, launches it under the restart command, and
// supervises it.
func TestStartedSessionSupervised(t *testing.T) {
	h := sessionsHarness(t)
	name := h.startSession("fix/spin", KindWork, 7)
	if ev := h.sessionEvents(); len(ev) != 1 || ev[0].Branch != "fix/spin" || ev[0].Issue != 7 {
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
	if st := h.state(name); st != Done {
		t.Fatalf("state = %v, want done", st)
	}
}

// One session per name: agentd's second start of a running name is
// refused and not logged, while a reviewer on the same branch is its own
// session; a start over max, or under a configured session's name, is
// refused too.
func TestStartRuntimeRefusals(t *testing.T) {
	h := sessionsHarness(t)
	work := runtimeName("widgets", "fix-spin", KindWork)
	if why := h.sup.startRuntime("widgets", work, RuntimePayload{Kind: KindWork, Branch: "fix-spin"}); why != "" {
		t.Fatalf("first start refused: %s", why)
	}
	if why := h.sup.startRuntime("widgets", work, RuntimePayload{Kind: KindWork, Branch: "fix-spin"}); !strings.Contains(why, "running already") {
		t.Fatalf("second start = %q", why)
	}
	if why := h.sup.startRuntime("widgets", runtimeName("widgets", "fix-spin", KindReviewer), RuntimePayload{Kind: KindReviewer, Branch: "fix-spin", Issue: 7}); why != "" {
		t.Fatalf("reviewer start refused: %s", why)
	}
	if why := h.sup.startRuntime("widgets", "a", RuntimePayload{Kind: KindWork, Branch: "a"}); !strings.Contains(why, "configured session") {
		t.Fatalf("start under a configured name = %q", why)
	}
	if why := h.sup.startRuntime("widgets", runtimeName("widgets", "third", KindWork), RuntimePayload{Kind: KindWork, Branch: "third"}); !strings.Contains(why, "its max") {
		t.Fatalf("start over max = %q", why)
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
	name := h.startSession("fix-spin", KindWork, 0)
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
	if err := h.endSession(name); err != nil {
		t.Fatal(err)
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
	if err := h.endSession(name); err == nil {
		t.Fatal("ended twice")
	}
}

// The owner ends any started session; a name nobody started is an error.
// A failed worktree is tried again on the backoff, not launched meanwhile.
func TestOwnerEndsAndProvisionRetries(t *testing.T) {
	h := sessionsHarness(t)
	h.git.worktreeErr = errors.New("fetch failed")
	name := h.startSession("fix-spin", KindWork, 0)
	h.ticks(1, time.Minute)
	if h.tmux.count("new-session", name) != 0 || h.count(KindError, "", name) == 0 {
		t.Fatal("a session without its worktree was launched, or its failure not logged")
	}
	h.git.worktreeErr = nil
	h.ticks(3, time.Minute)
	if h.tmux.count("new-session", name) != 1 {
		t.Fatal("not launched once its worktree was made")
	}
	if err := h.endSession("nobody"); err == nil {
		t.Fatal("ended a session nobody started")
	}
	if err := h.endSession(name); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, time.Minute)
	if h.count(KindAction, ActEnd, name) != 1 {
		t.Fatal("the owner's end did not stop it")
	}
}

// A sessions entry needs no other key: agentd starts the sessions itself
// (#53).
func TestSessionsEntryAlone(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Repositories = map[string]Repository{"widgets": {Forge: "gitlab", Project: "example-org/widgets", Sessions: &SessionsPolicy{Root: dir, Clone: dir, Target: "main"}}}
	if err := cfg.finish(dir); err != nil {
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
// is launched and not taken for DONE.
func TestStartAgainAfterEnd(t *testing.T) {
	h := sessionsHarness(t)
	name := h.startSession("fix-spin", KindWork, 7)
	h.ticks(2, time.Minute)
	h.alive(name, "working")
	h.ticks(1, time.Minute)
	h.status(name, "# s\n\nDONE\n")
	h.ticks(2, time.Minute)
	if err := h.endSession(name); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, time.Minute)
	h.startSession("fix-spin", KindWork, 8)
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
			Runtime: &RuntimePayload{ID: string(rune('x' + i)), Action: SessionStart, Repository: "widgets", Kind: KindWork, Branch: b, Max: 2, By: "test"}})
	}
	if rt := projectedRuntime(h.cfg, h.events()); len(rt) != 2 {
		t.Fatalf("running = %v, want max 2", rt)
	}
}

// A started session's decision is answered from the console, not taken
// for an instruction.
func TestConsoleAnswersStartedSession(t *testing.T) {
	h := sessionsHarness(t)
	name := h.startSession("fix-spin", KindWork, 0)
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
	// The test, not the loop, starts the reviewer, so the loop leaves it.
	review := func(issue int) string {
		name := runtimeName("widgets", "fix-spin", KindReviewer)
		h.sup.emit(name, Event{Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: newIntentID(), Action: SessionStart,
			Repository: "widgets", Kind: KindReviewer, Branch: "fix-spin", Issue: issue, Max: 2, By: "test"}})
		h.sup.refreshRuntime()
		return name
	}
	name := review(7)
	h.ticks(2, time.Minute)
	if g := h.git.worktrees; len(g) != 1 || !strings.HasSuffix(g[0], " detached") {
		t.Fatalf("worktrees = %q", g)
	}
	h.alive(name, "reviewing")
	h.ticks(1, time.Minute)
	if err := h.endSession(name); err != nil {
		t.Fatal(err)
	}
	review(8)
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
	name := h.startSession("fix-spin", KindWork, 0)
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
	h.startSession("a", KindWork, 0)
	h.startSession("b", KindWork, 0)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Sessions.Max = 1
	h.cfg.Repositories["widgets"] = w
	if rt := projectedRuntime(h.cfg, h.events()); len(rt) != 2 {
		t.Fatalf("running = %v", rt)
	}
}
