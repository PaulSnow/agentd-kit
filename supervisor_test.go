package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

const working = "# Working on the feature\nHalf done.\n"
const blocked = "# Blocked\nWaiting.\n\n## Questions\n1. Which version gates the fields?\n"

// The old supervisor restarted a crash-looping monitor 4,021 times. agentd
// restarts it MaxRestarts times within the window, then stops and escalates.
func TestCrashLoopStopsAfterNRestartsAndEscalates(t *testing.T) {
	h := newHarness(t, false, "mon")
	h.status("mon", working)
	h.tmux.crash["mon"] = true
	h.ticks(12*12, 5*time.Minute) // twelve hours

	if got := h.tmux.count("new-session", "mon"); got != 3 {
		t.Fatalf("restarts = %d, want 3", got)
	}
	if got := len(h.runner.calls); got != 1 {
		t.Fatalf("escalations = %d, want 1", got)
	}
	if !strings.Contains(h.runner.calls[0][4], "after 3 restarts") {
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
// backoff, the one after waits twice that.
func TestRestartBackoffDoubles(t *testing.T) {
	h := newHarness(t, false, "mon")
	h.status("mon", working)
	h.tmux.crash["mon"] = true
	h.ticks(20, 30*time.Second) // ten minutes in half-minute steps

	var at []time.Duration
	start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	for _, e := range h.events() {
		if e.Kind == KindAction && detailString(e, "action") == ActRestart {
			at = append(at, e.Time.Sub(start))
		}
	}
	want := []time.Duration{0, time.Minute, 3 * time.Minute}
	if !reflect.DeepEqual(at, want) {
		t.Fatalf("restart times = %v, want %v", at, want)
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

	// A different question is a different key.
	h.status("s1", "# Blocked\n\n## Questions\n2. Another thing?\n")
	h.ticks(2, 5*time.Minute)
	if got := len(h.runner.calls); got != 2 {
		t.Fatalf("posted = %d, want 2 for the new question", got)
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
	if got, _ := target("s4"); got != "=s4:" {
		t.Fatalf("target = %q, want exact match =s4:", got)
	}
	for _, bad := range []string{"", "s4*", "s4:1", "fix-*", "a b", "s4.0"} {
		if _, err := target(bad); err == nil {
			t.Fatalf("target(%q) accepted", bad)
		}
	}
}

// Shadow mode observes everything and acts on nothing: no tmux writes, no
// fetch, push or merge request, no escalation command. It still logs what it
// would do, with the same breaker and once-per-key rules.
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
	if h.git.fetches != 0 || len(h.forge.pushes) != 0 || len(h.forge.opens) != 0 {
		t.Fatalf("forge touched: fetches %d pushes %v opens %v", h.git.fetches, h.forge.pushes, h.forge.opens)
	}
	if len(h.runner.calls) != 0 {
		t.Fatalf("escalation command ran: %v", h.runner.calls)
	}
	if got := h.count(KindAction, "", ""); got != 0 {
		t.Fatalf("action events = %d in shadow mode", got)
	}
	wants := map[string]int{ActRestart: 3, ActNudge: 3, ActFetch: 1, ActPush: 1, ActOpen: 1}
	for action, want := range wants {
		if got := h.count(KindWould, action, ""); got != want {
			t.Fatalf("would %s = %d, want %d", action, got, want)
		}
	}
	// Nothing answers the prompt in shadow mode, so it would be answered every pass.
	if got := h.count(KindWould, ActTrust, ""); got < 100 {
		t.Fatalf("would trust = %d, want one per pass", got)
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

// The folder-trust prompt is answered.
func TestTrustPromptIsAnswered(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.status("s1", working)
	h.alive("s1", "Do you trust this folder?")
	h.ticks(3, 5*time.Minute)
	if got := h.tmux.count("keys", "s1"); got != 1 {
		t.Fatalf("key sends = %d, want 1", got)
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
		if o.Description != "The MR description.\n" || o.Target != "main" || o.Source != "feature" {
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

	h.cfg.Repositories["readonly"] = Repository{Forge: "gitlab", Project: "example-org/readonly", Allow: []string{"push", "open"}}
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
		if c.op == "new-session" && !strings.Contains(c.args[1], "claude --dangerously-skip-permissions --continue") {
			t.Fatalf("restart command = %q", c.args[1])
		}
	}
}

// A session whose transcript moved recently is working even when its screen
// is static, as during a long tool call.
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
	if h.state("s1") != Idle {
		t.Fatalf("state = %s once the transcript is stale too", h.state("s1"))
	}
}
