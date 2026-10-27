package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// The ops tools (#63) on the agentd MCP server, served to the ops
// console's agent alone: agentd mcp --ops. The reads answer from the
// supervisors' configs, logs and panes; each change tool checks the
// change, records it proposed in the ops-serving log, which the console
// shows as a decision entry, and answers with its entry id. No tool
// executes a change.

// NewOpsServer is the server of the ops tools and ask_expert, for the
// ops console's agent; as names it in the log, as the thread it serves.
func NewOpsServer(cfg *Config, as string, log *Log, read func() ([]Event, error), now func() time.Time) (*MCPServer, error) {
	if !cfg.Ops.on() {
		return nil, fmt.Errorf("%s has no ops block", cfg.Path)
	}
	srv := newServer(cfg, as, Session{Role: RoleOps, Cwd: cfg.Ops.Repo}, "", log, read, now)
	srv.ops = newOpsEnv(cfg)
	return srv, nil
}

// mcpOpsConfig is the --mcp-config of the ops agent of thread name.
func mcpOpsConfig(cfg *Config, name string) string {
	exe, err := os.Executable()
	if err != nil {
		exe = "agentd"
	}
	args := []string{"mcp"}
	if cfg.Path != "" {
		args = append(args, "--config", cfg.Path)
	}
	args = append(args, "--log", cfg.EventLog, "--ops")
	if name != "" {
		args = append(args, "--as", name)
	}
	return compact(map[string]any{"mcpServers": map[string]any{"agentd": map[string]any{"type": "stdio", "command": exe, "args": args}}})
}

// opsInstructions is what the ops server tells its agent.
const opsInstructions = "Read agentd's supervisors with ops_status, ops_accounts, ops_config, ops_tail and ops_events. " +
	"The change tools only propose: each change becomes an entry the owner answers yes or no, and Go carries it out on yes alone. When out of your depth, ask_expert."

func opsToolList() []MCPTool {
	sup := `"supervisor":{"type":"string","description":"a supervisor's name, as ops_status lists it"}`
	why := `"why":{"type":"string","description":"why the owner should make this change, in a sentence"}`
	obj := func(props string, required ...string) json.RawMessage {
		req := ""
		if len(required) > 0 {
			req = `,"required":["` + strings.Join(required, `","`) + `"]`
		}
		return json.RawMessage(`{"type":"object","properties":{` + props + `}` + req + `,"additionalProperties":false}`)
	}
	return []MCPTool{
		{Name: ToolOpsStatus, Description: "Each supervisor's agentd status table, as JSON, and whether its process is up; or one supervisor's.", InputSchema: obj(sup)},
		{Name: ToolOpsAccounts, Description: "Each account's plan usage, read now: five-hour and weekly utilization and their resets.", InputSchema: obj("")},
		{Name: ToolOpsConfig, Description: "A supervisor's resolved config, as agentd config prints it, with credentials and token paths redacted.", InputSchema: obj(sup, "supervisor")},
		{Name: ToolOpsTail, Description: "The last lines of a supervisor's tmux pane, screened.", InputSchema: obj(sup+`,"lines":{"type":"integer","minimum":1,"maximum":200}`, "supervisor")},
		{Name: ToolOpsEvents, Description: "The last events of a kind (for example error, route, ops, start, stop) in a supervisor's log, each as JSON, screened.", InputSchema: obj(sup+`,"kind":{"type":"string"},"n":{"type":"integer","minimum":1,"maximum":50}`, "supervisor")},
		{Name: ToolOpsConfigSet, Description: "Propose setting, or deleting, one value of a supervisor's config file at a JSON pointer (RFC 6901, such as /policies/nudge/max). The change is checked as agentd config checks it and shown to the owner as a diff; on the owner's yes Go writes it with a timestamped backup and restarts the supervisors that run the file.", InputSchema: obj(sup+`,"pointer":{"type":"string"},"value":{"description":"the JSON value to set"},"delete":{"type":"boolean"},`+why, "supervisor", "pointer", "why")},
		{Name: ToolOpsRestart, Description: "Propose restarting a supervisor in its tmux session: Ctrl-C, then Up and Enter. One that is down is restarted without the owner's yes.", InputSchema: obj(sup+","+why, "supervisor", "why")},
		{Name: ToolOpsInstall, Description: "Propose installing a commit of the clone's main: Go builds it, keeps the running binary as agentd.SHA, swaps the new one in, runs agentd config on every supervisor's config and restarts them all; on any failure it puts the previous binary back.", InputSchema: obj(`"commit":{"type":"string","description":"a commit on main, its hex id"},`+why, "commit", "why")},
		{Name: ToolOpsClear, Description: "Propose clearing a parked, crash-looping session on a supervisor, so it may be restarted (agentd clear).", InputSchema: obj(sup+`,"session":{"type":"string"},`+why, "supervisor", "session", "why")},
		{Name: ToolOpsEnd, Description: "Propose ending a session agentd started on a supervisor, keeping its files (agentd sessions end).", InputSchema: obj(sup+`,"session":{"type":"string"},`+why, "supervisor", "session", "why")},
		{Name: ToolOpsRollback, Description: "Propose rolling agentd back to a kept binary, agentd.SHA, which ops_status's facts list: swapped in, checked and every supervisor restarted.", InputSchema: obj(`"binary":{"type":"string","description":"a kept binary's name, agentd.SHA"},`+why, "binary", "why")},
	}
}

