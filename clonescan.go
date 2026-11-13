package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The clones on this machine (#96): the home page lists every repository
// cloned under the repositories' roots, configured or not, and Refresh
// looks again. A clone is known by its origin remote, so two clones of one
// repository (a worktree, a numbered copy) are one row, and a directory
// named unlike its project is still found. Each is marked a fork, as its
// forge says, and stale, when its last commit is older than staleAfter,
// so the page can hide both; the first commit dates it, for the age sort.

// staleAfter is how old a clone's last commit is for it to be stale.
const staleAfter = 2 * 365 * 24 * time.Hour

// scanDepth is how deep under a root a clone is looked for: host, owner,
// groups, name.
const scanDepth = 4

// scannedClone is one repository cloned on this machine.
type scannedClone struct {
	Forge   string `json:"forge"`
	Project string `json:"project"`
	Path    string `json:"path"`
	// Created is its first commit, Last its latest, on the clone's HEAD.
	Created time.Time `json:"created"`
	Last    time.Time `json:"last"`
	// Fork is what its forge says; Checked says it answered.
	Fork    bool `json:"fork"`
	Checked bool `json:"checked"`
}

// key is the clone's repository, as repoKey has it.
func (c scannedClone) key() string { return repoKey(c.Forge, c.Project) }

// stale says the clone's last commit is older than staleAfter at now.
func (c scannedClone) stale(now time.Time) bool {
	return !c.Last.IsZero() && now.Sub(c.Last) > staleAfter
}

// cloneGit is what the scan asks of git about a clone: its origin's URL,
// and its HEAD's first and last commit times.
type cloneGit interface {
	Origin(dir string) (string, error)
	Dates(dir string) (first, last time.Time, err error)
}

// execCloneGit asks git itself, each call bounded by Timeout.
type execCloneGit struct{ Timeout time.Duration }

func (g execCloneGit) Origin(dir string) (string, error) {
	out, err := runTimed(g.Timeout, "", nil, false, "git", "-C", dir, "remote", "get-url", "origin")
	return strings.TrimSpace(string(out)), err
}

func (g execCloneGit) Dates(dir string) (time.Time, time.Time, error) {
	out, err := runTimed(g.Timeout, "", nil, false, "git", "-C", dir, "log", "-1", "--format=%ct")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	last := unixLine(out)
	out, err = runTimed(g.Timeout, "", nil, false, "git", "-C", dir, "log", "--max-parents=0", "--format=%ct", "HEAD")
	if err != nil {
		return time.Time{}, last, err
	}
	// A history may have several roots; the oldest dates it.
	var first time.Time
	for _, l := range strings.Fields(string(out)) {
		if t := unixLine([]byte(l)); !t.IsZero() && (first.IsZero() || t.Before(first)) {
			first = t
		}
	}
	return first, last, nil
}

// unixLine is the Unix time out holds, zero when it holds none.
func unixLine(out []byte) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

// forgeOfURL is the forge and project a remote URL names, ok false for a
// URL on neither GitHub nor GitLab.
func forgeOfURL(remote string) (forge, project string, ok bool) {
	host, path := "", ""
	switch {
	case strings.Contains(remote, "://"):
		u, err := url.Parse(remote)
		if err != nil {
			return "", "", false
		}
		host, path = u.Hostname(), u.Path
	case strings.Contains(remote, ":"): // git@host:owner/name
		at := remote[strings.Index(remote, "@")+1:]
		i := strings.Index(at, ":")
		host, path = at[:i], at[i+1:]
	default:
		return "", "", false
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	forge = map[string]string{"github.com": "github", "gitlab.com": "gitlab"}[strings.ToLower(host)]
	if forge == "" || !strings.Contains(path, "/") {
		return "", "", false
	}
	return forge, path, true
}

// findClones lists the clones under each root, to scanDepth: a directory
// holding .git, a folder or a worktree's file, not looked into further.
func findClones(roots []string) []string {
	var out []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && depth > 0 {
			out = append(out, dir)
			return
		}
		if depth == scanDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				walk(filepath.Join(dir, e.Name()), depth+1)
			}
		}
	}
	for _, r := range roots {
		if r != "" {
			walk(r, 0)
		}
	}
	return out
}

