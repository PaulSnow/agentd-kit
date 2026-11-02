package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Outside schedules under agentd (#86): a job's own environment, its
// failure threshold, and a kept log of each run.

// jobEscalations are the escalations posted on job name's track.
func jobEscalations(h *harness, name string) []string {
	var out []string
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActEscalate && e.Session == jobTrack(name) {
			out = append(out, e.Act.Message)
		}
	}
	return out
}

// A job's env reaches its command, after agentd's own environment.
func TestScheduledJobEnv(t *testing.T) {
	t.Setenv("JOB_MODE", "inherited")
	h := schedHarness(t, false, map[string]ScheduledJob{
		"site": {Every: Duration{time.Hour}, Env: map[string]string{"JOB_MODE": "nightly", "PATH": "/opt/tools/bin:/usr/bin:/bin"},
			Argv: []string{"sh", "-c", "echo $JOB_MODE $PATH > env.out"}}})
	h.ticks(1, time.Minute)
	if got := readJobFile(t, h, "site", "env.out"); got != "nightly /opt/tools/bin:/usr/bin:/bin\n" {
		t.Fatalf("env.out = %q", got)
	}
}

// env refuses names that are not variables, names agentd drops or sets
// itself, secret-shaped names and confidential values; fail_after and keep
// refuse negatives.
func TestScheduledJobOutsideConfigRefused(t *testing.T) {
	job := func(f func(*ScheduledJob)) ScheduledJob {
		j := ScheduledJob{Every: Duration{time.Hour}, Argv: []string{"x"}}
		f(&j)
		return j
	}
	env := func(k, v string) ScheduledJob {
		return job(func(j *ScheduledJob) { j.Env = map[string]string{k: v} })
	}
	for _, c := range []struct {
		job  ScheduledJob
		want string
	}{
		{env("1BAD", "x"), `env: "1BAD" is not a variable name`},
		{env("A-B", "x"), `env: "A-B" is not a variable name`},
		{env("ANTHROPIC_API_KEY", "x"), `env: ANTHROPIC_API_KEY is set by agentd or kept from jobs`},
		{env("CLAUDE_CONFIG_DIR", "x"), "env: CLAUDE_CONFIG_DIR is set by agentd"},
		{env("AGENTD_TOKEN_FILE", "x"), "env: AGENTD_TOKEN_FILE is set by agentd"},
		{env("CLAUDE_CODE_OAUTH_TOKEN", "x"), "env: CLAUDE_CODE_OAUTH_TOKEN is set by agentd"},
		{env("CLAUDECODE", "1"), "env: CLAUDECODE is set by agentd"},
		{env("IMPORTER_API_KEY", "x"), "env: IMPORTER_API_KEY looks like a secret"},
		{env("GH_TOKEN", "x"), "env: GH_TOKEN looks like a secret"},
		{env("DB_PASSWORD", "x"), "env: DB_PASSWORD looks like a secret"},
		{env("MODE", "sk-live-0123"), "env.MODE matches a confidential pattern"},
		{job(func(j *ScheduledJob) { j.FailAfter = -1 }), "fail_after is negative"},
		{job(func(j *ScheduledJob) { j.Keep = -1 }), "keep is negative"},
		{job(func(j *ScheduledJob) { j.Argv, j.Step, j.Output = nil, "missing.json", "logs" }), "a step job takes no env, fail_after, output or keep"},
	} {
		h := newHarness(t, false)
		h.cfg.ConfidentialPatterns = []string{`sk-live-[0-9]+`}
		h.cfg.Jobs.Scheduled = map[string]ScheduledJob{"j": c.job}
		err := h.cfg.finish(h.dir)
		if err == nil || !strings.Contains(err.Error(), "jobs.scheduled.j") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.job, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "sk-live-0123") {
			t.Errorf("the refusal repeats the confidential value: %v", err)
		}
	}
}

// fail_after 1 escalates on the first failed run, once a streak, naming
// the command, its directory and account; a success ends the streak and
// the next failure escalates again.
func TestScheduledJobFailAfterOne(t *testing.T) {
	h := newHarness(t, false)
	h.cfg.Accounts = map[string]Account{"b": {ConfigDir: "/accounts/b"}}
	h = schedHarnessWith(t, h, map[string]ScheduledJob{
		"nightly": {Every: Duration{time.Minute}, FailAfter: 1, Account: "b",
			Argv: []string{"sh", "-c", "if [ -f ok ]; then exit 0; fi; echo broken >&2; exit 3"}}})
	h.ticks(1, time.Minute)
	if got := jobEscalations(h, "nightly"); len(got) != 0 {
		t.Fatalf("escalated before the run ended: %q", got)
	}
	h.ticks(1, time.Minute) // the pass after the first failure
	got := jobEscalations(h, "nightly")
	if len(got) != 1 || !strings.Contains(got[0], "nightly failed 1 time in a row") || !strings.Contains(got[0], "broken") ||
		!strings.Contains(got[0], "runs sh in "+filepath.Join(h.dir, "nightly")) || !strings.Contains(got[0], "under account b") {
		t.Fatalf("escalations after one failure: %q", got)
	}
	h.ticks(3, time.Minute)
	if got := jobEscalations(h, "nightly"); len(got) != 1 {
		t.Fatalf("escalations after four failures in one streak: %q", got)
	}
	writeFile(t, filepath.Join(h.dir, "nightly", "ok"), "")
	h.ticks(1, time.Minute)
	if err := os.Remove(filepath.Join(h.dir, "nightly", "ok")); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	if got := jobEscalations(h, "nightly"); len(got) != 2 {
		t.Fatalf("escalations after a second streak: %q", got)
	}
	sameAfterReplay(t, h, "after two streaks")
}

