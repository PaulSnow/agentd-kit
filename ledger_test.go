package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ledgerConfig is a config with accounts a and b, read every 15 minutes
// under a 90% limit, and widgets falling back from a to b.
func ledgerConfig() *Config {
	cfg := DefaultConfig()
	cfg.Accounts = map[string]Account{"a": {ConfigDir: "/nonexistent/a"}, "b": {ConfigDir: "/nonexistent/b"}}
	cfg.Usage = UsageConfig{Enabled: true, Every: Duration{15 * time.Minute}, Limit: 90}
	cfg.Repositories = map[string]Repository{"widgets": {Policy: RepoPolicy{Sessions: SessionRules{Fallback: []string{"a", "b"}}}}}
	return &cfg
}

// reading is a shared account event at t.
func reading(t time.Time, account string, five, week float64, reset time.Time) Event {
	return Event{V: EventVersion, Time: t, Kind: KindAccount, Mode: ModeShared, Account: &AccountPayload{Account: account, Method: UsageEndpoint,
		FiveHour: &five, Weekly: &week, FiveHourReset: &reset, WeeklyReset: &reset}}
}

// spent is a usage event of session name's transcript, spend so far.
func spent(t time.Time, name, account, transcript string, spend int64) Event {
	return Event{V: EventVersion, Time: t, Session: name, Kind: KindUsage, Mode: ModeLive,
		Usage: &UsagePayload{Transcript: transcript, Account: account, Calls: 1, Spend: spend}}
}

func reportOf(t *testing.T, reps []AccountReport, account string) AccountReport {
	t.Helper()
	for _, r := range reps {
		if r.Account == account {
			return r
		}
	}
	t.Fatalf("no report for %q in %+v", account, reps)
	return AccountReport{}
}

// Readings across a weekly reset: the week's line and the burn are the
// new week's alone, and the projection runs from the new week's rate to
// its reset, flagged at the limit.
func TestAccountReportAcrossWeeklyReset(t *testing.T) {
	cfg := ledgerConfig()
	reset1 := pageAt.Add(-2 * time.Hour)
	reset2 := reset1.Add(weekLength)
	st := NewLogState(cfg, ModeLive)
	var evs []Event
	// The old week ends high.
	evs = append(evs, reading(reset1.Add(-time.Hour), "a", 50, 85, reset1), reading(reset1.Add(-30*time.Minute), "a", 60, 88, reset1))
	// The new week, read every 15 minutes, rises 1% an hour.
	for i := 0; i <= 8; i++ {
		at := reset1.Add(time.Duration(i) * 15 * time.Minute)
		evs = append(evs, reading(at, "a", 10, float64(i)/4, reset2))
	}
	st.Fold(evs)
	rep := reportOf(t, AccountReports(cfg, st, pageAt, time.UTC), "a")
	if rep.Weekly == nil || *rep.Weekly != 2 || rep.WeeklyReset == nil || !rep.WeeklyReset.Equal(reset2) || rep.Stale {
		t.Fatalf("latest %+v", rep)
	}
	if len(rep.Week) != 9 {
		t.Fatalf("week %+v", rep.Week)
	}
	for _, p := range rep.Week {
		if p.At.Before(reset1) || p.Weekly == nil || *p.Weekly > 2 {
			t.Fatalf("an old week's point %+v", p)
		}
	}
	if r := rep.Burn.WeeklyPerHour; r == nil || *r != 1 {
		t.Fatalf("burn %+v", rep.Burn)
	}
	// 2% now and 1% an hour for the 166 hours left: over the limit.
	if rep.Projected == nil || *rep.Projected != 168 || !rep.Over {
		t.Fatalf("projected %v, over %v", rep.Projected, rep.Over)
	}
	// A slower week is projected under it.
	slow := NewLogState(cfg, ModeLive).Fold([]Event{reading(reset1, "a", 1, 10, reset2), reading(reset1.Add(30*time.Minute), "a", 1, 10.1, reset2),
		reading(reset1.Add(time.Hour), "a", 1, 10.1, reset2), reading(reset1.Add(90*time.Minute), "a", 1, 10.2, reset2), reading(reset1.Add(2*time.Hour), "a", 1, 10.2, reset2)})
	if rep := reportOf(t, AccountReports(cfg, slow, pageAt, time.UTC), "a"); rep.Projected == nil || *rep.Projected >= 90 || rep.Over {
		t.Fatalf("slow week %+v", rep)
	}
}

