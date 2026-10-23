package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// A worktree's origin must be the configured project, in any of git's URL
// forms; anything else, a fork or a neighbour, is not.
func TestOriginMatchesProject(t *testing.T) {
	for _, u := range []string{
		"git@gitlab.example.com:example-org/widgets.git",
		"git@gitlab.example.com:example-org/widgets",
		"https://gitlab.example.com/example-org/widgets.git",
		"https://oauth2:token@gitlab.example.com/example-org/widgets/",
		"ssh://git@gitlab.example.com:2222/example-org/widgets.git",
		"ssh://git@github.com/Example-Org/Widgets.git",
	} {
		if !originIs(u, "example-org/widgets") {
			t.Errorf("%s: not matched", u)
		}
	}
	for _, u := range []string{
		"git@gitlab.example.com:someone/widgets.git",
		"https://gitlab.example.com/example-org/widgets-fork.git",
		"https://gitlab.example.com/example-org/widgets/sub.git",
		"https://gitlab.example.com/other/example-org/widgets.git",
		"/srv/git/example-org/widgets.git",
		"",
	} {
		if originIs(u, "example-org/widgets") {
			t.Errorf("%s: matched", u)
		}
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
		if e.Kind == KindSkip && strings.Contains(detailString(e, "reason"), "origin") {
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
	err := ExecForge{Timeout: 10 * time.Second}.Push(t.TempDir(), "c0ffee", "feature")
	if err == nil || !strings.Contains(err.Error(), "push -q origin c0ffee:refs/heads/feature") {
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
