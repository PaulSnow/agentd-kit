package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// EventVersion is the version of the event format this agentd writes and
// the only one it reads. A log of another version is refused whole: start
// a fresh log rather than guess at an old shape.
const EventVersion = 2

// Event is one line of the event log: an observation, a transition, an
// action taken, an action shadow mode would have taken, or an error. Every
// event carries the version, the mode of the run that wrote it, and exactly
// one typed payload, the one its kind names; there is no untyped detail.
type Event struct {
	V       int       `json:"v"`
	Time    time.Time `json:"time"`
	Session string    `json:"session,omitempty"`
	Kind    string    `json:"kind"`
	Mode    string    `json:"mode,omitempty"`

	Start       *StartPayload       `json:"start,omitempty"`
	Stop        *StopPayload        `json:"stop,omitempty"`
	Observe     *ObservePayload     `json:"observe,omitempty"`
	Transition  *TransitionPayload  `json:"transition,omitempty"`
	Act         *ActPayload         `json:"act,omitempty"` // intent, action and would
	Skip        *SkipPayload        `json:"skip,omitempty"`
	Crash       *CrashPayload       `json:"crash,omitempty"`
	Clear       *ClearPayload       `json:"clear,omitempty"`
	Error       *ErrorPayload       `json:"error,omitempty"`
	Warning     *WarningPayload     `json:"warning,omitempty"`
	Item        *ItemPayload        `json:"item,omitempty"`
	Seen        *SeenPayload        `json:"seen,omitempty"`
	Cursor      *CursorPayload      `json:"cursor,omitempty"`
	Usage       *UsagePayload       `json:"usage,omitempty"`
	Checkpoint  *CheckpointPayload  `json:"checkpoint,omitempty"`
	Poll        *PollPayload        `json:"poll,omitempty"`
	Triage      *TriagePayload      `json:"triage,omitempty"`
	Decision    *DecisionPayload    `json:"decision,omitempty"`
	Answer      *AnswerPayload      `json:"answer,omitempty"`
	Deadline    *DeadlinePayload    `json:"deadline,omitempty"`
	Case        *CasePayload        `json:"case,omitempty"`
	Turn        *TurnPayload        `json:"turn,omitempty"`
	Coordinator *CoordinatorPayload `json:"coordinator,omitempty"`
	CI          *CIPayload          `json:"ci,omitempty"`
	Feed        *FeedPayload        `json:"feed,omitempty"`
	Instruction *InstructionPayload `json:"instruction,omitempty"`
	Runtime     *RuntimePayload     `json:"runtime,omitempty"`
	Policy      *PolicyPayload      `json:"policy,omitempty"`
	Approval    *ApprovalPayload    `json:"approval,omitempty"`
	Account     *AccountPayload     `json:"account,omitempty"`
	Message     *MessagePayload     `json:"message,omitempty"`
	Inbox       *InboxPayload       `json:"inbox,omitempty"`
	Job         *JobPayload         `json:"job,omitempty"`
	Loop        *LoopPayload        `json:"loop,omitempty"`
}

