package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// appendEvent logs e on the harness's log, as another process would.
func (h *harness) appendEvent(e Event) {
	h.t.Helper()
	e.V = EventVersion
	if e.Time.IsZero() {
		e.Time = h.clock.Now()
	}
	if err := NewLogWriter(h.buf).Append(e); err != nil {
		h.t.Fatal(err)
	}
}

func ruling(id, text string) Event {
	return Event{Kind: KindCase, Mode: ModeOwner, Case: &CasePayload{Case: widgetsCase, Ruling: &CaseRuling{Kind: RulingRestriction, ID: id, Text: text}}}
}

// storeOf is the harness's log folded into a context store.
func storeOf(h *harness) *ContextStore {
	h.t.Helper()
	cs := NewContextStore(h.cfg)
	if err := cs.Follow(NewLogWriter(h.buf)); err != nil {
		h.t.Fatal(err)
	}
	return cs
}

// History is drawn from the log: each record in its repository, on the
// topics its fields and text name, with an id a rebuild gives again.
func TestHistoryFromLog(t *testing.T) {
	h := caseHarness(t)
	exit := 1
	h.appendEvent(ruling("R1", "never push to main"))
	h.appendEvent(Event{Kind: KindTurn, Mode: ModeLive, Session: "s1", Turn: &TurnPayload{Transcript: "x", Line: 4, Role: TurnTool, Text: "Read " + h.cfg.Sessions["s1"].Cwd + "/ui/view.go", Failed: true, Exit: &exit}})
	h.appendEvent(Event{Kind: KindTurn, Mode: ModeLive, Session: "s1", Turn: &TurnPayload{Transcript: "x", Line: 5, Role: TurnSummary, Text: "fixed spin.go; see !12"}})
	h.appendEvent(Event{Kind: KindCI, Mode: ModeLive, Session: "s1", CI: &CIPayload{Repository: "widgets", Ref: "!12", State: "failed", Cause: "test", Excerpt: "FAIL TestSpin"}})
	h.appendEvent(Event{Kind: KindInstruction, Mode: ModeOwner, Instruction: &InstructionPayload{ID: "I4c94", Repositories: []string{"widgets"}, Text: "replies are routed, see #7", From: "owner"}})
	cs := storeOf(h)
	hist := cs.History("widgets")
	if hist.Len() != 5 {
		t.Fatalf("%d records, want 5: %+v", hist.Len(), hist.records)
	}
	on7 := hist.OnTopic("#7")
	if len(on7) != 5 { // s1 works #7: what it says is on its issue
		t.Fatalf("#7 has %d records", len(on7))
	}
	if r := hist.Record("I4c94"); r == nil || r.Kind != HistInstruction {
		t.Fatalf("the owner's instruction keeps its id: %+v", r)
	}
	ci := hist.OnTopic("!12")
	if len(ci) != 2 || ci[1].Kind != HistCI || !strings.Contains(ci[1].Text, "FAIL TestSpin") {
		t.Fatalf("!12: %+v", ci)
	}
	if len(hist.OnTopic("area:ui")) != 1 || len(hist.OnTopic("area:spin")) != 1 || len(hist.OnTopic("session:s1")) != 3 {
		t.Fatalf("topics: %v", hist.Topics())
	}
	again := storeOf(h).History("widgets")
	for _, r := range hist.records {
		if again.Record(r.ID) == nil {
			t.Fatalf("a rebuild lost %s", r.ID)
		}
	}
}

