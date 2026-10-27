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
// shell escape's input and output and a slash command are the harness's,
// not anyone's prompt; a compaction's summary is posted as one, linked to
// its text (#58). A long reply is posted whole, and the page folds it.
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
			{"type": "text", "text": "<ide_opened_file>w.go</ide_opened_file>"}, {"type": "text", "text": "<div> breaks on mobile"}}}})+
		promptLine("u10", "<b>bold</b> renders raw", at))
	h.ticks(3, time.Minute)
	got := h.turnMessages()
	if len(got) != 5 || got[4].Text != "<b>bold</b> renders raw" || got[0].Text != "Build the widget." || got[1].Text != compactPosted || got[2].Kind != MessageReply || got[2].Text != strings.TrimSpace(long) || got[3].Text != "<div> breaks on mobile" {
		var kinds []string
		for _, m := range got {
			kinds = append(kinds, m.Kind+":"+cutUTF8(m.Text, 30))
		}
		t.Fatalf("turns posted = %q", kinds)
	}
	u, _ := testUI(t, UIConfig{}, newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a"))
	page := owner(t, u, "/console/widgets")
	if strings.Contains(page, "task-notification") || !strings.Contains(page, `<details class="more" data-key="m-e-`) || !strings.Contains(page, "Show all") ||
		!strings.Contains(page, `<a href="/session/night/a/prompt/t1.7">Context compacted →</a>`) || strings.Contains(page, "being continued") {
		t.Fatalf("console:\n%s", page)
	}
	if !strings.Contains(owner(t, u, "/session/night/a"), `<details class="more" data-key="t-`) {
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

// The console shows its sessions as one line of chips above the box
// (#59 §3), each with its state and the tool it runs now, and the
// composer pinned below the box; a finished session takes no message.
func TestConsoleChips(t *testing.T) {
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
	chips, box, composer := strings.Index(page, `id="chips"`), strings.Index(page, `id="box"`), strings.Index(page, `id="composer"`)
	if chips < 0 || box < chips || composer < box || strings.Contains(page, `id="panels"`) {
		t.Fatalf("not chips, box, composer (%d, %d, %d):\n%s", chips, box, composer, page)
	}
	line := page[chips:box]
	for _, want := range []string{`data-session="a" data-to="a"`, `<span class="doing">Bash: ./bin/check</span>`, `href="/session/night/a"`, `data-session="b">`} {
		if !strings.Contains(line, want) {
			t.Fatalf("the chips lack %q:\n%s", want, line)
		}
	}
	if strings.Contains(page, "hunter2") || strings.Contains(page, "sleep 9") || strings.Contains(line, "Primed the widget.") {
		t.Fatalf("the chips leak, show a finished session's open call, or hold a reply:\n%s", line)
	}
	if to := page[composer:]; !strings.Contains(to, `<option value="a">a</option>`) || strings.Contains(to, `<option value="b">`) {
		t.Fatalf("the composer's To: offers the finished session or not the working one:\n%s", to)
	}
}
