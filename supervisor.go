package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Clock is time, so tests can move it.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time        { return time.Now() }
func (RealClock) Sleep(d time.Duration) { time.Sleep(d) }

// PaneLines is how much screen is read for the hash and the patterns.
const PaneLines = 40

// Supervisor runs the policies over the configured sessions. Every write to
// the world goes through act, which shadow mode turns into a would event.
type Supervisor struct {
	cfg    *Config
	log    *Log
	clock  Clock
	tmux   Tmux
	git    Git
	forge  AllowedForge
	runner Runner
	shadow bool

	tracks  map[string]*Track
	limitRe *regexp.Regexp
	trustRe *regexp.Regexp
}

// NewSupervisor wires the supervisor. The forge is wrapped with the
// repository allow-list here, so no caller can reach it unwrapped.
func NewSupervisor(cfg *Config, log *Log, clock Clock, tmux Tmux, git Git, forge Forge, runner Runner, shadow bool) *Supervisor {
	return &Supervisor{
		cfg:     cfg,
		log:     log,
		clock:   clock,
		tmux:    tmux,
		git:     git,
		forge:   AllowedForge{Repos: cfg.Repositories, Forge: forge},
		runner:  runner,
		shadow:  shadow,
		tracks:  map[string]*Track{},
		limitRe: regexp.MustCompile(cfg.Policies.UsageLimit.Pattern),
		trustRe: regexp.MustCompile(regexp.QuoteMeta(cfg.Policies.Trust.Pattern)),
	}
}

func (s *Supervisor) track(name string) *Track {
	t, ok := s.tracks[name]
	if !ok {
		t = newTrack()
		s.tracks[name] = t
	}
	return t
}

// Tracks exposes the memory for the status command and tests.
func (s *Supervisor) Tracks() map[string]*Track { return s.tracks }

func (s *Supervisor) emit(name, kind string, details map[string]any) {
	e := Event{Time: s.clock.Now(), Session: name, Kind: kind, Details: details}
	if err := s.log.Append(e); err != nil {
		fmt.Fprintf(os.Stderr, "agentd: event log: %v\n", err)
	}
}

// act performs one action, or in shadow mode records that it would. It
// returns whether the action counts as taken.
func (s *Supervisor) act(name, action string, details map[string]any, do func() error) bool {
	record := func(kind string, err error) {
		d := map[string]any{"action": action}
		for k, v := range details {
			d[k] = v
		}
		if err != nil {
			d["error"] = err.Error()
		}
		s.emit(name, kind, d)
	}
	if s.shadow {
		record(KindWould, nil)
		return true
	}
	if err := do(); err != nil {
		record(KindError, err)
		return false
	}
	record(KindAction, nil)
	return true
}

// skipOnce logs why a policy declined, once per key.
func (s *Supervisor) skipOnce(name, key, reason string, t *Track, details map[string]any) {
	if t.Skipped[key] {
		return
	}
	t.Skipped[key] = true
	d := map[string]any{"key": key, "reason": reason}
	for k, v := range details {
		d[k] = v
	}
	s.emit(name, KindSkip, d)
}

// Replay rebuilds the per-session memory from the event log. In shadow mode
// the would events count as if taken, so the shadow keeps its own counters.
func (s *Supervisor) Replay(events []Event) {
	for _, e := range events {
		if e.Session == "" {
			continue
		}
		t := s.track(e.Session)
		switch e.Kind {
		case KindObserve:
			t.Observed = true
			t.Hash = detailString(e, "pane_hash")
			if since, err := time.Parse(time.RFC3339Nano, detailString(e, "pane_since")); err == nil && !since.Equal(t.HashSince) {
				t.HashSince = since
				t.Nudges = 0
			}
			t.Limited, _ = e.Details["limited"].(bool)
			if st := detailString(e, "state"); st != "" && t.State == "" {
				t.State, t.Since = State(st), e.Time
			}
		case KindTransition:
			t.State, t.Since = State(detailString(e, "to")), e.Time
			t.Breaker = t.State == CrashLooping
		case KindSkip:
			if k := detailString(e, "delivered"); k != "" {
				t.Delivered[k] = detailString(e, "outcome")
			}
			t.Skipped[detailString(e, "key")] = true
		case KindAction, KindWould:
			if e.Kind == KindWould && !s.shadow {
				continue
			}
			switch detailString(e, "action") {
			case ActRestart:
				t.Restarts = append(t.Restarts, e.Time)
				t.Hash, t.HashSince, t.Observed = "", e.Time, true
			case ActNudge:
				t.Nudges++
				t.LastNudge = e.Time
			case ActResume:
				t.Limited = false
			case ActEscalate:
				t.Posted[detailString(e, "key")] = true
			case ActOpen:
				ref := detailString(e, "ref")
				if ref == "" {
					ref = "would-open"
				}
				t.Delivered[detailString(e, "delivered")] = ref
			}
		}
	}
}