// A session started on one account and restarted on another: each
// transcript's spend goes under the account it ran on, the review it ran
// and every job under theirs, and a spend logged with no account is the
// unknown account's, never a guess.
func TestAccountReportSessionRestartedOnAnotherAccount(t *testing.T) {
	cfg := ledgerConfig()
	t0 := pageAt.Add(-3 * time.Hour)
	st := NewLogState(cfg, ModeLive)
	five := 1.0
	reset := pageAt.Add(48 * time.Hour)
	st.Fold([]Event{
		reading(t0, "a", five, 5, reset), reading(t0, "b", five, 5, reset),
		{V: EventVersion, Time: t0, Session: "w1", Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: "i1", Action: SessionStart, Repository: "widgets", Kind: KindWork, Account: "a", By: "agentd"}},
		spent(t0.Add(time.Minute), "w1", "a", "t1", 100),
		spent(t0.Add(2*time.Minute), "w1", "a", "t1", 300),
		{V: EventVersion, Time: t0.Add(3 * time.Minute), Session: "w1", Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: "i1", Action: SessionEnd, Repository: "widgets", By: "agentd"}},
		{V: EventVersion, Time: t0.Add(4 * time.Minute), Session: "w1", Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: "i2", Action: SessionStart, Repository: "widgets", Kind: KindWork, Account: "b", By: "agentd"}},
		spent(t0.Add(5*time.Minute), "w1", "b", "t2", 50),
		spent(t0.Add(6*time.Minute), "w1", "b", "t2", 250),
		// Measured again unchanged: nothing more is charged.
		spent(t0.Add(7*time.Minute), "w1", "b", "t2", 250),
		{V: EventVersion, Time: t0.Add(8 * time.Minute), Session: "r1", Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: "i3", Action: SessionStart, Repository: "widgets", Kind: KindReviewer, Account: "b", By: "agentd"}},
		spent(t0.Add(9*time.Minute), "r1", "b", "t3", 40),
		{V: EventVersion, Time: t0.Add(10 * time.Minute), Kind: KindTriage, Mode: ModeShared, Triage: &TriagePayload{Usage: &JobUsage{InputTokens: 10, OutputTokens: 2, Account: "a"}}},
		{V: EventVersion, Time: t0.Add(11 * time.Minute), Session: "w1", Kind: KindStatus, Mode: ModeLive, Status: &StatusPayload{Usage: &JobUsage{InputTokens: 7}}},
		// From before the account was kept.
		spent(t0.Add(12*time.Minute), "old", "", "t4", 9),
		// A shadow run's measure of the same session is not this run's.
		{V: EventVersion, Time: t0.Add(13 * time.Minute), Session: "w1", Kind: KindUsage, Mode: ModeShadow, Usage: &UsagePayload{Transcript: "t2", Account: "b", Spend: 9999}},
	})
	reps := AccountReports(cfg, st, pageAt, time.UTC)
	a, b, unknown := reportOf(t, reps, "a"), reportOf(t, reps, "b"), reportOf(t, reps, AccountUnknown)
	if a.Today.Total != 320 || len(a.Today.Sessions) != 1 || a.Today.Sessions[0] != (NamedSpend{"w1", 300}) ||
		len(a.Today.Jobs) != 1 || a.Today.Jobs[0] != (NamedSpend{SpendTriage, 20}) {
		t.Fatalf("a %+v", a.Today)
	}
	if b.ThisWeek.Total != 290 || len(b.ThisWeek.Sessions) != 1 || b.ThisWeek.Sessions[0] != (NamedSpend{"w1", 250}) ||
		len(b.ThisWeek.Jobs) != 1 || b.ThisWeek.Jobs[0] != (NamedSpend{SpendReview, 40}) {
		t.Fatalf("b %+v", b.ThisWeek)
	}
	if unknown.Today.Total != 16 || unknown.ReadAt != nil || len(unknown.Week) != 0 {
		t.Fatalf("unknown %+v", unknown)
	}
	if b.Burn.Tokens != 290 || b.Burn.TokensPerHour != 48 {
		t.Fatalf("b burn %+v", b.Burn)
	}
	// Restarted on b, a fallback of widgets: the ledger says so, naming
	// the latest session started on it.
	if b.Fallback == nil || b.Fallback.Session != "r1" || a.Fallback != nil {
		t.Fatalf("fallback a %+v, b %+v", a.Fallback, b.Fallback)
	}
}

