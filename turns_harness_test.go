package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// injectedLine is a user entry Claude Code wrote itself, as a transcript
// holds it.
func injectedLine(uuid, text string, at time.Time, extra map[string]any) string {
	l := map[string]any{"type": "user", "uuid": uuid, "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"role": "user", "content": text}}
	for k, v := range extra {
		l[k] = v
	}
	b, _ := json.Marshal(l)
	return string(b) + "\n"
}

// Only real prompts are turns: a task notification, a system reminder, a
// shell escape's input and output, a slash command and a compaction's
// summary are the harness's, not anyone's prompt. A long reply is posted
// whole, and the page folds it.
func TestOnlyRealPromptsAndWholeReplies(t *testing.T) {
	h := turnsHarness(t)
	at := h.clock.Now()
	long := strings.Repeat("The widget squeaks no more; every check passes.\n", 60)
	h.transcriptOf("a", "t1", promptLine("u1", "Build the widget.", at)+
		injectedLine("u2", "<task-notification>\n<task-id>x</task-id>\n</task-notification>", at, nil)+
		injectedLine("u3", "<system-reminder>be brief</system-reminder>", at, nil)+
		injectedLine("u4", "<bash-input>ls</bash-input>", at, nil)+
		injectedLine("u5", "<bash-stdout>a b</bash-stdout><bash-stderr></bash-stderr>", at, nil)+
		injectedLine("u6", "<command-name>/clear</command-name>", at, nil)+
		injectedLine("u7", "This session is being continued from a previous conversation.", at, map[string]any{"isCompactSummary": true})+
		summaryLine("u8", "m1", long, at, 100)+
		injectedLine("u9", "", at, map[string]any{"message": map[string]any{"role": "user", "content": []map[string]any{
			{"type": "text", "text": "<ide_opened_file>w.go</ide_opened_file>"}, {"type": "text", "text": "<div> breaks on mobile"}}}}))
	h.ticks(3, time.Minute)
	got := h.turnMessages()
	if len(got) != 3 || got[0].Text != "Build the widget." || got[1].Kind != MessageReply || got[1].Text != strings.TrimSpace(long) || got[2].Text != "<div> breaks on mobile" {
		var kinds []string
		for _, m := range got {
			kinds = append(kinds, m.Kind+":"+cutUTF8(m.Text, 30))
		}
		t.Fatalf("turns posted = %q", kinds)
	}
	u, _ := testUI(t, UIConfig{}, newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a"))
	page := owner(t, u, "/console/widgets")
	if strings.Contains(page, "task-notification") || !strings.Contains(page, `<details class="more">`) || !strings.Contains(page, "Show all") {
		t.Fatalf("console:\n%s", page)
	}
	if !strings.Contains(owner(t, u, "/session/night/a"), `<details class="more">`) {
		t.Fatal("the session page does not fold the long reply")
	}
}

// A session's page lists the tools it ran lately, read from the end of
// its transcript, with the failed ones and their exit codes marked, and
// the one it runs now; a confidential one is withheld. Nothing of it is
// logged.
func TestSessionPageActivity(t *testing.T) {
	h := turnsHarness(t)
	h.cfg.ConfidentialPatterns = []string{`hunter2`}
	h.restart(false)
	at := h.clock.Now()
	running, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": "u9", "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"id": "m9", "stop_reason": "tool_use", "content": []map[string]any{{"type": "tool_use", "id": "c9", "name": "Bash", "input": map[string]any{"command": "./bin/check"}}}}})
	h.transcriptOf("a", "t1", promptLine("u1", "Build the widget.", at)+toolLines("u2", "c1", "go build ./...", 0, at)+
		toolLines("u3", "c2", "go test ./...", 1, at)+toolLines("u4", "c3", "echo hunter2", 0, at)+string(running)+"\n")
	h.ticks(2, time.Minute)
	u, _ := testUI(t, UIConfig{}, newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a"))
	page := owner(t, u, "/session/night/a")
	for _, want := range []string{"<code>Bash: go build ./...</code>", "<code>Bash: go test ./...</code> <span class=\"badge\">failed, exit 1</span>", "Running now: <code>Bash: ./bin/check</code>", withheldMessage} {
		if !strings.Contains(page, want) {
			t.Fatalf("session page lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "hunter2") || strings.Contains(h.buf.String(), "go build") {
		t.Fatal("the activity leaked or was logged")
	}
}

// The console shows a panel per session under the repository's box and
// above the history: its state, the tool it runs now, its last reply and
// a box addressed to it; a finished session takes no message.
func TestConsolePanels(t *testing.T) {
	h := turnsHarness(t)
	h.cfg.ConfidentialPatterns = []string{`hunter2`}
	h.restart(false)
	at := h.clock.Now()
	running, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": "u9", "timestamp": at.Format(time.RFC3339Nano),
		"message": map[string]any{"id": "m9", "stop_reason": "tool_use", "content": []map[string]any{{"type": "tool_use", "id": "c9", "name": "Bash", "input": map[string]any{"command": "./bin/check"}}}}})
	h.transcriptOf("a", "t1", promptLine("u1", "Build the widget.", at)+summaryLine("u2", "m1", "Primed the widget.", at, 100)+
		promptLine("u3", "Now paint it.", at)+string(running)+"\n")
	h.transcriptOf("b", "t2", promptLine("u1", "Count the widgets.", at)+summaryLine("u2", "m1", "Counted hunter2 widgets.", at, 100)+
		strings.Replace(string(running), "./bin/check", "sleep 9", 1)+"\n")
	h.status("b", "# b\n\nDONE\n")
	h.ticks(2, time.Minute)
	u, _ := testUI(t, UIConfig{}, newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a"))
	page := owner(t, u, "/console/widgets")
	box, panels, history := strings.Index(page, `id="reply-h"`), strings.Index(page, `id="panels"`), strings.Index(page, `id="messages"`)
	if box < 0 || panels < box || history < panels {
		t.Fatalf("the boxes and panels are not above the history (%d, %d, %d):\n%s", box, panels, history, page)
	}
	for _, want := range []string{`<a href="/session/night/a">a</a>`, "Now: <code>Bash: ./bin/check</code>", "Primed the widget.", `name="session" value="a">`, "Send to a<"} {
		if !strings.Contains(page[panels:history], want) {
			t.Fatalf("the panels lack %q:\n%s", want, page[panels:history])
		}
	}
	if strings.Contains(page, "hunter2") || !strings.Contains(page[panels:history], withheldMessage) || strings.Contains(page, "sleep 9") {
		t.Fatalf("the panels leak or show a finished session's open call:\n%s", page[panels:history])
	}
	if !strings.Contains(page[panels:history], `<a href="/session/night/b">b</a>`) || strings.Contains(page, "Send to b<") {
		t.Fatalf("the finished session's panel is missing or takes a message:\n%s", page[panels:history])
	}
}
