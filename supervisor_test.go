package main

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const working = "# Working on the feature\nHalf done.\n"
const blocked = "# Blocked\nWaiting.\n\n## Questions\n1. Which version gates the fields?\n"

// The old supervisor restarted a crash-looping monitor 4,021 times. agentd
// parks it as soon as a crash repeats its cause, here dying before its
// screen was ever seen, and escalates once.
func TestCrashLoopStopsAfterNRestartsAndEscalates(t *testing.T) {
	h := newHarness(t, false, "mon")
	h.status("mon", working)
	h.tmux.crash["mon"] = true
	h.ticks(12*12, 5*time.Minute) // twelve hours

	if got := h.tmux.count("new-session", "mon"); got != 2 {
		t.Fatalf("restarts = %d, want 2", got)
	}
	if got := len(h.runner.calls); got != 1 {
		t.Fatalf("escalations = %d, want 1", got)
	}
	if !strings.Contains(h.runner.calls[0][4], "crashed 2 times") {
		t.Fatalf("escalation text: %q", h.runner.calls[0][4])
	}
	if h.state("mon") != CrashLooping {
		t.Fatalf("state = %s, want crash-looping", h.state("mon"))
	}
	if got := h.count(KindAction, ActEscalate, "mon"); got != 1 {
		t.Fatalf("escalate events = %d, want 1", got)
	}
}

// Restarts back off: the first is immediate, the next waits the initial
// backoff, the one after waits twice that. Each crash here has its own
// cause, so none parks the session early.
func TestRestartBackoffDoubles(t *testing.T) {
	h := newHarness(t, false, "mon")
	h.cfg.Policies.Restart.MaxCrashes = 10
	h.status("mon", working)
	n := 0
	h.tmux.deadScreen["mon"] = func() string { n++; return fmt.Sprintf("error number %d", n) }
	h.tmux.crash["mon"], h.tmux.remain["mon"] = true, true
	h.ticks(20, 30*time.Second) // ten minutes in half-minute steps

	var at []time.Duration
	start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	for _, e := range h.events() {
		if e.Kind == KindAction && detailString(e, "action") == ActRestart {
			at = append(at, e.Time.Sub(start))
		}
	}
	want := []time.Duration{0, time.Minute, 3 * time.Minute, 7 * time.Minute}
	if !reflect.DeepEqual(at, want) {
		t.Fatalf("restart times = %v, want %v", at, want)
	}
}

// actionTimes lists when events of a kind and action happened, from the start of the clock.
func actionTimes(h *harness, kind, action string) []time.Duration {
	var at []time.Duration
	start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	for _, e := range h.events() {
		if e.Kind == kind && detailString(e, "action") == action {
			at = append(at, e.Time.Sub(start))
		}
	}
	return at
}

// A restart that fails is a crash, its error the cause: attempts follow the
// backoff schedule, the same error twice parks the session, and one
// escalation is posted, instead of a silent retry every pass.
func TestFailedRestartsCountTowardBreaker(t *testing.T) {
	h := newHarness(t, false, "mon")
	h.status("mon", working)
	h.tmux.newErr["mon"] = errors.New("tmux: server not running")
	h.ticks(20, 30*time.Second)

	want := []time.Duration{0, time.Minute}
	if got := actionTimes(h, KindError, ActRestart); !reflect.DeepEqual(got, want) {
		t.Fatalf("failed restart attempts at %v, want %v", got, want)
	}
	if got := h.tmux.count("new-session", "mon"); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if h.state("mon") != CrashLooping {
		t.Fatalf("state = %s, want crash-looping", h.state("mon"))
	}
	if len(h.runner.calls) != 1 {
		t.Fatalf("escalations = %d, want 1", len(h.runner.calls))
	}
	h.restart(false)
	h.ticks(24, 5*time.Minute)
	if got := h.tmux.count("new-session", "mon"); got != 2 {
		t.Fatalf("attempts = %d after replay, want still 2", got)
	}
}

// An escalation command that keeps failing is tried on the backoff schedule
// and given up after MaxRestarts tries, with one skip saying so; it is not
// run every pass forever, nor again after a replay.
func TestFailingEscalationStops(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", blocked)
	h.alive("s1", "waiting")
	h.runner.failing = 10
	h.ticks(12*12, 5*time.Minute)
	if h.runner.attempts != 3 || len(h.runner.calls) != 0 {
		t.Fatalf("attempts = %d posted = %d, want 3 attempts and nothing posted", h.runner.attempts, len(h.runner.calls))
	}
	gaveUp := 0
	for _, e := range h.events() {
		if e.Kind == KindSkip && e.Session == "s1" && strings.Contains(detailString(e, "reason"), "failed 3 times") {
			gaveUp++
		}
	}
	if gaveUp != 1 {
		t.Fatalf("give-up skips = %d, want 1", gaveUp)
	}
	h.restart(false)
	h.ticks(12, 5*time.Minute)
	if h.runner.attempts != 3 {
		t.Fatalf("attempts = %d after replay, want still 3", h.runner.attempts)
	}
}

