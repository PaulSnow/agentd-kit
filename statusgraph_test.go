package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// busyTranscript is a recorded busy five minutes of a work session's
// transcript, in its shape: every entry with Claude Code's bookkeeping
// (ids, the session, the working directory, the version, the branch, the
// usage), each tool result twice, as the message and as its
// toolUseResult copy, and the progress and file-history lines between. It runs n tools from start, one every few seconds:
// long file reads, test runs with long logs, edits, and every seventh a
// failed command. It returns the lines, the tool calls' commands or files,
// and the session's own texts.
func busyTranscript(start time.Time, n int, prefix string) (string, []string, []string) {
	var b strings.Builder
	var targets, says []string
	at := start
	line := func(m map[string]any) {
		at = at.Add(time.Second)
		m["uuid"] = fmt.Sprintf("%s-%08d-aaaa-bbbb-cccc-%012d", prefix, len(b.String())%99999999, at.UnixNano()%1e12)
		m["parentUuid"] = "00000000-1111-2222-3333-444444444444"
		m["sessionId"] = "55555555-6666-7777-8888-999999999999"
		m["cwd"], m["version"], m["gitBranch"], m["userType"] = "/work/widgets/wt", "2.1.0", "night/12-widgets", "external"
		m["isSidechain"], m["timestamp"] = false, at.Format(time.RFC3339Nano)
		j, _ := json.Marshal(m)
		b.Write(j)
		b.WriteByte('\n')
	}
	prompt := prefix + ": carry on with the widgets brief, the next step in STATUS.md"
	line(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}})
	line(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": "<system-reminder>context left: 40%</system-reminder>"}})
	usage := map[string]any{"input_tokens": 3, "cache_creation_input_tokens": 1200, "cache_read_input_tokens": 98000, "output_tokens": 450,
		"service_tier": "standard", "server_tool_use": map[string]int{"web_search_requests": 0}}
	for i := 0; i < n; i++ {
		say := fmt.Sprintf("%s step %d: the widget parser rejects empty frames, so I check the decoder next.", prefix, i)
		says = append(says, say)
		id := fmt.Sprintf("toolu_%s_%03d", prefix, i)
		var name, out string
		var input map[string]any
		failed := false
		switch i % 7 {
		case 0, 3:
			name = "Read"
			input = map[string]any{"file_path": fmt.Sprintf("/work/widgets/wt/parse%d.go", i)}
			out = strings.Repeat("\tfor i := range frames { if len(frames[i]) == 0 { return errEmpty } }\n", 400) // ~28 KB
		case 1, 4:
			name = "Bash"
			input = map[string]any{"command": fmt.Sprintf("go test ./widgets/... -run TestParse%d", i), "description": "run the parser tests"}
			out = strings.Repeat("=== RUN   TestParseFrames/empty_frame_is_rejected\n--- PASS: TestParseFrames/empty_frame_is_rejected (0.00s)\n", 120) + "ok  \twidgets\t0.412s"
		case 2:
			name = "Edit"
			input = map[string]any{"file_path": fmt.Sprintf("/work/widgets/wt/decode%d.go", i),
				"old_string": strings.Repeat("old line of the decoder\n", 80), "new_string": strings.Repeat("new line of the decoder\n", 90)}
			out = "The file has been updated."
		case 5:
			name = "Grep"
			input = map[string]any{"pattern": "errEmpty", "path": "/work/widgets/wt"}
			out = strings.Repeat("/work/widgets/wt/parse.go:12: errEmpty\n", 60)
		case 6:
			name, failed = "Bash", true
			input = map[string]any{"command": fmt.Sprintf("./bin/check # %d", i)}
			out = "Exit code 1\n" + strings.Repeat("--- FAIL: TestDecodeFrames (0.01s)\n    decode_test.go:41: got 3 frames, want 4\n", 15)
		}
		targets = append(targets, input[map[string]string{"Read": "file_path", "Bash": "command", "Edit": "file_path", "Grep": "pattern"}[name]].(string))
		line(map[string]any{"type": "assistant", "requestId": "req_011CUa" + id, "message": map[string]any{
			"id": "msg_01" + id, "type": "message", "role": "assistant", "model": "claude-opus-5-5", "stop_reason": "tool_use", "usage": usage,
			"content": []any{map[string]any{"type": "text", "text": say}, map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}})
		line(map[string]any{"type": "user", "toolUseResult": map[string]any{"stdout": out, "stderr": "", "interrupted": false, "file": map[string]any{"content": out}},
			"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "is_error": failed, "content": out}}}})
		line(map[string]any{"type": "progress", "toolUseID": id, "data": map[string]any{"type": "hook_progress", "hookEvent": "PostToolUse", "command": "true"}})
		if name == "Edit" {
			line(map[string]any{"type": "file-history-snapshot", "messageId": "msg_01" + id, "snapshot": map[string]any{"trackedFileBackups": map[string]any{"decode.go": map[string]any{"version": i}}}})
		}
	}
	end := prefix + ": the parser now rejects empty frames; the decoder test still fails on frame counts."
	says = append(says, end)
	line(map[string]any{"type": "assistant", "message": map[string]any{"id": "msg_end", "role": "assistant", "stop_reason": "end_turn", "usage": usage,
		"content": []any{map[string]any{"type": "text", "text": end}}}})
	return b.String(), targets, says
}

// The filtered input of a busy five minutes keeps every tool call, every
// prompt and all of the session's text, trims the outputs, keeps the
// start and end of each failure, drops the bookkeeping and stays under
// 12k tokens (#57), at the 2 bytes a token measured on real sessions'
// transcripts. This window runs as many tools as the busiest recorded
// one, with more failures, so the input is fitted: the outputs and inputs
// are cut harder, and nothing is dropped.
func TestStatusWindowOfABusyFiveMinutes(t *testing.T) {
	start := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	text, targets, says := busyTranscript(start, 45, "w")
	path := filepath.Join(t.TempDir(), "t.jsonl")
	appendFile(t, path, text)
	win, err := readStatusWindow(path, 0, start)
	if err != nil {
		t.Fatal(err)
	}
	if win.Offset != int64(len(text)) || win.Cut {
		t.Fatalf("offset %d of %d, cut %v", win.Offset, len(text), win.Cut)
	}
	in := statusInput{Session: "w", Minutes: 5, Entries: win.Entries}
	whole, _ := json.Marshal(in)
	b, err := in.fit(DefaultStatusMaxInput)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	t.Logf("before fitting: %d bytes", len(whole))
	t.Logf("transcript %d bytes, input %d bytes (~%dk tokens)", len(text), len(b), len(b)/2000)
	if len(b)*4/7 >= 12000 || in.Dropped != 0 || len(text) < 50*len(b) {
		t.Fatalf("input %d bytes, %d dropped, from %d", len(b), in.Dropped, len(text))
	}
	tools := 0
	for _, e := range in.Entries {
		if e.T == "tool" {
			tools++
			if e.Tool == "" || len(e.Input) == 0 || e.Out == "" {
				t.Fatalf("tool entry %+v", e)
			}
		}
	}
	if tools != 45 {
		t.Fatalf("%d tool entries, want 45", tools)
	}
	for _, want := range append(append(targets, says...), "w: carry on with the widgets brief") {
		if !strings.Contains(got, jsonText(want)) {
			t.Fatalf("input lacks %q", want)
		}
	}
	for _, gone := range []string{"55555555-6666", "/work/widgets/wt\",\"version", "cache_read_input_tokens", "toolUseResult", "msg_01", "system-reminder", "timestamp"} {
		if strings.Contains(got, gone) {
			t.Fatalf("input keeps bookkeeping %q", gone)
		}
	}
	// A long read is trimmed to its ends; each failure keeps its exit code
	// and its first and last lines.
	if !strings.Contains(got, "[… 398 lines …]") || strings.Count(got, "Exit code 1") != 6 || strings.Count(got, "got 3 frames, want 4") < 6*4 {
		t.Fatalf("outputs not trimmed as they should be: %d failure lines", strings.Count(got, "got 3 frames, want 4"))
	}
	// A quieter window is sent as read: outputs to three lines at each end,
	// a failure whole.
	quiet, _, _ := busyTranscript(start, 12, "q")
	qpath := filepath.Join(t.TempDir(), "q.jsonl")
	appendFile(t, qpath, quiet)
	qwin, _ := readStatusWindow(qpath, 0, start)
	q := statusInput{Session: "q", Entries: qwin.Entries}
	qb, _ := q.fit(DefaultStatusMaxInput)
	if !strings.Contains(string(qb), "[… 394 lines …]") || strings.Count(string(qb), "got 3 frames, want 4") != 15 {
		t.Fatalf("quiet window: %s", qb)
	}
	// The same transcript read again from its end holds nothing new.
	again, _ := readStatusWindow(path, win.Offset, time.Time{})
	if len(again.Entries) != 0 || again.Offset != win.Offset {
		t.Fatalf("read again: %+v", again)
	}
}

// jsonText is s as it appears inside a JSON string.
func jsonText(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}

// A window starts where the last ended, leaves a line not yet ended for
// the next, takes a result to its call when both are in it and alone when
// its call was before it, and reads a transcript new to it by time.
func TestStatusWindowOffsets(t *testing.T) {
	start := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	text, _, _ := busyTranscript(start, 3, "a")
	path := filepath.Join(t.TempDir(), "t.jsonl")
	appendFile(t, path, text)
	first, _ := readStatusWindow(path, 0, start.Add(-time.Minute))
	more, _, _ := busyTranscript(start.Add(time.Hour), 2, "b")
	lines := strings.SplitAfter(more, "\n")
	// The call of b's first tool ends the next window; its result is
	// half written.
	appendFile(t, path, strings.Join(lines[:3], "")+lines[3][:20])
	second, _ := readStatusWindow(path, first.Offset, time.Time{})
	if len(second.Entries) != 3 || second.Entries[0].T != "prompt" || second.Entries[2].Out != "" || second.Offset != int64(len(text)+len(strings.Join(lines[:3], ""))) {
		t.Fatalf("second window: %+v at %d", second.Entries, second.Offset)
	}
	appendFile(t, path, lines[3][20:]+strings.Join(lines[4:], ""))
	third, _ := readStatusWindow(path, second.Offset, time.Time{})
	if len(third.Entries) == 0 || third.Entries[0].T != "tool" || third.Entries[0].Tool != "" || third.Entries[0].Out == "" {
		t.Fatalf("third window starts %+v", third.Entries)
	}
	// Read by time, only what is after it.
	late, _ := readStatusWindow(path, 0, start.Add(30*time.Minute))
	if len(late.Entries) == 0 || strings.HasPrefix(late.Entries[0].Text, "a:") {
		t.Fatalf("by time: %+v", late.Entries)
	}
	for _, e := range late.Entries {
		if strings.HasPrefix(e.Text, "a") {
			t.Fatalf("by time kept %q", e.Text)
		}
	}
}

// Over its limit the input loses the middle of its outputs first, then
// its earliest entries, and says how many.
func TestStatusInputFits(t *testing.T) {
	start := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
	text, _, says := busyTranscript(start, 200, "w")
	path := filepath.Join(t.TempDir(), "t.jsonl")
	appendFile(t, path, text)
	win, _ := readStatusWindow(path, 0, start)
	in := statusInput{Session: "w", Entries: win.Entries}
	b, err := in.fit(20 << 10)
	if err != nil || len(b) > 20<<10 || in.Dropped == 0 {
		t.Fatalf("%d bytes, %d dropped, %v", len(b), in.Dropped, err)
	}
	if !strings.Contains(string(b), jsonText(says[len(says)-1])) || strings.Contains(string(b), jsonText(says[0])) {
		t.Fatal("the latest entries are not the ones kept")
	}
}

func TestStatusLines(t *testing.T) {
	added, removed := statusLines("# s\n\n| A | coding |\n| B | waiting |\n", "# s\n| B | waiting |\n| A | merged |\n| C | coding |\n")
	if strings.Join(added, ";") != "| A | merged |;| C | coding |" || strings.Join(removed, ";") != "| A | coding |" {
		t.Fatalf("added %q removed %q", added, removed)
	}
}

const goodStatus = `{"session":"s","phase":"code","progress":0.6,"blocked":false,"blocker":"","risk":"low","summary":"Fixed the parser.","next":"Run the decoder tests."}`

func TestCheckStatusRefuses(t *testing.T) {
	if r, err := checkStatus(json.RawMessage(goodStatus), "s"); err != nil || r.Phase != "code" || r.Progress != 0.6 {
		t.Fatalf("good: %+v %v", r, err)
	}
	for _, bad := range []string{
		`{"session":"s","phase":"code"`,
		strings.Replace(goodStatus, `"s"`, `"other"`, 1),
		strings.Replace(goodStatus, `"code"`, `"coding"`, 1),
		strings.Replace(goodStatus, `"low"`, `"none"`, 1),
		strings.Replace(goodStatus, `0.6`, `1.5`, 1),
		strings.Replace(goodStatus, `0.6`, `-0.1`, 1),
		strings.Replace(goodStatus, `"blocked":false`, `"blocked":true`, 1),
		strings.Replace(goodStatus, `"blocker":""`, `"blocker":"waits"`, 1),
		strings.Replace(goodStatus, `Fixed the parser.`, `Fixed\nthe parser.`, 1),
		strings.Replace(goodStatus, `Fixed the parser.`, strings.Repeat("x", 201), 1),
		strings.Replace(goodStatus, `"next":"Run the decoder tests."`, `"next":""`, 1),
		strings.Replace(goodStatus, `}`, `,"extra":1}`, 1),
		goodStatus + goodStatus,
	} {
		if _, err := checkStatus(json.RawMessage(bad), "s"); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

// statusHarness is a live harness whose session s runs under a
// transcript, with status checks on and a fake claude.
func statusHarness(t *testing.T) (*harness, string, string) {
	t.Helper()
	dir := fakeClaude(t)
	h, id := contextHarness(t, false)
	h.cfg.Jobs.Status.Enabled = true
	return h, id, dir
}

func (h *harness) statusEvents() []*StatusPayload {
	var out []*StatusPayload
	for _, e := range h.events() {
		if e.Kind == KindStatus {
			out = append(out, e.Status)
		}
	}
	return out
}

// An active session is checked every five minutes, with the window since
// the last check; a window with nothing new is not sent; a malformed
// answer is a failure and a gap; a restart starts where the log says; a
// finished session is not checked.
func TestStatusInTheLoop(t *testing.T) {
	h, id, dir := statusHarness(t)
	claudeAnswers(t, dir, 0, claudeResult(goodStatus))
	text, _, _ := busyTranscript(h.clock.Now().Add(-2*time.Minute), 4, "one")
	h.transcript(id, text)
	h.git.head = "c0ffee"
	h.ticks(1, time.Minute)
	ps := h.statusEvents()
	if n := claudeCalls(t, dir); n != 1 || len(ps) != 1 || ps[0].Record == nil || ps[0].Usage.InputTokens != 900 || ps[0].Usage.OutputTokens != 120 || ps[0].Facts.Tools != 4 || ps[0].Facts.Failed != 0 {
		t.Fatalf("%d calls, events %+v", n, ps)
	}
	in := readFake(t, dir, "in.1")
	args := readFake(t, dir, "args.1")
	if !strings.Contains(in, "one step 3") || !strings.Contains(in, `"status_md"`) || !strings.Contains(args, "haiku") || !strings.Contains(args, "--tools") {
		t.Fatalf("first call: %s\n%s", in, args)
	}
	// Within five minutes: no call, however much is new.
	more, _, _ := busyTranscript(h.clock.Now(), 2, "two")
	h.transcript(id, more)
	h.ticks(3, time.Minute)
	if n := claudeCalls(t, dir); n != 1 {
		t.Fatalf("%d calls within five minutes", n)
	}
	// After five minutes: only what is new, with the previous record, the
	// STATUS.md lines changed and the new commits.
	h.status("s", "# s\n\n| Item | State |\n|---|---|\n| L #21 contexts | merged |\n| K #20 poll | review |\n")
	h.git.head, h.git.subjects = "beef", []string{"beef Reject empty frames"}
	h.ticks(2, time.Minute)
	in = readFake(t, dir, "in.2")
	if n := claudeCalls(t, dir); n != 2 || strings.Contains(in, "one step") || !strings.Contains(in, "two step 1") ||
		!strings.Contains(in, `"previous":{"session":"s"`) || !strings.Contains(in, "| K #20 poll | review |") ||
		!strings.Contains(in, "beef Reject empty frames") || strings.Contains(in, `"status_md"`) {
		t.Fatalf("%d calls, second input %s", n, in)
	}
	// Nothing new: no call, and no event.
	h.git.subjects = nil
	h.ticks(12, time.Minute)
	if n := claudeCalls(t, dir); n != 2 || len(h.statusEvents()) != 2 {
		t.Fatalf("idle: %d calls, %d events", n, len(h.statusEvents()))
	}
	// A malformed answer is a failure: an event with the error, a gap in
	// the graph, and the last good record kept.
	// A session found with nothing new is read again five minutes later.
	claudeAnswers(t, dir, 0, claudeResult(`{"session":"s","phase":"coding"}`))
	three, _, _ := busyTranscript(h.clock.Now(), 1, "three")
	h.transcript(id, three)
	h.ticks(5, time.Minute)
	ps = h.statusEvents()
	tr := h.sup.Tracks()["s"]
	if n := claudeCalls(t, dir); n != 3 || len(ps) != 3 || ps[2].Record != nil || ps[2].Error == "" || ps[2].Usage == nil ||
		len(tr.StatusPoints) != 3 || tr.StatusPoints[2].Record != nil || tr.StatusLast == nil || tr.StatusLast.Phase != "code" {
		t.Fatalf("%d calls, events %+v, points %+v", n, ps, tr.StatusPoints)
	}
	// A restarted agentd starts where the log says: nothing is sent again.
	h.restart(false)
	claudeAnswers(t, dir, 0, claudeResult(goodStatus))
	h.ticks(6, time.Minute)
	if n := claudeCalls(t, dir); n != 3 {
		t.Fatalf("after a restart: %d calls", n)
	}
	four, _, _ := busyTranscript(h.clock.Now(), 1, "four")
	h.transcript(id, four)
	h.ticks(5, time.Minute)
	if in := readFake(t, dir, "in.4"); claudeCalls(t, dir) != 4 || strings.Contains(in, "three") || !strings.Contains(in, "four step 0") {
		t.Fatalf("after a restart sent %s", in)
	}
	// A finished session is not checked.
	h.status("s", "# s\n\nDONE\n")
	h.ticks(1, time.Minute)
	five, _, _ := busyTranscript(h.clock.Now(), 1, "five")
	h.transcript(id, five)
	h.ticks(10, time.Minute)
	if h.state("s") != Done || claudeCalls(t, dir) != 4 {
		t.Fatalf("state %s, %d calls", h.state("s"), claudeCalls(t, dir))
	}
}

// Off by default, and never in a shadow run: no session is checked.
func TestStatusOffByDefaultAndInShadow(t *testing.T) {
	dir := fakeClaude(t)
	h, id := contextHarness(t, false)
	text, _, _ := busyTranscript(h.clock.Now(), 2, "one")
	h.transcript(id, text)
	h.ticks(10, time.Minute)
	if n := claudeCalls(t, dir); n != 0 {
		t.Fatalf("%d calls", n)
	}
	h.cfg.Jobs.Status.Enabled = true
	h.sup.shadow = true
	more, _, _ := busyTranscript(h.clock.Now(), 2, "two")
	h.transcript(id, more)
	h.ticks(10, time.Minute)
	if n := claudeCalls(t, dir); n != 0 {
		t.Fatalf("shadow: %d calls", n)
	}
}

// The console draws each check of the last hours, a failed one as a gap,
// shows the latest record and counts today's tokens.
func TestStatusGraph(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	rec := func(phase string, p float64, blocked bool) *StatusRecord {
		r := &StatusRecord{Session: "s", Phase: phase, Progress: p, Risk: "low", Summary: "Did " + phase + ".", Next: "More."}
		if blocked {
			r.Blocked, r.Blocker = true, "Waits on the owner."
		}
		return r
	}
	tr := &Track{}
	tr.applyStatus(now.Add(-13*time.Hour), &StatusPayload{Record: rec("design", 0.1, false), Usage: &JobUsage{InputTokens: 5000}}) // yesterday
	tr.applyStatus(now.Add(-7*time.Hour), &StatusPayload{Record: rec("code", 0.2, false), Usage: &JobUsage{InputTokens: 1000}})    // today, before the graph
	tr.applyStatus(now.Add(-20*time.Minute), &StatusPayload{Record: rec("code", 0.5, false), Usage: &JobUsage{InputTokens: 3000, OutputTokens: 100}})
	tr.applyStatus(now.Add(-10*time.Minute), &StatusPayload{Error: "status answer: unknown phase", Usage: &JobUsage{InputTokens: 3000, OutputTokens: 50}})
	tr.applyStatus(now.Add(-5*time.Minute), &StatusPayload{Record: rec("test", 0.75, true), Usage: &JobUsage{InputTokens: 2000, OutputTokens: 100, CacheReadTokens: 10}})
	g := statusGraph(tr, 5*time.Minute, now, time.UTC, func(...string) bool { return false })
	if g == nil || len(g.Slots) != 3 || g.Gaps != 1 || g.Checks != 4 || g.Tokens != 1000+3100+3050+2110 || g.Width != 72*statusBar {
		t.Fatalf("graph %+v", g)
	}
	if s := g.Slots[1]; s.Class != "gap" || s.X != 70*statusBar {
		t.Fatalf("gap %+v", s)
	}
	if s := g.Slots[2]; s.Class != "phase-test blocked" || s.H != 18 || s.Y != 6 || s.X != 71*statusBar {
		t.Fatalf("last %+v", s)
	}
	if g.Last.Phase != "test" || !g.LastAt.Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("last %+v", g.Last)
	}
	if statusGraph(&Track{}, 5*time.Minute, now, time.UTC, nil) != nil {
		t.Fatal("a graph without checks")
	}
}

// The console's panel draws the session's checks, a failed one as a gap,
// with its latest record, and the page shows today's tokens.
func TestConsoleShowsStatusGraph(t *testing.T) {
	h, id, dir := statusHarness(t)
	claudeAnswers(t, dir, 1, claudeResult(strings.Replace(goodStatus, `"blocked":false,"blocker":""`, `"blocked":true,"blocker":"Waits on the owner's colour choice."`, 1)))
	claudeAnswers(t, dir, 0, claudeResult(`{"session":"s"}`))
	one, _, _ := busyTranscript(h.clock.Now().Add(-time.Minute), 2, "one")
	h.transcript(id, one)
	h.ticks(5, time.Minute)
	two, _, _ := busyTranscript(h.clock.Now(), 2, "two")
	h.transcript(id, two)
	h.ticks(5, time.Minute)
	if n := claudeCalls(t, dir); n != 2 {
		t.Fatalf("%d calls", n)
	}
	u, _ := testUI(t, UIConfig{}, newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a"))
	u.now = h.clock.Now
	page := owner(t, u, "/console/widgets")
	for _, want := range []string{`<figure class="status-graph">`, `class="phase-code blocked"`, `class="gap"`, "check failed",
		"Fixed the parser.", "Next: Run the decoder tests.", "<strong>blocked</strong>: Waits on the owner&#39;s colour choice.",
		"60% · risk low", "1 failed checks", "Status checks today: 2.1k tokens."} {
		if !strings.Contains(page, want) {
			t.Fatalf("console lacks %q:\n%s", want, page)
		}
	}
}
