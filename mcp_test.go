package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The issue body the goal tests record: its done-when items, with an intro
// line and a checkbox that are not items.
const widgetsBody = `Make the widgets spin.

## Done when
Tests show:
- a widget spins;
- [ ] a stopped widget stays stopped
  when asked twice.

## Notes
- not an item
`

// mcpHarness is the case harness with s1's worktree a git repository of
// one commit, s2 on another case, and the goal of widgets#7 recorded.
func mcpHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := caseHarness(t)
	s2 := h.cfg.Sessions["s1"]
	s2.Workdir, s2.Cwd, s2.Issue = filepath.Join(h.dir, "s2"), filepath.Join(h.dir, "s2", "wt"), 8
	h.cfg.Sessions["s2"] = s2
	wt := h.cfg.Sessions["s1"].Cwd
	gitRun(t, wt, "init", "-q")
	writeFile(t, filepath.Join(wt, "spin.go"), "package widgets\n\nfunc Spin() {}\n")
	gitRun(t, wt, "add", "spin.go")
	gitRun(t, wt, "commit", "-q", "-m", "Spin")
	head := gitRun(t, wt, "rev-parse", "HEAD")
	if err := RecordGoal(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), widgetsCase, widgetsBody); err != nil {
		t.Fatal(err)
	}
	return h, head
}

// logState is the log folded as the live run reads it.
func (h *harness) logState() *LogState {
	return NewLogState(h.cfg, ModeLive).Fold(h.events())
}

// mcpServerFor is the server agentd mcp runs for the session, over the
// harness's log.
func (h *harness) mcpServerFor(name string) *MCPServer {
	h.t.Helper()
	srv, err := NewMCPServer(h.cfg, name, NewLogWriter(h.buf), func() ([]Event, error) { return h.events(), nil }, h.clock.Now)
	if err != nil {
		h.t.Fatal(err)
	}
	return srv
}

// rpcReply is one JSON-RPC reply as the fake session reads it.
type rpcReply struct {
	ID     json.RawMessage `json:"id"`
	Result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError         bool            `json:"isError"`
		Tools           []MCPTool       `json:"tools"`
		ProtocolVersion string          `json:"protocolVersion"`
		ServerInfo      json.RawMessage `json:"serverInfo"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// session is a fake session: it writes each request as one line on the
// server's standard input and reads the replies from its standard output.
func session(t *testing.T, srv *MCPServer, requests ...string) []rpcReply {
	t.Helper()
	return sessionOver(t, srv, strings.NewReader(strings.Join(requests, "\n")+"\n"))
}

// sessionOver is session with its input as r.
func sessionOver(t *testing.T, srv *MCPServer, r io.Reader) []rpcReply {
	t.Helper()
	var out bytes.Buffer
	if err := srv.Serve(r, &out); err != nil {
		t.Fatal(err)
	}
	var replies []rpcReply
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		var r rpcReply
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("reply %q: %v", sc.Text(), err)
		}
		replies = append(replies, r)
	}
	return replies
}

// toolCall is a tools/call request for tool with args.
func toolCall(id int, tool string, args any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	return string(b)
}

// callOne makes one tool call and returns its text and whether it was refused.
func callOne(t *testing.T, srv *MCPServer, tool string, args any) (string, bool) {
	t.Helper()
	rs := session(t, srv, toolCall(1, tool, args))
	if len(rs) != 1 || rs[0].Error != nil || len(rs[0].Result.Content) != 1 {
		t.Fatalf("replies %+v", rs)
	}
	return rs[0].Result.Content[0].Text, rs[0].Result.IsError
}

// validReport is a report_progress call valid for widgets#7 with R1 active.
func validReport(head string) map[string]any {
	return map[string]any{
		"summary":  "Spin works. Stopping is next.",
		"approach": "A test per item, then the code.",
		"items": []any{
			map[string]any{"id": "A1", "status": "met", "evidence": []any{
				map[string]any{"kind": "commit", "sha": head[:12]},
				map[string]any{"kind": "file", "path": "spin.go", "line": 3}}},
			map[string]any{"id": "A2", "status": "in_progress", "evidence": []any{}},
		},
		"next_action":  "Write the stop test.",
		"risks":        []any{},
		"acknowledged": []any{"R1"},
		"goal_restatement": []any{
			map[string]any{"id": "A1", "text": "A widget spins."},
			map[string]any{"id": "A2", "text": "a stopped widget stays stopped when asked twice"},
		},
	}
}

func restrict(t *testing.T, h *harness, text string) {
	t.Helper()
	if err := RecordRuling(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), widgetsCase, RulingRestriction, text, "owner"); err != nil {
		t.Fatal(err)
	}
}

func caseByID(t *testing.T, h *harness, id string) CaseView {
	t.Helper()
	for _, c := range casesOf(h, ModeLive) {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no case %s", id)
	return CaseView{}
}

// The issue's done-when list is the case's goal, numbered A1 on; a
// recorded goal read again keeps an unchanged item's id, gives an edited
// one the next id, and marks the old one superseded.
func TestGoalFromDoneWhen(t *testing.T) {
	h, _ := mcpHarness(t)
	want := []GoalItem{{ID: "A1", Text: "a widget spins;", Kind: GoalOwner}, {ID: "A2", Text: "a stopped widget stays stopped when asked twice.", Kind: GoalOwner}}
	if got := caseByID(t, h, widgetsCase).Goal; !reflect.DeepEqual(got, want) {
		t.Fatalf("goal %+v, want %+v", got, want)
	}
	edited := strings.Replace(widgetsBody, "when asked twice.", "when asked three times.", 1)
	if err := RecordGoal(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), widgetsCase, edited); err != nil {
		t.Fatal(err)
	}
	want = []GoalItem{want[0], {ID: "A2", Text: want[1].Text, Kind: GoalOwner, SupersededBy: "A3"},
		{ID: "A3", Text: "a stopped widget stays stopped when asked three times.", Kind: GoalOwner}}
	if got := caseByID(t, h, widgetsCase).Goal; !reflect.DeepEqual(got, want) {
		t.Fatalf("goal %+v, want %+v", got, want)
	}
	if err := RecordGoal(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), widgetsCase, "No list here."); err == nil || !strings.Contains(err.Error(), "Done when") {
		t.Fatalf("a body with no done-when list: %v", err)
	}
	if err := RecordGoal(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), "gitlab/example-org/widgets#99", widgetsBody); err == nil {
		t.Fatal("a goal for an unknown case was recorded")
	}
}

// A fake session over stdio finds both tools, and a call with no such tool
// or method is an error.
func TestMCPHandshake(t *testing.T) {
	h, _ := mcpHarness(t)
	rs := session(t, h.mcpServerFor("s1"),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fake","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"resources/list"}`,
		toolCall(5, "close_case", map[string]any{}),
		`not json`)
	if len(rs) != 6 {
		t.Fatalf("%d replies, want 6 (none to the notification): %+v", len(rs), rs)
	}
	if rs[0].Result.ProtocolVersion != "2025-06-18" || len(rs[0].Result.ServerInfo) == 0 {
		t.Fatalf("initialize %+v", rs[0])
	}
	var names []string
	for _, tool := range rs[1].Result.Tools {
		names = append(names, tool.Name)
		if len(tool.InputSchema) == 0 {
			t.Fatalf("tool %s has no schema", tool.Name)
		}
	}
	if !reflect.DeepEqual(names, []string{"report_progress", "raise_decision", "request_step"}) {
		t.Fatalf("tools %v", names)
	}
	if rs[2].Error != nil || string(rs[2].ID) != "3" {
		t.Fatalf("ping %+v", rs[2])
	}
	if rs[3].Error == nil || rs[3].Error.Code != -32601 || rs[4].Error == nil || rs[4].Error.Code != -32602 || rs[5].Error == nil || rs[5].Error.Code != -32700 {
		t.Fatalf("errors %+v %+v %+v", rs[3], rs[4], rs[5])
	}
}

