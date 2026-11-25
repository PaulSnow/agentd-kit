package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Config reload, config drift and the owner's model for one session
// (#100). A run records the config it loaded, by its file's hash, in its
// start event; each pass it hashes the file on disk and records once when
// the file differs from what it runs, or stops differing. It reloads only
// when the owner asks: agentd config reload, or SIGHUP. The reload goes
// through LoadConfig, as at start; a file that does not load is refused
// and the running config stays. The swap is at a pass's start, never
// mid-pass.

// ConfigState is the config a run holds and the file as the run last saw
// it, folded from its start and config events.
type ConfigState struct {
	Path    string
	Running string // the hash of the file the run loaded or last reloaded; "" from a start before #100
	// File is the file's hash as the run last saw it, "" when the run has
	// not seen it differ since it loaded it; Error is its load error then.
	File  string
	Error string
	// RestartOnly and Removed are what the latest reload kept at the
	// running values and the configured sessions it no longer had.
	RestartOnly []string
	Removed     []string
	// Requested is when the owner last asked for a reload, Handled when the
	// run last loaded the file: at its start, or a reload applied or refused.
	Requested time.Time
	Handled   time.Time
	// Refused is the latest refusal's error, until a reload is applied.
	Refused string
}

func (c *ConfigState) started(at time.Time, p *StartPayload) {
	*c = ConfigState{Path: p.ConfigPath, Running: p.ConfigHash, Requested: c.Requested, Handled: at}
}

// drifted says whether the file the run last saw differs from what it runs.
func (c ConfigState) drifted() bool { return c.Running != "" && c.File != "" && c.File != c.Running }

// reloadDue says whether the owner asked for a reload the run has not made.
func (c ConfigState) reloadDue() bool { return c.Requested.After(c.Handled) }

func (st *LogState) applyConfig(e Event) {
	c, p := &st.Config, e.Config
	switch p.Step {
	case ConfigRequest:
		if e.Time.After(c.Requested) {
			c.Requested = e.Time
		}
	case ConfigDrift:
		c.File, c.Error = p.Hash, p.Error
	case ConfigApplied:
		c.Path, c.Running, c.File, c.Error = p.Path, p.Hash, p.Hash, ""
		c.RestartOnly, c.Removed, c.Refused, c.Handled = p.RestartOnly, p.Removed, "", e.Time
	case ConfigRefused:
		c.File, c.Error, c.Refused, c.Handled = p.Hash, p.Error, p.Error, e.Time
	}
}

// ModelOverride is the owner's model for one session: it runs ahead of
// the policies' until cleared. Now asks for the relaunch at the next pass;
// At is when it was set.
type ModelOverride struct {
	Model string
	Now   bool
	At    time.Time
}

func (st *LogState) applyModel(e Event) {
	if e.Model.Model == "" {
		delete(st.Models, e.Session)
		return
	}
	if st.Models == nil {
		st.Models = map[string]ModelOverride{}
	}
	st.Models[e.Session] = ModelOverride{Model: e.Model.Model, Now: e.Model.Now, At: e.Time}
}

// launchedModel keeps the model a launch ran, when it recorded one.
func (t *Track) launchedModel(a *ActPayload) {
	if a.Model != nil {
		t.Model, t.ModelKnown = *a.Model, true
	}
}

// launchModel is a launch's model for its ActPayload.
func launchModel(m string) *string { return &m }

// restartOnlyFields are the config's fields a run applies only at its
// start: the event log, which its instance lock is tied to, the ops
// console's environment, and what agentd ui, hub and node serve, each in
// its own process.
var restartOnlyFields = []struct {
	key string
	get func(*Config) any
	set func(dst, src *Config)
}{
	{"event_log", func(c *Config) any { return c.EventLog }, func(d, s *Config) { d.EventLog = s.EventLog }},
	{"ops", func(c *Config) any { return c.Ops }, func(d, s *Config) { d.Ops = s.Ops }},
	{"ui", func(c *Config) any { return c.UI }, func(d, s *Config) { d.UI = s.UI }},
	{"hub", func(c *Config) any { return c.Hub }, func(d, s *Config) { d.Hub = s.Hub }},
	{"node", func(c *Config) any { return c.Node }, func(d, s *Config) { d.Node = s.Node }},
}

// keepRestartOnly sets next's restart-only fields to running's and names
// those that differed.
func keepRestartOnly(running, next *Config) []string {
	var out []string
	for _, f := range restartOnlyFields {
		if !reflect.DeepEqual(f.get(running), f.get(next)) {
			out = append(out, f.key)
			f.set(next, running)
		}
	}
	return out
}

// SignalReload asks the run for a reload at its next pass, as SIGHUP does.
func (s *Supervisor) SignalReload() {
	s.work.hup.Store(true)
	s.work.signal()
}

// reloadOnHangup makes SIGHUP ask sup for a reload at its next pass, as
// agentd config reload does, until ctx ends or the returned stop.
func reloadOnHangup(ctx context.Context, sup *Supervisor) func() {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-hup:
				if !ok {
					return
				}
				sup.SignalReload()
			}
		}
	}()
	return func() { signal.Stop(hup) }
}

