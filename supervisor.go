package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Clock is time, so tests can move it. After is the wait between passes
// and between looks at the log for the owner's records.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
	After(d time.Duration) <-chan time.Time
}

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time                         { return time.Now() }
func (RealClock) Sleep(d time.Duration)                  { time.Sleep(d) }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// PaneLines is how many lines of history above the visible screen go into
// the screen hash. The patterns read the visible screen only, so a prompt
// that has scrolled away is never answered.
const PaneLines = 40

// Supervisor runs the policies over the configured sessions. Every write to
// the world goes through act, which shadow mode turns into a would event.
// The loop owns it; a worker runs on a copy whose worker flag is set and
// whose state holds only a copy of its session's Track (see workState).
type Supervisor struct {
	// home is the home directory whose .claude.json says which
	// directories Claude Code trusts, for a session with no account; run
	// sets it, and without it such a session's directory is not looked up.
	home string
	// cfg is base with the sessions agentd started (sessions.go),
	// swapped for a new copy whenever one starts or ends.
	cfg    *Config
	base   *Config
	log    *Log
	clock  Clock
	tmux   Tmux
	git    Git
	forge  AllowedForge
	runner Runner
	shadow bool
	worker bool       // this copy runs one job off the loop
	work   *workState // shared by the loop and its workers
	// status holds each session's STATUS.md as its last status check
	// read it, shared by the loop and its workers.
	status *statusCopies
	usage  *usageReader
	api    ForgeAPI    // the forges' APIs for the poll, or nil for none
	rel    *releaseRun // the release driver's reads (#67)
	idx    *indexRun   // the issue index's reads (#69)
	// live is each repository's issue list as last read, shared by the
	// loop and its workers (#62).
	live *liveIssues
	// cards is the repositories' context the curators keep (#60): the
	// loop's alone.
	cards *ContextStore
	// ops is what the ops console's changes run on (#63), nil without an
	// ops block.
	ops *opsEnv
	// checkedAt is when each work item's done_when was last checked, and
	// dueRetype the items due to be typed again (#66): the loop's alone.
	checkedAt map[string]time.Time
	dueRetype map[string]bool
	// designTried is when each item's designer last started (#68).
	designTried map[string]time.Time
	// watchedAt is when the ops watch last looked at the branch to
	// install from (#68).
	watchedAt time.Time
	// watchErr is the watch's last failure, shared with its workers, so
	// a failure is logged once until it changes.
	watchErr *atomic.Value

	checkpointRe *regexp.Regexp

	// halt is the first failed append of the pass. Once set, nothing more
	// is done until the next pass: an action whose record could be lost
	// could be repeated after a replay.
	halt error
	// runCtx ends when the run is told to stop; agent jobs end with it.
	runCtx context.Context
	// tailOff is how far into the event log this run has read, so each
	// pass picks up what the owner wrote meanwhile: agentd clear.
	tailOff int64
	// jobsLeft is set once this run has ended the scheduled jobs' runs an
	// earlier agentd left with no end (#51).
	jobsLeft bool

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
	for _, p := range screenPatterns(cfg) {
		confidential = append(confidential, regexp.MustCompile(p))
	}
	for _, p := range cfg.Policies.Dialog.Patterns {
		dialogs = append(dialogs, regexp.MustCompile(p))
	}
	return &Supervisor{
		cfg:    cfg,
		base:   cfg,
		log:    log,
		clock:  clock,
		tmux:   tmux,
		git:    git,
		forge:  AllowedForge{Repos: cfg.Repositories, Forge: forge},
		runner: runner,
		shadow: shadow,
		st:     NewLogState(cfg, mode(shadow)),
		work:   newWorkState(),
		status: &statusCopies{},
		live:   &liveIssues{},
		usage:  newUsageReader(),

		checkedAt:   map[string]time.Time{},
		dueRetype:   map[string]bool{},
		designTried: map[string]time.Time{},
		watchErr:    emptyValue(),

		checkpointRe: regexp.MustCompile(cfg.Policies.Context.Checkpoint),
		runCtx:       context.Background(),
		tailOff:      log.Size(),
		limitRe:      regexp.MustCompile(cfg.Policies.UsageLimit.Pattern),
		trustRe:      regexp.MustCompile(`^(?:` + cfg.Policies.Trust.Pattern + `)$`),
		dialogs:      dialogs,

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
// In a worker, a failed append stops its job, not the pass, and the event
// is also queued for the loop to apply to the session's own Track.
func (s *Supervisor) emit(name string, e Event) {
	s.screen(&e)
	if s.worker {
		s.emitQueued(name, e)
		return
	}
	e.V, e.Time, e.Session, e.Mode = EventVersion, s.clock.Now(), name, eventMode(e, s.mode())
	if err := s.log.Append(e); err != nil {
		fmt.Fprintf(os.Stderr, "agentd: event log: %v\n", err)
		if s.halt == nil {
			s.halt = fmt.Errorf("event log append failed; stopping this pass: %w", err)
		}
		return
	}
	s.st.apply(e)
}

// screen withholds what git, the forge CLIs, a gate or the escalation
// command said when it matches a confidential pattern: an error's text and
// a skip's reason never reach the log as they were.
func (s *Supervisor) screen(e *Event) {
	if e.Error != nil {
		e.Error.Error = s.withheld(e.Error.Error)
	}
	if e.Skip != nil {
		e.Skip.Reason = s.withheld(e.Skip.Reason)
	}
	if e.Index != nil {
		if i := s.confidentialMatch(e.Index.Title + "\n" + e.Index.Branch); i > 0 {
			e.Index.Title, e.Index.Branch, e.Index.Withheld = "", "", i
		}
	}
	if e.Sentence != nil {
		if i := s.confidentialMatch(e.Sentence.Text); i > 0 {
			e.Sentence.Text, e.Sentence.Withheld = "", i
			if e.Sentence.Failed == "" {
				e.Sentence.Failed = "withheld"
			}
		}
	}
	if e.Turn != nil {
		if i := s.confidentialMatch(e.Turn.Text); i > 0 {
			e.Turn.Text, e.Turn.Withheld = "", i
		}
		switch e.Turn.Role {
		case TurnCompact:
			// Its page reads it from the transcript, screened then (#58).
			e.Turn.Text, e.Turn.Withheld = "", 0
		case TurnSummary:
			e.Turn.Text = cutUTF8(e.Turn.Text, replyText)
		case TurnTool:
			e.Turn.Text = cutUTF8(e.Turn.Text, toolTargetLength)
		default:
			e.Turn.Text = cutUTF8(e.Turn.Text, turnText)
		}
	}
	if e.Account != nil {
		e.Account.Error = s.withheld(e.Account.Error)
	}
	if e.Job != nil {
		e.Job.Error = s.withheld(e.Job.Error)
	}
	if e.Status != nil {
		s.screenStatus(e.Status)
	}
	if t := e.Thread; t != nil {
		// Read screened already; screened again with every pattern the
		// repositories' own files add.
		if i := s.confidentialMatch(t.Title, t.Body, t.Author, strings.Join(t.Labels, "\n")); i > 0 {
			t.Title, t.Body, t.Author, t.Labels, t.Withheld = "", "", "", nil, i
		}
		for k := range t.Comments {
			if c := &t.Comments[k]; s.confidentialMatch(c.Body, c.Author) > 0 {
				c.Body, c.Author, c.Withheld = "", "", s.confidentialMatch(c.Body, c.Author)
			}
		}
	}
	// What a fleet's heartbeats and a release's checks said is another
	// party's text (#67).
	if f := e.Fleet; f != nil {
		f.Error = s.withheld(f.Error)
		for i := range f.Members {
			f.Members[i].Member, f.Members[i].Version = s.withheld(f.Members[i].Member), s.withheld(f.Members[i].Version)
		}
	}
	if r := e.Release; r != nil {
		r.Text, r.Seen, r.Compat, r.Error = s.withheld(r.Text), s.withheld(r.Seen), s.withheld(r.Compat), s.withheld(r.Error)
		for i := range r.Behind {
			r.Behind[i] = s.withheld(r.Behind[i])
		}
	}
	if c := e.Card; c != nil && c.Card != nil {
		if i := s.confidentialMatch(compact(c.Card)); i > 0 {
			c.Card, c.Refused = nil, fmt.Sprintf("the card's words match confidential pattern %d; it is not kept", i)
		}
	}
}

// withheld is text, or what the log says instead when it matches a
// confidential pattern.
func (s *Supervisor) withheld(text string) string {
	if i := s.confidentialMatch(text); i > 0 {
		return fmt.Sprintf("(withheld: matches confidential pattern %d)", i)
	}
	return text
}

// eventMode is the mode an event is written with: the run's, except for a
// fact about the world, which is shared.
func eventMode(e Event, run string) string {
	if e.Mode == ModeShared {
		return ModeShared
	}
	return run
}

// emitQueued is emit for a worker: the append and the queueing happen
// under one lock, so the loop applies the workers' events in the order the
// log holds them.
func (s *Supervisor) emitQueued(name string, e Event) {
	s.work.mu.Lock()
	defer s.work.mu.Unlock()
	e.V, e.Time, e.Session, e.Mode = EventVersion, s.clock.Now(), name, eventMode(e, s.mode())
	if err := s.log.Append(e); err != nil {
		fmt.Fprintf(os.Stderr, "agentd: event log: %v\n", err)
		if s.halt == nil {
			s.halt = fmt.Errorf("event log append failed; stopping this job: %w", err)
		}
		if s.work.err == nil {
			s.work.err = s.halt
		}
		return
	}
	s.st.apply(e)
	s.work.queue = append(s.work.queue, workerMsg{event: e})
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
		s.emit(name, Event{Kind: KindError, Error: &ErrorPayload{Key: a.Key, Error: err.Error(), Act: a, EmptyPipeline: errors.Is(err, errEmptyPipeline)}})
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
	s.refreshRuntime()
}

// surfaceCutShort logs a warning for each intent the log holds with no
// outcome: an action a crash of agentd cut short. apply counts what the
// owner or a session may have seen as done, so it is never repeated.
func (s *Supervisor) surfaceCutShort() {
	for _, e := range s.st.CutShort() {
		at := e.Time
		s.emit(e.Session, Event{Kind: KindWarning, Warning: &WarningPayload{
			Warning: "an action was cut short by a crash of agentd and has no recorded outcome; it is not repeated", Intent: e.Act, IntentAt: &at}})
	}
}

// Run passes until the context ends. A pass stopped by a failed append is
// reported and the next pass tries again; with once, its error is returned.
// The next pass comes after the poll interval (ChatPoll while a message
// waits on its answer), or sooner when another process appended an owner
// record to the log (looked for every WakeCheck), so a clear or a message
// is acted on within seconds. What a worker finished is applied
// at the next pass; it is in the log already. A run waits for its jobs
// before it stops: a --once run for all of them, one told to stop for up
// to StopWait.
func (s *Supervisor) Run(ctx context.Context, once bool) error {
	s.runCtx = ctx
	s.emit("", Event{Kind: KindStart, Start: &StartPayload{Sessions: s.cfg.SessionNames()}})
	s.surfaceCutShort()
	if !once {
		go s.watchOwner(ctx, s.log.Size())
	}
	select { // the first pass is due anyway
	case <-s.work.wake:
	default:
	}
	for {
		err := s.Pass()
		if err != nil {
			fmt.Fprintf(os.Stderr, "agentd: %v\n", err)
		}
		if once {
			if werr := s.Wait(); err == nil {
				err = werr
			}
			s.emit("", Event{Kind: KindStop, Stop: &StopPayload{Reason: "once"}})
			return err
		}
		select {
		case <-ctx.Done():
			s.waitStop()
			s.emit("", Event{Kind: KindStop, Stop: &StopPayload{Reason: ctx.Err().Error()}})
			return nil
		case <-s.clock.After(s.passWait()):
		case <-s.work.wake:
		}
	}
}

// passWait is how long the loop waits for the next pass: ChatPoll while
// an owner instruction is delivered and not answered, so its activity
// shows as it goes, else the poll interval.
func (s *Supervisor) passWait() time.Duration {
	if d := s.cfg.PollInterval.Duration; s.st.chatInFlight(s.clock.Now()) && ChatPoll < d {
		return ChatPoll
	}
	return s.cfg.PollInterval.Duration
}

// watchOwner looks at the log every WakeCheck for records the owner
// appended from another process, from off on, and wakes the loop when it
// finds one. It only reads; the pass applies them.
func (s *Supervisor) watchOwner(ctx context.Context, off int64) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.clock.After(WakeCheck):
		}
		events, next, err := s.log.Tail(off)
		if err != nil {
			continue
		}
		off = next
		for _, e := range events {
			// A change proposed on the ops console may need no yes (#63).
			if e.Mode == ModeOwner || e.Kind == KindOps && e.Ops.Step == OpsProposed {
				s.work.signal()
				break
			}
		}
	}
}

