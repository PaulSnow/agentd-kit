package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The owner's reply in a console that is neither an answer nor a seen mark
// is an instruction (#33). agentd routes each one (#56): a reply in a
// session's own box goes to that session; one in a repository's box goes
// to the repository's chat session, which agentd starts when the owner
// first writes and keeps while the conversation goes on. It is handed over
// the way answers are, at once, mid-turn too: instructions.json in the
// session's workdir and one fixed pointer line, never the owner's words
// typed. What became of it is folded from the log: the route, the
// delivery, the session's next prompt, the tools it ran since, and the
// reply it ended that turn with. agentd console inbox still reads them.

// InstructionPayload is an owner instruction: its id, the repositories it
// is for, and its text, left out when it matches a confidential pattern
// (Withheld, from 1, says which). Hash is the text's, and makes the id; a
// withheld text has none.
// Console names a console without a repository (#42) the instruction came
// from: it is for no repository.
type InstructionPayload struct {
	ID           string   `json:"id"`
	Repositories []string `json:"repositories"`
	Console      string   `json:"console,omitempty"`
	Text         string   `json:"text,omitempty"`
	Withheld     int      `json:"withheld,omitempty"`
	Hash         string   `json:"hash,omitempty"`
	From         string   `json:"from"`
	// To is the session the owner addressed, if any. Route marks an
	// instruction agentd routes: one recorded before #56 has none, and
	// is shown as recorded only, so an upgrade acts on no old backlog.
	To    string `json:"to,omitempty"`
	Route bool   `json:"route,omitempty"`
	// Re is the console entry the owner responded to (#59), as Go
	// filled it in from the log, and Answer the answer the response gave
	// it, when it is a decision.
	Re     *EntryRe     `json:"re,omitempty"`
	Answer *EntryAnswer `json:"answer,omitempty"`
}

// Instruction is an instruction as the log holds it, with when it came,
// and what became of it in the run's mode (#56): the session it was routed
// to, whether agentd started that session for it, when it was delivered,
// when the session took it (its next prompt) and the reply it ended that
// turn with; or why it waits, or failed.
type Instruction struct {
	InstructionPayload
	At        time.Time
	Session   string
	Started   bool
	Delivered time.Time
	Taken     time.Time
	Reply     string
	// ReplyWithheld: the reply matched a confidential pattern.
	ReplyWithheld bool
	Replied       time.Time
	// Activity is the tools the session ran since it took the
	// instruction, oldest first, the latest activityKept.
	Activity []TurnPayload
	Waiting  string
	Failed   string
}

// Route steps.
const (
	RouteTo   = "to"   // routed to a session; Started when agentd started it for the instruction
	RouteWait = "wait" // not delivered yet, and why
	RouteFail = "fail" // never to be delivered, and why
)

func validRouteStep(s string) bool { return s == RouteTo || s == RouteWait || s == RouteFail }

