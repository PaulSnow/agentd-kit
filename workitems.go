package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

// Work items (#66): told means done. Every owner instruction a thread
// agent reads as asking for work (its answer's work) becomes a work item
// in the log: the owner's words, the repository, done_when, the check
// the agent wrote from the instruction for the end state that shows it
// done, the session assigned and a state. Only Go changes the state: a
// session's or an agent's words never close an item. Each pass drives
// the open ones (driveItems): an item with no session is assigned one, a
// running session the agent named or one started for it; one not
// acknowledged in time is typed again once, then re-assigned; a session
// that writes DONE or ends with its item not done gives it back, its
// STATUS.md kept as a note; done_when is checked every poll_interval and
// at once on a claim, and only its check, or the owner, closes the item;
// one with no progress for items.escalate_after goes to the owner once.

// KindWorkItem is a step of an owner's work item.
const KindWorkItem = "work_item"

// Work item states.
const (
	WorkOpen         = "open"
	WorkAssigned     = "assigned"
	WorkAcknowledged = "acknowledged"
	WorkWorking      = "working"
	WorkBlocked      = "blocked"
	WorkDone         = "done"
	WorkDeclined     = "declined"
)

// Work item steps: open, then the state changes, and the bookkeeping
// that changes no state (typed, claim, check, escalate, told).
const (
	WorkStepOpen     = "open"
	WorkStepAssign   = "assign"
	WorkStepTyped    = "typed"    // the item's line was typed into its session
	WorkStepAck      = "ack"      // the session named the item: on its screen or in STATUS.md
	WorkStepWorking  = "working"  // the session's STATUS.md names it; or the owner said not done
	WorkStepBlocked  = "blocked"  // the session asks the owner a question
	WorkStepClaim    = "claim"    // the session says it is done, or wrote DONE
	WorkStepCheck    = "check"    // done_when checked: done when true
	WorkStepReopen   = "reopen"   // the session gave it back, or was taken off it
	WorkStepDone     = "done"     // the owner said it is done
	WorkStepDeclined = "declined" // the owner declined it
	WorkStepEscalate = "escalate" // told the owner
	WorkStepTold     = "told"     // the session was told its claim was refuted
)

// workSteps are the steps a work item event may take.
var workSteps = map[string]bool{WorkStepOpen: true, WorkStepAssign: true, WorkStepTyped: true, WorkStepAck: true, WorkStepWorking: true, WorkStepBlocked: true,
	WorkStepClaim: true, WorkStepCheck: true, WorkStepReopen: true, WorkStepDone: true, WorkStepDeclined: true, WorkStepEscalate: true, WorkStepTold: true,
	WorkStepDesign: true, WorkStepChange: true, WorkStepGo: true}

// done_when kinds.
const (
	DoneURL    = "url"    // a page whose body contains a string
	DoneForge  = "forge"  // an issue closed, a request merged, a tag made
	DoneFile   = "file"   // a file of the repository's clone contains a string
	DoneStatus = "status" // the assigned session's STATUS.md contains a string
	DoneOwner  = "owner"  // only the owner can say
)

// Bounds and defaults of the work items.
const (
	DefaultItemAckWithin     = 10 * time.Minute
	DefaultItemEscalateAfter = 2 * time.Hour
	itemTries                = 3       // sessions an item is assigned to before the owner says
	itemNoteMax              = 600     // bytes of a STATUS.md kept as a note
	itemContainsMax          = 200     // bytes of a check's string
	itemBodyMax              = 1 << 20 // bytes of a page a url check reads
	itemCheckTimeout         = 15 * time.Second
	itemNotes                = 5 // notes kept
)

// ItemsFile is where a session reads the work items assigned to it, in
// its workdir.
const ItemsFile = "items.json"

// entryWork is the entry id prefix of a work item: w:ID.
const entryWork = "w:"

// ItemsConfig bounds how long a work item waits: on its session's
// acknowledgement, and without progress before the owner is told.
type ItemsConfig struct {
	AckWithin     Duration `json:"ack_within"`
	EscalateAfter Duration `json:"escalate_after"`
}

func (c ItemsConfig) ackWithin() time.Duration {
	if c.AckWithin.Duration > 0 {
		return c.AckWithin.Duration
	}
	return DefaultItemAckWithin
}

func (c ItemsConfig) escalateAfter() time.Duration {
	if c.EscalateAfter.Duration > 0 {
		return c.EscalateAfter.Duration
	}
	return DefaultItemEscalateAfter
}

// EndCheck is an item's done_when: the observable end state that shows
// it done.
type EndCheck struct {
	Kind     string `json:"kind"`
	URL      string `json:"url,omitempty"`
	Contains string `json:"contains,omitempty"`
	Ref      string `json:"ref,omitempty"`   // forge: #N, !N, or a tag's name
	State    string `json:"state,omitempty"` // forge: closed, merged or tagged
	Path     string `json:"path,omitempty"`  // file: relative to the clone
}

