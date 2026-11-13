package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Scheduled jobs (#51): commands agentd runs on a schedule, one at a time
// each, under an account's login, so agent work that ran from an outside
// cron is supervised and logged like the rest. A job's start and end are
// job events; failed runs in a row (three, or the job's fail_after) are
// escalated once a streak.

// ScheduledJob is one job of jobs.scheduled: exactly one of Every and At
// (HH:MM, daily, in agentd's local time), the argv it runs in Cwd, the
// account whose login it runs under (none: agentd's own), and its timeout.
// Env adds variables to its environment (#86); FailAfter is how many
// failed runs in a row are escalated (0: scheduledFailures); Output, when
// set, is the directory each run's standard output and error are kept in,
// one file a run, the latest Keep of them (0: defaultScheduledKeep).
// A step job names a step spec (#85) in place of argv: it runs the step
// as agentd step does, under the spec's model, account and timeout, with
// the job's account and timeout when the spec sets none.
type ScheduledJob struct {
	Every     Duration          `json:"every"`
	At        string            `json:"at"`
	Argv      []string          `json:"argv"`
	Step      string            `json:"step"`
	Cwd       string            `json:"cwd"`
	Account   string            `json:"account"`
	Timeout   Duration          `json:"timeout"`
	Env       map[string]string `json:"env,omitempty"`
	FailAfter int               `json:"fail_after,omitempty"`
	Output    string            `json:"output,omitempty"`
	Keep      int               `json:"keep,omitempty"`
}

// failAfter is how many failed runs in a row the job escalates.
func (j ScheduledJob) failAfter() int {
	if j.FailAfter > 0 {
		return j.FailAfter
	}
	return scheduledFailures
}

// Scheduled job defaults and limits.
const (
	DefaultScheduledTimeout = 30 * time.Minute
	// scheduledLate is how late an at slot still runs, as after agentd was
	// down over it; an older slot is not run, and the next one is.
	scheduledLate = time.Hour
	// scheduledFailures failed runs in a row are escalated, once a streak,
	// unless the job sets fail_after.
	scheduledFailures = 3
	// defaultScheduledKeep is how many of a job's run logs are kept when
	// it sets output and not keep.
	defaultScheduledKeep = 14
	// scheduledStderr is how much of a failed run's standard error its end
	// event keeps.
	scheduledStderr = 300
)

// Job phases.
const (
	JobStart = "start" // a run started, for its slot
	JobEnd   = "end"   // a run ended: its exit code and duration
	JobSkip  = "skip"  // a slot not run, since the job's last run still runs
	JobWould = "would" // shadow mode: the run it would start
)

// validJobPhase says whether p is a job event's phase.
func validJobPhase(p string) bool {
	return p == JobStart || p == JobEnd || p == JobSkip || p == JobWould
}

// JobPayload is one fact of a scheduled job's run.
type JobPayload struct {
	Name  string    `json:"name"`
	Phase string    `json:"phase"`
	Slot  time.Time `json:"slot"`
	// An end: the exit code (-1 when the command did not run or was
	// killed), how long it ran, and why it failed, if it did.
	Exit       *int   `json:"exit,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Error      string `json:"error,omitempty"`
	// Stopped: agentd stopped during the run and killed it, or a restarted
	// agentd found it with no end. Such a run neither failed nor
	// succeeded, and leaves the failure streak as it was.
	Stopped bool `json:"stopped,omitempty"`
	// Log is the file the run's output was kept in, when the job sets
	// output.
	Log string `json:"log,omitempty"`
}

// Took is how long the run took, to the tenth of a second.
func (p JobPayload) Took() string {
	return (time.Duration(p.DurationMS) * time.Millisecond).Round(100 * time.Millisecond).String()
}

// JobRecord is a scheduled job as the fold keeps it: its latest slot,
// started or not, its latest start and end, and the failed runs in a row
// since FailedSince.
type JobRecord struct {
	Slot        time.Time
	Started     time.Time
	End         *JobPayload
	Ended       time.Time
	Failures    int
	FailedSince time.Time
}

// applyJob folds a job event.
func (st *LogState) applyJob(e Event) {
	if st.Jobs == nil {
		st.Jobs = map[string]JobRecord{}
	}
	p := *e.Job
	r := st.Jobs[p.Name]
	switch p.Phase {
	case JobStart:
		r.Slot, r.Started = p.Slot, e.Time
	case JobSkip, JobWould:
		r.Slot = p.Slot
	case JobEnd:
		r.End, r.Ended = &p, e.Time
		if p.Stopped {
			break
		}
		if p.Exit != nil && *p.Exit == 0 && p.Error == "" {
			r.Failures, r.FailedSince = 0, time.Time{}
		} else {
			if r.Failures == 0 {
				r.FailedSince = r.Started
			}
			r.Failures++
		}
	}
	st.Jobs[p.Name] = r
}

// Running says whether the job's latest run has not ended.
func (r JobRecord) Running() bool { return !r.Started.IsZero() && r.Started.After(r.Ended) }

// envName is a variable name a job's env may set.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// secretEnvName is a variable name that looks like it holds a secret,
// which belongs in the job's own wrapper or file, not in the
// configuration.
var secretEnvName = regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSW|API_?KEY|CREDENTIAL|PRIVATE_?KEY)`)

// agentdEnv says whether name is one agentd sets in a job's environment
// or keeps from it, which a job's env may not set.
func agentdEnv(name string) bool {
	if name == "AGENTD_TOKEN_FILE" {
		return true
	}
	for _, d := range append(append([]string(nil), jobEnvDrop...), jobAccountDrop...) {
		if name == d {
			return true
		}
	}
	return false
}

// checkScheduled checks jobs.scheduled and resolves each job's cwd: a
// relative one against the configuration's directory, none its directory.
// A job's output resolves the same way; it is made when a run needs it.
func (c *Config) checkScheduled(base string, path func(field, p string) string, isDir func(field, p string)) []error {
	var errs []error
	var confidential []*regexp.Regexp
	for _, p := range screenPatterns(c) {
		if re, err := regexp.Compile(p); err == nil {
			confidential = append(confidential, re)
		}
	}
	for _, name := range sortedKeys(c.Jobs.Scheduled) {
		j, field := c.Jobs.Scheduled[name], "jobs.scheduled."+name
		if !sessionName.MatchString(name) {
			errs = append(errs, fmt.Errorf("%s: %q is not a plain name", field, name))
		}
		if (j.Every.Duration > 0) == (j.At != "") {
			errs = append(errs, fmt.Errorf("%s: set exactly one of every and at", field))
		}
		if j.Every.Duration < 0 {
			errs = append(errs, fmt.Errorf("%s.every is negative", field))
		}
		if _, err := time.Parse("15:04", j.At); j.At != "" && err != nil {
			errs = append(errs, fmt.Errorf("%s.at: %q is not HH:MM", field, j.At))
		}
		switch {
		case len(j.Argv) > 0 && j.Step != "":
			errs = append(errs, fmt.Errorf("%s: set exactly one of argv and step", field))
		case j.Step != "":
			j.Step = path(field+".step", j.Step)
			if _, err := loadStepSpec(j.Step, c); err != nil {
				errs = append(errs, fmt.Errorf("%s.step: %v", field, err))
			}
			// A step runs no command and keeps its own record and card.
			if len(j.Env) > 0 || j.FailAfter != 0 || j.Output != "" || j.Keep != 0 {
				errs = append(errs, fmt.Errorf("%s: a step job takes no env, fail_after, output or keep; the step keeps its own record and card", field))
			}
		case len(j.Argv) == 0:
			errs = append(errs, fmt.Errorf("%s: set exactly one of argv and step", field))
		case j.Argv[0] == "":
			errs = append(errs, fmt.Errorf("%s.argv is empty", field))
		}
		if _, ok := c.Accounts[j.Account]; j.Account != "" && !ok {
			errs = append(errs, fmt.Errorf("%s.account %q is not in accounts", field, j.Account))
		}
		if j.Timeout.Duration < 0 {
			errs = append(errs, fmt.Errorf("%s.timeout is negative", field))
		}
		for _, k := range sortedKeys(j.Env) {
			switch {
			case !envName.MatchString(k):
				errs = append(errs, fmt.Errorf("%s.env: %q is not a variable name", field, k))
			case agentdEnv(k):
				errs = append(errs, fmt.Errorf("%s.env: %s is set by agentd or kept from jobs; it may not be set here", field, k))
			case secretEnvName.MatchString(k):
				errs = append(errs, fmt.Errorf("%s.env: %s looks like a secret; keep it in the job's own wrapper or file, not in the configuration", field, k))
			}
			for _, re := range confidential {
				if re.MatchString(j.Env[k]) {
					// The value is not repeated: it is what must not be shown.
					errs = append(errs, fmt.Errorf("%s.env.%s matches a confidential pattern; keep it in the job's own wrapper or file", field, k))
					break
				}
			}
		}
		if j.FailAfter < 0 {
			errs = append(errs, fmt.Errorf("%s.fail_after is negative", field))
		}
		if j.Keep < 0 {
			errs = append(errs, fmt.Errorf("%s.keep is negative", field))
		}
		if j.Cwd == "" {
			j.Cwd = base
		}
		j.Cwd = path(field+".cwd", j.Cwd)
		isDir(field+".cwd", j.Cwd)
		if j.Output != "" {
			j.Output = path(field+".output", j.Output)
			if j.Keep == 0 {
				j.Keep = defaultScheduledKeep
			}
		}
		c.Jobs.Scheduled[name] = j
	}
	return errs
}

// slot is the job's latest slot at or before now, and whether it is due:
// later than the latest slot it started, skipped or would have. An every
// job's first slot is now, and a slot it missed runs once, late; an at
// slot runs only within scheduledLate of it.
func (j ScheduledJob) slot(r JobRecord, now time.Time) (time.Time, bool) {
	if j.Every.Duration > 0 {
		if r.Slot.IsZero() {
			return now, true
		}
		if now.Before(r.Slot.Add(j.Every.Duration)) {
			return time.Time{}, false
		}
		n := now.Sub(r.Slot) / j.Every.Duration
		return r.Slot.Add(n * j.Every.Duration), true
	}
	at, err := time.Parse("15:04", j.At)
	if err != nil {
		return time.Time{}, false
	}
	slot := time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
	if slot.After(now) {
		slot = slot.AddDate(0, 0, -1)
	}
	return slot, slot.After(r.Slot) && now.Sub(slot) <= scheduledLate
}

// jobTrackPrefix begins the name of the track a scheduled job's
// escalations are kept on: no session's, since a session's name has no
// colon.
const jobTrackPrefix = "job:"

// jobTrack is the name of the track job's escalations are kept on.
func jobTrack(name string) string { return jobTrackPrefix + name }

// runJobs starts each scheduled job whose slot is due, in a worker, and
// escalates a job that failed its fail_after times in a row. A slot
// due while its job still runs is skipped; shadow mode records the run it
// would start.
func (s *Supervisor) runJobs(now time.Time) {
	left := !s.jobsLeft
	defer func() { s.jobsLeft = s.jobsLeft || s.halt == nil }()
	for _, name := range sortedKeys(s.cfg.Jobs.Scheduled) {
		if s.halt != nil {
			return
		}
		j, r := s.cfg.Jobs.Scheduled[name], s.st.Jobs[name]
		// A step job's failure is the step's own card (escalateSteps).
		if r.Failures >= j.failAfter() && r.End != nil && j.Step == "" {
			key := jobTrack(name) + ":" + r.FailedSince.UTC().Format(time.RFC3339)
			s.escalate(jobTrack(name), key, jobCard(name, j, r), s.track(jobTrack(name)), now)
		}
		key := jobTrack(name)
		if left && r.Running() && !s.work.inFlight[key] && !s.shadow {
			// Started by an earlier agentd, which ended without its end:
			// its process group may still run, and is not waited for.
			s.emit("", Event{Kind: KindJob, Job: &JobPayload{Name: name, Phase: JobEnd, Slot: r.Slot, Stopped: true,
				Error: "agentd restarted while it ran; how it ended is not known, and it may still run"}})
			if s.halt != nil {
				return
			}
			r = s.st.Jobs[name]
		}
		slot, due := j.slot(r, now)
		if !due {
			continue
		}
		switch {
		case s.shadow:
			s.emit("", Event{Kind: KindJob, Job: &JobPayload{Name: name, Phase: JobWould, Slot: slot}})
		case s.work.inFlight[key]:
			s.emit("", Event{Kind: KindJob, Job: &JobPayload{Name: name, Phase: JobSkip, Slot: slot}})
		default:
			if s.emit("", Event{Kind: KindJob, Job: &JobPayload{Name: name, Phase: JobStart, Slot: slot}}); s.halt != nil {
				return
			}
			acct := s.cfg.Accounts[j.Account]
			name, slot := name, slot
			if j.Step != "" {
				s.runStepJob(key, name, slot, j)
				continue
			}
			s.spawnShared(key, func(w *Supervisor) {
				end := runScheduled(w.runCtx, name, slot, j, acct)
				if w.runCtx.Err() != nil && (end.Exit == nil || *end.Exit != 0) {
					end.Error, end.Stopped = "agentd stopped, and the run was killed", true
				}
				end.Name, end.Phase, end.Slot = name, JobEnd, slot
				w.emit("", Event{Kind: KindJob, Job: &end})
			})
		}
	}
}

// jobCard is the escalation of job name's failure streak: how often and
// since when it failed, the latest failure, what it runs where and under
// which account, and the latest run's log. post screens it.
func jobCard(name string, j ScheduledJob, r JobRecord) string {
	times := "times"
	if r.Failures == 1 {
		times = "time"
	}
	acct := "agentd's own login"
	if j.Account != "" {
		acct = "account " + j.Account
	}
	msg := fmt.Sprintf("Scheduled job %s failed %d %s in a row since %s; the latest: %s. It runs %s in %s under %s.",
		name, r.Failures, times, r.FailedSince.Format("2006-01-02 15:04"), jobFailure(r.End), j.Argv[0], j.Cwd, acct)
	if r.End.Log != "" {
		msg += " The latest run's output is in its log: " + r.End.Log + "."
	}
	return msg + " agentd keeps running it on its schedule."
}

// runStepJob runs a step job in a worker: the step's event, then the
// job's end, which succeeds when the step ran or was skipped. The spec is
// read again for each run, so an edited spec needs no restart.
func (s *Supervisor) runStepJob(key, name string, slot time.Time, j ScheduledJob) {
	var last *StepPayload
	spec, err := loadStepSpec(j.Step, s.cfg)
	if err == nil {
		last = s.st.StepRuns[spec.Pipeline+"/"+spec.Name].LastOK
		if spec.Account == "" {
			spec.Account = j.Account
		}
		if spec.Timeout.Duration == 0 {
			spec.Timeout = j.Timeout
		}
	}
	cfg := s.cfg
	s.spawnShared(key, func(w *Supervisor) {
		start := time.Now()
		exit := 1
		end := JobPayload{Name: name, Phase: JobEnd, Slot: slot, Exit: &exit}
		if err != nil {
			end.Error = oneLine(err.Error(), scheduledStderr)
		} else {
			p := runStep(w.runCtx, spec, spec.job(cfg), last, false)
			if w.runCtx.Err() != nil && p.Outcome != StepOK && p.Outcome != StepSkipped {
				end.Error, end.Stopped = "agentd stopped, and the step was killed", true
			} else {
				w.emit("", Event{Kind: KindStep, Mode: ModeShared, Step: &p})
				if stepFailing(p.Outcome) {
					end.Error = p.Outcome + ": " + p.Error
				} else {
					exit = 0
				}
			}
		}
		end.DurationMS = time.Since(start).Milliseconds()
		w.emit("", Event{Kind: KindJob, Job: &end})
	})
}

// jobFailure says how a run failed.
func jobFailure(p *JobPayload) string {
	if p.Error != "" {
		return p.Error
	}
	if p.Exit != nil {
		return fmt.Sprintf("exit code %d", *p.Exit)
	}
	return "no exit code"
}

// runScheduled runs a job once and says how it ended. It runs as the
// triage job does: without the calling session's environment, with the
// account's config directory, and with its token read from the token file
// by the shell that execs argv, so agentd never holds the token and it is
// on no command line. The job's env is added after agentd's. Without
// output its output is not kept; with it, standard output and error go to
// one file for the slot, and only the job's latest keep files are kept. A
// failed run's end keeps the tail of its standard error either way.
func runScheduled(ctx context.Context, name string, slot time.Time, j ScheduledJob, acct Account) JobPayload {
	timeout := j.Timeout.Duration
	if timeout <= 0 {
		timeout = DefaultScheduledTimeout
	}
	exit := -1
	p := JobPayload{Exit: &exit}
	if acct.TokenFile != "" {
		if err := checkTokenFile(acct.TokenFile); err != nil {
			p.Error = err.Error()
			return p
		}
	}
	script := `exec "$@"`
	if acct.TokenFile != "" {
		script = tokenExecScript
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := groupCommand(ctx, j.Cwd, "sh", append([]string{"-c", script, "sh"}, j.Argv...)...)
	cmd.Env = jobEnv(os.Environ(), acct)
	for _, k := range sortedKeys(j.Env) {
		cmd.Env = append(cmd.Env, k+"="+j.Env[k])
	}
	stderr := &tailBuffer{max: scheduledStderr}
	cmd.Stderr = stderr
	if j.Output != "" {
		f, err := openJobLog(j.Output, name, slot)
		if err != nil {
			p.Error = err.Error()
			return p
		}
		defer func() {
			f.Close()
			pruneJobLogs(j.Output, name, j.Keep)
		}()
		p.Log = f.Name()
		cmd.Stdout, cmd.Stderr = f, io.MultiWriter(f, stderr)
	}
	start := time.Now()
	err := cmd.Run()
	p.DurationMS = time.Since(start).Milliseconds()
	var ee *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		p.Error = fmt.Sprintf("timed out after %s and was killed", timeout)
	case err == nil:
		exit = 0
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		exit = ee.ExitCode()
		// The tail may begin inside a character; the log keeps valid UTF-8
		// only, so a replay folds the same text.
		p.Error = strings.TrimSpace(strings.ToValidUTF8(string(stderr.b), ""))
	default:
		p.Error = err.Error()
	}
	return p
}

// jobLogStamp is how a run log's name gives its slot: UTC, RFC 3339 basic.
const jobLogStamp = "20060102T150405Z"

// jobLogName matches a run log's name after its job's name and a dash.
var jobLogName = regexp.MustCompile(`^\d{8}T\d{6}Z\.log$`)

// openJobLog makes dir if it is missing and opens job name's log for
// slot, emptied.
func openJobLog(dir, name string, slot time.Time) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("could not make its output directory: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, name+"-"+slot.UTC().Format(jobLogStamp)+".log"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("could not open its log: %v", err)
	}
	return f, nil
}

