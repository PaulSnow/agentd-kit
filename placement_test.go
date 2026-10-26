package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// placementHarness is the hub harness with widgets' started sessions on
// host-b, under account acct-b: each node has a sessions root and a launch
// command, and acct-b is logged in on host-b alone. host-b has widgets'
// clone.
func placementHarness(t *testing.T) *hubHarness {
	t.Helper()
	h := newHubHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Sessions = &SessionsPolicy{Root: t.TempDir(), Clone: t.TempDir(), Target: "main", Host: "host-b"}
	w.Policy.Sessions.Accounts = []string{"acct-b"}
	h.cfg.Repositories["widgets"] = w
	for _, name := range []string{"host-a", "host-b"} {
		n := h.nodes[name]
		n.cfg.SessionsRoot = filepath.Join(filepath.Dir(n.cfg.ReposRoot), "sessions")
		n.cfg.Launch = []string{"claude", "--model", "{model}", "Read {brief} and follow it."}
		n.cfg.Accounts = map[string]Account{}
	}
	dir := filepath.Join(filepath.Dir(h.nodes["host-b"].cfg.ReposRoot), "claude-b")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	h.nodes["host-b"].cfg.Accounts["acct-b"] = Account{ConfigDir: dir}
	h.makeClone("host-b", "github.com/example-org/widgets")
	return h
}

// start is agentd's start of a widgets work session on branch, as its
// loop records one, on host and account; it returns the name and the
// start's id.
func (h *hubHarness) start(branch, host, account string) (string, string) {
	h.t.Helper()
	name := runtimeName("widgets", branch, KindWork)
	id := newIntentID()
	if err := h.log.Append(Event{V: EventVersion, Time: h.now, Session: name, Kind: KindSession, Mode: ModeShared,
		Runtime: &RuntimePayload{ID: id, Action: SessionStart, Repository: "widgets", Kind: KindWork, Branch: branch, Issue: 7, Account: account, Host: host, Max: 4, By: "agentd"}}); err != nil {
		h.t.Fatal(err)
	}
	return name, id
}

func (h *hubHarness) end(name string) {
	h.t.Helper()
	if err := h.log.Append(Event{V: EventVersion, Time: h.now, Session: name, Kind: KindSession, Mode: ModeOwner,
		Runtime: &RuntimePayload{ID: newIntentID(), Action: SessionEnd, Repository: "widgets", By: "owner"}}); err != nil {
		h.t.Fatal(err)
	}
}

// sessionRows are the remote sessions' rows, by name.
func (h *hubHarness) sessionRows() map[string]SessionRow {
	h.t.Helper()
	events, _, err := ReadLog(h.cfg.EventLog)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]SessionRow{}
	for _, r := range RemoteSessionStatus(h.cfg, events, h.now) {
		out[r.Name] = r
	}
	return out
}

// results are the log's results of session commands.
func (h *hubHarness) sessionResults() []NodeResult {
	var out []NodeResult
	for _, p := range h.nodeEvents() {
		if p.Result != nil && p.Result.Session != "" {
			out = append(out, *p.Result)
		}
	}
	return out
}