// A valid report_progress is accepted, logged as a shared fact of the
// session's case, and shown in the case beside the facts.
func TestReportProgressAccepted(t *testing.T) {
	h, head := mcpHarness(t)
	restrict(t, h, "Do not touch the gears.")
	text, refused := callOne(t, h.mcpServerFor("s1"), "report_progress", validReport(head))
	if refused || !strings.Contains(text, `"accepted":true`) {
		t.Fatalf("refused: %s", text)
	}
	var call *Event
	for _, e := range h.events() {
		if e.Kind == KindCase && e.Case.Call != nil {
			e := e
			call = &e
		}
	}
	if call == nil || call.Mode != ModeShared || call.Session != "s1" || call.Case.Case != widgetsCase || call.Case.Call.Refused != "" {
		t.Fatalf("call event %+v", call)
	}
	c := caseByID(t, h, widgetsCase)
	p := c.Progress
	if p == nil || p.Session != "s1" || p.Summary != "Spin works. Stopping is next." || len(p.Items) != 2 || p.Items[0].Status != StatusMet ||
		len(p.Items[0].Evidence) != 2 || p.NextAction != "Write the stop test." || len(c.Drift) != 0 {
		t.Fatalf("progress %+v drift %+v", p, c.Drift)
	}
	if c.State != CaseNotStarted {
		t.Fatalf("a report moved the case to %q", c.State)
	}
	// R1 is acknowledged once; a later call need not repeat it.
	r := validReport(head)
	r["acknowledged"] = []any{}
	if text, refused := callOne(t, h.mcpServerFor("s1"), "report_progress", r); refused {
		t.Fatalf("second call refused: %s", text)
	}
}

