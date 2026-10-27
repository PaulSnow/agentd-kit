package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// Token use per account (#64). The fold keeps a ledger of the last
// ledgerKeep: each account's usage readings, and every token agentd
// measured, charged to the account it ran on. A session's spend goes
// under the account its usage event names, the one it was started with;
// a job's under the account its usage names, the jobs' account. Where
// none was recorded the spend is the unknown account's, never a guess.
// The account report reads it: the latest reading, the week's weekly
// line, the tokens of today and of the week, the burn of the last
// burnWindow and the weekly percent it comes to at the reset.

// AccountUnknown is the account of a spend whose account was not
// recorded, or that ran on the default login.
const AccountUnknown = "unknown"

// ledgerKeep is how long the ledger keeps a reading or a spend: a week
// and a day, so a week that began before a late reading is whole.
const ledgerKeep = 8 * 24 * time.Hour

// burnWindow is how far back the burn rate looks, and noticeEvery how
// often an account is told about at most.
const (
	burnWindow  = 6 * time.Hour
	noticeEvery = 6 * time.Hour
)

// weekLength is a weekly window's length.
const weekLength = 7 * 24 * time.Hour

// topSessions is how many sessions a report names by name.
const topSessions = 5

// Job kinds a spend is charged to; a session's own spend is
// SpendSession.
const (
	SpendSession = "session"
	SpendReview  = "review"
	SpendJudge   = "judge"
	SpendStatus  = "status"
	SpendCurator = "curator"
	SpendThread  = "thread"
	SpendExpert  = "ask_expert"
	SpendTriage  = "triage"
)

// LedgerReading is one account reading as the ledger keeps it; a failed
// reading has no numbers.
type LedgerReading struct {
	At          time.Time
	FiveHour    *float64
	Weekly      *float64
	WeeklyReset *time.Time
}

// LedgerSpend is tokens charged to an account, input-equivalent as a
// session's spend is.
type LedgerSpend struct {
	At         time.Time
	Account    string
	Repository string
	Session    string
	Kind       string
	Tokens     int64
}

// FallbackUse is a session placed on an account that is not the first of
// its repository's fallback.
type FallbackUse struct {
	At         time.Time
	Repository string
	Session    string
}

// AccountLedger is what the fold keeps for the account reports.
type AccountLedger struct {
	Readings map[string][]LedgerReading // by account, oldest first
	Spends   []LedgerSpend              // oldest first
	Fallback map[string]FallbackUse     // each account's latest fallback placement
	Noticed  map[string]time.Time       // each account's latest notice
	seen     map[string]int64           // each session's transcript's spend so far
}

func newAccountLedger() *AccountLedger {
	return &AccountLedger{Readings: map[string][]LedgerReading{}, Fallback: map[string]FallbackUse{},
		Noticed: map[string]time.Time{}, seen: map[string]int64{}}
}

// jobTokens is a job's usage as input-equivalent tokens.
func jobTokens(u *JobUsage) int64 {
	return spendOf(UsagePayload{Input: u.InputTokens, CacheWrite: u.CacheCreationTokens, CacheRead: u.CacheReadTokens, Output: u.OutputTokens})
}

// orUnknown is account, or the unknown account.
func orUnknown(account string) string {
	if account == "" {
		return AccountUnknown
	}
	return account
}

// sessionOf is the repository and kind of session name as the fold
// knows it now.
func (st *LogState) sessionOf(name string) (repo, kind string) {
	if p, ok := st.Runtime[name]; ok {
		return p.Repository, p.Kind
	}
	if st.cfg != nil {
		s := st.cfg.Sessions[name]
		return s.Repository, s.Kind
	}
	return "", ""
}

