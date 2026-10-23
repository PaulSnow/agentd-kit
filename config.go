package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Config is the whole declarative configuration: where the event log lives,
// the policies, the repositories agentd may write to, and the sessions it
// watches. The owner's copy lives outside the repository; agentd.example.json
// beside this file shows the shape.
type Config struct {
	EventLog      string   `json:"event_log"`
	PollInterval  Duration `json:"poll_interval"`
	TranscriptDir string   `json:"transcript_dir"`
	Timeouts      Timeouts `json:"timeouts"`
	// ConfidentialPatterns are regular expressions for text agentd must never
	// post. An escalation whose text matches one is replaced by a generic
	// message naming the session.
	ConfidentialPatterns []string              `json:"confidential_patterns"`
	Policies             Policies              `json:"policies"`
	Repositories         map[string]Repository `json:"repositories"`
	Accounts             map[string]Account    `json:"accounts"`
	Sessions             map[string]Session    `json:"sessions"`
}

// Account is a Claude login a session runs under: its config directory,
// its OAuth token file, or both. The token file holds the token; agentd
// never holds it, and the file must be readable by its owner only.
type Account struct {
	ConfigDir string `json:"config_dir"`
	TokenFile string `json:"token_file"`
}

// Timeouts bound every external command agentd runs. A command still
// running at its timeout is killed and its action fails with an error event;
// the pass goes on. Tmux bounds each tmux call, Forge each git, glab and gh
// call, Escalate the escalation command.
type Timeouts struct {
	Tmux     Duration `json:"tmux"`
	Forge    Duration `json:"forge"`
	Escalate Duration `json:"escalate"`
}

// Policies are the supervisor's rules, as data. Every number the Python
// supervisor hard-coded is a field here.
type Policies struct {
	Restart    RestartPolicy    `json:"restart"`
	Nudge      NudgePolicy      `json:"nudge"`
	UsageLimit UsageLimitPolicy `json:"usage_limit"`
	Trust      TrustPolicy      `json:"trust_prompt"`
	Escalate   EscalatePolicy   `json:"escalate"`
	Dialog     DialogPolicy     `json:"dialog"`
	// PaneCommands are the values of tmux's #{pane_current_command} that
	// mean the pane runs Claude Code. Nothing is typed or sent as keys into
	// a pane running anything else, such as the shell left after Claude
	// exited. A native install reports claude; an npm install reports node.
	PaneCommands []string `json:"pane_commands"`
}

// RestartPolicy restarts a session whose tmux session is gone, or whose
// pane is dead, and whose STATUS.md is not DONE and asks no Questions. The
// default Command starts Claude with a fresh context, told to read the
// brief and STATUS.md; it never continues the old conversation. It keeps
// Claude Code's permission prompts; bypassing them is the owner's explicit
// choice in their own config. Command ends with Claude itself, so the pane
// dies when Claude exits; agentd starts it with remain-on-exit, so the dead
// pane keeps its exit status and last screen. Restart n (counting from zero
// within Window) waits BackoffInitial doubled n-1 times, capped at
// BackoffMax; the first restart is immediate. Each disappearance of a
// session seen alive or restarted is a crash, as is a restart that fails; a
// crash whose cause (exit status and last screen) repeats an earlier one, or
// the MaxCrashes-th crash however far apart, parks the session for the
// owner with one escalation until agentd clear. The same backoff and
// MaxRestarts bound the retries of every other action that fails (an
// escalation command, a push, a merge request), so they are validated even
// when restarts are disabled.
type RestartPolicy struct {
	Enabled        bool     `json:"enabled"`
	MaxRestarts    int      `json:"max_restarts"`
	MaxCrashes     int      `json:"max_crashes"`
	Window         Duration `json:"window"`
	BackoffInitial Duration `json:"backoff_initial"`
	BackoffMax     Duration `json:"backoff_max"`
	Command        []string `json:"command"`
	Width          int      `json:"width"`
	Height         int      `json:"height"`
}

