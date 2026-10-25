package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a clock the test moves. After channels fire when advance
// passes their time.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []clockWaiter
}

type clockWaiter struct {
	at time.Time
	ch chan time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(time.Duration) {}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, clockWaiter{c.now.Add(d), ch})
	return ch
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
			continue
		}
		kept = append(kept, w)
	}
	c.waiters = kept
}

// waiting is how many After channels have not fired yet.
func (c *fakeClock) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

type call struct {
	op   string
	name string
	args []string
}

// fakeTmux is a tmux whose sessions and screens the test sets. A session
// started while crash is set dies at once, as a crash-looping one would.
// panes is the visible screen and history the scrollback above it. A
// sticky session keeps its trust prompt on screen after keys are sent, as a
// prompt that did not take them would. commands is what each pane runs,
// claude unless set. A crashing session with remain set keeps its dead
// pane, as tmux's remain-on-exit does, showing deadScreen with exit status
// exit; one without dies outright.
type fakeTmux struct {
	mu         sync.Mutex
	alive      map[string]bool
	panes      map[string]string
	history    map[string]string
	crash      map[string]bool
	remain     map[string]bool
	dead       map[string]bool
	exit       map[string]int
	deadScreen map[string]func() string
	sticky     map[string]bool
	chosen     map[string]int      // the trust-menu option Enter chose
	enterOn    map[string][]string // the selected option's text at each Enter
	onPane     func(name string)   // runs when Pane is asked, to change the screen under a caller
	commands   map[string]string
	paths      map[string]string // #{pane_current_path}
	newErr     map[string]error  // NewSession fails with this
	stopErr    map[string]error  // Stop fails with this
	replaceErr error             // Replace fails with this
	calls      []call
	mutations  int
}

func newFakeTmux() *fakeTmux {
	return &fakeTmux{alive: map[string]bool{}, panes: map[string]string{}, history: map[string]string{}, crash: map[string]bool{},
		remain: map[string]bool{}, dead: map[string]bool{}, exit: map[string]int{}, deadScreen: map[string]func() string{},
		sticky: map[string]bool{}, chosen: map[string]int{}, enterOn: map[string][]string{}, commands: map[string]string{}, paths: map[string]string{}, newErr: map[string]error{}}
}

func (f *fakeTmux) HasSession(name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{op: "has-session", name: name})
	return f.alive[name], nil
}

func (f *fakeTmux) CapturePane(name string, lines int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{op: "capture-pane", name: name})
	if !f.alive[name] {
		return "", fmt.Errorf("no session %s", name)
	}
	if lines > 0 && f.history[name] != "" {
		return f.history[name] + "\n" + f.panes[name], nil
	}
	return f.panes[name], nil
}

func (f *fakeTmux) Pane(name string) (PaneInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{op: "pane", name: name})
	if f.onPane != nil {
		f.onPane(name)
	}
	if !f.alive[name] {
		return PaneInfo{}, fmt.Errorf("no session %s", name)
	}
	cmd := f.commands[name]
	if cmd == "" {
		cmd = "claude"
	}
	if f.dead[name] {
		return PaneInfo{Command: cmd, Dead: true, Status: f.exit[name], HasStatus: true, Path: f.paths[name]}, nil
	}
	return PaneInfo{Command: cmd, Path: f.paths[name]}, nil
}

// start runs a session's command: it comes up, or it crashes, leaving a
// dead pane when remain is set.
func (f *fakeTmux) start(name string) {
	f.dead[name] = false
	switch {
	case f.crash[name] && f.remain[name]:
		f.alive[name], f.dead[name] = true, true
		f.panes[name] = "error: boom"
		if s := f.deadScreen[name]; s != nil {
			f.panes[name] = s()
		}
		f.panes[name] += "\nPane is dead (status 1, " + fmt.Sprint(len(f.calls)) + ")"
	case f.crash[name]:
		f.alive[name] = false
	default:
		f.alive[name] = true
		f.panes[name] = "fresh session " + name
	}
}

