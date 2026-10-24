package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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
// false, a choose_one with one option's key, a text in words.
const (
	DecisionYesNo     = "yes_no"
	DecisionChooseOne = "choose_one"
	DecisionText      = "text"
)

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

// Decision is one question a session raises, in raise_decision's shape:
// the decision form (context, why, what breaks if the call is wrong, the
// recommendation) and, for choose_one, the options.
type Decision struct {
	ID             string           `json:"id"`
	Type           string           `json:"type"`
	Question       string           `json:"question"`
	Context        string           `json:"context"`
	Why            string           `json:"why"`
	IfWrong        string           `json:"if_wrong"`
	Recommendation string           `json:"recommendation"`
	Options        []DecisionOption `json:"options,omitempty"`
	Default        string           `json:"default,omitempty"`
	Blocking       bool             `json:"blocking"`
	// Withheld is set by agentd, never read from the file: the texts
	// matched a confidential pattern and were left out of the log.
	Withheld bool `json:"withheld,omitempty"`
}

// DecisionOption is one of a choose_one's answers.
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
// session receives: answer for a yes_no, choice for a choose_one, text for
// a text, or other, the owner's own words instead, which answers but does
// not settle the decision. A note is context beside a binding answer.
type AnswerPayload struct {
	ID     string `json:"id"`
	Answer *bool  `json:"answer,omitempty"`
	Choice string `json:"choice,omitempty"`
	Text   string `json:"text,omitempty"`
	Other  string `json:"other,omitempty"`
	Note   string `json:"note,omitempty"`
	// From is the address the page was asked from, and Asked the hash of
	// the decision as the owner answered it; neither is sent on.
	From  string `json:"from,omitempty"`
	Asked string `json:"asked,omitempty"`
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
	return a.Answer != nil || a.Choice != "" || a.Text != ""
}

var decisionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)

// checkDecisions checks what a session raised: ids unique and plain,
// every field within its limit, and each question of the narrowest type,
// so a choose_one is not a yes_no and a text names no options.
func checkDecisions(ds []Decision) error {
	if len(ds) > maxDecisions {
		return fmt.Errorf("%d decisions; at most %d may be open", len(ds), maxDecisions)
	}
	seen := map[string]bool{}
	long := func(s string, n int) bool { return utf8.RuneCountInString(s) > n }
	for _, d := range ds {
		switch {
		case !decisionID.MatchString(d.ID) || strings.HasPrefix(d.ID, statusDecision):
			return fmt.Errorf("decision id %q: use up to 32 letters, digits, - or _, not starting with %q", cutUTF8(d.ID, 40), statusDecision)
		case seen[d.ID]:
			return fmt.Errorf("decision %s is raised twice", d.ID)
		case strings.TrimSpace(d.Question) == "" || long(d.Question, maxQuestion):
			return fmt.Errorf("decision %s: the question must be set and at most %d characters", d.ID, maxQuestion)
		case strings.TrimSpace(d.Context) == "" || strings.TrimSpace(d.Why) == "" || strings.TrimSpace(d.IfWrong) == "" || strings.TrimSpace(d.Recommendation) == "":
			return fmt.Errorf("decision %s: context, why, if_wrong and recommendation must all be set", d.ID)
		case long(d.Context, maxDecisionField) || long(d.Why, maxDecisionField) || long(d.IfWrong, maxDecisionField) || long(d.Recommendation, maxDecisionField):
			return fmt.Errorf("decision %s: context, why, if_wrong and recommendation are at most %d characters each", d.ID, maxDecisionField)
		}
		seen[d.ID] = true
		switch d.Type {
		case DecisionYesNo, DecisionText:
			if len(d.Options) > 0 {
				return fmt.Errorf("decision %s: a %s question lists no options; use choose_one", d.ID, d.Type)
			}
			if d.Default != "" && d.Type == DecisionText {
				return fmt.Errorf("decision %s: a text question has no default", d.ID)
			}
			if d.Type == DecisionYesNo && d.Default != "" && d.Default != "yes" && d.Default != "no" {
				return fmt.Errorf("decision %s: a yes_no default is yes or no", d.ID)
			}
		case DecisionChooseOne:
			if err := checkOptions(d); err != nil {
				return err
			}
		default:
			return fmt.Errorf("decision %s: type %q is not yes_no, choose_one or text", d.ID, d.Type)
		}
	}
	return nil
}