// A merge request that cannot be opened is retried on the backoff schedule,
// with no second push before a retry is due, and given up after MaxRestarts
// failures with one escalation.
func TestFailingOpenIsBounded(t *testing.T) {
	h := newHarness(t, false, "d")
	mr := h.dir + "/mr.md"
	writeFile(t, mr, "The MR description.\n")
	h.status("d", "# Finished\n\nDONE\n")
	h.deliver("d", Deliver{Repository: "widgets", Worktree: h.dir + "/d/wt", Target: "main", MRFile: mr})
	h.forge.fail = errors.New("glab: 502 Bad Gateway")
	h.ticks(20, 30*time.Second)

	want := []time.Duration{0, time.Minute, 3 * time.Minute}
	if got := actionTimes(h, KindAction, ActPush); !reflect.DeepEqual(got, want) {
		t.Fatalf("pushes at %v, want %v", got, want)
	}
	if got := h.count(KindError, ActOpen, "d"); got != 3 {
		t.Fatalf("failed opens = %d, want 3", got)
	}
	if len(h.runner.calls) != 1 || !strings.Contains(h.runner.calls[0][4], "failed 3 times") {
		t.Fatalf("escalations = %v, want one give-up", h.runner.calls)
	}
	h.restart(false)
	h.ticks(24, 5*time.Minute)
	if got := len(h.forge.pushes); got != 3 {
		t.Fatalf("pushes = %d after replay, want still 3", got)
	}
}

// A merge request that already exists for the branch, open or merged, is
// found before the push and recorded as the delivery, so a lost reply (or a
// lost log) never opens a second one.
func TestExistingMergeRequestIsFoundNotReopened(t *testing.T) {
	h := newHarness(t, false, "d")
	mr := h.dir + "/mr.md"
	writeFile(t, mr, "The MR description.\n")
	h.status("d", "# Finished\n\nDONE\n")
	h.deliver("d", Deliver{Repository: "widgets", Worktree: h.dir + "/d/wt", Target: "main", MRFile: mr})
	h.forge.recordThenFail = errors.New("glab: unreadable reply")
	h.ticks(1, 5*time.Minute)
	if len(h.forge.opens) != 1 || h.count(KindError, ActOpen, "d") != 1 {
		t.Fatalf("opens = %d, open errors = %d after the first pass", len(h.forge.opens), h.count(KindError, ActOpen, "d"))
	}
	h.ticks(3, 5*time.Minute)
	if len(h.forge.opens) != 1 || len(h.forge.pushes) != 1 {
		t.Fatalf("opens = %d pushes = %d, want one each: the request exists", len(h.forge.opens), len(h.forge.pushes))
	}
	if got := h.sup.Tracks()["d"].Delivered["d:"+h.dir+"/d/wt"]; got != "!1" {
		t.Fatalf("delivered = %q, want the found request", got)
	}
	h.restart(false)
	h.ticks(3, 5*time.Minute)
	if len(h.forge.opens) != 1 || len(h.forge.pushes) != 1 {
		t.Fatalf("opens = %d pushes = %d after replay", len(h.forge.opens), len(h.forge.pushes))
	}
}

// mark-delivered seeds a delivery made by other means. It refuses a worktree
// the session's deliver specs do not name, and what it writes stops the
// delivery after a replay.
func TestMarkDelivered(t *testing.T) {
	h := newHarness(t, false, "d")
	mr := h.dir + "/mr.md"
	writeFile(t, mr, "The MR description.\n")
	h.status("d", "# Finished\n\nDONE\n")
	h.deliver("d", Deliver{Repository: "widgets", Worktree: h.dir + "/d/wt", Target: "main", MRFile: mr})
	log := NewLogWriter(h.buf)
	if err := MarkDelivered(h.cfg, log, h.clock.Now(), "d", h.dir+"/other", "!7"); err == nil {
		t.Fatal("a worktree outside the deliver specs was accepted")
	}
	if err := MarkDelivered(h.cfg, log, h.clock.Now(), "nope", h.dir+"/d/wt", "!7"); err == nil {
		t.Fatal("an unconfigured session was accepted")
	}
	if err := MarkDelivered(h.cfg, log, h.clock.Now(), "d", h.dir+"/d/wt", "!7"); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	h.ticks(3, 5*time.Minute)
	if len(h.forge.pushes) != 0 || len(h.forge.opens) != 0 {
		t.Fatalf("pushes %v opens %d after mark-delivered", h.forge.pushes, len(h.forge.opens))
	}
	if got := h.sup.Tracks()["d"].Delivered["d:"+h.dir+"/d/wt"]; got != "!7" {
		t.Fatalf("delivered = %q, want !7", got)
	}
}

