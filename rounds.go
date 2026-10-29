package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Review rounds (#3 §1 and §3): reviewers advise, one party decides. A
// reviewer session reports its findings with review_findings and a judge
// session disposes each one with judge_dispositions; each call is a case
// event like any other tool call, so the rounds are a projection of the
// log. agentd numbers findings F1, F2… over the case's life and keeps an
// id across rounds. A round that brings no new finding stops the loop; a
// round count the owner sets is only the backstop, and only the owner's
// record resets it. Every stop proposes lessons, and a close is refused
// while a finding has no disposition.

// Session roles in a case's rounds.
const (
	RoleReviewer = "reviewer"
	RoleJudge    = "judge"
)

// Round tool names.
const (
	ToolReviewFindings    = "review_findings"
	ToolJudgeDispositions = "judge_dispositions"
)

// Dispositions.
const (
	DispositionAdopt   = "adopt"
	DispositionDecline = "decline"
	DispositionDefer   = "defer"
)

// Why a loop stopped. Repeating and backstop park the case for the owner.
const (
	RoundsConverged = "converged" // a round brought no findings at all
	RoundsRepeating = "repeating" // every finding of a round came back
	RoundsBackstop  = "backstop"  // the rounds since the owner's reset reached the count
)

// The option keys of a judge's decision for a finding it defers, each
// with its fixed meaning: a fix round, then review of the new head; the
// finding accepted as it is, so the round converges; or converge and file
// a follow-up issue on the forge.
const (
	FindingFix   = "fix"
	FindingMerge = "merge"
	FindingIssue = "issue"
)

var findingKeys = []string{FindingFix, FindingMerge, FindingIssue}

const findingKeysText = "fix, merge and issue"

// Finding severities.
var severities = []string{"high", "medium", "low"}

// Round limits.
const (
	maxRounds       = 20
	maxFindings     = 30
	maxFindingTitle = 200
	maxFindingText  = 600
	maxFindingPaths = 10
	maxReason       = 500
	maxPlan         = 30
	maxPlanStep     = 300
	maxLessons      = 5
	maxLesson       = 300
	// repeatRounds is how many rounds in a row a finding comes back
	// before it parks the case.
	repeatRounds = 3
)

// RoundsPolicy is the backstop a case's rounds have until the owner sets
// one: at most Max rounds since the last reset.
type RoundsPolicy struct {
	Max int `json:"max"`
}

// Finding is one review finding, its text screened. It names the
// done-when items it bears on, or says it widens the scope.
type Finding struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Severity    string   `json:"severity"`
	Items       []string `json:"items,omitempty"`
	WidensScope bool     `json:"widens_scope,omitempty"`
	Paths       []string `json:"paths,omitempty"`
	Consequence string   `json:"consequence"`
	Suggestion  string   `json:"suggestion,omitempty"`
	// Key is a hash of the title and paths as the reviewer gave them,
	// before screening, so a finding given again matches even when its
	// text was withheld.
	Key string `json:"key,omitempty"`
}

// CaseFindings is an accepted review_findings: round N's findings, the
// ids new to the case, and the stop agentd found when it accepted them.
type CaseFindings struct {
	Round    int        `json:"round"`
	Base     int        `json:"base"` // the round the owner last reset at, when checked
	Findings []Finding  `json:"findings"`
	New      []string   `json:"new"`
	Stop     *RoundStop `json:"stop,omitempty"`
}

// RoundStop is why and at which round the loop stopped, and the lessons
// proposed to the owner there.
type RoundStop struct {
	Reason  string   `json:"reason"`
	Round   int      `json:"round"`
	Lessons []string `json:"lessons,omitempty"`
}