// Pass observes and acts on every session once. When an append to the
// event log fails it stops, acting on nothing more, and returns the error.
// Slow work it starts runs on in workers; what they finished since the
// last pass is applied first.
func (s *Supervisor) Pass() error {
	s.halt = nil
	s.ownerRecords()
	s.endSessions(s.clock.Now())
	// Instructions are routed before the sessions' steps, so one is
	// delivered in the pass that read it.
	s.routeInstructions(s.clock.Now())
	s.driveItems(s.clock.Now())
	s.runOps(s.clock.Now())
	s.watchInstall(s.clock.Now())
	for _, name := range s.cfg.SessionNames() {
		if s.halt != nil {
			break
		}
		s.step(name, s.cfg.Sessions[name])
	}
	if s.halt == nil {
		s.pollForges(s.clock.Now())
		s.driveReleases(s.clock.Now())
		s.readIndexes(s.clock.Now())
		s.watchCI(s.clock.Now())
		s.mergeGreen(s.clock.Now())
		s.runLoops(s.clock.Now())
		s.feedConsoles(s.clock.Now())
		s.triageItems(s.clock.Now())
		s.readAccounts(s.clock.Now())
		s.noticeAccounts(s.clock.Now())
		s.runJobs(s.clock.Now())
		s.checkStatuses(s.clock.Now())
		s.noteFacts(s.clock.Now())
		s.curateCards(s.clock.Now())
	}
	return s.halt
}