var (
	issueRefPattern = regexp.MustCompile(`^#[0-9]+$`)
	mrRefPattern    = regexp.MustCompile(`^![0-9]+$`)
	tagPattern      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/+-]{0,99}$`)
)

// check says what d lacks to be checked.
func (d EndCheck) check() error {
	contains := func() error {
		if strings.TrimSpace(d.Contains) == "" || len(d.Contains) > itemContainsMax {
			return fmt.Errorf("done_when %s names the string to find, of at most %d bytes", d.Kind, itemContainsMax)
		}
		return nil
	}
	switch d.Kind {
	case DoneURL:
		u, err := url.Parse(d.URL)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			return errors.New("done_when url names an http or https URL")
		}
		return contains()
	case DoneForge:
		switch {
		case d.State == "closed" && issueRefPattern.MatchString(d.Ref),
			d.State == "merged" && mrRefPattern.MatchString(d.Ref),
			d.State == "tagged" && tagPattern.MatchString(d.Ref) && !strings.Contains(d.Ref, ".."):
			return nil
		}
		return errors.New("done_when forge is an issue #N closed, a request !N merged, or a tag tagged")
	case DoneFile:
		p := filepath.Clean(d.Path)
		if d.Path == "" || filepath.IsAbs(p) || p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
			return errors.New("done_when file names a path inside the repository's clone")
		}
		return contains()
	case DoneStatus:
		return contains()
	case DoneOwner:
		return nil
	}
	return fmt.Errorf("done_when kind %q is not url, forge, file, status or owner", d.Kind)
}

// checkable says whether Go can check d itself.
func (d EndCheck) checkable() bool { return d.Kind != DoneOwner && d.check() == nil }

// String is d as the owner reads it.
func (d EndCheck) String() string {
	switch d.Kind {
	case DoneURL:
		return fmt.Sprintf("%s contains %q", d.URL, d.Contains)
	case DoneForge:
		if d.State == "tagged" {
			return "tag " + d.Ref + " exists"
		}
		return d.Ref + " is " + d.State
	case DoneFile:
		return fmt.Sprintf("%s contains %q", d.Path, d.Contains)
	case DoneStatus:
		return fmt.Sprintf("the session's STATUS.md contains %q", d.Contains)
	}
	return "the owner says so"
}

// WorkCheck is one check of an item's done_when: whether it held, what
// was seen, and whether a claim of done asked for it.
type WorkCheck struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Claim  bool   `json:"claim,omitempty"`
}

// evidence is the check as an item's evidence: what was seen, timed.
func (c WorkCheck) evidence(at time.Time) string {
	return c.Detail + " (checked " + at.UTC().Format("2006-01-02 15:04:05Z") + ")"
}

// WorkPayload is a step of a work item. Open carries the item; the other
// steps what changed: the session, a note (a question, a claim, the
// STATUS.md a session left, why the owner was told), a check, and the
// owner's instruction that made a step.
type WorkPayload struct {
	ID          string      `json:"id"`
	Step        string      `json:"step"`
	Repository  string      `json:"repository,omitempty"`
	Instruction string      `json:"instruction,omitempty"`
	Work        *ThreadWork `json:"work,omitempty"`
	HandTo      string      `json:"hand_to,omitempty"`
	Session     string      `json:"session,omitempty"`
	Started     bool        `json:"started,omitempty"`
	Note        string      `json:"note,omitempty"`
	Key         string      `json:"key,omitempty"` // an escalation's: one per key
	Check       *WorkCheck  `json:"check,omitempty"`
	By          string      `json:"by,omitempty"`
	// Designer opens an item a designer designs before any session
	// starts, and Design is a round's design (#68).
	Designer bool        `json:"designer,omitempty"`
	Design   *WorkDesign `json:"design,omitempty"`
	// Words are the owner's words at a go, whole: the answers to the
	// design's questions; DoneWhen, the owner's done_when when the go
	// said "done by me" (#73).
	Words    string    `json:"words,omitempty"`
	DoneWhen *EndCheck `json:"done_when,omitempty"`
}

// check says what a work item step lacks.
func (p *WorkPayload) check() error {
	switch {
	case p.ID == "" || !workSteps[p.Step]:
		return errors.New("a work item step names its item and a known step")
	case p.Step == WorkStepOpen && (p.Work == nil || p.Repository == "" || p.Instruction == ""):
		return errors.New("an opened work item carries its work, repository and instruction")
	case p.Step == WorkStepAssign && p.Session == "":
		return errors.New("an assignment names its session")
	case p.Step == WorkStepCheck && p.Check == nil:
		return errors.New("a check carries its result")
	case p.Step == WorkStepDesign && p.Design == nil:
		return errors.New("a design step carries its design")
	case p.DoneWhen != nil && p.DoneWhen.check() != nil:
		return fmt.Errorf("a step's done_when: %v", p.DoneWhen.check())
	}
	return nil
}

// WorkNote is a note on an item: what a session left, or why it moved.
type WorkNote struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// WorkItem is an item as the log folds it.
type WorkItem struct {
	ID          string
	Repository  string
	Instruction string
	ThreadWork
	HandTo string
	State  string
	Opened time.Time
	// Progress is its last change of state.
	Progress time.Time
	Session  string
	Started  bool
	Assigned time.Time
	Typed    time.Time
	Retyped  time.Time
	Acked    time.Time
	// Tried are the sessions it was taken off, oldest first; Reassigns
	// how many more the owner allowed.
	Tried     []string
	Reassigns int
	Notes     []WorkNote
	Question  string
	Claim     string
	Claimed   time.Time
	Check     *WorkCheck
	Checked   time.Time
	// Refuted is a claim its check refuted, RefutedAt when, and Told
	// when the session was last told so.
	Refuted   string
	RefutedAt time.Time
	Told      time.Time
	Evidence  string
	DoneAt    time.Time
	DoneBy    string
	// Escalated are the keys the owner was told of.
	Escalated map[string]bool
	// Designer marks an item designed before its work starts (#68):
	// Round is the round designed now or last, Design its brief,
	// DesignError and DesignFails the round's failures, DesignWithheld
	// that the round's brief matched a confidential pattern and
	// WithheldRounds how many rounds were, which do not count against
	// designRounds, DesignCost what every round cost, Change the owner's
	// words on the last, and
	// Approved when it went on, by whom, and ApprovedWords the owner's
	// words then, whole.
	Designer    bool
	Round       int
	Design      *WorkDesign
	DesignError string
	DesignFails int
	// DesignWithheld and WithheldRounds: see above.
	DesignWithheld bool
	WithheldRounds int
	DesignCost     *JobUsage
	Designed       time.Time
	Change         string
	Approved       time.Time
	ApprovedBy     string
	ApprovedNote   string
	ApprovedWords  string
}

// terminal says whether the item is closed.
func (it *WorkItem) terminal() bool { return it.State == WorkDone || it.State == WorkDeclined }

// workID is the item an instruction opens: its first, and its only one
// unless it asked for several.
func workID(instruction string) string {
	return "W" + shortHash(HashPane("work\n" + instruction))[:8]
}

// workIDOf is the k-th item an instruction opens, from 0.
func workIDOf(instruction string, k int) string {
	if k == 0 {
		return workID(instruction)
	}
	return "W" + shortHash(HashPane(fmt.Sprintf("work\n%s\n%d", instruction, k)))[:8]
}

// workOf are the items instruction opened, in the order its agent named
// them.
func (st *LogState) workOf(instruction string) []*WorkItem {
	var out []*WorkItem
	n := 1
	if in := st.Instructions[instruction]; in != nil && len(in.Works) > n {
		n = len(in.Works)
	}
	for k := 0; k < n; k++ {
		if it := st.Work[workIDOf(instruction, k)]; it != nil {
			out = append(out, it)
		}
	}
	return out
}

// applyWork folds a work item step.
func (st *LogState) applyWork(e Event) {
	p := e.Work
	if st.Work == nil {
		st.Work = map[string]*WorkItem{}
	}
	it := st.Work[p.ID]
	if p.Step == WorkStepOpen {
		if it == nil {
			it = &WorkItem{ID: p.ID, Repository: p.Repository, Instruction: p.Instruction, ThreadWork: *p.Work, HandTo: p.HandTo,
				State: WorkOpen, Opened: e.Time, Progress: e.Time, Escalated: map[string]bool{}}
			if p.Designer {
				it.Designer, it.State, it.Round = true, WorkDesigning, 1
			}
			st.Work[p.ID] = it
		}
		return
	}
	if it == nil {
		return
	}
	// A closed item stays closed: only the owner's word on it is logged.
	if it.terminal() && p.Step != WorkStepEscalate {
		return
	}
	moved := func(state string) { it.State, it.Progress = state, e.Time }
	note := func() {
		if p.Note != "" {
			it.Notes = append(it.Notes, WorkNote{At: e.Time, Text: p.Note})
			if n := len(it.Notes) - itemNotes; n > 0 {
				it.Notes = it.Notes[n:]
			}
		}
	}
	switch p.Step {
	case WorkStepAssign:
		moved(WorkAssigned)
		it.Session, it.Started, it.Assigned = p.Session, p.Started, e.Time
		it.Typed, it.Retyped, it.Acked, it.Question, it.Claim, it.Claimed, it.Refuted = time.Time{}, time.Time{}, time.Time{}, "", "", time.Time{}, ""
	case WorkStepTyped:
		if it.Typed.IsZero() && !it.Started {
			it.Typed = e.Time
		} else {
			it.Retyped = e.Time
		}
	case WorkStepAck:
		moved(WorkAcknowledged)
		it.Acked = e.Time
	case WorkStepWorking:
		moved(WorkWorking)
		it.Question = ""
		if p.By == "owner" {
			// The owner said it is not done: the claim is spent.
			it.Refuted, it.Claim = it.Claim, ""
			note()
		}
	case WorkStepBlocked:
		moved(WorkBlocked)
		it.Question = p.Note
	case WorkStepClaim:
		it.Claim, it.Claimed = p.Note, e.Time
	case WorkStepCheck:
		c := *p.Check
		it.Check, it.Checked = &c, e.Time
		switch {
		case c.OK:
			moved(WorkDone)
			it.Evidence, it.DoneAt, it.DoneBy = c.evidence(e.Time), e.Time, "agentd"
		case c.Claim && it.Claim != "":
			it.Refuted, it.RefutedAt = it.Claim, e.Time
		}
	case WorkStepTold:
		it.Told = e.Time
	case WorkStepReopen:
		moved(WorkOpen)
		note()
		if it.Session != "" {
			it.Tried = append(it.Tried, it.Session)
		}
		if p.By == "owner" {
			it.Reassigns++
		}
		it.Session, it.Started, it.Question, it.Claim, it.Refuted = "", false, "", "", ""
	case WorkStepDone:
		moved(WorkDone)
		it.Evidence, it.DoneAt, it.DoneBy = p.Note, e.Time, p.By
	case WorkStepDeclined:
		moved(WorkDeclined)
		it.DoneAt, it.DoneBy = e.Time, p.By
	case WorkStepEscalate:
		it.Escalated[p.Key] = true
	case WorkStepDesign, WorkStepChange, WorkStepGo:
		it.applyDesign(p, e.Time)
	}
}

// itemsOf are the items of session name not closed, oldest first.
func (st *LogState) itemsOf(name string) []*WorkItem {
	var out []*WorkItem
	for _, id := range st.sortedWork() {
		if it := st.Work[id]; it.Session == name && !it.terminal() {
			out = append(out, it)
		}
	}
	return out
}

// sortedWork are the items' ids, oldest first.
func (st *LogState) sortedWork() []string {
	ids := sortedKeys(st.Work)
	sort.SliceStable(ids, func(i, j int) bool { return st.Work[ids[i]].Opened.Before(st.Work[ids[j]].Opened) })
	return ids
}

// answerDoneWhen makes the owner the done_when of w, an item of repo
// opened from the owner's answer to a session's decision, when it waits
// on a request merged that the loop cannot merge: nothing would merge it
// but the owner (#74).
func (s *Supervisor) answerDoneWhen(in *Instruction, repo string, w *ThreadWork) {
	d := w.DoneWhen
	if in.Re == nil || !strings.HasPrefix(in.Re.ID, entryDecision) || d.Kind != DoneForge || d.State != "merged" || s.loopCanMerge(repo, d.Ref) {
		return
	}
	w.DoneWhen = EndCheck{Kind: DoneOwner}
}

// loopCanMerge says whether agentd's loop merges request ref of repo: the
// repository's policy merges, the request is in its loop, and one no
// session delivered is by an author merge.authors lists.
func (s *Supervisor) loopCanMerge(repo, ref string) bool {
	if !s.cfg.Repositories[repo].Policy.Merge.Enabled {
		return false
	}
	for _, q := range openLoopRequests(s.cfg, s.st, s.clock.Now()) {
		if q.Repository != repo || q.Ref != ref {
			continue
		}
		r := s.st.CI[ciKey(repo, ref)]
		return q.Out == "" && (!q.Outside || contains(effectiveFor(s.cfg, s.st, repo, r.Target).Merge.Authors, r.Author))
	}
	return false
}

// workEmit logs a step of an item.
func (s *Supervisor) workEmit(p WorkPayload) {
	p.Note = s.withheld(p.Note)
	s.emit("", Event{Kind: KindWorkItem, Work: &p})
}

// driveItems is the pass over the work items: each instruction whose
// thread agent asked for work opens one, and each open one is driven a
// step. Shadow mode drives none.
func (s *Supervisor) driveItems(now time.Time) {
	if s.shadow {
		return
	}
	for _, id := range sortedKeys(s.st.Instructions) {
		in := s.st.Instructions[id]
		// Once its route settles: the session the agent handed it to, the
		// one Go started for it (#65), or none, when that failed.
		if len(in.Works) == 0 || len(in.Repositories) == 0 || in.Session == "" && in.Failed == "" {
			continue
		}
		// The owner dropped it on its next step: no work is opened for it.
		if r := s.st.nextAnswer(in); r != nil && r.Answer.Choice == nextDrop {
			continue
		}
		// Each item the agent named; the message, and the session it
		// went to, are the first's.
		for k, w := range in.Works {
			id, handTo := workIDOf(in.ID, k), ""
			if s.st.Work[id] != nil {
				continue
			}
			if k == 0 {
				handTo = in.Session
				scopedIssue(in, &w)
			}
			if s.issueForWork(id, in.Repositories[0], &w); s.halt != nil {
				return
			}
			s.answerDoneWhen(in, in.Repositories[0], &w)
			if s.workEmit(WorkPayload{ID: id, Step: WorkStepOpen, Repository: in.Repositories[0], Instruction: in.ID, Work: &w, HandTo: handTo}); s.halt != nil {
				return
			}
		}
	}
	for _, id := range s.st.sortedWork() {
		it := s.st.Work[id]
		if it.terminal() {
			continue
		}
		// Once, even after a stop between the item's opening and this.
		if in := s.st.Instructions[it.Instruction]; in != nil {
			if s.toldHandTo(in, it); s.halt != nil {
				return
			}
		}
		if s.driveItem(it, now); s.halt != nil {
			return
		}
	}
}

// driveItem takes one open item a step.
func (s *Supervisor) driveItem(it *WorkItem, now time.Time) {
	if it.State == WorkDesigning || it.State == WorkDesigned {
		s.driveDesign(it, now)
		return
	}
	if it.State == WorkOpen {
		s.assignItem(it, now)
		return
	}
	s.checkItem(it, now, false)
	if s.halt != nil || it.terminal() {
		return
	}
	t := s.st.Tracks[it.Session]
	_, running := s.cfg.Sessions[it.Session]
	sess := s.cfg.Sessions[it.Session]
	status, _ := readRegular(filepath.Join(sess.Workdir, "STATUS.md"), statusLimit)
	why := ""
	if running {
		why = s.untypeable(it.Session)
	}
	switch {
	case !running:
		// Gone: it is not doing the work.
		s.reopenItem(it, fmt.Sprintf("%s stopped with %s not done", it.Session, it.ID), string(status))
		return
	case t != nil && (t.State == Stopped || t.State == CrashLooping || t.Parked):
		// Stopped or parked: it is not doing the work.
		s.reopenItem(it, fmt.Sprintf("%s cannot be typed into: %s, and agentd does not restart it; %s is not done", it.Session, why, it.ID), string(status))
		return
	case t != nil && t.State == Done && s.reopening(it):
		return // #65 reopens it with the owner's message first
	case t != nil && t.State == Done:
		s.finishedItem(it, string(status), now)
		return
	case why != "" && s.noRestart(it.Session) != "":
		// Its tmux session is gone and nothing brings it back (#70); one
		// the restart policy brings back keeps the item.
		s.reopenItem(it, fmt.Sprintf("%s cannot be typed into: %s, and agentd does not restart %s; %s is not done", it.Session, why, s.noRestart(it.Session), it.ID), string(status))
		if s.halt == nil && it.State == WorkOpen {
			s.assignItem(it, now) // it waits on no tick
		}
		return
	}
	marks := itemMarks(it.ID, string(status))
	switch {
	case it.State == WorkAssigned && (marks.named || s.onScreen(it)):
		s.workEmit(WorkPayload{ID: it.ID, Step: WorkStepAck, Session: it.Session})
	case it.State == WorkAssigned:
		s.unacknowledged(it, now)
	case marks.blocked != "" && (it.State != WorkBlocked || it.Question != marks.blocked):
		if s.workEmit(WorkPayload{ID: it.ID, Step: WorkStepBlocked, Session: it.Session, Note: marks.blocked}); s.halt != nil {
			return
		}
		s.escalateItem(it, "blocked:"+shortHash(HashPane(marks.blocked))[:8],
			fmt.Sprintf("%s is blocked on you: %s asks: %s Respond on the item's entry; your words go to the session.", it.ID, it.Session, marks.blocked))
	case it.State == WorkBlocked && marks.blocked == "",
		it.State == WorkAcknowledged && marks.named:
		s.workEmit(WorkPayload{ID: it.ID, Step: WorkStepWorking, Session: it.Session})
	}
	if s.halt != nil {
		return
	}
	if marks.done != "" && marks.done != it.Claim && marks.done != it.Refuted {
		if s.workEmit(WorkPayload{ID: it.ID, Step: WorkStepClaim, Session: it.Session, Note: marks.done}); s.halt != nil {
			return
		}
		if it.DoneWhen.Kind == DoneOwner {
			s.escalateItem(it, fmt.Sprintf("claim:%s:%d", shortHash(HashPane(marks.done))[:8], it.Claimed.UnixNano()),
				fmt.Sprintf("%s says %s is done: %s Only you can say: answer done or not done on the item's entry.", it.Session, it.ID, marks.done))
		} else {
			s.checkItem(it, now, true)
		}
	}
	if s.halt == nil {
		s.refutedItem(it)
	}
	if s.halt == nil {
		s.stalledItem(it, sess.Workdir, now)
	}
}

// reopening says whether the item's message waits on its finished
// session, which #65 reopens to take it.
func (s *Supervisor) reopening(it *WorkItem) bool {
	in := s.st.Instructions[it.Instruction]
	return in != nil && in.Session == it.Session && s.reopens(in)
}

// itemMarksOf is what a session's STATUS.md says of an item: that it
// names it, and its "ID blocked: QUESTION" or "ID done: EVIDENCE" line,
// the later of the two alone: a done line after a question answers it.
type itemMarksOf struct {
	named         bool
	blocked, done string
}

func itemMarks(id, status string) itemMarksOf {
	var m itemMarksOf
	bi, di := -1, -1
	for n, line := range strings.Split(status, "\n") {
		i := strings.Index(line, id)
		if i < 0 {
			continue
		}
		m.named = true
		rest := strings.TrimLeft(line[i+len(id):], " *`")
		for _, w := range []string{"blocked:", "done:"} {
			if len(rest) < len(w) || !strings.EqualFold(rest[:len(w)], w) {
				continue
			}
			text := oneLine(strings.TrimSpace(rest[len(w):]), 1000)
			if text == "" {
				continue
			}
			if w == "blocked:" {
				m.blocked, bi = text, n
			} else {
				m.done, di = text, n
			}
		}
	}
	switch {
	case bi >= 0 && di > bi:
		m.blocked = ""
	case di >= 0 && bi > di:
		m.done = ""
	}
	return m
}