// A history answer never exceeds its budget, on a repository of 10,000
// records, and its pages reach every record exactly once.
func TestHistoryBudget10k(t *testing.T) {
	hist := newHistory("widgets")
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10000; i++ {
		text := strings.Repeat(fmt.Sprintf("line %d of a long tool output, ", i), 1+i%97)
		hist.add(&HistRecord{ID: fmt.Sprintf("T%05d", i), Kind: HistReply, At: base.Add(time.Duration(i/3) * time.Second), Text: text,
			Topics: []string{"#56", fmt.Sprintf("#%d", 100+i%50)}, Source: "test"})
	}
	for _, budget := range []int{0, 1, 200, 1000, 4000, 32000, 1 << 20} {
		out := hist.ByTopic(HistoryQuery{Topic: "#56", Budget: budget})
		if n := len(compact(out)); n > budgetBytes(budgetOf(budget)) {
			t.Fatalf("budget %d: %d bytes, over %d", budget, n, budgetBytes(budgetOf(budget)))
		}
	}
	seen := map[string]bool{}
	more := ""
	for pages := 0; ; pages++ {
		if pages > 10000 {
			t.Fatal("pages never end")
		}
		out := hist.ByTopic(HistoryQuery{Topic: "#56", Budget: 4000, More: more})
		if n := len(compact(out)); n > budgetBytes(4000) {
			t.Fatalf("page %d: %d bytes", pages, n)
		}
		recs := out["records"].([]histEntry)
		if len(recs) == 0 {
			t.Fatalf("page %d is empty", pages)
		}
		for _, r := range recs {
			if seen[r.ID] {
				t.Fatalf("%s twice", r.ID)
			}
			seen[r.ID] = true
		}
		m, ok := out["more"].(string)
		if !ok {
			break
		}
		more = m
	}
	if len(seen) != 10000 {
		t.Fatalf("pages reached %d records", len(seen))
	}
	var ids []string
	for i := 9900; i < 10000; i++ {
		ids = append(ids, fmt.Sprintf("T%05d", i))
	}
	for _, budget := range []int{0, 500, 32000} {
		out := hist.ByIDs(append(ids, "nope"), budget)
		if n := len(compact(out)); n > budgetBytes(budgetOf(budget)) {
			t.Fatalf("ids, budget %d: %d bytes", budget, n)
		}
	}
	since := hist.ByTopic(HistoryQuery{Topic: "#56", Since: base.Add(3332 * time.Second), Kinds: []string{HistReply}})
	if since["total"].(int) != 4 {
		t.Fatalf("since: %v", since["total"])
	}
}

func testSources(fresh ...string) cardSources {
	f := map[string]bool{}
	for _, id := range fresh {
		f[id] = true
	}
	return cardSources{known: func(id string) bool { return !strings.HasPrefix(id, "nope") }, fresh: f}
}

// Go checks a curator's diff: a rule in force or an open question leaves
// the card only with a source new since its last revision; sources must
// exist; a conflict becomes an open question and the rule stays.
func TestApplyDiff(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	if _, err := applyDiff(nil, "#7", CardDiff{AddRules: []DiffClaim{{Text: "x", Src: []string{"A1"}}}}, testSources(), now); err == nil || !strings.Contains(err.Error(), "first card") {
		t.Fatalf("a first card without status: %v", err)
	}
	c, err := applyDiff(nil, "#7", CardDiff{Title: "Spin", Status: "open", Summary: "Widgets spin.", Src: []string{"N1"},
		AddRules: []DiffClaim{{Text: "never push to main", Src: []string{"R0a"}}}, AddQuestions: []DiffClaim{{Text: "merge now?", Options: []string{"yes", "no"}, Src: []string{"D1"}}}}, testSources("N1", "R0a", "D1"), now)
	if err != nil || c.Rev != 1 || c.InForce[0].ID != "R1" || c.Open[0].ID != "Q2" {
		t.Fatalf("first card: %+v %v", c, err)
	}
	refused := []struct {
		name string
		d    CardDiff
		want string
	}{
		{"drop a rule, no source", CardDiff{Retire: []DiffRetire{{ID: "R1", Why: "old", Src: []string{"R0a"}}}}, "nothing says why"},
		{"drop a rule, no src at all", CardDiff{Retire: []DiffRetire{{ID: "R1", Why: "old"}}}, "no src"},
		{"drop a question, no source", CardDiff{Retire: []DiffRetire{{ID: "Q2", Why: "answered", Src: []string{"D1"}}}}, "nothing says why"},
		{"change a rule, no source", CardDiff{Change: []DiffClaim{{ID: "R1", Text: "push to main", Src: []string{"R0a"}}}}, "nothing says why"},
		{"unknown source", CardDiff{AddProblems: []DiffClaim{{Text: "slow", Src: []string{"nope1"}}}}, "does not hold"},
		{"unknown claim", CardDiff{Retire: []DiffRetire{{ID: "R9", Why: "x", Src: []string{"Z1"}}}}, "does not hold"},
		{"bad confidence", CardDiff{AddProblems: []DiffClaim{{Text: "slow", Src: []string{"Z1"}, Confidence: "high"}}}, "confidence"},
		{"too large", CardDiff{AddProblems: func() []DiffClaim {
			var out []DiffClaim
			for i := 0; i < 20; i++ {
				out = append(out, DiffClaim{Text: strings.Repeat("y", 300), Src: []string{"Z1"}})
			}
			return out
		}()}, "over 4000"},
	}
	for _, tc := range refused {
		if _, err := applyDiff(c, "#7", tc.d, testSources("Z1"), now); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	next, err := applyDiff(c, "#7", CardDiff{Retire: []DiffRetire{{ID: "R1", Why: "the owner lifted it", Src: []string{"I9"}}, {ID: "Q2", Why: "answered yes", Src: []string{"A3"}}}}, testSources("I9", "A3"), now)
	if err != nil || len(next.InForce) != 0 || len(next.Open) != 0 || next.Rev != 2 {
		t.Fatalf("retired with sources: %+v %v", next, err)
	}
	if len(c.InForce) != 1 {
		t.Fatal("applying a diff changed the card it was given")
	}
	con, err := applyDiff(c, "#7", CardDiff{Conflicts: []DiffConflict{{ID: "R1", Question: "R1 says never push to main; ruling R0b says push hotfixes to main. Which holds?", Options: []string{"R1", "R0b"}, Src: []string{"R0b"}}}}, testSources("R0b"), now)
	if err != nil || len(con.InForce) != 1 || len(con.Open) != 2 || con.Open[1].ID != "Q3" || !contains(con.Open[1].Src, "R0a") || !contains(con.Open[1].Src, "R0b") {
		t.Fatalf("conflict: %+v %v", con, err)
	}
}

// curatorHarness is the case harness with curators on and a fake model
// that answers each topic's curator on its own, since curators run at
// once: each call appends its topic's key (#7 is _7) to calls, saves its
// input as in.KEY, and prints out.KEY, or out.
func curatorHarness(t *testing.T) (*harness, string) {
	h := caseHarness(t)
	dir := t.TempDir()
	t.Setenv("FAKE_CLAUDE", dir)
	shim(t, "claude", `d=$FAKE_CLAUDE
f=$(mktemp "$d/tmp.XXXXXX"); cat > "$f"
k=$(sed -n 's/^{"topic":"\([^"]*\)".*/\1/p' "$f" | tr -c 'A-Za-z0-9\n' _)
echo "$k" >> "$d/calls"
mv "$f" "$d/in.$k"
if [ -f "$d/out.$k" ]; then cat "$d/out.$k"; else cat "$d/out"; fi`)
	h.cfg.Jobs.Curator.Enabled = true
	claudeAnswers(t, dir, 0, claudeResult(`{}`))
	return h, dir
}

// curatorAnswers sets what the fake answers key's curator.
func curatorAnswers(t *testing.T, dir, key, diff string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "out."+key), claudeResult(diff))
}

