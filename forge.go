package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Git reads a worktree. Fetch is its one write to the worktree's refs.
type Git interface {
	Fetch(dir string) error
	Clean(dir string) (bool, error)
	Branch(dir string) (string, error)
	Head(dir string) (string, error)
	// RemoteURLs are origin's fetch and push URLs.
	RemoteURLs(dir string) ([]string, error)
	// Rewrites are the URL prefixes git config rewrites (insteadOf,
	// pushInsteadOf), in every scope.
	Rewrites(dir string) ([]string, error)
	// ConfigHash hashes the worktree's whole git configuration.
	ConfigHash(dir string) (string, error)
	// Ahead counts the commits on HEAD whose changes origin/target lacks:
	// a commit the target already has as a patch, rebased or cherry-picked
	// under another hash, does not count.
	Ahead(dir, target string) (int, error)
	// GateFiles names those of paths, files or directories relative to
	// the worktree, whose content at HEAD or in the working tree is not
	// origin/target's. A path neither has is unchanged.
	GateFiles(dir, target string, paths []string) ([]string, error)
	// Contains says whether origin/target contains the commit sha.
	Contains(dir, sha, target string) (bool, error)
	FirstSubject(dir, target string) (string, error)
	// Messages is every commit message, in full, in origin/target..HEAD.
	Messages(dir, target string) (string, error)
	// LastMerge is the newest merge commit reachable from HEAD, its
	// subject and its commit time, or "" when there is none.
	LastMerge(dir string) (sha, subject string, at time.Time, err error)
}

// MergeRequest is what to open on the forge.
type MergeRequest struct {
	Project     string
	Source      string
	Target      string
	Title       string
	Description string
	Assignee    string
}

// MergeResult names what was opened.
type MergeResult struct {
	Ref    string // "!12" or "#12"
	URL    string
	Head   string // the request's head commit, from Find
	Merged bool   // merged, from Find; otherwise open
}

// Forge performs writes on a forge. It can push and open; it cannot merge.
// Find is its one read: the open and merged requests from a branch of the
// project itself, forks left out, each with its head commit. Closed ones are left out, since the branch may be
// delivered again; the supervisor decides which, if any, delivers HEAD.
type Forge interface {
	// Push sends commit sha to branch at url.
	Push(dir, url, sha, branch string) error
	Open(repo Repository, mr MergeRequest, dir string) (MergeResult, error)
	Find(repo Repository, source, target string) ([]MergeResult, error)
}

// AllowedForge wraps a Forge with the per-repository allow-list. Every write
// the supervisor makes goes through it; the test fakes sit beneath it.
type AllowedForge struct {
	Repos map[string]Repository
	Forge Forge
}

func (a AllowedForge) allowed(repo string, write string) error {
	r, ok := a.Repos[repo]
	if !ok {
		return fmt.Errorf("repository %q is not configured", repo)
	}
	for _, w := range r.Allow {
		if w == write {
			return nil
		}
	}
	return fmt.Errorf("repository %q does not allow %s", repo, write)
}

// PushAllowed pushes if the repository allows it.
func (a AllowedForge) PushAllowed(repo, dir, sha, branch string) error {
	if err := a.allowed(repo, "push"); err != nil {
		return err
	}
	return a.Forge.Push(dir, pushURL(a.Repos[repo]), sha, branch)
}

// pushURL is where a delivery for r goes, built from the configuration
// alone: nothing in the worktree, its remotes or its hooks, decides it.
func pushURL(r Repository) string {
	if r.Transport == "https" {
		return "https://" + r.Host + "/" + r.Project + ".git"
	}
	return "git@" + r.Host + ":" + r.Project + ".git"
}

// glabArgs points a glab call at the repository's host.
func glabArgs(r Repository, args ...string) []string {
	if r.Host != "" {
		return append([]string{"api", "--hostname", r.Host}, args...)
	}
	return append([]string{"api"}, args...)
}

// ghRepo names the repository for gh's -R, with its host.
func ghRepo(r Repository) string {
	if r.Host != "" {
		return r.Host + "/" + r.Project
	}
	return r.Project
}