// opsCall answers one ops tool call: a read, or a change proposed.
func (srv *MCPServer) opsCall(tool string, args json.RawMessage) (string, bool) {
	o := srv.ops
	var a struct {
		opsArgs
		Lines int    `json:"lines"`
		Kind  string `json:"kind"`
		N     int    `json:"n"`
	}
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	if p := srv.decodeArgs(args, &a); p != nil {
		return compact(map[string]any{"refused": p[0].reply()}), true
	}
	refuse := func(err error) (string, bool) {
		return compact(map[string]any{"refused": srv.withheld(cutUTF8(err.Error(), 2000))}), true
	}
	switch tool {
	case ToolOpsStatus:
		var names []string
		if a.Supervisor != "" {
			if _, ok := o.cfg.Ops.Supervisors[a.Supervisor]; !ok {
				_, _, err := o.supervisor(a.Supervisor)
				return refuse(err)
			}
			names = []string{a.Supervisor}
		}
		installed, main := o.installed()
		return compact(map[string]any{"supervisors": o.status(names, true), "installed": short(installed), "main": short(main), "kept": o.keptBinaries()}), false
	case ToolOpsAccounts:
		return compact(map[string]any{"accounts": o.accounts(o.configs()), "at": o.now().UTC()}), false
	case ToolOpsConfig:
		v, err := o.resolvedConfig(a.Supervisor)
		if err != nil {
			return refuse(err)
		}
		return compact(map[string]any{"supervisor": a.Supervisor, "config": v}), false
	case ToolOpsTail:
		text, err := o.tail(a.Supervisor, a.Lines)
		if err != nil {
			return refuse(err)
		}
		return compact(map[string]any{"supervisor": a.Supervisor, "tail": text}), false
	case ToolOpsEvents:
		events, err := o.events(a.Supervisor, a.Kind, a.N)
		if err != nil {
			return refuse(err)
		}
		return compact(map[string]any{"supervisor": a.Supervisor, "events": events}), false
	}
	op, ok := opsChangeOf[tool]
	if !ok {
		return refuse(fmt.Errorf("no tool %q", tool))
	}
	c, err := o.propose(op, a.opsArgs)
	if err != nil {
		return refuse(err)
	}
	c.Thread = strings.TrimPrefix(srv.name, "thread:")
	c.Summary, c.Why = srv.withheld(c.Summary), srv.withheld(c.Why)
	events, err := srv.load()
	if err != nil {
		return refuse(err)
	}
	st := NewLogState(srv.cfg, ModeLive).Fold(events)
	for _, id := range sortedKeys(st.Ops) {
		if r := st.Ops[id]; r.open() && sameChange(r.OpsChange, c) {
			return compact(map[string]any{"proposed": entryOps + id, "summary": r.Summary, "note": "this change is proposed already and waits on the owner's yes; nothing has changed yet"}), false
		}
	}
	now := srv.now()
	id := opsID(now, c)
	if err := srv.log.Append(Event{V: EventVersion, Time: now, Kind: KindOps, Mode: ModeShared, Ops: &OpsPayload{ID: id, Step: OpsProposed, Change: &c}}); err != nil {
		return refuse(fmt.Errorf("agentd could not record the change: %v", err))
	}
	reply := map[string]any{"proposed": entryOps + id, "summary": c.Summary,
		"note": "the owner answers yes or no on entry " + entryOps + id + "; nothing has changed yet, and Go posts the outcome under the entry"}
	if c.Diff != "" {
		reply["diff"] = c.Diff
	}
	if c.Auto {
		reply["note"] = c.Supervisor + " is down, so Go restarts it without a yes, at its next pass, and posts the outcome under " + entryOps + id
	}
	return compact(reply), false
}

// sameChange says whether two proposals make the same change.
func sameChange(a, b OpsChange) bool {
	return a.Op == b.Op && a.Supervisor == b.Supervisor && a.Pointer == b.Pointer && string(a.Value) == string(b.Value) && a.Delete == b.Delete &&
		a.Base == b.Base && a.Commit == b.Commit && a.Kept == b.Kept && a.Session == b.Session
}
