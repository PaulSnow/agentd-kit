package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
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
	// tailOff is how far into the event log this run has read, so each
	// pass picks up what the owner wrote meanwhile: agentd clear.
	tailOff int64

	st           *LogState
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
		st:      NewLogState(cfg, mode(shadow)),
		tailOff: log.Size(),
		limitRe: regexp.MustCompile(cfg.Policies.UsageLimit.Pattern),
		trustRe: regexp.MustCompile(`^(?:` + cfg.Policies.Trust.Pattern + `)$`),
		dialogs: dialogs,

		confidential: confidential,
	}
}

func (s *Supervisor) track(name string) *Track { return s.st.track(name) }

// Tracks exposes the memory for the status command and tests.
func (s *Supervisor) Tracks() map[string]*Track { return s.st.Tracks }

// mode is the mode a run tags its events with.
func mode(shadow bool) string {
	if shadow {
		return ModeShadow
	}
	return ModeLive
}

func (s *Supervisor) mode() string { return mode(s.shadow) }

// emit appends e for session name, tagged with this run's version, time
// and mode, then applies it: the live path changes the state only through
// apply, as replay does. An event that could not be written is not
// applied, and stops the pass.
func (s *Supervisor) emit(name string, e Event) {
	e.V, e.Time, e.Session, e.Mode = EventVersion, s.clock.Now(), name, s.mode()
	if err := s.log.Append(e); err != nil {
		fmt.Fprintf(os.Stderr, "agentd: event log: %v\n", err)
		if s.halt == nil {
			s.halt = fmt.Errorf("event log append failed; stopping this pass: %w", err)
		}
		return
	}
	s.st.apply(e)
}

// act performs one action, or in shadow mode records that it would. The
// intent is recorded before the action and the outcome after; when a record
// cannot be written the pass stops, and an action whose intent was not
// recorded is not taken. It returns the action's error; nil means the action
// counts as taken.
func (s *Supervisor) act(name, action string, a *ActPayload, do func() error) error {
	return s.perform(name, action, a, s.shadow, do)
}

// actRead performs an action that writes nothing anyone else sees, a fetch,
// in shadow mode too, so the shadow decides from current refs.
func (s *Supervisor) actRead(name, action string, a *ActPayload, do func() error) error {
	return s.perform(name, action, a, false, do)
}

// perform records the intent, with an id its outcome repeats, runs do,
// and records the outcome: the action, or an error naming it. do may fill
// in a, as an open fills in the reference it got.
func (s *Supervisor) perform(name, action string, a *ActPayload, would bool, do func() error) error {
	if s.halt != nil {
		return s.halt
	}
	a.Action = action
	if would {
		s.emit(name, Event{Kind: KindWould, Act: a})
		return nil
	}
	a.Intent = newIntentID()
	intent := *a
	if s.emit(name, Event{Kind: KindIntent, Act: &intent}); s.halt != nil {
		return s.halt
	}
	if err := do(); err != nil {
		s.emit(name, Event{Kind: KindError, Error: &ErrorPayload{Key: a.Key, Error: err.Error(), Act: a}})
		return err
	}
	s.emit(name, Event{Kind: KindAction, Act: a})
	return nil
}