// Each mistake is refused with what to fix, and nothing is recorded as
// the case's progress; the refusal itself is logged.
func TestReportProgressRefusals(t *testing.T) {
	cases := []struct {
		name   string
		change func(r map[string]any, head string)
		want   []string
	}{
		{"unknown item", func(r map[string]any, _ string) {
			r["items"] = []any{map[string]any{"id": "A9", "status": "in_progress", "evidence": []any{}}}
		}, []string{"A9", "A1, A2"}},
		{"prose evidence", func(r map[string]any, _ string) {
			r["items"] = []any{map[string]any{"id": "A1", "status": "met", "evidence": []any{"the tests pass"}}}
		}, []string{"prose", "typed reference"}},
		{"unknown evidence kind", func(r map[string]any, _ string) {
			r["items"] = []any{map[string]any{"id": "A1", "status": "met", "evidence": []any{map[string]any{"kind": "screenshot"}}}}
		}, []string{"screenshot", "commit, gate, file, ci or finding"}},
		{"met without evidence", func(r map[string]any, _ string) {
			r["items"] = []any{map[string]any{"id": "A1", "status": "met", "evidence": []any{}}}
		}, []string{"A1", "met needs evidence"}},
		{"missing acknowledgement", func(r map[string]any, _ string) {
			r["acknowledged"] = []any{}
		}, []string{"acknowledge", "R1"}},
		{"unknown commit", func(r map[string]any, _ string) {
			r["items"] = []any{map[string]any{"id": "A1", "status": "met", "evidence": []any{map[string]any{"kind": "commit", "sha": "deadbeefdeadbeef"}}}}
		}, []string{"deadbeefdeadbeef", "not a commit"}},
		{"file outside the worktree", func(r map[string]any, _ string) {
			r["items"] = []any{map[string]any{"id": "A1", "status": "met", "evidence": []any{map[string]any{"kind": "file", "path": "../../STATUS.md"}}}}
		}, []string{"../../STATUS.md", "inside the worktree"}},
		{"line past the end", func(r map[string]any, _ string) {
			r["items"] = []any{map[string]any{"id": "A1", "status": "met", "evidence": []any{map[string]any{"kind": "file", "path": "spin.go", "line": 40}}}}
		}, []string{"spin.go", "3 lines"}},
		{"file without a line", func(r map[string]any, _ string) {
			r["items"] = []any{map[string]any{"id": "A1", "status": "met", "evidence": []any{map[string]any{"kind": "file", "path": "spin.go"}}}}
		}, []string{"spin.go", "needs the line"}},
		{"gate never run", func(r map[string]any, head string) {
			r["items"] = []any{map[string]any{"id": "A1", "status": "met", "evidence": []any{map[string]any{"kind": "gate", "sha": head}}}}
		}, []string{"no passing gate"}},
		{"ci not yet recorded", func(r map[string]any, head string) {
			r["items"] = []any{map[string]any{"id": "A1", "status": "met", "evidence": []any{map[string]any{"kind": "ci", "sha": head, "job": "test"}}}}
		}, []string{"no CI results"}},
		{"bad status", func(r map[string]any, _ string) {
			r["items"] = []any{map[string]any{"id": "A1", "status": "done", "evidence": []any{}}}
		}, []string{"not_started, in_progress or met"}},
		{"duplicate item", func(r map[string]any, _ string) {
			r["items"] = []any{map[string]any{"id": "A2", "status": "in_progress", "evidence": []any{}}, map[string]any{"id": "A2", "status": "in_progress", "evidence": []any{}}}
		}, []string{"A2", "twice"}},
		{"unknown restatement", func(r map[string]any, _ string) {
			r["goal_restatement"] = []any{map[string]any{"id": "A7", "text": "something else"}}
		}, []string{"A7"}},
		{"long summary", func(r map[string]any, _ string) {
			r["summary"] = "One. Two. Three. Four."
		}, []string{"summary", "3 sentences"}},
		{"no next action", func(r map[string]any, _ string) {
			r["next_action"] = ""
		}, []string{"next_action"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, head := mcpHarness(t)
			restrict(t, h, "Do not touch the gears.")
			r := validReport(head)
			tc.change(r, head)
			text, refused := callOne(t, h.mcpServerFor("s1"), "report_progress", r)
			if !refused {
				t.Fatalf("accepted: %s", text)
			}
			for _, w := range tc.want {
				if !strings.Contains(text, w) {
					t.Fatalf("refusal %q does not say %q", text, w)
				}
			}
			c := caseByID(t, h, widgetsCase)
			if c.Progress != nil || c.Refused != 1 {
				t.Fatalf("progress %+v, refused %d", c.Progress, c.Refused)
			}
		})
	}
}

