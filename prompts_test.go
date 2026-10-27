package main

import (
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
	for _, target := range []string{"/session/night/a/prompt/t1.2", "/session/night/a/prompt/t1.99"} {
		if page := owner(t, u, target); !strings.Contains(page, "no prompt") && !strings.Contains(page, "no such line") {
			t.Errorf("%s:\n%s", target, page)
		}
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