// A session starts on the host its start names, under its account, through
// that host's node: the node adds the worktree from its own clone, writes
// the brief, and starts tmux with the account's config directory. The
// other host is asked for nothing. Its report then shows it running; the
// end stops it through the same node, once.
func TestSessionStartsOnNamedHostAndAccount(t *testing.T) {
	h := placementHarness(t)
	h.cfg.Policies.Models.Work = "model-w"
	name, _ := h.start("fix-spin", "host-b", "acct-b")
	h.tick("host-a")
	h.tick("host-b")
	if n := h.tmuxes["host-a"].count("new-session", ""); n != 0 {
		t.Fatalf("host-a started %d sessions", n)
	}
	if n := h.tmuxes["host-b"].count("new-session", name); n != 1 {
		t.Fatalf("host-b started %s %d times", name, n)
	}
	nb := h.nodes["host-b"].cfg
	work := filepath.Join(nb.SessionsRoot, name, "work")
	clone := filepath.Join(nb.ReposRoot, "github.com/example-org/widgets")
	if w := h.gits["host-b"].worktrees; len(w) != 1 || w[0] != clone+" "+work+" fix-spin main" {
		t.Fatalf("worktrees = %q", w)
	}
	var c call
	for _, k := range h.tmuxes["host-b"].calls {
		if k.op == "new-session" {
			c = k
		}
	}
	if c.args[0] != work || !strings.Contains(c.args[1], "CLAUDE_CONFIG_DIR="+nb.Accounts["acct-b"].ConfigDir) ||
		!strings.Contains(c.args[1], "exec claude --model model-w ") || !strings.Contains(c.args[1], filepath.Join(nb.SessionsRoot, name, "BRIEF.md")) {
		t.Fatalf("new-session = %q", c.args)
	}
	brief, err := os.ReadFile(filepath.Join(nb.SessionsRoot, name, "BRIEF.md"))
	if err != nil || !strings.HasPrefix(string(brief), briefInstruction) || !strings.Contains(string(brief), `"branch":"fix-spin"`) || !strings.Contains(string(brief), `"issue":7`) {
		t.Fatalf("brief = %q, %v", brief, err)
	}
	if r := h.sessionResults(); len(r) != 1 || r[0].Action != NodeSessionStart || r[0].Outcome != NodeDone {
		t.Fatalf("results = %+v", r)
	}
	h.now = h.now.Add(time.Minute)
	h.tick("host-b")
	if n := h.tmuxes["host-b"].count("new-session", name); n != 1 {
		t.Fatalf("started again: %d", n)
	}
	if r := h.sessionRows()[name]; r.Host != "host-b" || r.Account != "acct-b" || r.State != SessionRunning {
		t.Fatalf("row = %+v", r)
	}

	h.end(name)
	h.tick("host-a")
	h.tick("host-b")
	h.tick("host-b")
	if n := h.tmuxes["host-b"].count("stop", name); n != 1 {
		t.Fatalf("stopped %d times", n)
	}
	if n := h.tmuxes["host-a"].count("stop", ""); n != 0 {
		t.Fatalf("host-a stopped %d", n)
	}
	if _, ok := h.sessionRows()[name]; ok {
		t.Fatal("an ended, stopped session still has a row")
	}
}

