package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reportHarness runs the supervisor over three sessions: s1 working, s2
// blocked on the owner with Questions, s3 delivered. It returns the
// harness after a few passes.
func reportHarness(t *testing.T) *harness {
	h := newHarness(t, false, "s1", "s2", "s3")
	h.status("s1", "Painting the widgets\n")
	h.alive("s1", "working")
	h.status("s2", "Waiting\n\n## Questions\n\nShould the widgets be blue?\n")
	h.alive("s2", "waiting")
	h.ticks(2, 5*time.Minute)
	if err := MarkDelivered(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "s3", h.deliverSpec(t, "s3"), "!40"); err != nil {
		t.Fatal(err)
	}
	return h
}

// deliverSpec gives session name one deliver spec and returns its worktree.
func (h *harness) deliverSpec(t *testing.T, name string) string {
	wt := filepath.Join(h.dir, name, "wt")
	h.deliver(name, Deliver{Repository: "widgets", Worktree: wt, Target: "main", MRFile: filepath.Join(h.dir, name, "mr.md")})
	return wt
}

func readReport(t *testing.T, path string) (Report, string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r Report
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("report does not parse: %v\n%s", err, b)
	}
	return r, string(b)
}

// The report holds the schema, when and where it was written, each
// repository's cases, deliveries and the questions waiting on the owner,
// which are for the owner only, and a digest.
func TestReportFromTheEventLog(t *testing.T) {
	h := reportHarness(t)
	out := filepath.Join(t.TempDir(), "agentd", "report.json")
	now := h.clock.Now()
	if err := WriteReport(out, BuildReport(h.cfg, h.events(), ModeLive, "host-a", now)); err != nil {
		t.Fatal(err)
	}
	r, _ := readReport(t, out)
	if r.Schema != ReportSchema || r.Host != "host-a" || !r.WrittenAt.Equal(now) || r.Mode != ModeLive {
		t.Fatalf("header = %+v", r)
	}
	if len(r.Repositories) != 1 {
		t.Fatalf("repositories = %+v", r.Repositories)
	}
	repo := r.Repositories[0]
	if repo.Repository != "gitlab/example-org/widgets" || repo.Name != "widgets" {
		t.Fatalf("repository = %+v", repo)
	}
	states := map[string]string{}
	for _, c := range repo.Cases {
		states[c.Session] = c.State
	}
	if states["s1"] != string(Working) || states["s2"] != string(BlockedOnOwner) {
		t.Fatalf("cases = %+v", repo.Cases)
	}
	if len(repo.Deliveries) != 1 || repo.Deliveries[0].Ref != "!40" || repo.Deliveries[0].Session != "s3" {
		t.Fatalf("deliveries = %+v", repo.Deliveries)
	}
	if len(repo.Questions) != 1 || !repo.Questions[0].OwnerOnly || !strings.Contains(repo.Questions[0].Text, "Should the widgets be blue?") {
		t.Fatalf("questions = %+v", repo.Questions)
	}
	// The public digest counts what anyone may see; the question is the
	// owner's alone.
	if !strings.Contains(r.OwnerDigest, "1 question waiting") || !strings.Contains(r.Digest, "0 questions") || !strings.Contains(r.Digest, "1 request delivered") {
		t.Fatalf("digests = %q, %q", r.Digest, r.OwnerDigest)
	}
	// The question is the STATUS.md text, without the escalation's wording.
	if strings.Contains(repo.Questions[0].Text, "blocked on you") {
		t.Fatalf("question = %q", repo.Questions[0].Text)
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("report mode: %v, %v", info, err)
	}
}

// An event matching a confidential pattern never reaches the file: not a
// question, a STATUS.md head or a delivery reference, whether it was
// screened when logged or only matches a pattern added since.
func TestReportLeavesOutConfidentialText(t *testing.T) {
	h := reportHarness(t)
	// Written before the pattern existed, so the log holds the text.
	h.status("s1", "Embargoed: the widget launch\n")
	h.status("s2", "Waiting\n\n## Questions\n\nIs the embargoed launch on?\n")
	h.ticks(2, 31*time.Minute)
	if !strings.Contains(strings.ToLower(h.buf.String()), "embargoed") {
		t.Fatal("setup: the log should hold the text written before the pattern")
	}
	h.cfg.ConfidentialPatterns = []string{"(?i)embargoed"}
	out := filepath.Join(t.TempDir(), "report.json")
	if err := WriteReport(out, BuildReport(h.cfg, h.events(), ModeLive, "host-a", h.clock.Now())); err != nil {
		t.Fatal(err)
	}
	_, text := readReport(t, out)
	if strings.Contains(strings.ToLower(text), "embargoed") {
		t.Fatalf("confidential text reached the report:\n%s", text)
	}
	if !strings.Contains(text, `"s2"`) {
		t.Fatalf("the session itself should still be reported:\n%s", text)
	}
}