// NudgePolicy types a line into a session whose screen has not changed for
// Stall, at most Max times per idle episode, never when the session is
// blocked on the owner. A screen change within Settle after a nudge is the
// nudge echoing, not progress.
type NudgePolicy struct {
	Enabled       bool     `json:"enabled"`
	Stall         Duration `json:"stall"`
	Max           int      `json:"max"`
	Settle        Duration `json:"settle"`
	Message       string   `json:"message"`
	ResumeMessage string   `json:"resume_message"`
}

// UsageLimitPolicy recognises the usage-limit message on the visible screen
// by Pattern, a regular expression. While it shows, the session is left
// alone; when it clears, ResumeMessage is typed.
type UsageLimitPolicy struct {
	Pattern string `json:"pattern"`
}

// TrustPolicy answers the folder-trust prompt a fresh Claude Code session
// shows, by sending Keys. Pattern, a regular expression, must match a whole
// line of the visible screen, less indent and box border: the prompt's own
// question, not the words quoted inside other text. It is answered only in
// the session's start phase. The prompt's menu preselects option 1, "Yes, proceed"
// (or "Yes, I trust this folder"); option 2 is "No, exit". So the default
// is Enter alone: a Down first would choose to exit.
type TrustPolicy struct {
	Enabled bool     `json:"enabled"`
	Pattern string   `json:"pattern"`
	Keys    []string `json:"keys"`
}

// DialogPolicy recognises a Claude Code permission or confirmation dialog on
// the visible screen. Patterns are regular expressions; a session showing a
// match is blocked on the owner and never typed into, since a typed line or
// a key would answer the dialog.
type DialogPolicy struct {
	Patterns []string `json:"patterns"`
}

// EscalatePolicy runs Command, with {console} and {message} substituted, once
// per escalation key. DefaultConsole receives escalations for sessions that
// name no repository.
type EscalatePolicy struct {
	Command        []string `json:"command"`
	DefaultConsole string   `json:"default_console"`
}

// Repository is a forge project agentd may deliver to. Allow lists the writes
// the forge client may make for it: "push" and "open". There is no "merge".
// Host (by default gitlab.com or github.com) and Project build the one URL
// delivery pushes to, over Transport, ssh (the default) or https; glab and
// gh are pointed at Host.
type Repository struct {
	Forge     string   `json:"forge"`
	Host      string   `json:"host"`
	Transport string   `json:"transport"`
	Project   string   `json:"project"`
	Allow     []string `json:"allow"`
	Console   string   `json:"console"`
	Assignee  string   `json:"assignee"`
	// Gate is the argv agentd runs in the worktree, on the exact commit,
	// before it pushes; delivery pushes only when it exits 0. A repository
	// that allows push must have one. GateTimeout bounds it, by default
	// DefaultGateTimeout. It runs with PATH, HOME, LANG and TMPDIR and the
	// variables GateEnv names, nothing else; no agent socket, token or
	// Claude setting may be named.
	Gate        []string `json:"gate"`
	GateTimeout Duration `json:"gate_timeout"`
	GateEnv     []string `json:"gate_env"`
}

// Session is one tmux session running Claude Code. Workdir holds BRIEF.md
// and STATUS.md; Cwd is where the session runs; Account, when set, is the
// Claude login it is restarted under, and there is no fallback to another. Hold, when set, stops
// delivery with that reason until the owner clears it.
type Session struct {
	Workdir    string    `json:"workdir"`
	Cwd        string    `json:"cwd"`
	Account    string    `json:"account"`
	Repository string    `json:"repository"`
	Deliver    []Deliver `json:"deliver"`
	Hold       string    `json:"hold"`
}

// Deliver says what to push and open when the session is DONE.
type Deliver struct {
	Repository string `json:"repository"`
	Worktree   string `json:"worktree"`
	Target     string `json:"target"`
	MRFile     string `json:"mr_file"`
	Draft      bool   `json:"draft"`
}

// Duration is a time.Duration that reads and writes as a string such as "40m".
type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"40m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// DefaultRestartPrompt is what a restarted session is told. It starts with
// a fresh context, so it is pointed at its files. {brief} and {status} are
// substituted. The default command passes it to bash as its own argument,
// $1, so the paths are data whatever characters they hold.
const DefaultRestartPrompt = "You were restarted by the supervisor with a fresh context. Read your brief, {brief}, and your progress, {status}, then continue from where {status} says you are."

