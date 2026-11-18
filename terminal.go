package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Terminals (Wcace1380): a window on one machine's desktop, logged into
// another, for the owner. agentd terminal HOST TARGET asks the hub, over
// its socket as agentd send and agentd prompt do, and the hub hands HOST's
// node a terminal errand on the connection the node already holds open: the
// requester never reaches HOST itself. The node opens its desktop's
// terminal emulator running ssh TARGET, or a login shell for local, and
// answers what it launched or why it could not. A command given after --
// runs in that shell first, and the window stays open after it, for the
// owner to go on by hand. The command rides the errand alone: the log
// records who asked, the host, the target and the outcome, never the
// command.

const (
	ErrandTerminal = "terminal"
	// KindTerminal is a terminal asked for, as the hub logged it.
	KindTerminal = "terminal"
	// TerminalLocal is the target of a plain shell on the host itself.
	TerminalLocal = "local"
	// DefaultTerminalWait is how long agentd terminal waits for the node.
	DefaultTerminalWait = 3 * time.Minute
	// terminalWaitMax bounds any wait for one.
	terminalWaitMax = 30 * time.Minute
	// terminalCommandMax bounds a terminal's command.
	terminalCommandMax = 4096
	// terminalStartWait is how long the node watches an emulator it
	// started, so one that fails at once is reported as failed.
	terminalStartWait = 3 * time.Second
	localTerminalPath = "/local/terminal"
)

// terminalTarget is a host name or ssh alias: never an ssh option.
var terminalTarget = regexp.MustCompile(`^[A-Za-z0-9_@][A-Za-z0-9._@-]{0,252}$`)

// errNoDesktop is a node with no graphical session to show a window in.
var errNoDesktop = errors.New("no graphical session: neither DISPLAY nor WAYLAND_DISPLAY is set for the node, nor in its systemd user manager. " +
	"In the desktop session, run `systemctl --user import-environment DISPLAY WAYLAND_DISPLAY DBUS_SESSION_BUS_ADDRESS XAUTHORITY` (deploy/README-node.md)")

// NodeTerminalConfig is whether a node opens terminals, node.terminal:
// Allow (default true) says it does at all; Emulator is the argv the
// window's command follows, in place of the first emulator found; Targets,
// when set, are the only targets it opens a window into ("local" for its
// own shell).
type NodeTerminalConfig struct {
	Allow    *bool    `json:"allow,omitempty"`
	Emulator []string `json:"emulator,omitempty"`
	Targets  []string `json:"targets,omitempty"`
}

func (c *NodeTerminalConfig) allowed() bool { return c == nil || c.Allow == nil || *c.Allow }

func (c *NodeTerminalConfig) emulator() []string {
	if c == nil {
		return nil
	}
	return c.Emulator
}

// check validates node.terminal.
func (c *NodeTerminalConfig) check() error {
	if c == nil {
		return nil
	}
	for _, a := range c.Emulator {
		if a == "" {
			return errors.New("node.terminal.emulator: an argument is empty")
		}
	}
	for _, t := range c.Targets {
		if t != TerminalLocal && !terminalTarget.MatchString(t) {
			return fmt.Errorf("node.terminal.targets: %q is not a host name, an ssh alias or local", t)
		}
	}
	return nil
}

// checkTerminal refuses a target that is not a host name, an ssh alias or
// local, and a command that is too long or holds a NUL.
func checkTerminal(target, command string) error {
	if target != TerminalLocal && !terminalTarget.MatchString(target) {
		return fmt.Errorf("target %q is not a host name, an ssh alias or local", cutUTF8(target, 60))
	}
	if len(command) > terminalCommandMax || strings.ContainsRune(command, 0) {
		return fmt.Errorf("the command is over %d bytes or holds a NUL", terminalCommandMax)
	}
	return nil
}

// TerminalPayload is a terminal asked for and how it ended: who asked, the
// host whose desktop shows it, the target, whether a command was given
// (never the command), and the emulator and what it launched.
type TerminalPayload struct {
	ID         string `json:"id"`
	Host       string `json:"host"`
	Target     string `json:"target"`
	By         string `json:"by,omitempty"`
	HasCommand bool   `json:"has_command,omitempty"`
	OK         bool   `json:"ok"`
	Emulator   string `json:"emulator,omitempty"`
	Launched   string `json:"launched,omitempty"`
	Error      string `json:"error,omitempty"`
}

func (p *TerminalPayload) check() error {
	if p.ID == "" || p.Host == "" || p.Target == "" {
		return errors.New("a terminal event names its errand, host and target")
	}
	return nil
}

