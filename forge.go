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
	"time"
)

// Git reads a worktree. Fetch is its one write to the worktree's refs.
type Git interface {
	Fetch(dir string) error
	Clean(dir string) (bool, error)
	Branch(dir string) (string, error)
	Head(dir string) (string, error)
	Ahead(dir, target string) (int, error)
	FirstSubject(dir, target string) (string, error)
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
	Ref string // "!12" or "#12"
	URL string
}

// Forge performs writes on a forge. It can push and open; it cannot merge.
// Find is its one read: the request that already delivers head from a
// branch, if any. An open request on the branch counts whatever its head,
// since a push updates it; a merged one counts only when it carried head, so
// new commits on a reused branch name are delivered again; a closed one never.
type Forge interface {
	Push(dir, branch string) error
	Open(repo Repository, mr MergeRequest, dir string) (MergeResult, error)
	Find(repo Repository, source, target, head string) (MergeResult, bool, error)
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
func (a AllowedForge) PushAllowed(repo, dir, branch string) error {
	if err := a.allowed(repo, "push"); err != nil {
		return err
	}
	return a.Forge.Push(dir, branch)
}

// Find looks for an existing request from source to target that delivers
// head, in a configured repository. It reads only, so the allow-list does
// not apply.
func (a AllowedForge) Find(repo, source, target, head string) (MergeResult, bool, error) {
	r, ok := a.Repos[repo]
	if !ok {
		return MergeResult{}, false, fmt.Errorf("repository %q is not configured", repo)
	}
	return a.Forge.Find(r, source, target, head)
}

// OpenAllowed opens a merge request if the repository allows it.
func (a AllowedForge) OpenAllowed(repo string, mr MergeRequest, dir string) (MergeResult, error) {
	if err := a.allowed(repo, "open"); err != nil {
		return MergeResult{}, err
	}
	return a.Forge.Open(a.Repos[repo], mr, dir)
}

// runTimed runs a command in dir and returns its standard output, or its
// combined output when combined is set. A command still running after timeout
// is killed, and so is anything holding its output open a second later; the
// error then says it timed out, and is never an *exec.ExitError, so a hung
// command cannot pass for one that answered no. env is added to agentd's own
// environment.
func runTimed(timeout time.Duration, dir string, env []string, combined bool, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.WaitDelay = time.Second
	var out []byte
	var err error
	if combined {
		out, err = cmd.CombinedOutput()
	} else {
		out, err = cmd.Output()
	}
	if ctx.Err() != nil {
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

func (g ExecGit) Ahead(dir, target string) (int, error) {
	out, err := gitOut(g.Timeout, dir, "rev-list", "--count", "origin/"+target+"..HEAD")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

func (g ExecGit) FirstSubject(dir, target string) (string, error) {
	out, err := gitOut(g.Timeout, dir, "log", "--reverse", "--format=%s", "origin/"+target+"..HEAD")
	if err != nil {
		return "", err
	}
	lines := strings.SplitN(strings.TrimSpace(out), "\n", 2)
	return lines[0], nil
}

// ExecForge pushes with git and opens with glab or gh, by the repository's
// forge, each call bounded by Timeout. It has no merge method, so no
// configuration can make it merge.
type ExecForge struct{ Timeout time.Duration }

func (f ExecForge) Push(dir, branch string) error {
	_, err := gitOut(f.Timeout, dir, "push", "-q", "-u", "origin", branch)
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

func (f ExecForge) Find(repo Repository, source, target, head string) (MergeResult, bool, error) {
	switch repo.Forge {
	case "gitlab":
		return findGitLab(f.Timeout, repo, source, target, head)
	case "github":
		return findGitHub(f.Timeout, repo, source, target, head)
	}
	return MergeResult{}, false, fmt.Errorf("unknown forge %q", repo.Forge)
}

// findGitLab lists the project's merge requests from source to target in
// every state and returns the first that is open, or merged with head as its
// head commit (sha); a closed one does not count, since the branch may be
// delivered again.
func findGitLab(timeout time.Duration, repo Repository, source, target, head string) (MergeResult, bool, error) {
	proj := strings.ReplaceAll(repo.Project, "/", "%2F")
	path := "projects/" + proj + "/merge_requests?state=all&per_page=50&source_branch=" + url.QueryEscape(source) + "&target_branch=" + url.QueryEscape(target)
	out, err := runTimed(timeout, "", nil, false, "glab", "api", path)
	if err != nil {
		return MergeResult{}, false, fmt.Errorf("glab: %s", exitMessage(err))
	}
	var resp []struct {
		IID    int    `json:"iid"`
		State  string `json:"state"`
		SHA    string `json:"sha"`
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return MergeResult{}, false, fmt.Errorf("glab: unreadable reply: %w", err)
	}
	for _, r := range resp {
		if r.State == "opened" || (r.State == "merged" && r.SHA == head) {
			return MergeResult{Ref: "!" + strconv.Itoa(r.IID), URL: r.WebURL}, true, nil
		}
	}
	return MergeResult{}, false, nil
}

// findGitHub is findGitLab for GitHub, where the head commit is headRefOid.
func findGitHub(timeout time.Duration, repo Repository, source, target, head string) (MergeResult, bool, error) {
	out, err := runTimed(timeout, "", nil, false, "gh", "pr", "list", "-R", repo.Project, "--head", source, "--base", target,
		"--state", "all", "--json", "number,url,state,headRefOid")
	if err != nil {
		return MergeResult{}, false, fmt.Errorf("gh: %s", exitMessage(err))
	}
	var resp []struct {
		Number int    `json:"number"`
		State  string `json:"state"`
		Head   string `json:"headRefOid"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return MergeResult{}, false, fmt.Errorf("gh: unreadable reply: %w", err)
	}
	for _, r := range resp {
		if r.State == "OPEN" || (r.State == "MERGED" && r.Head == head) {
			return MergeResult{Ref: "#" + strconv.Itoa(r.Number), URL: r.URL}, true, nil
		}
	}
	return MergeResult{}, false, nil
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

// Runner runs the escalation command.
type Runner interface {
	Run(argv []string) error
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
