package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseStatus(t *testing.T) {
	cases := []struct {
		text      string
		done      bool
		questions bool
		head      string
	}{
		{"# Title\nbody\nDONE\n", true, false, "# Title"},
		{"# Title\nbody\nDONE\n\n", true, false, "# Title"},
		{"# Title\nDONE\nmore\n", false, false, "# Title"},
		{"", false, false, ""},
		{"# T\n## Questions\nnone\n## Next\nx\n", false, false, "# T"},
		{"# T\n## Questions\n- none\n", false, false, "# T"},
		{"# T\n## Questions\nNone yet.\n", false, false, "# T"},
		{"# T\n## Questions\n(none yet)\n", false, false, "# T"},
		{"# T\n## Questions\n* N/A\n", false, false, "# T"},
		{"# T\n## Questions\nNone of the tests pass.\n", false, true, "# T"},
		{"# T\n## Questions\n1. Which version?\n## Next\n", false, true, "# T"},
		{"# T\n### questions\nWhat now?\nDONE\n", true, true, "# T"},
		{"# T\n## Questions\n\n---\n\n# Next issue\n", false, false, "# T"},
	}
	for _, c := range cases {
		ev := ParseStatus(c.text)
		if ev.Done != c.done || (ev.Questions != "") != c.questions || ev.Head != c.head {
			t.Errorf("%q: done %v questions %q head %q", c.text, ev.Done, ev.Questions, ev.Head)
		}
	}
}

// A session that works through several issues keeps a Questions section per
// issue; all of them are the session's questions, and a rule or a new section
// added after one must not change what the questions are.
func TestParseStatusJoinsEveryQuestionsSection(t *testing.T) {
	before := "# A\n## Questions\n- first?\n"
	after := before + "\n---\n\n# B\nwork\n## Questions\n- second?\n"
	if got := ParseStatus(before).Questions; got != "- first?" {
		t.Fatalf("before: %q", got)
	}
	if got := ParseStatus(after).Questions; got != "- first?\n- second?" {
		t.Fatalf("after: %q", got)
	}
	ruled := before + "\n***\n\n# B\nwork\n"
	if got := ParseStatus(ruled).Questions; got != "- first?" {
		t.Fatalf("a rule after the section changed the questions: %q", got)
	}
}

// Questions are judged per section: two sections that each say "none" ask
// nothing, a subheading under Questions is part of the question, and a
// "none" section beside a real one leaves only the real one.
func TestParseStatusJudgesEachQuestionsSection(t *testing.T) {
	cases := map[string]string{
		"# A\n## Questions\nnone\n\n# B\n## Questions\nNone yet.\n":                "",
		"# A\n## Questions\n### Which config format?\nJSON or YAML?\n## Next\nx\n": "### Which config format?\nJSON or YAML?",
		"# A\n## Questions\n- none\n# B\n## Questions\n- Which port?\n":            "- Which port?",
	}
	for text, want := range cases {
		if got := ParseStatus(text).Questions; got != want {
			t.Errorf("%q: questions %q, want %q", text, got, want)
		}
	}
}

// Lines inside a fenced code block are content, never headings: a shell
// comment in a question's snippet does not end the section, and a
// Questions heading inside a fence does not start one (issue #2).
func TestParseStatusIgnoresHeadingsInFences(t *testing.T) {
	cases := map[string]string{
		"# A\n## Questions\n```sh\n# build first\nmake\n```\nIs that right?\n## Next\nx\n": "```sh\n# build first\nmake\n```\nIs that right?",
		"# A\n## Questions\n~~~\n# only a comment\n~~~\n":                                  "~~~\n# only a comment\n~~~",
		"# A\n```\n## Questions\nnot a question\n```\n":                                    "",
		"# A\n## Questions\n````\n```\n# still code\n````\nAfter?\n# B\n":                  "````\n```\n# still code\n````\nAfter?",
	}
	for text, want := range cases {
		if got := ParseStatus(text).Questions; got != want {
			t.Errorf("%q: questions %q, want %q", text, got, want)
		}
	}
}