// DefaultConfig carries the policies of the Python supervisor this replaces,
// plus the backoff and crash parking it lacked.
func DefaultConfig() Config {
	return Config{
		EventLog:      "~/.local/state/agentd-kit/events.jsonl",
		PollInterval:  Duration{5 * time.Minute},
		TranscriptDir: "~/.claude/projects",
		Timeouts:      Timeouts{Tmux: Duration{10 * time.Second}, Forge: Duration{2 * time.Minute}, Escalate: Duration{2 * time.Minute}},
		Policies: Policies{
			Restart: RestartPolicy{
				Enabled:        true,
				MaxRestarts:    3,
				MaxCrashes:     3,
				Window:         Duration{time.Hour},
				BackoffInitial: Duration{time.Minute},
				BackoffMax:     Duration{30 * time.Minute},
				Command:        []string{"bash", "-lic", `exec claude "$1"`, "agentd", DefaultRestartPrompt},
				Width:          220,
				Height:         50,
			},
			Nudge: NudgePolicy{
				Enabled: true,
				Stall:   Duration{40 * time.Minute},
				Max:     3,
				Settle:  Duration{2 * time.Minute},
				Message: "Carry on with your brief. If you are blocked, write the reason under Questions in STATUS.md " +
					"and go on with what does not depend on it; if you are finished, make DONE the last line of STATUS.md.",
				ResumeMessage: "Continue with your brief; the usage limit has reset.",
			},
			UsageLimit:   UsageLimitPolicy{Pattern: `(?i)limit reached.{0,80}reset|out of extra usage`},
			Trust:        TrustPolicy{Enabled: true, Pattern: `(?i)(do you )?trust (the files in )?this folder\??`, Keys: []string{"Enter"}},
			Escalate:     EscalatePolicy{},
			Dialog:       DialogPolicy{Patterns: []string{`Do you want to`, `❯\s*1\.`, `(?i)enter to confirm`}},
			PaneCommands: []string{"claude"},
		},
	}
}

