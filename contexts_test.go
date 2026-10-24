package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// call is one line of a fake transcript: an assistant turn and its usage.
func usageLine(id string, in, cw, cr, out int64, sidechain bool) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "isSidechain": sidechain, "timestamp": "2026-10-22T00:00:00Z",
		"message": map[string]any{"id": id, "usage": map[string]int64{"input_tokens": in, "cache_creation_input_tokens": cw,
			"cache_read_input_tokens": cr, "output_tokens": out}}})
	return string(b) + "\n"
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// A call written as several lines (one per content block, the same message
// id) counts once, with its last line's usage; subagent transcripts count
// toward spend and not toward the context; the context is the latest
// main-chain call's input, cache write and cache read.
func TestUsageFromTranscriptWithDuplicatesAndSubagents(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "s1.jsonl")
	appendFile(t, main, usageLine("m1", 10, 100, 0, 3, false)+usageLine("m1", 10, 100, 0, 7, false)+
		`{"type":"user","message":{"content":"ok"}}`+"\n"+
		usageLine("side", 1, 0, 0, 1, true)+
		usageLine("m2", 20, 200, 1000, 5, false)+`{"type":"assistant","message":{"id":"m3","usage"`)
	appendFile(t, filepath.Join(dir, "s1", "subagents", "agent-a.jsonl"), usageLine("a1", 40, 0, 0, 2, false))
	r := newUsageReader()
	u, ok, err := r.measure(dir, "s1")
	if err != nil || !ok {
		t.Fatalf("measure = %v, %v", ok, err)
	}
	want := UsagePayload{Transcript: "s1", Calls: 4, Input: 71, CacheWrite: 300, CacheRead: 1000, Output: 15, Context: 1220}
	want.Spend = spendOf(want)
	if u != want {
		t.Fatalf("usage = %+v, want %+v", u, want)
	}
	if want.Spend != 71+375+100+75 {
		t.Fatalf("spend = %d", want.Spend)
	}
	// Only appended bytes are read: an earlier line rewritten in place is
	// not read again, and the cut-off line completes.
	b, _ := os.ReadFile(main)
	if err := os.WriteFile(main, []byte(strings.Replace(string(b), `"input_tokens":10`, `"input_tokens":99`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	appendFile(t, main, `:{"input_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":2000,"output_tokens":1}}}`+"\n")
	u, _, _ = r.measure(dir, "s1")
	if u.Calls != 5 || u.Input != 72 || u.Context != 2001 {
		t.Fatalf("after append: %+v", u)
	}
	// A transcript with no calls yet is unknown, never zero.
	appendFile(t, filepath.Join(dir, "s2.jsonl"), `{"type":"user","message":{"content":"hi"}}`+"\n")
	if _, ok, _ := r.measure(dir, "s2"); ok {
		t.Fatal("a transcript with no calls was measured")
	}
	if _, ok, _ := r.measure(dir, "absent"); ok {
		t.Fatal("a missing transcript was measured")
	}
}

func TestParseProgress(t *testing.T) {
	p := ParseProgress("# Night\n\nSome text | with a bar\n\n| Sub-issue | Branch | State | Merge |\n|---|---|---|---|\n" +
		"| A #9 inventory | night/9 | merged, check PASS | 77b8023 |\n| L #21 contexts | night/21 | review round 1 | |\n" +
		"| K #20 poll | | waiting | |\n| G #15 placement | | deferred by the owner | |\n| E #13 jobs | | waiting | |\n\n## Log\n")
	if p.Item != "L #21 contexts" || p.State != "review round 1" || strings.Join(p.Next, ",") != "K #20 poll,E #13 jobs" {
		t.Fatalf("progress = %+v", p)
	}
	if p := ParseProgress("no table here\n"); p.Item != "" || p.Next != nil {
		t.Fatalf("progress without a table = %+v", p)
	}
}

// promptFacts reads the facts a restart prompt carries after its
// instruction.
func promptFacts(t *testing.T, prompt string) restartFacts {
	t.Helper()
	if !strings.HasPrefix(prompt, DefaultRestartPrompt+" ") {
		t.Fatalf("prompt does not start with the instruction: %q", prompt)
	}
	var f restartFacts
	if err := json.Unmarshal([]byte(strings.TrimPrefix(prompt, DefaultRestartPrompt+" ")), &f); err != nil {
		t.Fatalf("facts: %v: %q", err, prompt)
	}
	return f
}

// contextHarness is a live harness whose one session, s, runs under a
// transcript agentd launched: it was found gone and restarted.
func contextHarness(t *testing.T, shadow bool) (*harness, string) {
	t.Helper()
	h := newHarness(t, shadow, "s")
	h.cfg.TranscriptDir = filepath.Join(h.dir, "projects")
	h.status("s", "# s\n\n| Item | State |\n|---|---|\n| L #21 contexts | merged |\n| K #20 poll | coding |\n| E #13 jobs | waiting |\n")
	h.git.merge, h.git.mergeSubject = "m0", "Merge #18: items"
	h.ticks(1, time.Minute) // gone: restarted; m0 is the baseline
	id := ""
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActRestart {
			id = e.Act.Transcript
		}
	}
	if id == "" && !shadow {
		t.Fatal("the restart named no transcript")
	}
	return h, id
}

func (h *harness) transcript(id, text string) {
	h.t.Helper()
	appendFile(h.t, filepath.Join(TranscriptDir(h.cfg.TranscriptDir, h.cfg.Sessions["s"].Cwd), id+".jsonl"), text)
}

func TestRotateAtCheckpointNotBefore(t *testing.T) {
	h, id := contextHarness(t, false)
	h.transcript(id, usageLine("m1", 10, 150000, 0, 5, false))
	h.ticks(3, time.Minute)
	if n := h.count(KindAction, ActRotate, "s"); n != 0 {
		t.Fatalf("rotated with no new merge: %d", n)
	}
	if u, ok := h.sup.Tracks()["s"].current(); !ok || u.Context != 150010 {
		t.Fatalf("context = %+v, %v", u, ok)
	}
	h.git.merge, h.git.mergeSubject = "m1", "Merge #21: contexts"
	h.ticks(2, time.Minute) // the checkpoint is seen, and the grace runs
	if n := h.count(KindAction, ActRotate, "s"); n != 0 {
		t.Fatalf("rotated within the grace after the merge: %d", n)
	}
	h.ticks(4, time.Minute)
	if n := h.count(KindAction, ActRotate, "s"); n != 1 {
		t.Fatalf("rotations = %d, want 1", n)
	}
	var rot *ActPayload
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActRotate {
			rot = e.Act
		}
	}
	if rot.Transcript == "" || rot.Transcript == id || rot.Checkpoint != "m1" || rot.Context != 150010 {
		t.Fatalf("rotation = %+v", rot)
	}
	if h.tmux.count("replace", "s") != 1 {
		t.Fatalf("replace calls = %d", h.tmux.count("replace", "s"))
	}
	// Not a crash and not a restart.
	tr := h.sup.Tracks()["s"]
	if h.count(KindCrash, "", "s") != 0 || tr.RestartCount != 1 || len(tr.Restarts) != 1 || tr.Rotations != 1 {
		t.Fatalf("crashes %d, restarts %d (%d), rotations %d", h.count(KindCrash, "", "s"), tr.RestartCount, len(tr.Restarts), tr.Rotations)
	}
	// The new transcript is measured from scratch; the old one's spend stays.
	if _, ok := tr.current(); ok {
		t.Fatal("the new transcript's context is known before it has a call")
	}
	if sp, ok := tr.spend(); !ok || sp != spendOf(tr.Usage[id]) {
		t.Fatalf("spend = %d, %v", sp, ok)
	}
	h.ticks(3, time.Minute)
	if n := h.count(KindAction, ActRotate, "s"); n != 1 {
		t.Fatalf("rotated again for the same merge: %d", n)
	}
	// Replayed, the rotation stands and is still not a restart.
	h.restart(false)
	if tr := h.sup.Tracks()["s"]; tr.Rotations != 1 || tr.RestartCount != 1 || tr.Transcript != rot.Transcript {
		t.Fatalf("after replay: rotations %d, restarts %d, transcript %q", tr.Rotations, tr.RestartCount, tr.Transcript)
	}
}