// Event kinds.
const (
	KindStart       = "start"       // agentd started: mode, config, sessions
	KindStop        = "stop"        // agentd stopped
	KindObserve     = "observe"     // evidence for a session changed
	KindTransition  = "transition"  // the session's state changed
	KindIntent      = "intent"      // an action is about to be taken; its outcome follows
	KindAction      = "action"      // an action was taken
	KindWould       = "would"       // shadow mode: the action that would have been taken
	KindSkip        = "skip"        // an action a policy declined, with the reason
	KindCrash       = "crash"       // a session agentd saw alive, or restarted, is gone: its cause
	KindClear       = "clear"       // the owner cleared a parked session (agentd clear)
	KindError       = "error"       // observation or action failed
	KindWarning     = "warning"     // agentd itself noticed something: a torn log line, an intent cut short
	KindItem        = "item"        // something new the owner may want to know: an issue, a request, a discussion, mail
	KindSeen        = "seen"        // the owner marked items seen
	KindCursor      = "cursor"      // how far a source of items has been read
	KindUsage       = "usage"       // what a session's transcript has cost so far
	KindCheckpoint  = "checkpoint"  // a merge newly at the head of a session's worktree
	KindPoll        = "poll"        // a poll of a forge that found something, failed, or recovered
	KindTriage      = "triage"      // the checked answer of a triage job: each item's kind, needs and summary
	KindDecision    = "decision"    // a session's questions.json changed: the decisions it raises
	KindAnswer      = "answer"      // the owner answered one of a session's decisions, or agentd applied its default at the owner's deadline
	KindDeadline    = "deadline"    // the owner set a deadline on one of a session's decisions
	KindCase        = "case"        // a fact about a case: the pack a session was handed, its report, or the owner's ruling
	KindTurn        = "turn"        // a prompt or end-of-turn summary read from a session's transcript
	KindCoordinator = "coordinator" // a session claimed a repository's coordinator, or the owner released it
	KindCI          = "ci"          // a delivered request's pipeline changed state
	KindFeed        = "feed"        // a line for a repository's console: its baseline, its post, or the post's outcome
	KindSession     = "session"     // a coordinator started a session, or it or the owner ended one
	KindPolicy      = "policy"      // a repository's own policy file, as read at its target's tip
	KindApproval    = "approval"    // a coordinator approved a request at a head
	KindInstruction = "instruction" // the owner's free-text reply in a console, for the repositories it feeds
	KindAccount     = "account"     // a reading of an account's usage: its utilization, resets and overage
	KindMessage     = "message"     // a post to a console: a session's or a coordinator's report (agentd console post)
	KindInbox       = "inbox"       // a reader read the owner's instructions for a console (agentd console inbox)
	KindJob         = "job"         // a scheduled job's run started, ended or was skipped
	KindLoop        = "loop"        // a step agentd took in a request's loop, or the owner's retry of a failed one
)

// Modes, in the mode every event carries. A run reads only the events of
// its own mode, and the owner's: live and shadow never share state.
const (
	ModeLive   = "live"
	ModeShadow = "shadow"
	ModeOwner  = "owner"  // an owner's record, such as mark-delivered, read by both
	ModeShared = "shared" // a fact about the world, not a run's, such as a new item, read by both
)

// Action names, in the Act payload of intent, action and would events.
const (
	ActRestart   = "restart"
	ActNudge     = "nudge"
	ActResume    = "resume"
	ActTrust     = "trust"
	ActDecline   = "decline" // Escape sent to a dialog shown past the policy's time
	ActFetch     = "fetch"
	ActPush      = "push"
	ActOpen      = "open"
	ActGate      = "gate"
	ActEscalate  = "escalate"
	ActUpdate    = "update"    // the open Questions escalation's text changed
	ActRotate    = "rotate"    // a fresh context at a checkpoint: not a crash, not a restart
	ActStop      = "stop"      // the budget or the stop time was reached
	ActAnswer    = "answer"    // the owner's answers written to answers.json and the pointer typed
	ActWake      = "wake"      // a coordinator's pack written and its pointer typed: a judgment is due
	ActCIFix     = "ci-fix"    // a session relaunched to fix its request's own CI failure
	ActFixRound  = "fix-round" // a work session relaunched to fix its request's adopted review findings
	ActRaise     = "raise"     // agentd raised a decision for the owner on a session: a CI failure it will not fix again
	ActProvision = "provision" // a started session's workdir, brief and worktree made
	ActEnd       = "end"       // an ended session's tmux session stopped
	ActMerge     = "merge"     // a request's target fast-forwarded to its gated, green head
	ActRetest    = "retest"    // a request's pipeline run again on its merge result, its target having moved
)

// StartPayload is a run's start: the sessions it supervises.
type StartPayload struct {
	Sessions []string `json:"sessions"`
}

// StopPayload is why a run stopped.
type StopPayload struct {
	Reason string `json:"reason"`
}

