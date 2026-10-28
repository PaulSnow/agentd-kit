package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The owner's three levels (#69): home, a repository, an issue. Each says
// where things stand in sentences written when their events happened,
// read from the fold alone; building a level calls no forge and no model.
// The pages and the CLI print the same views.

// IssueStep is one dated step of an issue or request, in a sentence.
type IssueStep struct {
	At   time.Time `json:"at"`
	Ref  string    `json:"ref"`
	Text string    `json:"text"`
}

// stepsKept bounds the steps the fold keeps per issue or request.
const stepsKept = 100

// noteStep adds a step to ref's steps in repo.
func (st *LogState) noteStep(repo, ref string, at time.Time, text string) {
	if repo == "" || ref == "" || strings.TrimSpace(text) == "" {
		return
	}
	if st.Steps == nil {
		st.Steps = map[string][]IssueStep{}
	}
	k := sentenceKey(repo, ref)
	s := st.Steps[k]
	if n := len(s); n > 0 && s[n-1].Text == text {
		return
	}
	s = append(s, IssueStep{At: at, Ref: ref, Text: cutUTF8(text, 400)})
	if len(s) > stepsKept {
		s = append([]IssueStep(nil), s[len(s)-stepsKept:]...)
	}
	st.Steps[k] = s
}

// noteSteps folds what event e did to an issue or request into its steps,
// after e is applied: a work item's steps on its issue, a session started
// on one, and a request's loop steps, on the request and its case's issue.
// The index and the sentences note their own.
func (st *LogState) noteSteps(e Event) {
	switch {
	case e.Kind == KindWorkItem && e.Work != nil:
		p := e.Work
		it := st.Work[p.ID]
		if it == nil || it.Issue <= 0 {
			return
		}
		ref := "#" + strconv.Itoa(it.Issue)
		var text string
		switch p.Step {
		case WorkStepOpen:
			text = fmt.Sprintf("Work item %s opened: %s.", it.ID, it.Title)
		case WorkStepAssign:
			text = fmt.Sprintf("%s given to %s.", it.ID, p.Session)
		case WorkStepDesign:
			if p.Design != nil && p.Design.Error == "" {
				text = fmt.Sprintf("%s designed, round %d.", it.ID, p.Design.Round)
			}
		case WorkStepGo:
			text = fmt.Sprintf("%s's design approved.", it.ID)
		case WorkStepChange:
			text = fmt.Sprintf("%s's design sent back with your words.", it.ID)
		case WorkStepBlocked:
			text = fmt.Sprintf("%s asks you: %s", it.ID, oneLine(it.Question, 200))
		case WorkStepClaim:
			text = fmt.Sprintf("%s says it is done.", it.ID)
		case WorkStepCheck:
			if p.Check != nil {
				text = fmt.Sprintf("%s checked: %s", it.ID, oneLine(p.Check.Detail, 200))
			}
		case WorkStepDone:
			text = fmt.Sprintf("%s done.", it.ID)
		case WorkStepDeclined:
			text = fmt.Sprintf("%s declined.", it.ID)
		}
		st.noteStep(it.Repository, ref, e.Time, text)
	case e.Kind == KindSession && e.Runtime != nil && e.Runtime.Issue > 0 && e.Runtime.Action == SessionStart:
		st.noteStep(e.Runtime.Repository, "#"+strconv.Itoa(e.Runtime.Issue), e.Time, fmt.Sprintf("Session %s started on it.", e.Runtime.ID))
	case e.Kind == KindLoop && e.Loop != nil && e.Mode != ModeOwner:
		p := e.Loop
		var text string
		switch p.Step {
		case LoopReview:
			text = fmt.Sprintf("%s: review round %d started.", p.Ref, p.Round)
		case LoopJudge:
			text = fmt.Sprintf("%s: judge round %d started.", p.Ref, p.Round)
		case LoopFix:
			text = fmt.Sprintf("%s: fix round %d.", p.Ref, p.Round)
		case LoopApprove:
			text = fmt.Sprintf("%s approved by the loop.", p.Ref)
		case LoopFailed:
			text = fmt.Sprintf("%s: the %s step failed: %s", p.Ref, p.Failed, oneLine(p.Reason, 200))
		}
		st.noteStep(p.Repository, p.Ref, e.Time, text)
		if issue := caseIssue(p.Case); issue != "" {
			st.noteStep(p.Repository, issue, e.Time, text)
		}
	}
}

