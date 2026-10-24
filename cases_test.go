package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// progressText is a STATUS.md whose table says what the session works on.
func progressText(item, state string) string {
	return "# Status\n\n| Item | State |\n|---|---|\n| first | merged |\n| " + item + " | " + state + " |\n| later | not started |\n"
}

// caseHarness is a harness whose session s1 works issue 7 of widgets.
func caseHarness(t *testing.T) *harness {
	h := newHarness(t, false, "s1")
	s := h.cfg.Sessions["s1"]
	s.Issue = 7
	h.cfg.Sessions["s1"] = s
	return h
}

const widgetsCase = "gitlab/example-org/widgets#7"

func casesOf(h *harness, mode string) []CaseView {
	st := NewLogState(h.cfg, mode).Fold(h.events())
	return CasesFrom(h.cfg, st)
}

// A session's progress is its case's report, logged when it changes and
// not on every pass.
func TestCaseReportLoggedOnChange(t *testing.T) {
	h := caseHarness(t)
	h.alive("s1", "working")
	h.status("s1", progressText("second", "in progress"))
	h.ticks(3, 5*time.Minute)
	if n := h.count(KindCase, "", "s1"); n != 1 {
		t.Fatalf("%d case events, want 1", n)
	}
	h.status("s1", progressText("second", "review"))
	h.ticks(2, 5*time.Minute)
	if n := h.count(KindCase, "", "s1"); n != 2 {
		t.Fatalf("%d case events, want 2", n)
	}
	cs := casesOf(h, ModeLive)
	if len(cs) != 1 || cs[0].ID != widgetsCase || cs[0].Item != "second" || cs[0].ItemState != "review" ||
		!reflect.DeepEqual(cs[0].Next, []string{"later"}) || cs[0].State != CaseWorking {
		t.Fatalf("cases %+v", cs)
	}
}

// A session with no issue has no case and logs no case events.
func TestNoIssueNoCase(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.alive("s1", "working")
	h.status("s1", progressText("second", "in progress"))
	h.ticks(2, 5*time.Minute)
	if n := h.count(KindCase, "", ""); n != 0 {
		t.Fatalf("%d case events", n)
	}
	if cs := casesOf(h, ModeLive); len(cs) != 0 {
		t.Fatalf("cases %+v", cs)
	}
}

// A restart hands the session a context pack, and the pack is the case's:
// the facts agentd typed, logged with their hash.
func TestCasePackOnRestart(t *testing.T) {
	h := caseHarness(t)
	h.status("s1", progressText("second", "in progress"))
	h.alive("s1", "working")
	h.ticks(1, 5*time.Minute)
	h.tmux.alive["s1"] = false
	h.ticks(2, 5*time.Minute)
	if n := h.tmux.count("new-session", "s1"); n != 1 {
		t.Fatalf("%d starts", n)
	}
	var pack *CasePack
	for _, e := range h.events() {
		if e.Kind == KindCase && e.Case.Pack != nil {
			pack = e.Case.Pack
		}
	}
	if pack == nil {
		t.Fatal("no pack logged")
	}
	var f restartFacts
	if err := json.Unmarshal(pack.Facts, &f); err != nil || f.Why != "restart" || f.Item != "second" {
		t.Fatalf("pack %s: %v", pack.Facts, err)
	}
	if pack.Hash != shortHash(HashPane(string(pack.Facts))) {
		t.Fatalf("pack hash %q", pack.Hash)
	}
	if cs := casesOf(h, ModeLive); cs[0].Pack != pack.Hash || cs[0].Packs != 1 {
		t.Fatalf("cases %+v", cs)
	}
}

