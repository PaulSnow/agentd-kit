package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Scheduled jobs (#51): commands agentd runs on a schedule, one at a time
// each, under an account's login, so agent work that ran from an outside
// cron is supervised and logged like the rest. A job's start and end are
// job events; three failed runs in a row are escalated once.

// ScheduledJob is one job of jobs.scheduled: exactly one of Every and At
// (HH:MM, daily, in agentd's local time), the argv it runs in Cwd, the
// account whose login it runs under (none: agentd's own), and its timeout.
type ScheduledJob struct {
	Every   Duration `json:"every"`
	At      string   `json:"at"`
	Argv    []string `json:"argv"`
	Cwd     string   `json:"cwd"`
	Account string   `json:"account"`
	Timeout Duration `json:"timeout"`
}

// Scheduled job defaults and limits.
const (
	DefaultScheduledTimeout = 30 * time.Minute
	// scheduledLate is how late an at slot still runs, as after agentd was
	// down over it; an older slot is not run, and the next one is.
	scheduledLate = time.Hour
	// scheduledFailures failed runs in a row are escalated, once a streak.
	scheduledFailures = 3
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

// checkScheduled checks jobs.scheduled and resolves each job's cwd: a
// relative one against the configuration's directory, none its directory.
func (c *Config) checkScheduled(base string, path func(field, p string) string, isDir func(field, p string)) []error {
	var errs []error
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
		if len(j.Argv) == 0 || j.Argv[0] == "" {
			errs = append(errs, fmt.Errorf("%s.argv is empty", field))
		}
		if _, ok := c.Accounts[j.Account]; j.Account != "" && !ok {
			errs = append(errs, fmt.Errorf("%s.account %q is not in accounts", field, j.Account))
		}
		if j.Timeout.Duration < 0 {
			errs = append(errs, fmt.Errorf("%s.timeout is negative", field))
		}
		if j.Cwd == "" {
			j.Cwd = base
		}
		j.Cwd = path(field+".cwd", j.Cwd)
		isDir(field+".cwd", j.Cwd)
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

// jobTrack is the name of the track a scheduled job's escalations are
// kept on: no session's, since a session's name has no colon.
func jobTrack(name string) string { return "job:" + name }

// runJobs starts each scheduled job whose slot is due, in a worker, and
// escalates a job that failed scheduledFailures times in a row. A slot
// due while its job still runs is skipped; shadow mode records the run it
// would start.
func (s *Supervisor) runJobs(now time.Time) {
	for _, name := range sortedKeys(s.cfg.Jobs.Scheduled) {
		if s.halt != nil {
			return
		}
		j, r := s.cfg.Jobs.Scheduled[name], s.st.Jobs[name]
		if r.Failures >= scheduledFailures && r.End != nil {
			key := jobTrack(name) + ":" + r.FailedSince.UTC().Format(time.RFC3339)
			s.escalate(jobTrack(name), key, fmt.Sprintf("Scheduled job %s failed %d times in a row since %s; the latest: %s. agentd keeps running it on its schedule.",
				name, r.Failures, r.FailedSince.Format("2006-01-02 15:04"), jobFailure(r.End)), s.track(jobTrack(name)), now)
		}
		slot, due := j.slot(r, now)
		if !due {
			continue
		}
		key := "job:" + name
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
			s.spawnShared(key, func(w *Supervisor) {
				end := runScheduled(w.runCtx, j, acct)
				if w.runCtx.Err() != nil {
					end.Error = "agentd stopped, and the run was killed"
				}
				end.Name, end.Phase, end.Slot = name, JobEnd, slot
				w.emit("", Event{Kind: KindJob, Job: &end})
			})
		}
	}
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
// on no command line. Its output is not kept; a failed run's end keeps
// the tail of its standard error.
func runScheduled(ctx context.Context, j ScheduledJob, acct Account) JobPayload {
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
		script = `t=$(cat "$AGENTD_TOKEN_FILE") && case $t in *[![:space:]]*) true ;; *) false ;; esac || { echo "token file is empty" >&2; exit 1; }; CLAUDE_CODE_OAUTH_TOKEN=$t exec "$@"`
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := groupCommand(ctx, j.Cwd, "sh", append([]string{"-c", script, "sh"}, j.Argv...)...)
	cmd.Env = jobEnv(os.Environ(), acct)
	stderr := &tailBuffer{max: scheduledStderr}
	cmd.Stderr = stderr
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
		p.Error = strings.TrimSpace(string(stderr.b))
	default:
		p.Error = err.Error()
	}
	return p
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
		if r.End != nil && r.Failures > 0 && !secret(jobFailure(r.End)) {
			u.Error = jobFailure(r.End)
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
