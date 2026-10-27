package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// opsEnv is what the ops tools and the changes run on: the ops-serving
// config, tmux, git and the build, each replaceable in tests by a fake.
type opsEnv struct {
	cfg  *Config
	tmux Tmux
	// git runs git in dir and returns its output; build builds the clone
	// at commit sha into the binary out.
	git   func(dir string, args ...string) (string, error)
	build func(ctx context.Context, repo, sha, out string) error
	// accounts reads each account's usage now.
	accounts func(cfgs []*Config) []opsAccount
	// selfRestart restarts the tmux session this process runs in, from a
	// process of its own, since this one is what Ctrl-C stops.
	selfRestart func(tmux string) error
	// binary is the agentd an install replaces; self the supervisor this
	// process is, "" when none is.
	binary string
	self   string
	// wait bounds each wait of a restart, poll is how often it looks, and
	// sleep is how it waits.
	wait, poll time.Duration
	sleep      func(time.Duration)
	now        func() time.Time
}

// Restart waits: Ctrl-C lets a run wait StopWait for its jobs, so a
// stopping process is given more.
const (
	opsWait = StopWait + 30*time.Second
	opsPoll = time.Second
)

// newOpsEnv is the real environment for cfg's ops block.
func newOpsEnv(cfg *Config) *opsEnv {
	o := &opsEnv{cfg: cfg, tmux: ExecTmux{Timeout: cfg.Timeouts.Tmux.Duration}, wait: opsWait, poll: opsPoll, sleep: time.Sleep, now: time.Now,
		git: func(dir string, args ...string) (string, error) {
			out, err := runTimed(2*time.Minute, dir, nil, true, "git", args...)
			if err != nil {
				return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, oneLine(string(out), 300))
			}
			return strings.TrimSpace(string(out)), nil
		},
		build: goBuild, accounts: readOpsAccounts, selfRestart: detachedRestart}
	o.binary = cfg.Ops.Binary
	if o.binary == "" {
		if exe, err := os.Executable(); err == nil {
			o.binary, _ = filepath.EvalSymlinks(exe)
		}
	}
	o.self = opsSelf(cfg, tmuxSessionName())
	return o
}

// opsSelf is the supervisor this process is: the one in its tmux
// session, else the only one with its config.
func opsSelf(cfg *Config, tmux string) string {
	var byConfig []string
	for _, name := range cfg.Ops.names() {
		s := cfg.Ops.Supervisors[name]
		if tmux != "" && s.Tmux == tmux {
			return name
		}
		if cfg.Path != "" && realPath(s.Config) == realPath(cfg.Path) {
			byConfig = append(byConfig, name)
		}
	}
	if tmux == "" && len(byConfig) == 1 {
		return byConfig[0]
	}
	return ""
}

// goBuild builds commit sha of the clone repo into out, from a clone of
// its own that shares repo's objects, so the clone's checkout and
// worktrees are untouched and Go stamps the commit into the binary.
func goBuild(ctx context.Context, repo, sha, out string) error {
	dir, err := os.MkdirTemp("", "agentd-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "src")
	run := func(dir string, name string, args ...string) error {
		cmd := groupCommand(ctx, dir, name, args...)
		cmd.Env = append(os.Environ(), "GOFLAGS=-mod=readonly")
		b, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, oneLine(string(b), 500))
		}
		return nil
	}
	if err := run(dir, "git", "clone", "--quiet", "--shared", "--no-checkout", repo, src); err != nil {
		return err
	}
	if err := run(src, "git", "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", sha); err != nil {
		return err
	}
	goBin := "go"
	if _, err := exec.LookPath("go"); err != nil {
		goBin = "/usr/local/go/bin/go"
	}
	return run(src, goBin, "build", "-o", out, ".")
}