func (f *fakeTmux) Respawn(name, cwd, command string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{op: "respawn", name: name, args: []string{cwd, command}})
	f.mutations++
	if !f.dead[name] {
		return fmt.Errorf("pane %s still active", name)
	}
	f.start(name)
	return nil
}

func (f *fakeTmux) NewSession(name, cwd, command string, width, height int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{op: "new-session", name: name, args: []string{cwd, command}})
	f.mutations++
	if err := f.newErr[name]; err != nil {
		return err
	}
	if f.alive[name] {
		return fmt.Errorf("duplicate session: %s", name)
	}
	f.start(name)
	return nil
}

// Replace kills the pane's command and starts a fresh one in it.
func (f *fakeTmux) Replace(name, cwd, command string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{op: "replace", name: name, args: []string{cwd, command}})
	f.mutations++
	if f.replaceErr != nil {
		return f.replaceErr
	}
	if !f.alive[name] {
		return fmt.Errorf("no session %s", name)
	}
	f.start(name)
	return nil
}

func (f *fakeTmux) Stop(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{op: "stop", name: name})
	f.mutations++
	if err := f.stopErr[name]; err != nil {
		return err
	}
	if !f.alive[name] {
		return fmt.Errorf("no session %s", name)
	}
	f.alive[name], f.dead[name] = false, false
	return nil
}

func (f *fakeTmux) Type(name, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{op: "type", name: name, args: []string{text}})
	f.mutations++
	f.panes[name] += "\n> " + text
	return nil
}

// Keys works the trust prompt's menu on the screen as Claude Code does:
// the option marked ❯ is selected (option 1 when none is), Down and Up
// move the mark and redraw the screen, Enter chooses. Choosing an option
// that says No ends the session; any other trusts the folder. A sticky
// prompt ignores the keys.
func (f *fakeTmux) Keys(name string, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{op: "keys", name: name, args: keys})
	f.mutations++
	lines := strings.Split(f.panes[name], "\n")
	var opts []int // the lines of the options, in order
	sel := 0
	for i, l := range lines {
		t := strings.Trim(l, " │")
		if m := regexp.MustCompile(`^([❯>]\s*)?(\d)\.\s+(.*)$`).FindStringSubmatch(t); m != nil {
			if m[2] == "1" { // a later menu is the live one
				opts, sel = nil, 0
			}
			if m[1] != "" {
				sel = len(opts)
			}
			opts = append(opts, i)
		}
	}
	text := func(i int) string {
		return strings.TrimSpace(strings.TrimLeft(strings.Trim(lines[opts[i]], " │"), "❯> "))
	}
	if f.sticky[name] {
		for _, k := range keys {
			if k == "Enter" && len(opts) > sel {
				f.enterOn[name] = append(f.enterOn[name], text(sel))
			}
		}
		return nil
	}
	draw := func() {
		for i, at := range opts {
			mark := "  "
			if i == sel {
				mark = "❯ "
			}
			lines[at] = mark + text(i)
		}
		f.panes[name] = strings.Join(lines, "\n")
	}
	for _, k := range keys {
		switch k {
		case "Down":
			if sel < len(opts)-1 {
				sel++
				draw()
			}
		case "Up":
			if sel > 0 {
				sel--
				draw()
			}
		case "Enter":
			f.chosen[name] = sel + 1
			if len(opts) > sel {
				f.enterOn[name] = append(f.enterOn[name], text(sel))
			}
			if len(opts) > sel && strings.HasPrefix(text(sel)[3:], "No") {
				f.alive[name] = false
				return nil
			}
			f.panes[name] = "trusted; Claude Code is starting"
		}
	}
	return nil
}

func (f *fakeTmux) count(op, name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.op == op && (name == "" || c.name == name) {
			n++
		}
	}
	return n
}

