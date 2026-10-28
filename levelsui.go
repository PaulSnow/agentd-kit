package main

import (
	"net/url"
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
	Src string
	All bool
}

// Scope is what the page's console box is about: a repository, or one
// of its issues.
func (p *uiRepoPage) Scope() string { return p.Repository }

// ScopeNote says where the console box's words go.
func (p *uiRepoPage) ScopeNote() string {
	return "About " + p.Repository + ": a question is answered from its issues, requests and sessions; an instruction becomes work under the issue it names, or a new one. Nothing here interrupts a session."
}

// Scope is the issue's scope, repo#N.
func (p *uiIssuePage) Scope() string { return p.Repository + p.Ref }

// ScopeNote says where the console box's words go.
func (p *uiIssuePage) ScopeNote() string {
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
			IssueLevel: l, Src: src.Name, All: all}
	})
	if !ok || err != nil {
		return nil, err
	}
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
	names := map[string]bool{}
	spends := &AccountLedger{} // copies, read under each source's lock
	for _, src := range u.sources {
		src.mu.Lock()
		for r := range src.cfg.Repositories {
			names[r] = true
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
		row := HomeRow{Repository: repo, Sentence: "No live run serves " + repo + "."}
		if _, err := u.levelSource(repo, now, func(_ *uiSource, cfg *Config, st *LogState, ds []UIDecision) {
			row = BuildHomeRow(cfg, st, repo, ds, now)
		}); err != nil {
			return err
		}
		v.Home = append(v.Home, row)
	}
	v.Sentence = homeSentence(v.Home)
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