// A session under the threshold, or waiting on the owner, is not rotated.
func TestNoRotationUnderThresholdOrBlocked(t *testing.T) {
	h, id := contextHarness(t, false)
	h.transcript(id, usageLine("m1", 10, 1000, 0, 5, false))
	h.git.merge = "m1"
	h.ticks(10, time.Minute)
	if n := h.count(KindAction, ActRotate, "s"); n != 0 {
		t.Fatalf("rotated under the threshold: %d", n)
	}
	h.transcript(id, usageLine("m2", 10, 200000, 0, 5, false))
	h.status("s", "# s\n\n## Questions\n\nWhich way?\n")
	h.ticks(10, time.Minute)
	if n := h.count(KindAction, ActRotate, "s"); n != 0 {
		t.Fatalf("rotated a session waiting on the owner: %d", n)
	}
}

// The fresh session's prompt is built from the progress: the current item
// and its state, the last merge, and the next item.
func TestRestartPromptFromProgress(t *testing.T) {
	h, id := contextHarness(t, false)
	h.transcript(id, usageLine("m1", 10, 150000, 0, 5, false))
	h.git.merge, h.git.mergeSubject = "m1", "Merge #21: contexts"
	h.ticks(8, time.Minute)
	var cmd string
	for _, c := range h.tmux.calls {
		if c.op == "replace" {
			cmd = c.args[1]
		}
	}
	i := strings.Index(cmd, "{")
	j := strings.LastIndex(cmd, "}")
	if i < 0 || j < i {
		t.Fatalf("no facts in the prompt: %s", cmd)
	}
	var facts restartFacts
	if err := json.Unmarshal([]byte(strings.ReplaceAll(cmd[i:j+1], `'\''`, `'`)), &facts); err != nil {
		t.Fatalf("facts: %v: %s", err, cmd[i:j+1])
	}
	if facts.Why != "rotation" || facts.Item != "K #20 poll" || facts.State != "coding" || facts.LastMerge != "m1 Merge #21: contexts" ||
		strings.Join(facts.Next, ",") != "E #13 jobs" || !strings.HasSuffix(facts.Brief, "BRIEF.md") || !strings.HasSuffix(facts.Progress, "STATUS.md") {
		t.Fatalf("facts = %+v", facts)
	}
	if !strings.Contains(cmd, "--session-id") {
		t.Fatalf("the command does not name the transcript: %s", cmd)
	}
}