// Replay rebuilds the cases: a fresh fold of the log and a restarted
// agentd see what the running one saw, rulings included.
func TestCasesReplay(t *testing.T) {
	h := caseHarness(t)
	h.status("s1", progressText("second", "in progress"))
	h.alive("s1", "working")
	h.ticks(2, 5*time.Minute)
	h.tmux.alive["s1"] = false
	h.ticks(2, 5*time.Minute)
	st := NewLogState(h.cfg, ModeLive).Fold(h.events())
	if err := RecordRuling(NewLogWriter(h.buf), h.cfg, st, h.clock.Now(), widgetsCase, RulingRestriction, "Touch no other package.", "owner"); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, 5*time.Minute)
	live := CasesFrom(h.cfg, h.sup.st)
	if len(live) != 1 || len(live[0].Active) != 1 || live[0].Active[0].ID != "R1" {
		t.Fatalf("live cases %+v", live)
	}
	if folded := casesOf(h, ModeLive); !reflect.DeepEqual(folded, live) {
		t.Fatalf("folded %+v\nlive %+v", folded, live)
	}
	h.restart(false)
	if replayed := CasesFrom(h.cfg, h.sup.st); !reflect.DeepEqual(replayed, live) {
		t.Fatalf("replayed %+v\nlive %+v", replayed, live)
	}
	// The shadow run keeps its own reports; the owner's ruling counts for both.
	if sh := casesOf(h, ModeShadow); len(sh) != 1 || sh[0].Item != "" || len(sh[0].Active) != 1 {
		t.Fatalf("shadow cases %+v", sh)
	}
}

// Rulings are numbered R1, R2… for the case's life; closing the case
// expires them, and a closed case takes no restriction until reopened.
func TestCaseRulings(t *testing.T) {
	h := caseHarness(t)
	w := NewLogWriter(h.buf)
	rule := func(kind, text string) error {
		st := NewLogState(h.cfg, ModeLive).Fold(h.events())
		return RecordRuling(w, h.cfg, st, h.clock.Now(), widgetsCase, kind, text, "owner")
	}
	if err := rule(RulingRestriction, "Touch no other package."); err != nil {
		t.Fatal(err)
	}
	if err := rule(RulingWaiver, "The gate may skip the slow suite."); err != nil {
		t.Fatal(err)
	}
	cs := casesOf(h, ModeLive)
	if len(cs[0].Active) != 2 || cs[0].Active[1].ID != "R2" || cs[0].Active[1].Kind != RulingWaiver {
		t.Fatalf("active %+v", cs[0].Active)
	}
	if err := rule(RulingClose, ""); err != nil {
		t.Fatal(err)
	}
	if cs := casesOf(h, ModeLive); cs[0].State != CaseClosed || len(cs[0].Active) != 0 {
		t.Fatalf("closed %+v", cs[0])
	}
	if err := rule(RulingRestriction, "Late."); err == nil {
		t.Fatal("a restriction on a closed case was accepted")
	}
	if err := rule(RulingClose, ""); err == nil {
		t.Fatal("a closed case was closed again")
	}
	if err := rule(RulingReopen, ""); err != nil {
		t.Fatal(err)
	}
	if err := rule(RulingRestriction, "Again."); err != nil {
		t.Fatal(err)
	}
	if cs := casesOf(h, ModeLive); cs[0].State == CaseClosed || len(cs[0].Active) != 1 || cs[0].Active[0].ID != "R3" {
		t.Fatalf("reopened %+v", cs[0])
	}
	for _, bad := range []struct{ id, kind, text string }{
		{"gitlab/example-org/widgets#8", RulingRestriction, "Unknown case."},
		{widgetsCase, "pardon", "Unknown kind."},
		{widgetsCase, RulingRestriction, ""},
		{widgetsCase, RulingRestriction, strings.Repeat("x", maxRuling+1)},
	} {
		st := NewLogState(h.cfg, ModeLive).Fold(h.events())
		if err := RecordRuling(w, h.cfg, st, h.clock.Now(), bad.id, bad.kind, bad.text, "owner"); err == nil {
			t.Fatalf("ruling %+v accepted", bad)
		}
	}
}

// A ruling the owner records while agentd runs reaches the running agentd.
func TestCaseRulingReachesTheRun(t *testing.T) {
	h := caseHarness(t)
	h.alive("s1", "working")
	h.ticks(1, 5*time.Minute)
	st := NewLogState(h.cfg, ModeLive).Fold(h.events())
	if err := RecordRuling(NewLogWriter(h.buf), h.cfg, st, h.clock.Now(), widgetsCase, RulingRestriction, "Touch no other package.", "owner"); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, 5*time.Minute)
	if cs := CasesFrom(h.cfg, h.sup.st); len(cs[0].Active) != 1 {
		t.Fatalf("live %+v", cs)
	}
}

