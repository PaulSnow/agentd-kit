package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// opsTmux is a fake tmux of the supervisors' sessions: each runs agentd
// or a shell; Ctrl-C stops agentd, and Up then Enter starts it again,
// unless it is set to fail to start.
type opsTmux struct {
	mu      sync.Mutex
	command map[string]string
	screen  map[string]string
	noStart map[string]bool
	keys    []string
	up      map[string]bool // Up was the last key
}

func newOpsTmux() *opsTmux {
	return &opsTmux{command: map[string]string{}, screen: map[string]string{}, noStart: map[string]bool{}, up: map[string]bool{}}
}

func (f *opsTmux) HasSession(name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.command[name]
	return ok, nil
}

func (f *opsTmux) CapturePane(name string, lines int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.screen[name], nil
}

func (f *opsTmux) Pane(name string) (PaneInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return PaneInfo{Command: f.command[name]}, nil
}

func (f *opsTmux) Keys(name string, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range keys {
		f.keys = append(f.keys, name+" "+k)
		switch {
		case k == "C-c":
			f.command[name] = "bash"
		case k == "Enter" && f.up[name] && !f.noStart[name]:
			f.command[name] = "agentd"
		}
		f.up[name] = k == "Up"
	}
	return nil
}

func (f *opsTmux) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.keys...)
}

func (f *opsTmux) NewSession(name, cwd, command string, width, height int) error {
	return errors.New("no new sessions")
}
func (f *opsTmux) Respawn(name, cwd, command string) error { return errors.New("no respawn") }
func (f *opsTmux) Replace(name, cwd, command string) error { return errors.New("no replace") }
func (f *opsTmux) Stop(name string) error                  { return errors.New("no stop") }
func (f *opsTmux) Type(name, text string) error            { return errors.New("no typing") }

// opsH is an ops-serving agentd over files: live.json, which has the ops
// block and serves the console, and page.json, the page's; each in its
// tmux session; a clone with an origin whose main has a commit; and an
// installed binary, a script that passes agentd config.
type opsH struct {
	*harness
	t      *testing.T
	tmux   *opsTmux
	env    *opsEnv
	clone  string
	binary string
	built  string // what the fake build writes: a script's body
	builds int
	selfs  []string
}