// Disposition is the judge's word on one finding.
type Disposition struct {
	FindingID   string `json:"finding_id"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
}

// PlanStep is one step of the judge's plan, which the next fix and the
// next review check off by id.
type PlanStep struct {
	ID   string `json:"id"`
	Step string `json:"step"`
}

// CaseDispositions is an accepted judge_dispositions: one for each finding
// of round N, the plan and the lessons the judge proposes.
type CaseDispositions struct {
	Round        int           `json:"round"`
	Dispositions []Disposition `json:"dispositions"`
	Plan         []PlanStep    `json:"plan,omitempty"`
	Lessons      []string      `json:"lessons,omitempty"`
}

// CaseRounds is the owner's record: the backstop for the case's rounds,
// counted from now. It resets a stopped loop; nothing else does.
type CaseRounds struct {
	Max int `json:"max"`
}

// RoundRecord is one round as the case has it.
type RoundRecord struct {
	N            int           `json:"n"`
	Reviewer     string        `json:"reviewer"`
	Call         string        `json:"call"` // the review call's hash
	JudgeCall    string        `json:"judge_call,omitempty"`
	At           time.Time     `json:"at"`
	Findings     []Finding     `json:"findings"`
	New          []string      `json:"new"`
	Judge        string        `json:"judge,omitempty"`
	JudgedAt     time.Time     `json:"judged_at,omitempty"`
	Dispositions []Disposition `json:"dispositions,omitempty"`
	Plan         []PlanStep    `json:"plan,omitempty"`
	Lessons      []string      `json:"lessons,omitempty"`
}

// CloseRecord is what a close records: each current done-when item and
// whose it is to mark, the demo line or N/A with its reason, and every
// lesson the case's rounds proposed.
type CloseRecord struct {
	Items   []CloseItem `json:"items"`
	Demo    string      `json:"demo,omitempty"`
	NA      string      `json:"na,omitempty"`
	Lessons []string    `json:"lessons,omitempty"`
}

// CloseItem is a done-when item at close: machine, met when its latest
// report met it with checked evidence, or the owner's to mark.
type CloseItem struct {
	ID  string `json:"id"`
	By  string `json:"by"`
	Met bool   `json:"met,omitempty"`
}

func (r *CloseRecord) check() error {
	if (r.Demo == "") == (r.NA == "") {
		return errors.New("a close record has a demo line or N/A with its reason")
	}
	return nil
}

// applyFindings, applyDispositions and applyRounds fold a round's events
// into c, copying what they change.
//
// Two calls can be checked against the same state before either is
// appended; the later is then not the next round, nor its dispositions the
// latest round's, or the owner reset the loop or closed the case between,
// and the fold ignores it. The server
// reads the log back after appending and tells that session so.
func (c *Case) applyFindings(e Event, f *CaseFindings) {
	if c.Closed || f.Base != c.RoundBase || f.Round != len(c.Rounds)+1 || len(c.Rounds) > 0 && len(c.Rounds[len(c.Rounds)-1].Findings) > 0 && c.Rounds[len(c.Rounds)-1].Dispositions == nil ||
		c.RoundStop != nil {
		return
	}
	r := RoundRecord{N: f.Round, Reviewer: e.Session, Call: e.Case.Call.Hash, At: e.Time, Findings: f.Findings, New: f.New}
	c.Rounds = append(c.Rounds[:len(c.Rounds):len(c.Rounds)], r)
	if f.Stop != nil {
		c.RoundStop = f.Stop
		c.Stops = append(c.Stops[:len(c.Stops):len(c.Stops)], *f.Stop)
	}
}

func (c *Case) applyDispositions(e Event, d *CaseDispositions) {
	i := len(c.Rounds) - 1
	if c.Closed || i < 0 || c.Rounds[i].N != d.Round || c.Rounds[i].Dispositions != nil || len(d.Dispositions) != len(c.Rounds[i].Findings) {
		return
	}
	for _, x := range d.Dispositions {
		if !hasFinding(c.Rounds[i].Findings, x.FindingID) {
			return
		}
	}
	rounds := append([]RoundRecord(nil), c.Rounds...)
	r := rounds[i]
	r.Judge, r.JudgeCall, r.JudgedAt, r.Dispositions, r.Plan, r.Lessons = e.Session, e.Case.Call.Hash, e.Time, d.Dispositions, d.Plan, d.Lessons
	rounds[i] = r
	c.Rounds = rounds
}

// findingDecision is the live decision session raised for finding id of
// round r, following the decisions that superseded it, or nil.
func (c *Case) findingDecision(session string, r *RoundRecord, id string) *RaisedRecord {
	var live *RaisedRecord
	for i := range c.Decisions {
		d := &c.Decisions[i]
		if d.Session == session && d.Finding == id && d.SupersededBy == "" && !d.At.Before(r.At) {
			live = d
		}
	}
	return live
}

// deferral is a finding a round deferred to the owner: its decision,
// nil in a log from before a defer needed one, the decision's console
// entry, and the owner's answer, a finding key, or "" while it waits,
// with the owner's words on it.
type deferral struct {
	Finding  Finding
	Decision *RaisedRecord
	Entry    string
	Answer   string
	Words    string
}

// deferred is round r's deferrals, read from the log's folds alone, so a
// restart reads the same: an answer stands when it is a finding key the
// owner chose for the decision as it is; one in the owner's own words
// waits. A round at or before the owner's reset defers nothing more: the
// owner took the rounds up.
func (c *Case) deferred(st *LogState, r *RoundRecord) []deferral {
	if r == nil || r.Dispositions == nil || r.N <= c.RoundBase {
		return nil
	}
	var out []deferral
	for _, d := range r.Dispositions {
		if d.Disposition != DispositionDefer {
			continue
		}
		x := deferral{}
		for _, f := range r.Findings {
			if f.ID == d.FindingID {
				x.Finding = f
			}
		}
		if x.Decision = c.findingDecision(r.Judge, r, d.FindingID); x.Decision != nil {
			x.Entry = decisionEntry(x.Decision.Session, x.Decision.ID)
			if t := st.Tracks[x.Decision.Session]; t != nil {
				if a, ok := t.answerTo(x.Decision.Decision); ok && contains(findingKeys, a.Choice) {
					x.Answer, x.Words = a.Choice, a.Note
				}
			}
		}
		out = append(out, x)
	}
	return out
}

// ownerConverged says whether the owner settled every finding of the
// case's latest round that was not declined: each deferred, and answered
// merge or issue.
func (c *Case) ownerConverged(st *LogState) bool {
	n := len(c.Rounds)
	if n == 0 || adopts(&c.Rounds[n-1]) {
		return false
	}
	ds := c.deferred(st, &c.Rounds[n-1])
	for _, d := range ds {
		if d.Answer != FindingMerge && d.Answer != FindingIssue {
			return false
		}
	}
	return len(ds) > 0
}

// reviewConverged says whether the case's rounds converged: a round
// brought no findings, or the owner settled the latest one.
func (c *Case) reviewConverged(st *LogState) bool {
	return c != nil && (c.RoundStop != nil && c.RoundStop.Reason == RoundsConverged || c.RoundStop == nil && !c.Closed && c.ownerConverged(st))
}

func hasFinding(fs []Finding, id string) bool {
	for _, f := range fs {
		if f.ID == id {
			return true
		}
	}
	return false
}

// recorded says whether the fold took the call of round with hash h: a
// review that opened its round, or the dispositions of one.
func (c *Case) recorded(tool string, round int, h string) bool {
	for _, r := range c.Rounds {
		if r.N == round && (tool == ToolReviewFindings && r.Call == h || tool == ToolJudgeDispositions && r.JudgeCall == h) {
			return true
		}
	}
	return false
}

func (c *Case) applyRounds(r *CaseRounds) {
	c.RoundMax, c.RoundBase, c.RoundStop = r.Max, len(c.Rounds), nil
}

// findings is each finding the case has had, as its latest round gave it,
// in the order of its id.
func (c *Case) findings() []Finding {
	at := map[string]int{}
	var out []Finding
	for _, r := range c.Rounds {
		for _, f := range r.Findings {
			if i, ok := at[f.ID]; ok {
				out[i] = f
			} else {
				at[f.ID] = len(out)
				out = append(out, f)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return findingNumber(out[i].ID) < findingNumber(out[j].ID) })
	return out
}

func findingNumber(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "F"))
	return n
}

// Disposition is the finding's latest disposition, or nil when its
// latest round has none yet.
func (c *Case) Disposition(id string) *Disposition {
	var d *Disposition
	for _, r := range c.Rounds {
		in := false
		for _, f := range r.Findings {
			in = in || f.ID == id
		}
		if !in {
			continue
		}
		d = nil
		for i := range r.Dispositions {
			if r.Dispositions[i].FindingID == id {
				d = &r.Dispositions[i]
			}
		}
	}
	return d
}

// undisposed is the ids of findings whose round has no dispositions yet.
func (c *Case) undisposed() []string {
	var out []string
	for _, r := range c.Rounds {
		if r.Dispositions != nil {
			continue
		}
		for _, f := range r.Findings {
			if !contains(out, f.ID) {
				out = append(out, f.ID)
			}
		}
	}
	return out
}

// lessons is every lesson the rounds since round from proposed, judges'
// and stops', in order and once each.
func (c *Case) lessons(from int) []string {
	var out []string
	add := func(ls []string) {
		for _, l := range ls {
			if !contains(out, l) {
				out = append(out, l)
			}
		}
	}
	for _, r := range c.Rounds {
		if r.N > from {
			add(r.Lessons)
		}
	}
	if from == 0 {
		for _, s := range c.Stops {
			add(s.Lessons)
		}
	}
	return out
}

// backstop is the round count in force for the case.
func (c *Case) backstop(p RoundsPolicy) int {
	if c.RoundMax > 0 {
		return c.RoundMax
	}
	return p.Max
}

// checkFindings checks a review_findings against the case. It returns the
// findings to record and what to fix, if anything.
func (srv *MCPServer) checkFindings(args json.RawMessage, c *Case) (*CaseFindings, []problem) {
	if s := c.RoundStop; s != nil {
		return nil, []problem{{format: "the review loop stopped at round %d (%s); only the owner resets it, with agentd cases rounds %s N", args: []any{s.Round, s.Reason, srv.caseID}}}
	}
	if open := c.undisposed(); len(open) > 0 {
		return nil, []problem{{format: "round %d awaits the judge's dispositions of %s", args: []any{len(c.Rounds), strings.Join(open, ", ")}}}
	}
	var a struct {
		Findings []struct {
			ID          string   `json:"id"`
			Title       string   `json:"title"`
			Severity    string   `json:"severity"`
			Items       []string `json:"items"`
			WidensScope bool     `json:"widens_scope"`
			Paths       []string `json:"paths"`
			Consequence string   `json:"consequence"`
			Suggestion  string   `json:"suggestion"`
		} `json:"findings"`
	}
	if p := srv.decodeArgs(args, &a); p != nil {
		return nil, p
	}
	if len(a.Findings) > maxFindings {
		return nil, []problem{{format: "at most %d findings a round", args: []any{maxFindings}}}
	}
	known := c.findings()
	items := map[string]bool{}
	for _, it := range current(c.Goal) {
		items[it.ID] = true
	}
	next := len(known) + 1
	// The ids the reviewer names: a finding given without one never
	// takes one of these.
	var named []string
	for _, f := range a.Findings {
		if f.ID != "" {
			named = append(named, f.ID)
		}
	}
	var out []Finding
	var problems []problem
	var ids, fresh []string
	for i, f := range a.Findings {
		n := i + 1
		title, consequence, suggestion := strings.TrimSpace(f.Title), strings.TrimSpace(f.Consequence), strings.TrimSpace(f.Suggestion)
		bad := func(format string, args ...any) {
			problems = append(problems, problem{format: "finding %d: " + format, args: append([]any{n}, args...)})
		}
		if title == "" || utf8.RuneCountInString(title) > maxFindingTitle {
			bad("a title of 1 to %d characters", maxFindingTitle)
		}
		if !contains(severities, f.Severity) {
			bad("severity %q is not high, medium or low", cutUTF8(f.Severity, 40))
		}
		if consequence == "" || utf8.RuneCountInString(consequence) > maxFindingText || utf8.RuneCountInString(suggestion) > maxFindingText {
			bad("a consequence, and a suggestion if any, of at most %d characters", maxFindingText)
		}
		if len(f.Items) == 0 && !f.WidensScope {
			bad("names no done-when item; name the items it bears on, or set widens_scope")
		}
		for _, it := range f.Items {
			if !items[it] {
				bad("item %q is not one of the case's current done-when items", cutUTF8(it, 40))
			}
		}
		if len(f.Paths) > maxFindingPaths {
			bad("at most %d paths", maxFindingPaths)
		}
		for _, p := range f.Paths {
			if p == "" || len(p) > maxEvidencePath || filepath.IsAbs(p) || strings.HasPrefix(filepath.Clean(p), "..") {
				bad("path %q is not a path in the worktree", cutUTF8(p, 80))
			}
		}
		id := f.ID
		if id != "" {
			found := false
			for _, k := range known {
				found = found || k.ID == id
			}
			if !found {
				bad("names %s, which the case does not have; leave id out for a new finding", cutUTF8(id, 40))
				continue
			}
		} else {
			for _, k := range known {
				if contains(named, k.ID) || contains(ids, k.ID) {
					continue
				}
				_, like := alike(k.Title, title)
				if k.Key == findingKey(title, f.Paths) || samePaths(k.Paths, f.Paths) && (like || k.Title == title) {
					id = k.ID
					break
				}
			}
		}
		if id == "" {
			id = "F" + strconv.Itoa(next)
			next++
			fresh = append(fresh, id)
		}
		if contains(ids, id) {
			bad("%s is given twice", id)
			continue
		}
		ids = append(ids, id)
		paths := make([]string, len(f.Paths))
		for j, p := range f.Paths {
			paths[j] = srv.withheld(p)
		}
		out = append(out, Finding{ID: id, Title: srv.withheld(title), Severity: f.Severity, Items: f.Items, WidensScope: f.WidensScope,
			Paths: paths, Consequence: srv.withheld(consequence), Suggestion: srv.withheld(suggestion), Key: findingKey(title, f.Paths)})
	}
	if len(problems) > 0 {
		return nil, problems
	}
	rec := &CaseFindings{Round: len(c.Rounds) + 1, Base: c.RoundBase, Findings: out, New: append([]string{}, fresh...)}
	rec.Stop = srv.stopAt(c, rec)
	return rec, nil
}

// stopAt is the stop round f brings, if any: no new findings; a finding
// that keeps coming back, given in repeatRounds rounds in a row; or the
// last round the backstop allows. Its lessons are the judges' since the
// last reset and, at a park, what the facts show.
func (srv *MCPServer) stopAt(c *Case, f *CaseFindings) *RoundStop {
	var s *RoundStop
	var back []string // the findings in each of the last repeatRounds rounds
	if n := len(c.Rounds); n-c.RoundBase >= repeatRounds-1 {
		for _, x := range f.Findings {
			in := true
			for _, r := range c.Rounds[n-repeatRounds+1:] {
				in = in && hasFinding(r.Findings, x.ID)
			}
			if in {
				back = append(back, x.ID)
			}
		}
	}
	repo, _, _ := splitCase(c.ID)
	switch limit := c.backstop(srv.cfg.roundsFor(srv.st, repo)); {
	case len(f.Findings) == 0:
		s = &RoundStop{Reason: RoundsConverged}
	case len(f.New) == 0:
		var ids []string
		for _, x := range f.Findings {
			ids = append(ids, x.ID)
		}
		s = &RoundStop{Reason: RoundsRepeating, Lessons: []string{fmt.Sprintf(
			"Round %d brought nothing new: %s came back. The fixes did not hold, or the reviewer and the judge disagree; the owner decides.",
			f.Round, strings.Join(ids, ", "))}}
	case len(back) > 0:
		s = &RoundStop{Reason: RoundsRepeating, Lessons: []string{fmt.Sprintf(
			"%s came back in each of the last %d rounds: the fixes do not hold, or the reviewer and the judge disagree; the owner decides.",
			strings.Join(back, ", "), repeatRounds)}}
	case f.Round-c.RoundBase >= limit:
		s = &RoundStop{Reason: RoundsBackstop, Lessons: []string{fmt.Sprintf(
			"Round %d, the last of the %d the backstop allows since round %d, still brought %d new findings (%s): the work does not converge.",
			f.Round, limit, c.RoundBase, len(f.New), strings.Join(f.New, ", "))}}
	default:
		return nil
	}
	s.Round = f.Round
	s.Lessons = append(c.lessons(c.RoundBase), s.Lessons...)
	return s
}

// findingKey is a hash of a finding's title and paths as given.
func findingKey(title string, paths []string) string {
	ps := append([]string(nil), paths...)
	sort.Strings(ps)
	return shortHash(HashPane(strings.ToLower(title) + "\x00" + strings.Join(ps, "\x00")))
}

// samePaths says whether two findings name the same paths, in any order.
func samePaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// checkDispositions checks a judge_dispositions against the case's latest
// round: one disposition for each of its findings, with a reason, and none
// that adopts a finding widening the scope.
func (srv *MCPServer) checkDispositions(args json.RawMessage, c *Case) (*CaseDispositions, []problem) {
	if len(c.Rounds) == 0 || len(c.Rounds[len(c.Rounds)-1].Findings) == 0 {
		return nil, []problem{{format: "no round has findings to dispose"}}
	}
	r := c.Rounds[len(c.Rounds)-1]
	if r.Dispositions != nil {
		return nil, []problem{{format: "round %d is disposed; the next review opens the next round", args: []any{r.N}}}
	}
	var a struct {
		Dispositions []Disposition `json:"dispositions"`
		Plan         []PlanStep    `json:"plan"`
		Lessons      []string      `json:"lessons"`
	}
	if p := srv.decodeArgs(args, &a); p != nil {
		return nil, p
	}
	byID := map[string]Finding{}
	for _, f := range r.Findings {
		byID[f.ID] = f
	}
	var problems []problem
	bad := func(format string, args ...any) { problems = append(problems, problem{format: format, args: args}) }
	var seen []string
	out := &CaseDispositions{Round: r.N, Dispositions: []Disposition{}}
	for _, d := range a.Dispositions {
		id := cutUTF8(d.FindingID, 40)
		f, ok := byID[d.FindingID]
		switch {
		case !ok:
			bad("finding %s is not in round %d", id, r.N)
			continue
		case contains(seen, d.FindingID):
			bad("finding %s is disposed twice", id)
			continue
		}
		seen = append(seen, d.FindingID)
		reason := strings.TrimSpace(d.Reason)
		switch {
		case d.Disposition != DispositionAdopt && d.Disposition != DispositionDecline && d.Disposition != DispositionDefer:
			bad("finding %s: disposition %q is not adopt, decline or defer", id, cutUTF8(d.Disposition, 40))
		case reason == "" || utf8.RuneCountInString(reason) > maxReason:
			bad("finding %s needs a reason of at most %d characters", id, maxReason)
		case d.Disposition == DispositionAdopt && f.WidensScope:
			bad("finding %s widens the scope and cannot be adopted: defer it and ask the owner with raise_decision", id)
		case d.Disposition == DispositionDefer && c.findingDecision(srv.name, &r, d.FindingID) == nil:
			bad("finding %s is deferred but has no decision: raise it first with raise_decision (finding %s, choose_one with the option keys %s); "+
				"the question is at most %d characters, context, why, if_wrong and recommendation at most %d characters each", id, id, findingKeysText, maxQuestion, maxDecisionField)
		}
		out.Dispositions = append(out.Dispositions, Disposition{FindingID: d.FindingID, Disposition: d.Disposition, Reason: srv.withheld(reason)})
	}
	for _, f := range r.Findings {
		if !contains(seen, f.ID) {
			bad("finding %s has no disposition", f.ID)
		}
	}
	if len(a.Plan) > maxPlan {
		bad("at most %d plan steps", maxPlan)
	}
	var planIDs []string
	for _, p := range a.Plan {
		id, step := strings.TrimSpace(p.ID), strings.TrimSpace(p.Step)
		switch {
		case !planID.MatchString(id):
			bad("a plan step needs an id of 1 to 20 letters, digits, dots, dashes or underscores")
		case contains(planIDs, id):
			bad("plan id %s is given twice", id)
		case step == "" || utf8.RuneCountInString(step) > maxPlanStep:
			bad("plan step %s needs a step of at most %d characters", id, maxPlanStep)
		}
		planIDs = append(planIDs, id)
		out.Plan = append(out.Plan, PlanStep{ID: id, Step: srv.withheld(step)})
	}
	if len(a.Lessons) > maxLessons {
		bad("at most %d lessons", maxLessons)
	}
	for _, l := range a.Lessons {
		if l = strings.TrimSpace(l); l == "" || utf8.RuneCountInString(l) > maxLesson {
			bad("a lesson of 1 to %d characters", maxLesson)
			continue
		}
		out.Lessons = append(out.Lessons, srv.withheld(l))
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return out, nil
}

var planID = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,20}$`)