// The old supervisor nudged a session blocked on the owner every 40 minutes
// for 10 hours. A session with open Questions is never nudged; its question
// is escalated once.
func TestBlockedOnOwnerIsNotNudged(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", blocked)
	h.alive("s1", "waiting for the owner")
	h.ticks(10*12, 5*time.Minute)

	if got := h.tmux.count("type", "s1"); got != 0 {
		t.Fatalf("nudges = %d, want 0", got)
	}
	if h.state("s1") != BlockedOnOwner {
		t.Fatalf("state = %s", h.state("s1"))
	}
	if got := len(h.runner.calls); got != 1 {
		t.Fatalf("escalations = %d, want 1", got)
	}
	if !strings.Contains(h.runner.calls[0][4], "Which version gates the fields?") {
		t.Fatalf("escalation text: %q", h.runner.calls[0][4])
	}
	if h.runner.calls[0][1] != "widgets" {
		t.Fatalf("console = %q, want widgets", h.runner.calls[0][1])
	}
}

// An escalation is posted once per key: not every pass, not again after
// agentd restarts and replays its log, and a failed post is retried until it
// lands.
func TestEscalationIsPostedOnce(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", blocked)
	h.alive("s1", "waiting")
	h.runner.failing = 2
	h.ticks(6, 5*time.Minute)
	if got := len(h.runner.calls); got != 1 {
		t.Fatalf("posted = %d, want 1 after two failures", got)
	}
	if got := h.count(KindError, ActEscalate, "s1"); got != 2 {
		t.Fatalf("failed posts logged = %d, want 2", got)
	}
	h.restart(false)
	h.ticks(24, 5*time.Minute)
	if got := len(h.runner.calls); got != 1 {
		t.Fatalf("posted = %d after replay, want still 1", got)
	}

	// A changed question while they stay open is an update, not a new escalation.
	h.status("s1", "# Blocked\n\n## Questions\n2. Another thing?\n")
	h.ticks(2, 5*time.Minute)
	if got := len(h.runner.calls); got != 2 {
		t.Fatalf("posted = %d, want 2 for the changed question", got)
	}
	if got := h.count(KindAction, ActUpdate, "s1"); got != 1 {
		t.Fatalf("updates = %d, want 1", got)
	}
}

// An idle session that is not blocked is nudged, at most Max times per idle
// episode, Stall apart; then one escalation. The nudge's own echo on screen
// is not progress.
func TestIdleIsNudgedAtMostKTimes(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.alive("s1", "static screen")
	h.ticks(10*12, 5*time.Minute)

	if got := h.tmux.count("type", "s1"); got != 3 {
		t.Fatalf("nudges = %d, want 3", got)
	}
	var at []time.Time
	for _, e := range h.events() {
		if e.Kind == KindAction && detailString(e, "action") == ActNudge {
			at = append(at, e.Time)
		}
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < 40*time.Minute {
			t.Fatalf("nudges %d and %d only %s apart", i-1, i, gap)
		}
	}
	if len(h.runner.calls) != 1 || !strings.Contains(h.runner.calls[0][4], "3 nudges went unanswered") {
		t.Fatalf("escalations = %v", h.runner.calls)
	}
	if h.state("s1") != Idle {
		t.Fatalf("state = %s", h.state("s1"))
	}

	// Real progress starts a new episode with a fresh nudge allowance.
	h.tmux.panes["s1"] = "the session did something"
	h.ticks(1, 5*time.Minute)
	if h.state("s1") != Working {
		t.Fatalf("state = %s after progress, want working", h.state("s1"))
	}
	h.ticks(12, 5*time.Minute)
	if got := h.tmux.count("type", "s1"); got != 4 {
		t.Fatalf("nudges = %d, want a fourth in the new episode", got)
	}
}

