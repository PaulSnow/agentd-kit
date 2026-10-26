package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// schedHarness is a harness with the scheduled jobs jobs, each running in
// its own directory under h.dir unless it names one.
func schedHarness(t *testing.T, shadow bool, jobs map[string]ScheduledJob) *harness {
	t.Helper()
	return schedHarnessWith(t, newHarness(t, shadow), jobs)
}

// schedHarnessWith is schedHarness over h, as it is configured.
func schedHarnessWith(t *testing.T, h *harness, jobs map[string]ScheduledJob) *harness {
	t.Helper()
	shadow := h.sup.shadow
	for name, j := range jobs {
		if j.Cwd == "" {
			j.Cwd = filepath.Join(h.dir, name)
			if err := os.MkdirAll(j.Cwd, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		jobs[name] = j
	}
	h.cfg.Jobs.Scheduled = jobs
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	h.restart(shadow)
	return h
}

// jobEvents are the job events of name, of one phase or all.
func jobEvents(h *harness, name, phase string) []*JobPayload {
	var out []*JobPayload
	for _, e := range h.events() {
		if e.Kind == KindJob && e.Job.Name == name && (phase == "" || e.Job.Phase == phase) {
			out = append(out, e.Job)
		}
	}
	return out
}

func readJobFile(t *testing.T, h *harness, name, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, name, file))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// An every job runs at once and then each every; its start and end are
// logged with the exit code and duration, and a restarted agentd does not
// run the slot again.
func TestScheduledJobRunsEvery(t *testing.T) {
	h := schedHarness(t, false, map[string]ScheduledJob{
		"sweep": {Every: Duration{10 * time.Minute}, Argv: []string{"sh", "-c", "echo run >> runs"}}})
	h.ticks(1, time.Minute)
	if got := readJobFile(t, h, "sweep", "runs"); got != "run\n" {
		t.Fatalf("runs after the first pass: %q", got)
	}
	ends := jobEvents(h, "sweep", JobEnd)
	if len(ends) != 1 || ends[0].Exit == nil || *ends[0].Exit != 0 || ends[0].Error != "" || len(jobEvents(h, "sweep", JobStart)) != 1 {
		t.Fatalf("job events: %+v", jobEvents(h, "sweep", ""))
	}
	h.ticks(8, time.Minute)
	h.restart(false)
	h.ticks(1, time.Minute)
	if got := readJobFile(t, h, "sweep", "runs"); got != "run\n" {
		t.Fatalf("ran before its next slot: %q", got)
	}
	h.ticks(1, time.Minute) // 10 minutes after the first
	if got := readJobFile(t, h, "sweep", "runs"); got != "run\nrun\n" {
		t.Fatalf("runs at its next slot: %q", got)
	}
	sameAfterReplay(t, h, "after two runs")
}

// An at job runs at its time each day, once; a slot agentd sees more than
// scheduledLate after it is not run.
func TestScheduledJobRunsAt(t *testing.T) {
	now := time.Date(2026, 1, 5, 2, 50, 0, 0, time.UTC)
	j := ScheduledJob{At: "03:00"}
	if _, due := j.slot(JobRecord{}, now); due {
		t.Fatal("due before its time")
	}
	slot, due := j.slot(JobRecord{}, now.Add(15*time.Minute))
	if !due || !slot.Equal(time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("slot %v, due %v", slot, due)
	}
	if _, due := j.slot(JobRecord{Slot: slot}, now.Add(30*time.Minute)); due {
		t.Fatal("due twice for one slot")
	}
	if _, due := j.slot(JobRecord{Slot: slot}, slot.Add(26*time.Hour)); due {
		t.Fatal("a slot two hours old is due")
	}
	if next, due := j.slot(JobRecord{Slot: slot}, slot.Add(24*time.Hour+time.Minute)); !due || !next.Equal(slot.AddDate(0, 0, 1)) {
		t.Fatalf("next day's slot %v, due %v", next, due)
	}

	h := newHarness(t, false)
	at := h.clock.Now().Add(5 * time.Minute).Format("15:04")
	h = schedHarness(t, false, map[string]ScheduledJob{"nightly": {At: at, Argv: []string{"sh", "-c", "echo run >> runs"}}})
	h.ticks(4, time.Minute)
	if got := readJobFile(t, h, "nightly", "runs"); got != "" {
		t.Fatalf("ran before %s: %q", at, got)
	}
	h.ticks(5, 2*time.Minute)
	if got := readJobFile(t, h, "nightly", "runs"); got != "run\n" {
		t.Fatalf("runs at %s: %q", at, got)
	}
}

// A slot due while the job's last run still runs is skipped, logged, and
// not run after.
func TestScheduledJobOverlapSkipped(t *testing.T) {
	h := schedHarness(t, false, map[string]ScheduledJob{
		"slow": {Every: Duration{time.Minute}, Argv: []string{"sh", "-c", "echo run >> runs; while [ ! -f release ]; do sleep 0.02; done"}}})
	h.sup.Pass()
	h.clock.advance(time.Minute)
	waitFor(t, "the run to start", func() bool { return readJobFile(t, h, "slow", "runs") != "" })
	h.sup.Pass()
	writeFile(t, filepath.Join(h.dir, "slow", "release"), "")
	if err := h.sup.Wait(); err != nil {
		t.Fatal(err)
	}
	if n, s := len(jobEvents(h, "slow", JobStart)), len(jobEvents(h, "slow", JobSkip)); n != 1 || s != 1 {
		t.Fatalf("%d starts, %d skips, want 1 and 1", n, s)
	}
	h.clock.advance(30 * time.Second)
	h.sup.Pass()
	h.sup.Wait()
	if got := readJobFile(t, h, "slow", "runs"); got != "run\n" {
		t.Fatalf("the skipped slot ran: %q", got)
	}
}

// A run over its timeout is killed, and its end says so.
func TestScheduledJobTimeout(t *testing.T) {
	h := schedHarness(t, false, map[string]ScheduledJob{
		"stuck": {Every: Duration{time.Hour}, Timeout: Duration{200 * time.Millisecond}, Argv: []string{"sleep", "30"}}})
	start := time.Now()
	h.ticks(1, time.Minute)
	if time.Since(start) > 10*time.Second {
		t.Fatal("the run was not killed at its timeout")
	}
	ends := jobEvents(h, "stuck", JobEnd)
	if len(ends) != 1 || *ends[0].Exit != -1 || !strings.Contains(ends[0].Error, "timed out after 200ms") {
		t.Fatalf("end = %+v", ends)
	}
}

// A job runs under its account: the account's config directory and token
// in its environment, the session's own variables gone, and the token on
// no command line and not in the log.
func TestScheduledJobAccountEnvironment(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("tok-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDECODE", "1")
	h := newHarness(t, false)
	h.cfg.Accounts = map[string]Account{"b": {ConfigDir: "/accounts/b", TokenFile: tok}}
	cwd := filepath.Join(h.dir, "report")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	h.cfg.Jobs.Scheduled = map[string]ScheduledJob{
		"report": {Every: Duration{time.Hour}, Account: "b", Cwd: cwd, Argv: []string{"sh", "-c", "env > env.out; ps -o args= -p $$ > args.out"}}}
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	h.ticks(1, time.Minute)
	env := readJobFile(t, h, "report", "env.out")
	if !strings.Contains(env, "CLAUDE_CONFIG_DIR=/accounts/b\n") || !strings.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=tok-123\n") || strings.Contains(env, "CLAUDECODE=") {
		t.Fatalf("job env:\n%s", env)
	}
	if args := readJobFile(t, h, "report", "args.out"); strings.Contains(args, "tok-123") {
		t.Fatalf("the token is on the command line: %q", args)
	}
	if strings.Contains(h.buf.String(), "tok-123") {
		t.Fatal("the token is in the log")
	}
}

