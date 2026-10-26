package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// briefHarness is a live harness with session c in widgets, alive, its
// transcripts read and its brief assembled from a common part and a
// repository part.
func briefHarness(t *testing.T) (*harness, string, string) {
	t.Helper()
	h := newHarness(t, false, "c")
	h.cfg.TranscriptDir = filepath.Join(h.dir, "projects")
	common, repo := filepath.Join(h.dir, "common.md"), filepath.Join(h.dir, "widgets.md")
	writeFile(t, common, "# Work\n\nKeep STATUS.md current.\n")
	writeFile(t, repo, "## widgets\n\nMerge into main only.\n")
	c := h.cfg.Sessions["c"]
	c.Brief = []string{common, "widgets.md"} // relative to the config's directory
	h.cfg.Sessions["c"] = c
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatal(err)
	}
	h.alive("c", "screen of c")
	h.restart(false)
	return h, common, repo
}

// launchSession has name found gone and restarted, so it runs under a
// transcript agentd launched, and returns that transcript's id.
func launchSession(t *testing.T, h *harness, name string) string {
	t.Helper()
	h.tmux.alive[name] = false
	h.ticks(1, time.Minute)
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActRestart && e.Session == name {
			return e.Act.Transcript
		}
	}
	t.Fatalf("%s was not restarted", name)
	return ""
}

