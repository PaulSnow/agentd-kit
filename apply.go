package main

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// LogState is what the event log says for one mode: each session's Track,
// the intents still waiting for their outcome, and the run's start and
// stop. apply is the only code that changes what the log decides, so
// replay, the live path (emit, then apply) and every view read the log the
// same way. The one exception is the pane memory a pass keeps between its
// own observations (Classify's noteHash), which the observe event it logs
// then records.
type LogState struct {
	Mode    string
	Tracks  map[string]*Track
	Pending map[string]Event // intents with no outcome yet, by intent id
	Started time.Time
	Stopped time.Time
	Events  int                   // events of this mode, the owner's and the shared ones, applied
	Last    time.Time             // the latest time of those events
	Items   map[string]*Item      // new items by id, and whether the owner saw them
	Cursors map[string]time.Time  // how far each source of items has been read
	Polls   map[string]PollRecord // each forge source's latest poll record
	Cases   map[string]*Case      // each case by id
	// Instructions are the owner's console instructions, by id.
	Instructions map[string]*Instruction
	// CI is each watched request's latest CI, by repository and request.
	CI map[string]CIRecord
	// Feed is what the consoles' feed has posted.
	Feed FeedState
	// Runtime are the sessions agentd started and nobody ended, by
	// name; Ended are those ended whose tmux session is not yet stopped,
	// with their repository.
	Runtime map[string]RuntimePayload
	Ended   map[string]string
	// Policies are each repository's own policy file on each target
	// branch, as last read, by policyKey; filePats the confidential
	// patterns each adds, compiled.
	Policies map[string]PolicyPayload
	filePats map[string][]*regexp.Regexp
	// Approvals are each request no session delivered that the loop's
	// review converged on, at its head, by ciKey (#36, #53).
	Approvals map[string]ApprovalRecord
	// Accounts are each account's latest usage reading (#39), and
	// StartedAt when each started session was started.
	Accounts  map[string]AccountRecord
	StartedAt map[string]time.Time
	// Jobs are the scheduled jobs' latest slots and runs (#51).
	Jobs map[string]JobRecord
	// Loops are the steps agentd took in each request's loop, by ciKey
	// (#53).
	Loops map[string]LoopRecord
	// Commented are the thread outcomes agentd posted on issues, by key
	// (#60), so none is posted twice.
	Commented map[string]time.Time
	// Facts are what the latest fact of each session's run, brief and Now,
	// and of each issue's state, said (#62), so a change is logged once.
	Facts map[string]string
	// FactAt is when each session's Now was last a fact.
	FactAt map[string]time.Time
	// Ledger is the readings and the tokens charged to each account over
	// the last ledgerKeep (#64); a worker's state has none.
	Ledger *AccountLedger

	cfg *Config // its policies, read as each event is applied
}

// NewLogState is the empty state of mode under the configuration's policies.
func NewLogState(cfg *Config, mode string) *LogState {
	return &LogState{Mode: mode, Tracks: map[string]*Track{}, Pending: map[string]Event{}, Items: map[string]*Item{}, Instructions: map[string]*Instruction{},
		Cursors: map[string]time.Time{}, Polls: map[string]PollRecord{}, Cases: map[string]*Case{},
		CI: map[string]CIRecord{}, Runtime: map[string]RuntimePayload{}, Ended: map[string]string{}, Policies: map[string]PolicyPayload{}, Approvals: map[string]ApprovalRecord{}, Ledger: newAccountLedger(), cfg: cfg}
}

// setItem changes a copy of the item and puts the copy in its place: a
// worker's state shares the loop's items, so an item is never changed in
// place.
func (st *LogState) setItem(id string, change func(*Item)) {
	if it := st.Items[id]; it != nil {
		c := *it
		change(&c)
		st.Items[id] = &c
	}
}

// itemChanged says whether it is new to st, or carries another title than
// the item st has under its id.
func (st *LogState) itemChanged(it ItemPayload) bool {
	known, ok := st.Items[it.ID]
	return !ok || ItemHash(known.ItemPayload) != ItemHash(it)
}

