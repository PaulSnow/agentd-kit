package main

import (
	"fmt"
	"time"
)

// State is a session's state. Every transition is explained by evidence.
type State string

const (
	Starting       State = "starting"
	Working        State = "working"
	Idle           State = "idle"
	BlockedOnOwner State = "blocked-on-owner"
	UsageLimited   State = "usage-limited"
	Done           State = "done"
	Failed         State = "failed"
	CrashLooping   State = "crash-looping"
	Stopped        State = "stopped" // the budget or the stop time was reached
)

// Track is what the supervisor remembers about one session between passes.
// It is rebuilt from the event log at start, so the log is the only state.
type Track struct {
	State State
	Since time.Time

	Hash      string    // last pane hash seen
	HashSince time.Time // when the pane last changed, nudge echoes excluded
	Observed  bool      // at least one observation
	// Provisioned is set once a started session's workdir and worktree
	// were made (sessions.go); a new start clears it.
	Provisioned bool

	Restarts  []time.Time // restart times by agentd, for the backoff
	Live      bool        // seen alive, or restarted, since the last crash: its next disappearance is a crash
	Crashes   []Crash     // crashes since the owner last cleared the session
	Parked    bool        // crash-looping: parked for the owner until agentd clear
	Clears    int         // owner clears applied
	Nudges    int         // nudges in the current idle episode
	LastNudge time.Time
	Limited   bool
	TrustHash string // the screen hash the trust keys were last sent for
	// NotClaudeSince is when the pane was first seen running a command
	// that is not Claude, or zero while it runs Claude.
	NotClaudeSince time.Time

	// Context and spend. Transcript is the Claude Code session id agentd
	// last launched the session under, or "" when it has not; Measured is
	// the transcript last measured; Usage is every transcript's totals.
	Transcript string
	Measured   string
	Usage      map[string]UsagePayload
	Checkpoint Checkpoint // the latest merge seen at the worktree's head
	Launched   time.Time  // the latest restart or rotation started
	RotatedFor string     // the merge the latest rotation was at
	Rotations  int        // rotations, taken or would-be, ever
	Stopped    string     // why the session was stopped, until the owner clears it
	SpendBase  int64      // the spend when the owner last cleared it, which its cap counts from

	Posted    map[string]bool        // escalation keys already posted, or given up on
	Questions QuestionsEscalation    // the escalation of the Questions section
	Dialog    DialogEscalation       // the escalations of dialogs on screen
	Delivered map[string]string      // delivery keys: MR reference or the reason none was made
	Skipped   map[string]bool        // skip reasons already logged
	Failures  map[string][]time.Time // failed tries of a keyed action; retries follow the restart backoff
	LastObs   *ObservePayload        // last observation logged, to log only changes

	// What the views show, folded from the log by apply alone.
	LastSeen      time.Time
	Head, Reason  string
	RestartCount  int                // restarts, taken or would-be, ever
	NudgeCount    int                // nudges ever
	Escalations   int                // escalations and updates posted
	Escalated     []EscalationRecord // the latest escalations, oldest first
	Would         int                // would events (shadow mode)
	Errors        int                // error events
	DeliveredRefs []string           // what each delivery opened
	Blocked       map[string]string  // worktree: the last reason its delivery was declined
	GaveUp        []string           // escalation keys given up on
	CutShort      []string           // actions a crash of agentd cut short
	// Decisions: what the session raised in questions.json, as last read
	// (DecisionsHash), the hash of its STATUS.md Questions, and the
	// owner's answers by decision id. Raised is what it raised through its
	// tools and has not superseded, RaisedCount how many it raised in all,
	// and Deadlines the owner's deadlines by decision id.
	Decisions       []Decision
	DecisionsHash   string
	StatusQuestions string
	Answers         map[string]AnswerRecord
	Raised          []Decision
	RaisedCount     int
	Deadlines       map[string]DeadlineRecord

	Asked      string // the Questions text last posted, while they stay open
	AskedAt    time.Time
	Filtered   map[string]bool // escalation keys whose text was withheld
	Deliveries []DeliveryRecord
	Gated      []string // the commits a gate agentd ran passed on

	// Turns: the latest prompts and summaries read from the transcripts,
	// oldest first, how many were logged in all, and the last line logged
	// of each transcript, so none is logged twice.
	Turns     []TurnRecord
	TurnCount int
	TurnLines map[string]int
	// A coordinator's wakes: the latest, oldest first, how many in all,
	// and its spend at the first whose spend was known.
	Wakes     []WakeRecord
	WakeCount int
	WakeBase  *int64
	Woken     map[string]bool // the due keys the wakes within the margin covered
	Carried   map[string]int  // the owner instructions a wake carried, by id, and that wake's number
	// CI: the fix sessions started for the requests the session delivered,
	// oldest first, and the decisions raised on a repeated failure, by
	// request and cause.
	CIFixes  []CIFixRecord
	CIRaised map[string]string
	// Merges: each request agentd merged, or would have, by repository
	// and reference, to the head it merged; a request is merged once.
	Merges map[string]string
	// Merged: each request agentd did merge, its action taken and not
	// cut short, by repository and request, to the head merged.
	Merged map[string]string
}