// The server is bound to its session's case: a call cannot name another,
// and what it records goes to its own case only.
func TestMCPCannotWriteAnotherCase(t *testing.T) {
	h, head := mcpHarness(t)
	r := validReport(head)
	r["acknowledged"] = []any{}
	r["case"] = "gitlab/example-org/widgets#8"
	text, refused := callOne(t, h.mcpServerFor("s1"), "report_progress", r)
	if !refused || !strings.Contains(text, `"case"`) || !strings.Contains(text, widgetsCase) {
		t.Fatalf("a call naming a case: %s", text)
	}
	if text, refused := callOne(t, h.mcpServerFor("s1"), "request_step", map[string]any{"kind": "review", "case": "gitlab/example-org/widgets#8"}); !refused {
		t.Fatalf("a step naming a case: %s", text)
	}
	delete(r, "case")
	if text, refused := callOne(t, h.mcpServerFor("s1"), "report_progress", r); refused {
		t.Fatalf("refused: %s", text)
	}
	for _, e := range h.events() {
		if e.Kind == KindCase && e.Case.Call != nil && (e.Case.Case != widgetsCase || e.Session != "s1") {
			t.Fatalf("s1's server wrote %s for %s", e.Case.Case, e.Session)
		}
	}
	if c := caseByID(t, h, "gitlab/example-org/widgets#8"); c.Progress != nil || c.Refused != 0 {
		t.Fatalf("widgets#8 %+v", c)
	}
	// A session that works no case gets no server.
	s := h.cfg.Sessions["s2"]
	s.Issue = 0
	h.cfg.Sessions["s2"] = s
	if _, err := NewMCPServer(h.cfg, "s2", NewLogWriter(h.buf), func() ([]Event, error) { return h.events(), nil }, h.clock.Now); err == nil {
		t.Fatal("a server for a session with no case")
	}
}

// A restatement that rewords a done-when item, or leaves one out, raises a
// drift flag on the case; the report is still accepted, and a later
// faithful one clears it.
func TestRewordedItemRaisesDrift(t *testing.T) {
	h, head := mcpHarness(t)
	restrict(t, h, "Do not touch the gears.")
	r := validReport(head)
	r["goal_restatement"] = []any{map[string]any{"id": "A2", "text": "a stopped widget may restart"}}
	text, refused := callOne(t, h.mcpServerFor("s1"), "report_progress", r)
	if refused || !strings.Contains(text, "drift") {
		t.Fatalf("drifting report: %s (refused %v)", text, refused)
	}
	want := []DriftFlag{{Item: "A1", Kind: DriftMissing}, {Item: "A2", Kind: DriftReworded, Text: "a stopped widget may restart"}}
	if got := caseByID(t, h, widgetsCase).Drift; !reflect.DeepEqual(got, want) {
		t.Fatalf("drift %+v, want %+v", got, want)
	}
	if text, refused := callOne(t, h.mcpServerFor("s1"), "report_progress", validReport(head)); refused {
		t.Fatalf("refused: %s", text)
	}
	if got := caseByID(t, h, widgetsCase).Drift; len(got) != 0 {
		t.Fatalf("drift after a faithful report %+v", got)
	}
}

// request_step(delivery) is recorded with the facts, never moves the case,
// and says there is no passing gate; a passing gate agentd ran is what
// the reply and gate evidence then name.
func TestRequestDeliveryWithoutGateDoesNotMove(t *testing.T) {
	h, head := mcpHarness(t)
	before := caseByID(t, h, widgetsCase).State
	text, refused := callOne(t, h.mcpServerFor("s1"), "request_step", map[string]any{"kind": "delivery"})
	if refused || !strings.Contains(text, `"moved":false`) || !strings.Contains(text, "no passing gate") {
		t.Fatalf("delivery request: %s", text)
	}
	c := caseByID(t, h, widgetsCase)
	if c.State != before || len(c.Steps) != 1 || c.Steps[0].Kind != StepDelivery || c.Steps[0].Gated || c.Steps[0].Head != head {
		t.Fatalf("case %q (was %q), steps %+v", c.State, before, c.Steps)
	}
	if text, refused := callOne(t, h.mcpServerFor("s1"), "request_step", map[string]any{"kind": "merge"}); !refused || !strings.Contains(text, "review, delivery or stop") {
		t.Fatalf("unknown step: %s", text)
	}
	// The gate passes on HEAD, as agentd ran it.
	zero := 0
	h.sup.emit("s1", Event{Kind: KindAction, Act: &ActPayload{Action: ActGate, Key: "deliver:x:gate:" + head, Gate: []string{"./bin/check"}, SHA: head, Exit: &zero}})
	text, _ = callOne(t, h.mcpServerFor("s1"), "request_step", map[string]any{"kind": "delivery"})
	if !strings.Contains(text, `"moved":false`) || !strings.Contains(text, "passing gate") || strings.Contains(text, "no passing gate") {
		t.Fatalf("delivery request after a gate: %s", text)
	}
	if c := caseByID(t, h, widgetsCase); c.State != before || !c.Steps[len(c.Steps)-1].Gated {
		t.Fatalf("case %q, steps %+v", c.State, c.Steps)
	}
	r := validReport(head)
	r["acknowledged"] = []any{}
	r["items"] = []any{map[string]any{"id": "A1", "status": "met", "evidence": []any{map[string]any{"kind": "gate", "sha": head[:10]}}}}
	if text, refused := callOne(t, h.mcpServerFor("s1"), "report_progress", r); refused {
		t.Fatalf("gate evidence refused: %s", text)
	}
}