func TestBudgetStopsSession(t *testing.T) {
	h, id := contextHarness(t, false)
	h.cfg.Policies.Budget.SessionTokens = 100000
	h.transcript(id, usageLine("m1", 10, 1000, 0, 5, false))
	h.ticks(2, time.Minute)
	if n := h.count(KindAction, ActStop, "s"); n != 0 {
		t.Fatalf("stopped under the cap: %d", n)
	}
	h.transcript(id, usageLine("m2", 100000, 0, 0, 5, false))
	h.ticks(2, time.Minute)
	if n := h.count(KindAction, ActStop, "s"); n != 1 {
		t.Fatalf("stops = %d, want 1", n)
	}
	tr := h.sup.Tracks()["s"]
	if h.state("s") != Stopped || !strings.Contains(tr.Stopped, "session spend") || h.tmux.alive["s"] {
		t.Fatalf("state %s, stopped %q, alive %v", h.state("s"), tr.Stopped, h.tmux.alive["s"])
	}
	restarts := h.tmux.count("new-session", "s")
	h.ticks(5, time.Minute)
	if h.tmux.count("new-session", "s") != restarts || h.count(KindCrash, "", "s") != 0 || h.count(KindAction, ActStop, "s") != 1 {
		t.Fatalf("a stopped session was restarted (%d), crashed (%d) or stopped again", h.tmux.count("new-session", "s")-restarts, h.count(KindCrash, "", "s"))
	}
	h.restart(false)
	if h.sup.Tracks()["s"].Stopped == "" {
		t.Fatal("the stop did not survive replay")
	}
}