// DeliveryRecord is one request a delivery opened, found, or was told of.
type DeliveryRecord struct {
	Ref        string
	Repository string // as configured; "" for an owner's mark-delivered record
	Delivered  string // the delivery key, session:worktree
	At         time.Time
}

// QuestionsEscalation is the one escalation a session's Questions section
// has at a time. It opens when Questions become non-empty, closes when they
// empty or the session is DONE, and is numbered so a reopened section is a
// new escalation. Open and Episode follow the observe events; Hash and
// Updates follow the escalate and update events, so replay rebuilds them.
type QuestionsEscalation struct {
	Open    bool
	Episode int    // escalations opened so far; the open one's number
	Hash    string // short hash of the text last posted, or "" before the first post
	Updates int    // updates posted in this episode
	// LastUpdate is when the session's last update was posted; the next
	// waits QuestionsUpdateGap, so a session rewording every pass does not
	// flood the console.
	LastUpdate time.Time
}

// DialogEscalation tracks a session's dialog escalations: one per dialog
// episode (a dialog showing, whatever its screen does, until a pass sees
// none), and at most one per DialogEscalationGap. Showing and Episode
// follow the observe events; Escalated, N and Last follow the escalations,
// which carry the episode they were made for, so they settle the same
// whichever comes first: an escalation a worker finishes after the dialog
// went away settles an episode that is over.
type DialogEscalation struct {
	Showing    bool      // a dialog shows, as the last observation logged said
	Episode    int       // dialog episodes seen so far; the one showing, if any
	Since      time.Time // since when a dialog's shape has shown, or zero
	Declined   int       // the latest episode declined, or would have been
	DeclinedAt time.Time // when it was
	Escalated  int       // the latest episode escalated
	N          int       // dialog escalations so far, numbering their keys
	Last       time.Time // when the last was posted
}

// Open says whether the dialog now showing has been escalated.
func (d DialogEscalation) Open() bool { return d.Showing && d.Escalated == d.Episode }

// DialogEscalationGap is the least time between two dialog escalations of
// one session.
const DialogEscalationGap = 30 * time.Minute

// QuestionsUpdateGap is the least time between two Questions updates of
// one session.
const QuestionsUpdateGap = 30 * time.Minute

// observeQuestions opens or closes the escalation from what STATUS.md asks.
func (q *QuestionsEscalation) observeQuestions(asks, done bool) {
	switch {
	case asks && !done && !q.Open:
		q.Open, q.Episode, q.Hash, q.Updates = true, q.Episode+1, "", 0
	case (!asks || done) && q.Open:
		q.Open = false
	}
}

// Crash is the cause of one crash: the pane's exit status, when tmux kept
// it, and the short hash of the last screen seen. Two crashes with the same
// cause are the same failure repeating.
type Crash struct {
	Exit   int // -1 when unknown
	Screen string
}

// noteCrash records a crash and parks the session when the cause repeats
// one already seen, or when max crashes have happened since the owner last
// cleared it, however far apart.
func (t *Track) noteCrash(c Crash, max int) {
	for _, p := range t.Crashes {
		if p == c {
			t.Parked = true
		}
	}
	t.Crashes = append(t.Crashes, c)
	t.Live = false
	if len(t.Crashes) >= max {
		t.Parked = true
	}
}

// clear is the owner's clearing of a parked session.
func (t *Track) clear() {
	t.Crashes, t.Parked, t.Stopped = nil, false, ""
	t.Clears++
}

func newTrack() *Track {
	return &Track{Posted: map[string]bool{}, Delivered: map[string]string{}, Skipped: map[string]bool{}, Failures: map[string][]time.Time{},
		Blocked: map[string]string{}, Filtered: map[string]bool{}, Usage: map[string]UsagePayload{},
		Answers: map[string]AnswerRecord{}, Deadlines: map[string]DeadlineRecord{}, CIRaised: map[string]string{}, Merges: map[string]string{}, Merged: map[string]string{}}
}

