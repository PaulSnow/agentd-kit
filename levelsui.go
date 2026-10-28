package main

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The pages of the owner's three levels (#69): /repo/{repo} and
// /repo/{repo}/issue/{N}, built from the fold of the run that serves the
// repository; a page load reads the log and calls no forge and no model.

// uiRepoPage is level 2.
type uiRepoPage struct {
	uiPage
	RepoLevel
	Talk         []ScopeEntry
	Src          string
	IssueSorts   []string
	RequestSorts []string
	// Shown is the count line's last figure before the page's script
	// filters: every row.
	Shown int
}

// uiIssuePage is level 3.
type uiIssuePage struct {
	uiPage
	IssueLevel
	Talk []ScopeEntry
	Src  string
	All  bool
	// To is the session that works the issue and takes messages, "" for
	// none: the box offers now only with one.
	To string
}

// Scope is what the page's console box is about: a repository, or one
// of its issues.
func (p *uiRepoPage) Scope() string { return p.Repository }

// ScopeNote says where the console box's words go.
func (p *uiRepoPage) ScopeNote() string {
	return "About " + p.Repository + ": a question is answered from its issues, requests and sessions; an instruction becomes work under the issue it names, or a new one. Nothing here interrupts a session."
}

// NowOffered says whether the box offers "now": at an issue only.
func (p *uiRepoPage) NowOffered() bool { return false }

// NowOffered says whether the box offers "now": at an issue a session
// that takes messages works.
func (p *uiIssuePage) NowOffered() bool { return p.To != "" }

// NowOffered says whether the box offers "now": at an issue only.
func (v *uiIndex) NowOffered() bool { return false }

// Scope is the issue's scope, repo#N.
func (p *uiIssuePage) Scope() string { return p.Repository + p.Ref }

// ScopeNote says where the console box's words go.
func (p *uiIssuePage) ScopeNote() string {
	if p.To == "" {
		return "About " + p.Ref + ": a question is answered from its steps and its requests; no session works it now, so a message goes to the repository's thread."
	}
	return "About " + p.Ref + ": a question is answered from its steps, its session and its requests; a message goes to its session at its next turn."
}

// levelSource runs fn on the live source that serves repo, the run of its
// loop first, under its lock, with the config, the fold and the open
// session decisions; it says whether one serves it.
func (u *UI) levelSource(repo string, now time.Time, fn func(src *uiSource, cfg *Config, st *LogState, ds []UIDecision)) (bool, error) {
	var pick *uiSource
	for _, src := range u.sources {
		if _, ok := src.cfg.Repositories[repo]; !ok || src.st.Mode != ModeLive || src.echo {
			continue
		}
		if pick == nil || looped(src.cfg.Repositories[repo]) && !looped(pick.cfg.Repositories[repo]) {
			pick = src
		}
	}
	if pick == nil { // only a live run's index is the forge's, and takes a form
		return false, nil
	}
	pick.mu.Lock()
	defer pick.mu.Unlock()
	cfg, rep, err := pick.view(now)
	if err != nil {
		return true, err
	}
	fn(pick, cfg, pick.st, DecisionsFrom(cfg, pick.st, rep))
	return true, nil
}

// repoView is level 2 of repo, nil when no source configures it.
func (u *UI) repoView(repo string, now time.Time) (*uiRepoPage, error) {
	var v *uiRepoPage
	ok, err := u.levelSource(repo, now, func(src *uiSource, cfg *Config, st *LogState, ds []UIDecision) {
		v = &uiRepoPage{uiPage: uiPage{Title: repo, Token: u.token, Back: "/repo/" + url.PathEscape(repo), Now: now, loc: u.loc},
			RepoLevel: BuildRepoLevel(cfg, st, repo, ds, now), Src: src.Name, IssueSorts: IssueSorts, RequestSorts: RequestSorts}
		v.Shown = len(v.Issues)
	})
	if !ok || err != nil {
		return nil, err
	}
	v.Talk = u.conversation(v.Src, v.Scope())
	return v, nil
}