// ownerRecords applies what the workers queued, then what others wrote to
// the event log since the last pass: a clear lifts a session's park, a
// mark-delivered record settles a delivery, a seen mark takes items off
// the triage list, an item another process logged (agentd items) is added
// or retitled, another run's triage counts, and a session's tool call
// its server logged is folded into its case, while agentd runs. The
// queue is taken and the log read under the workers' lock, so every
// queued event is in what was read and none is applied after a later one.
func (s *Supervisor) ownerRecords() {
	s.work.mu.Lock()
	events, off, err := s.log.Tail(s.tailOff)
	s.drainLocked()
	s.work.mu.Unlock()
	if err != nil {
		s.emit("", Event{Kind: KindError, Error: &ErrorPayload{Step: "reading owner records", Error: err.Error()}})
		return
	}
	s.tailOff = off
	defer s.refreshRuntime()
	for _, e := range events {
		// agentd started or ended a session, or the owner ended one.
		if e.Kind == KindSession {
			s.st.apply(e)
			continue
		}
		shared := e.Kind == KindSeen || e.Kind == KindInstruction || e.Kind == KindAttachment || e.Kind == KindAccount || e.Kind == KindTriage || e.Kind == KindError && e.Error.Triage != nil ||
			e.Kind == KindItem && s.st.itemChanged(*e.Item) || e.Kind == KindCase && e.Mode == ModeOwner ||
			e.Kind == KindLoop && e.Mode == ModeOwner || e.Kind == KindOps && e.Ops.Step == OpsProposed ||
			e.Kind == KindRelease && e.Mode == ModeOwner || e.Kind == KindNode && e.Node.Result != nil && e.Node.Result.Action == NodeDeployStep
		if e.Session == "" && shared {
			s.st.apply(e)
			continue
		}
		// A session's tool call, which its server logged, is a shared fact
		// of that session's case.
		if _, ok := s.cfg.Sessions[e.Session]; ok && e.Kind == KindCase && e.Mode == ModeShared && e.Case.Call != nil {
			s.st.apply(e)
			continue
		}
		if _, ok := s.cfg.Sessions[e.Session]; ok && e.Mode == ModeOwner {
			s.st.apply(e)
			if e.Kind == KindClear {
				t := s.track(e.Session)
				sp, _ := t.spend(!s.shadow)
				base := sp + t.SpendBase
				s.emit(e.Session, Event{Kind: KindClear, Clear: &ClearPayload{By: "agentd", Base: &base}})
			}
		}
	}
}

// answerTrust answers the folder-trust menu by its accepting option, only
// for the directory agentd launches the session in: the arrows to that
// option, or, once the screen shows it selected, the confirm keys. A menu
// for another directory goes to the owner, once; one agentd cannot read
// safely gets nothing, and one skip says why.
func (s *Supervisor) answerTrust(name string, sess Session, t *Track, ev Evidence, now time.Time) {
	m := ev.TrustMenu
	if m != nil && !trustDirOK(sess, ev.PanePath, m) {
		dir := ev.PanePath
		if len(m.Dirs) > 0 {
			dir = strings.Join(m.Dirs, ", ")
		}
		if dir == "" {
			dir = "a directory tmux does not report"
		}
		s.escalate(name, "trust-elsewhere:"+name+":"+dir, fmt.Sprintf("%s shows Claude Code's folder-trust dialog for %s, not %s where agentd launches it; agentd does not answer it. Attach and answer it yourself.",
			name, dir, sess.Cwd), t, now)
		return
	}
	keys, why := trustKeys(m, s.cfg.Policies.Trust.Keys)
	if keys == nil {
		s.skipOnce(name, "trust-menu:"+name+":"+ev.PaneHash, why+"; not answering it", t, &SkipPayload{Action: ActTrust})
		if !now.After(t.HashSince) {
			return // a screen first seen this pass may be mid-redraw
		}
		s.escalate(name, "trust-unanswered:"+name+":"+ev.PaneHash, fmt.Sprintf("%s shows Claude Code's folder-trust prompt, but %s, so agentd does not answer it. Attach and answer it yourself.",
			name, why), t, now)
		return
	}
	s.typeInto(name, ActTrust, &ActPayload{Keys: keys, PaneHash: ev.PaneHash, Cwd: sess.Cwd, Reason: "selected: " + m.Options[m.Selected]}, t, ev.PaneHash, func() error {
		return s.tmux.Keys(name, keys...)
	})
}

