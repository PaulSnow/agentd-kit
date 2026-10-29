package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// The owner's three levels (#69): home, a repository, an issue. Each says
// where things stand in sentences written when their events happened,
// read from the fold alone; building a level calls no forge and no model.
// The pages and the CLI print the same views.

// IssueStep is one dated step of an issue or request, in a sentence a
// person would write: things by their names, never by their ids, which
// are in Chips alone. Key marks a step the trim of the ring keeps.
type IssueStep struct {
	At    time.Time `json:"at"`
	Ref   string    `json:"ref"`
	Text  string    `json:"text"`
	Chips []StepRef `json:"chips,omitempty"`
	Key   bool      `json:"key,omitempty"`
}

// StepRef is what a step is about, by its id: a work item, a session or
// a request; Link is where it opens, set when the page is built.
type StepRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Link string `json:"link,omitempty"`
}

// stepsKept bounds the steps the fold keeps per issue or request.
const stepsKept = 100

// noteStep adds a step to ref's steps in repo.
func (st *LogState) noteStep(repo, ref string, at time.Time, text string) {
	st.addStep(repo, IssueStep{At: at, Ref: ref, Text: text})
}

// addStep adds step s to its ref's steps in repo, unless it says what the
// latest says. Past stepsKept the oldest step goes, but one that matters
// (Key: opened, a design approved or declined, a session started, merged,
// done) goes only when the ring holds nothing else.
func (st *LogState) addStep(repo string, s IssueStep) {
	if repo == "" || s.Ref == "" || strings.TrimSpace(s.Text) == "" {
		return
	}
	if st.Steps == nil {
		st.Steps = map[string][]IssueStep{}
	}
	k := sentenceKey(repo, s.Ref)
	steps := st.Steps[k]
	if n := len(steps); n > 0 && steps[n-1].Text == s.Text && sameChips(steps[n-1].Chips, s.Chips) {
		return
	}
	s.Text = cutUTF8(s.Text, 400)
	steps = append(steps[:len(steps):len(steps)], s)
	if len(steps) > stepsKept {
		// The oldest step that does not matter goes; when every older
		// one matters, the new one goes unless it matters too.
		drop := 0
		for drop < len(steps)-1 && steps[drop].Key {
			drop++
		}
		if drop == len(steps)-1 && s.Key {
			drop = 0
		}
		steps = append(append([]IssueStep(nil), steps[:drop]...), steps[drop+1:]...)
	}
	st.Steps[k] = steps
}

// sameChips says whether two steps' chips are the same.
func sameChips(a, b []StepRef) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if a[k] != b[k] {
			return false
		}
	}
	return true
}

// stepIDs are the ids a reason Go or a tool wrote may hold: an item's, a
// session's hex id, a request's or an issue's number.
var stepIDs = regexp.MustCompile(`\bW[0-9a-f]{8}\b|\b[0-9a-f]{12,}\b|[#!][0-9]+\b`)

// scrubIDs is a reason with its ids put as words, for a step's sentence.
func scrubIDs(s string) string {
	return stepIDs.ReplaceAllStringFunc(s, func(id string) string {
		switch {
		case id[0] == '!':
			return "a request"
		case id[0] == '#':
			return "an issue"
		case id[0] == 'W':
			return "an item"
		}
		return "a session"
	})
}

// itemName is a work item by its title, as a sentence names it: "the
// item to pick the cache".
func itemName(it *WorkItem) string {
	t := oneLine(it.Title, 120)
	if strings.TrimSpace(t) == "" {
		return "the work item"
	}
	r := []rune(t)
	if len(r) == 1 || len(r) > 1 && !unicode.IsUpper(r[1]) {
		r[0] = unicode.ToLower(r[0])
	}
	return "the item to " + strings.TrimSuffix(string(r), ".")
}

// capital is s with its first letter a capital.
func capital(s string) string {
	r := []rune(s)
	if len(r) > 0 {
		r[0] = unicode.ToUpper(r[0])
	}
	return string(r)
}

