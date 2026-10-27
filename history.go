package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The history layer (#60): everything agentd logged, as records an agent
// reaches by id or topic, and rarely. What is true now is a topic's state
// card (cards.go); each claim on a card names the records it was drawn
// from.

// History record kinds.
const (
	HistIssue       = "issue"        // an issue's title, body, labels and state, as the forge has it
	HistRequest     = "request"      // a request's, the same
	HistComment     = "comment"      // a comment on an issue or request
	HistItem        = "item"         // something new agentd's poll found
	HistDecision    = "decision"     // a decision a session raised, or agentd raised for it
	HistAnswer      = "answer"       // the owner's answer to one
	HistInstruction = "instruction"  // the owner's words in a console
	HistRuling      = "ruling"       // the owner's ruling on a case
	HistStatus      = "status"       // a status check's record (#57)
	HistPrompt      = "prompt"       // a turn's prompt
	HistReply       = "reply"        // the text a session ended a turn with
	HistToolFailure = "tool_failure" // a tool a session ran that failed (#56 C)
	HistCommit      = "commit"       // a merge newly at a session's head
	HistCI          = "ci"           // a request's pipeline changed state
	HistMerge       = "merge"        // agentd merged a request
	HistEscalation  = "escalation"   // agentd escalated to the owner
	HistPost        = "post"         // a session's post to a console
	HistFact        = "fact"         // a change in a session's run, brief or Now, or an issue's state (#62)
)

// HistKinds are the kinds history keeps, in the order a filter lists them.
var HistKinds = []string{HistIssue, HistRequest, HistComment, HistItem, HistDecision, HistAnswer, HistInstruction, HistRuling,
	HistStatus, HistPrompt, HistReply, HistToolFailure, HistCommit, HistCI, HistMerge, HistEscalation, HistPost, HistFact}

// HistRecord is one history record. Its id is drawn from its source, so a
// rebuild from the log gives it again.
type HistRecord struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	At      time.Time `json:"at"`
	Session string    `json:"session,omitempty"`
	Title   string    `json:"title,omitempty"`
	Text    string    `json:"text"`
	Topics  []string  `json:"topics"`
	Source  string    `json:"source"` // where it came from: the log's kind and key, or the forge's page
	repo    string
}

// histLetter is the first letter of a kind's ids.
var histLetter = map[string]string{HistIssue: "N", HistRequest: "N", HistComment: "C", HistItem: "W", HistDecision: "D", HistAnswer: "A",
	HistInstruction: "I", HistRuling: "R", HistStatus: "S", HistPrompt: "P", HistReply: "T", HistToolFailure: "F", HistCommit: "G",
	HistCI: "K", HistMerge: "M", HistEscalation: "E", HistPost: "O", HistFact: "H"}

// histID is a record's id: its kind's letter and a hash of its key.
func histID(kind, key string) string {
	h := sha256.Sum256([]byte(kind + "\x00" + key))
	return histLetter[kind] + hex.EncodeToString(h[:5])
}

// Topic forms. An issue or request is its reference, #N or !N; the rest
// carry a prefix.
const (
	TopicRepo    = "repo"
	topicSession = "session:"
	topicArea    = "area:"
	topicLabel   = "label:"
)

var (
	refPattern  = regexp.MustCompile(`(?:^|[\s(\[{,;"'\x60])([#!][0-9]{1,6})\b`)
	pathPattern = regexp.MustCompile(`(?:^|[\s"'(\x60])((?:[\w.-]+/)*[\w-]+\.(?:go|md|json|jsonl|ya?ml|sh|py|ts|tsx|js|html|css|toml|txt|mod|sql))\b`)
)

// refsIn are the issue and request references text names, in order.
func refsIn(text string) []string {
	var out []string
	for _, m := range refPattern.FindAllStringSubmatchIndex(text, -1) {
		ref := text[m[2]:m[3]]
		// Not a value after a colon, as a colour is, nor part of a word.
		if before := strings.TrimRight(text[:m[2]], " \t"); strings.HasSuffix(before, ":") || m[3] < len(text) && text[m[3]] == '-' {
			continue
		}
		if !contains(out, ref) {
			out = append(out, ref)
		}
	}
	return out
}