type fakeGit struct {
	mu           sync.Mutex
	clean        bool
	branch       string
	branches     map[string]string // branch per worktree, over branch
	ahead        int
	aheads       map[string]int // ahead per worktree, over ahead
	subject      string
	messages     string // the commit messages ahead of the target
	head         string
	inTarget     map[string]bool // commits the target contains
	remote       string
	remotes      map[string]string // origin URL per worktree, over remote
	pushRemote   string            // origin's separate push URL, if any
	rewrites     []string          // url.*.insteadOf and pushInsteadOf prefixes in git config
	config       string            // git config as listed; a change is a rewritten config
	fetches      int
	merge        string // the newest merge at HEAD, and its subject
	mergeSubject string
	mergeAt      map[string]time.Time // each merge's commit time, zero by default
	gateChanged  []string             // the gate files the session changed
	gatePaths    []string             // the gate files last asked about
	gateChecks   int                  // GateFiles calls
	onGateFiles  func(n int)          // runs, locked, in the nth GateFiles call
	gateUnpinned bool                 // the target has none of the gate files
	gateErr      error                // what GateFiles fails with
	worktrees    []string             // the worktrees added: clone, dir, branch, target
	worktreeErr  error                // what AddWorktree fails with
}

// AddWorktree makes dir and records the worktree it would have added.
func (g *fakeGit) AddWorktree(clone, dir, branch, target string, detach bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.worktreeErr != nil {
		return g.worktreeErr
	}
	w := clone + " " + dir + " " + branch + " " + target
	if detach {
		w += " detached"
	}
	g.worktrees = append(g.worktrees, w)
	return os.MkdirAll(dir, 0o755)
}

func (g *fakeGit) Fetch(string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fetches++
	return nil
}
func (g *fakeGit) Clean(string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.clean, nil
}
func (g *fakeGit) Branch(dir string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if b, ok := g.branches[dir]; ok {
		return b, nil
	}
	return g.branch, nil
}
func (g *fakeGit) Head(string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.head, nil
}
func (g *fakeGit) RemoteURLs(dir string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	u := g.remote
	if r, ok := g.remotes[dir]; ok {
		u = r
	}
	if g.pushRemote != "" {
		return []string{u, g.pushRemote}, nil
	}
	return []string{u}, nil
}
func (g *fakeGit) Rewrites(string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.rewrites, nil
}
func (g *fakeGit) ConfigHash(string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return HashPane(g.config), nil
}
func (g *fakeGit) Ahead(dir, target string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n, ok := g.aheads[dir]; ok {
		return n, nil
	}
	return g.ahead, nil
}
func (g *fakeGit) GateFiles(dir, url, target, head string, paths []string) ([]string, int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gatePaths = paths
	g.gateChecks++
	if g.onGateFiles != nil {
		g.onGateFiles(g.gateChecks)
	}
	if g.gateErr != nil {
		return nil, 0, g.gateErr
	}
	if g.gateUnpinned {
		return nil, 0, nil
	}
	return g.gateChanged, len(paths), nil
}

func (g *fakeGit) Contains(dir, sha, target string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inTarget[sha], nil
}
func (g *fakeGit) FirstSubject(string, string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.subject, nil
}
func (g *fakeGit) LastMerge(string) (string, string, time.Time, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.merge, g.mergeSubject, g.mergeAt[g.merge], nil
}

func (g *fakeGit) Messages(string, string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.messages, nil
}

// fakeForge records pushes and opened requests, which stay open. merged are
// requests merged before the test, with the head commit each carried. fail
// makes Open fail without opening; recordThenFail opens the request and then
// fails, as a forge whose reply could not be read would.
type fakeForge struct {
	mu             sync.Mutex
	pushes         []string
	pushedSHAs     []string
	pushURLs       []string
	opens          []MergeRequest
	merged         []mergedRequest
	fail           error
	recordThenFail error
	pushErr        error // what a push returns, after recording it
	merges         []mergedRequest
	mergeErr       error // what a merge returns, after recording it
}

