package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The calendar connector's tools as the tests name them: the real names
// come from #101's scratch call.
var (
	testCalendarRead  = []string{"mcp__claude_ai_Google_Calendar__list_events", "mcp__claude_ai_Google_Calendar__get_event"}
	testCalendarWrite = []string{"mcp__claude_ai_Google_Calendar__create_event", "mcp__claude_ai_Google_Calendar__update_event", "mcp__claude_ai_Google_Calendar__delete_event"}
)

// withCalendarTools sets the connector's tools for one test.
func withCalendarTools(t *testing.T) {
	t.Helper()
	read, write := calendarReadTools, calendarWriteTools
	calendarReadTools, calendarWriteTools = testCalendarRead, testCalendarWrite
	t.Cleanup(func() { calendarReadTools, calendarWriteTools = read, write })
}

// calendarConfig is a config with the check on, a personal account and
// the jobs on a shared one, as a caller's own login would be.
func calendarConfig(t *testing.T) *Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Accounts = map[string]Account{
		"personal": {ConfigDir: "/home/owner/.claude"},
		"acme":     {ConfigDir: "/home/owner/.claude-acme", Shared: true},
	}
	cfg.Jobs.Account = "acme"
	cfg.Jobs.Model = "haiku"
	cfg.Jobs.Calendar = CalendarConfig{Enabled: true}
	cfg.ConfidentialPatterns = []string{`(?i)project falcon`}
	cfg.Repositories = map[string]Repository{
		"widgets": {Forge: "github", Project: "example-org/widgets"},
		"site":    {Forge: "github", Project: "example-org/site", Public: true},
	}
	return &cfg
}

const goodCalendar = `{"text":"Thursday 2pm is taken by a dentist appointment.","events":[` +
	`{"title":"Dentist","start":"2026-11-19T14:00:00-06:00","end":"2026-11-19T15:00:00-06:00","all_day":false,"location":"Main St","calendar":"Personal","free_busy":"busy"},` +
	`{"title":"Holiday","start":"2026-11-26","end":"2026-11-27","all_day":true,"location":"","calendar":"Holidays","free_busy":"free"}],"answered":true}`

var calendarNow = time.Date(2026, 11, 18, 10, 0, 0, 0, time.FixedZone("CST", -6*3600))

func calendarLog(t *testing.T) (*Log, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return NewLogWriter(&buf), &buf
}

func calendarEvents(t *testing.T, buf *bytes.Buffer) []*CalendarPayload {
	t.Helper()
	events, torn, err := ReadEvents(bytes.NewReader(buf.Bytes()))
	if err != nil || torn != 0 {
		t.Fatal(torn, err)
	}
	var out []*CalendarPayload
	for _, e := range events {
		if e.Kind == KindQuery {
			t.Fatalf("a calendar check logged a query event: %+v", e.Query)
		}
		if e.Calendar != nil {
			out = append(out, e.Calendar)
		}
	}
	return out
}