// ObservePayload is a session's evidence, logged when it changes.
// UnchangedS is absent until the screen has been seen, since until then
// the time is unknown; Restarts is absent when restarts are off.
type ObservePayload struct {
	State     string `json:"state"`
	Reason    string `json:"reason"`
	Alive     bool   `json:"alive"`
	Done      bool   `json:"done"`
	Questions bool   `json:"questions"`
	// StatusQuestions is the hash of STATUS.md's Questions section, as an
	// answer names it, or "" when it asks nothing.
	StatusQuestions string     `json:"status_questions,omitempty"`
	Limited         bool       `json:"limited"`
	Trust           bool       `json:"trust"`
	Dialog          bool       `json:"dialog"`
	DialogShape     bool       `json:"dialog_shape,omitempty"` // the dialog has a dialog's shape, which alone may be declined
	Prompt          bool       `json:"prompt,omitempty"`       // the screen ends at the idle input prompt
	Command         string     `json:"command"`
	Head            string     `json:"head"`
	PaneHash        string     `json:"pane_hash,omitempty"`
	PaneSince       *time.Time `json:"pane_since,omitempty"`
	TranscriptRole  string     `json:"transcript_role,omitempty"`
	TranscriptStop  string     `json:"transcript_stop,omitempty"`
	TranscriptAt    *time.Time `json:"transcript_at,omitempty"`
	Restarts        *int       `json:"restarts,omitempty"`
	UnchangedS      *int       `json:"unchanged_s,omitempty"`
}

// TransitionPayload is a change of state and the evidence for it.
type TransitionPayload struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// ActPayload is one action: its name, the intent id that pairs an intent
// with its outcome, the key it settles, and the facts of its kind.
type ActPayload struct {
	Action string `json:"action"`
	Intent string `json:"intent,omitempty"`
	Key    string `json:"key,omitempty"`
	By     string `json:"by,omitempty"` // an owner's record: mark-delivered

	// A restart.
	Attempt int    `json:"attempt,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Respawn bool   `json:"respawn,omitempty"`
	Account string `json:"account,omitempty"`
	Command string `json:"command,omitempty"`
	// A restart or rotation: the Claude Code session id it launched, and
	// for a rotation the context it left and the merge it rotated at.
	Transcript string `json:"transcript,omitempty"`
	Context    int64  `json:"context,omitempty"`
	Checkpoint string `json:"checkpoint,omitempty"`
	// A coordinator's restart or rotation: the brief parts it wrote.
	Brief []BriefPart `json:"brief,omitempty"`
	// A stop: why. A wake: what is due.
	Reason string `json:"reason,omitempty"`
	// A wake: the coordinator's spend when it was woken, when known, and
	// the keys of what it covered, so none is due again.
	Spend   *int64   `json:"spend,omitempty"`
	Covered []string `json:"covered,omitempty"`
	// An answer, or a restart that hands answers over: the decisions whose
	// answers answers.json holds, newly delivered.
	Answers []string `json:"answers,omitempty"`
	// AnswersAt is when each of Answers was given, so a later answer to
	// the same decision is not taken as delivered.
	AnswersAt []time.Time `json:"answers_at,omitempty"`

	// Typing: a nudge, a resume, the trust keys.
	Message string `json:"message,omitempty"`
	Nudge   int    `json:"nudge,omitempty"`
	// A continue nudge: the hash of the STATUS.md it was given for.
	StatusHash string   `json:"status_hash,omitempty"`
	Keys       []string `json:"keys,omitempty"`
	PaneHash   string   `json:"pane_hash,omitempty"`

	// An escalation or Questions update; Message is its text.
	Console       string `json:"console,omitempty"`
	QuestionsHash string `json:"questions_hash,omitempty"`
	Update        int    `json:"update,omitempty"`
	Dialog        int    `json:"dialog,omitempty"`
	Episode       int    `json:"episode,omitempty"` // the Questions or dialog episode it settles

	// Delivery: a fetch, push or open.
	Delivered  string `json:"delivered,omitempty"`
	Repository string `json:"repository,omitempty"`
	Worktree   string `json:"worktree,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Head       string `json:"head,omitempty"`
	Target     string `json:"target,omitempty"`
	Method     string `json:"method,omitempty"` // a merge's method
	Title      string `json:"title,omitempty"`
	Draft      bool   `json:"draft,omitempty"`
	Ref        string `json:"ref,omitempty"`
	URL        string `json:"url,omitempty"`

	// A CI fix or the decision a repeated failure raised: the failure's
	// cause, the decision whose retry a fix took, and the decision raised.
	Cause    string    `json:"cause,omitempty"`
	Decision string    `json:"decision,omitempty"`
	Raised   *Decision `json:"raised,omitempty"`

	// A gate run.
	Gate []string `json:"gate,omitempty"`
	// Reachable are the credential paths under the real HOME a gate could
	// still open by path.
	Reachable []string `json:"reachable,omitempty"`
	SHA       string   `json:"sha,omitempty"`
	Exit      *int     `json:"exit,omitempty"`
	DurationS *float64 `json:"duration_s,omitempty"`
	Output    string   `json:"output,omitempty"`
}