// pruneJobLogs removes all but the latest keep of job name's logs in dir.
// Another job's logs are not its, though its name begins with name and a
// dash: what follows must be a slot.
func pruneJobLogs(dir, name string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var logs []string
	for _, e := range entries {
		if rest, ok := strings.CutPrefix(e.Name(), name+"-"); ok && e.Type().IsRegular() && jobLogName.MatchString(rest) {
			logs = append(logs, e.Name())
		}
	}
	sort.Strings(logs) // by slot, oldest first
	for len(logs) > keep {
		os.Remove(filepath.Join(dir, logs[0]))
		logs = logs[1:]
	}
}

// uiJob is a scheduled job as the UI lists it: its schedule and its latest
// run.
type uiJob struct {
	JobRecord
	Name     string
	Schedule string
	Running  bool
	Failing  bool
	Error    string // the latest end's failure, screened
	Log      string // the latest run's log, screened
}

// addJobs lists src's scheduled jobs, the configured ones, in the index:
// the source whose latest run is newest stands.
func (src *uiSource) addJobs(v *uiIndex, cfg *Config) {
	secret := confidentialMatcher(cfg)
	for _, name := range sortedKeys(cfg.Jobs.Scheduled) {
		j, r := cfg.Jobs.Scheduled[name], src.st.Jobs[name]
		u := uiJob{JobRecord: r, Name: name, Schedule: "at " + j.At, Running: r.Running(), Failing: r.Failures > 0}
		if j.Every.Duration > 0 {
			u.Schedule = "every " + j.Every.Duration.String()
		}
		if r.End != nil && (r.Failures > 0 || r.End.Stopped) && !secret(jobFailure(r.End)) {
			u.Error = jobFailure(r.End)
		}
		if r.End != nil && r.End.Log != "" && !secret(r.End.Log) {
			u.Log = r.End.Log
		}
		i := sort.Search(len(v.Jobs), func(i int) bool { return v.Jobs[i].Name >= name })
		switch {
		case i == len(v.Jobs) || v.Jobs[i].Name != name:
			v.Jobs = append(v.Jobs[:i], append([]uiJob{u}, v.Jobs[i:]...)...)
		case r.Started.After(v.Jobs[i].Started):
			v.Jobs[i] = u
		}
	}
}