// rotateAt gives c, launched under transcript id, a context over the
// threshold and a merge since its launch, after the one its checkout was
// first seen at, and lets the grace pass.
func rotateAt(h *harness, id, merge string) {
	h.t.Helper()
	appendFile(h.t, filepath.Join(TranscriptDir(h.cfg.TranscriptDir, h.cfg.Sessions["c"].Cwd), id+".jsonl"), usageLine("u-"+merge, 10, 150000, 0, 5, false))
	if h.git.merge == "" {
		h.git.merge, h.git.mergeSubject = "base", "Merge #20: base"
		h.ticks(1, time.Minute)
	}
	h.git.merge, h.git.mergeSubject = merge, "Merge #21: "+merge
	h.ticks(8, time.Minute)
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

// A configured session's brief is written from its parts at each start
// (#50): a changed part reaches the next start, and the launch and the
// session's track record the parts' hashes.
func TestSessionBriefAssembledAtStart(t *testing.T) {
	h, common, repo := briefHarness(t)
	launchSession(t, h, "c")
	want := "# Work\n\nKeep STATUS.md current.\n\n## widgets\n\nMerge into main only.\n"
	if got := h.brief(); got != want {
		t.Fatalf("brief = %q, want %q", got, want)
	}
	parts := []BriefPart{{Path: common, Hash: shortHash(HashPane("# Work\n\nKeep STATUS.md current.\n"))},
		{Path: repo, Hash: shortHash(HashPane("## widgets\n\nMerge into main only.\n"))}}
	if a := lastLaunch(h, "c"); a == nil || !reflect.DeepEqual(a.Brief, parts) {
		t.Fatalf("launch brief = %+v, want %+v", a, parts)
	}
	if b := h.sup.Tracks()["c"].Brief; !reflect.DeepEqual(b, parts) {
		t.Fatalf("track brief = %+v, want %+v", b, parts)
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
	// After a restore from the log the track still has them.
	h.restart(false)
	if b := h.sup.Tracks()["c"].Brief; len(b) != 2 || b[1] != lastLaunch(h, "c").Brief[1] {
		t.Fatalf("track brief after replay = %+v", b)
	}
}

// A part that cannot be read refuses the start, naming its path, with one
// escalation; the brief written last is left as it was.
func TestSessionBriefMissingPartRefusesStart(t *testing.T) {
	h, _, repo := briefHarness(t)
	launchSession(t, h, "c")
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
	// The part back, the session starts.
	writeFile(t, repo, "## widgets\n\nBack.\n")
	h.ticks(1, time.Minute)
	if n := h.count(KindAction, ActRestart, "c"); n != restarts+1 || !strings.HasSuffix(h.brief(), "Back.\n") {
		t.Fatalf("restarts %d, brief %q", n, h.brief())
	}
}

// A rotation writes the brief from its parts too.
func TestSessionBriefAssembledAtRotation(t *testing.T) {
	h, _, repo := briefHarness(t)
	id := launchSession(t, h, "c")
	writeFile(t, repo, "## widgets\n\nRotated.\n")
	rotateAt(h, id, "m1")
	if n := h.count(KindAction, ActRotate, "c"); n != 1 {
		t.Fatalf("rotations = %d, want 1", n)
	}
	if !strings.HasSuffix(h.brief(), "Rotated.\n") || lastLaunch(h, "c").Brief[1].Hash != shortHash(HashPane("## widgets\n\nRotated.\n")) {
		t.Fatalf("brief at rotation = %q", h.brief())
	}
}

// A brief part is a path; an empty one is refused at load.
func TestSessionBriefEmptyPartRefused(t *testing.T) {
	h, _, _ := briefHarness(t)
	c := h.cfg.Sessions["c"]
	c.Brief = []string{""}
	h.cfg.Sessions["c"] = c
	if err := h.cfg.finish(h.dir); err == nil || !strings.Contains(err.Error(), "sessions.c.brief[0]") {
		t.Fatalf("an empty brief part: %v", err)
	}
}

// A start refused after the brief is read, here for its account, leaves
// the brief and the parts the track holds as the latest launch made them,
// as a replay of the log has them.
func TestSessionBriefRefusedLaunchWritesNothing(t *testing.T) {
	h, _, repo := briefHarness(t)
	launchSession(t, h, "c")
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
	if b := h.sup.Tracks()["c"].Brief; !reflect.DeepEqual(b, parts) {
		t.Fatalf("track brief = %+v, want the latest launch's %+v", b, parts)
	}
	sameAfterReplay(t, h, "after a refused start")
}

// Shadow mode reads the parts and records the start it would make, but
// writes no brief.
func TestSessionBriefShadowWritesNothing(t *testing.T) {
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
func TestSessionNoBriefLeavesOwners(t *testing.T) {
	h, _, _ := briefHarness(t)
	c := h.cfg.Sessions["c"]
	c.Brief = nil
	h.cfg.Sessions["c"] = c
	writeFile(t, filepath.Join(h.dir, "c", "BRIEF.md"), "the owner's\n")
	launchSession(t, h, "c")
	if got := h.brief(); got != "the owner's\n" {
		t.Fatalf("brief = %q", got)
	}
	if a := lastLaunch(h, "c"); a.Brief != nil {
		t.Fatalf("launch brief = %+v", a.Brief)
	}
}

// A rotation refused for a part that cannot be read leaves the session
// running, with one escalation naming the part.
func TestSessionBriefMissingPartRefusesRotation(t *testing.T) {
	h, _, repo := briefHarness(t)
	id := launchSession(t, h, "c")
	before := h.brief()
	if err := os.Remove(repo); err != nil {
		t.Fatal(err)
	}
	h.alive("c", "screen of c")
	rotateAt(h, id, "m1")
	if n := h.count(KindAction, ActRotate, "c"); n != 0 {
		t.Fatalf("rotations = %d, want 0", n)
	}
	var escalations []string
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActEscalate && e.Session == "c" && strings.HasPrefix(e.Act.Key, "brief:") {
			escalations = append(escalations, e.Act.Message)
		}
	}
	if len(escalations) != 1 || !strings.Contains(escalations[0], repo) || !strings.Contains(escalations[0], "not rotated") || !strings.Contains(escalations[0], "sessions.c") {
		t.Fatalf("escalations %q", escalations)
	}
	if !h.tmux.alive["c"] || h.brief() != before {
		t.Fatalf("alive %v, brief %q", h.tmux.alive["c"], h.brief())
	}
}
