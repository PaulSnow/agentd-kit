package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fileHarness is a live harness whose config is loaded from a file, as
// agentd run loads it, and whose run has started.
func fileHarness(t *testing.T, text string) (*harness, string) {
	t.Helper()
	h := newHarness(t, false)
	path := filepath.Join(h.dir, "agentd.json")
	writeFile(t, path, text)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	h.sup = NewSupervisor(cfg, NewLogWriter(h.buf), h.clock, h.tmux, h.git, h.forge, h.runner, false)
	h.sup.home = h.dir
	if err := h.sup.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	return h, path
}

// configSteps are the run's config events, as step:hash-prefix.
func (h *harness) configEvents() []ConfigPayload {
	var out []ConfigPayload
	for _, e := range h.events() {
		if e.Kind == KindConfig {
			out = append(out, *e.Config)
		}
	}
	return out
}

// statusText is agentd status over the harness's log, with cfg as status
// loaded it.
func (h *harness) statusText(cfg *Config) (string, ConfigView) {
	rows, meta := SummarizeAt(cfg, h.events(), ModeLive, h.clock.Now())
	var out strings.Builder
	PrintStatus(&out, rows, meta, h.clock.Now())
	v, _ := meta["config"].(ConfigView)
	return out.String(), v
}

// A changed file is drift until the owner asks for a reload; the reload
// swaps the config at the next pass's start, and a file that does not
// load is refused, the running config staying.
func TestConfigReloadAndDrift(t *testing.T) {
	a := `{"poll_interval": "1m", "policies": {"models": {"work": "sonnet"}}}`
	h, path := fileHarness(t, a)
	hashA := h.cfg.Hash
	if s := h.events()[0].Start; s == nil || s.ConfigHash != hashA || s.ConfigPath != path {
		t.Fatalf("start = %+v, want the config's path and hash %s", s, hashA)
	}
	h.ticks(2, time.Minute)
	if got := h.configEvents(); len(got) != 0 {
		t.Fatalf("config events with the file unchanged: %+v", got)
	}
	if text, v := h.statusText(h.cfg); v.Drifted || strings.Contains(text, "config:") || v.Running != hashA {
		t.Fatalf("status before a change: %+v\n%s", v, text)
	}

	// Changed: drift, once per hash, shown by status.
	b := `{"poll_interval": "2m", "policies": {"models": {"work": "opus"}}}`
	writeFile(t, path, b)
	fresh, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	hashB := fresh.Hash
	// agentd status, loading the file itself, sees it before the run does.
	if text, v := h.statusText(fresh); !v.Drifted || v.File != hashB || !strings.Contains(text, "config: running "+shortHash(hashA)+", file "+shortHash(hashB)+" differs (agentd config reload applies it)") {
		t.Fatalf("status from the file: %+v\n%s", v, text)
	}
	h.ticks(3, time.Minute)
	got := h.configEvents()
	if len(got) != 1 || got[0].Step != ConfigDrift || got[0].Hash != hashB || got[0].Previous != hashA || got[0].Error != "" {
		t.Fatalf("drift events = %+v", got)
	}
	if text, v := h.statusText(h.sup.cfg); !v.Drifted || !strings.Contains(text, "differs (agentd config reload applies it)") {
		t.Fatalf("status from the run's record: %+v\n%s", v, text)
	}
	if h.sup.cfg.PollInterval.Duration != time.Minute {
		t.Fatal("the drift was applied without a reload")
	}

	// agentd config reload: applied at the next pass.
	if _, err := RequestReload(path, NewLogWriter(h.buf), h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, time.Minute)
	got = h.configEvents()
	last := got[len(got)-1]
	if last.Step != ConfigApplied || !last.Applied || last.Hash != hashB || last.Previous != hashA || last.Signal {
		t.Fatalf("reload = %+v", last)
	}
	if h.sup.cfg.PollInterval.Duration != 2*time.Minute || h.sup.cfg.Policies.Models.Work != "opus" || h.sup.st.cfg != h.sup.base {
		t.Fatalf("running config not swapped: every %s, work model %q", h.sup.cfg.PollInterval, h.sup.cfg.Policies.Models.Work)
	}
	if text, v := h.statusText(h.sup.cfg); v.Drifted || strings.Contains(text, "config:") || v.Running != hashB {
		t.Fatalf("status after the reload: %+v\n%s", v, text)
	}
	h.ticks(2, time.Minute)
	if n := len(h.configEvents()); n != len(got) {
		t.Fatalf("reloaded again, or drift logged, with nothing new: %+v", h.configEvents()[len(got):])
	}

	// A file that does not load: drift with its error; agentd config
	// reload refuses it and records nothing; SIGHUP's reload is refused
	// and the running config stays.
	writeFile(t, path, `{"poll_interval": "5m", "no_such_key": 1}`)
	h.ticks(1, time.Minute)
	got = h.configEvents()
	if last := got[len(got)-1]; last.Step != ConfigDrift || last.Error == "" || last.Previous != hashB {
		t.Fatalf("drift to a bad file = %+v", last)
	}
	if text, _ := h.statusText(h.sup.cfg); !strings.Contains(text, "does not load") || !strings.Contains(text, "no_such_key") {
		t.Fatalf("status with a bad file:\n%s", text)
	}
	before := len(h.events())
	if _, err := RequestReload(path, NewLogWriter(h.buf), h.clock.Now()); err == nil || !strings.Contains(err.Error(), "no_such_key") {
		t.Fatalf("reload of a bad file: %v", err)
	}
	if n := len(h.events()); n != before {
		t.Fatalf("a refused reload request appended %d events", n-before)
	}
	h.sup.SignalReload()
	h.ticks(1, time.Minute)
	got = h.configEvents()
	if last := got[len(got)-1]; last.Step != ConfigRefused || !last.Signal || !strings.Contains(last.Error, "no_such_key") || last.Applied {
		t.Fatalf("SIGHUP reload of a bad file = %+v", last)
	}
	if h.sup.cfg.PollInterval.Duration != 2*time.Minute || h.sup.st.Config.Running != hashB {
		t.Fatalf("a refused reload changed the running config: every %s", h.sup.cfg.PollInterval)
	}

	// Back to the running file: the drift clears.
	writeFile(t, path, b)
	h.ticks(1, time.Minute)
	got = h.configEvents()
	if last := got[len(got)-1]; last.Step != ConfigDrift || last.Hash != hashB {
		t.Fatalf("cleared drift = %+v", last)
	}
	if _, v := h.statusText(h.sup.cfg); v.Drifted {
		t.Fatalf("drift not cleared: %+v", v)
	}

	// A reload that changes the event log keeps the running one and says
	// only a restart applies it; replayed, the state holds the same.
	running := h.sup.cfg.EventLog
	writeFile(t, path, `{"poll_interval": "3m", "event_log": "elsewhere.jsonl", "policies": {"models": {"work": "opus"}}}`)
	h.sup.SignalReload()
	h.ticks(1, time.Minute)
	got = h.configEvents()
	last = got[len(got)-1]
	if last.Step != ConfigApplied || !reflect.DeepEqual(last.RestartOnly, []string{"event_log"}) {
		t.Fatalf("reload with a new event log = %+v", last)
	}
	if h.sup.cfg.EventLog != running || h.sup.cfg.PollInterval.Duration != 3*time.Minute {
		t.Fatalf("event log %q (want %q kept), every %s", h.sup.cfg.EventLog, running, h.sup.cfg.PollInterval)
	}
	if text, _ := h.statusText(h.sup.cfg); !strings.Contains(text, "only a restart applies event_log") {
		t.Fatalf("status after a restart-only change:\n%s", text)
	}
	st := NewLogState(h.cfg, ModeLive).Fold(h.events())
	if st.Config.Running != last.Hash || st.Config.drifted() || !reflect.DeepEqual(st.Config.RestartOnly, []string{"event_log"}) {
		t.Fatalf("replayed config state = %+v", st.Config)
	}
}