// The top five sessions are named, the rest summed; a day starts at
// midnight where the owner is.
func TestAccountReportTopSessions(t *testing.T) {
	cfg := ledgerConfig()
	st := NewLogState(cfg, ModeLive)
	var evs []Event
	for i, n := range []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7"} {
		evs = append(evs, spent(pageAt.Add(-time.Duration(i+1)*time.Minute), n, "a", "t-"+n, int64(100*(i+1))))
	}
	evs = append(evs, spent(pageAt.Add(-13*time.Hour), "s9", "a", "t-s9", 5)) // yesterday, at 23:00
	st.Fold(evs)
	rep := reportOf(t, AccountReports(cfg, st, pageAt, time.UTC), "a")
	if len(rep.Today.Sessions) != 5 || rep.Today.Sessions[0].Name != "s7" || rep.Today.Others != 300 || rep.Today.Total != 2800 {
		t.Fatalf("today %+v", rep.Today)
	}
	if rep.ThisWeek.Total != 2805 {
		t.Fatalf("week %+v", rep.ThisWeek)
	}
	west := time.FixedZone("west", -13*3600) // still the 22nd, since 13:00 UTC
	if rep := reportOf(t, AccountReports(cfg, st, pageAt, west), "a"); rep.Today.Total != 2805 {
		t.Fatalf("today west %+v", rep.Today)
	}
}

// A missing reading is a gap in the week's line, never a zero; a failed
// one is a gap too; the rate starts again after a gap, and a stale
// latest reading projects nothing.
func TestAccountReportMissingReading(t *testing.T) {
	cfg := ledgerConfig()
	reset := pageAt.Add(72 * time.Hour)
	t0 := pageAt.Add(-5 * time.Hour)
	st := NewLogState(cfg, ModeLive)
	st.Fold([]Event{
		reading(t0, "a", 1, 20, reset),
		reading(t0.Add(15*time.Minute), "a", 1, 21, reset),
		{V: EventVersion, Time: t0.Add(30 * time.Minute), Kind: KindAccount, Mode: ModeShared, Account: &AccountPayload{Account: "a", Error: "usage endpoint: HTTP 500"}},
		// Nothing for three hours.
		reading(t0.Add(3*time.Hour+30*time.Minute), "a", 1, 30, reset),
		reading(t0.Add(4*time.Hour), "a", 1, 30.5, reset),
		reading(t0.Add(4*time.Hour+30*time.Minute), "a", 1, 31, reset),
	})
	rep := reportOf(t, AccountReports(cfg, st, pageAt, time.UTC), "a")
	var gaps, zeros int
	for _, p := range rep.Week {
		switch {
		case p.Weekly == nil:
			gaps++
		case *p.Weekly == 0:
			zeros++
		}
	}
	if gaps != 1 || zeros != 0 || rep.Week[2].Weekly != nil {
		t.Fatalf("week %+v", rep.Week)
	}
	// From 30% to 31% over the hour since the gap, not from 20%.
	if r := rep.Burn.WeeklyPerHour; r == nil || *r != 1 {
		t.Fatalf("burn %+v", rep.Burn)
	}
	if rep.Projected == nil || *rep.Projected != 103 || !rep.Over {
		t.Fatalf("projected %v", rep.Projected)
	}
	// An hour on with no reading: stale, a gap at the end, no projection.
	later := pageAt.Add(time.Hour)
	rep = reportOf(t, AccountReports(cfg, st, later, time.UTC), "a")
	if !rep.Stale || rep.Projected != nil || rep.Over || rep.Burn.WeeklyPerHour != nil || rep.Week[len(rep.Week)-1].Weekly != nil {
		t.Fatalf("stale %+v", rep)
	}
	// An account never read has no numbers at all, and no line.
	if b := reportOf(t, AccountReports(cfg, st, pageAt, time.UTC), "b"); b.Weekly != nil || b.ReadAt != nil || len(b.Week) != 0 || b.Projected != nil {
		t.Fatalf("b %+v", b)
	}
}

// A session placed on a fallback account is told to its repository's
// console once, naming the session, however many passes follow.
func TestFallbackUseNoticed(t *testing.T) {
	h, f := usageHarness(t)
	f.set(tokenA, 95, 40)
	h.ticks(1, time.Minute)
	name := h.startSession("fix/spin", KindWork, 7)
	h.ticks(30, time.Minute)
	var notices []MessagePayload
	for _, e := range h.events() {
		if e.Kind == KindMessage && e.Message.Account != "" {
			notices = append(notices, *e.Message)
		}
	}
	if len(notices) != 1 || notices[0].Account != "b" || notices[0].Console != "widgets" || notices[0].Kind != "info" ||
		!strings.Contains(notices[0].Text, name) || !strings.Contains(notices[0].Text, "fallback account b") {
		t.Fatalf("notices %+v", notices)
	}
}