// agentd never kills anything. The tmux surface it uses has no kill, every
// call names one exact session, and the real client targets exact names.
func TestNoBroadKill(t *testing.T) {
	h := newHarness(t, false, "mon", "idle", "blocked", "done")
	h.status("mon", working)
	h.tmux.crash["mon"] = true
	h.status("idle", working)
	h.alive("idle", "static")
	h.status("blocked", blocked)
	h.alive("blocked", "static")
	h.status("done", "# Finished\n\nDONE\n")
	h.alive("done", "bye")
	h.ticks(3*12, 5*time.Minute)

	for _, c := range h.tmux.calls {
		if strings.Contains(c.op, "kill") {
			t.Fatalf("tmux kill issued: %+v", c)
		}
		if _, ok := h.cfg.Sessions[c.name]; !ok {
			t.Fatalf("tmux call on a session not in the config: %+v", c)
		}
	}
	iface := reflect.TypeOf((*Tmux)(nil)).Elem()
	for i := 0; i < iface.NumMethod(); i++ {
		if strings.Contains(strings.ToLower(iface.Method(i).Name), "kill") {
			t.Fatalf("Tmux interface has %s", iface.Method(i).Name)
		}
	}
	if got, _ := target("w1"); got != "=w1:" {
		t.Fatalf("target = %q, want exact match =w1:", got)
	}
	for _, bad := range []string{"", "w1*", "w1:1", "job-*", "a b", "w1.0"} {
		if _, err := target(bad); err == nil {
			t.Fatalf("target(%q) accepted", bad)
		}
	}
}

// Shadow mode observes everything and acts on nothing: no tmux writes, no
// push or merge request, no escalation command. It fetches, which writes
// nothing the remote sees. It still logs what it would do, with the same
// crash parking and once-per-key rules.
func TestShadowNeverActs(t *testing.T) {
	h := newHarness(t, true, "mon", "idle", "blocked", "done", "trust")
	h.status("mon", working)
	h.tmux.crash["mon"] = true
	h.status("idle", working)
	h.alive("idle", "static")
	h.status("blocked", blocked)
	h.alive("blocked", "static")
	h.status("done", "# Finished\n\nDONE\n")
	h.alive("done", "bye")
	h.deliver("done", Deliver{Repository: "widgets", Worktree: h.dir, Target: "main", MRFile: h.dir + "/done/STATUS.md"})
	h.status("trust", working)
	h.alive("trust", "Do you trust this folder?")
	h.ticks(12*12, 5*time.Minute)
	h.restart(true)
	h.ticks(12, 5*time.Minute)

	if h.tmux.mutations != 0 {
		t.Fatalf("tmux mutations = %d", h.tmux.mutations)
	}
	if len(h.forge.pushes) != 0 || len(h.forge.opens) != 0 {
		t.Fatalf("forge written: pushes %v opens %v", h.forge.pushes, h.forge.opens)
	}
	if len(h.runner.calls) != 0 {
		t.Fatalf("escalation command ran: %v", h.runner.calls)
	}
	if got, fetches := h.count(KindAction, "", ""), h.count(KindAction, ActFetch, ""); got != fetches {
		t.Fatalf("action events = %d in shadow mode, only %d of them fetches", got, fetches)
	}
	wants := map[string]int{ActRestart: 2, ActNudge: 3, ActPush: 1, ActOpen: 1}
	for action, want := range wants {
		if got := h.count(KindWould, action, ""); got != want {
			t.Fatalf("would %s = %d, want %d", action, got, want)
		}
	}
	// Nothing answers the prompt in shadow mode; the keys would be sent once for the screen.
	if got := h.count(KindWould, ActTrust, ""); got != 1 {
		t.Fatalf("would trust = %d, want 1", got)
	}
	// crash loop, blocked question, exhausted nudges: one each
	if got := h.count(KindWould, ActEscalate, ""); got != 3 {
		t.Fatalf("would escalate = %d, want 3", got)
	}
	if h.state("mon") != CrashLooping || h.state("blocked") != BlockedOnOwner || h.state("done") != Done {
		t.Fatalf("states: mon %s blocked %s done %s", h.state("mon"), h.state("blocked"), h.state("done"))
	}
}

// A session showing the usage-limit message is left alone until it clears,
// then told to continue, once.
func TestUsageLimitWaitsThenResumes(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.alive("s1", "You've hit your limit · Limit reached, resets 3pm")
	h.ticks(2*12, 5*time.Minute)
	if h.state("s1") != UsageLimited {
		t.Fatalf("state = %s", h.state("s1"))
	}
	if got := h.tmux.count("type", "s1"); got != 0 {
		t.Fatalf("typed %d lines while limited", got)
	}
	h.tmux.panes["s1"] = "> "
	h.ticks(3, 5*time.Minute)
	if got := h.count(KindAction, ActResume, "s1"); got != 1 {
		t.Fatalf("resumes = %d, want 1", got)
	}
	if h.state("s1") != Working {
		t.Fatalf("state = %s after reset", h.state("s1"))
	}
}