// A delivery found already open is reported; one given up on, or with
// nothing to deliver, is not.
func TestReportDeliveriesFromSkips(t *testing.T) {
	h := newHarness(t, false, "s1")
	at := h.clock.Now()
	events := []Event{
		{V: EventVersion, Time: at, Kind: KindStart, Mode: ModeLive, Start: &StartPayload{}},
		{V: EventVersion, Time: at, Session: "s1", Kind: KindSkip, Mode: ModeLive, Skip: &SkipPayload{Key: "a", Delivered: "s1:a", Outcome: "!7", Repository: "widgets"}},
		{V: EventVersion, Time: at, Session: "s1", Kind: KindSkip, Mode: ModeLive, Skip: &SkipPayload{Key: "b", Delivered: "s1:b", Outcome: "failed", Repository: "widgets"}},
		{V: EventVersion, Time: at, Session: "s1", Kind: KindSkip, Mode: ModeLive, Skip: &SkipPayload{Key: "c", Delivered: "s1:c", Outcome: "nothing", Repository: "widgets"}},
	}
	r := BuildReport(h.cfg, events, ModeLive, "host-a", at)
	if len(r.Repositories) != 1 || len(r.Repositories[0].Deliveries) != 1 || r.Repositories[0].Deliveries[0].Ref != "!7" {
		t.Fatalf("report = %+v", r.Repositories)
	}
}

// A live report never shows what a shadow run would have done.
func TestReportKeepsModesApart(t *testing.T) {
	h := newHarness(t, true, "s1")
	h.status("s1", "Waiting\n\n## Questions\n\nShould the widgets be blue?\n")
	h.alive("s1", "waiting")
	h.ticks(2, 5*time.Minute)
	live := BuildReport(h.cfg, h.events(), ModeLive, "host-a", h.clock.Now())
	if len(live.Repositories) != 0 {
		t.Fatalf("live report from a shadow log: %+v", live.Repositories)
	}
	shadow := BuildReport(h.cfg, h.events(), ModeShadow, "host-a", h.clock.Now())
	if len(shadow.Repositories) != 1 || len(shadow.Repositories[0].Questions) != 1 {
		t.Fatalf("shadow report: %+v", shadow.Repositories)
	}
}

// The file is replaced whole: a reader sees the old report or the new one,
// and no temporary file is left beside it.
func TestReportIsWrittenAtomically(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "report.json")
	writeFile(t, out, "old")
	if err := WriteReport(out, Report{Schema: ReportSchema}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("directory holds %v", entries)
	}
	if r, _ := readReport(t, out); r.Schema != ReportSchema {
		t.Fatalf("report = %+v", r)
	}
	if err := WriteReport(filepath.Join(dir, "missing", "deeper", "x", "report.json"), Report{}); err != nil {
		t.Fatalf("a missing directory is created: %v", err)
	}
}

// agentd report --out writes the file from the configured event log.
func TestReportCommand(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	writeFile(t, logPath, `{"v":2,"time":"2026-10-22T00:00:00Z","kind":"start","mode":"live","start":{"sessions":[]}}`+"\n")
	cfgPath := filepath.Join(dir, "agentd.json")
	writeFile(t, cfgPath, `{"event_log": "`+logPath+`", "transcript_dir": ""}`)
	out := filepath.Join(dir, "site", "report.json")
	if err := reportCmd([]string{"--config", cfgPath, "--out", out, "--host", "host-a"}); err != nil {
		t.Fatal(err)
	}
	if r, _ := readReport(t, out); r.Schema != ReportSchema || r.Host != "host-a" {
		t.Fatalf("report = %+v", r)
	}
	if err := reportCmd([]string{"--config", cfgPath}); err == nil {
		t.Fatal("report without --out")
	}
}