// originIs says whether a git remote URL names project on host: an https
// or ssh URL, or git's scp-like user@host:path form, whose host is host and
// whose path, less a trailing .git, is the project path. Hosts and forge
// paths ignore case. A local path names no forge project.
func originIs(remote, host, project string) bool {
	var h, path string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || u.Host == "" {
			return false
		}
		h, path = u.Hostname(), u.Path
	} else {
		i := strings.Index(remote, ":")
		if i <= 0 || strings.Contains(remote[:i], "/") {
			return false
		}
		h, path = remote[:i], remote[i+1:]
		if at := strings.LastIndex(h, "@"); at >= 0 {
			h = h[at+1:]
		}
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	return path != "" && strings.EqualFold(h, host) && strings.EqualFold(path, project)
}

// redactRemote drops any credentials from a remote URL, so it can be logged.
func redactRemote(remote string) string {
	if u, err := url.Parse(remote); err == nil && u.User != nil {
		u.User = nil
		return u.String()
	}
	return remote
}

// Find lists the open and merged requests from source to target in a
// configured repository. It reads only, so the allow-list does not apply.
func (a AllowedForge) Find(repo, source, target string) ([]MergeResult, error) {
	r, ok := a.Repos[repo]
	if !ok {
		return nil, fmt.Errorf("repository %q is not configured", repo)
	}
	return a.Forge.Find(r, source, target)
}

// OpenAllowed opens a merge request if the repository allows it.
func (a AllowedForge) OpenAllowed(repo string, mr MergeRequest, dir string) (MergeResult, error) {
	if err := a.allowed(repo, "open"); err != nil {
		return MergeResult{}, err
	}
	return a.Forge.Open(a.Repos[repo], mr, dir)
}

// runTimed runs a command in dir and returns its standard output, or its
// combined output when combined is set. The command runs in a process group
// of its own; if it is still running after timeout the whole group is killed,
// so a child it started (an ssh under git, a pager) dies with it, and its
// output pipes are closed a second later even if something still holds them.
// A command that failed after the deadline gets an error saying it timed
// out, never an *exec.ExitError, so a hung command cannot pass for one that
// answered no; one that succeeded just as the deadline passed is a success.
// env is added to agentd's own environment.
func runTimed(timeout time.Duration, dir string, env []string, combined bool, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := groupCommand(ctx, dir, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var out []byte
	var err error
	if combined {
		out, err = cmd.CombinedOutput()
	} else {
		out, err = cmd.Output()
	}
	if err != nil && ctx.Err() != nil {
		return out, fmt.Errorf("%s timed out after %s and was killed", name, timeout)
	}
	return out, err
}

// groupCommand makes a command that runs in dir in a process group of its
// own, so that when ctx ends the whole group is killed, and whose output
// pipes are closed a second later even if something still holds them.
func groupCommand(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	return cmd
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

// GateOutputMax is how much of a gate's output, its end, is kept.
const GateOutputMax = 64 * 1024

// gitEnv turns off every way git could stop to ask for credentials: an
// unattended fetch or push must fail, not wait for a human.
var gitEnv = []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "GCM_INTERACTIVE=never"}

// ExecGit runs git in a directory, each call bounded by Timeout.
type ExecGit struct{ Timeout time.Duration }

func gitOut(timeout time.Duration, dir string, args ...string) (string, error) {
	out, err := runTimed(timeout, dir, gitEnv, false, "git", args...)
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %s", strings.Join(args, " "), dir, exitMessage(err))
	}
	return string(out), nil
}

// exitMessage prefers a failed command's stderr to Go's "exit status 1".
func exitMessage(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return strings.TrimSpace(string(exit.Stderr))
	}
	return err.Error()
}

func (g ExecGit) Fetch(dir string) error {
	_, err := gitOut(g.Timeout, dir, "fetch", "-q", "origin")
	return err
}

