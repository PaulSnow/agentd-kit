package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Account usage (#39, building on #6). agentd reads each account's plan
// usage, the five-hour and weekly utilization with their resets and
// whether overage is on, and logs the numbers as shared account events.
// A session agentd starts with no account is placed on the first
// account of its repository's sessions.fallback with room left; when
// none has, its start waits, and the owner is told once.

// UsageConfig turns the readings on: every Every, each account is read
// through API, and an account is placed on only while both its
// utilizations are under Limit percent. Model is the model of the
// minimal request the header reading makes.
type UsageConfig struct {
	Enabled bool     `json:"enabled"`
	Every   Duration `json:"every"`
	Limit   float64  `json:"limit"`
	API     string   `json:"api"`
	Model   string   `json:"model"`
}

// Usage defaults.
const (
	DefaultUsageEvery = 15 * time.Minute
	DefaultUsageLimit = 90
	DefaultUsageAPI   = "https://api.anthropic.com"
	DefaultUsageModel = "claude-haiku-4-5"
)

// Account usage methods.
const (
	UsageAuto     = "auto"     // the endpoint, else the headers when the token may not read it
	UsageEndpoint = "endpoint" // the OAuth usage endpoint
	UsageHeaders  = "headers"  // the rate-limit headers of a minimal request
	UsageOff      = "off"
)

// usageStale is how many readings' intervals a reading stands for: an
// older one is unknown.
const usageStale = 3

// usageRelog is how long a reading that says the same is not logged
// again, at most: never so long that the logged reading goes stale.
const usageRelog = time.Hour

// relogAfter is how long a reading that says the same waits to be logged
// again under u.
func relogAfter(u UsageConfig) time.Duration {
	if half := usageStale * u.Every.Duration / 2; half < usageRelog {
		return half
	}
	return usageRelog
}

// usageAPIOK says whether api is a URL a token may be sent to: https, or
// http to this machine only.
func usageAPIOK(api string) bool {
	u, err := url.Parse(api)
	if err != nil || u.Host == "" {
		return false
	}
	switch h := u.Hostname(); {
	case u.Scheme == "https":
		return true
	case u.Scheme == "http":
		ip := net.ParseIP(h)
		return h == "localhost" || ip != nil && ip.IsLoopback()
	}
	return false
}

// AccountPayload is one reading of an account's usage: the numbers only,
// utilization in percent. Error is why the reading failed, with the token
// taken out and screened; the numbers are then absent.
type AccountPayload struct {
	Account       string     `json:"account"`
	Method        string     `json:"method,omitempty"`
	FiveHour      *float64   `json:"five_hour,omitempty"`
	FiveHourReset *time.Time `json:"five_hour_reset,omitempty"`
	Weekly        *float64   `json:"weekly,omitempty"`
	WeeklyReset   *time.Time `json:"weekly_reset,omitempty"`
	Overage       *bool      `json:"overage,omitempty"`
	Error         string     `json:"error,omitempty"`
}

// AccountRecord is an account's latest reading as the fold keeps it: the
// numbers of the latest reading that had them, when it was, and the
// latest error since, if any.
type AccountRecord struct {
	AccountPayload
	At      time.Time // when the numbers were read
	ErrorAt time.Time
}

// applyAccount folds a reading. A failed one keeps the numbers before it.
func (st *LogState) applyAccount(e Event) {
	if st.Accounts == nil {
		st.Accounts = map[string]AccountRecord{}
	}
	p := *e.Account
	r := st.Accounts[p.Account]
	if p.Error != "" {
		r.Account, r.Error, r.ErrorAt = p.Account, p.Error, e.Time
		st.Accounts[p.Account] = r
		return
	}
	st.Accounts[p.Account] = AccountRecord{AccountPayload: p, At: e.Time}
}

// usageHigh is the higher of a reading's utilizations, and whether it has
// both.
func (p AccountPayload) usageHigh() (float64, bool) {
	if p.FiveHour == nil || p.Weekly == nil {
		return 0, false
	}
	return math.Max(*p.FiveHour, *p.Weekly), true
}

// sameReading says whether a reading says what the record does: each
// utilization within a percent of it, the same resets, overage and method.
func (r AccountRecord) sameReading(p AccountPayload) bool {
	near := func(a, b *float64) bool {
		return a == nil && b == nil || a != nil && b != nil && math.Abs(*a-*b) < 1
	}
	tm := func(t *time.Time) string {
		if t == nil {
			return "-"
		}
		return t.UTC().Format(time.RFC3339)
	}
	b := func(v *bool) string {
		if v == nil {
			return "-"
		}
		return strconv.FormatBool(*v)
	}
	if p.Error != "" {
		return r.Error == p.Error
	}
	// After an error, a reading is news.
	return r.Error == "" && !r.At.IsZero() && near(r.FiveHour, p.FiveHour) && near(r.Weekly, p.Weekly) &&
		tm(r.FiveHourReset)+tm(r.WeeklyReset)+b(r.Overage)+r.Method == tm(p.FiveHourReset)+tm(p.WeeklyReset)+b(p.Overage)+p.Method
}

// errNoUsageScope is the endpoint refusing the token: a token without
// the profile scope, which the headers still serve.
var errNoUsageScope = errors.New("the token may not read the usage endpoint")

// accountToken reads an account's token: its token_file, else the OAuth
// access token in config_dir's .credentials.json. The caller must never
// keep, log or print it.
func accountToken(a Account) (string, error) {
	if a.TokenFile != "" {
		b, err := readRegular(a.TokenFile, 1<<16)
		if err != nil {
			// Not the error itself: it names the file's whole path.
			return "", fmt.Errorf("the token file %s cannot be read as a regular file", filepath.Base(a.TokenFile))
		}
		if tok := strings.TrimSpace(string(b)); tok != "" {
			return tok, nil
		}
		return "", errors.New("the token file is blank")
	}
	if a.ConfigDir == "" {
		return "", errors.New("the account has neither a token_file nor a config_dir")
	}
	b, err := readRegular(filepath.Join(a.ConfigDir, ".credentials.json"), 1<<16)
	if err != nil {
		return "", errors.New("the account's config_dir holds no readable .credentials.json")
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"` // milliseconds
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &creds); err != nil || strings.TrimSpace(creds.ClaudeAiOauth.AccessToken) == "" {
		return "", errors.New("the account's .credentials.json holds no OAuth access token")
	}
	// Claude Code refreshes the token only while it runs under the
	// account; agentd never refreshes it.
	if exp := creds.ClaudeAiOauth.ExpiresAt; exp > 0 && time.Now().After(time.UnixMilli(exp)) {
		return "", fmt.Errorf("the access token in .credentials.json expired at %s; Claude Code refreshes it when it next runs under the account", time.UnixMilli(exp).UTC().Format(time.RFC3339))
	}
	return strings.TrimSpace(creds.ClaudeAiOauth.AccessToken), nil
}

// planReader reads accounts' usage through one HTTP client.
type planReader struct {
	cfg    UsageConfig
	client *http.Client
}

func newPlanReader(cfg UsageConfig, timeout time.Duration) planReader {
	return planReader{cfg: cfg, client: &http.Client{Timeout: timeout,
		// A redirect could carry the token to another host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// read reads one account by its method. The payload's error, if any, has
// the token taken out; the token is in nothing it returns.
func (u planReader) read(name string, a Account) AccountPayload {
	p := AccountPayload{Account: name}
	tok, err := accountToken(a)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	method := a.Usage
	if method == "" {
		method = UsageAuto
	}
	switch method {
	case UsageEndpoint:
		err = u.endpoint(tok, &p)
	case UsageHeaders:
		err = u.headers(tok, &p)
	default:
		if err = u.endpoint(tok, &p); errors.Is(err, errNoUsageScope) {
			p = AccountPayload{Account: name}
			err = u.headers(tok, &p)
		}
	}
	if err != nil {
		p = AccountPayload{Account: name, Method: p.Method, Error: scrubToken(err.Error(), tok)}
	}
	return p
}

// scrubToken takes the token, and any long run of it, out of text, and
// bounds it.
func scrubToken(text, tok string) string {
	if tok != "" {
		text = strings.ReplaceAll(text, tok, "[token]")
		if len(tok) > 16 {
			text = strings.ReplaceAll(text, tok[:16], "[token]")
			text = strings.ReplaceAll(text, tok[len(tok)-16:], "[token]")
		}
	}
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	return text
}

// oauthBeta is the beta header an OAuth token needs.
const oauthBeta = "oauth-2025-04-20"

// endpoint reads the OAuth usage endpoint.
func (u planReader) endpoint(tok string, p *AccountPayload) error {
	p.Method = UsageEndpoint
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(u.cfg.API, "/")+"/api/oauth/usage", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("Accept", "application/json")
	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("usage endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w (HTTP %d)", errNoUsageScope, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("usage endpoint: HTTP %d: %s", resp.StatusCode, oneLine(scrubToken(string(body), tok), 200))
	}
	var doc struct {
		FiveHour *struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *string  `json:"resets_at"`
		} `json:"five_hour"`
		SevenDay *struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *string  `json:"resets_at"`
		} `json:"seven_day"`
		ExtraUsage *struct {
			IsEnabled *bool `json:"is_enabled"`
		} `json:"extra_usage"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("usage endpoint: unreadable answer: %w", err)
	}
	reset := func(s *string) *time.Time {
		if s == nil {
			return nil
		}
		t, err := time.Parse(time.RFC3339Nano, *s)
		if err != nil {
			return nil
		}
		t = t.UTC()
		return &t
	}
	if doc.FiveHour != nil {
		p.FiveHour, p.FiveHourReset = doc.FiveHour.Utilization, reset(doc.FiveHour.ResetsAt)
	}
	if doc.SevenDay != nil {
		p.Weekly, p.WeeklyReset = doc.SevenDay.Utilization, reset(doc.SevenDay.ResetsAt)
	}
	if doc.ExtraUsage != nil {
		p.Overage = doc.ExtraUsage.IsEnabled
	}
	if p.FiveHour == nil || p.Weekly == nil {
		return errors.New("usage endpoint: the answer has no five-hour or weekly utilization")
	}
	return nil
}

// claudeCodeSystem is the system line Claude Code sends, which an OAuth
// token's request carries.
const claudeCodeSystem = "You are Claude Code, Anthropic's official CLI for Claude."

// headers reads the unified rate-limit headers of a minimal request made
// as Claude Code makes it: one token of the configured model.
func (u planReader) headers(tok string, p *AccountPayload) error {
	p.Method = UsageHeaders
	body, _ := json.Marshal(map[string]any{
		"model": u.cfg.Model, "max_tokens": 1, "system": claudeCodeSystem,
		"messages": []map[string]string{{"role": "user", "content": "."}},
	})
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(u.cfg.API, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("anthropic-beta", oauthBeta)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("messages: %w", err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	h := resp.Header
	pct := func(k string) *float64 {
		v, err := strconv.ParseFloat(strings.TrimSpace(h.Get(k)), 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
		v = math.Round(v*1000) / 10 // a fraction, as percent
		return &v
	}
	at := func(k string) *time.Time {
		v, err := strconv.ParseInt(strings.TrimSpace(h.Get(k)), 10, 64)
		if err != nil || v <= 0 {
			return nil
		}
		t := time.Unix(v, 0).UTC()
		return &t
	}
	const pre = "anthropic-ratelimit-unified-"
	p.FiveHour, p.FiveHourReset = pct(pre+"5h-utilization"), at(pre+"5h-reset")
	p.Weekly, p.WeeklyReset = pct(pre+"7d-utilization"), at(pre+"7d-reset")
	if o := h.Get(pre + "overage-status"); o != "" {
		on := o != "rejected" && o != "disabled"
		p.Overage = &on
	}
	// A refused request (429, the limit reached) still carries the
	// headers; without them the reading failed.
	if p.FiveHour == nil || p.Weekly == nil {
		return fmt.Errorf("messages: HTTP %d without the rate-limit headers: %s", resp.StatusCode, oneLine(scrubToken(string(answer), tok), 200))
	}
	return nil
}

// readAccounts reads each account whose reading is due, in a worker of
// its own, live only: a shadow run reads the live run's readings. A
// reading is logged when it says something new, or usageRelog after the
// last one logged.
func (s *Supervisor) readAccounts(now time.Time) {
	u := s.cfg.Usage
	if !u.Enabled || s.shadow {
		return
	}
	s.work.mu.Lock()
	if s.work.usageSince.IsZero() {
		s.work.usageSince = now
	}
	s.work.mu.Unlock()
	for _, name := range sortedKeys(s.cfg.Accounts) {
		a := s.cfg.Accounts[name]
		if a.Usage == UsageOff {
			continue
		}
		s.work.mu.Lock()
		next := s.work.nextUsage[name]
		s.work.mu.Unlock()
		if now.Before(next) {
			continue
		}
		name, last := name, s.st.Accounts[name]
		s.spawnShared("usage:"+name, func(w *Supervisor) {
			p := newPlanReader(u, w.cfg.Timeouts.Forge.Duration).read(name, a)
			w.work.mu.Lock()
			w.work.nextUsage[name] = now.Add(u.Every.Duration)
			w.work.mu.Unlock()
			latest := last.At
			if last.ErrorAt.After(latest) {
				latest = last.ErrorAt
			}
			if last.sameReading(p) && now.Sub(latest) < relogAfter(u) {
				return
			}
			w.emit("", Event{Kind: KindAccount, Mode: ModeShared, Account: &p})
		})
	}
}

// placeAccount is the account a started session of repo goes on: the
// first of the repository's sessions.fallback whose latest reading is
// fresh and under the limit on both windows, or, with the readings off,
// the first; an account whose usage is off counts as having room once
// every account before it is known to have none. Tried says whether this
// run has read an account yet (nil: all have been): an earlier account
// not yet tried is waited for, never passed over for a later one, since
// a placement holds for good. When no account is chosen, it is "" and why
// says each account's state; full says every account was read and is at
// or over the limit, rather than unread.
func placeAccount(cfg *Config, st *LogState, repo string, now time.Time, tried func(string) bool) (account, why string, full bool) {
	rules, _ := effectivePolicy(cfg, st, repo)
	u := cfg.Usage
	var states []string
	full = len(rules.Sessions.Fallback) > 0
	for _, a := range rules.Sessions.Fallback {
		if !u.Enabled || cfg.Accounts[a].Usage == UsageOff {
			return a, "", false
		}
		if tried != nil && !tried(a) {
			return "", strings.Join(append(states, a+": not read yet by this run"), "; "), false
		}
		r, ok := st.Accounts[a]
		high, both := r.usageHigh()
		switch {
		case !ok || !both:
			states, full = append(states, a+": no reading yet"), false
		case now.Sub(r.At) > usageStale*u.Every.Duration:
			states, full = append(states, fmt.Sprintf("%s: no reading since %s", a, r.At.UTC().Format(time.RFC3339))), false
		case high >= u.Limit:
			states = append(states, fmt.Sprintf("%s: %s", a, usageText(r.AccountPayload)))
		default:
			return a, "", false
		}
	}
	if len(states) == 0 {
		return "", "the repository's policy has no sessions.fallback", false
	}
	return "", strings.Join(states, "; "), full
}

// usageText is a reading in words.
func usageText(p AccountPayload) string {
	part := func(label string, v *float64, at *time.Time) string {
		if v == nil {
			return label + " unknown"
		}
		s := fmt.Sprintf("%s %.0f%%", label, *v)
		if at != nil {
			s += " until " + at.UTC().Format("2006-01-02 15:04Z")
		}
		return s
	}
	s := part("5h", p.FiveHour, p.FiveHourReset) + ", " + part("weekly", p.Weekly, p.WeeklyReset)
	if p.Overage != nil {
		if *p.Overage {
			s += ", overage on"
		} else {
			s += ", overage off"
		}
	}
	return s
}

// unplaced says whether a start waits for agentd to place it: it was
// recorded for placement, and has no account yet. The start says so, not
// today's policy, so a session never starts on the default login.
func unplaced(p RuntimePayload) bool {
	return p.Place && p.Account == ""
}

// place puts a started session with no account on one before it is
// prepared: a shared session event of action place names the start's id
// and the account. While no account has room the start waits; the owner
// is told once per start. A shadow run places nothing: it waits for the
// live run's placement.
func (s *Supervisor) place(name string, t *Track, now time.Time) {
	p := s.st.Runtime[name]
	s.work.mu.Lock()
	read := copyMap(s.work.nextUsage)
	since := s.work.usageSince
	s.work.mu.Unlock()
	tried := func(a string) bool { _, ok := read[a]; return ok }
	account, why, full := placeAccount(s.cfg, s.st, p.Repository, now, tried)
	if s.shadow {
		if account != "" {
			why = "the live run places it; it would go on " + account
		}
		s.skipOnce(name, "placement:"+name+":"+p.ID, "not placing in shadow mode: "+why, t, nil)
		return
	}
	if account == "" {
		key := "placement:" + name + ":" + p.ID
		s.skipOnce(name, key, "waiting for an account with room: "+why, t, nil)
		// The owner is told once: when every account was read and is
		// full, or when the readings have not come for as long as a
		// reading stands, never only because the first is not in yet.
		// Waited since the start, or since this run began reading,
		// whichever is later: a run just started has not had its chance.
		from := s.st.StartedAt[name]
		if since.After(from) {
			from = since
		}
		waited := now.Sub(from)
		switch {
		case full:
			s.escalate(name, key, fmt.Sprintf("%s, started for branch %s of %s, waits for an account: every account of its fallback is at or over %.0f%% (%s). It starts on the first with room.",
				name, p.Branch, p.Repository, s.cfg.Usage.Limit, why), t, now)
		case !since.IsZero() && waited >= usageStale*s.cfg.Usage.Every.Duration:
			s.escalate(name, key, fmt.Sprintf("%s, started for branch %s of %s, has waited %s for an account: %s. It starts on the first fallback account with room once it is read.",
				name, p.Branch, p.Repository, waited.Round(time.Minute), why), t, now)
		}
		return
	}
	s.emit(name, Event{Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: p.ID, Action: SessionPlace, Repository: p.Repository, Account: account, By: "agentd"}})
	s.refreshRuntime()
}

// accountsCmd reads every configured account's usage now and prints it.
// It logs nothing.
func accountsCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("accounts", flag.ContinueOnError)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	return printAccounts(cfg, out)
}

// printAccounts reads each account and prints one line for it.
func printAccounts(cfg *Config, out io.Writer) error {
	if len(cfg.Accounts) == 0 {
		return errors.New("no accounts are configured")
	}
	r := newPlanReader(cfg.Usage, cfg.Timeouts.Forge.Duration)
	names := sortedKeys(cfg.Accounts)
	sort.Strings(names)
	failed := 0
	for _, name := range names {
		a := cfg.Accounts[name]
		if a.Usage == UsageOff {
			fmt.Fprintf(out, "%s: usage off\n", name)
			continue
		}
		p := r.read(name, a)
		if p.Error != "" {
			failed++
			fmt.Fprintf(out, "%s: %s\n", name, withheldText(cfg, p.Error))
			continue
		}
		fmt.Fprintf(out, "%s (%s): %s\n", name, p.Method, usageText(p))
	}
	if failed == len(names) {
		return errors.New("no account could be read")
	}
	return nil
}

// withheldText is text, or a notice in its place when it matches a
// confidential pattern.
func withheldText(cfg *Config, text string) string {
	if confidentialMatcher(cfg)(text) {
		return "(withheld: it matched a confidential pattern)"
	}
	return text
}