type mergedRequest struct {
	ref, source, target, head string
}

func (f *fakeForge) Push(dir, url, sha, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushes = append(f.pushes, branch)
	f.pushURLs = append(f.pushURLs, url)
	f.pushedSHAs = append(f.pushedSHAs, sha)
	return f.pushErr
}

func (f *fakeForge) Open(repo Repository, mr MergeRequest, dir string) (MergeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return MergeResult{}, f.fail
	}
	f.opens = append(f.opens, mr)
	if f.recordThenFail != nil {
		return MergeResult{}, f.recordThenFail
	}
	return f.result(len(f.opens)), nil
}

func (f *fakeForge) Merge(repo Repository, ref, head, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merges = append(f.merges, mergedRequest{ref: ref, target: target, head: head})
	return f.mergeErr
}

func (f *fakeForge) result(n int) MergeResult {
	return MergeResult{Ref: fmt.Sprintf("!%d", n), URL: "https://forge.example/mr/" + fmt.Sprint(n)}
}

func (f *fakeForge) Find(repo Repository, source, target string) ([]MergeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found []MergeResult
	for _, m := range f.merged {
		if m.source == source && m.target == target {
			found = append(found, MergeResult{Ref: m.ref, Head: m.head, Merged: true})
		}
	}
	for i, o := range f.opens {
		if o.Source == source && o.Target == target {
			found = append(found, f.result(i+1))
		}
	}
	return found, nil
}

// fakeRunner runs escalations and gates. A gate exits gateExit, or fails to
// run with gateErr; onGate runs during it, to change the tree under it.
type fakeRunner struct {
	mu       sync.Mutex
	calls    [][]string
	attempts int
	failing  int // fail this many calls first
	gates    []gateRun
	gateExit int
	gateErr  error
	onGate   func()
	onRun    func()   // runs during an escalation command
	gateEnv  []string // the environment the last gate ran with
}

type gateRun struct {
	dir     string
	argv    []string
	timeout time.Duration
}

func (r *fakeRunner) Gate(dir string, argv, env []string, timeout time.Duration) (int, string, error) {
	r.mu.Lock()
	r.gates = append(r.gates, gateRun{dir, argv, timeout})
	r.gateEnv = env
	onGate := r.onGate
	r.mu.Unlock()
	if onGate != nil { // unlocked: it may wait, as a slow gate does
		onGate()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gateExit, "gate output", r.gateErr
}

// attemptCount is how many escalation commands have started.
func (r *fakeRunner) attemptCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts
}

// setPane changes a session's screen while the supervisor may be reading it.
func (f *fakeTmux) setPane(name, pane string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panes[name] = pane
}

// gateCount is how many gates have started.
func (r *fakeRunner) gateCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.gates)
}

