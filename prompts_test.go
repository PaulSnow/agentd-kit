package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// promptsFixture is a harness whose session a has the recorded transcript
// testdata/prompts/transcript.jsonl, as t1, read: a rotation's restart
// prompt, the answers and the instructions pointers, a compaction and a
// typed prompt, each with its reply. a's workdir holds the files they
// point at. It returns the UI over the harness's log.
func promptsFixture(t *testing.T) (*harness, *UI) {
	t.Helper()
	h := turnsHarness(t)
	h.ticks(1, time.Minute)
	sess := h.cfg.Sessions["a"]
	writeFile(t, filepath.Join(sess.Workdir, "BRIEF.md"), "# Paint the widgets\n\nBlue, **primer** first.\n")
	writeFile(t, filepath.Join(sess.Workdir, "STATUS.md"), "# Progress\n\n- [x] primer\n- [ ] second coat\n")
	writeFile(t, filepath.Join(sess.Workdir, AnswersFile), `{"answers":[{"id":"Q1","choice":"blue"}]}`)
	writeFile(t, filepath.Join(sess.Workdir, InstructionsFile), `[{"id":"I1","text":"Start the second coat after lunch.","new":true}]`)
	b, err := os.ReadFile("testdata/prompts/transcript.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	r := strings.NewReplacer("{workdir}", sess.Workdir, "{cwd}", sess.Cwd, "{at}", h.clock.Now().Format(time.RFC3339Nano))
	h.transcriptOf("a", "t1", r.Replace(string(b)))
	h.ticks(2, time.Minute)
	u, _ := testUI(t, UIConfig{}, newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a"))
	u.now = h.clock.Now
	return h, u
}

// Every prompt on the console and on the session's page is one line
// linked to its page: a typed prompt by its first sentence, agentd's own
// prompts and a compaction by what they are.
func TestPromptLinesLinkTheirPages(t *testing.T) {
	_, u := promptsFixture(t)
	lines := []string{
		`<a href="/session/night/a/prompt/t1.1">Restarted (rotation): brief, progress →</a>`,
		`<a href="/session/night/a/prompt/t1.3">Answers delivered →</a>`,
		`<a href="/session/night/a/prompt/t1.5">Your message delivered →</a>`,
		`<a href="/session/night/a/prompt/t1.7">Context compacted →</a>`,
		`Paint the widgets blue. <a href="/session/night/a/prompt/t1.8">full prompt →</a>`,
	}
	for _, target := range []string{"/console/widgets", "/session/night/a"} {
		page := owner(t, u, target)
		for _, want := range lines {
			if !strings.Contains(page, want) {
				t.Errorf("%s lacks %q", target, want)
			}
		}
		for _, not := range []string{"You were restarted by the supervisor", "This session is being continued", "check the primer"} {
			if strings.Contains(page, not) {
				t.Errorf("%s shows the prompt's text %q", target, not)
			}
		}
	}
}

// A prompt's page shows the prompt whole, read from the transcript now,
// and the files it points at as they are now: Markdown rendered, JSON
// pretty.
func TestPromptPageShowsTheWholePromptAndItsFiles(t *testing.T) {
	h, u := promptsFixture(t)
	page := owner(t, u, "/session/night/a/prompt/t1.1")
	for _, want := range []string{"Restarted (rotation): brief, progress", "You were restarted by the supervisor with a fresh context.",
		"What the supervisor recorded", `&#34;why&#34;: &#34;rotation&#34;,`, `&#34;last_merge&#34;: &#34;abc1234&#34;`,
		"<h3>brief</h3>", "<h3>Paint the widgets</h3>", "<strong>primer</strong>", "<h3>progress</h3>", "<li>[ ] second coat</li>"} {
		if !strings.Contains(page, want) {
			t.Errorf("restart page lacks %q:\n%s", want, page)
		}
	}
	page = owner(t, u, "/session/night/a/prompt/t1.3")
	if !strings.Contains(page, "<h3>answers</h3>") || !strings.Contains(page, "&#34;choice&#34;: &#34;blue&#34;") || !strings.Contains(page, "The owner answered your questions.") {
		t.Errorf("answers page:\n%s", page)
	}
	page = owner(t, u, "/session/night/a/prompt/t1.5")
	if !strings.Contains(page, "<h3>instructions</h3>") || !strings.Contains(page, "Start the second coat after lunch.") {
		t.Errorf("instructions page:\n%s", page)
	}
	page = owner(t, u, "/session/night/a/prompt/t1.7")
	if !strings.Contains(page, "Context compacted") || !strings.Contains(page, "so a cut prompt would lose it.") || strings.Contains(page, "<h3>brief</h3>") {
		t.Errorf("compaction page:\n%s", page)
	}
	page = owner(t, u, "/session/night/a/prompt/t1.8")
	if !strings.Contains(page, "report what it says, in full, with the exit code.") || strings.Contains(page, `id="files"`) {
		t.Errorf("typed prompt's page:\n%s", page)
	}
	// As they are now: the progress file changed since the restart.
	writeFile(t, filepath.Join(h.cfg.Sessions["a"].Workdir, "STATUS.md"), "# Progress\n\n- [x] second coat\n")
	if page := owner(t, u, "/session/night/a/prompt/t1.1"); !strings.Contains(page, "<li>[x] second coat</li>") {
		t.Errorf("progress not read now:\n%s", page)
	}
	for _, target := range []string{"/session/night/a/prompt/t1", "/session/night/a/prompt/..%2Ft1.1", "/session/night/a/prompt/t1.01",
		"/session/night/a/prompt/t1.0", "/session/night/nobody/prompt/t1.1", "/session/elsewhere/a/prompt/t1.1", "/session/night/a/other/t1.1"} {
		if w := get(u, "GET", target, pageAddr, "192.0.2.20:51000"); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
}

// The prompt and each file are screened whole and withheld whole; a file
// is read only as a regular file under the session's workdir, through no
// link.
func TestPromptPageScreensAndReadsOnlyTheWorkdir(t *testing.T) {
	h, _ := promptsFixture(t)
	workdir := h.cfg.Sessions["a"].Workdir
	writeFile(t, filepath.Join(workdir, "BRIEF.md"), "# Paint\n\nThe embargoed colour.\n")
	if err := os.Remove(filepath.Join(workdir, "STATUS.md")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "STATUS.md")
	writeFile(t, outside, "# Elsewhere\n")
	if err := os.Symlink(outside, filepath.Join(workdir, "STATUS.md")); err != nil {
		t.Fatal(err)
	}
	h.cfg.ConfidentialPatterns = []string{"(?i)embargoed", "(?i)second coat starts"}
	u2, _ := testUI(t, UIConfig{}, newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a"))
	u2.now = h.clock.Now
	page := owner(t, u2, "/session/night/a/prompt/t1.1")
	if strings.Contains(page, "embargoed") || strings.Contains(page, "Elsewhere") || !strings.Contains(page, "withheld because it matched") || !strings.Contains(page, "is a link") {
		t.Errorf("restart page:\n%s", page)
	}
	h.cfg.ConfidentialPatterns = []string{"(?i)host-a and report"}
	u3, _ := testUI(t, UIConfig{}, newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a"))
	u3.now = h.clock.Now
	if page := owner(t, u3, "/session/night/a/prompt/t1.8"); strings.Contains(page, "Paint the widgets") || !strings.Contains(page, "The prompt was withheld") {
		t.Errorf("a confidential prompt is shown:\n%s", page)
	}
	for _, c := range []struct{ path, want string }{
		{filepath.Join(filepath.Dir(workdir), "BRIEF.md"), "not under the session's workdir"},
		{filepath.Join(workdir, "..", "x", "BRIEF.md"), "not under the session's workdir"},
		{"", "names no path"},
	} {
		if _, err := underWorkdir(workdir, c.path); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v", c.path, err)
		}
	}
	linked := filepath.Join(workdir, "docs")
	if err := os.Symlink(t.TempDir(), linked); err != nil {
		t.Fatal(err)
	}
	if _, err := underWorkdir(workdir, filepath.Join(linked, "BRIEF.md")); err == nil || !strings.Contains(err.Error(), "through a link") {
		t.Errorf("a file through a linked directory: %v", err)
	}
}

// A prompt as the log holds it, cut, is still named by what it is.
func TestLabelPromptCut(t *testing.T) {
	pol := DefaultConfig().Policies
	cut := cutUTF8(DefaultRestartPrompt+" "+pol.Answers.Pointer+` {"why":"answered","brief":"b"}`, turnText)
	for _, c := range []struct {
		role, text string
		want       promptLabel
	}{
		{TurnPrompt, cut, promptLabel{Link: "Restarted (answered): brief, progress, answers"}},
		{TurnPrompt, DefaultRestartPrompt + ` {"why":"instructed","brief":"b","progress":"p"}`, promptLabel{Link: "Restarted (instructed): brief, progress, instructions"}},
		{TurnPrompt, cutUTF8(pol.Instructions.Pointer, 60), promptLabel{Link: "Your message delivered"}},
		{TurnPrompt, cutUTF8(DefaultRestartPrompt, 60), promptLabel{Link: "Restarted: brief, progress"}},
		{TurnPrompt, cutUTF8(DefaultRestartPrompt+" "+pol.Answers.Pointer+" "+pol.Instructions.Pointer+` {"why":"answered-instructed"}`, 360), promptLabel{Link: "Restarted (answered-instructed): brief, progress, answers, instructions"}},
		{TurnPrompt, pol.Nudge.Message, promptLabel{Link: "Nudged to carry on"}},
		{TurnPrompt, compactPosted, promptLabel{Link: "Context compacted"}},
		{TurnPrompt, "Why does it squeak? Look at the hinge.", promptLabel{Lead: "Why does it squeak?", Link: "full prompt"}},
		{TurnPrompt, "Line one\nline two", promptLabel{Lead: "Line one", Link: "full prompt"}},
		{TurnPrompt, "v1.2 is out", promptLabel{Lead: "v1.2 is out", Link: "full prompt"}},
	} {
		if got := labelPrompt(pol, c.role, c.text); got != c.want {
			t.Errorf("%q: %+v, want %+v", cutUTF8(c.text, 50), got, c.want)
		}
	}
}

// A compaction is logged without its text, and the supervisor judges a
// session's turn by the turns around it, as before (#58).
func TestCompactionIsNoTurnToJudge(t *testing.T) {
	h, _ := promptsFixture(t)
	tr := NewLogState(h.cfg, ModeLive).Fold(h.events()).Tracks["a"]
	if tr.TurnCount != 8 {
		t.Fatalf("turn count %d, want 8: the compaction counted", tr.TurnCount)
	}
	for _, e := range h.events() {
		if e.Turn != nil && e.Turn.Role == TurnCompact && (e.Turn.Text != "" || e.Turn.Line != 7) {
			t.Fatalf("compaction logged as %+v", e.Turn)
		}
	}
	tr.Turns = append(tr.Turns, TurnRecord{Role: TurnCompact, Transcript: "t1", Line: 10, At: h.clock.Now()})
	if last := tr.lastTurn(); last.Role != TurnSummary || last.Line != 9 {
		t.Fatalf("last turn = %+v", last)
	}
}

// A prompt link never dead-ends (#58 follow-up): a line that is no prompt
// is named in plain words and shown, screened whole; only a missing
// transcript or line is an error, which names the one missing.
func TestPromptPageSaysWhatALineIs(t *testing.T) {
	h, u := promptsFixture(t)
	for _, c := range []struct{ id, what, text string }{
		{"t1.2", "The session&#39;s reply, not a prompt.", "<pre>Read the brief; painting goes on.</pre>"},
		{"t1.10", "A background-task notice from Claude Code, not a prompt.", "&lt;result&gt;The primer branch is ready: two findings, both fixed.&lt;/result&gt;"},
		{"t1.11", "The session&#39;s own words or work, not a prompt.", "Bash: {&#34;command&#34;:&#34;./bin/check&#34;}"},
		{"t1.12", "A tool&#39;s result, which Claude Code hands back to the session, not a prompt.", "CHECK PASS"},
		{"t1.13", "Claude Code&#39;s note before a slash command&#39;s output, not a prompt.", "DO NOT respond to these messages."},
		{"t1.14", "A file-history-snapshot line Claude Code keeps for its own bookkeeping, not a prompt.", "&#34;isSnapshotUpdate&#34;: false"},
		{"t1.99", "", "Line 99 of transcript t1 is missing."},
		{"t9.1", "", "Transcript t9 is missing."},
	} {
		page := owner(t, u, "/session/night/a/prompt/"+c.id)
		if !strings.Contains(page, c.what) || !strings.Contains(page, c.text) {
			t.Errorf("%s lacks %q or %q:\n%s", c.id, c.what, c.text, page)
		}
		if c.what != "" && !strings.Contains(page, "<h2 id=\"prompt-h\">Not a prompt") {
			t.Errorf("%s is not headed as no prompt:\n%s", c.id, page)
		}
	}
	// Screened whole, as read and as shown: a match anywhere withholds all
	// of it, one only the decoded text has too.
	h.cfg.ConfidentialPatterns = []string{"(?i)two findings", "(?m)^CHECK PASS"}
	u2, _ := testUI(t, UIConfig{}, newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a"))
	u2.now = h.clock.Now
	page := owner(t, u2, "/session/night/a/prompt/t1.10")
	if strings.Contains(page, "primer branch") || !strings.Contains(page, "background-task notice") || !strings.Contains(page, "withheld because it matched") {
		t.Errorf("a confidential line is shown:\n%s", page)
	}
	if page := owner(t, u2, "/session/night/a/prompt/t1.12"); strings.Contains(page, "CHECK PASS") || !strings.Contains(page, "withheld because it matched") {
		t.Errorf("a line confidential only as shown is shown:\n%s", page)
	}
}

// Turns a build before #56 B logged as prompts, recorded in that shape,
// are judged by its rule when the views are built: a harness entry is no
// prompt, a compaction is named as one, and the rest still link their
// pages. The supervisor takes neither as the session's last instruction.
func TestPre56PromptsJudgedByTodaysRule(t *testing.T) {
	h, _ := promptsFixture(t)
	b, err := os.ReadFile("testdata/prompts/pre56-log.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	pairs := []string{"{at}", h.clock.Now().Format(time.RFC3339Nano)}
	for _, n := range []int{5, 12, 14, 15} {
		pairs = append(pairs, fmt.Sprintf("{id:t0.%d}", n), turnMessageID("a", "t0", n))
	}
	h.buf.WriteString(strings.NewReplacer(pairs...).Replace(string(b)))
	u, _ := testUI(t, UIConfig{}, newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a"))
	u.now = h.clock.Now
	for _, target := range []string{"/console/widgets", "/session/night/a"} {
		page := owner(t, u, target)
		for _, want := range []string{
			`<a href="/session/night/a/prompt/t0.5">Restarted (rotation): brief, progress →</a>`,
			`<a href="/session/night/a/prompt/t0.12">Context compacted →</a>`,
			`Continue with the second coat as the brief says. <a href="/session/night/a/prompt/t0.14">full prompt →</a>`,
		} {
			if !strings.Contains(page, want) {
				t.Errorf("%s lacks %q", target, want)
			}
		}
		for _, not := range []string{"task-notification", "a0ld5e55", "prompt/t0.15"} {
			if strings.Contains(page, not) {
				t.Errorf("%s shows the harness entry: %q", target, not)
			}
		}
	}
	tr := NewLogState(h.cfg, ModeLive).Fold(h.events()).Tracks["a"]
	if got := h.sup.lastInstruction(tr); got != "Continue with the second coat as the brief says." {
		t.Errorf("last instruction %q", got)
	}
}

// loggedNoPrompt reads the log's text, whole or cut: an entry the harness
// wrote is none; a typed prompt that opens with a tag, or is cut, is one.
func TestLoggedNoPrompt(t *testing.T) {
	for _, c := range []struct {
		text string
		want bool
	}{
		{"<task-notification>\n<task-id>a1</task-id>\n</task-notification>", true},
		{cutUTF8("<task-notification>\n<task-id>a1</task-id>\n<result>"+strings.Repeat("ok ", 200), turnText), true},
		{cutUTF8("<system-reminder>"+strings.Repeat("x", 400), turnText), true},
		{"<command-name>/clear</command-name>", true},
		{"<widget> is the tag to paint, not <gadget>.", false},
		{cutUTF8("<widget> "+strings.Repeat("paint ", 100), turnText), false},
		{"<task-notification> is what I mean; explain it", false},
		{cutUTF8("Paint the widgets "+strings.Repeat("blue ", 100), turnText), false},
	} {
		if got := loggedNoPrompt(c.text); got != c.want {
			t.Errorf("%q: %v", cutUTF8(c.text, 40), got)
		}
	}
}
