package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// agentd mcp serves the session's tools over stdio (#3 §2): the session
// reports progress, raises decisions (decisions.go) and asks for a step
// only through calls agentd checks
// against its case. The server is started for one session, bound to that
// session's case, so a call never names a case. Every call, accepted or
// refused, is a case event in the one log; the case's state still comes
// from the facts, never from what a call claims.

// CaseGoal is the issue's done-when items as the owner recorded them: every
// item the case has had, an edited one superseded by the item that replaced
// it.
type CaseGoal struct {
	Items []GoalItem `json:"items"`
}

// GoalItem is one done-when item. An item is the owner's to judge until
// the owner confirms a machine can (#3 §1); for now every item is.
type GoalItem struct {
	ID           string `json:"id"` // A1, A2…
	Text         string `json:"text"`
	Kind         string `json:"kind"`
	SupersededBy string `json:"superseded_by,omitempty"`
}

// Goal item kinds.
const (
	GoalOwner   = "owner"
	GoalMachine = "machine"
)

// CaseCall is one tool call a session made through its server: refused,
// with why, or accepted, with the progress or the step it carries. Hash is
// the arguments' hash; a refused call's arguments are not kept.
type CaseCall struct {
	Tool     string        `json:"tool"`
	Hash     string        `json:"hash"`
	Refused  string        `json:"refused,omitempty"`
	Progress *CaseProgress `json:"progress,omitempty"`
	Drift    []DriftFlag   `json:"drift,omitempty"`
	Step     *CaseStep     `json:"step,omitempty"`
	Raised   *Decision     `json:"raised,omitempty"`
	// Findings and Dispositions are a review round's (rounds.go).
	Findings     *CaseFindings     `json:"findings,omitempty"`
	Dispositions *CaseDispositions `json:"dispositions,omitempty"`
}

// Tool names.
const (
	ToolReportProgress = "report_progress"
	ToolRequestStep    = "request_step"
)

// mcpTools are the tools a working session's facts name, in the order
// they list; a reviewer and a judge have their own (toolsFor).
var mcpTools = []string{ToolReportProgress, ToolRaiseDecision, ToolRequestStep}

// allTools is every tool a call event may name.
var allTools = []string{ToolReportProgress, ToolRaiseDecision, ToolRequestStep, ToolReviewFindings, ToolJudgeDispositions}

// toolsFor is the tools a session of the role is offered: a reviewer
// only reports findings, since reviewers advise and ask no one; a judge
// disposes them and asks the owner what is the owner's.
func toolsFor(role string) []string {
	switch role {
	case RoleReviewer:
		return []string{ToolReviewFindings}
	case RoleJudge:
		return []string{ToolJudgeDispositions, ToolRaiseDecision}
	}
	return mcpTools
}

// CaseProgress is an accepted report_progress, its text screened.
type CaseProgress struct {
	Summary      string         `json:"summary"`
	Approach     string         `json:"approach,omitempty"`
	Items        []ItemProgress `json:"items"`
	NextAction   string         `json:"next_action"`
	Risks        []string       `json:"risks,omitempty"`
	Acknowledged []string       `json:"acknowledged,omitempty"`
}

// ItemProgress is one done-when item's status as the session reports it,
// with the evidence agentd checked.
type ItemProgress struct {
	ID       string        `json:"id"`
	Status   string        `json:"status"`
	Evidence []EvidenceRef `json:"evidence,omitempty"`
}

// Item statuses.
const (
	StatusNotStarted = "not_started"
	StatusInProgress = "in_progress"
	StatusMet        = "met"
)

// EvidenceRef is a reference agentd can check (#3 §1), never prose: a
// commit on the session's branch, a gate agentd ran, a file and line in the
// worktree, a CI job's result or a review finding.
type EvidenceRef struct {
	Kind string `json:"kind"`
	SHA  string `json:"sha,omitempty"`
	Path string `json:"path,omitempty"`
	Line int    `json:"line,omitempty"`
	Job  string `json:"job,omitempty"`
	ID   string `json:"id,omitempty"`
}

// Evidence kinds.
const (
	EvidenceCommit  = "commit"
	EvidenceGate    = "gate"
	EvidenceFile    = "file"
	EvidenceCI      = "ci"
	EvidenceFinding = "finding"
)

// DriftFlag is a done-when item a report's restatement left out or worded
// otherwise than the issue.
type DriftFlag struct {
	Item string `json:"item"`
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"` // the restatement, when reworded
}

// Drift kinds.
const (
	DriftMissing  = "missing"
	DriftReworded = "reworded"
)

// CaseStep is a step a session asked for, with the facts when it asked. A
// request is recorded, never evidence: agentd moves the case on the facts.
type CaseStep struct {
	Kind  string `json:"kind"`
	Head  string `json:"head,omitempty"`
	Gated bool   `json:"gated,omitempty"` // a gate agentd ran passed on Head
}

// Step kinds.
const (
	StepReview   = "review"
	StepDelivery = "delivery"
	StepStop     = "stop"
)

// ProgressRecord is a case's latest accepted report: who made it and when.
type ProgressRecord struct {
	CaseProgress
	Session string    `json:"session"`
	At      time.Time `json:"at"`
}

// StepRecord is a step a session asked for and when.
type StepRecord struct {
	CaseStep
	Session string    `json:"session"`
	At      time.Time `json:"at"`
}