// The state comes from facts: the sessions' tracks and the owner's rulings.
func TestCaseStateFromFacts(t *testing.T) {
	at := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		track *Track
		want  string
	}{
		{"none", nil, CaseNotStarted},
		{"working", &Track{State: Working, Live: true}, CaseWorking},
		{"blocked", &Track{State: BlockedOnOwner, Live: true}, CaseNeedsYou},
		{"parked", &Track{State: CrashLooping}, CaseNeedsYou},
		{"stopped", &Track{State: Stopped, Stopped: "budget"}, CaseStopped},
		{"delivered", &Track{State: Done, Deliveries: []DeliveryRecord{{Ref: "!3", At: at}}}, CaseDelivered},
		{"done, undelivered", &Track{State: Done}, CaseWorking},
	} {
		var ts []*Track
		if tc.track != nil {
			ts = append(ts, tc.track)
		}
		if got := caseState(nil, ts); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	if got := caseState(&Case{Closed: true}, []*Track{{State: BlockedOnOwner}}); got != CaseClosed {
		t.Errorf("closed: %s", got)
	}
}

// An issue item no session works yet is a case not started, and its triage
// is the case's urgency.
func TestCaseFromIssueItem(t *testing.T) {
	h := caseHarness(t)
	at := h.clock.Now()
	w := NewLogWriter(h.buf)
	issue := ItemPayload{ID: "gitlab/example-org/widgets#9", Kind: ItemIssue, Repository: "gitlab/example-org/widgets", Title: "Widgets wobble"}
	w.Append(Event{V: EventVersion, Time: at, Kind: KindItem, Mode: ModeShared, Item: &issue})
	w.Append(Event{V: EventVersion, Time: at, Kind: KindItem, Mode: ModeShared, Item: &ItemPayload{ID: "gitlab/example-org/widgets#10",
		Kind: ItemIssue, Repository: "gitlab/example-org/widgets", Title: "Not a case: the owner's", OwnerOnly: true}})
	w.Append(Event{V: EventVersion, Time: at, Kind: KindTriage, Mode: ModeShared, Triage: &TriagePayload{Results: []TriageResult{
		{ID: "gitlab/example-org/widgets#9", Hash: ItemHash(issue), Kind: "bug", NeedsOwner: "no", Urgency: "today", Summary: "It wobbles."}}}})
	cs := casesOf(h, ModeLive)
	if len(cs) != 2 {
		t.Fatalf("cases %+v", cs)
	}
	var c CaseView
	for _, v := range cs {
		if v.ID == "gitlab/example-org/widgets#9" {
			c = v
		}
	}
	if c.State != CaseNotStarted || c.Title != "Widgets wobble" || c.Urgency != "today" || c.Issue != 9 {
		t.Fatalf("case %+v", c)
	}
}

func rankIDs(cs []CaseView) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func rankFixture() []CaseView {
	at := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	return []CaseView{
		{ID: "github/example-org/gadgets#1", Repository: "github/example-org/gadgets", State: CaseWorking, Since: at},
		{ID: "gitlab/example-org/widgets#2", Repository: "gitlab/example-org/widgets", State: CaseWorking, Since: at.Add(time.Hour)},
		{ID: "gitlab/example-org/widgets#3", Repository: "gitlab/example-org/widgets", State: CaseNeedsYou, Since: at.Add(2 * time.Hour)},
		{ID: "github/example-org/gadgets#4", Repository: "github/example-org/gadgets", State: CaseNotStarted, Urgency: "now", Since: at.Add(3 * time.Hour)},
		{ID: "github/example-org/gadgets#5", Repository: "github/example-org/gadgets", State: CaseClosed, Since: at},
		{ID: "github/example-org/sprockets#6", Repository: "github/example-org/sprockets", State: CaseWorking, Since: at},
		{ID: "github/example-org/sprockets#7", Repository: "github/example-org/sprockets", State: CaseWorking, Since: at.Add(-time.Hour)},
	}
}