// detachedRestart restarts tmux session name from a process of its own
// session, which outlives the Ctrl-C it sends: once the pane no longer
// runs agentd, it sends Up and Enter, as the owner would.
func detachedRestart(name string) error {
	tg, err := target(name)
	if err != nil {
		return err
	}
	script := `sleep 2; tmux send-keys -t "$1" C-c; i=0; ` +
		`while [ "$(tmux display-message -p -t "$1" '#{pane_current_command}')" = agentd ] && [ $i -lt 150 ]; do sleep 1; i=$((i+1)); done; ` +
		`sleep 1; tmux send-keys -t "$1" Up; sleep 1; tmux send-keys -t "$1" Enter`
	cmd := exec.Command("sh", "-c", script, "sh", tg)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// supervisor is the named supervisor and its config, loaded now.
func (o *opsEnv) supervisor(name string) (OpsSupervisor, *Config, error) {
	s, ok := o.cfg.Ops.Supervisors[name]
	if !ok {
		return s, nil, fmt.Errorf("no supervisor %q; the supervisors are %s", cutUTF8(name, 60), strings.Join(o.cfg.Ops.names(), ", "))
	}
	cfg, err := LoadConfig(s.Config)
	return s, cfg, err
}

// up says whether the supervisor's tmux session runs agentd now, and
// what its pane runs.
func (o *opsEnv) up(s OpsSupervisor) (bool, string) {
	ok, err := o.tmux.HasSession(s.Tmux)
	if err != nil {
		return false, "tmux: " + oneLine(err.Error(), 200)
	}
	if !ok {
		return false, "no tmux session " + s.Tmux
	}
	info, err := o.tmux.Pane(s.Tmux)
	if err != nil {
		return false, "tmux: " + oneLine(err.Error(), 200)
	}
	if info.Dead {
		return false, "its pane is dead"
	}
	return info.Command == o.command(), info.Command
}

// command is what a pane running agentd shows as its command.
func (o *opsEnv) command() string {
	if o.binary != "" {
		return filepath.Base(o.binary)
	}
	return "agentd"
}

// opsRow is a session of a supervisor as ops_status gives it: the status
// table's row, as JSON.
type opsRow struct {
	Name     string    `json:"name"`
	State    State     `json:"state"`
	Since    time.Time `json:"since"`
	LastSeen time.Time `json:"last_seen"`
	Head     string    `json:"head,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Restarts int       `json:"restarts,omitempty"`
	Errors   int       `json:"errors,omitempty"`
	Context  int64     `json:"context,omitempty"`
	Stopped  string    `json:"stopped,omitempty"`
	Model    string    `json:"model,omitempty"`
}

// opsStatus is one supervisor as ops_status gives it: whether its process
// is up, its config's check, and its sessions.
type opsStatus struct {
	Name     string   `json:"name"`
	Tmux     string   `json:"tmux"`
	Up       bool     `json:"up"`
	Pane     string   `json:"pane,omitempty"` // what its pane runs, or why it could not be asked
	Self     bool     `json:"self,omitempty"` // the process serving the ops console
	Config   string   `json:"config_error,omitempty"`
	Summary  string   `json:"summary,omitempty"` // its sessions by state, in a line
	Sessions []opsRow `json:"sessions,omitempty"`
}

// status is each named supervisor's status, all of them for none.
func (o *opsEnv) status(names []string, rows bool) []opsStatus {
	if len(names) == 0 {
		names = o.cfg.Ops.names()
	}
	secret := confidentialMatcher(o.cfg)
	var out []opsStatus
	for _, name := range names {
		s, cfg, err := o.supervisor(name)
		st := opsStatus{Name: name, Tmux: s.Tmux, Self: name == o.self}
		st.Up, st.Pane = o.up(s)
		if err != nil {
			st.Config = oneLine(err.Error(), 300)
			if secret(err.Error()) {
				st.Config = withheldMessage
			}
			out = append(out, st)
			continue
		}
		events, _, err := ReadLog(cfg.EventLog)
		if err != nil {
			st.Config = "its log: " + oneLine(err.Error(), 200)
			out = append(out, st)
			continue
		}
		table, _ := SummarizeAt(projected(cfg, events), events, statusMode(events), o.now())
		counts := map[string]int{}
		for _, r := range table {
			counts[string(r.State)]++
			if !rows {
				continue
			}
			row := opsRow{Name: r.Name, State: r.State, Since: r.Since, LastSeen: r.LastSeen, Head: r.Head, Reason: r.Reason,
				Restarts: r.Restarts, Errors: r.Errors, Stopped: r.Stopped, Model: r.Model}
			if r.Context > 0 {
				row.Context = r.Context
			}
			if secret(row.Reason, row.Stopped) {
				row.Reason, row.Stopped = withheldIf(row.Reason), withheldIf(row.Stopped)
			}
			st.Sessions = append(st.Sessions, row)
		}
		var parts []string
		for _, k := range sortedKeys(counts) {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
		st.Summary = strings.Join(parts, ", ")
		if st.Summary == "" {
			st.Summary = "no sessions"
		}
		out = append(out, st)
	}
	return out
}

// opsAccount is an account's plan usage, read now.
type opsAccount struct {
	Name  string `json:"name"`
	Usage string `json:"usage,omitempty"`
	Error string `json:"error,omitempty"`
}

// configs are the supervisors' configs that load.
func (o *opsEnv) configs() []*Config {
	var out []*Config
	for _, name := range o.cfg.Ops.names() {
		if _, cfg, err := o.supervisor(name); err == nil {
			out = append(out, cfg)
		}
	}
	return out
}

// readOpsAccounts reads each account of cfgs once by name, the first
// config's of a name.
func readOpsAccounts(cfgs []*Config) []opsAccount {
	seen := map[string]bool{}
	var out []opsAccount
	for _, cfg := range cfgs {
		r := newPlanReader(cfg.Usage, cfg.Timeouts.Forge.Duration)
		for _, name := range sortedKeys(cfg.Accounts) {
			if seen[name] {
				continue
			}
			seen[name] = true
			a := cfg.Accounts[name]
			if a.Usage == UsageOff {
				out = append(out, opsAccount{Name: name, Usage: "off"})
				continue
			}
			p := r.read(name, a)
			if p.Error != "" {
				out = append(out, opsAccount{Name: name, Error: withheldText(cfg, oneLine(p.Error, 200))})
				continue
			}
			out = append(out, opsAccount{Name: name, Usage: p.Method + ": " + usageText(p)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// resolvedConfig is a supervisor's config as agentd config prints it,
// with credentials and the paths to them redacted.
func (o *opsEnv) resolvedConfig(name string) (any, error) {
	_, cfg, err := o.supervisor(name)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return redactValue(v, confidentialMatcher(o.cfg)), nil
}

// Bounds of what the read tools give.
const (
	opsTailMax   = 200
	opsEventsMax = 50
	opsEventText = 2 << 10
)

// tail is the last n lines of the supervisor's pane, each line that
// matches a confidential pattern withheld.
func (o *opsEnv) tail(name string, n int) (string, error) {
	s, ok := o.cfg.Ops.Supervisors[name]
	if !ok {
		_, _, err := o.supervisor(name)
		return "", err
	}
	if n <= 0 || n > opsTailMax {
		n = min(max(n, 40), opsTailMax)
	}
	screen, err := o.tmux.CapturePane(s.Tmux, n)
	if err != nil {
		return "", err
	}
	lines := splitLines([]byte(strings.TrimRight(screen, "\n")))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return redactLines(strings.Join(lines, "\n"), confidentialMatcher(o.cfg)), nil
}

// events are the last n events of kind in the supervisor's log, each as
// compact JSON, cut, and withheld whole when it matches a confidential
// pattern.
func (o *opsEnv) events(name, kind string, n int) ([]string, error) {
	_, cfg, err := o.supervisor(name)
	if err != nil {
		return nil, err
	}
	if n <= 0 || n > opsEventsMax {
		n = min(max(n, 10), opsEventsMax)
	}
	events, _, err := ReadLog(cfg.EventLog)
	if err != nil {
		return nil, err
	}
	secret := confidentialMatcher(o.cfg)
	var out []string
	for i := len(events) - 1; i >= 0 && len(out) < n; i-- {
		if kind != "" && events[i].Kind != kind {
			continue
		}
		b, _ := json.Marshal(events[i])
		text := string(b)
		if secret(text) {
			text = fmt.Sprintf(`{"t":%q,"kind":%q,"withheld":true}`, events[i].Time.Format(time.RFC3339), events[i].Kind)
		}
		out = append(out, cutUTF8(text, opsEventText))
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// installed is the commit the installed binary was built from, and the
// tip of the clone's main as last fetched.
func (o *opsEnv) installed() (installed, main string) {
	if o.binary != "" {
		installed = binaryCommit(o.binary)
	}
	main, _ = o.git(o.cfg.Ops.Repo, "rev-parse", "--verify", "--quiet", "origin/main^{commit}")
	return installed, main
}

// keptBinaries are the binaries kept beside the installed one, agentd.SHA,
// newest first.
func (o *opsEnv) keptBinaries() []string {
	if o.binary == "" {
		return nil
	}
	base := filepath.Base(o.binary)
	entries, _ := os.ReadDir(filepath.Dir(o.binary))
	type kept struct {
		name string
		at   time.Time
	}
	var ks []kept
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, base+".") && keptName.MatchString(strings.TrimPrefix(n, base+".")) && e.Type().IsRegular() {
			info, _ := e.Info()
			ks = append(ks, kept{n, info.ModTime()})
		}
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i].at.After(ks[j].at) })
	var out []string
	for _, k := range ks {
		out = append(out, k.name)
	}
	return out
}

// keptName is the suffix of a kept binary: a commit, or unknown and a
// hash of its bytes.
var keptName = regexp.MustCompile(`^([0-9a-f]{12}|unknown-[0-9a-f]{12})$`)

// copyFileTo copies src to a new file beside dst and renames it over dst,
// executable, so dst is one binary or the other at every moment.
func copyFileTo(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".new-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// keep keeps the installed binary as agentd.SHA beside it, once, and
// says its name.
func (o *opsEnv) keep() (string, error) {
	b, err := os.ReadFile(o.binary)
	if err != nil {
		return "", err
	}
	suffix := short(strings.TrimSuffix(binaryCommit(o.binary), "+modified"))
	if len(suffix) != 12 {
		suffix = "unknown-" + fileHash(b)[:12]
	}
	name := filepath.Base(o.binary) + "." + suffix
	path := filepath.Join(filepath.Dir(o.binary), name)
	if _, err := os.Stat(path); err == nil {
		return name, nil
	}
	return name, copyFileTo(o.binary, path)
}

// checkConfigs runs the installed binary's agentd config on every
// supervisor's config, and says which failed.
func (o *opsEnv) checkConfigs() error {
	var errs []string
	for _, name := range o.cfg.Ops.names() {
		s := o.cfg.Ops.Supervisors[name]
		out, err := runTimed(time.Minute, "", nil, true, o.binary, "config", "--config", s.Config)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v: %s", name, err, oneLine(lastLine(string(out)), 300)))
		}
	}
	if len(errs) > 0 {
		return errors.New("agentd config failed on " + strings.Join(errs, "; "))
	}
	return nil
}

// lastLine is text's last non-empty line.
func lastLine(text string) string {
	lines := splitLines([]byte(strings.TrimSpace(text)))
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// waitUp waits until the supervisor is up, or down, as want says, and
// says whether it came to that within the wait.
func (o *opsEnv) waitUp(s OpsSupervisor, want bool) bool {
	for waited := time.Duration(0); ; waited += o.poll {
		if up, _ := o.up(s); up == want {
			return true
		}
		if waited >= o.wait {
			return false
		}
		o.sleep(o.poll)
	}
}

// restart restarts one supervisor the owner's way: Ctrl-C when it is up,
// then, once its pane no longer runs agentd, Up and Enter, which run its
// command again; and it waits for it to be up. This process restarts
// itself from a process of its own: errSelf says that one is under way.
func (o *opsEnv) restart(name string) error {
	s, ok := o.cfg.Ops.Supervisors[name]
	if !ok {
		return fmt.Errorf("no supervisor %q", name)
	}
	if name == o.self {
		if err := o.selfRestart(s.Tmux); err != nil {
			return fmt.Errorf("restarting %s, this process: %v", name, err)
		}
		return errSelf
	}
	if ok, err := o.tmux.HasSession(s.Tmux); err != nil || !ok {
		return fmt.Errorf("%s has no tmux session %s to restart in", name, s.Tmux)
	}
	if up, _ := o.up(s); up {
		if err := o.tmux.Keys(s.Tmux, "C-c"); err != nil {
			return err
		}
		if !o.waitUp(s, false) {
			return fmt.Errorf("%s still runs agentd %s after Ctrl-C", name, o.wait)
		}
	}
	if err := o.tmux.Keys(s.Tmux, "Up", "Enter"); err != nil {
		return err
	}
	if !o.waitUp(s, true) {
		_, pane := o.up(s)
		return fmt.Errorf("%s is not up %s after Up and Enter; its pane runs %s", name, o.wait, oneLine(pane, 80))
	}
	return nil
}

// errSelf is a restart of this process, left to a process of its own:
// the next agentd here says how it went.
var errSelf = errors.New("this process restarts")

// restartAll restarts the supervisors that run config, or every one for
// "", this process last; it says which it restarted, whether this
// process restarts, and the one that failed, stopping there.
func (o *opsEnv) restartAll(config string) ([]string, bool, string, error) {
	var done []string
	self := false
	for _, name := range o.cfg.Ops.names() {
		s := o.cfg.Ops.Supervisors[name]
		if config != "" && realPath(s.Config) != realPath(config) {
			continue
		}
		if name == o.self {
			self = true
			continue
		}
		if err := o.restart(name); err != nil {
			return done, false, name, err
		}
		done = append(done, name)
	}
	if self {
		if err := o.restart(o.self); err != errSelf {
			return done, false, "", err
		}
	}
	return done, self, "", nil
}