// argAfter is the value that follows flag in args, and whether flag is
// there.
func argAfter(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// A check runs under the personal account's login whatever the jobs'
// account, with Sonnet, without the calling session's key or marker, with
// no built-in tools, the connector's read tools its only allowed tools
// and its write tools denied by name; the directive goes on standard
// input as data, with now and the time zone.
func TestCalendarJobIsReadOnlyUnderThePersonalLogin(t *testing.T) {
	withCalendarTools(t)
	dir := fakeClaude(t)
	claudeAnswers(t, dir, 0, claudeResult(goodCalendar))
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/owner/.claude-acme")
	cfg := calendarConfig(t)
	log, buf := calendarLog(t)
	r, _, err := Calendar(context.Background(), cfg, log, calendarNow, "widgets-s1", "anything on Friday at noon", compiledPatterns(cfg))
	if err != nil || !r.Answered || len(r.Events) != 2 || r.Events[0].Title != "Dentist" || !r.Events[1].AllDay {
		t.Fatalf("reply = %+v, %v", r, err)
	}
	args := strings.Split(strings.TrimSuffix(readFake(t, dir, "args.1"), "\n"), "\n")
	if m, _ := argAfter(args, "--model"); m != "sonnet" {
		t.Errorf("model %q, want sonnet", m)
	}
	if tools, ok := argAfter(args, "--tools"); !ok || tools != "" {
		t.Errorf("--tools %q: a check has no built-in tools", tools)
	}
	if allow, _ := argAfter(args, "--allowedTools"); allow != strings.Join(testCalendarRead, ",") {
		t.Errorf("--allowedTools %q, want the read tools alone", allow)
	}
	deny, _ := argAfter(args, "--disallowedTools")
	for _, w := range testCalendarWrite {
		if !contains(strings.Split(deny, ","), w) {
			t.Errorf("--disallowedTools %q lacks %s", deny, w)
		}
	}
	allow, _ := argAfter(args, "--allowedTools")
	for _, a := range strings.Split(allow, ",") {
		if contains(testCalendarWrite, a) || !strings.HasPrefix(a, "mcp__") {
			t.Errorf("--allowedTools names %q, which is not a connector read tool", a)
		}
	}
	env := readFake(t, dir, "env.1")
	if !strings.Contains(env, "CLAUDE_CONFIG_DIR=/home/owner/.claude\n") || strings.Contains(env, ".claude-acme") {
		t.Errorf("the check did not run under the personal login:\n%s", env)
	}
	if strings.Contains(env, "ANTHROPIC_API_KEY") || strings.Contains(env, "CLAUDECODE=") {
		t.Errorf("the check inherited the session's environment:\n%s", env)
	}
	wd, _ := os.Getwd()
	if !strings.Contains(env, "PWD="+os.TempDir()) || !strings.Contains(env, "agentd-calendar-") || strings.Contains(env, "PWD="+wd+"\n") {
		t.Errorf("the check did not run in an empty directory of its own:\n%s", env)
	}
	var in map[string]string
	if err := json.Unmarshal([]byte(readFake(t, dir, "in.1")), &in); err != nil || in["prompt"] != "anything on Friday at noon" || in["now"] != "2026-11-18T10:00:00-06:00" || in["tz"] != "CST" {
		t.Errorf("input %v, %v", in, err)
	}
	if strings.Contains(strings.Join(args, "\n"), "Friday at noon") {
		t.Error("the directive was passed in the arguments, not as data")
	}
	if ev := calendarEvents(t, buf); len(ev) != 1 || ev[0].Caller != "widgets-s1" || ev[0].Account != "personal" || ev[0].Outcome != CalendarOK {
		t.Fatalf("events %+v", ev)
	}
}

// The model follows jobs.calendar.model, and the account
// jobs.calendar.account.
func TestCalendarFollowsItsConfig(t *testing.T) {
	withCalendarTools(t)
	dir := fakeClaude(t)
	claudeAnswers(t, dir, 0, claudeResult(goodCalendar))
	cfg := calendarConfig(t)
	cfg.Accounts["home"] = Account{ConfigDir: "/home/owner/.claude-home"}
	cfg.Jobs.Calendar = CalendarConfig{Enabled: true, Account: "home", Model: "claude-sonnet-5-5", Timeout: Duration{time.Minute}}
	log, buf := calendarLog(t)
	if _, _, err := Calendar(context.Background(), cfg, log, calendarNow, "cli", "what do I have tomorrow afternoon", compiledPatterns(cfg)); err != nil {
		t.Fatal(err)
	}
	args := strings.Split(readFake(t, dir, "args.1"), "\n")
	if m, _ := argAfter(args, "--model"); m != "claude-sonnet-5-5" {
		t.Errorf("model %q", m)
	}
	if env := readFake(t, dir, "env.1"); !strings.Contains(env, "CLAUDE_CONFIG_DIR=/home/owner/.claude-home\n") {
		t.Errorf("env:\n%s", env)
	}
	if ev := calendarEvents(t, buf); len(ev) != 1 || ev[0].Account != "home" {
		t.Fatalf("events %+v", ev)
	}
}

// No check runs, and claude is never called, while the check is off, its
// account is missing or shared, the connector's read tools are not
// known, or the directive is empty; each refusal is logged as one.
func TestCalendarRefusesWithoutCalling(t *testing.T) {
	dir := fakeClaude(t)
	claudeAnswers(t, dir, 0, claudeResult(goodCalendar))
	cases := map[string]func(cfg *Config){
		"off":     func(cfg *Config) { cfg.Jobs.Calendar.Enabled = false },
		"missing": func(cfg *Config) { cfg.Jobs.Calendar.Account = "nobody" },
		"shared":  func(cfg *Config) { cfg.Jobs.Calendar.Account = "acme" },
	}
	for name, change := range cases {
		withCalendarTools(t)
		cfg := calendarConfig(t)
		change(cfg)
		log, buf := calendarLog(t)
		if r, _, err := Calendar(context.Background(), cfg, log, calendarNow, "cli", "is Thursday 2pm free", compiledPatterns(cfg)); err == nil || r != nil {
			t.Errorf("%s: ran: %+v", name, r)
		}
		if ev := calendarEvents(t, buf); len(ev) != 1 || ev[0].Outcome != CalendarRefused {
			t.Errorf("%s: events %+v", name, ev)
		}
	}
	calendarReadTools = nil
	log, _ := calendarLog(t)
	if _, _, err := Calendar(context.Background(), calendarConfig(t), log, calendarNow, "cli", "is Thursday 2pm free", compiledPatterns(calendarConfig(t))); err == nil || !strings.Contains(err.Error(), "scratch call") {
		t.Errorf("ran with no read tools known: %v", err)
	}
	withCalendarTools(t)
	calendarWriteTools = nil
	if _, _, err := Calendar(context.Background(), calendarConfig(t), log, calendarNow, "cli", "is Thursday 2pm free", compiledPatterns(calendarConfig(t))); err == nil {
		t.Errorf("ran with no write tools known to deny")
	}
	withCalendarTools(t)
	if _, _, err := Calendar(context.Background(), calendarConfig(t), log, calendarNow, "cli", "  ", compiledPatterns(calendarConfig(t))); err == nil {
		t.Error("ran an empty directive")
	}
	if n := claudeCalls(t, dir); n != 0 {
		t.Fatalf("claude was called %d times", n)
	}
}

// Config validation refuses a calendar account that is not configured or
// is marked shared.
func TestCalendarConfigIsChecked(t *testing.T) {
	for _, tc := range []struct{ jobs, want string }{
		{`{"calendar": {"enabled": true}}`, `jobs.calendar.account "personal" is not in accounts`},
		{`{"calendar": {"enabled": true, "account": "acme"}}`, `marked shared`},
		{`{"calendar": {"account": "nobody"}}`, `jobs.calendar.account "nobody"`},
		{`{"calendar": {"timeout": "-1s"}}`, `jobs.calendar.timeout`},
	} {
		cfg := DefaultConfig()
		if err := json.Unmarshal([]byte(`{"accounts": {"acme": {"config_dir": "/x", "shared": true}}, "jobs": `+tc.jobs+`}`), &cfg); err != nil {
			t.Fatal(err)
		}
		if err := cfg.finish(t.TempDir()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("jobs %s: %v", tc.jobs, err)
		}
	}
	cfg := DefaultConfig()
	if err := json.Unmarshal([]byte(`{"accounts": {"personal": {"config_dir": "/x"}}, "jobs": {"calendar": {"enabled": true}}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.finish(t.TempDir()); err != nil && strings.Contains(err.Error(), "calendar") {
		t.Errorf("a good calendar config: %v", err)
	}
}

// A reply matching a confidential pattern, in its text or any event's
// field, is withheld whole; the log holds neither the directive nor
// anything the calendar said, in any outcome.
func TestCalendarWithholdsAndLogsNothingOfTheCalendar(t *testing.T) {
	withCalendarTools(t)
	dir := fakeClaude(t)
	secret := strings.Replace(goodCalendar, `"location":"Main St"`, `"location":"Project Falcon war room"`, 1)
	claudeAnswers(t, dir, 1, claudeResult(goodCalendar))
	claudeAnswers(t, dir, 2, claudeResult(secret))
	claudeAnswers(t, dir, 3, claudeResult(`{"text":"Dentist at 2","events":[{"title":"Dentist"}],"answered":true}`))
	claudeAnswers(t, dir, 4, `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Dentist on Thursday could not be read"}`)
	claudeAnswers(t, dir, 5, `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"Project Falcon review on Friday"}`)
	cfg := calendarConfig(t)
	log, buf := calendarLog(t)
	if r, _, err := Calendar(context.Background(), cfg, log, calendarNow, "s1", "is Thursday 2pm free", compiledPatterns(cfg)); err != nil || r.Withheld != 0 {
		t.Fatal(r, err)
	}
	r, _, err := Calendar(context.Background(), cfg, log, calendarNow, "s1", "is Thursday 2pm free", compiledPatterns(cfg))
	if err != nil || r.Withheld != 1 || len(r.Events) != 0 || strings.Contains(r.Text, "Dentist") || strings.Contains(r.Text, "Falcon") {
		t.Fatalf("withheld reply %+v, %v", r, err)
	}
	if _, _, err := Calendar(context.Background(), cfg, log, calendarNow, "s1", "is Thursday 2pm free", compiledPatterns(cfg)); err == nil {
		t.Fatal("a malformed reply was taken")
	}
	if _, _, err := Calendar(context.Background(), cfg, log, calendarNow, "s1", "is Thursday 2pm free", compiledPatterns(cfg)); err == nil {
		t.Fatal("a failed call was taken")
	}
	// A failed call's words are screened for the caller as a reply is.
	if _, _, err := Calendar(context.Background(), cfg, log, calendarNow, "s1", "is Thursday 2pm free", compiledPatterns(cfg)); err == nil || strings.Contains(err.Error(), "Falcon") || !strings.Contains(err.Error(), "withheld") {
		t.Fatalf("a failed call's error: %v", err)
	}
	ev := calendarEvents(t, buf)
	var outcomes []string
	for _, e := range ev {
		outcomes = append(outcomes, e.Outcome)
	}
	if strings.Join(outcomes, ",") != "ok,withheld,failed,failed,failed" {
		t.Fatalf("outcomes %v", outcomes)
	}
	for _, w := range []string{"Thursday", "Dentist", "Main St", "Falcon", "Holiday", "2pm"} {
		if strings.Contains(buf.String(), w) {
			t.Errorf("the log holds %q:\n%s", w, buf.String())
		}
	}
}

// A malformed reply is an error: a field the schema lacks, no text, an
// unreadable time, an end before its start, or an all-day event with
// times.
func TestCheckCalendarReplyRefuses(t *testing.T) {
	if _, err := checkCalendarReply(json.RawMessage(goodCalendar)); err != nil {
		t.Fatal(err)
	}
	ev := func(fields string) string {
		return `{"text":"t","events":[{"title":"x","location":"","calendar":"","free_busy":"busy",` + fields + `}],"answered":true}`
	}
	for _, raw := range []string{
		`{"text":"t","events":[],"answered":true,"extra":1}`,
		`{"text":" ","events":[],"answered":true}`,
		`{"text":"t","events":[],"answered":true,"withheld":2}`,
		ev(`"start":"Thursday","end":"2026-11-19T15:00:00Z","all_day":false`),
		ev(`"start":"2026-11-19T15:00:00Z","end":"2026-11-19T14:00:00Z","all_day":false`),
		ev(`"start":"2026-11-19T14:00:00Z","end":"2026-11-19T15:00:00Z","all_day":true`),
		strings.Replace(ev(`"start":"2026-11-19T14:00:00Z","end":"2026-11-19T15:00:00Z","all_day":false`), `"busy"`, `"maybe"`, 1),
	} {
		if r, err := checkCalendarReply(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: taken as %+v", raw, r)
		}
	}
	if r, err := checkCalendarReply(json.RawMessage(`{"text":"nothing then","events":null,"answered":true}`)); err != nil || r.Events == nil {
		t.Errorf("no events: %+v, %v", r, err)
	}
}

// calendar_check is offered, when the check is on, to a session with a
// repository, a console thread's agent and the ops agent, and to none
// while it is off; a public repository's session is refused it and
// claude is never called; the call logs the check and no query.
func TestCalendarTool(t *testing.T) {
	withCalendarTools(t)
	dir := fakeClaude(t)
	claudeAnswers(t, dir, 0, claudeResult(goodCalendar))
	cfg := calendarConfig(t)
	var buf bytes.Buffer
	read := func() ([]Event, error) { return nil, nil }
	server := func(cfg *Config, name string, sess Session) *MCPServer {
		return newServer(cfg, name, sess, "", NewLogWriter(&buf), read, func() time.Time { return calendarNow })
	}
	offered := map[string]*MCPServer{
		"session": server(cfg, "widgets-s1", Session{Repository: "widgets"}),
		"query":   server(cfg, "thread-1", Session{Repository: "widgets", Role: RoleQuery}),
		"ops":     server(cfg, "thread:i1", Session{Role: RoleOps}),
	}
	for name, srv := range offered {
		if !contains(srv.tools(), ToolCalendar) {
			t.Errorf("%s is not offered calendar_check: %v", name, srv.tools())
		}
	}
	if srv := server(cfg, "loose", Session{}); contains(srv.tools(), ToolCalendar) {
		t.Error("a session with no repository is offered calendar_check")
	}
	if srv := server(cfg, "site-s1", Session{Repository: "site"}); contains(srv.tools(), ToolCalendar) {
		t.Error("a public repository's session is offered calendar_check")
	}
	off := calendarConfig(t)
	off.Jobs.Calendar.Enabled = false
	if srv := server(off, "widgets-s1", Session{Repository: "widgets"}); contains(srv.tools(), ToolCalendar) {
		t.Error("calendar_check is offered while the check is off")
	}
	rs := session(t, offered["session"], `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	listed := false
	for _, tool := range rs[0].Result.Tools {
		if tool.Name == ToolCalendar {
			listed = strings.Contains(tool.Description, "never put it")
		}
	}
	if !listed {
		t.Error("tools/list does not give calendar_check with its warning")
	}

	text, refused := server(cfg, "site-s1", Session{Repository: "site"}).calendarCall(json.RawMessage(`{"prompt":"is Thursday 2pm free"}`))
	if !refused || !strings.Contains(text, "public repository") {
		t.Fatalf("a public repository's session: %s", text)
	}
	if n := claudeCalls(t, dir); n != 0 {
		t.Fatalf("claude was called %d times for a public repository", n)
	}
	text, refused = callOne(t, offered["session"], ToolCalendar, map[string]any{"prompt": "is Thursday 2pm free"})
	var r CalendarReply
	if refused || json.Unmarshal([]byte(text), &r) != nil || len(r.Events) != 2 {
		t.Fatalf("check: %s", text)
	}
	if _, refused := callOne(t, offered["session"], ToolCalendar, map[string]any{"prompt": "x", "calendar": "work"}); !refused {
		t.Error("an argument the tool does not take was accepted")
	}
	ev := calendarEvents(t, &buf)
	if len(ev) != 2 || ev[0].Caller != "site-s1" || ev[0].Outcome != CalendarRefused || ev[1].Caller != "widgets-s1" || ev[1].Outcome != CalendarOK {
		t.Fatalf("events %+v", ev)
	}
	if strings.Contains(buf.String(), "Thursday") || strings.Contains(buf.String(), "Dentist") {
		t.Fatalf("the log holds the check:\n%s", buf.String())
	}
}

// The headless agents may call calendar_check only while it is on.
func TestCalendarAllowedForAgents(t *testing.T) {
	cfg := calendarConfig(t)
	if withCalendar(cfg, "site", "x") != "x" {
		t.Error("a public repository's thread agent is allowed calendar_check")
	}
	if !strings.Contains(opsAllowed(cfg, "/clone"), "mcp__agentd__"+ToolCalendar) || !strings.Contains(withCalendar(cfg, "widgets", "x"), "mcp__agentd__"+ToolCalendar) {
		t.Error("an agent is not allowed calendar_check while it is on")
	}
	cfg.Jobs.Calendar.Enabled = false
	if strings.Contains(opsAllowed(cfg, "/clone"), ToolCalendar) || withCalendar(cfg, "widgets", "x") != "x" {
		t.Error("an agent is allowed calendar_check while it is off")
	}
}

// agentd calendar prints the text and an event list, or with --json the
// reply, and logs the check.
func TestCalendarCommand(t *testing.T) {
	withCalendarTools(t)
	fd := fakeClaude(t)
	claudeAnswers(t, fd, 0, claudeResult(goodCalendar))
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	cfgPath := filepath.Join(dir, "agentd.json")
	if err := os.WriteFile(cfgPath, []byte(`{"event_log": "`+logPath+`", "accounts": {"personal": {"config_dir": "`+dir+`"}}, "jobs": {"calendar": {"enabled": true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := calendarCmd([]string{"--config", cfgPath, "is", "Thursday", "2pm", "free"}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.HasPrefix(s, "Thursday 2pm is taken") || !strings.Contains(s, "- 2026-11-19T14:00:00-06:00 to 2026-11-19T15:00:00-06:00  Dentist @ Main St\n") || !strings.Contains(s, "2026-11-26 to 2026-11-27 (all day)  Holiday [free]") {
		t.Fatalf("output:\n%s", s)
	}
	out.Reset()
	if err := calendarCmd([]string{"--config", cfgPath, "--json", "is Thursday 2pm free"}, &out); err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || r["answered"] != true || len(r["events"].([]any)) != 2 || r["text"] == nil {
		t.Fatalf("--json: %s, %v", out.String(), err)
	}
	if err := calendarCmd([]string{"--config", cfgPath}, &out); err == nil {
		t.Error("ran with no directive")
	}
	events, _, err := ReadLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Calendar != nil {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d calendar events logged", n)
	}
}

// Existing jobs keep their arguments: a job with no MCP, Allow or
// NoStrictMCP set runs with the strict, empty MCP config and no
// --allowedTools (TestJobRunIsStripped pins the whole list); a job that
// sets them gets them.
func TestJobMCPAndAllow(t *testing.T) {
	dir := fakeClaude(t)
	claudeAnswers(t, dir, 0, claudeResult(`{}`))
	j := testJob()
	if _, _, err := j.Run(context.Background(), "s", []byte(`{}`), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	args := readFake(t, dir, "args.1")
	if !strings.Contains(args, "--strict-mcp-config\n--mcp-config\n"+emptyMCP+"\n") || strings.Contains(args, "--allowedTools") {
		t.Fatalf("args:\n%s", args)
	}
	j.MCP, j.NoStrictMCP, j.Allow = `{"mcpServers":{"x":{}}}`, true, []string{"mcp__x__read"}
	if _, _, err := j.Run(context.Background(), "s", []byte(`{}`), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	args = readFake(t, dir, "args.2")
	if strings.Contains(args, "--strict-mcp-config") || !strings.Contains(args, "--mcp-config\n"+`{"mcpServers":{"x":{}}}`+"\n") || !strings.Contains(args, "--allowedTools\nmcp__x__read\n") {
		t.Fatalf("args:\n%s", args)
	}
}