var sessionName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// LoadConfig reads path over the defaults, expands paths, and validates.
// Relative paths in the file are relative to the file's directory.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.finish(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// finish expands paths against base, the config file's directory, and
// validates every field a policy depends on. It refuses rather than guessing:
// a ~user path, which it does not expand, and a session directory that does
// not exist, so a mis-resolved path fails at start, not at the first restart.
func (c *Config) finish(base string) error {
	var errs []error
	path := func(field, p string) string {
		if strings.HasPrefix(p, "~") && p != "~" && !strings.HasPrefix(p, "~/") {
			errs = append(errs, fmt.Errorf("%s: %q: ~user paths are not supported; write the full path", field, p))
			return p
		}
		return expandPath(p, base)
	}
	isDir := func(field, p string) {
		if info, err := os.Stat(p); err != nil || !info.IsDir() {
			errs = append(errs, fmt.Errorf("%s: directory %s does not exist", field, p))
		}
	}
	c.EventLog = path("event_log", c.EventLog)
	c.TranscriptDir = path("transcript_dir", c.TranscriptDir)
	if c.PollInterval.Duration <= 0 {
		errs = append(errs, errors.New("poll_interval must be positive"))
	}
	if c.Timeouts.Tmux.Duration <= 0 || c.Timeouts.Forge.Duration <= 0 || c.Timeouts.Escalate.Duration <= 0 {
		errs = append(errs, errors.New("timeouts: tmux, forge and escalate must be positive"))
	}
	r := &c.Policies.Restart
	if r.MaxRestarts < 1 || r.MaxCrashes < 1 {
		errs = append(errs, errors.New("policies.restart.max_restarts and max_crashes must be at least 1"))
	}
	if r.Window.Duration <= 0 || r.BackoffInitial.Duration <= 0 || r.BackoffMax.Duration < r.BackoffInitial.Duration {
		errs = append(errs, errors.New("policies.restart: window and backoff_initial must be positive and backoff_max at least backoff_initial"))
	}
	if r.Enabled && len(r.Command) == 0 {
		errs = append(errs, errors.New("policies.restart.command must not be empty"))
	}
	n := &c.Policies.Nudge
	if n.Enabled && (n.Stall.Duration <= 0 || n.Max < 0 || n.Settle.Duration < 0 || n.Message == "") {
		errs = append(errs, errors.New("policies.nudge: stall must be positive, max and settle non-negative, message non-empty"))
	}
	for i, p := range c.ConfidentialPatterns {
		if _, err := regexp.Compile(p); err != nil {
			errs = append(errs, fmt.Errorf("confidential_patterns[%d]: %w", i, err))
		}
	}
	if _, err := regexp.Compile(c.Policies.UsageLimit.Pattern); err != nil {
		errs = append(errs, fmt.Errorf("policies.usage_limit.pattern: %w", err))
	}
	for i, p := range c.Policies.Dialog.Patterns {
		if _, err := regexp.Compile(p); err != nil {
			errs = append(errs, fmt.Errorf("policies.dialog.patterns[%d]: %w", i, err))
		}
	}
	if len(c.Policies.PaneCommands) == 0 {
		errs = append(errs, errors.New("policies.pane_commands must name at least one command"))
	}
	if c.Policies.Trust.Enabled && (c.Policies.Trust.Pattern == "" || len(c.Policies.Trust.Keys) == 0) {
		errs = append(errs, errors.New("policies.trust_prompt needs a pattern and keys"))
	}
	if _, err := regexp.Compile(c.Policies.Trust.Pattern); err != nil {
		errs = append(errs, fmt.Errorf("policies.trust_prompt.pattern: %w", err))
	}
	for name, repo := range c.Repositories {
		if repo.Forge != "gitlab" && repo.Forge != "github" {
			errs = append(errs, fmt.Errorf("repositories.%s.forge must be gitlab or github", name))
		}
		if repo.Project == "" {
			errs = append(errs, fmt.Errorf("repositories.%s.project must be set", name))
		}
		if repo.Host == "" {
			repo.Host = map[string]string{"gitlab": "gitlab.com", "github": "github.com"}[repo.Forge]
		}
		switch repo.Transport {
		case "":
			repo.Transport = "ssh"
		case "ssh", "https":
		default:
			errs = append(errs, fmt.Errorf("repositories.%s.transport must be ssh or https", name))
		}
		c.Repositories[name] = repo
		for _, a := range repo.Allow {
			if a != "push" && a != "open" {
				errs = append(errs, fmt.Errorf("repositories.%s.allow: %q is not push or open", name, a))
			}
			if a == "push" && len(repo.Gate) == 0 {
				errs = append(errs, fmt.Errorf("repositories.%s allows push but has no gate: nothing is delivered without one", name))
			}
		}
		if repo.GateTimeout.Duration < 0 {
			errs = append(errs, fmt.Errorf("repositories.%s.gate_timeout must not be negative", name))
		}
		if repo.GateTimeout.Duration == 0 {
			repo.GateTimeout = Duration{DefaultGateTimeout}
		}
		for _, v := range repo.GateEnv {
			if secretEnv(v) || v == "" || strings.Contains(v, "=") {
				errs = append(errs, fmt.Errorf("repositories.%s.gate_env: %q may not be passed to a gate", name, v))
			}
		}
	}
	for name, a := range c.Accounts {
		if a.ConfigDir == "" && a.TokenFile == "" {
			errs = append(errs, fmt.Errorf("accounts.%s needs a config_dir, a token_file, or both", name))
		}
		a.ConfigDir = path("accounts."+name+".config_dir", a.ConfigDir)
		a.TokenFile = path("accounts."+name+".token_file", a.TokenFile)
		if a.TokenFile != "" {
			if err := checkTokenFile(a.TokenFile); err != nil {
				errs = append(errs, fmt.Errorf("accounts.%s.token_file: %w", name, err))
			}
		}
		c.Accounts[name] = a
	}
	for name, s := range c.Sessions {
		if s.Account != "" {
			if _, ok := c.Accounts[s.Account]; !ok {
				errs = append(errs, fmt.Errorf("sessions.%s.account %q is not in accounts", name, s.Account))
			}
		}
		if !sessionName.MatchString(name) {
			errs = append(errs, fmt.Errorf("sessions: %q is not a plain tmux session name", name))
		}
		if s.Workdir == "" || s.Cwd == "" {
			errs = append(errs, fmt.Errorf("sessions.%s needs workdir and cwd", name))
		}
		if s.Workdir != "" && s.Cwd != "" {
			s.Workdir = path("sessions."+name+".workdir", s.Workdir)
			s.Cwd = path("sessions."+name+".cwd", s.Cwd)
			isDir("sessions."+name+".workdir", s.Workdir)
			isDir("sessions."+name+".cwd", s.Cwd)
		}
		if s.Repository != "" {
			if _, ok := c.Repositories[s.Repository]; !ok {
				errs = append(errs, fmt.Errorf("sessions.%s.repository %q is not in repositories", name, s.Repository))
			}
		}
		for i := range s.Deliver {
			d := &s.Deliver[i]
			if _, ok := c.Repositories[d.Repository]; !ok {
				errs = append(errs, fmt.Errorf("sessions.%s.deliver[%d].repository %q is not in repositories", name, i, d.Repository))
			}
			if d.Worktree == "" || d.Target == "" || d.MRFile == "" {
				errs = append(errs, fmt.Errorf("sessions.%s.deliver[%d] needs worktree, target and mr_file", name, i))
			}
			d.Worktree = path(fmt.Sprintf("sessions.%s.deliver[%d].worktree", name, i), d.Worktree)
			d.MRFile = path(fmt.Sprintf("sessions.%s.deliver[%d].mr_file", name, i), d.MRFile)
		}
		c.Sessions[name] = s
	}
	// Escalation is the only path to the owner; without it a blocked
	// session would wait unseen.
	if len(c.Sessions) > 0 && len(c.Policies.Escalate.Command) == 0 {
		errs = append(errs, errors.New("policies.escalate.command must be set when sessions are configured"))
	}
	// A bare command name is left for PATH to find; a path is expanded.
	if cmd := c.Policies.Escalate.Command; len(cmd) > 0 && (strings.HasPrefix(cmd[0], "~") || strings.Contains(cmd[0], "/")) {
		cmd[0] = path("policies.escalate.command", cmd[0])
	}
	return errors.Join(errs...)
}