// onScreen says whether the item's session shows the item's id on a line
// agentd did not type: its own lines acknowledge nothing.
func (s *Supervisor) onScreen(it *WorkItem) bool {
	pane, err := s.tmux.CapturePane(it.Session, PaneLines)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(pane, "\n") {
		if strings.Contains(line, it.ID) && !strings.Contains(line, itemPointerHead) && !strings.Contains(line, itemRefutedHead) {
			return true
		}
	}
	return false
}

// unacknowledged types an item its session has not named within
// items.ack_within once more, then takes the session off it. A session
// started for the item read it in its brief.
func (s *Supervisor) unacknowledged(it *WorkItem, now time.Time) {
	within := s.cfg.Items.ackWithin()
	first := it.Typed
	if it.Started && first.IsZero() {
		first = it.Assigned
	}
	switch {
	case first.IsZero():
		// Not typed yet: the session has not taken a line.
	case it.Retyped.IsZero():
		if now.Sub(first) >= within {
			s.dueRetype[it.ID] = true // deliverItems types it again
		}
	case now.Sub(it.Retyped) >= within:
		s.reopenItem(it, fmt.Sprintf("%s did not name %s within %s of its typing, nor of its typing again", it.Session, it.ID, within), "")
	}
}

// finishedItem is an item whose session wrote DONE: a claim, which its
// check settles, or the owner when only the owner can say. A done line
// its check refutes is the owner's to judge, and is checked each
// poll_interval meanwhile (#73): a new session would write the same line.
// A bare DONE refuted gives the item back.
func (s *Supervisor) finishedItem(it *WorkItem, status string, now time.Time) {
	line := itemMarks(it.ID, status).done
	switch {
	case it.Claimed.Before(it.Assigned) || it.Claim == "" || line != "" && line != it.Claim && line != it.Refuted:
		// No claim yet, or a new done line written with DONE.
		claim := line
		if claim == "" {
			// A DONE that names not this item is this assignment's only
			// when written after it, and naming no other item (#68).
			if why := s.foreignDone(it, status); why != "" {
				s.reopenItem(it, it.Session+" wrote DONE, not for "+it.ID+": "+why, status)
				return
			}
			claim = it.Session + " wrote DONE"
		}
		if s.workEmit(WorkPayload{ID: it.ID, Step: WorkStepClaim, Session: it.Session, Note: claim}); s.halt != nil {
			return
		}
		if it.DoneWhen.Kind == DoneOwner {
			s.escalateItem(it, fmt.Sprintf("claim:%s:%d", shortHash(HashPane(claim))[:8], it.Claimed.UnixNano()),
				fmt.Sprintf("%s finished and says %s is done: %s Only you can say: answer done or not done on the item's entry.", it.Session, it.ID, claim))
			return
		}
		s.checkItem(it, now, true)
	case it.DoneWhen.Kind == DoneOwner:
		// The owner's answer settles it.
	case it.Refuted != "" || it.Check != nil && !it.Check.OK && it.Checked.After(it.Claimed):
		detail := ""
		if it.Check != nil {
			detail = it.Check.Detail
		}
		if line != "" && it.Claim != "" {
			if s.escalateItem(it, fmt.Sprintf("claim-refuted:%s:%d", shortHash(HashPane(it.Claim))[:8], it.Claimed.UnixNano()),
				fmt.Sprintf("%s finished and says %s is done: %s; the check says: %s. Answer done, not-done or reassign on the item's entry; agentd checks again each poll meanwhile.",
					it.Session, it.ID, oneLine(it.Claim, 300), detail)); s.halt == nil {
				s.checkItem(it, now, false)
			}
			return
		}
		if detail != "" {
			detail = " Its check: " + detail + "."
		}
		s.reopenItem(it, fmt.Sprintf("%s wrote DONE with %s not done.%s", it.Session, it.ID, detail), status)
	default:
		s.checkItem(it, now, true)
	}
}

