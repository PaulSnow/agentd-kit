package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Each repository's coordinator (#3) carries the judgment the state machine
// cannot make. It used to wake on its own timer and re-read the repository
// each time, so most of its tokens went on collecting, not judging. agentd
// collects instead: it reads every session's prompts and end-of-turn
// summaries from the transcripts it already reads for usage, folds them
// with what else it holds into one small pack per repository, and wakes
// the coordinator, by typing one fixed pointer line, only when a judgment
// is due. Nothing due costs nothing. A coordinator is rotated at a wake it
// has handled, and its fresh session starts from the pack.

// PackFile is the pack agentd writes to a coordinator's workdir.
const PackFile = "pack.json"

const (
	turnText    = 300          // bytes of a prompt or summary logged, cut after it is screened whole
	turnRead    = 4 * turnText // bytes of a prompt or summary read, so a term across the cut is screened
	turnsKept   = 16           // a session's latest turns its Track keeps
	turnsRead   = 256          // turns a transcript reader holds until they are logged
	wakesKept   = 20           // a coordinator's latest wakes its Track keeps
	wakeReasons = 500          // bytes of a wake's reasons kept
	wakeMargin  = time.Hour    // how late a fact may be applied after the time it was logged
)

// Turn roles.
const (
	TurnPrompt  = "prompt"  // what the session was asked
	TurnSummary = "summary" // the text it ended a turn with
)

// TurnPayload is one prompt or end-of-turn summary of a session's
// transcript, at its line, so each is logged once however often the
// transcript is read. Text is bounded; Withheld is the number of the
// confidential pattern it matched, when it did, and Text is then empty.
type TurnPayload struct {
	Transcript string    `json:"transcript"`
	Line       int       `json:"line"`
	Role       string    `json:"role"`
	Text       string    `json:"text,omitempty"`
	At         time.Time `json:"at"`
	Withheld   int       `json:"withheld,omitempty"`
}

// TurnRecord is a turn as a Track and a pack hold it.
type TurnRecord struct {
	Role     string    `json:"role"`
	Text     string    `json:"text,omitempty"`
	Withheld bool      `json:"withheld,omitempty"`
	At       time.Time `json:"at"`
}

// CoordinatorPayload is a session's claim on a repository's coordinator,
// a shared fact any run reads, or, with no session, the owner's release of
// it.
type CoordinatorPayload struct {
	Repository string `json:"repository"`
}

// WakeRecord is one wake of a coordinator: when, why, whether the pointer
// was typed or, in shadow mode, would have been (a wake cut short may have
// typed nothing, so nothing is outstanding), and its spend then.
type WakeRecord struct {
	At     time.Time
	Reason string
	Taken  bool
	Spend  *int64
}

// turnOf reads a transcript line as a turn: a main-chain user prompt in
// the user's own words (not a tool result or a meta line), or a main-chain
// assistant line that ends its turn with text.
func turnOf(line []byte, n int) (TurnPayload, bool) {
	var l struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		IsMeta      bool   `json:"isMeta"`
		Timestamp   string `json:"timestamp"`
		Message     struct {
			StopReason string          `json:"stop_reason"`
			Content    json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil || l.IsSidechain || l.IsMeta {
		return TurnPayload{}, false
	}
	var role string
	switch {
	case l.Type == "user":
		role = TurnPrompt
	case l.Type == "assistant" && l.Message.StopReason == "end_turn":
		role = TurnSummary
	default:
		return TurnPayload{}, false
	}
	var text string
	if json.Unmarshal(l.Message.Content, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			return TurnPayload{}, false
		}
		var parts []string
		for _, b := range blocks {
			switch b.Type {
			case "text":
				parts = append(parts, b.Text)
			case "tool_result", "tool_use":
				return TurnPayload{}, false
			}
		}
		text = strings.Join(parts, "\n")
	}
	if text = strings.TrimSpace(text); text == "" {
		return TurnPayload{}, false
	}
	at, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
	return TurnPayload{Line: n, Role: role, Text: cutUTF8(text, turnRead), At: at}, true
}

// noteTurn keeps a turn read from the transcript until it is logged; the
// oldest go first when a transcript gains more than turnsRead at once.
func (f *transcriptFile) noteTurn(line []byte) {
	if tn, ok := turnOf(line, f.lines); ok {
		if len(f.turns) == turnsRead {
			f.turns = f.turns[1:]
		}
		f.turns = append(f.turns, tn)
	}
}