// The folder-trust prompt is answered with its first option, Yes: Claude
// Code preselects it, and the second is "No, exit".
func TestTrustPromptIsAnswered(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.alive("s1", "Do you trust this folder?")
	h.ticks(3, 5*time.Minute)
	if got := h.tmux.count("keys", "s1"); got != 1 {
		t.Fatalf("key sends = %d, want 1", got)
	}
	if got := h.tmux.chosen["s1"]; got != 1 {
		t.Fatalf("trust menu option chosen = %d, want 1, Yes", got)
	}
	if h.state("s1") != Working {
		t.Fatalf("state = %s", h.state("s1"))
	}
}

// A DONE session's branch is pushed and opened once, as a draft when
// STATUS.md still asks questions; a repository that allows neither is
// refused and escalated once; a held session is not delivered.
func TestDoneDeliversOnce(t *testing.T) {
	h := newHarness(t, false, "d1", "d2", "d3", "d4")
	mr := h.dir + "/mr.md"
	h.status("d1", "# Finished\n\nDONE\n")
	h.alive("d1", "bye")
	h.deliver("d1", Deliver{Repository: "widgets", Worktree: h.dir + "/d1/wt", Target: "main", MRFile: mr})
	h.status("d2", "# Finished\n\n## Questions\n- Is the name right?\n\nDONE\n")
	h.alive("d2", "bye")
	h.deliver("d2", Deliver{Repository: "widgets", Worktree: h.dir + "/d2/wt", Target: "main", MRFile: mr})
	h.status("d3", "# Finished\n\nDONE\n")
	h.deliver("d3", Deliver{Repository: "readonly", Worktree: h.dir + "/d3/wt", Target: "main", MRFile: mr})
	h.status("d4", "# Finished\n\nDONE\n")
	h.deliver("d4", Deliver{Repository: "widgets", Worktree: h.dir + "/d4/wt", Target: "main", MRFile: mr})
	s := h.cfg.Sessions["d4"]
	s.Hold = "gate change first"
	h.cfg.Sessions["d4"] = s
	h.git.branches = map[string]string{h.dir + "/d2/wt": "feature-2"}

	h.ticks(2, 5*time.Minute)
	if got := len(h.forge.opens); got != 0 {
		t.Fatalf("opened %d before the MR text existed", got)
	}
	writeFile(t, mr, "The MR description.\n")
	h.ticks(6, 5*time.Minute)
	h.restart(false)
	h.ticks(6, 5*time.Minute)

	if got := h.forge.pushes; len(got) != 2 {
		t.Fatalf("pushes = %v, want 2", got)
	}
	if got := len(h.forge.opens); got != 2 {
		t.Fatalf("opens = %d, want 2", got)
	}
	titles := map[string]bool{}
	for _, o := range h.forge.opens {
		titles[o.Title] = true
		if o.Description != "The MR description.\n" || o.Target != "main" || !strings.HasPrefix(o.Source, "feature") {
			t.Fatalf("merge request: %+v", o)
		}
	}
	if !titles["Add the feature"] || !titles["Draft: Add the feature"] {
		t.Fatalf("titles = %v", titles)
	}
	if got := h.sup.Tracks()["d1"].Delivered; got["d1:"+h.dir+"/d1/wt"] != "!1" && got["d1:"+h.dir+"/d1/wt"] != "!2" {
		t.Fatalf("delivered = %v", got)
	}
	if got := h.sup.Tracks()["d3"].Delivered; len(got) != 0 {
		t.Fatalf("a refused delivery latched: %v", got)
	}
	notAllowed := 0
	for _, c := range h.runner.calls {
		if strings.Contains(c[4], "does not allow push") {
			notAllowed++
		}
	}
	if notAllowed != 1 {
		t.Fatalf("not-allowed escalations = %d, want 1", notAllowed)
	}
	if got := h.count(KindSkip, "", "d4"); got != 1 {
		t.Fatalf("hold skips = %d, want 1", got)
	}
}