// doneLine is a line of STATUS.md that says an item is done.
var doneLine = regexp.MustCompile(`(?i)\b(W[0-9a-f]{8})[ *` + "`" + `]*done:`)

// foreignDone is why the DONE of an item's session, in status, is not a
// claim of the item: written before the item was assigned, or with done
// lines that name other items only; "" when it may be its claim.
func (s *Supervisor) foreignDone(it *WorkItem, status string) string {
	if at := readSessionFiles(s.cfg.Sessions[it.Session].Workdir).StatusAt; !at.IsZero() && at.Before(it.Assigned.Add(-doneSkew)) {
		return "DONE predates the assignment"
	}
	// An item closed before is history; one open, or unknown, is not.
	var others []string
	for _, m := range doneLine.FindAllStringSubmatch(status, -1) {
		id := "W" + strings.ToLower(m[1][1:])
		if o := s.st.Work[id]; id != it.ID && !contains(others, id) && (o == nil || !o.terminal()) {
			others = append(others, id)
		}
	}
	switch {
	case len(others) > 0:
		return "DONE names " + strings.Join(others, ", ") + ", not this item"
	case it.DoneWhen.Kind == DoneOwner && !it.Started && it.Acked.IsZero() && !itemMarks(it.ID, status).named:
		// Only the owner could settle it: no check tells a bare DONE of
		// other work from the item's.
		return "DONE written without ever naming the item"
	}
	return ""
}

// doneSkew is how far a STATUS.md's time may lag the clock of the
// assignment and still be after it.
const doneSkew = 5 * time.Second

// reopenItem gives an item back to be assigned again, with why and the
// last STATUS.md its session left.
func (s *Supervisor) reopenItem(it *WorkItem, why, status string) {
	note := why
	if status = strings.TrimSpace(status); status != "" {
		if len(status) > itemNoteMax {
			status = "…" + status[len(status)-itemNoteMax:]
			status = strings.ToValidUTF8(status, "")
		}
		note += "\nSTATUS.md:\n" + status
	}
	session := it.Session
	delete(s.dueRetype, it.ID)
	if s.workEmit(WorkPayload{ID: it.ID, Step: WorkStepReopen, Session: session, Note: note}); s.halt != nil {
		return
	}
	s.letGo(it, session)
}

// letGo takes session off an item it no longer holds: a session started
// for the item or its message alone is ended, so it holds no place and does the work no
// more; any other has its items.json written without it.
func (s *Supervisor) letGo(it *WorkItem, session string) {
	if session == "" {
		return
	}
	if p, ok := s.st.Runtime[session]; ok && (p.Item == it.ID || p.Owner == it.Instruction) {
		s.emit(session, Event{Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: newIntentID(), Action: SessionEnd, Repository: p.Repository, By: "agentd"}})
		s.refreshRuntime()
		return
	}
	if sess, ok := s.cfg.Sessions[session]; ok && sess.Workdir != "" {
		if _, err := os.Stat(filepath.Join(sess.Workdir, ItemsFile)); err == nil {
			if err := writeItems(sess.Workdir, s.itemDocs(session, sess.Workdir)); err != nil {
				s.emit(session, Event{Kind: KindError, Error: &ErrorPayload{Step: "work item " + it.ID + ": items.json", Error: err.Error()}})
			}
		}
	}
}