// The facts pack names the tools when the launch command passes the
// server, and the server is passed for the session's own case.
func TestFactsPackNamesTheTools(t *testing.T) {
	h, _ := mcpHarness(t)
	restrict(t, h, "Do not touch the gears.")
	h.sup.Replay(h.events())
	sess := h.cfg.Sessions["s1"]
	cmd, facts := h.sup.launchCommand("s1", sess, newTrack(), "id-1", "restart")
	var f restartFacts
	if err := json.Unmarshal(facts, &f); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Tools, []string{"report_progress", "raise_decision", "request_step"}) || len(f.Items) != 2 || f.Items[0].ID != "A1" ||
		!reflect.DeepEqual(f.Acknowledge, []FactRuling{{ID: "R1", Kind: RulingRestriction, Text: "Do not touch the gears."}}) {
		t.Fatalf("facts %s", facts)
	}
	var mcp struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cmd[len(cmd)-2]), &mcp); err != nil {
		t.Fatalf("mcp config %q: %v", cmd[len(cmd)-2], err)
	}
	srv, ok := mcp.MCPServers["agentd"]
	if !ok || srv.Command == "" || len(srv.Args) < 3 || srv.Args[0] != "mcp" || !strings.HasSuffix(strings.Join(srv.Args, " "), "--session s1") {
		t.Fatalf("mcp config %+v", mcp)
	}
	// A command without {mcp} gets no server, and its facts name no tools.
	h.cfg.Policies.Restart.Command = []string{"claude", "{prompt}"}
	if _, facts := h.sup.launchCommand("s1", sess, newTrack(), "id-1", "restart"); strings.Contains(string(facts), "report_progress") {
		t.Fatalf("facts without a server %s", facts)
	}
}

// STATUS.md's report stays readable, marked unverified, beside the
// checked one.
func TestStatusReportIsUnverified(t *testing.T) {
	h, _ := mcpHarness(t)
	h.alive("s1", "working")
	h.status("s1", progressText("second", "in progress"))
	h.ticks(1, time.Minute)
	if c := caseByID(t, h, widgetsCase); c.Item != "second" || !c.Unverified {
		t.Fatalf("case %+v", c)
	}
}

// agentd cases goal and agentd mcp, as the owner and a launched session run
// them: a configuration file, the event log on disk, stdio.
func TestMCPCommand(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, "s1", "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath, log := filepath.Join(dir, "agentd.json"), filepath.Join(dir, "events.jsonl")
	b, _ := json.Marshal(map[string]any{"event_log": log, "policies": map[string]any{"escalate": map[string]any{"command": []string{"route", "{message}"}}},
		"repositories": map[string]any{"widgets": map[string]any{"forge": "gitlab", "host": "gitlab.example.com", "project": "example-org/widgets"}},
		"sessions":     map[string]any{"s1": map[string]any{"workdir": filepath.Join(dir, "s1"), "cwd": wt, "repository": "widgets", "issue": 7}}})
	writeFile(t, cfgPath, string(b))
	body := filepath.Join(dir, "body.md")
	writeFile(t, body, widgetsBody)
	if err := casesCmd([]string{"--config", cfgPath, "goal", widgetsCase, body}, io.Discard); err != nil {
		t.Fatal(err)
	}
	report := toolCall(2, "report_progress", map[string]any{"summary": "Started.", "items": []any{map[string]any{"id": "A1", "status": "in_progress", "evidence": []any{}}},
		"next_action": "Write the spin test.", "acknowledged": []any{}, "goal_restatement": []any{map[string]any{"id": "A1", "text": "a widget spins"}}})
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n" + report + "\n")
	var out bytes.Buffer
	if err := mcpCmd([]string{"--config", cfgPath, "--session", "s1"}, in, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `\"accepted\":true`) || !strings.Contains(out.String(), `\"drift\":[{\"item\":\"A2\",\"kind\":\"missing\"}]`) {
		t.Fatalf("replies %s", out.String())
	}
	var cases bytes.Buffer
	if err := casesCmd([]string{"--config", cfgPath, "--json"}, &cases); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cases.String(), `"summary":"Started."`) {
		t.Fatalf("cases %s", cases.String())
	}
	if err := mcpCmd([]string{"--config", cfgPath, "--session", "nobody"}, strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("a server for no session")
	}
}