// A delivery is filed under the repository it went to, with that
// repository's owner_only, not under the session's own; sessions that name
// no repository are grouped under "" for the owner only.
func TestReportGroupsByDeliveredRepository(t *testing.T) {
	h := newHarness(t, false, "s1", "s2")
	h.cfg.Repositories["private"] = Repository{Forge: "github", Host: "github.com", Project: "example-org/private", OwnerOnly: true}
	s2 := h.cfg.Sessions["s2"]
	s2.Repository = ""
	h.cfg.Sessions["s2"] = s2
	wt := filepath.Join(h.dir, "s1", "wt")
	h.deliver("s1", Deliver{Repository: "private", Worktree: wt, Target: "main", MRFile: filepath.Join(h.dir, "s1", "mr.md")})
	h.status("s1", "Working\n")
	h.alive("s1", "working")
	h.status("s2", "Working\n")
	h.alive("s2", "working")
	h.ticks(1, 5*time.Minute)
	if err := MarkDelivered(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "s1", wt, "#9"); err != nil {
		t.Fatal(err)
	}
	r := BuildReport(h.cfg, h.events(), ModeLive, "host-a", h.clock.Now())
	byKey := map[string]ReportRepo{}
	for _, rr := range r.Repositories {
		byKey[rr.Repository] = rr
	}
	priv, widgets, none := byKey["github/example-org/private"], byKey["gitlab/example-org/widgets"], byKey[""]
	if len(priv.Deliveries) != 1 || !priv.OwnerOnly || !priv.Deliveries[0].OwnerOnly || priv.Deliveries[0].Ref != "#9" {
		t.Fatalf("private = %+v", priv)
	}
	if len(widgets.Deliveries) != 0 || len(widgets.Cases) != 1 || widgets.Cases[0].OwnerOnly {
		t.Fatalf("widgets = %+v", widgets)
	}
	if len(none.Cases) != 1 || none.Cases[0].Session != "s2" || !none.OwnerOnly || !none.Cases[0].OwnerOnly {
		t.Fatalf("no repository = %+v", none)
	}
	if strings.Contains(r.Digest, "1 request") || !strings.Contains(r.OwnerDigest, "1 request") {
		t.Fatalf("digests = %q, %q", r.Digest, r.OwnerDigest)
	}
}

// Names are screened too: a session, a repository or a delivery reference
// that matches a pattern is left out whole, and a session only the log
// knows, no longer configured, is not reported at all.
func TestReportScreensNames(t *testing.T) {
	h := newHarness(t, false, "s1", "embargoed1", "s3")
	h.cfg.Repositories["hush"] = Repository{Forge: "gitlab", Host: "gitlab.example.com", Project: "example-org/embargoed-hush"}
	s3 := h.cfg.Sessions["s3"]
	s3.Repository = "hush"
	h.cfg.Sessions["s3"] = s3
	for _, n := range []string{"s1", "embargoed1", "s3"} {
		h.status(n, "Working\n")
		h.alive(n, "working")
	}
	h.ticks(1, 5*time.Minute)
	if err := MarkDelivered(h.cfg, NewLogWriter(h.buf), h.clock.Now(), "s1", h.deliverSpec(t, "s1"), "embargoed-!41"); err != nil {
		t.Fatal(err)
	}
	h.buf.WriteString(`{"v":2,"time":"2026-10-22T00:10:00Z","session":"ghost","kind":"observe","mode":"live","observe":{"state":"working","reason":"","alive":true,"done":false,"questions":false,"limited":false,"trust":false,"dialog":false,"command":"claude","head":""}}` + "\n")
	h.cfg.ConfidentialPatterns = []string{"(?i)embargoed"}
	out := filepath.Join(t.TempDir(), "report.json")
	if err := WriteReport(out, BuildReport(h.cfg, h.events(), ModeLive, "host-a", h.clock.Now())); err != nil {
		t.Fatal(err)
	}
	_, text := readReport(t, out)
	if strings.Contains(strings.ToLower(text), "embargoed") || strings.Contains(text, "ghost") || !strings.Contains(text, `"s1"`) {
		t.Fatalf("report:\n%s", text)
	}
}