// Run passes until the context ends.
func (s *Supervisor) Run(ctx context.Context, once bool) {
	mode := "live"
	if s.shadow {
		mode = "shadow"
	}
	s.emit("", KindStart, map[string]any{"mode": mode, "sessions": s.cfg.SessionNames()})
	for {
		s.Pass()
		if once {
			break
		}
		select {
		case <-ctx.Done():
			s.emit("", KindStop, map[string]any{"reason": ctx.Err().Error()})
			return
		case <-time.After(s.cfg.PollInterval.Duration):
		}
	}
	s.emit("", KindStop, map[string]any{"reason": "once"})
}

// Pass observes and acts on every session once.
func (s *Supervisor) Pass() {
	for _, name := range s.cfg.SessionNames() {
		s.step(name, s.cfg.Sessions[name])
	}
}

func (s *Supervisor) observe(name string, sess Session) (Evidence, error) {
	var ev Evidence
	alive, err := s.tmux.HasSession(name)
	if err != nil {
		return ev, fmt.Errorf("has-session: %w", err)
	}
	ev.Alive = alive
	if ev.Status, err = ReadStatus(sess.Workdir + "/STATUS.md"); err != nil {
		return ev, fmt.Errorf("STATUS.md: %w", err)
	}
	if alive {
		pane, err := s.tmux.CapturePane(name, PaneLines)
		if err != nil {
			return ev, err
		}
		ev.PaneHash = HashPane(pane)
		ev.Limited = s.limitRe.MatchString(pane)
		ev.TrustPrompt = s.cfg.Policies.Trust.Enabled && s.trustRe.MatchString(pane)
	}
	if s.cfg.TranscriptDir != "" {
		ev.Transcript = ReadTranscript(TranscriptDir(s.cfg.TranscriptDir, sess.Cwd))
	}
	return ev, nil
}

func (s *Supervisor) step(name string, sess Session) {
	now := s.clock.Now()
	t := s.track(name)
	ev, err := s.observe(name, sess)
	if err != nil {
		s.emit(name, KindError, map[string]any{"error": err.Error()})
		return
	}
	state, reason := Classify(t, ev, s.cfg.Policies, now)
	s.logObserve(name, t, ev, state, reason, now)
	state = s.actOn(name, sess, t, ev, state, now)
	if state != t.State {
		s.emit(name, KindTransition, map[string]any{"from": string(t.State), "to": string(state), "reason": reason})
		t.State, t.Since = state, now
	}
}

func (s *Supervisor) logObserve(name string, t *Track, ev Evidence, state State, reason string, now time.Time) {
	d := map[string]any{
		"state":     string(state),
		"reason":    reason,
		"alive":     ev.Alive,
		"done":      ev.Status.Done,
		"questions": ev.Status.Questions != "",
		"limited":   ev.Limited,
		"trust":     ev.TrustPrompt,
		"head":      ev.Status.Head,
	}
	if ev.Alive {
		d["pane_hash"] = ev.PaneHash
		d["pane_since"] = t.HashSince.Format(time.RFC3339Nano)
	}
	if ev.Transcript.Found {
		d["transcript_role"] = ev.Transcript.Role
		d["transcript_stop"] = ev.Transcript.StopReason
		d["transcript_at"] = ev.Transcript.At.Format(time.RFC3339)
	}
	if s.cfg.Policies.Restart.Enabled {
		d["restarts"] = t.restartsInWindow(now, s.cfg.Policies.Restart.Window.Duration)
	}
	same := t.LastObs != nil && len(t.LastObs) == len(d)
	if same {
		for k, v := range d {
			if k == "reason" {
				continue
			}
			if t.LastObs[k] != v {
				same = false
				break
			}
		}
	}
	if same {
		return
	}
	t.LastObs = d
	d = copyDetails(d)
	d["unchanged_s"] = int(now.Sub(t.HashSince).Seconds())
	s.emit(name, KindObserve, d)
}

func copyDetails(d map[string]any) map[string]any {
	c := make(map[string]any, len(d)+1)
	for k, v := range d {
		c[k] = v
	}
	return c
}