// A message over the limit is answered as unreadable and the server goes
// on with the next.
func TestMCPOversizedMessage(t *testing.T) {
	h, _ := mcpHarness(t)
	rs := session(t, h.mcpServerFor("s1"), `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"`+strings.Repeat("x", mcpMaxLine)+`"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if len(rs) != 2 || rs[0].Error == nil || rs[0].Error.Code != -32700 || rs[1].Error != nil || string(rs[1].ID) != "2" {
		t.Fatalf("replies %+v", rs)
	}
	head, tail := `{"jsonrpc":"2.0","id":3,"method":"ping","params":{"pad":"`, `"}}`
	exact := head + strings.Repeat("x", mcpMaxLine-len(head)-len(tail)) + tail
	if rs := session(t, h.mcpServerFor("s1"), exact); len(rs) != 1 || rs[0].Error != nil {
		t.Fatalf("a message of exactly the limit: %+v", rs)
	}
}

// A refusal tells the session what it sent; the log keeps only the
// wording, so nothing the session wrote reaches it unscreened.
func TestRefusalLogsNoArguments(t *testing.T) {
	h, head := mcpHarness(t)
	r := validReport(head)
	r["items"] = []any{map[string]any{"id": "SECRET-abc123", "status": "in_progress", "evidence": []any{}},
		map[string]any{"id": "A1", "status": "met", "evidence": []any{"secret prose here"}}}
	text, refused := callOne(t, h.mcpServerFor("s1"), "report_progress", r)
	if !refused || !strings.Contains(text, "SECRET-abc123") {
		t.Fatalf("reply %s", text)
	}
	for _, e := range h.events() {
		if e.Kind == KindCase && e.Case.Call != nil {
			if got := e.Case.Call.Refused; got == "" || strings.Contains(got, "SECRET") || strings.Contains(got, "secret prose") || !strings.Contains(got, "is not one of the case's") {
				t.Fatalf("logged refusal %q", got)
			}
		}
	}
}

// A call the session's server logged while agentd runs is folded into the
// running state as replay folds it. A fresh launch is a fresh context: its
// facts give the rulings' text, and its first report acknowledges them
// again.
func TestRunningAgentdFoldsCalls(t *testing.T) {
	h, head := mcpHarness(t)
	restrict(t, h, "Do not touch the gears.")
	h.alive("s1", "working")
	h.ticks(1, time.Minute)
	if _, refused := callOne(t, h.mcpServerFor("s1"), "report_progress", validReport(head)); refused {
		t.Fatal("refused")
	}
	h.ticks(1, time.Minute)
	if c := h.sup.st.Cases[widgetsCase]; c == nil || c.Progress == nil || !reflect.DeepEqual(c.Acked["s1"], []string{"R1"}) {
		t.Fatalf("running case %+v", c)
	}
	r := validReport(head)
	r["acknowledged"] = []any{}
	if text, refused := callOne(t, h.mcpServerFor("s1"), "report_progress", r); refused {
		t.Fatalf("the same context asked again: %s", text)
	}
	_, facts := h.sup.launchCommand("s1", h.cfg.Sessions["s1"], h.sup.Tracks()["s1"], "id-2", "restart")
	if !strings.Contains(string(facts), `"acknowledge":[{"id":"R1","kind":"restriction","text":"Do not touch the gears."}]`) {
		t.Fatalf("facts %s", facts)
	}
	h.sup.notePack("s1", h.cfg.Sessions["s1"], facts)
	if text, refused := callOne(t, h.mcpServerFor("s1"), "report_progress", r); !refused || !strings.Contains(text, "R1") {
		t.Fatalf("a fresh launch's report without the acknowledgement: %s", text)
	}
}

// Recording the goal again after one item is dropped and another edited
// supersedes the edited one by its new wording and marks the dropped one
// removed.
func TestGoalDropAndEdit(t *testing.T) {
	h, _ := mcpHarness(t)
	body := "## Done when\n- the gears turn\n- the lid opens\n- the bell rings once\n"
	if err := RecordGoal(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), "gitlab/example-org/widgets#8", body); err != nil {
		t.Fatal(err)
	}
	body = "## Done when\n- the gears turn\n- the bell rings twice\n"
	if err := RecordGoal(NewLogWriter(h.buf), h.cfg, h.logState(), h.clock.Now(), "gitlab/example-org/widgets#8", body); err != nil {
		t.Fatal(err)
	}
	want := []GoalItem{{ID: "A1", Text: "the gears turn", Kind: GoalOwner}, {ID: "A2", Text: "the lid opens", Kind: GoalOwner, SupersededBy: "removed"},
		{ID: "A3", Text: "the bell rings once", Kind: GoalOwner, SupersededBy: "A4"}, {ID: "A4", Text: "the bell rings twice", Kind: GoalOwner}}
	if got := caseByID(t, h, "gitlab/example-org/widgets#8").Goal; !reflect.DeepEqual(got, want) {
		t.Fatalf("goal %+v, want %+v", got, want)
	}
}

// A nested item belongs to the item above it, and a section headed in bold
// ends at the next line in bold.
func TestDoneWhenNestedAndBold(t *testing.T) {
	got := DoneWhen("## Done when\n- a\n  - sub a\n- b\n")
	if !reflect.DeepEqual(got, []string{"a - sub a", "b"}) {
		t.Fatalf("nested %q", got)
	}
	got = DoneWhen("**Done when**\n- a\n\n**Notes**\n- not an item\n")
	if !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("bold %q", got)
	}
}

// An evidence reference carries only its kind's fields; a top-level list
// indented a little is still a list of items; a call sent as a
// notification is not acted on; two short items sharing a word are not
// alike.
func TestReview2Edges(t *testing.T) {
	h, head := mcpHarness(t)
	r := validReport(head)
	r["acknowledged"] = []any{}
	r["items"] = []any{map[string]any{"id": "A1", "status": "met", "evidence": []any{map[string]any{"kind": "commit", "sha": head, "path": "anything at all"}}}}
	if text, refused := callOne(t, h.mcpServerFor("s1"), "report_progress", r); !refused || !strings.Contains(text, "only its own fields") {
		t.Fatalf("extra fields: %s", text)
	}
	if got := DoneWhen("## Done when\n  - a\n  - b\n"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("indented list %q", got)
	}
	before := len(h.events())
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "tools/call", "params": map[string]any{"name": "request_step", "arguments": map[string]any{"kind": "review"}}})
	if rs := session(t, h.mcpServerFor("s1"), string(b)); len(rs) != 0 || len(h.events()) != before {
		t.Fatalf("a call as a notification: replies %+v, events %d to %d", rs, before, len(h.events()))
	}
	if _, ok := alike("tests pass", "docs pass"); ok {
		t.Fatal("tests pass and docs pass are alike")
	}
}

// staleServer is s1's server as a build older than its log runs it: every
// read fails as the old reader fails on a field it does not know.
func staleServer(h *harness, replaced bool) *MCPServer {
	srv := h.mcpServerFor("s1")
	srv.read = func() ([]Event, error) {
		return nil, errors.New(`event log line 2 is malformed (json: unknown field "kind")`)
	}
	srv.replaced = func() bool { return replaced }
	return srv
}

// A server whose log it cannot read, once the binary it runs was
// replaced, re-executes the binary on disk with the call it has not
// answered and the input after it; the new server answers them, and its
// first tool reply says it restarted (#45).
func TestMCPRestartsOnReplacedBuild(t *testing.T) {
	h, head := mcpHarness(t)
	srv := staleServer(h, true)
	var pending []byte
	var why string
	srv.restart = func(p []byte, w string) error { pending, why = p, w; return nil }
	args := validReport(head)
	args["acknowledged"] = []any{}
	report, ping := toolCall(1, "report_progress", args), `{"jsonrpc":"2.0","id":2,"method":"ping"}`
	if rs := session(t, srv, report, ping); len(rs) != 0 {
		t.Fatalf("the old server answered: %+v", rs)
	}
	if string(pending) != report+"\n"+ping+"\n" || !strings.Contains(why, `unknown field "kind"`) {
		t.Fatalf("handed over %q, why %q", pending, why)
	}
	if n := h.count(KindCase, "", "s1"); n != 0 {
		t.Fatalf("the old server logged %d calls", n)
	}
	next := h.mcpServerFor("s1")
	next.note = restartNote(why)
	rs := session(t, next, strings.Split(strings.TrimSuffix(string(pending), "\n"), "\n")...)
	if len(rs) != 2 || rs[0].Result.IsError || string(rs[1].ID) != "2" {
		t.Fatalf("replies %+v", rs)
	}
	text := rs[0].Result.Content[0].Text
	if !strings.Contains(text, `"accepted":true`) || !strings.Contains(text, `"restarted":"agentd's tool server was on an older build`) {
		t.Fatalf("first reply %s", text)
	}
	if text, _ := callOne(t, next, "report_progress", args); strings.Contains(text, "restarted") {
		t.Fatalf("a later reply says it restarted: %s", text)
	}
}