// applyLedger notes what e tells the ledger. A worker's state keeps none.
func (st *LogState) applyLedger(e Event) {
	l := st.Ledger
	if l == nil {
		return
	}
	job := func(kind, repo string, u *JobUsage) {
		if u != nil {
			l.spend(LedgerSpend{At: e.Time, Account: orUnknown(u.Account), Repository: repo, Session: e.Session, Kind: kind, Tokens: jobTokens(u)})
		}
	}
	switch {
	case e.Account != nil:
		p := e.Account
		r := LedgerReading{At: e.Time}
		if p.Error == "" {
			r.FiveHour, r.Weekly, r.WeeklyReset = p.FiveHour, p.Weekly, p.WeeklyReset
		}
		l.Readings[p.Account] = append(l.Readings[p.Account], r)
	case e.Usage != nil && e.Session != "":
		u := e.Usage
		key := e.Session + "\n" + u.Transcript
		prev, seen := l.seen[key]
		l.seen[key] = u.Spend
		delta := u.Spend - prev
		switch {
		case !seen && u.Guessed:
			// A guessed transcript is counted from when it was first
			// seen: what it spent before may not be the session's.
			delta = 0
		case delta < 0:
			delta = 0
		}
		if delta == 0 {
			return
		}
		repo, kind := st.sessionOf(e.Session)
		k := SpendSession
		switch kind {
		case KindReviewer:
			k = SpendReview
		case KindJudge:
			k = SpendJudge
		}
		l.spend(LedgerSpend{At: e.Time, Account: orUnknown(u.Account), Repository: repo, Session: e.Session, Kind: k, Tokens: delta})
	case e.Triage != nil:
		job(SpendTriage, "", e.Triage.Usage)
	case e.Card != nil:
		job(SpendCurator, e.Card.Repository, e.Card.Usage)
	case e.Query != nil:
		job(SpendExpert, e.Query.Repository, e.Query.Usage)
	case e.Status != nil:
		repo, _ := st.sessionOf(e.Session)
		job(SpendStatus, repo, e.Status.Usage)
	case e.Route != nil && e.Route.Agent != nil:
		repo := ""
		if in := st.Instructions[e.Route.Instruction]; in != nil && len(in.Repositories) > 0 {
			repo = in.Repositories[0]
		}
		job(SpendThread, repo, e.Route.Agent.Usage)
	case e.Runtime != nil && e.Kind == KindSession && (e.Runtime.Action == SessionStart || e.Runtime.Action == SessionPlace):
		p := e.Runtime
		if r, ok := st.Runtime[e.Session]; !ok || r.ID != p.ID || r.Account != p.Account || p.Account == "" || st.cfg == nil {
			return // one the fold ignored
		}
		rules, _ := effectivePolicy(st.cfg, st, p.Repository)
		if fb := rules.Sessions.Fallback; len(fb) > 1 && fb[0] != p.Account && contains(fb, p.Account) {
			l.Fallback[p.Account] = FallbackUse{At: e.Time, Repository: p.Repository, Session: e.Session}
		}
	case e.Message != nil && e.Message.Account != "":
		l.Noticed[e.Message.Account] = e.Time
	}
	l.prune(e.Time)
}

// spend charges s, if it is anything.
func (l *AccountLedger) spend(s LedgerSpend) {
	if s.Tokens > 0 {
		l.Spends = append(l.Spends, s)
	}
}

// prune drops what is older than ledgerKeep before now.
func (l *AccountLedger) prune(now time.Time) {
	cut := now.Add(-ledgerKeep)
	i := 0
	for i < len(l.Spends) && l.Spends[i].At.Before(cut) {
		i++
	}
	if i > 0 {
		l.Spends = append([]LedgerSpend(nil), l.Spends[i:]...)
	}
	for a, rs := range l.Readings {
		j := 0
		for j < len(rs) && rs[j].At.Before(cut) {
			j++
		}
		if j > 0 {
			l.Readings[a] = append([]LedgerReading(nil), rs[j:]...)
		}
	}
}

