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
	if !strings.Contains(r.Digest, "1 question") {
		t.Fatalf("digest = %q", r.Digest)
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
		{Time: at, Kind: KindStart, Details: map[string]any{"mode": ModeLive}},
		{Time: at, Session: "s1", Kind: KindSkip, Details: map[string]any{"delivered": "s1:a", "outcome": "!7", "mode": ModeLive}},
		{Time: at, Session: "s1", Kind: KindSkip, Details: map[string]any{"delivered": "s1:b", "outcome": "failed", "mode": ModeLive}},
		{Time: at, Session: "s1", Kind: KindSkip, Details: map[string]any{"delivered": "s1:c", "outcome": "nothing", "mode": ModeLive}},
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
	writeFile(t, logPath, `{"time":"2026-10-22T00:00:00Z","kind":"start","details":{"mode":"live"}}`+"\n")
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