// A repository that allows no writes is refused and escalated once, but the
// refusal does not latch: once the allow-list grants push and open, the next
// pass after a replay delivers exactly once.
func TestNotAllowedDoesNotLatch(t *testing.T) {
	h := newHarness(t, false, "d")
	mr := h.dir + "/mr.md"
	writeFile(t, mr, "The MR description.\n")
	h.status("d", "# Finished\n\nDONE\n")
	h.deliver("d", Deliver{Repository: "readonly", Worktree: h.dir + "/d/wt", Target: "main", MRFile: mr})
	h.ticks(2, 5*time.Minute)
	if len(h.runner.calls) != 1 || !strings.Contains(h.runner.calls[0][4], "does not allow push") {
		t.Fatalf("escalations = %v, want one refusal", h.runner.calls)
	}
	if len(h.forge.pushes) != 0 || len(h.sup.Tracks()["d"].Delivered) != 0 {
		t.Fatalf("pushes %v delivered %v while refused", h.forge.pushes, h.sup.Tracks()["d"].Delivered)
	}

	h.cfg.Repositories["readonly"] = Repository{Forge: "gitlab", Project: "example-org/readonly", Allow: []string{"push", "open"}, Gate: []string{"make", "check"}}
	h.git.remote = "git@gitlab.example.com:example-org/readonly.git"
	h.restart(false)
	h.ticks(3, 5*time.Minute)
	if len(h.forge.pushes) != 1 || len(h.forge.opens) != 1 {
		t.Fatalf("pushes %v opens %d after the allow-list changed, want one each", h.forge.pushes, len(h.forge.opens))
	}
	if got := h.sup.Tracks()["d"].Delivered["d:"+h.dir+"/d/wt"]; got != "!1" {
		t.Fatalf("delivered = %q", got)
	}
}

// A replayed log restores counters: nudges already sent are not sent again.
func TestReplayRestoresCounters(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.alive("s1", "static")
	h.ticks(19, 5*time.Minute) // first nudge at 40m, second at 80m
	if got := h.tmux.count("type", "s1"); got != 2 {
		t.Fatalf("nudges before restart = %d, want 2", got)
	}
	h.restart(false)
	h.ticks(5*12, 5*time.Minute)
	if got := h.tmux.count("type", "s1"); got != 3 {
		t.Fatalf("nudges after restart = %d, want 3 in all", got)
	}
}

// A session agentd has restarted comes back as starting, then working.
func TestRestartedSessionComesBack(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.ticks(1, 5*time.Minute)
	if h.state("s1") != Starting || h.tmux.count("new-session", "s1") != 1 {
		t.Fatalf("state = %s, restarts = %d", h.state("s1"), h.tmux.count("new-session", "s1"))
	}
	h.tmux.panes["s1"] = "thinking"
	h.ticks(1, 5*time.Minute)
	if h.state("s1") != Working {
		t.Fatalf("state = %s", h.state("s1"))
	}
	for _, c := range h.tmux.calls {
		if c.op == "new-session" && (strings.Contains(c.args[1], "--continue") || !strings.Contains(c.args[1], "BRIEF.md")) {
			t.Fatalf("restart command = %q", c.args[1])
		}
	}
}

// A session whose transcript moved recently is working even when its screen
// is static, as during a long tool call. Once the transcript is stale too,
// a tool call that never returned is waiting on the owner, not idle.
func TestFreshTranscriptIsNotIdle(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.cfg.TranscriptDir = h.dir + "/transcripts"
	h.status("s1", working)
	h.alive("s1", "static")
	dir := TranscriptDir(h.cfg.TranscriptDir, h.cfg.Sessions["s1"].Cwd)
	line := `{"type":"assistant","timestamp":"` + h.clock.now.Add(55*time.Minute).Format(time.RFC3339Nano) + `","message":{"stop_reason":"tool_use"}}` + "\n"
	writeFile(t, dir+"/abc.jsonl", `{"type":"progress"}`+"\n"+line+`{"type":"bridge-session"}`+"\n")
	h.ticks(13, 5*time.Minute) // 65 minutes: the screen is 60 minutes old, the transcript 10
	if h.state("s1") != Working || h.tmux.count("type", "s1") != 0 {
		t.Fatalf("state = %s, nudges = %d", h.state("s1"), h.tmux.count("type", "s1"))
	}
	h.ticks(12, 5*time.Minute)
	if h.state("s1") != BlockedOnOwner || h.tmux.count("type", "s1") != 0 {
		t.Fatalf("state = %s, nudges = %d once the transcript is stale too", h.state("s1"), h.tmux.count("type", "s1"))
	}
}

