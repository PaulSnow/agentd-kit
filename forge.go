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
	// RemoteURL is the URL of the worktree's origin.
	RemoteURL(dir string) (string, error)
	// Ahead counts the commits on HEAD whose changes origin/target lacks:
	// a commit the target already has as a patch, rebased or cherry-picked
	// under another hash, does not count.
	Ahead(dir, target string) (int, error)
	// Contains says whether origin/target contains the commit sha.
	Contains(dir, sha, target string) (bool, error)
	FirstSubject(dir, target string) (string, error)
	// Messages is every commit message, in full, in origin/target..HEAD.
	Messages(dir, target string) (string, error)
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
	// Push sends commit sha to the remote branch.
	Push(dir, sha, branch string) error
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
	return a.Forge.Push(dir, sha, branch)
}

// originIs says whether a git remote URL names project on a forge: an
// https or ssh URL, or git's scp-like user@host:path form, whose path, less
// a trailing .git, is the project path. Forge paths ignore case. A local
// path names no forge project.
func originIs(remote, project string) bool {
	var path string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || u.Host == "" {
			return false
		}
		path = u.Path
	} else {
		i := strings.Index(remote, ":")
		if i <= 0 || strings.Contains(remote[:i], "/") {
			return false
		}
		path = remote[i+1:]
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	return path != "" && strings.EqualFold(path, project)
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
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
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

func (g ExecGit) RemoteURL(dir string) (string, error) {
	out, err := gitOut(g.Timeout, dir, "remote", "get-url", "origin")
	return strings.TrimSpace(out), err
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

// ExecForge pushes with git and opens with glab or gh, by the repository's
// forge, each call bounded by Timeout. It has no merge method, so no
// configuration can make it merge.
type ExecForge struct{ Timeout time.Duration }

// Push sends exactly sha, the commit the supervisor checked, never
// whatever HEAD has become since.
func (f ExecForge) Push(dir, sha, branch string) error {
	_, err := gitOut(f.Timeout, dir, "push", "-q", "origin", sha+":refs/heads/"+branch)
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
	out, err := runTimed(timeout, "", nil, false, "glab", "api", path)
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
	out, err := runTimed(timeout, "", nil, false, "gh", "pr", "list", "-R", repo.Project, "--head", source, "--base", target,
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
	args := []string{"api", "-X", "POST", "projects/" + proj + "/merge_requests",
		"-f", "source_branch=" + mr.Source, "-f", "target_branch=" + mr.Target,
		"-f", "title=" + mr.Title, "-f", "description=" + mr.Description, "-f", "remove_source_branch=true"}
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
	args := []string{"pr", "create", "-R", repo.Project, "--head", mr.Source, "--base", mr.Target,
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
	Gate(dir string, argv []string, timeout time.Duration) (int, error)
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

// Gate runs the gate in the worktree. A relative command such as
// ./bin/check is the worktree's own. Its output is not kept: the exit
// status is the result, and the output may hold anything.
func (r ExecRunner) Gate(dir string, argv []string, timeout time.Duration) (int, error) {
	if len(argv) == 0 {
		return 0, fmt.Errorf("no gate command")
	}
	_, err := runTimed(timeout, dir, nil, true, argv[0], argv[1:]...)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return 0, err
}