// LevelIssue is an issue as its row and its page head show it.
type LevelIssue struct {
	Repository string    `json:"repository"`
	Ref        string    `json:"ref"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	Labels     []string  `json:"labels,omitempty"`
	Milestone  string    `json:"milestone,omitempty"`
	Author     string    `json:"author,omitempty"`
	Opened     time.Time `json:"opened"`
	Changed    time.Time `json:"changed"`
	// Word is its state in a word: needs you, working, stopped or open.
	Word       string    `json:"state"`
	Sentence   string    `json:"sentence,omitempty"`
	Writer     string    `json:"writer,omitempty"`
	SentenceAt time.Time `json:"sentence_at,omitempty"`
	Requests   []string  `json:"requests,omitempty"`
	// Decision is the first open decision, whose options the row offers.
	Decision *LevelDecision `json:"decision,omitempty"`
	Working  bool           `json:"working,omitempty"`
}

// FilterText is what the filter's words are matched in: title,
// sentence, labels, milestone and author, in lower case.
func (i LevelIssue) FilterText() string {
	return strings.ToLower(i.Title + " " + i.Sentence + " " + strings.Join(i.Labels, " ") + " " + i.Milestone + " " + i.Author)
}

// WordClass is the state word as a class name.
func (i LevelIssue) WordClass() string { return strings.ReplaceAll(i.Word, " ", "-") }

// Anchor is the decision card's id on its issue's page.
func (d LevelDecision) Anchor() string {
	return "decision-" + strings.NewReplacer(":", "-", "/", "-").Replace(d.Entry)
}

// Issue state words.
const (
	WordNeedsYou = "needs you"
	WordWorking  = "working"
	WordStopped  = "stopped"
	WordOpen     = "open"
)

// LevelRequest is an open request as the MR box shows it.
type LevelRequest struct {
	Ref     string    `json:"ref"`
	Number  int       `json:"number"`
	Title   string    `json:"title"`
	Issue   string    `json:"issue,omitempty"`
	Place   string    `json:"place,omitempty"`
	Rank    int       `json:"rank"`
	CI      string    `json:"ci,omitempty"`
	Failing bool      `json:"ci_failing,omitempty"`
	Author  string    `json:"author,omitempty"`
	Branch  string    `json:"branch,omitempty"`
	Draft   bool      `json:"draft,omitempty"`
	Opened  time.Time `json:"opened"`
	Changed time.Time `json:"changed"`
}

// LevelDecision is an open decision as a card shows it: its entry, what
// is asked, what came back, what Go checked by itself, and the options,
// each with its consequence.
type LevelDecision struct {
	// Decision is a session's decision, whose own inputs the card shows.
	Decision *Decision     `json:"-"`
	Entry    string        `json:"entry"`
	Session  string        `json:"session,omitempty"`
	Asked    string        `json:"asked"`
	CameBack string        `json:"came_back,omitempty"`
	Checked  string        `json:"checked,omitempty"`
	Options  []LevelOption `json:"options,omitempty"`
}

// LevelOption is one answer a decision takes, with what it does.
type LevelOption struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Consequence string `json:"consequence,omitempty"`
}

// RepoLevel is level 2: a repository's state sentence, its issues and
// its open requests.
type RepoLevel struct {
	Repository string         `json:"repository"`
	Tracked    bool           `json:"tracked"`
	Read       time.Time      `json:"read,omitempty"`
	Sentence   string         `json:"sentence"`
	Counts     IndexCounts    `json:"counts"`
	Issues     []LevelIssue   `json:"issues"`
	Requests   []LevelRequest `json:"requests"`
}

// IssueLevel is level 3: an issue, what belongs to it, its open
// decisions, its steps and what is next.
type IssueLevel struct {
	LevelIssue
	Known     bool            `json:"known"`
	Refs      []LevelRef      `json:"refs"`
	Decisions []LevelDecision `json:"decisions,omitempty"`
	Steps     []IssueStep     `json:"steps"`
	More      int             `json:"more_steps,omitempty"`
	Next      []string        `json:"next,omitempty"`
	Request   []LevelRequest  `json:"request_rows,omitempty"`
}

// LevelRef is one thing that belongs to an issue.
type LevelRef struct {
	Kind string `json:"kind"` // item, session, request, case
	Name string `json:"name"`
	Note string `json:"note,omitempty"`
}

// workConsequences say what each answer to a work item's entry does.
var workConsequences = map[string]string{
	WorkAnswerDone:     "the item closes as done",
	WorkAnswerNotDone:  "its session is told the work is not done",
	WorkAnswerReassign: "the item is taken off its session and given to another",
	WorkAnswerDecline:  "the item closes and nothing more is done on it",
}

// levelDecisions are the decisions open on issue n of repo: a design that
// waits on the owner's go (b:), an item that asks the owner or says it is
// done under the owner's own check (w:), and the open decisions (d:) of
// the joined sessions whose own issue it is (a session's configured
// issue, else its open item's), from decisions. A card whose words match
// a confidential pattern is withheld, its options kept.
func levelDecisions(cfg *Config, st *LogState, n int, j IssueJoin, decisions []UIDecision, secret func(...string) bool) []LevelDecision {
	out := levelDecisionsOf(cfg, st, n, j, decisions)
	for k := range out {
		d := &out[k]
		if secret(d.Asked, d.CameBack, d.Checked) {
			d.Asked, d.CameBack, d.Checked = withheldSecret, "", ""
		}
	}
	return out
}

func levelDecisionsOf(cfg *Config, st *LogState, n int, j IssueJoin, decisions []UIDecision) []LevelDecision {
	var out []LevelDecision
	for _, it := range j.Work {
		if it.terminal() {
			continue
		}
		if it.State == WorkDesigned {
			d := designDecision(it, "")
			ld := LevelDecision{Entry: entryDesign + it.ID, Session: it.Session, Asked: d.Question}
			if it.Design != nil && it.Design.Brief != nil {
				ld.CameBack = fmt.Sprintf("The designer's round %d brief.", it.Design.Round)
			}
			if it.Design != nil && it.Design.Rating != nil {
				ld.Checked = "Go checked whether the design is contained; it waits on your go."
			}
			for _, o := range d.Options {
				ld.Options = append(ld.Options, LevelOption{Key: o.Key, Label: o.Label, Consequence: o.Consequence})
			}
			out = append(out, ld)
			continue
		}
		if it.Question == "" && !(it.DoneWhen.Kind == DoneOwner && it.Claim != "") {
			continue
		}
		ld := LevelDecision{Entry: entryWork + it.ID, Session: it.Session}
		if it.Question != "" {
			ld.Asked = fmt.Sprintf("%s asks: %s", it.Session, oneLine(it.Question, 400))
		} else {
			ld.Asked = fmt.Sprintf("Is %s done?", it.ID)
			ld.CameBack = fmt.Sprintf("%s says: %s", it.Session, oneLine(it.Claim, 400))
		}
		if it.Check != nil {
			ld.Checked = oneLine(it.Check.Detail, 300)
		}
		for _, o := range it.workOptions() {
			ld.Options = append(ld.Options, LevelOption{Key: o, Label: o, Consequence: workConsequences[o]})
		}
		out = append(out, ld)
	}
	in := map[string]bool{}
	for _, s := range j.Sessions {
		in[s] = true
	}
	c := withRuntime(cfg, st.Runtime)
	for _, d := range decisions {
		if !in[d.Session] {
			continue
		}
		if own := c.Sessions[d.Session].Issue; own > 0 && own != n {
			continue // its decisions are its own issue's
		}
		dec := d.Decision
		ld := LevelDecision{Decision: &dec, Entry: decisionEntry(d.Session, d.ID), Session: d.Session, Asked: oneLine(d.Question, 400), CameBack: oneLine(d.Context, 400)}
		if d.Withheld != "" {
			ld.Asked, ld.CameBack = d.Withheld, ""
		}
		if d.Recommendation != "" {
			ld.Checked = "Recommended: " + oneLine(d.Recommendation, 300)
		}
		for _, o := range d.Options {
			ld.Options = append(ld.Options, LevelOption{Key: o.Key, Label: o.Label, Consequence: o.Consequence})
		}
		out = append(out, ld)
	}
	return out
}

// levelIssue is issue i of repo as a row: its sentence, its word, its
// requests and its first open decision.
func levelIssue(cfg *Config, st *LogState, repo string, i *IndexIssue, decisions []UIDecision, busy map[string]bool, secret func(...string) bool) (LevelIssue, IssueJoin, []LevelDecision) {
	n := i.Number()
	j := joinIssue(cfg, st, repo, n)
	li := LevelIssue{Repository: repo, Ref: i.Ref, Number: n, Title: i.Title, Labels: i.Labels, Milestone: i.Milestone, Author: i.Author,
		Opened: i.Opened, Changed: i.Changed, Requests: j.Requests, Working: busy[i.Ref], Word: WordOpen}
	if i.Withheld > 0 {
		li.Title = "(withheld: it matched a confidential pattern)"
	}
	if s := st.sentenceOf(repo, i.Ref); s != nil {
		li.Sentence, li.Writer, li.SentenceAt = s.Text, s.Writer, s.At
	}
	ds := levelDecisions(cfg, st, n, j, decisions, secret)
	switch {
	case len(ds) > 0:
		li.Word, li.Decision = WordNeedsYou, &ds[0]
	case li.Working:
		li.Word = WordWorking
		for _, it := range j.Work {
			if !it.terminal() && it.Started && it.Session != "" {
				if _, live := st.Runtime[it.Session]; !live {
					li.Word = WordStopped
				}
			}
		}
	}
	return li, j, ds
}

// loopRanks order the request loop's places for the MR box: what waits on
// the owner first, then what is moving, then what waits on CI, and what is
// outside the loop last.
var loopRanks = []string{PlaceOwner, PlaceFixing, PlaceJudging, PlaceReviewing, PlaceMerging, PlaceCI, PlaceOutside}

// levelRequest is request i of repo as the MR box shows it.
func levelRequest(st *LogState, repo string, i *IndexIssue) LevelRequest {
	r := LevelRequest{Ref: i.Ref, Number: i.Number(), Title: i.Title, Author: i.Author, Branch: i.Branch, Draft: i.Draft, Opened: i.Opened, Changed: i.Changed, Rank: len(loopRanks)}
	if i.Withheld > 0 {
		r.Title = "(withheld: it matched a confidential pattern)"
	}
	if s := st.sentenceOf(repo, i.Ref); s != nil {
		r.Place = s.Text
		for k, p := range loopRanks {
			if strings.HasPrefix(s.Text, i.Ref+" is "+p) {
				r.Rank = k
				break
			}
		}
	}
	if c, ok := st.CI[ciKey(repo, i.Ref)]; ok {
		r.CI = c.State
		r.Failing = c.State == "failed"
	}
	if m := issueInTitle.FindStringSubmatch(i.Title); m != nil {
		r.Issue = "#" + m[1]
	}
	return r
}

// BuildRepoLevel is level 2 of repo from the fold, with decisions the open
// session decisions; it calls no forge and no model.
func BuildRepoLevel(cfg *Config, st *LogState, repo string, decisions []UIDecision, now time.Time) RepoLevel {
	v := RepoLevel{Repository: repo, Tracked: indexed(cfg.Repositories[repo]) && cfg.Poll.Index, Issues: []LevelIssue{}, Requests: []LevelRequest{}}
	v.Read = st.IndexRead[repo]
	busy := workingOn(cfg, st, repo)
	secret := confidentialMatcher(cfg)
	needs := map[string]bool{}
	for _, ref := range sortedKeys(st.Index[repo]) {
		i := st.Index[repo][ref]
		if ref[0] == '!' {
			v.Requests = append(v.Requests, levelRequest(st, repo, i))
			continue
		}
		li, j, _ := levelIssue(cfg, st, repo, i, decisions, busy, secret)
		if li.Decision != nil {
			needs[ref] = true
		}
		for _, q := range j.Requests {
			for k := range v.Requests {
				if v.Requests[k].Ref == q && v.Requests[k].Issue == "" {
					v.Requests[k].Issue = ref
				}
			}
		}
		v.Issues = append(v.Issues, li)
	}
	for k := range v.Requests {
		if v.Requests[k].Issue != "" {
			continue
		}
		for _, li := range v.Issues {
			if contains(li.Requests, v.Requests[k].Ref) {
				v.Requests[k].Issue = li.Ref
				break
			}
		}
	}
	sortIssues(v.Issues, "needs-me")
	sort.SliceStable(v.Requests, func(a, b int) bool {
		if v.Requests[a].Rank != v.Requests[b].Rank {
			return v.Requests[a].Rank < v.Requests[b].Rank
		}
		return v.Requests[a].Changed.After(v.Requests[b].Changed)
	})
	v.Counts = indexCounts(cfg, st, repo, func(ref string) bool { return needs[ref] })
	v.Sentence = repoSentence(v, now)
	return v
}

// repoSentence says where repo stands, in a sentence.
func repoSentence(v RepoLevel, now time.Time) string {
	switch {
	case !v.Tracked:
		return fmt.Sprintf("agentd does not list %s's issues: it is not a GitHub or GitLab project, or poll.index is off.", v.Repository)
	case v.Read.IsZero():
		return fmt.Sprintf("agentd has not listed %s's issues yet.", v.Repository)
	}
	c := v.Counts
	s := fmt.Sprintf("%s has %s and %s open", v.Repository, plural(c.Open, "issue"), plural(c.Requests, "request"))
	var parts []string
	if c.Waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d %s you", c.Waiting, map[bool]string{true: "needs", false: "need"}[c.Waiting == 1]))
	}
	if c.Working > 0 {
		parts = append(parts, fmt.Sprintf("%d being worked", c.Working))
	}
	if len(parts) > 0 {
		s += ": " + strings.Join(parts, ", ")
	}
	s += "."
	if !c.Last.IsZero() {
		s += " Last change " + uiPage{Now: now}.Ago(c.Last) + " ago."
	}
	return s
}

// issueInTitle finds the first #N a request's title names.
var issueInTitle = regexp.MustCompile(`#([0-9]+)\b`)

// IssueSorts are the issue box's sorts, the first the default.
var IssueSorts = []string{"needs-me", "last-change", "working", "has-mr", "newest", "oldest", "stale", "label", "milestone", "author"}

// RequestSorts are the MR box's sorts, the first the default.
var RequestSorts = []string{"loop", "last-change", "ci-failing", "newest", "oldest"}

// wordRank orders the state words for the needs-me sort.
func wordRank(w string) int {
	switch w {
	case WordNeedsYou:
		return 0
	case WordWorking:
		return 1
	case WordStopped:
		return 2
	}
	return 3
}

// sortIssues sorts the issue box's rows by one of IssueSorts, as ui.js
// does on the page; ties go to the latest change, then the number.
func sortIssues(is []LevelIssue, by string) {
	key := func(i LevelIssue) string {
		v := ""
		switch by {
		case "label":
			v = strings.Join(i.Labels, " ")
		case "milestone":
			v = i.Milestone
		case "author":
			v = i.Author
		}
		if v == "" {
			return "\uffff" // none last, as on the page
		}
		return v
	}
	sort.SliceStable(is, func(a, b int) bool {
		x, y := is[a], is[b]
		switch by {
		case "needs-me":
			if wordRank(x.Word) != wordRank(y.Word) {
				return wordRank(x.Word) < wordRank(y.Word)
			}
		case "working":
			if x.Working != y.Working {
				return x.Working
			}
		case "has-mr":
			if (len(x.Requests) > 0) != (len(y.Requests) > 0) {
				return len(x.Requests) > 0
			}
		case "newest":
			if !x.Opened.Equal(y.Opened) {
				return x.Opened.After(y.Opened)
			}
		case "oldest":
			if !x.Opened.Equal(y.Opened) {
				return x.Opened.Before(y.Opened)
			}
		case "stale":
			if !x.Changed.Equal(y.Changed) {
				return x.Changed.Before(y.Changed)
			}
		case "label", "milestone", "author":
			if key(x) != key(y) {
				return key(x) < key(y)
			}
		}
		if !x.Changed.Equal(y.Changed) {
			return x.Changed.After(y.Changed)
		}
		return x.Number > y.Number
	})
}

// sortRequests sorts the MR box's rows by one of RequestSorts.
func sortRequests(rs []LevelRequest, by string) {
	sort.SliceStable(rs, func(a, b int) bool {
		x, y := rs[a], rs[b]
		switch by {
		case "loop":
			if x.Rank != y.Rank {
				return x.Rank < y.Rank
			}
		case "ci-failing":
			if x.Failing != y.Failing {
				return x.Failing
			}
		case "newest":
			if !x.Opened.Equal(y.Opened) {
				return x.Opened.After(y.Opened)
			}
		case "oldest":
			if !x.Opened.Equal(y.Opened) {
				return x.Opened.Before(y.Opened)
			}
		}
		if !x.Changed.Equal(y.Changed) {
			return x.Changed.After(y.Changed)
		}
		return x.Number > y.Number
	})
}

// filterIssues keeps the rows whose title, sentence, labels or refs hold
// every word of filter, case aside; a word starting with # or ! matches a
// ref exactly.
func filterIssues(is []LevelIssue, filter string) []LevelIssue {
	words := strings.Fields(strings.ToLower(filter))
	if len(words) == 0 {
		return is
	}
	var out []LevelIssue
	for _, i := range is {
		hay := i.FilterText()
		refs := append([]string{i.Ref}, i.Requests...)
		ok := true
		for _, w := range words {
			if w[0] == '#' || w[0] == '!' {
				ok = ok && contains(refs, w)
			} else {
				ok = ok && strings.Contains(hay, w)
			}
		}
		if ok {
			out = append(out, i)
		}
	}
	return out
}

// stepsShown are the steps an issue page shows before "every entry".
const stepsShown = 12

// BuildIssueLevel is level 3 of repo's issue n from the fold; all keeps
// every step. It calls no forge and no model. An issue the index does not
// hold is shown from what joins it, if anything does.
func BuildIssueLevel(cfg *Config, st *LogState, repo string, n int, decisions []UIDecision, all bool, now time.Time) IssueLevel {
	ref := "#" + strconv.Itoa(n)
	busy := workingOn(cfg, st, repo)
	secret := confidentialMatcher(cfg)
	i := st.Index[repo][ref]
	known := i != nil
	if i == nil {
		i = &IndexIssue{IndexPayload: IndexPayload{Repository: repo, Ref: ref}}
	}
	li, j, ds := levelIssue(cfg, st, repo, i, decisions, busy, secret)
	v := IssueLevel{LevelIssue: li, Known: known, Decisions: ds, Steps: []IssueStep{}, Refs: []LevelRef{}}
	for _, it := range j.Work {
		v.Refs = append(v.Refs, LevelRef{Kind: "item", Name: it.ID, Note: it.State})
		if v.Title == "" && !secret(it.Title) {
			v.Title = it.Title
		}
	}
	for _, s := range j.Sessions {
		v.Refs = append(v.Refs, LevelRef{Kind: "session", Name: s})
	}
	for _, q := range j.Requests {
		v.Refs = append(v.Refs, LevelRef{Kind: "request", Name: q})
		if r := st.Index[repo][q]; r != nil {
			v.Request = append(v.Request, levelRequest(st, repo, r))
		}
	}
	if j.Case != "" {
		v.Refs = append(v.Refs, LevelRef{Kind: "case", Name: j.Case})
	}
	if !known && len(v.Refs) == 0 && st.Sentences[sentenceKey(repo, ref)] == nil {
		return v
	}
	v.Known = true
	// The issue's steps and its requests', each once: a loop step is
	// noted on the request and on its issue both.
	var steps []IssueStep
	seen := map[string]bool{}
	for _, r := range append([]string{ref}, j.Requests...) {
		for _, s := range st.Steps[sentenceKey(repo, r)] {
			k := s.At.String() + "\n" + s.Text
			if seen[k] {
				continue
			}
			seen[k] = true
			if secret(s.Text) {
				s.Text = "(withheld: it matched a confidential pattern)"
			}
			steps = append(steps, s)
		}
	}
	sort.SliceStable(steps, func(a, b int) bool { return steps[a].At.Before(steps[b].At) })
	if !all && len(steps) > stepsShown {
		v.More = len(steps) - stepsShown
		steps = steps[len(steps)-stepsShown:]
	}
	v.Steps = append(v.Steps, steps...)
	for _, it := range j.Work {
		if it.terminal() {
			continue
		}
		next := fmt.Sprintf("%s is done when %s.", it.ID, it.DoneWhen.String())
		if secret(next) {
			next = it.ID + "'s end is withheld: it matched a confidential pattern."
		}
		switch {
		case it.State == WorkDesigning:
			next += " Go waits on its designer."
		case it.State == WorkDesigned:
			next += " Go waits on your go."
		case it.Question != "":
			next += " Go waits on your answer."
		case it.Session != "":
			next += fmt.Sprintf(" Go waits on %s.", it.Session)
		default:
			next += " Go waits for a session to take it."
		}
		v.Next = append(v.Next, next)
	}
	for _, r := range v.Request {
		if r.Place != "" {
			v.Next = append(v.Next, r.Place+".")
		}
	}
	if len(v.Next) == 0 && len(ds) == 0 {
		v.Next = append(v.Next, "Nothing is under way on it; an instruction here starts work on it.")
	}
	return v
}

// HomeRow is one repository on the home page: its counts and last change,
// or that agentd does not list its issues.
type HomeRow struct {
	Repository string      `json:"repository"`
	Tracked    bool        `json:"tracked"`
	Listed     bool        `json:"listed"`
	Counts     IndexCounts `json:"counts"`
	Sentence   string      `json:"sentence"`
}

// homeSentence says where the repositories stand together.
func homeSentence(rows []HomeRow) string {
	if len(rows) == 0 {
		return "No repository is configured."
	}
	var need []string
	open, working, requests, waiting, listed := 0, 0, 0, 0, 0
	for _, r := range rows {
		if !r.Listed {
			continue
		}
		listed++
		open += r.Counts.Open
		working += r.Counts.Working
		requests += r.Counts.Requests
		waiting += r.Counts.Waiting
		if r.Counts.Waiting > 0 {
			need = append(need, r.Repository)
		}
	}
	if listed == 0 {
		if len(rows) == 1 {
			return "1 repository configured; agentd does not list its issues yet."
		}
		return fmt.Sprintf("%s configured; agentd lists the issues of none of them yet.", listedRepos(len(rows)))
	}
	s := fmt.Sprintf("Across %s: %s and %s open, %d being worked.", listedRepos(listed), plural(open, "issue"), plural(requests, "request"), working)
	switch {
	case waiting == 1:
		s += " 1 issue waits on you, in " + strings.Join(need, ", ") + "."
	case waiting > 1:
		s += fmt.Sprintf(" %d issues wait on you, in %s.", waiting, strings.Join(need, ", "))
	default:
		s += " Nothing waits on you."
	}
	if n := len(rows) - listed; n > 0 {
		s += fmt.Sprintf(" %s not listed yet.", listedRepos(n))
	}
	return s
}

// BuildHomeRow is repo's row on the home page, from the fold.
func BuildHomeRow(cfg *Config, st *LogState, repo string, decisions []UIDecision, now time.Time) HomeRow {
	v := BuildRepoLevel(cfg, st, repo, decisions, now)
	return HomeRow{Repository: repo, Tracked: v.Tracked, Listed: v.Tracked && !v.Read.IsZero(), Counts: v.Counts, Sentence: v.Sentence}
}

// listedRepos is n repositories, in words.
func listedRepos(n int) string {
	if n == 1 {
		return "1 repository"
	}
	return fmt.Sprintf("%d repositories", n)
}