// AccountReport is one account's token use (#64). A figure that is not
// known is absent, never zero.
type AccountReport struct {
	Account       string     `json:"account"`
	At            time.Time  `json:"at"` // when the report was made
	ReadAt        *time.Time `json:"read_at,omitempty"`
	Stale         bool       `json:"stale,omitempty"`
	Error         string     `json:"error,omitempty"`
	FiveHour      *float64   `json:"five_hour,omitempty"`
	FiveHourReset *time.Time `json:"five_hour_reset,omitempty"`
	Weekly        *float64   `json:"weekly,omitempty"`
	WeeklyReset   *time.Time `json:"weekly_reset,omitempty"`
	// Week is the weekly percent over the current week, oldest first; a
	// point without a value is a gap.
	Week     []WeekPoint `json:"week"`
	Today    TokenSplit  `json:"today"`
	ThisWeek TokenSplit  `json:"this_week"`
	Burn     Burn        `json:"burn"`
	// Projected is the weekly percent at the reset at the burn's rate;
	// Over says it reaches Limit.
	Projected *float64     `json:"projected,omitempty"`
	Limit     float64      `json:"limit"`
	Over      bool         `json:"over,omitempty"`
	Fallback  *FallbackUse `json:"fallback,omitempty"`
}

// WeekPoint is one reading of the week's line.
type WeekPoint struct {
	At     time.Time `json:"at"`
	Weekly *float64  `json:"weekly,omitempty"`
}

// TokenSplit is tokens over a span: the sessions' by name, the largest
// topSessions, the rest together, and the jobs' by kind.
type TokenSplit struct {
	Since    time.Time    `json:"since"`
	Total    int64        `json:"total"`
	Sessions []NamedSpend `json:"sessions"`
	Others   int64        `json:"other_sessions,omitempty"`
	Jobs     []NamedSpend `json:"jobs"`
}

// NamedSpend is tokens under a name.
type NamedSpend struct {
	Name   string `json:"name"`
	Tokens int64  `json:"tokens"`
}

// Burn is the use over the last burnWindow: the tokens charged, and the
// weekly percent's rise per hour when the readings say it.
type Burn struct {
	Hours         float64  `json:"hours"`
	Tokens        int64    `json:"tokens"`
	TokensPerHour int64    `json:"tokens_per_hour"`
	WeeklyPerHour *float64 `json:"weekly_per_hour,omitempty"`
}

// sameWeek says whether two weekly resets are the same window's: the
// endpoint's reset can move by seconds between readings.
func sameWeek(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	d := a.Sub(*b)
	return d > -time.Hour && d < time.Hour
}

// AccountReports is a report per account: each configured one, each with
// a reading, and the unknown account when anything is charged to it.
func AccountReports(cfg *Config, st *LogState, now time.Time, loc *time.Location) []AccountReport {
	names := map[string]bool{}
	for a := range cfg.Accounts {
		names[a] = true
	}
	for a := range st.Accounts {
		names[a] = true
	}
	l := st.Ledger
	if l == nil {
		l = newAccountLedger()
	}
	for _, s := range l.Spends {
		names[s.Account] = true
	}
	var out []AccountReport
	for _, a := range sortedKeys(names) {
		out = append(out, accountReport(cfg, st, l, a, now, loc))
	}
	return out
}