// SkipPayload is an action a policy declined, keyed so it is logged once,
// with the reason and the facts it was declined on. Escalation names an
// escalation given up on; Delivered and Outcome settle a delivery without
// one (found already open, nothing to deliver, given up); Filtered names an
// escalation whose text was withheld.
type SkipPayload struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`

	Action     string `json:"action,omitempty"`
	Command    string `json:"command,omitempty"`
	Account    string `json:"account,omitempty"`
	Worktree   string `json:"worktree,omitempty"`
	Repository string `json:"repository,omitempty"`
	Target     string `json:"target,omitempty"`
	MRFile     string `json:"mr_file,omitempty"`
	Origin     string `json:"origin,omitempty"`
	Branch     string `json:"branch,omitempty"`
	SHA        string `json:"sha,omitempty"`
	Head       string `json:"head,omitempty"`
	Delivered  string `json:"delivered,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	URL        string `json:"url,omitempty"`

	Escalation    string `json:"escalation,omitempty"`
	Console       string `json:"console,omitempty"`
	Message       string `json:"message,omitempty"`
	Filtered      string `json:"filtered,omitempty"`
	QuestionsHash string `json:"questions_hash,omitempty"`
	Update        int    `json:"update,omitempty"`
	Dialog        int    `json:"dialog,omitempty"`
	Episode       int    `json:"episode,omitempty"`
}

// CrashPayload is one crash: its cause (exit status when known, and the
// last screen's hash), the crashes since the owner last cleared the
// session, and whether it is now parked.
type CrashPayload struct {
	ScreenHash   string `json:"screen_hash"`
	Exit         *int   `json:"exit,omitempty"`
	Crashes      int    `json:"crashes"`
	Parked       bool   `json:"parked"`
	RestartError bool   `json:"restart_error,omitempty"`
}

// ClearPayload is the owner's clearing of a parked session.
type ClearPayload struct {
	By string `json:"by"`
	// Base is set on the record a run writes after applying the owner's
	// clear: the session's spend then, which its cap counts from. It is
	// computed once, in the order the run applied events, so a replay
	// reads the same base rather than recomputing it.
	Base *int64 `json:"base,omitempty"`
}

// ErrorPayload is a failed step or action. Key, when set, is the keyed
// action whose failed try this is; Act is the action that failed.
type ErrorPayload struct {
	Step  string      `json:"step,omitempty"`
	Key   string      `json:"key,omitempty"`
	Error string      `json:"error"`
	Act   *ActPayload `json:"act,omitempty"`
	// EmptyPipeline: the forge answered that the action's pipeline would be
	// empty, so asking again for the same head and tip cannot help (#46).
	EmptyPipeline bool `json:"empty_pipeline,omitempty"`
	// Triage is a failed triage: the items it failed for, and what its
	// calls cost.
	Triage *TriagePayload `json:"triage,omitempty"`
}