// Questions whose text was withheld when they were posted are left out:
// the log holds only the generic message, which is not a question.
func TestReportLeavesOutWithheldQuestions(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.cfg.ConfidentialPatterns = []string{"(?i)embargoed"}
	h.restart(false)
	h.status("s1", "Waiting\n\n## Questions\n\nIs the embargoed launch on?\n")
	h.alive("s1", "waiting")
	h.ticks(2, 5*time.Minute)
	if h.count(KindAction, ActEscalate, "s1") != 1 {
		t.Fatalf("setup: want one escalation:\n%s", h.buf.String())
	}
	r := BuildReport(h.cfg, h.events(), ModeLive, "host-a", h.clock.Now())
	if len(r.Repositories) != 1 || len(r.Repositories[0].Questions) != 0 {
		t.Fatalf("report = %+v", r.Repositories)
	}
}

// A shadow report lists the deliveries the shadow run would have opened.
func TestShadowReportListsWouldOpen(t *testing.T) {
	h := newHarness(t, true, "s1")
	at := h.clock.Now()
	events := []Event{
		{V: EventVersion, Time: at, Kind: KindStart, Mode: ModeShadow, Start: &StartPayload{}},
		{V: EventVersion, Time: at, Session: "s1", Kind: KindWould, Mode: ModeShadow, Act: &ActPayload{Action: ActOpen, Delivered: "s1:wt", Repository: "widgets"}},
	}
	r := BuildReport(h.cfg, events, ModeShadow, "host-a", at)
	if len(r.Repositories) != 1 || len(r.Repositories[0].Deliveries) != 1 || r.Repositories[0].Deliveries[0].Ref != "would-open" {
		t.Fatalf("report = %+v", r.Repositories)
	}
	if live := BuildReport(h.cfg, events, ModeLive, "host-a", at); len(live.Repositories) != 0 {
		t.Fatalf("live report = %+v", live.Repositories)
	}
}

// appendItem logs an item as agentd items would, and its triage when one
// is given.
func appendItem(h *harness, at time.Time, it ItemPayload, tr *TriageResult) {
	w := NewLogWriter(h.buf)
	w.Append(Event{V: EventVersion, Time: at, Kind: KindItem, Mode: ModeShared, Item: &it})
	if tr != nil {
		tr.ID, tr.Hash = it.ID, ItemHash(it)
		w.Append(Event{V: EventVersion, Time: at, Kind: KindTriage, Mode: ModeShared, Triage: &TriagePayload{Results: []TriageResult{*tr}}})
	}
}