// Three failed runs in a row are escalated once, naming the job and its
// error; a success ends the streak, and the next streak is escalated again.
func TestScheduledJobFailuresEscalatedOnce(t *testing.T) {
	h := schedHarness(t, false, map[string]ScheduledJob{
		"flaky": {Every: Duration{time.Minute}, Argv: []string{"sh", "-c", "if [ -f ok ]; then exit 0; fi; echo broken >&2; exit 3"}}})
	escalations := func() []string {
		var out []string
		for _, e := range h.events() {
			if e.Kind == KindAction && e.Act.Action == ActEscalate && e.Session == jobTrack("flaky") {
				out = append(out, e.Act.Message)
			}
		}
		return out
	}
	h.ticks(2, time.Minute)
	if got := escalations(); len(got) != 0 {
		t.Fatalf("escalated after two failures: %q", got)
	}
	h.ticks(4, time.Minute)
	got := escalations()
	if len(got) != 1 || !strings.Contains(got[0], "flaky failed 3 times") || !strings.Contains(got[0], "broken") {
		t.Fatalf("escalations after six failures: %q", got)
	}
	if ends := jobEvents(h, "flaky", JobEnd); *ends[0].Exit != 3 || ends[0].Error != "broken" {
		t.Fatalf("end = %+v", ends[0])
	}
	writeFile(t, filepath.Join(h.dir, "flaky", "ok"), "")
	h.ticks(2, time.Minute)
	if r := h.sup.st.Jobs["flaky"]; r.Failures != 0 {
		t.Fatalf("failures after a success = %d", r.Failures)
	}
	if err := os.Remove(filepath.Join(h.dir, "flaky", "ok")); err != nil {
		t.Fatal(err)
	}
	h.ticks(5, time.Minute)
	if got := escalations(); len(got) != 2 {
		t.Fatalf("escalations after a second streak: %q", got)
	}
	sameAfterReplay(t, h, "after two streaks")
}