// naText is a close text that says N/A, any case, and its reason.
var naText = regexp.MustCompile(`(?is)^n/a\b[\s:.,;-]*(.*)$`)

// RecordRounds records the owner's backstop for a case's rounds, counted
// from now, which also resets a stopped loop.
func RecordRounds(log *Log, cfg *Config, st *LogState, now time.Time, id string, n int) error {
	id = strings.ToLower(id)
	if !knownCase(cfg, st, id) {
		return fmt.Errorf("no case %q", id)
	}
	c := st.Cases[id]
	if c == nil {
		c = &Case{}
	}
	if c.Closed {
		return fmt.Errorf("case %s is closed; reopen it first", id)
	}
	if n < 1 || n > maxRounds {
		return fmt.Errorf("a backstop is 1 to %d rounds", maxRounds)
	}
	return log.Append(Event{V: EventVersion, Time: now, Kind: KindCase, Mode: ModeOwner, Case: &CasePayload{Case: id, Rounds: &CaseRounds{Max: n}}})
}

// closeRecord is the record a close with text makes: text is the demo
// line, or "N/A: " and the reason. It is refused while a finding has no
// disposition.
func closeRecord(c *Case, text string) (*CloseRecord, error) {
	if open := c.undisposed(); len(open) > 0 {
		return nil, fmt.Errorf("finding %s has no disposition; the judge disposes every finding before a close", strings.Join(open, ", "))
	}
	r := &CloseRecord{}
	text = strings.TrimSpace(text)
	if m := naText.FindStringSubmatch(text); m != nil {
		r.NA = strings.TrimSpace(m[1])
		if r.NA == "" {
			return nil, errors.New(`N/A needs its reason: "N/A: why there is nothing to see"`)
		}
	} else if r.Demo = text; text == "" {
		return nil, errors.New(`a close needs a demo line saying how to see the change working, or "N/A: reason"`)
	}
	if utf8.RuneCountInString(text) > maxRuling || !utf8.ValidString(text) {
		return nil, fmt.Errorf("a demo line or N/A is at most %d characters", maxRuling)
	}
	met := map[string]bool{}
	if p := c.Progress; p != nil {
		for _, it := range p.Items {
			met[it.ID] = it.Status == StatusMet && len(it.Evidence) > 0
		}
	}
	r.Items = []CloseItem{}
	for _, it := range current(c.Goal) {
		ci := CloseItem{ID: it.ID, By: GoalOwner}
		if it.Kind == GoalMachine {
			ci.By, ci.Met = GoalMachine, met[it.ID]
		}
		r.Items = append(r.Items, ci)
	}
	r.Lessons = c.lessons(0)
	return r, nil
}