// curatorCalls are the keys of the fake's calls, in order.
func curatorCalls(t *testing.T, dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, "calls"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// withoutSessions is the curator calls that are not of a session's card.
func withoutSessions(calls []string) []string {
	var out []string
	for _, c := range calls {
		if !strings.HasPrefix(c, "session_") {
			out = append(out, c)
		}
	}
	return out
}

func countOf(list []string, s string) int {
	n := 0
	for _, x := range list {
		if x == s {
			n++
		}
	}
	return n
}

func cardEvents(h *harness) []*CardPayload {
	var out []*CardPayload
	for _, e := range h.events() {
		if e.Card != nil {
			out = append(out, e.Card)
		}
	}
	return out
}

func onTopic(h *harness, topic, word string) string {
	for _, r := range storeOf(h).History("widgets").OnTopic(topic) {
		if strings.Contains(r.Text, word) {
			return r.ID
		}
	}
	h.t.Fatalf("no record on %s says %q", topic, word)
	return ""
}

// Two rulings that disagree become an open question for the owner, end
// to end: the curator is called once the topic is quiet, with only the
// new records, and its conflict lands on the card as a question.
func TestCuratorConflictingRulings(t *testing.T) {
	h, dir := curatorHarness(t)
	h.appendEvent(ruling("R1", "never push to main"))
	r1 := onTopic(h, "#7", "never push")
	curatorAnswers(t, dir, "_7", fmt.Sprintf(`{"title":"Spin","status":"in progress","summary":"Widgets spin.","src":[%q],"add_rules":[{"text":"never push to main","src":[%q]}]}`, r1, r1))
	curatorAnswers(t, dir, "repo", `{"status":"one issue in progress","summary":"#7 is in progress.","src":["card:#7"]}`)
	h.ticks(1, time.Minute)
	if n := len(curatorCalls(t, dir)); n != 0 {
		t.Fatalf("%d calls before the topic was quiet", n)
	}
	h.ticks(4, time.Minute)
	// The session's card is curated too, its start a fact (#62).
	if calls := withoutSessions(curatorCalls(t, dir)); fmt.Sprint(calls) != "[_7 repo]" {
		t.Fatalf("calls %v, want #7's, then the repository's", calls)
	}
	if in := readFake(t, dir, "in._7"); !strings.Contains(in, r1) || !strings.Contains(in, `"card":null`) {
		t.Fatalf("first input: %s", in)
	}
	if in := readFake(t, dir, "in.repo"); !strings.Contains(in, `"id":"card:#7"`) {
		t.Fatalf("the repository's curator is not given #7's card: %s", in)
	}
	cs := storeOf(h)
	if c := cs.Card("widgets", "#7"); c == nil || c.InForce[0].ID != "R1" {
		t.Fatalf("card: %+v", c)
	}
	if c := cs.Card("widgets", TopicRepo); c == nil || c.Src[0] != "card:#7" {
		t.Fatalf("repository card: %+v", c)
	}
	var p7 *CardPayload
	for _, p := range cardEvents(h) {
		if p.Topic == "#7" && p7 == nil {
			p7 = p
		}
	}
	if p := p7; p == nil || p.Usage == nil || p.Usage.InputTokens != 900 || p.InputBytes == 0 || p.Model != "haiku" {
		t.Fatalf("cost not logged: %+v", p)
	}

	h.clock.advance(15 * time.Minute)
	h.appendEvent(ruling("R2", "push hotfixes straight to main"))
	r2 := onTopic(h, "#7", "hotfixes")
	curatorAnswers(t, dir, "_7", fmt.Sprintf(`{"conflicts":[{"id":"R1","question":"Never push to main, or push hotfixes to main?","options":["never push to main","push hotfixes to main"],"src":[%q]}]}`, r2))
	h.ticks(4, time.Minute)
	if in := readFake(t, dir, "in._7"); strings.Contains(in, `"id":"`+r1+`","kind"`) || !strings.Contains(in, r2) {
		t.Fatalf("the second curation gets only the new record: %s", in)
	}
	c := storeOf(h).Card("widgets", "#7")
	if c == nil || c.Rev != 2 || len(c.InForce) != 1 || len(c.Open) != 1 || !contains(c.Open[0].Src, r1) || !contains(c.Open[0].Src, r2) {
		t.Fatalf("card: %+v", c)
	}
}

// A curator that drops an in-force rule with no source for why is
// refused and logged, the card stays as it was, and the next curator is
// sent the same records again and told why.
func TestCuratorRefusesUnsourcedDrop(t *testing.T) {
	h, dir := curatorHarness(t)
	h.appendEvent(ruling("R1", "never push to main"))
	r1 := onTopic(h, "#7", "never push")
	curatorAnswers(t, dir, "_7", fmt.Sprintf(`{"status":"open","summary":"s","src":[%q],"add_rules":[{"text":"never push to main","src":[%q]}]}`, r1, r1))
	h.ticks(5, time.Minute)
	h.clock.advance(15 * time.Minute)
	h.appendEvent(Event{Kind: KindCI, Mode: ModeLive, Session: "s1", CI: &CIPayload{Repository: "widgets", Ref: "#7", State: "passed"}})
	curatorAnswers(t, dir, "_7", fmt.Sprintf(`{"retire":[{"id":"R1","why":"no longer needed","src":[%q]}]}`, r1))
	h.ticks(4, time.Minute)
	var refused *CardPayload
	for _, p := range cardEvents(h) {
		if p.Topic == "#7" && p.Refused != "" {
			refused = p
		}
	}
	if refused == nil || !strings.Contains(refused.Refused, "retire R1, an in-force rule cites no record new") {
		t.Fatalf("no refusal: %+v", cardEvents(h))
	}
	if c := storeOf(h).Card("widgets", "#7"); c.Rev != 1 || len(c.InForce) != 1 {
		t.Fatalf("card changed: %+v", c)
	}
	h.clock.advance(15 * time.Minute)
	curatorAnswers(t, dir, "_7", `{}`)
	h.ticks(1, time.Minute)
	last := readFake(t, dir, "in._7")
	if !strings.Contains(last, `"refused":"retire R1`) || !strings.Contains(last, `"title":"#7 passed"`) {
		t.Fatalf("the next curator is not told, or not sent the records again: %s", last)
	}
	if c := storeOf(h).Card("widgets", "#7"); c.Rev != 1 {
		t.Fatalf("an empty diff made revision %d", c.Rev)
	}
}

// Curators are off by default and in a shadow run.
func TestCuratorOff(t *testing.T) {
	h := caseHarness(t)
	dir := fakeClaude(t)
	h.appendEvent(ruling("R1", "never push to main"))
	h.ticks(5, time.Minute)
	if claudeCalls(t, dir) != 0 {
		t.Fatal("curated with curators off")
	}
}

// queryServer is a server of the context tools alone, for a thread agent.
func (h *harness) queryServer() *MCPServer {
	h.t.Helper()
	srv, err := NewQueryServer(h.cfg, "widgets", "thread-1", NewLogWriter(h.buf), func() ([]Event, error) { return h.events(), nil }, h.clock.Now)
	if err != nil {
		h.t.Fatal(err)
	}
	return srv
}

func replyJSON(t *testing.T, r rpcReply) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(r.Result.Content[0].Text), &out); err != nil {
		t.Fatalf("reply %q: %v", r.Result.Content[0].Text, err)
	}
	return out
}