// Shadow mode runs nothing and records the run it would start, once a
// slot.
func TestScheduledJobShadow(t *testing.T) {
	h := schedHarness(t, true, map[string]ScheduledJob{
		"sweep": {Every: Duration{10 * time.Minute}, Argv: []string{"sh", "-c", "echo run >> runs"}}})
	h.ticks(3, time.Minute)
	if got := readJobFile(t, h, "sweep", "runs"); got != "" {
		t.Fatalf("shadow mode ran the job: %q", got)
	}
	if w := jobEvents(h, "sweep", ""); len(w) != 1 || w[0].Phase != JobWould {
		t.Fatalf("shadow job events: %+v", w)
	}
}

// A job's schedule, argv and account are checked at load.
func TestScheduledJobConfigRefused(t *testing.T) {
	for _, c := range []struct {
		job  ScheduledJob
		want string
	}{
		{ScheduledJob{Argv: []string{"x"}}, "exactly one of every and at"},
		{ScheduledJob{Every: Duration{time.Hour}, At: "03:00", Argv: []string{"x"}}, "exactly one of every and at"},
		{ScheduledJob{At: "3am", Argv: []string{"x"}}, "is not HH:MM"},
		{ScheduledJob{Every: Duration{time.Hour}}, "argv is empty"},
		{ScheduledJob{Every: Duration{time.Hour}, Argv: []string{"x"}, Account: "nobody"}, `account "nobody" is not in accounts`},
		{ScheduledJob{Every: Duration{time.Hour}, Argv: []string{"x"}, Cwd: "missing"}, "does not exist"},
	} {
		h := newHarness(t, false)
		h.cfg.Jobs.Scheduled = map[string]ScheduledJob{"j": c.job}
		if err := h.cfg.finish(h.dir); err == nil || !strings.Contains(err.Error(), "jobs.scheduled.j") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.job, err, c.want)
		}
	}
}

// The UI lists the scheduled jobs: schedule, latest run, and a failing one.
func TestScheduledJobsInUI(t *testing.T) {
	h := schedHarness(t, false, map[string]ScheduledJob{
		"flaky": {Every: Duration{time.Minute}, Argv: []string{"sh", "-c", "echo broken >&2; exit 3"}},
		"sweep": {At: "03:00", Argv: []string{"true"}}})
	h.ticks(1, time.Minute)
	v := &uiIndex{}
	(&uiSource{Name: "live", st: h.sup.st}).addJobs(v, h.cfg)
	if len(v.Jobs) != 2 || v.Jobs[0].Name != "flaky" || !v.Jobs[0].Failing || v.Jobs[0].Error != "broken" || v.Jobs[0].Schedule != "every 1m0s" ||
		v.Jobs[1].Name != "sweep" || v.Jobs[1].Schedule != "at 03:00" || v.Jobs[1].Failing {
		t.Fatalf("ui jobs = %+v", v.Jobs)
	}
	var b strings.Builder
	if err := uiTemplates.ExecuteTemplate(&b, "index", v); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); !strings.Contains(out, "<strong>flaky</strong>") || !strings.Contains(out, "exit 3") || !strings.Contains(out, "every 1m0s") {
		t.Fatalf("index:\n%s", out)
	}
}

// Slots an every job missed while agentd was down run once, late, at the
// latest of them.
func TestScheduledJobMissedSlotsRunOnce(t *testing.T) {
	h := schedHarness(t, false, map[string]ScheduledJob{
		"sweep": {Every: Duration{10 * time.Minute}, Argv: []string{"sh", "-c", "echo run >> runs"}}})
	first := h.clock.Now()
	h.ticks(1, time.Minute)
	h.clock.advance(34 * time.Minute)
	h.restart(false)
	h.ticks(2, time.Minute)
	starts := jobEvents(h, "sweep", JobStart)
	if got := readJobFile(t, h, "sweep", "runs"); got != "run\nrun\n" || len(starts) != 2 || !starts[1].Slot.Equal(first.Add(30*time.Minute)) {
		t.Fatalf("runs %q, starts %+v", got, starts)
	}
}