// assignItem assigns an open item: to the session the agent handed it
// to, when it runs here and was not taken off it, else to a work session
// started for it. At the tries' end, or when none can be started, the
// owner is told, and it stays open.
func (s *Supervisor) assignItem(it *WorkItem, now time.Time) {
	tried := map[string]bool{}
	for _, n := range it.Tried {
		tried[n] = true
	}
	if len(it.Tried) >= itemTries+it.Reassigns {
		s.escalateItem(it, fmt.Sprintf("tries:%d", len(it.Tried)), fmt.Sprintf("%s is still not done after %d sessions (%s). Re-assign it to try again, mark it done, or decline it.%s",
			it.ID, len(it.Tried), strings.Join(it.Tried, ", "), lastNote(it)))
		return
	}
	// The session its message went to: the one the agent handed it to,
	// or the one the owner's next step sent it to; else a running work
	// session of the repository on its issue. One that finished or
	// stopped takes no item (#68).
	cands := []string{it.HandTo}
	if in := s.st.Instructions[it.Instruction]; in != nil && in.Failed == "" {
		cands = append(cands, in.Session)
	}
	if it.Issue > 0 {
		for _, name := range sortedKeys(s.st.Runtime) {
			if p := s.st.Runtime[name]; p.Kind == KindWork && p.Repository == it.Repository && p.Issue == it.Issue {
				cands = append(cands, name)
			}
		}
	}
	for _, h := range cands {
		if h != "" && !tried[h] && s.takesItem(h) && !s.holdsSibling(h, it) {
			if s.workEmit(WorkPayload{ID: it.ID, Step: WorkStepAssign, Session: h}); s.halt == nil {
				s.followItem(it, h, false)
			}
			return
		}
	}
	repo := it.Repository
	switch sp := s.cfg.Repositories[repo].Sessions; {
	case sp == nil:
		s.escalateItem(it, "start:none", fmt.Sprintf("agentd cannot start a session for %s: %s has no sessions entry here, so agentd starts none for it. Configure one, or re-assign it.", it.ID, repo))
		return
	case sp.Host != "":
		s.escalateItem(it, "start:host", fmt.Sprintf("agentd cannot start a session for %s: %s places its sessions on %s, whose node cannot brief one with the owner's words yet. Re-assign it once a session of its runs here.", it.ID, repo, sp.Host))
		return
	}
	branch := fmt.Sprintf("owner/%s-%s-%d", itemSlug(it.Title), strings.ToLower(it.ID), len(it.Tried)+1)
	name := runtimeName(repo, branch, KindWork)
	why := s.startRuntime(repo, name, RuntimePayload{Kind: KindWork, Branch: branch, Issue: it.Issue, Owner: it.Instruction, Item: it.ID})
	if s.halt != nil {
		return
	}
	if why == "" {
		s.refreshRuntime()
		if s.workEmit(WorkPayload{ID: it.ID, Step: WorkStepAssign, Session: name, Started: true}); s.halt != nil {
			return
		}
		s.followItem(it, name, true)
		return
	}
	holders := sessionsOf(s.st.Runtime, repo)
	s.escalateItem(it, "start:"+shortHash(HashPane(digits.ReplaceAllString(why, "N")))[:8], fmt.Sprintf("agentd cannot start a session for %s: %s. Holding places: %s. End one, and agentd starts it.",
		it.ID, why, strings.Join(holders, ", ")))
}

// holdsSibling says whether session name holds, or was started for,
// another item of it's message: each item of a message that asked for
// several takes a session of its own (#68).
func (s *Supervisor) holdsSibling(name string, it *WorkItem) bool {
	if p, ok := s.st.Runtime[name]; ok && p.Item != "" && p.Item != it.ID && s.st.Work[p.Item] != nil && s.st.Work[p.Item].Instruction == it.Instruction {
		return true
	}
	for _, o := range s.st.itemsOf(name) {
		if o.ID != it.ID && o.Instruction == it.Instruction {
			return true
		}
	}
	return false
}

// toldHandTo tells the owner once, on the repository's console, that
// the session in's agent handed it to is not running, and that Go opened
// item it from the message instead (#68).
func (s *Supervisor) toldHandTo(in *Instruction, it *WorkItem) {
	a := in.AgentLast
	if a == nil || a.HandToDropped == "" || it == nil || it.ID != workID(in.ID) {
		return
	}
	did := "starts a session for it as soon as one can start"
	switch {
	case it.Designer:
		did = "has it designed first: the design comes to you on its entry"
	case in.Failed != "":
		did = "could not start a session for it yet: " + oneLine(in.Failed, 300)
	case in.Session != "":
		did = "started " + in.Session + " for it"
	}
	which := "which is not running"
	if s.doneAlive(a.HandToDropped) {
		which = "which has finished and takes no new work"
	}
	s.escalateItem(it, "handto", fmt.Sprintf("The thread agent named %s, %s; agentd opened %s from your message and %s.", a.HandToDropped, which, it.ID, did))
}

// followItem sends the item's message, not yet delivered, to the
// session given the item: a designed item's to the session started for
// it, whose reply answers it, and one the agent handed to a session that
// takes no item away from it (#68).
func (s *Supervisor) followItem(it *WorkItem, name string, started bool) {
	in := s.st.Instructions[it.Instruction]
	switch {
	case in == nil || in.Session == name || in.Failed != "":
		return
	case in.Session != "" && len(in.Works) > 1:
		// One of several items took the message already.
		return
	case in.Session == "":
		// The designer took it: the session started answers it.
		if !it.Designer || !in.Replied.IsZero() {
			return
		}
	case !in.Delivered.IsZero():
		return
	}
	reason := "assigned " + it.ID
	if started {
		reason = "started " + name
	}
	s.routeStep(in, RouteTo, name, started, reason)
}

// digits are the counts a reason names, which change while it holds.
var digits = regexp.MustCompile(`[0-9]+`)

// itemSlug is a title as a branch's word.
func itemSlug(title string) string {
	s := strings.Trim(nonName.ReplaceAllString(strings.ToLower(title), "-"), "-_")
	if len(s) > 30 {
		s = strings.TrimRight(s[:30], "-_")
	}
	if s == "" {
		s = "work"
	}
	return s
}

// lastNote is an item's last note, as a sentence for the owner.
func lastNote(it *WorkItem) string {
	if len(it.Notes) == 0 {
		return ""
	}
	return " Last: " + oneLine(it.Notes[len(it.Notes)-1].Text, 300)
}

// takesItem says whether session name may be given an item: it runs
// here, takes instructions, Go can type into it now, and its STATUS.md
// does not end with DONE.
func (s *Supervisor) takesItem(name string) bool {
	return s.takesInstructions(name) && s.reachable(name) && s.untypeable(name) == "" && !readSessionFiles(s.cfg.Sessions[name].Workdir).Done
}

// untypeable is why Go cannot type into session name now, or "" when it
// can: its tmux session must run with a live pane, and it must not be
// parked, stopped, crash-looping, or blocked on the owner.
func (s *Supervisor) untypeable(name string) string {
	t := s.st.Tracks[name]
	switch {
	case t != nil && t.Parked:
		return "it is parked"
	case t != nil && t.State == Stopped:
		return "it is stopped: " + t.Stopped
	case t != nil && t.State == CrashLooping:
		return "it is crash-looping"
	}
	alive, err := s.tmux.HasSession(name)
	if err == nil && alive && (t == nil || t.LastObs == nil || t.LastObs.Alive) {
		return ""
	}
	if _, started := s.st.Runtime[name]; started && (t == nil || t.State == "") {
		return "" // agentd started it, and its pass launches it
	}
	if t != nil && t.State == BlockedOnOwner {
		return "it is blocked on the owner and its tmux session is gone"
	}
	return "its tmux session is gone"
}

// noRestart is why agentd's restart policy does not bring session name
// back once its tmux session is gone, or "" when it does.
func (s *Supervisor) noRestart(name string) string {
	switch t := s.st.Tracks[name]; {
	case !s.cfg.Policies.Restart.Enabled:
		return "it: the restart policy is off"
	case t != nil && t.State == BlockedOnOwner:
		return "a session blocked on the owner"
	}
	return ""
}