// Without priorities: what needs the owner first, then triage urgency,
// then the oldest waiting, then the id; closed cases last.
func TestRankCasesWithoutPriorities(t *testing.T) {
	cs := rankFixture()
	RankCases(cs, nil)
	want := []string{"gitlab/example-org/widgets#3", "github/example-org/gadgets#4", "github/example-org/sprockets#7",
		"github/example-org/gadgets#1", "github/example-org/sprockets#6", "gitlab/example-org/widgets#2", "github/example-org/gadgets#5"}
	if got := rankIDs(cs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	for i, c := range cs {
		if c.Rank != i+1 {
			t.Fatalf("rank of %s is %d", c.ID, c.Rank)
		}
	}
}

// With the week's priorities: the owner's pins first in pin order, then
// what needs the owner, then the repository's priority (unlisted after
// listed), then urgency and age.
func TestRankCasesWithPriorities(t *testing.T) {
	cs := rankFixture()
	pr := &Priorities{Week: "2026-W43", Repositories: []RepoPriority{
		{Repository: "gitlab/example-org/widgets", Priority: 1}, {Repository: "github/example-org/gadgets", Priority: 2}},
		Pins: []string{"github/example-org/sprockets#6"}}
	RankCases(cs, pr)
	// sprockets is unlisted: its unpinned #7, though the oldest, comes
	// after every listed repository's case.
	want := []string{"github/example-org/sprockets#6", "gitlab/example-org/widgets#3", "gitlab/example-org/widgets#2",
		"github/example-org/gadgets#4", "github/example-org/gadgets#1", "github/example-org/sprockets#7", "github/example-org/gadgets#5"}
	if got := rankIDs(cs); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	if !cs[0].Pinned || cs[2].Priority != 1 || cs[6].Priority != 2 || cs[0].Priority != 0 || cs[5].Priority != 0 {
		t.Fatalf("cases %+v", cs)
	}
}

// The priorities file is the tracking system's weekly review: a field
// agentd does not know refuses it, and a missing file is no priorities.
func TestLoadPriorities(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "priorities.json")
	if pr, err := LoadPriorities(p); err != nil || pr != nil {
		t.Fatalf("missing: %+v, %v", pr, err)
	}
	writeFile(t, p, `{"week":"2026-W43","pins":["widgets"]}`)
	if _, err := LoadPriorities(p); err == nil {
		t.Fatal("a pin that is no case was accepted")
	}
	os.Remove(p)
	if pr, err := LoadPriorities(""); err != nil || pr != nil {
		t.Fatalf("unconfigured: %+v, %v", pr, err)
	}
	writeFile(t, p, `{"week":"2026-W43","repositories":[{"repository":"gitlab/example-org/widgets","priority":1}],"pins":["gitlab/example-org/widgets#7"]}`)
	pr, err := LoadPriorities(p)
	if err != nil || pr.Week != "2026-W43" || pr.Repositories[0].Priority != 1 || pr.Pins[0] != widgetsCase {
		t.Fatalf("%+v, %v", pr, err)
	}
	writeFile(t, p, `{"week":"2026-W43","weights":{}}`)
	if _, err := LoadPriorities(p); err == nil {
		t.Fatal("an unknown field was accepted")
	}
	writeFile(t, p, `{"week":"2026-W43","repositories":[{"repository":"gitlab/example-org/widgets","priority":0}]}`)
	if _, err := LoadPriorities(p); err == nil {
		t.Fatal("priority 0 was accepted")
	}
}