// areasIn are the areas of the code text names: a path's directory, or a
// file at the top its name without extension or test suffix.
func areasIn(text string) []string {
	var out []string
	for _, m := range pathPattern.FindAllStringSubmatch(text, -1) {
		if a := areaOf(m[1]); a != "" && !contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

func areaOf(p string) string {
	p = strings.TrimPrefix(path.Clean(p), "./")
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "..") {
		return "" // outside a repository
	}
	if d := path.Dir(p); d != "." {
		return topicArea + strings.SplitN(d, "/", 2)[0]
	}
	stem := strings.TrimSuffix(p, path.Ext(p))
	return topicArea + strings.TrimSuffix(stem, "_test")
}

// History is one repository's history, built from the log as it grows.
type History struct {
	Repo    string
	records []*HistRecord
	byID    map[string]*HistRecord
	byTopic map[string][]*HistRecord // oldest first
}

func newHistory(repo string) *History {
	return &History{Repo: repo, byID: map[string]*HistRecord{}, byTopic: map[string][]*HistRecord{}}
}

// add files r, unless a record of its id is there already. It reports
// whether r is new.
func (h *History) add(r *HistRecord) bool {
	if _, ok := h.byID[r.ID]; ok {
		return false
	}
	r.repo = h.Repo
	h.records = append(h.records, r)
	h.byID[r.ID] = r
	for _, t := range r.Topics {
		h.byTopic[t] = append(h.byTopic[t], r)
	}
	return true
}

// Record is the record of id, or nil.
func (h *History) Record(id string) *HistRecord { return h.byID[id] }

// Len is how many records the history holds.
func (h *History) Len() int { return len(h.records) }

// OnTopic is the records on topic, oldest first.
func (h *History) OnTopic(topic string) []*HistRecord { return h.byTopic[topic] }

// Topics are the topics the history has records on.
func (h *History) Topics() []string { return sortedKeys(h.byTopic) }

// topicsOf derives a record's topics from its fields and the references in
// its text: its session, the references and areas its title and text name,
// and those given.
func topicsOf(session string, given []string, texts ...string) []string {
	var out []string
	add := func(t string) {
		if t != "" && !contains(out, t) {
			out = append(out, t)
		}
	}
	for _, t := range given {
		add(t)
	}
	if session != "" {
		add(topicSession + session)
	}
	for _, text := range texts {
		for _, r := range refsIn(text) {
			add(r)
		}
		for _, a := range areasIn(text) {
			add(a)
		}
	}
	return out
}

// refOfCase is a case's reference, #N, from its id, repo#N.
func refOfCase(id string) (repo, ref string) {
	if i := strings.LastIndexAny(id, "#!"); i > 0 {
		return id[:i], id[i:]
	}
	return "", ""
}

// histSource tells which repository a log event is about and what
// records it makes. sessionRepo is the repository of each session the log
// has named so far; consoleRepos the repositories a console feeds.
type histSource struct {
	cfg          *Config
	sessionRepo  map[string]string
	sessionIssue map[string]int
	sessionCwd   map[string]string
}

func newHistSource(cfg *Config) *histSource {
	s := &histSource{cfg: cfg, sessionRepo: map[string]string{}, sessionIssue: map[string]int{}, sessionCwd: map[string]string{}}
	for name, sess := range cfg.Sessions {
		s.sessionRepo[name], s.sessionIssue[name], s.sessionCwd[name] = sess.Repository, sess.Issue, sess.Cwd
	}
	return s
}

// repoNamed is the configured repository a key names: its name, or the
// key of its forge and project that items and cases carry.
func (s *histSource) repoNamed(key string) string {
	if _, ok := s.cfg.Repositories[key]; ok {
		return key
	}
	for _, n := range sortedKeys(s.cfg.Repositories) {
		if r := s.cfg.Repositories[n]; r.Project != "" && (strings.EqualFold(repoKey(r.Forge, r.Project), key) || strings.EqualFold(r.Project, key)) {
			return n
		}
	}
	return ""
}

// records are what event e adds to history, each with its repository.
func (s *histSource) records(e Event) []*HistRecord {
	if e.Runtime != nil && e.Session != "" && e.Runtime.Repository != "" {
		s.sessionRepo[e.Session], s.sessionIssue[e.Session] = e.Runtime.Repository, e.Runtime.Issue
	}
	repo := s.sessionRepo[e.Session]
	var own []string // the session's own issue, a topic of what it says
	if n := s.sessionIssue[e.Session]; n > 0 {
		own = []string{"#" + strconv.Itoa(n)}
	}
	key := e.Time.UTC().Format(time.RFC3339Nano) + "|" + e.Session
	// A path in the session's worktree is named from the worktree's root.
	inTree := func(text string) string {
		if cwd := s.sessionCwd[e.Session]; cwd != "" {
			return strings.ReplaceAll(text, strings.TrimSuffix(cwd, "/")+"/", "")
		}
		return text
	}
	one := func(kind, k, title, text, source string, topics []string) []*HistRecord {
		if repo == "" {
			return nil
		}
		return []*HistRecord{{ID: histID(kind, k), Kind: kind, At: e.Time, Session: e.Session, Title: title, Text: text,
			Topics: topicsOf(e.Session, topics, title, inTree(text)), Source: source, repo: repo}}
	}
	switch {
	case e.Fact != nil:
		if r := factRecord(e); r != nil {
			return []*HistRecord{r}
		}
		return nil
	case e.Item != nil:
		it := e.Item
		repo = s.repoNamed(it.Repository)
		kind := HistItem
		var topics []string
		if i := strings.LastIndexAny(it.ID, "#!"); i >= 0 {
			ref := it.ID[i:]
			if j := strings.IndexByte(ref, '@'); j > 0 {
				ref = ref[:j]
			}
			topics = append(topics, ref)
		}
		return one(kind, it.ID, it.Title, strings.TrimSpace(it.Kind+" "+it.Author+" "+it.Link), "item:"+it.ID, topics)
	case e.Decision != nil:
		var out []*HistRecord
		for _, d := range e.Decision.Decisions {
			text := strings.TrimSpace(d.Question + "\n" + d.Context + "\n" + d.Why + "\nrecommendation: " + d.Recommendation)
			for _, o := range d.Options {
				text += "\n" + o.Key + ": " + o.Label
			}
			topics := append([]string{e.Session + "/" + d.ID}, own...)
			out = append(out, one(HistDecision, e.Session+"/"+d.ID+"|"+d.Question, "", text, "decision:"+e.Session+"/"+d.ID, topics)...)
		}
		return out
	case e.Answer != nil:
		a := e.Answer
		text := a.Choice
		if a.Answer != nil {
			text = strconv.FormatBool(*a.Answer)
		}
		if len(a.Choices) > 0 {
			text = strings.Join(a.Choices, ",")
		}
		if len(a.Value) > 0 {
			text = string(a.Value)
		}
		text = strings.TrimSpace(strings.Join([]string{text, a.Text, a.Other, a.Note}, " "))
		if a.ByDefault {
			text += " (the default, at the owner's deadline)"
		}
		return one(HistAnswer, key+"|"+a.ID, "", text, "answer:"+e.Session+"/"+a.ID, append([]string{e.Session + "/" + a.ID}, own...))
	case e.Instruction != nil:
		in := e.Instruction
		var out []*HistRecord
		for _, r := range in.Repositories {
			repo = r
			var topics []string
			if in.To != "" {
				topics = append(topics, topicSession+in.To)
			}
			rec := one(HistInstruction, in.ID+"|"+r, "", in.Text, "instruction:"+in.ID, topics)
			if len(rec) > 0 {
				rec[0].ID = in.ID // the owner's instruction keeps its own id
				if len(in.Repositories) > 1 {
					rec[0].ID = in.ID + "@" + r
				}
			}
			out = append(out, rec...)
		}
		return out
	case e.Case != nil && e.Case.Ruling != nil:
		r, ref := refOfCase(e.Case.Case)
		repo = s.repoNamed(r)
		ru := e.Case.Ruling
		return one(HistRuling, key+"|"+e.Case.Case+"|"+ru.Kind+ru.ID, "", strings.TrimSpace(ru.Kind+" "+ru.ID+": "+ru.Text), "case:"+e.Case.Case, []string{ref})
	case e.Status != nil && e.Status.Record != nil:
		st := e.Status.Record
		text := fmt.Sprintf("phase %s, progress %.0f%%, risk %s. %s Next: %s", st.Phase, st.Progress*100, st.Risk, st.Summary, st.Next)
		if st.Blocked {
			text += " Blocked: " + st.Blocker
		}
		return one(HistStatus, key, "", text, "status:"+e.Session, own)
	case e.Turn != nil:
		t := e.Turn
		src := fmt.Sprintf("transcript:%s:%d", t.Transcript, t.Line)
		switch {
		case t.Role == TurnPrompt && t.Text != "":
			return one(HistPrompt, src, "", t.Text, src, own)
		case t.Role == TurnSummary && t.Text != "":
			return one(HistReply, src, "", t.Text, src, own)
		case t.Role == TurnTool && t.Failed:
			text := t.Text
			if t.Exit != nil {
				text += fmt.Sprintf(" (exit %d)", *t.Exit)
			}
			return one(HistToolFailure, src, "", text, src, own)
		}
	case e.Checkpoint != nil && !e.Checkpoint.Baseline:
		c := e.Checkpoint
		return one(HistCommit, c.SHA, c.Subject, c.SHA, "commit:"+c.SHA, own)
	case e.CI != nil:
		c := e.CI
		repo = c.Repository
		text := strings.TrimSpace(strings.Join([]string{c.State, c.Verdict, c.Cause, strings.Join(c.Jobs, ", "), c.Excerpt, c.Error}, "\n"))
		return one(HistCI, key+"|"+c.Ref+"|"+c.Head+"|"+c.State+"|"+c.Run, c.Ref+" "+c.State, text, "ci:"+c.Ref, append([]string{c.Ref}, own...))
	case e.Message != nil && e.Message.Text != "":
		m := e.Message
		var out []*HistRecord
		for _, r := range s.consoleRepos(m.Console) {
			repo = r
			out = append(out, one(HistPost, m.ID+"|"+r, "", m.Text, "message:"+m.ID, nil)...)
		}
		return out
	case e.Act != nil && e.Kind == KindAction:
		a := e.Act
		switch a.Action {
		case ActMerge:
			if a.Repository != "" {
				repo = a.Repository
			}
			return one(HistMerge, key+"|"+a.Ref+"|"+a.Head, a.Ref+" merged", strings.TrimSpace(a.Head+" into "+a.Target+" "+a.Method), "merge:"+a.Ref, []string{a.Ref})
		case ActEscalate, ActRaise:
			text := a.Message
			if a.Raised != nil {
				text = strings.TrimSpace(text + "\n" + a.Raised.Question)
			}
			return one(HistEscalation, key+"|"+a.Action+"|"+a.Key, "", text, "action:"+a.Action, own)
		}
	}
	return nil
}

// consoleRepos are the repositories whose console is name.
func (s *histSource) consoleRepos(name string) []string {
	var out []string
	for _, n := range sortedKeys(s.cfg.Repositories) {
		if r := s.cfg.Repositories[n]; r.Console == name || r.Console == "" && n == name {
			out = append(out, n)
		}
	}
	return out
}

// Budgets. An answer's size is counted in tokens as bytes × 4/7: the
// JSON of code and commands measured about 1.75 bytes a token (#57).
const (
	DefaultHistoryBudget = 4000
	MaxHistoryBudget     = 32000
	minHistoryBudget     = 200
)

func tokensOf(bytes int) int { return (bytes*4 + 6) / 7 }

func budgetBytes(tokens int) int { return tokens * 7 / 4 }

// budgetOf is a requested budget, defaulted and bounded.
func budgetOf(tokens int) int {
	switch {
	case tokens <= 0:
		return DefaultHistoryBudget
	case tokens < minHistoryBudget:
		return minHistoryBudget
	case tokens > MaxHistoryBudget:
		return MaxHistoryBudget
	}
	return tokens
}

// histEntry is a record as history(topic) lists it: its text cut to fit.
type histEntry struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	At      string `json:"at"`
	Session string `json:"session,omitempty"`
	Summary string `json:"summary"`
	Text    string `json:"text,omitempty"`
	Cut     bool   `json:"cut,omitempty"`
}