// WarningPayload is something agentd noticed about itself: a torn last
// line of the log it cut off, or an intent with no outcome, an action cut
// short by a crash of agentd, which is shown and never silently repeated.
type WarningPayload struct {
	Warning  string      `json:"warning"`
	Bytes    int         `json:"bytes,omitempty"`
	Intent   *ActPayload `json:"intent,omitempty"`
	IntentAt *time.Time  `json:"intent_at,omitempty"` // when the intent was recorded
}

// check refuses an event this agentd cannot read exactly: another version,
// an unknown kind or mode, or a payload other than its kind's alone.
func (e Event) check() error {
	if e.V != EventVersion {
		return fmt.Errorf("version %d, not %d", e.V, EventVersion)
	}
	payloads := map[string]bool{
		KindStart: e.Start != nil, KindStop: e.Stop != nil, KindObserve: e.Observe != nil, KindTransition: e.Transition != nil,
		KindSkip: e.Skip != nil, KindCrash: e.Crash != nil, KindClear: e.Clear != nil, KindError: e.Error != nil, KindWarning: e.Warning != nil,
		KindItem: e.Item != nil, KindSeen: e.Seen != nil, KindCursor: e.Cursor != nil, KindUsage: e.Usage != nil,
		KindCheckpoint: e.Checkpoint != nil, KindPoll: e.Poll != nil, KindTriage: e.Triage != nil, KindDecision: e.Decision != nil,
		KindAnswer: e.Answer != nil, KindDeadline: e.Deadline != nil, KindCase: e.Case != nil, KindTurn: e.Turn != nil, KindCoordinator: e.Coordinator != nil, KindCI: e.CI != nil, KindFeed: e.Feed != nil, KindInstruction: e.Instruction != nil, KindSession: e.Runtime != nil, KindPolicy: e.Policy != nil, KindApproval: e.Approval != nil, KindAccount: e.Account != nil, KindMessage: e.Message != nil, KindInbox: e.Inbox != nil, KindJob: e.Job != nil, KindLoop: e.Loop != nil, "act": e.Act != nil,
	}
	want := e.Kind
	switch e.Kind {
	case KindIntent, KindAction, KindWould:
		want = "act"
	}
	if _, ok := payloads[want]; !ok {
		return fmt.Errorf("unknown kind %q", e.Kind)
	}
	for kind, set := range payloads {
		if set != (kind == want) {
			return fmt.Errorf("a %s event must carry its own payload and no other", e.Kind)
		}
	}
	// Items, cursors and triage are facts both modes read, a failed triage
	// too; the owner marks items seen.
	failedTriage := e.Kind == KindError && e.Error.Triage != nil
	switch {
	case e.Kind == KindMessage && (e.Session != "" || e.Mode != ModeShared || e.Message.ID == "" || e.Message.Console == "" || !validMessageKind(e.Message.Kind)):
		return errors.New("a message is a shared fact, for no one session, and names its id, console and kind")
	case e.Kind == KindInbox && (e.Session != "" || e.Mode != ModeShared || e.Inbox.Console == "" || e.Inbox.Reader == "" || len(e.Inbox.IDs) == 0):
		return errors.New("an inbox event is a shared fact, for no one session, and names its console, reader and the ids read")
	case e.Kind == KindJob && (e.Session != "" || e.Mode != ModeLive && e.Mode != ModeShadow || e.Job.Name == "" || !validJobPhase(e.Job.Phase)):
		return errors.New("a job event is a run's, for no one session, and names its job and a phase")
	case e.Kind == KindLoop && (e.Session != "" || e.Loop.Repository == "" || e.Loop.Ref == "" || !validLoopStep(e.Loop.Step) || (e.Mode == ModeOwner) != (e.Loop.Step == LoopRetry) || e.Mode != ModeOwner && e.Mode != ModeLive && e.Mode != ModeShadow):
		return errors.New("a loop event is a run's step, or the owner's retry, for no one session, and names its repository, request and step")
	case e.Kind == KindAccount && (e.Session != "" || e.Mode != ModeShared || e.Account.Account == ""):
		return errors.New("an account reading is a shared fact, for no one session, and names its account")
	case e.Kind == KindApproval && (e.Session == "" || e.Mode != ModeShared || e.Approval.Repository == "" || e.Approval.Ref == "" || e.Approval.SHA == ""):
		return errors.New("an approval is a shared fact of the coordinator that gave it, and names its repository, request and head")
	case e.Kind == KindPolicy && (e.Session != "" || e.Mode != ModeShared || e.Policy.Repository == "" || e.Policy.Tip == ""):
		return errors.New("a policy event is a shared fact, for no one session, and names its repository and tip")
	case e.Kind != KindCase && e.Kind != KindCoordinator && e.Kind != KindSession && e.Kind != KindPolicy && e.Kind != KindApproval && e.Kind != KindAccount && e.Kind != KindMessage && e.Kind != KindInbox && (e.Kind == KindItem || e.Kind == KindCursor || e.Kind == KindTriage || failedTriage) != (e.Mode == ModeShared):
		return fmt.Errorf("a %s event must not carry mode %q", e.Kind, e.Mode)
	case e.Kind == KindSeen && e.Mode != ModeOwner:
		return fmt.Errorf("a seen event is the owner's, not %q", e.Mode)
	case e.Kind == KindAnswer && (e.Session == "" || e.Mode == ModeShared || (e.Mode == ModeOwner) == e.Answer.ByDefault):
		return errors.New("an answer event is the owner's, or a run's applying a default, for one session")
	case e.Kind == KindDeadline && (e.Mode != ModeOwner || e.Session == ""):
		return errors.New("a deadline event is the owner's, for one session")
	case e.Kind == KindDecision && (e.Mode == ModeOwner || e.Session == ""):
		return errors.New("a decision event is a run's, for one session")
	case e.Kind == KindTurn && (e.Mode == ModeOwner || e.Mode == ModeShared || e.Session == "" || e.Turn.Role != TurnPrompt && e.Turn.Role != TurnSummary):
		return errors.New("a turn event is a run's, for one session, and a prompt or a summary")
	case e.Kind == KindCoordinator && (e.Coordinator.Repository == "" || !(e.Mode == ModeShared && e.Session != "" || e.Mode == ModeOwner && e.Session == "")):
		return errors.New("a coordinator claim is a shared fact of one session; a release is the owner's, for no session")
	case e.Kind == KindCI && (e.Mode == ModeOwner || e.Mode == ModeShared || e.Session == "" || e.CI.Repository == "" || e.CI.Ref == ""):
		return errors.New("a ci event is a run's, for the session that delivered the request, and names its repository and request")
	case e.Kind == KindFeed && (e.Session != "" || (e.Feed.Phase == FeedImported) != (e.Mode == ModeOwner) || e.Mode != ModeLive && e.Mode != ModeShadow && e.Mode != ModeOwner):
		return errors.New("a feed event is a run's, for no one session; an imported line is the owner's")
	case e.Kind == KindInstruction && (e.Session != "" || e.Mode != ModeOwner || e.Instruction.ID == "" || (len(e.Instruction.Repositories) == 0) == (e.Instruction.Console == "")):
		return errors.New("an instruction event is the owner's, for no one session, and names its id and either its repositories or its console")
	case e.Kind == KindSession && (e.Session == "" || e.Mode != ModeShared && e.Mode != ModeOwner || e.Runtime.ID == "" || e.Runtime.Repository == "" ||
		e.Runtime.Action != SessionStart && e.Runtime.Action != SessionEnd && e.Runtime.Action != SessionPlace ||
		e.Runtime.Action == SessionStart && (e.Mode != ModeShared || e.Runtime.Branch == "") ||
		e.Runtime.Action == SessionPlace && (e.Mode != ModeShared || e.Runtime.Account == "")):
		return errors.New("a session event names its session, id and repository: a start is a coordinator's, shared, and names its branch; a placement is shared and names its account; an end is shared or the owner's")
	case e.Kind == KindCase:
		if err := e.Case.check(); err != nil {
			return err
		}
		// A session's tool call is a fact both runs read, like an item.
		owner := e.Mode == ModeOwner && e.Session == ""
		run := (e.Mode == ModeLive || e.Mode == ModeShadow) && e.Session != ""
		shared := e.Mode == ModeShared && e.Session != ""
		switch c := e.Case; {
		case (c.Ruling != nil || c.Goal != nil || c.Rounds != nil) && !owner, (c.Pack != nil || c.Report != nil) && !run, c.Call != nil && !shared:
			return errors.New("a ruling, goal or rounds record is the owner's, for no one session; a pack or report is a run's, for one; a call is a shared fact of one session")
		}
	}
	switch e.Mode {
	case ModeLive, ModeShadow, ModeOwner, ModeShared:
	case "":
		if e.Kind != KindWarning {
			return fmt.Errorf("a %s event must carry its mode", e.Kind)
		}
	default:
		return fmt.Errorf("unknown mode %q", e.Mode)
	}
	return nil
}