// reachable says whether Go can type into session name: one it
// supervises here. A session on another machine, or one it does not run,
// would receive nothing.
func (s *Supervisor) reachable(name string) bool {
	_, ok := s.cfg.Sessions[name]
	return ok
}

// escalateItem tells the owner of an item once per key: a line on the
// repository's console, and the item's entry shows it.
func (s *Supervisor) escalateItem(it *WorkItem, key, text string) {
	if it.Escalated[key] {
		return
	}
	if s.workEmit(WorkPayload{ID: it.ID, Step: WorkStepEscalate, Key: key, Note: text}); s.halt != nil {
		return
	}
	m := &MessagePayload{ID: "W" + shortHash(HashPane(it.ID + "\n" + key))[:8], Console: s.cfg.RepositoryConsole(it.Repository), Kind: "info", From: "agentd", Text: text}
	if i := s.confidentialMatch(text); i > 0 {
		m.Text, m.Withheld = "", i
	}
	s.emit("", Event{Kind: KindMessage, Mode: ModeShared, Message: m})
}

// undelivered fails the owner's words in, which could not be typed into
// session, and tells the owner on it, the item they are about, at once:
// which answer, on which entry, why, and what the owner can do.
func (s *Supervisor) undelivered(in *Instruction, it *WorkItem, session, why string) {
	reason := "could not be typed into " + session + ": " + why
	if s.routeStep(in, RouteFail, "", false, reason); s.halt != nil {
		return
	}
	s.escalateItem(it, "undelivered:"+in.ID, fmt.Sprintf("%s: your answer %s on %s %s. Reopen the session, reassign %s, or type the answer into its pane by hand.", it.ID, in.ID, itemEntry(in), reason, it.ID))
}

// losesItem is why driveItem takes its item off session name (#70): it
// cannot be typed into, and is parked, stopped or crash-looping, or its
// tmux session is gone and nothing restarts it; "" when it keeps it.
func (s *Supervisor) losesItem(name string) string {
	why := s.untypeable(name)
	if t := s.st.Tracks[name]; why == "" || !(t != nil && (t.Parked || t.State == Stopped || t.State == CrashLooping) || s.noRestart(name) != "") {
		return ""
	}
	return why
}

// reassigning holds the owner's words in, which cannot be typed into the
// item's session, which nothing restarts, for the session the item is
// given to this pass (#70), and tells the owner on the item at once.
func (s *Supervisor) reassigning(in *Instruction, it *WorkItem, why string) {
	reason := "could not be typed into " + it.Session + ": " + why + "; it waits for the session " + it.ID + " is given to"
	if s.routeStep(in, RouteWait, "", false, reason); s.halt != nil {
		return
	}
	s.escalateItem(it, "waiting:"+in.ID, fmt.Sprintf("%s: your answer %s on %s %s. agentd reassigns %s and hands your answer to its new session; you can instead reopen %s, or type the answer into its pane by hand.", it.ID, in.ID, itemEntry(in), reason, it.ID, it.Session))
}

// stalledItem tells the owner, once a stall, of an item with no change of
// state and no change of its session's STATUS.md for items.escalate_after.
func (s *Supervisor) stalledItem(it *WorkItem, workdir string, now time.Time) {
	since := it.Progress
	if at := readSessionFiles(workdir).StatusAt; at.After(since) && it.Session != "" {
		since = at
	}
	after := s.cfg.Items.escalateAfter()
	if now.Sub(since) < after {
		return
	}
	tried := "none"
	if len(it.Tried) > 0 {
		tried = strings.Join(it.Tried, ", ")
	}
	s.escalateItem(it, "stall:"+since.UTC().Format(time.RFC3339), fmt.Sprintf("%s has made no progress for %s: it is %s with %s; sessions taken off it: %s.%s Re-assign it, mark it done, or decline it.",
		it.ID, after, it.State, it.Session, tried, lastNote(it)))
}

// refutedItem tells the owner of a claim its check refuted, and has the
// session told (deliverItems types it).
func (s *Supervisor) refutedItem(it *WorkItem) {
	if it.Refuted == "" || it.Check == nil || it.Check.OK {
		return
	}
	s.escalateItem(it, "refuted:"+shortHash(HashPane(it.Refuted))[:8], fmt.Sprintf("%s says %s is done (%s), but its check says not: %s. agentd told the session.",
		it.Session, it.ID, oneLine(it.Refuted, 300), it.Check.Detail))
}

// checkItem checks a checkable item's done_when in a worker: at once on
// a claim, else once a poll_interval. A result is logged when it differs
// from the last one logged, and always on a claim.
func (s *Supervisor) checkItem(it *WorkItem, now time.Time, claim bool) {
	d := it.DoneWhen
	if !d.checkable() || it.terminal() {
		return
	}
	key := "work-check:" + it.ID
	if s.work.busy(key) {
		return
	}
	// A claim not yet checked makes any check its own.
	claim = claim || it.Claim != "" && it.Claim != it.Refuted && !it.Checked.After(it.Claimed)
	if last, ok := s.checkedAt[it.ID]; !claim && ok && now.Sub(last) < s.cfg.PollInterval.Duration {
		return
	}
	s.checkedAt[it.ID] = now
	repo := s.cfg.Repositories[it.Repository]
	// A file is read where the work is done: the session's worktree, else
	// the clone.
	clone := s.threadCwd(it.Repository)
	if cwd := s.cfg.Sessions[it.Session].Cwd; cwd != "" {
		clone = cwd
	}
	workdir := s.cfg.Sessions[it.Session].Workdir
	last := it.Check
	id := it.ID
	s.spawnShared(key, func(w *Supervisor) {
		ok, detail := w.runCheck(d, repo, clone, workdir)
		c := WorkCheck{OK: ok, Detail: w.withheld(cutUTF8(detail, 500)), Claim: claim}
		if !claim && last != nil && last.OK == c.OK && last.Detail == c.Detail {
			return
		}
		w.workEmit(WorkPayload{ID: id, Step: WorkStepCheck, Check: &c})
	})
}

// runCheck checks d: whether it holds, and what was seen, timed.
func (s *Supervisor) runCheck(d EndCheck, repo Repository, clone, workdir string) (bool, string) {
	ok, seen := false, ""
	switch d.Kind {
	case DoneURL:
		ok, seen = checkURL(d.URL, d.Contains)
	case DoneForge:
		ok, seen = s.checkForge(d, repo)
	case DoneFile:
		if clone == "" {
			seen = "no worktree or clone here to read " + d.Path + " in"
			break
		}
		ok, seen = checkFile(filepath.Join(clone, filepath.Clean(d.Path)), d.Path, d.Contains)
	case DoneStatus:
		ok, seen = checkFile(filepath.Join(workdir, "STATUS.md"), "STATUS.md", d.Contains)
	}
	return ok, fmt.Sprintf("%s: %s", d, seen)
}

// itemHTTP is the client a url check uses: it follows redirects on the
// URL's own host only, and dials no loopback, link-local or unspecified
// address, so an agent's URL reads no service of this machine's or the
// cloud's metadata.
var itemHTTP = &http.Client{Timeout: itemCheckTimeout,
	CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 || r.URL.Host != via[0].URL.Host {
			return errors.New("a redirect off the URL's host")
		}
		return nil
	},
	Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: itemCheckTimeout, Control: dialPublic}).DialContext}}

// itemLoopback lets a url check dial loopback, for tests alone.
var itemLoopback = false

// dialPublic refuses a loopback, link-local or unspecified address.
func dialPublic(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsLoopback() && !itemLoopback {
		return fmt.Errorf("a url check dials no address %s", host)
	}
	return nil
}

// checkURL reads u and says whether its body contains want.
func checkURL(u, want string) (bool, string) {
	resp, err := itemHTTP.Get(u)
	if err != nil {
		return false, "the page could not be read: " + oneLine(err.Error(), 200)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, itemBodyMax))
	if err != nil {
		return false, "the page could not be read: " + oneLine(err.Error(), 200)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("status %d", resp.StatusCode)
	}
	if strings.Contains(string(b), want) {
		return true, fmt.Sprintf("status 200, the body contains %q", want)
	}
	return false, fmt.Sprintf("status 200, the body does not contain %q", want)
}