// terminalText says how a terminal ended, in a line.
func terminalText(host string, e Errand, d ErrandDone) string {
	if !d.OK {
		return "failed: " + d.Error
	}
	return fmt.Sprintf("opened %s on %s into %s", d.Emulator, host, e.Target)
}

// addTerminal queues a terminal for host. Like a prompt it is held in
// memory alone, the command with it: whoever asked waits for the answer.
func (q *errandQueue) addTerminal(host string, e Errand) *pendingErrand {
	e.ID, e.Kind = newErrandID(q.now()), ErrandTerminal
	p := &pendingErrand{Errand: e, host: host, done: make(chan ErrandDone, 1)}
	q.add(p)
	return p
}

// logTerminal records a terminal's outcome in the hub's log, without its
// command.
func (h *Hub) logTerminal(host string, e Errand, d ErrandDone) {
	p := &TerminalPayload{ID: e.ID, Host: host, Target: e.Target, By: e.By, HasCommand: e.Command != "",
		OK: d.OK, Emulator: d.Emulator, Launched: d.Launched, Error: cutUTF8(d.Error, 2000)}
	if err := h.log.Append(Event{V: EventVersion, Time: h.now().UTC(), Kind: KindTerminal, Mode: ModeShared, Terminal: p}); err != nil {
		fmt.Fprintln(os.Stderr, "agentd hub: logging terminal", e.ID+":", err)
	}
}

// terminalRequest is agentd terminal's request on the hub's socket.
type terminalRequest struct {
	Host    string `json:"host"`
	Target  string `json:"target"`
	Command string `json:"command,omitempty"`
	By      string `json:"by,omitempty"`
	WaitSec int64  `json:"wait_sec,omitempty"`
}

// serveLocalTerminal hands a terminal to the host's node and answers how
// it ended. One no node took within the wait is withdrawn, so it never
// opens later, unseen.
func (h *Hub) serveLocalTerminal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req terminalRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, nodeBodyMax)).Decode(&req); err != nil {
		http.Error(w, "a terminal names its host and target: "+err.Error(), http.StatusBadRequest)
		return
	}
	if _, ok := h.cfg.Hosts[req.Host]; !ok {
		http.Error(w, fmt.Sprintf("%q is not one of the hub's hosts", req.Host), http.StatusNotFound)
		return
	}
	if err := checkTerminal(req.Target, req.Command); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	wait := time.Duration(req.WaitSec) * time.Second
	if wait <= 0 || wait > terminalWaitMax {
		wait = DefaultTerminalWait
	}
	p := h.errands.addTerminal(req.Host, Errand{Target: req.Target, Command: req.Command, By: req.By})
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var d ErrandDone
	select {
	case d = <-p.done:
	case <-timer.C:
		d = h.withdrawTerminal(p, fmt.Sprintf("%s's node did not open it within %s; nothing will open. Is the node running, with node.errands on?", req.Host, wait))
	case <-r.Context().Done():
		h.withdrawTerminal(p, "whoever asked stopped waiting; nothing will open")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(d)
}

// withdrawTerminal withdraws p unless its node began it, and says how it
// ended: why, when withdrawn, which is logged; else that it may still
// open, or how it did, when its answer came meanwhile.
func (h *Hub) withdrawTerminal(p *pendingErrand, why string) ErrandDone {
	h.errands.cancel(p)
	select {
	case d := <-p.done:
		if d.Error == "cancelled" {
			d.Error = why
			h.logTerminal(p.host, p.Errand, d)
			fmt.Fprintf(os.Stderr, "agentd hub: terminal %s on %s into %s for %s: %s\n", p.ID, p.host, p.Target, p.By, why)
		}
		return d
	default:
		return ErrandDone{Error: fmt.Sprintf("%s's node began opening it and has not answered; it may still open", p.host)}
	}
}

// requestTerminal asks the hub on socket for a terminal on host, waiting
// up to wait for it.
func requestTerminal(socket, host string, e Errand, wait time.Duration) (ErrandDone, error) {
	body, _ := json.Marshal(terminalRequest{Host: host, Target: e.Target, Command: e.Command, By: e.By, WaitSec: int64(wait / time.Second)})
	client := localClient(socket)
	client.Timeout = wait + time.Minute
	resp, err := client.Post("http://hub"+localTerminalPath, "application/json", bytes.NewReader(body))
	b, err := localAnswer(resp, err, socket)
	if err != nil {
		return ErrandDone{}, err
	}
	var d ErrandDone
	if err := json.Unmarshal(b, &d); err != nil {
		return d, fmt.Errorf("hub: answer is not JSON: %w", err)
	}
	return d, nil
}

