package main

import (
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"time"
)

// The operations console (#63): the owner talks to an agent that runs
// agentd itself. One agentd process, the one whose config has the ops
// block, serves the ops console for every agentd process on the host:
// its supervisors and the page, each by its config and its tmux session.
// The ops console's thread agent reads them through Go-executed tools,
// and proposes changes through others; a proposed change is a decision
// entry of the ops console, showing the exact change, which Go carries
// out only when the owner answers yes on that entry, posting the outcome
// under it. Nothing the agent types is ever run: every read and every
// change is Go's own code.

// OpsConfig is the ops block: the console's name, every agentd process
// on the host by name, and the agentd-kit clone an install builds from.
// Binary is the agentd an install replaces, by default the running one.
type OpsConfig struct {
	Console     string                   `json:"console,omitempty"`
	Supervisors map[string]OpsSupervisor `json:"supervisors,omitempty"`
	Repo        string                   `json:"repo,omitempty"`
	Binary      string                   `json:"binary,omitempty"`
	// Branch is the branch of Repo's origin installs come from, main by
	// default; Watch names the configured repository whose forge reports
	// its CI, and when set Go proposes the install of each tip whose CI
	// is green, which AutoInstall carries out without a yes (#68).
	Branch      string `json:"branch,omitempty"`
	Watch       string `json:"watch,omitempty"`
	AutoInstall bool   `json:"auto_install,omitempty"`
}

// branch is the branch installs come from.
func (o OpsConfig) branch() string {
	if o.Branch == "" {
		return "main"
	}
	return o.Branch
}

// OpsSupervisor is one agentd process: the config it runs and the tmux
// session it runs in, restarted the way the owner does it, by Ctrl-C,
// Up and Enter.
type OpsSupervisor struct {
	Config string `json:"config"`
	Tmux   string `json:"tmux"`
}

// DefaultOpsConsole is the ops console's name when the block names none.
const DefaultOpsConsole = "ops"

// on says whether the config has an ops block.
func (o OpsConfig) on() bool { return len(o.Supervisors) > 0 || o.Repo != "" || o.Console != "" }

// checkOps expands the ops block's paths and checks it: at least one
// supervisor, each with a config file that exists and a plain tmux
// session name of its own, a clone to build from, and a console name no
// repository's console has. The console is listed as one without a
// repository, so the page shows it and it takes replies.
func (c *Config) checkOps(path func(field, p string) string, errs *[]error) {
	o := &c.Ops
	if !o.on() {
		return
	}
	if o.Console == "" {
		o.Console = DefaultOpsConsole
	}
	if !sessionName.MatchString(o.Console) {
		*errs = append(*errs, fmt.Errorf("ops.console %q is not a plain name", o.Console))
	}
	if len(o.Supervisors) == 0 {
		*errs = append(*errs, errors.New("ops.supervisors must name every agentd process on the host, the page included"))
	}
	tmux := map[string]string{}
	for _, name := range sortedKeys(o.Supervisors) {
		s := o.Supervisors[name]
		if !sessionName.MatchString(name) {
			*errs = append(*errs, fmt.Errorf("ops.supervisors: %q is not a plain name", name))
		}
		if s.Config == "" {
			*errs = append(*errs, fmt.Errorf("ops.supervisors.%s.config must be set", name))
		} else {
			s.Config = path("ops.supervisors."+name+".config", s.Config)
			if info, err := os.Stat(s.Config); err != nil || info.IsDir() {
				*errs = append(*errs, fmt.Errorf("ops.supervisors.%s.config: no file %s", name, s.Config))
			}
		}
		if !plainName.MatchString(s.Tmux) {
			*errs = append(*errs, fmt.Errorf("ops.supervisors.%s.tmux %q is not a plain tmux session name", name, s.Tmux))
		} else if other, ok := tmux[s.Tmux]; ok {
			*errs = append(*errs, fmt.Errorf("ops.supervisors.%s.tmux: %s runs in tmux session %s already", name, other, s.Tmux))
		}
		tmux[s.Tmux] = name
		o.Supervisors[name] = s
	}
	if o.Repo == "" {
		*errs = append(*errs, errors.New("ops.repo must name the agentd-kit clone to build from"))
	} else {
		o.Repo = path("ops.repo", o.Repo)
		if info, err := os.Stat(filepath.Join(o.Repo, ".git")); err != nil || info == nil {
			*errs = append(*errs, fmt.Errorf("ops.repo: %s is not a git clone", o.Repo))
		}
	}
	if o.Binary != "" {
		o.Binary = path("ops.binary", o.Binary)
	}
	if !plainBranch(o.branch()) {
		*errs = append(*errs, fmt.Errorf("ops.branch %q is not a plain branch name", o.Branch))
	}
	if r, ok := c.Repositories[o.Watch]; o.Watch != "" && (!ok || r.Project == "") {
		*errs = append(*errs, fmt.Errorf("ops.watch %q names no configured repository with a forge project", o.Watch))
	}
	if o.AutoInstall && o.Watch == "" {
		*errs = append(*errs, errors.New("ops.auto_install needs ops.watch, the repository whose CI says a build is green"))
	}
	for repo := range c.Repositories {
		if repo == o.Console || c.RepositoryConsole(repo) == o.Console {
			*errs = append(*errs, fmt.Errorf("ops.console: repository %s's console has the name %s", repo, o.Console))
		}
	}
	if c.Consoles == nil {
		c.Consoles = map[string]ConsoleConfig{}
	}
	if _, ok := c.Consoles[o.Console]; !ok {
		c.Consoles[o.Console] = ConsoleConfig{Title: "Operations"}
	}
}