// RoutePayload is a step agentd took with an owner instruction.
type RoutePayload struct {
	Instruction string `json:"instruction"`
	Step        string `json:"step"`
	Session     string `json:"session,omitempty"`
	Started     bool   `json:"started,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// InstructionsFile is where a session reads the owner's instructions
// routed to it, in its workdir.
const InstructionsFile = "instructions.json"

// KindChat is the kind of a repository's chat session: the one the
// owner talks to on the console (#56). It works on a worktree detached at
// the target's tip, delivers nothing, is never nudged and never counts
// against sessions.max, and runs on the repository's work model.
const KindChat = "chat"

// chatBranch is the branch a chat session is named by.
const chatBranch = "chat"

// ChatIdle is how long a chat session is kept with no message from the
// owner; the next message starts it again.
const ChatIdle = 2 * time.Hour

// ChatPoll is how often agentd passes while an instruction is delivered
// and not answered, so the page shows the turn as it goes.
const ChatPoll = 5 * time.Second

// activityKept bounds the tool lines an instruction keeps.
const activityKept = 40

// routeSkew is how much earlier than its delivery a session's prompt may
// be stamped and still be taken as the delivery's: the transcript's clock
// is the session's host's.
const routeSkew = 30 * time.Second

// InboxInstruction is an instruction as agentd console inbox shows it.
type InboxInstruction struct {
	ID       string    `json:"id"`
	Text     string    `json:"text,omitempty"`
	Withheld bool      `json:"withheld,omitempty"`
	From     string    `json:"from"`
	At       time.Time `json:"at"`
}

// decisionLike is a word that reads as a decision's id, with or without
// the version a fed line names.
var decisionLike = regexp.MustCompile(`^[A-Za-z]\d+(@\S*)?$`)

// RecordInstruction records text as the owner's instruction from console,
// for every repository that console feeds, screened. Each reply is
// recorded, the same words twice too: nothing carries an instruction to a
// session, so no earlier copy is waiting to be carried.
func RecordInstruction(cfg *Config, log *Log, now time.Time, console, text string) error {
	if _, ok := cfg.Consoles[console]; ok {
		return recordInstruction(cfg, log, now, nil, console, "", "console:"+console, text)
	}
	repos := cfg.consoleRepos(console)
	if len(repos) == 0 {
		return fmt.Errorf("console %q feeds no repository, so it takes no instruction", console)
	}
	return recordInstruction(cfg, log, now, repos, "", "", "console:"+console, text)
}

// UIInstruction records text as the owner's instruction from the UI for
// one repository (#41), or for a console without one (#42), as a console
// reply is recorded; to, when set, is the session the owner addressed,
// which must be one of repo's, or one the console shows.
func UIInstruction(cfg *Config, log *Log, now time.Time, repo, to, text string) error {
	if to != "" {
		sess, ok := cfg.Sessions[to]
		if !ok {
			return fmt.Errorf("no session %q", to)
		}
		if sess.Role != "" {
			return fmt.Errorf("%s is a %s and takes no messages", to, sess.Role)
		}
		if sess.Repository != repo && cfg.Console(to) != repo {
			return fmt.Errorf("session %q is not %s's", to, repo)
		}
	}
	if _, ok := cfg.Consoles[repo]; ok {
		return recordInstruction(cfg, log, now, nil, repo, to, "ui", text)
	}
	if _, ok := cfg.Repositories[repo]; !ok {
		return fmt.Errorf("no repository %q", repo)
	}
	return recordInstruction(cfg, log, now, []string{repo}, "", to, "ui", text)
}

// recordInstruction records text as the owner's instruction for repos, or
// for console when it has none, screened, from where it was typed, for
// agentd to route.
func recordInstruction(cfg *Config, log *Log, now time.Time, repos []string, console, to, from, text string) error {
	return recordInstructionRe(cfg, log, now, repos, console, to, from, text, nil, nil)
}

// recordInstructionRe is recordInstruction for a response to entry re,
// which may be no more than answer, its words left out (#59).
func recordInstructionRe(cfg *Config, log *Log, now time.Time, repos []string, console, to, from, text string, re *EntryRe, answer *EntryAnswer) error {
	text = strings.TrimSpace(text)
	if text == "" && answer == nil {
		return errors.New("an empty instruction")
	}
	p := &InstructionPayload{Repositories: repos, Console: console, Text: text, Hash: shortHash(HashPane(text)), From: from, To: to, Route: true, Re: re, Answer: answer}
	for i, pat := range screenPatterns(cfg) {
		if regexp.MustCompile(pat).MatchString(text) {
			// Nothing of a withheld text is logged, not even its hash,
			// so it is never taken for a copy.
			p.Text, p.Hash, p.Withheld = "", "", i+1
			break
		}
	}
	// The entry's words were screened as the page showed them; once more
	// here, so none is logged that matches now.
	if r := p.Re; r != nil {
		for _, pat := range screenPatterns(cfg) {
			if m := regexp.MustCompile(pat); m.MatchString(r.Text) || m.MatchString(r.Question) {
				r.Text, r.Question = "", ""
			}
		}
	}
	p.ID = "I" + shortHash(HashPane(now.UTC().Format(time.RFC3339Nano) + "\n" + from + "\n" + p.Hash))[:8]
	return log.Append(Event{V: EventVersion, Time: now, Kind: KindInstruction, Mode: ModeOwner, Instruction: p})
}

// ImportLine is one message of the old console's history, as agentd
// console import reads it: one JSON object per line. The outside console
// program's lines (#42) give their time as TS, Unix seconds, a Kind, and
// To, the session a line posted with --to was typed to; a field of theirs
// import does not use, such as edited, is ignored (#44). Repository may
// name a console without a repository too.
type ImportLine struct {
	At         time.Time `json:"at"`
	TS         *float64  `json:"ts,omitempty"`
	From       string    `json:"from"`
	Kind       string    `json:"kind,omitempty"`
	Text       string    `json:"text"`
	Repository string    `json:"repository,omitempty"`
	To         string    `json:"to,omitempty"`
}

// importKey identifies an imported line, so a second import skips it.
func importKey(repo string, at time.Time, text string) string {
	return "imported:" + shortHash(HashPane(repo+"\n"+at.UTC().Format(time.RFC3339Nano)+"\n"+text))
}

// ImportConsole appends each line of r, the old console's history, as a
// feed event of phase imported for repo, or the line's own repository,
// with its time and author (#41). A line already imported, by repository,
// time and text, is skipped; every text is screened. Nothing is appended
// unless every line reads.
func ImportConsole(cfg *Config, log *Log, events []Event, now time.Time, repo string, r io.Reader) (added, skipped int, err error) {
	have := map[string]bool{}
	for _, e := range events {
		if e.Kind == KindFeed && e.Feed != nil && e.Feed.Phase == FeedImported {
			have[e.Feed.Key] = true
		}
	}
	var lines []ImportLine
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var l ImportLine
		if err := json.Unmarshal([]byte(text), &l); err != nil {
			return 0, 0, fmt.Errorf("line %d: %w", n, err)
		}
		if l.Repository == "" {
			l.Repository = repo
		}
		_, isRepo := cfg.Repositories[l.Repository]
		if _, isConsole := cfg.Consoles[l.Repository]; !isRepo && !isConsole {
			return 0, 0, fmt.Errorf("line %d: no repository or console %q", n, l.Repository)
		}
		if l.TS != nil && l.At.IsZero() {
			if !(*l.TS > 0 && *l.TS < 1e11) {
				return 0, 0, fmt.Errorf("line %d: ts %v is not a time in Unix seconds", n, *l.TS)
			}
			sec, frac := math.Modf(*l.TS)
			l.At = time.Unix(int64(sec), int64(math.Round(frac*1e9))).UTC()
		}
		if l.At.IsZero() || strings.TrimSpace(l.Text) == "" {
			return 0, 0, fmt.Errorf("line %d: a line needs its time and text", n)
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	pats := screenPatterns(cfg)
	for _, l := range lines {
		key := importKey(l.Repository, l.At, l.Text)
		if have[key] {
			skipped++
			continue
		}
		have[key] = true
		at := l.At
		p := &FeedPayload{Phase: FeedImported, Key: key, Repository: l.Repository, Text: l.Text, At: &at, From: l.From, Kind: l.Kind, To: l.To}
		for i, pat := range pats {
			if regexp.MustCompile(pat).MatchString(l.Text + "\n" + l.From + "\n" + l.To) {
				p.Text, p.From, p.To, p.Withheld = "", "", "", i+1
				break
			}
		}
		if err := log.Append(Event{V: EventVersion, Time: now, Kind: KindFeed, Mode: ModeOwner, Feed: p}); err != nil {
			return added, skipped, err
		}
		added++
	}
	return added, skipped, nil
}

// routeInstructions routes each owner instruction agentd routes that is
// neither delivered nor failed, and not held by a session that can still
// take it: to the session addressed, else to its repository's chat
// session, started for it. Each step is logged once; one that cannot be
// taken now waits, and is tried again each pass. Then it ends each chat
// session the owner has not written to for ChatIdle.
func (s *Supervisor) routeInstructions(now time.Time) {
	for _, id := range sortedKeys(s.st.Instructions) {
		in := s.st.Instructions[id]
		if !in.Route || !in.Delivered.IsZero() || in.Failed != "" || in.Session != "" && s.takesInstructions(in.Session) {
			continue
		}
		if s.routeOne(in, now); s.halt != nil {
			return
		}
	}
	s.endIdleChats(now)
}

// endIdleChats ends each chat session agentd started whose last
// instruction, or its start when it has none, is ChatIdle old, and whose
// instructions are all answered.
func (s *Supervisor) endIdleChats(now time.Time) {
	if s.shadow {
		return
	}
	for _, name := range sortedKeys(s.st.Runtime) {
		p := s.st.Runtime[name]
		if p.Kind != KindChat {
			continue
		}
		last, open := s.st.StartedAt[name], false
		for _, in := range s.st.instructionsFor(name, false) {
			for _, at := range []time.Time{in.At, in.Replied} {
				if at.After(last) {
					last = at
				}
			}
			open = open || in.open(s.st, now)
		}
		if !open && now.Sub(last) >= ChatIdle {
			if s.emit(name, Event{Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: newIntentID(), Action: SessionEnd, Repository: p.Repository, By: "agentd"}}); s.halt != nil {
				return
			}
		}
	}
}

// open says whether in is delivered and not yet answered, by a session
// that may still answer it, within ChatIdle of its delivery.
func (in *Instruction) open(st *LogState, now time.Time) bool {
	t := st.Tracks[in.Session]
	return in.Route && in.Failed == "" && !in.Delivered.IsZero() && in.Replied.IsZero() && now.Sub(in.Delivered) < ChatIdle &&
		(t == nil || !t.Parked && t.State != Done && t.State != Stopped && t.State != CrashLooping)
}

// chatInFlight says whether an instruction is open, for a pass every
// ChatPoll.
func (st *LogState) chatInFlight(now time.Time) bool {
	for _, in := range st.Instructions {
		if in.open(st, now) {
			return true
		}
	}
	return false
}

// openSince is the earliest delivery of the instructions open on session
// name, zero when none is: the tools it runs from then on are logged.
func (st *LogState) openSince(name string, now time.Time) time.Time {
	var since time.Time
	for _, in := range st.Instructions {
		if in.Session == name && in.open(st, now) && (since.IsZero() || in.Delivered.Before(since)) {
			since = in.Delivered
		}
	}
	return since
}

// routeOne finds the session for in, starting one when it must.
func (s *Supervisor) routeOne(in *Instruction, now time.Time) {
	if in.Withheld > 0 {
		s.routeStep(in, RouteFail, "", false, fmt.Sprintf("its text matched confidential pattern %d, so it was never logged and agentd cannot deliver it; reword it, or type it into the session yourself", in.Withheld))
		return
	}
	if in.To != "" {
		// A message the owner addressed goes to that session or nowhere:
		// another session has not its context.
		t := s.st.Tracks[in.To]
		switch sess, ok := s.cfg.Sessions[in.To]; {
		case s.takesInstructions(in.To):
			s.routeStep(in, RouteTo, in.To, false, "")
		case !ok || sess.Role != "" || t != nil && t.State == Done:
			s.routeStep(in, RouteFail, "", false, in.To+" is not running, or finished, or takes no messages; write to the console's chat instead")
		default:
			s.routeStep(in, RouteWait, "", false, in.To+" is "+string(stateOf(t))+"; agentd hands it the message once it can take one")
		}
		return
	}
	if len(in.Repositories) == 0 {
		why := "console " + in.Console + " feeds no repository, so agentd has no session to start for it; reply in one of its sessions' boxes"
		if in.To != "" {
			why = in.To + " is not running, and console " + in.Console + " feeds no repository to start a session in"
		}
		s.routeStep(in, RouteFail, "", false, why)
		return
	}
	repo := in.Repositories[0]
	if s.cfg.Repositories[repo].Sessions == nil {
		s.routeStep(in, RouteFail, "", false, repo+" has no sessions entry, so agentd starts no chat session there; reply in one of its sessions' boxes")
		return
	}
	name := runtimeName(repo, chatBranch, KindChat)
	if _, ok := s.st.Runtime[name]; ok {
		if s.takesInstructions(name) {
			s.routeStep(in, RouteTo, name, false, "")
			return
		}
		if t := s.st.Tracks[name]; t == nil || t.State != Done {
			s.routeStep(in, RouteWait, "", false, name+" is "+string(stateOf(t))+"; agentd hands it nothing until the owner clears it")
			return
		}
		// A chat session that wrote DONE takes no line: it is ended, and
		// started again for this instruction once it has stopped.
		if !s.shadow {
			s.emit(name, Event{Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: newIntentID(), Action: SessionEnd, Repository: repo, By: "agentd"}})
		}
		s.routeStep(in, RouteWait, "", false, name+" finished; agentd ends it and starts it again for this instruction")
		return
	}
	if s.shadow {
		s.routeStep(in, RouteWait, "", false, repo+" has no chat session, and shadow mode starts none")
		return
	}
	if why := s.startRuntime(repo, name, RuntimePayload{Kind: KindChat, Branch: chatBranch}); why != "" {
		s.routeStep(in, RouteWait, "", false, "agentd cannot start "+name+" for it yet: "+why)
		return
	}
	s.refreshRuntime()
	s.routeStep(in, RouteTo, name, true, "")
}

// stateOf is a track's state, or unknown.
func stateOf(t *Track) State {
	if t == nil || t.State == "" {
		return "unknown"
	}
	return t.State
}

// routeStep logs a step of in unless it is where in stands already.
func (s *Supervisor) routeStep(in *Instruction, step, session string, started bool, reason string) {
	switch {
	case step == RouteTo && in.Session == session && in.Waiting == "",
		step == RouteWait && in.Waiting == reason,
		step == RouteFail && in.Failed == reason:
		return
	}
	s.emit("", Event{Kind: KindRoute, Route: &RoutePayload{Instruction: in.ID, Step: step, Session: session, Started: started, Reason: reason}})
}

// takesInstructions says whether session name can be handed the owner's
// instructions: a session of agentd's or the owner's, not a reviewer or a
// judge, and not finished, stopped or parked. One gone is restarted with
// them.
func (s *Supervisor) takesInstructions(name string) bool {
	sess, ok := s.cfg.Sessions[name]
	if !ok || sess.Role != "" {
		return false
	}
	t := s.st.Tracks[name]
	return t == nil || !t.Parked && t.State != Done && t.State != Stopped && t.State != CrashLooping
}

// instructionsFor lists the instructions routed to session name, oldest
// first; with undelivered, only those not delivered yet.
func (st *LogState) instructionsFor(name string, undelivered bool) []*Instruction {
	var out []*Instruction
	for _, in := range st.Instructions {
		if in.Route && in.Session == name && in.Failed == "" && (!undelivered || in.Delivered.IsZero()) {
			out = append(out, in)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// instructionsKept bounds the instructions instructions.json holds: the
// latest.
const instructionsKept = 20

// InstructionDoc is one instruction as instructions.json holds it. New
// marks one not answered yet, within ChatIdle of its delivery.
type InstructionDoc struct {
	ID   string    `json:"id"`
	At   time.Time `json:"at"`
	From string    `json:"from"`
	// Re is the entry a response is about and Answer the answer it gave
	// a decision (#59): each record stands alone.
	Re     *EntryRe     `json:"re,omitempty"`
	Answer *EntryAnswer `json:"answer,omitempty"`
	Text   string       `json:"text,omitempty"`
	New    bool         `json:"new,omitempty"`
}

// writeInstructions writes the instructions routed to the session to
// instructions.json in its workdir, through a temporary file, and names
// the new ones.
func writeInstructions(workdir string, ins []*Instruction, now time.Time) error {
	if len(ins) > instructionsKept {
		ins = ins[len(ins)-instructionsKept:]
	}
	doc := struct {
		Instructions []InstructionDoc `json:"instructions"`
	}{Instructions: []InstructionDoc{}}
	for _, in := range ins {
		doc.Instructions = append(doc.Instructions, InstructionDoc{ID: in.ID, Text: in.Text, From: in.From, At: in.At, Re: in.Re, Answer: in.Answer, New: in.Replied.IsZero() && (in.Delivered.IsZero() || now.Sub(in.Delivered) < ChatIdle)})
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(workdir, InstructionsFile), append(b, '\n'))
}

// freshInstructions names the instructions routed to session name and not
// yet delivered; with unanswered, also those delivered and not answered
// within ChatIdle, which a fresh session launched in its place is handed
// again. One older is let go: the owner has moved on.
func (st *LogState) freshInstructions(name string, unanswered bool, now time.Time) []string {
	var ids []string
	for _, in := range st.instructionsFor(name, false) {
		if in.Delivered.IsZero() || unanswered && in.Replied.IsZero() && now.Sub(in.Delivered) < ChatIdle {
			ids = append(ids, in.ID)
		}
	}
	return ids
}

// deliverInstructions hands the owner's new instructions to a live
// session as deliverAnswers hands answers: every instruction routed to it
// written to instructions.json, then the fixed pointer line typed, only
// into a session that can take a line. It says whether it tried to type.
func (s *Supervisor) deliverInstructions(name string, sess Session, t *Track, ev Evidence, state State) bool {
	ids := s.st.freshInstructions(name, false, s.clock.Now())
	// Only a session that can take a line: never one waiting on the
	// owner, whose answers reach it through deliverAnswers.
	if len(ids) == 0 || !ev.Alive || t.Limited || ev.Dialog != "" || ev.TrustPrompt || state != Working && state != Idle {
		return false
	}
	key := "instruct:" + name
	if !s.retryDue(t, key, s.clock.Now()) {
		return false
	}
	ins := s.st.instructionsFor(name, false)
	pointer := s.cfg.Policies.Instructions.Pointer
	s.typeInto(name, ActInstruct, &ActPayload{Key: key, Instructions: ids, Message: pointer}, t, ev.PaneHash, func() error {
		if err := writeInstructions(sess.Workdir, ins, s.clock.Now()); err != nil {
			return err
		}
		return s.tmux.Type(name, pointer)
	})
	return true
}

// applyRoute folds a route step.
func (st *LogState) applyRoute(e Event) {
	p := e.Route
	in := st.Instructions[p.Instruction]
	if in == nil {
		return
	}
	switch p.Step {
	case RouteTo:
		in.Session, in.Started, in.Waiting = p.Session, p.Started, ""
	case RouteWait:
		in.Waiting = p.Reason
	case RouteFail:
		in.Failed, in.Waiting = p.Reason, ""
	}
}

// instructionsDelivered marks the instructions an instruct or a restart
// handed over, a would-be one's too, so shadow mode records each once. A
// restart hands an unanswered one over again: what the session before it
// did is not its answer, and its own launch prompt is it taking it.
func (st *LogState) instructionsDelivered(session string, a *ActPayload, at time.Time) {
	for _, id := range a.Instructions {
		if in := st.Instructions[id]; in != nil && in.Session == session && in.Replied.IsZero() {
			in.Delivered, in.Taken, in.Activity, in.Waiting = at, time.Time{}, nil, ""
		}
	}
}

// instructionTurn threads a session's turn under the instructions
// delivered to it: its first prompt after a delivery is the session taking
// them, each tool it runs then is its activity, and the reply it ends that
// turn with is its answer.
func (st *LogState) instructionTurn(session string, tn *TurnPayload) {
	for _, in := range st.Instructions {
		// A turn ChatIdle after the delivery, by its own time, so a
		// replay agrees, answers it no more: the owner has moved on.
		if in.Session != session || in.Delivered.IsZero() || !in.Replied.IsZero() || !tn.At.IsZero() && tn.At.Sub(in.Delivered) >= ChatIdle {
			continue
		}
		switch {
		case tn.Role == TurnPrompt && in.Taken.IsZero() && (tn.At.IsZero() || !tn.At.Before(in.Delivered.Add(-routeSkew))):
			in.Taken = tn.At
			if in.Taken.IsZero() {
				in.Taken = in.Delivered
			}
		case tn.Role == TurnTool && !in.Taken.IsZero():
			if len(in.Activity) == activityKept {
				in.Activity = in.Activity[1:len(in.Activity):len(in.Activity)]
			}
			in.Activity = append(in.Activity, *tn)
		case tn.Role == TurnSummary && !in.Taken.IsZero():
			in.Reply, in.ReplyWithheld, in.Replied = tn.Text, tn.Withheld > 0, tn.At
			if in.Replied.IsZero() {
				in.Replied = in.Taken
			}
		}
	}
}