// histEntryText is the most of a record's text one listed entry carries.
const histEntryText = 1500

func (r *HistRecord) summary() string {
	if r.Title != "" {
		return oneLine(r.Title, 140)
	}
	return oneLine(r.Text, 140)
}

func entryOf(r *HistRecord, text int) histEntry {
	e := histEntry{ID: r.ID, Kind: r.Kind, At: r.At.UTC().Format(time.RFC3339), Session: r.Session, Summary: r.summary()}
	if text > 0 && r.Text != "" && r.Text != e.Summary {
		e.Text = cutUTF8(r.Text, text)
		e.Cut = len(e.Text) < len(r.Text)
	}
	return e
}

// HistoryQuery is history(topic, since?, kinds?, budget): the records on
// a topic, newest first, from the cursor More gave.
type HistoryQuery struct {
	Topic  string
	Since  time.Time
	Kinds  []string
	Budget int
	More   string
}

// historyCursor is where a page ended: the last record's time and id.
func historyCursor(r *HistRecord) string {
	return r.At.UTC().Format(time.RFC3339Nano) + "|" + r.ID
}

func newerFirst(a, b *HistRecord) bool {
	if !a.At.Equal(b.At) {
		return a.At.After(b.At)
	}
	return a.ID > b.ID
}

// ByTopic answers q, never more than its budget in bytes × 4/7: each
// record whole up to histEntryText, then by its summary only once whole
// ones no longer fit, and More names the next page when any is left.
func (h *History) ByTopic(q HistoryQuery) map[string]any {
	var recs []*HistRecord
	for _, r := range h.byTopic[q.Topic] {
		if (q.Since.IsZero() || !r.At.Before(q.Since)) && (len(q.Kinds) == 0 || contains(q.Kinds, r.Kind)) {
			recs = append(recs, r)
		}
	}
	sort.Slice(recs, func(i, j int) bool { return newerFirst(recs[i], recs[j]) })
	if q.More != "" {
		at, id, _ := strings.Cut(q.More, "|")
		t, err := time.Parse(time.RFC3339Nano, at)
		if err == nil {
			cur := &HistRecord{At: t, ID: id}
			i := sort.Search(len(recs), func(i int) bool { return newerFirst(cur, recs[i]) })
			recs = recs[i:]
		}
	}
	out := map[string]any{"topic": q.Topic, "total": len(recs)}
	limit := budgetBytes(budgetOf(q.Budget))
	// What the envelope costs with a cursor in it, and a comma a record.
	size := len(compact(out)) + len(`,"records":[],"more":""`) + 2*len(historyCursor(&HistRecord{At: time.Now(), ID: "X0000000000@" + h.Repo}))
	entries := []histEntry{}
	for i, r := range recs {
		e := entryOf(r, histEntryText)
		n := len(compact(e)) + 1
		if size+n > limit && e.Text != "" {
			e = entryOf(r, 0)
			n = len(compact(e)) + 1
		}
		if size+n > limit {
			if i > 0 {
				out["more"] = historyCursor(recs[i-1])
			} else {
				out["over_budget"] = true // not even one fits: the budget is too small
			}
			break
		}
		entries = append(entries, e)
		size += n
	}
	out["records"] = entries
	return out
}