// terminalCmd opens a terminal on a host's desktop into a target, through
// the hub, and says what was launched.
func terminalCmd(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("terminal", flag.ContinueOnError)
	wait := fs.Duration("wait", DefaultTerminalWait, "how long to wait for the host's node to open it")
	from := fs.String("from", "", "who asks, as the log records it (default: $USER)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: agentd terminal [--config PATH] [-wait D] [-from NAME] HOST TARGET [-- COMMAND...]\n"+
			"  HOST is the host whose desktop shows the window; TARGET a host name or ssh alias it logs into, or local for a shell on HOST.\n"+
			"  COMMAND runs in that shell first; the window stays open after it.")
		fs.PrintDefaults()
	}
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		fs.Usage()
		return errors.New("terminal names a host and a target")
	}
	host, target, rest := rest[0], rest[1], rest[2:]
	command := ""
	if len(rest) > 0 {
		if rest[0] != "--" {
			return fmt.Errorf("%q follows the target: flags go before the host, and a command after --", rest[0])
		}
		command = strings.Join(rest[1:], " ")
	}
	if err := checkTerminal(target, command); err != nil {
		return err
	}
	if *wait < time.Second || *wait > terminalWaitMax {
		return fmt.Errorf("-wait must be between a second and %s", terminalWaitMax)
	}
	by := *from
	if by == "" {
		by = os.Getenv("USER")
	}
	d, err := requestTerminal(hubSocket(cfg), host, Errand{Target: target, Command: command, By: by}, *wait)
	if err != nil {
		return err
	}
	if !d.OK {
		return fmt.Errorf("no terminal on %s: %s", host, d.Error)
	}
	fmt.Fprintf(w, "opened %s on %s into %s\n", d.Emulator, host, target)
	fmt.Fprintf(w, "launched: %s\n", d.Launched)
	return nil
}

// TerminalLauncher is how a node finds and starts a terminal emulator.
type TerminalLauncher interface {
	// LookPath finds an executable as exec.LookPath does.
	LookPath(file string) (string, error)
	// Env is the environment a window starts in: the node's, with the
	// desktop session's variables its service manager holds.
	Env() []string
	// Start starts argv detached, and fails when it exits at once with
	// an error.
	Start(argv, env []string) error
}

// execLauncher launches for real.
type execLauncher struct{}

func (execLauncher) LookPath(file string) (string, error) { return exec.LookPath(file) }

// desktopVars are the variables a window needs from the desktop session.
var desktopVars = []string{"DISPLAY", "WAYLAND_DISPLAY", "DBUS_SESSION_BUS_ADDRESS", "XAUTHORITY", "XDG_RUNTIME_DIR", "XDG_SESSION_TYPE", "XDG_CURRENT_DESKTOP"}

// Env is the node's environment, with each desktop variable it lacks taken
// from the systemd user manager, where import-environment put it: so a
// node started before the desktop session was needs no restart.
func (execLauncher) Env() []string {
	env := os.Environ()
	if runtime.GOOS != "linux" {
		return env
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "show-environment").Output()
	if err != nil {
		return env
	}
	return mergeDesktopEnv(env, string(out))
}

// mergeDesktopEnv adds to env each desktop variable it lacks that
// manager, systemctl show-environment's lines, has.
func mergeDesktopEnv(env []string, manager string) []string {
	have := map[string]bool{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		have[k] = v != ""
	}
	for _, line := range strings.Split(manager, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok && v != "" && !have[k] && contains(desktopVars, k) {
			env = append(env, k+"="+v)
			have[k] = true
		}
	}
	return env
}

func (execLauncher) Start(argv, env []string) error { return startDetached(argv, env) }

// startDetached starts argv in a session of its own, in the home
// directory, and waits up to terminalStartWait: an emulator that exits
// with an error in that time failed, with what it printed; one still
// running, or done with 0 as a client that asked a server for the window
// is, opened it.
func startDetached(argv, env []string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	out := &tailBuffer{max: 2000}
	cmd.Stdout, cmd.Stderr = out, out
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			if msg := strings.TrimSpace(string(out.b)); msg != "" {
				return fmt.Errorf("%s: %v: %s", filepath.Base(argv[0]), err, msg)
			}
			return fmt.Errorf("%s: %v", filepath.Base(argv[0]), err)
		}
	case <-time.After(terminalStartWait):
	}
	return nil
}

// linuxEmulators are tried in order, each with what goes before the
// window's command.
var linuxEmulators = [][]string{
	{"x-terminal-emulator", "-e"},
	{"gnome-terminal", "--"},
	{"konsole", "-e"},
	{"kitty"},
	{"alacritty", "-e"},
	{"xterm", "-e"},
}

