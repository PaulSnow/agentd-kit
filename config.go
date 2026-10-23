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
	EventLog      string                `json:"event_log"`
	PollInterval  Duration              `json:"poll_interval"`
	TranscriptDir string                `json:"transcript_dir"`
	Timeouts      Timeouts              `json:"timeouts"`
	Policies      Policies              `json:"policies"`
	Repositories  map[string]Repository `json:"repositories"`
	Sessions      map[string]Session    `json:"sessions"`
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
}

// RestartPolicy restarts a session whose tmux session is gone and whose
// STATUS.md is not DONE. The default Command keeps Claude Code's permission
// prompts; bypassing them is the owner's explicit choice in their own
// config. Command ends with Claude itself, so the tmux
// session ends when Claude exits and the policy sees it; its last screen is
// not kept, the event log is the record. Restart n (counting from zero within the window)
// waits BackoffInitial doubled n-1 times, capped at BackoffMax; the first
// restart is immediate. After MaxRestarts within Window the breaker trips:
// no more restarts, one escalation. A restart that fails counts as one. The
// same backoff and MaxRestarts bound the retries of every other action that
// fails (an escalation command, a push, a merge request), so they are
// validated even when restarts are disabled.
type RestartPolicy struct {
	Enabled        bool     `json:"enabled"`
	MaxRestarts    int      `json:"max_restarts"`
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

// UsageLimitPolicy recognises the usage-limit message on screen. While it
// shows, the session is left alone; when it clears, ResumeMessage is typed.
type UsageLimitPolicy struct {
	Pattern string `json:"pattern"`
}

// TrustPolicy answers the folder-trust prompt a fresh Claude Code session
// shows, by sending Keys.
type TrustPolicy struct {
	Enabled bool     `json:"enabled"`
	Pattern string   `json:"pattern"`
	Keys    []string `json:"keys"`
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
type Repository struct {
	Forge    string   `json:"forge"`
	Project  string   `json:"project"`
	Allow    []string `json:"allow"`
	Console  string   `json:"console"`
	Assignee string   `json:"assignee"`
}

// Session is one tmux session running Claude Code. Workdir holds BRIEF.md
// and STATUS.md; Cwd is where the session runs. Hold, when set, stops
// delivery with that reason until the owner clears it.
type Session struct {
	Workdir    string    `json:"workdir"`
	Cwd        string    `json:"cwd"`
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

// DefaultRestartPrompt is what a restarted session is told. {brief} and
// {status} are substituted.
const DefaultRestartPrompt = "You were restarted by the supervisor. Continue your brief, {brief}, from where you were; your progress is in {status}."

// DefaultConfig carries the policies of the Python supervisor this replaces,
// plus the backoff and breaker it lacked.
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
				Window:         Duration{time.Hour},
				BackoffInitial: Duration{time.Minute},
				BackoffMax:     Duration{30 * time.Minute},
				Command: []string{"bash", "-lic",
					"claude --continue \"" + DefaultRestartPrompt + "\""},
				Width:  220,
				Height: 50,
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
			UsageLimit: UsageLimitPolicy{Pattern: `(?i)limit reached.{0,80}reset|out of extra usage`},
			Trust:      TrustPolicy{Enabled: true, Pattern: "trust this folder", Keys: []string{"Down", "Enter"}},
			Escalate:   EscalatePolicy{},
		},
	}
}

var sessionName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// LoadConfig reads path over the defaults, expands paths, and validates.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.finish(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

// finish expands paths against the home directory and validates every field
// a policy depends on. It refuses rather than guessing.
func (c *Config) finish() error {
	c.EventLog = expandPath(c.EventLog)
	c.TranscriptDir = expandPath(c.TranscriptDir)
	var errs []error
	if c.PollInterval.Duration <= 0 {
		errs = append(errs, errors.New("poll_interval must be positive"))
	}
	if c.Timeouts.Tmux.Duration <= 0 || c.Timeouts.Forge.Duration <= 0 || c.Timeouts.Escalate.Duration <= 0 {
		errs = append(errs, errors.New("timeouts: tmux, forge and escalate must be positive"))
	}
	r := &c.Policies.Restart
	if r.MaxRestarts < 1 {
		errs = append(errs, errors.New("policies.restart.max_restarts must be at least 1"))
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
	if _, err := regexp.Compile(c.Policies.UsageLimit.Pattern); err != nil {
		errs = append(errs, fmt.Errorf("policies.usage_limit.pattern: %w", err))
	}
	if c.Policies.Trust.Enabled && (c.Policies.Trust.Pattern == "" || len(c.Policies.Trust.Keys) == 0) {
		errs = append(errs, errors.New("policies.trust_prompt needs a pattern and keys"))
	}
	for name, repo := range c.Repositories {
		if repo.Forge != "gitlab" && repo.Forge != "github" {
			errs = append(errs, fmt.Errorf("repositories.%s.forge must be gitlab or github", name))
		}
		if repo.Project == "" {
			errs = append(errs, fmt.Errorf("repositories.%s.project must be set", name))
		}
		for _, a := range repo.Allow {
			if a != "push" && a != "open" {
				errs = append(errs, fmt.Errorf("repositories.%s.allow: %q is not push or open", name, a))
			}
		}
	}
	for name, s := range c.Sessions {
		if !sessionName.MatchString(name) {
			errs = append(errs, fmt.Errorf("sessions: %q is not a plain tmux session name", name))
		}
		if s.Workdir == "" || s.Cwd == "" {
			errs = append(errs, fmt.Errorf("sessions.%s needs workdir and cwd", name))
		}
		s.Workdir = expandPath(s.Workdir)
		s.Cwd = expandPath(s.Cwd)
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
			d.Worktree = expandPath(d.Worktree)
			d.MRFile = expandPath(d.MRFile)
		}
		c.Sessions[name] = s
	}
	if len(c.Policies.Escalate.Command) > 0 {
		c.Policies.Escalate.Command[0] = expandPath(c.Policies.Escalate.Command[0])
	}
	return errors.Join(errs...)
}

// expandPath resolves ~ and makes a relative path home-relative, as the Python
// supervisor's sessions.json did. Absolute paths pass through.
func expandPath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return filepath.Join(home, p)
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