// Log appends events to a JSONL file. It is safe for concurrent use, and
// across processes: each append holds an exclusive flock on the file, so
// mark-delivered cannot interleave with run, and is synced before it returns.
type Log struct {
	mu sync.Mutex
	w  io.Writer
	f  *os.File // the log file, when there is one
}

// OpenLog opens path for appending, creating its directory and the file,
// readable by its owner only.
func OpenLog(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{w: f, f: f}, nil
}

// NewLogWriter logs to any writer; tests use a buffer. A writer with a Sync
// method is synced after each append.
func NewLogWriter(w io.Writer) *Log { return &Log{w: w} }

// lock takes the file's exclusive lock, waiting for another process's
// append to finish, and returns its release.
func (l *Log) lock() (func(), error) {
	if l.f == nil {
		return func() {}, nil
	}
	fd := int(l.f.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("locking the event log: %w", err)
	}
	return func() { syscall.Flock(fd, syscall.LOCK_UN) }, nil
}

// Append writes one event as one line and syncs it. It first repairs a
// torn final line, so the event never lands glued to a fragment that the
// next run would cut off with it.
func (l *Log) Append(e Event) error {
	if err := e.check(); err != nil {
		return fmt.Errorf("refusing to log an event agentd could not read back: %w", err)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, err := l.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if l.f != nil {
		if _, err := l.repairTail(); err != nil {
			return err
		}
	}
	return l.write(append(b, '\n'))
}

// write writes and syncs, with the lock held. When the write or the sync
// fails, a log file is cut back to its size before, so a failed append, a
// full disk say, never leaves a fragment for the next append to follow.
func (l *Log) write(b []byte) error {
	size := int64(-1)
	if l.f != nil {
		info, err := l.f.Stat()
		if err != nil {
			return err
		}
		size = info.Size()
	}
	_, err := l.w.Write(b)
	if s, ok := l.w.(interface{ Sync() error }); ok && err == nil {
		err = s.Sync()
	}
	if err != nil && size >= 0 {
		if terr := l.f.Truncate(size); terr != nil {
			return errors.Join(err, fmt.Errorf("cutting back the failed append: %w", terr))
		}
	}
	return err
}

// Load reads every event in the open log under its lock, after repairing a
// torn final line. It returns the events and the length of the torn line it
// cut off, if any.
func (l *Log) Load() ([]Event, int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, err := l.lock()
	if err != nil {
		return nil, 0, err
	}
	defer unlock()
	torn, err := l.repairTail()
	if err != nil {
		return nil, 0, err
	}
	info, err := l.f.Stat()
	if err != nil {
		return nil, 0, err
	}
	events, _, err := ReadEvents(io.NewSectionReader(l.f, 0, info.Size()))
	return events, torn, err
}

// repairTail makes the file end at a line boundary, with the lock held. A
// final line without its newline is a write cut short: one that does not
// parse is cut off, with a warning event saying how long it was, and one
// that parses gets its newline. It returns the length cut off.
func (l *Log) repairTail() (int, error) {
	info, err := l.f.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	start := size // where the final line begins
	buf := make([]byte, 64*1024)
	for start > 0 {
		n := int64(len(buf))
		if n > start {
			n = start
		}
		if _, err := l.f.ReadAt(buf[:n], start-n); err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			start = start - n + int64(i) + 1
			break
		}
		start -= n
	}
	if start == size {
		return 0, nil
	}
	tail := make([]byte, size-start)
	if _, err := l.f.ReadAt(tail, start); err != nil {
		return 0, err
	}
	if _, err := parseEvent(tail); err == nil {
		return 0, l.write([]byte{'\n'})
	}
	if err := l.f.Truncate(start); err != nil {
		return 0, err
	}
	torn := int(size - start)
	w, err := json.Marshal(Event{V: EventVersion, Time: time.Now(), Kind: KindWarning, Warning: &WarningPayload{
		Warning: "dropped a torn final line of the event log", Bytes: torn}})
	if err != nil {
		return 0, err
	}
	return torn, l.write(append(w, '\n'))
}