// A second start of a running session, from another run or hub writing
// the same log and naming another host, is refused by the log's fold
// alone: the first start's host holds, and only its node is asked.
func TestSecondStartRefusedFromLog(t *testing.T) {
	h := placementHarness(t)
	h.nodes["host-a"].cfg.Accounts["acct-b"] = h.nodes["host-b"].cfg.Accounts["acct-b"]
	h.makeClone("host-a", "github.com/example-org/widgets")
	name, id := h.start("fix-spin", "host-b", "acct-b")
	again, _ := h.start("fix-spin", "host-a", "acct-b")
	if again != name {
		t.Fatalf("names %s and %s", name, again)
	}
	events, _, err := ReadLog(h.cfg.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	if rt := projectedRuntime(h.cfg, events); len(rt) != 1 || rt[name].ID != id || rt[name].Host != "host-b" {
		t.Fatalf("runtime = %+v", rt)
	}
	h.tick("host-a")
	h.tick("host-b")
	if a, b := h.tmuxes["host-a"].count("new-session", ""), h.tmuxes["host-b"].count("new-session", name); a != 0 || b != 1 {
		t.Fatalf("host-a started %d, host-b %d", a, b)
	}
	// A hub restarted from the log agrees.
	h.startHub()
	h.now = h.now.Add(time.Minute)
	h.tick("host-a")
	h.tick("host-b")
	if a, b := h.tmuxes["host-a"].count("new-session", ""), h.tmuxes["host-b"].count("new-session", name); a != 0 || b != 1 {
		t.Fatalf("after a restart host-a started %d, host-b %d", a, b)
	}
}

// An account not logged in on the session's host is refused by its node,
// which starts nothing: agentd uses no other account and no other host.
// The row says why; the hub asks again only after retry_after.
func TestAccountNotOnHostRefused(t *testing.T) {
	h := placementHarness(t)
	name, _ := h.start("fix-spin", "host-b", "acct-c")
	h.nodes["host-b"].cfg.Accounts["acct-c"] = Account{ConfigDir: filepath.Join(t.TempDir(), "absent")}
	h.tick("host-b")
	if n := h.tmuxes["host-b"].count("new-session", ""); n != 0 {
		t.Fatalf("started %d", n)
	}
	r := h.sessionResults()
	if len(r) != 1 || r[0].Outcome != NodeRefused || !strings.Contains(r[0].Error, "not logged in on host-b") {
		t.Fatalf("results = %+v", r)
	}
	if row := h.sessionRows()[name]; row.State != SessionFailed || row.Host != "host-b" || !strings.Contains(row.Error, "not logged in") {
		t.Fatalf("row = %+v", row)
	}
	h.now = h.now.Add(time.Minute)
	h.tick("host-b")
	if n := len(h.sessionResults()); n != 1 {
		t.Fatalf("asked again within retry_after: %d results", n)
	}
	delete(h.nodes["host-b"].cfg.Accounts, "acct-c")
	h.now = h.now.Add(h.cfg.Hub.RetryAfter.Duration)
	h.tick("host-b")
	if r := h.sessionResults(); len(r) != 2 || !strings.Contains(r[1].Error, "acct-c is not an account on host-b") {
		t.Fatalf("results = %+v", r)
	}
	if n := h.tmuxes["host-a"].count("new-session", "") + h.tmuxes["host-b"].count("new-session", ""); n != 0 {
		t.Fatalf("started %d", n)
	}
}

// A host that stops reporting leaves its sessions unknown, never gone or
// idle; nothing starts them elsewhere, and its start keeps its host.
func TestUnreachableHostsSessionsShowUnknown(t *testing.T) {
	h := placementHarness(t)
	h.makeClone("host-a", "github.com/example-org/widgets")
	h.nodes["host-a"].cfg.Accounts["acct-b"] = h.nodes["host-b"].cfg.Accounts["acct-b"]
	running, _ := h.start("fix-spin", "host-b", "acct-b")
	h.tick("host-b")
	h.tick("host-b")
	waiting, _ := h.start("fix-wait", "host-b", "acct-b")
	if r := h.sessionRows(); r[running].State != SessionRunning || r[waiting].State != SessionStarting {
		t.Fatalf("rows = %+v", r)
	}
	for i := 0; i < 3; i++ {
		h.now = h.now.Add(h.cfg.Hub.SilentAfter.Duration)
		h.tick("host-a")
	}
	rows := h.sessionRows()
	for _, n := range []string{running, waiting} {
		if rows[n].State != NodeUnknown || rows[n].Host != "host-b" {
			t.Fatalf("%s = %+v", n, rows[n])
		}
	}
	if n := h.tmuxes["host-a"].count("new-session", ""); n != 0 {
		t.Fatalf("host-a started %d", n)
	}
	events, _, err := ReadLog(h.cfg.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{running, waiting} {
		if p := projectedRuntime(h.cfg, events)[n]; p.Host != "host-b" {
			t.Fatalf("%s moved: %+v", n, p)
		}
	}
	// Back, it is asked for what it missed and nothing else.
	h.tick("host-b")
	if a, b := h.tmuxes["host-a"].count("new-session", ""), h.tmuxes["host-b"].count("new-session", ""); a != 0 || b != 2 {
		t.Fatalf("host-a started %d, host-b %d", a, b)
	}
}

// A session gone from its host shows gone; it is not started again.
func TestRemoteSessionGoneShowsGone(t *testing.T) {
	h := placementHarness(t)
	name, _ := h.start("fix-spin", "host-b", "acct-b")
	h.tick("host-b")
	h.tmuxes["host-b"].Stop(name)
	h.now = h.now.Add(time.Minute)
	h.tick("host-b")
	h.tick("host-b")
	if n := h.tmuxes["host-b"].count("new-session", name); n != 1 {
		t.Fatalf("started %d times", n)
	}
	if r := h.sessionRows()[name]; r.State != SessionGone {
		t.Fatalf("row = %+v", r)
	}
}

// The node refuses what it cannot do safely: a session of a name it
// already runs, a repository it has no clone of, and a start when it has
// no sessions root or launch command.
func TestNodeRefusesUnsafeSessionCommands(t *testing.T) {
	h := placementHarness(t)
	n := h.nodes["host-b"]
	h.gits["host-b"].fail = errors.New("no network") // gadgets stays absent
	h.tick("host-b")
	cmd := NodeCommand{ID: "session-start:x:1", Action: NodeSessionStart, Session: "widgets-x", Repository: "widgets", Path: "github.com/example-org/widgets",
		Branch: "x", Target: "main", Kind: KindWork, Account: "acct-b", Start: "1"}
	h.tmuxes["host-b"].alive["widgets-x"] = true
	if r := n.do(t.Context(), cmd); r.Outcome != NodeRefused || !strings.Contains(r.Error, "already running") {
		t.Fatalf("running name: %+v", r)
	}
	h.tmuxes["host-b"].alive["widgets-x"] = false
	for why, c := range map[string]NodeCommand{
		"not a session name": func() NodeCommand { c := cmd; c.Session = "../x"; return c }(),
		"plain branch":       func() NodeCommand { c := cmd; c.Branch = "-x"; return c }(),
		"does not watch":     func() NodeCommand { c := cmd; c.Repository = "gizmos"; return c }(),
	} {
		if r := n.do(t.Context(), c); r.Outcome != NodeRefused || !strings.Contains(r.Error, why) {
			t.Fatalf("%s: %+v", why, r)
		}
	}
	n.cfg.Launch = nil
	if r := n.do(t.Context(), cmd); r.Outcome != NodeRefused || !strings.Contains(r.Error, "node.launch") {
		t.Fatalf("no launch: %+v", r)
	}
	if n := h.tmuxes["host-b"].count("new-session", ""); n != 0 {
		t.Fatalf("started %d", n)
	}
	// No clone: gadgets is watched on host-b but not cloned there.
	n.cfg.Launch = []string{"claude"}
	c := cmd
	c.Repository, c.Path = "gadgets", "gitlab.com/example-org/tools/gadgets"
	if r := n.do(t.Context(), c); r.Outcome != NodeRefused || !strings.Contains(r.Error, "no clone") {
		t.Fatalf("no clone: %+v", r)
	}
	// A stop of a session that is not running is done.
	if r := n.do(t.Context(), NodeCommand{ID: "session-stop:widgets-x:2", Action: NodeSessionStop, Session: "widgets-x"}); r.Outcome != NodeDone {
		t.Fatalf("stop of none: %+v", r)
	}
}

// agentd run does not supervise a session on another host: it neither
// prepares nor launches it here, and its loop records the start's host
// from the repository's sessions entry.
func TestRunLeavesRemoteSessionToItsNode(t *testing.T) {
	h := sessionsHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Sessions.Host = "host-b"
	w.Policy.Sessions.Accounts = []string{"acct-b"}
	h.cfg.Repositories["widgets"] = w
	h.restart(false)
	name := h.startSession("fix/spin", KindWork, 7)
	if ev := h.sessionEvents(); len(ev) != 1 || ev[0].Host != "host-b" {
		t.Fatalf("session events = %+v", ev)
	}
	h.ticks(3, time.Minute)
	if n := h.count(KindAction, ActProvision, name); n != 0 {
		t.Fatalf("provisioned %d times here", n)
	}
	if n := h.tmux.count("new-session", name); n != 0 {
		t.Fatalf("launched %d times here", n)
	}
	if err := h.endSession(name); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	if n := h.tmux.count("stop", name) + h.count(KindAction, ActEnd, name); n != 0 {
		t.Fatalf("stopped here: %d", n)
	}
	// The name is free for a start once the end is in.
	h.startSession("fix/spin", KindWork, 7)
}

// A host for started sessions is a configured host with the clone, and
// placement by usage, which reads accounts here, does not go with it.
func TestSessionsHostConfig(t *testing.T) {
	cases := map[string]struct {
		host     string
		cloneOn  []string
		accounts []string
		fallback []string
		want     string
	}{
		"ok":         {host: "host-b", cloneOn: []string{"host-b"}, accounts: []string{"acct-b"}},
		"not a host": {host: "host-z", cloneOn: []string{"host-b"}, accounts: []string{"acct-b"}, want: `sessions.host "host-z" is not in hosts`},
		"no clone":   {host: "host-a", cloneOn: []string{"host-b"}, accounts: []string{"acct-b"}, want: "clone_on"},
		"no account": {host: "host-b", cloneOn: []string{"host-b"}, want: "needs policy.sessions.accounts"},
		"fallback":   {host: "host-b", cloneOn: []string{"host-b"}, accounts: []string{"acct-b"}, fallback: []string{"acct-c"}, want: "fallback"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Hosts = map[string]HostConfig{"host-a": {}, "host-b": {}}
			r := Repository{CloneOn: c.cloneOn, Sessions: &SessionsPolicy{Host: c.host}}
			r.Policy.Sessions.Accounts, r.Policy.Sessions.Fallback = c.accounts, c.fallback
			cfg.Repositories = map[string]Repository{"widgets": r}
			var errs []error
			cfg.checkNodeConfig(func(_, p string) string { return p }, &errs)
			got := ""
			for _, e := range errs {
				got += e.Error() + "\n"
			}
			if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
				t.Fatalf("errors = %q, want %q", got, c.want)
			}
		})
	}
}

