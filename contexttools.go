package main

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"
)

// The context tools (#60) on agentd's tool server: state and find are the
// normal path, history is read with a reason, and ask_expert asks a
// stronger model (expert.go). Every answer is compact JSON and bounded.

// Context tool names.
const (
	ToolState     = "state"
	ToolFind      = "find"
	ToolHistory   = "history"
	ToolAskExpert = "ask_expert"
)

// historyIDs is the most ids one history or ask_expert call names.
const historyIDs = 50

// contextTools are offered on every server, after a session's own.
var contextTools = []string{ToolState, ToolFind, ToolHistory, ToolAskExpert, ToolIssueBody}

// RoleQuery is a server that serves the context tools alone, for an
// agent that works no case: a console thread's agent (#59).
const RoleQuery = "query"

func contextToolList() []MCPTool {
	state := `{"type":"object","properties":{"topic":{"type":"string","description":"#N for an issue, !N for a request, session:NAME, or leave out for the repository's own card"}},"additionalProperties":false}`
	find := `{"type":"object","properties":{"q":{"type":"string","description":"words, or a reference such as #56"},"limit":{"type":"integer","minimum":1,"maximum":50}},"required":["q"],"additionalProperties":false}`
	history := `{"type":"object","properties":{` +
		`"ids":{"type":"array","items":{"type":"string"},"description":"record ids, from a card's src"},` +
		`"topic":{"type":"string"},` +
		`"since":{"type":"string","description":"RFC 3339 or YYYY-MM-DD"},` +
		`"kinds":{"type":"array","items":{"enum":["` + strings.Join(HistKinds, `","`) + `"]}},` +
		`"budget_tokens":{"type":"integer","description":"default 4000, at most 32000"},` +
		`"more":{"type":"string","description":"the more of the page before"},` +
		`"why":{"enum":["spec","debug-decision","verify-claim","missing-from-state"]},` +
		`"question":{"type":"string","description":"for missing-from-state: what state lacked; the card's curator is asked to add it"}},` +
		`"required":["why"],"additionalProperties":false}`
	expert := `{"type":"object","properties":{` +
		`"question":{"type":"string"},` +
		`"record_ids":{"type":"array","items":{"type":"string"},"description":"history records the expert is given whole; cards' src ids"},` +
		`"topics":{"type":"array","items":{"type":"string"},"description":"topics whose cards the expert is given"},` +
		`"budget_tokens":{"type":"integer","description":"the most the records may take, default 8000"}},` +
		`"required":["question"],"additionalProperties":false}`
	return []MCPTool{
		{Name: ToolState, Description: "What is true now about a topic: its card, with each claim's src record ids. Read this first; leave topic out for the repository's card.", InputSchema: json.RawMessage(state)},
		{Name: ToolFind, Description: "The topics whose cards match words or a reference, best first; topics with history but no card yet say card false.", InputSchema: json.RawMessage(find)},
		{Name: ToolHistory, Description: "History records, only when state is not enough: by ids, or by topic newest first, cut to budget_tokens. Say why; missing-from-state asks the card's curator to add what was missing.", InputSchema: json.RawMessage(history)},
		{Name: ToolIssueBody, Description: "Write the body of an issue you are filing: Go quotes the owner's words from the records you name, lists your facts and the records, and gives the decisions so far from the topics' cards. File it with the body as returned.", InputSchema: issueBodySchema()},
		{Name: ToolAskExpert, Description: "Ask a stronger model, with the records and cards you name. It answers as JSON: answer, confidence and the record ids it used. Say in your answer when you relied on it.", InputSchema: json.RawMessage(expert)},
	}
}

// tools are the tools the server offers: a case's own, then the context
// tools when the server knows its repository.
func (srv *MCPServer) tools() []string {
	if srv.sess.Role == RoleQuery {
		return contextTools
	}
	tools := toolsFor(srv.sess.Role)
	if srv.sess.Repository != "" {
		tools = append(append([]string{}, tools...), contextTools...)
	}
	return tools
}

// contextStore is the server's store, brought up to the log.
func (srv *MCPServer) contextStore() (*ContextStore, error) {
	if srv.store == nil {
		srv.store = NewContextStore(srv.cfg)
	}
	return srv.store, srv.store.Follow(srv.log)
}