// A failed run's end keeps the last scheduledStderr bytes of its standard
// error, screened against the confidential patterns.
func TestScheduledJobStderrCutAndScreened(t *testing.T) {
	h := schedHarness(t, false, map[string]ScheduledJob{
		"long":   {Every: Duration{time.Hour}, Argv: []string{"sh", "-c", "printf '%0500d' 0 >&2; echo tail-end >&2; exit 1"}},
		"secret": {Every: Duration{time.Hour}, Argv: []string{"sh", "-c", "echo leaked-widget-key >&2; exit 1"}}})
	h.cfg.ConfidentialPatterns = []string{`widget-key`}
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	h.ticks(1, time.Minute)
	if e := jobEvents(h, "long", JobEnd); len(e) != 1 || len(e[0].Error) > scheduledStderr || !strings.HasSuffix(e[0].Error, "tail-end") {
		t.Fatalf("long end = %+v", e)
	}
	if e := jobEvents(h, "secret", JobEnd); len(e) != 1 || !strings.HasPrefix(e[0].Error, "(withheld") {
		t.Fatalf("secret end = %+v", e)
	}
	if strings.Contains(h.buf.String(), "widget-key") {
		t.Fatal("the confidential text is in the log")
	}
}

// A token file made readable by others after load is refused at the run:
// its end says why, and the command does not run.
func TestScheduledJobTokenFileRefused(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("tok-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, false)
	h.cfg.Accounts = map[string]Account{"b": {TokenFile: tok}}
	h = schedHarnessWith(t, h, map[string]ScheduledJob{
		"report": {Every: Duration{time.Hour}, Account: "b", Argv: []string{"sh", "-c", "echo run >> runs"}}})
	if err := os.Chmod(tok, 0o644); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, time.Minute)
	if e := jobEvents(h, "report", JobEnd); len(e) != 1 || *e[0].Exit != -1 || e[0].Error == "" {
		t.Fatalf("end = %+v", e)
	}
	if got := readJobFile(t, h, "report", "runs"); got != "" {
		t.Fatalf("ran with a refused token file: %q", got)
	}
}

// A run killed because agentd stopped ends as stopped, and does not count
// toward the failure streak.
func TestScheduledJobStoppedRun(t *testing.T) {
	h := schedHarness(t, false, map[string]ScheduledJob{
		"slow": {Every: Duration{time.Hour}, Argv: []string{"sh", "-c", "echo run >> runs; sleep 30"}}})
	ctx, cancel := context.WithCancel(context.Background())
	h.sup.runCtx = ctx
	start := time.Now()
	h.sup.Pass()
	waitFor(t, "the run to start", func() bool { return readJobFile(t, h, "slow", "runs") != "" })
	cancel()
	h.sup.Wait()
	if time.Since(start) > 10*time.Second {
		t.Fatal("the run was not killed when agentd stopped")
	}
	e := jobEvents(h, "slow", JobEnd)
	if len(e) != 1 || !e[0].Stopped || !strings.Contains(e[0].Error, "agentd stopped") {
		t.Fatalf("end = %+v", e)
	}
	if r := h.sup.st.Jobs["slow"]; r.Failures != 0 || r.Running() {
		t.Fatalf("record = %+v", r)
	}
}

// A run an earlier agentd started and never ended is ended as stopped by
// the next one, which then runs the job's next slot.
func TestScheduledJobLeftByEarlierAgentd(t *testing.T) {
	h := schedHarness(t, false, map[string]ScheduledJob{
		"sweep": {Every: Duration{10 * time.Minute}, Argv: []string{"sh", "-c", "echo run >> runs"}}})
	h.sup.emit("", Event{Kind: KindJob, Job: &JobPayload{Name: "sweep", Phase: JobStart, Slot: h.clock.Now()}})
	h.restart(false)
	h.ticks(1, time.Minute)
	e := jobEvents(h, "sweep", JobEnd)
	if len(e) != 1 || !e[0].Stopped || !strings.Contains(e[0].Error, "agentd restarted") {
		t.Fatalf("end = %+v", e)
	}
	if r := h.sup.st.Jobs["sweep"]; r.Failures != 0 || r.Running() {
		t.Fatalf("record = %+v", r)
	}
	h.ticks(10, time.Minute)
	if got := readJobFile(t, h, "sweep", "runs"); got != "run\n" {
		t.Fatalf("runs after the next slot: %q", got)
	}
	sameAfterReplay(t, h, "after a left run")
}

// A scheduled job's escalations go to the default console, even when a
// console's sessions glob matches every name.
func TestScheduledJobConsole(t *testing.T) {
	h := newHarness(t, false)
	h.cfg.Consoles = map[string]ConsoleConfig{"ops": {Sessions: []string{"*"}}}
	if got := h.cfg.Console(jobTrack("sweep")); got != "infra" {
		t.Fatalf("console = %q, want the default", got)
	}
}
