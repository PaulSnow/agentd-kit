package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// githubIssues is a GitHub issues page: issues n..m, with labels, a
// milestone and an author, and request r among them.
func githubIssues(from, to, request int, changed string) string {
	var parts []string
	for n := from; n <= to; n++ {
		pr := ""
		if n == request {
			pr = `,"pull_request":{},"draft":true`
		}
		parts = append(parts, fmt.Sprintf(`{"number":%d,"title":"Issue %d","labels":[{"name":"bug"},{"name":"area/ui"}],"milestone":{"title":"v2"},"user":{"login":"author%d"},"created_at":"2026-10-01T00:00:00Z","updated_at":"%s"%s}`, n, n, n%3, changed, pr))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// The index lists every open issue and request on both forges, page by
// page, with labels, milestone, author, dates and a request's branch.
func TestListOpenBothForges(t *testing.T) {
	gh := &fakeForgeAPI{github: map[string]string{
		"/repos/o/p/issues?state=open&per_page=100&page=1": githubIssues(1, 100, 7, "2026-10-20T00:00:00Z"),
		"/repos/o/p/issues?state=open&per_page=100&page=2": githubIssues(101, 103, 0, "2026-10-21T00:00:00Z"),
		"/repos/o/p/pulls?state=open&per_page=100&page=1":  `[{"number":7,"head":{"ref":"fix/12-crash"}}]`,
	}}
	list, cut, err := listOpen(gh, Repository{Forge: "github", Project: "o/p"})
	if err != nil || len(list) != 103 || cut {
		t.Fatalf("%d listed, %v", len(list), err)
	}
	if i := list[0]; i.Ref != "#1" || i.Title != "Issue 1" || strings.Join(i.Labels, ",") != "area/ui,bug" || i.Milestone != "v2" || i.Author != "author1" || i.Opened.IsZero() || i.Changed.IsZero() {
		t.Fatalf("first: %+v", i)
	}
	if r := list[6]; r.Ref != "!7" || r.Branch != "fix/12-crash" || !r.Draft {
		t.Fatalf("request: %+v", r)
	}
	gl := &fakeForgeAPI{gitlab: map[string]string{
		"/issues?state=opened":         `[{"iid":4,"title":"Slow page","labels":["perf"],"milestone":{"title":"m1"},"author":{"username":"ann"},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-02T00:00:00Z"}]`,
		"/merge_requests?state=opened": `[{"iid":9,"title":"Speed up #4","labels":[],"author":{"username":"bo"},"source_branch":"perf-4","created_at":"2026-10-03T00:00:00Z","updated_at":"2026-10-04T00:00:00Z"}]`,
	}}
	list, _, err = listOpen(gl, Repository{Forge: "gitlab", Project: "g/w"})
	if err != nil || len(list) != 2 || list[0].Ref != "#4" || list[0].Milestone != "m1" || list[0].Author != "ann" || list[0].Labels[0] != "perf" ||
		list[1].Ref != "!9" || list[1].Branch != "perf-4" || list[1].Author != "bo" {
		t.Fatalf("%+v %v", list, err)
	}
	gl.set(func() { gl.gitlabErr = errors.New("glab down") })
	if _, _, err := listOpen(gl, Repository{Forge: "gitlab", Project: "g/w"}); err == nil {
		t.Fatal("a failed listing is an error")
	}
}

// indexHarness is a harness whose widgets is a GitLab repository listed by
// a fake forge API.
func indexHarness(t *testing.T) (*harness, *fakeForgeAPI) {
	h := newHarness(t, false)
	api := &fakeForgeAPI{gitlab: map[string]string{
		"widgets/issues?state=opened": `[{"iid":4,"title":"Slow page","labels":["perf"],"milestone":{"title":"m1"},"author":{"username":"ann"},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-02T00:00:00Z"},` +
			`{"iid":5,"title":"Crash on save","labels":["bug"],"author":{"username":"cy"},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-02T00:00:00Z"}]`,
		"widgets/merge_requests?state=opened": `[{"iid":9,"title":"Speed up #4","author":{"username":"bo"},"source_branch":"perf","created_at":"2026-10-03T00:00:00Z","updated_at":"2026-10-04T00:00:00Z"}]`,
		"readonly/issues":                     `[]`, "readonly/merge_requests": `[]`,
	}}
	h.sup.api = api
	h.cfg.Poll.Every, h.cfg.Poll.Index = Duration{time.Minute}, true
	return h, api
}

func (h *harness) indexEvents() []IndexPayload {
	var out []IndexPayload
	for _, e := range h.events() {
		if e.Kind == KindIndex {
			out = append(out, *e.Index)
		}
	}
	return out
}

// The first listing logs every open issue, marked first; a listing with
// nothing new logs nothing; a change logs that issue alone, and one gone
// from the list is logged gone. The fold holds each with its fields, and
// a restarted agentd reads the same index from the log.
func TestIndexFoldsTheListing(t *testing.T) {
	h, api := indexHarness(t)
	h.ticks(1, time.Minute)
	ev := h.indexEvents()
	if len(ev) != 3 || !ev[0].First || ev[0].Repository != "widgets" {
		t.Fatalf("first listing: %+v", ev)
	}
	i := h.sup.st.Index["widgets"]["#4"]
	if i == nil || i.Title != "Slow page" || i.Milestone != "m1" || i.Author != "ann" || i.Labels[0] != "perf" || i.Quiet.IsZero() {
		t.Fatalf("fold: %+v", i)
	}
	h.ticks(2, time.Minute)
	if n := len(h.indexEvents()); n != 3 {
		t.Fatalf("an unchanged listing logged %d events", n)
	}
	api.set(func() {
		api.gitlab["widgets/issues?state=opened"] = `[{"iid":4,"title":"Slow page, again","labels":["perf"],"milestone":{"title":"m1"},"author":{"username":"ann"},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-05T00:00:00Z"}]`
	})
	h.ticks(2, time.Minute) // gone once a second listing confirms it
	ev = h.indexEvents()[3:]
	if len(ev) != 2 || ev[0].Ref != "#4" || ev[0].First || ev[1].Ref != "#5" || !ev[1].Gone {
		t.Fatalf("change: %+v", ev)
	}
	if _, ok := h.sup.st.Index["widgets"]["#5"]; ok || h.sup.st.Index["widgets"]["#4"].Title != "Slow page, again" {
		t.Fatalf("fold after change: %+v", h.sup.st.Index["widgets"])
	}
	c := indexCounts(h.cfg, h.sup.st, "widgets", nil)
	if c.Open != 1 || c.Requests != 1 {
		t.Fatalf("counts %+v", c)
	}
	h.restart(false)
	if i := h.sup.st.Index["widgets"]["#4"]; i == nil || i.Title != "Slow page, again" || i.Quiet.IsZero() {
		t.Fatalf("replayed: %+v", i)
	}
}

// A listing that fails is logged once, and the index stands.
func TestIndexFailureToldOnce(t *testing.T) {
	h, api := indexHarness(t)
	h.ticks(1, time.Minute)
	api.set(func() { api.gitlabErr = errors.New("glab: 502") })
	h.ticks(3, time.Minute)
	n := 0
	for _, e := range h.events() {
		if e.Kind == KindError && strings.Contains(e.Error.Step, "open issues") {
			n++
		}
	}
	if n != 2 { // widgets and readonly, once each
		t.Fatalf("%d errors told", n)
	}
	if len(h.sup.st.Index["widgets"]) != 3 {
		t.Fatal("a failed listing changed the index")
	}
}

// An idle issue gets its sentence once per change on the forge: one each
// back-filled after the first listing, one call when it changes, none
// again until it changes again.
func TestIdleSentenceOncePerChange(t *testing.T) {
	dir := fakeClaude(t)
	claudeAnswers(t, dir, 0, claudeResult(`{"sentence":"Asks for a faster page; the author added a profile."}`))
	h, api := indexHarness(t)
	h.cfg.Jobs.Sentences.Enabled = true
	h.ticks(3, time.Minute)
	if n := claudeCalls(t, dir); n != 2 {
		t.Fatalf("%d calls back-filling the first listing", n)
	}
	api.set(func() {
		api.gitlab["widgets/issues?state=opened"] = strings.Replace(api.gitlab["widgets/issues?state=opened"], `"updated_at":"2026-10-02T00:00:00Z"}`, `"updated_at":"2026-10-06T00:00:00Z"}`, 1)
	})
	h.ticks(3, time.Minute)
	if n := claudeCalls(t, dir); n != 3 {
		t.Fatalf("%d calls for one change", n)
	}
	s := h.sup.st.sentenceOf("widgets", "#4")
	if s == nil || s.Writer != WriterIdle || !strings.Contains(s.Text, "faster page") || s.For.IsZero() {
		t.Fatalf("sentence %+v", s)
	}
	if in := readFake(t, dir, "in.3"); !strings.Contains(in, `"ref":"#4"`) || !strings.Contains(in, "Slow page") {
		t.Fatalf("input %s", in)
	}
	h.restart(false)
	h.sup.api = api
	h.ticks(3, time.Minute)
	if n := claudeCalls(t, dir); n != 3 {
		t.Fatalf("%d calls with no new change", n)
	}
}

// idleCalls counts the idle sentences logged, written or failed, for ref
// or for any.
func (h *harness) idleCalls(ref string) int {
	n := 0
	for _, e := range h.events() {
		if e.Kind == KindSentence && e.Sentence.Writer == WriterIdle && (ref == "" || e.Sentence.Ref == ref) {
			n++
		}
	}
	return n
}

// A failed idle call is logged, and tried again only after an hour: #4's
// for its change and #5's back-fill; #4's back-fill, which failed before
// it changed, is not, as its change's call stands for it.
func TestIdleSentenceFailureRetried(t *testing.T) {
	dir := fakeClaude(t)
	claudeAnswers(t, dir, 0, "not json")
	h, api := indexHarness(t)
	h.cfg.Jobs.Sentences.Enabled = true
	h.ticks(1, time.Minute)
	api.set(func() {
		api.gitlab["widgets/issues?state=opened"] = strings.Replace(api.gitlab["widgets/issues?state=opened"], `"updated_at":"2026-10-02T00:00:00Z"}`, `"updated_at":"2026-10-06T00:00:00Z"}`, 1)
	})
	h.ticks(5, time.Minute)
	if n := h.idleCalls(""); n != 3 {
		t.Fatalf("%d calls", n)
	}
	h.ticks(60, time.Minute)
	if n := h.idleCalls(""); n != 5 {
		t.Fatalf("%d calls after an hour", n)
	}
}

// The status check's summary becomes the sentence of the issue its
// session works, once per new summary; the loop's place becomes its
// request's and its issue's, unless the status check writes the issue's.
func TestStatusAndLoopSentences(t *testing.T) {
	h := newHarness(t, false, "s")
	sess := h.cfg.Sessions["s"]
	sess.Issue = 4
	h.sup.noteStatusSentence("s", sess, &StatusRecord{Summary: "Profiling the page; next the cache."}, "")
	h.sup.noteStatusSentence("s", sess, &StatusRecord{Summary: "Profiling the page; next the cache."}, textHash("Profiling the page; next the cache."))
	q := loopRequest{ciWatched: ciWatched{Repository: "widgets", Ref: "!9"}, Case: "widgets#4"}
	h.sup.noteLoopSentence(q, loopPlace{Phase: PlaceReviewing, Reason: "round 1"})
	h.sup.noteLoopSentence(q, loopPlace{Phase: PlaceReviewing, Reason: "round 1"})
	if s := h.sup.st.sentenceOf("widgets", "#4"); s == nil || s.Writer != WriterStatus || s.Session != "s" {
		t.Fatalf("issue: %+v", s)
	}
	if s := h.sup.st.sentenceOf("widgets", "!9"); s == nil || s.Writer != WriterLoop || s.Text != "!9 is "+PlaceReviewing+": round 1" {
		t.Fatalf("request: %+v", s)
	}
	if n := h.count(KindSentence, "", ""); n != 2 {
		t.Fatalf("%d sentences logged", n)
	}
	h.sup.noteLoopSentence(loopRequest{ciWatched: ciWatched{Repository: "widgets", Ref: "!10"}, Case: "widgets#6"}, loopPlace{Phase: PlaceCI})
	if s := h.sup.st.sentenceOf("widgets", "#6"); s == nil || s.Writer != WriterLoop {
		t.Fatalf("an issue only the loop knows: %+v", s)
	}
}

// Work that names no issue gets one opened only where delivery.allow
// holds issue; elsewhere Go says why it opened none.
func TestIssueForWork(t *testing.T) {
	h := newHarness(t, false)
	w := ThreadWork{Title: "Add a dark mode"}
	h.sup.issueForWork("w1", "widgets", &w)
	if w.Issue != 0 || len(h.forge.issues) != 0 {
		t.Fatalf("opened without allow: %+v %v", w, h.forge.issues)
	}
	found := false
	for _, e := range h.events() {
		if e.Kind == KindSkip && e.Skip.Action == ActIssue && strings.Contains(e.Skip.Reason, "does not allow issue") {
			found = true
		}
	}
	if !found {
		t.Fatal("no reason logged")
	}
	r := h.cfg.Repositories["widgets"]
	r.Policy.Delivery.Allow = append(r.Policy.Delivery.Allow, ActIssue)
	h.cfg.Repositories["widgets"] = r
	h.restart(false)
	h.sup.issueForWork("w2", "widgets", &w)
	if w.Issue != 101 || len(h.forge.issues) != 1 || !strings.HasPrefix(h.forge.issues[0], "Add a dark mode\n") || !strings.Contains(h.forge.issues[0], "w2") {
		t.Fatalf("opened: %+v %v", w, h.forge.issues)
	}
	if strings.Contains(h.forge.issues[0], "/home/") || strings.Contains(h.forge.issues[0], "example.com") {
		t.Fatalf("the body names a path or host: %q", h.forge.issues[0])
	}
	ended := ThreadWork{Title: "Close it", DoneWhen: EndCheck{Kind: "forge", Ref: "#8", State: "closed"}}
	h.sup.issueForWork("w4", "widgets", &ended)
	if ended.Issue != 8 || len(h.forge.issues) != 1 {
		t.Fatalf("work ending with #8's close opened another: %+v", ended)
	}
	named := ThreadWork{Title: "Fix it", Issue: 3}
	h.sup.issueForWork("w3", "widgets", &named)
	if named.Issue != 3 || len(h.forge.issues) != 1 {
		t.Fatal("work naming an issue opened another")
	}
}

// What belongs to an issue is joined by its number: items, sessions, and
// the requests whose title or branch name it.
func TestJoinIssue(t *testing.T) {
	h, _ := indexHarness(t)
	h.ticks(1, time.Minute)
	h.sup.track("s").State = Working
	h.sup.st.Work = map[string]*WorkItem{"w1": {ID: "w1", Repository: "widgets", ThreadWork: ThreadWork{Issue: 4}, State: WorkWorking, Session: "s"},
		"w2": {ID: "w2", Repository: "widgets", ThreadWork: ThreadWork{DoneWhen: EndCheck{Kind: "forge", Ref: "#5", State: "closed"}}}}
	j := joinIssue(h.cfg, h.sup.st, "widgets", 4)
	if len(j.Work) != 1 || j.Work[0].ID != "w1" || len(j.Requests) != 1 || j.Requests[0] != "!9" {
		t.Fatalf("#4: %+v", j)
	}
	if j := joinIssue(h.cfg, h.sup.st, "widgets", 5); len(j.Work) != 1 || len(j.Requests) != 0 {
		t.Fatalf("#5: %+v", j)
	}
	if !branchNumber(12).MatchString("fix/12-crash") || branchNumber(12).MatchString("fix/120") || !titleNumber(4).MatchString("Speed up #4") || titleNumber(4).MatchString("#45") {
		t.Fatal("number matching")
	}
	if c := indexCounts(h.cfg, h.sup.st, "widgets", func(ref string) bool { return ref == "#5" }); c.Working != 1 || c.Waiting != 1 || c.Open != 2 {
		t.Fatalf("counts %+v", c)
	}
}

// A failed idle call followed by one that works settles: no call again
// until the issue changes. The first call, #4's back-fill, fails; the
// calls for #4's change and #5's back-fill work, and none comes after.
func TestIdleSentenceSettlesAfterARetry(t *testing.T) {
	dir := fakeClaude(t)
	claudeAnswers(t, dir, 1, "not json")
	claudeAnswers(t, dir, 0, claudeResult(`{"sentence":"Asks for a faster page."}`))
	h, api := indexHarness(t)
	h.cfg.Jobs.Sentences.Enabled = true
	h.ticks(1, time.Minute)
	api.set(func() {
		api.gitlab["widgets/issues?state=opened"] = strings.Replace(api.gitlab["widgets/issues?state=opened"], `"updated_at":"2026-10-02T00:00:00Z"}`, `"updated_at":"2026-10-06T00:00:00Z"}`, 1)
	})
	h.ticks(62, time.Minute)
	h.ticks(10, time.Minute)
	if n := h.idleCalls(""); n != 3 {
		t.Fatalf("%d calls", n)
	}
	for _, ref := range []string{"#4", "#5"} {
		if s := h.sup.st.sentenceOf("widgets", ref); s == nil || s.Text != "Asks for a faster page." {
			t.Fatalf("%s sentence %+v", ref, s)
		}
	}
}

// An issue whose words match a confidential pattern is logged once,
// withheld, its label, milestone and author too; a withheld sentence is
// not logged again for the same text.
func TestIndexWithheld(t *testing.T) {
	h, _ := indexHarness(t)
	h.cfg.ConfidentialPatterns = []string{"(?i)crash on"}
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	api := &fakeForgeAPI{gitlab: map[string]string{
		"widgets/issues?state=opened": `[{"iid":5,"title":"Crash on save","labels":["bug"],"milestone":{"title":"m"},"author":{"username":"cy"},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-02T00:00:00Z"}]`,
	}}
	h.sup.api = api
	h.ticks(4, time.Minute)
	ev := h.indexEvents()
	if len(ev) != 1 || ev[0].Title != "" || ev[0].Labels != nil || ev[0].Milestone != "" || ev[0].Author != "" || ev[0].Withheld == 0 {
		t.Fatalf("withheld issue logged %+v", ev)
	}
	q := loopRequest{ciWatched: ciWatched{Repository: "widgets", Ref: "!9"}, Case: "widgets#5"}
	for k := 0; k < 3; k++ {
		h.sup.noteLoopSentence(q, loopPlace{Phase: PlaceOwner, Reason: "crash on the judge"})
	}
	if n := h.count(KindSentence, "", ""); n != 2 { // the request and its issue, once each
		t.Fatalf("%d withheld sentences logged", n)
	}
}

// gitlabIssues is a GitLab issues page: idle issues from..to, changed at
// changed.
func gitlabIssues(from, to int, changed string) string {
	var parts []string
	for n := from; n <= to; n++ {
		parts = append(parts, fmt.Sprintf(`{"iid":%d,"title":"Issue %d","labels":["bug"],"author":{"username":"author%d"},"created_at":"2026-10-01T00:00:00Z","updated_at":"%s"}`, n, n, n%3, changed))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// A forge with 400 idle issues gets 300 sentences on the first back-fill
// run, spread over the poll interval at most backfillAtOnce at a time,
// with the loop coming back sooner for them, and the other 100 on the
// second; then an issue that changes gets exactly one new sentence. A
// page rendered while the back-fill runs calls no model and says how far
// it is.
func TestBackfillIdleSentences(t *testing.T) {
	dir := fakeClaude(t)
	claudeAnswers(t, dir, 0, claudeResult(`{"sentence":"Asks for a fix; nobody has answered."}`))
	h, api := indexHarness(t)
	h.cfg.Jobs.Sentences.Enabled = true
	h.cfg.Poll.Every = Duration{10 * time.Minute}
	api.set(func() {
		delete(api.gitlab, "widgets/issues?state=opened")
		for p := 0; p < 4; p++ {
			api.gitlab[fmt.Sprintf("widgets/issues?state=opened|&page=%d", p+1)] = gitlabIssues(p*100+1, p*100+100, "2026-10-02T00:00:00Z")
		}
		api.gitlab["widgets/issues?state=opened|&page=5"] = `[]`
	})
	tick := 20 * time.Second
	h.ticks(1, tick) // the listing
	if n := len(h.sup.st.Index["widgets"]); n != 401 {
		t.Fatalf("%d listed", n)
	}
	if p := backfillProgress(h.cfg, h.sup.st, "widgets"); p == nil || p.Done != 0 || p.Of != 400 {
		t.Fatalf("progress %v before the back-fill", p)
	}
	// A run starts; each pass starts at most backfillAtOnce, and the loop
	// waits less than poll_interval for the next.
	last := 0
	for k := 0; k < 15; k++ {
		h.ticks(1, tick)
		n := h.idleCalls("")
		if n-last > backfillAtOnce {
			t.Fatalf("pass %d started %d", k, n-last)
		}
		last = n
		if w := h.sup.passWait(); w != BackfillPoll {
			t.Fatalf("pass %d: next pass in %s", k, w)
		}
	}
	mid := h.idleCalls("")
	if mid < 100 || mid >= 300 {
		t.Fatalf("%d sentences half-way through the first run", mid)
	}

	src := newUISource("live", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a")
	u, _ := testUI(t, UIConfig{}, src)
	before := claudeCalls(t, dir)
	want := fmt.Sprintf("sentences being written, %d of 400 done", mid)
	for _, target := range []string{"/", "/repo/widgets", "/repo/widgets/issue/7"} {
		if body := owner(t, u, target); target != "/repo/widgets/issue/7" && !strings.Contains(strings.ToLower(body), want) {
			t.Fatalf("%s does not say %q", target, want)
		}
	}
	if n := claudeCalls(t, dir); n != before {
		t.Fatalf("%d model calls from page loads", n-before)
	}

	h.ticks(16, tick) // the first run's interval is over
	if n := h.idleCalls(""); n != 300 {
		t.Fatalf("%d sentences after the first run", n)
	}
	h.ticks(31, tick) // the second run
	if n := h.idleCalls(""); n != 400 {
		t.Fatalf("%d sentences after the second run", n)
	}
	if p := backfillProgress(h.cfg, h.sup.st, "widgets"); p != nil {
		t.Fatalf("progress %v after the back-fill", p)
	}
	if w := h.sup.passWait(); w != h.cfg.PollInterval.Duration {
		t.Fatalf("next pass in %s after the back-fill", w)
	}
	h.ticks(30, tick)
	if n := h.idleCalls(""); n != 400 {
		t.Fatalf("%d sentences with nothing changed", n)
	}

	api.set(func() {
		k := "widgets/issues?state=opened|&page=2"
		api.gitlab[k] = strings.Replace(api.gitlab[k], `"iid":150,"title":"Issue 150","labels":["bug"],"author":{"username":"author0"},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-02T00:00:00Z"`,
			`"iid":150,"title":"Issue 150","labels":["bug"],"author":{"username":"author0"},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-06T00:00:00Z"`, 1)
	})
	h.ticks(60, tick)
	if n, one := h.idleCalls(""), h.idleCalls("#150"); n != 401 || one != 2 {
		t.Fatalf("%d sentences, %d for #150, after #150 changed", n, one)
	}
	if s := h.sup.st.sentenceOf("widgets", "#150"); s == nil || s.Writer != WriterIdle || !s.For.Equal(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("#150's sentence %+v", s)
	}
}

// A back-filled sentence that is withheld is not written again, and the
// issue counts out of the progress, so it does not stand for ever.
func TestBackfillWithheldSettles(t *testing.T) {
	dir := fakeClaude(t)
	claudeAnswers(t, dir, 0, claudeResult(`{"sentence":"Asks about the secret plan."}`))
	h, api := indexHarness(t)
	h.cfg.Jobs.Sentences.Enabled = true
	h.cfg.ConfidentialPatterns = []string{"(?i)secret plan"}
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	h.sup.api = api
	h.ticks(130, time.Minute)
	if n := h.idleCalls(""); n != 2 {
		t.Fatalf("%d calls for two withheld sentences", n)
	}
	if p := backfillProgress(h.cfg, h.sup.st, "widgets"); p != nil {
		t.Fatalf("progress %v with every sentence withheld", p)
	}
}

// An issue Go opened for a work item is remembered: the item asks again
// and gets the same number, from the log, after a restart too. When agentd
// stops between the forge's open and the log's record of it, the next try
// finds the issue by the line its body carries and opens no second one.
func TestIssueForWorkOpensOnce(t *testing.T) {
	h, api := indexHarness(t)
	r := h.cfg.Repositories["widgets"]
	r.Policy.Delivery.Allow = append(r.Policy.Delivery.Allow, ActIssue)
	h.cfg.Repositories["widgets"] = r
	h.restart(false)
	h.sup.api = api
	w := ThreadWork{Title: "Add a dark mode"}
	h.sup.issueForWork("w1", "widgets", &w)
	again := ThreadWork{Title: "Add a dark mode"}
	h.sup.issueForWork("w1", "widgets", &again)
	h.restart(false)
	h.sup.api = api
	after := ThreadWork{Title: "Add a dark mode"}
	h.sup.issueForWork("w1", "widgets", &after)
	if len(h.forge.issues) != 1 || w.Issue != 101 || again.Issue != 101 || after.Issue != 101 {
		t.Fatalf("opened %d: %d %d %d", len(h.forge.issues), w.Issue, again.Issue, after.Issue)
	}

	// w2's open reaches the forge; the record of it does not reach the log.
	h.sup.issueForWork("w2", "widgets", &ThreadWork{Title: "Fix the crash"})
	lines := strings.SplitAfter(h.buf.String(), "\n")
	cut := ""
	for _, l := range lines {
		if strings.Contains(l, `"kind":"action"`) && strings.Contains(l, `"issue:w2"`) {
			continue
		}
		cut += l
	}
	h.buf.Reset()
	h.buf.WriteString(cut)
	body := strings.SplitN(h.forge.issues[1], "\n", 2)[1]
	api.set(func() {
		api.gitlab["widgets/issues?state=all"] = `[{"iid":102,"title":"Fix the crash","description":` + strconv.Quote(body) + `}]`
	})
	h.restart(false)
	h.sup.api = api
	h.sup.surfaceCutShort()
	found := ThreadWork{Title: "Fix the crash"}
	h.sup.issueForWork("w2", "widgets", &found)
	if len(h.forge.issues) != 2 || found.Issue != 102 {
		t.Fatalf("after the cut: opened %d, issue %d", len(h.forge.issues), found.Issue)
	}
	h.restart(false)
	h.sup.api = api
	again = ThreadWork{Title: "Fix the crash"}
	if h.sup.issueForWork("w2", "widgets", &again); again.Issue != 102 || len(h.forge.issues) != 2 {
		t.Fatalf("the found issue is not remembered: %d, opened %d", again.Issue, len(h.forge.issues))
	}

	// One cut short whose issue the forge does not show is opened again,
	// once; a listing that fails opens none.
	h.sup.emit("", Event{Kind: KindIntent, Act: &ActPayload{Action: ActIssue, Key: "issue:w3", Repository: "widgets", Message: "Speed it up", Intent: "i3"}})
	h.restart(false)
	h.sup.api = api
	h.sup.surfaceCutShort()
	api.set(func() { api.gitlabErr = errors.New("the forge is down") })
	none := ThreadWork{Title: "Speed it up"}
	if h.sup.issueForWork("w3", "widgets", &none); none.Issue != 0 || len(h.forge.issues) != 2 {
		t.Fatalf("opened with the listing failing: %d", len(h.forge.issues))
	}
	api.set(func() { api.gitlabErr = nil })
	if h.sup.issueForWork("w3", "widgets", &none); none.Issue != 103 || len(h.forge.issues) != 3 {
		t.Fatalf("not found and not opened: %d, %d", none.Issue, len(h.forge.issues))
	}
}

// On GitHub the issue is found by its body among issues and requests; a
// listing too long to look through to its end finds none and is an error.
func TestFindOpenedIssue(t *testing.T) {
	r := Repository{Forge: "github", Project: "example-org/widgets"}
	line := fmt.Sprintf(issueBodyLine, "w7")
	api := &fakeForgeAPI{github: map[string]string{
		fmt.Sprintf("repos/example-org/widgets/issues?state=all&sort=created&direction=desc&per_page=%d&page=1", indexPerPage): `[{"number":12,"body":null,"pull_request":{}},{"number":11,"body":"x\n` + line + `"}]`,
	}}
	if n, err := findOpenedIssue(api, r, line); err != nil || n != 11 {
		t.Fatalf("found %d, %v", n, err)
	}
	full := "[" + strings.TrimSuffix(strings.Repeat(`{"number":1,"body":""},`, indexPerPage), ",") + "]"
	for p := 1; p <= indexPages; p++ {
		api.github[fmt.Sprintf("repos/example-org/widgets/issues?state=all&sort=created&direction=desc&per_page=%d&page=%d", indexPerPage, p)] = full
	}
	if n, err := findOpenedIssue(api, r, line); err == nil || n != 0 {
		t.Fatalf("a listing past its end: %d, %v", n, err)
	}
}

// A sentence withheld for an issue's change is not asked for again until
// the issue changes on the forge: over three hours, one call. A failed one
// is tried again with a growing wait, three times at most for one change,
// and the fold says why the issue has none.
func TestIdleSentenceWithheldAndFailedAreBounded(t *testing.T) {
	dir := fakeClaude(t)
	claudeAnswers(t, dir, 0, claudeResult(`{"sentence":"Asks about the secret plan."}`))
	h, api := indexHarness(t)
	h.cfg.Jobs.Sentences.Enabled = true
	h.cfg.ConfidentialPatterns = []string{"(?i)secret plan"}
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	h.sup.api = api
	h.ticks(1, time.Minute)
	api.set(func() {
		api.gitlab["widgets/issues?state=opened"] = strings.Replace(api.gitlab["widgets/issues?state=opened"], `"updated_at":"2026-10-02T00:00:00Z"}`, `"updated_at":"2026-10-06T00:00:00Z"}`, 1)
	})
	h.ticks(180, time.Minute)
	if n := h.idleCallsFor("#4", "2026-10-06T00:00:00Z"); n != 1 {
		t.Fatalf("%d calls for one withheld change over three hours", n)
	}
	if page := h.issuePage(4); !strings.Contains(page, "was withheld") {
		t.Fatalf("the page does not say the sentence was withheld:\n%s", page)
	}

	claudeAnswers(t, dir, 0, "not json")
	api.set(func() {
		api.gitlab["widgets/issues?state=opened"] = strings.Replace(api.gitlab["widgets/issues?state=opened"], `"updated_at":"2026-10-06T00:00:00Z"}`, `"updated_at":"2026-10-07T00:00:00Z"}`, 1)
	})
	h.ticks(24*60, time.Minute)
	if n := h.idleCallsFor("#4", "2026-10-07T00:00:00Z"); n != 3 {
		t.Fatalf("%d calls for one failing change over a day", n)
	}
	if page := h.issuePage(4); !strings.Contains(page, "could not be written") {
		t.Fatalf("the page does not say the sentence could not be written:\n%s", page)
	}
}

// idleCallsFor counts the idle sentences logged for ref's change at
// changed.
func (h *harness) idleCallsFor(ref, changed string) int {
	at, _ := time.Parse(time.RFC3339, changed)
	n := 0
	for _, e := range h.events() {
		if e.Kind == KindSentence && e.Sentence.Writer == WriterIdle && e.Sentence.Ref == ref && e.Sentence.For.Equal(at) {
			n++
		}
	}
	return n
}

// issuePage is widgets' issue n as the command line prints it.
func (h *harness) issuePage(n int) string {
	var b strings.Builder
	now := h.clock.Now()
	printIssueLevel(&b, uiPage{Now: now, loc: time.UTC}, BuildIssueLevel(h.cfg, h.sup.st, "widgets", n, nil, false, now))
	return b.String()
}

// A closed issue keeps its steps and sentence: its page says it is
// closed, today or on its date, and a request gone from the list keeps
// its steps on its issue's page. One listing that misses an issue, as a
// page shifted by a close between two pages may, closes nothing; a second
// confirms it. What is closed is dropped after closedKept.
func TestClosedIssueKeepsItsHistory(t *testing.T) {
	h, api := indexHarness(t)
	h.ticks(1, time.Minute)
	h.sup.noteLoopSentence(loopRequest{ciWatched: ciWatched{Repository: "widgets", Ref: "!9"}, Case: "widgets#4"}, loopPlace{Phase: PlaceReviewing, Reason: "round 1"})
	listed := api.gitlab["widgets/issues?state=opened"]
	api.set(func() {
		api.gitlab["widgets/issues?state=opened"] = `[{"iid":4,"title":"Slow page, again","labels":["perf"],"author":{"username":"ann"},"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-02T00:00:00Z"}]`
		api.gitlab["widgets/merge_requests?state=opened"] = `[]`
	})
	h.ticks(1, time.Minute)
	if _, ok := h.sup.st.Index["widgets"]["#5"]; !ok {
		t.Fatal("one listing that missed #5 closed it")
	}
	h.ticks(1, time.Minute)
	if _, ok := h.sup.st.Index["widgets"]["#5"]; ok {
		t.Fatal("#5 is open after two listings missed it")
	}
	page := h.issuePage(5)
	if !strings.Contains(page, "#5 Crash on save (closed today)") {
		t.Fatalf("the closed issue's page:\n%s", page)
	}
	if page := h.issuePage(4); !strings.Contains(page, "round 1") {
		t.Fatalf("#4's page lost its merged request's steps:\n%s", page)
	}
	h.clock.advance(3 * 24 * time.Hour)
	if page := h.issuePage(5); !strings.Contains(page, "(closed Oct 22)") {
		t.Fatalf("the closed issue's page, days later:\n%s", page)
	}
	h.restart(false)
	h.sup.api = api
	if page := h.issuePage(5); !strings.Contains(page, "closed") {
		t.Fatalf("replayed:\n%s", page)
	}
	// Reopened, it is open again with what it had.
	api.set(func() { api.gitlab["widgets/issues?state=opened"] = listed })
	h.ticks(1, time.Minute)
	if i := h.sup.st.Index["widgets"]["#5"]; i == nil {
		t.Fatal("the reopened issue is not open")
	}
	api.set(func() { api.gitlab["widgets/issues?state=opened"] = `[]` })
	h.ticks(2, time.Minute)
	h.clock.advance(31 * 24 * time.Hour)
	api.set(func() {
		api.gitlab["widgets/issues?state=opened"] = `[{"iid":6,"title":"New","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-02T00:00:00Z"}]`
	})
	h.ticks(1, time.Minute)
	if page := h.issuePage(5); strings.Contains(page, "closed") || len(h.sup.st.Steps[sentenceKey("widgets", "#5")]) != 0 {
		t.Fatalf("kept past its time:\n%s", page)
	}
}

// An issue is being worked only when a session works it now: a live
// session whose issue it is, or one holding an item on it that is
// assigned or under way. An item being designed, or one whose session is
// gone, does not count, nor does a session that is done.
func TestBeingWorkedMeansASession(t *testing.T) {
	h, _ := indexHarness(t)
	h.ticks(1, time.Minute)
	h.sup.st.Work = map[string]*WorkItem{
		"w1": {ID: "w1", Repository: "widgets", ThreadWork: ThreadWork{Issue: 4}, State: WorkDesigned},
		"w2": {ID: "w2", Repository: "widgets", ThreadWork: ThreadWork{Issue: 5}, State: WorkWorking, Session: "gone"},
	}
	if w := workingOn(h.cfg, h.sup.st, "widgets"); len(w) != 0 {
		t.Fatalf("worked with no session: %v", w)
	}
	h.cfg.Sessions["s"] = Session{Repository: "widgets", Issue: 4, Workdir: h.dir, Cwd: h.dir}
	if w := workingOn(h.cfg, h.sup.st, "widgets"); len(w) != 0 {
		t.Fatalf("worked by a session never seen: %v", w)
	}
	h.sup.track("s").State = Working
	h.sup.st.Work["w2"].Session = "s"
	if w := workingOn(h.cfg, h.sup.st, "widgets"); !w["#4"] || !w["#5"] {
		t.Fatalf("a live session's issue and item: %v", w)
	}
	h.sup.track("s").State = Done
	if w := workingOn(h.cfg, h.sup.st, "widgets"); len(w) != 0 {
		t.Fatalf("worked by a session that is done: %v", w)
	}
}