// requestName is a request by its title, as a sentence names it: "the
// request “Speed up the page”", or "its request" with no title known.
func (st *LogState) requestName(repo, ref string) string {
	i := st.Index[repo][ref]
	if i == nil {
		i = st.Closed[repo][ref]
	}
	if i == nil || i.Title == "" {
		return "its request"
	}
	return "the request “" + oneLine(i.Title, 120) + "”"
}

// roleName is how a step names a session of kind: a reviewer, a judge,
// or a session.
func roleName(kind string) string {
	switch kind {
	case KindReviewer:
		return "A reviewer"
	case KindJudge:
		return "A judge"
	}
	return "A session"
}

// noteSteps folds what event e did to an issue or request into its steps,
// after e is applied, each as one sentence: a work item's steps on its
// issue, a session started on one, a request's loop steps and its merge,
// on the request and its case's issue. The index notes its own; a
// sentence rewritten is no step.
func (st *LogState) noteSteps(e Event) {
	switch {
	case e.Kind == KindWorkItem && e.Work != nil:
		p := e.Work
		it := st.Work[p.ID]
		if it == nil || it.Issue <= 0 {
			return
		}
		name := itemName(it)
		s := IssueStep{At: e.Time, Ref: "#" + strconv.Itoa(it.Issue), Chips: []StepRef{{Kind: "item", ID: it.ID}}}
		switch p.Step {
		case WorkStepOpen:
			s.Text, s.Key = capital(name)+" was opened.", true
		case WorkStepAssign:
			s.Text = capital(name) + " was given to a session."
			if p.Session != "" {
				s.Chips = append(s.Chips, StepRef{Kind: "session", ID: p.Session})
			}
		case WorkStepDesign:
			if p.Design != nil && p.Design.Error == "" {
				s.Text = capital(name) + " was designed."
				if p.Design.Round > 1 {
					s.Text = fmt.Sprintf("%s was designed again, a round %d design.", capital(name), p.Design.Round)
				}
			}
		case WorkStepGo:
			s.Text, s.Key = capital(name)+"'s design was approved.", true
		case WorkStepChange:
			s.Text = capital(name) + "'s design was sent back with your words."
		case WorkStepBlocked:
			s.Text = fmt.Sprintf("%s asks you: %s", capital(name), oneLine(it.Question, 200))
		case WorkStepClaim:
			s.Text = fmt.Sprintf("Its session says %s is done.", name)
		case WorkStepCheck:
			if p.Check != nil {
				s.Text = fmt.Sprintf("Go checked whether %s is done: not yet.", name)
				if p.Check.OK {
					s.Text = fmt.Sprintf("Go checked whether %s is done: it is.", name)
				}
			}
		case WorkStepDone:
			s.Text, s.Key = capital(name)+" is done.", true
		case WorkStepDeclined:
			s.Text, s.Key = capital(name)+" was declined.", true
		}
		st.addStep(it.Repository, s)
	case e.Kind == KindSession && e.Runtime != nil && e.Runtime.Issue > 0 && e.Runtime.Action == SessionStart:
		// The clock time is the zone agentd runs in, as its pages show.
		s := IssueStep{At: e.Time, Ref: "#" + strconv.Itoa(e.Runtime.Issue), Key: true,
			Text: fmt.Sprintf("%s started on it at %s.", roleName(e.Runtime.Kind), e.Time.In(time.Local).Format("15:04"))}
		if name := e.Session; name != "" {
			s.Chips = []StepRef{{Kind: "session", ID: name}}
		} else if e.Runtime.ID != "" {
			s.Chips = []StepRef{{Kind: "session", ID: e.Runtime.ID}}
		}
		st.addStep(e.Runtime.Repository, s)
	case e.Kind == KindLoop && e.Loop != nil && e.Mode != ModeOwner:
		p := e.Loop
		name := st.requestName(p.Repository, p.Ref)
		s := IssueStep{At: e.Time, Ref: p.Ref, Chips: []StepRef{{Kind: "request", ID: p.Ref}}}
		switch p.Step {
		case LoopReview:
			s.Text = fmt.Sprintf("Review round %d started on %s.", p.Round, name)
		case LoopJudge:
			s.Text = fmt.Sprintf("A judge took up round %d of %s.", p.Round, name)
		case LoopFix:
			s.Text = fmt.Sprintf("Its session was sent back to fix what round %d of %s found.", p.Round, name)
		case LoopApprove:
			s.Text = fmt.Sprintf("The loop approved %s.", name)
		case LoopWait:
			what := "reviewer"
			if p.Waits == LoopJudge {
				what = "judge"
			}
			s.Text = fmt.Sprintf("The %s of round %d of %s waits for a place under sessions.max.", what, p.Round, name)
		case LoopFailed:
			what := "review"
			if p.Failed == LoopJudge {
				what = "judging"
			}
			s.Text = fmt.Sprintf("The %s of %s failed: %s.", what, name, strings.TrimSuffix(scrubIDs(oneLine(p.Reason, 200)), "."))
		}
		st.addStep(p.Repository, s)
		if issue := caseIssue(p.Case); issue != "" {
			s.Ref = issue
			st.addStep(p.Repository, s)
		}
	case e.Kind == KindAction && e.Act != nil && e.Act.Action == ActMerge && requestRef.MatchString(e.Act.Ref):
		a := e.Act
		name := st.requestName(a.Repository, a.Ref)
		s := IssueStep{At: e.Time, Ref: a.Ref, Key: true, Text: capital(name) + " was merged.", Chips: []StepRef{{Kind: "request", ID: a.Ref}}}
		st.addStep(a.Repository, s)
		// Its issue as the loop's steps have it: the case of the session
		// that delivered it, else the issue its title names.
		issue := ""
		if st.cfg != nil {
			issue = caseIssue(st.cfg.caseOf(withRuntime(st.cfg, st.Runtime).Sessions[e.Session]))
		}
		if i := st.Index[a.Repository][a.Ref]; issue == "" && i != nil {
			if k := requestIssue(i.Title); k > 0 {
				issue = "#" + strconv.Itoa(k)
			}
		}
		if issue != "" {
			s.Ref = issue
			st.addStep(a.Repository, s)
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
	// Missing says why it has no sentence: SentenceWithheld or
	// SentenceFailed, "" when none was tried or it has one.
	Missing  string   `json:"missing,omitempty"`
	Requests []string `json:"requests,omitempty"`
	// Decision is the first open decision, whose options the row offers.
	Decision *LevelDecision `json:"decision,omitempty"`
	Working  bool           `json:"working,omitempty"`
	// RunSince is when the longest-running session that works it started,
	// zero when none does: the working sort puts the longest run first.
	RunSince time.Time `json:"run_since,omitempty"`
	// MRRank is where its foremost request stands in the loop, as
	// loopRanks orders the places; len(loopRanks)+1 with none.
	MRRank int `json:"mr_rank"`
	// IDs are its items' and sessions' ids, which the filter finds it by.
	IDs []string `json:"ids,omitempty"`
	// URL is the issue's page on its forge, "" when none is configured.
	URL string `json:"url,omitempty"`
}

// FilterText is what the filter's words are matched in: title,
// sentence, labels, milestone and author, in lower case.
func (i LevelIssue) FilterText() string {
	return strings.ToLower(i.Title + " " + i.Sentence + " " + strings.Join(i.Labels, " ") + " " + i.Milestone + " " + i.Author + " " + strings.Join(i.IDs, " "))
}

// RunUnix is RunSince for the page's script, 0 for none.
func (i LevelIssue) RunUnix() int64 {
	if i.RunSince.IsZero() {
		return 0
	}
	return i.RunSince.Unix()
}

// WordClass is the state word as a class name.
func (i LevelIssue) WordClass() string { return strings.ReplaceAll(i.Word, " ", "-") }

// Anchor is the decision card's id on its issue's page.
func (d LevelDecision) Anchor() string {
	return "decision-" + strings.NewReplacer(":", "-", "/", "-", "#", "n").Replace(d.Entry)
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
	// URL is the request's page on its forge, "" when none is configured.
	URL string `json:"url,omitempty"`
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
	// Prompt is what the card's words field asks for: the answer itself,
	// for a question the owner answers in words.
	Prompt string `json:"prompt,omitempty"`
	// Command answers it on the command line, KEY one of the options';
	// "" for agentd console reply --entry.
	Command string `json:"command,omitempty"`
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
	// Needs are what waits on the owner that no listed issue shows: a
	// session's decision, an item, or a request the loop waits on the
	// owner for, whose issue is not listed or who has none.
	Needs []LevelDecision `json:"needs,omitempty"`
	// URL is the repository's page on its forge, "" when none is
	// configured.
	URL string `json:"url,omitempty"`
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
		if ld, ok := workDecision(it); ok {
			out = append(out, ld)
		}
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
		out = append(out, sessionDecision(d))
	}
	return out
}

// workDecision is work item it's card, when it waits on the owner: its
// design waiting on a go, its question, or its claim of being done under
// the owner's own check.
func workDecision(it *WorkItem) (LevelDecision, bool) {
	if it.terminal() {
		return LevelDecision{}, false
	}
	if it.State == WorkDesigned {
		d := designDecision(it, "")
		ld := LevelDecision{Entry: entryDesign + it.ID, Session: it.Session, Asked: d.Question}
		switch {
		case it.DesignWithheld:
			ld.CameBack = fmt.Sprintf("The designer's round %d brief was withheld: it matched a confidential pattern.", it.Round)
		case it.Design != nil && it.Design.Brief != nil:
			ld.CameBack = fmt.Sprintf("The designer's round %d brief.", it.Design.Round)
		}
		if it.Design != nil && it.Design.Rating != nil && !it.DesignWithheld {
			ld.Checked = "Go checked whether the design is contained; it waits on your go."
		}
		for _, o := range d.Options {
			ld.Options = append(ld.Options, LevelOption{Key: o.Key, Label: o.Label, Consequence: o.Consequence})
		}
		return ld, true
	}
	if it.Question == "" && !(it.DoneWhen.Kind == DoneOwner && it.Claim != "") {
		return LevelDecision{}, false
	}
	ld := LevelDecision{Entry: entryWork + it.ID, Session: it.Session}
	if it.State == WorkBlocked {
		ld.Prompt = "Your answer"
	}
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
	return ld, true
}

// sessionDecision is a session's open decision d as a card.
func sessionDecision(d UIDecision) LevelDecision {
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
	return ld
}

// repoNeed is what waits on the owner in a repository, as a card, and the
// issue it is of, "" for none known, so what waits on one issue counts
// once.
type repoNeed struct {
	LevelDecision
	Issue   string
	Request bool // a request the loop waits on the owner for
}

// repoNeeds is everything in repo that waits on the owner, as the old
// console's Needs you lists it and more: every open work item that asks
// or waits on a go, every session's open decision, and every request the
// loop waits on the owner for. An issue's cards are among them.
func repoNeeds(cfg *Config, st *LogState, repo string, decisions []UIDecision, now time.Time) []repoNeed {
	var out []repoNeed
	c := withRuntime(cfg, st.Runtime)
	number := func(n int) string {
		if n <= 0 {
			return ""
		}
		return "#" + strconv.Itoa(n)
	}
	for _, id := range sortedKeys(st.Work) {
		it := st.Work[id]
		if it.Repository != repo {
			continue
		}
		if ld, ok := workDecision(it); ok {
			out = append(out, repoNeed{LevelDecision: ld, Issue: number(it.Issue)})
		}
	}
	for _, d := range decisions {
		if d.Repository == repo {
			out = append(out, repoNeed{LevelDecision: sessionDecision(d), Issue: number(c.Sessions[d.Session].Issue)})
		}
	}
	return append(out, requestNeeds(cfg, st, repo, now)...)
}

// requestNeeds are the cards of repo's requests the loop waits on the
// owner for, each with the words `agentd loop` takes now, and the issue
// each is of.
func requestNeeds(cfg *Config, st *LogState, repo string, now time.Time) []repoNeed {
	var out []repoNeed
	c := withRuntime(cfg, st.Runtime)
	secret := confidentialMatcher(cfg)
	for _, q := range loopRequests(cfg, st, now) {
		if q.Repository != repo {
			continue
		}
		p := placeOf(cfg, st, q, now)
		if p.Phase == PlaceWaitPlace {
			out = append(out, waitNeed(cfg, st, c, q, p, now))
			continue
		}
		if p.Phase != PlaceOwner || p.Do != "" {
			continue // moving, or a step of the loop's own is due
		}
		n := repoNeed{Request: true, LevelDecision: LevelDecision{Entry: entryLoop + q.Ref, Asked: fmt.Sprintf("%s is %s: %s.", q.Ref, PlaceOwner, p.Reason),
			Options: loopOptions(st.Loops[ciKey(repo, q.Ref)]), Command: fmt.Sprintf("agentd loop KEY %s %s", repo, q.Ref)}}
		if secret(n.Asked) {
			n.Asked = fmt.Sprintf("%s is %s; the reason was withheld, as it matched a confidential pattern.", q.Ref, PlaceOwner)
		}
		if k := c.Sessions[q.Session].Issue; k > 0 && !q.Outside {
			n.Issue = "#" + strconv.Itoa(k)
		} else if i := st.Index[repo][q.Ref]; i != nil {
			if k := requestIssue(i.Title); k > 0 {
				n.Issue = "#" + strconv.Itoa(k)
			}
		}
		out = append(out, n)
	}
	return out
}

// waitNeed is the card of q's step p, waiting for a place (#71): end one
// of the holders, each its own option, which the step then takes; or
// raise sessions.max, which the card says in words, as it is the
// configuration's.
func waitNeed(cfg *Config, st *LogState, c *Config, q loopRequest, p loopPlace, now time.Time) repoNeed {
	what := "reviewer"
	if p.Do == LoopJudge {
		what = "judge"
	}
	w := placeWaitOf(cfg, st, q.Repository)
	n := repoNeed{Request: true, LevelDecision: LevelDecision{Entry: entryLoop + q.Ref,
		Asked:   fmt.Sprintf("%s is %s: %s. %s", q.Ref, PlaceWaitPlace, p.Reason, yieldNote(cfg, st, w, now)),
		Command: "agentd sessions end NAME, NAME one of " + strings.Join(p.Holders, ", ") + "; or raise repositories." + q.Repository + ".policy.sessions.max"}}
	for _, h := range p.Holders {
		n.Options = append(n.Options, LevelOption{Key: nextEnd + h, Label: "End " + h + ", then start the " + what, Consequence: h + " is stopped; its workdir and worktree stay"})
	}
	if confidentialMatcher(cfg)(n.Asked) {
		n.Asked = fmt.Sprintf("%s is %s; the reason was withheld, as it matched a confidential pattern.", q.Ref, PlaceWaitPlace)
	}
	if k := c.Sessions[q.Session].Issue; k > 0 && !q.Outside {
		n.Issue = "#" + strconv.Itoa(k)
	} else if i := st.Index[q.Repository][q.Ref]; i != nil {
		if k := requestIssue(i.Title); k > 0 {
			n.Issue = "#" + strconv.Itoa(k)
		}
	}
	return n
}

// Why an issue has no sentence.
const (
	SentenceWithheld = "withheld"
	SentenceFailed   = "failed"
)

// NoSentence says, in a sentence, that the issue has none and why.
func (i LevelIssue) NoSentence() string {
	switch i.Missing {
	case SentenceWithheld:
		return "No sentence: the last one written was withheld, as it matched a confidential pattern; the forge's title stands."
	case SentenceFailed:
		return "No sentence: the last one could not be written; the forge's title stands."
	}
	return "No sentence yet; the forge's title stands."
}

// closedWord is the state word of an issue closed at: closed today, or
// closed on its date.
func closedWord(at, now time.Time) string {
	at = at.In(now.Location())
	if y, m, d := at.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return "closed today"
	}
	if at.Year() != now.Year() {
		return "closed " + at.Format("Jan 2, 2006")
	}
	return "closed " + at.Format("Jan 2")
}

