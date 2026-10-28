package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The console box at each level (#69). What the owner writes in it is an
// instruction carrying its scope: home, a repository, or one of its
// issues. A question is answered by a thread agent from that level's
// state; an instruction goes to that level's work. Nothing written at
// home or at a repository is handed to a running session; at an issue it
// goes to the issue's session at its next turn, or now when the owner
// says so. A typed answer that is one of the options of the level's one
// open decision is that decision's answer, as its button records it.

// ScopeHome is the home level's scope.
const ScopeHome = "home"

// Scope is a level: home, a repository, or an issue of one.
type Scope struct {
	Repository string
	Issue      int
}

// String is the scope as an instruction records it: home, REPO or REPO#N.
func (c Scope) String() string {
	switch {
	case c.Repository == "":
		return ScopeHome
	case c.Issue > 0:
		return c.Repository + "#" + strconv.Itoa(c.Issue)
	}
	return c.Repository
}

// ParseScope reads home, REPO or REPO#N, the repository one cfg holds.
func ParseScope(cfg *Config, s string) (Scope, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == ScopeHome {
		return Scope{}, nil
	}
	repo, n := s, 0
	if i := strings.LastIndex(s, "#"); i > 0 {
		v, err := strconv.Atoi(s[i+1:])
		if err != nil || v <= 0 {
			return Scope{}, fmt.Errorf("%q is not home, a repository or REPO#N", s)
		}
		repo, n = s[:i], v
	}
	if _, ok := cfg.Repositories[repo]; !ok {
		return Scope{}, fmt.Errorf("no repository %q", repo)
	}
	return Scope{Repository: repo, Issue: n}, nil
}

// homeQuestion says whether p was written at home naming no repository:
// it is the home thread's, for no repository or console.
func (p *InstructionPayload) homeQuestion() bool {
	return p.Scope == ScopeHome && len(p.Repositories) == 0 && p.Console == ""
}

// scopeOf is an instruction's scope, as recorded.
func scopeOf(scope string) Scope {
	if scope == "" || scope == ScopeHome {
		return Scope{}
	}
	if i := strings.LastIndex(scope, "#"); i > 0 {
		if n, err := strconv.Atoi(scope[i+1:]); err == nil && n > 0 {
			return Scope{Repository: scope[:i], Issue: n}
		}
	}
	return Scope{Repository: scope}
}

// scopeHandsOver says whether a message of scope may be handed to a
// running session: one with no scope, as before, or one at an issue.
func scopeHandsOver(scope string) bool { return scope == "" || scopeOf(scope).Issue > 0 }

// handOverNote is what the owner is told of a hand-over not followed.
const handOverNote = "\n\n(Written at this level, it goes to no running session: write at the issue's page, or to the session, to reach one.)"

// dropHandOver takes the hand-over out of a thread agent's answer, and
// says so in the answer when there was one; an answer that does not read
// is left for the check to refuse.
func dropHandOver(raw json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	var to, answer string
	json.Unmarshal(m["hand_to"], &to)
	if strings.TrimSpace(to) == "" {
		return raw
	}
	json.Unmarshal(m["answer"], &answer)
	m["hand_to"], _ = json.Marshal("")
	if len([]rune(answer+handOverNote)) <= agentAnswer {
		m["answer"], _ = json.Marshal(answer + handOverNote)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

// issueWords finds #N in an instruction's words.
var issueWords = regexp.MustCompile(`(?:^|[^\w&!])#([0-9]+)\b`)

// scopedIssue sets the issue of work read in an instruction written at an
// issue, or at a repository naming exactly one issue, when the work names
// none.
func scopedIssue(in *Instruction, w *ThreadWork) {
	if in == nil || in.Scope == "" || w.Issue > 0 {
		return
	}
	if c := scopeOf(in.Scope); c.Issue > 0 {
		w.Issue = c.Issue
		return
	}
	if scopeOf(in.Scope).Repository == "" {
		return
	}
	if ms := issueWords.FindAllStringSubmatch(in.Text, -1); len(ms) > 0 {
		n := ms[0][1]
		for _, m := range ms[1:] {
			if m[1] != n {
				return // more than one: the agent's work says which
			}
		}
		w.Issue, _ = strconv.Atoi(n)
	}
}

// namedRepository is the one configured repository words name as a word,
// or "".
func namedRepository(cfg *Config, text string) string {
	found := ""
	lower := strings.ToLower(text)
	for _, r := range sortedKeys(cfg.Repositories) {
		if regexp.MustCompile(`(^|[^\w/-])` + regexp.QuoteMeta(strings.ToLower(r)) + `($|[^\w/-])`).MatchString(lower) {
			if found != "" {
				return ""
			}
			found = r
		}
	}
	return found
}

// ScopeInstruction records what the owner wrote in a level's console box.
// At home it goes to the repository it names, if one, else to the home
// thread; at a repository to that repository; at an issue to the issue's
// session, to, when it has one that takes messages, else to the issue's
// repository. now asks for the issue's session to be typed into at once.
func ScopeInstruction(cfg *Config, log *Log, at time.Time, c Scope, to, text string, now bool, files []stagedAttachment) error {
	if strings.TrimSpace(text) == "" && len(files) == 0 {
		return errors.New("an empty instruction")
	}
	repo := c.Repository
	if c.Repository == "" {
		repo = namedRepository(cfg, text)
		to, now = "", false
		if repo == "" && len(files) > 0 {
			return errors.New("a question at home takes no files: send them at a repository or an issue")
		}
	} else if c.Issue == 0 {
		to, now = "", false
	}
	if to != "" {
		if sess, ok := cfg.Sessions[to]; !ok || sess.Repository != repo || sess.Role != "" {
			return fmt.Errorf("%s is not a session of %s that takes messages", to, repo)
		}
	}
	var repos []string
	if repo != "" {
		repos = []string{repo}
	}
	scope := c.String()
	return recordInstructionRe(cfg, log, at, repos, "", to, "ui", text, nil, nil, files, func(p *InstructionPayload) {
		p.Scope, p.Now = scope, now && to != ""
	})
}

// homeInput is what the home thread is given: every repository's state as
// its level says it, the open decisions, and the tokens.
type homeInput struct {
	Message      string       `json:"message"`
	At           time.Time    `json:"at"`
	Repositories []homeRepo   `json:"repositories"`
	Today        TodayTokens  `json:"today_tokens"`
	Earlier      []homeRecord `json:"earlier,omitempty"`
}

// homeRepo is one repository as the home thread is given it.
type homeRepo struct {
	Name     string      `json:"name"`
	Sentence string      `json:"sentence"`
	NeedsYou []homeIssue `json:"needs_you,omitempty"`
	Working  []homeIssue `json:"working,omitempty"`
	Requests []string    `json:"requests,omitempty"`
}

// homeIssue is an issue as the home thread is given it.
type homeIssue struct {
	Ref      string `json:"ref"`
	Title    string `json:"title"`
	Sentence string `json:"sentence,omitempty"`
	Asks     string `json:"asks,omitempty"`
}

// homeRecord is an earlier exchange at home.
type homeRecord struct {
	At     time.Time `json:"at"`
	Owner  string    `json:"owner"`
	Answer string    `json:"answer,omitempty"`
}

// homeKept bounds what the home thread is given of each repository.
const homeKept = 12

const homeSystem = `You answer the owner of several software repositories on agentd's home console. You are given each repository's state as agentd's pages say it: a sentence, the issues that wait on the owner, those being worked, and the open requests, and today's token use. Answer the owner's message from those facts only, briefly, naming issues as REPO#N. Say when the facts do not hold the answer; never guess. You cannot start work or message a session from here: if the owner asks for work, say to write it at the repository or the issue. Answer with the JSON the schema asks for.`

func homeSchema() []byte {
	return []byte(`{"type":"object","properties":{"answer":{"type":"string","maxLength":4000}},"required":["answer"],"additionalProperties":false}`)
}

// homeThreadInput builds the home thread's input for in from the fold,
// screened.
func (s *Supervisor) homeThreadInput(in *Instruction, now time.Time) homeInput {
	x := homeInput{Message: in.Text, At: now, Today: todayTokens([]*AccountLedger{s.st.Ledger}, now, time.Local)}
	ds := DecisionsFrom(s.cfg, s.st, ReportFrom(s.cfg, s.st, "", now))
	keep := func(t string) string { return s.keepText(t) }
	for _, repo := range sortedKeys(s.cfg.Repositories) {
		v := BuildRepoLevel(s.cfg, s.st, repo, ds, now)
		r := homeRepo{Name: repo, Sentence: keep(v.Sentence)}
		for _, i := range v.Issues {
			hi := homeIssue{Ref: repo + i.Ref, Title: keep(i.Title), Sentence: keep(i.Sentence)}
			switch {
			case i.Decision != nil && len(r.NeedsYou) < homeKept:
				hi.Asks = keep(i.Decision.Asked)
				r.NeedsYou = append(r.NeedsYou, hi)
			case i.Working && len(r.Working) < homeKept:
				r.Working = append(r.Working, hi)
			}
		}
		for _, q := range v.Requests {
			if len(r.Requests) < homeKept {
				r.Requests = append(r.Requests, keep(q.Ref+" "+q.Title+": "+q.Place))
			}
		}
		x.Repositories = append(x.Repositories, r)
	}
	for _, e := range s.st.scopeConversation(ScopeHome, 6) {
		if e.ID != in.ID {
			x.Earlier = append(x.Earlier, homeRecord{At: e.At, Owner: keep(e.Text), Answer: keep(e.Answer)})
		}
	}
	return x
}

// startHomeThread answers a home question in a worker, with one call on
// the jobs' model, given the home input; the answer is the instruction's,
// as a thread agent's is.
func (s *Supervisor) startHomeThread(in *Instruction, now time.Time) {
	key := "thread:" + in.ID
	if s.work.busy(key) {
		return
	}
	if in.Withheld > 0 {
		s.routeStep(in, RouteFail, "", false, fmt.Sprintf("its text matched confidential pattern %d, so it was never logged", in.Withheld))
		return
	}
	if s.emit("", Event{Kind: KindRoute, Route: &RoutePayload{Instruction: in.ID, Step: RouteAgent}}); s.halt != nil {
		return
	}
	x := s.homeThreadInput(in, now)
	job := threadJob(s.cfg)
	s.spawnShared(key, func(w *Supervisor) {
		p := &AgentPayload{Model: job.Model}
		input, err := json.Marshal(x)
		// Over the bound, the lists go, the least pressing first.
		for step := 0; err == nil && len(input) > job.MaxInput && step < 3; step++ {
			switch step {
			case 0:
				for k := range x.Repositories {
					x.Repositories[k].Working, x.Repositories[k].Requests = nil, nil
				}
			case 1:
				x.Earlier = nil
			case 2:
				for k := range x.Repositories {
					if len(x.Repositories[k].NeedsYou) > 3 {
						x.Repositories[k].NeedsYou = x.Repositories[k].NeedsYou[:3]
					}
				}
			}
			input, err = json.Marshal(x)
		}
		if err == nil && len(input) > job.MaxInput {
			err = fmt.Errorf("the home input is %d bytes, over the %d allowed", len(input), job.MaxInput)
		}
		if err == nil {
			p.InputBytes = len(input)
			var raw json.RawMessage
			var usage JobUsage
			raw, usage, err = job.Run(w.runCtx, homeSystem, homeSchema(), input)
			p.Usage = &usage
			if err == nil {
				var a struct {
					Answer string `json:"answer"`
				}
				if err = json.Unmarshal(raw, &a); err == nil {
					if p.Answer = strings.TrimSpace(a.Answer); p.Answer == "" {
						err = errors.New("the answer is empty")
					}
				}
			}
		}
		if w.runCtx.Err() != nil {
			return
		}
		if err != nil {
			p.Answer, p.Error = "", cutUTF8(err.Error(), 300)
		}
		if i := w.confidentialMatch(p.Answer); i > 0 {
			p.Answer, p.Withheld = "", i
		}
		w.emit("", Event{Kind: KindRoute, Route: &RoutePayload{Instruction: in.ID, Step: RouteAnswered, Agent: p}})
	})
}

// ScopeEntry is one exchange of a level's conversation: what the owner
// wrote, and what came of it.
type ScopeEntry struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Text    string    `json:"text"`
	Answer  string    `json:"answer,omitempty"`
	Session string    `json:"session,omitempty"`
	State   string    `json:"state"`
}

// scopeConversation is the latest n exchanges at scope, newest last.
func (st *LogState) scopeConversation(scope string, n int) []ScopeEntry {
	var out []ScopeEntry
	for _, in := range st.Instructions {
		if in.Scope != scope {
			continue
		}
		e := ScopeEntry{ID: in.ID, At: in.At, Text: in.Text, Session: in.Session}
		if in.Withheld > 0 {
			e.Text = withheldMessage
		}
		switch {
		case in.Reply != "":
			e.Answer, e.State = in.Reply, "answered"
		case in.AgentLast != nil && in.AgentLast.Answer != "":
			e.Answer, e.State = in.AgentLast.Answer, "answered"
		case in.Failed != "":
			e.Answer, e.State = in.Failed, "failed"
		case in.AgentLast != nil && in.AgentLast.Error != "":
			e.Answer, e.State = in.AgentLast.Error, "failed"
		case in.Waiting != "":
			e.Answer, e.State = in.Waiting, "waiting"
		case !in.Delivered.IsZero() && in.Session != "":
			e.State = "with " + in.Session
		case in.Session != "":
			e.State = "for " + in.Session + " at its next turn"
		default:
			e.State = "received"
		}
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].At.Before(out[b].At) })
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// typedDecision is the entry and answer text names, when it is one of the
// options of the only open decision of decisions: its key or label, or
// yes or no for a yes/no decision.
func typedDecision(decisions []LevelDecision, text string) (string, *AnswerPayload) {
	if len(decisions) != 1 {
		return "", nil
	}
	d := decisions[0]
	t := strings.ToLower(strings.TrimSpace(strings.TrimRight(strings.TrimSpace(text), ".!")))
	if d.Decision != nil && d.Decision.Type == DecisionYesNo && (t == "yes" || t == "no") {
		yes := t == "yes"
		return d.Entry, &AnswerPayload{ID: d.Decision.ID, Answer: &yes}
	}
	for _, o := range d.Options {
		if t != "" && (t == strings.ToLower(o.Key) || t == strings.ToLower(o.Label)) {
			a := &AnswerPayload{Choice: o.Key}
			if d.Decision != nil {
				a.ID = d.Decision.ID
			}
			return d.Entry, a
		}
	}
	return "", nil
}