// A worktree on the deliver target itself, or on a detached HEAD, is never
// pushed: pushing it would write the target branch directly, or push nothing
// nameable. One skip says why, and nothing latches, so the session can move
// to a branch and be delivered.
func TestTargetOrDetachedBranchIsNotPushed(t *testing.T) {
	h := newHarness(t, false, "onmain", "detached", "empty")
	mr := h.dir + "/mr.md"
	writeFile(t, mr, "The MR description.\n")
	for _, n := range []string{"onmain", "detached", "empty"} {
		h.status(n, "# Finished\n\nDONE\n")
		h.deliver(n, Deliver{Repository: "widgets", Worktree: h.dir + "/" + n + "/wt", Target: "main", MRFile: mr})
	}
	h.git.branches = map[string]string{h.dir + "/onmain/wt": "main", h.dir + "/detached/wt": "HEAD", h.dir + "/empty/wt": ""}
	h.ticks(6, 5*time.Minute)
	if len(h.forge.pushes) != 0 || len(h.forge.opens) != 0 {
		t.Fatalf("pushes %v opens %d from the target or a detached HEAD", h.forge.pushes, len(h.forge.opens))
	}
	for _, n := range []string{"onmain", "detached", "empty"} {
		skips := 0
		for _, e := range h.events() {
			if e.Kind == KindSkip && e.Session == n && strings.Contains(detailString(e, "reason"), "not delivering") {
				skips++
			}
		}
		if skips != 1 {
			t.Fatalf("%s: skips = %d, want 1", n, skips)
		}
		if got := h.sup.Tracks()[n].Delivered; len(got) != 0 {
			t.Fatalf("%s: delivery latched: %v", n, got)
		}
	}
	h.git.branches[h.dir+"/onmain/wt"] = "feature"
	h.ticks(1, 5*time.Minute)
	if len(h.forge.pushes) != 1 || h.forge.pushes[0] != "feature" {
		t.Fatalf("pushes %v after moving to a branch", h.forge.pushes)
	}
}

// A branch name is reused: its old request was merged, and the session has
// since committed more on it. The merged request is the delivery only when
// it carried the worktree's current HEAD; new commits are delivered.
func TestReusedBranchDeliversNewCommits(t *testing.T) {
	h := newHarness(t, false, "same", "newer")
	mr := h.dir + "/mr.md"
	writeFile(t, mr, "The MR description.\n")
	for _, n := range []string{"same", "newer"} {
		h.status(n, "# Finished\n\nDONE\n")
		h.deliver(n, Deliver{Repository: "widgets", Worktree: h.dir + "/" + n + "/wt", Target: "main", MRFile: mr})
	}
	h.git.branches = map[string]string{h.dir + "/same/wt": "feature-same", h.dir + "/newer/wt": "feature-newer"}
	h.forge.merged = []mergedRequest{
		{ref: "!40", source: "feature-same", target: "main", head: "c0ffee"},
		{ref: "!41", source: "feature-newer", target: "main", head: "0ld"},
	}
	h.ticks(3, 5*time.Minute)
	if got := h.sup.Tracks()["same"].Delivered["same:"+h.dir+"/same/wt"]; got != "!40" {
		t.Fatalf("same: delivered = %q, want the merged request that carried HEAD", got)
	}
	if len(h.forge.pushes) != 1 || h.forge.pushes[0] != "feature-newer" || len(h.forge.opens) != 1 {
		t.Fatalf("pushes %v opens %d, want the new commits on feature-newer delivered once", h.forge.pushes, len(h.forge.opens))
	}
}

// Text that matches a confidential pattern is never posted: the escalation
// names the session and says the details were withheld, and the log records
// that it was filtered without the text itself.
func TestConfidentialEscalationIsWithheld(t *testing.T) {
	h := newHarness(t, false, "s1", "s2")
	h.cfg.ConfidentialPatterns = []string{`(?i)project\s+nightingale`}
	h.restart(false)
	h.status("s1", "# Blocked\n\n## Questions\n1. May the Project  Nightingale launch date go in the release notes?\n")
	h.alive("s1", "waiting")
	h.status("s2", blocked)
	h.alive("s2", "waiting")
	h.ticks(6, 5*time.Minute)

	if len(h.runner.calls) != 2 {
		t.Fatalf("escalations = %v, want one per session", h.runner.calls)
	}
	for _, c := range h.runner.calls {
		msg := c[4]
		switch {
		case strings.HasPrefix(msg, "s1"):
			if strings.Contains(strings.ToLower(msg), "nightingale") || !strings.Contains(msg, "withheld") {
				t.Fatalf("s1 escalation leaked or did not say it was withheld: %q", msg)
			}
		case !strings.Contains(msg, "Which version gates the fields?"):
			t.Fatalf("s2 escalation was filtered: %q", msg)
		}
	}
	if strings.Contains(strings.ToLower(h.buf.String()), "nightingale") {
		t.Fatal("the event log carries the confidential text")
	}
	filtered := 0
	for _, e := range h.events() {
		if e.Kind == KindSkip && e.Session == "s1" && detailString(e, "filtered") != "" {
			filtered++
		}
	}
	if filtered != 1 {
		t.Fatalf("filtered events = %d, want 1", filtered)
	}
}