const opsPassing = "#!/bin/sh\nexit 0\n"

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=agentd test", "-c", "user.email=test@example.invalid", "-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeOps(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

func newOpsH(t *testing.T) *opsH {
	t.Helper()
	h := &opsH{harness: newHarness(t, false), t: t, tmux: newOpsTmux(), built: opsPassing}
	dir := h.dir
	origin := filepath.Join(dir, "origin.git")
	gitT(t, dir, "init", "--quiet", "--bare", origin)
	h.clone = filepath.Join(dir, "clone")
	gitT(t, dir, "clone", "--quiet", origin, h.clone)
	writeOps(t, filepath.Join(h.clone, "README"), "widgets\n", 0o644)
	gitT(t, h.clone, "add", "README")
	gitT(t, h.clone, "commit", "--quiet", "-m", "First")
	gitT(t, h.clone, "push", "--quiet", "origin", "HEAD:main")
	gitT(t, h.clone, "fetch", "--quiet", "origin")
	h.binary = filepath.Join(dir, "bin", "agentd")
	writeOps(t, h.binary, opsPassing, 0o755)
	if err := os.MkdirAll(filepath.Join(dir, "w1", "wt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeOps(t, filepath.Join(dir, "page.json"), `{
  "event_log": "page.jsonl",
  "consoles": {"infra": {"sessions": ["*"]}},
  "sessions": {
    "w1": {"workdir": "w1", "cwd": "w1/wt"}
  },
  "policies": {
    "nudge": {"max": 2}
  }
}
`, 0o600)
	writeOps(t, filepath.Join(dir, "live.json"), `{
  "event_log": "live.jsonl",
  "ops": {
    "supervisors": {
      "live": {"config": "live.json", "tmux": "agentd-live"},
      "page": {"config": "page.json", "tmux": "agentd-page"}
    },
    "repo": "clone",
    "binary": "bin/agentd"
  }
}
`, 0o600)
	cfg, err := LoadConfig(filepath.Join(dir, "live.json"))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	h.tmux.command["agentd-live"] = "agentd"
	h.tmux.command["agentd-page"] = "agentd"
	h.env = &opsEnv{cfg: cfg, tmux: h.tmux, wait: 50 * time.Millisecond, poll: time.Millisecond, sleep: func(time.Duration) {}, now: h.clock.Now,
		binary: h.binary, self: "live",
		git: func(dir string, args ...string) (string, error) {
			out, err := runTimed(time.Minute, dir, nil, true, "git", args...)
			if err != nil {
				return "", errors.New(strings.TrimSpace(string(out)))
			}
			return strings.TrimSpace(string(out)), nil
		},
		build: func(ctx context.Context, repo, sha, out string) error {
			h.builds++
			return os.WriteFile(out, []byte(h.built), 0o755)
		},
		accounts:    func([]*Config) []opsAccount { return nil },
		selfRestart: func(tmux string) error { h.selfs = append(h.selfs, tmux); return nil }}
	h.restartOps()
	return h
}

// restartOps is a new agentd over the same log, as after a restart.
func (h *opsH) restartOps() {
	h.clock.advance(time.Second)
	h.restart(false)
	h.sup.ops = h.env
	h.sup.emit("", Event{Kind: KindStart, Start: &StartPayload{}})
	h.clock.advance(time.Second)
}

// server is the ops agent's tool server over the log.
func (h *opsH) server() *MCPServer {
	read := func() ([]Event, error) { return h.events(), nil }
	srv, err := NewOpsServer(h.cfg, "thread:i1", NewLogWriter(h.buf), read, h.clock.Now)
	if err != nil {
		h.t.Fatal(err)
	}
	srv.ops = h.env
	return srv
}

// call calls tool with args on a server, and decodes its reply.
func (h *opsH) call(tool string, args string) (map[string]any, bool) {
	h.t.Helper()
	text, refused := h.server().call(tool, json.RawMessage(args))
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		h.t.Fatalf("%s's reply %q: %v", tool, text, err)
	}
	return out, refused
}

// propose proposes a change and returns its entry id.
func (h *opsH) propose(tool, args string) string {
	h.t.Helper()
	out, refused := h.call(tool, args)
	if refused {
		h.t.Fatalf("%s refused: %v", tool, out["refused"])
	}
	return out["proposed"].(string)
}

// page is the page's source over the log.
func (h *opsH) page() *uiSource {
	return newUISource("live", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a")
}

// answer is the owner's yes or no on entry id, from the page.
func (h *opsH) answer(id string, yes bool) error {
	return h.page().respond("ops", id, "", "", &AnswerPayload{Answer: &yes}, nil)
}

func (h *opsH) record(id string) *OpsRecord {
	h.t.Helper()
	r := h.logState().Ops[strings.TrimPrefix(id, entryOps)]
	if r == nil {
		h.t.Fatalf("no change %s", id)
	}
	return r
}

func (h *opsH) logState() *LogState {
	st := NewLogState(h.cfg, ModeLive)
	return st.Fold(h.events())
}

func (h *opsH) file(name string) string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, name))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

// The ops block is checked by agentd config: every supervisor with its
// config and a tmux session of its own, a clone, and a console no
// repository has; the console is listed as one without a repository.
func TestOpsConfigChecked(t *testing.T) {
	h := newOpsH(t)
	if _, ok := h.cfg.Consoles["ops"]; !ok || h.cfg.opsConsole() != "ops" || h.cfg.Ops.Supervisors["page"].Config != filepath.Join(h.dir, "page.json") {
		t.Fatalf("ops = %+v, consoles %+v", h.cfg.Ops, h.cfg.Consoles)
	}
	for _, c := range []struct{ block, want string }{
		{`{"repo": "clone"}`, "ops.supervisors must name"},
		{`{"supervisors": {"a": {"config": "nope.json", "tmux": "t-a"}}, "repo": "clone"}`, "no file"},
		{`{"supervisors": {"a": {"config": "page.json", "tmux": "t a"}}, "repo": "clone"}`, "not a plain tmux session name"},
		{`{"supervisors": {"a": {"config": "page.json", "tmux": "t"}, "b": {"config": "page.json", "tmux": "t"}}, "repo": "clone"}`, "runs in tmux session t already"},
		{`{"supervisors": {"a": {"config": "page.json", "tmux": "t"}}, "repo": "bin"}`, "is not a git clone"},
	} {
		writeOps(t, filepath.Join(h.dir, "bad.json"), `{"event_log": "bad.jsonl", "ops": `+c.block+`}`, 0o600)
		if _, err := LoadConfig(filepath.Join(h.dir, "bad.json")); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want %q", c.block, err, c.want)
		}
	}
}

// The read tools answer from the supervisors' configs, logs and panes:
// whether each is up, its sessions, a config with its credentials'
// paths redacted, a pane's last lines screened, a log's last events.
func TestOpsReads(t *testing.T) {
	h := newOpsH(t)
	h.tmux.command["agentd-page"] = "bash"
	h.tmux.screen["agentd-live"] = "agentd live: 1 sessions\nall well\n"
	out, refused := h.call(ToolOpsStatus, `{}`)
	b, _ := json.Marshal(out)
	if refused || !strings.Contains(string(b), `"name":"live"`) || !strings.Contains(string(b), `"up":true`) || !strings.Contains(string(b), `"pane":"bash"`) || !strings.Contains(string(b), `"name":"w1"`) {
		t.Fatalf("ops_status = %s", b)
	}
	writeOps(t, filepath.Join(h.dir, "tok"), "secret\n", 0o600)
	writeOps(t, filepath.Join(h.dir, "page.json"), `{"event_log": "page.jsonl", "accounts": {"a": {"token_file": "tok"}}}`, 0o600)
	out, refused = h.call(ToolOpsConfig, `{"supervisor": "page"}`)
	b, _ = json.Marshal(out)
	if refused || strings.Contains(string(b), filepath.Join(h.dir, "tok")) || !strings.Contains(string(b), `"token_file":"(redacted)"`) {
		t.Fatalf("ops_config = %s", b)
	}
	if out, refused = h.call(ToolOpsTail, `{"supervisor": "live", "lines": 5}`); refused || out["tail"] != "agentd live: 1 sessions\nall well" {
		t.Fatalf("ops_tail = %v", out)
	}
	if out, refused = h.call(ToolOpsConfig, `{"supervisor": "nope"}`); !refused {
		t.Fatalf("an unknown supervisor was read: %v", out)
	}
}

// A config set that fails the check agentd config makes is not written:
// refused when proposed, and when the file no longer passes with it by
// the owner's yes, failed and nothing written.
func TestOpsConfigSetFailingValidationNotWritten(t *testing.T) {
	h := newOpsH(t)
	before := h.file("page.json")
	out, refused := h.call(ToolOpsConfigSet, `{"supervisor": "page", "pointer": "/poll_interval", "value": "-5m", "why": "poll faster"}`)
	if !refused || !strings.Contains(out["refused"].(string), "would not pass agentd config") || h.count(KindOps, "", "") != 0 || h.file("page.json") != before {
		t.Fatalf("an invalid set was proposed: %v", out)
	}
	// A set that passes now, and fails by the yes: w1's workdir is gone.
	id := h.propose(ToolOpsConfigSet, `{"supervisor": "page", "pointer": "/policies/nudge/max", "value": 3, "why": "nudge more"}`)
	if err := os.RemoveAll(filepath.Join(h.dir, "w1")); err != nil {
		t.Fatal(err)
	}
	if err := h.answer(id, true); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	r := h.record(id)
	if !r.Failed || !strings.Contains(r.Outcome, "nothing was written") || h.file("page.json") != before || len(h.tmux.sent()) != 0 {
		t.Fatalf("change = %+v, keys %v", r, h.tmux.sent())
	}
	if m, _ := filepath.Glob(filepath.Join(h.dir, "page.json.bak-ops-*")); len(m) != 0 {
		t.Fatalf("a backup was written: %v", m)
	}
}

// A config set is shown as the diff of that one line, the file's other
// lines as they were; on the owner's yes it is written with a backup,
// the supervisor that runs it is restarted, and the outcome is posted
// under the yes.
func TestOpsConfigSetApproved(t *testing.T) {
	h := newOpsH(t)
	before := h.file("page.json")
	out, refused := h.call(ToolOpsConfigSet, `{"supervisor": "page", "pointer": "/policies/nudge/max", "value": 3, "why": "nudge more"}`)
	if refused {
		t.Fatal(out)
	}
	id := out["proposed"].(string)
	if d := out["diff"].(string); !strings.Contains(d, "-    \"nudge\": {\"max\": 2}\n+    \"nudge\": {\"max\": 3}\n") || strings.Count(d, "\n-") != 1 {
		t.Fatalf("diff = %q", d)
	}
	// The page shows it as an open yes-no decision with its diff.
	v, err := (&UI{sources: []*uiSource{h.page()}}).consoleView("ops", h.clock.Now())
	if err != nil || v == nil {
		t.Fatal(err)
	}
	var e *uiEntry
	for i := range v.Entries {
		if v.Entries[i].Entry == id {
			e = &v.Entries[i]
		}
	}
	if e == nil || !e.Open || e.Decision == nil || e.Decision.Type != DecisionYesNo || !strings.Contains(e.Decision.Context, "```diff") || !v.Ops {
		t.Fatalf("entry = %+v", e)
	}
	h.ticks(2, time.Minute)
	if h.file("page.json") != before || len(h.tmux.sent()) != 0 {
		t.Fatal("the change was made without a yes")
	}
	if err := h.answer(id, true); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	r := h.record(id)
	if r.Failed || r.Ended.IsZero() || !strings.Contains(h.file("page.json"), `"nudge": {"max": 3}`) || !strings.Contains(r.Outcome, "restarted page") {
		t.Fatalf("change = %+v\n%s", r, h.file("page.json"))
	}
	if got := strings.Join(h.tmux.sent(), ","); got != "agentd-page C-c,agentd-page Up,agentd-page Enter" {
		t.Fatalf("keys = %s", got)
	}
	m, _ := filepath.Glob(filepath.Join(h.dir, "page.json.bak-ops-*"))
	if len(m) != 1 {
		t.Fatalf("backups = %v", m)
	}
	if b, _ := os.ReadFile(m[0]); string(b) != before {
		t.Fatal("the backup is not the old file")
	}
	in := h.logState().Instructions[r.AnsweredBy]
	if in == nil || !strings.HasPrefix(in.Reply, "Done: wrote page.json") || in.AgentLast.By != "agentd" {
		t.Fatalf("the yes was answered with %+v", in)
	}
	// A second yes on the entry is refused by the page.
	if err := h.answer(id, true); err == nil {
		t.Fatal("a second answer was taken")
	}
}

// A change with no yes is never carried out; a no declines it, and
// nothing is done.
func TestOpsNoYesNeverExecutes(t *testing.T) {
	h := newOpsH(t)
	id := h.propose(ToolOpsRestart, `{"supervisor": "page", "why": "it is stuck"}`)
	h.ticks(3, time.Minute)
	if r := h.record(id); !r.open() || len(h.tmux.sent()) != 0 {
		t.Fatalf("change = %+v, keys %v", r, h.tmux.sent())
	}
	if err := h.answer(id, false); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	if r := h.record(id); r.Answer != "no" || r.Ended.IsZero() || r.Failed || len(h.tmux.sent()) != 0 {
		t.Fatalf("change = %+v, keys %v", r, h.tmux.sent())
	}
	// An answer in the owner's own words settles nothing: it goes to the
	// agent, and the change stays as it was.
	id2 := h.propose(ToolOpsRestart, `{"supervisor": "page", "why": "still stuck"}`)
	if err := h.page().respond("ops", id2, "", "why?", &AnswerPayload{}, nil); err != nil {
		t.Fatal(err)
	}
	if r := h.record(id2); !r.open() {
		t.Fatalf("words settled the change: %+v", r)
	}
}

// A yes to one entry runs that entry's change and no other's.
func TestOpsYesRunsOnlyItsEntry(t *testing.T) {
	h := newOpsH(t)
	restart := h.propose(ToolOpsRestart, `{"supervisor": "page", "why": "it is stuck"}`)
	set := h.propose(ToolOpsConfigSet, `{"supervisor": "page", "pointer": "/policies/nudge/max", "value": 4, "why": "nudge more"}`)
	if err := h.answer(set, true); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	if r := h.record(restart); !r.open() {
		t.Fatalf("the restart was settled by the set's yes: %+v", r)
	}
	if r := h.record(set); r.Ended.IsZero() || r.Failed {
		t.Fatalf("set = %+v", r)
	}
	// One restart, the set's own; none for the restart's entry.
	if got := strings.Join(h.tmux.sent(), ","); got != "agentd-page C-c,agentd-page Up,agentd-page Enter" {
		t.Fatalf("keys = %s", got)
	}
	// An instruction that names the restart's entry but answers nothing
	// is not a yes either.
	if err := h.page().respond("ops", restart, "", "yes please", nil, nil); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	if r := h.record(restart); !r.open() || len(h.tmux.sent()) != 3 {
		t.Fatalf("words ran the restart: %+v", r)
	}
}

// A supervisor that is down is restarted without a yes: Up and Enter,
// and the outcome on its entry.
func TestOpsDownRestartNeedsNoYes(t *testing.T) {
	h := newOpsH(t)
	h.tmux.command["agentd-page"] = "bash"
	out, _ := h.call(ToolOpsRestart, `{"supervisor": "page", "why": "it is down"}`)
	id := out["proposed"].(string)
	if r := h.record(id); !r.Auto {
		t.Fatalf("change = %+v", r)
	}
	h.ticks(2, time.Minute)
	r := h.record(id)
	if r.Ended.IsZero() || r.Failed || r.Outcome != "page is up again" {
		t.Fatalf("change = %+v", r)
	}
	if got := strings.Join(h.tmux.sent(), ","); got != "agentd-page Up,agentd-page Enter" {
		t.Fatalf("keys = %s", got)
	}
	// One that is up needs the yes, as any change.
	id = h.propose(ToolOpsRestart, `{"supervisor": "page", "why": "again"}`)
	h.ticks(2, time.Minute)
	if r := h.record(id); r.Auto || !r.open() {
		t.Fatalf("change = %+v", r)
	}
}

// commitMain is the clone's main's tip.
func (h *opsH) commitMain() string { return gitT(h.t, h.clone, "rev-parse", "origin/main") }

// An install whose new binary fails agentd config is rolled back: the
// previous binary is put back, byte for byte, and kept; no supervisor is
// restarted on the new one.
func TestOpsInstallRolledBack(t *testing.T) {
	h := newOpsH(t)
	h.built = "#!/bin/sh\necho 'config: policies are wrong' >&2\nexit 1\n"
	id := h.propose(ToolOpsInstall, `{"commit": "`+h.commitMain()+`", "why": "the fixes on main"}`)
	if err := h.answer(id, true); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	r := h.record(id)
	if !r.Failed || !strings.Contains(r.Outcome, "agentd config failed") || !strings.Contains(r.Outcome, "is back") || h.builds != 1 {
		t.Fatalf("change = %+v, %d builds", r, h.builds)
	}
	if b, _ := os.ReadFile(h.binary); string(b) != opsPassing {
		t.Fatalf("the binary is %q", b)
	}
	if kept := h.env.keptBinaries(); len(kept) != 1 || !strings.HasPrefix(kept[0], "agentd.unknown-") {
		t.Fatalf("kept = %v", kept)
	}
	if len(h.tmux.sent()) != 0 || len(h.selfs) != 0 {
		t.Fatalf("restarted on the failed binary: %v %v", h.tmux.sent(), h.selfs)
	}
}

// An install that passes is swapped in, every supervisor restarted, this
// process last from a process of its own; the next agentd says it is up.
// A supervisor that does not come up on it rolls the install back.
func TestOpsInstall(t *testing.T) {
	h := newOpsH(t)
	h.built = "#!/bin/sh\n# the new one\nexit 0\n"
	out, refused := h.call(ToolOpsInstall, `{"commit": "`+h.commitMain()+`", "why": "the fixes on main"}`)
	if refused || !strings.Contains(out["diff"].(string), "First") {
		t.Fatalf("install = %v", out)
	}
	if _, refused := h.call(ToolOpsInstall, `{"commit": "0123456789ab", "why": "x"}`); !refused {
		t.Fatal("a commit the clone lacks was proposed")
	}
	id := out["proposed"].(string)
	if err := h.answer(id, true); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	if b, _ := os.ReadFile(h.binary); string(b) != h.built {
		t.Fatalf("the binary is %q", b)
	}
	r := h.record(id)
	if !r.Ended.IsZero() || !strings.Contains(r.Restarting, "restarted page") || len(h.selfs) != 1 || h.selfs[0] != "agentd-live" {
		t.Fatalf("change = %+v, self restarts %v", r, h.selfs)
	}
	h.restartOps()
	h.ticks(1, time.Minute)
	if r = h.record(id); r.Ended.IsZero() || r.Failed || !strings.Contains(r.Outcome, "live is up again") {
		t.Fatalf("change = %+v", r)
	}
	// The next install's page does not come up: the binary is put back.
	gitT(t, h.clone, "commit", "--quiet", "--allow-empty", "-m", "Second")
	gitT(t, h.clone, "push", "--quiet", "origin", "HEAD:main")
	h.built = "#!/bin/sh\n# the third\nexit 0\n"
	h.tmux.noStart["agentd-page"] = true
	before, _ := os.ReadFile(h.binary)
	id = h.propose(ToolOpsInstall, `{"commit": "`+h.commitMain()+`", "why": "more fixes"}`)
	if err := h.answer(id, true); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	if r = h.record(id); !r.Failed || !strings.Contains(r.Outcome, "is back") {
		t.Fatalf("change = %+v", r)
	}
	if b, _ := os.ReadFile(h.binary); !bytes.Equal(b, before) {
		t.Fatalf("the binary is %q", b)
	}
}

// A change the last agentd left started, with no restart of its own to
// finish it, is reported cut short, never carried out again.
func TestOpsCutShortNotRepeated(t *testing.T) {
	h := newOpsH(t)
	id := h.propose(ToolOpsRestart, `{"supervisor": "page", "why": "stuck"}`)
	if err := h.answer(id, true); err != nil {
		t.Fatal(err)
	}
	h.sup.emit("", Event{Kind: KindOps, Mode: ModeShared, Ops: &OpsPayload{ID: strings.TrimPrefix(id, entryOps), Step: OpsStarted}})
	h.restartOps()
	h.ticks(2, time.Minute)
	if r := h.record(id); !r.Failed || !strings.Contains(r.Outcome, "stopped while") || len(h.tmux.sent()) != 0 {
		t.Fatalf("change = %+v, keys %v", r, h.tmux.sent())
	}
}

// A message on the ops console goes to the ops agent: its own tools on
// agentd mcp --ops, Read in the clone, and the live facts of the
// supervisors; its answer is threaded under the message.
func TestOpsAgentAnswers(t *testing.T) {
	dir := fakeClaude(t)
	h := newOpsH(t)
	h.tmux.command["agentd-page"] = "bash"
	claudeAnswers(t, dir, 1, `{"type":"result","subtype":"success","is_error":false,"structured_output":{"answer":"The page is down; I proposed restarting it as o:x."},"usage":{"input_tokens":5,"output_tokens":50},"total_cost_usd":0.001}`+"\n")
	if err := RecordInstruction(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "ops", "Why is the page not answering?"); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	in := h.instruction(t)
	if !in.Agent || in.Reply != "The page is down; I proposed restarting it as o:x." {
		t.Fatalf("instruction = %+v", in)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args.1"))
	for _, want := range []string{`"--ops"`, "mcp__agentd__ops_config_set", "mcp__agentd__ask_expert", "Read(/" + h.clone + "/**)", "never say a change was made"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("the agent's arguments lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(string(args), "mcp__agentd__state") {
		t.Fatal("the ops agent was given the repository's context tools")
	}
	var x opsInput
	b, _ := os.ReadFile(filepath.Join(dir, "in.1"))
	if err := json.Unmarshal(b, &x); err != nil {
		t.Fatal(err)
	}
	up := map[string]bool{}
	for _, s := range x.Facts.Supervisors {
		up[s.Name] = s.Up
	}
	if x.Message.Text != "Why is the page not answering?" || len(up) != 2 || !up["live"] || up["page"] || x.Facts.Main == "" || x.Clone != h.clone {
		t.Fatalf("the agent's input = %s", b)
	}
}

// The edit of a config's text changes that value alone: an existing
// one, a member not there yet, a nested one, an append and a delete,
// each keeping the other lines as written.
func TestEditJSONKeepsTheText(t *testing.T) {
	src := "{\n  \"b\": 1,\n  \"a\": {\"x\": [1, 2]},\n  \"c\": {\n    \"d\": \"e\"\n  }\n}\n"
	for _, c := range []struct {
		ptr, value string
		del        bool
		want       string
	}{
		{"/b", "2", false, "{\n  \"b\": 2,\n  \"a\": {\"x\": [1, 2]},\n  \"c\": {\n    \"d\": \"e\"\n  }\n}\n"},
		{"/c/f", `{"g": true}`, false, "{\n  \"b\": 1,\n  \"a\": {\"x\": [1, 2]},\n  \"c\": {\n    \"d\": \"e\",\n    \"f\": {\n      \"g\": true\n    }\n  }\n}\n"},
		{"/z/y", "3", false, "{\n  \"b\": 1,\n  \"a\": {\"x\": [1, 2]},\n  \"c\": {\n    \"d\": \"e\"\n  },\n  \"z\": {\n    \"y\": 3\n  }\n}\n"},
		{"/a/x/-", "3", false, "{\n  \"b\": 1,\n  \"a\": {\"x\": [1, 2, 3]},\n  \"c\": {\n    \"d\": \"e\"\n  }\n}\n"},
		{"/b", "", true, "{\n  \"a\": {\"x\": [1, 2]},\n  \"c\": {\n    \"d\": \"e\"\n  }\n}\n"},
		{"/c/d", "", true, "{\n  \"b\": 1,\n  \"a\": {\"x\": [1, 2]},\n  \"c\": {}\n}\n"},
		{"/a/x/0", "", true, "{\n  \"b\": 1,\n  \"a\": {\"x\": [2]},\n  \"c\": {\n    \"d\": \"e\"\n  }\n}\n"},
	} {
		got, err := setConfigText([]byte(src), c.ptr, json.RawMessage(c.value), c.del)
		if err != nil || string(got) != c.want {
			t.Fatalf("%s: err %v\n%s\nwant\n%s", c.ptr, err, got, c.want)
		}
	}
	for _, ptr := range []string{"/a/x/5", "/b/c", "", "/nope"} {
		if _, err := setConfigText([]byte(src), ptr, nil, true); err == nil {
			t.Fatalf("%q: deleted", ptr)
		}
	}
}