// The report holds the new items: each under its repository, with its
// triage, through the fields the report names alone. Everything from mail
// is for the owner only, and so is an item of a repository agentd is not
// configured for; an item with confidential text is left out whole, and
// one the owner saw long ago is no longer new.
func TestReportItems(t *testing.T) {
	h := reportHarness(t)
	h.cfg.ConfidentialPatterns = []string{`(?i)secret-project`}
	now := h.clock.Now()
	widgets := "gitlab/example-org/widgets"
	appendItem(h, now, ItemPayload{ID: widgets + "#5", Kind: ItemIssue, Repository: widgets, Title: "Widgets wobble",
		Link: "https://gitlab.example.com/example-org/widgets/-/issues/5", Author: "someone"},
		&TriageResult{Kind: "bug", NeedsOwner: "no", Urgency: "today", Summary: "The widgets wobble.", Suggested: "fix"})
	appendItem(h, now, ItemPayload{ID: "mail:t1", Kind: ItemMailQuestion, Repository: widgets, Title: "Are widgets blue?", OwnerOnly: true}, nil)
	appendItem(h, now, ItemPayload{ID: "mail:t2", Kind: ItemMailReport, Title: "Monthly numbers", OwnerOnly: true}, nil)
	appendItem(h, now, ItemPayload{ID: "github/example-org/gadgets#2", Kind: ItemIssue, Repository: "github/example-org/gadgets", Title: "Gadgets jam"}, nil)
	appendItem(h, now, ItemPayload{ID: widgets + "#6", Kind: ItemIssue, Repository: widgets, Title: "Port the secret-project widget"}, nil)
	appendItem(h, now.Add(-30*24*time.Hour), ItemPayload{ID: widgets + "#1", Kind: ItemIssue, Repository: widgets, Title: "Old news"}, nil)
	if err := MarkSeen(NewLogWriter(h.buf), now, []string{widgets + "#1"}, ""); err != nil {
		t.Fatal(err)
	}
	r := BuildReport(h.cfg, h.events(), ModeLive, "host-a", now)
	b, _ := json.Marshal(r)
	for _, gone := range []string{"secret-project", "Old news", "someone"} {
		if strings.Contains(string(b), gone) {
			t.Fatalf("the report carries %q:\n%s", gone, b)
		}
	}
	items := map[string]ReportItem{}
	repoOf := map[string]ReportRepo{}
	for _, rr := range r.Repositories {
		for _, it := range rr.Items {
			items[it.ID] = it
			repoOf[it.ID] = rr
		}
	}
	if len(items) != 4 {
		t.Fatalf("items %+v", items)
	}
	if it := items[widgets+"#5"]; it.OwnerOnly || it.Urgency != "today" || it.Summary != "The widgets wobble." || it.Title != "Widgets wobble" ||
		it.Link == "" || repoOf[it.ID].Repository != widgets || repoOf[it.ID].OwnerOnly {
		t.Fatalf("issue %+v in %+v", it, repoOf[it.ID].Repository)
	}
	if it := items["mail:t1"]; !it.OwnerOnly || repoOf[it.ID].Repository != widgets {
		t.Fatalf("mail with a repository %+v", it)
	}
	if it := items["mail:t2"]; !it.OwnerOnly || repoOf[it.ID].Repository != "" || !repoOf[it.ID].OwnerOnly {
		t.Fatalf("mail without one %+v", it)
	}
	if it := items["github/example-org/gadgets#2"]; !it.OwnerOnly || !repoOf[it.ID].OwnerOnly {
		t.Fatalf("an unconfigured repository's item %+v", it)
	}
	if !strings.Contains(r.Digest, "1 new item") || !strings.Contains(r.OwnerDigest, "4 new items") {
		t.Fatalf("digests = %q, %q", r.Digest, r.OwnerDigest)
	}
	// A mail item's triage is the owner's as the item is.
	appendItem(h, now, ItemPayload{ID: "mail:t3", Kind: ItemMailReport, Repository: widgets, Title: "Quarterly", OwnerOnly: true},
		&TriageResult{Kind: "report", NeedsOwner: "no", Urgency: "later", Summary: "Numbers are in.", Suggested: "read"})
	r = BuildReport(h.cfg, h.events(), ModeLive, "host-a", now)
	for _, rr := range r.Repositories {
		for _, it := range rr.Items {
			if strings.HasPrefix(it.ID, "mail:") && !it.OwnerOnly {
				t.Fatalf("mail item published: %+v", it)
			}
			if it.ID == "mail:t3" && it.Summary != "Numbers are in." {
				t.Fatalf("mail triage %+v", it)
			}
		}
	}
}

// reportItems is every item of the report by id, and the repository it is
// filed under.
func reportItems(r Report) (map[string]ReportItem, map[string]ReportRepo) {
	items, repoOf := map[string]ReportItem{}, map[string]ReportRepo{}
	for _, rr := range r.Repositories {
		for _, it := range rr.Items {
			items[it.ID], repoOf[it.ID] = it, rr
		}
	}
	return items, repoOf
}