func (g ExecGit) GateFiles(dir, target string, paths []string) ([]string, error) {
	var changed []string
	for _, p := range paths {
		// The object each side names: a blob or a tree, "" when absent.
		object := func(rev string) (string, error) {
			out, err := runTimed(g.Timeout, dir, gitEnv, false, "git", "rev-parse", "--verify", "--quiet", rev+":"+p)
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				return "", nil
			}
			if err != nil {
				return "", fmt.Errorf("git rev-parse %s:%s in %s: %s", rev, p, dir, exitMessage(err))
			}
			return strings.TrimSpace(string(out)), nil
		}
		want, err := object("refs/remotes/origin/" + target)
		if err != nil {
			return nil, err
		}
		have, err := object("HEAD")
		if err != nil {
			return nil, err
		}
		// The working tree: any change, staged or not, or a file git does
		// not track, ignored ones included.
		status, err := gitOut(g.Timeout, dir, "status", "--porcelain", "--untracked-files=all", "--ignored", "--", p)
		if err != nil {
			return nil, err
		}
		if want != have || strings.TrimSpace(status) != "" {
			changed = append(changed, p)
		}
	}
	return changed, nil
}

func (g ExecGit) Clean(dir string) (bool, error) {
	out, err := gitOut(g.Timeout, dir, "status", "--porcelain")
	return strings.TrimSpace(out) == "", err
}