// accountReport is account's report.
func accountReport(cfg *Config, st *LogState, l *AccountLedger, account string, now time.Time, loc *time.Location) AccountReport {
	if loc == nil {
		loc = time.Local
	}
	u := cfg.Usage
	rep := AccountReport{Account: account, At: now, Limit: u.Limit, Week: []WeekPoint{}}
	stale := usageStale * u.Every.Duration
	if stale <= 0 {
		stale = usageStale * DefaultUsageEvery
	}
	if r, ok := st.Accounts[account]; ok {
		if !r.At.IsZero() {
			at := r.At
			rep.ReadAt = &at
			rep.FiveHour, rep.FiveHourReset, rep.Weekly, rep.WeeklyReset = r.FiveHour, r.FiveHourReset, r.Weekly, r.WeeklyReset
		}
		rep.Stale = r.At.IsZero() || now.Sub(r.At) > stale
		if r.ErrorAt.After(r.At) && !confidentialMatcher(cfg)(r.Error) {
			rep.Error = r.Error
		}
	}
	if fb, ok := l.Fallback[account]; ok && now.Sub(fb.At) < weekLength {
		rep.Fallback = &fb
	}
	// The week: from the latest reset a week back, else the last week.
	weekStart := now.Add(-weekLength)
	if rep.WeeklyReset != nil {
		weekStart = rep.WeeklyReset.Add(-weekLength)
	}
	var prev *LedgerReading
	for i, r := range l.Readings[account] {
		if r.At.Before(weekStart) || r.At.After(now) {
			continue
		}
		if r.Weekly != nil && !sameWeek(r.WeeklyReset, rep.WeeklyReset) {
			continue // an earlier week's
		}
		// A reading missing for longer than one stands for is a gap.
		if prev != nil && r.At.Sub(prev.At) > stale && prev.Weekly != nil {
			rep.Week = append(rep.Week, WeekPoint{At: prev.At.Add(stale)})
		}
		rep.Week = append(rep.Week, WeekPoint{At: r.At, Weekly: r.Weekly})
		prev = &l.Readings[account][i]
	}
	if prev != nil && prev.Weekly != nil && now.Sub(prev.At) > stale {
		rep.Week = append(rep.Week, WeekPoint{At: prev.At.Add(stale)})
	}
	y, m, d := now.In(loc).Date()
	rep.Today = splitSpends(l.Spends, account, time.Date(y, m, d, 0, 0, 0, 0, loc), now)
	rep.ThisWeek = splitSpends(l.Spends, account, weekStart, now)
	rep.Burn = burnOf(l, account, rep.WeeklyReset, now, stale)
	if rate := rep.Burn.WeeklyPerHour; rate != nil && rep.Weekly != nil && rep.WeeklyReset != nil && !rep.Stale {
		left := rep.WeeklyReset.Sub(now).Hours()
		if left < 0 {
			left = 0
		}
		p := math.Round((*rep.Weekly+math.Max(*rate, 0)*left)*10) / 10
		rep.Projected = &p
		rep.Over = u.Limit > 0 && p >= u.Limit
	}
	return rep
}

// splitSpends is account's tokens from since to now.
func splitSpends(spends []LedgerSpend, account string, since, now time.Time) TokenSplit {
	t := TokenSplit{Since: since, Sessions: []NamedSpend{}, Jobs: []NamedSpend{}}
	sessions, jobs := map[string]int64{}, map[string]int64{}
	for _, s := range spends {
		if s.Account != account || s.At.Before(since) || s.At.After(now) {
			continue
		}
		t.Total += s.Tokens
		if s.Kind == SpendSession {
			sessions[s.Session] += s.Tokens
		} else {
			jobs[s.Kind] += s.Tokens
		}
	}
	byTokens := func(m map[string]int64) []NamedSpend {
		out := []NamedSpend{}
		for k, v := range m {
			out = append(out, NamedSpend{Name: k, Tokens: v})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Tokens != out[j].Tokens {
				return out[i].Tokens > out[j].Tokens
			}
			return out[i].Name < out[j].Name
		})
		return out
	}
	t.Sessions, t.Jobs = byTokens(sessions), byTokens(jobs)
	if len(t.Sessions) > topSessions {
		for _, s := range t.Sessions[topSessions:] {
			t.Others += s.Tokens
		}
		t.Sessions = t.Sessions[:topSessions]
	}
	return t
}