// issueView is level 3 of repo's issue n, nil when no source configures
// repo or nothing is known of the issue.
func (u *UI) issueView(repo string, n int, all bool, now time.Time) (*uiIssuePage, error) {
	var v *uiIssuePage
	ok, err := u.levelSource(repo, now, func(src *uiSource, cfg *Config, st *LogState, ds []UIDecision) {
		l := BuildIssueLevel(cfg, st, repo, n, ds, all, now)
		if !l.Known {
			return
		}
		title := repo + " #" + strconv.Itoa(n)
		v = &uiIssuePage{uiPage: uiPage{Title: title, Token: u.token, Back: "/repo/" + url.PathEscape(repo) + "/issue/" + strconv.Itoa(n), Now: now, loc: u.loc},
			IssueLevel: l, Src: src.Name, All: all, To: issueSession(cfg, st, l)}
	})
	if !ok || err != nil || v == nil {
		return v, err
	}
	v.Talk = u.conversation(v.Src, v.Scope())
	return v, nil
}

// levelPath splits /repo/{repo}[/issue/{N}] into the repository and the
// issue's number, 0 for the repository's page; ok is false for any other
// path. A repository's name may hold a slash, as the console's may.
func levelPath(path string) (repo string, n int, ok bool) {
	rest := strings.TrimPrefix(path, "/repo/")
	if i := strings.LastIndex(rest, "/issue/"); i > 0 {
		n, err := strconv.Atoi(rest[i+len("/issue/"):])
		if err != nil || n <= 0 {
			return "", 0, false
		}
		return rest[:i], n, true
	}
	if rest == "" || strings.HasSuffix(rest, "/") {
		return "", 0, false
	}
	return rest, 0, true
}

// homeLevel fills the index's home level: a row per configured
// repository from the run that serves it, the sentence across them, and
// today's tokens from every source's ledger.
func (u *UI) homeLevel(v *uiIndex, now time.Time) error {
	names := map[string]string{} // each repository's forge page
	spends := &AccountLedger{}   // copies, read under each source's lock
	for _, src := range u.sources {
		src.mu.Lock()
		for r, c := range src.cfg.Repositories {
			if w := webURL(c, 0, 0); w != "" || names[r] == "" {
				names[r] = w
			}
		}
		if src.st.Ledger != nil && src.st.Mode == ModeLive && !src.echo { // as the account reports count
			spends.Spends = append(spends.Spends, src.st.Ledger.Spends...)
		}
		if v.Src == "" && src.st.Mode == ModeLive {
			v.Src = src.Name
		}
		src.mu.Unlock()
	}
	for _, repo := range sortedKeys(names) {
		row := HomeRow{Repository: repo, Sentence: "No live run serves " + repo + ".", URL: names[repo]}
		if _, err := u.levelSource(repo, now, func(_ *uiSource, cfg *Config, st *LogState, ds []UIDecision) {
			row = BuildHomeRow(cfg, st, repo, ds, now)
		}); err != nil {
			return err
		}
		v.Home = append(v.Home, row)
	}
	v.Sentence = homeSentence(v.Home)
	if v.Src != "" {
		v.Talk = u.conversation(v.Src, ScopeHome)
	}
	v.Today = todayTokens([]*AccountLedger{spends}, now, u.loc)
	return nil
}

// Scope is the home page's scope.
func (v *uiIndex) Scope() string { return "home" }

// Repository is the home page's: none.
func (v *uiIndex) Repository() string { return "" }

// ScopeNote says where the home console box's words go.
func (v *uiIndex) ScopeNote() string {
	return "About every repository: a question is answered from their issues, requests and the usage ledger; an instruction goes to the repository it names, or to agentd itself. Nothing here interrupts a session."
}

