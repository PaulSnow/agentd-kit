package main

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	// the worktree, whose content at head or on disk is not the target
	// branch's, as url has it now; a path none has is unchanged. pinned
	// counts the paths the target has.
	GateFiles(dir, url, target, head string, paths []string) (changed []string, pinned int, err error)
	// Contains says whether origin/target contains the commit sha.
	Contains(dir, sha, target string) (bool, error)
	FirstSubject(dir, target string) (string, error)
	// Messages is every commit message, in full, in origin/target..HEAD.
	Messages(dir, target string) (string, error)
	// Subjects are the short hash and subject of each commit in
	// from..HEAD, newest first, at most max.
	Subjects(dir, from string, max int) ([]string, error)
	// LastMerge is the newest merge commit reachable from HEAD, its
	// subject and its commit time, or "" when there is none.
	LastMerge(dir string) (sha, subject string, at time.Time, err error)
	// AddWorktree adds a worktree at dir from the clone for branch: as
	// the clone or origin has it when it is there, else cut from
	// origin/target. A detached one is at the branch's commit, so a
	// reviewer's worktree beside its worker's, which has the branch out.
	AddWorktree(clone, dir, branch, target string, detach bool) error
	// Tags fetches the clone's tags and lists them, the oldest first
	// (#67); TagNotes is a
	// tag's own message, a NUL, and the CHANGELOG.md the tag holds.
	Tags(clone string) ([]string, error)
	TagNotes(clone, tag string) (string, error)
	// TagWorktree adds a detached worktree at dir at tag, which
	// RemoveWorktree removes.
	TagWorktree(clone, dir, tag string) error
	RemoveWorktree(clone, dir string) error
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