// burnOf is account's burn over the last burnWindow. The weekly rate runs
// from the reading in force when the window began, or the first in it,
// to the latest, within the current week and with no gap, over at least
// an hour; else it is not known.
func burnOf(l *AccountLedger, account string, reset *time.Time, now time.Time, stale time.Duration) Burn {
	b := Burn{Hours: burnWindow.Hours()}
	from := now.Add(-burnWindow)
	for _, s := range l.Spends {
		if s.Account == account && !s.At.Before(from) && !s.At.After(now) {
			b.Tokens += s.Tokens
		}
	}
	b.TokensPerHour = int64(math.Round(float64(b.Tokens) / b.Hours))
	var base, last *LedgerReading
	rs := l.Readings[account]
	for i := range rs {
		r := &rs[i]
		if r.At.After(now) {
			break
		}
		if r.Weekly == nil || !sameWeek(r.WeeklyReset, reset) || last != nil && r.At.Sub(last.At) > stale {
			// A failed reading, another week's, or a gap: the rate starts
			// again after it.
			base, last = nil, nil
			if r.Weekly == nil || !sameWeek(r.WeeklyReset, reset) {
				continue
			}
		}
		if base == nil || !r.At.After(from) {
			base = r
		}
		last = r
	}
	if base == nil || last == nil || now.Sub(last.At) > stale {
		return b
	}
	start := base.At
	if start.Before(from) {
		start = from
	}
	if h := last.At.Sub(start).Hours(); h >= 1 {
		rate := math.Round((*last.Weekly-*base.Weekly)/h*100) / 100
		b.WeeklyPerHour = &rate
	}
	return b
}

// noticeAccounts tells the repository most responsible, once per
// noticeEvery an account at most, when the account's projected week
// reaches the limit, or a fallback account was placed on, and which
// sessions use it. Shadow mode tells nobody.
func (s *Supervisor) noticeAccounts(now time.Time) {
	if s.shadow || s.st.Ledger == nil || !s.cfg.Usage.Enabled {
		return
	}
	l := s.st.Ledger
	for _, rep := range AccountReports(s.cfg, s.st, now, nil) {
		if rep.Account == AccountUnknown || now.Sub(l.Noticed[rep.Account]) < noticeEvery {
			continue
		}
		var why string
		fb, fallback := l.Fallback[rep.Account]
		switch {
		case rep.Over:
			why = fmt.Sprintf("account %s is projected to reach %.0f%% of its week by the reset at %s (now %.0f%%, rising %.2f%% an hour; the limit is %.0f%%)",
				rep.Account, *rep.Projected, rep.WeeklyReset.UTC().Format("2006-01-02 15:04Z"), *rep.Weekly, *rep.Burn.WeeklyPerHour, rep.Limit)
		case fallback && now.Sub(fb.At) < noticeEvery && fb.At.After(l.Noticed[rep.Account]):
			why = fmt.Sprintf("fallback account %s is in use: %s was placed on it", rep.Account, fb.Session)
		default:
			continue
		}
		repo, users := l.users(rep.Account, now)
		if repo == "" && fallback {
			repo = fb.Repository
		}
		if repo == "" {
			continue
		}
		text := why + "."
		if len(users) > 0 {
			text += " Sessions using it in the last " + burnWindow.String() + ": " + strings.Join(users, ", ") + "."
		}
		m := &MessagePayload{ID: "U" + shortHash(HashPane(rep.Account + "\n" + now.UTC().Format(time.RFC3339Nano)))[:8],
			Console: s.cfg.RepositoryConsole(repo), Kind: "info", From: "agentd", Text: text, Account: rep.Account}
		if i := s.confidentialMatch(text, ""); i > 0 {
			m.Text, m.Withheld = "", i
		}
		s.emit("", Event{Kind: KindMessage, Mode: ModeShared, Message: m})
	}
}

// users is the repository whose sessions and jobs charged account the
// most over the last burnWindow, and the sessions charged to it then,
// the largest first, with their tokens.
func (l *AccountLedger) users(account string, now time.Time) (string, []string) {
	repos, sessions := map[string]int64{}, map[string]int64{}
	for _, s := range l.Spends {
		if s.Account != account || now.Sub(s.At) > burnWindow {
			continue
		}
		if s.Repository != "" {
			repos[s.Repository] += s.Tokens
		}
		if s.Kind == SpendSession || s.Kind == SpendReview || s.Kind == SpendJudge {
			sessions[s.Session] += s.Tokens
		}
	}
	repo := ""
	for _, r := range sortedKeys(repos) {
		if repo == "" || repos[r] > repos[repo] {
			repo = r
		}
	}
	names := sortedKeys(sessions)
	sort.SliceStable(names, func(i, j int) bool { return sessions[names[i]] > sessions[names[j]] })
	var out []string
	for _, n := range names {
		out = append(out, fmt.Sprintf("%s (%s tokens)", n, tokens(sessions[n])))
	}
	return repo, out
}