// questionLine says whether a line of screen, less its indent and any box
// border, is wholly the trust prompt's question: the same words quoted in a
// diff, a dialog's body or a session's output sit inside other text.
func questionLine(re *regexp.Regexp, screen string) bool {
	for _, line := range strings.Split(screen, "\n") {
		// A blank line is no question, whatever a pattern matches.
		if line = strings.Trim(line, " \t│┃|"); line != "" && re.MatchString(line) {
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
		ev.Command, ev.PanePath = info.Command, info.Path
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
		ev.PaneHash = StillHash(pane)
		screen, err := s.tmux.CapturePane(name, 0)
		if err != nil {
			return ev, err
		}
		ev.Limited = s.limitRe.MatchString(screen)
		ev.TrustPrompt = s.cfg.Policies.Trust.Enabled && questionLine(s.trustRe, screen)
		if ev.TrustPrompt {
			ev.TrustMenu = trustMenu(screen, s.trustRe, s.home)
		}
		for _, re := range s.dialogs {
			if re.MatchString(screen) {
				ev.Dialog = re.String()
				break
			}
		}
		if ev.DialogShape = dialogShape(screen); ev.Dialog == "" && ev.DialogShape {
			ev.Dialog = "a question line with numbered options"
		}
		ev.Prompt = idlePrompt(screen)
	}
	if root := s.transcriptRoot(sess); root != "" {
		ev.Transcript = ReadTranscript(TranscriptDir(root, sess.Cwd))
	}
	return ev, nil
}

func (s *Supervisor) step(name string, sess Session) {
	now := s.clock.Now()
	t := s.track(name)
	// A started session is supervised once its workdir and worktree are made.
	if sess.Runtime && s.st.Ended[name] != "" {
		return // the end of an earlier session of the name stops it first
	}
	if p := s.st.Runtime[name]; sess.Runtime && unplaced(p) && t.Provisioned != provisionKey(name, p) {
		if s.place(name, t, now); s.st.Runtime[name].Account == "" {
			return
		}
	}
	if sess.Runtime && t.Provisioned != provisionKey(name, s.st.Runtime[name]) {
		s.provision(name, sess, t, now)
		return
	}
	ev, err := s.observe(name, sess)
	if err != nil {
		s.emit(name, Event{Kind: KindError, Error: &ErrorPayload{Step: "observe", Error: err.Error()}})
		return
	}
	// A session stopped for its budget, or due to be, is gone by policy;
	// so is one whose stop a crash of agentd cut short.
	if !ev.Alive && t.Live && !ev.Status.Done && t.Stopped == "" && s.overBudget(name, t, now) == "" {
		screen := ev.DeadScreen
		if !ev.PaneDead {
			screen = shortHash(t.Hash) // gone outright: the last screen seen
		}
		if s.crash(name, t, Crash{Exit: ev.Exit, Screen: screen}, false); s.halt != nil {
			return
		}
	}
	if s.readDecisions(name, sess, t, &ev); s.halt != nil {
		return
	}
	state, reason := Classify(t, ev, s.cfg.Policies, now)
	// The observe event, applied, ends a dialog episode and opens or
	// closes the Questions escalation: each is a field it compares.
	if s.logObserve(name, t, ev, state, reason, now); s.halt != nil {
		return
	}
	if s.noteReport(name, sess, ev); s.halt != nil {
		return
	}
	// After the observation, whose applying marks a live session live: the
	// stop, applied, marks it not, so its going is no crash.
	if s.stopForBudget(name, t, ev, now); s.halt != nil {
		return
	}
	if t.Stopped != "" && state != Done && state != Stopped {
		state, reason = Stopped, "stopped: "+t.Stopped
	}
	classified := state
	if state = s.actOn(name, sess, t, ev, state, now); classified == Done && state == Idle {
		reason = "its DONE was taken off for the owner's message"
	}
	if ev.Alive && state != Done && state != CrashLooping {
		s.watchCommand(name, ev.Command, t, now)
	}
	if state != t.State {
		s.emit(name, Event{Kind: KindTransition, Transition: &TransitionPayload{From: string(t.State), To: string(state), Reason: reason}})
	}
	// The transcript and the worktree are read in a worker, from what the
	// transcript gained since the last read; the next pass acts on them.
	if s.halt == nil {
		since := s.st.openSince(name, now)
		s.spawn(name, "measure:"+name, func(w *Supervisor, wt *Track) { w.measure(name, sess, wt, since) })
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

		StatusQuestions: ev.StatusQuestions,
		Limited:         ev.Limited,
		Trust:           ev.TrustPrompt,
		Dialog:          ev.Dialog != "",
		DialogShape:     ev.Dialog != "" && ev.DialogShape,
		Prompt:          ev.Prompt,
		Command:         ev.Command,
		Head:            ev.Status.Head,
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
	logged := o
	if !t.HashSince.IsZero() { // unknown until the screen is first seen
		n := int(now.Sub(t.HashSince).Seconds())
		logged.UnchangedS = &n
	}
	// Only an observation that reached the log counts as logged: one that
	// did not is logged again at the next pass.
	if s.emit(name, Event{Kind: KindObserve, Observe: &logged}); s.halt == nil {
		t.LastObs = &o
	}
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
	// A work session is rotated at the end of its round too, DONE once
	// its branch is delivered (#48).
	// Nor while the owner waits on its answer to a message (#56).
	if (state == Working || state == Idle || state == Done && sess.Kind == KindWork && delivered(name, sess, t)) && !t.Limited && s.st.openSince(name, now).IsZero() && s.rotate(name, sess, t, ev, now) {
		return Starting
	}
	// The limit has cleared. Only a session that can take a line is told to
	// go on: never one that is DONE, waits on the owner, or shows a dialog.
	// One line is typed per pass at most: the screen a second line would
	// go to has not been read since the first.
	typed := false
	if t.Limited && ev.Alive && (state == Working || state == Idle) {
		typed = true
		s.typeInto(name, ActResume, &ActPayload{Message: pol.Nudge.ResumeMessage}, t, ev.PaneHash, func() error {
			return s.tmux.Type(name, pol.Nudge.ResumeMessage)
		})
	}
	if !typed {
		typed = s.deliverAnswers(name, sess, t, ev, state)
	}
	// A finished session whose tmux session runs, handed the owner's
	// message, takes it once its DONE is off (#65).
	if !typed && state == Done && ev.Alive && !t.Limited && s.reopen(name, sess, t, ev) {
		state = Idle
	}
	if !typed {
		typed = s.deliverInstructions(name, sess, t, ev, state)
	}
	if !typed {
		typed = s.deliverItems(name, sess, t, ev, state)
	}
	if !typed && s.fixCI(name, sess, t, ev, state, now) {
		return Starting
	}
	if !typed && s.fixRound(name, sess, t, ev, state) {
		return Starting
	}
	switch state {
	case Done:
		// Each delivery runs in a worker: its gate and forge calls take
		// minutes, and the other sessions do not wait for them. One held,
		// or waiting out its backoff, starts none.
		for _, d := range sess.Deliver {
			key := name + ":" + d.Worktree
			if t.Delivered[key] != "" || !s.retryDue(t, "deliver:"+key, now) || sess.Hold != "" && t.Skipped["hold:"+key] {
				continue
			}
			d := d
			s.spawn(name, "deliver:"+name+":"+d.Worktree, func(w *Supervisor, wt *Track) {
				w.deliver(name, sess, d, ev, wt, now)
			})
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
		brief, ok := s.readBrief(name, sess, t, now, "started")
		if !ok {
			return state
		}
		// A session gone with answers waiting is restarted with them: the
		// file is written first, and the prompt names it.
		why := "restart"
		ids, at := t.freshAnswers()
		iids := s.st.freshInstructions(name, true, now)
		switch {
		case len(ids) > 0 && len(iids) > 0:
			why = "answered-instructed"
		case len(ids) > 0:
			why = "answered"
		case len(iids) > 0:
			why = "instructed"
		}
		id := newSessionID()
		cmd, facts := s.launchCommandWith(name, sess, t, id, why, nil)
		line, err := s.launchLine(sess, cmd)
		if err != nil {
			// Not final, and not a crash: the owner may fix the account.
			// There is no fallback to another account.
			s.skipOnce(name, "account:"+name, "not restarting: "+err.Error(), t, &SkipPayload{Account: sess.Account})
			s.escalate(name, "account:"+name, fmt.Sprintf("%s is gone and cannot be restarted under its account: %v. Fix the account; agentd will not use another.", name, err), t, now)
			return state
		}
		answers, ins := t.answersFor(false), s.st.instructionsFor(name, false)
		err = s.act(name, ActRestart, &ActPayload{Attempt: n + 1, Cwd: sess.Cwd, Respawn: ev.PaneDead, Account: sess.Account, Command: line, Transcript: id, Answers: ids, AnswersAt: at, Instructions: iids, Brief: brief.parts()}, func() error {
			if err := brief.write(sess.Workdir); err != nil {
				return err
			}
			if len(ids) > 0 {
				if err := writeAnswers(sess.Workdir, answers); err != nil {
					return err
				}
			}
			if len(iids) > 0 {
				if err := writeInstructions(s.cfg, sess.Workdir, ins, now); err != nil {
					return err
				}
			}
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
			s.notePack(name, sess, facts)
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
		// whatever replaced it. Still showing after stall, it goes to the
		// owner; in shadow no keys went, so nothing failed to take.
		switch {
		case ev.TrustPrompt && ev.PaneHash != t.TrustHash:
			s.answerTrust(name, sess, t, ev, now)
		case ev.TrustPrompt && !s.shadow && pol.Nudge.Stall.Duration > 0 && now.Sub(t.HashSince) >= pol.Nudge.Stall.Duration:
			s.escalate(name, "trust-stuck:"+name+":"+ev.PaneHash, fmt.Sprintf("%s still shows Claude Code's folder-trust prompt %s after agentd sent it keys; they did not take, and agentd does not send them twice. Attach and answer it yourself.",
				name, now.Sub(t.HashSince).Round(time.Minute)), t, now)
		}
	case BlockedOnOwner:
		switch {
		case ev.Status.Questions != "":
			s.escalateQuestions(name, ev.Status.Questions, t, now)
		case ev.Dialog != "":
			s.escalateDialog(name, ev.Dialog, ev.DialogShape, t, now)
			s.declineDialog(name, t, ev, now)
		default:
			s.escalate(name, fmt.Sprintf("tool-wait:%s:%d", name, t.HashSince.Unix()),
				fmt.Sprintf("%s has shown the same screen since %s with a tool call as its last turn; it is probably waiting at a permission prompt. Attach and answer it.", name, t.HashSince.Format(time.RFC3339)), t, now)
		}
	case Idle:
		// A chat session waits for the owner's next message.
		if !pol.Nudge.Enabled || typed || sess.Kind == KindChat {
			return state
		}
		if t.turnEnded(ev) && t.lastTurn().At.After(t.LastNudge) && s.continueTurn(name, sess, t, ev, now) {
			return state
		}
		// A turn that has not ended, one that ended before the last nudge
		// (which never reached it), or one continued as often as it may
		// be, waits out the stall.
		if now.Sub(t.HashSince) < pol.Nudge.Stall.Duration {
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
		nudge := &ActPayload{Nudge: t.Nudges + 1, Message: pol.Nudge.Message}
		s.typeInto(name, ActNudge, nudge, t, ev.PaneHash, func() error {
			if err := s.tmux.Type(name, pol.Nudge.Message); err != nil {
				return err
			}
			// Absorb the echo of the typed line, so it is not mistaken for
			// progress: the screen after it goes on the nudge's event, and
			// apply takes it as the screen last seen.
			s.clock.Sleep(time.Second)
			if pane, err := s.tmux.CapturePane(name, PaneLines); err == nil {
				nudge.PaneHash = StillHash(pane)
			}
			return nil
		})
	}
	return state
}

// continueTurn types the continue line into a session that ended its turn
// with work left, once Settle has passed since the turn ended and since its
// screen last changed: the continue message and the progress facts its
// STATUS.md gives, as a restart prompt hands them, or the message alone
// when they match a confidential pattern. The event records the message
// and the facts' hash, not the facts. A rotation due at a checkpoint comes
// first. It is a nudge, counted as one. A session continued nudge.max
// times in a row with the same facts goes to the owner once, and is left
// to the stall like a turn that has not ended: continueTurn says false.
func (s *Supervisor) continueTurn(name string, sess Session, t *Track, ev Evidence, now time.Time) bool {
	pol := s.cfg.Policies.Nudge
	facts := s.packFacts(name, sess, t, "continue")
	hash := shortHash(HashPane(string(facts)))
	if pol.Max == 0 || t.ContinueStatus == hash && t.Continues >= pol.Max {
		if pol.Max > 0 {
			s.escalate(name, fmt.Sprintf("continued:%s:%s:%d", name, hash, t.Launched.Unix()),
				fmt.Sprintf("%s: ended its turn %d times after being told to continue, with STATUS.md's progress unchanged and not DONE; not continuing it again.", name, t.Continues), t, now)
		}
		return false
	}
	if now.Sub(t.lastTurn().At) < pol.Settle.Duration || now.Sub(t.HashSince) < pol.Settle.Duration || s.rotationPending(name, sess, t) {
		return true
	}
	line := pol.ContinueMessage
	if s.confidentialMatch(string(facts)) == 0 {
		line += " " + string(facts)
	}
	nudge := &ActPayload{Nudge: t.Nudges + 1, Message: pol.ContinueMessage, StatusHash: hash}
	s.typeInto(name, ActNudge, nudge, t, ev.PaneHash, func() error {
		if err := s.tmux.Type(name, line); err != nil {
			return err
		}
		s.clock.Sleep(time.Second)
		if pane, err := s.tmux.CapturePane(name, PaneLines); err == nil {
			nudge.PaneHash = StillHash(pane)
		}
		return nil
	})
	return true
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
	since := t.NotClaudeSince
	if since.IsZero() || now.Sub(since) < s.cfg.Policies.Nudge.Stall.Duration {
		return
	}
	s.escalate(name, fmt.Sprintf("not-claude:%s:%d", name, since.Unix()),
		fmt.Sprintf("%s's pane has run %q, not Claude, since %s; agentd does not type into it. Attach and see what happened.", name, cmd, since.Format(time.RFC3339)), t, now)
}

// errNotClaude stops a typing action whose pane does not run Claude,
// errScreenChanged one whose screen changed after the pass read it,
// errParked one for a parked session, and errTrustPrompt one whose screen
// shows the folder-trust question.
var (
	errNotClaude     = errors.New("the pane does not run Claude")
	errScreenChanged = errors.New("the screen changed since it was read")
	errParked        = errors.New("the session is parked")
	errTrustPrompt   = errors.New("the screen shows the folder-trust prompt")
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
	if t.Parked {
		s.skipOnce(name, "parked:"+action+":"+name+":"+fmt.Sprint(t.Clears), "the session is parked; not typing", t, &SkipPayload{Action: action})
		return errParked
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
		// A line or a key but the trust keys, sent to the folder-trust
		// prompt, would answer it (#43). Only the visible screen counts, as
		// when the prompt is recognised: a question in the history has
		// been answered.
		if action != ActTrust && s.cfg.Policies.Trust.Pattern != "" {
			visible, err := s.tmux.CapturePane(name, 0)
			if err != nil {
				s.emit(name, Event{Kind: KindError, Error: &ErrorPayload{Step: action + ": capture", Error: err.Error()}})
				return err
			}
			if questionLine(s.trustRe, visible) {
				s.skipOnce(name, "trust-prompt:"+action+":"+name+":"+shortHash(StillHash(visible)), "the screen shows the folder-trust prompt; not typing", t,
					&SkipPayload{Action: action})
				return errTrustPrompt
			}
		}
		if now := StillHash(pane); now != seen {
			// Answers wait for a working session whose screen may move
			// every pass: that is logged once for the answers.
			key := "screen-changed:" + action + ":" + shortHash(now)
			if len(a.Answers) > 0 {
				key = "screen-changed:" + action + ":" + strings.Join(a.Answers, ",") + ":" + fmt.Sprint(a.AnswersAt[len(a.AnswersAt)-1].Unix())
			}
			s.skipOnce(name, key, "the screen changed since it was read this pass; not typing", t,
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
	res := append(append([]*regexp.Regexp(nil), s.confidential...), filePatterns(s.st)...)
	for i, re := range res {
		for _, text := range texts {
			if re.MatchString(text) {
				return i + 1
			}
		}
	}
	return 0
}

// configMatch is confidentialMatch with the configuration's patterns
// alone, every repository's included, and none a repository's file added.
func (s *Supervisor) configMatch(texts ...string) int {
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
// pass and one per QuestionsUpdateGap, carrying the text current then. Both
// carry the hash of the text they cover, so replay knows which text the
// console has seen.
func (s *Supervisor) escalateQuestions(name, questions string, t *Track, now time.Time) {
	q := &t.Questions
	hash := shortHash(HashPane(questions))
	full := questions // screened whole by post, before the cut can hide a match
	questions = cutUTF8(questions, 2000)
	key := fmt.Sprintf("questions:%s:%d", name, q.Episode)
	if !t.Posted[key] {
		s.post(name, ActEscalate, key, fmt.Sprintf("%s is blocked on you. Questions from STATUS.md:\n%s", name, questions), t, now,
			settles{QuestionsHash: hash, Episode: q.Episode}, full)
		return
	}
	if hash == q.Hash || (!q.LastUpdate.IsZero() && now.Sub(q.LastUpdate) < QuestionsUpdateGap) {
		return
	}
	n := q.Updates + 1
	s.post(name, ActUpdate, fmt.Sprintf("%s:update:%d", key, n), fmt.Sprintf("%s changed its open Questions in STATUS.md; they now read:\n%s", name, questions), t, now,
		settles{QuestionsHash: hash, Update: n, Episode: q.Episode}, full)
}

// escalateDialog posts one escalation per dialog episode, keyed to the
// session rather than the screen, so a dialog whose screen keeps changing
// is one post, and at most one per DialogEscalationGap.
func (s *Supervisor) escalateDialog(name, pattern string, shape bool, t *Track, now time.Time) {
	dq := &t.Dialog
	if dq.Open() || (!dq.Last.IsZero() && now.Sub(dq.Last) < DialogEscalationGap) {
		return
	}
	n := dq.N + 1
	msg := fmt.Sprintf("%s is stopped at a permission or confirmation dialog (the screen matches %q); agentd will not answer it. Attach and answer it.", name, pattern)
	if after := s.cfg.Policies.Dialog.DeclineAfter.Duration; after > 0 && shape {
		msg += fmt.Sprintf(" If it still shows at %s, agentd will decline it (Escape).", dq.Since.Add(after).Format(time.RFC3339))
	}
	s.post(name, ActEscalate, fmt.Sprintf("dialog:%s:%d", name, n), msg, t, now, settles{Dialog: n, Episode: dq.Episode})
}

// declineDialog sends Escape, once per episode, to a dialog that has shown
// for the policy's DeclineAfter: a refusal lets the agent adapt where a
// dialog nobody answers stops it for good. It never approves: Escape is the
// only key it sends, and only to the screen the pass read, which must have
// a dialog's shape: a pattern alone may match words in a session's output.
// A dialog still showing at a later pass, the same one or the next the
// agent's retry raised, is escalated once more: the owner was told it
// would be declined. Only an episode the owner was told of is declined: one
// the escalation gap kept quiet waits for its escalation.
func (s *Supervisor) declineDialog(name string, t *Track, ev Evidence, now time.Time) {
	after := s.cfg.Policies.Dialog.DeclineAfter.Duration
	dq := &t.Dialog
	if after <= 0 || !dq.Showing {
		return
	}
	if dq.Declined == dq.Episode {
		if now.After(dq.DeclinedAt) {
			did := "declined"
			if s.shadow {
				did = "would have declined"
			}
			s.escalate(name, fmt.Sprintf("dialog-declined:%s:%d", name, dq.Episode),
				fmt.Sprintf("%s: agentd %s its dialog at %s, but a dialog still shows; it will not decline again. Attach and answer it.", name, did, dq.DeclinedAt.Format(time.RFC3339)), t, now)
		}
		return
	}
	if dq.Declined > dq.Episode || dq.Escalated != dq.Episode || !ev.DialogShape || dq.Since.IsZero() || now.Sub(dq.Since) < after {
		return
	}
	keys := []string{"Escape"}
	s.typeInto(name, ActDecline, &ActPayload{Keys: keys, PaneHash: ev.PaneHash, Episode: dq.Episode}, t, ev.PaneHash, func() error {
		return s.tmux.Keys(name, keys...)
	})
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
// session; one skip records that it was filtered, without the text. Each
// text in also, the uncut text a message was cut from, is screened the same
// way. what goes on the events that settle the key, posted or given up on.
func (s *Supervisor) post(name, action, key, message string, t *Track, now time.Time, what settles, also ...string) bool {
	if t.Posted[key] {
		return true
	}
	// The escalation command runs in a worker, once per key at a time;
	// one started from a worker already is one.
	if !s.worker {
		if !s.retryDue(t, key, now) {
			return false
		}
		s.spawn(name, "post:"+key, func(w *Supervisor, wt *Track) {
			w.post(name, action, key, message, wt, now, what, also...)
		})
		return false
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
	if repo := s.st.Ended[name]; repo != "" { // an ended session is in no configuration
		if r, ok := s.cfg.Repositories[repo]; ok && r.Console != "" {
			console = r.Console
		} else {
			console = repo
		}
	}
	if !s.retryDue(t, key, now) {
		return false
	}
	// Without a command the action event is the post: the UI shows it
	// in its console (#42).
	run := func() error { return nil }
	if tmpl := s.cfg.Policies.Escalate.Command; len(tmpl) > 0 {
		argv := make([]string, len(tmpl))
		for i, part := range tmpl {
			argv[i] = strings.NewReplacer("{console}", console, "{message}", message).Replace(part)
		}
		run = func() error { return s.runner.Run(argv) }
	}
	err := s.act(name, action, &ActPayload{Key: key, Console: console, Message: message, QuestionsHash: what.QuestionsHash, Update: what.Update, Dialog: what.Dialog, Episode: what.Episode}, run)
	if err == nil {
		return true
	}
	if s.halt == nil && s.failed(t, key) {
		s.skipOnce(name, key+":gave-up", fmt.Sprintf("escalation command failed %d times; not retrying: %v", len(t.Failures[key]), err), t,
			&SkipPayload{Escalation: key, Console: console, Message: message, QuestionsHash: what.QuestionsHash, Update: what.Update, Dialog: what.Dialog, Episode: what.Episode})
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
	Episode       int // the Questions or dialog episode it is for
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
	// A CI fix session's commit goes to its request once the session has
	// finished, not while it works.
	fix, fixing := t.pendingCIFix(key)
	if fixing && !s.ciFixFinished(t, fix, ev, now) {
		s.skipOnce(name, "deliver:"+key+":ci-fix:"+fix.Transcript, fmt.Sprintf("the CI fix session for %s has not finished; delivery waits", fix.Ref), t, wt)
		return
	}
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
	// commits is delivered again. The open request a CI fix session was
	// started for takes the fix's new commits: they are pushed to it.
	// Each fix's delivery is recorded anew, so one that changed nothing
	// settles its own wait.
	var refresh *MergeResult
	again := ""
	if fixing {
		again = ":" + fix.Transcript
	}
	for _, r := range reqs {
		if fixing && r.Ref == fix.Ref && !r.Merged && head != fix.Head {
			r := r
			refresh = &r
			continue
		}
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
			s.skipOnce(name, "deliver:"+key+":exists"+again, fmt.Sprintf("%s already has merge request %s; not delivering again", branch, r.Ref), t,
				&SkipPayload{Delivered: key, Outcome: r.Ref, URL: r.URL, Branch: branch, Head: head, Repository: d.Repository, Worktree: d.Worktree, Target: d.Target})
			return
		}
	}
	if ahead == 0 {
		s.skipOnce(name, "deliver:"+key+":nothing"+again, fmt.Sprintf("%s has nothing ahead of %s; not delivering", branch, d.Target), t,
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
	if refresh != nil {
		s.skipOnce(name, fkey+":ci-fix:"+head, fmt.Sprintf("pushed the CI fix %s to %s", shortHash(head), refresh.Ref), t,
			&SkipPayload{Delivered: key, Outcome: refresh.Ref, URL: refresh.URL, Branch: branch, Head: head, Repository: d.Repository, Worktree: d.Worktree, Target: d.Target})
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
	gate := repo.Policy.Gate
	if len(gate.Argv) == 0 { // refused at load; never deliver ungated
		s.skipOnce(name, "deliver:"+key+":no-gate", fmt.Sprintf("repository %s has no gate; not delivering", d.Repository), t, &SkipPayload{Worktree: d.Worktree})
		return false
	}
	timeout := gate.Timeout.Duration
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
	// The gate is the target branch's (#3: authority comes from the target
	// branch). A session that changed it is refused until its branch has
	// the gate as the target has it; the owner changes gates by merging.
	if !s.gatePinned(name, key, fkey, d, repo, head, t, fail) {
		return false
	}
	gd := &ActPayload{Key: gkey, Gate: gate.Argv, SHA: head, Worktree: d.Worktree}
	var leftHome error
	err = s.act(name, ActGate, gd, func() error {
		// HOME is an empty directory of the gate's own, so tools find no
		// credentials there; what the gate could still reach by path is
		// recorded.
		home, err := os.MkdirTemp("", "agentd-gate-home-")
		if err != nil {
			return err
		}
		defer func() { leftHome = removeAll(home) }()
		gd.Reachable = s.reachableCredentials()
		start := s.clock.Now()
		exit, output, err := s.runner.Gate(d.Worktree, gate.Argv, gateEnv(repo, home), timeout)
		took := s.clock.Now().Sub(start).Seconds()
		gd.DurationS = &took
		if err == nil && exit != 0 {
			err = fmt.Errorf("gate %s exited %d", strings.Join(gate.Argv, " "), exit)
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
	if leftHome != nil {
		s.emit(name, Event{Kind: KindError, Error: &ErrorPayload{Key: fkey, Step: "gate home", Error: leftHome.Error()}})
	}
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
				// The gate files again: one still changed when the gate
				// ends is refused. One changed and put back while it ran
				// is not seen.
				return s.gatePinned(name, key, fkey, d, repo, head, t, fail)
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
	return accountLine(sess.Account, acct, line)
}

// accountLine runs line under acct: its config directory, and its token
// read from its file at launch, so the token is never in a command line
// agentd logs.
func accountLine(name string, acct Account, line string) (string, error) {
	var pre string
	var env []string
	if acct.ConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+shellJoin([]string{acct.ConfigDir}))
	}
	if acct.TokenFile != "" {
		if err := checkTokenFile(acct.TokenFile); err != nil {
			return "", fmt.Errorf("account %s: token file: %w", name, err)
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

// gatePaths are the paths that make up r's gate: each argument of its argv
// that is a relative path in the worktree, its script, and its paths.
func gatePaths(r Repository) []string {
	var paths []string
	seen := map[string]bool{}
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, a := range r.Policy.Gate.Argv {
		if strings.Contains(a, "/") && !filepath.IsAbs(a) && !strings.HasPrefix(a, "-") && !strings.Contains(a, "=") {
			if p := filepath.Clean(a); p != "." && p != ".." && !strings.HasPrefix(p, "../") {
				add(p)
			}
		}
	}
	for _, p := range r.Policy.Gate.Paths {
		add(p)
	}
	return paths
}

// gatePinned says whether the gate in the worktree is the target branch's
// (#3: authority comes from the target branch), as url has the target now.
// A session that changed it is refused until its branch has the gate as the
// target has it, and so is a gate none of whose paths the target has, which
// nothing pins; the owner changes gates by merging.
func (s *Supervisor) gatePinned(name, key, fkey string, d Deliver, repo Repository, head string, t *Track, fail func(string, error)) bool {
	paths := gatePaths(repo)
	changed, pinned, err := s.git.GateFiles(d.Worktree, pushURL(repo), d.Target, head, paths)
	if err != nil {
		s.emit(name, Event{Kind: KindError, Error: &ErrorPayload{Key: fkey, Step: "gate files", Error: err.Error()}})
		fail("gate files", err)
		return false
	}
	if len(changed) > 0 {
		ck := "deliver:" + key + ":gate-changed:" + head
		s.skipOnce(name, ck, fmt.Sprintf("the gate (%s) differs from the target branch's %s; not running it or delivering %s",
			strings.Join(changed, ", "), d.Target, shortHash(head)), t, &SkipPayload{Worktree: d.Worktree, SHA: head})
		s.escalate(name, ck, fmt.Sprintf("%s is DONE but its gate (%s) differs from what the target branch %s has; nothing is delivered. "+
			"Restore the gate in the branch, or change it on %s through review.", name, strings.Join(changed, ", "), d.Target, d.Target), t, s.clock.Now())
		return false
	}
	if pinned == 0 {
		ck := "deliver:" + key + ":gate-unpinned"
		s.skipOnce(name, ck, fmt.Sprintf("the target branch %s has none of the gate's paths (%s), so nothing pins it; name its files in repositories.%s.policy.gate.paths",
			d.Target, strings.Join(paths, ", "), d.Repository), t, &SkipPayload{Worktree: d.Worktree})
		s.escalate(name, ck, fmt.Sprintf("%s is DONE but nothing on %s pins the gate of %s; nothing is delivered until its files are named in its policy's gate.paths.",
			name, d.Target, d.Repository), t, s.clock.Now())
		return false
	}
	return true
}

// removeAll removes dir, making what is read-only in it writable first,
// as Go's module cache is.
func removeAll(dir string) error {
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// credentialPaths are where common tools keep credentials, under HOME.
var credentialPaths = []string{".aws", ".claude", ".claude.json", ".config/agentd-kit", ".config/gcloud", ".config/gh", ".config/git/credentials",
	".config/glab-cli", ".docker/config.json", ".git-credentials", ".gnupg", ".kube/config", ".netrc", ".npmrc", ".pypirc", ".ssh"}

// reachableCredentials lists the credential paths a process running as
// agentd's user can still find, however its HOME is set: the common ones
// under the real home as ~/ paths, each configured account's files by
// account and field, never by path, and the ssh agent's socket. Existence is enough, and nothing is opened.
func (s *Supervisor) reachableCredentials() []string {
	home, _ := os.UserHomeDir()
	var out []string
	seen := map[string]bool{}
	add := func(label, p string) {
		if _, err := os.Lstat(p); err == nil && !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	if home != "" {
		for _, p := range credentialPaths {
			add("~/"+p, filepath.Join(home, p))
		}
	}
	var names []string
	for name := range s.cfg.Accounts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		a := s.cfg.Accounts[name]
		for _, f := range [][2]string{{"token_file", a.TokenFile}, {"config_dir", a.ConfigDir}} {
			if f[1] != "" {
				add("account "+name+" "+f[0], f[1])
			}
		}
	}
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		add("$SSH_AUTH_SOCK", sock)
	}
	return out
}
