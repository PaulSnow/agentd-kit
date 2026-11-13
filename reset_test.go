package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// standIn is a stand-in for Claude Code under tmux: it shows a prompt,
// and for each line entered writes the turn to its transcript, where
// Claude Code keeps it, and answers. What it is given it also appends to
// given.txt in its account's directory.
const standIn = `id=$1
dir="$CLAUDE_CONFIG_DIR/projects/$(pwd | sed 's/[^A-Za-z0-9]/-/g')"
mkdir -p "$dir"
f="$dir/$id.jsonl"
while true; do
  printf '❯ '
  IFS= read -r line || exit 0
  now=$(date -u +%Y-%m-%dT%H:%M:%S.000Z)
  printf '%s\n' "$line" >> "$CLAUDE_CONFIG_DIR/given.txt"
  printf '{"type":"user","isSidechain":false,"timestamp":"%s","message":{"role":"user","content":"%s"}}\n' "$now" "$line" >> "$f"
  printf '{"type":"assistant","isSidechain":false,"timestamp":"%s","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"The work stands at step three of five."}]}}\n' "$now" >> "$f"
  printf 'done\n'
done`

// An idle page session on the wrong account is asked for a summary, ended,
// started again on the account placement picks, and given the summary; the
// summary is kept for the owner; one with text at its prompt, or active a
// moment ago, is left alone (#96, #97).
func TestIdleReset(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	f := newLevelsFixture(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	f.cfg.Accounts = map[string]Account{"a": {ConfigDir: dirA}, "b": {ConfigDir: dirB}}
	f.cfg.Usage.Enabled = true
	f.cfg.TranscriptDir = filepath.Join(t.TempDir(), "projects")
	read := func(account string, weekly float64, reset time.Time) {
		five := 1.0
		if err := f.log.Append(Event{V: EventVersion, Time: pageAt.Add(-time.Minute), Kind: KindAccount, Mode: ModeShared,
			Account: &AccountPayload{Account: account, Method: UsageEndpoint, FiveHour: &five, Weekly: &weekly, WeeklyReset: &reset}}); err != nil {
			t.Fatal(err)
		}
	}
	read("a", 40, pageAt.Add(96*time.Hour))
	read("b", 5, pageAt.Add(48*time.Hour))
	u, _ := testUI(t, UIConfig{SessionCommand: []string{"bash", "-c", standIn, "stand-in", "{session_id}"}}, f.src)
	u.home = t.TempDir()
	u.tmuxArgs = []string{"-L", fmt.Sprintf("agentd-test-reset-%d", time.Now().UnixNano()), "-f", "/dev/null"}
	t.Cleanup(func() { u.tmux("kill-server") })
	u.resetPoll, u.resetPause, u.resetSummary, u.resetPrompt = 50*time.Millisecond, 100*time.Millisecond, 10*time.Second, 10*time.Second
	clone := filepath.Join(u.home, "repos", "github.com", "example-org", "widgets")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	name := workspaceSession("widgets")
	s := pageSession{name, clone, defaultWorkspaceCommand, f.src.cfg}
	if err := u.ensureSession(name, clone, defaultWorkspaceCommand); err != nil {
		t.Fatal(err)
	}
	if got := u.sessionAccount(name); got != "b" {
		t.Fatalf("started on %q, want b", got)
	}
	idle := func(what string) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if u.screenBusy(name) == "" {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("%s: the session is not at its prompt: %s", what, u.screenBusy(name))
	}
	idle("at start")
	// A session with no transcript has done no work.
	if why, worked := u.sessionBusy(s, time.Now()); why != "" || worked {
		t.Errorf("a new session: busy %q, worked %v", why, worked)
	}
	if err := u.typeLine(name, "do the first thing"); err != nil {
		t.Fatal(err)
	}
	idle("after its first turn")

	// Now a's week resets first: the session should move to a.
	read("a", 40, pageAt.Add(24*time.Hour))
	if on, to := u.sessionMove(name, pageAt); on != "b" || to != "a" {
		t.Fatalf("on %q, to %q", on, to)
	}
	// Active a moment ago: not idle yet.
	if why, _ := u.sessionBusy(s, time.Now()); !strings.Contains(why, "active within") {
		t.Errorf("a session that just wrote: %q", why)
	}
	u.resetIdle(pageAt)
	if st := u.resetOf(name); st != nil {
		t.Fatalf("a session active a moment ago was reset: %+v", st)
	}
	if text, title := u.resetNote(s, "b", "a"); text != "Needs a reset → a" || !strings.Contains(title, "active work") {
		t.Errorf("note %q, %q", text, title)
	}
	u.resetQuiet = 0
	// Text typed at its prompt: the owner's, so not idle.
	if out, err := u.tmux("send-keys", "-t", "="+name+":", "-l", "--", "half a thought"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	time.Sleep(200 * time.Millisecond)
	if why, _ := u.sessionBusy(s, time.Now()); why != "text is typed at its prompt" {
		t.Errorf("a draft at the prompt: %q", why)
	}
	u.resetIdle(pageAt)
	if st := u.resetOf(name); st != nil {
		t.Fatalf("a session with a draft was reset: %+v", st)
	}
	if out, err := u.tmux("send-keys", "-t", "="+name+":", "C-u"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	idle("the draft cleared")
	if text, _ := u.resetNote(s, "b", "a"); text != "Reset → a next" {
		t.Errorf("an idle session's note: %q", text)
	}

	u.resetIdle(pageAt)
	var st *resetState
	for i := 0; i < 400; i++ {
		if st = u.resetOf(name); st != nil && !st.going() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st == nil || st.Step != resetDone {
		t.Fatalf("the reset did not finish: %+v", st)
	}
	if got := u.sessionAccount(name); got != "a" || st.From != "b" || st.To != "a" {
		t.Errorf("after the reset: on %q, %+v", got, st)
	}
	asked, _ := os.ReadFile(filepath.Join(dirB, "given.txt"))
	if !strings.Contains(string(asked), "Reply with a summary of the current work") || !strings.Contains(string(asked), "from the Claude account b to a") {
		t.Errorf("the old session was asked: %q", asked)
	}
	kept, err := os.ReadFile(st.File)
	if err != nil || !strings.Contains(string(kept), "The work stands at step three of five.") || !strings.Contains(string(kept), "from the account b to a") {
		t.Errorf("the summary kept: %q %v", kept, err)
	}
	var given string
	for i := 0; i < 100 && !strings.Contains(given, "step three of five"); i++ {
		time.Sleep(50 * time.Millisecond)
		b, _ := os.ReadFile(filepath.Join(dirA, "given.txt"))
		given = string(b)
	}
	if !strings.Contains(given, "this session replaces one that ran on the Claude account b") || !strings.Contains(given, "The work stands at step three of five.") {
		t.Errorf("the new session was given: %q", given)
	}
	// The page names the new account, marks nothing, and links the summary.
	page := owner(t, u, "/r/widgets")
	if !strings.Contains(page, ` · account a</span>`) || !strings.Contains(page, `id="term-move" class="term-move" hidden`) || strings.Contains(page, `id="term-summary" class="term-summary" href="/r/widgets/handover?token=`+u.token+`" target="_blank" rel="noopener" title="The summary this session's predecessor left when agentd moved it to another account" hidden`) {
		t.Error("after the reset the page does not name account a, unmarked, with its summary")
	}
	if got := owner(t, u, "/r/widgets/handover?token="+u.token); !strings.Contains(got, "step three of five") {
		t.Errorf("the summary served: %q", got)
	}
	if w := get(u, "GET", "/r/widgets/handover?token=wrong", pageAddr, "192.0.2.20:51000"); w.Code != 403 {
		t.Errorf("the summary with a wrong token: %d", w.Code)
	}
	// It is not reset again within the gap, whatever placement picks.
	read("b", 5, pageAt.Add(12*time.Hour))
	idle("the new session")
	u.resetIdle(pageAt)
	if now := u.resetOf(name); now.Step != resetDone || u.sessionAccount(name) != "a" {
		t.Errorf("reset again within the gap: %+v", now)
	}
}

// A summary that never comes leaves the session as it was, and the page
// says the try failed; it is not tried again within the gap (#96).
func TestIdleResetNoSummary(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	f := newLevelsFixture(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	f.cfg.Accounts = map[string]Account{"a": {ConfigDir: dirA}, "b": {ConfigDir: dirB}}
	f.cfg.Usage.Enabled = true
	f.cfg.TranscriptDir = filepath.Join(t.TempDir(), "projects")
	read := func(account string, weekly float64, reset time.Time) {
		five := 1.0
		if err := f.log.Append(Event{V: EventVersion, Time: pageAt.Add(-time.Minute), Kind: KindAccount, Mode: ModeShared,
			Account: &AccountPayload{Account: account, Method: UsageEndpoint, FiveHour: &five, Weekly: &weekly, WeeklyReset: &reset}}); err != nil {
			t.Fatal(err)
		}
	}
	read("a", 40, pageAt.Add(96*time.Hour))
	read("b", 5, pageAt.Add(48*time.Hour))
	// It shows a prompt and takes lines, and writes no reply.
	mute := `printf '❯ '; while IFS= read -r line; do printf '❯ '; done`
	u, _ := testUI(t, UIConfig{SessionCommand: []string{"bash", "-c", mute}}, f.src)
	u.home = t.TempDir()
	u.tmuxArgs = []string{"-L", fmt.Sprintf("agentd-test-mute-%d", time.Now().UnixNano()), "-f", "/dev/null"}
	t.Cleanup(func() { u.tmux("kill-server") })
	u.resetPoll, u.resetPause, u.resetSummary, u.resetQuiet = 50*time.Millisecond, 100*time.Millisecond, 500*time.Millisecond, 0
	clone := filepath.Join(u.home, "repos", "github.com", "example-org", "widgets")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	name := workspaceSession("widgets")
	s := pageSession{name, clone, defaultWorkspaceCommand, f.src.cfg}
	if err := u.ensureSession(name, clone, defaultWorkspaceCommand); err != nil {
		t.Fatal(err)
	}
	path, ok := u.sessionTranscript(s)
	if !ok {
		t.Fatal("no transcript place")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	turn := `{"type":"assistant","isSidechain":false,"timestamp":"2026-11-01T00:00:00.000Z","message":{"role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"Done."}]}}` + "\n"
	if err := os.WriteFile(path, []byte(turn), 0o644); err != nil {
		t.Fatal(err)
	}
	read("a", 40, pageAt.Add(24*time.Hour))
	for i := 0; i < 100 && u.screenBusy(name) != ""; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	u.resetIdle(pageAt)
	var st *resetState
	for i := 0; i < 200; i++ {
		if st = u.resetOf(name); st != nil && !st.going() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st == nil || st.Step != resetFailed || !strings.Contains(st.Err, "no summary was returned") {
		t.Fatalf("the reset: %+v", st)
	}
	if !u.sessionExists(name) || u.sessionAccount(name) != "b" {
		t.Error("the session is not as it was")
	}
	if text, title := u.resetNote(s, "b", "a"); text != "Needs a reset → a (the last try failed)" || !strings.Contains(title, "no summary was returned") {
		t.Errorf("note %q, %q", text, title)
	}
	u.resetIdle(pageAt)
	if now := u.resetOf(name); now.At != st.At {
		t.Error("a failed reset was tried again at once")
	}
}

// What stands at the prompt: nothing, a dim suggestion, or typed text; a
// dialog's option and a screen with no prompt are not the prompt; a
// running turn's spinner line is busy though the prompt is empty.
func TestPromptText(t *testing.T) {
	foot := "\n──────\n  repo ⎇ main · 6% ctx\n  ⏵⏵ auto mode on\n"
	for name, c := range map[string]struct {
		screen string
		text   string
		found  bool
	}{
		"empty":      {"● Done.\n──────\n\x1b[39m❯ \x1b[0m" + foot, "", true},
		"suggestion": {"● Done.\n──────\n\x1b[39m❯ \x1b[2myes, do that\x1b[0m" + foot, "", true},
		"typed":      {"● Done.\n──────\n\x1b[39m❯ half a thought\x1b[0m" + foot, "half a thought", true},
		"coloured":   {"● Done.\n──────\n\x1b[39m❯ \x1b[38;5;2mgreen words\x1b[0m" + foot, "green words", true},
		"dialog":     {"Do you trust this folder?\n❯ 1. Yes, proceed\n  2. No, exit\n", "", false},
		"no prompt":  {"building…\nstill building\n", "", false},
	} {
		if text, found := promptText(c.screen); text != c.text || found != c.found {
			t.Errorf("%s: %q, %v, want %q, %v", name, text, found, c.text, c.found)
		}
	}
	if !spinnerLine.MatchString("· Churning… (5m 6s · ↓ 22.9k tokens)") || spinnerLine.MatchString("✻ Brewed for 24s · done 9:43 AM") {
		t.Error("the spinner line of a running turn, or an ended one's, is misread")
	}
}