func (g ExecGit) Branch(dir string) (string, error) {
	out, err := gitOut(g.Timeout, dir, "rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out), err
}

func (g ExecGit) Head(dir string) (string, error) {
	out, err := gitOut(g.Timeout, dir, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// RemoteURLs lists origin's fetch URLs, then its push URLs.
func (g ExecGit) RemoteURLs(dir string) ([]string, error) {
	var urls []string
	for _, args := range [][]string{{"remote", "get-url", "--all", "origin"}, {"remote", "get-url", "--push", "--all", "origin"}} {
		out, err := gitOut(g.Timeout, dir, args...)
		if err != nil {
			return nil, err
		}
		urls = append(urls, strings.Fields(out)...)
	}
	return urls, nil
}

// Rewrites lists the prefixes git config rewrites; git config answers 1
// when there are none.
func (g ExecGit) Rewrites(dir string) ([]string, error) {
	out, err := runTimed(g.Timeout, dir, gitEnv, false, "git", "config", "--get-regexp", `^url\..*\.(insteadof|pushinsteadof)$`)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("git config in %s: %s", dir, exitMessage(err))
	}
	var prefixes []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if _, value, ok := strings.Cut(line, " "); ok {
			prefixes = append(prefixes, value)
		}
	}
	return prefixes, nil
}

// ConfigHash hashes every setting git would use in dir, with the file each
// comes from.
func (g ExecGit) ConfigHash(dir string) (string, error) {
	out, err := gitOut(g.Timeout, dir, "config", "--list", "--show-origin")
	return HashPane(out), err
}

func (g ExecGit) Ahead(dir, target string) (int, error) {
	out, err := gitOut(g.Timeout, dir, "rev-list", "--count", "--right-only", "--cherry-pick", "origin/"+target+"...HEAD")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// Contains asks merge-base whether sha is an ancestor of origin/target. It
// answers no with status 1, and fails with 128 when sha is unknown here; after
// the fetch, a commit this clone lacks is not in origin/target either.
func (g ExecGit) Contains(dir, sha, target string) (bool, error) {
	_, err := runTimed(g.Timeout, dir, gitEnv, false, "git", "merge-base", "--is-ancestor", sha, "origin/"+target)
	var exit *exec.ExitError
	if errors.As(err, &exit) && (exit.ExitCode() == 1 || exit.ExitCode() == 128) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("git merge-base in %s: %s", dir, exitMessage(err))
	}
	return true, nil
}

func (g ExecGit) FirstSubject(dir, target string) (string, error) {
	out, err := gitOut(g.Timeout, dir, "log", "--reverse", "--format=%s", "origin/"+target+"..HEAD")
	if err != nil {
		return "", err
	}
	lines := strings.SplitN(strings.TrimSpace(out), "\n", 2)
	return lines[0], nil
}

func (g ExecGit) Messages(dir, target string) (string, error) {
	return gitOut(g.Timeout, dir, "log", "--format=%B", "origin/"+target+"..HEAD")
}

func (g ExecGit) LastMerge(dir string) (string, string, time.Time, error) {
	out, err := gitOut(g.Timeout, dir, "log", "-1", "--merges", "--format=%H%x00%cI%x00%s", "HEAD")
	if err != nil || strings.TrimSpace(out) == "" {
		return "", "", time.Time{}, err
	}
	f := strings.SplitN(strings.TrimSpace(out), "\x00", 3)
	if len(f) != 3 {
		return "", "", time.Time{}, fmt.Errorf("git log: unexpected output %q", out)
	}
	at, err := time.Parse(time.RFC3339, f[1])
	if err != nil {
		return "", "", time.Time{}, err
	}
	return f[0], f[2], at, nil
}

// ExecForge pushes with git and opens with glab or gh, by the repository's
// forge, each call bounded by Timeout. It has no merge method, so no
// configuration can make it merge.
type ExecForge struct{ Timeout time.Duration }

// Push sends exactly sha, the commit the supervisor checked, never
// whatever HEAD has become since, to url, never to a remote the worktree
// names. Hooks are off: a pre-push hook is the branch's own code.
func (f ExecForge) Push(dir, url, sha, branch string) error {
	_, err := gitOut(f.Timeout, dir, "-c", "core.hooksPath=/dev/null", "push", "--no-verify", "-q", url, sha+":refs/heads/"+branch)
	return err
}

func (f ExecForge) Open(repo Repository, mr MergeRequest, dir string) (MergeResult, error) {
	switch repo.Forge {
	case "gitlab":
		return openGitLab(f.Timeout, repo, mr, dir)
	case "github":
		return openGitHub(f.Timeout, repo, mr, dir)
	}
	return MergeResult{}, fmt.Errorf("unknown forge %q", repo.Forge)
}

func (f ExecForge) Find(repo Repository, source, target string) ([]MergeResult, error) {
	switch repo.Forge {
	case "gitlab":
		return findGitLab(f.Timeout, repo, source, target)
	case "github":
		return findGitHub(f.Timeout, repo, source, target)
	}
	return nil, fmt.Errorf("unknown forge %q", repo.Forge)
}

// findGitLab lists the project's merge requests from source to target in
// every state and keeps the open and merged ones, with their head commit
// (sha). A request from a fork, whose source project is not the target
// project, is left out: a same-named branch elsewhere is not this delivery.
func findGitLab(timeout time.Duration, repo Repository, source, target string) ([]MergeResult, error) {
	proj := strings.ReplaceAll(repo.Project, "/", "%2F")
	path := "projects/" + proj + "/merge_requests?state=all&per_page=50&source_branch=" + url.QueryEscape(source) + "&target_branch=" + url.QueryEscape(target)
	out, err := runTimed(timeout, "", nil, false, "glab", glabArgs(repo, path)...)
	if err != nil {
		return nil, fmt.Errorf("glab: %s", exitMessage(err))
	}
	var resp []struct {
		IID    int    `json:"iid"`
		State  string `json:"state"`
		SHA    string `json:"sha"`
		WebURL string `json:"web_url"`
		Source int    `json:"source_project_id"`
		Target int    `json:"target_project_id"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("glab: unreadable reply: %w", err)
	}
	var found []MergeResult
	for _, r := range resp {
		if r.Source != r.Target {
			continue
		}
		if r.State == "opened" || r.State == "merged" {
			found = append(found, MergeResult{Ref: "!" + strconv.Itoa(r.IID), URL: r.WebURL, Head: r.SHA, Merged: r.State == "merged"})
		}
	}
	return found, nil
}

// findGitHub is findGitLab for GitHub, where the head commit is headRefOid
// and a fork's request is cross-repository.
func findGitHub(timeout time.Duration, repo Repository, source, target string) ([]MergeResult, error) {
	out, err := runTimed(timeout, "", nil, false, "gh", "pr", "list", "-R", ghRepo(repo), "--head", source, "--base", target,
		"--state", "all", "--json", "number,url,state,headRefOid,isCrossRepository")
	if err != nil {
		return nil, fmt.Errorf("gh: %s", exitMessage(err))
	}
	var resp []struct {
		Number int    `json:"number"`
		State  string `json:"state"`
		Head   string `json:"headRefOid"`
		URL    string `json:"url"`
		Fork   bool   `json:"isCrossRepository"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("gh: unreadable reply: %w", err)
	}
	var found []MergeResult
	for _, r := range resp {
		if r.Fork {
			continue
		}
		if r.State == "OPEN" || r.State == "MERGED" {
			found = append(found, MergeResult{Ref: "#" + strconv.Itoa(r.Number), URL: r.URL, Head: r.Head, Merged: r.State == "MERGED"})
		}
	}
	return found, nil
}

func openGitLab(timeout time.Duration, repo Repository, mr MergeRequest, dir string) (MergeResult, error) {
	proj := strings.ReplaceAll(repo.Project, "/", "%2F")
	args := glabArgs(repo, "-X", "POST", "projects/"+proj+"/merge_requests",
		"-f", "source_branch="+mr.Source, "-f", "target_branch="+mr.Target,
		"-f", "title="+mr.Title, "-f", "description="+mr.Description, "-f", "remove_source_branch=true")
	if mr.Assignee != "" {
		args = append(args, "-f", "assignee_id="+mr.Assignee)
	}
	out, err := runTimed(timeout, dir, nil, false, "glab", args...)
	if err != nil {
		return MergeResult{}, fmt.Errorf("glab: %s", exitMessage(err))
	}
	var resp struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return MergeResult{}, fmt.Errorf("glab: unreadable reply: %w", err)
	}
	return MergeResult{Ref: "!" + strconv.Itoa(resp.IID), URL: resp.WebURL}, nil
}