// RoundsFact is what a fresh session's facts say of its case's rounds:
// every finding with its latest disposition, the latest plan and the
// stop. The worker's own account is never here (#3 §2): a reviewer sees
// the work, not the worker's word on it.
type RoundsFact struct {
	Round    int           `json:"round"`
	Findings []FindingFact `json:"findings,omitempty"`
	Plan     []PlanStep    `json:"plan,omitempty"`
	Stopped  string        `json:"stopped,omitempty"`
}

// FindingFact is a finding, its latest disposition and, for one deferred
// to the owner, the owner's answer: fix, merge or issue, with the
// decision's question and the owner's words on it (#76).
type FindingFact struct {
	Finding
	Disposition string `json:"disposition,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Owner       string `json:"owner,omitempty"`
	Question    string `json:"question,omitempty"`
	Words       string `json:"words,omitempty"`
}

func roundsFact(c *Case, st *LogState) *RoundsFact {
	if c == nil || len(c.Rounds) == 0 {
		return nil
	}
	f := &RoundsFact{Round: len(c.Rounds)}
	owner := map[string]deferral{}
	for i := range c.Rounds {
		for _, d := range c.deferred(st, &c.Rounds[i]) {
			owner[d.Finding.ID] = d
		}
	}
	for _, x := range c.findings() {
		ff := FindingFact{Finding: x}
		if d := c.Disposition(x.ID); d != nil {
			ff.Disposition, ff.Reason = d.Disposition, d.Reason
			if o, ok := owner[x.ID]; ok && d.Disposition == DispositionDefer {
				ff.Owner = o.Answer
				if o.Answer != "" && o.Decision != nil {
					ff.Question, ff.Words = o.Decision.Question, o.Words
				}
			}
		}
		f.Findings = append(f.Findings, ff)
	}
	for i := len(c.Rounds) - 1; i >= 0; i-- {
		if c.Rounds[i].Dispositions != nil {
			f.Plan = c.Rounds[i].Plan
			break
		}
	}
	if c.RoundStop != nil {
		f.Stopped = c.RoundStop.Reason
	}
	return f
}

// RoundsView is a case's rounds as the owner sees them.
type RoundsView struct {
	Round   int        `json:"round"`
	Max     int        `json:"max"`
	Since   int        `json:"since"` // rounds counted toward the backstop
	Open    []string   `json:"open,omitempty"`
	Stop    *RoundStop `json:"stop,omitempty"`
	Lessons []string   `json:"lessons,omitempty"`
}

func roundsView(c *Case, p RoundsPolicy) *RoundsView {
	if c == nil || len(c.Rounds) == 0 && c.RoundMax == 0 {
		return nil
	}
	return &RoundsView{Round: len(c.Rounds), Max: c.backstop(p), Since: len(c.Rounds) - c.RoundBase, Open: c.undisposed(),
		Stop: c.RoundStop, Lessons: c.lessons(0)}
}

// roundsParked says whether the case's rounds wait on the owner.
func (c *Case) roundsParked() bool {
	return c.RoundStop != nil && c.RoundStop.Reason != RoundsConverged
}