// levelIssue is issue i of repo as a row: its sentence, its word, its
// requests and its first open decision.
func levelIssue(cfg *Config, st *LogState, repo string, i *IndexIssue, decisions []UIDecision, busy map[string]bool, secret func(...string) bool) (LevelIssue, IssueJoin, []LevelDecision) {
	n := i.Number()
	j := joinIssue(cfg, st, repo, n)
	li := LevelIssue{Repository: repo, Ref: i.Ref, Number: n, Title: i.Title, Labels: i.Labels, Milestone: i.Milestone, Author: i.Author,
		Opened: i.Opened, Changed: i.Changed, Requests: j.Requests, Working: busy[i.Ref], Word: WordOpen, URL: webURL(cfg.Repositories[repo], '#', n),
		MRRank: len(loopRanks) + 1}
	for _, it := range j.Work {
		li.IDs = append(li.IDs, it.ID)
	}
	for _, name := range j.Sessions {
		li.IDs = append(li.IDs, name)
		if at := st.StartedAt[name]; li.Working && st.sessionLive(name) && !at.IsZero() && (li.RunSince.IsZero() || at.Before(li.RunSince)) {
			li.RunSince = at
		}
	}
	if i.Withheld > 0 {
		li.Title = "(withheld: it matched a confidential pattern)"
	}
	if s := st.sentenceOf(repo, i.Ref); s != nil {
		li.Sentence, li.Writer, li.SentenceAt = s.Text, s.Writer, s.At
	} else if s := st.Sentences[sentenceKey(repo, i.Ref)]; s != nil && !s.IdleFailed.IsZero() {
		li.Missing = SentenceFailed
		if s.IdleWithheld {
			li.Missing = SentenceWithheld
		}
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
var loopRanks = []string{PlaceOwner, PlaceWaitPlace, PlaceFixing, PlaceJudging, PlaceReviewing, PlaceMerging, PlaceCI, PlaceOutside}

// levelRequest is request i of repo as the MR box shows it.
func levelRequest(cfg *Config, st *LogState, repo string, i *IndexIssue) LevelRequest {
	r := LevelRequest{Ref: i.Ref, Number: i.Number(), Title: i.Title, Author: i.Author, Branch: i.Branch, Draft: i.Draft, Opened: i.Opened, Changed: i.Changed, Rank: len(loopRanks),
		URL: webURL(cfg.Repositories[repo], '!', i.Number())}
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
	if k := requestIssue(i.Title); k > 0 {
		r.Issue = "#" + strconv.Itoa(k)
	}
	return r
}

// BuildRepoLevel is level 2 of repo from the fold, with decisions the open
// session decisions; it calls no forge and no model.
func BuildRepoLevel(cfg *Config, st *LogState, repo string, decisions []UIDecision, now time.Time) RepoLevel {
	v := RepoLevel{Repository: repo, Tracked: indexed(cfg.Repositories[repo]) && cfg.Poll.Index, Issues: []LevelIssue{}, Requests: []LevelRequest{},
		URL: webURL(cfg.Repositories[repo], 0, 0)}
	v.Read = st.IndexRead[repo]
	busy := workingOn(cfg, st, repo)
	secret := confidentialMatcher(cfg)
	needs := map[string]bool{}
	shown := map[string]bool{} // the entries an issue's card shows
	for _, ref := range sortedKeys(st.Index[repo]) {
		i := st.Index[repo][ref]
		if ref[0] == '!' {
			v.Requests = append(v.Requests, levelRequest(cfg, st, repo, i))
			continue
		}
		li, j, ds := levelIssue(cfg, st, repo, i, decisions, busy, secret)
		if li.Decision != nil {
			needs[ref] = true
		}
		for _, d := range ds {
			shown[d.Entry] = true
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
	sort.SliceStable(v.Requests, func(a, b int) bool {
		if v.Requests[a].Rank != v.Requests[b].Rank {
			return v.Requests[a].Rank < v.Requests[b].Rank
		}
		return v.Requests[a].Changed.After(v.Requests[b].Changed)
	})
	// What waits on the owner and no listed issue's card shows: a request
	// the loop waits on the owner for counts its listed issue; the rest
	// are the repository's own needs, counted once an issue they name.
	loose := map[string]bool{}
	cards := map[string]*LevelDecision{} // a listed issue's request card
	for _, n := range repoNeeds(cfg, st, repo, decisions, now) {
		switch {
		case n.Entry != "" && shown[n.Entry]:
		case n.Request && st.Index[repo][n.Issue] != nil:
			needs[n.Issue] = true
			if cards[n.Issue] == nil {
				d := n.LevelDecision
				cards[n.Issue] = &d
			}
		default:
			if secret(n.Asked, n.CameBack, n.Checked) {
				n.Asked, n.CameBack, n.Checked = withheldSecret, "", ""
			}
			v.Needs = append(v.Needs, n.LevelDecision)
			if n.Issue == "" || !loose[n.Issue] {
				v.Counts.Loose++
			}
			loose[n.Issue] = n.Issue != ""
		}
	}
	ranks := map[string]int{}
	for _, r := range v.Requests {
		ranks[r.Ref] = r.Rank
	}
	for k := range v.Issues {
		i := &v.Issues[k]
		if i.Decision == nil && cards[i.Ref] != nil {
			i.Word, i.Decision = WordNeedsYou, cards[i.Ref]
		}
		for _, q := range i.Requests {
			if r, ok := ranks[q]; ok && r < i.MRRank {
				i.MRRank = r
			}
		}
	}
	sortIssues(v.Issues, "needs-me")
	loosely := v.Counts.Loose
	v.Counts = indexCounts(cfg, st, repo, func(ref string) bool { return needs[ref] })
	v.Counts.Loose = loosely
	v.Counts.Waiting += loosely
	v.Sentence = repoSentence(v, now)
	return v
}

// IssuesWaiting are the listed issues that wait on the owner, the issue
// box's own count.
func (v RepoLevel) IssuesWaiting() int { return v.Counts.Waiting - v.Counts.Loose }

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
		// The box counts issues; the sentence counts all that waits, and
		// says how many of those are on no listed issue.
		p := fmt.Sprintf("%d %s you", c.Waiting, map[bool]string{true: "needs", false: "need"}[c.Waiting == 1])
		switch {
		case c.Loose >= c.Waiting:
			p += ", on no listed issue"
		case c.Loose > 0:
			p += fmt.Sprintf(", %d on no listed issue", c.Loose)
		}
		parts = append(parts, p)
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
	if c.Backfill != nil {
		s += fmt.Sprintf(" Sentences being written, %d of %d done.", c.Backfill.Done, c.Backfill.Of)
	}
	return s
}

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
			return "\U0010FFFF" // none last, as on the page
		}
		// A-Z as a-z, compared byte by byte, which is by code point, in no
		// locale, as the page does.
		return strings.Map(func(r rune) rune {
			if 'A' <= r && r <= 'Z' {
				return r + 'a' - 'A'
			}
			return r
		}, v)
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
			if rx, ry := x.RunUnix(), y.RunUnix(); x.Working && rx != ry {
				return rx != 0 && (ry == 0 || rx < ry) // the longest run first
			}
		case "has-mr":
			if (len(x.Requests) > 0) != (len(y.Requests) > 0) {
				return len(x.Requests) > 0
			}
			if len(x.Requests) > 0 && x.MRRank != y.MRRank {
				return x.MRRank < y.MRRank
			}
		case "newest":
			if x.Number != y.Number {
				return x.Number > y.Number
			}
		case "oldest":
			if x.Number != y.Number {
				return x.Number < y.Number
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
			if x.Number != y.Number {
				return x.Number > y.Number
			}
		case "oldest":
			if x.Number != y.Number {
				return x.Number < y.Number
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
	if i == nil {
		i = st.Closed[repo][ref]
	}
	known := i != nil
	if i == nil {
		i = &IndexIssue{IndexPayload: IndexPayload{Repository: repo, Ref: ref}}
	}
	li, j, ds := levelIssue(cfg, st, repo, i, decisions, busy, secret)
	if !i.Closed.IsZero() {
		li.Word = closedWord(i.Closed, now)
	}
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
			v.Request = append(v.Request, levelRequest(cfg, st, repo, r))
		}
	}
	if j.Case != "" {
		v.Refs = append(v.Refs, LevelRef{Kind: "case", Name: j.Case})
	}
	if !known && len(v.Refs) == 0 && st.Sentences[sentenceKey(repo, ref)] == nil {
		return v
	}
	v.Known = true
	for _, n := range requestNeeds(cfg, st, repo, now) {
		if n.Issue == ref {
			v.Decisions = append(v.Decisions, n.LevelDecision)
			v.Word = WordNeedsYou
			if v.Decision == nil {
				v.Decision = &v.Decisions[len(v.Decisions)-1]
			}
		}
	}
	// The issue's steps and its requests', each once: a loop step is
	// noted on the request and on its issue both.
	var steps []IssueStep
	seen := map[string]bool{}
	for _, r := range append(append([]string{ref}, j.Requests...), j.Closed...) {
		for _, s := range st.Steps[sentenceKey(repo, r)] {
			k := s.At.String() + "\n" + s.Text
			if seen[k] {
				continue
			}
			seen[k] = true
			if secret(s.Text) {
				s.Text = "(withheld: it matched a confidential pattern)"
			}
			chips := make([]StepRef, len(s.Chips))
			for k, c := range s.Chips {
				if c.Kind == "request" && contains(j.Requests, c.ID) {
					c.Link = "#mr-" + c.ID[1:]
				}
				// A step written before the request was listed names it
				// now that its title is known.
				if name := st.requestName(repo, c.ID); c.Kind == "request" && name != "its request" && !secret(name) {
					s.Text = strings.Replace(s.Text, "its request", name, 1)
				}
				chips[k] = c
			}
			s.Chips = chips
			steps = append(steps, s)
		}
	}
	sort.SliceStable(steps, func(a, b int) bool { return steps[a].At.Before(steps[b].At) })
	if !all && len(steps) > stepsShown {
		v.More = len(steps) - stepsShown
		steps = steps[len(steps)-stepsShown:]
	}
	v.Steps = append(v.Steps, steps...)
	asked := map[string]bool{}
	for _, it := range j.Work {
		// A message that asked for several items says how many it got,
		// once (#68).
		if in := st.Instructions[it.Instruction]; in != nil && !asked[in.ID] && (in.Asked > 1 || len(in.Works) > 1) {
			asked[in.ID] = true
			v.Next = append(v.Next, "The message of "+it.ID+": "+opened(in, st.workOf(in.ID)))
		}
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
		case it.State == WorkDesigned && it.DesignWithheld:
			next += " Go waits on your change or decline."
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
	// URL is the repository's page on its forge, "" when none is
	// configured.
	URL string `json:"url,omitempty"`
}

// homeSentence says where the repositories stand together.
func homeSentence(rows []HomeRow) string {
	if len(rows) == 0 {
		return "No repository is configured."
	}
	var need []string
	open, working, requests, waiting, loose, listed := 0, 0, 0, 0, 0, 0
	for _, r := range rows {
		if !r.Listed {
			continue
		}
		listed++
		open += r.Counts.Open
		working += r.Counts.Working
		requests += r.Counts.Requests
		waiting += r.Counts.Waiting
		loose += r.Counts.Loose
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
	// Issues only when every wait is on a listed issue; a decision or a
	// question on none is a thing.
	what := "issue"
	if loose > 0 {
		what = "thing"
	}
	switch {
	case waiting == 1:
		s += " 1 " + what + " waits on you, in " + strings.Join(need, ", ") + "."
	case waiting > 1:
		s += fmt.Sprintf(" %d %ss wait on you, in %s.", waiting, what, strings.Join(need, ", "))
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
	return HomeRow{Repository: repo, Tracked: v.Tracked, Listed: v.Tracked && !v.Read.IsZero(), Counts: v.Counts, Sentence: v.Sentence, URL: v.URL}
}

// listedRepos is n repositories, in words.
func listedRepos(n int) string {
	if n == 1 {
		return "1 repository"
	}
	return fmt.Sprintf("%d repositories", n)
}