// A start of a name whose ended session the node has not yet stopped waits
// for the stop, while the host is away and after it is back; then the
// node stops the old session and starts the new one, in that order.
func TestStartWaitsForItsNamesStop(t *testing.T) {
	h := placementHarness(t)
	name, _ := h.start("fix-spin", "host-b", "acct-b")
	h.tick("host-b")
	h.end(name)
	h.start("fix-spin", "host-b", "acct-b")
	if r := h.sessionRows()[name]; r.State != SessionStopping {
		t.Fatalf("row = %+v", r)
	}
	h.tick("host-b")
	var ops []string
	for _, c := range h.tmuxes["host-b"].calls {
		if c.op == "new-session" || c.op == "stop" {
			ops = append(ops, c.op)
		}
	}
	if strings.Join(ops, ",") != "new-session,stop" {
		t.Fatalf("ops = %v", ops)
	}
	h.tick("host-b")
	if n := h.tmuxes["host-b"].count("new-session", name); n != 2 {
		t.Fatalf("started %d times", n)
	}
	if r := h.sessionRows()[name]; r.State != SessionRunning {
		t.Fatalf("row = %+v", r)
	}
}

// A start that failed is asked again after retry_after; one whose tmux
// came up though its start reported an error is done, not refused as
// running already.
func TestFailedStartIsRetried(t *testing.T) {
	h := placementHarness(t)
	name, _ := h.start("fix-spin", "host-b", "acct-b")
	h.tmuxes["host-b"].newErr[name] = errors.New("tmux timed out")
	h.tick("host-b")
	if r := h.sessionRows()[name]; r.State != SessionFailed || !strings.Contains(r.Error, "timed out") {
		t.Fatalf("row = %+v", r)
	}
	delete(h.tmuxes["host-b"].newErr, name)
	h.tmuxes["host-b"].alive[name] = true // it came up after all
	h.now = h.now.Add(h.cfg.Hub.RetryAfter.Duration)
	h.tick("host-b")
	if r := h.sessionResults(); len(r) != 2 || r[1].Outcome != NodeDone {
		t.Fatalf("results = %+v", r)
	}
	if r := h.sessionRows()[name]; r.State != SessionRunning {
		t.Fatalf("row = %+v", r)
	}
}