// SIGHUP to agentd run asks for the same reload, through the handler run
// installs.
func TestSighupReloads(t *testing.T) {
	h, path := fileHarness(t, `{"poll_interval": "1m"}`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := reloadOnHangup(ctx, h.sup)
	defer stop()
	writeFile(t, path, `{"poll_interval": "4m"}`)
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !h.sup.work.hup.Load() {
		if time.Now().After(deadline) {
			t.Fatal("SIGHUP did not reach the run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.ticks(1, time.Minute)
	got := h.configEvents()
	if len(got) == 0 || got[0].Step != ConfigApplied || !got[0].Signal || h.sup.cfg.PollInterval.Duration != 4*time.Minute {
		t.Fatalf("config events = %+v; every %s", got, h.sup.cfg.PollInterval)
	}
}

// The owner's model for one session runs ahead of the policies', within
// the repository's sessions.models, and reaches that session's command
// and status row and no other's.
func TestSessionModelOverride(t *testing.T) {
	h := repoHarness(t)
	h.cfg.Policies.Restart.Command = []string{"claude", "--model", "{model}", "{prompt}"}
	h.cfg.Policies.Models = ModelsPolicy{Work: "sonnet"}
	log := NewLogWriter(h.buf)
	if err := SetSessionModel(h.cfg, log, h.events(), h.clock.Now(), "a", "opus", false); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, time.Minute)
	for name, want := range map[string]string{"a": "opus", "b": "sonnet", "d": "sonnet"} {
		cmd, _ := h.sup.launchCommand(name, h.cfg.Sessions[name], newTrack(), "id-1", "restart")
		if got := modelOf(cmd); got != want {
			t.Errorf("%s: command model %q, want %q", name, got, want)
		}
	}
	rows, _ := SummarizeState(h.cfg, h.sup.st)
	got := map[string][2]string{}
	for _, r := range rows {
		got[r.Name] = [2]string{r.Model, r.Wanted}
	}
	// a runs a launch agentd did not record a model for: unknown, until
	// its relaunch on opus.
	want := map[string][2]string{"a": {"unknown", "opus"}, "b": {"sonnet", ""}, "d": {"sonnet", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status models = %v, want %v", got, want)
	}

	// A model the repository's sessions.models leaves out is refused,
	// naming the list; so is an unknown session.
	w := h.cfg.Repositories["widgets"]
	w.Policy.Sessions.Models = []string{"sonnet", "haiku"}
	h.cfg.Repositories["widgets"] = w
	if err := SetSessionModel(h.cfg, log, h.events(), h.clock.Now(), "b", "opus", false); err == nil || !strings.Contains(err.Error(), "sonnet, haiku") {
		t.Fatalf("a model outside sessions.models: %v", err)
	}
	if err := SetSessionModel(h.cfg, log, h.events(), h.clock.Now(), "nope", "opus", false); err == nil || !strings.Contains(err.Error(), `no session "nope"`) {
		t.Fatalf("an unknown session: %v", err)
	}
	// The narrowing holds over an override set before it.
	h.restart(false)
	if m := sessionModel(h.cfg, h.sup.st, "a", h.cfg.Sessions["a"]); m != "sonnet" {
		t.Fatalf("narrowed override = %q, want sonnet", m)
	}
	// Cleared, the policies' runs again.
	w.Policy.Sessions.Models = nil
	h.cfg.Repositories["widgets"] = w
	if err := SetSessionModel(h.cfg, log, h.events(), h.clock.Now(), "a", "", false); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	if m := sessionModel(h.cfg, h.sup.st, "a", h.cfg.Sessions["a"]); m != "sonnet" {
		t.Fatalf("cleared override = %q, want sonnet", m)
	}
}

// A live session whose model changed is relaunched on it once, at its next
// checkpoint: a rotation, not a crash or a restart, whose launch records
// the model.
func TestModelChangeRelaunchesAtCheckpoint(t *testing.T) {
	h, id := contextHarness(t, false)
	tr := h.sup.Tracks()["s"]
	if !tr.ModelKnown || tr.Model != "" {
		t.Fatalf("launched model = %q, known %v; want the command's default, known", tr.Model, tr.ModelKnown)
	}
	h.transcript(id, usageLine("m1", 10, 1000, 0, 5, false))
	if err := SetSessionModel(h.cfg, NewLogWriter(h.buf), h.events(), h.clock.Now(), "s", "opus", false); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, time.Minute)
	if n := h.count(KindAction, ActRotate, "s"); n != 0 {
		t.Fatalf("relaunched with no checkpoint: %d", n)
	}
	rows, _ := SummarizeState(h.sup.cfg, h.sup.st)
	if r := rows[0]; r.Model != "default" || r.Wanted != "opus" || !strings.Contains(r.ModelText(), "wanted opus, relaunch pending") {
		t.Fatalf("pending row = %+v", r)
	}
	h.git.merge, h.git.mergeSubject = "m1", "Merge #21: contexts"
	h.ticks(6, time.Minute)
	if n := h.count(KindAction, ActRotate, "s"); n != 1 {
		t.Fatalf("relaunches = %d, want 1", n)
	}
	var rot *ActPayload
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActRotate {
			rot = e.Act
		}
	}
	if rot.Reason != "model" || rot.Model == nil || *rot.Model != "opus" || rot.Checkpoint != "m1" || !strings.Contains(rot.Command, "opus") {
		t.Fatalf("relaunch = %+v", rot)
	}
	tr = h.sup.Tracks()["s"]
	if h.count(KindCrash, "", "s") != 0 || tr.RestartCount != 1 || tr.Model != "opus" {
		t.Fatalf("crashes %d, restarts %d, model %q", h.count(KindCrash, "", "s"), tr.RestartCount, tr.Model)
	}
	rows, _ = SummarizeState(h.sup.cfg, h.sup.st)
	if r := rows[0]; r.Model != "opus" || r.Wanted != "" {
		t.Fatalf("row after the relaunch = %+v", r)
	}
	h.git.merge, h.git.mergeSubject = "m2", "Merge #22: more"
	h.ticks(6, time.Minute)
	if n := h.count(KindAction, ActRotate, "s"); n != 1 {
		t.Fatalf("relaunched again on the same model: %d", n)
	}
	// Replayed, the launched model stands.
	h.restart(false)
	if tr := h.sup.Tracks()["s"]; tr.Model != "opus" || !tr.ModelKnown {
		t.Fatalf("after replay: model %q, known %v", tr.Model, tr.ModelKnown)
	}
}

// With --now the relaunch is at the next pass, checkpoint or not, but
// never while the session waits on the owner.
func TestModelChangeNow(t *testing.T) {
	h, id := contextHarness(t, false)
	h.transcript(id, usageLine("m1", 10, 1000, 0, 5, false))
	h.status("s", "# s\n\n## Questions\n\nWhich way?\n")
	h.ticks(1, time.Minute)
	if err := SetSessionModel(h.cfg, NewLogWriter(h.buf), h.events(), h.clock.Now(), "s", "haiku", true); err != nil {
		t.Fatal(err)
	}
	h.ticks(3, time.Minute)
	if n := h.count(KindAction, ActRotate, "s"); n != 0 {
		t.Fatalf("relaunched a session waiting on the owner: %d", n)
	}
	h.status("s", "# s\n\nWorking.\n")
	h.ticks(2, time.Minute)
	if n := h.count(KindAction, ActRotate, "s"); n != 1 {
		t.Fatalf("relaunches = %d, want 1", n)
	}
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActRotate {
			if e.Act.Reason != "model" || !strings.HasPrefix(e.Act.Checkpoint, "model:haiku:") || *e.Act.Model != "haiku" {
				t.Fatalf("relaunch = %+v", e.Act)
			}
		}
	}
}

// The web console's home and the ops view show a run's config drift.
func TestConfigAlertOnConsole(t *testing.T) {
	st := NewLogState(nil, ModeLive)
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	st.apply(Event{V: EventVersion, Time: now, Kind: KindStart, Mode: ModeLive, Start: &StartPayload{ConfigPath: "/c.json", ConfigHash: "aaaaaaaaaaaa"}})
	src := &uiSource{Name: "run", st: st}
	if _, ok := src.configAlert(); ok {
		t.Fatal("an alert with no drift")
	}
	st.apply(Event{V: EventVersion, Time: now, Kind: KindConfig, Mode: ModeLive, Config: &ConfigPayload{Step: ConfigDrift, Path: "/c.json", Hash: "bbbbbbbbbbbb", Previous: "aaaaaaaaaaaa"}})
	a, ok := src.configAlert()
	if !ok || a.Src != "run" || !strings.Contains(a.Line, "file "+shortHash("bbbbbbbbbbbb")+" differs") {
		t.Fatalf("alert = %+v, %v", a, ok)
	}
}
