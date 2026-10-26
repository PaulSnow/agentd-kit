package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// feedHarness is a harness with the consoles' feed on.
func feedHarness(t *testing.T, shadow bool, names ...string) *harness {
	h := newHarness(t, shadow, names...)
	h.cfg.Policies.Feed.Enabled = true
	return h
}

// feedPosts are the escalation command's calls that are feed lines, by
// their text, to the console each went to.
func feedPosts(h *harness) [][2]string {
	var posted []string
	for _, e := range h.events() {
		if e.Kind == KindFeed && e.Feed.Phase == FeedIntent {
			posted = append(posted, e.Feed.Text)
		}
	}
	var out [][2]string
	for _, c := range h.runner.calls {
		for _, p := range posted {
			if c[4] == p {
				out = append(out, [2]string{c[1], c[4]})
				break
			}
		}
	}
	return out
}

// newItem logs a new item of a repository, as a poll does.
func newItem(t *testing.T, h *harness, id, repository, title string) {
	t.Helper()
	it := ItemPayload{ID: id, Kind: "issue", Repository: repository, Title: title, SourceTime: h.clock.Now()}
	if err := NewLogWriter(h.buf).Append(Event{V: EventVersion, Time: h.clock.Now(), Kind: KindItem, Mode: ModeShared, Item: &it}); err != nil {
		t.Fatal(err)
	}
}

// The first pass with the feed on records what is there as its baseline
// and posts none of it; a delivery after that is posted once to its
// repository's console, in the text the page shows, and not again after
// agentd restarts.
func TestFeedPostsOnceAcrossRestarts(t *testing.T) {
	h := newHarness(t, false, "d", "e")
	doneWithDelivery(t, h, "d")
	h.alive("d", "waiting")
	h.ticks(2, 5*time.Minute)
	if len(h.forge.pushes) != 1 {
		t.Fatalf("setup: pushes %v", h.forge.pushes)
	}
	// The feed begins after d's delivery: its baseline holds it.
	h.cfg.Policies.Feed.Enabled = true
	h.ticks(2, 5*time.Minute)
	if got := feedPosts(h); len(got) != 0 {
		t.Fatalf("history was posted: %v", got)
	}
	doneWithDelivery(t, h, "e")
	h.alive("e", "waiting")
	h.ticks(3, 5*time.Minute)
	got := feedPosts(h)
	if len(got) != 1 || got[0][0] != "widgets" || !strings.HasPrefix(got[0][1], "e delivered") {
		t.Fatalf("feed posts = %v, want e's delivery once to widgets", got)
	}
	h.restart(false)
	h.ticks(4, 5*time.Minute)
	if got := feedPosts(h); len(got) != 1 {
		t.Fatalf("feed posts after a restart = %v, want still 1", got)
	}
}

// A decision that does not block is fed once, with the exact reply to
// type; a blocking one is in the session's escalation to the same
// console, and is not fed again.
func TestFeedDecisionsCarryTheReply(t *testing.T) {
	h := feedHarness(t, false, "s1", "s2")
	h.alive("s1", "waiting")
	h.alive("s2", "waiting")
	h.ticks(1, 5*time.Minute)
	writeQuestions(t, h, "s1", strings.Replace(chooseColour, `"blocking":true`, `"blocking":false`, 1))
	writeQuestions(t, h, "s2", chooseColour)
	h.ticks(3, 5*time.Minute)
	got := feedPosts(h)
	if len(got) != 1 || got[0][0] != "widgets" || !strings.HasPrefix(got[0][1], "s1 asks Q1") || !strings.HasSuffix(got[0][1], "Reply: s1 Q1@"+decisionHash(h.sup.Tracks()["s1"].Decisions[0])+" <answer>") {
		t.Fatalf("feed posts = %v, want s1's decision once with its reply", got)
	}
	if n := h.count(KindAction, ActEscalate, "s2"); n != 1 || len(h.runner.calls) != 2 {
		t.Fatalf("s2 escalations %d, posts %v: want one escalation and one feed line", n, h.runner.calls)
	}
	// A fed decision is answered with the version its line names.
	st := NewLogState(h.cfg, ModeLive).Fold(h.events())
	if err := ConsoleReply(h.cfg, NewLogWriter(h.buf), st, h.clock.Now(), "widgets", "s1 Q1 b"); err == nil {
		t.Fatal("a reply to a fed decision without its version was applied")
	}
	if err := ConsoleReply(h.cfg, NewLogWriter(h.buf), st, h.clock.Now(), "widgets", "s1 Q1@"+decisionHash(st.Tracks["s1"].Decisions[0])+" b"); err != nil {
		t.Fatal(err)
	}
}