// turns are the turns of transcript id in dir read so far, after line.
func (r *usageReader) turns(dir, id string, after int) []TurnPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.files[filepath.Join(dir, id+".jsonl")]
	if f == nil {
		return nil
	}
	var out []TurnPayload
	for _, tn := range f.turns {
		if tn.Line > after {
			tn.Transcript = id
			out = append(out, tn)
		}
	}
	return out
}

// applyTurn folds a logged turn into the session's Track.
func (t *Track) applyTurn(tn *TurnPayload) {
	if t.TurnLines == nil {
		t.TurnLines = map[string]int{}
	}
	if tn.Line > t.TurnLines[tn.Transcript] {
		t.TurnLines[tn.Transcript] = tn.Line
	}
	t.TurnCount++
	// A turn ended since the last nudge answered it, however soon after:
	// the idle clock and the nudge count start again from its end.
	if tn.Role == TurnSummary && t.Nudges > 0 && tn.At.After(t.LastNudge) {
		t.Nudges, t.HashSince = 0, tn.At
	}
	r := TurnRecord{Role: tn.Role, Text: tn.Text, Withheld: tn.Withheld > 0, At: tn.At}
	if len(t.Turns) == turnsKept {
		t.Turns = t.Turns[1:len(t.Turns):len(t.Turns)]
	}
	t.Turns = append(t.Turns[:len(t.Turns):len(t.Turns)], r)
}

// applyWake folds a coordinator's wake, taken or would-be, into its Track.
func (t *Track) applyWake(a *ActPayload, taken, counted bool, at time.Time) {
	if counted {
		t.WakeCount++
	}
	if t.WakeBase == nil && a.Spend != nil {
		sp := *a.Spend
		t.WakeBase = &sp
	}
	// A wake cut short may never have typed its pointer, so it is not
	// outstanding; a second pointer is harmless.
	w := WakeRecord{At: at, Reason: a.Reason, Taken: counted, Spend: a.Spend}
	// What it covered is not due again; what is older than the margin
	// before it cannot be due anyway.
	if t.Woken == nil {
		t.Woken = map[string]bool{}
	}
	for _, k := range a.Covered {
		id, ok := instructionKey(k)
		switch {
		case !ok:
			t.Woken[k] = true
		case counted:
			// A wake cut short leaves the instruction due.
			if t.Carried == nil {
				t.Carried = map[string]int{}
			}
			t.Carried[id] = t.WakeCount
		}
	}
	for k := range t.Woken {
		if when, ok := dueTime(k); !ok || when.Before(at.Add(-wakeMargin)) {
			delete(t.Woken, k)
		}
	}
	if len(t.Wakes) == wakesKept {
		t.Wakes = t.Wakes[1:len(t.Wakes):len(t.Wakes)]
	}
	t.Wakes = append(t.Wakes[:len(t.Wakes):len(t.Wakes)], w)
}

// lastWake is the coordinator's latest wake, if it has had one.
func (t *Track) lastWake() (WakeRecord, bool) {
	if len(t.Wakes) == 0 {
		return WakeRecord{}, false
	}
	return t.Wakes[len(t.Wakes)-1], true
}

// wakeTokens is the coordinator's spend per wake since its first, when its
// spend is known.
func (t *Track) wakeTokens(exact bool) (int64, bool) {
	sp, ok := t.spend(exact)
	if !ok || t.WakeBase == nil || t.WakeCount == 0 {
		return 0, false
	}
	return (sp - *t.WakeBase) / int64(t.WakeCount), true
}

// handled says whether the coordinator has finished its latest wake, so
// another may be typed or it may be rotated: it has had none, or none was
// typed, or its transcript ended a turn after it. A transcript that has
// not moved since the wake for the nudge policy's stall never took the
// line, and the wake counts as handled. Without transcripts, a screen
// still for the stall stands in.
func (s *Supervisor) handled(t *Track, ev Evidence, now time.Time) bool {
	w, ok := t.lastWake()
	if !ok || !w.Taken {
		return true
	}
	stall := s.cfg.Policies.Nudge.Stall.Duration
	still := now.Sub(t.HashSince) >= stall && now.Sub(w.At) >= stall
	if s.cfg.TranscriptDir == "" {
		return still
	}
	tr := ev.Transcript
	if tr.Found && tr.At.After(w.At) {
		// A turn that ended otherwise, interrupted or failed, has ended
		// once the screen has been still for the stall.
		return tr.Role == "assistant" && tr.StopReason == "end_turn" || still
	}
	return now.Sub(w.At) >= stall
}