// contextCall answers one context tool call and logs it.
func (srv *MCPServer) contextCall(tool string, args json.RawMessage) (string, bool) {
	repo := srv.sess.Repository
	cs, err := srv.contextStore()
	if err != nil {
		return compact(map[string]any{"refused": "agentd could not read its log: " + err.Error()}), true
	}
	q := &QueryPayload{Repository: repo, Tool: tool}
	var reply any
	var problems []problem
	switch tool {
	case ToolState:
		var a struct {
			Topic string `json:"topic"`
		}
		if problems = srv.decodeArgs(args, &a); problems == nil {
			reply = srv.state(cs, a.Topic, q)
		}
	case ToolFind:
		var a struct {
			Q     string `json:"q"`
			Limit int    `json:"limit"`
		}
		if problems = srv.decodeArgs(args, &a); problems == nil {
			if strings.TrimSpace(a.Q) == "" {
				problems = []problem{{format: "q is empty"}}
			} else {
				q.Q = oneLine(srv.withheld(a.Q), 200)
				reply = findTopics(cs, repo, a.Q, a.Limit, q)
			}
		}
	case ToolHistory:
		reply, problems = srv.history(cs, args, q)
	case ToolAskExpert:
		reply, problems = srv.askExpert(cs, args, q)
	case ToolIssueBody:
		var a issueBodyArgs
		if problems = srv.decodeArgs(args, &a); problems == nil {
			var body string
			if body, problems = issueBody(cs, repo, a, srv.withheld); problems == nil {
				q.IDs, q.Records = append(append([]string{}, a.Owner...), a.IDs...), len(a.Owner)+len(a.IDs)
				reply = map[string]any{"title": oneLine(srv.withheld(a.Title), 300), "body": body}
			}
		}
	}
	if len(problems) > 0 {
		var said []string
		for _, p := range problems {
			said = append(said, p.reply())
		}
		q.Error = cutUTF8(problems[0].logged(), 300)
		reply = map[string]any{"refused": cutUTF8(strings.Join(said, "; "), 2000)}
	}
	text := compact(reply)
	q.Bytes, q.Tokens = len(text), tokensOf(len(text))
	if err := srv.log.Append(Event{V: EventVersion, Time: srv.now(), Session: srv.name, Kind: KindQuery, Mode: ModeShared, Query: q}); err != nil {
		return compact(map[string]any{"refused": "agentd could not log the query: " + err.Error()}), true
	}
	return text, len(problems) > 0
}

// state is state(topic): the card, or what history holds when there is
// none yet.
func (srv *MCPServer) state(cs *ContextStore, topic string, q *QueryPayload) any {
	if topic == "" {
		topic = TopicRepo
	}
	q.Topic = oneLine(srv.withheld(topic), 200)
	h := cs.History(srv.sess.Repository)
	if c := cs.Card(srv.sess.Repository, topic); c != nil {
		q.Hit, q.Records = true, 1
		return map[string]any{"card": c, "history": len(h.OnTopic(topic))}
	}
	out := map[string]any{"topic": cutUTF8(topic, 200), "card": nil, "history": len(h.OnTopic(topic))}
	if cardKindOf(topic) == "" {
		out["say"] = "this topic has no card: state keeps cards for #N, !N, session:NAME and the repository; read history on it, or find the topic"
	} else {
		out["say"] = "no card yet: read history with why missing-from-state, and its curator makes one"
	}
	return out
}