// printAccountReports prints the account reports from the event log:
// agentd accounts --report [--json].
func printAccountReports(cfg *Config, asJSON bool, now time.Time, out io.Writer) error {
	events, _, err := ReadLog(cfg.EventLog)
	if err != nil {
		return err
	}
	st := NewLogState(cfg, ModeLive).Fold(events)
	reps := AccountReports(cfg, st, now, time.Local)
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(reps)
	}
	if len(reps) == 0 {
		return errors.New("no accounts are configured or logged")
	}
	for _, r := range reps {
		fmt.Fprint(out, accountReportText(r))
	}
	return nil
}

// accountReportText is a report in a few lines.
func accountReportText(r AccountReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", r.Account, usageText(AccountPayload{FiveHour: r.FiveHour, FiveHourReset: r.FiveHourReset, Weekly: r.Weekly, WeeklyReset: r.WeeklyReset}))
	switch {
	case r.ReadAt == nil:
		b.WriteString(" (never read)")
	case r.Stale:
		fmt.Fprintf(&b, " (stale: read %s)", r.ReadAt.UTC().Format("2006-01-02 15:04Z"))
	}
	if r.Error != "" {
		b.WriteString("; latest error: " + r.Error)
	}
	b.WriteString("\n")
	split := func(label string, t TokenSplit) {
		fmt.Fprintf(&b, "  %s: %s tokens", label, tokens(t.Total))
		var parts []string
		for _, s := range t.Sessions {
			parts = append(parts, s.Name+" "+tokens(s.Tokens))
		}
		if t.Others > 0 {
			parts = append(parts, "other sessions "+tokens(t.Others))
		}
		for _, j := range t.Jobs {
			parts = append(parts, j.Name+" "+tokens(j.Tokens))
		}
		if len(parts) > 0 {
			b.WriteString(" (" + strings.Join(parts, ", ") + ")")
		}
		b.WriteString("\n")
	}
	split("today", r.Today)
	split("this week", r.ThisWeek)
	fmt.Fprintf(&b, "  burn over %.0fh: %s tokens an hour", r.Burn.Hours, tokens(r.Burn.TokensPerHour))
	if r.Burn.WeeklyPerHour != nil {
		fmt.Fprintf(&b, ", weekly +%.2f%% an hour", *r.Burn.WeeklyPerHour)
	} else {
		b.WriteString(", weekly rate unknown")
	}
	b.WriteString("\n")
	if r.Projected != nil {
		fmt.Fprintf(&b, "  projected at the weekly reset: %.0f%%", *r.Projected)
		if r.Over {
			fmt.Fprintf(&b, " — OVER the %.0f%% limit", r.Limit)
		}
		b.WriteString("\n")
	}
	if r.Fallback != nil {
		fmt.Fprintf(&b, "  in use as a fallback: %s placed %s\n", r.Fallback.Session, r.Fallback.At.UTC().Format("2006-01-02 15:04Z"))
	}
	return b.String()
}

// accountsFlags reads the accounts command's flags.
func accountsFlags(args []string) (*Config, bool, bool, error) {
	fs := flag.NewFlagSet("accounts", flag.ContinueOnError)
	report := fs.Bool("report", false, "print each account's token use from the event log, reading nothing")
	asJSON := fs.Bool("json", false, "with --report, print the reports as JSON")
	cfg, _, err := loadConfigFlag(fs, args)
	return cfg, *report, *asJSON, err
}
