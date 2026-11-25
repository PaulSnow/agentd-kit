package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// An account that cannot log in (#102) is a fault of the account, not of
// the job or session that met it: every call on it would fail the same
// way. agentd marks it once, asks the owner once for a token, and starts
// no job or session on it, nor retries one, until its token file changes
// or the owner says to try again. A repository whose sessions.fallback
// names another account uses that one meanwhile; there is no other
// fallback.

// DefaultAccount names the default login, the one with no account, in
// an account's fault and on the owner's entry.
const DefaultAccount = "default"

// acctName is an account's name, DefaultAccount for the default login.
func acctName(a string) string {
	if a == "" {
		return DefaultAccount
	}
	return a
}

// Ways an account's fault is cleared.
const (
	FaultClearedToken  = "token-changed"
	FaultClearedOwner  = "owner"
	FaultClearedConfig = "config"
)

// authFault is an auth failure met by a job or a session, waiting for the
// loop to mark its account.
type authFault struct {
	account, source, message string
}

// accountFaulted says whether account (DefaultAccount or "" for the
// default login) cannot log in, as the log says.
func (st *LogState) accountFaulted(account string) bool {
	return st.Accounts[acctName(account)].faulted()
}

// tokenStamp is the account's token file as it is now: its size, mtime
// and inode, never its contents; "" when it has none, or it cannot be
// read. A token the owner saves changes it.
func tokenStamp(cfg *Config, account string) string {
	a, ok := cfg.Accounts[account]
	if !ok || a.TokenFile == "" {
		return ""
	}
	fi, err := os.Stat(a.TokenFile)
	if err != nil {
		return "missing"
	}
	stamp := fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		stamp += fmt.Sprintf(":%d", sys.Ino)
	}
	return stamp
}

// noteAuthFault queues an auth failure of account met at source, job:NAME
// or session:NAME. It may be called from any goroutine: the loop marks
// the account at its next pass (accountFaults).
func (s *Supervisor) noteAuthFault(account, source, message string) {
	s.work.mu.Lock()
	s.work.authFaults = append(s.work.authFaults, authFault{acctName(account), source, message})
	s.work.mu.Unlock()
}

// noteJobErr queues err's account fault, when err is an auth failure.
func (s *Supervisor) noteJobErr(source string, err error) {
	var auth *AuthError
	if errors.As(err, &auth) {
		s.noteAuthFault(auth.Account, "job:"+source, auth.Message)
	}
}

// accountFaults marks each account a job or session found unable to log
// in, once while its fault is open, and clears each fault whose token
// file has changed, or that gained a token file in the config, since. A
// shadow run marks and clears nothing: it reads the live run's.
func (s *Supervisor) accountFaults(now time.Time) {
	s.work.mu.Lock()
	queued := s.work.authFaults
	s.work.authFaults = nil
	s.work.mu.Unlock()
	if s.shadow {
		return
	}
	for _, f := range queued {
		if s.halt != nil || s.st.accountFaulted(f.account) {
			continue
		}
		msg := cutUTF8(s.withheld(scrubToken(f.message, "")), 600)
		s.emit("", Event{Kind: KindAccount, Mode: ModeShared, Account: &AccountPayload{Account: f.account, Fault: msg, FaultSource: f.source,
			TokenStamp: tokenStamp(s.cfg, f.account)}})
	}
	for _, name := range sortedKeys(s.st.Accounts) {
		r := s.st.Accounts[name]
		if !r.faulted() || s.halt != nil {
			continue
		}
		a, ok := s.cfg.Accounts[name]
		if !ok || a.TokenFile == "" {
			continue
		}
		why := FaultClearedToken
		if r.TokenStamp == "" {
			why = FaultClearedConfig
		} else if tokenStamp(s.cfg, name) == r.TokenStamp {
			continue
		}
		s.emit("", Event{Kind: KindAccount, Mode: ModeShared, Account: &AccountPayload{Account: name, Cleared: why}})
	}
}

// jobFor is the job the config describes for repo ("" for a job of no
// repository), on an account that can log in: jobs.account, or, while it
// cannot, the first of repo's sessions.fallback, then sessions.accounts,
// that is configured and can. A job of no repository waits for its
// account (#102 Q1). It says false when there is none: the job is not
// run, nor retried, until the fault clears. The job's auth failures are
// noted for the loop.
func (s *Supervisor) jobFor(repo, source string) (Job, bool) {
	return s.gateJob(jobFromConfig(s.cfg), repo, source)
}

