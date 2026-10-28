package main

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	add(at(4*time.Hour), KindTransition, func(e *Event) {
		e.Session, e.Transition = "w4", &TransitionPayload{From: "starting", To: "working", Reason: "working"}
	})
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
		"widgets has 4 issues and 2 requests open: 1 needs you, 1 being worked.",
		"4 issues, 1 need you, 1 being worked, <span class=\"box-shown\">4</span> shown",
		`class="box-filter"`, "Profiling the settings page; the cache is next.", "(status)", "(idle)",
		"No sentence yet; the forge&#39;s title stands.", "(withheld: it matched a confidential pattern)",
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
		"The item to pick the cache was opened.", "The item to pick the cache was given to a session.", "The item to pick the cache asks you: Which cache size",
		"wa1 is done when the owner says so. Go waits on your answer.", `name="scope" value="widgets#5"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	body = owner(t, u, "/repo/widgets/issue/4")
	for _, want := range []string{"Profiling the settings page; the cache is next.", "status", "request !9", "Review round 1 started on the request “Speed up #4”.", `href="#mr-9">request !9</a>`, `id="mr-9"`} {
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
		p := IndexPayload{Repository: "widgets", Ref: "#6", Title: "Step " + string(rune('a'+k)), State: "open", Labels: []string{"docs"}, Author: "cy", Opened: pageAt.Add(-480 * time.Hour), Changed: pageAt.Add(time.Duration(k-30) * time.Minute)}
		if err := f.log.Append(Event{V: EventVersion, Time: pageAt.Add(time.Duration(k-30) * time.Minute), Kind: KindIndex, Mode: ModeLive, Index: &p}); err != nil {
			t.Fatal(err)
		}
	}
	u, _ := testUI(t, UIConfig{}, f.src)
	body := owner(t, u, "/repo/widgets/issue/6")
	if strings.Contains(body, "Step a") || !strings.Contains(body, "Step t") || !strings.Contains(body, `href="?steps=all">every entry`) {
		t.Fatal("the latest steps, with every entry behind a link")
	}
	if body := owner(t, u, "/repo/widgets/issue/6?steps=all"); !strings.Contains(body, "Step a") || !strings.Contains(body, "Retitled on the forge to “Typo in the help page”.") {
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
	if n := strings.Count(owner(t, u, "/repo/widgets/issue/4"), "Review round 1 started on the request"); n != 1 {
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

// An issue's or a request's number opens it on the repository's forge,
// GitHub's or GitLab's, in a new tab, and the title agentd's own page;
// with no forge address configured, no forge link is made up.
func TestLevelForgeLinks(t *testing.T) {
	for _, c := range []struct {
		repo Repository
		kind byte
		n    int
		want string
	}{
		{Repository{Forge: "github", Project: "o/w"}, 0, 0, "https://github.com/o/w"},
		{Repository{Forge: "github", Project: "o/w"}, '#', 5, "https://github.com/o/w/issues/5"},
		{Repository{Forge: "github", Host: "ghe.example", Project: "o/w"}, '!', 9, "https://ghe.example/o/w/pull/9"},
		{Repository{Forge: "gitlab", Project: "g/s/w"}, '#', 5, "https://gitlab.com/g/s/w/-/issues/5"},
		{Repository{Forge: "gitlab", Host: "gitlab.example", Project: "g/w"}, '!', 9, "https://gitlab.example/g/w/-/merge_requests/9"},
		{Repository{Forge: "gitlab", Project: "g/a b"}, 0, 0, "https://gitlab.com/g/a%20b"},
		{Repository{Forge: "github"}, '#', 5, ""},
		{Repository{Project: "o/w"}, '#', 5, ""},
		{Repository{Forge: "github", Project: "o/../w"}, 0, 0, ""},
		{Repository{Forge: "github", Host: "evil.example/x", Project: "o/w"}, 0, 0, ""},
	} {
		if got := webURL(c.repo, c.kind, c.n); got != c.want {
			t.Errorf("%+v %c%d: %q, want %q", c.repo, c.kind, c.n, got, c.want)
		}
	}
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{}, f.src)
	for path, want := range map[string]string{
		"/repo/widgets/issue/5": `<h2><a class="forge-ref" href="https://github.com/example-org/widgets/issues/5" target="_blank" rel="noopener"`,
		"/repo/widgets?issue":   `<a class="forge-ref" href="https://github.com/example-org/widgets/issues/5" target="_blank" rel="noopener" title="Open #5 on its forge">#5</a> <strong class="row-title"><a href="/repo/widgets/issue/5">Choose a cache</a></strong>`,
		"/repo/widgets":         `<a class="forge-ref" href="https://github.com/example-org/widgets/pull/9" target="_blank" rel="noopener" title="Open !9 on its forge">!9</a> <strong class="row-title"><a href="/repo/widgets/issue/4#mr-9">Speed up #4</a></strong>`,
	} {
		if body := owner(t, u, strings.TrimSuffix(path, "?issue")); !strings.Contains(body, want) {
			t.Errorf("%s: missing %q", path, want)
		}
	}
	r := f.cfg.Repositories["widgets"]
	r.Project = ""
	f.cfg.Repositories["widgets"] = r
	u, _ = testUI(t, UIConfig{}, f.src)
	for _, path := range []string{"/", "/repo/widgets", "/repo/widgets/issue/5"} {
		body := owner(t, u, path)
		if path == "/" && !strings.Contains(body, `<a href="/repo/widgets">widgets</a></th>`) {
			t.Errorf("/: the repository's row is missing:\n%s", body)
		}
		if strings.Contains(body, "forge-link") || strings.Contains(body, "forge-ref") || strings.Contains(body, "github.com") {
			t.Errorf("%s: a forge link with no forge address:\n%s", path, body)
		}
	}
	if body := owner(t, u, "/repo/widgets"); !strings.Contains(body, `<a href="/repo/widgets/issue/5">#5</a> <strong class="row-title"><a href="/repo/widgets/issue/5">Choose a cache</a></strong>`) {
		t.Errorf("with no forge address the number does not open agentd's page:\n%s", body)
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
		"Across 1 repository: 4 issues and 2 requests open, 1 being worked. 1 issue waits on you, in widgets.",
		`<a href="/repo/widgets">widgets</a> <a class="forge-link" href="https://github.com/example-org/widgets" target="_blank" rel="noopener" title="Open on its forge">github.com/example-org/widgets ↗</a></th><td>4</td><td>1</td><td>2</td><td><strong>1</strong></td><td>3h</td>`,
		`class="meter"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	for rows, want := range map[string][]HomeRow{
		"Across 2 repositories: 3 issues and 0 requests open, 0 being worked. 2 issues wait on you, in a, b.": {
			{Repository: "a", Listed: true, Counts: IndexCounts{Open: 1, Waiting: 1}}, {Repository: "b", Listed: true, Counts: IndexCounts{Open: 2, Waiting: 1}}},
		"1 repository configured; agentd does not list its issues yet.": {{Repository: "a"}},
		"Across 1 repository: 1 issue and 1 request open, 1 being worked. Nothing waits on you. 1 repository not listed yet.": {
			{Repository: "a", Listed: true, Counts: IndexCounts{Open: 1, Working: 1, Requests: 1}}, {Repository: "b"}},
	} {
		if got := homeSentence(want); got != rows {
			t.Errorf("sentence %q, want %q", got, rows)
		}
	}
	day := func(h int) time.Time { return time.Date(2026, 10, 23, h, 0, 0, 0, time.UTC) }
	l := &AccountLedger{Spends: []LedgerSpend{
		{At: day(1), Account: "a", Repository: "widgets", Session: "w4", Kind: SpendSession, Tokens: 300},
		{At: day(2), Account: "a", Repository: "widgets", Kind: "status", Tokens: 20},
		{At: day(3), Account: "b", Kind: "thread", Tokens: 50},
		{At: day(0).Add(-time.Hour), Account: "a", Repository: "widgets", Session: "w4", Kind: SpendSession, Tokens: 9000},
	}}
	// A day starts at midnight where the owner is: at 01:00 in UTC+3
	// it is 22:00 the day before in UTC.
	east := time.FixedZone("UTC+3", 3*3600)
	if e := todayTokens([]*AccountLedger{l}, time.Date(2026, 10, 23, 1, 0, 0, 0, time.UTC), east); e.Since != time.Date(2026, 10, 23, 0, 0, 0, 0, east) || e.Total != 300+9000 {
		t.Fatalf("today in UTC+3: %+v", e)
	}
	tt := todayTokens([]*AccountLedger{l}, pageAt, time.UTC)
	if tt.Total != 370 || tt.ByKind[0] != (NamedSpend{Name: SpendSession, Tokens: 300}) || len(tt.ByKind) != 3 ||
		tt.ByRepository[0] != (NamedSpend{Name: "widgets", Tokens: 320}) || tt.ByRepository[1].Name != "(agentd)" || tt.BySession[0].Tokens != 300 {
		t.Fatalf("today %+v", tt)
	}
}

// levelsCLI writes the levels fixture's log and a config for it to files,
// as the CLI reads them, and returns the config's path.
func levelsCLI(t *testing.T, f *levelsFixture) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(logPath, f.buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	wd := f.cfg.Sessions["w4"].Workdir
	cfgPath := filepath.Join(dir, "agentd.json")
	writeFile(t, cfgPath, `{"event_log": "`+logPath+`", "transcript_dir": "", "poll": {"index": true},
		"repositories": {"widgets": {"forge": "github", "project": "example-org/widgets"}},
		"sessions": {"w4": {"workdir": "`+wd+`", "cwd": "`+wd+`", "repository": "widgets", "issue": 4}}}`)
	return cfgPath
}

// The CLI prints the repository's page, its issue box sorted and
// filtered, its history and the issue's page in the pages' sentences,
// and records the console box's forms as the pages do; the old forms
// still work, and a repository named like a subcommand is refused.
func TestLevelsCLI(t *testing.T) {
	f := newLevelsFixture(t)
	cfgPath := levelsCLI(t, f)
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		ok, err := levelConsoleCmd(append([]string{"--config", cfgPath}, args...), &out)
		if !ok || err != nil {
			t.Fatalf("%v: %v %v", args, ok, err)
		}
		return out.String()
	}
	page := run("widgets")
	for _, want := range []string{
		"widgets has 4 issues and 2 requests open: 1 needs you, 1 being worked.",
		"4 issues, 1 need you, 1 being worked, 4 shown", "Profiling the settings page; the cache is next. (status)",
		"Asks you (w:wa1): w4 asks: Which cache size, 64 or 256 MB? [decline]",
		"2 open requests", "!9 is reviewing: round 1",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("console widgets lacks %q:\n%s", want, page)
		}
	}
	if box := run("widgets", "--issues", "--sort", "oldest", "--filter", "docs"); !strings.Contains(box, "4 issues, 1 need you, 1 being worked, 1 shown") || !strings.Contains(box, "#6") || strings.Contains(box, "#4 ") {
		t.Errorf("issue box:\n%s", box)
	}
	if h := run("widgets", "--history"); !strings.Contains(h, "#5  The item to pick the cache was opened.") || !strings.Contains(h, "!9  Review round 1 started on the request “Speed up #4”.") {
		t.Errorf("history:\n%s", h)
	}
	var out bytes.Buffer
	if err := issueCmd([]string{"--config", cfgPath, "widgets", "5"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#5 Choose a cache (needs you)", "Asks you (w:wa1): w4 asks: Which cache size", "decline: the item closes and nothing more is done on it", "Answer: agentd console reply --entry w:wa1 YOUR ANSWER, or --choice decline",
		"The item to pick the cache was opened. [item wa1]", "wa1 is done when the owner says so. Go waits on your answer."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("issue widgets 5 lacks %q:\n%s", want, out.String())
		}
	}
	if err := issueCmd([]string{"--config", cfgPath, "widgets", "99"}, io.Discard); err == nil {
		t.Error("an unknown issue printed")
	}
	// The console box's forms.
	run("ask", "widgets#4", "Use", "the", "small", "cache.")
	run("ask", "home", "What needs me?")
	run("message", "--to", "w4", "--now", "Push it.")
	run("reply", "--entry", "w:wa1", "--choice", "decline")
	log, err := OpenLog(filepath.Join(filepath.Dir(cfgPath), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	events, _, _ := log.Tail(0)
	log.Close()
	var got []*InstructionPayload
	for _, e := range events {
		if e.Kind == KindInstruction {
			got = append(got, e.Instruction)
		}
	}
	if len(got) != 4 || got[0].Scope != "widgets#4" || got[0].To != "w4" || got[0].Now || got[1].Scope != "home" ||
		got[2].Scope != "widgets#4" || !got[2].Now || got[3].Re == nil || got[3].Re.ID != "w:wa1" || got[3].Answer == nil || got[3].Answer.Choice != "decline" {
		for _, g := range got {
			t.Logf("%+v", g)
		}
		t.Fatal("the CLI's forms recorded otherwise than the pages'")
	}
	// The old forms are not taken for a repository.
	if ok, _ := levelConsoleCmd([]string{"--config", cfgPath, "reply", "widgets", "hello"}, io.Discard); ok {
		t.Error("console reply CONSOLE TEXT was taken as a level form")
	}
	if ok, _ := levelConsoleCmd([]string{"--config", cfgPath, "post", "widgets", "hello"}, io.Discard); ok {
		t.Error("console post was taken as a level form")
	}
	bad := filepath.Join(t.TempDir(), "agentd.json")
	writeFile(t, bad, `{"event_log": "`+filepath.Join(t.TempDir(), "e.jsonl")+`", "transcript_dir": "", "repositories": {"ask": {"forge": "github", "project": "example-org/ask"}}}`)
	if ok, err := levelConsoleCmd([]string{"--config", bad, "ask", "home", "hi"}, io.Discard); !ok || err == nil || !strings.Contains(err.Error(), "named like") {
		t.Errorf("a repository named ask: %v %v", ok, err)
	}
}

// The CLI's flags may come anywhere a user writes them: --entry after
// another flag, and agentd issue's flags after the issue; a flag written
// after the text is refused, not taken as words; --now for a session that
// works no issue is refused; the conversation is screened as the page's;
// a view of a log that does not exist fails.
func TestLevelsCLIFlags(t *testing.T) {
	f := newLevelsFixture(t)
	cfgPath := levelsCLI(t, f)
	var out bytes.Buffer
	if ok, err := levelConsoleCmd([]string{"--config", cfgPath, "reply", "--choice", "decline", "--entry", "w:wa1"}, &out); !ok || err != nil {
		t.Fatalf("--entry second: %v %v", ok, err)
	}
	if ok, err := levelConsoleCmd([]string{"--config", cfgPath, "ask", "widgets#4", "Push", "it", "--now"}, io.Discard); !ok || err == nil || !strings.Contains(err.Error(), "put the flags first") {
		t.Fatalf("a flag after the text: %v %v", ok, err)
	}
	if err := issueCmd([]string{"widgets", "6", "--config", cfgPath, "--all"}, &out); err != nil || !strings.Contains(out.String(), "#6 Typo in the help page") {
		t.Fatalf("issue flags after the issue: %v\n%s", err, out.String())
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Sessions["w9"] = Session{Workdir: cfg.Sessions["w4"].Workdir, Cwd: cfg.Sessions["w4"].Cwd, Repository: "widgets"}
	if err := consoleMessageCmd(cfg, []string{"--to", "w9", "--now", "hi"}, io.Discard); err == nil || !strings.Contains(err.Error(), "works no issue") {
		t.Fatalf("--now with no issue: %v", err)
	}
	talk := screenTalk(&Config{ConfidentialPatterns: []string{"(?i)secret"}}, []ScopeEntry{{Text: "the secret plan", Answer: "a secret answer"}})
	if talk[0].Text != withheldMessage || talk[0].Answer != withheldMessage {
		t.Fatalf("talk %+v", talk)
	}
	missing := *cfg
	missing.EventLog = filepath.Join(t.TempDir(), "none.jsonl")
	if err := levelCmd(&missing, "widgets", nil, io.Discard); err == nil {
		t.Fatal("a view of a missing log printed")
	}
	if _, err := os.Stat(missing.EventLog); err == nil {
		t.Fatal("a view created a log")
	}
}

// What waits on the owner is counted on the home row and the repository
// page whether or not an indexed issue shows it: a request the loop waits
// on the owner for, a work item that asks with no issue, and a session's
// decision in a repository whose issues are not listed. Each is listed
// among the repository's needs.
func TestWaitsOnYouCountsWhatWaits(t *testing.T) {
	h, api := loopHarness(t)
	outsideOpen(api)
	h.ticks(3, 5*time.Minute)
	tr := h.sup.track(judgmentName("!1", KindReviewer))
	tr.State, tr.Stopped = Stopped, "its spend cap"
	h.ticks(1, 5*time.Minute)
	if p := h.place("!1"); p.Phase != PlaceOwner {
		t.Fatalf("place = %+v", p)
	}
	now := h.clock.Now()
	if row := BuildHomeRow(h.cfg, h.sup.st, "widgets", nil, now); row.Counts.Waiting != 1 {
		t.Fatalf("a request waiting on the owner: home counts %d", row.Counts.Waiting)
	}
	if h.sup.st.Work == nil {
		h.sup.st.Work = map[string]*WorkItem{}
	}
	h.sup.st.Work["w9"] = &WorkItem{ID: "w9", Repository: "widgets", State: WorkBlocked, Question: "Which cache size?", ThreadWork: ThreadWork{Title: "Pick the cache"}}
	ds := []UIDecision{{Session: "e", Repository: "widgets", Decision: Decision{ID: "q1", Question: "Ship it on Friday?"}}}
	v := BuildRepoLevel(h.cfg, h.sup.st, "widgets", ds, now)
	if v.Counts.Waiting != 3 || len(v.Needs) != 3 {
		t.Fatalf("waiting %d, needs %+v", v.Counts.Waiting, v.Needs)
	}
	if row := BuildHomeRow(h.cfg, h.sup.st, "widgets", ds, now); row.Counts.Waiting != 3 {
		t.Fatalf("home counts %d", row.Counts.Waiting)
	}
	// The delivering session's own decision is about the same issue as its
	// request, which is not listed: one thing waits, shown twice.
	ds = append(ds, UIDecision{Session: "d", Repository: "widgets", Decision: Decision{ID: "q2", Question: "Rename the flag?"}})
	if v := BuildRepoLevel(h.cfg, h.sup.st, "widgets", ds, now); v.Counts.Waiting != 3 || len(v.Needs) != 4 || v.IssuesWaiting() != 0 {
		t.Fatalf("waiting %d, issues waiting %d, needs %+v", v.Counts.Waiting, v.IssuesWaiting(), v.Needs)
	}
}

// The issue box and the MR box are scroll containers a refresh swaps in:
// the refresh keeps each one's scroll position across the swap and the
// sort that follows it, so the box does not jump back to its top every
// 30 seconds.
func TestRefreshKeepsTheBoxScroll(t *testing.T) {
	js, err := uiAssets.ReadFile("ui/ui.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	start := strings.Index(s, "function refresh()")
	end := strings.Index(s[start:], "\n  }\n")
	if start < 0 || end < 0 {
		t.Fatal("no refresh in ui.js")
	}
	body := s[start : start+end]
	read, swap, sorted, restore := strings.Index(body, "top: old.scrollTop"), strings.Index(body, "old.replaceWith"), strings.Index(body, "levelBoxes()"), strings.LastIndex(body, "el.scrollTop =")
	if !strings.Contains(body, `inner: inner ? inner.scrollTop`) || !strings.Contains(body, "inner.scrollTop = scrolled[id].inner") {
		t.Fatalf("refresh does not keep the scroll of the list inside a section:\n%s", body)
	}
	if read < 0 || swap < 0 || sorted < 0 || restore < 0 || !(read < swap && swap < sorted && sorted < restore) {
		t.Fatalf("refresh does not keep a box's scroll across the swap and the sort:\n%s", body)
	}
}

// stepTexts are the words of the steps on an issue page, chips and times
// left out, and the chips' markup apart.
func stepTexts(t *testing.T, body string) (texts []string, chips string) {
	t.Helper()
	start := strings.Index(body, `<ol class="steps">`)
	end := strings.Index(body[start:], "</ol>")
	if start < 0 || end < 0 {
		t.Fatalf("no steps:\n%s", body)
	}
	for _, li := range strings.Split(body[start:start+end], "<li>")[1:] {
		if c := strings.Index(li, `<span class="step-chips">`); c >= 0 {
			chips += li[c:]
			li = li[:c]
		}
		li = regexp.MustCompile(`<time[^>]*>[^<]*</time>|<[^>]+>`).ReplaceAllString(li, "")
		texts = append(texts, strings.TrimSpace(html.UnescapeString(li)))
	}
	return texts, chips
}

// The steps say what happened in sentences with names, the item by its
// title and the request by its title; ids are in chips alone. No step
// holds an item's id, a session's name or hex id, or a bare #N or !N
// outside a quoted title. A sentence rewritten is no step, and the steps
// that matter are kept when the ring is trimmed.
func TestStepsAreSentences(t *testing.T) {
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{}, f.src)
	ids := regexp.MustCompile(`\bW[0-9a-f]{8}\b|\b[0-9a-f]{12,}\b|\bwa1\b|\bw4\b`)
	bare := regexp.MustCompile(`[#!][0-9]+`)
	quoted := regexp.MustCompile(`“[^”]*”`)
	var all []string
	chips := ""
	for _, target := range []string{"/repo/widgets/issue/4?steps=all", "/repo/widgets/issue/5?steps=all", "/repo/widgets/issue/6?steps=all"} {
		texts, c := stepTexts(t, owner(t, u, target))
		all, chips = append(all, texts...), chips+c
	}
	for _, s := range all {
		if ids.MatchString(s) || bare.MatchString(quoted.ReplaceAllString(s, "")) {
			t.Errorf("a step holds an id: %q", s)
		}
		if strings.Contains(s, "Profiling the settings page") {
			t.Errorf("a sentence is a step: %q", s)
		}
	}
	for _, want := range []string{"The item to pick the cache was opened.", "The item to pick the cache was given to a session.", "The item to pick the cache asks you: Which cache size, 64 or 256 MB?", "Review round 1 started on the request “Speed up #4”."} {
		if !contains(all, want) {
			t.Errorf("no step %q among the %d", want, len(all))
		}
	}
	if !strings.Contains(chips, "wa1") || !strings.Contains(chips, "!9") {
		t.Errorf("the chips hold no ids: %s", chips)
	}

	// Past the ring's size the item's opening stays.
	for k := 0; k < stepsKept+20; k++ {
		p := IndexPayload{Repository: "widgets", Ref: "#5", Title: fmt.Sprintf("Choose a cache, take %d", k), State: "open", Labels: []string{"design"}, Author: "bo", Opened: pageAt.Add(-48 * time.Hour), Changed: pageAt.Add(time.Duration(k-50) * time.Minute)}
		if err := f.log.Append(Event{V: EventVersion, Time: pageAt.Add(time.Duration(k-50) * time.Minute), Kind: KindIndex, Mode: ModeLive, Index: &p}); err != nil {
			t.Fatal(err)
		}
	}
	texts, _ := stepTexts(t, owner(t, u, "/repo/widgets/issue/5?steps=all"))
	if len(texts) != stepsKept || !contains(texts, "The item to pick the cache was opened.") {
		t.Errorf("%d steps, the opening kept: %v", len(texts), contains(texts, "The item to pick the cache was opened."))
	}
}

// Two things that read alike are two steps when their chips differ; a
// ring of steps that all matter keeps them over a new one that does not;
// a reason's ids are put as words.
func TestAddStep(t *testing.T) {
	st := NewLogState(&Config{}, ModeLive)
	st.addStep("r", IssueStep{Ref: "#1", Text: "A reviewer started on it at 21:56.", Chips: []StepRef{{Kind: "session", ID: "a"}}, Key: true})
	st.addStep("r", IssueStep{Ref: "#1", Text: "A reviewer started on it at 21:56.", Chips: []StepRef{{Kind: "session", ID: "b"}}, Key: true})
	st.addStep("r", IssueStep{Ref: "#1", Text: "A reviewer started on it at 21:56.", Chips: []StepRef{{Kind: "session", ID: "b"}}, Key: true})
	if n := len(st.Steps[sentenceKey("r", "#1")]); n != 2 {
		t.Fatalf("%d steps", n)
	}
	for k := 0; k < stepsKept; k++ {
		st.addStep("r", IssueStep{Ref: "#2", Text: fmt.Sprintf("Key %d.", k), Key: true})
	}
	st.addStep("r", IssueStep{Ref: "#2", Text: "Routine."})
	if s := st.Steps[sentenceKey("r", "#2")]; len(s) != stepsKept || s[0].Text != "Key 0." || s[len(s)-1].Text != "Key 99." {
		t.Fatalf("the ring: %d, %q … %q", len(s), s[0].Text, s[len(s)-1].Text)
	}
	if got := scrubIDs("the reviewer 1bdb6beef6abadb2 of !12 for W0a1b2c3d on #7 failed"); got != "the reviewer a session of a request for an item on an issue failed" {
		t.Fatalf("scrubbed: %q", got)
	}
}

// The issue's console box offers "now" only when a session that takes
// messages works the issue; a message sent with now where none does is
// refused and says why, from the page and the command line alike, and
// nothing is recorded.
func TestNowOnlyWithASession(t *testing.T) {
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{}, f.src)
	if body := owner(t, u, "/repo/widgets/issue/4"); !strings.Contains(body, `name="now"`) {
		t.Fatal("#4, which w4 works, offers no now")
	}
	if body := owner(t, u, "/repo/widgets/issue/6"); strings.Contains(body, `name="now"`) || !strings.Contains(body, "no session works it now") {
		t.Fatal("#6, which no session works, offers now")
	}
	before := f.buf.Len()
	w := ownerPost(u, "/reply", url.Values{"src": {"live"}, "repository": {"widgets"}, "scope": {"widgets#6"}, "text": {"Look at the typo."}, "now": {"1"}, "back": {"/repo/widgets/issue/6"}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "no session") || f.buf.Len() != before {
		t.Fatalf("now with no session: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.src.scopeReply(Scope{Repository: "widgets", Issue: 6}, "Look at the typo.", true, nil); err == nil || f.buf.Len() != before {
		t.Fatal("the command line's ask --now with no session was recorded")
	}
	if _, err := f.src.scopeReply(Scope{Repository: "widgets"}, "Look at the typo.", true, nil); err == nil || !strings.Contains(err.Error(), "only at an issue") || f.buf.Len() != before {
		t.Fatalf("now at a repository: %v", err)
	}
	if w := ownerPost(u, "/reply", url.Values{"src": {"live"}, "repository": {"widgets"}, "scope": {"widgets#6"}, "text": {"Look at the typo."}, "back": {"/repo/widgets/issue/6"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("without now: %d %s", w.Code, w.Body.String())
	}
}

// A blocked item's question gets an answer card: the owner's words go to
// its session as the answer, and decline stays; done and reassign are not
// offered to a question.
func TestBlockedItemTakesAnAnswer(t *testing.T) {
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{}, f.src)
	body := owner(t, u, "/repo/widgets/issue/5")
	start := strings.Index(body, `id="decision-w-wa1"`)
	end := strings.Index(body[start:], "</article>")
	if start < 0 || end < 0 {
		t.Fatalf("no card:\n%s", body)
	}
	card := body[start : start+end]
	if strings.Contains(card, `value="done"`) || strings.Contains(card, `value="reassign"`) || !strings.Contains(card, `value="decline"`) || !strings.Contains(card, `placeholder="Your answer"`) {
		t.Fatalf("the card:\n%s", card)
	}
	w := ownerPost(u, "/respond", url.Values{"src": {"live"}, "repository": {"widgets"}, "entry": {"w:wa1"}, "choice": {"done"}, "back": {"/repo/widgets/issue/5"}})
	if w.Code == http.StatusSeeOther {
		t.Fatal("done was taken as the answer to a question")
	}
	if w := ownerPost(u, "/respond", url.Values{"src": {"live"}, "repository": {"widgets"}, "entry": {"w:wa1"}, "text": {"256 MB."}, "back": {"/repo/widgets/issue/5"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("the answer: %d %s", w.Code, w.Body.String())
	}
}