// claimed says whether name holds its repository's coordinator, claiming
// it when no one does. Another session's claim holds until the owner
// releases it: one coordinator per repository, from the log (#6).
func (s *Supervisor) claimed(name string, sess Session, t *Track) bool {
	switch holder := s.st.Coordinators[sess.Repository]; holder {
	case name:
		return true
	case "":
		// A shadow run holds it in its own memory: a claim in the log
		// would hold against the live run.
		if s.shadow {
			s.st.Coordinators[sess.Repository] = name
			return true
		}
		if s.emit(name, Event{Kind: KindCoordinator, Mode: ModeShared, Coordinator: &CoordinatorPayload{Repository: sess.Repository}}); s.halt != nil {
			return false
		}
		// Another run may have claimed it since this pass read the log:
		// the claim the log holds first stands, as a replay reads it.
		events, _, err := s.log.Tail(s.tailOff)
		if err != nil {
			return false
		}
		holder := ""
		for _, e := range events {
			if e.Kind != KindCoordinator || e.Coordinator.Repository != sess.Repository {
				continue
			}
			switch {
			case e.Mode == ModeOwner:
				holder = ""
			case e.Mode == ModeShared && holder == "":
				holder = e.Session
			}
		}
		if holder != "" {
			s.st.Coordinators[sess.Repository] = holder
		}
		return s.st.Coordinators[sess.Repository] == name
	default:
		s.skipOnce(name, "coordinator:"+sess.Repository+":"+holder, fmt.Sprintf("%s already coordinates %s; %s is not launched or woken until the owner runs agentd coordinator release %s",
			holder, sess.Repository, name, sess.Repository), t, nil)
		return false
	}
}

// due is what the coordinator of repo should judge, as reasons and the
// keys that settle them: a session of the repository that finished or
// waits on the owner, a new item for it, an owner answer to one of its
// sessions, an owner instruction for it, each once. A fact counts from
// the margin before the latest wake, less what that wake and those before
// it covered, so one another process logged during a wake's pass, at its
// time, is not lost. An instruction counts until a wake carried it, so
// one a wake cut short covered is due again. The coordinator's own
// answers reach it by their pointer.
func (s *Supervisor) due(repo string, t *Track) (reasons, keys []string) {
	since := time.Time{}
	if w, ok := t.lastWake(); ok {
		since = w.At.Add(-wakeMargin)
	}
	add := func(reason string, at time.Time) {
		if k := reason + "@" + at.UTC().Format(time.RFC3339Nano); at.After(since) && !t.Woken[k] {
			reasons, keys = append(reasons, reason), append(keys, k)
		}
	}
	for _, n := range s.cfg.SessionNames() {
		sess, o := s.cfg.Sessions[n], s.st.Tracks[n]
		if sess.Repository != repo || sess.Role == RoleCoordinator || o == nil {
			continue
		}
		switch o.State {
		case Done:
			add("finished: "+n, o.Since)
		case BlockedOnOwner:
			add("waiting: "+n, o.Since)
		}
	}
	for _, it := range s.packItems(repo) {
		add("item: "+it.ID, s.st.Items[it.ID].FirstSeen)
	}
	for _, n := range s.cfg.SessionNames() {
		sess, o := s.cfg.Sessions[n], s.st.Tracks[n]
		if sess.Repository != repo || sess.Role == RoleCoordinator || o == nil {
			continue
		}
		ids := make([]string, 0, len(o.Answers))
		for id := range o.Answers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			add("answer: "+n+"/"+id, o.Answers[id].At)
		}
	}
	// A request no session delivered whose own CI failed, once per head,
	// run and cause: its coordinator may start a fix session for it.
	for _, r := range s.outsideRecords(repo) {
		if r.State == CIFailed && r.Verdict == CIOwn {
			add(ciReason(r), r.FailedAt)
		}
	}
	for _, in := range s.st.instructionsFor(repo) {
		if _, carried := t.Carried[in.ID]; !carried {
			reasons = append(reasons, instructionReason+in.ID)
			keys = append(keys, instructionReason+in.ID+"@"+in.At.UTC().Format(time.RFC3339Nano))
		}
	}
	return reasons, keys
}

// dueTime is the time a due key names.
func dueTime(key string) (time.Time, bool) {
	i := strings.LastIndexByte(key, '@')
	if i < 0 {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, key[i+1:])
	return at, err == nil
}