// findTerms are the words and references of a query, lower-cased.
func findTerms(q string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '#' || r == '!' || r == ':' || r == '-' || r == '_' || r == '.' || r == '/')
	}) {
		f = strings.Trim(f, ".:/-")
		if len(f) >= 2 && !stopword[f] && !contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

var stopword = map[string]bool{"the": true, "and": true, "of": true, "to": true, "in": true, "on": true, "is": true, "are": true,
	"be": true, "it": true, "for": true, "with": true, "what": true, "why": true, "how": true, "did": true, "does": true, "was": true}

// findTopics is find(q): cards ranked by how often they name each term,
// a topic the query names exactly first; then topics with history and no
// card whose names match.
func findTopics(cs *ContextStore, repo, q string, limit int, qp *QueryPayload) any {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	terms := findTerms(q)
	type hit struct {
		Topic   string `json:"topic"`
		Kind    string `json:"kind,omitempty"`
		Title   string `json:"title,omitempty"`
		Status  string `json:"status,omitempty"`
		Rev     int    `json:"rev,omitempty"`
		Card    bool   `json:"card"`
		History int    `json:"history"`
		score   float64
	}
	h := cs.History(repo)
	var hits []hit
	for topic, c := range cs.Cards(repo) {
		text := strings.ToLower(c.text())
		score := 0.0
		for _, t := range terms {
			if t == strings.ToLower(topic) || topicSession+t == topic {
				score += 10
			}
			if n := strings.Count(text, t); n > 0 {
				score += 1 + math.Log(float64(n))
			}
		}
		if score > 0 {
			hits = append(hits, hit{Topic: topic, Kind: c.Kind, Title: c.Title, Status: c.Status, Rev: c.Rev, Card: true, History: len(h.OnTopic(topic)), score: score})
		}
	}
	for _, topic := range h.Topics() {
		if cs.Card(repo, topic) != nil {
			continue
		}
		score := 0.0
		for _, t := range terms {
			if lt := strings.ToLower(topic); lt == t || topicSession+t == lt || topicArea+t == lt {
				score += 5
			} else if strings.Contains(lt, t) {
				score += 0.5
			}
		}
		if score > 0 {
			hits = append(hits, hit{Topic: topic, Kind: cardKindOf(topic), History: len(h.OnTopic(topic)), score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].Topic < hits[j].Topic
	})
	limitBytes := budgetBytes(DefaultHistoryBudget)
	out := []hit{}
	size := len(`{"q":"","topics":[]}`) + len(q)
	for _, x := range hits {
		if len(out) == limit {
			break
		}
		x.Title = oneLine(x.Title, 140)
		n := len(compact(x)) + 1
		if size+n > limitBytes {
			break
		}
		out = append(out, x)
		size += n
		qp.Hit = qp.Hit || x.Card
	}
	qp.Records = len(out)
	return map[string]any{"q": cutUTF8(q, 200), "topics": out}
}

// history is history(ids) or history(topic, since?, kinds?, budget), with
// its reason. A topic of an issue or request whose thread agentd has not
// read lately is read from the forge first, and logged.
func (srv *MCPServer) history(cs *ContextStore, args json.RawMessage, q *QueryPayload) (any, []problem) {
	var a struct {
		IDs      []string `json:"ids"`
		Topic    string   `json:"topic"`
		Since    string   `json:"since"`
		Kinds    []string `json:"kinds"`
		Budget   int      `json:"budget_tokens"`
		More     string   `json:"more"`
		Why      string   `json:"why"`
		Question string   `json:"question"`
	}
	if p := srv.decodeArgs(args, &a); p != nil {
		return nil, p
	}
	var problems []problem
	if !contains(historyWhys, a.Why) {
		problems = append(problems, problem{format: "why must be one of %s", args: []any{strings.Join(historyWhys, ", ")}})
	}
	if (len(a.IDs) == 0) == (a.Topic == "") {
		problems = append(problems, problem{format: "give ids or a topic, not both"})
	}
	for _, k := range a.Kinds {
		if !contains(HistKinds, k) {
			problems = append(problems, problem{format: "no history kind %q", args: []any{k}})
		}
	}
	var since time.Time
	if a.Since != "" {
		var err error
		if since, err = time.Parse(time.RFC3339, a.Since); err != nil {
			if since, err = time.Parse("2006-01-02", a.Since); err != nil {
				problems = append(problems, problem{format: "since is not RFC 3339 or YYYY-MM-DD"})
			}
		}
	}
	if len(problems) > 0 {
		return nil, problems
	}
	q.Why, q.Question = a.Why, oneLine(srv.withheld(a.Question), 500)
	repo := srv.sess.Repository
	if a.Topic != "" {
		q.Topic = oneLine(srv.withheld(a.Topic), 200)
		if k := cardKindOf(a.Topic); (k == CardIssue || k == CardRequest) && srv.api != nil && cs.ThreadDue(repo, a.Topic, srv.now()) {
			p := cs.ReadThread(srv.api, repo, a.Topic)
			e := Event{V: EventVersion, Time: srv.now(), Kind: KindThread, Mode: ModeShared, Thread: p}
			if srv.log.Append(e) == nil {
				cs.Follow(srv.log)
			}
		}
		out := cs.History(repo).ByTopic(HistoryQuery{Topic: a.Topic, Since: since, Kinds: a.Kinds, Budget: a.Budget, More: a.More})
		q.Records = len(out["records"].([]histEntry))
		return out, nil
	}
	if len(a.IDs) > historyIDs {
		return nil, []problem{{format: "at most %d ids a call", args: []any{historyIDs}}}
	}
	q.IDs = a.IDs
	out := cs.History(repo).ByIDs(a.IDs, a.Budget)
	if rs, ok := out["records"].([]HistWhole); ok {
		q.Records = len(rs)
	}
	return out, nil
}