func openGitHub(timeout time.Duration, repo Repository, mr MergeRequest, dir string) (MergeResult, error) {
	args := []string{"pr", "create", "-R", ghRepo(repo), "--head", mr.Source, "--base", mr.Target,
		"--title", mr.Title, "--body", mr.Description}
	if mr.Assignee != "" {
		args = append(args, "--assignee", mr.Assignee)
	}
	if strings.HasPrefix(mr.Title, "Draft: ") {
		args = append(args, "--draft")
	}
	out, err := runTimed(timeout, dir, nil, false, "gh", args...)
	if err != nil {
		return MergeResult{}, fmt.Errorf("gh: %s", exitMessage(err))
	}
	url := strings.TrimSpace(string(out))
	ref := url
	if i := strings.LastIndex(url, "/"); i >= 0 {
		ref = "#" + url[i+1:]
	}
	return MergeResult{Ref: ref, URL: url}, nil
}

// Runner runs the escalation command and the delivery gate.
type Runner interface {
	Run(argv []string) error
	// Gate runs argv in dir, bounded by timeout, and returns its exit
	// status; err is a gate that could not run or did not finish.
	Gate(dir string, argv, env []string, timeout time.Duration) (exit int, output string, err error)
}

// ExecRunner runs a command, bounded by Timeout, and reports its failure output.
type ExecRunner struct{ Timeout time.Duration }

func (r ExecRunner) Run(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("no command")
	}
	out, err := runTimed(r.Timeout, "", nil, true, argv[0], argv[1:]...)
	if err != nil {
		return fmt.Errorf("%s: %w: %s", argv[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Gate runs the gate in the worktree with exactly env, nothing inherited,
// in a process group of its own killed at timeout. A relative command such
// as ./bin/check is the worktree's own. The last GateOutputMax bytes of its
// combined output are kept.
func (r ExecRunner) Gate(dir string, argv, env []string, timeout time.Duration) (int, string, error) {
	if len(argv) == 0 {
		return 0, "", fmt.Errorf("no gate command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := groupCommand(ctx, dir, argv[0], argv[1:]...)
	cmd.Env = env
	out := &tailBuffer{max: GateOutputMax}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	if err != nil && ctx.Err() != nil {
		return 0, string(out.b), fmt.Errorf("gate timed out after %s and was killed", timeout)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), string(out.b), nil
	}
	return 0, string(out.b), err
}
