package main

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCloneGit answers from a table by directory.
type fakeCloneGit struct {
	origin      map[string]string
	first, last map[string]time.Time
}

func (g fakeCloneGit) Origin(dir string) (string, error) {
	if o, ok := g.origin[dir]; ok {
		return o, nil
	}
	return "", errors.New("no origin")
}

func (g fakeCloneGit) Dates(dir string) (time.Time, time.Time, error) {
	return g.first[dir], g.last[dir], nil
}

// forkAPI says which projects are forks, and counts what it was asked.
type forkAPI struct {
	mu    sync.Mutex
	forks map[string]bool
	asked []string
}

func (a *forkAPI) answer(project string) (APIResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, project)
	if a.forks[project] {
		return APIResponse{Status: 200, Body: []byte(`{"fork":true,"forked_from_project":{"id":1}}`)}, nil
	}
	return APIResponse{Status: 200, Body: []byte(`{"fork":false,"forked_from_project":null}`)}, nil
}

func (a *forkAPI) GitHub(path string, _ map[string]string) (APIResponse, error) {
	return a.answer(strings.TrimPrefix(path, "repos/"))
}

func (a *forkAPI) GitLab(path string) (APIResponse, error) {
	p, _ := url.PathUnescape(strings.TrimPrefix(path, "projects/"))
	return a.answer(p)
}

func (a *forkAPI) At(string) ForgeAPI { return a }

