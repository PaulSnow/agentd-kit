package main

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// levelsFixture is a live run's log for widgets, a GitHub repository with
// the index on: issues #4 (a session works it, its request !9 in review),
// #5 (a work item that asks the owner), #6 (idle, retitled) and #7
// (withheld), and requests !9 and !10.
type levelsFixture struct {
	cfg *Config
	buf *bytes.Buffer
	log *Log
	src *uiSource
}

func newLevelsFixture(t *testing.T) *levelsFixture {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Poll.Index = true
	cfg.Repositories = map[string]Repository{
		"widgets": {Forge: "github", Project: "example-org/widgets"},
	}
	wd := t.TempDir()
	cfg.Sessions = map[string]Session{"w4": {Workdir: wd, Cwd: wd, Repository: "widgets", Issue: 4}}
	if err := cfg.finish(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	f := &levelsFixture{cfg: &cfg, buf: &bytes.Buffer{}}
	f.log = NewLogWriter(f.buf)
	at := func(d time.Duration) time.Time { return pageAt.Add(-d) }
	day := func(n int) time.Time { return time.Date(2026, 10, n, 9, 0, 0, 0, time.UTC) }
	add := func(t0 time.Time, kind string, set func(*Event)) {
		e := Event{V: EventVersion, Time: t0, Kind: kind, Mode: ModeLive}
		set(&e)
		if err := f.log.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	index := func(t0 time.Time, p IndexPayload) {
		p.Repository = "widgets"
		add(t0, KindIndex, func(e *Event) { e.Index = &p })
	}
	sentence := func(t0 time.Time, p SentencePayload) {
		p.Repository = "widgets"
		add(t0, KindSentence, func(e *Event) { e.Sentence = &p })
	}
	index(at(5*time.Hour), IndexPayload{Ref: "#4", Title: "Slow settings page", State: "open", Labels: []string{"perf"}, Milestone: "v2", Author: "ann", Opened: day(1), Changed: day(20), First: true})
	index(at(5*time.Hour), IndexPayload{Ref: "#5", Title: "Choose a cache", State: "open", Labels: []string{"design"}, Author: "bo", Opened: day(2), Changed: day(21), First: true})
	index(at(5*time.Hour), IndexPayload{Ref: "#6", Title: "Typo in help", State: "open", Labels: []string{"docs"}, Author: "cy", Opened: day(3), Changed: day(10), First: true})
	index(at(5*time.Hour), IndexPayload{Ref: "#7", State: "open", Opened: day(4), Changed: day(11), First: true, Withheld: 1})
	index(at(5*time.Hour), IndexPayload{Ref: "!9", Title: "Speed up #4", State: "open", Author: "ann", Branch: "perf-4", Opened: day(15), Changed: day(22), First: true})
	index(at(5*time.Hour), IndexPayload{Ref: "!10", Title: "Docs", State: "open", Author: "cy", Branch: "docs", Opened: day(16), Changed: day(18), First: true})
	add(at(4*time.Hour), KindLoop, func(e *Event) {
		e.Loop = &LoopPayload{Repository: "widgets", Ref: "!9", Case: "widgets#4", Step: LoopReview, Round: 1, Session: "widgets-perf-4-reviewer"}
	})
	sentence(at(4*time.Hour), SentencePayload{Ref: "!9", Text: "!9 is reviewing: round 1", Writer: WriterLoop})
	sentence(at(3*time.Hour), SentencePayload{Ref: "#4", Text: "Profiling the settings page; the cache is next.", Writer: WriterStatus, Session: "w4"})
	add(at(3*time.Hour), KindWorkItem, func(e *Event) {
		e.Work = &WorkPayload{ID: "wa1", Step: WorkStepOpen, Repository: "widgets", Instruction: "in1", Work: &ThreadWork{Title: "Pick the cache", Issue: 5, DoneWhen: EndCheck{Kind: DoneOwner}}}
	})
	add(at(2*time.Hour), KindWorkItem, func(e *Event) {
		e.Work = &WorkPayload{ID: "wa1", Step: WorkStepAssign, Session: "w4"}
	})
	add(at(time.Hour), KindWorkItem, func(e *Event) {
		e.Work = &WorkPayload{ID: "wa1", Step: WorkStepBlocked, Note: "Which cache size, 64 or 256 MB?"}
	})
	index(at(30*time.Minute), IndexPayload{Ref: "#6", Title: "Typo in the help page", State: "open", Labels: []string{"docs"}, Author: "cy", Opened: day(3), Changed: day(23)})
	sentence(at(20*time.Minute), SentencePayload{Ref: "#6", Text: "Asks to fix a typo; nobody has answered.", Writer: WriterIdle, For: day(23)})
	f.src = newUISource("live", f.cfg, f.log, ModeLive, "host-a")
	return f
}

// The repository page and the issue page match their golden HTML. Run
// go test -run TestLevelsGolden -update to rewrite them after a change.
func TestLevelsGolden(t *testing.T) {
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{}, f.src)
	for name, target := range map[string]string{"repo": "/repo/widgets", "issue": "/repo/widgets/issue/5"} {
		body := owner(t, u, target)
		path := filepath.Join("testdata", "ui", name+".html")
		if *updateGolden {
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update to write it)", err)
		}
		if body != string(want) {
			t.Errorf("%s differs from %s (run with -update to rewrite it):\n%s", target, path, body)
		}
	}
}