// checkTokenFile refuses a token file that is missing, not a regular file,
// readable by group or others, or blank. It reads the file only to see that
// it holds something; the token is not kept.
func checkTokenFile(p string) error {
	info, err := os.Stat(p)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", p)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is readable by others (mode %04o); make it 0600", p, info.Mode().Perm())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(b)) == "" {
		return fmt.Errorf("%s is blank", p)
	}
	return nil
}

// DefaultGateTimeout bounds a gate whose repository sets no gate_timeout.
const DefaultGateTimeout = 10 * time.Minute

// gateBaseEnv are the variables every gate gets, when agentd has them.
var gateBaseEnv = []string{"PATH", "HOME", "LANG", "TMPDIR"}

// secretEnv says whether a variable must never reach a gate: the ssh
// agent's socket, any token, any Claude setting.
func secretEnv(name string) bool {
	return name == "SSH_AUTH_SOCK" || strings.HasSuffix(name, "_TOKEN") || strings.HasPrefix(name, "CLAUDE_")
}

// gateEnv is the environment a gate for r runs with.
func gateEnv(r Repository) []string {
	var env []string
	for _, name := range append(append([]string{}, gateBaseEnv...), r.GateEnv...) {
		if v, ok := os.LookupEnv(name); ok && !secretEnv(name) {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// expandPath resolves ~ to the home directory and a relative path against
// base, or against the working directory when base is "" (a path given on
// the command line). Absolute paths pass through.
func expandPath(p, base string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, p[1:])
	}
	if base == "" {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	return filepath.Join(base, p)
}

// SessionNames returns the configured session names in a stable order.
func (c *Config) SessionNames() []string {
	names := make([]string, 0, len(c.Sessions))
	for n := range c.Sessions {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Console returns the console an escalation about session name goes to.
func (c *Config) Console(name string) string {
	s := c.Sessions[name]
	if s.Repository != "" {
		if r, ok := c.Repositories[s.Repository]; ok && r.Console != "" {
			return r.Console
		}
		return s.Repository
	}
	return c.Policies.Escalate.DefaultConsole
}