// envGet is key's value in env, the last one.
func envGet(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// windowArgv is what a window runs: ssh -t into target, or a login shell
// for local; with a command, the command and then a login shell, so the
// window stays open for the owner.
func windowArgv(goos, target, command string, env []string) []string {
	if target != TerminalLocal {
		argv := []string{"ssh", "-t", "--", target}
		if command != "" {
			argv = append(argv, command+"\nexec \"${SHELL:-/bin/sh}\" -l")
		}
		return argv
	}
	if goos == "windows" {
		if command != "" {
			return []string{"cmd.exe", "/k", command}
		}
		return []string{"cmd.exe"}
	}
	shell := envGet(env, "SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	if command != "" {
		return []string{shell, "-l", "-c", command + "\nexec " + shellJoin([]string{shell}) + " -l"}
	}
	return []string{shell, "-l"}
}

// terminalArgv is the emulator's argv that opens a window running inner on
// goos, and the emulator's name: node.terminal.emulator when set, else on
// Linux and other Unix the first of linuxEmulators found, on macOS
// Terminal by osascript, on Windows Windows Terminal, else cmd's start.
func terminalArgv(goos string, emulator, inner []string, env []string, look func(string) (string, error)) ([]string, string, error) {
	switch goos {
	case "darwin":
		if len(emulator) > 0 {
			return append(append([]string{}, emulator...), inner...), filepath.Base(emulator[0]), nil
		}
		line := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(shellJoin(inner))
		return []string{"osascript", "-e", `tell application "Terminal" to do script "` + line + `"`, "-e", `tell application "Terminal" to activate`}, "Terminal", nil
	case "windows":
		if len(emulator) > 0 {
			return append(append([]string{}, emulator...), inner...), filepath.Base(emulator[0]), nil
		}
		if _, err := look("wt.exe"); err == nil {
			// Windows Terminal splits its command line at ;.
			argv := []string{"wt.exe", "new-tab"}
			for _, a := range inner {
				argv = append(argv, strings.ReplaceAll(a, ";", `\;`))
			}
			return argv, "Windows Terminal", nil
		}
		return append([]string{"cmd.exe", "/c", "start", "agentd terminal"}, inner...), "cmd", nil
	}
	if envGet(env, "DISPLAY") == "" && envGet(env, "WAYLAND_DISPLAY") == "" {
		return nil, "", errNoDesktop
	}
	if len(emulator) > 0 {
		return append(append([]string{}, emulator...), inner...), filepath.Base(emulator[0]), nil
	}
	var tried []string
	for _, e := range linuxEmulators {
		if _, err := look(e[0]); err == nil {
			return append(append([]string{}, e...), inner...), e[0], nil
		}
		tried = append(tried, e[0])
	}
	return nil, "", fmt.Errorf("no terminal emulator on PATH (tried %s); set node.terminal.emulator", strings.Join(tried, ", "))
}

// openTerminal opens e's window on this node's desktop, if its
// configuration allows that target, and says what it launched. Observe
// does not stop it: a window changes no checkout, and is for the owner.
func (n *Node) openTerminal(e Errand) ErrandDone {
	tc := n.cfg.Terminal
	if !tc.allowed() {
		return ErrandDone{Error: n.cfg.Name + " refuses terminals: node.terminal.allow is false"}
	}
	if err := checkTerminal(e.Target, e.Command); err != nil {
		return ErrandDone{Error: err.Error()}
	}
	if tc != nil && len(tc.Targets) > 0 && !contains(tc.Targets, e.Target) {
		return ErrandDone{Error: fmt.Sprintf("%s opens no terminal into %s: it is not in node.terminal.targets", n.cfg.Name, e.Target)}
	}
	launcher := n.term
	if launcher == nil {
		launcher = execLauncher{}
	}
	env := launcher.Env()
	argv, emu, err := terminalArgv(runtime.GOOS, tc.emulator(), windowArgv(runtime.GOOS, e.Target, e.Command, env), env, launcher.LookPath)
	if err != nil {
		return ErrandDone{Error: err.Error()}
	}
	// What it launched, as logged: the argv without the command.
	shown, _, _ := terminalArgv(runtime.GOOS, tc.emulator(), windowArgv(runtime.GOOS, e.Target, "", env), env, launcher.LookPath)
	launched := strings.Join(shown, " ")
	if e.Command != "" {
		launched += " (with a command)"
	}
	if err := launcher.Start(argv, env); err != nil {
		return ErrandDone{Error: "starting " + emu + ": " + err.Error(), Emulator: emu, Launched: launched}
	}
	return ErrandDone{OK: true, Emulator: emu, Launched: launched}
}
