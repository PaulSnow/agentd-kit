package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"
)

// A case is one issue: the sessions that work it, the context pack agentd
// handed them, what they report, and the owner's rulings. It is a
// projection of the event log, never a separate store: a case event carries
// exactly one of a pack, a report, a ruling, the owner's goal or a
// session's tool call, and apply folds it.

// CasePayload is one fact about a case, named by its id: the repository's
// forge and project and the issue number, as an issue item's id names it.
type CasePayload struct {
	Case   string      `json:"case"`
	Pack   *CasePack   `json:"pack,omitempty"`
	Report *CaseReport `json:"report,omitempty"`
	Ruling *CaseRuling `json:"ruling,omitempty"`
	Goal   *CaseGoal   `json:"goal,omitempty"`
	Call   *CaseCall   `json:"call,omitempty"`
	Rounds *CaseRounds `json:"rounds,omitempty"`
}

// CasePack is the context pack a launch handed the session: the facts
// document it typed, as compact JSON, and its hash.
type CasePack struct {
	Hash  string          `json:"hash"`
	Facts json.RawMessage `json:"facts"`
}

// CaseReport is what the session's progress file says, logged when it
// changes: the current item, its state and the items after it.
type CaseReport struct {
	Hash  string   `json:"hash"`
	Item  string   `json:"item,omitempty"`
	State string   `json:"state,omitempty"`
	Next  []string `json:"next,omitempty"`
}

// CaseRuling is the owner's: a one-shot restriction or waiver for this case
// only, kept verbatim with its id, or the case closed or reopened. Lasting
// rules live in the repository's policy, not here.
type CaseRuling struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"` // R1, R2… for a restriction or waiver
	Text string `json:"text,omitempty"`
	By   string `json:"by,omitempty"`
	// Close is a close's record (rounds.go); a close logged before
	// closes had records has none.
	Close *CloseRecord `json:"close,omitempty"`
}

// Ruling kinds.
const (
	RulingRestriction = "restriction"
	RulingWaiver      = "waiver"
	RulingClose       = "close"
	RulingReopen      = "reopen"
)

// check refuses a case payload that names no case, carries other than
// exactly one of a pack, a report, a ruling, a goal, a call or the owner's
// rounds, a ruling of an unknown kind or without the id a restriction or
// waiver has, a close record without its demo or N/A, or a call of an
// unknown tool.
func (p *CasePayload) check() error {
	n := 0
	for _, set := range []bool{p.Pack != nil, p.Report != nil, p.Ruling != nil, p.Goal != nil, p.Call != nil, p.Rounds != nil} {
		if set {
			n++
		}
	}
	if _, _, ok := splitCase(p.Case); !ok || n != 1 || p.Case != strings.ToLower(p.Case) {
		return errors.New("a case event names its case and carries exactly one of a pack, a report, a ruling, a goal, a call or the owner's rounds")
	}
	if r := p.Rounds; r != nil && (r.Max < 1 || r.Max > maxRounds) {
		return fmt.Errorf("a backstop is 1 to %d rounds", maxRounds)
	}
	if p.Call != nil {
		return p.Call.check()
	}
	if r := p.Ruling; r != nil {
		switch r.Kind {
		case RulingRestriction, RulingWaiver:
			if r.ID == "" {
				return fmt.Errorf("a %s ruling needs its id", r.Kind)
			}
		case RulingClose, RulingReopen:
			if r.ID != "" || r.Text != "" {
				return fmt.Errorf("a %s ruling takes no id or text", r.Kind)
			}
		default:
			return fmt.Errorf("unknown ruling %q", r.Kind)
		}
		if r.Close != nil && r.Kind != RulingClose {
			return errors.New("only a close carries a close record")
		}
		if r.Close != nil {
			return r.Close.check()
		}
	}
	return nil
}

// maxRuling bounds a ruling's text.
const maxRuling = 500

// Case states, derived from facts: the owner's rulings and the tracks of
// the sessions that work the case, never from what a session claims.
const (
	CaseClosed     = "closed"
	CaseNeedsYou   = "needs you"
	CaseStopped    = "stopped"
	CaseDelivered  = "delivered"
	CaseWorking    = "working"
	CaseNotStarted = "not started"
)