// wake writes the pack and types the fixed pointer into a coordinator that
// has handled its latest wake and can take a line, when a judgment is due.
// In shadow mode it is a would event; nothing is written or typed. It says
// whether it tried to type.
func (s *Supervisor) wake(name string, sess Session, t *Track, ev Evidence, state State, now time.Time) bool {
	due, keys := s.due(sess.Repository, t)
	if len(due) == 0 || !ev.Alive || t.Limited || ev.Dialog != "" || ev.TrustPrompt || state != Working && state != Idle || !s.handled(t, ev, now) {
		return false
	}
	key := fmt.Sprintf("wake:%s:%d", sess.Repository, t.WakeCount+1)
	if !s.retryDue(t, key, now) {
		return false
	}
	_, pack := s.buildPack(sess.Repository, due, now, true)
	pointer := s.cfg.Policies.Coordinator.Pointer
	a := &ActPayload{Key: key, Reason: cutUTF8(strings.Join(due, "; "), wakeReasons), Message: pointer, Covered: keys}
	if sp, ok := t.spend(!s.shadow); ok {
		a.Spend = &sp
	}
	s.typeInto(name, ActWake, a, t, ev.PaneHash, func() error {
		if err := writeAtomic(filepath.Join(sess.Workdir, PackFile), pack); err != nil {
			return err
		}
		return s.tmux.Type(name, pointer)
	})
	return true
}

// Pack is a repository's state as its coordinator reads it: data, not
// instructions. Summarized counts what the byte limit, or the per-session
// limit on turns, left out.
type Pack struct {
	Repository string     `json:"repository"`
	At         time.Time  `json:"at"`
	LastWake   *time.Time `json:"last_wake,omitempty"`
	Due        []string   `json:"due,omitempty"`
	// Policy is the repository's effective policy (policy.go), so the
	// coordinator's brief need not repeat it. Its confidential patterns
	// are left out: they name what must not reach a session.
	Policy    RepoPolicy     `json:"policy"`
	Sessions  []PackSession  `json:"sessions"`
	Decisions []PackDecision `json:"decisions,omitempty"`
	Items     []PackItem     `json:"items,omitempty"`
	// OwnerInstructions are the owner's console instructions for the
	// repository, oldest first, until a wake that carried them is handled.
	OwnerInstructions []PackInstruction `json:"owner_instructions,omitempty"`
	// Requests are the repository's open requests no session delivered,
	// as the CI watcher last recorded them (#36).
	Requests   []PackRequest `json:"requests,omitempty"`
	Summarized PackCounts    `json:"summarized"`
}

// PackSession is one session of the repository: its state, its case and
// its latest turns, oldest first.
type PackSession struct {
	Name   string       `json:"name"`
	State  State        `json:"state"`
	Since  time.Time    `json:"since"`
	Reason string       `json:"reason,omitempty"`
	Case   string       `json:"case,omitempty"`
	Turns  []TurnRecord `json:"turns,omitempty"`
}

// PackDecision is a decision a session of the repository raised.
type PackDecision struct {
	Session  string `json:"session"`
	ID       string `json:"id"`
	Question string `json:"question,omitempty"`
	Blocking bool   `json:"blocking"`
	Answered bool   `json:"answered"`
	Withheld bool   `json:"withheld,omitempty"`
}

// PackItem is an item for the repository the owner has not marked seen.
type PackItem struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Link       string `json:"link,omitempty"`
	Summary    string `json:"summary,omitempty"`
	NeedsOwner string `json:"needs_owner,omitempty"`
}

// PackCounts are the entries a pack left out, by kind.
type PackCounts struct {
	Turns        int `json:"turns"`
	Decisions    int `json:"decisions"`
	Items        int `json:"items"`
	Due          int `json:"due"`
	Instructions int `json:"instructions"`
	Requests     int `json:"requests"`
}

// packItems are repo's unseen items, oldest first, never the owner's own.
func (s *Supervisor) packItems(repo string) []PackItem {
	r, ok := s.cfg.Repositories[repo]
	if !ok {
		return nil
	}
	key := repoKey(r.Forge, r.Project)
	var its []*Item
	for _, it := range s.st.Items {
		if it.Repository == key && !it.Seen && !it.OwnerOnly {
			its = append(its, it)
		}
	}
	sort.Slice(its, func(i, j int) bool {
		if !its[i].FirstSeen.Equal(its[j].FirstSeen) {
			return its[i].FirstSeen.Before(its[j].FirstSeen)
		}
		return its[i].ID < its[j].ID
	})
	out := make([]PackItem, 0, len(its))
	for _, it := range its {
		p := PackItem{ID: it.ID, Kind: it.Kind, Title: cutUTF8(it.Title, turnText), Link: it.Link}
		if tr := it.Triage; tr != nil {
			p.Summary, p.NeedsOwner = cutUTF8(tr.Summary, turnText), tr.NeedsOwner
		}
		out = append(out, p)
	}
	return out
}

