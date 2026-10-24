package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Decisions raised through the session's tools (#24, #3 section 2). A
// session raises a decision with raise_decision, checked like any call
// before it is accepted; agentd numbers it D1 on for that session and it
// joins the session's open decisions, answered on the page and delivered
// through answers.json like those of questions.json. An answer in the
// owner's own words keeps the decision open: the session raises it again
// with options drawn from it, superseding the old one. The default is
// applied only once a deadline the owner set on the question has passed;
// the session's own deadline is a suggestion shown to the owner.

// ToolRaiseDecision is the tool a session raises a decision with.
const ToolRaiseDecision = "raise_decision"

// defaultNote is the note an answer agentd applied carries, so the
// session knows the owner did not choose it.
const defaultNote = "the default, applied at the owner's deadline"

// RaisedRecord is a decision raised through the tools, as the case holds
// it: who raised it and when, and the decision that replaced it.
type RaisedRecord struct {
	Decision
	Session      string    `json:"session"`
	At           time.Time `json:"at"`
	SupersededBy string    `json:"superseded_by,omitempty"`
}

// DeadlinePayload is a deadline the owner set on one decision, as it was
// asked when the owner set it (Asked, its hash): a reworded question has
// none.
type DeadlinePayload struct {
	ID    string    `json:"id"`
	At    time.Time `json:"at"`
	Asked string    `json:"asked"`
}

// DeadlineRecord is a deadline as a Track holds it, with when it was set.
type DeadlineRecord struct {
	DeadlinePayload
	Set time.Time
}

// raise folds a decision the session raised: it joins the open
// decisions, replacing the one it supersedes. An id already open stands;
// the later raise of it is not taken.
func (t *Track) raise(d Decision) {
	for _, o := range t.Raised {
		if o.ID == d.ID {
			return
		}
	}
	t.RaisedCount++
	raised := make([]Decision, 0, len(t.Raised)+1)
	for _, o := range t.Raised {
		if o.ID != d.Supersedes || d.Supersedes == "" {
			raised = append(raised, o)
		}
	}
	t.Raised = append(raised, d)
}

// raiseOnCase folds the decision into the case's record.
func (c *Case) raiseOnCase(e Event, d Decision) {
	ds := make([]RaisedRecord, 0, len(c.Decisions)+1)
	for _, r := range c.Decisions {
		if r.Session == e.Session && r.ID == d.ID {
			return // the first raise of an id stands, as on the track
		}
		if d.Supersedes != "" && r.Session == e.Session && r.ID == d.Supersedes && r.SupersededBy == "" {
			r.SupersededBy = d.ID
		}
		ds = append(ds, r)
	}
	c.Decisions = append(ds, RaisedRecord{Decision: d, Session: e.Session, At: e.Time})
}

// deadline is the owner's deadline on d, when it was set on d as it is.
func (t *Track) deadline(d Decision) (DeadlineRecord, bool) {
	dl, ok := t.Deadlines[d.ID]
	return dl, ok && dl.Asked == decisionHash(d)
}

// RecordDeadline appends the owner's deadline on the session's decision
// id. Only an open, unanswered decision with a default takes one, since
// the default is what the deadline applies, and only a deadline still to
// come.
func RecordDeadline(log *Log, st *LogState, now time.Time, session, id string, at time.Time) error {
	t := st.Tracks[session]
	if t == nil {
		return fmt.Errorf("no session %q", session)
	}
	d, ok := t.decision(id)
	switch _, answered := t.answerTo(d); {
	case !ok:
		return fmt.Errorf("%s has no open decision %q", session, id)
	case d.Default == "":
		return fmt.Errorf("decision %s has no default, so a deadline would apply nothing; answer it", id)
	case answered:
		return fmt.Errorf("decision %s is answered already", id)
	case !at.After(now):
		return fmt.Errorf("the deadline %s has passed", at.Format(time.RFC3339))
	}
	return log.Append(Event{V: EventVersion, Time: now, Kind: KindDeadline, Mode: ModeOwner, Session: session,
		Deadline: &DeadlinePayload{ID: id, At: at.UTC(), Asked: decisionHash(d)}})
}

// applyDefaults records the default as the answer to each of the
// session's open decisions whose owner deadline has passed with no answer
// at all; an answer in the owner's own words is an answer, so its default
// is not applied over it. In shadow mode the answer is the shadow's
// alone, and it delivers nothing.
func (s *Supervisor) applyDefaults(name string, t *Track) {
	now := s.clock.Now()
	for _, d := range t.open() {
		dl, ok := t.deadline(d)
		if d.Default == "" || !ok || now.Before(dl.At) {
			continue
		}
		if a, answered := t.Answers[d.ID]; answered && a.Asked == decisionHash(d) {
			continue
		}
		a, err := defaultAnswer(d)
		if err != nil || checkAnswer(d, a) != nil {
			continue // checked when it was raised; a default that no longer fits waits on the owner
		}
		a.Note, a.Asked, a.ByDefault = defaultNote, decisionHash(d), true
		if s.emit(name, Event{Kind: KindAnswer, Answer: &a}); s.halt != nil {
			return
		}
	}
}