// track returns the session's Track, creating it.
func (st *LogState) track(name string) *Track {
	t, ok := st.Tracks[name]
	if !ok {
		t = newTrack()
		st.Tracks[name] = t
	}
	return t
}

// Fold applies every event in order.
func (st *LogState) Fold(events []Event) *LogState {
	for _, e := range events {
		st.apply(e)
	}
	return st
}

// CutShort lists the intents with no outcome, oldest first: actions a
// crash of agentd cut short.
func (st *LogState) CutShort() []Event {
	out := make([]Event, 0, len(st.Pending))
	for _, e := range st.Pending {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

// apply folds one event into the state. Events of the other mode are
// ignored; the owner's records and the shared facts count for both. In
// shadow mode a would event counts as the action taken, so the shadow keeps
// its own counters.
func (st *LogState) apply(e Event) {
	if e.Mode != st.Mode && e.Mode != ModeOwner && e.Mode != ModeShared && e.Mode != "" {
		return
	}
	st.Events++
	if e.Time.After(st.Last) {
		st.Last = e.Time
	}
	// After the fold, so a placement is the ledger's only once it holds.
	defer st.applyLedger(e)
	if e.Kind == KindCase {
		st.applyCase(e)
		return
	}
	if e.Kind == KindCI {
		st.applyCI(e)
		st.track(e.Session).noteCI(e.Time, e.CI)
		return
	}
	if e.Kind == KindSession {
		st.applySession(e)
		return
	}
	if e.Kind == KindPolicy {
		st.applyPolicy(e)
		return
	}
	if e.Kind == KindApproval || e.Kind == KindCoordinator || e.Act != nil && e.Act.Action == ActWake {
		// An earlier version's coordinator approval, claim, release or
		// wake: the coordinator is gone (#53), and only the loop approves.
		return
	}
	if e.Kind == KindLoop {
		st.applyLoop(e)
		return
	}
	if e.Kind == KindCard || e.Kind == KindThread || e.Kind == KindQuery {
		return // the repository's context, which its store folds (#60)
	}
	if e.Kind == KindRoute {
		st.applyRoute(e)
		return
	}
	if e.Kind == KindFact {
		st.applyFact(e)
		return
	}
	if e.Session == "" {
		switch e.Kind {
		case KindAccount:
			st.applyAccount(e)
		case KindJob:
			st.applyJob(e)
		case KindFeed:
			st.applyFeed(e)
		case KindStart:
			st.Started, st.Stopped = e.Time, time.Time{}
		case KindStop:
			st.Stopped = e.Time
		case KindItem:
			// An item is one however often it is seen: the first sighting's
			// time stands, and so does the owner's having seen it. A
			// sighting under a new title replaces the item's text, so it is
			// triaged again.
			if _, ok := st.Items[e.Item.ID]; !ok {
				st.Items[e.Item.ID] = &Item{ItemPayload: *e.Item, FirstSeen: e.Time}
			} else if st.itemChanged(*e.Item) {
				st.setItem(e.Item.ID, func(it *Item) { it.ItemPayload = *e.Item })
			}
		case KindAction:
			if e.Act.Action == ActComment {
				if st.Commented == nil {
					st.Commented = map[string]time.Time{}
				}
				st.Commented[e.Act.Key] = e.Time
			}
		case KindInstruction:
			if _, ok := st.Instructions[e.Instruction.ID]; !ok {
				st.Instructions[e.Instruction.ID] = &Instruction{InstructionPayload: *e.Instruction, At: e.Time}
			}
		case KindAttachment:
			if in := st.Instructions[e.Attachment.Message]; in != nil && !hasAttachment(in.Attachments, e.Attachment.ID) {
				in.Attachments = append(in.Attachments, *e.Attachment)
			}
		case KindSeen:
			for _, id := range e.Seen.IDs {
				st.setItem(id, func(it *Item) { it.Seen = true })
			}
		case KindCursor:
			st.Cursors[e.Cursor.Source] = e.Cursor.AsOf
		case KindPoll:
			st.Polls[e.Poll.Source] = PollRecord{PollPayload: *e.Poll, At: e.Time}
		case KindTriage:
			for _, r := range e.Triage.Results {
				r := r
				st.setItem(r.ID, func(it *Item) { it.Triage = &r })
			}
		case KindError:
			if e.Error.Triage != nil {
				for _, id := range e.Error.Triage.Failed {
					st.setItem(id, func(it *Item) { it.TriageFailed = e.Time })
				}
			}
		}
		return
	}
	t := st.track(e.Session)
	switch e.Kind {
	case KindObserve:
		o := e.Observe
		t.LastSeen, t.Head, t.Reason = e.Time, o.Head, o.Reason
		if o.Alive {
			t.Live = true
			t.noteCommand(o.Command, st.cfg.Policies.PaneCommands, e.Time)
		}
		t.Observed = true
		if o.Alive {
			t.Hash = o.PaneHash
		}
		// Claude Code's input prompt, or a transcript written since the
		// launch, says the session is past its folder-trust prompt.
		if o.Alive && (o.Prompt || o.TranscriptAt != nil && !o.TranscriptAt.Before(t.Launched)) {
			t.Ready = true
		}
		if o.PaneSince != nil && !o.PaneSince.Equal(t.HashSince) {
			t.HashSince = *o.PaneSince
			t.Nudges = 0
		}
		// The limit is remembered from a pass that found the session
		// usage-limited until the resume line is typed, not only while the
		// message shows: that is when it is typed.
		if State(o.State) == UsageLimited {
			t.Limited = true
		}
		if o.Dialog && !t.Dialog.Showing {
			t.Dialog.Episode++
		}
		// The decline is timed from the first pass that showed a dialog's
		// shape, not from words a pattern matched before it.
		if !o.DialogShape {
			t.Dialog.Since = time.Time{}
		} else if t.Dialog.Since.IsZero() {
			t.Dialog.Since = e.Time
		}
		t.Dialog.Showing = o.Dialog
		t.StatusQuestions = o.StatusQuestions
		t.Questions.observeQuestions(o.Questions, o.Done)
		if !o.Questions || o.Done {
			t.Asked = ""
		}
		if o.State != "" && t.State == "" {
			t.State, t.Since = State(o.State), e.Time
		}
	case KindUsage:
		u := *e.Usage
		t.Usage[u.Transcript] = u
		if t.Transcript == "" || u.Transcript == t.Transcript || u.Guessed || u.Cleared {
			t.Measured = u.Transcript
		}
	case KindCheckpoint:
		t.Checkpoint = Checkpoint{CheckpointPayload: *e.Checkpoint, At: e.Time}
	case KindStatus:
		t.applyStatus(e.Time, e.Status)
	case KindTurn:
		t.applyTurn(e.Turn)
		st.instructionTurn(e.Session, e.Turn)
	case KindTransition:
		t.State, t.Since = State(e.Transition.To), e.Time
		t.Reason = e.Transition.Reason
	case KindCrash:
		c := Crash{Exit: -1, Screen: e.Crash.ScreenHash}
		if e.Crash.Exit != nil {
			c.Exit = *e.Crash.Exit
		}
		t.noteCrash(c, st.cfg.Policies.Restart.MaxCrashes)
	case KindDecision:
		t.DecisionsHash = e.Decision.Hash
		if e.Decision.Refused == "" {
			t.Decisions = e.Decision.Decisions
		}
	case KindAnswer:
		// The latest answer to a decision stands, to be delivered anew; a
		// default never replaces an answer to the decision as it was
		// asked, such as one the owner gave while the pass that applied
		// it was under way.
		if prev, ok := t.Answers[e.Answer.ID]; e.Answer.ByDefault && ok && prev.Asked == e.Answer.Asked {
			break
		}
		t.Answers[e.Answer.ID] = AnswerRecord{AnswerPayload: *e.Answer, At: e.Time}
	case KindDeadline:
		t.Deadlines[e.Deadline.ID] = DeadlineRecord{DeadlinePayload: *e.Deadline, Set: e.Time}
	case KindClear:
		if b := e.Clear.Base; b != nil {
			t.SpendBase = *b
			break
		}
		t.clear()
	case KindSkip:
		k := e.Skip
		if k.Delivered != "" {
			t.notePushed(k.Delivered, k.Head)
			st.ciSeen(t)
			t.Delivered[k.Delivered] = k.Outcome
			t.DeliveredRefs = append(t.DeliveredRefs, k.Outcome)
			// Found already open or merged; failed and nothing deliver nothing.
			if k.Outcome != "failed" && k.Outcome != "nothing" {
				t.Deliveries = append(t.Deliveries, DeliveryRecord{Ref: k.Outcome, Repository: k.Repository, Delivered: k.Delivered, At: e.Time})
			}
		}
		if k.Filtered != "" {
			t.Filtered[k.Filtered] = true
		}
		if k.Escalation != "" {
			t.Posted[k.Escalation] = true // given up on
			t.GaveUp = append(t.GaveUp, k.Escalation)
			t.settleQuestions(k.QuestionsHash, k.Update, k.Episode, e.Time)
			t.settleDialog(k.Dialog, k.Episode, e.Time)
		}
		if k.Worktree != "" && k.Delivered == "" {
			t.Blocked[k.Worktree] = k.Reason
		}
		t.Skipped[k.Key] = true
	case KindError:
		t.Errors++
		if k := e.Error.Key; k != "" {
			t.Failures[k] = append(t.Failures[k], e.Time)
		}
		if a := e.Error.Act; a != nil {
			delete(st.Pending, a.Intent)
			if a.Action == ActRestart {
				t.Restarts = append(t.Restarts, e.Time)
			}
			if a.Action == ActRetest && e.Error.EmptyPipeline {
				if t.Unretestable == nil {
					t.Unretestable = map[string]bool{}
				}
				t.Unretestable[a.Key] = true
			}
		}
	case KindIntent:
		if e.Act.Intent != "" {
			st.Pending[e.Act.Intent] = e
		}
	case KindAction, KindWould:
		delete(st.Pending, e.Act.Intent)
		if e.Kind == KindWould {
			if st.Mode != ModeShadow {
				return
			}
			t.Would++
		}
		t.applyAction(e.Act, e.Kind == KindAction, true, e.Time)
		st.instructionsDelivered(e.Session, e.Act, e.Time)
		st.ciSeen(t)
		if e.Act.Action == ActEnd {
			delete(st.Ended, e.Session)
		}
		if a := e.Act; e.Kind == KindAction && a.Action == ActGate && a.Exit != nil && *a.Exit == 0 && a.SHA != "" && !contains(t.Gated, a.SHA) {
			t.Gated = append(t.Gated[:len(t.Gated):len(t.Gated)], a.SHA)
		}
		if a := e.Act; (a.Action == ActEscalate || a.Action == ActUpdate) && strings.HasPrefix(a.Key, "questions:") && a.Episode == t.Questions.Episode {
			t.Asked, t.AskedAt = questionText(e.Session, a.Message), e.Time
			if t.Filtered[a.Key] {
				t.Asked = "" // the generic message, not the questions
			}
		}
	case KindWarning:
		// An intent with no outcome was cut short by a crash of agentd. It
		// is shown, and what the owner or the session may have seen is
		// counted as done, so it is never repeated: an escalation never
		// posts twice and a line is never typed twice.
		if a := e.Warning.Intent; a != nil {
			delete(st.Pending, a.Intent)
			t.CutShort = append(t.CutShort, a.Action+" "+a.Key)
			at := e.Time
			if e.Warning.IntentAt != nil {
				at = *e.Warning.IntentAt
			}
			switch a.Action {
			case ActEscalate, ActUpdate, ActNudge, ActResume, ActTrust, ActRotate, ActDecline, ActMerge, ActRetest:
				t.applyAction(a, false, false, at)
			}
			// An answer cut short may not have reached the file: it is
			// delivered again, and the fixed pointer typed twice is harmless.
			// A rotation cut short may have started its session: measure
			// the transcript it named, which a guess replaces if it never
			// appears. A stop cut short is simply tried again.
			if a.Action == ActRotate {
				t.launched(a.Transcript, at)
			}
		}
	}
}

// applyAction folds an action taken (or, in shadow mode, one that would
// have been). taken is false for a would event and for an action cut short,
// which started nothing that can be observed; counted is false for an
// action cut short, which the views list apart rather than count.
func (t *Track) applyAction(a *ActPayload, taken, counted bool, at time.Time) {
	n := 0
	if counted {
		n = 1
	}
	// The answers an answer or a restart handed over are delivered, a
	// would-be one's too, so shadow mode records each once.
	for i, id := range a.Answers {
		if r, ok := t.Answers[id]; ok && (i >= len(a.AnswersAt) || r.At.Equal(a.AnswersAt[i])) {
			r.Delivered = true
			t.Answers[id] = r
		}
	}
	switch a.Action {
	case ActRestart:
		t.Restarts = append(t.Restarts, at)
		t.RestartCount += n
		if taken { // a would-be restart started nothing
			t.Hash, t.HashSince, t.Observed, t.Live = "", at, true, true
			t.Nudges, t.LastNudge, t.Limited = 0, time.Time{}, false
			t.launched(a.Transcript, at)
			t.launchedBrief(a)
		}
	case ActRotate:
		// Not a restart: the backoff, the restart count and the crash
		// record are untouched, and the session stays live.
		t.Rotations += n
		t.RotatedFor = a.Checkpoint
		if taken {
			t.Hash, t.HashSince, t.Observed = "", at, true
			t.Nudges, t.LastNudge, t.Limited = 0, time.Time{}, false
			t.launched(a.Transcript, at)
			t.launchedBrief(a)
		}
	case ActCIFix, ActFixRound:
		// Not a restart or a crash: its count is the fix's own. A would-be
		// fix is recorded too, so shadow mode would start it once. A fix
		// round (#53) is delivered and watched as a CI fix is.
		if counted {
			t.CIFixes = append(t.CIFixes, CIFixRecord{Ref: a.Ref, Repository: a.Repository, Delivered: a.Delivered, Head: a.Head, Cause: a.Cause,
				Decision: a.Decision, Transcript: a.Transcript, Taken: taken, At: at, Review: a.Action == ActFixRound})
		}
		if taken {
			// The delivery waits for the fix, and is recorded again after it.
			delete(t.Delivered, a.Delivered)
			t.Hash, t.HashSince, t.Observed, t.Live = "", at, true, true
			t.Nudges, t.LastNudge, t.Limited = 0, time.Time{}, false
			t.launched(a.Transcript, at)
		}
	case ActMerge:
		// Taken, would-be or cut short: the forge may have merged it, so
		// it is never tried again.
		if t.Merges == nil {
			t.Merges = map[string]string{}
		}
		t.Merges[ciKey(a.Repository, a.Ref)] = a.Head
		if taken {
			if t.Merged == nil {
				t.Merged = map[string]string{}
			}
			t.Merged[ciKey(a.Repository, a.Ref)] = a.Head
		}
	case ActRetest:
		// Taken, would-be or cut short: once per head and tip.
		if t.Retests == nil {
			t.Retests = map[string]bool{}
		}
		t.Retests[a.Key] = true
	case ActRaise:
		if counted && a.Raised != nil {
			if t.CIRaised == nil {
				t.CIRaised = map[string]string{}
			}
			t.CIRaised[a.Key] = a.Raised.ID
			// Not the session's own: its D numbering is left alone.
			if !t.hasRaised(a.Raised.ID) {
				t.Raised = append(t.Raised[:len(t.Raised):len(t.Raised)], *a.Raised)
			}
		}
	case ActProvision:
		t.Provisioned = a.Key
	case ActStop:
		t.Stopped = a.Reason
		if taken { // its session is gone by the owner's policy, not a crash
			t.Live = false
		}
	case ActNudge:
		t.Nudges++
		if a.StatusHash != "" {
			if a.StatusHash != t.ContinueStatus {
				t.Continues, t.ContinueStatus = 0, a.StatusHash
			}
			t.Continues++
		}
		t.NudgeCount += n
		t.LastNudge = at
		if a.PaneHash != "" { // the screen after the nudge's echo
			t.Hash = a.PaneHash
		}
	case ActResume:
		t.Limited = false
	case ActTrust:
		t.TrustHash = a.PaneHash
	case ActDecline:
		// Once per episode, a would-be or cut-short one too: Escape twice
		// could reach whatever the first left on screen.
		if a.Episode > t.Dialog.Declined {
			t.Dialog.Declined, t.Dialog.DeclinedAt = a.Episode, at
		}
	case ActEscalate, ActUpdate:
		t.Posted[a.Key] = true
		if a.Action == ActEscalate {
			t.Escalations += n
			if counted {
				t.Escalated = append(t.Escalated, EscalationRecord{Key: a.Key, Message: a.Message, At: at})
				if len(t.Escalated) > keptEscalations {
					t.Escalated = t.Escalated[len(t.Escalated)-keptEscalations:]
				}
			}
		}
		t.settleQuestions(a.QuestionsHash, a.Update, a.Episode, at)
		t.settleDialog(a.Dialog, a.Episode, at)
	case ActOpen:
		if taken {
			t.notePushed(a.Delivered, a.Head)
		}
		ref := a.Ref
		if ref == "" {
			ref = "would-open"
		}
		t.Delivered[a.Delivered] = ref
		t.DeliveredRefs = append(t.DeliveredRefs, ref)
		t.Deliveries = append(t.Deliveries, DeliveryRecord{Ref: ref, Repository: a.Repository, Delivered: a.Delivered, At: at})
	}
}

// keptEscalations is how many of a session's latest escalations its Track
// keeps, for the page.
const keptEscalations = 10

// EscalationRecord is one escalation posted: its key, its text and when.
type EscalationRecord struct {
	Key     string
	Message string
	At      time.Time
}

// settleQuestions settles a Questions escalation or update, posted or given
// up on: it carries the hash of the text it covered, and the episode it was
// posted for. One for an episode that has since closed settles nothing.
func (t *Track) settleQuestions(hash string, update, episode int, at time.Time) {
	if hash == "" || episode != t.Questions.Episode {
		return
	}
	t.Questions.Hash = hash
	if update > 0 {
		t.Questions.Updates, t.Questions.LastUpdate = update, at
	}
}

// settleDialog settles dialog escalation n, posted or given up on, for
// its episode.
func (t *Track) settleDialog(n, episode int, at time.Time) {
	if n <= 0 {
		return
	}
	if n > t.Dialog.N {
		t.Dialog.N, t.Dialog.Last = n, at
	}
	if episode > t.Dialog.Escalated {
		t.Dialog.Escalated = episode
	}
}

// launched records the transcript a restart or rotation started: the
// session is measured under it from now on, unknown until its first call.
func (t *Track) launched(transcript string, at time.Time) {
	// A new Claude can draw the same trust prompt, the same screen: it
	// has not had the keys.
	t.Launched, t.TrustHash, t.Ready = at, "", false
	t.Continues, t.ContinueStatus = 0, ""
	if transcript != "" {
		t.Transcript, t.Measured = transcript, ""
	}
}

// launchedBrief keeps the brief parts a launch wrote; a launch that wrote
// none leaves the last ones, since its BRIEF.md is as they made it.
func (t *Track) launchedBrief(a *ActPayload) {
	if a.Brief != nil {
		t.Brief = a.Brief
	}
}