// A console reply is applied as the owner's answer, through the same path
// as the page's POST, and wakes the running loop: the answer reaches the
// session long before the next poll. A reply from another repository's
// console is refused, and so is an answer the decision cannot take.
func TestConsoleReplyWakesTheLoopAndAnswers(t *testing.T) {
	h := feedHarness(t, false, "s1")
	h.cfg.PollInterval = Duration{time.Hour}
	h.alive("s1", "waiting")
	writeQuestions(t, h, "s1", chooseColour)
	buf := &lockedBuffer{}
	h.sup = NewSupervisor(h.cfg, NewLogWriter(buf), h.clock, h.tmux, h.git, h.forge, h.runner, false)
	h.sup.Pass()
	h.sup.Wait()
	if h.state("s1") != BlockedOnOwner {
		t.Fatalf("setup: state %s", h.state("s1"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.sup.Run(ctx, false) }()
	waitFor(t, "the loop to wait for the poll and the wake check", func() bool { return h.clock.waiting() == 2 })

	fold := func() *LogState {
		events, bad, err := ReadEvents(bytes.NewReader(buf.Bytes()))
		if err != nil || bad != 0 {
			t.Fatalf("log unreadable: %v, %d bad", err, bad)
		}
		return NewLogState(h.cfg, ModeLive).Fold(events)
	}
	if err := ConsoleReply(h.cfg, NewLogWriter(buf), fold(), h.clock.Now(), "elsewhere", "s1 Q1 b"); err == nil {
		t.Fatal("a reply from another console was applied")
	}
	if err := ConsoleReply(h.cfg, NewLogWriter(buf), fold(), h.clock.Now(), "widgets", "s1 Q1@0123abcd b"); err == nil {
		t.Fatal("a reply to the question as it was before was applied")
	}
	if err := ConsoleReply(h.cfg, NewLogWriter(buf), fold(), h.clock.Now(), "widgets", "s1 Q1 c"); err == nil {
		t.Fatal("an answer that is no option was applied")
	}
	asked := decisionHash(fold().Tracks["s1"].Decisions[0])
	if err := ConsoleReply(h.cfg, NewLogWriter(buf), fold(), h.clock.Now(), "widgets", "s1 Q1@"+asked+" b"); err != nil {
		t.Fatal(err)
	}
	start := h.clock.Now()
	waitFor(t, "the answer to reach the session", func() bool {
		if doc, ok := readAnswers(t, h, "s1"); ok && len(doc.Answers) == 1 {
			return true
		}
		// The clock moves only while the loop waits on it, so a slow
		// machine's pass does not count as time the reply waited.
		if h.clock.waiting() == 2 {
			h.clock.advance(time.Second)
		}
		return false
	})
	if waited := h.clock.Now().Sub(start); waited > 2*WakeCheck {
		t.Fatalf("the answer came %s after the reply, want within %s", waited, 2*WakeCheck)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	doc, _ := readAnswers(t, h, "s1")
	if a := doc.Answers[0]; a.ID != "Q1" || a.Choice != "b" {
		t.Fatalf("answers.json = %+v", doc)
	}
	var from string
	for _, e := range fold().Tracks["s1"].Answers {
		from = e.From
	}
	if from != "console:widgets" {
		t.Fatalf("answer from %q, want console:widgets", from)
	}
}

// "seen ID..." marks a repository's items seen from its console, and only
// from its console.
func TestConsoleReplyMarksItemsSeen(t *testing.T) {
	h := feedHarness(t, false)
	at := h.clock.Now()
	log := NewLogWriter(h.buf)
	for _, it := range []ItemPayload{
		{ID: "gitlab/example-org/widgets#4", Kind: "issue", Repository: "gitlab/example-org/widgets", Title: "Widgets wobble", SourceTime: at},
		{ID: "gitlab/example-org/readonly#2", Kind: "issue", Repository: "gitlab/example-org/readonly", Title: "Read only", SourceTime: at},
	} {
		it := it
		if err := log.Append(Event{V: EventVersion, Time: at, Kind: KindItem, Mode: ModeShared, Item: &it}); err != nil {
			t.Fatal(err)
		}
	}
	st := NewLogState(h.cfg, ModeLive).Fold(h.events())
	if err := ConsoleReply(h.cfg, log, st, at, "widgets", "seen gitlab/example-org/readonly#2"); err == nil {
		t.Fatal("another repository's item was marked from this console")
	}
	if err := ConsoleReply(h.cfg, log, st, at, "widgets", "seen gitlab/example-org/widgets#4"); err != nil {
		t.Fatal(err)
	}
	st = NewLogState(h.cfg, ModeLive).Fold(h.events())
	if !st.Items["gitlab/example-org/widgets#4"].Seen || st.Items["gitlab/example-org/readonly#2"].Seen {
		t.Fatalf("seen: %+v, %+v", st.Items["gitlab/example-org/widgets#4"], st.Items["gitlab/example-org/readonly#2"])
	}
}

// A line that matches a confidential pattern is never posted, nor logged
// by the feed: a line naming only the repository goes in its place, and
// the feed event records the pattern's number.
func TestFeedNeverPostsAConfidentialTerm(t *testing.T) {
	h := feedHarness(t, false)
	h.cfg.ConfidentialPatterns = []string{`(?i)project\s+nightingale`}
	h.restart(false)
	h.ticks(1, 5*time.Minute)
	newItem(t, h, "gitlab/example-org/widgets#5", "gitlab/example-org/widgets", "Ship Project Nightingale first")
	h.ticks(2, 5*time.Minute)
	got := feedPosts(h)
	if len(got) != 1 || !strings.Contains(got[0][1], "withheld") || !strings.Contains(got[0][1], "widgets") {
		t.Fatalf("feed posts = %v, want one withheld line", got)
	}
	for _, c := range h.runner.calls {
		if strings.Contains(strings.ToLower(strings.Join(c, " ")), "nightingale") {
			t.Fatalf("posted a confidential term: %q", c)
		}
	}
	withheld := 0
	for _, e := range h.events() {
		if e.Kind != KindFeed {
			continue
		}
		if strings.Contains(strings.ToLower(e.Feed.Text), "nightingale") {
			t.Fatalf("the feed logged a confidential term: %+v", e.Feed)
		}
		if e.Feed.Withheld == 1 {
			withheld++
		}
	}
	if withheld != 1 {
		t.Fatalf("withheld records = %d, want 1", withheld)
	}
}

// A post that fails is retried on the restart policy's backoff and given
// up after max_restarts tries; shadow mode posts nothing and logs what it
// would post, once, across a restart.
func TestFeedRetriesThenGivesUpAndShadowWould(t *testing.T) {
	h := feedHarness(t, false, "e")
	h.ticks(1, 5*time.Minute)
	h.runner.failing = 100
	doneWithDelivery(t, h, "e")
	h.alive("e", "waiting")
	h.ticks(40, 30*time.Minute)
	phases := map[string]int{}
	for _, e := range h.events() {
		if e.Kind == KindFeed {
			phases[e.Feed.Phase]++
		}
	}
	max := h.cfg.Policies.Restart.MaxRestarts
	if phases[FeedFailed] != max || phases[FeedGaveUp] != 1 || phases[FeedIntent] != max {
		t.Fatalf("phases = %v, want %d failures then one give-up", phases, max)
	}

	s := feedHarness(t, true)
	s.ticks(1, 5*time.Minute)
	newItem(t, s, "gitlab/example-org/widgets#4", "gitlab/example-org/widgets", "Widgets wobble")
	s.ticks(3, 5*time.Minute)
	s.restart(true)
	s.ticks(3, 5*time.Minute)
	would := 0
	for _, e := range s.events() {
		if e.Kind == KindFeed && e.Feed.Phase == FeedWould && strings.HasPrefix(e.Feed.Text, "New issue in widgets: Widgets wobble") {
			would++
		}
	}
	if would != 1 || len(s.runner.calls) != 0 {
		t.Fatalf("shadow: would %d, posts %v; want one would and no post", would, s.runner.calls)
	}
}

// What is there when a repository's feed begins is its baseline, posted
// never: when the feed is turned on, when a repository is configured
// later, and when the feed is turned on again after it was off. A line
// whose post a crash cut short is not posted again after the restart.
func TestFeedBaselinesEachBeginning(t *testing.T) {
	h := newHarness(t, false, "s1")
	h.alive("s1", "waiting")
	writeQuestions(t, h, "s1", strings.Replace(chooseColour, `"blocking":true`, `"blocking":false`, 1))
	newItem(t, h, "gitlab/example-org/widgets#4", "gitlab/example-org/widgets", "Widgets wobble")
	newItem(t, h, "gitlab/example-org/gadgets#1", "gitlab/example-org/gadgets", "Gadgets jam")
	h.ticks(2, 5*time.Minute)
	h.cfg.Policies.Feed.Enabled = true
	h.ticks(2, 5*time.Minute)
	if got := feedPosts(h); len(got) != 0 {
		t.Fatalf("history was posted: %v", got)
	}
	// A repository configured later begins with its own baseline.
	h.cfg.Repositories["gadgets"] = Repository{Forge: "gitlab", Host: "gitlab.example.com", Project: "example-org/gadgets"}
	h.ticks(2, 5*time.Minute)
	// A repository taken out of the configuration and put back begins
	// again with a baseline too.
	gadgets := h.cfg.Repositories["gadgets"]
	delete(h.cfg.Repositories, "gadgets")
	h.ticks(1, 5*time.Minute)
	newItem(t, h, "gitlab/example-org/gadgets#3", "gitlab/example-org/gadgets", "Gadgets drip")
	h.ticks(1, 5*time.Minute)
	h.cfg.Repositories["gadgets"] = gadgets
	h.ticks(2, 5*time.Minute)
	// Off, then on again: what came meanwhile is history too.
	h.cfg.Policies.Feed.Enabled = false
	h.ticks(2, 5*time.Minute)
	newItem(t, h, "gitlab/example-org/widgets#5", "gitlab/example-org/widgets", "Widgets squeak")
	h.ticks(1, 5*time.Minute)
	h.cfg.Policies.Feed.Enabled = true
	h.ticks(2, 5*time.Minute)
	if got := feedPosts(h); len(got) != 0 {
		t.Fatalf("history was posted: %v", got)
	}
	paused := 0
	for _, e := range h.events() {
		if e.Kind == KindFeed && e.Feed.Phase == FeedPaused {
			paused++
		}
	}
	if paused != 2 {
		t.Fatalf("paused events = %d, want 2", paused)
	}
	// An intent with no outcome: agentd crashed mid-post.
	newItem(t, h, "gitlab/example-org/widgets#6", "gitlab/example-org/widgets", "Widgets rattle")
	if err := NewLogWriter(h.buf).Append(Event{V: EventVersion, Time: h.clock.Now(), Kind: KindFeed, Mode: ModeLive,
		Feed: &FeedPayload{Phase: FeedIntent, Key: "item:gitlab/example-org/widgets#6", Repository: "widgets", Console: "widgets", Text: "New issue in widgets: Widgets rattle"}}); err != nil {
		t.Fatal(err)
	}
	h.restart(false)
	newItem(t, h, "gitlab/example-org/gadgets#2", "gitlab/example-org/gadgets", "Gadgets hum")
	h.ticks(3, 5*time.Minute)
	if len(h.runner.calls) != 1 || h.runner.calls[0][1] != "gadgets" || !strings.HasPrefix(h.runner.calls[0][4], "New issue in gadgets: Gadgets hum") {
		t.Fatalf("posts = %v, want only the new gadgets item", h.runner.calls)
	}
}

// A pass posts at most FeedPerPass lines; the rest follow in later passes,
// each once, and none is longer than FeedTextLimit.
func TestFeedPostsABurstOverPasses(t *testing.T) {
	h := feedHarness(t, false)
	h.ticks(1, 5*time.Minute)
	for i := 0; i < FeedPerPass+5; i++ {
		newItem(t, h, fmt.Sprintf("gitlab/example-org/widgets#%d", 100+i), "gitlab/example-org/widgets", strings.Repeat("wobble ", 400))
	}
	h.ticks(1, 5*time.Minute)
	if n := len(h.runner.calls); n != FeedPerPass {
		t.Fatalf("posts in one pass = %d, want %d", n, FeedPerPass)
	}
	h.ticks(3, 5*time.Minute)
	if n := len(h.runner.calls); n != FeedPerPass+5 {
		t.Fatalf("posts = %d, want %d", n, FeedPerPass+5)
	}
	for _, c := range h.runner.calls {
		if len(c[4]) > FeedTextLimit || !strings.Contains(c[4], "…") || !strings.Contains(c[4], "… (reply: seen gitlab/example-org/widgets#") {
			t.Fatalf("a line of %d bytes, cut or its reply lost, was posted: %q", len(c[4]), c[4][len(c[4])-60:])
		}
	}
}

// A merge agentd made is one line, not two with its request's CI record
// saying merged as well; a merge cut short is not said to be made.
func TestFeedMergeLines(t *testing.T) {
	h := feedHarness(t, false, "d")
	key := ciKey("widgets", "!1")
	h.sup.track("d").Merges = map[string]string{key: "c0ffee", ciKey("widgets", "!2"): "beef"}
	h.sup.track("d").Merged = map[string]string{key: "c0ffee"}
	h.sup.st.CI = map[string]CIRecord{
		key:                    {CIPayload: CIPayload{Repository: "widgets", Ref: "!1", Head: "c0ffee", State: CIMerged}, Session: "d"},
		ciKey("widgets", "!3"): {CIPayload: CIPayload{Repository: "widgets", Ref: "!3", Head: "f00d", State: CIMerged}, Session: "d"},
	}
	var got []string
	for _, e := range h.sup.feedEntries() {
		got = append(got, e.Text)
	}
	want := []string{"agentd merged !1 in widgets at " + shortHash("c0ffee") + ", d's delivery.", "!3 in widgets: CI merged on " + shortHash("f00d") + "."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}