func TestStopTimeAndRunBudget(t *testing.T) {
	h, _ := contextHarness(t, false)
	at := h.clock.Now().Add(10 * time.Minute)
	h.cfg.Policies.Budget.StopAt = &at
	h.ticks(5, time.Minute)
	if n := h.count(KindAction, ActStop, "s"); n != 0 {
		t.Fatalf("stopped before the stop time: %d", n)
	}
	h.ticks(10, time.Minute)
	if n := h.count(KindAction, ActStop, "s"); n != 1 || !strings.Contains(h.sup.Tracks()["s"].Stopped, "stop time") {
		t.Fatalf("stops = %d, why %q", n, h.sup.Tracks()["s"].Stopped)
	}

	h2 := newHarness(t, false, "a", "b")
	h2.cfg.TranscriptDir = filepath.Join(h2.dir, "projects")
	h2.cfg.Policies.Budget.RunTokens = 1500
	h2.ticks(1, time.Minute)
	for _, n := range []string{"a", "b"} {
		id := h2.sup.Tracks()[n].Transcript
		appendFile(t, filepath.Join(TranscriptDir(h2.cfg.TranscriptDir, h2.cfg.Sessions[n].Cwd), id+".jsonl"), usageLine("m", 800, 0, 0, 0, false))
	}
	h2.ticks(3, time.Minute)
	if h2.count(KindAction, ActStop, "a") != 1 || h2.count(KindAction, ActStop, "b") != 1 || !strings.Contains(h2.sup.Tracks()["a"].Stopped, "run spend") {
		t.Fatalf("run budget: a %d, b %d, why %q", h2.count(KindAction, ActStop, "a"), h2.count(KindAction, ActStop, "b"), h2.sup.Tracks()["a"].Stopped)
	}
}

// Shadow mode measures the newest transcript of a session it did not
// launch, marks it a guess, and records the rotation it would make.
func TestShadowGuessesTranscriptAndWouldRotate(t *testing.T) {
	h := newHarness(t, true, "s")
	h.cfg.TranscriptDir = filepath.Join(h.dir, "projects")
	h.alive("s", "working")
	h.git.merge = "m0"
	h.transcript("outside", usageLine("m1", 10, 150000, 0, 5, false))
	h.ticks(2, time.Minute)
	u, ok := h.sup.Tracks()["s"].current()
	if !ok || !u.Guessed || u.Transcript != "outside" {
		t.Fatalf("guessed usage = %+v, %v", u, ok)
	}
	h.git.merge = "m1"
	for i := 0; i < 8; i++ {
		h.tmux.setPane("s", fmt.Sprint("working ", i))
		h.ticks(1, time.Minute)
	}
	if n := h.count(KindWould, ActRotate, "s"); n != 1 {
		t.Fatalf("would-rotations = %d, want 1", n)
	}
	if h.tmux.count("replace", "") != 0 {
		t.Fatal("shadow mode replaced a pane")
	}
}

// Live mode acts only on the transcript it launched, never on a guess.
func TestLiveDoesNotRotateOnGuess(t *testing.T) {
	h := newHarness(t, false, "s")
	h.cfg.TranscriptDir = filepath.Join(h.dir, "projects")
	h.alive("s", "working")
	h.git.merge = "m0"
	h.transcript("outside", usageLine("m1", 10, 150000, 0, 5, false))
	h.ticks(2, time.Minute)
	h.git.merge = "m1"
	for i := 0; i < 8; i++ {
		h.tmux.setPane("s", fmt.Sprint("working ", i))
		h.ticks(1, time.Minute)
	}
	if n := h.count(KindAction, ActRotate, "s") + h.count(KindIntent, ActRotate, "s"); n != 0 {
		t.Fatalf("live rotated on a guessed transcript: %d", n)
	}
}

func TestStatusShowsContextAndSpend(t *testing.T) {
	h, id := contextHarness(t, false)
	h.transcript(id, usageLine("m1", 10, 150000, 0, 5, false))
	h.ticks(2, time.Minute)
	rows, meta := Summarize(h.cfg, h.events(), ModeLive)
	if rows[0].Context != 150010 || rows[0].Spend != spendOf(h.sup.Tracks()["s"].Usage[id]) {
		t.Fatalf("row = %+v", rows[0])
	}
	var b strings.Builder
	PrintStatus(&b, rows, meta, h.clock.Now())
	if !strings.Contains(b.String(), "CONTEXT") || !strings.Contains(b.String(), "150k") {
		t.Fatalf("status:\n%s", b.String())
	}
	h2 := newHarness(t, false, "s")
	rows, _ = Summarize(h2.cfg, h2.events(), ModeLive)
	if rows[0].Context != -1 || rows[0].Spend != -1 {
		t.Fatalf("unmeasured row = %+v", rows[0])
	}
}