// The forge rebased the request before merging, so its head is not the
// worktree's HEAD; the target contains it and HEAD has nothing new, so the
// merged request is the delivery. A session that rebased onto a squash
// merge has nothing ahead either, and nothing is delivered again. The
// delivery records the head it covered.
func TestRebasedMergesAreNotDeliveredAgain(t *testing.T) {
	h := newHarness(t, false, "forge", "squash", "more")
	mr := h.dir + "/mr.md"
	writeFile(t, mr, "The MR description.\n")
	for _, n := range []string{"forge", "squash", "more"} {
		h.status(n, "# Finished\n\nDONE\n")
		h.deliver(n, Deliver{Repository: "widgets", Worktree: h.dir + "/" + n + "/wt", Target: "main", MRFile: mr})
	}
	h.git.branches = map[string]string{h.dir + "/forge/wt": "f-forge", h.dir + "/squash/wt": "f-squash", h.dir + "/more/wt": "f-more"}
	h.git.aheads = map[string]int{h.dir + "/forge/wt": 0, h.dir + "/squash/wt": 0, h.dir + "/more/wt": 1}
	h.git.inTarget = map[string]bool{"rebased": true, "rebased-more": true}
	h.forge.merged = []mergedRequest{
		{ref: "!50", source: "f-forge", target: "main", head: "rebased"},
		{ref: "!51", source: "f-squash", target: "main", head: "pre-squash"},
		{ref: "!52", source: "f-more", target: "main", head: "rebased-more"},
	}
	h.ticks(3, 5*time.Minute)
	tr := func(n string) string { return h.sup.Tracks()[n].Delivered[n+":"+h.dir+"/"+n+"/wt"] }
	if tr("forge") != "!50" || tr("squash") != "nothing" {
		t.Fatalf("delivered: forge %q squash %q", tr("forge"), tr("squash"))
	}
	if len(h.forge.pushes) != 1 || h.forge.pushes[0] != "f-more" {
		t.Fatalf("pushes = %v, want only the branch with new work", h.forge.pushes)
	}
	for _, e := range h.events() {
		if detailString(e, "delivered") != "" && (e.Kind == KindSkip || detailString(e, "action") == ActOpen) && detailString(e, "head") != "c0ffee" {
			t.Fatalf("delivery event without the head it covered: %+v", e)
		}
	}
}

// The confidential patterns cover the STATUS.md first line the log keeps,
// and the merge-request title and text sent to the forge: a match is not
// delivered, is skipped once and escalated without the text, and is
// delivered once the text no longer matches.
func TestConfidentialMergeRequestIsNotDelivered(t *testing.T) {
	h := newHarness(t, false, "d", "s")
	h.cfg.ConfidentialPatterns = []string{`Nightingale`}
	h.restart(false)
	mr := h.dir + "/mr.md"
	writeFile(t, mr, "Ships the Nightingale launch page.\n")
	h.status("d", "# Nightingale page\n\nDONE\n")
	h.deliver("d", Deliver{Repository: "widgets", Worktree: h.dir + "/d/wt", Target: "main", MRFile: mr})
	h.status("s", working)
	h.alive("s", "busy")
	h.git.subject = "Add the Nightingale page"
	h.git.branches = map[string]string{h.dir + "/s/wt": "other"}
	h.ticks(4, 5*time.Minute)

	if len(h.forge.pushes) != 0 || len(h.forge.opens) != 0 {
		t.Fatalf("pushes %v opens %d of confidential text", h.forge.pushes, len(h.forge.opens))
	}
	if strings.Contains(h.buf.String(), "Nightingale") {
		t.Fatalf("the event log carries the confidential text:\n%s", h.buf.String())
	}
	if got := h.count(KindSkip, "", "d"); got != 1 {
		t.Fatalf("skips = %d, want 1", got)
	}
	if len(h.runner.calls) != 1 || strings.Contains(h.runner.calls[0][4], "Nightingale") || !strings.Contains(h.runner.calls[0][4], "confidential") {
		t.Fatalf("escalations = %v, want one naming the filter, not the text", h.runner.calls)
	}
	writeFile(t, mr, "Ships the launch page.\n")
	h.git.subject = "Add the launch page"
	h.ticks(1, 5*time.Minute)
	if len(h.forge.opens) != 1 {
		t.Fatalf("opens = %d once the text was cleaned, want 1", len(h.forge.opens))
	}
}