// gateJob is j, a job of repo built from the config, as jobFor makes it.
func (s *Supervisor) gateJob(j Job, repo, source string) (Job, bool) {
	j.onAuth = func(err *AuthError) { s.noteAuthFault(err.Account, "job:"+source, err.Message) }
	if !s.st.accountFaulted(j.AccountName) {
		return j, true
	}
	if repo == "" {
		return j, false
	}
	rules, _ := effectivePolicy(s.cfg, s.st, repo)
	for _, a := range append(append([]string{}, rules.Sessions.Fallback...), rules.Sessions.Accounts...) {
		if _, ok := s.cfg.Accounts[a]; ok && !s.st.accountFaulted(a) {
			j.Account, j.AccountName = s.cfg.Accounts[a], a
			return j, true
		}
	}
	return j, false
}

// entryAccount is the entry id prefix of an account's fault: a:NAME:UNIX,
// UNIX the second it was marked.
const entryAccount = "a:"

// The owner's answers to an account's fault.
const (
	AccountRetry = "retry"
	AccountLeave = "leave"
)

// accountEntryID is the entry id of r's fault, without its prefix.
func accountEntryID(name string, r AccountRecord) string {
	return fmt.Sprintf("%s:%d", name, r.FaultAt.Unix())
}

// accountConsole is the console the owner is asked on about accounts: the
// ops console, else policies.escalate.default_console when it is one of
// consoles, else the first console the config names.
func (c *Config) accountConsole() string {
	if o := c.opsConsole(); o != "" {
		return o
	}
	if d := c.Policies.Escalate.DefaultConsole; d != "" {
		if _, ok := c.Consoles[d]; ok {
			return d
		}
	}
	if names := sortedKeys(c.Consoles); len(names) > 0 {
		return names[0]
	}
	return ""
}

// accountDecision asks the owner for account name's token, with the exact
// commands.
func accountDecision(cfg *Config, name string, r AccountRecord) Decision {
	tok := "~/.config/agentd-kit/" + name + ".token"
	setup := "claude setup-token"
	if a, ok := cfg.Accounts[name]; ok {
		if a.TokenFile != "" {
			tok = a.TokenFile
		}
		if a.ConfigDir != "" {
			setup = "CLAUDE_CONFIG_DIR=" + a.ConfigDir + " claude setup-token"
		}
	}
	ctx := fmt.Sprintf("Account %s cannot log in since %s (met by %s): %s\n\n"+
		"agentd starts no job or session on it, and retries none, until its token file changes or you answer. "+
		"An interactive login expires and cannot be renewed unattended, so give it a long-lived token:\n\n"+
		"1. Run, once, in a terminal: %s\n"+
		"2. Save the token it prints: install -m 600 /dev/null %s && $EDITOR %s\n"+
		"3. In the config, set accounts.%s.token_file to %q.\n\n"+
		"agentd sees the new token file by itself; answer here only to try again without one.",
		name, r.FaultAt.UTC().Format(time.RFC3339), r.FaultSource, r.Fault, setup, tok, tok, name, tok)
	return Decision{ID: accountEntryID(name, r), Type: DecisionChooseOne, Question: "Account " + name + " cannot log in: give it a token?", Context: ctx,
		Options: []DecisionOption{{Key: AccountRetry, Label: "I saved a token: try again", Consequence: "agentd uses the account again; a new failure asks again"},
			{Key: AccountLeave, Label: "Leave it off", Consequence: "the account stays unused, and agentd does not ask again until its token file changes"}}}
}

// accountAnswer is the owner's answer to entry, nil when there is none.
func (st *LogState) accountAnswer(entry string) *Instruction {
	var latest *Instruction
	for _, in := range st.Instructions {
		if in.Re != nil && in.Re.ID == entry && in.Answer != nil && in.Answer.Choice != "" && (latest == nil || in.At.After(latest.At)) {
			latest = in
		}
	}
	return latest
}

// accountNeeds are the faulted accounts that wait on the owner, as cards.
func accountNeeds(cfg *Config, st *LogState) []repoNeed {
	var out []repoNeed
	for _, name := range sortedKeys(st.Accounts) {
		r := st.Accounts[name]
		if !r.faulted() || st.accountAnswer(entryAccount+accountEntryID(name, r)) != nil {
			continue
		}
		d := accountDecision(cfg, name, r)
		n := repoNeed{LevelDecision: LevelDecision{Entry: entryAccount + d.ID, Asked: d.Question, CameBack: oneLine(r.Fault, 400)}}
		for _, o := range d.Options {
			n.Options = append(n.Options, LevelOption{Key: o.Key, Label: o.Label, Consequence: o.Consequence})
		}
		out = append(out, n)
	}
	return out
}

