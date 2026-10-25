package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tokens the fake usage API knows. Each is long enough that its first
// and last sixteen characters are scrubbed too.
const (
	tokenA = "test-oat-tokenA-0123456789abcdefghijklmnop" // may read the usage endpoint
	tokenB = "test-oat-tokenB-0123456789abcdefghijklmnop" // may not: the headers serve it
	tokenC = "test-oat-tokenC-0123456789abcdefghijklmnop" // the endpoint fails and echoes it
)

var usageReset = time.Date(2026, 10, 23, 17, 0, 0, 0, time.UTC)

// fakeUsageAPI is the usage endpoint and the messages endpoint, with
// each account's utilization, in percent, settable.
type fakeUsageAPI struct {
	mu       sync.Mutex
	pct      map[string][2]float64 // by token: five-hour, weekly
	calls    []string
	messages []map[string]any
}

func newFakeUsageAPI(t *testing.T) (*fakeUsageAPI, *httptest.Server) {
	f := &fakeUsageAPI{pct: map[string][2]float64{tokenA: {12, 40}, tokenB: {55.5, 20}, tokenC: {1, 1}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		if r.Header.Get("anthropic-beta") != oauthBeta {
			http.Error(w, "no oauth beta", http.StatusBadRequest)
			return
		}
		p, ok := f.pct[tok]
		switch {
		case !ok:
			http.Error(w, "unknown token "+tok, http.StatusUnauthorized)
		case r.URL.Path == "/api/oauth/usage" && tok == tokenB:
			http.Error(w, `{"error":"the token lacks the user:profile scope"}`, http.StatusForbidden)
		case r.URL.Path == "/api/oauth/usage" && tok == tokenC:
			http.Error(w, "internal error for "+tok, http.StatusInternalServerError)
		case r.URL.Path == "/api/oauth/usage":
			fmt.Fprintf(w, `{"five_hour":{"utilization":%g,"resets_at":"2026-10-23T17:00:00.123+00:00"},"seven_day":{"utilization":%g,"resets_at":"2026-10-28T09:00:00+00:00"},"extra_usage":{"is_enabled":true}}`, p[0], p[1])
		case r.URL.Path == "/v1/messages" && r.Method == http.MethodPost:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.messages = append(f.messages, body)
			h := w.Header()
			h.Set("anthropic-ratelimit-unified-5h-utilization", fmt.Sprint(p[0]/100))
			h.Set("anthropic-ratelimit-unified-5h-reset", fmt.Sprint(usageReset.Unix()))
			h.Set("anthropic-ratelimit-unified-7d-utilization", fmt.Sprint(p[1]/100))
			h.Set("anthropic-ratelimit-unified-7d-reset", fmt.Sprint(usageReset.Add(96*time.Hour).Unix()))
			h.Set("anthropic-ratelimit-unified-overage-status", "rejected")
			fmt.Fprint(w, `{"type":"message","content":[{"type":"text","text":"."}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeUsageAPI) set(tok string, fiveHour, weekly float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pct[tok] = [2]float64{fiveHour, weekly}
}

// usageAccounts writes the three accounts' credentials under dir: a's and
// c's as token files, b's as a Claude config directory.
func usageAccounts(t *testing.T, dir string) map[string]Account {
	t.Helper()
	write := func(path, text string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "a.token"), tokenA+"\n")
	write(filepath.Join(dir, "c.token"), tokenC)
	write(filepath.Join(dir, "b", ".credentials.json"), `{"claudeAiOauth":{"accessToken":"`+tokenB+`","refreshToken":"r"}}`)
	return map[string]Account{
		"a": {TokenFile: filepath.Join(dir, "a.token")},
		"b": {ConfigDir: filepath.Join(dir, "b")},
		"c": {TokenFile: filepath.Join(dir, "c.token"), Usage: UsageEndpoint},
	}
}

// noToken fails when any token is in text.
func noToken(t *testing.T, where, text string) {
	t.Helper()
	for _, tok := range []string{tokenA, tokenB, tokenC} {
		if strings.Contains(text, tok) || strings.Contains(text, tok[:16]) || strings.Contains(text, tok[len(tok)-16:]) {
			t.Fatalf("%s holds a token:\n%s", where, text)
		}
	}
}

// agentd accounts reads a token that may read the usage endpoint there,
// and one that may not through the rate-limit headers of a minimal
// request made as Claude Code makes it; a failure is printed without the
// token the server echoed.
func TestAccountsBothMethods(t *testing.T) {
	f, srv := newFakeUsageAPI(t)
	cfg := DefaultConfig()
	cfg.Usage.API = srv.URL
	cfg.Accounts = usageAccounts(t, t.TempDir())
	var out bytes.Buffer
	if err := printAccounts(&cfg, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"a (endpoint): 5h 12% until 2026-10-23 17:00Z, weekly 40% until 2026-10-28 09:00Z, overage on\n",
		"b (headers): 5h 56% until 2026-10-23 17:00Z, weekly 20% until 2026-10-27 17:00Z, overage off\n",
		"c: usage endpoint: HTTP 500: internal error for [token]\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	noToken(t, "agentd accounts", got)
	if len(f.messages) != 1 {
		t.Fatalf("%d minimal requests, want b's one", len(f.messages))
	}
	m := f.messages[0]
	if m["model"] != DefaultUsageModel || m["max_tokens"] != float64(1) || m["system"] != claudeCodeSystem {
		t.Fatalf("minimal request %v", m)
	}
	// The endpoint was asked for a and b, the messages for b only.
	if calls := strings.Join(f.calls, ","); calls != "GET /api/oauth/usage,GET /api/oauth/usage,POST /v1/messages,GET /api/oauth/usage" {
		t.Fatalf("calls %s", calls)
	}
}

// A redirect is not followed: it could carry the token elsewhere.
func TestUsageRedirectNotFollowed(t *testing.T) {
	var other int
	away := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other++ }))
	defer away.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, away.URL+"/steal", http.StatusFound)
	}))
	defer srv.Close()
	cfg := DefaultConfig()
	cfg.Usage.API = srv.URL
	p := newPlanReader(cfg.Usage, 5*time.Second).read("a", usageAccounts(t, t.TempDir())["a"])
	if p.Error == "" || other != 0 {
		t.Fatalf("reading %+v, %d requests elsewhere", p, other)
	}
}

// usageHarness is sessionsHarness with accounts a, b and c, their usage
// read from the fake API every 15 minutes, and widgets' sessions falling
// back to a, then b.
func usageHarness(t *testing.T) (*harness, *fakeUsageAPI) {
	t.Helper()
	h := sessionsHarness(t)
	f, srv := newFakeUsageAPI(t)
	h.cfg.Accounts = usageAccounts(t, filepath.Join(h.dir, "accounts"))
	h.cfg.Usage.Enabled, h.cfg.Usage.API = true, srv.URL
	w := h.cfg.Repositories["widgets"]
	w.Policy.Sessions.Fallback = []string{"a", "b"}
	h.cfg.Repositories["widgets"] = w
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	return h, f
}

// accountEvents are the account events logged.
func accountEvents(h *harness) []AccountPayload {
	var out []AccountPayload
	for _, e := range h.events() {
		if e.Kind == KindAccount {
			out = append(out, *e.Account)
		}
	}
	return out
}

// agentd run reads each account when due and logs the numbers as shared
// account events, only when they move or the logged reading grows old
// (TestUsageSteadyAccountStaysFresh); no token is ever in the log.
func TestUsageReadingsLogged(t *testing.T) {
	h, f := usageHarness(t)
	h.ticks(2, time.Minute)
	ev := accountEvents(h)
	if len(ev) != 3 {
		t.Fatalf("readings %+v", ev)
	}
	for _, e := range h.events() {
		if e.Kind == KindAccount && (e.Mode != ModeShared || e.Session != "") {
			t.Fatalf("an account event of mode %q, session %q", e.Mode, e.Session)
		}
	}
	st := h.logState()
	if r := st.Accounts["b"]; r.Method != UsageHeaders || *r.FiveHour != 55.5 || *r.Weekly != 20 || r.FiveHourReset == nil || !r.FiveHourReset.Equal(usageReset) {
		t.Fatalf("b %+v", r)
	}
	if r := st.Accounts["c"]; r.Error != "usage endpoint: HTTP 500: internal error for [token]" || r.FiveHour != nil {
		t.Fatalf("c %+v", r)
	}
	// Due again after 15 minutes, saying the same: nothing logged.
	h.ticks(16, time.Minute)
	if n := len(accountEvents(h)); n != 3 {
		t.Fatalf("%d readings after the same numbers", n)
	}
	// A move of a whole percent is logged.
	f.set(tokenA, 30, 40)
	h.ticks(16, time.Minute)
	if r := h.logState().Accounts["a"]; *r.FiveHour != 30 {
		t.Fatalf("after a's move: %+v", r)
	}
	noToken(t, "the event log", h.buf.String())
	// The coordinator's pack carries the readings of the accounts its
	// sessions may fall back to.
	pack, _ := h.sup.buildPack("widgets", nil, h.clock.Now(), true)
	if len(pack.Accounts) != 2 || pack.Accounts[0].Account != "a" || pack.Accounts[1].Account != "b" || *pack.Accounts[0].FiveHour != 30 {
		t.Fatalf("pack accounts %+v", pack.Accounts)
	}
	// The UI shows each account.
	body := owner(t, harnessUI(t, h), "/")
	for _, want := range []string{"<strong>a</strong>", "5h 30%", "week 20%", "overage off", "internal error for [token]"} {
		if !strings.Contains(body, want) {
			t.Errorf("UI lacks %q", want)
		}
	}
}

// A shadow run reads nothing: the live run's readings are shared.
func TestUsageShadowReadsNothing(t *testing.T) {
	h, f := usageHarness(t)
	h.restart(true)
	h.ticks(2, time.Minute)
	if len(accountEvents(h)) != 0 || len(f.calls) != 0 {
		t.Fatalf("the shadow read %v", f.calls)
	}
}

// A session a coordinator starts with no account goes on the first
// account of the fallback order with room: a is over the limit, so b.
func TestPlacementFallsBack(t *testing.T) {
	h, f := usageHarness(t)
	f.set(tokenA, 95, 40)
	h.ticks(1, time.Minute) // the readings
	reply, refused := h.startSession(map[string]any{"branch": "fix/spin", "issue": 7})
	name, _ := reply["name"].(string)
	if refused || reply["account"] == nil {
		t.Fatalf("start = %v", reply)
	}
	h.ticks(3, time.Minute)
	var placed []RuntimePayload
	for _, p := range h.sessionEvents() {
		if p.Action == SessionPlace {
			placed = append(placed, p)
		}
	}
	if len(placed) != 1 || placed[0].Account != "b" {
		t.Fatalf("placements %+v", placed)
	}
	if acct := projected(h.cfg, h.events()).Sessions[name].Account; acct != "b" {
		t.Fatalf("session account %q", acct)
	}
	if n := h.count(KindAction, ActProvision, name); n != 1 {
		t.Fatalf("provisioned %d times", n)
	}
	// It stays on b: a coming back under the limit does not move it.
	f.set(tokenA, 5, 5)
	h.ticks(16, time.Minute)
	if acct := projected(h.cfg, h.events()).Sessions[name].Account; acct != "b" {
		t.Fatalf("moved to %q", acct)
	}
}

// When every fallback account is over the limit the start waits, with
// one notice to the owner however many passes it waits, and starts once
// an account has room.
func TestPlacementWaits(t *testing.T) {
	h, f := usageHarness(t)
	f.set(tokenA, 95, 40)
	f.set(tokenB, 20, 99)
	h.ticks(1, time.Minute)
	reply, _ := h.startSession(map[string]any{"branch": "fix/spin", "issue": 7})
	name, _ := reply["name"].(string)
	h.ticks(5, time.Minute)
	if n := h.count(KindAction, ActProvision, name); n != 0 {
		t.Fatalf("provisioned while waiting")
	}
	var notices []string
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActEscalate && strings.HasPrefix(e.Act.Key, "placement:") {
			notices = append(notices, e.Act.Message)
		}
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "a: 5h 95%") || !strings.Contains(notices[0], "b: 5h 20%") || !strings.Contains(notices[0], "weekly 99%") {
		t.Fatalf("notices %q", notices)
	}
	f.set(tokenB, 20, 30)
	h.ticks(18, time.Minute)
	if acct := projected(h.cfg, h.events()).Sessions[name].Account; acct != "b" || h.count(KindAction, ActProvision, name) != 1 {
		t.Fatalf("after b has room: account %q, provisioned %d", acct, h.count(KindAction, ActProvision, name))
	}
}

// An account the coordinator names is used as before, with no placement.
func TestPlacementNotForANamedAccount(t *testing.T) {
	h, _ := usageHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Sessions.Accounts = []string{"c"}
	h.cfg.Repositories["widgets"] = w
	h.restart(false)
	reply, refused := h.startSession(map[string]any{"branch": "fix/spin", "issue": 7, "account": "c"})
	if refused || reply["account"] != nil {
		t.Fatalf("start = %v", reply)
	}
	h.ticks(2, time.Minute)
	for _, p := range h.sessionEvents() {
		if p.Action == SessionPlace {
			t.Fatalf("placed %+v", p)
		}
	}
}

// The fold keeps an account's numbers across a failed reading; a
// placement holds only for the start it names, once.
func TestAccountFold(t *testing.T) {
	cfg := DefaultConfig()
	st := NewLogState(&cfg, ModeLive)
	five, week := 10.0, 20.0
	t0 := pageAt
	st.Fold([]Event{
		{V: EventVersion, Time: t0, Kind: KindAccount, Mode: ModeShared, Account: &AccountPayload{Account: "a", Method: UsageEndpoint, FiveHour: &five, Weekly: &week}},
		{V: EventVersion, Time: t0.Add(time.Minute), Kind: KindAccount, Mode: ModeShared, Account: &AccountPayload{Account: "a", Error: "down"}},
	})
	if r := st.Accounts["a"]; *r.FiveHour != 10 || r.Error != "down" || !r.At.Equal(t0) {
		t.Fatalf("a %+v", r)
	}
	st.Runtime = map[string]RuntimePayload{"s": {ID: "i1", Action: SessionStart, Repository: "widgets"}}
	st.Fold([]Event{
		{V: EventVersion, Time: t0, Session: "s", Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: "i0", Action: SessionPlace, Repository: "widgets", Account: "x"}},
		{V: EventVersion, Time: t0, Session: "s", Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: "i1", Action: SessionPlace, Repository: "widgets", Account: "b"}},
		{V: EventVersion, Time: t0, Session: "s", Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: "i1", Action: SessionPlace, Repository: "widgets", Account: "a"}},
	})
	if a := st.Runtime["s"].Account; a != "b" {
		t.Fatalf("placed on %q", a)
	}
	for _, bad := range []Event{
		{V: EventVersion, Time: t0, Kind: KindAccount, Mode: ModeLive, Account: &AccountPayload{Account: "a"}},
		{V: EventVersion, Time: t0, Session: "s", Kind: KindAccount, Mode: ModeShared, Account: &AccountPayload{Account: "a"}},
		{V: EventVersion, Time: t0, Kind: KindAccount, Mode: ModeShared, Account: &AccountPayload{}},
		{V: EventVersion, Time: t0, Session: "s", Kind: KindSession, Mode: ModeOwner, Runtime: &RuntimePayload{ID: "i1", Action: SessionPlace, Repository: "widgets", Account: "a"}},
		{V: EventVersion, Time: t0, Session: "s", Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: "i1", Action: SessionPlace, Repository: "widgets"}},
	} {
		if err := bad.check(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

// placementNotices are the placement escalations posted.
func placementNotices(h *harness) []string {
	var out []string
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActEscalate && strings.HasPrefix(e.Act.Key, "placement:") {
			out = append(out, e.Act.Message)
		}
	}
	return out
}

// A steady account is logged again before its reading goes stale, so a
// start an hour on is placed at once.
func TestUsageSteadyAccountStaysFresh(t *testing.T) {
	h, _ := usageHarness(t)
	h.ticks(61, time.Minute)
	n := 0
	for _, e := range accountEvents(h) {
		if e.Account == "a" {
			n++
		}
	}
	if n < 3 {
		t.Fatalf("a logged %d times in an hour, want a reading at least every %s", n, relogAfter(h.cfg.Usage))
	}
	if a, why, _ := placeAccount(h.cfg, h.logState(), "widgets", h.clock.Now(), nil); a != "a" {
		t.Fatalf("placed on %q: %s", a, why)
	}
}

// A start made before the first readings are in waits for them without
// telling the owner, then is placed.
func TestPlacementNoNoticeBeforeReadings(t *testing.T) {
	h, _ := usageHarness(t)
	reply, _ := h.startSession(map[string]any{"branch": "fix/spin", "issue": 7})
	name, _ := reply["name"].(string)
	h.ticks(3, time.Minute)
	if n := placementNotices(h); len(n) != 0 {
		t.Fatalf("notices before the readings: %q", n)
	}
	if acct := projected(h.cfg, h.events()).Sessions[name].Account; acct != "a" {
		t.Fatalf("placed on %q", acct)
	}
}

// Readings that never come are told to the owner once, after three
// intervals; a start recorded for placement never starts on the default
// login, even when the fallback order is taken out while it waits.
func TestPlacementNeverOnTheDefaultLogin(t *testing.T) {
	h, f := usageHarness(t)
	f.set(tokenA, 95, 95)
	f.set(tokenB, 95, 95)
	h.ticks(1, time.Minute)
	reply, _ := h.startSession(map[string]any{"branch": "fix/spin", "issue": 7})
	name, _ := reply["name"].(string)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Sessions.Fallback = nil
	h.cfg.Repositories["widgets"] = w
	h.restart(false)
	h.ticks(50, time.Minute)
	if n := h.count(KindAction, ActProvision, name); n != 0 {
		t.Fatalf("provisioned %d times with no account", n)
	}
	if n := placementNotices(h); len(n) != 1 || !strings.Contains(n[0], "no sessions.fallback") {
		t.Fatalf("notices %q", n)
	}
}

// A placed session keeps its account across a restart of agentd: the
// replay gives the same account, and its launch is under it. A shadow
// run reads the live run's placement and places nothing itself.
func TestPlacementSurvivesRestart(t *testing.T) {
	h, f := usageHarness(t)
	f.set(tokenA, 95, 40)
	h.ticks(1, time.Minute)
	reply, _ := h.startSession(map[string]any{"branch": "fix/spin", "issue": 7})
	name, _ := reply["name"].(string)
	h.ticks(3, time.Minute)
	f.set(tokenA, 1, 1)
	h.restart(false)
	h.ticks(3, time.Minute)
	var accounts []string
	for _, e := range h.events() {
		if e.Session == name && e.Kind == KindAction && e.Act.Action == ActRestart {
			accounts = append(accounts, e.Act.Account)
		}
	}
	if len(accounts) == 0 || strings.Join(accounts, ",") != strings.Repeat("b,", len(accounts)-1)+"b" {
		t.Fatalf("launches under %v", accounts)
	}
	places := 0
	for _, p := range h.sessionEvents() {
		if p.Action == SessionPlace {
			places++
		}
	}
	h.restart(true)
	h.ticks(3, time.Minute)
	n := 0
	for _, p := range h.sessionEvents() {
		if p.Action == SessionPlace {
			n++
		}
	}
	if places != 1 || n != 1 {
		t.Fatalf("placements: %d live, %d after the shadow", places, n)
	}
	if acct := NewLogState(h.cfg, ModeShadow).Fold(h.events()).Runtime[name].Account; acct != "b" {
		t.Fatalf("the shadow reads %q", acct)
	}
}

// An account reading's error is screened before it is logged.
func TestUsageErrorScreened(t *testing.T) {
	h, _ := usageHarness(t)
	h.cfg.ConfidentialPatterns = []string{"internal error"}
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	h.ticks(2, time.Minute)
	if strings.Contains(h.buf.String(), "internal error") || h.logState().Accounts["c"].Error == "" {
		t.Fatalf("c's error: %q", h.logState().Accounts["c"].Error)
	}
}

// A token is sent only over https, or http to this machine; an expired
// .credentials.json token is not sent at all.
func TestUsageAPIAndExpiry(t *testing.T) {
	for api, ok := range map[string]bool{"https://api.anthropic.com": true, "http://127.0.0.1:8080": true, "http://localhost:1": true,
		"http://api.example.com": false, "ftp://x": false, "https://": false} {
		if usageAPIOK(api) != ok {
			t.Errorf("%s: %v", api, !ok)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"`+tokenB+`","expiresAt":1000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := accountToken(Account{ConfigDir: dir}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired token: %v", err)
	}
	if _, err := accountToken(Account{TokenFile: filepath.Join(dir, "missing", "x.token")}); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("a missing token file: %v", err)
	}
}

// A start recorded while agentd was down is not told to the owner as a
// long wait when agentd comes back: the run reads first, then places.
// An earlier account not yet read by this run is waited for, never passed
// over for a later one whose usage is off.
func TestPlacementAfterDowntime(t *testing.T) {
	h, _ := usageHarness(t)
	b := h.cfg.Accounts["b"]
	b.Usage = UsageOff
	h.cfg.Accounts["b"] = b
	h.restart(false)
	reply, _ := h.startSession(map[string]any{"branch": "fix/spin", "issue": 7})
	name, _ := reply["name"].(string)
	h.clock.advance(2 * time.Hour)
	h.restart(false)
	h.ticks(3, time.Minute)
	if n := placementNotices(h); len(n) != 0 {
		t.Fatalf("notices after the downtime: %q", n)
	}
	if acct := projected(h.cfg, h.events()).Sessions[name].Account; acct != "a" {
		t.Fatalf("placed on %q, want a, which has room", acct)
	}
}

// The default login is never a fallback.
func TestFallbackNamesAnAccount(t *testing.T) {
	h, _ := usageHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Sessions.Fallback = []string{"a", ""}
	h.cfg.Repositories["widgets"] = w
	if err := h.cfg.finish(h.dir); err == nil || !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("an empty fallback: %v", err)
	}
}