// buildPack is repo's pack, and its compact JSON, which stays under the
// policy's byte limit: the oldest turns go first, then the oldest items,
// then the latest decisions, then the last reasons due, then the oldest
// instructions, each counted in Summarized. Handled says the coordinator
// has handled its latest wake, as it has when it is woken again.
func (s *Supervisor) buildPack(repo string, due []string, now time.Time, handled bool) (Pack, []byte) {
	pol := s.cfg.Policies.Coordinator
	p := Pack{Repository: repo, At: now, Due: append([]string(nil), due...), Sessions: []PackSession{}}
	p.Policy, _ = effectivePolicy(s.cfg, s.st, repo)
	p.Policy.ConfidentialPatterns = nil
	if co := s.cfg.Repositories[repo].Coordinator; co != nil {
		if w, ok := s.track(co.Session).lastWake(); ok {
			at := w.At
			p.LastWake = &at
		}
	}
	for _, n := range s.cfg.SessionNames() {
		sess := s.cfg.Sessions[n]
		if sess.Repository != repo || sess.Role == RoleCoordinator {
			continue
		}
		t := s.track(n)
		ps := PackSession{Name: n, State: t.State, Since: t.Since, Reason: cutUTF8(t.Reason, turnText), Case: s.cfg.caseOf(sess)}
		shown := t.Turns
		if len(shown) > pol.Turns {
			shown = shown[len(shown)-pol.Turns:]
		}
		ps.Turns = append([]TurnRecord(nil), shown...)
		p.Summarized.Turns += t.TurnCount - len(shown)
		p.Sessions = append(p.Sessions, ps)
		for _, d := range t.Decisions {
			_, answered := t.answerTo(d)
			p.Decisions = append(p.Decisions, PackDecision{Session: n, ID: d.ID, Question: cutUTF8(d.Question, turnText),
				Blocking: d.Blocking, Answered: answered, Withheld: d.Withheld})
		}
	}
	p.Items = s.packItems(repo)
	p.OwnerInstructions = s.packInstructions(repo, handled)
	p.Requests = s.packRequests(repo)
	for {
		b := compactJSON(p)
		if len(b) <= pol.PackBytes {
			return p, b
		}
		switch {
		case dropOldestTurn(&p):
			p.Summarized.Turns++
		case len(p.Items) > 0:
			p.Items = p.Items[1:]
			p.Summarized.Items++
		case len(p.Requests) > 0:
			p.Requests = p.Requests[:len(p.Requests)-1]
			p.Summarized.Requests++
		case len(p.Decisions) > 0:
			p.Decisions = p.Decisions[:len(p.Decisions)-1]
			p.Summarized.Decisions++
		case len(p.Due) > 1:
			p.Due = p.Due[:len(p.Due)-1] // the first reasons are kept
			p.Summarized.Due++
		case len(p.OwnerInstructions) > 0:
			p.OwnerInstructions = p.OwnerInstructions[1:]
			p.Summarized.Instructions++
		default:
			return p, b // only the sessions' states are left
		}
	}
}

// dropOldestTurn drops the pack's oldest turn, of whichever session.
func dropOldestTurn(p *Pack) bool {
	oldest := -1
	for i, ps := range p.Sessions {
		if len(ps.Turns) > 0 && (oldest < 0 || ps.Turns[0].At.Before(p.Sessions[oldest].Turns[0].At)) {
			oldest = i
		}
	}
	if oldest < 0 {
		return false
	}
	p.Sessions[oldest].Turns = p.Sessions[oldest].Turns[1:]
	return true
}

// compactJSON is v as one line of JSON, with no HTML escaping.
func compactJSON(v any) []byte {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return []byte(strings.TrimSuffix(b.String(), "\n"))
}

// ReleaseCoordinator is the owner's record that repo's coordinator claim
// is released, so the session its configuration names may claim it.
func ReleaseCoordinator(cfg *Config, log *Log, now time.Time, repo string) error {
	if _, ok := cfg.Repositories[repo]; !ok {
		return fmt.Errorf("repository %q is not configured", repo)
	}
	return log.Append(Event{V: EventVersion, Time: now, Kind: KindCoordinator, Mode: ModeOwner, Coordinator: &CoordinatorPayload{Repository: repo}})
}