// check refuses a call of an unknown tool, or one that carries other than
// its refusal, or else what its tool records.
func (c *CaseCall) check() error {
	refused := c.Refused != ""
	n := 0
	for _, set := range []bool{c.Progress != nil, c.Step != nil, c.Raised != nil, c.Findings != nil, c.Dispositions != nil} {
		if set {
			n++
		}
	}
	switch {
	case !contains(allTools, c.Tool):
		return fmt.Errorf("unknown tool %q", c.Tool)
	case refused && (n > 0 || c.Drift != nil):
		return errors.New("a refused call carries only why")
	case refused:
		return nil
	case n != 1 || c.Drift != nil && c.Progress == nil:
		return fmt.Errorf("an accepted %s carries what it records and nothing else", c.Tool)
	case c.Tool == ToolReportProgress && c.Progress == nil:
		return errors.New("an accepted report_progress carries its progress")
	case c.Tool == ToolRequestStep && c.Step == nil:
		return errors.New("an accepted request_step carries its step")
	case c.Tool == ToolRaiseDecision && (c.Raised == nil || !raisedID.MatchString(c.Raised.ID)):
		return errors.New("an accepted raise_decision carries its decision, numbered D1 on")
	case c.Tool == ToolReviewFindings && (c.Findings == nil || c.Findings.Round < 1):
		return errors.New("an accepted review_findings carries its round's findings")
	case c.Tool == ToolJudgeDispositions && (c.Dispositions == nil || c.Dispositions.Round < 1):
		return errors.New("an accepted judge_dispositions carries its round's dispositions")
	}
	return nil
}

// applyGoal and applyCall fold a case's goal and a session's call into c.
func (c *Case) applyGoal(g *CaseGoal) {
	c.Goal = g.Items
}

func (c *Case) applyCall(e Event) {
	call := e.Case.Call
	switch {
	case call.Refused != "":
		c.Refused++
	case call.Progress != nil:
		c.Progress = &ProgressRecord{CaseProgress: *call.Progress, Session: e.Session, At: e.Time}
		c.Drift = call.Drift
		if len(call.Progress.Acknowledged) > 0 {
			acked := make(map[string][]string, len(c.Acked)+1)
			for k, v := range c.Acked {
				acked[k] = v
			}
			acked[e.Session] = append(append([]string(nil), acked[e.Session]...), call.Progress.Acknowledged...)
			c.Acked = acked
		}
	case call.Step != nil:
		c.Steps = append(c.Steps[:len(c.Steps):len(c.Steps)], StepRecord{CaseStep: *call.Step, Session: e.Session, At: e.Time})
	case call.Raised != nil && e.Session != "":
		c.raiseOnCase(e, *call.Raised)
	case call.Findings != nil:
		c.applyFindings(e, call.Findings)
	case call.Dispositions != nil:
		c.applyDispositions(e, call.Dispositions)
	}
}

// current is the goal's items not superseded.
func current(goal []GoalItem) []GoalItem {
	var out []GoalItem
	for _, it := range goal {
		if it.SupersededBy == "" {
			out = append(out, it)
		}
	}
	return out
}

// unacknowledged is the active rulings the session has not acknowledged in
// an accepted call.
func (c *Case) unacknowledged(session string) []string {
	var out []string
	for _, r := range c.Active {
		if !contains(c.Acked[session], r.ID) {
			out = append(out, r.ID)
		}
	}
	return out
}

// doneWhenHeading starts the issue's done-when list: a heading, or a line
// in bold, that says "Done when".
var doneWhenHeading = regexp.MustCompile(`(?i)^\s*(#+\s*|\*\*)done when\b`)
var listItem = regexp.MustCompile(`^\s*([-*+]|\d+[.)])\s+(\[[ xX]\]\s+)?(.*)$`)

// boldHeading is a line wholly in bold, such as "**Notes**", which ends a
// section that began with one.
var boldHeading = regexp.MustCompile(`^\s*\*\*[^*]+\*\*\s*:?\s*$`)

// DoneWhen is the list items under the body's "Done when" heading, to the
// next heading or line in bold. A line indented under an item, a nested
// item too, continues it; a line that is not an item before the first one,
// such as "Tests show:", is not one.
func DoneWhen(body string) []string {
	var items []string
	in := false
	indent := -1 // the first item's indent: a deeper item is nested
	for _, line := range strings.Split(body, "\n") {
		if doneWhenHeading.MatchString(line) {
			in, items = true, nil
			continue
		}
		if !in {
			continue
		}
		if anyHeading.MatchString(line) || boldHeading.MatchString(line) {
			break
		}
		depth := len(line) - len(strings.TrimLeft(line, " \t"))
		if m := listItem.FindStringSubmatch(line); m != nil && (indent < 0 || depth <= indent) {
			if indent < 0 {
				indent = depth
			}
			items = append(items, strings.TrimSpace(m[3]))
		} else if t := strings.TrimSpace(line); t != "" && len(items) > 0 && depth > indent {
			items[len(items)-1] += " " + t
		}
	}
	var out []string
	for _, it := range items {
		if it != "" {
			out = append(out, it)
		}
	}
	return out
}

// maxGoalItem bounds a done-when item's text.
const maxGoalItem = 500

// RecordGoal records the issue body's done-when items as the case's goal,
// the owner's record. An item whose text the case already has keeps its
// id; a new or edited one takes the next id of the case's life, and an
// item no longer in the list is superseded by the new item most like it,
// or by "removed" when none shares half its words. Like RecordRuling, the
// ids are read from st.
func RecordGoal(log *Log, cfg *Config, st *LogState, now time.Time, id, body string) error {
	id = strings.ToLower(id)
	if !knownCase(cfg, st, id) {
		return fmt.Errorf("no case %q", id)
	}
	texts := DoneWhen(body)
	if len(texts) == 0 {
		return errors.New(`the body has no list under a "Done when" heading`)
	}
	var old []GoalItem
	if c := st.Cases[id]; c != nil {
		old = c.Goal
	}
	goal := append([]GoalItem(nil), old...)
	next := len(goal) + 1
	var kept []string // the ids the new list has
	for i, text := range texts {
		if utf8.RuneCountInString(text) > maxGoalItem || !utf8.ValidString(text) {
			return fmt.Errorf("item %d is over %d characters", i+1, maxGoalItem)
		}
		found := ""
		for _, it := range goal {
			if it.SupersededBy == "" && it.Text == text && !contains(kept, it.ID) {
				found = it.ID
				break
			}
		}
		if found == "" {
			found = "A" + strconv.Itoa(next)
			next++
			goal = append(goal, GoalItem{ID: found, Text: text, Kind: GoalOwner})
		}
		kept = append(kept, found)
	}
	// What the list no longer has is superseded by the new item most like
	// it, the closest pairs first and each item in one pair at most, or
	// else removed.
	type pair struct {
		old, fresh int
		score      float64
	}
	var pairs []pair
	for i, it := range goal {
		if it.SupersededBy != "" || contains(kept, it.ID) {
			continue
		}
		goal[i].SupersededBy = "removed"
		for j, f := range goal {
			if contains(kept, f.ID) && !hasGoal(old, f.ID) {
				if score, ok := alike(it.Text, f.Text); ok {
					pairs = append(pairs, pair{i, j, score})
				}
			}
		}
	}
	sort.SliceStable(pairs, func(a, b int) bool { return pairs[a].score > pairs[b].score })
	taken := map[int]bool{}
	for _, p := range pairs {
		if goal[p.old].SupersededBy == "removed" && !taken[p.fresh] {
			goal[p.old].SupersededBy, taken[p.fresh] = goal[p.fresh].ID, true
		}
	}
	return log.Append(Event{V: EventVersion, Time: now, Kind: KindCase, Mode: ModeOwner, Case: &CasePayload{Case: id, Goal: &CaseGoal{Items: goal}}})
}

