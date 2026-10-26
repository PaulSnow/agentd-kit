package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// briefHarness is coordHarness with c's brief assembled from a common part
// and a repository part.
func briefHarness(t *testing.T) (*harness, string, string) {
	t.Helper()
	h := coordHarness(t)
	common, repo := filepath.Join(h.dir, "common.md"), filepath.Join(h.dir, "widgets.md")
	writeFile(t, common, "# Coordinator\n\nJudge each due item.\n")
	writeFile(t, repo, "## widgets\n\nMerge into main only.\n")
	w := h.cfg.Repositories["widgets"]
	w.Coordinator.Brief = []string{common, "widgets.md"} // relative to the config's directory
	h.cfg.Repositories["widgets"] = w
	delete(h.cfg.Sessions, "c")
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	return h, common, repo
}

func (h *harness) brief() string {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, "c", "BRIEF.md"))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

// lastLaunch is the latest restart or rotation of name.
func lastLaunch(h *harness, name string) *ActPayload {
	var a *ActPayload
	for _, e := range h.events() {
		if e.Kind == KindAction && (e.Act.Action == ActRestart || e.Act.Action == ActRotate) && e.Session == name {
			a = e.Act
		}
	}
	return a
}

// A coordinator's brief is written from its parts at each start (#50): a
// changed part reaches the next start, and the launch and the pack record
// the parts' hashes.
func TestCoordinatorBriefAssembledAtStart(t *testing.T) {
	h, common, repo := briefHarness(t)
	launchCoordinator(t, h)
	want := "# Coordinator\n\nJudge each due item.\n\n## widgets\n\nMerge into main only.\n"
	if got := h.brief(); got != want {
		t.Fatalf("brief = %q, want %q", got, want)
	}
	parts := []BriefPart{{Path: common, Hash: shortHash(HashPane("# Coordinator\n\nJudge each due item.\n"))},
		{Path: repo, Hash: shortHash(HashPane("## widgets\n\nMerge into main only.\n"))}}
	if a := lastLaunch(h, "c"); a == nil || !reflect.DeepEqual(a.Brief, parts) {
		t.Fatalf("launch brief = %+v, want %+v", a, parts)
	}
	if p := h.pack(t); !reflect.DeepEqual(p.Brief, parts) {
		t.Fatalf("pack brief = %+v, want %+v", p.Brief, parts)
	}

	writeFile(t, repo, "## widgets\n\nMerge into main and release.\n")
	h.ticks(1, time.Hour) // past the restart backoff
	h.tmux.alive["c"] = false
	h.ticks(1, time.Minute)
	if got := h.brief(); !strings.HasSuffix(got, "Merge into main and release.\n") {
		t.Fatalf("a changed part did not reach the next start: %q", got)
	}
	if a := lastLaunch(h, "c"); a.Brief[1].Hash != shortHash(HashPane("## widgets\n\nMerge into main and release.\n")) {
		t.Fatalf("launch brief = %+v", a.Brief)
	}
	// After a restore from the log the pack still has them.
	h.restart(false)
	if p, _ := h.sup.buildPack("widgets", nil, h.clock.Now(), false); len(p.Brief) != 2 || p.Brief[1] != lastLaunch(h, "c").Brief[1] {
		t.Fatalf("pack brief after replay = %+v", p.Brief)
	}
}

// A part that cannot be read refuses the start, naming its path, with one
// escalation; the brief written last is left as it was.
func TestCoordinatorBriefMissingPartRefusesStart(t *testing.T) {
	h, _, repo := briefHarness(t)
	launchCoordinator(t, h)
	before := h.brief()
	if err := os.Remove(repo); err != nil {
		t.Fatal(err)
	}
	restarts := h.count(KindAction, ActRestart, "c")
	h.ticks(1, time.Hour)
	h.tmux.alive["c"] = false
	h.ticks(3, time.Hour)
	if n := h.count(KindAction, ActRestart, "c"); n != restarts {
		t.Fatalf("restarted with a missing part: %d restarts, want %d", n, restarts)
	}
	var skips, escalations []string
	for _, e := range h.events() {
		switch {
		case e.Kind == KindSkip && e.Session == "c" && strings.HasPrefix(e.Skip.Key, "brief:"):
			skips = append(skips, e.Skip.Reason)
		case e.Kind == KindAction && e.Act.Action == ActEscalate && e.Session == "c" && strings.HasPrefix(e.Act.Key, "brief:"):
			escalations = append(escalations, e.Act.Message)
		}
	}
	if len(skips) != 1 || !strings.Contains(skips[0], repo) || len(escalations) != 1 || !strings.Contains(escalations[0], repo) {
		t.Fatalf("skips %q, escalations %q", skips, escalations)
	}
	if h.brief() != before {
		t.Fatal("the brief changed on a refused start")
	}
	// The part back, the coordinator starts.
	writeFile(t, repo, "## widgets\n\nBack.\n")
	h.ticks(1, time.Minute)
	if n := h.count(KindAction, ActRestart, "c"); n != restarts+1 || !strings.HasSuffix(h.brief(), "Back.\n") {
		t.Fatalf("restarts %d, brief %q", n, h.brief())
	}
}

// A rotation writes the brief from its parts too.
func TestCoordinatorBriefAssembledAtRotation(t *testing.T) {
	h, _, repo := briefHarness(t)
	id := launchCoordinator(t, h)
	writeFile(t, repo, "## widgets\n\nRotated.\n")
	h.ticks(2, time.Minute)
	h.transcriptOf("c", id, promptLine("c0", "start", h.clock.Now())+usageLine("m0", 10, 150000, 0, 5, false))
	h.status("a", "# a\n\nDONE\n")
	h.ticks(3, time.Minute)
	h.transcriptOf("c", id, summaryLine("c1", "m1", "Handled.", h.clock.Now(), 150000))
	h.ticks(2, time.Minute)
	if n := h.count(KindAction, ActRotate, "c"); n != 1 {
		t.Fatalf("rotations = %d, want 1", n)
	}
	if !strings.HasSuffix(h.brief(), "Rotated.\n") || lastLaunch(h, "c").Brief[1].Hash != shortHash(HashPane("## widgets\n\nRotated.\n")) {
		t.Fatalf("brief at rotation = %q", h.brief())
	}
}

// A brief part is a path; an empty one is refused at load.
func TestCoordinatorBriefEmptyPartRefused(t *testing.T) {
	h := coordHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Coordinator.Brief = []string{""}
	h.cfg.Repositories["widgets"] = w
	delete(h.cfg.Sessions, "c")
	if err := h.cfg.finish(h.dir); err == nil || !strings.Contains(err.Error(), "coordinator.brief") {
		t.Fatalf("an empty brief part: %v", err)
	}
}