// A projection over the limit is told once per noticeEvery, naming the
// sessions using the account, to the repository that used it most; a
// shadow run tells nobody.
func TestProjectionNoticedOncePerWindow(t *testing.T) {
	h, _ := usageHarness(t)
	now := h.clock.Now()
	reset := now.Add(100 * time.Hour)
	var evs []Event
	for i := 0; i <= 8; i++ {
		evs = append(evs, reading(now.Add(-2*time.Hour+time.Duration(i)*15*time.Minute), "a", 10, 40+float64(i), reset))
	}
	evs = append(evs,
		Event{V: EventVersion, Time: now.Add(-time.Hour), Session: "w1", Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: "i1", Action: SessionStart, Repository: "widgets", Kind: KindWork, Account: "a", By: "agentd"}},
		spent(now.Add(-30*time.Minute), "w1", "a", "t1", 5000))
	h.sup.st.Fold(evs)
	h.sup.noticeAccounts(now)
	h.sup.noticeAccounts(now.Add(time.Hour))
	notices := func() []MessagePayload {
		var out []MessagePayload
		for _, e := range h.events() {
			if e.Kind == KindMessage && e.Message.Account == "a" {
				out = append(out, *e.Message)
			}
		}
		return out
	}
	ns := notices()
	if len(ns) != 1 || ns[0].Console != "widgets" || !strings.Contains(ns[0].Text, "projected to reach") || !strings.Contains(ns[0].Text, "w1 (5000 tokens)") {
		t.Fatalf("notices %+v", ns)
	}
	h.sup.noticeAccounts(now.Add(noticeEvery))
	if len(notices()) != 1 {
		t.Fatalf("told again with no new readings: %+v", notices())
	}
	h.sup.shadow = true
	h.sup.st.Ledger.Noticed = map[string]time.Time{}
	h.sup.noticeAccounts(now)
	if len(notices()) != 1 {
		t.Fatalf("a shadow run told: %+v", notices())
	}
}

// agentd accounts --report prints the reports from the log, as text or
// JSON, reading no account.
func TestAccountsReportCommand(t *testing.T) {
	h, _ := usageHarness(t)
	h.ticks(2, time.Minute)
	logPath := filepath.Join(h.dir, "events.jsonl")
	if err := os.WriteFile(logPath, h.buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.dir, "agentd.json")
	writeFile(t, path, `{"event_log": "`+logPath+`", "transcript_dir": ""}`)
	var out bytes.Buffer
	if err := accountsCmd([]string{"-config", path, "--report"}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "a: 5h 12%") || !strings.Contains(s, "today:") || !strings.Contains(s, "burn over 6h") {
		t.Fatalf("text:\n%s", s)
	}
	out.Reset()
	if err := accountsCmd([]string{"-config", path, "--report", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var reps []AccountReport
	if err := json.Unmarshal(out.Bytes(), &reps); err != nil || len(reps) < 3 || reps[0].Account != "a" || reps[0].Weekly == nil {
		t.Fatalf("json %v:\n%s", err, out.String())
	}
	if err := accountsCmd([]string{"-config", path, "--json"}, &out); err == nil {
		t.Fatal("--json without --report accepted")
	}
}

// A session's measured spend is logged with the account it runs on, and
// the report charges it there.
func TestUsageRecordsTheAccount(t *testing.T) {
	h, id := contextHarness(t, false)
	h.cfg.Accounts = map[string]Account{"b": {TokenFile: filepath.Join(h.dir, "token-b")}}
	s := h.cfg.Sessions["s"]
	s.Account = "b"
	h.cfg.Sessions["s"] = s
	h.transcript(id, usageLine("m1", 10, 150000, 0, 5, false))
	h.ticks(2, time.Minute)
	var accounts []string
	for _, e := range h.events() {
		if e.Kind == KindUsage {
			accounts = append(accounts, e.Usage.Account)
		}
	}
	if len(accounts) == 0 || accounts[0] != "b" {
		t.Fatalf("usage accounts %q", accounts)
	}
	rep := reportOf(t, AccountReports(h.cfg, h.logState(), h.clock.Now(), time.UTC), "b")
	if rep.Today.Total != spendOf(h.sup.Tracks()["s"].Usage[id]) || rep.Today.Sessions[0].Name != "s" {
		t.Fatalf("b today %+v", rep.Today)
	}
}