// raiseArgs is raise_decision's arguments: a decision without its id,
// which agentd gives.
type raiseArgs struct {
	Type           string           `json:"type"`
	Question       string           `json:"question"`
	Context        string           `json:"context"`
	Why            string           `json:"why"`
	IfWrong        string           `json:"if_wrong"`
	Recommendation string           `json:"recommendation"`
	Options        []DecisionOption `json:"options"`
	Limits         *DecisionLimits  `json:"limits"`
	Default        string           `json:"default"`
	Deadline       string           `json:"deadline"`
	Blocking       bool             `json:"blocking"`
	Supersedes     string           `json:"supersedes"`
}

// sameQuestion is a question as two raises are compared: case, spacing
// and closing punctuation aside.
func sameQuestion(q string) string {
	return strings.TrimRight(strings.Join(strings.Fields(strings.ToLower(q)), " "), "?.! ")
}

// checkRaise checks a raise_decision: a decision of the narrowest type
// with its whole form, not the same as one the session has open, and
// superseding only an open decision it raised. It returns the decision to
// record, its words screened, and whether they were withheld.
func (srv *MCPServer) checkRaise(args json.RawMessage, t *Track) (*Decision, bool, []problem) {
	var a raiseArgs
	if p := srv.decodeArgs(args, &a); p != nil {
		return nil, false, p
	}
	if t == nil {
		t = newTrack()
	}
	d := Decision{ID: fmt.Sprintf("D%d", t.RaisedCount+1), Type: a.Type, Question: a.Question, Context: a.Context, Why: a.Why, IfWrong: a.IfWrong,
		Recommendation: a.Recommendation, Options: a.Options, Limits: a.Limits, Default: a.Default, Deadline: a.Deadline, Blocking: a.Blocking, Supersedes: a.Supersedes}
	if err := checkDecision(d); err != nil {
		return nil, false, []problem{srv.refusal(err, d)}
	}
	open := 0
	for _, o := range t.Raised {
		// agentd's own CI decisions do not count against the session's.
		if !t.waits(o) || o.ID == d.Supersedes || ciRaisedID.MatchString(o.ID) {
			continue
		}
		open++
		if !o.Withheld && sameQuestion(o.Question) == sameQuestion(d.Question) {
			return nil, false, []problem{{format: "the question is the same as open decision %s; wait for its answer, or supersede it", args: []any{o.ID}}}
		}
	}
	if d.Supersedes != "" {
		found := false
		for _, o := range t.Raised {
			found = found || o.ID == d.Supersedes && t.waits(o) && !ciRaisedID.MatchString(o.ID)
		}
		if !found {
			return nil, false, []problem{{format: "supersedes %q: not a decision this session raised that still waits on the owner", args: []any{cutUTF8(d.Supersedes, 40)}}}
		}
	}
	if open >= maxDecisions {
		return nil, false, []problem{{format: "%d decisions are open; at most %d may be", args: []any{open, maxDecisions}}}
	}
	for _, text := range decisionTexts(d) {
		if srv.withheld(text) != text {
			w := withholdDecision(d)
			return &w, true, nil
		}
	}
	return &d, false, nil
}

// refusal is a check's error as a problem: the session is told it whole,
// and the log keeps its wording, each value the session gave that the
// error quotes left out, as for every refusal.
func (srv *MCPServer) refusal(err error, d Decision) problem {
	logged := err.Error()
	for _, v := range []string{d.Type, d.Default, d.Supersedes} {
		if v != "" {
			logged = strings.ReplaceAll(logged, fmt.Sprintf("%q", cutUTF8(v, 40)), "…")
		}
	}
	return problem{format: "%s", args: []any{err.Error()}, log: srv.withheld(cutUTF8(logged, 300))}
}

// withholdDecision is d with its words left out: its id, type, option
// keys and whether it blocks stay, so it can still be answered from the
// session's own screen. Its limits and default go too, since either may
// be what matched; with no default, no deadline applies one.
func withholdDecision(d Decision) Decision {
	w := Decision{ID: d.ID, Type: d.Type, Blocking: d.Blocking, Supersedes: d.Supersedes, Withheld: true}
	for _, o := range d.Options {
		w.Options = append(w.Options, DecisionOption{Key: o.Key})
	}
	return w
}