// Case is a case as the log has it.
type Case struct {
	ID       string
	First    time.Time
	Sessions []string // the sessions whose events name it, in order of first
	Pack     CasePack
	PackAt   time.Time
	Packs    int
	Report   CaseReport // the latest report of any of its sessions
	ReportAt time.Time
	Reports  map[string]string // each session's latest report hash
	Rulings  []RulingRecord    // every ruling, in order
	Active   []RulingRecord    // the restrictions and waivers in force
	Closed   bool
	Goal     []GoalItem          // the done-when items, the superseded too
	Progress *ProgressRecord     // the latest accepted report_progress
	Drift    []DriftFlag         // what that report's restatement drifted on
	Steps    []StepRecord        // every step a session asked for
	Refused  int                 // calls refused
	Acked    map[string][]string // the rulings each session acknowledged
	// Decisions is every decision its sessions raised through their
	// tools, the superseded too, in order.
	Decisions []RaisedRecord
	// Rounds is every review round, in order (rounds.go). RoundStop is
	// the stop in force, Stops every stop; RoundMax is the owner's
	// backstop, 0 for the policy's, counted from round RoundBase.
	Rounds    []RoundRecord
	RoundStop *RoundStop
	Stops     []RoundStop
	RoundMax  int
	RoundBase int
}

// RulingRecord is a ruling and when it was made.
type RulingRecord struct {
	CaseRuling
	At time.Time `json:"at"`
}