// stopWords do not count toward two items being alike.
var stopWords = map[string]bool{"a": true, "an": true, "the": true, "and": true, "or": true, "of": true, "to": true, "in": true,
	"on": true, "is": true, "are": true, "be": true, "it": true, "its": true, "with": true, "for": true, "when": true, "that": true}

// alike says whether an edited item's text b is a's: they share at least
// two words that are not stop words, more than half of the longer one's.
// The score is that share.
func alike(a, b string) (float64, bool) {
	words := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, w := range strings.Fields(strings.ToLower(s)) {
			if w = strings.Trim(w, ".,;:!?`*()[]\"'"); w != "" && !stopWords[w] {
				m[w] = true
			}
		}
		return m
	}
	wa, wb := words(a), words(b)
	n := 0
	for w := range wa {
		if wb[w] {
			n++
		}
	}
	score := float64(n) / float64(max(len(wa), len(wb), 1))
	return score, n >= 2 && score > 0.5
}

func hasGoal(goal []GoalItem, id string) bool {
	for _, it := range goal {
		if it.ID == id {
			return true
		}
	}
	return false
}

// MCPServer is one session's server.
type MCPServer struct {
	cfg          *Config
	name         string
	sess         Session
	caseID       string
	log          *Log
	read         func() ([]Event, error) // the log as it is now
	now          func() time.Time
	confidential []*regexp.Regexp
	st           *LogState // the log as folded for the call being checked
	// replaced says whether the binary on disk is no longer the one the
	// server runs, and restart re-executes it with the input not yet
	// answered, returning only when it cannot (upgrade.go). Nil: never.
	replaced   func() bool
	restart    func(pending []byte, why string) error
	restarting string        // why the call just handled is left to the binary on disk
	restartErr error         // why re-executing it failed
	note       string        // said in the next tool reply: the server restarted
	held       *bytes.Reader // the handover input, which Serve reads before its own
}

// NewMCPServer is the server for the named session, which must work a
// case.
func NewMCPServer(cfg *Config, name string, log *Log, read func() ([]Event, error), now func() time.Time) (*MCPServer, error) {
	sess, ok := cfg.Sessions[name]
	if !ok {
		return nil, fmt.Errorf("no session %q", name)
	}
	id := cfg.caseOf(sess)
	if id == "" {
		return nil, fmt.Errorf("session %s works no case: it needs a repository and an issue", name)
	}
	srv := &MCPServer{cfg: cfg, name: name, sess: sess, caseID: id, log: log, read: read, now: now}
	for _, p := range screenPatterns(cfg) {
		srv.confidential = append(srv.confidential, regexp.MustCompile(p))
	}
	return srv, nil
}