// Size is the log's length in bytes, the offset Tail starts after.
func (l *Log) Size() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		if info, err := l.f.Stat(); err == nil {
			return info.Size()
		}
		return 0
	}
	if b, ok := l.w.(interface{ Bytes() []byte }); ok {
		return int64(len(b.Bytes()))
	}
	return 0
}

// Tail reads the whole lines appended after offset off, by this process or
// another (agentd clear), and returns their events and the offset after
// them. A log written to a plain writer has no tail unless the writer can
// show its bytes, as the tests' buffers do.
func (l *Log) Tail(off int64) ([]Event, int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var data []byte
	switch b, ok := l.w.(interface{ Bytes() []byte }); {
	case l.f != nil:
		unlock, err := l.lock()
		if err != nil {
			return nil, off, err
		}
		defer unlock()
		info, err := l.f.Stat()
		if err != nil || info.Size() <= off {
			return nil, off, err
		}
		data = make([]byte, info.Size()-off)
		if _, err := l.f.ReadAt(data, off); err != nil {
			return nil, off, err
		}
	case ok && int64(len(b.Bytes())) > off:
		data = b.Bytes()[off:]
	default:
		return nil, off, nil
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return nil, off, nil
	}
	events, _, err := ReadEvents(bytes.NewReader(data[:end+1]))
	return events, off + int64(end+1), err
}

