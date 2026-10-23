package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Git reads a worktree. Fetch is its one write to the worktree's refs.
type Git interface {
	Fetch(dir string) error
	Clean(dir string) (bool, error)
	Branch(dir string) (string, error)
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
type Forge interface {
	Push(dir, branch string) error
	Open(repo Repository, mr MergeRequest, dir string) (MergeResult, error)
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

// OpenAllowed opens a merge request if the repository allows it.
func (a AllowedForge) OpenAllowed(repo string, mr MergeRequest, dir string) (MergeResult, error) {
	if err := a.allowed(repo, "open"); err != nil {
		return MergeResult{}, err
	}
	return a.Forge.Open(a.Repos[repo], mr, dir)
}

// ExecGit runs git in a directory.
type ExecGit struct{}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
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

func (ExecGit) Fetch(dir string) error {
	_, err := gitOut(dir, "fetch", "-q", "origin")
	return err
}

func (ExecGit) Clean(dir string) (bool, error) {
	out, err := gitOut(dir, "status", "--porcelain")
	return strings.TrimSpace(out) == "", err
}

func (ExecGit) Branch(dir string) (string, error) {
	out, err := gitOut(dir, "rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out), err
}

func (ExecGit) Ahead(dir, target string) (int, error) {
	out, err := gitOut(dir, "rev-list", "--count", "origin/"+target+"..HEAD")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

func (ExecGit) FirstSubject(dir, target string) (string, error) {
	out, err := gitOut(dir, "log", "--reverse", "--format=%s", "origin/"+target+"..HEAD")
	if err != nil {
		return "", err
	}
	lines := strings.SplitN(strings.TrimSpace(out), "\n", 2)
	return lines[0], nil
}

// ExecForge pushes with git and opens with glab or gh, by the repository's
// forge. It has no merge method, so no configuration can make it merge.
type ExecForge struct{}

func (ExecForge) Push(dir, branch string) error {
	_, err := gitOut(dir, "push", "-q", "-u", "origin", branch)
	return err
}

func (ExecForge) Open(repo Repository, mr MergeRequest, dir string) (MergeResult, error) {
	switch repo.Forge {
	case "gitlab":
		return openGitLab(repo, mr, dir)
	case "github":
		return openGitHub(repo, mr, dir)
	}
	return MergeResult{}, fmt.Errorf("unknown forge %q", repo.Forge)
}

func openGitLab(repo Repository, mr MergeRequest, dir string) (MergeResult, error) {
	proj := strings.ReplaceAll(repo.Project, "/", "%2F")
	args := []string{"api", "-X", "POST", "projects/" + proj + "/merge_requests",
		"-f", "source_branch=" + mr.Source, "-f", "target_branch=" + mr.Target,
		"-f", "title=" + mr.Title, "-f", "description=" + mr.Description, "-f", "remove_source_branch=true"}
	if mr.Assignee != "" {
		args = append(args, "-f", "assignee_id="+mr.Assignee)
	}
	cmd := exec.Command("glab", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
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

func openGitHub(repo Repository, mr MergeRequest, dir string) (MergeResult, error) {
	args := []string{"pr", "create", "-R", repo.Project, "--head", mr.Source, "--base", mr.Target,
		"--title", mr.Title, "--body", mr.Description}
	if mr.Assignee != "" {
		args = append(args, "--assignee", mr.Assignee)
	}
	if strings.HasPrefix(mr.Title, "Draft: ") {
		args = append(args, "--draft")
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
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

// ExecRunner runs a command and reports its failure output.
type ExecRunner struct{}

func (ExecRunner) Run(argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("no command")
	}
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", argv[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}