// Level 2: the state sentence, the issue box's count, its ten sorts and
// its filter field, each issue's word, sentence and writer, the requests
// joined to their issues, and the MR box's five sorts; needs-me puts the
// issue that waits on the owner first, then the one being worked.
func TestRepoLevel(t *testing.T) {
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{}, f.src)
	body := owner(t, u, "/repo/widgets")
	for _, want := range []string{
		"widgets has 4 issues and 2 requests open: 1 needs you, 2 being worked.",
		"4 issues, 1 need you, 2 being worked, <span class=\"box-shown\">4</span> shown",
		`class="box-filter"`, "Profiling the settings page; the cache is next.", "(status)", "(idle)",
		"No sentence yet; the forge's title stands.", "(withheld: it matched a confidential pattern)",
		`href="/repo/widgets/issue/4#mr-9"`, "!9 is reviewing: round 1", `href="/console/widgets">history</a>`,
		`name="scope" value="widgets"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, s := range append(append([]string{}, IssueSorts...), RequestSorts...) {
		if !strings.Contains(body, `<option value="`+s+`">`) {
			t.Errorf("no sort %s", s)
		}
	}
	if len(IssueSorts) != 10 || len(RequestSorts) != 5 {
		t.Fatalf("%d issue sorts, %d request sorts", len(IssueSorts), len(RequestSorts))
	}
	i5, i4, i6 := strings.Index(body, `data-ref="#5"`), strings.Index(body, `data-ref="#4"`), strings.Index(body, `data-ref="#6"`)
	if i5 < 0 || i4 < i5 || i6 < i4 {
		t.Fatalf("needs-me order: #5 %d, #4 %d, #6 %d", i5, i4, i6)
	}
	if !strings.Contains(body, `data-ref="#5" data-number="5" data-word="needs you"`) {
		t.Fatal("#5 does not need the owner")
	}
	// The same sorts and filter as the page's script, in Go.
	v := BuildRepoLevel(f.cfg, f.src.st, "widgets", nil, pageAt)
	sortIssues(v.Issues, "oldest")
	if v.Issues[0].Ref != "#4" {
		t.Fatalf("oldest: %s", v.Issues[0].Ref)
	}
	sortIssues(v.Issues, "has-mr")
	if v.Issues[0].Ref != "#4" {
		t.Fatalf("has-mr: %s", v.Issues[0].Ref)
	}
	if got := filterIssues(v.Issues, "!9"); len(got) != 1 || got[0].Ref != "#4" {
		t.Fatalf("filter by request: %+v", got)
	}
	if got := filterIssues(v.Issues, "docs"); len(got) != 1 || got[0].Ref != "#6" {
		t.Fatalf("filter by label: %+v", got)
	}
	if got := filterIssues(v.Issues, "typo help"); len(got) != 1 {
		t.Fatalf("filter by words: %+v", got)
	}
	// A repository agentd does not list says so.
	f.cfg.Poll.Index = false
	if body := owner(t, u, "/repo/widgets"); !strings.Contains(body, "agentd does not list widgets&#39;s issues") {
		t.Fatalf("an untracked repository is not said to be so:\n%s", body)
	}
}

// Level 3: refs, the sentence, the decision card with its options and
// their consequences, the dated steps with "every entry", and next;
// answering the card records the same instruction as the console's
// buttons.
func TestIssueLevel(t *testing.T) {
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{}, f.src)
	body := owner(t, u, "/repo/widgets/issue/5")
	for _, want := range []string{
		"item wa1 · blocked", "session w4", "w4 asks: Which cache size, 64 or 256 MB?",
		`name="entry" value="w:wa1"`, `value="decline">decline</button> <span class="muted">the item closes and nothing more is done on it</span>`,
		"Work item wa1 opened: Pick the cache.", "wa1 given to w4.", "wa1 asks you: Which cache size",
		"wa1 is done when the owner says so. Go waits on your answer.", `name="scope" value="widgets#5"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	body = owner(t, u, "/repo/widgets/issue/4")
	for _, want := range []string{"Profiling the settings page; the cache is next.", "status", "request !9", "!9: review round 1 started.", `id="mr-9"`} {
		if !strings.Contains(body, want) {
			t.Errorf("#4 missing %q", want)
		}
	}
	if w := get(u, "GET", "/repo/widgets/issue/99", pageAddr, "192.0.2.20:51000"); w.Code != 404 {
		t.Fatalf("an unknown issue: %d", w.Code)
	}
	if w := get(u, "GET", "/repo/widgets/issue/x", pageAddr, "192.0.2.20:51000"); w.Code != 404 {
		t.Fatalf("a bad number: %d", w.Code)
	}
	// The card's answer is the console's: an instruction about w:wa1.
	w := ownerPost(u, "/respond", url.Values{"src": {"live"}, "repository": {"widgets"}, "entry": {"w:wa1"}, "choice": {"decline"}, "back": {"/repo/widgets/issue/5"}})
	if w.Code != 303 {
		t.Fatalf("respond: %d %s", w.Code, w.Body.String())
	}
	events, _, _ := f.log.Tail(0)
	found := false
	for _, e := range events {
		if e.Kind == KindInstruction && e.Instruction.Re != nil && e.Instruction.Re.ID == "w:wa1" {
			found = true
		}
	}
	if !found {
		t.Fatal("the card's answer recorded no instruction about w:wa1")
	}
}