// checkFile says whether the regular file at path, named name, contains
// want.
func checkFile(path, name, want string) (bool, string) {
	b, err := readRegular(path, statusLimit)
	if err != nil {
		return false, name + " could not be read"
	}
	if strings.Contains(string(b), want) {
		return true, fmt.Sprintf("%s contains %q", name, want)
	}
	return false, fmt.Sprintf("%s does not contain %q", name, want)
}

// checkForge asks repo's forge for an issue's, a request's or a tag's
// state.
func (s *Supervisor) checkForge(d EndCheck, r Repository) (bool, string) {
	if s.api == nil || r.Project == "" {
		return false, "no forge to ask"
	}
	api := s.api.At(r.Host)
	n := strings.TrimLeft(d.Ref, "#!")
	gitlab := r.Forge == "gitlab"
	var path string
	switch {
	case d.State == "tagged" && gitlab:
		path = fmt.Sprintf("/projects/%s/repository/tags/%s", url.PathEscape(r.Project), url.PathEscape(d.Ref))
	case d.State == "tagged":
		path = fmt.Sprintf("/repos/%s/git/ref/tags/%s", r.Project, d.Ref)
	case d.State == "merged" && gitlab:
		path = fmt.Sprintf("/projects/%s/merge_requests/%s", url.PathEscape(r.Project), n)
	case d.State == "merged":
		path = fmt.Sprintf("/repos/%s/pulls/%s", r.Project, n)
	case gitlab:
		path = fmt.Sprintf("/projects/%s/issues/%s", url.PathEscape(r.Project), n)
	default:
		path = fmt.Sprintf("/repos/%s/issues/%s", r.Project, n)
	}
	var resp APIResponse
	var err error
	if gitlab {
		resp, err = api.GitLab(path)
	} else {
		resp, err = api.GitHub(path, nil)
	}
	if err != nil {
		return false, "the forge could not be asked: " + oneLine(err.Error(), 200)
	}
	if d.State == "tagged" {
		if resp.Status == 200 {
			return true, "the tag exists"
		}
		return false, fmt.Sprintf("the forge answers %d for the tag", resp.Status)
	}
	var v struct {
		State  string `json:"state"`
		Merged bool   `json:"merged"`
	}
	if err := getJSON(resp, err, "the forge", &v); err != nil {
		return false, oneLine(err.Error(), 200)
	}
	state := v.State
	if v.Merged {
		state = "merged"
	}
	return state == d.State, "it is " + state
}

// itemDoc is an item as items.json holds it for its session.
type itemDoc struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Owner       string          `json:"owner_words,omitempty"`
	Withheld    bool            `json:"owner_words_withheld,omitempty"`
	Attachments []AttachmentRef `json:"attachments,omitempty"`
	Brief       string          `json:"brief"`
	Issue       int             `json:"issue,omitempty"`
	DoneWhen    EndCheck        `json:"done_when"`
	State       string          `json:"state"`
	Notes       []WorkNote      `json:"notes,omitempty"`
	Check       *WorkCheck      `json:"last_check,omitempty"`
	Refuted     string          `json:"refuted_claim,omitempty"`
	// Designed: Brief is the design the owner approved (#68).
	Designed bool `json:"designed,omitempty"`
}

// itemProtocol is how a session tells Go of an item, as items.json and
// the brief say it.
const itemProtocol = "Each item is the owner's work, given to you. Name its id in STATUS.md's Now when you take it. " +
	"When you need the owner, write a line \"ID blocked: QUESTION\" in STATUS.md. When it is done, write \"ID done: EVIDENCE\" there: " +
	"agentd then checks done_when itself, and only that check, or the owner, closes the item. Do not write DONE while an item of yours is not done: it is given to another session."

// itemDocs are session name's items as items.json holds them, with the
// owner's words and attachments copied into workdir.
func (s *Supervisor) itemDocs(name, workdir string) []itemDoc {
	out := []itemDoc{}
	for _, it := range s.st.itemsOf(name) {
		d := itemDoc{ID: it.ID, Title: it.Title, Brief: it.Brief, Issue: it.Issue, DoneWhen: it.DoneWhen, State: it.State, Notes: it.Notes, Check: it.Check, Refuted: it.Refuted}
		if in := s.st.Instructions[it.Instruction]; in != nil {
			d.Owner, d.Withheld = in.Text, in.Withheld > 0
			// A designed item's brief is its design (#68).
			if it.Designer && !it.Approved.IsZero() {
				words := ""
				if in.Withheld == 0 {
					words = in.Text
				}
				d.Brief, d.Designed = designMarkdown(it, words), true
			}
			if workdir != "" {
				d.Attachments, _ = copyAttachments(s.cfg, workdir, in.Attachments)
			}
		}
		out = append(out, d)
	}
	return out
}

// writeItems writes session name's items to items.json in its workdir.
func writeItems(workdir string, docs []itemDoc) error {
	b, err := json.Marshal(struct {
		Protocol string    `json:"protocol"`
		Items    []itemDoc `json:"items"`
	}{itemProtocol, docs})
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(workdir, ItemsFile), append(b, '\n'))
}

// Lines typed into a session about its items: fixed, with the item's id
// only, never the owner's words.
const (
	itemPointerHead = "agentd assigned you work item "
	itemRefutedHead = "agentd checked work item "
	itemPointer     = itemPointerHead + "%[1]s: read it in " + ItemsFile + " in your workdir. Name %[1]s in STATUS.md as you take it; write \"%[1]s done: EVIDENCE\" or \"%[1]s blocked: QUESTION\" there."
	itemRefuted     = itemRefutedHead + "%[1]s and it is not done: its check is in " + ItemsFile + ". Carry on, or write \"%[1]s blocked: QUESTION\" in STATUS.md."
)

// deliverItems types into a live session that can take a line the
// pointer of an item assigned to it and not yet typed, or due again, or
// that its claim was refuted; one line a pass. It says whether it tried.
func (s *Supervisor) deliverItems(name string, sess Session, t *Track, ev Evidence, state State) bool {
	if s.shadow || !ev.Alive || t.Limited || ev.Dialog != "" || ev.TrustPrompt || state != Working && state != Idle {
		return false
	}
	for _, it := range s.st.itemsOf(name) {
		var line string
		switch {
		case it.State == WorkAssigned && it.Typed.IsZero() && !it.Started, s.dueRetype[it.ID]:
			line = fmt.Sprintf(itemPointer, it.ID)
		case it.Refuted != "" && it.Told.Before(it.RefutedAt):
			line = fmt.Sprintf(itemRefuted, it.ID)
		default:
			continue
		}
		key := "item:" + it.ID
		if !s.retryDue(t, key, s.clock.Now()) {
			return false
		}
		docs := s.itemDocs(name, sess.Workdir)
		err := s.typeInto(name, ActItem, &ActPayload{Key: key, Message: line}, t, ev.PaneHash, func() error {
			if err := writeItems(sess.Workdir, docs); err != nil {
				return err
			}
			return s.tmux.Type(name, line)
		})
		if err == nil && s.halt == nil {
			step := WorkStepTyped
			if strings.HasPrefix(line, itemRefutedHead) {
				step = WorkStepTold
			}
			delete(s.dueRetype, it.ID)
			s.workEmit(WorkPayload{ID: it.ID, Step: step, Session: name})
		}
		return true
	}
	return false
}

// Owner's answers on an item's entry.
const (
	WorkAnswerDone     = "done"
	WorkAnswerNotDone  = "not-done"
	WorkAnswerReassign = "reassign"
	WorkAnswerDecline  = "decline"
)

// workOptions are the answers an item's entry takes now.
func (it *WorkItem) workOptions() []string {
	if it.terminal() {
		return nil
	}
	// Being designed, it takes its design's answers on its design entry;
	// asking a question, it takes the owner's words as the answer, which
	// go to its session, or decline.
	if it.State == WorkDesigning || it.State == WorkDesigned || it.State == WorkBlocked {
		return []string{WorkAnswerDecline}
	}
	opts := []string{WorkAnswerDone}
	if it.Claim != "" {
		opts = append(opts, WorkAnswerNotDone)
	}
	return append(opts, WorkAnswerReassign, WorkAnswerDecline)
}