func queries(h *harness) []*QueryPayload {
	var out []*QueryPayload
	for _, e := range h.events() {
		if e.Query != nil {
			out = append(out, e.Query)
		}
	}
	return out
}

// An agent reads state first, finds topics, and reads history only with a
// reason; every call is logged with its size, and missing-from-state asks
// the topic's curator to add what was missing.
func TestContextTools(t *testing.T) {
	h, dir := curatorHarness(t)
	srv := h.queryServer()
	rs := session(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if len(rs[0].Result.Tools) != len(contextTools) {
		t.Fatalf("a query server offers %d tools", len(rs[0].Result.Tools))
	}
	h.appendEvent(ruling("R1", "never push to main"))
	rs = session(t, srv, toolCall(1, ToolState, map[string]any{"topic": "#7"}))
	if got := replyJSON(t, rs[0]); got["card"] != nil || got["history"].(float64) != 1 {
		t.Fatalf("state before a card: %v", got)
	}
	rs = session(t, srv, toolCall(2, ToolHistory, map[string]any{"topic": "#7"}))
	if !rs[0].Result.IsError || !strings.Contains(rs[0].Result.Content[0].Text, "why must be") {
		t.Fatalf("history without why: %+v", rs[0])
	}
	rs = session(t, srv, toolCall(3, ToolHistory, map[string]any{"topic": "#7", "why": WhyMissingFromState, "question": "which rules hold on #7?"}))
	got := replyJSON(t, rs[0])
	if recs := got["records"].([]any); len(recs) != 1 {
		t.Fatalf("history: %v", got)
	}
	r1 := storeOf(h).History("widgets").OnTopic("#7")[0].ID
	curatorAnswers(t, dir, "_7", fmt.Sprintf(`{"title":"Spin the widgets","status":"open","summary":"Widgets spin.","src":[%q],"add_rules":[{"text":"never push to main","src":[%q]}]}`, r1, r1))
	h.ticks(4, time.Minute)
	if in := readFake(t, dir, "in._7"); !strings.Contains(in, `"asked":["which rules hold on #7?"]`) {
		t.Fatalf("the curator is not told what was missing: %s", in)
	}
	rs = session(t, srv, toolCall(4, ToolState, map[string]any{"topic": "#7"}),
		toolCall(5, ToolFind, map[string]any{"q": "spin widgets"}),
		toolCall(6, ToolHistory, map[string]any{"ids": []string{r1, "nope"}, "why": WhyVerifyClaim}))
	// A card says how fresh it is: its sources' newest time (#62).
	r1At := storeOf(h).History("widgets").Record(r1).At.UTC().Format(time.RFC3339Nano)
	if got := replyJSON(t, rs[0]); got["sources_at"] != r1At || got["newest_record_at"] == nil || got["card"].(map[string]any)["updated"] == nil {
		t.Fatalf("state does not say how fresh the card is: %v", got)
	}
	if c := replyJSON(t, rs[0])["card"].(map[string]any); c["topic"] != "#7" || c["rev"].(float64) != 1 {
		t.Fatalf("state: %v", c)
	}
	if ts := replyJSON(t, rs[1])["topics"].([]any); len(ts) == 0 || ts[0].(map[string]any)["topic"] != "#7" {
		t.Fatalf("find: %v", ts)
	}
	if got := replyJSON(t, rs[2]); len(got["records"].([]any)) != 1 || got["missing"].([]any)[0] != "nope" {
		t.Fatalf("history by ids: %v", got)
	}
	qs := queries(h)
	if len(qs) != 6 {
		t.Fatalf("%d queries logged, want 6", len(qs))
	}
	if q := qs[3]; q.Tool != ToolState || !q.Hit || q.Bytes == 0 || q.Tokens != tokensOf(q.Bytes) {
		t.Fatalf("state query: %+v", q)
	}
	if q := qs[2]; q.Why != WhyMissingFromState || q.Topic != "#7" || q.Question == "" {
		t.Fatalf("history query: %+v", q)
	}
	for _, e := range h.events() {
		if e.Query != nil && e.Session != "thread-1" {
			t.Fatalf("query logged as %q", e.Session)
		}
	}
}

// history on an issue whose thread agentd has not read reads it from the
// forge, logs it screened, and serves its comments as records; a second
// call within threadAge reads nothing.
func TestHistoryReadsThread(t *testing.T) {
	h := caseHarness(t)
	h.cfg.ConfidentialPatterns = []string{`secret-[0-9]+`}
	api := &fakeForgeAPI{gitlab: map[string]string{
		"=/projects/example-org%2Fwidgets/issues/7":                                                  `{"title":"Spin","description":"Make the widgets spin.","state":"opened","labels":["bug"],"author":{"username":"ann"}}`,
		"=/projects/example-org%2Fwidgets/issues/7/notes?sort=desc&order_by=created_at&per_page=100": `[{"id":11,"body":"why not !12?","created_at":"2026-10-27T07:00:00Z","author":{"username":"bob"}},{"id":12,"body":"secret-42","created_at":"2026-10-27T07:01:00Z","author":{"username":"bob"}},{"id":13,"body":"assigned","system":true,"created_at":"2026-10-27T07:02:00Z"}]`,
	}}
	srv := h.queryServer()
	srv.api = api
	rs := session(t, srv, toolCall(1, ToolHistory, map[string]any{"topic": "#7", "why": WhySpec}))
	got := replyJSON(t, rs[0])
	if n := len(got["records"].([]any)); n != 3 {
		t.Fatalf("%d records: %v", n, got)
	}
	text := rs[0].Result.Content[0].Text
	if strings.Contains(text, "secret-42") || !strings.Contains(text, "withheld") || !strings.Contains(text, "C11") || strings.Contains(text, "assigned") {
		t.Fatalf("thread: %s", text)
	}
	cs := storeOf(h)
	if len(cs.History("widgets").OnTopic("!12")) != 1 || len(cs.History("widgets").OnTopic("label:bug")) != 1 {
		t.Fatalf("topics: %v", cs.History("widgets").Topics())
	}
	calls := len(api.callList())
	session(t, srv, toolCall(2, ToolHistory, map[string]any{"topic": "#7", "why": WhySpec}))
	if len(api.callList()) != calls {
		t.Fatal("read the thread again within threadAge")
	}
	if strings.Contains(h.buf.String(), "secret-42") {
		t.Fatal("the log holds a confidential comment")
	}
}

// ask_expert calls the expert model with the question and the records
// named, in the worktree with read-only tools, returns its answer, logs
// its tokens, and refuses once the repository's daily budget is spent.
func TestAskExpert(t *testing.T) {
	h := caseHarness(t)
	dir := fakeClaude(t)
	h.cfg.Jobs.Expert.BudgetTokens = 1000
	h.appendEvent(ruling("R1", "never push to main"))
	r1 := storeOf(h).History("widgets").OnTopic("#7")[0].ID
	claudeAnswers(t, dir, 0, claudeResult(fmt.Sprintf(`{"answer":"No: R1 forbids it.","confidence":"high","used":[%q,"made-up"]}`, r1)))
	srv := h.mcpServerFor("s1")
	rs := session(t, srv, toolCall(1, ToolAskExpert, map[string]any{"question": "may I push to main?", "record_ids": []string{r1}}))
	got := replyJSON(t, rs[0])
	if got["answer"] != "No: R1 forbids it." || got["confidence"] != "high" || fmt.Sprint(got["used"]) != "["+r1+"]" || got["model"] != "opus" {
		t.Fatalf("answer: %v", got)
	}
	args := readFake(t, dir, "args.1")
	if !strings.Contains(args, "opus") || !strings.Contains(args, "Read,Grep,Glob") || !strings.Contains(args, "Read(~/.ssh/**)") || !strings.Contains(readFake(t, dir, "in.1"), "never push to main") {
		t.Fatalf("call: %s", args)
	}
	q := queries(h)[0]
	if q.Tool != ToolAskExpert || q.Usage == nil || q.Usage.InputTokens != 900 || q.Model != "opus" {
		t.Fatalf("logged: %+v", q)
	}
	rs = session(t, srv, toolCall(2, ToolAskExpert, map[string]any{"question": "and to a branch?"}))
	if !rs[0].Result.IsError || !strings.Contains(rs[0].Result.Content[0].Text, "expert budget is spent") || claudeCalls(t, dir) != 1 {
		t.Fatalf("over budget: %+v", rs[0])
	}
}

// What a thread agent gets for "what's the status of #N" is the card,
// which at its largest is well under 5k tokens.
func TestCardBoundsStatusAnswer(t *testing.T) {
	if n := tokensOf(CardMaxBytes); n > 2500 {
		t.Fatalf("a full card is %d tokens", n)
	}
}

// The stats count a question answered from state alone when no history
// call from the same agent followed it within contextFollowUp.
func TestContextStats(t *testing.T) {
	at := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	q := func(min int, who, tool, why string) Event {
		return Event{Time: at.Add(time.Duration(min) * time.Minute), Session: who, Kind: KindQuery, Query: &QueryPayload{Repository: "widgets", Tool: tool, Why: why, Hit: who != "missed" && tool != ToolHistory}}
	}
	card := func(min int, tokens int64, refused string) Event {
		p := &CardPayload{Repository: "widgets", Topic: "#7", Refused: refused, Usage: &JobUsage{InputTokens: tokens}}
		if refused == "" {
			p.Card = &Card{Topic: "#7"}
		}
		return Event{Time: at.Add(time.Duration(min) * time.Minute), Kind: KindCard, Card: p}
	}
	events := []Event{
		q(0, "a", ToolState, ""), q(1, "a", ToolFind, ""), q(2, "a", ToolHistory, WhyVerifyClaim), // 2 questions, 1 from state
		q(3, "b", ToolState, ""), q(20, "b", ToolHistory, WhyMissingFromState), // late: its own question
		q(30, "a", ToolState, ""), q(31, "other", ToolState, ""), q(32, "missed", ToolState, ""), // no card: not from state
		card(5, 3000, ""), card(6, 1000, "no src"), card(7, 5000, ""),
		{Time: at, Kind: KindQuery, Query: &QueryPayload{Repository: "gadgets", Tool: ToolState}},
	}
	s := contextStats(events, "widgets", at)
	if s.Questions != 7 || s.FromState != 4 || s.HistoryBy[WhyMissingFromState] != 1 || s.Calls[ToolState] != 5 {
		t.Fatalf("stats: %+v", s)
	}
	if s.Curations != 3 || s.Refused != 1 || s.CuratorMedian != 3000 || s.CuratorMax != 5000 || s.Cards != 1 {
		t.Fatalf("curators: %+v", s)
	}
}

// Review regressions (#60 review 1).

// history(ids) of ids history lacks, on a small budget, answers within it
// and never panics; more than historyIDs ids are refused.
func TestHistoryIDsOverBudget(t *testing.T) {
	hist := newHistory("widgets")
	var ids []string
	for i := 0; i < 100; i++ {
		ids = append(ids, fmt.Sprintf("Xunknown%03d", i))
	}
	out := hist.ByIDs(ids, 200)
	if n := len(compact(out)); n > budgetBytes(200) || out["missing_count"] != 100 {
		t.Fatalf("%d bytes: %v", n, out)
	}
	h := caseHarness(t)
	rs := session(t, h.queryServer(), toolCall(1, ToolHistory, map[string]any{"ids": ids, "why": WhySpec}))
	if !rs[0].Result.IsError || !strings.Contains(rs[0].Result.Content[0].Text, "at most 50 ids") {
		t.Fatalf("100 ids: %+v", rs[0])
	}
}

// A colour or a number after a colon is no reference; a word's part is
// none either.
func TestRefsIn(t *testing.T) {
	got := refsIn("see #56 and (!12), color: #333; issue-#9, #7-ish, [#8]")
	if fmt.Sprint(got) != "[#56 !12 #8]" {
		t.Fatalf("refs %v", got)
	}
}

// The repository's curator is not called again for cards it was sent,
// whatever it made of them; and a topic whose diff is refused three
// times in a row is given up, not paid for forever.
func TestCuratorStopsRetrying(t *testing.T) {
	h, dir := curatorHarness(t)
	h.appendEvent(ruling("R1", "never push to main"))
	r1 := onTopic(h, "#7", "never push")
	curatorAnswers(t, dir, "_7", fmt.Sprintf(`{"status":"open","summary":"s","src":[%q]}`, r1))
	h.ticks(6, time.Minute)
	h.ticks(30, 10*time.Minute) // five hours
	revised := 0
	for _, p := range cardEvents(h) {
		if p.Topic != TopicRepo && p.Card != nil {
			revised++
		}
	}
	if n := countOf(curatorCalls(t, dir), "repo"); n != revised {
		t.Fatalf("the repository's curator was called %d times for %d revised cards: %v", n, revised, curatorCalls(t, dir))
	}
	curatorAnswers(t, dir, "_7", fmt.Sprintf(`{"retire":[{"id":"R9","why":"x","src":[%q]}]}`, r1))
	before := countOf(curatorCalls(t, dir), "_7")
	h.appendEvent(Event{Kind: KindCI, Mode: ModeLive, Session: "s1", CI: &CIPayload{Repository: "widgets", Ref: "#7", State: "failed"}})
	h.ticks(10, 10*time.Minute)
	if n := countOf(curatorCalls(t, dir), "_7") - before; n != curatorRefusals {
		t.Fatalf("%d calls on #7 after the CI result, want %d", n, curatorRefusals)
	}
	var on7 []*CardPayload
	for _, p := range cardEvents(h) {
		if p.Topic == "#7" {
			on7 = append(on7, p)
		}
	}
	last := on7[len(on7)-1]
	if !strings.Contains(last.Refused, "given up") || on7[len(on7)-2].Refused == "" || on7[len(on7)-3].Refused == "" {
		t.Fatalf("last: %+v", last)
	}
}

// per_day bounds the curations a repository starts in a day, counting
// each call, and records that do not fit one input go to the next, none
// lost.
func TestCuratorPerDayAndLeftovers(t *testing.T) {
	h, dir := curatorHarness(t)
	h.cfg.Jobs.Curator.PerDay = 2
	h.cfg.Jobs.Curator.MaxInput = 3000
	for i := 0; i < 6; i++ {
		h.appendEvent(Event{Kind: KindCI, Mode: ModeLive, Session: "s1", CI: &CIPayload{Repository: "widgets", Ref: "#7", State: "failed", Excerpt: fmt.Sprintf("run %d ", i) + strings.Repeat("x", 900)}})
		h.clock.advance(time.Second)
	}
	h.ticks(30, 10*time.Minute)
	calls := curatorCalls(t, dir)
	if len(calls) != 2 {
		t.Fatalf("calls %v, want 2 in the day", calls)
	}
	h.cfg.Jobs.Curator.PerDay = 100
	h.ticks(12, 10*time.Minute)
	sent := map[string]bool{}
	for _, p := range cardEvents(h) {
		if p.Topic == "#7" {
			for _, id := range p.Records {
				sent[id] = true
			}
		}
	}
	for _, r := range storeOf(h).History("widgets").OnTopic("#7") {
		if !sent[r.ID] {
			t.Fatalf("%s never reached a curator", r.ID)
		}
	}
}

// The run reads an issue's thread when an item arrives on it, in a
// worker, with the title screened, and its comments reach the curator
// even when written before the last curation.
func TestCuratorReadsThreads(t *testing.T) {
	h, dir := curatorHarness(t)
	h.cfg.ConfidentialPatterns = []string{`secret-[0-9]+`}
	api := &fakeForgeAPI{gitlab: map[string]string{
		"=/projects/example-org%2Fwidgets/issues/7":                                                  `{"title":"secret-1 spin","description":"Make the widgets spin.","state":"opened","author":{"username":"ann"}}`,
		"=/projects/example-org%2Fwidgets/issues/7/notes?sort=desc&order_by=created_at&per_page=100": `[{"id":21,"body":"use the slow path","created_at":"2026-10-01T00:00:00Z","author":{"username":"bob"}}]`,
	}}
	h.sup.api = api
	h.appendEvent(Event{Kind: KindItem, Mode: ModeShared, Item: &ItemPayload{ID: "gitlab/example-org/widgets#7", Kind: ItemIssue, Repository: "gitlab/example-org/widgets", Title: "spin"}})
	h.ticks(6, time.Minute)
	if threadCalls := withoutLists(api.callList()); len(threadCalls) != 2 {
		t.Fatalf("forge calls %v", threadCalls)
	}
	if strings.Contains(h.buf.String(), "secret-1") {
		t.Fatal("a confidential title reached the log")
	}
	if in := readFake(t, dir, "in._7"); !strings.Contains(in, `"id":"C21"`) || !strings.Contains(in, "bob at 2026-10-01") {
		t.Fatalf("curator input: %s", in)
	}
}

// withoutLists is the forge calls that are not the live issue lists the
// curators have read (#62).
func withoutLists(calls []string) []string {
	var out []string
	for _, c := range calls {
		if !strings.Contains(c, "updated_after=") && !strings.Contains(c, "&since=") {
			out = append(out, c)
		}
	}
	return out
}