// Many steps show the latest, with every entry behind a link.
func TestIssueStepsEveryEntry(t *testing.T) {
	f := newLevelsFixture(t)
	for k := 0; k < 20; k++ {
		p := SentencePayload{Repository: "widgets", Ref: "#6", Text: "Step " + string(rune('a'+k)), Writer: WriterIdle, For: pageAt}
		if err := f.log.Append(Event{V: EventVersion, Time: pageAt.Add(time.Duration(k-30) * time.Minute), Kind: KindSentence, Mode: ModeLive, Sentence: &p}); err != nil {
			t.Fatal(err)
		}
	}
	u, _ := testUI(t, UIConfig{}, f.src)
	body := owner(t, u, "/repo/widgets/issue/6")
	if strings.Contains(body, "Step a") || !strings.Contains(body, "Step t") || !strings.Contains(body, `href="?steps=all">every entry`) {
		t.Fatal("the latest steps, with every entry behind a link")
	}
	if body := owner(t, u, "/repo/widgets/issue/6?steps=all"); !strings.Contains(body, "Step a") || !strings.Contains(body, "Retitled on the forge: Typo in the help page") {
		t.Fatal("every entry")
	}
}

// A page load calls no forge and no model: the UI has no forge client,
// and a fake claude on the PATH is never run.
func TestLevelPagesCallNoModel(t *testing.T) {
	dir := fakeClaude(t)
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{}, f.src)
	for _, target := range []string{"/", "/repo/widgets", "/repo/widgets/issue/4", "/repo/widgets/issue/5", "/repo/widgets/issue/6?steps=all"} {
		owner(t, u, target)
	}
	if n := claudeCalls(t, dir); n != 0 {
		t.Fatalf("%d model calls from page loads", n)
	}
}