// restartsInWindow counts restarts inside the backoff window.
func (t *Track) restartsInWindow(now time.Time, window time.Duration) int {
	n := 0
	for _, at := range t.Restarts {
		if now.Sub(at) < window {
			n++
		}
	}
	return n
}

// lastRestart is the time of the latest restart, or zero.
func (t *Track) lastRestart() time.Time {
	if len(t.Restarts) == 0 {
		return time.Time{}
	}
	return t.Restarts[len(t.Restarts)-1]
}

// backoff is the wait before restart number n (0-based) within the window:
// none for the first, then initial doubled each time, capped.
func backoff(n int, p RestartPolicy) time.Duration {
	if n <= 0 {
		return 0
	}
	d := p.BackoffInitial.Duration
	for i := 1; i < n; i++ {
		d *= 2
		if d >= p.BackoffMax.Duration {
			return p.BackoffMax.Duration
		}
	}
	if d > p.BackoffMax.Duration {
		return p.BackoffMax.Duration
	}
	return d
}

// noteHash records the pane hash and decides whether a change is progress.
// After a nudge, a change is the nudge's echo rather than progress when it
// comes within the settle time, or when the transcript shows no turn since
// the nudge; an echo keeps the idle clock running. Any other change restarts
// the clock and the nudge count.
func (t *Track) noteHash(hash string, now time.Time, p NudgePolicy, tr TranscriptEvidence) {
	if !t.Observed {
		t.Hash, t.HashSince, t.Observed = hash, now, true
		return
	}
	if hash == t.Hash {
		return
	}
	t.Hash = hash
	if t.Nudges > 0 && !t.LastNudge.IsZero() {
		if now.Sub(t.LastNudge) <= p.Settle.Duration {
			return
		}
		if tr.Found && !tr.At.After(t.LastNudge) {
			return
		}
	}
	t.HashSince = now
	t.Nudges = 0
}

// Classify derives the state from evidence and memory. It changes nothing
// but the pane memory. The reason names the evidence that decided.
func Classify(t *Track, ev Evidence, pol Policies, now time.Time) (State, string) {
	switch {
	case t.Parked:
		return CrashLooping, fmt.Sprintf("parked after %d crashes; waiting for the owner to run agentd clear", len(t.Crashes))
	case ev.Status.Done:
		return Done, "STATUS.md ends with DONE"
	case t.Stopped != "":
		return Stopped, "stopped: " + t.Stopped + "; waiting for the owner to run agentd clear"
	case !ev.Alive && ev.Status.Questions != "":
		return BlockedOnOwner, "tmux session gone and STATUS.md has open Questions; parked for the owner, not restarted"
	case !ev.Alive:
		return Failed, "tmux session gone and STATUS.md not DONE"
	}
	first := !t.Observed
	t.noteHash(ev.PaneHash, now, pol.Nudge, ev.Transcript)
	// Open Questions outrank everything on screen, so no prompt is answered
	// for a session that waits on the owner. The trust prompt is itself a
	// numbered menu, so in the start phase, since the session was first seen
	// or last restarted and before it was seen in any other state, it is
	// recognised before the dialog patterns; after that, a menu is a dialog.
	startPhase := t.State == "" || t.State == Starting
	switch {
	case ev.Status.Questions != "":
		return BlockedOnOwner, "STATUS.md has open Questions"
	case ev.TrustPrompt && startPhase:
		return Starting, "screen shows the folder-trust prompt"
	case ev.Limited:
		return UsageLimited, "screen shows the usage-limit message"
	case ev.Dialog != "":
		return BlockedOnOwner, fmt.Sprintf("screen shows a permission or confirmation dialog (matches %q)", ev.Dialog)
	}
	if first {
		return Starting, "first observation"
	}
	unchanged := now.Sub(t.HashSince)
	if pol.Nudge.Enabled && unchanged >= pol.Nudge.Stall.Duration {
		if ev.Transcript.Found && now.Sub(ev.Transcript.At) < pol.Nudge.Stall.Duration {
			return Working, fmt.Sprintf("screen unchanged for %s but the transcript moved %s ago", unchanged.Round(time.Minute), now.Sub(ev.Transcript.At).Round(time.Minute))
		}
		if ev.Transcript.Found && ev.Transcript.Role == "assistant" && ev.Transcript.StopReason == "tool_use" {
			return BlockedOnOwner, fmt.Sprintf("screen unchanged for %s and the last transcript turn is a tool call; probably waiting at a permission prompt", unchanged.Round(time.Minute))
		}
		return Idle, fmt.Sprintf("screen unchanged for %s", unchanged.Round(time.Minute))
	}
	return Working, "screen changed recently"
}