// A fence left open to the end of the file is a typo, not a code block that
// swallows every later heading: the text is read again without fences.
func TestParseStatusUnclosedFence(t *testing.T) {
	text := "# A\n## Questions\n```\nWhich port?\n## Next\nnot a question\n"
	if got := ParseStatus(text).Questions; got != "```\nWhich port?" {
		t.Fatalf("questions %q", got)
	}
	if got := ParseStatus("# A\n```\n## Questions\nWhich port?\n").Questions; got != "Which port?" {
		t.Fatalf("questions after an unclosed fence %q", got)
	}
}

func TestTranscriptDirAndLastTurn(t *testing.T) {
	if got := TranscriptDir("/root", "/home/me/repos/github.com/org/proj.wt"); got != "/root/-home-me-repos-github-com-org-proj-wt" {
		t.Fatalf("dir = %q", got)
	}
	dir := t.TempDir()
	if ev := ReadTranscript(dir); ev.Found {
		t.Fatal("found a transcript in an empty directory")
	}
	big := strings.Repeat("x", 300*1024)
	writeFile(t, dir+"/a.jsonl", `{"type":"user","timestamp":"2026-10-22T01:00:00Z","message":{"content":"`+big+`"}}`+"\n"+
		`{"type":"assistant","timestamp":"2026-10-22T02:00:00Z","message":{"stop_reason":"end_turn"}}`+"\n"+
		`{"type":"atis-latch"}`+"\n")
	ev := ReadTranscript(dir)
	if !ev.Found || ev.Role != "assistant" || ev.StopReason != "end_turn" || !ev.At.Equal(time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("last turn = %+v", ev)
	}
}

func TestLogRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "events.jsonl")
	l, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 22, 3, 4, 5, 0, time.UTC)
	two := 2
	if err := l.Append(Event{V: EventVersion, Time: at, Session: "s", Kind: KindObserve, Mode: ModeLive, Observe: &ObservePayload{State: "idle", Restarts: &two}}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"time":"2026-10-22T03:04:06Z","kind":"obs`) // torn last line
	f.Close()
	events, torn, err := ReadLog(path)
	if err != nil || torn == 0 || len(events) != 1 {
		t.Fatalf("events %d torn %d err %v", len(events), torn, err)
	}
	e := events[0]
	if !e.Time.Equal(at) || e.Session != "s" || e.Observe.State != "idle" || e.Observe.Restarts == nil || *e.Observe.Restarts != 2 {
		t.Fatalf("event = %+v", e)
	}
	if events, _, err := ReadLog(filepath.Join(t.TempDir(), "missing")); err != nil || events != nil {
		t.Fatalf("missing log: %v %v", events, err)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, d := range []string{"work-17", "repos/widgets-wt/17", "work-42", "repos/gadgets"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	token := filepath.Join(home, ".config", "agentd-kit", "spare.token")
	writeFile(t, token, "a token\n")
	if err := os.Chmod(token, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig("agentd.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Policies.Nudge.Message == "" || cfg.Policies.Restart.MaxRestarts != 3 || len(cfg.Sessions) != 2 {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.Console("w17") != "widgets" || cfg.Console("g42") != "gadgets" {
		t.Fatalf("consoles: %s %s", cfg.Console("w17"), cfg.Console("g42"))
	}
	if !filepath.IsAbs(cfg.Sessions["w17"].Deliver[0].MRFile) || !filepath.IsAbs(cfg.EventLog) {
		t.Fatalf("paths not expanded: %+v", cfg.Sessions["w17"])
	}
	if cfg.Policies.Escalate.Command[0] == "~/bin/post-to-console" {
		t.Fatal("escalate command not expanded")
	}
}

// The default restart command ends with Claude itself, so the tmux session
// ends when Claude exits and the restart policy sees it. A trailing shell
// would keep a crashed session alive and take the nudges meant for Claude.
func TestDefaultRestartCommandEndsWithClaude(t *testing.T) {
	cmd := DefaultConfig().Policies.Restart.Command
	if cmd[2] != `exec claude --session-id "$1" "$2"` {
		t.Fatalf("restart command does not end with the claude invocation: %q", cmd)
	}
	for _, part := range cmd {
		if strings.Contains(part, "exec bash") {
			t.Fatalf("restart command keeps a shell after claude: %q", part)
		}
	}
}

// The compiled default never bypasses Claude Code's permission prompts; an
// owner who wants that writes the flag into their own restart command, and
// it is kept.
func TestDefaultRestartCommandKeepsPermissions(t *testing.T) {
	for _, part := range DefaultConfig().Policies.Restart.Command {
		if strings.Contains(part, "dangerously") {
			t.Fatalf("default restart command bypasses permissions: %q", part)
		}
	}
	if cmd := strings.Join(DefaultConfig().Policies.Restart.Command, " "); !strings.Contains(cmd, `claude --session-id "$1" "$2"`) || !strings.Contains(cmd, "{prompt}") || strings.Contains(cmd, "--continue") {
		t.Fatalf("default restart command is not a fresh claude with a prompt: %q", cmd)
	}
	path := filepath.Join(t.TempDir(), "c.json")
	writeFile(t, path, `{"policies": {"restart": {"command": ["bash", "-lic", "claude --dangerously-skip-permissions --continue"]}}}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Policies.Restart.Command[2]; got != "claude --dangerously-skip-permissions --continue" {
		t.Fatalf("configured command = %q", got)
	}
}

// Relative paths in a config file name places beside it, wherever agentd
// runs from; ~ is still the home directory, and a bare command name is left
// for PATH to find.
func TestConfigPathsAreRelativeToTheConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf", "agentd.json")
	writeFile(t, filepath.Join(dir, "conf", "work", "mr.md"), "")
	writeFile(t, filepath.Join(dir, "repo", "README"), "")
	writeFile(t, path, `{
		"event_log": "state/events.jsonl",
		"transcript_dir": "~/transcripts",
		"policies": {"escalate": {"command": ["bin/post", "{console}", "{message}"]}},
		"repositories": {"r": {"forge": "gitlab", "project": "a/b"}},
		"sessions": {"s": {"workdir": "work", "cwd": "../repo", "deliver": [{"repository": "r", "worktree": "../repo", "target": "main", "mr_file": "work/mr.md"}]}}
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	conf := filepath.Join(dir, "conf")
	s := cfg.Sessions["s"]
	got := map[string]string{
		"event_log": cfg.EventLog, "transcript_dir": cfg.TranscriptDir, "escalate": cfg.Policies.Escalate.Command[0],
		"workdir": s.Workdir, "cwd": s.Cwd, "worktree": s.Deliver[0].Worktree, "mr_file": s.Deliver[0].MRFile,
	}
	want := map[string]string{
		"event_log": filepath.Join(conf, "state", "events.jsonl"), "transcript_dir": filepath.Join(home, "transcripts"),
		"escalate": filepath.Join(conf, "bin", "post"), "workdir": filepath.Join(conf, "work"), "cwd": filepath.Join(dir, "repo"),
		"worktree": filepath.Join(dir, "repo"), "mr_file": filepath.Join(conf, "work", "mr.md"),
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
	writeFile(t, path, `{"policies": {"escalate": {"command": ["post-to-console", "{message}"]}}}`)
	if cfg, err := LoadConfig(path); err != nil || cfg.Policies.Escalate.Command[0] != "post-to-console" {
		t.Fatalf("bare command name: %v %v", cfg, err)
	}
}

// A session whose workdir or cwd does not exist is refused at load, naming
// the session and the path, rather than failing at its first restart; and
// ~user, which agentd does not expand, is refused rather than read as a
// directory beside the config.
func TestConfigRefusesMissingDirsAndTildeUser(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "work", "STATUS.md"), "")
	path := filepath.Join(dir, "agentd.json")
	writeFile(t, path, `{"sessions": {"s1": {"workdir": "work", "cwd": "no-such-repo"}}}`)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "s1") || !strings.Contains(err.Error(), filepath.Join(dir, "no-such-repo")) {
		t.Fatalf("missing cwd: %v", err)
	}
	writeFile(t, path, `{"event_log": "~bob/events.jsonl"}`)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "~bob") {
		t.Fatalf("~user path: %v", err)
	}
}

func TestConfigRefusesMistakes(t *testing.T) {
	cases := map[string]string{
		"unknown field":         `{"sessions": {"s": {"workdir": "w", "cwd": "c", "typo": 1}}}`,
		"merge in allow":        `{"repositories": {"r": {"forge": "gitlab", "project": "a/b", "allow": ["merge"]}}}`,
		"unknown repository":    `{"sessions": {"s": {"workdir": "w", "cwd": "c", "repository": "nope"}}}`,
		"pattern name":          `{"sessions": {"w1*": {"workdir": "w", "cwd": "c"}}}`,
		"bad duration":          `{"poll_interval": "soon"}`,
		"deliver without mr":    `{"repositories": {"r": {"forge": "gitlab", "project": "a/b"}}, "sessions": {"s": {"workdir": "w", "cwd": "c", "deliver": [{"repository": "r", "worktree": "x", "target": "main"}]}}}`,
		"backoff max too low":   `{"policies": {"restart": {"backoff_initial": "10m", "backoff_max": "1m"}}}`,
		"bad confidential":      `{"confidential_patterns": ["(unclosed"]}`,
		"sessions, no escalate": `{"sessions": {"s": {"workdir": ".", "cwd": "."}}}`,
	}
	for name, text := range cases {
		path := filepath.Join(t.TempDir(), "c.json")
		writeFile(t, path, text)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStatusTable(t *testing.T) {
	h := newHarness(t, false, "mon", "idle", "blocked")
	h.status("mon", working)
	h.tmux.crash["mon"] = true
	h.status("idle", working)
	h.alive("idle", "static")
	h.status("blocked", blocked)
	h.alive("blocked", "static")
	h.sup.Run(contextWithCancel(t), true)
	h.ticks(3*12, 5*time.Minute)

	rows, meta := Summarize(h.cfg, h.events(), ModeLive)
	byName := map[string]SessionStatus{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if byName["mon"].State != CrashLooping || byName["mon"].Restarts != 2 || byName["mon"].Escalations != 1 {
		t.Fatalf("mon row = %+v", byName["mon"])
	}
	if byName["idle"].State != Idle || byName["idle"].Nudges != 3 {
		t.Fatalf("idle row = %+v", byName["idle"])
	}
	if byName["blocked"].State != BlockedOnOwner || byName["blocked"].Nudges != 0 {
		t.Fatalf("blocked row = %+v", byName["blocked"])
	}
	var out bytes.Buffer
	PrintStatus(&out, rows, meta, h.clock.now)
	text := out.String()
	for _, want := range []string{"agentd live", "SESSION", "crash-looping", "blocked-on-owner", "STATUS.md has open Questions"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status table lacks %q:\n%s", want, text)
		}
	}
}

func TestBackoff(t *testing.T) {
	p := RestartPolicy{BackoffInitial: Duration{time.Minute}, BackoffMax: Duration{5 * time.Minute}}
	want := []time.Duration{0, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for n, w := range want {
		if got := backoff(n, p); got != w {
			t.Errorf("backoff(%d) = %s, want %s", n, got, w)
		}
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"bash", "-lic", `claude --continue "Go on; it's fine"; exec bash`})
	want := `bash -lic 'claude --continue "Go on; it'\''s fine"; exec bash'`
	if got != want {
		t.Fatalf("shellJoin = %s", got)
	}
}
