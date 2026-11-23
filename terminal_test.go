package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLauncher finds only the executables in have and records what it
// starts, in env.
type fakeLauncher struct {
	mu      sync.Mutex
	have    []string
	env     []string
	started [][]string
	fail    error
}

func (f *fakeLauncher) LookPath(file string) (string, error) {
	if contains(f.have, file) {
		return "/usr/bin/" + file, nil
	}
	return "", errors.New("not found")
}

func (f *fakeLauncher) Env() []string { return f.env }

func (f *fakeLauncher) Start(argv, env []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, argv)
	return f.fail
}

func (f *fakeLauncher) starts() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.started...)
}

// localTerminal asks for a terminal through the hub's local side, as
// agentd terminal does.
func localTerminal(t *testing.T, h *hubHarness, req terminalRequest) (int, ErrandDone) {
	t.Helper()
	srv := httptest.NewServer(h.hub.localHandler())
	defer srv.Close()
	b, _ := json.Marshal(req)
	resp, err := srv.Client().Post(srv.URL+localTerminalPath, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d ErrandDone
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, d
}

// terminalEvents are the hub log's terminal events.
func terminalEvents(t *testing.T, h *hubHarness) []*TerminalPayload {
	t.Helper()
	events, _, err := ReadLog(h.cfg.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	var out []*TerminalPayload
	for _, e := range events {
		if e.Kind == KindTerminal {
			out = append(out, e.Terminal)
		}
	}
	return out
}

// A terminal goes through the hub to the host's node, which opens the
// first emulator it has, even observing; the hub logs who, host and
// target, never the command.
func TestTerminalOpensOnNode(t *testing.T) {
	h := newHubHarness(t)
	startErrands(t, h, "host-a")
	n := h.nodes["host-a"]
	n.cfg.Observe = true
	fake := &fakeLauncher{have: []string{"gnome-terminal", "xterm"}, env: []string{"DISPLAY=:0", "SHELL=/bin/bash"}}
	n.term = fake
	const secret = "echo hunter2-s3cret"
	code, d := localTerminal(t, h, terminalRequest{Host: "host-a", Target: "buildbox", Command: secret, By: "owner", WaitSec: 30})
	if code != http.StatusOK || !d.OK || d.Emulator != "gnome-terminal" {
		t.Fatalf("terminal: %d %+v", code, d)
	}
	if want := "gnome-terminal -- ssh -t -- buildbox (with a command)"; d.Launched != want {
		t.Errorf("launched %q, want %q", d.Launched, want)
	}
	starts := fake.starts()
	want := []string{"gnome-terminal", "--", "ssh", "-t", "--", "buildbox", secret + "\nexec \"${SHELL:-/bin/sh}\" -l"}
	if len(starts) != 1 || !reflect.DeepEqual(starts[0], want) {
		t.Errorf("started %q", starts)
	}
	b, err := os.ReadFile(h.cfg.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("hunter2")) {
		t.Errorf("the log holds the command:\n%s", b)
	}
	evs := terminalEvents(t, h)
	if len(evs) != 1 || evs[0].By != "owner" || evs[0].Host != "host-a" || evs[0].Target != "buildbox" || !evs[0].HasCommand || !evs[0].OK || evs[0].Emulator != "gnome-terminal" {
		t.Fatalf("terminal events %+v", evs)
	}
	// A plain shell on the host itself.
	if _, d := localTerminal(t, h, terminalRequest{Host: "host-a", Target: "local", WaitSec: 30}); !d.OK || d.Launched != "gnome-terminal -- /bin/bash -l" {
		t.Errorf("local: %+v", d)
	}
	if code, _ := localTerminal(t, h, terminalRequest{Host: "elsewhere", Target: "buildbox"}); code != http.StatusNotFound {
		t.Errorf("an unknown host: %d", code)
	}
	if code, _ := localTerminal(t, h, terminalRequest{Host: "host-a", Target: "-oProxyCommand=sh"}); code != http.StatusBadRequest {
		t.Errorf("an ssh option as target: %d", code)
	}
}

// A node with no graphical session fails, naming import-environment, and
// the hub logs the failure.
func TestTerminalNoDesktop(t *testing.T) {
	h := newHubHarness(t)
	startErrands(t, h, "host-a")
	fake := &fakeLauncher{have: []string{"xterm"}, env: []string{"SHELL=/bin/sh"}}
	h.nodes["host-a"].term = fake
	_, d := localTerminal(t, h, terminalRequest{Host: "host-a", Target: "buildbox", WaitSec: 30})
	if d.OK || !strings.Contains(d.Error, "systemctl --user import-environment DISPLAY WAYLAND_DISPLAY DBUS_SESSION_BUS_ADDRESS") {
		t.Fatalf("no desktop: %+v", d)
	}
	if len(fake.starts()) != 0 {
		t.Errorf("started %v", fake.starts())
	}
	if evs := terminalEvents(t, h); len(evs) != 1 || evs[0].OK || evs[0].Error == "" {
		t.Errorf("events %+v", evs)
	}
}

// A terminal no node takes within the wait is withdrawn, and never opens
// later.
func TestTerminalWithdrawnUntaken(t *testing.T) {
	h := newHubHarness(t)
	_, d := localTerminal(t, h, terminalRequest{Host: "host-a", Target: "buildbox", WaitSec: 1})
	if d.OK || !strings.Contains(d.Error, "did not open it within 1s") {
		t.Fatalf("untaken: %+v", d)
	}
	fake := &fakeLauncher{have: []string{"xterm"}, env: []string{"DISPLAY=:0"}}
	startErrands(t, h, "host-a")
	h.nodes["host-a"].term = fake
	time.Sleep(200 * time.Millisecond)
	if len(fake.starts()) != 0 {
		t.Errorf("a withdrawn terminal opened: %v", fake.starts())
	}
}

// The emulator is node.terminal.emulator when set, else the first of the
// Linux list found; macOS uses Terminal, Windows Windows Terminal or cmd.
func TestTerminalEmulatorChoice(t *testing.T) {
	inner := []string{"ssh", "-t", "--", "buildbox"}
	desk := []string{"WAYLAND_DISPLAY=wayland-0"}
	for i, e := range linuxEmulators {
		var have []string
		for _, later := range linuxEmulators[i:] {
			have = append(have, later[0])
		}
		f := &fakeLauncher{have: have}
		argv, emu, err := terminalArgv("linux", nil, inner, desk, f.LookPath)
		if err != nil || emu != e[0] || !reflect.DeepEqual(argv, append(append([]string{}, e...), inner...)) {
			t.Errorf("with %v: %q %s %v", have, argv, emu, err)
		}
	}
	all := &fakeLauncher{have: []string{"x-terminal-emulator"}}
	if argv, emu, _ := terminalArgv("linux", []string{"foot"}, inner, desk, all.LookPath); emu != "foot" || argv[0] != "foot" || argv[1] != "ssh" {
		t.Errorf("override: %q %s", argv, emu)
	}
	if _, _, err := terminalArgv("linux", nil, inner, desk, (&fakeLauncher{}).LookPath); err == nil || !strings.Contains(err.Error(), "node.terminal.emulator") {
		t.Errorf("none found: %v", err)
	}
	if _, _, err := terminalArgv("linux", []string{"foot"}, inner, nil, all.LookPath); !errors.Is(err, errNoDesktop) {
		t.Errorf("no desktop: %v", err)
	}
	argv, emu, err := terminalArgv("darwin", nil, inner, nil, all.LookPath)
	if err != nil || emu != "Terminal" || argv[0] != "osascript" || !strings.Contains(argv[2], `do script "ssh -t -- buildbox"`) {
		t.Errorf("darwin: %q %s %v", argv, emu, err)
	}
	if argv, emu, _ := terminalArgv("windows", nil, inner, nil, (&fakeLauncher{have: []string{"wt.exe"}}).LookPath); emu != "Windows Terminal" || argv[0] != "wt.exe" {
		t.Errorf("windows terminal: %q %s", argv, emu)
	}
	if argv, emu, _ := terminalArgv("windows", nil, inner, nil, all.LookPath); emu != "cmd" || argv[0] != "cmd.exe" {
		t.Errorf("cmd: %q %s", argv, emu)
	}
}

// A node refuses terminals when its config says so, a target outside its
// list, and a target that is not a safe name.
func TestTerminalRefused(t *testing.T) {
	no := false
	fake := &fakeLauncher{have: []string{"xterm"}, env: []string{"DISPLAY=:0"}}
	for _, c := range []struct {
		tc     *NodeTerminalConfig
		target string
		want   string
	}{
		{&NodeTerminalConfig{Allow: &no}, "buildbox", "node.terminal.allow is false"},
		{&NodeTerminalConfig{Targets: []string{"local"}}, "buildbox", "not in node.terminal.targets"},
		{nil, "-oProxyCommand=sh", "is not a host name"},
		{nil, "a b", "is not a host name"},
		{nil, "", "is not a host name"},
	} {
		n := &Node{cfg: NodeConfig{Name: "laptop", Terminal: c.tc}, term: fake}
		if d := n.openTerminal(Errand{Target: c.target}); d.OK || !strings.Contains(d.Error, c.want) {
			t.Errorf("%+v into %q: %+v", c.tc, c.target, d)
		}
	}
	if len(fake.starts()) != 0 {
		t.Errorf("started %v", fake.starts())
	}
	if err := (&NodeTerminalConfig{Targets: []string{"--x"}}).check(); err == nil {
		t.Error("a bad target in node.terminal.targets passed the check")
	}
	if err := (&NodeTerminalConfig{Emulator: []string{""}}).check(); err == nil {
		t.Error("an empty emulator argument passed the check")
	}
}

func TestTerminalCmdArgs(t *testing.T) {
	var out bytes.Buffer
	for _, args := range [][]string{{"laptop"}, {"laptop", "buildbox", "ls"}, {"laptop", "-x", "--", "ls"}} {
		if err := terminalCmd(args, &out); err == nil {
			t.Errorf("%q passed", args)
		}
	}
}

func TestMergeDesktopEnv(t *testing.T) {
	env := mergeDesktopEnv([]string{"DISPLAY=", "HOME=/h"}, "DISPLAY=:1\nWAYLAND_DISPLAY=wayland-0\nSECRET=x\n")
	if envGet(env, "DISPLAY") != ":1" || envGet(env, "WAYLAND_DISPLAY") != "wayland-0" || envGet(env, "SECRET") != "" {
		t.Errorf("merged %q", env)
	}
}

// open_terminal only proposes: nothing is asked of the hub until the
// owner's yes, which opens it and posts what opened.
func TestOpenTerminalNeedsTheOwnersYes(t *testing.T) {
	h := newOpsH(t)
	h.cfg.Hosts = map[string]HostConfig{"laptop": {}}
	var asked []Errand
	h.env.terminal = func(_, host string, e Errand, _ time.Duration) (ErrandDone, error) {
		asked = append(asked, e)
		return ErrandDone{OK: true, Emulator: "kitty"}, nil
	}
	if out, refused := h.call(ToolOpsTerminal, `{"host": "nowhere", "target": "buildbox", "why": "x"}`); !refused {
		t.Fatalf("an unknown host: %v", out)
	}
	id := h.propose(ToolOpsTerminal, `{"host": "laptop", "target": "buildbox", "command": "gh auth login", "why": "the owner must log in"}`)
	r := h.record(id)
	if !strings.Contains(r.Summary, "laptop") || !strings.Contains(r.Summary, "buildbox") || !strings.Contains(r.Summary, "gh auth login") {
		t.Errorf("summary %q", r.Summary)
	}
	h.ticks(3, time.Minute)
	if len(asked) != 0 || !h.record(id).open() {
		t.Fatalf("opened before the yes: %v", asked)
	}
	if err := h.answer(id, true); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	r = h.record(id)
	if len(asked) != 1 || asked[0].Target != "buildbox" || asked[0].Command != "gh auth login" || r.Ended.IsZero() || r.Failed || !strings.Contains(r.Outcome, "opened kitty on laptop into buildbox") {
		t.Fatalf("after the yes: asked %v, change %+v", asked, r)
	}
}

// A working session is offered open_terminal only with an ops console,
// and its call proposes the change there in its name.
func TestSessionOpenTerminal(t *testing.T) {
	h := newOpsH(t)
	h.cfg.Hosts = map[string]HostConfig{"laptop": {}}
	read := func() ([]Event, error) { return h.events(), nil }
	srv := &MCPServer{cfg: h.cfg, name: "w1", log: NewLogWriter(h.buf), read: read, now: h.clock.Now}
	if !contains(srv.tools(), ToolOpsTerminal) {
		t.Errorf("not offered: %v", srv.tools())
	}
	text, refused := srv.call(ToolOpsTerminal, json.RawMessage(`{"host": "laptop", "target": "local", "why": "set up the printer"}`))
	if refused {
		t.Fatalf("refused: %s", text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	r := h.record(out["proposed"].(string))
	if r.Op != OpsTerminal || r.By != "w1" || !strings.HasPrefix(r.Summary, "w1 asks:") || !r.open() {
		t.Errorf("proposed %+v", r)
	}
	bare := DefaultConfig()
	srv = &MCPServer{cfg: &bare, name: "w1", log: NewLogWriter(h.buf), read: read, now: h.clock.Now}
	if contains(srv.tools(), ToolOpsTerminal) {
		t.Error("offered with no ops console")
	}
	if _, refused := srv.call(ToolOpsTerminal, json.RawMessage(`{"host": "laptop", "target": "local", "why": "x"}`)); !refused {
		t.Error("a call with no ops console was not refused")
	}
}