// Forge performs writes on a forge: push, open, and a fast-forward merge.
// Find is its one read: the open and merged requests from a branch of the
// project itself, forks left out, each with its head commit. Closed ones are left out, since the branch may be
// delivered again; the supervisor decides which, if any, delivers HEAD.
type Forge interface {
	// Push sends commit sha to branch at url.
	Push(dir, url, sha, branch string) error
	Open(repo Repository, mr MergeRequest, dir string) (MergeResult, error)
	Find(repo Repository, source, target string) ([]MergeResult, error)
	// Merge merges the request ref, whose head is head, into target by
	// the method of repo's policy: a fast-forward, which fails when it is
	// not one; the forge's merge call, a merge commit or a squash, which
	// fails when the head moved; or the policy's merge command.
	Merge(repo Repository, ref, head, target string) error
	// Retest runs the request's pipeline again, on its merge result where
	// the project tests merge results.
	Retest(repo Repository, ref string) error
	// Comment posts body on an issue or request (#60).
	Comment(repo Repository, ref, body string) error
	// OpenIssue opens an issue and returns its number (#69).
	OpenIssue(repo Repository, title, body string) (int, error)
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
	if write == "merge" && r.Policy.Merge.Enabled || write != "merge" && contains(r.Policy.Delivery.Allow, write) {
		return nil
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

// MergeAllowed fast-forwards a request's target if the repository allows
// merge. Whether the merge is due (merge.go) is the caller's to check.
func (a AllowedForge) MergeAllowed(repo, ref, head, target string) error {
	if err := a.allowed(repo, "merge"); err != nil {
		return err
	}
	return a.Forge.Merge(a.Repos[repo], ref, head, target)
}

// RetestAllowed runs a request's pipeline again if the repository allows
// merge: it is done only on the way to a merge. An answer that the
// pipeline would be empty wraps errEmptyPipeline.
func (a AllowedForge) RetestAllowed(repo, ref string) error {
	if err := a.allowed(repo, "merge"); err != nil {
		return err
	}
	err := a.Forge.Retest(a.Repos[repo], ref)
	if err != nil && emptyPipelineAnswer(err.Error()) {
		return fmt.Errorf("%w: %v", errEmptyPipeline, err)
	}
	return err
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
// unattended fetch or push must fail, not wait for a human. Replace refs,
// which would let a session swap one object for another, are not used.
var gitEnv = []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "GCM_INTERACTIVE=never", "GIT_NO_REPLACE_OBJECTS=1"}

// forgeEnv is gitEnv for gh and glab, which ask for nothing either.
var forgeEnv = append(append([]string(nil), gitEnv...), "GH_PROMPT_DISABLED=1", "NO_PROMPT=1")

// gitSafe go before every git command agentd runs: the worktree's config,
// which the session can write, may not start a file-system monitor or a
// hook in agentd's environment, or answer for files from a cache, and
// paths are literal. Filters, an ssh command and a credential helper the
// config names still run.
var gitSafe = []string{"-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "core.hooksPath=/dev/null", "--literal-pathspecs"}

// git runs git in dir with gitEnv and gitSafe.
func git(timeout time.Duration, dir string, args ...string) ([]byte, error) {
	return runTimed(timeout, dir, gitEnv, false, "git", append(append([]string{}, gitSafe...), args...)...)
}

// ExecGit runs git in a directory, each call bounded by Timeout.
type ExecGit struct{ Timeout time.Duration }

func gitOut(timeout time.Duration, dir string, args ...string) (string, error) {
	out, err := git(timeout, dir, args...)
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

func (g ExecGit) GateFiles(dir, url, target, head string, paths []string) ([]string, int, error) {
	// The target's commit is asked of the configured URL: origin/target is
	// a ref the session can point anywhere, or keep a fetch from moving.
	if err := remoteNamed(g.Timeout, dir, url); err != nil {
		return nil, 0, err
	}
	ref := "refs/heads/" + target
	out, err := gitOut(g.Timeout, dir, "ls-remote", "--", url, ref)
	if err != nil {
		return nil, 0, err
	}
	// ls-remote matches the ref's tail, so refs/heads/x/refs/heads/main
	// answers too; only the exact ref counts.
	want := ""
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == ref {
			want = f[0]
		}
	}
	if want == "" {
		return nil, 0, fmt.Errorf("the target branch %s is not at %s", target, url)
	}
	if _, err := gitOut(g.Timeout, dir, "cat-file", "-e", want+"^{commit}"); err != nil {
		return nil, 0, fmt.Errorf("the target branch %s's commit %s has not been fetched into %s", target, shortHash(want), dir)
	}
	format, err := gitOut(g.Timeout, dir, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, 0, err
	}
	var changed []string
	pinned := 0
	for _, p := range paths {
		target, err := g.treeFiles(dir, want, p)
		if err != nil {
			return nil, 0, err
		}
		committed, err := g.treeFiles(dir, head, p)
		if err != nil {
			return nil, 0, err
		}
		// The working tree is read from disk, not asked of git status,
		// which the index's skip-worktree and assume-unchanged flags and
		// the session's git config can blind.
		disk, err := diskFiles(dir, p, strings.TrimSpace(format))
		if err != nil {
			return nil, 0, err
		}
		if len(target) > 0 {
			pinned++
		}
		if !maps.Equal(target, committed) || !maps.Equal(target, disk) {
			changed = append(changed, p)
		}
	}
	return changed, pinned, nil
}

// treeFiles lists the files at or under p in rev, path to mode and object.
func (g ExecGit) treeFiles(dir, rev, p string) (map[string]string, error) {
	out, err := gitOut(g.Timeout, dir, "ls-tree", "-r", "-z", "--full-tree", rev, "--", p)
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	for _, entry := range strings.Split(out, "\x00") {
		meta, name, ok := strings.Cut(entry, "\t")
		if f := strings.Fields(meta); ok && len(f) == 3 {
			files[name] = f[0] + " " + f[2]
		}
	}
	return files, nil
}

// diskFiles lists the files at or under p in the working tree as git
// would record them, path to mode and object, symbolic links not
// followed. A path that crosses a symbolic link, or a file git cannot
// hold, is listed as such and never matches.
func diskFiles(dir, p, format string) (map[string]string, error) {
	files := map[string]string{}
	parts := strings.Split(p, "/")
	for i := 1; i < len(parts); i++ {
		fi, err := os.Lstat(filepath.Join(dir, filepath.Join(parts[:i]...)))
		if errors.Is(err, os.ErrNotExist) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			files[p] = "crosses " + strings.Join(parts[:i], "/")
			return files, nil
		}
	}
	count := 0
	err := filepath.WalkDir(filepath.Join(dir, p), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		var data []byte
		mode := "100644"
		switch t := d.Type(); {
		case t.IsDir():
			return nil
		case t&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			data, mode = []byte(link), "120000"
		case t.IsRegular():
			if count++; count > gateFilesMax {
				files[rel] = "too many files"
				return filepath.SkipAll
			}
			sum, exec, err := hashFile(path, format)
			if err != nil {
				return err
			}
			if exec {
				mode = "100755"
			}
			files[rel] = mode + " " + sum
			return nil
		default:
			files[rel] = "not a file: " + t.String()
			return nil
		}
		files[rel] = mode + " " + blobHash(format, data)
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return files, nil
	}
	return files, err
}

// gateFilesMax and gateFileMax bound what diskFiles reads under one gate
// path: a gate is a few small scripts, and anything larger is a change.
const (
	gateFilesMax = 1000
	gateFileMax  = 16 << 20
)

// hashFile is the blob name of the regular file at path, and whether git
// would record it executable. It opens without blocking and checks what it
// opened, so a file swapped for a pipe cannot hang it; a file over
// gateFileMax gets a name no blob has.
func hashFile(path, format string) (string, bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	if !fi.Mode().IsRegular() {
		return "not a file: " + fi.Mode().Type().String(), false, nil
	}
	if fi.Size() > gateFileMax {
		return "too large", false, nil
	}
	data := make([]byte, fi.Size())
	if _, err := io.ReadFull(f, data); err != nil {
		return "", false, err
	}
	return blobHash(format, data), fi.Mode()&0o100 != 0, nil
}

// remoteNamed refuses a worktree whose git config has a remote named url:
// git would take url as that remote's name and use its url or pushurl,
// which the session can point anywhere.
func remoteNamed(timeout time.Duration, dir, url string) error {
	out, err := git(timeout, dir, "config", "--get-regexp", `^remote\..*\.(url|pushurl)$`)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil
	}
	if err != nil {
		return fmt.Errorf("git config in %s: %s", dir, exitMessage(err))
	}
	for _, line := range strings.Split(string(out), "\n") {
		key, _, _ := strings.Cut(line, " ")
		name := strings.TrimPrefix(key, "remote.")
		if i := strings.LastIndex(name, "."); i >= 0 && name[:i] == url {
			return fmt.Errorf("git config in %s names a remote %s, which git would use in place of that URL", dir, url)
		}
	}
	return nil
}

// blobHash is the object name git gives a blob holding data.
func blobHash(format string, data []byte) string {
	var h hash.Hash = sha1.New()
	if format == "sha256" {
		h = sha256.New()
	}
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func (g ExecGit) AddWorktree(clone, dir, branch, target string, detach bool) error {
	if _, err := gitOut(g.Timeout, clone, "fetch", "-q", "origin"); err != nil {
		return err
	}
	local := gitOK(g.Timeout, clone, "show-ref", "--verify", "-q", "refs/heads/"+branch)
	remote := gitOK(g.Timeout, clone, "show-ref", "--verify", "-q", "refs/remotes/origin/"+branch)
	var args []string
	switch {
	case detach && local:
		args = []string{"worktree", "add", "-q", "--detach", dir, "refs/heads/" + branch}
	case detach && remote:
		args = []string{"worktree", "add", "-q", "--detach", dir, "refs/remotes/origin/" + branch}
	case detach:
		args = []string{"worktree", "add", "-q", "--detach", dir, "refs/remotes/origin/" + target}
	case local:
		args = []string{"worktree", "add", "-q", dir, branch}
	case remote:
		args = []string{"worktree", "add", "-q", "--track", "-b", branch, dir, "origin/" + branch}
	default:
		args = []string{"worktree", "add", "-q", "--no-track", "-b", branch, dir, "origin/" + target}
	}
	_, err := gitOut(g.Timeout, clone, args...)
	return err
}

func (g ExecGit) Tags(clone string) ([]string, error) {
	if _, err := gitOut(g.Timeout, clone, "fetch", "-q", "--tags", "origin"); err != nil {
		return nil, err
	}
	out, err := gitOut(g.Timeout, clone, "tag", "--list", "--sort=creatordate")
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func (g ExecGit) TagNotes(clone, tag string) (string, error) {
	notes, err := gitOut(g.Timeout, clone, "tag", "--list", "--format=%(contents)", "--", tag)
	if err != nil {
		return "", err
	}
	changelog, _ := gitOut(g.Timeout, clone, "show", "refs/tags/"+tag+":CHANGELOG.md")
	return notes + "\x00" + changelog, nil
}

func (g ExecGit) TagWorktree(clone, dir, tag string) error {
	_, err := gitOut(g.Timeout, clone, "worktree", "add", "-q", "--detach", dir, "refs/tags/"+tag+"^{commit}")
	return err
}

func (g ExecGit) RemoveWorktree(clone, dir string) error {
	_, err := gitOut(g.Timeout, clone, "worktree", "remove", "--force", dir)
	return err
}

// gitOK says whether a git command exits 0.
func gitOK(timeout time.Duration, dir string, args ...string) bool {
	_, err := git(timeout, dir, args...)
	return err == nil
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
	out, err := git(g.Timeout, dir, "config", "--get-regexp", `^url\..*\.(insteadof|pushinsteadof)$`)
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
	_, err := git(g.Timeout, dir, "merge-base", "--is-ancestor", sha, "origin/"+target)
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

func (g ExecGit) Subjects(dir, from string, max int) ([]string, error) {
	out, err := gitOut(g.Timeout, dir, "log", "--format=%h %s", "--max-count="+strconv.Itoa(max), from+"..HEAD")
	if err != nil || strings.TrimSpace(out) == "" {
		return nil, err
	}
	return strings.Split(strings.TrimSpace(out), "\n"), nil
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

// ExecForge pushes with git, and opens and merges with glab or gh, by the
// repository's forge, each call bounded by Timeout. Its merge is a
// fast-forward only.
type ExecForge struct{ Timeout time.Duration }

// Push sends exactly sha, the commit the supervisor checked, never
// whatever HEAD has become since, to url, never to a remote the worktree
// names. Hooks are off: a pre-push hook is the branch's own code.
func (f ExecForge) Push(dir, url, sha, branch string) error {
	if err := remoteNamed(f.Timeout, dir, url); err != nil {
		return err
	}
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

func (f ExecForge) Merge(repo Repository, ref, head, target string) error {
	method := repo.Policy.Merge.Method
	if method == MergeByCommand {
		return mergeByCommand(f.Timeout, repo, ref, head, target)
	}
	switch repo.Forge {
	case "gitlab":
		return mergeGitLab(f.Timeout, repo, ref, head, method == MergeSquash)
	case "github":
		if method == MergeCommit || method == MergeSquash {
			return mergeGitHubCall(f.Timeout, repo, ref, head, method)
		}
		return mergeGitHub(f.Timeout, repo, head, target)
	}
	return fmt.Errorf("unknown forge %q", repo.Forge)
}

// errEmptyPipeline begins the error of a retest the forge answered with
// an empty pipeline: the project's CI rules make no merge-request
// pipelines, so asking again cannot help (#46).
var errEmptyPipeline = errors.New("the request's new pipeline would be empty")

// emptyPipelineAnswers are the ways GitLab's English API answers say so:
// the merge request's own, the rules' and an older one. An answer in another wording, or
// another language, is taken as any failed retest: retried, then given up.
var emptyPipelineAnswers = []string{"would have been empty", "rules configuration prevented any jobs", "No stages / jobs for this pipeline"}

// emptyPipelineAnswer reports whether a forge answer says the pipeline
// would be empty.
func emptyPipelineAnswer(s string) bool {
	for _, a := range emptyPipelineAnswers {
		if strings.Contains(s, a) {
			return true
		}
	}
	return false
}

// Retest creates a new pipeline for a GitLab merge request, which runs
// on its merge result where merged results pipelines are on. GitHub has
// no such call.
func (f ExecForge) Retest(repo Repository, ref string) error {
	if repo.Forge != "gitlab" {
		return fmt.Errorf("%s cannot run a request's pipeline again", repo.Forge)
	}
	proj := strings.ReplaceAll(repo.Project, "/", "%2F")
	args := glabArgs(repo, "-X", "POST", "projects/"+proj+"/merge_requests/"+strings.TrimPrefix(ref, "!")+"/pipelines")
	if out, err := runTimed(f.Timeout, "", forgeEnv, false, "glab", args...); err != nil {
		return retestError(exitMessage(err), string(out))
	}
	return nil
}

// retestError is a failed retest's error, with the forge's answer from
// standard output too when only that says the pipeline would be empty.
func retestError(msg, out string) error {
	if !emptyPipelineAnswer(msg) && emptyPipelineAnswer(out) {
		msg += ": " + strings.TrimSpace(cutUTF8(out, 500))
	}
	return fmt.Errorf("glab: %s", msg)
}

// mergeByCommand runs the policy's merge command, with no shell, its
// placeholders substituted, and the forge's no-prompt environment.
func mergeByCommand(timeout time.Duration, repo Repository, ref, head, target string) error {
	r := strings.NewReplacer("{repository}", repo.Project, "{request}", ref, "{head}", head, "{target}", target)
	argv := make([]string, len(repo.Policy.Merge.Command))
	for i, a := range repo.Policy.Merge.Command {
		argv[i] = r.Replace(a)
	}
	if _, err := runTimed(timeout, "", forgeEnv, false, argv[0], argv[1:]...); err != nil {
		return fmt.Errorf("%s: %s", argv[0], exitMessage(err))
	}
	return nil
}

// mergeGitLab merges the request through GitLab's merge call, which
// merges only while the request's head is sha. Without squash it
// fast-forwards when the caller saw the project's merge method is ff,
// under which GitLab refuses a request that is not a fast-forward, and
// otherwise makes the project's merge.
func mergeGitLab(timeout time.Duration, repo Repository, ref, head string, squash bool) error {
	proj := strings.ReplaceAll(repo.Project, "/", "%2F")
	args := glabArgs(repo, "-X", "PUT", "projects/"+proj+"/merge_requests/"+strings.TrimPrefix(ref, "!")+"/merge",
		"-f", "sha="+head, "-F", "squash="+strconv.FormatBool(squash))
	if _, err := runTimed(timeout, "", forgeEnv, false, "glab", args...); err != nil {
		return fmt.Errorf("glab: %s", exitMessage(err))
	}
	return nil
}

// mergeGitHub moves the target branch to head without force, which GitHub
// does only for a fast-forward; GitHub has no fast-forward merge method,
// and marks the request merged once the target contains its head.
func mergeGitHub(timeout time.Duration, repo Repository, head, target string) error {
	args := []string{"api", "--method", "PATCH", "repos/" + repo.Project + "/git/refs/heads/" + escapeRef(target),
		"-f", "sha=" + head, "-F", "force=false"}
	if repo.Host != "" {
		args = append(args, "--hostname", repo.Host)
	}
	if _, err := runTimed(timeout, "", forgeEnv, false, "gh", args...); err != nil {
		return fmt.Errorf("gh: %s", exitMessage(err))
	}
	return nil
}

// mergeGitHubCall merges the pull request through GitHub's merge call,
// which merges only while its head is head, as a merge commit or a squash.
func mergeGitHubCall(timeout time.Duration, repo Repository, ref, head, method string) error {
	args := []string{"api", "--method", "PUT", "repos/" + repo.Project + "/pulls/" + strings.TrimPrefix(ref, "#") + "/merge",
		"-f", "sha=" + head, "-f", "merge_method=" + method}
	if repo.Host != "" {
		args = append(args, "--hostname", repo.Host)
	}
	if _, err := runTimed(timeout, "", forgeEnv, false, "gh", args...); err != nil {
		return fmt.Errorf("gh: %s", exitMessage(err))
	}
	return nil
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