// scanClones finds the clones under roots, one per repository, with their
// dates, and marks each fork as known, from was, the last scan, or from
// the forge when was does not know it. Of two clones of one repository the
// one laid out as its project is kept, else one that is no worktree, else
// the first by path.
func scanClones(roots []string, git cloneGit, api ForgeAPI, was map[string]scannedClone) map[string]scannedClone {
	type found struct {
		c       scannedClone
		laidOut bool
		tree    bool
	}
	dirs := findClones(roots)
	results := make([]*found, len(dirs))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, dir := range dirs {
		wg.Add(1)
		go func(i int, dir string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			remote, err := git.Origin(dir)
			if err != nil {
				return
			}
			forge, project, ok := forgeOfURL(remote)
			if !ok {
				return
			}
			c := scannedClone{Forge: forge, Project: project, Path: dir}
			c.Created, c.Last, _ = git.Dates(dir)
			fi, err := os.Lstat(filepath.Join(dir, ".git"))
			results[i] = &found{c: c, laidOut: strings.HasSuffix(strings.ToLower(dir), strings.ToLower("/"+project)), tree: err == nil && !fi.IsDir()}
		}(i, dir)
	}
	wg.Wait()
	best := map[string]*found{}
	for _, f := range results {
		if f == nil {
			continue
		}
		k := f.c.key()
		b := best[k]
		if b == nil || f.laidOut && !b.laidOut || f.laidOut == b.laidOut && !f.tree && b.tree {
			best[k] = f
		}
	}
	out := map[string]scannedClone{}
	var ask []string
	for k, f := range best {
		c := f.c
		if w, ok := was[k]; ok && w.Checked {
			c.Fork, c.Checked = w.Fork, true
		} else {
			ask = append(ask, k)
		}
		out[k] = c
	}
	if api != nil {
		sort.Strings(ask)
		var mu sync.Mutex
		for _, k := range ask {
			wg.Add(1)
			go func(k string) {
				defer wg.Done()
				slots <- struct{}{}
				defer func() { <-slots }()
				mu.Lock()
				c := out[k]
				mu.Unlock()
				fork, ok := forgeFork(api, c.Forge, c.Project)
				mu.Lock()
				c.Fork, c.Checked = fork, ok
				out[k] = c
				mu.Unlock()
			}(k)
		}
		wg.Wait()
	}
	return out
}

// forgeFork says whether the forge has project as a fork; ok false when it
// did not answer, so the next scan asks again.
func forgeFork(api ForgeAPI, forge, project string) (fork, ok bool) {
	var body []byte
	switch forge {
	case "github":
		resp, err := api.GitHub("repos/"+project, nil)
		if err != nil || resp.Status != 200 {
			return false, false
		}
		body = resp.Body
	case "gitlab":
		resp, err := api.GitLab("projects/" + url.PathEscape(project))
		if err != nil || resp.Status != 200 {
			return false, false
		}
		body = resp.Body
	default:
		return false, false
	}
	var r struct {
		Fork       bool            `json:"fork"`
		ForkedFrom json.RawMessage `json:"forked_from_project"`
	}
	if json.Unmarshal(body, &r) != nil {
		return false, false
	}
	return r.Fork || len(r.ForkedFrom) > 0 && string(r.ForkedFrom) != "null", true
}

// cloneFile is where the last scan is kept, beside the event log.
func cloneFile(eventLog string) string {
	return filepath.Join(filepath.Dir(eventLog), "clones.json")
}

// loadClones reads the last scan; none when there is none.
func loadClones(path string) map[string]scannedClone {
	out := map[string]scannedClone{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var list []scannedClone
	if err := json.Unmarshal(b, &list); err != nil {
		fmt.Fprintf(os.Stderr, "agentd ui: %s: %v; scanning again\n", path, err)
		return out
	}
	for _, c := range list {
		out[c.key()] = c
	}
	return out
}

// saveClones keeps a scan, replacing the file whole.
func saveClones(path string, clones map[string]scannedClone) error {
	list := make([]scannedClone, 0, len(clones))
	for _, k := range sortedKeys(clones) {
		list = append(list, clones[k])
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// shortAge is how long ago t was, in its largest unit: 3y, 5mo, 12d.
func shortAge(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch day := 24 * time.Hour; {
	case d >= 365*day:
		return strconv.Itoa(int(d/(365*day))) + "y"
	case d >= 30*day:
		return strconv.Itoa(int(d/(30*day))) + "mo"
	case d >= day:
		return strconv.Itoa(int(d/day)) + "d"
	default:
		return "today"
	}
}