// checkConfig reloads the config when the owner asked, then records
// whether the file on disk differs from what the run holds. A config with
// no file, as tests build, has nothing to check.
func (s *Supervisor) checkConfig() {
	if s.base.Path == "" || s.worker {
		return
	}
	if signal := s.work.hup.Swap(false); signal || s.st.Config.reloadDue() {
		s.reloadConfig(signal)
	}
	if s.halt == nil {
		s.noteDrift()
	}
}

// runningHash is the hash of the config the run holds.
func (s *Supervisor) runningHash() string {
	if h := s.st.Config.Running; h != "" {
		return h
	}
	return s.base.Hash
}

// noteDrift hashes the file on disk and records, once per hash, that it
// differs from the running config, with its load error if it has one, or
// that it no longer differs.
func (s *Supervisor) noteDrift() {
	path, running := s.base.Path, s.runningHash()
	hash, lerr := "missing", ""
	if b, err := os.ReadFile(path); err != nil {
		lerr = err.Error()
	} else {
		hash = fileHash(b)
	}
	c := s.st.Config
	if hash == c.File || c.File == "" && hash == running {
		return
	}
	if hash != running && lerr == "" {
		if _, err := LoadConfig(path); err != nil {
			lerr = err.Error()
		}
	}
	s.emit("", Event{Kind: KindConfig, Config: &ConfigPayload{Step: ConfigDrift, Path: path, Hash: hash, Previous: running, Error: lerr}})
}

// reloadConfig loads the config file again, as at start, and swaps it in;
// a file that does not load is refused and the running config stays. It
// runs at a pass's start, before anything acts.
func (s *Supervisor) reloadConfig(signal bool) {
	path, running := s.base.Path, s.runningHash()
	next, err := LoadConfig(path)
	if err == nil {
		err = compileAll(next)
	}
	if err != nil {
		hash := "missing"
		if b, rerr := os.ReadFile(path); rerr == nil {
			hash = fileHash(b)
		}
		s.emit("", Event{Kind: KindConfig, Config: &ConfigPayload{Step: ConfigRefused, Path: path, Hash: hash, Previous: running, Error: err.Error(), Signal: signal}})
		return
	}
	if s.override != nil {
		s.override(next)
	}
	restartOnly := keepRestartOnly(s.base, next)
	var removed []string
	for _, n := range s.base.SessionNames() {
		if _, ok := next.Sessions[n]; !ok {
			removed = append(removed, n)
		}
	}
	s.useConfig(next)
	s.emit("", Event{Kind: KindConfig, Config: &ConfigPayload{Step: ConfigApplied, Path: path, Hash: next.Hash, Previous: running,
		Applied: true, RestartOnly: restartOnly, Removed: removed, Signal: signal}})
}