// A card or a step whose words match a confidential pattern is withheld
// on the issue page, as the console withholds it; a loop step shows once;
// a repository's name may hold a slash.
func TestIssueLevelScreensAndDedupes(t *testing.T) {
	f := newLevelsFixture(t)
	f.cfg.ConfidentialPatterns = []string{"(?i)256 MB"}
	u, _ := testUI(t, UIConfig{}, f.src)
	body := owner(t, u, "/repo/widgets/issue/5")
	if strings.Contains(body, "256 MB") || !strings.Contains(body, "withheld") || !strings.Contains(body, `name="entry" value="w:wa1"`) {
		t.Fatalf("not withheld:\n%s", body)
	}
	if n := strings.Count(owner(t, u, "/repo/widgets/issue/4"), "!9: review round 1 started."); n != 1 {
		t.Fatalf("the loop step shows %d times", n)
	}
	for path, want := range map[string]struct {
		repo string
		n    int
		ok   bool
	}{"/repo/a/b": {"a/b", 0, true}, "/repo/a/b/issue/3": {"a/b", 3, true}, "/repo/w/issue/x": {"", 0, false}, "/repo/": {"", 0, false}, "/repo/w/": {"", 0, false}} {
		if repo, n, ok := levelPath(path); repo != want.repo || n != want.n || ok != want.ok {
			t.Errorf("%s: %q %d %v", path, repo, n, ok)
		}
	}
}

// Level 1: a sentence across the repositories, a row each with its
// counts, linked to its page, and the token meter; today's tokens split
// by kind, repository and session.
func TestHomeLevel(t *testing.T) {
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{}, f.src)
	body := owner(t, u, "/")
	for _, want := range []string{
		"Across 1 repository: 4 issues and 2 requests open, 2 being worked. 1 issue waits on you, in widgets.",
		`<a href="/repo/widgets">widgets</a></th><td>4</td><td>2</td><td>2</td><td><strong>1</strong></td><td>3h</td>`,
		`name="scope" value="home"`, `class="meter"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	day := func(h int) time.Time { return time.Date(2026, 10, 23, h, 0, 0, 0, time.UTC) }
	l := &AccountLedger{Spends: []LedgerSpend{
		{At: day(1), Account: "a", Repository: "widgets", Session: "w4", Kind: SpendSession, Tokens: 300},
		{At: day(2), Account: "a", Repository: "widgets", Kind: "status", Tokens: 20},
		{At: day(3), Account: "b", Kind: "thread", Tokens: 50},
		{At: day(0).Add(-time.Hour), Account: "a", Repository: "widgets", Session: "w4", Kind: SpendSession, Tokens: 9000},
	}}
	tt := todayTokens([]*AccountLedger{l}, pageAt, time.UTC)
	if tt.Total != 370 || tt.ByKind[0] != (NamedSpend{Name: SpendSession, Tokens: 300}) || len(tt.ByKind) != 3 ||
		tt.ByRepository[0] != (NamedSpend{Name: "widgets", Tokens: 320}) || tt.ByRepository[1].Name != "agentd" || tt.BySession[0].Tokens != 300 {
		t.Fatalf("today %+v", tt)
	}
}