// A re-executed server that restarts again, before it has read all it
// was handed, hands on the rest of it too, ahead of its own input.
func TestMCPRestartHandsOnHeldInput(t *testing.T) {
	h, _ := mcpHarness(t)
	srv := staleServer(h, true)
	var pending []byte
	srv.restart = func(p []byte, _ string) error { pending = p; return nil }
	call := toolCall(1, ToolRequestStep, map[string]any{"kind": StepReview})
	held := call + "\n" + `{"jsonrpc":"2.0","id":2,"method":"ping","params":{"pad":"` + strings.Repeat("x", 200<<10) + `"}}` + "\n"
	own := `{"jsonrpc":"2.0","id":3,"method":"ping"}` + "\n"
	srv.held = bytes.NewReader([]byte(held))
	var out bytes.Buffer
	if err := srv.Serve(io.MultiReader(srv.held, strings.NewReader(own)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || string(pending) != held {
		t.Fatalf("replied %q; handed on %d bytes, want the %d held", out.String(), len(pending), len(held))
	}
	// A restart that fails serves all of it still.
	srv = staleServer(h, true)
	srv.restart = func([]byte, string) error { return errors.New("exec format error") }
	srv.held = bytes.NewReader([]byte(held))
	rs := sessionOver(t, srv, io.MultiReader(srv.held, strings.NewReader(own)))
	if len(rs) != 3 || string(rs[1].ID) != "2" || string(rs[2].ID) != "3" {
		t.Fatalf("%d replies after a failed restart", len(rs))
	}
}

// A server whose binary is unchanged refuses as before, and says what to
// do; one whose re-execution fails says that, and goes on serving.
func TestMCPStaleWithoutRestart(t *testing.T) {
	h, head := mcpHarness(t)
	text, refused := callOne(t, staleServer(h, false), "report_progress", validReport(head))
	if !refused || !strings.Contains(text, `unknown field \"kind\"`) || !strings.Contains(text, "restart this session") {
		t.Fatalf("unchanged binary: %s", text)
	}
	srv := staleServer(h, true)
	tries := 0
	srv.restart = func([]byte, string) error { tries++; return errors.New("exec format error") }
	rs := session(t, srv, toolCall(1, "report_progress", validReport(head)), toolCall(2, "report_progress", validReport(head)))
	if len(rs) != 2 || tries != 1 {
		t.Fatalf("%d replies, %d tries", len(rs), tries)
	}
	for _, r := range rs {
		if text := r.Result.Content[0].Text; !r.Result.IsError || !strings.Contains(text, "exec format error") || !strings.Contains(text, "restart this session") {
			t.Fatalf("reply %s", text)
		}
	}
}

// agentd mcp started with a handed-over input answers it before its own
// standard input, removes the file and says it restarted.
func TestMCPCommandResumesHandover(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "s1"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "agentd.json")
	b, _ := json.Marshal(map[string]any{"event_log": filepath.Join(dir, "events.jsonl"), "policies": map[string]any{"escalate": map[string]any{"command": []string{"route", "{message}"}}},
		"repositories": map[string]any{"widgets": map[string]any{"forge": "gitlab", "host": "gitlab.example.com", "project": "example-org/widgets"}},
		"sessions":     map[string]any{"s1": map[string]any{"workdir": filepath.Join(dir, "s1"), "cwd": dir, "repository": "widgets", "issue": 7}}})
	writeFile(t, cfgPath, string(b))
	handover := filepath.Join(dir, "handover.json")
	call := toolCall(1, ToolRequestStep, map[string]any{"kind": StepReview})
	hb, _ := json.Marshal(mcpHandover{Input: []byte(call + "\n"), Why: "line 2 is malformed"})
	writeFile(t, handover, string(hb))
	t.Setenv(mcpHandoverEnv, handover)
	var out bytes.Buffer
	if err := mcpCmd([]string{"--config", cfgPath, "--session", "s1"}, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"ping"}`+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `\"restarted\":`) || !strings.Contains(lines[1], `"id":2`) {
		t.Fatalf("replies %s", out.String())
	}
	if _, err := os.Stat(handover); !os.IsNotExist(err) {
		t.Fatalf("the handover file is left: %v", err)
	}
	if os.Getenv(mcpHandoverEnv) != "" {
		t.Fatal("the handover is still in the environment")
	}
}