// actOn applies the policy for the state and returns the state the session
// is in afterwards.
func (s *Supervisor) actOn(name string, sess Session, t *Track, ev Evidence, state State, now time.Time) State {
	pol := s.cfg.Policies
	if state != UsageLimited && t.Limited && ev.Alive {
		t.Limited = false
		s.act(name, ActResume, map[string]any{"message": pol.Nudge.ResumeMessage}, func() error {
			return s.tmux.Type(name, pol.Nudge.ResumeMessage)
		})
	}
	switch state {
	case Done:
		for _, d := range sess.Deliver {
			s.deliver(name, sess, d, ev, t, now)
		}
	case Failed:
		if !pol.Restart.Enabled {
			s.skipOnce(name, "restart:"+name, "restart policy disabled", t, nil)
			return state
		}
		n := t.restartsInWindow(now, pol.Restart.Window.Duration)
		if n > 0 {
			if since := now.Sub(t.lastRestart()); since < backoff(n, pol.Restart) {
				return state
			}
		}
		brief, status := sess.Workdir+"/BRIEF.md", sess.Workdir+"/STATUS.md"
		cmd := make([]string, len(pol.Restart.Command))
		for i, part := range pol.Restart.Command {
			cmd[i] = strings.NewReplacer("{brief}", brief, "{status}", status).Replace(part)
		}
		ok := s.act(name, ActRestart, map[string]any{"attempt": n + 1, "cwd": sess.Cwd}, func() error {
			return s.tmux.NewSession(name, sess.Cwd, shellJoin(cmd), pol.Restart.Width, pol.Restart.Height)
		})
		if ok {
			t.Restarts = append(t.Restarts, now)
			t.Hash, t.HashSince, t.Observed = "", now, true
			t.Nudges, t.LastNudge = 0, time.Time{}
			return Starting
		}
	case CrashLooping:
		key := fmt.Sprintf("crash-loop:%s:%d", name, t.lastRestart().Unix())
		msg := fmt.Sprintf("%s: tmux session gone again after %d restarts within %s; not restarting it again. Attach, fix the cause, and start it by hand.",
			name, pol.Restart.MaxRestarts, pol.Restart.Window)
		s.escalate(name, key, msg, t)
	case UsageLimited:
		t.Limited = true
	case Starting:
		if ev.TrustPrompt {
			s.act(name, ActTrust, map[string]any{"keys": pol.Trust.Keys}, func() error {
				return s.tmux.Keys(name, pol.Trust.Keys...)
			})
		}
	case BlockedOnOwner:
		key := fmt.Sprintf("questions:%s:%s", name, shortHash(HashPane(ev.Status.Questions)))
		q := ev.Status.Questions
		if len(q) > 2000 {
			q = q[:2000] + "…"
		}
		s.escalate(name, key, fmt.Sprintf("%s is blocked on you. Questions from STATUS.md:\n%s", name, q), t)
	case Idle:
		if !pol.Nudge.Enabled {
			return state
		}
		if t.Nudges >= pol.Nudge.Max {
			key := fmt.Sprintf("stalled:%s:%d", name, t.HashSince.Unix())
			msg := fmt.Sprintf("%s: screen unchanged since %s and %d nudges went unanswered; not nudging again.",
				name, t.HashSince.Format(time.RFC3339), t.Nudges)
			s.escalate(name, key, msg, t)
			return state
		}
		if !t.LastNudge.IsZero() && now.Sub(t.LastNudge) < pol.Nudge.Stall.Duration {
			return state
		}
		ok := s.act(name, ActNudge, map[string]any{"nudge": t.Nudges + 1, "message": pol.Nudge.Message}, func() error {
			if err := s.tmux.Type(name, pol.Nudge.Message); err != nil {
				return err
			}
			// Absorb the echo of the typed line, so it is not mistaken for progress.
			s.clock.Sleep(time.Second)
			if pane, err := s.tmux.CapturePane(name, PaneLines); err == nil {
				t.Hash = HashPane(pane)
			}
			return nil
		})
		if ok {
			t.Nudges++
			t.LastNudge = now
		}
	}
	return state
}

// escalate posts one decision to the console, once per key.
func (s *Supervisor) escalate(name, key, message string, t *Track) {
	if t.Posted[key] {
		return
	}
	console := s.cfg.Console(name)
	tmpl := s.cfg.Policies.Escalate.Command
	if len(tmpl) == 0 {
		s.skipOnce(name, key, "no escalate command configured", t, map[string]any{"message": message})
		t.Posted[key] = true
		return
	}
	argv := make([]string, len(tmpl))
	for i, part := range tmpl {
		argv[i] = strings.NewReplacer("{console}", console, "{message}", message).Replace(part)
	}
	ok := s.act(name, ActEscalate, map[string]any{"key": key, "console": console, "message": message}, func() error {
		return s.runner.Run(argv)
	})
	if ok {
		t.Posted[key] = true
	}
}