func TestForgeOfURL(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:example-org/tool.git":               "github example-org/tool",
		"https://github.com/Example/Rules":                  "github Example/Rules",
		"https://gitlab.com/example-group/core/payouts.git": "gitlab example-group/core/payouts",
		"ssh://git@gitlab.com/a/b.git":                      "gitlab a/b",
		"https://example.org/a/b.git":                       "",
		"/some/local/path":                                  "",
	} {
		f, p, ok := forgeOfURL(in)
		got := ""
		if ok {
			got = f + " " + p
		}
		if got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

// mkClone makes dir a clone, its .git a folder or, for a worktree, a file.
func mkClone(t *testing.T, dir string, worktree bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if worktree {
		writeFile(t, filepath.Join(dir, ".git"), "gitdir: elsewhere\n")
	} else if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The home page lists every clone under the roots once, by its remote,
// marks forks and stale ones for the page to hide but never a configured
// repository, dates each, and Refresh finds one cloned since (#96).
func TestClonesOnHome(t *testing.T) {
	f := newLevelsFixture(t)
	root := t.TempDir()
	now := time.Now()
	old := now.Add(-3 * 365 * 24 * time.Hour)
	git := fakeCloneGit{origin: map[string]string{}, first: map[string]time.Time{}, last: map[string]time.Time{}}
	clone := func(rel, origin string, worktree bool, first, last time.Time) string {
		dir := filepath.Join(root, rel)
		mkClone(t, dir, worktree)
		git.origin[dir], git.first[dir], git.last[dir] = origin, first, last
		return dir
	}
	recent := now.Add(-10 * 24 * time.Hour)
	fresh := clone("github.com/someone/fresh-tool", "git@github.com:someone/fresh-tool.git", false, now.Add(-400*24*time.Hour), recent)
	clone("github.com/someone/fresh-tool-57", "git@github.com:someone/fresh-tool.git", true, now.Add(-400*24*time.Hour), recent)
	clone("github.com/someone/forked-lib", "https://github.com/someone/forked-lib.git", false, old, recent)
	clone("github.com/someone/ancient", "https://github.com/someone/ancient", false, old, old)
	clone("github.com/elsewhere", "https://gitlab.com/group/sub/elsewhere.git", false, now.Add(-30*24*time.Hour), recent)
	clone("github.com/no-remote", "", false, old, old)
	delete(git.origin, filepath.Join(root, "github.com/no-remote"))
	var widgets string
	for name, r := range f.cfg.Repositories {
		widgets = name
		clone("github.com/"+r.Project, "git@github.com:"+r.Project+".git", false, old, old)
		break
	}
	api := &forkAPI{forks: map[string]bool{"someone/forked-lib": true}}
	u, _ := testUI(t, UIConfig{}, f.src)
	u.home = t.TempDir()
	u.tmuxArgs = []string{"-L", "agentd-test-none", "-f", "/dev/null"}
	u.cloneRoots, u.cloneGit, u.cloneAPI = []string{root}, git, api
	u.cloneFile = filepath.Join(t.TempDir(), "clones.json")
	u.rescan()

	home := owner(t, u, "/")
	for _, want := range []string{`href="/r/fresh-tool"`, `href="/r/forked-lib"`, `href="/r/ancient"`, `href="/r/elsewhere"`, `href="/r/` + widgets + `"`, `class="box-hide" data-hide="fork" checked`, `action="/refresh"`} {
		if !strings.Contains(home, want) {
			t.Errorf("home lacks %s", want)
		}
	}
	if strings.Contains(home, "fresh-tool-57") || strings.Contains(home, "no-remote") {
		t.Error("home lists a second clone of one repository, or one with no forge")
	}
	if page := owner(t, u, "/r/fresh-tool"); !strings.Contains(page, fresh) {
		t.Error("a cloned repository's page is not in the clone laid out as its project")
	}
	rowOf := func(name string) string {
		i := strings.Index(home, `data-ref="`+name+`"`)
		if i < 0 {
			t.Fatalf("no row for %s", name)
		}
		return home[i : i+strings.Index(home[i:], ">")]
	}
	for name, want := range map[string][2]bool{"fresh-tool": {false, false}, "forked-lib": {true, false}, "ancient": {false, true}, widgets: {false, false}} {
		r := rowOf(name)
		if got := [2]bool{strings.Contains(r, `data-fork="1"`), strings.Contains(r, `data-stale="1"`)}; got != want {
			t.Errorf("%s: fork, stale %v, want %v: %s", name, got, want, r)
		}
		if strings.Contains(r, `data-created="0"`) {
			t.Errorf("%s is not dated: %s", name, r)
		}
	}

	// A second scan asks the forge about no repository it answered for.
	asked := len(api.asked)
	if kept := loadClones(u.cloneFile); len(kept) != 5 {
		t.Errorf("the scan kept %d clones, want 5", len(kept))
	}
	clone("github.com/someone/brand-new", "git@github.com:someone/brand-new.git", false, recent, recent)
	if w := ownerPost(u, "/refresh", url.Values{}); w.Code != http.StatusSeeOther {
		t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
	}
	if home := owner(t, u, "/"); !strings.Contains(home, `href="/r/brand-new"`) {
		t.Error("Refresh did not find a new clone")
	}
	if n := len(api.asked) - asked; n != 1 {
		t.Errorf("the second scan asked the forge %d times, want 1: %v", n, api.asked[asked:])
	}
}

// The age sorts put the newest or the oldest first, an undated one last.
func TestRepoAgeSorts(t *testing.T) {
	row := func(name, created string) map[string]string {
		return map[string]string{"ref": name, "name": name, "created": created}
	}
	got := scriptOrder(t, []map[string]string{row("mid", "200"), row("undated", "0"), row("young", "300"), row("old", "100")}, []string{"repo-newest", "repo-oldest"})
	for by, want := range map[string]string{"repo-newest": "young mid old undated", "repo-oldest": "old mid young undated"} {
		if s := strings.Join(got[by], " "); s != want {
			t.Errorf("%s: %s, want %s", by, s, want)
		}
	}
}

func TestShortAge(t *testing.T) {
	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	for d, want := range map[time.Duration]string{3 * 365 * day: "3y", 70 * day: "2mo", 5 * day: "5d", time.Hour: "today"} {
		if got := shortAge(now.Add(-d), now); got != want {
			t.Errorf("%s: %s, want %s", d, got, want)
		}
	}
	if shortAge(time.Time{}, now) != "" {
		t.Error("an unknown date has an age")
	}
}