func (r *fakeRunner) Run(argv []string) error {
	r.mu.Lock()
	r.attempts++
	onRun := r.onRun
	r.mu.Unlock()
	if onRun != nil { // unlocked: it may wait, as a slow console does
		onRun()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failing > 0 {
		r.failing--
		return errors.New("console unreachable")
	}
	r.calls = append(r.calls, argv)
	return nil
}

// harness wires a supervisor to fakes and a temp directory of status files.
type harness struct {
	t      *testing.T
	dir    string
	cfg    *Config
	clock  *fakeClock
	tmux   *fakeTmux
	git    *fakeGit
	forge  *fakeForge
	runner *fakeRunner
	buf    *bytes.Buffer
	sup    *Supervisor
}

func newHarness(t *testing.T, shadow bool, names ...string) *harness {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.TranscriptDir = ""
	cfg.Policies.Nudge.Settle = Duration{6 * time.Minute}
	cfg.Policies.Escalate.Command = []string{"route", "{console}", "--kind", "decision", "{message}"}
	cfg.Policies.Escalate.DefaultConsole = "infra"
	cfg.Repositories = map[string]Repository{
		"widgets":  {Forge: "gitlab", Host: "gitlab.example.com", Project: "example-org/widgets", Allow: []string{"push", "open"}, Console: "widgets", Gate: []string{"./bin/check"}},
		"readonly": {Forge: "gitlab", Host: "gitlab.example.com", Project: "example-org/readonly", Allow: nil},
	}
	cfg.Sessions = map[string]Session{}
	tmux := newFakeTmux()
	for _, n := range names {
		cfg.Sessions[n] = Session{Workdir: filepath.Join(dir, n), Cwd: filepath.Join(dir, n, "wt"), Repository: "widgets"}
		tmux.paths[n] = filepath.Join(dir, n, "wt")
		if err := os.MkdirAll(filepath.Join(dir, n, "wt"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.finish(dir); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, dir: dir, cfg: &cfg, clock: &fakeClock{now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)},
		tmux: tmux, git: &fakeGit{clean: true, branch: "feature", ahead: 2, subject: "Add the feature", head: "c0ffee",
			remote: "git@gitlab.example.com:example-org/widgets.git"},
		forge: &fakeForge{}, runner: &fakeRunner{}, buf: &bytes.Buffer{}}
	h.sup = NewSupervisor(h.cfg, NewLogWriter(h.buf), h.clock, h.tmux, h.git, h.forge, h.runner, shadow)
	h.sup.home = dir
	return h
}

// restart builds a new supervisor over the same log, as a restarted agentd would.
func (h *harness) restart(shadow bool) {
	events, bad, err := ReadEvents(bytes.NewReader(h.buf.Bytes()))
	if err != nil || bad != 0 {
		h.t.Fatalf("log unreadable: %v, %d bad", err, bad)
	}
	h.sup = NewSupervisor(h.cfg, NewLogWriter(h.buf), h.clock, h.tmux, h.git, h.forge, h.runner, shadow)
	h.sup.home = h.dir
	h.sup.Replay(events)
}

func (h *harness) status(name, text string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, name, "STATUS.md"), []byte(text), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) alive(name, pane string) {
	h.tmux.alive[name] = true
	h.tmux.panes[name] = pane
}

func (h *harness) deliver(name string, d Deliver) {
	s := h.cfg.Sessions[name]
	s.Deliver = append(s.Deliver, d)
	h.cfg.Sessions[name] = s
}

// ticks runs n passes, every d apart, each with the jobs it started run to
// their end, so a test sees a pass whole.
func (h *harness) ticks(n int, d time.Duration) {
	for i := 0; i < n; i++ {
		h.sup.Pass()
		h.sup.Wait()
		h.clock.advance(d)
	}
}

func (h *harness) events() []Event {
	events, bad, err := ReadEvents(bytes.NewReader(h.buf.Bytes()))
	if err != nil || bad != 0 {
		h.t.Fatalf("log unreadable: %v, %d bad", err, bad)
	}
	return events
}

// count counts events of a kind, optionally with an action and session.
func (h *harness) count(kind, action, session string) int {
	n := 0
	for _, e := range h.events() {
		if e.Kind == kind && (action == "" || actionOf(e) == action) && (session == "" || e.Session == session) {
			n++
		}
	}
	return n
}

func (h *harness) state(name string) State {
	return h.sup.Tracks()[name].State
}

// actionOf is the action an intent, action, would or failed action's error
// event names, or a warning about a cut-short intent, or "".
func actionOf(e Event) string {
	switch {
	case e.Act != nil:
		return e.Act.Action
	case e.Error != nil && e.Error.Act != nil:
		return e.Error.Act.Action
	case e.Warning != nil && e.Warning.Intent != nil:
		return e.Warning.Intent.Action
	}
	return ""
}

// skipReason is a skip event's reason, or "".
func skipReason(e Event) string {
	if e.Skip != nil {
		return e.Skip.Reason
	}
	return ""
}