// agentd cases prints the ranked cases, as JSON on request, and records a
// ruling.
func TestCasesCmd(t *testing.T) {
	h := caseHarness(t)
	h.alive("s1", "working")
	h.status("s1", progressText("second", "in progress"))
	h.ticks(1, 5*time.Minute)
	logPath := filepath.Join(h.dir, "events.jsonl")
	if err := os.WriteFile(logPath, h.buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(h.dir, "agentd.json")
	writeFile(t, cfgPath, `{"event_log": "`+logPath+`", "transcript_dir": "",
		"policies": {"escalate": {"command": ["route", "{console}", "{message}"], "default_console": "infra"}},
		"repositories": {"widgets": {"forge": "gitlab", "project": "example-org/widgets"}},
		"sessions": {"s1": {"workdir": "`+filepath.Join(h.dir, "s1")+`", "cwd": "`+filepath.Join(h.dir, "s1", "wt")+`", "repository": "widgets", "issue": 7}}}`)
	var out bytes.Buffer
	if err := casesCmd([]string{"--config", cfgPath, "rule", widgetsCase, RulingRestriction, "Touch no other package."}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := casesCmd([]string{"--config", cfgPath}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, widgetsCase) || !strings.Contains(s, "working") || !strings.Contains(s, "R1") {
		t.Fatalf("table %q", s)
	}
	out.Reset()
	if err := casesCmd([]string{"--config", cfgPath, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []CaseView `json:"cases"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || len(doc.Cases) != 1 || doc.Cases[0].Rank != 1 || doc.Cases[0].Item != "second" {
		t.Fatalf("json %s: %v", out.String(), err)
	}
}

// packs is the facts of every pack logged, in order.
func packs(t *testing.T, h *harness) []restartFacts {
	t.Helper()
	var out []restartFacts
	for _, e := range h.events() {
		if e.Kind == KindCase && e.Case.Pack != nil {
			var f restartFacts
			if err := json.Unmarshal(e.Case.Pack.Facts, &f); err != nil {
				t.Fatal(err)
			}
			out = append(out, f)
		}
	}
	return out
}

// A rotation hands the fresh session a pack too; a shadow run, which
// launches nothing, logs none.
func TestCasePackOnRotationNotInShadow(t *testing.T) {
	h := newHarness(t, false, "s")
	s := h.cfg.Sessions["s"]
	s.Issue = 7
	h.cfg.Sessions["s"] = s
	h.cfg.TranscriptDir = filepath.Join(h.dir, "projects")
	h.status("s", progressText("second", "coding"))
	h.git.merge, h.git.mergeSubject = "m0", "Merge #18: items"
	h.ticks(1, time.Minute)
	id := ""
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActRestart {
			id = e.Act.Transcript
		}
	}
	h.transcript(id, usageLine("m1", 10, 150000, 0, 5, false))
	h.git.merge, h.git.mergeSubject = "m1", "Merge #21: contexts"
	h.ticks(8, time.Minute)
	if n := h.count(KindAction, ActRotate, "s"); n != 1 {
		t.Fatalf("rotations = %d", n)
	}
	if ps := packs(t, h); len(ps) != 2 || ps[0].Why != "restart" || ps[1].Why != "rotation" {
		t.Fatalf("packs %+v", ps)
	}

	sh := newHarness(t, true, "s1")
	s1 := sh.cfg.Sessions["s1"]
	s1.Issue = 7
	sh.cfg.Sessions["s1"] = s1
	sh.status("s1", progressText("second", "coding"))
	sh.ticks(2, 5*time.Minute)
	if n := sh.count(KindWould, ActRestart, "s1"); n == 0 {
		t.Fatal("the shadow would not restart")
	}
	if ps := packs(t, sh); len(ps) != 0 {
		t.Fatalf("shadow packs %+v", ps)
	}
}

// A case event is refused unless it names a case and carries exactly one
// of a pack, a report or a well-formed ruling, in the mode its part needs.
func TestCaseEventCheck(t *testing.T) {
	at := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	pack := &CasePack{Hash: "h", Facts: json.RawMessage(`{}`)}
	report := &CaseReport{Hash: "h"}
	ruling := &CaseRuling{Kind: RulingRestriction, ID: "R1", Text: "x"}
	run := func(p CasePayload) Event {
		return Event{V: EventVersion, Time: at, Kind: KindCase, Mode: ModeLive, Session: "s1", Case: &p}
	}
	owner := func(p CasePayload) Event {
		return Event{V: EventVersion, Time: at, Kind: KindCase, Mode: ModeOwner, Case: &p}
	}
	for _, ok := range []Event{run(CasePayload{Case: widgetsCase, Pack: pack}), run(CasePayload{Case: widgetsCase, Report: report}),
		owner(CasePayload{Case: widgetsCase, Ruling: ruling}), owner(CasePayload{Case: widgetsCase, Ruling: &CaseRuling{Kind: RulingClose}})} {
		if err := ok.check(); err != nil {
			t.Errorf("%+v refused: %v", ok.Case, err)
		}
	}
	for _, bad := range []Event{
		run(CasePayload{Case: widgetsCase}),
		run(CasePayload{Case: "", Pack: pack}),
		run(CasePayload{Case: "widgets", Pack: pack}),
		run(CasePayload{Case: widgetsCase, Pack: pack, Report: report}),
		owner(CasePayload{Case: widgetsCase, Pack: pack, Ruling: ruling}),
		owner(CasePayload{Case: widgetsCase, Ruling: &CaseRuling{Kind: "pardon"}}),
		owner(CasePayload{Case: widgetsCase, Ruling: &CaseRuling{Kind: RulingWaiver, Text: "no id"}}),
		run(CasePayload{Case: widgetsCase, Ruling: ruling}),
		owner(CasePayload{Case: widgetsCase, Pack: pack}),
		{V: EventVersion, Time: at, Kind: KindCase, Mode: ModeLive, Case: &CasePayload{Case: widgetsCase, Pack: pack}},
		{V: EventVersion, Time: at, Kind: KindCase, Mode: ModeOwner, Session: "s1", Case: &CasePayload{Case: widgetsCase, Report: report}},
		owner(CasePayload{Case: widgetsCase, Ruling: &CaseRuling{Kind: RulingClose, ID: "R9", Text: "x"}}),
		run(CasePayload{Case: "GitLab/example-org/widgets#7", Pack: pack}),
	} {
		if err := bad.check(); err == nil {
			t.Errorf("%+v (mode %s) accepted", bad.Case, bad.Mode)
		}
	}
}

// A case only an issue item names can be ruled on, as agentd cases lists it.
func TestRulingOnAnItemCase(t *testing.T) {
	h := caseHarness(t)
	NewLogWriter(h.buf).Append(Event{V: EventVersion, Time: h.clock.Now(), Kind: KindItem, Mode: ModeShared,
		Item: &ItemPayload{ID: "gitlab/example-org/widgets#9", Kind: ItemIssue, Repository: "gitlab/example-org/widgets", Title: "Wobble"}})
	st := NewLogState(h.cfg, ModeLive).Fold(h.events())
	if err := RecordRuling(NewLogWriter(h.buf), h.cfg, st, h.clock.Now(), "gitlab/example-org/widgets#9", RulingClose, "", "owner"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range casesOf(h, ModeLive) {
		if c.ID == "gitlab/example-org/widgets#9" {
			found = c.State == CaseClosed
		}
	}
	if !found {
		t.Fatalf("cases %+v", casesOf(h, ModeLive))
	}
	// A ruling names its case in any letter case, as a pin does.
	st = NewLogState(h.cfg, ModeLive).Fold(h.events())
	if err := RecordRuling(NewLogWriter(h.buf), h.cfg, st, h.clock.Now(), "GitLab/Example-Org/widgets#9", RulingReopen, "", "owner"); err != nil {
		t.Fatal(err)
	}
}

// A pin matches whatever case it is written in, and a case with no
// time yet ranks after the ones that have waited.
func TestRankPinsAndUndated(t *testing.T) {
	at := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	cs := []CaseView{
		{ID: "gitlab/example-org/widgets#1", Repository: "gitlab/example-org/widgets", State: CaseWorking},
		{ID: "gitlab/example-org/widgets#2", Repository: "gitlab/example-org/widgets", State: CaseWorking, Since: at},
		{ID: "gitlab/example-org/widgets#3", Repository: "gitlab/example-org/widgets", State: CaseWorking, Since: at.Add(time.Hour)},
	}
	RankCases(cs, &Priorities{Pins: []string{"gitlab/Example-Org/widgets#3"}})
	if got := rankIDs(cs); !reflect.DeepEqual(got, []string{"gitlab/example-org/widgets#3", "gitlab/example-org/widgets#2", "gitlab/example-org/widgets#1"}) {
		t.Fatalf("got %v", got)
	}
}

// A session moved to another issue no longer speaks for its old case.
func TestMovedSessionLeavesItsCase(t *testing.T) {
	h := caseHarness(t)
	h.alive("s1", "working")
	h.status("s1", progressText("second", "in progress"))
	h.ticks(1, 5*time.Minute)
	s := h.cfg.Sessions["s1"]
	s.Issue = 8
	h.cfg.Sessions["s1"] = s
	h.ticks(1, 5*time.Minute)
	for _, c := range casesOf(h, ModeLive) {
		switch c.ID {
		case widgetsCase:
			if c.State != CaseNotStarted || !reflect.DeepEqual(c.Sessions, []string{"s1"}) {
				t.Fatalf("old case %+v", c)
			}
		case "gitlab/example-org/widgets#8":
			if c.State != CaseWorking {
				t.Fatalf("new case %+v", c)
			}
		}
	}
}
