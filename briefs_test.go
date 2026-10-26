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

// A start refused after the brief is read, here for its account, leaves
// the brief and the parts the pack shows as the latest launch made them,
// as a replay of the log has them.
func TestCoordinatorBriefRefusedLaunchWritesNothing(t *testing.T) {
	h, _, repo := briefHarness(t)
	launchCoordinator(t, h)
	before, parts := h.brief(), lastLaunch(h, "c").Brief
	writeFile(t, repo, "## widgets\n\nNot yet.\n")
	h.cfg.Accounts = map[string]Account{"acct": {TokenFile: filepath.Join(h.dir, "no-token")}}
	c := h.cfg.Sessions["c"]
	c.Account = "acct"
	h.cfg.Sessions["c"] = c
	restarts := h.count(KindAction, ActRestart, "c")
	h.ticks(1, time.Hour)
	h.tmux.alive["c"] = false
	h.ticks(2, time.Minute)
	if n := h.count(KindAction, ActRestart, "c"); n != restarts {
		t.Fatalf("restarted without its account: %d restarts, want %d", n, restarts)
	}
	if h.brief() != before {
		t.Fatalf("a refused start wrote the brief: %q", h.brief())
	}
	if p, _ := h.sup.buildPack("widgets", nil, h.clock.Now(), false); !reflect.DeepEqual(p.Brief, parts) {
		t.Fatalf("pack brief = %+v, want the latest launch's %+v", p.Brief, parts)
	}
	sameAfterReplay(t, h, "after a refused start")
}

// Shadow mode reads the parts and records the start it would make, but
// writes no brief.
func TestCoordinatorBriefShadowWritesNothing(t *testing.T) {
	h, _, _ := briefHarness(t)
	h.restart(true)
	h.tmux.alive["c"] = false
	h.ticks(1, time.Minute)
	if _, err := os.Stat(filepath.Join(h.dir, "c", "BRIEF.md")); !os.IsNotExist(err) {
		t.Fatalf("shadow mode wrote BRIEF.md: %v", err)
	}
	var would *ActPayload
	for _, e := range h.events() {
		if e.Kind == KindWould && e.Act.Action == ActRestart && e.Session == "c" {
			would = e.Act
		}
	}
	if would == nil || len(would.Brief) != 2 {
		t.Fatalf("shadow launch = %+v, want the parts it would write", would)
	}
	if b := h.sup.Tracks()["c"].Brief; b != nil {
		t.Fatalf("shadow track brief = %+v", b)
	}
	sameAfterReplay(t, h, "in shadow mode")
}

// With no brief listed BRIEF.md is the owner's: a start leaves it and
// records no parts.
func TestCoordinatorNoBriefLeavesOwners(t *testing.T) {
	h := coordHarness(t)
	writeFile(t, filepath.Join(h.dir, "c", "BRIEF.md"), "the owner's\n")
	launchCoordinator(t, h)
	if got := h.brief(); got != "the owner's\n" {
		t.Fatalf("brief = %q", got)
	}
	if a := lastLaunch(h, "c"); a.Brief != nil {
		t.Fatalf("launch brief = %+v", a.Brief)
	}
}

// A rotation refused for a part that cannot be read leaves the
// coordinator running, with one escalation naming the part.
func TestCoordinatorBriefMissingPartRefusesRotation(t *testing.T) {
	h, _, repo := briefHarness(t)
	id := launchCoordinator(t, h)
	before := h.brief()
	if err := os.Remove(repo); err != nil {
		t.Fatal(err)
	}
	h.ticks(2, time.Minute)
	h.transcriptOf("c", id, promptLine("c0", "start", h.clock.Now())+usageLine("m0", 10, 150000, 0, 5, false))
	h.status("a", "# a\n\nDONE\n")
	h.ticks(3, time.Minute)
	h.transcriptOf("c", id, summaryLine("c1", "m1", "Handled.", h.clock.Now(), 150000))
	h.ticks(3, time.Minute)
	if n := h.count(KindAction, ActRotate, "c"); n != 0 {
		t.Fatalf("rotations = %d, want 0", n)
	}
	var escalations []string
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActEscalate && e.Session == "c" && strings.HasPrefix(e.Act.Key, "brief:") {
			escalations = append(escalations, e.Act.Message)
		}
	}
	if len(escalations) != 1 || !strings.Contains(escalations[0], repo) || !strings.Contains(escalations[0], "not rotated") {
		t.Fatalf("escalations %q", escalations)
	}
	if !h.tmux.alive["c"] || h.brief() != before {
		t.Fatalf("alive %v, brief %q", h.tmux.alive["c"], h.brief())
	}
}