// scopeReply records what the owner wrote in scope c's console box from
// src. A typed answer that is an option of the scope's one open decision
// is recorded as that decision's answer, as its button records it;
// anything else is the scope's instruction, to the issue's session when
// the scope is an issue one works.
func (src *uiSource) scopeReply(c Scope, text string, now bool, files []stagedAttachment) (string, error) {
	at := src.clock()
	src.mu.Lock()
	cfg, rep, err := src.view(at)
	if err != nil {
		src.mu.Unlock()
		return "", err
	}
	ds := DecisionsFrom(cfg, src.st, rep)
	var open []LevelDecision
	repoOf := map[string]string{}
	to := ""
	switch {
	case c.Issue > 0:
		l := BuildIssueLevel(cfg, src.st, c.Repository, c.Issue, ds, false, at)
		open = l.Decisions
		for _, d := range open {
			repoOf[d.Entry] = c.Repository
		}
		to = issueSession(cfg, src.st, l)
	case c.Repository != "":
		// Every decision open at the repository: on its issues, and its
		// sessions' and items' that are on none; a typed answer binds
		// only when there is one.
		open = repoDecisions(cfg, src.st, c.Repository, ds)
		for _, d := range open {
			repoOf[d.Entry] = c.Repository
		}
	}
	// At home a typed answer binds nothing: the decisions are every
	// repository's, and the owner answers one on its page.
	src.mu.Unlock()
	if entry, a := typedDecision(open, text); entry != "" {
		return "the answer to " + entry, src.respond(repoOf[entry], entry, "", "", a, files)
	}
	switch {
	case now && c.Issue == 0:
		return "", errors.New("now applies only at an issue a session works; at home or a repository nothing interrupts a session")
	case now && to == "":
		return "", fmt.Errorf("no session that takes messages works %s, so nothing can be typed into one now; send it without now and it goes to the repository's thread", c)
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if err := src.catchUp(); err != nil {
		return "", err
	}
	what := "a message for " + c.String()
	if to != "" {
		what = "a message for " + to
	}
	return what, ScopeInstruction(withRuntime(src.cfg, src.st.Runtime), src.log, src.clock(), c, to, text, now, files)
}

// issueSession is the session that works issue l and takes messages, ""
// for none.
func issueSession(cfg *Config, st *LogState, l IssueLevel) string {
	for _, r := range l.Refs {
		if r.Kind == "session" && takesMessages(cfg, st, r.Name) {
			return r.Name
		}
	}
	return ""
}

// takesMessages says whether session name can be handed a message from
// the issue's box: one of the repository's that is not a reviewer or a
// judge, and not done, stopped, failed, crash-looping or parked; else the
// repository's thread takes the message.
func takesMessages(cfg *Config, st *LogState, name string) bool {
	s, ok := cfg.Sessions[name]
	if !ok || s.Role != "" {
		return false
	}
	t := st.Tracks[name]
	switch stateOf(t) {
	case Done, Stopped, Failed, CrashLooping:
		return false
	}
	return t == nil || !t.Parked
}

// repoDecisions are every decision open at repo, each once: those on its
// issues, and those of its sessions and its items on none.
func repoDecisions(cfg *Config, st *LogState, repo string, ds []UIDecision) []LevelDecision {
	var out []LevelDecision
	seen := map[string]bool{}
	add := func(d LevelDecision) {
		if !seen[d.Entry] {
			seen[d.Entry] = true
			out = append(out, d)
		}
	}
	secret := confidentialMatcher(cfg)
	c := withRuntime(cfg, st.Runtime)
	for ref, i := range st.Index[repo] {
		if ref[0] == '#' {
			for _, d := range levelDecisions(cfg, st, i.Number(), joinIssue(cfg, st, repo, i.Number()), ds, secret) {
				add(d)
			}
		}
	}
	// Those on no issue the index holds count too, so one cannot be
	// taken for the only one.
	var items []*WorkItem
	for _, id := range st.sortedWork() {
		if it := st.Work[id]; it.Repository == repo {
			items = append(items, it)
		}
	}
	var sessions []string
	for _, name := range sortedKeys(c.Sessions) {
		if c.Sessions[name].Repository == repo {
			sessions = append(sessions, name)
		}
	}
	for _, d := range levelDecisionsOf(cfg, st, 0, IssueJoin{Work: items, Sessions: sessions}, ds) {
		add(d)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Entry < out[b].Entry })
	return out
}

// Conversation is the page's console box's exchanges, newest last.
func (u *UI) conversation(srcName, scope string) []ScopeEntry {
	src := u.source(srcName)
	if src == nil {
		return nil
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	secret := confidentialMatcher(src.cfg)
	out := src.st.scopeConversation(scope, scopeShown)
	for k := range out {
		if secret(out[k].Text) {
			out[k].Text = withheldMessage
		}
		if secret(out[k].Answer) {
			out[k].Answer = withheldMessage
		}
	}
	return out
}

// scopeShown is how many exchanges a console box shows.
const scopeShown = 10
