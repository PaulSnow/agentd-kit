package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// authFailureJSON is what claude -p writes on standard output when its
// login reached its time limit, before it exits 1 (#102).
const authFailureJSON = `{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","result":"Failed to authenticate: this login reached its time limit and cannot be renewed ... use claude setup-token and set CLAUDE_CODE_OAUTH_TOKEN for unattended jobs"}`

// claudeExits makes the fake claude exit with code after it writes.
func claudeExits(t *testing.T, dir, code string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "exit"), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A call that exits non-zero is reported by the result it wrote on
// standard output: its text, subtype and terminal_reason, not only
// "exit status 1". A login that failed is an AuthError of the call's
// account; any other failure is not.
func TestJobRunReportsResultOnFailure(t *testing.T) {
	dir := fakeClaude(t)
	claudeAnswers(t, dir, 1, authFailureJSON)
	claudeAnswers(t, dir, 2, `{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"tool_error","result":"the tool broke"}`)
	claudeExits(t, dir, "1")
	j := testJob()
	j.AccountName = "personal"
	_, _, err := j.Run(context.Background(), "s", []byte(`{}`), []byte(`{}`))
	var auth *AuthError
	if err == nil || !errors.As(err, &auth) || auth.Account != "personal" {
		t.Fatalf("auth failure: %v", err)
	}
	for _, want := range []string{"Failed to authenticate: this login reached its time limit", "api_error", "success", "exit status 1", "account personal cannot log in"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	_, _, err = j.Run(context.Background(), "s", []byte(`{}`), []byte(`{}`))
	if err == nil || errors.As(err, &auth) || !strings.Contains(err.Error(), "error_during_execution (tool_error): the tool broke") {
		t.Fatalf("job failure: %v", err)
	}
	// A failed login is not retried: it would fail again.
	claudeAnswers(t, dir, 0, authFailureJSON)
	if _, _, _, err := j.Triage(context.Background(), triageItemsFixture()); !errors.As(err, &auth) || claudeCalls(t, dir) != 3 {
		t.Fatalf("triage: %v after %d calls", err, claudeCalls(t, dir))
	}
}

// faultHarness is a triage harness whose jobs run on account personal,
// which has a token file, and whose claude fails to authenticate.
func faultHarness(t *testing.T) (*harness, string, string) {
	t.Helper()
	h, dir := triageHarness(t)
	tok := filepath.Join(h.dir, "personal.token")
	if err := os.WriteFile(tok, []byte("old-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.cfg.Accounts = map[string]Account{"personal": {TokenFile: tok}, "spare": {ConfigDir: filepath.Join(h.dir, "spare")}}
	h.cfg.Jobs.Account = "personal"
	h.cfg.Consoles = map[string]ConsoleConfig{"infra": {Title: "Infrastructure"}}
	claudeAnswers(t, dir, 0, authFailureJSON)
	claudeExits(t, dir, "1")
	return h, dir, tok
}

// faultEvents are the account events that mark or clear a fault.
func faultEvents(h *harness) []AccountPayload {
	var out []AccountPayload
	for _, p := range accountEvents(h) {
		if p.Fault != "" || p.Cleared != "" {
			out = append(out, p)
		}
	}
	return out
}

// One failed login marks its account once, with one decision for the
// owner, however many passes follow; no job runs on it, nor is retried,
// until its token file changes.
func TestAuthFaultMarkedOnceAndCleared(t *testing.T) {
	h, dir, tok := faultHarness(t)
	h.ticks(1, time.Minute)
	if n := claudeCalls(t, dir); n != 1 {
		t.Fatalf("%d calls", n)
	}
	h.ticks(10, 2*time.Hour) // past triageRetry many times over
	if n := claudeCalls(t, dir); n != 1 {
		t.Fatalf("retried on a faulted account: %d calls", n)
	}
	f := faultEvents(h)
	if len(f) != 1 || f[0].Account != "personal" || f[0].FaultSource != "job:triage" || !strings.Contains(f[0].Fault, "Failed to authenticate") || f[0].TokenStamp == "" ||
		strings.Contains(f[0].TokenStamp, "old-token") {
		t.Fatalf("fault events %+v", f)
	}
	st := h.logState()
	needs := accountNeeds(h.cfg, st)
	if len(needs) != 1 || !strings.HasPrefix(needs[0].Entry, "a:personal:") || len(needs[0].Options) != 2 {
		t.Fatalf("needs %+v", needs)
	}
	d := accountDecision(h.cfg, "personal", st.Accounts["personal"])
	for _, want := range []string{"claude setup-token", "install -m 600 /dev/null " + tok, `accounts.personal.token_file`} {
		if !strings.Contains(d.Context, want) {
			t.Errorf("decision lacks %q:\n%s", want, d.Context)
		}
	}
	// A restart replays the fault: still no call, still one event.
	h.restart(false)
	h.ticks(3, 2*time.Hour)
	if n := claudeCalls(t, dir); n != 1 || len(faultEvents(h)) != 1 {
		t.Fatalf("after a restart: %d calls, %+v", n, faultEvents(h))
	}
	// The owner saves a new token: the fault clears and triage runs again.
	if err := os.WriteFile(tok, []byte("a-new-longer-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeExits(t, dir, "0")
	claudeAnswers(t, dir, 0, claudeResult(goodTriage))
	h.ticks(2, time.Minute)
	f = faultEvents(h)
	if len(f) != 2 || f[1].Cleared != FaultClearedToken || h.logState().accountFaulted("personal") {
		t.Fatalf("after the token changed: %+v", f)
	}
	if n := claudeCalls(t, dir); n != 2 {
		t.Fatalf("%d calls after the token changed", n)
	}
}

// The owner's "try again" clears the fault; "leave it off" keeps it and
// asks nothing more.
func TestAuthFaultOwnerAnswers(t *testing.T) {
	h, dir, _ := faultHarness(t)
	h.ticks(2, time.Minute)
	entry := entryAccount + accountEntryID("personal", h.logState().Accounts["personal"])
	if err := h.replyEntry(entry, AccountLeave); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, 2*time.Hour)
	st := h.logState()
	if !st.accountFaulted("personal") || len(accountNeeds(h.cfg, st)) != 0 || claudeCalls(t, dir) != 1 {
		t.Fatalf("after leave: faulted %v, needs %+v, %d calls", st.accountFaulted("personal"), accountNeeds(h.cfg, st), claudeCalls(t, dir))
	}
	if err := h.replyEntry(entry, AccountRetry); err == nil {
		t.Fatal("a settled entry took a second answer")
	}

	h, dir, _ = faultHarness(t)
	h.ticks(2, time.Minute)
	entry = entryAccount + accountEntryID("personal", h.logState().Accounts["personal"])
	if err := h.replyEntry(entry, AccountRetry); err != nil {
		t.Fatal(err)
	}
	h.ticks(1, 2*time.Hour)
	f := faultEvents(h)
	if len(f) < 2 || f[1].Cleared != FaultClearedOwner {
		t.Fatalf("after try again: %+v", f)
	}
	// It is tried again, fails again, and is marked again: one new fault.
	h.ticks(3, 2*time.Hour)
	if n := claudeCalls(t, dir); n != 2 {
		t.Fatalf("%d calls after try again", n)
	}
	if f := faultEvents(h); len(f) != 3 || f[2].Fault == "" {
		t.Fatalf("after the second failure: %+v", f)
	}
}

// A repository's job moves to its policy's fallback account while the
// jobs' account cannot log in; a job of no repository waits.
func TestJobForFallsBack(t *testing.T) {
	h, _, _ := faultHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Sessions.Fallback = []string{"personal", "spare"}
	h.cfg.Repositories["widgets"] = w
	if j, ok := h.sup.jobFor("widgets", "thread"); !ok || j.AccountName != "personal" {
		t.Fatalf("before the fault: %v %q", ok, j.AccountName)
	}
	h.sup.emit("", Event{Kind: KindAccount, Mode: ModeShared, Account: &AccountPayload{Account: "personal", Fault: "Failed to authenticate", FaultSource: "job:triage",
		TokenStamp: tokenStamp(h.cfg, "personal")}})
	if j, ok := h.sup.jobFor("widgets", "thread"); !ok || j.AccountName != "spare" || j.Account.ConfigDir == "" {
		t.Fatalf("widgets' job: %v %+v", ok, j)
	}
	if _, ok := h.sup.jobFor("", "triage"); ok {
		t.Fatal("a job of no repository moved to another account")
	}
	if _, ok := h.sup.jobFor("readonly", "thread"); ok {
		t.Fatal("a repository with no other account ran its job")
	}
}

// Placement passes over an account that cannot log in, and says why
// when it has none left.
func TestPlacementSkipsFaultedAccount(t *testing.T) {
	h, f := usageHarness(t)
	f.set(tokenA, 5, 5)
	h.ticks(1, time.Minute) // the readings
	h.sup.emit("", Event{Kind: KindAccount, Mode: ModeShared, Account: &AccountPayload{Account: "a", Fault: "Not logged in", FaultSource: "session:x",
		TokenStamp: tokenStamp(h.cfg, "a")}})
	name := h.startSession("fix/spin", KindWork, 7)
	h.ticks(3, time.Minute)
	if acct := projected(h.cfg, h.events()).Sessions[name].Account; acct != "b" {
		t.Fatalf("placed on %q", acct)
	}
	st := h.logState()
	st.Accounts["b"] = AccountRecord{FaultAt: pageAt}
	if a, why, _, _ := pickAccount(h.cfg, st, []string{"a", "b"}, h.clock.Now(), nil); a != "" || !strings.Contains(why, "a: cannot log in since") || !strings.Contains(why, "b: cannot log in since") {
		t.Fatalf("both faulted: %q, %q", a, why)
	}
	h.cfg.Usage.Enabled = false
	if a, _, _, _ := pickAccount(h.cfg, h.logState(), []string{"a", "b"}, h.clock.Now(), nil); a != "b" {
		t.Fatalf("usage off: %q", a)
	}
}

// loggedOutScreen is Claude Code's screen once its login has expired.
const loggedOutScreen = "Working on it...\n\n> hello\n  ⎿  API Error: 401 · Please run /login\n\n╭──────╮\n│ >    │\n╰──────╯\n"

// A session whose screen ends on a login prompt is logged-out: its account
// is marked once, nothing is typed into it, and once gone it is not
// restarted while the fault stands. Text about logins higher up is not one.
func TestSessionLoggedOut(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.cfg.Consoles = map[string]ConsoleConfig{"infra": {Title: "Infrastructure"}}
	h.status("s1", "# Status\nworking\n")
	h.alive("s1", "Notes: what if the user is not logged in?\n"+strings.Repeat("line\n", 10)+"> ")
	h.ticks(1, time.Minute)
	if s := h.state("s1"); s == LoggedOut {
		t.Fatal("text above the end read as a login prompt")
	}
	h.alive("s1", loggedOutScreen)
	h.ticks(3, time.Hour)
	if s := h.state("s1"); s != LoggedOut {
		t.Fatalf("state %s", s)
	}
	f := faultEvents(h)
	if len(f) != 1 || f[0].Account != DefaultAccount || f[0].FaultSource != "session:s1" || !strings.Contains(f[0].Fault, "Please run /login") {
		t.Fatalf("fault events %+v", f)
	}
	if typed := h.typedBy(h.tmux, "s1"); len(typed) != 0 {
		t.Fatalf("typed into a logged-out session: %q", typed)
	}
	// Gone, it is not restarted while its account cannot log in.
	h.tmux.alive["s1"] = false
	h.ticks(5, time.Hour)
	if n := h.count(KindAction, ActRestart, "s1"); n != 0 {
		t.Fatalf("%d restarts", n)
	}
	skipped := 0
	for _, e := range h.events() {
		if e.Kind == KindSkip && e.Session == "s1" && strings.HasPrefix(e.Skip.Key, "account-fault:s1:") {
			skipped++
		}
	}
	if skipped != 1 {
		t.Fatalf("%d account-fault skips", skipped)
	}
	// The owner's try again: it is restarted.
	st := h.logState()
	if err := h.replyEntry(entryAccount+accountEntryID(DefaultAccount, st.Accounts[DefaultAccount]), AccountRetry); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	if n := h.count(KindAction, ActRestart, "s1"); n != 1 {
		t.Fatalf("%d restarts after try again", n)
	}
}

// The console's home and agentd status show an account that cannot log
// in, with its message.
func TestAuthFaultShown(t *testing.T) {
	h, _, _ := faultHarness(t)
	h.ticks(2, time.Minute)
	body := owner(t, harnessUI(t, h), "/")
	if !strings.Contains(body, "cannot log in") || !strings.Contains(body, "Failed to authenticate") {
		t.Errorf("home page lacks the fault:\n%s", body)
	}
	rows, meta := SummarizeAt(h.cfg, h.events(), ModeLive, h.clock.Now())
	var b bytes.Buffer
	PrintStatus(&b, rows, meta, h.clock.Now())
	if out := b.String(); !strings.Contains(out, "accounts:\n") || !strings.Contains(out, "personal: cannot log in since ") || !strings.Contains(out, "spare: ok") {
		t.Errorf("status:\n%s", out)
	}
}

// A scheduled job on an account that cannot log in is not run: its slot
// is a skip that says why, and it runs again once the token changes.
func TestScheduledJobSkipsFaultedAccount(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("tok-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, false)
	h.cfg.Accounts = map[string]Account{"b": {TokenFile: tok}}
	h = schedHarnessWith(t, h, map[string]ScheduledJob{
		"sweep": {Every: Duration{10 * time.Minute}, Account: "b", Argv: []string{"sh", "-c", "echo run >> runs"}}})
	h.sup.emit("", Event{Kind: KindAccount, Mode: ModeShared, Account: &AccountPayload{Account: "b", Fault: "Failed to authenticate", FaultSource: "job:triage",
		TokenStamp: tokenStamp(h.cfg, "b")}})
	h.ticks(25, time.Minute)
	if got := readJobFile(t, h, "sweep", "runs"); got != "" {
		t.Fatalf("ran on a faulted account: %q", got)
	}
	skips := jobEvents(h, "sweep", JobSkip)
	if len(skips) != 3 || skips[0].Reason != "account b cannot log in" || len(jobEvents(h, "sweep", JobStart)) != 0 {
		t.Fatalf("job events: %+v", jobEvents(h, "sweep", ""))
	}
	if err := os.WriteFile(tok, []byte("tok-4567\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.ticks(11, time.Minute)
	if got := readJobFile(t, h, "sweep", "runs"); got != "run\n" {
		t.Fatalf("after the token changed: %q", got)
	}
}