// HistWhole is a record given whole, or cut to a budget.
type HistWhole struct {
	HistRecord
	Cut bool `json:"cut,omitempty"`
}

// ByIDs is history(ids): each record whole, with its source, cut so the
// answer fits the budget; an unknown id is named in missing.
func (h *History) ByIDs(ids []string, budget int) map[string]any {
	limit := budgetBytes(budgetOf(budget))
	recs := []HistWhole{}
	var missing []string
	for _, id := range ids {
		if r := h.byID[id]; r != nil {
			recs = append(recs, HistWhole{HistRecord: *r})
		} else if !contains(missing, id) {
			missing = append(missing, id)
		}
	}
	out := map[string]any{"records": recs}
	if len(missing) > 0 {
		out["missing"] = missing
	}
	// Cut the longest text to what the answer is over, then drop records
	// from the end, then the missing ids, until the answer fits.
	for over := len(compact(out)) - limit; over > 0; over = len(compact(out)) - limit {
		longest := -1
		for i := range recs {
			if longest < 0 || len(recs[i].Text) > len(recs[longest].Text) {
				longest = i
			}
		}
		switch {
		case longest >= 0 && len(recs[longest].Text) >= 64:
			keep := max(len(recs[longest].Text)-over-16, len(recs[longest].Text)/2)
			recs[longest].Text, recs[longest].Cut = cutUTF8(recs[longest].Text, keep), true
		case len(recs) > 0:
			recs = recs[:len(recs)-1]
			out["over_budget"] = true
		default:
			delete(out, "missing")
			out["missing_count"] = len(missing)
			out["over_budget"] = true
			out["records"] = recs
			return out
		}
		out["records"] = recs
	}
	return out
}