// A node stops only a session it started: a stop of a running tmux
// session with no mark of this node's in its workdir, such as the owner's
// own, is refused, and the row says why.
func TestNodeStopsOnlyItsOwnSessions(t *testing.T) {
	h := placementHarness(t)
	name, _ := h.start("fix-spin", "host-b", "acct-b")
	h.tick("host-b")
	if err := os.Remove(filepath.Join(h.nodes["host-b"].cfg.SessionsRoot, name, nodeMark)); err != nil {
		t.Fatal(err)
	}
	h.end(name)
	h.tick("host-b")
	if n := h.tmuxes["host-b"].count("stop", ""); n != 0 {
		t.Fatalf("stopped %d", n)
	}
	if r := h.sessionRows()[name]; r.State != SessionStopping || !strings.Contains(r.Error, "not a session this node started") {
		t.Fatalf("row = %+v", r)
	}
	n := h.nodes["host-b"]
	h.tmuxes["host-b"].alive["owners-own"] = true
	if r := n.do(t.Context(), NodeCommand{ID: "session-stop:owners-own:1", Action: NodeSessionStop, Session: "owners-own"}); r.Outcome != NodeRefused {
		t.Fatalf("stop of an unwatched session: %+v", r)
	}
}

// A session whose host cannot say whether it runs is unknown, not gone.
func TestTmuxErrorShowsUnknown(t *testing.T) {
	h := placementHarness(t)
	name, _ := h.start("fix-spin", "host-b", "acct-b")
	h.tick("host-b")
	h.tmuxes["host-b"].hasErr = errors.New("no server")
	h.now = h.now.Add(time.Minute)
	h.tick("host-b")
	if r := h.sessionRows()[name]; r.State != NodeUnknown || !strings.Contains(r.Error, "no server") {
		t.Fatalf("row = %+v", r)
	}
}
