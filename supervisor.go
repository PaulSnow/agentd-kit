package main

import (
	"context"
	"errors"
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

// PaneLines is how many lines of history above the visible screen go into
// the screen hash. The patterns read the visible screen only, so a prompt
// that has scrolled away is never answered.
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

	// halt is the first failed append of the pass. Once set, nothing more
	// is done until the next pass: an action whose record could be lost
	// could be repeated after a replay.
	halt error

	tracks       map[string]*Track
	limitRe      *regexp.Regexp
	trustRe      *regexp.Regexp
	dialogs      []*regexp.Regexp
	confidential []*regexp.Regexp
}

// NewSupervisor wires the supervisor. The forge is wrapped with the
// repository allow-list here, so no caller can reach it unwrapped.
func NewSupervisor(cfg *Config, log *Log, clock Clock, tmux Tmux, git Git, forge Forge, runner Runner, shadow bool) *Supervisor {
	var confidential, dialogs []*regexp.Regexp
	for _, p := range cfg.ConfidentialPatterns {
		confidential = append(confidential, regexp.MustCompile(p))
	}
	for _, p := range cfg.Policies.Dialog.Patterns {
		dialogs = append(dialogs, regexp.MustCompile(p))
	}
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
		dialogs: dialogs,

		confidential: confidential,
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

// mode is the mode this run tags its events with.
func (s *Supervisor) mode() string {
	if s.shadow {
		return ModeShadow
	}
	return ModeLive
}

func (s *Supervisor) emit(name, kind string, details map[string]any) {
	d := make(map[string]any, len(details)+1)
	for k, v := range details {
		d[k] = v
	}
	d["mode"] = s.mode()
	e := Event{Time: s.clock.Now(), Session: name, Kind: kind, Details: d}
	if err := s.log.Append(e); err != nil {
		fmt.Fprintf(os.Stderr, "agentd: event log: %v\n", err)
		if s.halt == nil {
			s.halt = fmt.Errorf("event log append failed; stopping this pass: %w", err)
		}
	}
}

// act performs one action, or in shadow mode records that it would. The
// intent is recorded before the action and the outcome after; when a record
// cannot be written the pass stops, and an action whose intent was not
// recorded is not taken. It returns the action's error; nil means the action
// counts as taken.
func (s *Supervisor) act(name, action string, details map[string]any, do func() error) error {
	return s.perform(name, action, details, s.shadow, do)
}

// actRead performs an action that writes nothing anyone else sees, a fetch,
// in shadow mode too, so the shadow decides from current refs.
func (s *Supervisor) actRead(name, action string, details map[string]any, do func() error) error {
	return s.perform(name, action, details, false, do)
}

func (s *Supervisor) perform(name, action string, details map[string]any, would bool, do func() error) error {
	if s.halt != nil {
		return s.halt
	}
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
	if would {
		record(KindWould, nil)
		return nil
	}
	if record(KindIntent, nil); s.halt != nil {
		return s.halt
	}
	if err := do(); err != nil {
		record(KindError, err)
		return err
	}
	record(KindAction, nil)
	return nil
}

// retryDue says whether a keyed action that has failed may be tried again:
// the first try is immediate, later ones wait the restart policy's backoff.
func (s *Supervisor) retryDue(t *Track, key string, now time.Time) bool {
	f := t.Failures[key]
	if len(f) == 0 {
		return true
	}
	return now.Sub(f[len(f)-1]) >= backoff(len(f), s.cfg.Policies.Restart)
}

// failed records a failed try of a keyed action and says whether the
// policy's MaxRestarts tries are used up.
func (s *Supervisor) failed(t *Track, key string, now time.Time) bool {
	t.Failures[key] = append(t.Failures[key], now)
	return len(t.Failures[key]) >= s.cfg.Policies.Restart.MaxRestarts
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

// Replay rebuilds the per-session memory from the event log. A run reads
// only the events of its own mode, plus the owner's records: live and shadow
// never share state. In shadow mode the would events count as if taken, so
// the shadow keeps its own counters.
func (s *Supervisor) Replay(events []Event) {
	for _, e := range events {
		if e.Session == "" {
			continue
		}
		if m := detailString(e, "mode"); m != s.mode() && m != ModeOwner {
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
			asks, _ := e.Details["questions"].(bool)
			done, _ := e.Details["done"].(bool)
			t.Questions.observeQuestions(asks, done)
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
			if k := detailString(e, "escalation"); k != "" {
				t.Posted[k] = true // given up on
				t.replayQuestionsPost(e)
			}
			t.Skipped[detailString(e, "key")] = true
		case KindError:
			if k := detailString(e, "key"); k != "" {
				t.Failures[k] = append(t.Failures[k], e.Time)
			}
			if detailString(e, "action") == ActRestart {
				t.Restarts = append(t.Restarts, e.Time)
			}
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
			case ActTrust:
				t.TrustHash = detailString(e, "pane_hash")
			case ActEscalate, ActUpdate:
				t.Posted[detailString(e, "key")] = true
				t.replayQuestionsPost(e)
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

// replayQuestionsPost settles a Questions escalation or update: posted, or
// given up on, it carries the hash of the text it covered.
func (t *Track) replayQuestionsPost(e Event) {
	h := detailString(e, "questions_hash")
	if h == "" {
		return
	}
	t.Questions.Hash = h
	if n, ok := detailInt(e, "update"); ok {
		t.Questions.Updates = n
	}
}

// Run passes until the context ends. A pass stopped by a failed append is
// reported and the next pass tries again; with once, its error is returned.
func (s *Supervisor) Run(ctx context.Context, once bool) error {
	s.emit("", KindStart, map[string]any{"sessions": s.cfg.SessionNames()})
	for {
		err := s.Pass()
		if err != nil {
			fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		}
		if once {
			s.emit("", KindStop, map[string]any{"reason": "once"})
			return err
		}
		select {
		case <-ctx.Done():
			s.emit("", KindStop, map[string]any{"reason": ctx.Err().Error()})
			return nil
		case <-time.After(s.cfg.PollInterval.Duration):
		}
	}
}

// Pass observes and acts on every session once. When an append to the
// event log fails it stops, acting on nothing more, and returns the error.
func (s *Supervisor) Pass() error {
	s.halt = nil
	for _, name := range s.cfg.SessionNames() {
		if s.halt != nil {
			break
		}
		s.step(name, s.cfg.Sessions[name])
	}
	return s.halt
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
		screen, err := s.tmux.CapturePane(name, 0)
		if err != nil {
			return ev, err
		}
		ev.Limited = s.limitRe.MatchString(screen)
		ev.TrustPrompt = s.cfg.Policies.Trust.Enabled && s.trustRe.MatchString(screen)
		for _, re := range s.dialogs {
			if re.MatchString(screen) {
				ev.Dialog = re.String()
				break
			}
		}
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
	if s.logObserve(name, t, ev, state, reason, now); s.halt != nil {
		return
	}
	t.Questions.observeQuestions(ev.Status.Questions != "", ev.Status.Done)
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
		"dialog":    ev.Dialog != "",
		"head":      ev.Status.Head,
	}
	if s.confidentialMatch(ev.Status.Head) > 0 {
		d["head"] = "(withheld: matches a confidential pattern)"
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
	if !t.HashSince.IsZero() { // unknown until the screen is first seen
		d["unchanged_s"] = int(now.Sub(t.HashSince).Seconds())
	}
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
	// The limit has cleared. Only a session that can take a line is told to
	// go on: never one that is DONE, waits on the owner, or shows a dialog.
	if t.Limited && ev.Alive && (state == Working || state == Idle) {
		if s.typeInto(name, ActResume, map[string]any{"message": pol.Nudge.ResumeMessage}, t, func() error {
			return s.tmux.Type(name, pol.Nudge.ResumeMessage)
		}) == nil {
			t.Limited = false
		}
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
		err := s.act(name, ActRestart, map[string]any{"attempt": n + 1, "cwd": sess.Cwd}, func() error {
			return s.tmux.NewSession(name, sess.Cwd, shellJoin(cmd), pol.Restart.Width, pol.Restart.Height)
		})
		t.Restarts = append(t.Restarts, now) // a failed attempt counts: backoff and the breaker apply to it
		if err == nil {
			t.Hash, t.HashSince, t.Observed = "", now, true
			t.Nudges, t.LastNudge = 0, time.Time{}
			return Starting
		}
	case CrashLooping:
		key := fmt.Sprintf("crash-loop:%s:%d", name, t.lastRestart().Unix())
		msg := fmt.Sprintf("%s: tmux session gone again after %d restart attempts within %s; not restarting it again. Attach, fix the cause, and start it by hand.",
			name, pol.Restart.MaxRestarts, pol.Restart.Window)
		s.escalate(name, key, msg, t, now)
	case UsageLimited:
		t.Limited = true
	case Starting:
		// Once per screen: a prompt still showing the same screen after the
		// keys did not take them, and sending them again could answer
		// whatever replaced it.
		if ev.TrustPrompt && ev.PaneHash != t.TrustHash {
			if s.typeInto(name, ActTrust, map[string]any{"keys": pol.Trust.Keys, "pane_hash": ev.PaneHash}, t, func() error {
				return s.tmux.Keys(name, pol.Trust.Keys...)
			}) == nil {
				t.TrustHash = ev.PaneHash
			}
		}
	case BlockedOnOwner:
		switch {
		case ev.Status.Questions != "":
			s.escalateQuestions(name, ev.Status.Questions, t, now)
		case ev.Dialog != "":
			s.escalate(name, fmt.Sprintf("dialog:%s:%s", name, shortHash(ev.PaneHash)),
				fmt.Sprintf("%s is stopped at a permission or confirmation dialog (the screen matches %q); agentd will not answer it. Attach and answer it.", name, ev.Dialog), t, now)
		default:
			s.escalate(name, fmt.Sprintf("tool-wait:%s:%d", name, t.HashSince.Unix()),
				fmt.Sprintf("%s has shown the same screen since %s with a tool call as its last turn; it is probably waiting at a permission prompt. Attach and answer it.", name, t.HashSince.Format(time.RFC3339)), t, now)
		}
	case Idle:
		if !pol.Nudge.Enabled {
			return state
		}
		if t.Nudges >= pol.Nudge.Max {
			key := fmt.Sprintf("stalled:%s:%d", name, t.HashSince.Unix())
			msg := fmt.Sprintf("%s: screen unchanged since %s and %d nudges went unanswered; not nudging again.",
				name, t.HashSince.Format(time.RFC3339), t.Nudges)
			s.escalate(name, key, msg, t, now)
			return state
		}
		if !t.LastNudge.IsZero() && now.Sub(t.LastNudge) < pol.Nudge.Stall.Duration {
			return state
		}
		err := s.typeInto(name, ActNudge, map[string]any{"nudge": t.Nudges + 1, "message": pol.Nudge.Message}, t, func() error {
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
		if err == nil {
			t.Nudges++
			t.LastNudge = now
		}
	}
	return state
}

// errNotClaude stops a typing action whose pane does not run Claude.
var errNotClaude = errors.New("the pane does not run Claude")

// typeInto performs an action that types or sends keys, only after asking
// tmux what the pane runs: a pane running anything but a configured Claude
// command is skipped, once per command, with a skip naming it. The check is
// a read, so shadow mode makes it too.
func (s *Supervisor) typeInto(name, action string, details map[string]any, t *Track, do func() error) error {
	if s.halt != nil {
		return s.halt
	}
	info, err := s.tmux.Pane(name)
	if err != nil {
		s.emit(name, KindError, map[string]any{"action": action, "step": "pane", "error": err.Error()})
		return err
	}
	for _, c := range s.cfg.Policies.PaneCommands {
		if info.Command == c {
			return s.act(name, action, details, do)
		}
	}
	s.skipOnce(name, "pane-command:"+name+":"+info.Command,
		fmt.Sprintf("pane runs %q, not one of %s; not typing into it", info.Command, strings.Join(s.cfg.Policies.PaneCommands, ", ")), t,
		map[string]any{"action": action, "command": info.Command})
	return errNotClaude
}

// confidentialMatch returns the number, from 1, of the first confidential
// pattern that matches any of texts, or 0.
func (s *Supervisor) confidentialMatch(texts ...string) int {
	for i, re := range s.confidential {
		for _, text := range texts {
			if re.MatchString(text) {
				return i + 1
			}
		}
	}
	return 0
}

// escalateQuestions posts the session's open Questions once per episode,
// and each later change of their text as one short update, at most one per
// pass. Both carry the hash of the text they cover, so replay knows which
// text the console has seen.
func (s *Supervisor) escalateQuestions(name, questions string, t *Track, now time.Time) {
	q := &t.Questions
	hash := shortHash(HashPane(questions))
	if len(questions) > 2000 {
		questions = questions[:2000] + "…"
	}
	key := fmt.Sprintf("questions:%s:%d", name, q.Episode)
	if !t.Posted[key] {
		if s.post(name, ActEscalate, key, fmt.Sprintf("%s is blocked on you. Questions from STATUS.md:\n%s", name, questions), t, now,
			map[string]any{"questions_hash": hash}) {
			q.Hash = hash
		}
		return
	}
	if hash == q.Hash {
		return
	}
	n := q.Updates + 1
	if s.post(name, ActUpdate, fmt.Sprintf("%s:update:%d", key, n), fmt.Sprintf("%s changed its open Questions in STATUS.md; they now read:\n%s", name, questions), t, now,
		map[string]any{"questions_hash": hash, "update": n}) {
		q.Hash, q.Updates = hash, n
	}
}

// escalate posts one decision to the console, once per key.
func (s *Supervisor) escalate(name, key, message string, t *Track, now time.Time) {
	s.post(name, ActEscalate, key, message, t, now, nil)
}

// post sends one message to the console as action, once per key, and says
// whether the key is settled: posted, or given up on. A failing command is
// retried on the backoff schedule and given up after MaxRestarts tries,
// with one skip saying so. A message that matches a confidential pattern is
// replaced, before it is posted or logged, by one that names only the
// session; one skip records that it was filtered, without the text. extra
// details go on the events that settle the key.
func (s *Supervisor) post(name, action, key, message string, t *Track, now time.Time, extra map[string]any) bool {
	if t.Posted[key] {
		return true
	}
	with := func(d map[string]any) map[string]any {
		for k, v := range extra {
			d[k] = v
		}
		return d
	}
	if i := s.confidentialMatch(message); i > 0 {
		var what string
		what, message = "escalation", fmt.Sprintf("%s needs your attention; the details were withheld because they matched a confidential pattern. Look at the session directly.", name)
		if action == ActUpdate {
			what, message = "update", fmt.Sprintf("%s changed its open Questions; the details were withheld because they matched a confidential pattern. Look at the session directly.", name)
		}
		s.skipOnce(name, key+":filtered", fmt.Sprintf("%s text matched confidential pattern %d; posted a generic message instead", what, i), t,
			map[string]any{"filtered": key})
	}
	console := s.cfg.Console(name)
	tmpl := s.cfg.Policies.Escalate.Command
	if len(tmpl) == 0 {
		s.skipOnce(name, key, "no escalate command configured", t, with(map[string]any{"escalation": key, "message": message}))
		t.Posted[key] = true
		return true
	}
	if !s.retryDue(t, key, now) {
		return false
	}
	argv := make([]string, len(tmpl))
	for i, part := range tmpl {
		argv[i] = strings.NewReplacer("{console}", console, "{message}", message).Replace(part)
	}
	err := s.act(name, action, with(map[string]any{"key": key, "console": console, "message": message}), func() error {
		return s.runner.Run(argv)
	})
	if err == nil {
		t.Posted[key] = true
		return true
	}
	if s.failed(t, key, now) {
		t.Posted[key] = true
		s.skipOnce(name, key+":gave-up", fmt.Sprintf("escalation command failed %d times; not retrying: %v", len(t.Failures[key]), err), t,
			with(map[string]any{"escalation": key, "console": console, "message": message}))
		return true
	}
	return false
}

// deliver pushes a finished session's branch and opens a merge request,
// once per worktree, when the deliver spec is met. A step that fails is
// retried on the backoff schedule; after MaxRestarts failures the delivery
// is given up and escalated once.
func (s *Supervisor) deliver(name string, sess Session, d Deliver, ev Evidence, t *Track, now time.Time) {
	key := name + ":" + d.Worktree
	if t.Delivered[key] != "" {
		return
	}
	fkey := "deliver:" + key
	if !s.retryDue(t, fkey, now) {
		return
	}
	details := map[string]any{"delivered": key, "repository": d.Repository, "worktree": d.Worktree, "target": d.Target}
	if sess.Hold != "" {
		s.skipOnce(name, "hold:"+key, "delivery held: "+sess.Hold, t, map[string]any{"worktree": d.Worktree})
		return
	}
	for _, write := range []string{"push", "open"} {
		if err := s.forge.allowed(d.Repository, write); err != nil {
			// Not final: the allow-list may grant the write later, so this is
			// not recorded as a delivery outcome.
			s.skipOnce(name, "deliver:"+key+":not-allowed", err.Error(), t, map[string]any{"worktree": d.Worktree, "repository": d.Repository})
			s.escalate(name, "deliver:"+key+":not-allowed", fmt.Sprintf("%s is DONE but %s; nothing delivered.", name, err), t, now)
			return
		}
	}
	if _, err := os.Stat(d.MRFile); err != nil {
		// Not final: the text may still be written, and the stat is cheap.
		s.skipOnce(name, "deliver:"+key+":no-mr-text", "DONE but the merge-request text is missing: "+d.MRFile, t, map[string]any{"worktree": d.Worktree, "mr_file": d.MRFile})
		s.escalate(name, "deliver:"+key+":no-mr-text", fmt.Sprintf("%s is DONE but %s is missing; nothing delivered.", name, d.MRFile), t, now)
		return
	}
	// fail records a failed step and, when the tries are used up, gives the delivery up.
	fail := func(step string, err error) {
		if !s.failed(t, fkey, now) {
			return
		}
		n := len(t.Failures[fkey])
		t.Delivered[key] = "failed"
		s.skipOnce(name, fkey+":gave-up", fmt.Sprintf("delivery failed %d times; not retrying", n), t, map[string]any{"delivered": key, "outcome": "failed", "worktree": d.Worktree})
		s.escalate(name, fkey+":failed", fmt.Sprintf("%s is DONE but delivering %s failed %d times, last at %s: %v. Not retrying; deliver it by hand and run agentd mark-delivered.",
			name, d.Worktree, n, step, err), t, now)
	}
	// read reports a failed read of the worktree the same way.
	read := func(step string, err error) {
		s.emit(name, KindError, map[string]any{"key": fkey, "step": step, "error": err.Error()})
		fail(step, err)
	}
	if err := s.actRead(name, ActFetch, map[string]any{"key": fkey, "worktree": d.Worktree}, func() error { return s.git.Fetch(d.Worktree) }); err != nil {
		fail("fetch", err)
		return
	}
	clean, err := s.git.Clean(d.Worktree)
	if err != nil {
		read("status", err)
		return
	}
	if !clean {
		s.skipOnce(name, "deliver:"+key+":dirty", "worktree is not clean; not delivering", t, map[string]any{"worktree": d.Worktree})
		return
	}
	branch, err := s.git.Branch(d.Worktree)
	if err != nil {
		read("branch", err)
		return
	}
	// Pushing the target itself would write it directly, and a detached HEAD
	// has no branch to push. Not final: the session may move to a branch.
	if branch == "" || branch == "HEAD" || branch == d.Target {
		reason := fmt.Sprintf("worktree is on the target branch %s; not delivering", d.Target)
		if branch != d.Target {
			reason = "worktree has a detached HEAD; not delivering"
		}
		s.skipOnce(name, "deliver:"+key+":branch:"+branch, reason, t, map[string]any{"worktree": d.Worktree, "branch": branch})
		return
	}
	head, err := s.git.Head(d.Worktree)
	if err != nil {
		read("head", err)
		return
	}
	details["head"] = head
	reqs, err := s.forge.Find(d.Repository, branch, d.Target)
	if err != nil {
		read("find", err)
		return
	}
	ahead, err := s.git.Ahead(d.Worktree, d.Target)
	if err != nil {
		read("ahead", err)
		return
	}
	// A request that already delivers HEAD is the delivery: the log may have
	// missed it (a lost reply, a lost log), and a second push after a squash
	// merge would recreate the branch and open a duplicate. An open request on
	// the branch counts. A merged one counts when it carried HEAD, or when the
	// target contains its head and HEAD has nothing the target lacks, as after
	// the forge rebased it before merging. A reused branch name with new
	// commits is delivered again.
	for _, r := range reqs {
		delivers := !r.Merged || r.Head == head
		if !delivers && ahead == 0 && r.Head != "" {
			in, err := s.git.Contains(d.Worktree, r.Head, d.Target)
			if err != nil {
				read("contains", err)
				return
			}
			delivers = in
		}
		if delivers {
			t.Delivered[key] = r.Ref
			details["outcome"], details["url"], details["branch"] = r.Ref, r.URL, branch
			s.skipOnce(name, "deliver:"+key+":exists", fmt.Sprintf("%s already has merge request %s; not delivering again", branch, r.Ref), t, details)
			return
		}
	}
	if ahead == 0 {
		t.Delivered[key] = "nothing"
		details["outcome"] = "nothing"
		s.skipOnce(name, "deliver:"+key+":nothing", fmt.Sprintf("%s has nothing ahead of %s; not delivering", branch, d.Target), t, details)
		return
	}
	title, err := s.git.FirstSubject(d.Worktree, d.Target)
	if err != nil {
		read("subject", err)
		return
	}
	draft := d.Draft || ev.Status.Questions != ""
	if draft {
		title = "Draft: " + title
	}
	body, err := os.ReadFile(d.MRFile)
	if err != nil {
		read("mr-text", err)
		return
	}
	// Text bound for the forge that matches a confidential pattern is not
	// sent. Not final: delivery waits for the owner to change the text or
	// the patterns.
	if i := s.confidentialMatch(title, string(body)); i > 0 {
		s.skipOnce(name, "deliver:"+key+":confidential", fmt.Sprintf("merge request title or text matches confidential pattern %d; delivery waits for the owner", i), t,
			map[string]any{"worktree": d.Worktree, "mr_file": d.MRFile})
		s.escalate(name, "deliver:"+key+":confidential", fmt.Sprintf("%s is DONE but its merge request title or text (%s) matches confidential pattern %d; nothing delivered. Change the text or the patterns.",
			name, d.MRFile, i), t, now)
		return
	}
	repo := s.cfg.Repositories[d.Repository]
	if err := s.act(name, ActPush, map[string]any{"key": fkey, "branch": branch, "repository": d.Repository}, func() error {
		return s.forge.PushAllowed(d.Repository, d.Worktree, branch)
	}); err != nil {
		fail("push", err)
		return
	}
	mr := MergeRequest{Project: repo.Project, Source: branch, Target: d.Target, Title: title, Description: string(body), Assignee: repo.Assignee}
	var res MergeResult
	od := map[string]any{"key": fkey, "delivered": key, "repository": d.Repository, "branch": branch, "head": head, "target": d.Target, "title": title, "draft": draft}
	if s.shadow {
		s.act(name, ActOpen, od, nil)
		t.Delivered[key] = "would-open"
		return
	}
	err = s.act(name, ActOpen, od, func() (err error) {
		res, err = s.forge.OpenAllowed(d.Repository, mr, d.Worktree)
		if err == nil {
			od["ref"], od["url"] = res.Ref, res.URL
		}
		return err
	})
	if err != nil {
		fail("open", err)
		return
	}
	t.Delivered[key] = res.Ref
}

// MarkDelivered records a delivery made by other means, as the action event
// deliver would have written, so the worktree is never delivered again. The
// worktree must be one of the session's deliver specs. It is the owner's
// record of the world, so live and shadow runs both read it.
func MarkDelivered(cfg *Config, log *Log, now time.Time, session, worktree, ref string) error {
	sess, ok := cfg.Sessions[session]
	if !ok {
		return fmt.Errorf("session %q is not configured", session)
	}
	worktree = expandPath(worktree, "")
	known := false
	for _, d := range sess.Deliver {
		if d.Worktree == worktree {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("session %q has no deliver spec for worktree %s", session, worktree)
	}
	if ref == "" {
		return fmt.Errorf("the merge request reference must not be empty")
	}
	return log.Append(Event{Time: now, Session: session, Kind: KindAction, Details: map[string]any{
		"action": ActOpen, "delivered": session + ":" + worktree, "ref": ref, "by": "mark-delivered", "mode": ModeOwner}})
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
