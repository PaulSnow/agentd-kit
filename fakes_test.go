package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Sleep(time.Duration)     {}
func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

type call struct {
	op   string
	name string
	args []string
}

// fakeTmux is a tmux whose sessions and screens the test sets. A session
// started while crash is set dies at once, as a crash-looping one would.
type fakeTmux struct {
	alive     map[string]bool
	panes     map[string]string
	crash     map[string]bool
	newErr    map[string]error // NewSession fails with this
	calls     []call
	mutations int
}

func newFakeTmux() *fakeTmux {
	return &fakeTmux{alive: map[string]bool{}, panes: map[string]string{}, crash: map[string]bool{}, newErr: map[string]error{}}
}

func (f *fakeTmux) HasSession(name string) (bool, error) {
	f.calls = append(f.calls, call{op: "has-session", name: name})
	return f.alive[name], nil
}

func (f *fakeTmux) CapturePane(name string, lines int) (string, error) {
	f.calls = append(f.calls, call{op: "capture-pane", name: name})
	if !f.alive[name] {
		return "", fmt.Errorf("no session %s", name)
	}
	return f.panes[name], nil
}

func (f *fakeTmux) NewSession(name, cwd, command string, width, height int) error {
	f.calls = append(f.calls, call{op: "new-session", name: name, args: []string{cwd, command}})
	f.mutations++
	if err := f.newErr[name]; err != nil {
		return err
	}
	if f.crash[name] {
		return nil
	}
	f.alive[name] = true
	f.panes[name] = "fresh session " + name
	return nil
}

func (f *fakeTmux) Type(name, text string) error {
	f.calls = append(f.calls, call{op: "type", name: name, args: []string{text}})
	f.mutations++
	f.panes[name] += "\n> " + text
	return nil
}

func (f *fakeTmux) Keys(name string, keys ...string) error {
	f.calls = append(f.calls, call{op: "keys", name: name, args: keys})
	f.mutations++
	f.panes[name] = strings.ReplaceAll(f.panes[name], "trust this folder", "trusted")
	return nil
}

func (f *fakeTmux) count(op, name string) int {
	n := 0
	for _, c := range f.calls {
		if c.op == op && (name == "" || c.name == name) {
			n++
		}
	}
	return n
}

type fakeGit struct {
	clean    bool
	branch   string
	branches map[string]string // branch per worktree, over branch
	ahead    int
	subject  string
	head     string
	fetches  int
}

func (g *fakeGit) Fetch(string) error         { g.fetches++; return nil }
func (g *fakeGit) Clean(string) (bool, error) { return g.clean, nil }
func (g *fakeGit) Branch(dir string) (string, error) {
	if b, ok := g.branches[dir]; ok {
		return b, nil
	}
	return g.branch, nil
}
func (g *fakeGit) Head(string) (string, error)                 { return g.head, nil }
func (g *fakeGit) Ahead(string, string) (int, error)           { return g.ahead, nil }
func (g *fakeGit) FirstSubject(string, string) (string, error) { return g.subject, nil }

// fakeForge records pushes and opened requests, which stay open. merged are
// requests merged before the test, with the head commit each carried. fail
// makes Open fail without opening; recordThenFail opens the request and then
// fails, as a forge whose reply could not be read would.
type fakeForge struct {
	pushes         []string
	opens          []MergeRequest
	merged         []mergedRequest
	fail           error
	recordThenFail error
}

type mergedRequest struct {
	ref, source, target, head string
}

func (f *fakeForge) Push(dir, branch string) error {
	f.pushes = append(f.pushes, branch)
	return nil
}

func (f *fakeForge) Open(repo Repository, mr MergeRequest, dir string) (MergeResult, error) {
	if f.fail != nil {
		return MergeResult{}, f.fail
	}
	f.opens = append(f.opens, mr)
	if f.recordThenFail != nil {
		return MergeResult{}, f.recordThenFail
	}
	return f.result(len(f.opens)), nil
}

func (f *fakeForge) result(n int) MergeResult {
	return MergeResult{Ref: fmt.Sprintf("!%d", n), URL: "https://forge.example/mr/" + fmt.Sprint(n)}
}

func (f *fakeForge) Find(repo Repository, source, target, head string) (MergeResult, bool, error) {
	for i, o := range f.opens {
		if o.Source == source && o.Target == target {
			return f.result(i + 1), true, nil
		}
	}
	for _, m := range f.merged {
		if m.source == source && m.target == target && m.head == head {
			return MergeResult{Ref: m.ref}, true, nil
		}
	}
	return MergeResult{}, false, nil
}

type fakeRunner struct {
	calls    [][]string
	attempts int
	failing  int // fail this many calls first
}

func (r *fakeRunner) Run(argv []string) error {
	r.attempts++
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
		"widgets":  {Forge: "gitlab", Project: "example-org/widgets", Allow: []string{"push", "open"}, Console: "widgets"},
		"readonly": {Forge: "gitlab", Project: "example-org/readonly", Allow: nil},
	}
	cfg.Sessions = map[string]Session{}
	for _, n := range names {
		cfg.Sessions[n] = Session{Workdir: filepath.Join(dir, n), Cwd: filepath.Join(dir, n, "wt"), Repository: "widgets"}
		if err := os.MkdirAll(filepath.Join(dir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.finish(); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, dir: dir, cfg: &cfg, clock: &fakeClock{now: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)},
		tmux: newFakeTmux(), git: &fakeGit{clean: true, branch: "feature", ahead: 2, subject: "Add the feature", head: "c0ffee"},
		forge: &fakeForge{}, runner: &fakeRunner{}, buf: &bytes.Buffer{}}
	h.sup = NewSupervisor(h.cfg, NewLogWriter(h.buf), h.clock, h.tmux, h.git, h.forge, h.runner, shadow)
	return h
}

// restart builds a new supervisor over the same log, as a restarted agentd would.
func (h *harness) restart(shadow bool) {
	events, bad, err := ReadEvents(bytes.NewReader(h.buf.Bytes()))
	if err != nil || bad != 0 {
		h.t.Fatalf("log unreadable: %v, %d bad", err, bad)
	}
	h.sup = NewSupervisor(h.cfg, NewLogWriter(h.buf), h.clock, h.tmux, h.git, h.forge, h.runner, shadow)
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

// ticks runs n passes, every d apart.
func (h *harness) ticks(n int, d time.Duration) {
	for i := 0; i < n; i++ {
		h.sup.Pass()
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
		if e.Kind == kind && (action == "" || detailString(e, "action") == action) && (session == "" || e.Session == session) {
			n++
		}
	}
	return n
}

func (h *harness) state(name string) State {
	return h.sup.Tracks()[name].State
}