// opsConsole is the ops console's name, "" when the config has no ops
// block.
func (c *Config) opsConsole() string {
	if !c.Ops.on() {
		return ""
	}
	return c.Ops.Console
}

// KindOps is a change to agentd proposed on the ops console, or a step
// Go took with it.
const KindOps = "ops"

// The changes the ops agent may propose.
const (
	OpsConfigSet = "config_set"
	OpsRestart   = "restart"
	OpsInstall   = "install"
	OpsClear     = "clear"
	OpsEnd       = "end"
	OpsRollback  = "rollback"
)

// The steps of a change: proposed by the agent's tool; started by the
// run that carries it out, once, after the owner's yes; then done or
// failed with its outcome; or declined on the owner's no.
const (
	OpsProposed = "proposed"
	OpsStarted  = "started"
	OpsDone     = "done"
	OpsFailed   = "failed"
	OpsDeclined = "declined"
)

// entryOps is the entry id prefix of a proposed change: o:ID.
const entryOps = "o:"

// OpsChange is one change exactly as the owner is shown it: what it does,
// to which supervisor, and the diff, commit or session it names. Base and
// Result are the config file's hashes before and after a config set: the
// file it is carried out on must be the one it was shown against.
type OpsChange struct {
	Op         string          `json:"op"`
	Supervisor string          `json:"supervisor,omitempty"`
	Pointer    string          `json:"pointer,omitempty"`
	Value      json.RawMessage `json:"value,omitempty"`
	Delete     bool            `json:"delete,omitempty"`
	Base       string          `json:"base,omitempty"`
	Result     string          `json:"result,omitempty"`
	Diff       string          `json:"diff,omitempty"`
	Commit     string          `json:"commit,omitempty"`
	Kept       string          `json:"kept,omitempty"` // a rollback's kept binary, agentd.SHA
	Session    string          `json:"session,omitempty"`
	Summary    string          `json:"summary"`
	Why        string          `json:"why,omitempty"`
	Thread     string          `json:"thread,omitempty"` // the instruction whose agent proposed it
	// Auto marks a restart of a supervisor that was down when it was
	// proposed: it needs no yes, while the supervisor is still down.
	Auto bool `json:"auto,omitempty"`
}

// OpsPayload is a change's step: its id, the change when proposed, the
// owner's instruction that answered it, and the outcome.
type OpsPayload struct {
	ID          string     `json:"id"`
	Step        string     `json:"step"`
	Change      *OpsChange `json:"change,omitempty"`
	Instruction string     `json:"instruction,omitempty"`
	Outcome     string     `json:"outcome,omitempty"`
	// Swapped is the kept binary an install or a rollback put another in
	// place of, logged as the swap is made, so the next process can put
	// it back when this one stopped before the end.
	Swapped string `json:"swapped,omitempty"`
}

// check says what an ops event lacks.
func (p *OpsPayload) check() error {
	switch {
	case p.ID == "":
		return errors.New("an ops event names its change")
	case p.Step == OpsProposed && (p.Change == nil || p.Change.Op == "" || p.Change.Summary == ""):
		return errors.New("a proposed change carries what it does")
	case p.Step != OpsProposed && p.Step != OpsStarted && p.Step != OpsDone && p.Step != OpsFailed && p.Step != OpsDeclined:
		return fmt.Errorf("ops step %q is not proposed, started, done, failed or declined", p.Step)
	}
	return nil
}

// OpsRecord is a change as the log folds it: proposed when, answered yes
// or no by which instruction of the owner's, and what Go did.
type OpsRecord struct {
	OpsChange
	ID       string
	Proposed time.Time
	// Seq is its place among the proposals, in the log's order: two
	// proposed at one time are carried out in the order they came.
	Seq        int
	Answer     string // "yes" or "no", the owner's first answer on its entry
	AnsweredBy string // that instruction's id
	AnsweredAt time.Time
	Started    time.Time
	// Restarting is what was done when the process carrying it out
	// restarted itself to finish it: the next one reports it done.
	Restarting string
	Swapped    string
	Ended      time.Time
	Outcome    string
	Failed     bool
}

// open says whether the change waits on the owner.
func (r *OpsRecord) open() bool { return r.Answer == "" && r.Started.IsZero() && r.Ended.IsZero() }

