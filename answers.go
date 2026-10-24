package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Owner answers (#12, #3 section 2). A session asks in its Questions
// section of STATUS.md, as every running session does, or raises decisions
// in questions.json in its workdir, in #3's raise_decision shape. The
// owner answers on the page; the answer is an owner event keyed by the
// decision id. agentd writes the session's answers to answers.json in its
// workdir and, in live mode, types one fixed line pointing at it, never
// the answer itself; a session gone is restarted with a prompt naming the
// file. Shadow mode records only.

// The files a session raises decisions in and reads its answers from, in
// its workdir.
const (
	QuestionsFile = "questions.json"
	AnswersFile   = "answers.json"
)

// Decision types: the narrowest that fits. A yes_no is answered true or
// false, a choose_one with one option's key, a choose_many with some of
// its keys, a number or a date with a value within its limits, a text in
// words.
const (
	DecisionYesNo      = "yes_no"
	DecisionChooseOne  = "choose_one"
	DecisionChooseMany = "choose_many"
	DecisionNumber     = "number"
	DecisionDate       = "date"
	DecisionText       = "text"
)

// dateLayout is how a date question's limits, default and answer are
// written.
const dateLayout = "2006-01-02"

// Limits on what a session may raise and the owner may answer, in
// characters.
const (
	maxDecisions     = 20
	maxQuestion      = 500
	maxDecisionField = 1000
	maxOptions       = 9
	maxOptionField   = 300
	maxAnswerText    = 2000
	maxNote          = 500
	maxQuestionsFile = 64 * 1024
)

// statusDecision is the id prefix of the decision a STATUS.md Questions
// section stands for: the prefix, then the section's hash, so an answer
// covers the questions as they were asked and a reworded section asks
// again.
const statusDecision = "status-"

// raisedID is the form of the ids agentd gives the decisions a session
// raises through its tools, D1 on, and directiveID that of the owner's
// rulings, R1 on; a session's own ids in questions.json take neither.
var (
	raisedID    = regexp.MustCompile(`^D[0-9]+$`)
	directiveID = regexp.MustCompile(`^R[0-9]+$`)
)

// Decision is one question a session raises, in raise_decision's shape:
// the decision form (context, why, what breaks if the call is wrong, the
// recommendation) and, for choose_one and choose_many, the options. The
// default is written as a string: yes or no, a key, keys joined by
// commas, a number or a date. Deadline is the session's suggestion, shown
// to the owner; only a deadline the owner sets applies the default.
type Decision struct {
	ID             string           `json:"id"`
	Type           string           `json:"type"`
	Question       string           `json:"question"`
	Context        string           `json:"context"`
	Why            string           `json:"why"`
	IfWrong        string           `json:"if_wrong"`
	Recommendation string           `json:"recommendation"`
	Options        []DecisionOption `json:"options,omitempty"`
	Limits         *DecisionLimits  `json:"limits,omitempty"`
	Default        string           `json:"default,omitempty"`
	Deadline       string           `json:"deadline,omitempty"`
	Blocking       bool             `json:"blocking"`
	// Supersedes is the open decision a raise replaces, one the owner
	// answered in their own words.
	Supersedes string `json:"supersedes,omitempty"`
	// Withheld is set by agentd, never read from the file: the texts
	// matched a confidential pattern and were left out of the log.
	Withheld bool `json:"withheld,omitempty"`
}

// DecisionLimits bound a number's value, a date's (as YYYY-MM-DD) or how
// many keys a choose_many takes. Either may be left out.
type DecisionLimits struct {
	Min json.RawMessage `json:"min,omitempty"`
	Max json.RawMessage `json:"max,omitempty"`
}

// MinText and MaxText are the limits as the page writes them.
func (l *DecisionLimits) MinText() string { return limitText(l.Min) }
func (l *DecisionLimits) MaxText() string { return limitText(l.Max) }

func limitText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// DecisionOption is one of a choose_one's or a choose_many's answers.
type DecisionOption struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Consequence string `json:"consequence"`
}

// QuestionsDoc is questions.json.
type QuestionsDoc struct {
	Decisions []Decision `json:"decisions"`
}