// Close closes the underlying file, if any.
func (l *Log) Close() error {
	if l.f == nil {
		return nil
	}
	return l.f.Close()
}

// ReadLog reads every event in path, in order, without changing it. A
// missing file is an empty log. See ReadEvents for malformed lines.
func ReadLog(path string) ([]Event, int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	return ReadEvents(f)
}

// ReadEvents parses JSONL events from r. A malformed final line without its
// newline is a write cut short: it is left out and its length in bytes
// returned, so the caller can warn. Any other malformed line, a complete
// last line included, is corruption, and an error: a replay that skipped it
// could repeat the action it recorded.
func ReadEvents(r io.Reader) ([]Event, int, error) {
	var events []Event
	br := bufio.NewReader(r)
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, 0, fmt.Errorf("reading event log: %w", err)
		}
		final := err == io.EOF // the line has no newline
		if len(bytes.TrimSpace(line)) > 0 {
			e, perr := parseEvent(line)
			if perr != nil {
				if final {
					return events, len(line), nil
				}
				return nil, 0, fmt.Errorf("event log line %d is malformed (%v); fix or remove it, or start a fresh log, before starting", n, perr)
			}
			events = append(events, e)
		}
		if final {
			return events, 0, nil
		}
	}
}

// parseEvent reads one line strictly: a field the format lacks, or an
// event check refuses, is malformed.
func parseEvent(line []byte) (Event, error) {
	var e Event
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return Event{}, err
	}
	return e, e.check()
}