// uiAccountEntry is an account's fault as a console shows it.
type uiAccountEntry struct {
	uiDecisionEntry
	ID   string
	open bool
}

// accountEntries are the faulted accounts, on the console the owner is
// asked about accounts on, each with the owner's answer.
func (src *uiSource) accountEntries(cfg *Config, console string, secret func(...string) bool) []uiAccountEntry {
	if console == "" || console != cfg.accountConsole() {
		return nil
	}
	var out []uiAccountEntry
	for _, name := range sortedKeys(src.st.Accounts) {
		r := src.st.Accounts[name]
		if !r.faulted() {
			continue
		}
		d := accountDecision(cfg, name, r)
		e := uiAccountEntry{uiDecisionEntry: uiDecisionEntry{UIDecision: UIDecision{Repository: console, Decision: d}, At: r.FaultAt}, ID: d.ID, open: true}
		if secret(d.Question, d.Context) {
			e.Withheld = withheldSecret
			e.Decision.Question, e.Decision.Context = "", ""
		}
		if in := src.st.accountAnswer(entryAccount + d.ID); in != nil {
			e.open = false
			e.Answer = &AnswerRecord{AnswerPayload: AnswerPayload{ID: d.ID, Choice: in.Answer.Choice}, At: in.At}
		}
		out = append(out, e)
	}
	return out
}

// accountEntryDecision is the decision entry id answers, for the console
// it is on, and whether it still takes an answer.
func accountEntryDecision(cfg *Config, st *LogState, console, id string) (EntryRe, *Decision, error) {
	name, _, _ := strings.Cut(strings.TrimPrefix(id, entryAccount), ":")
	r, ok := st.Accounts[name]
	if console != cfg.accountConsole() || !ok || !r.faulted() || entryAccount+accountEntryID(name, r) != id {
		return EntryRe{}, nil, fmt.Errorf("%s's console holds no open account fault %q", console, id)
	}
	d := accountDecision(cfg, name, r)
	re := EntryRe{ID: id, Kind: "decision", At: r.FaultAt, Decision: d.ID, Options: decisionOptions(d), Question: d.Question}
	if st.accountAnswer(id) != nil {
		return re, nil, nil
	}
	return re, &d, nil
}

// routeAccount takes the owner's answer on an account's fault, and says
// whether the response was one. Try again clears the fault; leave it
// keeps it, and nothing asks again.
func (s *Supervisor) routeAccount(in *Instruction) bool {
	if in.Re == nil || !strings.HasPrefix(in.Re.ID, entryAccount) || in.Answer == nil || in.Answer.Choice == "" {
		return false
	}
	if !in.Route || in.Failed != "" || !in.Replied.IsZero() || s.shadow {
		return true
	}
	answer := func(text string) {
		s.emit("", Event{Kind: KindRoute, Route: &RoutePayload{Instruction: in.ID, Step: RouteAnswered, Agent: &AgentPayload{Answer: text, By: "agentd"}}})
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(in.Re.ID, entryAccount), ":")
	r := s.st.Accounts[name]
	switch {
	case !r.faulted() || entryAccount+accountEntryID(name, r) != in.Re.ID:
		answer(in.Re.ID + " is settled already; this answer changes nothing.")
	case in.Answer.Choice == AccountRetry:
		if s.emit("", Event{Kind: KindAccount, Mode: ModeShared, Account: &AccountPayload{Account: name, Cleared: FaultClearedOwner}}); s.halt == nil {
			answer("agentd uses account " + name + " again; if it still cannot log in, you are asked again.")
		}
	case in.Answer.Choice == AccountLeave:
		answer("Account " + name + " stays unused until its token file changes.")
	default:
		answer("An account's fault takes retry or leave; nothing was done.")
	}
	return true
}

// accountFaultText is an account's fault in a line, "" when it can log in.
func accountFaultText(r AccountRecord) string {
	if !r.faulted() {
		return ""
	}
	return "cannot log in since " + r.FaultAt.UTC().Format(time.RFC3339) + ": " + oneLine(r.Fault, 200)
}

// loginTail is how many of the screen's last lines that are not blank are
// read for a login prompt: Claude Code shows it at the end, and text
// higher up, a session's own work about logins, is not one.
const loginTail = 6

// loginLine is the line of the end of screen that re matches, "" when
// none does.
func loginLine(re *regexp.Regexp, screen string) string {
	lines := strings.Split(screen, "\n")
	for i, n := len(lines)-1, 0; i >= 0 && n < loginTail; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		n++
		if re.MatchString(l) {
			return l
		}
	}
	return ""
}