// DecisionPayload is a session's questions.json as agentd last read it,
// logged when it changes: its hash, and its decisions, or why the file was
// refused, in which case the decisions read before stand.
type DecisionPayload struct {
	Hash      string     `json:"hash"`
	Decisions []Decision `json:"decisions"`
	Refused   string     `json:"refused,omitempty"`
}

// AnswerPayload is the owner's answer to one decision, in the JSON the
// session receives: answer for a yes_no, choice for a choose_one, choices
// for a choose_many, value for a number or a date, text for a text, or
// other, the owner's own words instead, which answers but does not settle
// the decision. A note is context beside a binding answer.
type AnswerPayload struct {
	ID      string          `json:"id"`
	Answer  *bool           `json:"answer,omitempty"`
	Choice  string          `json:"choice,omitempty"`
	Choices []string        `json:"choices,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	Text    string          `json:"text,omitempty"`
	Other   string          `json:"other,omitempty"`
	Note    string          `json:"note,omitempty"`
	// From is the address the page was asked from, Asked the hash of the
	// decision as the owner answered it, and ByDefault says agentd applied
	// the default at the owner's deadline; none is sent on.
	From      string `json:"from,omitempty"`
	Asked     string `json:"asked,omitempty"`
	ByDefault bool   `json:"by_default,omitempty"`
}

// AnswerRecord is an answer as a session's Track holds it.
type AnswerRecord struct {
	AnswerPayload
	At        time.Time
	Delivered bool
}

// Binding says whether the answer settles its decision: anything but
// other.
func (a AnswerPayload) Binding() bool {
	return a.Answer != nil || a.Choice != "" || len(a.Choices) > 0 || len(a.Value) > 0 || a.Text != ""
}

// ValueText is a number's or a date's value as the page shows it.
func (a AnswerPayload) ValueText() string { return limitText(a.Value) }

var decisionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

// checkDecisions checks what a session raised: ids unique and plain,
// every field within its limit, and each question of the narrowest type,
// so a choose_one is not a yes_no and a text names no options.
func checkDecisions(ds []Decision) error {
	if len(ds) > maxDecisions {
		return fmt.Errorf("%d decisions; at most %d may be open", len(ds), maxDecisions)
	}
	seen := map[string]bool{}
	for _, d := range ds {
		switch {
		case !decisionID.MatchString(d.ID) || strings.HasPrefix(d.ID, statusDecision) || raisedID.MatchString(d.ID) || directiveID.MatchString(d.ID):
			return fmt.Errorf("decision id %q: use up to 32 letters, digits, - or _, not starting with %q, and not D or R and a number, which agentd gives", cutUTF8(d.ID, 40), statusDecision)
		case seen[d.ID]:
			return fmt.Errorf("decision %s is raised twice", d.ID)
		case d.Supersedes != "":
			return fmt.Errorf("decision %s: supersedes is for a decision raised through raise_decision", d.ID)
		}
		seen[d.ID] = true
		if err := checkDecision(d); err != nil {
			return err
		}
	}
	return nil
}

// optionMarker is an option a question names in its own words, (a) or
// a), and yesOrNo a question that asks for yes or no. Only the question is
// read, not its context, and a list counts only when it starts at its
// first marker, (a) and (b) or (1) and (2), so a sentence such as "the
// retry count is 3." is not taken for one.
var (
	optionMarker = regexp.MustCompile(`(?i)\(([a-e1-5])\)|(?:^|\s)([a-e])\)`)
	yesOrNo      = regexp.MustCompile(`(?i)\byes\s*(?:or|/)\s*no\s*\??\s*$`)
)

// checkDecision checks one decision's type against what it asks: options
// only for the types that list them, limits only for those that take
// them, a default the decision could be answered with, and no type wider
// than it needs. A refusal names the type to use.
func checkDecision(d Decision) error {
	long := func(s string, n int) bool { return utf8.RuneCountInString(s) > n }
	switch {
	case strings.TrimSpace(d.Question) == "" || long(d.Question, maxQuestion):
		return fmt.Errorf("decision %s: the question must be set and at most %d characters", d.ID, maxQuestion)
	case strings.TrimSpace(d.Context) == "" || strings.TrimSpace(d.Why) == "" || strings.TrimSpace(d.IfWrong) == "" || strings.TrimSpace(d.Recommendation) == "":
		return fmt.Errorf("decision %s: context, why, if_wrong and recommendation must all be set", d.ID)
	case long(d.Context, maxDecisionField) || long(d.Why, maxDecisionField) || long(d.IfWrong, maxDecisionField) || long(d.Recommendation, maxDecisionField):
		return fmt.Errorf("decision %s: context, why, if_wrong and recommendation are at most %d characters each", d.ID, maxDecisionField)
	case long(d.Default, maxOptionField) || long(d.Deadline, 40):
		return fmt.Errorf("decision %s: the default is at most %d characters, the deadline 40", d.ID, maxOptionField)
	}
	switch d.Type {
	case DecisionYesNo, DecisionText, DecisionNumber, DecisionDate:
		if len(d.Options) > 0 {
			return fmt.Errorf("decision %s: a %s question lists no options; use choose_one", d.ID, d.Type)
		}
	case DecisionChooseOne, DecisionChooseMany:
		if err := checkOptions(d); err != nil {
			return err
		}
	default:
		return fmt.Errorf("decision %s: type %q is not yes_no, choose_one, choose_many, number, date or text", d.ID, cutUTF8(d.Type, 40))
	}
	if d.Type == DecisionText {
		if yesOrNo.MatchString(d.Question) {
			return fmt.Errorf("decision %s: a text question answered yes or no is a yes_no; use yes_no", d.ID)
		}
		marks := map[string]bool{}
		for _, m := range optionMarker.FindAllStringSubmatch(d.Question, -1) {
			marks[strings.ToLower(m[1]+m[2])] = true
		}
		if marks["a"] && marks["b"] || marks["1"] && marks["2"] {
			return fmt.Errorf("decision %s: a text question that names its own options is a choice; use choose_one or choose_many", d.ID)
		}
		if d.Default != "" {
			return fmt.Errorf("decision %s: a text question has no default", d.ID)
		}
	}
	if err := checkLimits(d); err != nil {
		return err
	}
	if d.Deadline != "" {
		if _, err := parseDeadline(d.Deadline); err != nil {
			return fmt.Errorf("decision %s: the deadline is a time (RFC 3339) or a date (YYYY-MM-DD)", d.ID)
		}
	}
	if d.Default != "" {
		a, err := defaultAnswer(d)
		if err == nil {
			err = checkAnswer(d, a)
		}
		if err != nil {
			return fmt.Errorf("decision %s: the default %q is not an answer it takes: %v", d.ID, cutUTF8(d.Default, 40), err)
		}
	}
	return nil
}

// checkLimits checks a decision's limits: numbers for a number, dates for
// a date, counts within its options for a choose_many, and min not above
// max.
func checkLimits(d Decision) error {
	l := d.Limits
	if l == nil {
		return nil
	}
	if d.Type != DecisionNumber && d.Type != DecisionDate && d.Type != DecisionChooseMany {
		return fmt.Errorf("decision %s: a %s question takes no limits", d.ID, d.Type)
	}
	if len(l.Min) > 40 || len(l.Max) > 40 {
		return fmt.Errorf("decision %s: limits min and max are at most 40 characters each", d.ID)
	}
	var bounds []float64
	for _, raw := range []json.RawMessage{l.Min, l.Max} {
		if len(raw) == 0 {
			bounds = append(bounds, math.NaN())
			continue
		}
		v, err := limitValue(d, raw)
		if err != nil {
			return fmt.Errorf("decision %s: limits min and max are %s", d.ID, limitKind(d.Type))
		}
		bounds = append(bounds, v)
	}
	if bounds[0] > bounds[1] {
		return fmt.Errorf("decision %s: the limit min is above max", d.ID)
	}
	if d.Type == DecisionChooseMany && (bounds[0] < 1 || bounds[1] < 1 || bounds[0] > float64(len(d.Options)) || bounds[1] > float64(len(d.Options))) {
		return fmt.Errorf("decision %s: a choose_many's limits min and max count keys, 1 to its %d options", d.ID, len(d.Options))
	}
	return nil
}

func limitKind(typ string) string {
	switch typ {
	case DecisionDate:
		return "dates, YYYY-MM-DD"
	case DecisionChooseMany:
		return "whole numbers of keys"
	}
	return "numbers"
}

// limitValue is a limit or a value as a number to compare: the number, a
// date's day, or a count, which must be whole.
func limitValue(d Decision, raw json.RawMessage) (float64, error) {
	if d.Type == DecisionDate {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, err
		}
		t, err := time.Parse(dateLayout, s)
		if err != nil {
			return 0, err
		}
		return float64(t.Unix()), nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil || math.IsInf(v, 0) {
		return 0, errors.New("not a number")
	}
	if d.Type == DecisionChooseMany && v != math.Trunc(v) {
		return 0, errors.New("not a whole number")
	}
	return v, nil
}

// within says whether v is within the decision's limits.
func within(d Decision, v float64) bool {
	if d.Limits == nil {
		return true
	}
	for i, raw := range []json.RawMessage{d.Limits.Min, d.Limits.Max} {
		if len(raw) == 0 {
			continue
		}
		b, err := limitValue(d, raw)
		if err != nil || i == 0 && v < b || i == 1 && v > b {
			return false
		}
	}
	return true
}

// defaultAnswer is the answer a decision's default stands for.
func defaultAnswer(d Decision) (AnswerPayload, error) {
	a := AnswerPayload{ID: d.ID}
	switch d.Type {
	case DecisionYesNo:
		if d.Default != "yes" && d.Default != "no" {
			return a, errors.New("a yes_no default is yes or no")
		}
		yes := d.Default == "yes"
		a.Answer = &yes
	case DecisionChooseOne:
		a.Choice = d.Default
	case DecisionChooseMany:
		for _, k := range strings.Split(d.Default, ",") {
			a.Choices = append(a.Choices, strings.TrimSpace(k))
		}
	case DecisionNumber:
		a.Value = json.RawMessage(strings.TrimSpace(d.Default))
		if !json.Valid(a.Value) {
			return a, errors.New("a number default is a number")
		}
	case DecisionDate:
		a.Value, _ = json.Marshal(d.Default)
	default:
		return a, fmt.Errorf("a %s question has no default", d.Type)
	}
	return a, nil
}

// parseDeadline reads a deadline: a time in RFC 3339, or a date, which
// ends at midnight UTC.
func parseDeadline(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse(dateLayout, s)
}

func checkOptions(d Decision) error {
	if len(d.Options) < 2 || len(d.Options) > maxOptions {
		return fmt.Errorf("decision %s: a %s lists 2 to %d options; a question of one option is a yes_no", d.ID, d.Type, maxOptions)
	}
	keys := map[string]bool{}
	yesNo := 0
	for _, o := range d.Options {
		switch {
		case len(o.Key) != 1 || o.Key[0] < 'a' || o.Key[0] > 'z' || keys[o.Key]:
			return fmt.Errorf("decision %s: option keys are distinct single letters a to z", d.ID)
		case strings.TrimSpace(o.Label) == "" || strings.TrimSpace(o.Consequence) == "":
			return fmt.Errorf("decision %s: option %s needs a label and a consequence", d.ID, o.Key)
		case utf8.RuneCountInString(o.Label) > maxOptionField || utf8.RuneCountInString(o.Consequence) > maxOptionField:
			return fmt.Errorf("decision %s: option %s's label and consequence are at most %d characters each", d.ID, o.Key, maxOptionField)
		}
		keys[o.Key] = true
		if l := strings.ToLower(strings.TrimSpace(o.Label)); l == "yes" || l == "no" {
			yesNo++
		}
	}
	if len(d.Options) == 2 && yesNo == 2 {
		return fmt.Errorf("decision %s: a %s of yes and no is a yes_no; use yes_no", d.ID, d.Type)
	}
	return nil
}

// checkAnswer checks an answer against its decision before it is
// recorded, and again before it is delivered: one binding answer of the
// decision's type, or other, never both; a choice among the keys; every
// text within its limit.
func checkAnswer(d Decision, a AnswerPayload) error {
	if a.ID != d.ID {
		return fmt.Errorf("the answer is for %q, not %q", a.ID, d.ID)
	}
	given := 0
	for _, set := range []bool{a.Answer != nil, a.Choice != "", a.Choices != nil, len(a.Value) > 0, strings.TrimSpace(a.Text) != "", strings.TrimSpace(a.Other) != ""} {
		if set {
			given++
		}
	}
	if given != 1 {
		return fmt.Errorf("decision %s: give one answer, or other, and not both", d.ID)
	}
	switch {
	case utf8.RuneCountInString(a.Note) > maxNote || utf8.RuneCountInString(a.Other) > maxAnswerText || utf8.RuneCountInString(a.Text) > maxAnswerText:
		return fmt.Errorf("decision %s: a note is at most %d characters, an answer %d", d.ID, maxNote, maxAnswerText)
	case a.Other != "" && d.Type == DecisionText:
		return fmt.Errorf("decision %s: a text question is answered in text, not other", d.ID)
	case a.Answer != nil && d.Type != DecisionYesNo, a.Choice != "" && d.Type != DecisionChooseOne, a.Choices != nil && d.Type != DecisionChooseMany,
		len(a.Value) > 0 && d.Type != DecisionNumber && d.Type != DecisionDate, a.Text != "" && d.Type != DecisionText:
		return fmt.Errorf("decision %s: the answer is not of its type, %s", d.ID, d.Type)
	}
	keys := map[string]bool{}
	for _, o := range d.Options {
		keys[o.Key] = true
	}
	switch {
	case a.Choice != "" && !keys[a.Choice]:
		return fmt.Errorf("decision %s: %q is not one of its options", d.ID, cutUTF8(a.Choice, 40))
	case a.Choices != nil:
		chosen := map[string]bool{}
		for _, k := range a.Choices {
			if !keys[k] || chosen[k] {
				return fmt.Errorf("decision %s: choices are distinct keys of its options", d.ID)
			}
			chosen[k] = true
		}
		if len(a.Choices) == 0 || !within(d, float64(len(a.Choices))) {
			return fmt.Errorf("decision %s: %d choices is not within its limits", d.ID, len(a.Choices))
		}
	case len(a.Value) > 0:
		v, err := limitValue(d, a.Value)
		if err != nil {
			what := "a number"
			if d.Type == DecisionDate {
				what = "a date, YYYY-MM-DD"
			}
			return fmt.Errorf("decision %s: the value is not %s", d.ID, what)
		}
		if !within(d, v) {
			return fmt.Errorf("decision %s: the value is not within its limits", d.ID)
		}
	}
	return nil
}

// readQuestions reads a session's questions.json: its hash, "" when there
// is none, and its decisions, or the reason it is refused.
func readQuestions(workdir string) (hash string, ds []Decision, refused error) {
	b, err := readRegular(filepath.Join(workdir, QuestionsFile), maxQuestionsFile+1)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	hash = shortHash(HashPane(string(b)))
	if len(b) > maxQuestionsFile {
		return hash, nil, fmt.Errorf("%s is %d bytes, over %d", QuestionsFile, len(b), maxQuestionsFile)
	}
	var doc QuestionsDoc
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return hash, nil, fmt.Errorf("%s: %v", QuestionsFile, err)
	}
	if err := checkDecisions(doc.Decisions); err != nil {
		return hash, nil, fmt.Errorf("%s: %v", QuestionsFile, err)
	}
	return hash, doc.Decisions, nil
}

// statusDecisionFor is the text decision STATUS.md's Questions section
// with hash stands for.
func statusDecisionFor(hash string) Decision {
	return Decision{ID: statusDecision + hash, Type: DecisionText, Question: "the Questions in STATUS.md", Blocking: true}
}

// decisionHash identifies a decision as it was raised, every field of it,
// so an answer is to that question only.
func decisionHash(d Decision) string {
	b, _ := json.Marshal(d)
	return shortHash(HashPane(string(b)))
}

// decisionTexts are the words of a decision, for screening.
func decisionTexts(d Decision) []string {
	texts := []string{d.Question, d.Context, d.Why, d.IfWrong, d.Recommendation, d.Default, d.Deadline}
	if l := d.Limits; l != nil {
		texts = append(texts, string(l.Min), string(l.Max))
	}
	for _, o := range d.Options {
		texts = append(texts, o.Label, o.Consequence)
	}
	return texts
}

// readRegular reads at most max bytes of path, which must be a regular
// file and not a link: a session's file must not make agentd read a
// device, wait on a pipe, or read a file the session links to.
func readRegular(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a link; only a regular file is read", filepath.Base(path))
		}
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	return io.ReadAll(io.LimitReader(f, max))
}

// decision is the open decision id names: one raised in questions.json
// or through the session's tools, or the one STATUS.md's current
// Questions section stands for, a text question.
func (t *Track) decision(id string) (Decision, bool) {
	if t.StatusQuestions != "" && id == statusDecision+t.StatusQuestions {
		return statusDecisionFor(t.StatusQuestions), true
	}
	for _, d := range t.Decisions {
		if d.ID == id {
			return d, true
		}
	}
	for _, d := range t.Raised {
		if d.ID == id {
			return d, true
		}
	}
	return Decision{}, false
}

// open is every decision the session has open: those of questions.json,
// then those it raised through its tools.
func (t *Track) open() []Decision {
	return append(t.Decisions[:len(t.Decisions):len(t.Decisions)], t.Raised...)
}

// answer is the owner's answer to the open decision id, when it was given
// to the decision as the session last raised it: a reworded question, or
// an id raised again for another, waits for the owner again.
func (t *Track) answer(id string) (AnswerRecord, bool) {
	d, open := t.decision(id)
	if !open {
		return AnswerRecord{}, false
	}
	return t.answerTo(d)
}

// answerTo is the owner's answer to d, when it was given to d as it is.
func (t *Track) answerTo(d Decision) (AnswerRecord, bool) {
	a, ok := t.Answers[d.ID]
	return a, ok && a.Asked == decisionHash(d) && checkAnswer(d, a.AnswerPayload) == nil
}

// waits says whether d still waits on the owner: it has no answer, or
// one that does not settle it and has been delivered. An answer not yet
// delivered holds nothing up, so a session gone is restarted with it.
func (t *Track) waits(d Decision) bool {
	a, ok := t.answerTo(d)
	return !ok || !a.Binding() && a.Delivered
}

// answersFor lists the answers to the session's open decisions, by id;
// with undelivered, only those not delivered yet.
func (t *Track) answersFor(undelivered bool) []AnswerPayload {
	var out []AnswerPayload
	for id := range t.Answers {
		a, ok := t.answer(id)
		if !ok || undelivered && a.Delivered {
			continue
		}
		p := a.AnswerPayload
		p.From, p.Asked, p.ByDefault = "", "", false
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// freshAnswers names the answers to the session's open decisions not yet
// delivered, and when each was given, so that delivering them marks those
// answers only and not one given since.
func (t *Track) freshAnswers() (ids []string, at []time.Time) {
	for _, a := range t.answersFor(true) {
		ids = append(ids, a.ID)
		at = append(at, t.Answers[a.ID].At)
	}
	return ids, at
}

// openQuestions is the text the session waits on the owner for: STATUS.md's
// Questions as they stand this pass, and each blocking decision, unless
// they are answered (see waits). "" when nothing blocks.
func (t *Track) openQuestions(status, statusHash string) string {
	var parts []string
	if status != "" && t.waits(statusDecisionFor(statusHash)) {
		parts = append(parts, status)
	}
	for _, d := range t.open() {
		if d.Blocking && t.waits(d) {
			parts = append(parts, fmt.Sprintf("%s (%s): %s Recommended: %s", d.ID, d.Type, d.Question, d.Recommendation))
		}
	}
	return strings.Join(parts, "\n")
}

// answersDoc is answers.json.
type answersDoc struct {
	Answers []AnswerPayload `json:"answers"`
}

// writeAnswers writes the session's answers to answers.json in its
// workdir, through a temporary file renamed over it, so the session never
// reads half a file.
func writeAnswers(workdir string, answers []AnswerPayload) error {
	b, err := json.Marshal(answersDoc{Answers: answers})
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(workdir, AnswersFile), append(b, '\n'))
}

// writeAtomic writes b to path through a temporary file beside it, synced
// and renamed over it, readable by its owner only.
func writeAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// RecordAnswer appends the owner's answer to the session's decision id,
// checked against the decision as st holds it: an unknown or closed
// decision, or an answer that does not fit it, records nothing.
func RecordAnswer(log *Log, st *LogState, now time.Time, session string, a AnswerPayload) error {
	t := st.Tracks[session]
	if t == nil {
		return fmt.Errorf("no session %q", session)
	}
	d, ok := t.decision(a.ID)
	if !ok {
		return fmt.Errorf("%s has no open decision %q", session, a.ID)
	}
	if err := checkAnswer(d, a); err != nil {
		return err
	}
	a.Asked = decisionHash(d)
	return log.Append(Event{V: EventVersion, Time: now, Kind: KindAnswer, Mode: ModeOwner, Session: session, Answer: &a})
}

// readDecisions reads the session's questions.json, logging a decision
// event when it changed, then counts the owner's answers into ev: what
// STATUS.md asks no longer waits on the owner once the owner has answered
// it as it stands, and each blocking decision waits until it has a binding
// answer. A file that cannot be read or is refused is logged once; the
// decisions read before it stand.
func (s *Supervisor) readDecisions(name string, sess Session, t *Track, ev *Evidence) {
	hash, ds, err := readQuestions(sess.Workdir)
	if err != nil && hash == "" {
		hash = "unreadable"
	}
	if hash != t.DecisionsHash {
		p := &DecisionPayload{Hash: hash, Decisions: s.screenDecisions(ds)}
		if err != nil {
			p.Decisions, p.Refused = nil, cutUTF8(err.Error(), 300)
			if s.confidentialMatch(p.Refused) > 0 {
				p.Refused = QuestionsFile + " was refused (the reason is withheld: it matches a confidential pattern)"
			}
		}
		if s.emit(name, Event{Kind: KindDecision, Decision: p}); s.halt != nil {
			return
		}
	}
	if s.applyDefaults(name, t); s.halt != nil {
		return
	}
	if ev.Status.Questions != "" {
		ev.StatusQuestions = shortHash(HashPane(ev.Status.Questions))
	}
	ev.Status.Questions = t.openQuestions(ev.Status.Questions, ev.StatusQuestions)
}

// screenDecisions leaves out the words of each decision that match a
// confidential pattern (see withholdDecision).
func (s *Supervisor) screenDecisions(ds []Decision) []Decision {
	out := make([]Decision, len(ds))
	for i, d := range ds {
		if s.confidentialMatch(decisionTexts(d)...) == 0 {
			out[i] = d
			continue
		}
		out[i] = withholdDecision(d)
	}
	return out
}

// deliverAnswers hands the owner's new answers to a live session: it
// writes every answer to its open decisions to answers.json, then types
// the fixed pointer line, through the typing safeguards and only into a
// session that can take a line: working, idle, or waiting on its own
// questions, never one showing a dialog, the trust prompt or the usage
// limit. In shadow mode it is a would event; nothing is written or typed.
// It says whether it tried to type.
func (s *Supervisor) deliverAnswers(name string, sess Session, t *Track, ev Evidence, state State) bool {
	ids, at := t.freshAnswers()
	if len(ids) == 0 || !ev.Alive || t.Limited || ev.Dialog != "" || ev.TrustPrompt {
		return false
	}
	if state != Working && state != Idle && !(state == BlockedOnOwner && ev.Status.Questions != "") {
		return false
	}
	// A file that cannot be written is tried again on the restart backoff.
	key := "answer:" + name
	if !s.retryDue(t, key, s.clock.Now()) {
		return false
	}
	answers := t.answersFor(false)
	pointer := s.cfg.Policies.Answers.Pointer
	s.typeInto(name, ActAnswer, &ActPayload{Key: key, Answers: ids, AnswersAt: at, Message: pointer}, t, ev.PaneHash, func() error {
		if err := writeAnswers(sess.Workdir, answers); err != nil {
			return err
		}
		return s.tmux.Type(name, pointer)
	})
	return true
}