func checkOptions(d Decision) error {
	if len(d.Options) < 2 || len(d.Options) > maxOptions {
		return fmt.Errorf("decision %s: a choose_one lists 2 to %d options", d.ID, maxOptions)
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
		return fmt.Errorf("decision %s: a choose_one of yes and no is a yes_no", d.ID)
	}
	if d.Default != "" && !keys[d.Default] {
		return fmt.Errorf("decision %s: the default %q is not an option's key", d.ID, d.Default)
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
	for _, set := range []bool{a.Answer != nil, a.Choice != "", strings.TrimSpace(a.Text) != "", strings.TrimSpace(a.Other) != ""} {
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
	case a.Answer != nil && d.Type != DecisionYesNo, a.Choice != "" && d.Type != DecisionChooseOne, a.Text != "" && d.Type != DecisionText:
		return fmt.Errorf("decision %s: the answer is not of its type, %s", d.ID, d.Type)
	}
	if a.Choice != "" {
		for _, o := range d.Options {
			if o.Key == a.Choice {
				return nil
			}
		}
		return fmt.Errorf("decision %s: %q is not one of its options", d.ID, a.Choice)
	}
	return nil
}

// readQuestions reads a session's questions.json: its hash, "" when there
// is none, and its decisions, or the reason it is refused.
func readQuestions(workdir string) (hash string, ds []Decision, refused error) {
	b, err := os.ReadFile(filepath.Join(workdir, QuestionsFile))
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
	texts := []string{d.Question, d.Context, d.Why, d.IfWrong, d.Recommendation, d.Default}
	for _, o := range d.Options {
		texts = append(texts, o.Label, o.Consequence)
	}
	return texts
}

// decision is the open decision id names: one raised in questions.json,
// or the one STATUS.md's current Questions section stands for, a text
// question.
func (t *Track) decision(id string) (Decision, bool) {
	if t.StatusQuestions != "" && id == statusDecision+t.StatusQuestions {
		return statusDecisionFor(t.StatusQuestions), true
	}
	for _, d := range t.Decisions {
		if d.ID == id {
			return d, true
		}
	}
	return Decision{}, false
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
		p.From, p.Asked = "", ""
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
	for _, d := range t.Decisions {
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
	if ev.Status.Questions != "" {
		ev.StatusQuestions = shortHash(HashPane(ev.Status.Questions))
	}
	ev.Status.Questions = t.openQuestions(ev.Status.Questions, ev.StatusQuestions)
}

// screenDecisions leaves out the words of each decision that match a
// confidential pattern, keeping its id, type, option keys and whether it
// blocks, so it can still be answered from the session's own screen.
func (s *Supervisor) screenDecisions(ds []Decision) []Decision {
	out := make([]Decision, len(ds))
	for i, d := range ds {
		if s.confidentialMatch(decisionTexts(d)...) == 0 {
			out[i] = d
			continue
		}
		w := Decision{ID: d.ID, Type: d.Type, Blocking: d.Blocking, Withheld: true}
		for _, o := range d.Options {
			w.Options = append(w.Options, DecisionOption{Key: o.Key})
		}
		out[i] = w
	}
	return out
}

// deliverAnswers hands the owner's new answers to a live session: it
// writes every answer to its open decisions to answers.json, then types
// the fixed pointer line, through the typing safeguards and only into a
// session that can take a line: working, idle, or waiting on its own
// questions, never one showing a dialog, the trust prompt or the usage
// limit. In shadow mode it is a would event; nothing is written or typed.
func (s *Supervisor) deliverAnswers(name string, sess Session, t *Track, ev Evidence, state State) {
	ids, at := t.freshAnswers()
	if len(ids) == 0 || !ev.Alive || t.Limited || ev.Dialog != "" || ev.TrustPrompt {
		return
	}
	if state != Working && state != Idle && !(state == BlockedOnOwner && ev.Status.Questions != "") {
		return
	}
	// A file that cannot be written is tried again on the restart backoff.
	key := "answer:" + name
	if !s.retryDue(t, key, s.clock.Now()) {
		return
	}
	answers := t.answersFor(false)
	pointer := s.cfg.Policies.Answers.Pointer
	s.typeInto(name, ActAnswer, &ActPayload{Key: key, Answers: ids, AnswersAt: at, Message: pointer}, t, ev.PaneHash, func() error {
		if err := writeAnswers(sess.Workdir, answers); err != nil {
			return err
		}
		return s.tmux.Type(name, pointer)
	})
}