// deliver pushes a finished session's branch and opens a merge request,
// once per worktree, when the deliver spec is met.
func (s *Supervisor) deliver(name string, sess Session, d Deliver, ev Evidence, t *Track, now time.Time) {
	key := name + ":" + d.Worktree
	if t.Delivered[key] != "" {
		return
	}
	details := map[string]any{"delivered": key, "repository": d.Repository, "worktree": d.Worktree, "target": d.Target}
	if sess.Hold != "" {
		s.skipOnce(name, "hold:"+key, "delivery held: "+sess.Hold, t, map[string]any{"worktree": d.Worktree})
		return
	}
	for _, write := range []string{"push", "open"} {
		if err := s.forge.allowed(d.Repository, write); err != nil {
			t.Delivered[key] = "not-allowed"
			details["outcome"] = "not-allowed"
			s.skipOnce(name, "deliver:"+key+":not-allowed", err.Error(), t, details)
			s.escalate(name, "deliver:"+key+":not-allowed", fmt.Sprintf("%s is DONE but %s; nothing delivered.", name, err), t)
			return
		}
	}
	if _, err := os.Stat(d.MRFile); err != nil {
		// Not final: the text may still be written, and the stat is cheap.
		s.skipOnce(name, "deliver:"+key+":no-mr-text", "DONE but the merge-request text is missing: "+d.MRFile, t, map[string]any{"worktree": d.Worktree, "mr_file": d.MRFile})
		s.escalate(name, "deliver:"+key+":no-mr-text", fmt.Sprintf("%s is DONE but %s is missing; nothing delivered.", name, d.MRFile), t)
		return
	}
	if !s.act(name, ActFetch, map[string]any{"worktree": d.Worktree}, func() error { return s.git.Fetch(d.Worktree) }) {
		return
	}
	clean, err := s.git.Clean(d.Worktree)
	if err != nil {
		s.emit(name, KindError, map[string]any{"error": err.Error()})
		return
	}
	if !clean {
		s.skipOnce(name, "deliver:"+key+":dirty", "worktree is not clean; not delivering", t, map[string]any{"worktree": d.Worktree})
		return
	}
	branch, err := s.git.Branch(d.Worktree)
	if err != nil {
		s.emit(name, KindError, map[string]any{"error": err.Error()})
		return
	}
	ahead, err := s.git.Ahead(d.Worktree, d.Target)
	if err != nil {
		s.emit(name, KindError, map[string]any{"error": err.Error()})
		return
	}
	if ahead == 0 {
		t.Delivered[key] = "nothing"
		details["outcome"] = "nothing"
		s.skipOnce(name, "deliver:"+key+":nothing", fmt.Sprintf("%s has nothing ahead of %s; not delivering", branch, d.Target), t, details)
		return
	}
	title, err := s.git.FirstSubject(d.Worktree, d.Target)
	if err != nil {
		s.emit(name, KindError, map[string]any{"error": err.Error()})
		return
	}
	draft := d.Draft || ev.Status.Questions != ""
	if draft {
		title = "Draft: " + title
	}
	body, err := os.ReadFile(d.MRFile)
	if err != nil {
		s.emit(name, KindError, map[string]any{"error": err.Error()})
		return
	}
	repo := s.cfg.Repositories[d.Repository]
	if !s.act(name, ActPush, map[string]any{"branch": branch, "repository": d.Repository}, func() error {
		return s.forge.PushAllowed(d.Repository, d.Worktree, branch)
	}) {
		return
	}
	mr := MergeRequest{Project: repo.Project, Source: branch, Target: d.Target, Title: title, Description: string(body), Assignee: repo.Assignee}
	var res MergeResult
	od := map[string]any{"delivered": key, "repository": d.Repository, "branch": branch, "target": d.Target, "title": title, "draft": draft}
	if s.shadow {
		s.act(name, ActOpen, od, nil)
		t.Delivered[key] = "would-open"
		return
	}
	ok := s.act(name, ActOpen, od, func() (err error) {
		res, err = s.forge.OpenAllowed(d.Repository, mr, d.Worktree)
		if err == nil {
			od["ref"], od["url"] = res.Ref, res.URL
		}
		return err
	})
	if ok {
		t.Delivered[key] = res.Ref
	}
}

// shellJoin quotes argv for the shell tmux hands a new session's command to.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"$`\\!#&|;<>()*?[]{}~") {
			parts[i] = a
			continue
		}
		parts[i] = "'" + strings.ReplaceAll(a, "'", "'\\''") + "'"
	}
	return strings.Join(parts, " ")
}