// fail_after unset still waits for three failures in a row.
func TestScheduledJobFailAfterDefault(t *testing.T) {
	h := schedHarness(t, false, map[string]ScheduledJob{
		"flaky": {Every: Duration{time.Minute}, Argv: []string{"false"}}})
	h.ticks(2, time.Minute)
	if got := jobEscalations(h, "flaky"); len(got) != 0 {
		t.Fatalf("escalated after two failures: %q", got)
	}
	h.ticks(2, time.Minute)
	if got := jobEscalations(h, "flaky"); len(got) != 1 || !strings.Contains(got[0], "flaky failed 3 times in a row") {
		t.Fatalf("escalations after three failures: %q", got)
	}
}

// output keeps each run's stdout and stderr in one file named for the job
// and its slot, the end event names it, and only keep of the job's files
// remain; another job's files in the same directory are left alone.
func TestScheduledJobOutput(t *testing.T) {
	h := newHarness(t, false)
	out := filepath.Join(h.dir, "runs")
	other := filepath.Join(out, "nightly-extra-20260101T000000Z.log")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, other, "another job's run\n")
	h = schedHarnessWith(t, h, map[string]ScheduledJob{
		"nightly":       {Every: Duration{time.Minute}, Output: "runs", Keep: 2, Argv: []string{"sh", "-c", "echo said; echo complained >&2; exit 4"}},
		"nightly-extra": {At: "03:00", Argv: []string{"true"}}})
	h.ticks(4, time.Minute)
	ends := jobEvents(h, "nightly", JobEnd)
	if len(ends) != 4 {
		t.Fatalf("ends: %+v", ends)
	}
	name := regexp.MustCompile(`^nightly-\d{8}T\d{6}Z\.log$`)
	for i, e := range ends {
		if filepath.Dir(e.Log) != out || !name.MatchString(filepath.Base(e.Log)) || e.Error != "complained" {
			t.Fatalf("end %d: %+v", i, e)
		}
		if want := "nightly-" + e.Slot.UTC().Format("20060102T150405Z") + ".log"; filepath.Base(e.Log) != want {
			t.Fatalf("end %d log %s, want %s", i, e.Log, want)
		}
	}
	b, err := os.ReadFile(ends[3].Log)
	if err != nil || !strings.Contains(string(b), "said\n") || !strings.Contains(string(b), "complained\n") {
		t.Fatalf("latest log %q: %v", b, err)
	}
	files, _ := filepath.Glob(filepath.Join(out, "nightly-2*.log"))
	if len(files) != 2 || files[0] != ends[2].Log || files[1] != ends[3].Log {
		t.Fatalf("kept %q, want the latest two", files)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("another job's file was pruned: %v", err)
	}
	// Posted on the pass after the third failure, naming that run's log.
	if got := jobEscalations(h, "nightly"); len(got) != 1 || !strings.Contains(got[0], "its log: "+ends[2].Log) {
		t.Fatalf("escalation does not name the latest log: %q", got)
	}
	v := &uiIndex{}
	(&uiSource{Name: "live", st: h.sup.st}).addJobs(v, h.cfg)
	if v.Jobs[0].Name != "nightly" || v.Jobs[0].Log != ends[3].Log {
		t.Fatalf("ui jobs = %+v", v.Jobs)
	}
	var sb strings.Builder
	if err := uiTemplates.ExecuteTemplate(&sb, "index", v); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "log "+ends[3].Log) {
		t.Fatalf("index does not name the log:\n%s", sb.String())
	}
	sameAfterReplay(t, h, "after four logged runs")
}

// output with keep unset keeps defaultScheduledKeep runs; a job without
// output names no log.
func TestScheduledJobOutputDefaults(t *testing.T) {
	h := schedHarness(t, false, map[string]ScheduledJob{
		"sweep": {Every: Duration{time.Minute}, Output: "runs", Argv: []string{"true"}},
		"plain": {Every: Duration{time.Minute}, Argv: []string{"true"}}})
	if j := h.cfg.Jobs.Scheduled["sweep"]; j.Keep != defaultScheduledKeep || j.Output != filepath.Join(h.dir, "runs") {
		t.Fatalf("resolved job = %+v", j)
	}
	h.ticks(defaultScheduledKeep+2, time.Minute)
	files, _ := filepath.Glob(filepath.Join(h.dir, "runs", "sweep-*.log"))
	if len(files) != defaultScheduledKeep {
		t.Fatalf("%d files kept, want %d", len(files), defaultScheduledKeep)
	}
	for _, e := range jobEvents(h, "plain", JobEnd) {
		if e.Log != "" {
			t.Fatalf("a job without output names a log: %+v", e)
		}
	}
}

// The example shows a scheduled job with #86's keys, resolved.
func TestExampleConfigShowsScheduledJob(t *testing.T) {
	home, cfg := loadExampleConfig(t)
	j, ok := cfg.Jobs.Scheduled["nightly"]
	if !ok || j.FailAfter != 1 || j.Keep != 14 || j.Env["PATH"] == "" || j.Account != "spare" ||
		j.Output != filepath.Join(home, ".local", "state", "agentd-kit", "jobs") || j.Cwd != filepath.Join(home, "work-17") {
		t.Fatalf("jobs.scheduled.nightly = %+v", j)
	}
}