// newIntentID is a random id pairing an intent with its outcome.
func newIntentID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprint(time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
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

// failed says whether a keyed action's failed tries, each recorded by its
// error event, have used up the policy's MaxRestarts.
func (s *Supervisor) failed(t *Track, key string) bool {
	return len(t.Failures[key]) >= s.cfg.Policies.Restart.MaxRestarts
}

// skipOnce logs why a policy declined, once per key. facts, when given,
// are the facts it was declined on.
func (s *Supervisor) skipOnce(name, key, reason string, t *Track, facts *SkipPayload) {
	if t.Skipped[key] {
		return
	}
	p := SkipPayload{}
	if facts != nil {
		p = *facts
	}
	p.Key, p.Reason = key, reason
	s.emit(name, Event{Kind: KindSkip, Skip: &p})
}

// Replay rebuilds the per-session memory from the event log, through the
// same apply the live path uses. A run reads only the events of its own
// mode, plus the owner's records: live and shadow never share state.
func (s *Supervisor) Replay(events []Event) {
	s.st.Fold(events)
}

// surfaceCutShort logs a warning for each intent the log holds with no
// outcome: an action a crash of agentd cut short. apply counts what the
// owner or a session may have seen as done, so it is never repeated.
func (s *Supervisor) surfaceCutShort() {
	for _, e := range s.st.CutShort() {
		s.emit(e.Session, Event{Kind: KindWarning, Warning: &WarningPayload{
			Warning: "an action was cut short by a crash of agentd and has no recorded outcome; it is not repeated", Intent: e.Act}})
	}
}

// Run passes until the context ends. A pass stopped by a failed append is
// reported and the next pass tries again; with once, its error is returned.
func (s *Supervisor) Run(ctx context.Context, once bool) error {
	s.emit("", Event{Kind: KindStart, Start: &StartPayload{Sessions: s.cfg.SessionNames()}})
	s.surfaceCutShort()
	for {
		err := s.Pass()
		if err != nil {
			fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		}
		if once {
			s.emit("", Event{Kind: KindStop, Stop: &StopPayload{Reason: "once"}})
			return err
		}
		select {
		case <-ctx.Done():
			s.emit("", Event{Kind: KindStop, Stop: &StopPayload{Reason: ctx.Err().Error()}})
			return nil
		case <-time.After(s.cfg.PollInterval.Duration):
		}
	}
}

// Pass observes and acts on every session once. When an append to the
// event log fails it stops, acting on nothing more, and returns the error.
func (s *Supervisor) Pass() error {
	s.halt = nil
	s.ownerRecords()
	for _, name := range s.cfg.SessionNames() {
		if s.halt != nil {
			break
		}
		s.step(name, s.cfg.Sessions[name])
	}
	return s.halt
}

// ownerRecords applies what the owner wrote to the event log since the last
// pass: a clear lifts a session's park, and a mark-delivered record settles
// a delivery, while agentd runs.
func (s *Supervisor) ownerRecords() {
	events, off, err := s.log.Tail(s.tailOff)
	if err != nil {
		s.emit("", Event{Kind: KindError, Error: &ErrorPayload{Step: "reading owner records", Error: err.Error()}})
		return
	}
	s.tailOff = off
	for _, e := range events {
		if _, ok := s.cfg.Sessions[e.Session]; ok && e.Mode == ModeOwner {
			s.st.apply(e)
		}
	}
}

// questionLine says whether a line of screen, less its indent and any box
// border, is wholly the trust prompt's question: the same words quoted in a
// diff, a dialog's body or a session's output sit inside other text.
func questionLine(re *regexp.Regexp, screen string) bool {
	for _, line := range strings.Split(screen, "\n") {
		if re.MatchString(strings.Trim(line, " \t│┃|")) {
			return true
		}
	}
	return false
}

// deadPaneLine is tmux's own line on a dead pane, which carries the time.
var deadPaneLine = regexp.MustCompile(`(?m)^Pane is dead.*$`)

func (s *Supervisor) observe(name string, sess Session) (Evidence, error) {
	ev := Evidence{Exit: -1}
	alive, err := s.tmux.HasSession(name)
	if err != nil {
		return ev, fmt.Errorf("has-session: %w", err)
	}
	if alive {
		info, err := s.tmux.Pane(name)
		if err != nil {
			return ev, err
		}
		ev.Command = info.Command
		if info.Dead {
			// The command exited and remain-on-exit kept the pane: its exit
			// status and last screen are the crash's cause.
			alive, ev.PaneDead = false, true
			if info.HasStatus {
				ev.Exit = info.Status
			}
			screen, err := s.tmux.CapturePane(name, 0)
			if err != nil {
				return ev, err
			}
			ev.DeadScreen = shortHash(HashPane(deadPaneLine.ReplaceAllString(screen, "")))
		}
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
		ev.TrustPrompt = s.cfg.Policies.Trust.Enabled && questionLine(s.trustRe, screen)
		for _, re := range s.dialogs {
			if re.MatchString(screen) {
				ev.Dialog = re.String()
				break
			}
		}
	}
	if root := s.cfg.TranscriptDir; root != "" {
		// An account with its own config directory keeps its transcripts there.
		if a := s.cfg.Accounts[sess.Account]; sess.Account != "" && a.ConfigDir != "" {
			root = filepath.Join(a.ConfigDir, "projects")
		}
		ev.Transcript = ReadTranscript(TranscriptDir(root, sess.Cwd))
	}
	return ev, nil
}

func (s *Supervisor) step(name string, sess Session) {
	now := s.clock.Now()
	t := s.track(name)
	ev, err := s.observe(name, sess)
	if err != nil {
		s.emit(name, Event{Kind: KindError, Error: &ErrorPayload{Step: "observe", Error: err.Error()}})
		return
	}
	if !ev.Alive && t.Live && !ev.Status.Done {
		screen := ev.DeadScreen
		if !ev.PaneDead {
			screen = shortHash(t.Hash) // gone outright: the last screen seen
		}
		if s.crash(name, t, Crash{Exit: ev.Exit, Screen: screen}, false); s.halt != nil {
			return
		}
	}
	state, reason := Classify(t, ev, s.cfg.Policies, now)
	// The observe event, applied, ends a dialog episode and opens or
	// closes the Questions escalation: each is a field it compares.
	if s.logObserve(name, t, ev, state, reason, now); s.halt != nil {
		return
	}
	state = s.actOn(name, sess, t, ev, state, now)
	if ev.Alive && state != Done && state != CrashLooping {
		s.watchCommand(name, ev.Command, t, now)
	}
	if state != t.State {
		s.emit(name, Event{Kind: KindTransition, Transition: &TransitionPayload{From: string(t.State), To: string(state), Reason: reason}})
	}
}

// crash records one crash and its cause, and whether it parked the session.
func (s *Supervisor) crash(name string, t *Track, c Crash, restartError bool) {
	// The event says what the crash makes of the count; apply makes it so.
	after := Track{Crashes: append([]Crash(nil), t.Crashes...)}
	after.noteCrash(c, s.cfg.Policies.Restart.MaxCrashes)
	p := CrashPayload{ScreenHash: c.Screen, Crashes: len(after.Crashes), Parked: after.Parked, RestartError: restartError}
	if c.Exit >= 0 {
		exit := c.Exit
		p.Exit = &exit
	}
	s.emit(name, Event{Kind: KindCrash, Crash: &p})
}

func (s *Supervisor) logObserve(name string, t *Track, ev Evidence, state State, reason string, now time.Time) {
	o := ObservePayload{
		State:     string(state),
		Reason:    reason,
		Alive:     ev.Alive,
		Done:      ev.Status.Done,
		Questions: ev.Status.Questions != "",
		Limited:   ev.Limited,
		Trust:     ev.TrustPrompt,
		Dialog:    ev.Dialog != "",
		Command:   ev.Command,
		Head:      ev.Status.Head,
	}
	if s.confidentialMatch(ev.Status.Head) > 0 {
		o.Head = "(withheld: matches a confidential pattern)"
	}
	if ev.Alive {
		since := t.HashSince
		o.PaneHash, o.PaneSince = ev.PaneHash, &since
	}
	if ev.Transcript.Found {
		at := ev.Transcript.At
		o.TranscriptRole, o.TranscriptStop, o.TranscriptAt = ev.Transcript.Role, ev.Transcript.StopReason, &at
	}
	if s.cfg.Policies.Restart.Enabled {
		n := t.restartsInWindow(now, s.cfg.Policies.Restart.Window.Duration)
		o.Restarts = &n
	}
	if t.LastObs != nil && sameObservation(*t.LastObs, o) {
		return
	}
	t.LastObs = &o
	logged := o
	if !t.HashSince.IsZero() { // unknown until the screen is first seen
		n := int(now.Sub(t.HashSince).Seconds())
		logged.UnchangedS = &n
	}
	s.emit(name, Event{Kind: KindObserve, Observe: &logged})
}

// sameObservation says whether two observations differ only in their
// reason, which rewords as time passes and is not evidence of its own.
func sameObservation(a, b ObservePayload) bool {
	a.Reason, b.Reason = "", ""
	eqTime := func(x, y *time.Time) bool { return (x == nil) == (y == nil) && (x == nil || x.Equal(*y)) }
	eqInt := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	if !eqTime(a.PaneSince, b.PaneSince) || !eqTime(a.TranscriptAt, b.TranscriptAt) || !eqInt(a.Restarts, b.Restarts) {
		return false
	}
	a.PaneSince, b.PaneSince, a.TranscriptAt, b.TranscriptAt, a.Restarts, b.Restarts = nil, nil, nil, nil, nil, nil
	return a == b
}

func (s *Supervisor) actOn(name string, sess Session, t *Track, ev Evidence, state State, now time.Time) State {
	pol := s.cfg.Policies
	// The limit has cleared. Only a session that can take a line is told to
	// go on: never one that is DONE, waits on the owner, or shows a dialog.
	if t.Limited && ev.Alive && (state == Working || state == Idle) {
		s.typeInto(name, ActResume, &ActPayload{Message: pol.Nudge.ResumeMessage}, t, ev.PaneHash, func() error {
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
		line, err := s.launchLine(sess, cmd)
		if err != nil {
			// Not final, and not a crash: the owner may fix the account.
			// There is no fallback to another account.
			s.skipOnce(name, "account:"+name, "not restarting: "+err.Error(), t, &SkipPayload{Account: sess.Account})
			s.escalate(name, "account:"+name, fmt.Sprintf("%s is gone and cannot be restarted under its account: %v. Fix the account; agentd will not use another.", name, err), t, now)
			return state
		}
		err = s.act(name, ActRestart, &ActPayload{Attempt: n + 1, Cwd: sess.Cwd, Respawn: ev.PaneDead, Account: sess.Account, Command: line}, func() error {
			if ev.PaneDead {
				return s.tmux.Respawn(name, sess.Cwd, line)
			}
			return s.tmux.NewSession(name, sess.Cwd, line, pol.Restart.Width, pol.Restart.Height)
		})
		if s.halt != nil {
			return state
		}
		// A failed attempt counts toward the backoff too: its error event
		// is applied as a restart.
		if s.shadow {
			// Nothing was started, so the session still gone next pass has
			// not crashed: it is not live, and stays failed.
			return state
		}
		if err == nil {
			return Starting
		}
		// A restart that fails is a crash, its error the cause: the same
		// error twice parks the session like any repeated crash.
		if s.crash(name, t, Crash{Exit: -1, Screen: "restart:" + shortHash(HashPane(err.Error()))}, true); t.Parked {
			return CrashLooping
		}
	case CrashLooping:
		last := t.Crashes[len(t.Crashes)-1]
		exit := "unknown"
		if last.Exit >= 0 {
			exit = fmt.Sprint(last.Exit)
		}
		key := fmt.Sprintf("crash-loop:%s:%d", name, t.Clears)
		msg := fmt.Sprintf("%s crashed %d times (last exit status %s, last screen %s); parked, not restarting or typing into it. Fix the cause, then run agentd clear %s.",
			name, len(t.Crashes), exit, last.Screen, name)
		s.escalate(name, key, msg, t, now)
	case UsageLimited:
		// The observe event, applied, remembers the limit.
	case Starting:
		// Once per screen: a prompt still showing the same screen after the
		// keys did not take them, and sending them again could answer
		// whatever replaced it.
		if ev.TrustPrompt && ev.PaneHash != t.TrustHash {
			s.typeInto(name, ActTrust, &ActPayload{Keys: pol.Trust.Keys, PaneHash: ev.PaneHash}, t, ev.PaneHash, func() error {
				return s.tmux.Keys(name, pol.Trust.Keys...)
			})
		}
	case BlockedOnOwner:
		switch {
		case ev.Status.Questions != "":
			s.escalateQuestions(name, ev.Status.Questions, t, now)
		case ev.Dialog != "":
			s.escalateDialog(name, ev.Dialog, t, now)
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
		s.typeInto(name, ActNudge, &ActPayload{Nudge: t.Nudges + 1, Message: pol.Nudge.Message}, t, ev.PaneHash, func() error {
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
	}
	return state
}

// noteCommand follows how long the pane has run something not in commands.
func (t *Track) noteCommand(cmd string, commands []string, now time.Time) {
	for _, c := range commands {
		if cmd == c {
			t.NotClaudeSince = time.Time{}
			return
		}
	}
	if t.NotClaudeSince.IsZero() {
		t.NotClaudeSince = now
	}
}

// watchCommand escalates once when the pane has run something that is not
// Claude, such as the shell left after Claude exited, for the stall time:
// nothing is typed into it, so without a word it would sit there unseen.
func (s *Supervisor) watchCommand(name, cmd string, t *Track, now time.Time) {
	t.noteCommand(cmd, s.cfg.Policies.PaneCommands, now)
	since := t.NotClaudeSince
	if since.IsZero() || now.Sub(since) < s.cfg.Policies.Nudge.Stall.Duration {
		return
	}
	s.escalate(name, fmt.Sprintf("not-claude:%s:%d", name, since.Unix()),
		fmt.Sprintf("%s's pane has run %q, not Claude, since %s; agentd does not type into it. Attach and see what happened.", name, cmd, since.Format(time.RFC3339)), t, now)
}

// errNotClaude stops a typing action whose pane does not run Claude, and
// errScreenChanged one whose screen changed after the pass read it.
var (
	errNotClaude     = errors.New("the pane does not run Claude")
	errScreenChanged = errors.New("the screen changed since it was read")
)

// typeInto performs an action that types or sends keys, only after asking
// tmux what the pane runs and reading the screen again: a pane running
// anything but a configured Claude command is skipped, once per command,
// with a skip naming it, and so is a screen whose hash is no longer seen,
// the one the decision was made from. The checks are reads, so shadow mode
// makes them too.
func (s *Supervisor) typeInto(name, action string, a *ActPayload, t *Track, seen string, do func() error) error {
	if s.halt != nil {
		return s.halt
	}
	info, err := s.tmux.Pane(name)
	if err != nil {
		s.emit(name, Event{Kind: KindError, Error: &ErrorPayload{Step: action + ": pane", Error: err.Error()}})
		return err
	}
	for _, c := range s.cfg.Policies.PaneCommands {
		if info.Command != c {
			continue
		}
		// The decision was made from the screen this pass read; if it has
		// changed since, a dialog may have opened, and a typed line or key
		// would answer it.
		pane, err := s.tmux.CapturePane(name, PaneLines)
		if err != nil {
			s.emit(name, Event{Kind: KindError, Error: &ErrorPayload{Step: action + ": capture", Error: err.Error()}})
			return err
		}
		if now := HashPane(pane); now != seen {
			s.skipOnce(name, "screen-changed:"+action+":"+shortHash(now), "the screen changed since it was read this pass; not typing", t,
				&SkipPayload{Action: action})
			return errScreenChanged
		}
		return s.act(name, action, a, do)
	}
	s.skipOnce(name, "pane-command:"+name+":"+info.Command,
		fmt.Sprintf("pane runs %q, not one of %s; not typing into it", info.Command, strings.Join(s.cfg.Policies.PaneCommands, ", ")), t,
		&SkipPayload{Action: action, Command: info.Command})
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
// pass and one per QuestionsUpdateGap, carrying the text current then. Both carry the hash of the text they cover, so replay knows which
// text the console has seen.
func (s *Supervisor) escalateQuestions(name, questions string, t *Track, now time.Time) {
	q := &t.Questions
	hash := shortHash(HashPane(questions))
	full := questions // screened whole by post, before the cut can hide a match
	questions = cutUTF8(questions, 2000)
	key := fmt.Sprintf("questions:%s:%d", name, q.Episode)
	if !t.Posted[key] {
		s.post(name, ActEscalate, key, fmt.Sprintf("%s is blocked on you. Questions from STATUS.md:\n%s", name, questions), t, now,
			settles{QuestionsHash: hash}, full)
		return
	}
	if hash == q.Hash || (!q.LastUpdate.IsZero() && now.Sub(q.LastUpdate) < QuestionsUpdateGap) {
		return
	}
	n := q.Updates + 1
	s.post(name, ActUpdate, fmt.Sprintf("%s:update:%d", key, n), fmt.Sprintf("%s changed its open Questions in STATUS.md; they now read:\n%s", name, questions), t, now,
		settles{QuestionsHash: hash, Update: n}, full)
}

// escalateDialog posts one escalation per dialog episode, keyed to the
// session rather than the screen, so a dialog whose screen keeps changing
// is one post, and at most one per DialogEscalationGap.
func (s *Supervisor) escalateDialog(name, pattern string, t *Track, now time.Time) {
	dq := &t.Dialog
	if dq.Open || (!dq.Last.IsZero() && now.Sub(dq.Last) < DialogEscalationGap) {
		return
	}
	n := dq.N + 1
	s.post(name, ActEscalate, fmt.Sprintf("dialog:%s:%d", name, n),
		fmt.Sprintf("%s is stopped at a permission or confirmation dialog (the screen matches %q); agentd will not answer it. Attach and answer it.", name, pattern),
		t, now, settles{Dialog: n})
}

// cutUTF8 cuts text to at most n bytes on a UTF-8 boundary, marking the cut.
func cutUTF8(text string, n int) string {
	if len(text) <= n {
		return text
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n] + "…"
}

// escalate posts one decision to the console, once per key.
func (s *Supervisor) escalate(name, key, message string, t *Track, now time.Time) {
	s.post(name, ActEscalate, key, message, t, now, settles{})
}

// post sends one message to the console as action, once per key, and says
// whether the key is settled: posted, or given up on. A failing command is
// retried on the backoff schedule and given up after MaxRestarts tries,
// with one skip saying so. A message that matches a confidential pattern is
// replaced, before it is posted or logged, by one that names only the
// session; one skip records that it was filtered, without the text. Each of
// also, the uncut text a message was cut from, is screened the same way.
// what goes on the events that settle the key, posted or given up on.
func (s *Supervisor) post(name, action, key, message string, t *Track, now time.Time, what settles, also ...string) bool {
	if t.Posted[key] {
		return true
	}
	if i := s.confidentialMatch(append(also, message)...); i > 0 {
		var what string
		what, message = "escalation", fmt.Sprintf("%s needs your attention; the details were withheld because they matched a confidential pattern. Look at the session directly.", name)
		if action == ActUpdate {
			what, message = "update", fmt.Sprintf("%s changed its open Questions; the details were withheld because they matched a confidential pattern. Look at the session directly.", name)
		}
		s.skipOnce(name, key+":filtered", fmt.Sprintf("%s text matched confidential pattern %d; posted a generic message instead", what, i), t,
			&SkipPayload{Filtered: key})
	}
	console := s.cfg.Console(name)
	tmpl := s.cfg.Policies.Escalate.Command // never empty: refused at load
	if !s.retryDue(t, key, now) {
		return false
	}
	argv := make([]string, len(tmpl))
	for i, part := range tmpl {
		argv[i] = strings.NewReplacer("{console}", console, "{message}", message).Replace(part)
	}
	err := s.act(name, action, &ActPayload{Key: key, Console: console, Message: message, QuestionsHash: what.QuestionsHash, Update: what.Update, Dialog: what.Dialog},
		func() error { return s.runner.Run(argv) })
	if err == nil {
		return true
	}
	if s.halt == nil && s.failed(t, key) {
		s.skipOnce(name, key+":gave-up", fmt.Sprintf("escalation command failed %d times; not retrying: %v", len(t.Failures[key]), err), t,
			&SkipPayload{Escalation: key, Console: console, Message: message, QuestionsHash: what.QuestionsHash, Update: what.Update, Dialog: what.Dialog})
		return true
	}
	return false
}

// settles is what an escalation's events record for the Questions or
// dialog episode it settles.
type settles struct {
	QuestionsHash string
	Update        int
	Dialog        int
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
	wt := &SkipPayload{Worktree: d.Worktree}
	if sess.Hold != "" {
		s.skipOnce(name, "hold:"+key, "delivery held: "+sess.Hold, t, wt)
		return
	}
	for _, write := range []string{"push", "open"} {
		if err := s.forge.allowed(d.Repository, write); err != nil {
			// Not final: the allow-list may grant the write later, so this is
			// not recorded as a delivery outcome.
			s.skipOnce(name, "deliver:"+key+":not-allowed", err.Error(), t, &SkipPayload{Worktree: d.Worktree, Repository: d.Repository})
			s.escalate(name, "deliver:"+key+":not-allowed", fmt.Sprintf("%s is DONE but %s; nothing delivered.", name, err), t, now)
			return
		}
	}
	if _, err := os.Stat(d.MRFile); err != nil {
		// Not final: the text may still be written, and the stat is cheap.
		s.skipOnce(name, "deliver:"+key+":no-mr-text", "DONE but the merge-request text is missing: "+d.MRFile, t, &SkipPayload{Worktree: d.Worktree, MRFile: d.MRFile})
		s.escalate(name, "deliver:"+key+":no-mr-text", fmt.Sprintf("%s is DONE but %s is missing; nothing delivered.", name, d.MRFile), t, now)
		return
	}
	// fail records a failed step and, when the tries are used up, gives the delivery up.
	// Each failed try was counted by its error event.
	fail := func(step string, err error) {
		if s.halt != nil || !s.failed(t, fkey) {
			return
		}
		n := len(t.Failures[fkey])
		s.skipOnce(name, fkey+":gave-up", fmt.Sprintf("delivery failed %d times; not retrying", n), t,
			&SkipPayload{Delivered: key, Outcome: "failed", Worktree: d.Worktree, Repository: d.Repository})
		s.escalate(name, fkey+":failed", fmt.Sprintf("%s is DONE but delivering %s failed %d times, last at %s: %v. Not retrying; deliver it by hand and run agentd mark-delivered.",
			name, d.Worktree, n, step, err), t, now)
	}
	// read reports a failed read of the worktree the same way.
	read := func(step string, err error) {
		s.emit(name, Event{Kind: KindError, Error: &ErrorPayload{Key: fkey, Step: step, Error: err.Error()}})
		fail(step, err)
	}
	// The push goes to the URL built from the configuration, whatever the
	// worktree says. Origin still decides what the fetch brings and what
	// counts as ahead, so every fetch and push URL it has must be the
	// configured project on the configured host. Not final: it may be fixed.
	remotes, err := s.git.RemoteURLs(d.Worktree)
	if err != nil {
		read("remote", err)
		return
	}
	repoCfg := s.cfg.Repositories[d.Repository]
	for _, remote := range remotes {
		if originIs(remote, repoCfg.Host, repoCfg.Project) {
			continue
		}
		shown := redactRemote(remote)
		s.skipOnce(name, "deliver:"+key+":origin:"+shortHash(HashPane(strings.Join(remotes, " "))),
			fmt.Sprintf("worktree's origin %s is not the project %s on %s; not delivering", shown, repoCfg.Project, repoCfg.Host), t,
			&SkipPayload{Worktree: d.Worktree, Origin: shown})
		return
	}
	if err := s.actRead(name, ActFetch, &ActPayload{Key: fkey, Worktree: d.Worktree}, func() error { return s.git.Fetch(d.Worktree) }); err != nil {
		fail("fetch", err)
		return
	}
	clean, err := s.git.Clean(d.Worktree)
	if err != nil {
		read("status", err)
		return
	}
	if !clean {
		s.skipOnce(name, "deliver:"+key+":dirty", "worktree is not clean; not delivering", t, wt)
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
		s.skipOnce(name, "deliver:"+key+":branch:"+branch, reason, t, &SkipPayload{Worktree: d.Worktree, Branch: branch})
		return
	}
	head, err := s.git.Head(d.Worktree)
	if err != nil {
		read("head", err)
		return
	}
	// The branch name and every commit message go to the forge with the
	// push, so they are screened before anything names them, in the log
	// or on the forge. Not final: delivery waits for the text or the
	// patterns to change.
	messages, err := s.git.Messages(d.Worktree, d.Target)
	if err != nil {
		read("messages", err)
		return
	}
	if i := s.confidentialMatch(branch, messages); i > 0 {
		s.skipOnce(name, "deliver:"+key+":confidential-commits", fmt.Sprintf("the branch name or a commit message ahead of %s matches confidential pattern %d; delivery waits for the owner", d.Target, i), t,
			wt)
		s.escalate(name, "deliver:"+key+":confidential-commits", fmt.Sprintf("%s is DONE but its branch name or a commit message ahead of %s matches confidential pattern %d; nothing pushed. Reword the commits or the branch, or change the patterns.",
			name, d.Target, i), t, now)
		return
	}
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
			s.skipOnce(name, "deliver:"+key+":exists", fmt.Sprintf("%s already has merge request %s; not delivering again", branch, r.Ref), t,
				&SkipPayload{Delivered: key, Outcome: r.Ref, URL: r.URL, Branch: branch, Head: head, Repository: d.Repository, Worktree: d.Worktree, Target: d.Target})
			return
		}
	}
	if ahead == 0 {
		s.skipOnce(name, "deliver:"+key+":nothing", fmt.Sprintf("%s has nothing ahead of %s; not delivering", branch, d.Target), t,
			&SkipPayload{Delivered: key, Outcome: "nothing", Branch: branch, Head: head, Repository: d.Repository, Worktree: d.Worktree, Target: d.Target})
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
			&SkipPayload{Worktree: d.Worktree, MRFile: d.MRFile})
		s.escalate(name, "deliver:"+key+":confidential", fmt.Sprintf("%s is DONE but its merge request title or text (%s) matches confidential pattern %d; nothing delivered. Change the text or the patterns.",
			name, d.MRFile, i), t, now)
		return
	}
	repo := s.cfg.Repositories[d.Repository]
	if !s.gate(name, key, fkey, d, repo, head, t, fail) {
		return
	}
	// A git config rewrite (insteadOf, pushInsteadOf) of the push URL would
	// send the push elsewhere; refuse rather than guess. Not final.
	rewrites, err := s.git.Rewrites(d.Worktree)
	if err != nil {
		read("rewrites", err)
		return
	}
	for _, prefix := range rewrites {
		if url := pushURL(repo); prefix != "" && strings.HasPrefix(url, prefix) {
			s.skipOnce(name, "deliver:"+key+":rewrite:"+shortHash(HashPane(prefix)), fmt.Sprintf("git config rewrites the push URL %s (prefix %s); not pushing", url, prefix), t,
				wt)
			return
		}
	}
	if err := s.act(name, ActPush, &ActPayload{Key: fkey, Branch: branch, Repository: d.Repository}, func() error {
		return s.forge.PushAllowed(d.Repository, d.Worktree, head, branch)
	}); err != nil {
		fail("push", err)
		return
	}
	mr := MergeRequest{Project: repo.Project, Source: branch, Target: d.Target, Title: title, Description: string(body), Assignee: repo.Assignee}
	var res MergeResult
	od := &ActPayload{Key: fkey, Delivered: key, Repository: d.Repository, Branch: branch, Head: head, Target: d.Target, Title: title, Draft: draft}
	if s.shadow {
		s.act(name, ActOpen, od, nil)
		return
	}
	err = s.act(name, ActOpen, od, func() (err error) {
		res, err = s.forge.OpenAllowed(d.Repository, mr, d.Worktree)
		if err == nil {
			od.Ref, od.URL = res.Ref, res.URL
		}
		return err
	})
	if err != nil {
		fail("open", err)
	}
}

// gate runs the repository's gate in the worktree on HEAD and says whether
// delivery may push: the gate exited 0 and the tree is still the one it
// ran on, HEAD unmoved and clean. Its command, commit, exit status and
// duration go on the gate's event. A failing gate is a failed delivery step;
// a tree that changed under it is skipped, and the next pass gates the new
// tree. Shadow mode logs the gate it would run and runs none.
func (s *Supervisor) gate(name, key, fkey string, d Deliver, repo Repository, head string, t *Track, fail func(string, error)) bool {
	if len(repo.Gate) == 0 { // refused at load; never deliver ungated
		s.skipOnce(name, "deliver:"+key+":no-gate", fmt.Sprintf("repository %s has no gate; not delivering", d.Repository), t, &SkipPayload{Worktree: d.Worktree})
		return false
	}
	timeout := repo.GateTimeout.Duration
	if timeout <= 0 {
		timeout = DefaultGateTimeout
	}
	// Failures count against the commit: a deterministic failure is given
	// up on that commit, and a new commit gets fresh tries.
	gkey := fkey + ":gate:" + head
	if len(t.Failures[gkey]) >= s.cfg.Policies.Restart.MaxRestarts || !s.retryDue(t, gkey, s.clock.Now()) {
		return false
	}
	config, err := s.git.ConfigHash(d.Worktree)
	if err != nil {
		s.emit(name, Event{Kind: KindError, Error: &ErrorPayload{Key: fkey, Step: "config", Error: err.Error()}})
		fail("config", err)
		return false
	}
	gd := &ActPayload{Key: gkey, Gate: repo.Gate, SHA: head, Worktree: d.Worktree}
	err = s.act(name, ActGate, gd, func() error {
		start := s.clock.Now()
		exit, output, err := s.runner.Gate(d.Worktree, repo.Gate, gateEnv(repo), timeout)
		took := s.clock.Now().Sub(start).Seconds()
		gd.DurationS = &took
		if err == nil && exit != 0 {
			err = fmt.Errorf("gate %s exited %d", strings.Join(repo.Gate, " "), exit)
			gd.Exit = &exit
		}
		if err != nil {
			// The end of its output says why, unless it is confidential.
			if output = strings.ToValidUTF8(output[max(0, len(output)-2000):], ""); s.confidentialMatch(output) > 0 {
				output = "(withheld: matches a confidential pattern)"
			}
			gd.Output = output
			return err
		}
		zero := 0
		gd.Exit = &zero
		return nil
	})
	if err != nil {
		if s.halt == nil && s.failed(t, gkey) {
			n := len(t.Failures[gkey])
			s.skipOnce(name, gkey+":gave-up", fmt.Sprintf("the gate failed %d times on %s; not retrying that commit", n, shortHash(head)), t, &SkipPayload{Worktree: d.Worktree, SHA: head})
			s.escalate(name, gkey+":failed", fmt.Sprintf("%s is DONE but its gate failed %d times on %s, last: %v. Nothing is pushed; a new commit is gated afresh.",
				name, n, shortHash(head), err), t, s.clock.Now())
		}
		return false
	}
	if s.shadow {
		return true
	}
	now, err := s.git.Head(d.Worktree)
	if err == nil && now == head {
		var clean bool
		if clean, err = s.git.Clean(d.Worktree); err == nil && clean {
			var after string
			if after, err = s.git.ConfigHash(d.Worktree); err == nil && after == config {
				return true
			}
		}
	}
	if err != nil {
		s.emit(name, Event{Kind: KindError, Error: &ErrorPayload{Key: fkey, Step: "after gate", Error: err.Error()}})
		fail("after gate", err)
		return false
	}
	s.skipOnce(name, "deliver:"+key+":moved:"+head, fmt.Sprintf("the tree or the git config changed while the gate ran on %s; not pushing it, the next pass gates it again", shortHash(head)), t,
		&SkipPayload{Worktree: d.Worktree, SHA: head, Head: now})
	return false
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
	return log.Append(Event{V: EventVersion, Time: now, Session: session, Kind: KindAction, Mode: ModeOwner, Act: &ActPayload{
		Action: ActOpen, Delivered: session + ":" + worktree, Ref: ref, By: "mark-delivered"}})
}

// ClearSession records the owner's clearing of a parked session: its crash
// count starts again and it may be restarted. A running agentd reads the
// record at its next pass. It is the owner's record, read by live and
// shadow runs alike.
func ClearSession(cfg *Config, log *Log, now time.Time, session string) error {
	if _, ok := cfg.Sessions[session]; !ok {
		return fmt.Errorf("session %q is not configured", session)
	}
	return log.Append(Event{V: EventVersion, Time: now, Session: session, Kind: KindClear, Mode: ModeOwner, Clear: &ClearPayload{By: "clear"}})
}

// launchLine is the shell command line that runs argv for sess under its
// account: CLAUDE_CONFIG_DIR for its config directory, and for a token
// account CLAUDE_CODE_OAUTH_TOKEN, read from the token file by the shell as
// the command launches. The token is never in agentd's memory, the log, or
// any process's arguments, where other users could see it. The launch fails
// closed: when the file cannot be read or holds only white space by then,
// the shell stops before starting anything, so Claude never falls back to
// another login. A token file that fails checkTokenFile now is refused.
func (s *Supervisor) launchLine(sess Session, argv []string) (string, error) {
	line := shellJoin(argv)
	if sess.Account == "" {
		return line, nil
	}
	acct, ok := s.cfg.Accounts[sess.Account]
	if !ok {
		return "", fmt.Errorf("account %q is not configured", sess.Account)
	}
	var pre string
	var env []string
	if acct.ConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+shellJoin([]string{acct.ConfigDir}))
	}
	if acct.TokenFile != "" {
		if err := checkTokenFile(acct.TokenFile); err != nil {
			return "", fmt.Errorf("account %s: token file: %w", sess.Account, err)
		}
		pre = `t=$(cat ` + shellJoin([]string{acct.TokenFile}) + `) && case $t in *[![:space:]]*) true ;; *) false ;; esac && `
		env = append(env, `CLAUDE_CODE_OAUTH_TOKEN="$t"`)
	}
	return pre + strings.Join(env, " ") + " exec " + line, nil
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