// mcpVersions are the protocol versions the server speaks, newest first.
var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// rpcRequest is a JSON-RPC message from the session: a request, or a
// notification when it has no id.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC error codes.
const (
	rpcParseError     = -32700
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

// MCPTool is a tool as tools/list describes it.
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// mcpMaxLine bounds one message from the session.
const mcpMaxLine = 1 << 20

// Serve reads one JSON-RPC message per line from r and writes each reply
// as one line to w, until r ends. A line over mcpMaxLine is answered as
// unreadable and skipped; the server goes on.
func (srv *MCPServer) Serve(r io.Reader, w io.Writer) error {
	br := bufio.NewReaderSize(r, 64*1024)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	unreadable := func() error {
		return enc.Encode(map[string]any{"jsonrpc": "2.0", "id": nil, "error": rpcError{rpcParseError, "not a JSON-RPC message of at most 1 MiB"}})
	}
	for {
		line, long, err := readLine(br, mcpMaxLine)
		if err != nil && err != io.EOF {
			return err
		}
		if long {
			if werr := unreadable(); werr != nil {
				return werr
			}
		} else if line = bytes.TrimSpace(line); len(line) > 0 {
			var req rpcRequest
			if jerr := json.Unmarshal(line, &req); jerr != nil || req.Method == "" {
				if werr := unreadable(); werr != nil {
					return werr
				}
			} else if len(req.ID) > 0 { // a notification is not acted on, nor answered
				result, rerr := srv.handle(req)
				if why := srv.restarting; why != "" {
					srv.restarting = ""
					pending := append(append(line, '\n'), buffered(br)...)
					if srv.held != nil { // copied, not read: a failed restart serves it still
						rest := make([]byte, srv.held.Len())
						srv.held.ReadAt(rest, srv.held.Size()-int64(len(rest)))
						pending = append(pending, rest...)
					}
					if srv.restartErr = srv.restart(pending, why); srv.restartErr == nil {
						return nil
					}
					result, rerr = srv.handle(req)
				}
				reply := map[string]any{"jsonrpc": "2.0", "id": req.ID}
				if rerr != nil {
					reply["error"] = rerr
				} else {
					reply["result"] = result
				}
				if werr := enc.Encode(reply); werr != nil {
					return werr
				}
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

// readLine reads one line, without its newline. A line over max is read
// to its end and dropped, and long says so.
func readLine(br *bufio.Reader, max int) (line []byte, long bool, err error) {
	for {
		chunk, err := br.ReadSlice('\n')
		if !long {
			line = append(line, chunk...)
			if n := len(line); n > max+1 || n == max+1 && line[max] != '\n' {
				line, long = nil, true
			}
		}
		if err != bufio.ErrBufferFull {
			return bytes.TrimSuffix(line, []byte("\n")), long, err
		}
	}
}

func (srv *MCPServer) handle(req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		version := mcpVersions[0]
		if contains(mcpVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo":   map[string]any{"name": "agentd", "version": version},
			"instructions": srv.instructions()}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		var tools []MCPTool
		for _, name := range toolsFor(srv.sess.Role) {
			for _, t := range toolList() {
				if t.Name == name {
					tools = append(tools, t)
				}
			}
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{rpcInvalidParams, "params: " + err.Error()}
		}
		// A tool the server does not have is a protocol error, answered
		// and not logged: nothing reached the case.
		if tools := toolsFor(srv.sess.Role); !contains(tools, p.Name) {
			return nil, &rpcError{rpcInvalidParams, fmt.Sprintf("no tool %q: %s", p.Name, strings.Join(tools, ", "))}
		}
		reply, refused := srv.call(p.Name, p.Arguments)
		if srv.note != "" && srv.restarting == "" {
			reply, srv.note = withField(reply, "restarted", srv.note), ""
		}
		return map[string]any{"content": []map[string]string{{"type": "text", "text": reply}}, "isError": refused}, nil
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	}
	return nil, &rpcError{rpcMethodNotFound, "no method " + req.Method}
}

// call checks and records one tool call. It returns the reply, compact
// JSON, and whether the call was refused.
func (srv *MCPServer) call(tool string, args json.RawMessage) (string, bool) {
	events, err := srv.load()
	if err != nil {
		return compact(map[string]any{"refused": "agentd could not read its log: " + err.Error()}), true
	}
	st := NewLogState(srv.cfg, ModeLive).Fold(events)
	srv.st = st
	c := st.Cases[srv.caseID]
	if c == nil {
		c = &Case{ID: srv.caseID}
	}
	rec := &CaseCall{Tool: tool, Hash: shortHash(HashPane(string(args)))}
	var reply map[string]any
	var problems []problem
	if c.Closed {
		problems = []problem{{format: "case %s is closed", args: []any{srv.caseID}}}
	} else if tool == ToolReportProgress {
		rec.Progress, rec.Drift, problems = srv.checkProgress(args, c, st.Tracks[srv.name])
		reply = map[string]any{"accepted": true, "case": srv.caseID, "drift": rec.Drift}
		if len(rec.Drift) == 0 {
			delete(reply, "drift")
		}
	} else if tool == ToolReviewFindings {
		rec.Findings, problems = srv.checkFindings(args, c)
		if f := rec.Findings; f != nil {
			ids := []string{}
			for _, x := range f.Findings {
				ids = append(ids, x.ID)
			}
			reply = map[string]any{"accepted": true, "case": srv.caseID, "round": f.Round, "ids": ids, "new": f.New}
			if f.Stop != nil {
				reply["stopped"] = f.Stop.Reason
			}
		}
	} else if tool == ToolJudgeDispositions {
		rec.Dispositions, problems = srv.checkDispositions(args, c)
		if d := rec.Dispositions; d != nil {
			reply = map[string]any{"accepted": true, "case": srv.caseID, "round": d.Round}
		}
	} else if tool == ToolRaiseDecision {
		var withheld bool
		rec.Raised, withheld, problems = srv.checkRaise(args, st.Tracks[srv.name])
		if rec.Raised != nil {
			reply = map[string]any{"raised": true, "id": rec.Raised.ID, "case": srv.caseID, "answers": AnswersFile}
			if withheld {
				reply["withheld"] = "the decision's words match a confidential pattern; the owner sees its id and type only"
			}
		}
	} else {
		var facts string
		rec.Step, facts, problems = srv.checkStep(args, st.Tracks[srv.name])
		reply = map[string]any{"recorded": true, "moved": false, "case": srv.caseID, "facts": facts}
	}
	if len(problems) > 0 {
		var logged, said []string
		for _, p := range problems {
			if l := p.logged(); !contains(logged, l) {
				logged = append(logged, l)
			}
			said = append(said, p.reply())
		}
		rec = &CaseCall{Tool: tool, Hash: rec.Hash, Refused: cutUTF8(strings.Join(logged, "; "), 2000)}
		reply = map[string]any{"refused": cutUTF8(strings.Join(said, "; "), 4000), "case": srv.caseID}
	}
	err = srv.log.Append(Event{V: EventVersion, Time: srv.now(), Session: srv.name, Kind: KindCase, Mode: ModeShared,
		Case: &CasePayload{Case: srv.caseID, Call: rec}})
	if err != nil {
		return compact(map[string]any{"refused": "agentd could not record the call: " + err.Error()}), true
	}
	// A round call checked against the state another session's call
	// changed before this one was appended is not folded (rounds.go).
	if (rec.Findings != nil || rec.Dispositions != nil) && rec.Refused == "" {
		round := 0
		if rec.Findings != nil {
			round = rec.Findings.Round
		} else {
			round = rec.Dispositions.Round
		}
		events, err := srv.read()
		if err != nil {
			return compact(map[string]any{"refused": "the call was logged, but agentd could not read its log back to confirm the round took it: " + err.Error() + "; check the case before calling again", "case": srv.caseID}), true
		}
		if c := NewLogState(srv.cfg, ModeLive).Fold(events).Cases[srv.caseID]; c == nil || !c.recorded(tool, round, rec.Hash) {
			return compact(map[string]any{"refused": "another call, or the owner, changed the case first; nothing of this call was recorded; read the case again and call again", "case": srv.caseID}), true
		}
	}
	return compact(reply), rec.Refused != ""
}

// instructions is what the server tells the session of its tools.
func (srv *MCPServer) instructions() string {
	switch srv.sess.Role {
	case RoleReviewer:
		return "Review the work on case " + srv.caseID + " and report every finding with review_findings, once a round. You advise; the judge decides. " +
			"Repeat an earlier finding by its id, F1…, from your facts; name the done-when items each bears on, or set widens_scope."
	case RoleJudge:
		return "Dispose every finding of case " + srv.caseID + "'s latest round with judge_dispositions: adopt, decline or defer, each with a reason, and a plan by id. " +
			"A finding that widens the scope is deferred and asked of the owner with raise_decision. The owner's answers arrive in " + AnswersFile + " in your workdir."
	}
	return "Report progress on case " + srv.caseID + " with report_progress, ask the owner with raise_decision, and ask for review, delivery or stop with request_step. " +
		"Arguments are JSON data; evidence is a typed reference agentd checks, never prose. The owner's answers arrive in " + AnswersFile + " in your workdir."
}

func compact(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

// decodeArgs decodes a call's arguments strictly: an argument the tool
// does not take, such as a case, is refused.
func (srv *MCPServer) decodeArgs(args json.RawMessage, v any) []problem {
	if len(args) == 0 {
		args = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		msg := err.Error()
		if strings.Contains(msg, `unknown field "case"`) && srv.caseID != "" {
			return []problem{{format: `the arguments name a "case": a call never names one; this server reports on %s only`, args: []any{srv.caseID}}}
		}
		return []problem{{format: "the arguments are not the tool's: %s", args: []any{strings.TrimPrefix(msg, "json: ")}}}
	}
	return nil
}

// problem is what to fix in a call: the session is told it whole, and
// the log keeps only its wording, the session's values left out, so no
// argument reaches the log unscreened.
type problem struct {
	format string
	args   []any
	log    string // when set, what the log keeps instead of the wording
}

func problemOf(format string, args ...any) *problem { return &problem{format: format, args: args} }

func (p problem) reply() string { return fmt.Sprintf(p.format, p.args...) }

var formatVerb = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)

func (p problem) logged() string {
	if p.log != "" {
		return p.log
	}
	return formatVerb.ReplaceAllString(p.format, "…")
}

// Report limits.
const (
	maxSummary      = 600
	maxSentences    = 3
	maxApproach     = 1000
	maxNextAction   = 300
	maxRisk         = 300
	maxRisks        = 10
	maxEvidence     = 10
	maxEvidencePath = 300
)

var sentenceEnd = regexp.MustCompile(`[.!?](\s|$)`)

// checkProgress checks a report_progress against the case and the facts.
// It returns the progress to record, the drift its restatement shows, and
// what to fix, if anything.
func (srv *MCPServer) checkProgress(args json.RawMessage, c *Case, t *Track) (*CaseProgress, []DriftFlag, []problem) {
	var a struct {
		Summary  string `json:"summary"`
		Approach string `json:"approach"`
		Items    []struct {
			ID       string            `json:"id"`
			Status   string            `json:"status"`
			Evidence []json.RawMessage `json:"evidence"`
		} `json:"items"`
		NextAction      string   `json:"next_action"`
		Risks           []string `json:"risks"`
		Acknowledged    []string `json:"acknowledged"`
		GoalRestatement []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"goal_restatement"`
	}
	if p := srv.decodeArgs(args, &a); p != nil {
		return nil, nil, p
	}
	var problems []problem
	bad := func(format string, v ...any) { problems = append(problems, problem{format: format, args: v}) }
	text := func(field, s string, max int) {
		if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
			bad("%s is over %d characters", field, max)
		}
	}
	a.Summary, a.NextAction = strings.TrimSpace(a.Summary), strings.TrimSpace(a.NextAction)
	switch {
	case a.Summary == "":
		bad("summary is empty: say where the work stands in at most %d sentences", maxSentences)
	case len(sentenceEnd.FindAllString(a.Summary, -1)) > maxSentences:
		bad("summary is over %d sentences", maxSentences)
	}
	text("summary", a.Summary, maxSummary)
	text("approach", a.Approach, maxApproach)
	if a.NextAction == "" {
		bad("next_action is empty: name the next step")
	}
	text("next_action", a.NextAction, maxNextAction)
	if len(a.Risks) > maxRisks {
		bad("risks has %d entries, over %d", len(a.Risks), maxRisks)
	}
	for i, r := range a.Risks {
		text(fmt.Sprintf("risks[%d]", i), r, maxRisk)
	}
	goal := current(c.Goal)
	ids := make([]string, len(goal))
	for i, it := range goal {
		ids[i] = it.ID
	}
	known := "the case has no done-when items recorded; report items: []"
	if len(ids) > 0 {
		known = "the case's items are " + strings.Join(ids, ", ")
	}
	p := &CaseProgress{Summary: srv.withheld(a.Summary), Approach: srv.withheld(a.Approach), NextAction: srv.withheld(a.NextAction)}
	var seen []string
	for _, it := range a.Items {
		if !contains(ids, it.ID) {
			bad("item %q is not one of the case's: %s", it.ID, known)
			continue
		}
		if contains(seen, it.ID) {
			bad("item %s is reported twice", it.ID)
			continue
		}
		seen = append(seen, it.ID)
		switch it.Status {
		case StatusNotStarted, StatusInProgress, StatusMet:
		default:
			bad("item %s: status %q is not not_started, in_progress or met", it.ID, it.Status)
		}
		if it.Status == StatusMet && len(it.Evidence) == 0 {
			bad("item %s: met needs evidence agentd can check, such as {\"kind\":\"commit\",\"sha\":\"…\"}", it.ID)
		}
		if len(it.Evidence) > maxEvidence {
			bad("item %s has %d pieces of evidence, over %d", it.ID, len(it.Evidence), maxEvidence)
		}
		ip := ItemProgress{ID: it.ID, Status: it.Status}
		for _, raw := range it.Evidence {
			ref, p := srv.checkEvidence(raw, t, c)
			if p != nil {
				bad("item %s: "+p.format, append([]any{it.ID}, p.args...)...)
				continue
			}
			ip.Evidence = append(ip.Evidence, ref)
		}
		p.Items = append(p.Items, ip)
	}
	for _, r := range a.Risks {
		p.Risks = append(p.Risks, srv.withheld(r))
	}
	// Every active ruling the session has not acknowledged must be; an id
	// that is not active is a mistake to fix.
	var active []string
	for _, r := range c.Active {
		active = append(active, r.ID)
	}
	for _, id := range a.Acknowledged {
		if !contains(active, id) {
			bad("acknowledged %q is not an active restriction or waiver of the case", id)
		}
	}
	var missing []string
	for _, id := range c.unacknowledged(srv.name) {
		if !contains(a.Acknowledged, id) {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		bad("acknowledge the case's active restrictions and waivers: %s", strings.Join(missing, ", "))
	}
	for _, id := range a.Acknowledged {
		if contains(active, id) && !contains(c.Acked[srv.name], id) && !contains(p.Acknowledged, id) {
			p.Acknowledged = append(p.Acknowledged, id)
		}
	}
	// Drift: each current item restated in words that are the issue's.
	restated := map[string]string{}
	for _, r := range a.GoalRestatement {
		if !contains(ids, r.ID) {
			bad("goal_restatement names %q, not one of the case's: %s", r.ID, known)
			continue
		}
		text("goal_restatement "+r.ID, r.Text, maxGoalItem)
		restated[r.ID] = r.Text
	}
	var drift []DriftFlag
	for _, it := range goal {
		r, ok := restated[it.ID]
		switch {
		case !ok:
			drift = append(drift, DriftFlag{Item: it.ID, Kind: DriftMissing})
		case normalItem(r) != normalItem(it.Text):
			drift = append(drift, DriftFlag{Item: it.ID, Kind: DriftReworded, Text: srv.withheld(r)})
		}
	}
	if len(problems) > 0 {
		return nil, nil, problems
	}
	if p.Items == nil {
		p.Items = []ItemProgress{}
	}
	return p, drift, nil
}

// normalItem is an item's text as drift compares it: case, spacing,
// emphasis and trailing punctuation do not count.
func normalItem(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	s = strings.NewReplacer("`", "", "*", "").Replace(s)
	return strings.TrimRight(s, ".;:, ")
}

var shaRef = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// checkEvidence checks one piece of evidence against the facts: it is a
// typed reference, and what it names is so.
func (srv *MCPServer) checkEvidence(raw json.RawMessage, t *Track, c *Case) (EvidenceRef, *problem) {
	var ref EvidenceRef
	if s := bytes.TrimSpace(raw); len(s) > 0 && s[0] == '"' {
		return ref, problemOf(`evidence %s is prose; give a typed reference such as {"kind":"commit","sha":"…"}`, cutUTF8(string(s), 80))
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ref); err != nil {
		return ref, problemOf("evidence is not a typed reference: %s", strings.TrimPrefix(err.Error(), "json: "))
	}
	// A reference carries only its kind's fields, so no other text rides
	// along into the log.
	extra := map[string]bool{
		EvidenceCommit:  ref.Path != "" || ref.Line != 0 || ref.Job != "" || ref.ID != "",
		EvidenceGate:    ref.Path != "" || ref.Line != 0 || ref.Job != "" || ref.ID != "",
		EvidenceFile:    ref.SHA != "" || ref.Job != "" || ref.ID != "",
		EvidenceCI:      ref.Path != "" || ref.Line != 0 || ref.ID != "",
		EvidenceFinding: ref.SHA != "" || ref.Path != "" || ref.Line != 0 || ref.Job != "",
	}
	if extra[ref.Kind] {
		return ref, problemOf("%s evidence takes only its own fields: commit and gate a sha, file a path and line, ci a sha and job, finding an id", ref.Kind)
	}
	if utf8.RuneCountInString(ref.Path) > maxEvidencePath {
		return ref, problemOf("file evidence's path is over %d characters", maxEvidencePath)
	}
	wt := srv.sess.Cwd
	switch ref.Kind {
	case EvidenceCommit, EvidenceGate, EvidenceCI:
		if !shaRef.MatchString(ref.SHA) {
			return ref, problemOf("%s evidence needs the commit's sha, 7 to 40 lowercase hex digits, not %q", ref.Kind, cutUTF8(ref.SHA, 60))
		}
	}
	switch ref.Kind {
	case EvidenceCommit:
		full, err := gitOut(srv.cfg.Timeouts.Forge.Duration, wt, "rev-parse", "--verify", "--quiet", ref.SHA+"^{commit}")
		full = strings.TrimSpace(full)
		if err != nil {
			return ref, problemOf("commit %s is not a commit in the session's worktree", ref.SHA)
		}
		if _, err := git(srv.cfg.Timeouts.Forge.Duration, wt, "merge-base", "--is-ancestor", full, "HEAD"); err != nil {
			return ref, problemOf("commit %s is not on the session's branch (not an ancestor of HEAD)", ref.SHA)
		}
	case EvidenceGate:
		passed := false
		if t != nil {
			for _, g := range t.Gated {
				passed = passed || strings.HasPrefix(g, ref.SHA)
			}
		}
		if !passed {
			return ref, problemOf("agentd has no passing gate on %s for this session; the gate is agentd's to run", ref.SHA)
		}
	case EvidenceFile:
		if ref.Path == "" || !filepath.IsLocal(ref.Path) {
			return ref, problemOf("file %q must be a path inside the worktree, relative to it", ref.Path)
		}
		root, err := filepath.EvalSymlinks(wt)
		if err != nil {
			return ref, problemOf("the session's worktree cannot be read: %s", err.Error())
		}
		path, err := filepath.EvalSymlinks(filepath.Join(root, ref.Path))
		if err != nil {
			return ref, problemOf("file %s does not exist in the worktree", ref.Path)
		}
		if rel, err := filepath.Rel(root, path); err != nil || !filepath.IsLocal(rel) {
			return ref, problemOf("file %q must be a path inside the worktree", ref.Path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return ref, problemOf("file %s cannot be read: it must be a file", ref.Path)
		}
		n := bytes.Count(b, []byte("\n"))
		if len(b) > 0 && b[len(b)-1] != '\n' {
			n++
		}
		if ref.Line < 1 {
			return ref, problemOf("file %s evidence needs the line it shows, 1 or more", ref.Path)
		}
		if ref.Line > n {
			return ref, problemOf("file %s has %d lines, not line %d", ref.Path, n, ref.Line)
		}
	case EvidenceCI:
		return ref, problemOf("agentd records no CI results yet; CI evidence cannot be checked")
	case EvidenceFinding:
		found := false
		for _, f := range c.findings() {
			found = found || f.ID == ref.ID
		}
		if !found {
			return ref, problemOf("finding %s is not one of the case's review findings", cutUTF8(ref.ID, 40))
		}
		if c.Disposition(ref.ID) == nil {
			return ref, problemOf("finding %s has no disposition yet; cite it once the judge disposed it", ref.ID)
		}
	default:
		return ref, problemOf("evidence kind %q is not commit, gate, file, ci or finding", cutUTF8(ref.Kind, 40))
	}
	return ref, nil
}

// checkStep records a requested step with the facts as they stand. The
// case is not moved: delivery happens when a gate agentd runs passes.
func (srv *MCPServer) checkStep(args json.RawMessage, t *Track) (*CaseStep, string, []problem) {
	var a struct {
		Kind string `json:"kind"`
	}
	if p := srv.decodeArgs(args, &a); p != nil {
		return nil, "", p
	}
	step := &CaseStep{Kind: a.Kind}
	switch a.Kind {
	case StepReview:
		return step, "recorded; review starts when agentd's facts call for it", nil
	case StepStop:
		return step, "recorded; the owner or the policy stops a case, not a request", nil
	case StepDelivery:
		head, err := gitOut(srv.cfg.Timeouts.Forge.Duration, srv.sess.Cwd, "rev-parse", "HEAD")
		head = strings.TrimSpace(head)
		if err != nil {
			return step, "recorded; the worktree has no HEAD to deliver", nil
		}
		step.Head = head
		if t != nil {
			step.Gated = contains(t.Gated, head)
		}
		if step.Gated {
			return step, "recorded; a passing gate agentd ran is on HEAD " + shortHash(head) + "; delivery follows the facts", nil
		}
		return step, "recorded; no passing gate on HEAD " + shortHash(head) + "; agentd delivers only after its gate passes", nil
	}
	return nil, "", []problem{{format: "step %q is not review, delivery or stop", args: []any{cutUTF8(a.Kind, 40)}}}
}

func (srv *MCPServer) withheld(text string) string {
	for i, re := range srv.confidential {
		if re.MatchString(text) {
			return fmt.Sprintf("(withheld: matches confidential pattern %d)", i+1)
		}
	}
	return text
}

// toolList is the tools the server offers, with their input schemas.
func toolList() []MCPTool {
	evidence := `{"type":"object","properties":{"kind":{"enum":["commit","gate","file","ci","finding"]},"sha":{"type":"string"},"path":{"type":"string"},"line":{"type":"integer","minimum":1,"description":"for a file, the line it shows"},"job":{"type":"string"},"id":{"type":"string"}},"required":["kind"],"additionalProperties":false}`
	report := `{"type":"object","properties":{` +
		`"summary":{"type":"string","description":"where the work stands, at most 3 sentences"},` +
		`"approach":{"type":"string"},` +
		`"items":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string","description":"a done-when item id, A1…"},"status":{"enum":["not_started","in_progress","met"]},"evidence":{"type":"array","items":` + evidence + `}},"required":["id","status","evidence"],"additionalProperties":false}},` +
		`"next_action":{"type":"string"},` +
		`"risks":{"type":"array","items":{"type":"string"}},` +
		`"acknowledged":{"type":"array","items":{"type":"string"},"description":"the active restriction and waiver ids, R1…, not yet acknowledged"},` +
		`"goal_restatement":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"text":{"type":"string"}},"required":["id","text"],"additionalProperties":false}}},` +
		`"required":["summary","items","next_action","acknowledged","goal_restatement"],"additionalProperties":false}`
	step := `{"type":"object","properties":{"kind":{"enum":["review","delivery","stop"]}},"required":["kind"],"additionalProperties":false}`
	limit := `{"type":["number","string"],"description":"a number, a date YYYY-MM-DD, or for choose_many a count of keys"}`
	raise := `{"type":"object","properties":{` +
		`"type":{"enum":["yes_no","choose_one","choose_many","number","date","text"],"description":"the narrowest that fits; text only when the answers cannot be listed"},` +
		`"question":{"type":"string","description":"plain text, at most 500 characters"},` +
		`"context":{"type":"string","description":"what is true today"},` +
		`"why":{"type":"string","description":"why it came up"},` +
		`"if_wrong":{"type":"string","description":"what breaks if the call is wrong, and whether that can be undone"},` +
		`"recommendation":{"type":"string"},` +
		`"options":{"type":"array","description":"for choose_one and choose_many, 2 to 9","items":{"type":"object","properties":{"key":{"type":"string","description":"a, b, c…"},"label":{"type":"string"},"consequence":{"type":"string"}},"required":["key","label","consequence"],"additionalProperties":false}},` +
		`"limits":{"type":"object","properties":{"min":` + limit + `,"max":` + limit + `},"additionalProperties":false},` +
		`"default":{"type":"string","description":"yes or no, a key, keys joined by commas, a number or a date; applied only if the owner sets a deadline"},` +
		`"deadline":{"type":"string","description":"a suggested deadline, RFC 3339 or YYYY-MM-DD; the owner sets the one that applies"},` +
		`"blocking":{"type":"boolean"},` +
		`"supersedes":{"type":"string","description":"the open decision this replaces, after the owner answered it in their own words"}},` +
		`"required":["type","question","context","why","if_wrong","recommendation","blocking"],"additionalProperties":false}`
	finding := `{"type":"object","properties":{` +
		`"id":{"type":"string","description":"an earlier finding's id, F1…, when it comes back; leave out for a new one"},` +
		`"title":{"type":"string","description":"at most 200 characters"},` +
		`"severity":{"enum":["high","medium","low"]},` +
		`"items":{"type":"array","items":{"type":"string"},"description":"the done-when item ids, A1…, it bears on"},` +
		`"widens_scope":{"type":"boolean","description":"true when it bears on no done-when item"},` +
		`"paths":{"type":"array","items":{"type":"string"},"description":"affected paths in the worktree"},` +
		`"consequence":{"type":"string"},"suggestion":{"type":"string"}},` +
		`"required":["title","severity","items","paths","consequence"],"additionalProperties":false}`
	findings := `{"type":"object","properties":{"findings":{"type":"array","items":` + finding + `}},"required":["findings"],"additionalProperties":false}`
	dispositions := `{"type":"object","properties":{` +
		`"dispositions":{"type":"array","items":{"type":"object","properties":{"finding_id":{"type":"string"},"disposition":{"enum":["adopt","decline","defer"]},"reason":{"type":"string"}},"required":["finding_id","disposition","reason"],"additionalProperties":false}},` +
		`"plan":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"step":{"type":"string"}},"required":["id","step"],"additionalProperties":false}},` +
		`"lessons":{"type":"array","items":{"type":"string"},"description":"what went wrong or was surprising, proposed to the owner"}},` +
		`"required":["dispositions","plan"],"additionalProperties":false}`
	return []MCPTool{
		{Name: ToolReportProgress, Description: "Report progress on this session's case. Item ids and restriction ids are the case's; evidence is a typed reference agentd checks, never prose. Restate each done-when item in the issue's words.", InputSchema: json.RawMessage(report)},
		{Name: ToolRaiseDecision, Description: "Ask the owner a decision, with its whole form. agentd gives it an id, D1 on; the answer arrives in answers.json as JSON of its type. An answer in the owner's own words (other) does not settle it: raise it again with options drawn from it, superseding it.", InputSchema: json.RawMessage(raise)},
		{Name: ToolRequestStep, Description: "Ask for review, delivery or stop. A request is recorded, never evidence; agentd moves the case only on the facts.", InputSchema: json.RawMessage(step)},
		{Name: ToolReviewFindings, Description: "Report this round's findings, all of them in one call. agentd numbers new ones F1 on and keeps an id across rounds; a round with nothing new stops the loop.", InputSchema: json.RawMessage(findings)},
		{Name: ToolJudgeDispositions, Description: "Dispose every finding of the latest round exactly once: adopt, decline or defer, with a reason. A finding that widens the scope cannot be adopted.", InputSchema: json.RawMessage(dispositions)},
	}
}

// mcpLaunchConfig is the --mcp-config document that starts the session's
// server: agentd mcp for its session, over this configuration and log.
// A session servesTools refuses gets an empty one.
func mcpLaunchConfig(cfg *Config, name string, sess Session) string {
	if !servesTools(cfg, sess) {
		return emptyMCP
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "agentd"
	}
	args := []string{"mcp"}
	if cfg.Path != "" {
		args = append(args, "--config", cfg.Path)
	}
	args = append(args, "--log", cfg.EventLog, "--session", name)
	return compact(map[string]any{"mcpServers": map[string]any{"agentd": map[string]any{"type": "stdio", "command": exe, "args": args}}})
}

// servesTools says whether the session gets a server: one that works a
// case.
func servesTools(cfg *Config, sess Session) bool {
	return cfg.caseOf(sess) != ""
}

// FactRuling is an active restriction or waiver as a fresh session's
// facts give it, to acknowledge in its first report.
type FactRuling struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// mcpFacts is what a fresh session's facts say of its case, when its
// launch passes the server: the tools, the current done-when items and
// every active ruling, each of which the launch's first report
// acknowledges, since the launch's pack clears what an earlier context
// acknowledged.
func mcpFacts(st *LogState, id, role string) ([]string, []GoalItem, []FactRuling) {
	c := st.Cases[id]
	if c == nil {
		return toolsFor(role), nil, nil
	}
	var rs []FactRuling
	for _, r := range c.Active {
		rs = append(rs, FactRuling{ID: r.ID, Kind: r.Kind, Text: r.Text})
	}
	return toolsFor(role), current(c.Goal), rs
}

// mcpCmd is agentd mcp --session NAME: the session's server on stdio.
func mcpCmd(args []string, r io.Reader, w io.Writer) error {
	// A re-executed server's handover is taken first, so it is removed
	// whatever follows; one it cannot read is said and left.
	pending, why, err := takeHandover()
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentd mcp:", err)
	}
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	session := fs.String("session", "", "the session the server reports for")
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	if *session == "" || fs.NArg() > 0 {
		return errors.New("usage: agentd mcp [--config PATH] [--log PATH] --session NAME")
	}
	log, err := OpenLog(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	read := func() ([]Event, error) {
		events, _, err := log.Load()
		return events, err
	}
	// A session agentd started is the log's, not the configuration's.
	events, err := read()
	if err != nil {
		return err
	}
	srv, err := NewMCPServer(projected(cfg, events), *session, log, read, time.Now)
	if err != nil {
		return err
	}
	if pending != nil {
		srv.note = restartNote(why)
		fmt.Fprintln(os.Stderr, "agentd mcp:", srv.note)
		srv.held = bytes.NewReader(pending)
		r = io.MultiReader(srv.held, r)
	}
	if exe, err := os.Executable(); err == nil {
		srv.replaced = replacedSince(exe, "/proc/self/exe")
		srv.restart = func(pending []byte, why string) error {
			return reexecMCP(exe, append([]string{"mcp"}, args...), pending, why)
		}
	}
	return srv.Serve(r, w)
}