// routeWork takes the owner's response on an item's entry, and says
// whether it was the item's: a choice is carried out by Go and answered
// with what it did; words alone go to the item's session.
func (s *Supervisor) routeWork(in *Instruction) bool {
	if in.Re == nil || !strings.HasPrefix(in.Re.ID, entryWork) || !in.Route || in.Failed != "" || !in.Replied.IsZero() || in.Session != "" || in.Agent {
		return false
	}
	it := s.st.Work[strings.TrimPrefix(in.Re.ID, entryWork)]
	answer := func(text string) {
		s.emit("", Event{Kind: KindRoute, Route: &RoutePayload{Instruction: in.ID, Step: RouteAnswered, Agent: &AgentPayload{Answer: text, By: "agentd"}}})
	}
	switch {
	case it == nil:
		s.routeStep(in, RouteFail, "", false, "the log holds no work item "+in.Re.ID)
		return true
	case in.Answer == nil || in.Answer.Choice == "":
		if in.Withheld > 0 {
			return false // routeOne says why it is never delivered
		}
		if it.Session == "" && it.State == WorkOpen && in.Waiting != "" {
			return true // it waits for the session the item is given to
		}
		if it.Session != "" && s.reachable(it.Session) {
			if why := s.losesItem(it.Session); why != "" {
				s.reassigning(in, it, why)
				return true
			}
		}
		if it.Session != "" && s.takesInstructions(it.Session) && s.reachable(it.Session) {
			s.routeStep(in, RouteTo, it.Session, false, "")
			return true
		}
		s.routeStep(in, RouteFail, "", false, it.ID+" has no session to take your words now; answer "+strings.Join(it.workOptions(), " or ")+" on it")
		return true
	case it.terminal():
		answer(it.ID + " is " + it.State + " already; nothing was changed.")
		return true
	case !contains(it.workOptions(), in.Answer.Choice) && (it.State == WorkDesigning || it.State == WorkDesigned):
		answer(it.ID + " is " + it.State + ": answer " + it.designAnswers() + " on its design entry, " + entryDesign + it.ID + "; nothing was changed.")
		return true
	}
	word := strings.TrimSpace(in.Text)
	switch in.Answer.Choice {
	case WorkAnswerDone:
		ev := "marked done by the owner"
		if word != "" {
			ev += ": " + oneLine(word, 300)
		}
		s.workEmit(WorkPayload{ID: it.ID, Step: WorkStepDone, Instruction: in.ID, Note: ev, By: "owner"})
		answer(it.ID + " is done, as you said.")
	case WorkAnswerNotDone:
		if it.Claim == "" {
			answer(it.ID + " has no claim of done waiting on you; nothing was changed.")
			return true
		}
		note := "the owner said not done"
		if word != "" {
			note += ": " + oneLine(word, 300)
		}
		if t := s.st.Tracks[it.Session]; t != nil && t.State == Done || it.Session == "" {
			s.workEmit(WorkPayload{ID: it.ID, Step: WorkStepReopen, Session: it.Session, Instruction: in.ID, Note: note})
			answer(it.ID + " is open again; agentd assigns it to a new session.")
		} else {
			s.workEmit(WorkPayload{ID: it.ID, Step: WorkStepWorking, Session: it.Session, Instruction: in.ID, Note: note, By: "owner"})
			s.dueRetype[it.ID] = true
			answer(it.ID + " is not done: " + it.Session + " keeps it.")
		}
	case WorkAnswerReassign:
		note := "the owner re-assigned it"
		if word != "" {
			note += ": " + oneLine(word, 300)
		}
		s.workEmit(WorkPayload{ID: it.ID, Step: WorkStepReopen, Session: it.Session, Instruction: in.ID, Note: note, By: "owner"})
		answer(it.ID + " is open again; agentd assigns it to a new session.")
	case WorkAnswerDecline:
		if s.workEmit(WorkPayload{ID: it.ID, Step: WorkStepDeclined, Instruction: in.ID, Note: oneLine(word, 300), By: "owner"}); s.halt != nil {
			return true
		}
		answer(it.ID + " is declined; agentd drops it.")
		s.declinedMessage(it)
	default:
		answer(in.Answer.Choice + " is not an answer a work item takes; nothing was changed.")
	}
	return true
}

// WorkView is an item as the page and agentd items show it: the owner's
// words cut, its state and age, its session, its last progress and its
// done_when with the last check.
type WorkView struct {
	ID         string    `json:"id"`
	Repository string    `json:"repository"`
	Title      string    `json:"title"`
	Words      string    `json:"words,omitempty"`
	State      string    `json:"state"`
	Opened     time.Time `json:"opened"`
	Age        string    `json:"age"`
	Session    string    `json:"session,omitempty"`
	Tried      []string  `json:"tried,omitempty"`
	Progress   string    `json:"progress,omitempty"`
	DoneWhen   string    `json:"done_when"`
	LastCheck  string    `json:"last_check,omitempty"`
	Question   string    `json:"question,omitempty"`
	Claim      string    `json:"claim,omitempty"`
	Evidence   string    `json:"evidence,omitempty"`
	Options    []string  `json:"options,omitempty"`
	// Entry and Src are what the page's form names.
	Entry string `json:"-"`
	Src   string `json:"-"`
}

// workWords bounds the owner's words an item shows.
const workWords = 200

// WorkViews are st's items on console, or of every repository for "",
// the closed ones too with all, oldest first, screened.
func WorkViews(cfg *Config, st *LogState, console string, all bool, now time.Time, secret func(...string) bool) []WorkView {
	var out []WorkView
	for _, id := range st.sortedWork() {
		it := st.Work[id]
		if console != "" && it.Repository != console && cfg.RepositoryConsole(it.Repository) != console || !all && it.terminal() {
			continue
		}
		v := WorkView{ID: it.ID, Repository: it.Repository, Title: it.Title, State: it.State, Opened: it.Opened, Age: ago(now, it.Opened), Session: it.Session,
			Tried: it.Tried, DoneWhen: it.DoneWhen.String(), Question: it.Question, Claim: it.Claim, Evidence: it.Evidence, Options: it.workOptions(), Entry: entryWork + it.ID}
		if in := st.Instructions[it.Instruction]; in != nil {
			v.Words = cutUTF8(in.Text, workWords)
		}
		if it.Check != nil {
			v.LastCheck = it.Check.Detail
		}
		switch {
		case it.State == WorkDesigning:
			v.Progress = fmt.Sprintf("the designer is writing round %d", it.Round)
		case it.State == WorkDesigned:
			v.Progress = "its design waits on your " + it.designAnswers() + ": " + entryDesign + it.ID
		case len(it.Notes) > 0 && it.Notes[len(it.Notes)-1].At.After(it.Progress.Add(-time.Second)):
			v.Progress = oneLine(it.Notes[len(it.Notes)-1].Text, 300)
		case it.Session != "":
			v.Progress = it.State + " since " + ago(now, it.Progress) + " ago"
		}
		if secret(v.Title, v.Words, v.Progress, v.DoneWhen, v.LastCheck, v.Question, v.Claim, v.Evidence) {
			v = WorkView{ID: v.ID, Repository: v.Repository, State: v.State, Opened: v.Opened, Age: v.Age, Session: v.Session, Options: v.Options, Entry: v.Entry, Title: withheldSecret}
		}
		out = append(out, v)
	}
	return append(out, releaseViews(cfg, st, console, all, now, secret)...)
}

// PrintWork writes the work items table.
func PrintWork(w io.Writer, views []WorkView) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ITEM\tSTATE\tAGE\tSESSION\tDONE WHEN\tWORDS")
	for _, v := range views {
		session := v.Session
		if session == "" {
			session = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", v.ID, v.State, v.Age, session, oneLine(v.DoneWhen, 60), oneLine(v.Words, 60))
	}
	tw.Flush()
}