// applyOps folds a change's step.
func (st *LogState) applyOps(e Event) {
	p := e.Ops
	if st.Ops == nil {
		st.Ops = map[string]*OpsRecord{}
	}
	r := st.Ops[p.ID]
	if p.Step == OpsProposed {
		if r == nil {
			st.Ops[p.ID] = &OpsRecord{OpsChange: *p.Change, ID: p.ID, Proposed: e.Time, Seq: len(st.Ops)}
		}
		return
	}
	if r == nil {
		return
	}
	switch p.Step {
	case OpsStarted:
		if r.Started.IsZero() {
			r.Started = e.Time
		}
		if p.Outcome != "" {
			r.Restarting = p.Outcome
		}
		if p.Swapped != "" {
			r.Swapped = p.Swapped
		}
	case OpsDone, OpsFailed, OpsDeclined:
		if r.Ended.IsZero() {
			r.Ended, r.Outcome, r.Failed = e.Time, p.Outcome, p.Step == OpsFailed
		}
	}
}

// opsAnswer folds the owner's answer to a change, from an instruction:
// only a yes or a no given on the change's own entry, from the page, in
// the ops console, and only the first; any later one changes nothing.
func (st *LogState) opsAnswer(in *Instruction, at time.Time) {
	// A withheld instruction is refused as any is: it settles nothing.
	if in.Re == nil || in.Answer == nil || in.Answer.Answer == nil || in.From != "ui" || in.Withheld > 0 {
		return
	}
	id, ok := strings.CutPrefix(in.Re.ID, entryOps)
	if !ok || st.cfg == nil || in.Console == "" || in.Console != st.cfg.opsConsole() {
		return
	}
	r := st.Ops[id]
	if r == nil || r.Answer != "" {
		return
	}
	r.Answer, r.AnsweredBy, r.AnsweredAt = "no", in.ID, at
	if *in.Answer.Answer {
		r.Answer = "yes"
	}
}

// opsID is a new change's id: o and a hash of when and what.
func opsID(now time.Time, c OpsChange) string {
	b, _ := json.Marshal(c)
	return "o" + shortHash(HashPane(now.UTC().Format(time.RFC3339Nano) + "\n" + string(b)))[:10]
}

// fileHash is the hex SHA-256 of b.
func fileHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// buildCommit is the commit this binary was built from, as Go stamped
// it, or "".
func buildCommit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return vcsRevision(info.Settings)
}

// binaryCommit is the commit the binary at path was built from, or "".
func binaryCommit(path string) string {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return ""
	}
	return vcsRevision(info.Settings)
}

func vcsRevision(settings []debug.BuildSetting) string {
	rev, modified := "", false
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev != "" && modified {
		return rev + "+modified"
	}
	return rev
}

// short is a commit as a line names it.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// redactedKeys are the config keys whose values ops tools never show:
// credentials and the paths to them.
var redactedKeys = regexp.MustCompile(`(?i)(^|_)token(_file)?$|secret|password|credential|config_dir|api_key`)

// hasRedactedKey says whether v, a decoded JSON value, holds a redacted
// key at any depth.
func hasRedactedKey(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if redactedKeys.MatchString(k) || hasRedactedKey(val) {
				return true
			}
		}
	case []any:
		for _, val := range x {
			if hasRedactedKey(val) {
				return true
			}
		}
	}
	return false
}

// redactValue is v, a decoded config, with every redacted key's value
// replaced and every string that matches a confidential pattern withheld.
func redactValue(v any, secret func(...string) bool) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			if redactedKeys.MatchString(k) {
				if s, ok := val.(string); ok && s == "" {
					out[k] = ""
				} else {
					out[k] = "(redacted)"
				}
				continue
			}
			out[k] = redactValue(val, secret)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = redactValue(val, secret)
		}
		return out
	case string:
		if secret(x) {
			return withheldMessage
		}
	}
	return v
}

// redactLines is text, a diff or a pane's lines, with the value of each
// redacted key cut, the lines of a value that spans several among it,
// and each line that matches a confidential pattern withheld.
var keyLine = regexp.MustCompile(`^([-+ ]?\s*"([^"]*)"\s*:)\s*(.*)$`)

func redactLines(text string, secret func(...string) bool) string {
	lines := strings.Split(text, "\n")
	depth := 0 // the brackets of a redacted value still open
	for i, l := range lines {
		if depth > 0 {
			depth += strings.Count(l, "{") + strings.Count(l, "[") - strings.Count(l, "}") - strings.Count(l, "]")
			if depth > 0 {
				lines[i] = "  (redacted)"
				continue
			}
			depth = 0
			continue
		}
		if m := keyLine.FindStringSubmatch(l); m != nil && redactedKeys.MatchString(m[2]) {
			lines[i] = m[1] + ` "(redacted)"`
			rest := strings.TrimRight(m[3], ", ")
			if strings.HasSuffix(rest, "{") || strings.HasSuffix(rest, "[") {
				depth = 1
			}
			continue
		}
		if secret(l) {
			lines[i] = withheldMessage
		}
	}
	return strings.Join(lines, "\n")
}

// names are the ops block's supervisors, sorted.
func (o OpsConfig) names() []string { return sortedKeys(o.Supervisors) }