// compileAll compiles the patterns NewSupervisor compiles, so a reload
// that would panic on one is refused instead.
func compileAll(cfg *Config) error {
	pats := append(append([]string{cfg.Policies.Context.Checkpoint, cfg.Policies.UsageLimit.Pattern, `^(?:` + cfg.Policies.Trust.Pattern + `)$`},
		cfg.Policies.Dialog.Patterns...), screenPatterns(cfg)...)
	var errs []error
	for _, p := range pats {
		if _, err := regexp.Compile(p); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// useConfig makes next the run's config and derives again what
// NewSupervisor and run derived from it. The started sessions, the state
// and the workers' shared state stay.
func (s *Supervisor) useConfig(next *Config) {
	fresh := NewSupervisor(next, s.log, s.clock, s.tmux, s.git, s.forge.Forge, s.runner, s.shadow)
	s.base = next
	s.st.cfg = next
	s.forge = fresh.forge
	s.checkpointRe, s.limitRe, s.loginRe, s.trustRe = fresh.checkpointRe, fresh.limitRe, fresh.loginRe, fresh.trustRe
	s.dialogs, s.confidential = fresh.dialogs, fresh.confidential
	to := next.Timeouts
	if _, ok := s.tmux.(ExecTmux); ok {
		s.tmux = ExecTmux{Timeout: to.Tmux.Duration}
	}
	if _, ok := s.git.(ExecGit); ok {
		s.git = ExecGit{Timeout: to.Forge.Duration}
	}
	if _, ok := s.forge.Forge.(ExecForge); ok {
		s.forge.Forge = ExecForge{Timeout: to.Forge.Duration}
	}
	if _, ok := s.runner.(ExecRunner); ok {
		s.runner = ExecRunner{Timeout: to.Escalate.Duration}
	}
	switch _, isExec := s.api.(ExecForgeAPI); {
	case !next.Poll.GitHub && !next.Poll.GitLab:
		s.api = nil
	case s.api == nil || isExec:
		s.api = ExecForgeAPI{Timeout: to.Forge.Duration}
	}
	s.refreshRuntime()
}

// ConfigView is the config line status, the console and the ops view
// show: the hash a run holds and the file's, when they differ.
type ConfigView struct {
	Path        string   `json:"path"`
	Running     string   `json:"running"`
	File        string   `json:"file"`
	Drifted     bool     `json:"drifted"`
	Error       string   `json:"error,omitempty"`
	Refused     string   `json:"refused,omitempty"`
	RestartOnly []string `json:"restart_only,omitempty"`
	Removed     []string `json:"removed,omitempty"`
}

// configView is the config the state's run holds against the file: as
// the run last saw it, or, when cfg was loaded from the same file and
// holds neither what the run runs nor what it saw, as cfg has it, since
// agentd status read the file after the run did. A cfg that holds what
// the run runs may be the run's own, so the run's record stands. nil when
// the run recorded no config.
func configView(cfg *Config, st *LogState) *ConfigView {
	c := st.Config
	if c.Running == "" {
		return nil
	}
	v := &ConfigView{Path: c.Path, Running: c.Running, File: c.File, Error: c.Error, Refused: c.Refused, RestartOnly: c.RestartOnly, Removed: c.Removed}
	if cfg != nil && cfg.Hash != "" && cfg.Path == c.Path && cfg.Hash != c.File && cfg.Hash != c.Running {
		v.File, v.Error = cfg.Hash, "" // it loaded
	}
	if v.File == "" {
		v.File = v.Running
	}
	v.Drifted = v.File != v.Running
	if !v.Drifted {
		v.Error = ""
	}
	return v
}

// Line is the view as one line.
func (v ConfigView) Line() string {
	line := "config: running " + shortHash(v.Running)
	switch {
	case v.Drifted && v.Error != "":
		line += ", file " + shortHash(v.File) + " differs and does not load, so a reload would be refused: " + oneLine(v.Error, 300)
	case v.Drifted:
		line += ", file " + shortHash(v.File) + " differs (agentd config reload applies it)"
	default:
		line += ", file the same"
	}
	if len(v.RestartOnly) > 0 {
		line += "; only a restart applies " + strings.Join(v.RestartOnly, ", ")
	}
	if len(v.Removed) > 0 {
		line += "; no longer configured, still running: " + strings.Join(v.Removed, ", ")
	}
	return line
}

// Notice is the view's line when it says something the owner may act on:
// the file differs, a reload was refused, or one left fields to a restart
// or sessions it no longer configures.
func (v ConfigView) Notice() (string, bool) {
	if !v.Drifted && v.Refused == "" && len(v.RestartOnly) == 0 && len(v.Removed) == 0 {
		return "", false
	}
	line := v.Line()
	if v.Refused != "" && !v.Drifted {
		line += "; the last reload was refused: " + oneLine(v.Refused, 300)
	}
	return line, true
}

// RequestReload is agentd config reload: the file must load as at start,
// or nothing is recorded; then the owner's request is appended, which the
// running agentd applies at its next pass.
func RequestReload(path string, log *Log, now time.Time) (*Config, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	if err := compileAll(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.Path, err)
	}
	return cfg, log.Append(Event{V: EventVersion, Time: now, Kind: KindConfig, Mode: ModeOwner,
		Config: &ConfigPayload{Step: ConfigRequest, Path: cfg.Path, Hash: cfg.Hash}})
}

// SetSessionModel is agentd sessions model: the owner's model for one
// configured or started session, or its clearing ("" model). A model the
// session's repository's sessions.models leaves out is refused.
func SetSessionModel(cfg *Config, log *Log, events []Event, now time.Time, name, model string, now2 bool) error {
	all := projected(cfg, events)
	sess, ok := all.Sessions[name]
	if !ok {
		names := all.SessionNames()
		sort.Strings(names)
		return fmt.Errorf("no session %q is configured or started; the sessions are: %s", name, strings.Join(names, ", "))
	}
	if model != "" && sess.Repository != "" {
		st := NewLogState(cfg, ModeLive).Fold(events)
		p, _ := effectivePolicy(cfg, st, sess.Repository)
		if l := p.Sessions.Models; len(l) > 0 && !contains(l, model) {
			return fmt.Errorf("%s: repository %s allows only these models (sessions.models): %s", name, sess.Repository, strings.Join(l, ", "))
		}
	}
	return log.Append(Event{V: EventVersion, Time: now, Session: name, Kind: KindModel, Mode: ModeOwner, Model: &ModelPayload{Model: model, Now: now2 && model != ""}})
}

// modelChange is the model a live session should be relaunched on, and
// whether at the next pass (the owner's --now): its wanted model when a
// launch recorded another, or the owner's model for it when no launch
// recorded one.
func (s *Supervisor) modelChange(name string, sess Session, t *Track) (string, bool, bool) {
	o, set := s.st.Models[name]
	want := sessionModel(s.cfg, s.st, name, sess)
	due := t.ModelKnown && t.Model != want || !t.ModelKnown && set
	if !due {
		return "", false, false
	}
	return want, true, set && o.Now && o.At.After(t.Launched)
}

// modelMark is the mark a model relaunch at the next pass is recorded at:
// once per owner's setting.
func modelMark(model string, o ModelOverride) string {
	return fmt.Sprintf("model:%s:%d", model, o.At.UnixNano())
}