// applyCase folds a case event. A case is changed as a copy, since a
// worker's state shares the loop's cases.
func (st *LogState) applyCase(e Event) {
	p := e.Case
	c := Case{ID: p.Case, First: e.Time}
	if old := st.Cases[p.Case]; old != nil {
		c = *old
	}
	if s := e.Session; s != "" && !contains(c.Sessions, s) {
		c.Sessions = append(c.Sessions[:len(c.Sessions):len(c.Sessions)], s)
	}
	switch {
	case p.Pack != nil:
		c.Pack, c.PackAt = *p.Pack, e.Time
		c.Packs++
		// A launch is a fresh context: what the session acknowledged
		// before, it has not seen, and acknowledges again.
		if _, ok := c.Acked[e.Session]; ok {
			acked := make(map[string][]string, len(c.Acked))
			for k, v := range c.Acked {
				if k != e.Session {
					acked[k] = v
				}
			}
			c.Acked = acked
		}
	case p.Report != nil:
		c.Report, c.ReportAt = *p.Report, e.Time
		reports := make(map[string]string, len(c.Reports)+1)
		for k, v := range c.Reports {
			reports[k] = v
		}
		reports[e.Session] = p.Report.Hash
		c.Reports = reports
	case p.Ruling != nil:
		r := RulingRecord{CaseRuling: *p.Ruling, At: e.Time}
		c.Rulings = append(c.Rulings[:len(c.Rulings):len(c.Rulings)], r)
		switch r.Kind {
		case RulingRestriction, RulingWaiver:
			c.Active = append(c.Active[:len(c.Active):len(c.Active)], r)
		case RulingClose:
			// One-shot rulings expire when the case closes.
			c.Closed, c.Active = true, nil
		case RulingReopen:
			c.Closed = false
		}
	case p.Goal != nil:
		c.applyGoal(p.Goal)
	case p.Rounds != nil:
		c.applyRounds(p.Rounds)
	case p.Call != nil:
		c.applyCall(e)
		if d := p.Call.Raised; d != nil && e.Session != "" {
			st.track(e.Session).raise(*d)
		}
	}
	st.Cases[p.Case] = &c
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// caseOf is the id of the case the session works, or "" when it names no
// issue.
func (c *Config) caseOf(sess Session) string {
	repo, ok := c.Repositories[sess.Repository]
	if sess.Issue <= 0 || !ok {
		return ""
	}
	return caseID(repoKey(repo.Forge, repo.Project), sess.Issue)
}

// caseID is the case of issue n of the repository with key repo: the id of
// the issue's item.
func caseID(repo string, n int) string {
	return repo + "#" + strconv.Itoa(n)
}

// splitCase is a case id's repository key and issue number.
func splitCase(id string) (string, int, bool) {
	i := strings.LastIndexByte(id, '#')
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(id[i+1:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return id[:i], n, true
}

// notePack logs the context pack a launch handed the session, when it
// works a case. The facts are as typed, unscreened, so their hash is the
// prompt's; the launch's act event logs the same text in its command.
func (s *Supervisor) notePack(name string, sess Session, facts []byte) {
	id := s.cfg.caseOf(sess)
	if id == "" {
		return
	}
	s.emit(name, Event{Kind: KindCase, Case: &CasePayload{Case: id, Pack: &CasePack{Hash: shortHash(HashPane(string(facts))), Facts: facts}}})
}

// noteReport logs the session's progress as its case's report when it
// changed since the session last reported.
func (s *Supervisor) noteReport(name string, sess Session, ev Evidence) {
	id := s.cfg.caseOf(sess)
	if id == "" || !ev.Status.Exists {
		return
	}
	p := ev.Status.Progress
	r := CaseReport{Item: s.withheld(p.Item), State: s.withheld(p.State)}
	for _, n := range p.Next {
		r.Next = append(r.Next, s.withheld(n))
	}
	b, _ := json.Marshal(r)
	r.Hash = shortHash(HashPane(string(b)))
	if c := s.st.Cases[id]; c != nil && c.Reports[name] == r.Hash {
		return
	}
	s.emit(name, Event{Kind: KindCase, Case: &CasePayload{Case: id, Report: &r}})
}

// RecordRuling logs the owner's ruling on a case: a known case (one the
// log, a configured session or an issue item names), a known kind, the
// text a restriction or waiver needs, the demo line or "N/A: reason" a
// close needs, with its record, and nothing a closed case cannot take. A
// restriction or waiver gets the next id of the case's life. The id is
// read from st, folded before the append without a lock held across both,
// so two rulings recorded at the same moment could share an id.
func RecordRuling(log *Log, cfg *Config, st *LogState, now time.Time, id, kind, text, by string) error {
	id = strings.ToLower(id)
	known := st.Cases[id] != nil
	for _, sess := range cfg.Sessions {
		known = known || cfg.caseOf(sess) == id
	}
	if it := st.Items[id]; it != nil && it.Kind == ItemIssue && !it.OwnerOnly {
		known = true
	}
	if !known {
		return fmt.Errorf("no case %q", id)
	}
	c := st.Cases[id]
	if c == nil {
		c = &Case{}
	}
	r := CaseRuling{Kind: kind, By: by}
	switch kind {
	case RulingRestriction, RulingWaiver:
		text = strings.TrimSpace(text)
		if text == "" || utf8.RuneCountInString(text) > maxRuling || !utf8.ValidString(text) {
			return fmt.Errorf("a %s needs a text of at most %d characters", kind, maxRuling)
		}
		if c.Closed {
			return fmt.Errorf("case %s is closed; reopen it first", id)
		}
		n := 1
		for _, old := range c.Rulings {
			if old.ID != "" {
				n++
			}
		}
		r.ID, r.Text = "R"+strconv.Itoa(n), text
	case RulingClose:
		if c.Closed {
			return fmt.Errorf("case %s is already closed", id)
		}
		rec, err := closeRecord(c, text)
		if err != nil {
			return err
		}
		r.Close = rec
	case RulingReopen:
		if text != "" {
			return fmt.Errorf("a %s takes no text", kind)
		}
		if !c.Closed {
			return fmt.Errorf("case %s is open", id)
		}
	default:
		return fmt.Errorf("unknown ruling %q: restriction, waiver, close or reopen", kind)
	}
	return log.Append(Event{V: EventVersion, Time: now, Kind: KindCase, Mode: ModeOwner, Case: &CasePayload{Case: id, Ruling: &r}})
}

// caseState is what the facts say of a case: closed by the owner; waiting
// on the owner when its review rounds parked or any of its sessions waits;
// stopped by policy; delivered
// when a session's work went out; working while a session runs or is done
// and not yet delivered; else not started.
func caseState(c *Case, ts []*Track) string {
	if c != nil && c.Closed {
		return CaseClosed
	}
	if c != nil && c.roundsParked() {
		return CaseNeedsYou
	}
	var stopped, delivered, working bool
	for _, t := range ts {
		switch {
		case t.State == BlockedOnOwner || t.State == CrashLooping || t.Parked || t.Questions.Open:
			return CaseNeedsYou
		case t.Stopped != "":
			stopped = true
		case len(t.Deliveries) > 0:
			delivered = true
		case t.Live || t.State != "":
			working = true
		}
	}
	switch {
	case stopped:
		return CaseStopped
	case delivered:
		return CaseDelivered
	case working:
		return CaseWorking
	}
	return CaseNotStarted
}

// CaseView is a case as agentd cases and the page show it.
type CaseView struct {
	ID         string    `json:"id"`
	Repository string    `json:"repository"` // the forge and project, as items name it
	Issue      int       `json:"issue"`
	Title      string    `json:"title,omitempty"`
	State      string    `json:"state"`
	Since      time.Time `json:"since"` // waiting since: the state's start, or the case's first fact
	Sessions   []string  `json:"sessions,omitempty"`
	Item       string    `json:"item,omitempty"`
	ItemState  string    `json:"item_state,omitempty"`
	Next       []string  `json:"next,omitempty"`
	// Unverified marks Item, ItemState and Next as STATUS.md's, which
	// agentd parses but cannot check; Progress is the checked report.
	Unverified bool            `json:"unverified,omitempty"`
	Goal       []GoalItem      `json:"goal,omitempty"`
	Progress   *ProgressRecord `json:"progress,omitempty"`
	Drift      []DriftFlag     `json:"drift,omitempty"`
	Steps      []StepRecord    `json:"steps,omitempty"`
	Refused    int             `json:"refused,omitempty"`
	Pack       string          `json:"pack,omitempty"` // the last context pack's hash
	Packs      int             `json:"packs,omitempty"`
	Active     []RulingRecord  `json:"active,omitempty"`
	Urgency    string          `json:"urgency,omitempty"` // the issue item's triage
	Rank       int             `json:"rank"`
	Pinned     bool            `json:"pinned,omitempty"`
	Priority   int             `json:"priority,omitempty"` // the repository's this week, 0 unlisted
	// CI is the pipeline of each request its sessions delivered, as the
	// watcher last saw it.
	CI []CIRecord `json:"ci,omitempty"`
	// Rounds is its review rounds: the count, the backstop, the findings
	// awaiting the judge, the stop and the lessons proposed.
	Rounds *RoundsView `json:"rounds,omitempty"`
}

// CasesFrom is every case the state knows, sorted by id: the cases of the
// log, the configured sessions' cases, and the issues new items name.
func CasesFrom(cfg *Config, st *LogState) []CaseView {
	sessions := map[string][]string{}
	for _, name := range cfg.SessionNames() {
		if id := cfg.caseOf(cfg.Sessions[name]); id != "" {
			sessions[id] = append(sessions[id], name)
		}
	}
	ids := map[string]bool{}
	for id := range st.Cases {
		ids[id] = true
	}
	for id := range sessions {
		ids[id] = true
	}
	for id, it := range st.Items {
		if _, _, ok := splitCase(id); ok && it.Kind == ItemIssue && !it.OwnerOnly {
			ids[id] = true
		}
	}
	out := make([]CaseView, 0, len(ids))
	for id := range ids {
		repo, n, ok := splitCase(id)
		if !ok {
			continue
		}
		v := CaseView{ID: id, Repository: repo, Issue: n, Sessions: append([]string(nil), sessions[id]...)}
		c := st.Cases[id]
		if c != nil {
			for _, s := range c.Sessions {
				if !contains(v.Sessions, s) {
					v.Sessions = append(v.Sessions, s)
				}
			}
			v.Since = c.First
			v.Item, v.ItemState, v.Next = c.Report.Item, c.Report.State, c.Report.Next
			v.Unverified = c.Report.Hash != ""
			v.Goal, v.Progress, v.Drift, v.Steps, v.Refused = c.Goal, c.Progress, c.Drift, c.Steps, c.Refused
			v.Pack, v.Packs = c.Pack.Hash, c.Packs
			v.Active = c.Active
			v.Rounds = roundsView(c, cfg.Policies.Rounds)
		}
		if it := st.Items[id]; it != nil {
			v.Title = it.Title
			if v.Since.IsZero() || it.FirstSeen.Before(v.Since) {
				v.Since = it.FirstSeen
			}
			if it.Triage != nil && it.Triage.Hash == ItemHash(it.ItemPayload) {
				v.Urgency = it.Triage.Urgency
			}
		}
		// The state is the sessions' that work the case now: a session
		// moved to another issue stays listed, and no longer speaks for it.
		var ts []*Track
		for _, s := range sessions[id] {
			if t := st.Tracks[s]; t != nil {
				ts = append(ts, t)
			}
		}
		v.State = caseState(c, ts)
		for _, s := range v.Sessions {
			for _, r := range st.CI {
				if r.Session == s {
					if t := st.Tracks[s]; t != nil && r.State != CIMerged && r.State != CIClosed && t.ciStale(r) {
						r.State, r.Verdict, r.Jobs, r.Cause, r.Excerpt = CIUnknown, "", nil, "", ""
					}
					v.CI = append(v.CI, r)
				}
			}
		}
		sort.Slice(v.CI, func(i, j int) bool { return v.CI[i].Ref < v.CI[j].Ref })
		if v.State == CaseNeedsYou {
			// Waiting on the owner since the first of its sessions to wait.
			var since time.Time
			for _, t := range ts {
				at := time.Time{}
				switch {
				case t.State == BlockedOnOwner || t.State == CrashLooping || t.Parked:
					at = t.Since
				case t.Questions.Open:
					at = t.AskedAt
				}
				if !at.IsZero() && (since.IsZero() || at.Before(since)) {
					since = at
				}
			}
			if c != nil && c.roundsParked() {
				for _, r := range c.Rounds {
					if r.N == c.RoundStop.Round && (since.IsZero() || r.At.Before(since)) {
						since = r.At
					}
				}
			}
			if !since.IsZero() {
				v.Since = since
			}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Priorities is the tracking system's weekly review: each repository's
// priority this week (1 first) and the owner's pinned cases, in order.
type Priorities struct {
	Week         string         `json:"week"`
	Repositories []RepoPriority `json:"repositories"`
	Pins         []string       `json:"pins"`
}

// RepoPriority is one repository's priority, the repository named by its
// forge and project as items name it.
type RepoPriority struct {
	Repository string `json:"repository"`
	Priority   int    `json:"priority"`
}

// LoadPriorities reads the weekly priorities file. None configured, or none
// written yet this week, is no priorities; a file of a shape agentd does
// not know is an error naming it.
func LoadPriorities(path string) (*Priorities, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pr Priorities
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&pr); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, r := range pr.Repositories {
		if r.Repository == "" || r.Priority < 1 {
			return nil, fmt.Errorf("%s: repository %q needs a priority of 1 or more", path, r.Repository)
		}
	}
	for _, id := range pr.Pins {
		if _, _, ok := splitCase(id); !ok {
			return nil, fmt.Errorf("%s: pin %q is not a case such as gitlab/example-org/widgets#17", path, id)
		}
	}
	return &pr, nil
}

// urgencyOrder ranks a triage urgency; an untriaged case comes after all.
var urgencyOrder = map[string]int{"now": 0, "today": 1, "later": 2}

// RankCases orders the cases and numbers them from 1: the owner's pins
// first, in pin order; then what needs the owner; then, with priorities,
// the repository's priority this week, listed before unlisted; then triage
// urgency, the oldest waiting (a case with no time yet after the dated),
// and the id. Closed cases come last. Without
// priorities there are no pins and no repository order.
func RankCases(cs []CaseView, pr *Priorities) {
	pin, prio := map[string]int{}, map[string]int{}
	if pr != nil {
		for i, id := range pr.Pins {
			id = strings.ToLower(id)
			if _, ok := pin[id]; !ok {
				pin[id] = i + 1
			}
		}
		for _, r := range pr.Repositories {
			prio[strings.ToLower(r.Repository)] = r.Priority
		}
	}
	for i := range cs {
		cs[i].Pinned = pin[cs[i].ID] > 0
		cs[i].Priority = prio[cs[i].Repository]
	}
	urgency := func(c CaseView) int {
		if u, ok := urgencyOrder[c.Urgency]; ok {
			return u
		}
		return len(urgencyOrder)
	}
	priority := func(c CaseView) int {
		if c.Priority == 0 {
			return int(^uint(0) >> 1)
		}
		return c.Priority
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if ac, bc := a.State == CaseClosed, b.State == CaseClosed; ac != bc {
			return bc
		}
		if pa, pb := pin[a.ID], pin[b.ID]; pa != pb {
			return pa != 0 && (pb == 0 || pa < pb)
		}
		if an, bn := a.State == CaseNeedsYou, b.State == CaseNeedsYou; an != bn {
			return an
		}
		if pa, pb := priority(a), priority(b); pa != pb {
			return pa < pb
		}
		if ua, ub := urgency(a), urgency(b); ua != ub {
			return ua < ub
		}
		// A case with no time yet has waited for nothing: after the dated.
		if az, bz := a.Since.IsZero(), b.Since.IsZero(); az != bz {
			return bz
		}
		if !a.Since.Equal(b.Since) {
			return a.Since.Before(b.Since)
		}
		return a.ID < b.ID
	})
	for i := range cs {
		cs[i].Rank = i + 1
	}
}

// casesCmd is agentd cases: the ranked cases as a table or JSON, or, as
// agentd cases rule CASE KIND [TEXT], the owner's ruling on one.
func casesCmd(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("cases", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the cases as JSON")
	shadow := fs.Bool("shadow", false, "the cases as the shadow run saw them")
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	mode := ModeLive
	if *shadow {
		mode = ModeShadow
	}
	if fs.NArg() > 0 {
		goal := fs.Arg(0) == "goal" && fs.NArg() == 3
		rounds := fs.Arg(0) == "rounds" && fs.NArg() == 3
		if !goal && !rounds && (fs.Arg(0) != "rule" || fs.NArg() < 3 || fs.NArg() > 4) {
			return errors.New("usage: agentd cases rule CASE restriction|waiver|close|reopen [TEXT], agentd cases goal CASE FILE|-, or agentd cases rounds CASE N")
		}
		var body []byte
		if goal {
			if fs.Arg(2) == "-" {
				body, err = io.ReadAll(os.Stdin)
			} else {
				body, err = os.ReadFile(fs.Arg(2))
			}
			if err != nil {
				return err
			}
		}
		log, events, err := openLoaded(cfg.EventLog)
		if err != nil {
			return err
		}
		defer log.Close()
		st := NewLogState(cfg, mode).Fold(events)
		if goal {
			return RecordGoal(log, cfg, st, time.Now(), fs.Arg(1), string(body))
		}
		if rounds {
			n, err := strconv.Atoi(fs.Arg(2))
			if err != nil {
				return fmt.Errorf("rounds %q is not a number", fs.Arg(2))
			}
			return RecordRounds(log, cfg, st, time.Now(), fs.Arg(1), n)
		}
		return RecordRuling(log, cfg, st, time.Now(), fs.Arg(1), fs.Arg(2), fs.Arg(3), "owner")
	}
	events, _, err := ReadLog(cfg.EventLog)
	if err != nil {
		return err
	}
	pr, err := LoadPriorities(cfg.Tracking.Priorities)
	if err != nil {
		return err
	}
	cs := CasesFrom(cfg, NewLogState(cfg, mode).Fold(events))
	RankCases(cs, pr)
	if *asJSON {
		week := ""
		if pr != nil {
			week = pr.Week
		}
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		return enc.Encode(struct {
			Week  string     `json:"week,omitempty"`
			Cases []CaseView `json:"cases"`
		}{week, cs})
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RANK\tCASE\tSTATE\tSINCE\tSESSIONS\tITEM\tREPORT\tURGENCY\tRULINGS\tTITLE")
	for _, c := range cs {
		rank := strconv.Itoa(c.Rank)
		if c.Pinned {
			rank += " pinned"
		}
		var rulings []string
		for _, r := range c.Active {
			rulings = append(rulings, r.ID+" "+r.Kind)
		}
		item := c.Item
		if c.ItemState != "" {
			item += " (" + c.ItemState + ")"
		}
		if c.Unverified && item != "" {
			item += " unverified"
		}
		report := ""
		if p := c.Progress; p != nil {
			met := 0
			for _, it := range p.Items {
				if it.Status == StatusMet {
					met++
				}
			}
			report = fmt.Sprintf("%d/%d met", met, len(current(c.Goal)))
			if len(c.Drift) > 0 {
				report += fmt.Sprintf(", drift %d", len(c.Drift))
			}
		}
		if r := c.Rounds; r != nil {
			if report != "" {
				report += ", "
			}
			report += fmt.Sprintf("round %d/%d", r.Since, r.Max)
			if r.Stop != nil {
				report += " " + r.Stop.Reason
			} else if len(r.Open) > 0 {
				report += fmt.Sprintf(", %d to judge", len(r.Open))
			}
		}
		since := ""
		if !c.Since.IsZero() {
			since = c.Since.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", rank, c.ID, c.State, since, strings.Join(c.Sessions, ","),
			cutUTF8(item, 60), report, c.Urgency, strings.Join(rulings, ","), cutUTF8(c.Title, 60))
	}
	return tw.Flush()
}
