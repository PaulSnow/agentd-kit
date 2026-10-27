package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The state layer (#60): one card per topic that says what is true now.
// Curators (curator.go) write a card's content as a diff; Go checks the
// diff, applies it, versions the card and serves it. Every claim names the
// history records (history.go) it was drawn from.

// Card kinds.
const (
	CardIssue      = "issue"
	CardRequest    = "request"
	CardSession    = "session"
	CardRepository = "repository"
)

// Card is a topic's state.
type Card struct {
	Topic         string         `json:"topic"`
	Kind          string         `json:"kind"`
	Title         string         `json:"title,omitempty"`
	Status        string         `json:"status"`
	Summary       string         `json:"summary"`
	Src           []string       `json:"src,omitempty"` // what the status and summary were drawn from
	InForce       []CardRule     `json:"in_force"`
	Open          []CardQuestion `json:"open"`
	KnownProblems []CardProblem  `json:"known_problems"`
	Related       []string       `json:"related"`
	Updated       time.Time      `json:"updated"`
	Rev           int            `json:"rev"`
	// Seq is the last claim id given: ids are never reused on a card.
	Seq int `json:"seq"`
}

// CardRule is a rule in force.
type CardRule struct {
	ID         string   `json:"id"`
	Rule       string   `json:"rule"`
	Since      string   `json:"since,omitempty"`
	Src        []string `json:"src"`
	Confidence string   `json:"confidence,omitempty"`
}

// CardQuestion is a question open for the owner.
type CardQuestion struct {
	ID         string   `json:"id"`
	Question   string   `json:"question"`
	Options    []string `json:"options,omitempty"`
	AskedBy    string   `json:"asked_by,omitempty"`
	Src        []string `json:"src"`
	Confidence string   `json:"confidence,omitempty"`
}

// CardProblem is a known problem.
type CardProblem struct {
	ID         string   `json:"id"`
	Text       string   `json:"text"`
	Src        []string `json:"src"`
	Confidence string   `json:"confidence,omitempty"`
}

// Card limits. A card is at most CardMaxBytes of compact JSON, about 1k
// tokens of its prose.
const (
	CardMaxBytes   = 4000
	cardStatusMax  = 200
	cardSummaryMax = 600
	cardClaimMax   = 400
)

// cardKindOf is the kind of card topic has, or "" when it has none.
func cardKindOf(topic string) string {
	switch {
	case topic == TopicRepo:
		return CardRepository
	case strings.HasPrefix(topic, "#"):
		return CardIssue
	case strings.HasPrefix(topic, "!"):
		return CardRequest
	case strings.HasPrefix(topic, topicSession):
		return CardSession
	}
	return ""
}

// CardDiff is what a curator returns: the card's new title, status and
// summary when they change, and the claims added, changed or retired.
type CardDiff struct {
	Title         string         `json:"title,omitempty"`
	Status        string         `json:"status,omitempty"`
	Summary       string         `json:"summary,omitempty"`
	Src           []string       `json:"src,omitempty"`
	AddRules      []DiffClaim    `json:"add_rules,omitempty"`
	AddQuestions  []DiffClaim    `json:"add_questions,omitempty"`
	AddProblems   []DiffClaim    `json:"add_problems,omitempty"`
	Change        []DiffClaim    `json:"change,omitempty"`
	Retire        []DiffRetire   `json:"retire,omitempty"`
	Conflicts     []DiffConflict `json:"conflicts,omitempty"`
	RelatedAdd    []string       `json:"related_add,omitempty"`
	RelatedRemove []string       `json:"related_remove,omitempty"`
}

// DiffClaim is a claim added, or changed when it names an id.
type DiffClaim struct {
	ID         string   `json:"id,omitempty"`
	Text       string   `json:"text"`
	Options    []string `json:"options,omitempty"`
	AskedBy    string   `json:"asked_by,omitempty"`
	Src        []string `json:"src"`
	Confidence string   `json:"confidence,omitempty"`
}

// DiffRetire takes a claim off the card: superseded, answered or fixed.
type DiffRetire struct {
	ID  string   `json:"id"`
	Why string   `json:"why"`
	Src []string `json:"src"`
}

// DiffConflict is two sources that disagree: Go makes it an open question
// for the owner, and the rule it names stays in force until answered.
type DiffConflict struct {
	ID       string   `json:"id,omitempty"`
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Src      []string `json:"src"`
}

// cardSources says what a diff may cite: known is every record the
// history holds (and, for the repository's card, each topic card as
// card:TOPIC); fresh is the records new since the card's last curation,
// the only sources that can say why a rule or question leaves.
type cardSources struct {
	known func(id string) bool
	fresh map[string]bool
}

// applyDiff checks d against card, nil for a topic's first, and returns
// the next revision. A diff that changes or drops an in-force rule or an
// open question without a fresh source for why, cites a record that does
// not exist, or makes the card too large, is refused whole.
func applyDiff(card *Card, topic string, d CardDiff, src cardSources, now time.Time) (*Card, error) {
	next := &Card{Topic: topic, Kind: cardKindOf(topic), InForce: []CardRule{}, Open: []CardQuestion{}, KnownProblems: []CardProblem{}, Related: []string{}}
	if card != nil {
		b, _ := json.Marshal(card)
		next = &Card{}
		json.Unmarshal(b, next)
	}
	if next.Kind == "" {
		return nil, fmt.Errorf("topic %q has no card", topic)
	}
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	cite := func(what string, ids []string, needFresh bool) {
		if len(ids) == 0 {
			fail("%s has no src", what)
			return
		}
		fresh := false
		for _, id := range ids {
			if !src.known(id) {
				fail("%s cites %s, which history does not hold", what, id)
			}
			fresh = fresh || src.fresh[id]
		}
		if needFresh && !fresh {
			fail("%s cites no record new since the card's last revision, so nothing says why", what)
		}
	}
	confidence := func(what, c string) {
		if c != "" && c != "low" {
			fail("%s has confidence %q: only \"low\" or none", what, c)
		}
	}
	text := func(what, s string, max int) {
		if strings.TrimSpace(s) == "" {
			fail("%s is empty", what)
		} else if len(s) > max {
			fail("%s is %d bytes, over %d", what, len(s), max)
		}
	}
	newID := func(letter string) string {
		next.Seq++
		return letter + strconv.Itoa(next.Seq)
	}

	if d.Title != "" {
		next.Title = oneLine(d.Title, cardStatusMax)
	}
	if d.Status != "" || d.Summary != "" {
		cite("the status and summary", d.Src, false)
		if d.Status != "" {
			text("status", d.Status, cardStatusMax)
			next.Status = d.Status
		}
		if d.Summary != "" {
			text("summary", d.Summary, cardSummaryMax)
			next.Summary = d.Summary
		}
		next.Src = d.Src
	}
	if card == nil && (next.Status == "" || next.Summary == "") {
		fail("a first card needs a status and a summary")
	}
	for i, c := range d.AddRules {
		what := fmt.Sprintf("add_rules[%d]", i)
		text(what, c.Text, cardClaimMax)
		cite(what, c.Src, false)
		confidence(what, c.Confidence)
		next.InForce = append(next.InForce, CardRule{ID: newID("R"), Rule: c.Text, Since: now.UTC().Format(time.RFC3339), Src: c.Src, Confidence: c.Confidence})
	}
	for i, c := range d.AddQuestions {
		what := fmt.Sprintf("add_questions[%d]", i)
		text(what, c.Text, cardClaimMax)
		cite(what, c.Src, false)
		confidence(what, c.Confidence)
		next.Open = append(next.Open, CardQuestion{ID: newID("Q"), Question: c.Text, Options: c.Options, AskedBy: c.AskedBy, Src: c.Src, Confidence: c.Confidence})
	}
	for i, c := range d.AddProblems {
		what := fmt.Sprintf("add_problems[%d]", i)
		text(what, c.Text, cardClaimMax)
		cite(what, c.Src, false)
		confidence(what, c.Confidence)
		next.KnownProblems = append(next.KnownProblems, CardProblem{ID: newID("K"), Text: c.Text, Src: c.Src, Confidence: c.Confidence})
	}
	for _, c := range d.Change {
		what := "change " + c.ID
		text(what, c.Text, cardClaimMax)
		confidence(what, c.Confidence)
		found := false
		for i := range next.InForce {
			if r := &next.InForce[i]; r.ID == c.ID {
				cite(what+", an in-force rule", c.Src, true)
				r.Rule, r.Src, r.Confidence, found = c.Text, c.Src, c.Confidence, true
			}
		}
		for i := range next.Open {
			if q := &next.Open[i]; q.ID == c.ID {
				cite(what+", an open question", c.Src, true)
				q.Question, q.Src, q.Confidence, found = c.Text, c.Src, c.Confidence, true
				if c.Options != nil {
					q.Options = c.Options
				}
			}
		}
		for i := range next.KnownProblems {
			if p := &next.KnownProblems[i]; p.ID == c.ID {
				cite(what, c.Src, false)
				p.Text, p.Src, p.Confidence, found = c.Text, c.Src, c.Confidence, true
			}
		}
		if !found {
			fail("change names %s, which the card does not hold", c.ID)
		}
	}
	for _, r := range d.Retire {
		what := "retire " + r.ID
		n := len(next.InForce) + len(next.Open) + len(next.KnownProblems)
		if strings.TrimSpace(r.Why) == "" {
			fail("%s gives no why", what)
		}
		if i := ruleIndex(next.InForce, r.ID); i >= 0 {
			cite(what+", an in-force rule", r.Src, true)
			next.InForce = append(next.InForce[:i], next.InForce[i+1:]...)
		}
		if i := questionIndex(next.Open, r.ID); i >= 0 {
			cite(what+", an open question", r.Src, true)
			next.Open = append(next.Open[:i], next.Open[i+1:]...)
		}
		if i := problemIndex(next.KnownProblems, r.ID); i >= 0 {
			cite(what, r.Src, false)
			next.KnownProblems = append(next.KnownProblems[:i], next.KnownProblems[i+1:]...)
		}
		if n == len(next.InForce)+len(next.Open)+len(next.KnownProblems) {
			fail("retire names %s, which the card does not hold", r.ID)
		}
	}
	for i, c := range d.Conflicts {
		what := fmt.Sprintf("conflicts[%d]", i)
		text(what, c.Question, cardClaimMax)
		cite(what, c.Src, false)
		if c.ID != "" && ruleIndex(next.InForce, c.ID) < 0 {
			fail("%s names rule %s, which is not in force", what, c.ID)
		}
		opts := c.Options
		if len(opts) < 2 {
			fail("%s gives %d options: a conflict has at least the two sides", what, len(opts))
		}
		src := c.Src
		if i := ruleIndex(next.InForce, c.ID); i >= 0 {
			src = append(append([]string{}, next.InForce[i].Src...), c.Src...)
		}
		next.Open = append(next.Open, CardQuestion{ID: newID("Q"), Question: c.Question, Options: opts, AskedBy: "curator: sources disagree", Src: dedup(src)})
	}
	for _, t := range d.RelatedRemove {
		if i := indexOf(next.Related, t); i >= 0 {
			next.Related = append(next.Related[:i], next.Related[i+1:]...)
		}
	}
	for _, t := range d.RelatedAdd {
		if t != topic && !contains(next.Related, t) {
			next.Related = append(next.Related, t)
		}
	}
	sort.Strings(next.Related)
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	next.Rev++
	next.Updated = now.UTC()
	if n := len(compact(next)); n > CardMaxBytes {
		return nil, fmt.Errorf("the card would be %d bytes, over %d: retire what no longer holds, or say it shorter", n, CardMaxBytes)
	}
	return next, nil
}

func ruleIndex(rs []CardRule, id string) int {
	for i, r := range rs {
		if r.ID == id {
			return i
		}
	}
	return -1
}

func questionIndex(qs []CardQuestion, id string) int {
	for i, q := range qs {
		if q.ID == id {
			return i
		}
	}
	return -1
}

func problemIndex(ps []CardProblem, id string) int {
	for i, p := range ps {
		if p.ID == id {
			return i
		}
	}
	return -1
}

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

func dedup(list []string) []string {
	var out []string
	for _, s := range list {
		if !contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// cardText is what find matches a card by.
func (c *Card) text() string {
	parts := []string{c.Topic, c.Title, c.Status, c.Summary}
	for _, r := range c.InForce {
		parts = append(parts, r.Rule)
	}
	for _, q := range c.Open {
		parts = append(parts, q.Question)
	}
	for _, p := range c.KnownProblems {
		parts = append(parts, p.Text)
	}
	return strings.Join(parts, "\n")
}