// An item stays until the owner has seen it and seven days have passed; a
// triage of an older text is not published; any mark of mail makes an item
// the owner's.
func TestReportItemWindowTriageAndMailMarks(t *testing.T) {
	h := newHarness(t, false, "s1")
	now := h.clock.Now()
	widgets := "gitlab/example-org/widgets"
	appendItem(h, now.Add(-30*24*time.Hour), ItemPayload{ID: widgets + "#1", Kind: ItemIssue, Repository: widgets, Title: "Old, unseen"}, nil)
	appendItem(h, now.Add(-2*24*time.Hour), ItemPayload{ID: widgets + "#2", Kind: ItemIssue, Repository: widgets, Title: "Recent, seen"}, nil)
	if err := MarkSeen(NewLogWriter(h.buf), now, []string{widgets + "#2"}, ""); err != nil {
		t.Fatal(err)
	}
	appendItem(h, now, ItemPayload{ID: widgets + "#3", Kind: ItemIssue, Repository: widgets, Title: "Before"},
		&TriageResult{Kind: "bug", NeedsOwner: "no", Urgency: "now", Summary: "Stale summary.", Suggested: "fix"})
	appendItem(h, now, ItemPayload{ID: widgets + "#3", Kind: ItemIssue, Repository: widgets, Title: "After"}, nil)
	appendItem(h, now, ItemPayload{ID: "x1", Kind: ItemMailReport, Repository: widgets, Title: "By kind"}, nil)
	appendItem(h, now, ItemPayload{ID: "mail:x2", Kind: ItemIssue, Repository: widgets, Title: "By id"}, nil)
	items, _ := reportItems(BuildReport(h.cfg, h.events(), ModeLive, "host-a", now))
	if _, ok := items[widgets+"#1"]; !ok {
		t.Fatal("an unseen item left the report")
	}
	if it, ok := items[widgets+"#2"]; !ok || !it.Seen {
		t.Fatalf("a recently seen item: %+v, %v", it, ok)
	}
	if it := items[widgets+"#3"]; it.Title != "After" || it.Summary != "" || it.Urgency != "" {
		t.Fatalf("a stale triage was published: %+v", it)
	}
	if !items["x1"].OwnerOnly || !items["mail:x2"].OwnerOnly {
		t.Fatalf("mail marks: %+v, %+v", items["x1"], items["mail:x2"])
	}
	// Seen, and first seen over seven days ago: gone.
	if err := MarkSeen(NewLogWriter(h.buf), now, []string{widgets + "#1"}, ""); err != nil {
		t.Fatal(err)
	}
	if items, _ := reportItems(BuildReport(h.cfg, h.events(), ModeLive, "host-a", now)); items[widgets+"#1"].ID != "" {
		t.Fatal("an old seen item stayed")
	}
}

// A confidential match in any published field of an item leaves the item
// out whole: its id, title, link or triage summary.
func TestReportItemsScreenEveryField(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.cfg.ConfidentialPatterns = []string{`(?i)hush`}
	now := h.clock.Now()
	widgets := "gitlab/example-org/widgets"
	appendItem(h, now, ItemPayload{ID: widgets + "#1", Kind: ItemIssue, Repository: widgets, Title: "Plain"}, nil)
	appendItem(h, now, ItemPayload{ID: widgets + "#hush", Kind: ItemIssue, Repository: widgets, Title: "By id"}, nil)
	appendItem(h, now, ItemPayload{ID: widgets + "#3", Kind: ItemIssue, Repository: widgets, Title: "By link", Link: "https://example.com/hush"}, nil)
	appendItem(h, now, ItemPayload{ID: widgets + "#4", Kind: ItemIssue, Repository: widgets, Title: "By summary"},
		&TriageResult{Kind: "bug", NeedsOwner: "no", Urgency: "later", Summary: "Hush now.", Suggested: "fix"})
	r := BuildReport(h.cfg, h.events(), ModeLive, "host-a", now)
	b, _ := json.Marshal(r)
	items, _ := reportItems(r)
	if strings.Contains(strings.ToLower(string(b)), "hush") || len(items) != 1 {
		t.Fatalf("report %s", b)
	}
}

// Repositories that share a forge and project share one entry, for the
// owner only when any of them is, however the configuration is read; a
// public repository with only the owner's entries is the owner's too.
func TestReportSharedKeyAndOwnerOnlyRepository(t *testing.T) {
	h := reportHarness(t)
	inner := h.cfg.Repositories["widgets"]
	inner.Host, inner.OwnerOnly = "gitlab.internal.example", true
	h.cfg.Repositories["a-inner"] = inner
	appendItem(h, h.clock.Now(), ItemPayload{ID: "mail:m1", Kind: ItemMailReport, Repository: "github/example-org/gadgets", Title: "Mail", OwnerOnly: true}, nil)
	h.cfg.Repositories["gadgets"] = Repository{Forge: "github", Project: "example-org/gadgets"}
	for i := 0; i < 20; i++ {
		r := BuildReport(h.cfg, h.events(), ModeLive, "host-a", h.clock.Now())
		for _, rr := range r.Repositories {
			if !rr.OwnerOnly {
				t.Fatalf("repository %q (%s) is public", rr.Repository, rr.Name)
			}
			if rr.Repository == "gitlab/example-org/widgets" && rr.Name != "a-inner" {
				t.Fatalf("name %q", rr.Name)
			}
			for _, c := range rr.Cases {
				if !c.OwnerOnly {
					t.Fatalf("case %+v public", c)
				}
			}
		}
	}
}