// reexecMCP replaces the process with the binary given, which reads the
// handover the variable names and then the same standard input.
func TestMCPReexec(t *testing.T) {
	if os.Getenv("AGENTD_TEST_REEXEC") != "" {
		err := reexecMCP(os.Getenv("AGENTD_TEST_REEXEC"), []string{"mcp", "--session", "s1"}, []byte("pending\n"), "why")
		fmt.Fprintln(os.Stderr, "reexec returned:", err)
		os.Exit(3)
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "agentd")
	writeFile(t, exe, "#!/bin/sh\necho \"args $*\"\ncat \"$"+mcpHandoverEnv+"\"; echo\ncat\n")
	if err := os.Chmod(exe, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMCPReexec$")
	cmd.Env = append(os.Environ(), "AGENTD_TEST_REEXEC="+exe, "TMPDIR="+dir)
	cmd.Stdin = strings.NewReader("rest of the input\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	hb, _ := json.Marshal(mcpHandover{Input: []byte("pending\n"), Why: "why"})
	if want := "args mcp --session s1\n" + string(hb) + "\nrest of the input\n"; string(out) != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "agentd-mcp-*.json")); len(left) != 1 {
		t.Fatalf("handover files %v, want the one the new binary takes", left)
	}
}

// replacedSince says a binary moved over the one noted is replaced, and
// the one noted is not.
func TestReplacedSince(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agentd")
	writeFile(t, exe, "old")
	replaced := replacedSince(exe, filepath.Join(dir, "no image"))
	if replaced == nil || replaced() {
		t.Fatal("the binary noted is replaced")
	}
	writeFile(t, exe+".new", "new build")
	if err := os.Rename(exe+".new", exe); err != nil {
		t.Fatal(err)
	}
	if !replaced() {
		t.Fatal("a binary moved over it is not replaced")
	}
	writeFile(t, filepath.Join(dir, "elsewhere"), "another build")
	if image := replacedSince(filepath.Join(dir, "elsewhere"), exe); image == nil || !image() {
		t.Fatal("the running image is not compared with the binary on disk")
	}
	if replacedSince(filepath.Join(dir, "missing"), filepath.Join(dir, "missing")) != nil {
		t.Fatal("a binary that cannot be read is watched")
	}
}
