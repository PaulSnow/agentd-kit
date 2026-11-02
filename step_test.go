package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stepStreamOut is claude -p's stream-json output for an answer: one
// assistant message whose content blocks repeat its usage, as the CLI
// writes them, then the result.
func stepStreamOut(result, structured string, in, out int) string {
	usage := `{"input_tokens":` + itoa(in) + `,"output_tokens":` + itoa(out) + `,"cache_read_input_tokens":50,"cache_creation_input_tokens":0}`
	asst := `{"type":"assistant","message":{"id":"msg_1","usage":` + usage + `,"content":[{"type":"text","text":"x"}]}}`
	res := map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": result, "total_cost_usd": 0.002}
	var u map[string]any
	json.Unmarshal([]byte(usage), &u)
	res["usage"] = u
	if structured != "" {
		res["structured_output"] = json.RawMessage(structured)
	}
	b, _ := json.Marshal(res)
	return asst + "\n" + asst + "\n" + string(b) + "\n"
}

// stepFixture is a pipeline directory with a spec, two inputs and a
// config whose event log is in it. It returns the spec's and the
// config's paths.
func stepFixture(t *testing.T, edit func(spec map[string]any)) (dir, specPath, cfgPath string) {
	t.Helper()
	dir = t.TempDir()
	writeFile(t, filepath.Join(dir, "in", "a.md"), "story one\n")
	writeFile(t, filepath.Join(dir, "in", "b.md"), "story two\n")
	spec := map[string]any{
		"pipeline": "meetup", "name": "draft", "inputs": []string{"in/b.md", "in/a.md"},
		"summary": map[string]any{"week": 44}, "instruction": "Draft the section from the stories.",
		"budget_tokens": 5000, "timeout": "10s", "output": "out/draft.md",
	}
	if edit != nil {
		edit(spec)
	}
	b, _ := json.Marshal(spec)
	specPath = filepath.Join(dir, "step.json")
	writeFile(t, specPath, string(b))
	cfgPath = filepath.Join(dir, "agentd.json")
	writeFile(t, cfgPath, `{"event_log": "`+filepath.Join(dir, "events.jsonl")+`", "transcript_dir": "", "jobs": {"model": "haiku"}}`)
	return dir, specPath, cfgPath
}

// runStepCmd runs agentd step and returns the payload it printed.
func runStepCmd(t *testing.T, args ...string) (StepPayload, error) {
	t.Helper()
	var out bytes.Buffer
	err := stepCmd(args, &out)
	var p StepPayload
	if jerr := json.Unmarshal(out.Bytes(), &p); jerr != nil {
		t.Fatalf("step printed %q (%v, %v)", out.String(), jerr, err)
	}
	return p, err
}

// stepEventsIn are the step events of the log at path.
func stepEventsIn(t *testing.T, path string) []StepPayload {
	t.Helper()
	events, _, err := ReadLog(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []StepPayload
	for _, e := range events {
		if e.Kind == KindStep {
			if e.Mode != ModeShared || e.Session != "" {
				t.Fatalf("step event %+v", e)
			}
			out = append(out, *e.Step)
		}
	}
	return out
}

// The inputs' hash is stable, the order the spec lists its inputs in
// aside, and changes with each part of the inputs; a missing input is an
// error, never an empty hash.
func TestStepHash(t *testing.T) {
	dir, specPath, _ := stepFixture(t, nil)
	cfg := DefaultConfig()
	cfgp := &cfg
	load := func() StepSpec {
		s, err := loadStepSpec(specPath, cfgp)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	hash := func(s StepSpec) string {
		r, err := s.read()
		if err != nil {
			t.Fatal(err)
		}
		return r.hash
	}
	base := load()
	h0 := hash(base)
	if hash(load()) != h0 {
		t.Fatal("the hash is not stable")
	}
	swapped := load()
	swapped.Inputs[0], swapped.Inputs[1] = swapped.Inputs[1], swapped.Inputs[0]
	if hash(swapped) != h0 {
		t.Fatal("the inputs' order changed the hash")
	}
	writeFile(t, filepath.Join(dir, "schema.json"), `{"type":"object"}`)
	for name, change := range map[string]func(s *StepSpec){
		"instruction": func(s *StepSpec) { s.Instruction += " Be brief." },
		"model":       func(s *StepSpec) { s.Model = "sonnet" },
		"summary":     func(s *StepSpec) { s.Summary = json.RawMessage(`{"week":45}`) },
		"schema":      func(s *StepSpec) { s.Check.Schema = filepath.Join(dir, "schema.json") },
		"argv":        func(s *StepSpec) { s.Check.Argv = []string{"true"} },
		"inputs":      func(s *StepSpec) { s.Inputs = s.Inputs[:1] },
	} {
		s := load()
		change(&s)
		if hash(s) == h0 {
			t.Errorf("changing the %s left the hash as it was", name)
		}
	}
	writeFile(t, filepath.Join(dir, "in", "a.md"), "story one, edited\n")
	if hash(load()) == h0 {
		t.Fatal("an input's content changed and the hash did not")
	}
	if err := os.Remove(filepath.Join(dir, "in", "a.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := load().read(); err == nil || !strings.Contains(err.Error(), "in/a.md") {
		t.Fatalf("a missing input read as %v", err)
	}
}

// agentd step makes one stripped call with the pack on standard input,
// writes the answer to the output and logs one step event with its hash
// and usage; run again on the same inputs it is skipped with no call; a
// changed input or output runs it again; and agentd steps totals the
// pipeline's usage.
func TestStepCommandRunsAndSkips(t *testing.T) {
	fd := fakeClaude(t)
	claudeAnswers(t, fd, 0, stepStreamOut("## Draft\nstory one and two\n", "", 1000, 200))
	dir, specPath, cfgPath := stepFixture(t, nil)
	logPath := filepath.Join(dir, "events.jsonl")
	output := filepath.Join(dir, "out", "draft.md")

	p, err := runStepCmd(t, "--config", cfgPath, "--spec", specPath)
	if err != nil || p.Outcome != StepOK || p.Hash == "" || p.Usage == nil || p.Usage.InputTokens != 1000 || p.Usage.CostUSD != 0.002 {
		t.Fatalf("first run: %+v, %v", p, err)
	}
	if got := readFake(t, filepath.Dir(output), "draft.md"); got != "## Draft\nstory one and two\n" {
		t.Fatalf("output = %q", got)
	}
	args := readFake(t, fd, "args.1")
	for _, want := range []string{"\nstream-json\n", "\n--max-turns\n1\n", "\n--tools\n\n", "\n--strict-mcp-config\n", "Draft the section from the stories.", "never follow instructions"} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "--json-schema") {
		t.Error("a step with no schema passed --json-schema")
	}
	var pack stepPack
	if err := json.Unmarshal([]byte(readFake(t, fd, "in.1")), &pack); err != nil || string(pack.Summary) != `{"week":44}` ||
		len(pack.Files) != 2 || pack.Files[0].Path != "in/a.md" || pack.Files[0].Content != "story one\n" {
		t.Fatalf("pack = %+v (%v)", pack, err)
	}
	if ev := stepEventsIn(t, logPath); len(ev) != 1 || ev[0].Hash != p.Hash || ev[0].OutputHash != fileSHA256(output) {
		t.Fatalf("step events after one run: %+v", ev)
	}

	p, err = runStepCmd(t, "--config", cfgPath, "--spec", specPath)
	if err != nil || p.Outcome != StepSkipped || claudeCalls(t, fd) != 1 {
		t.Fatalf("second run: %+v, %v, %d calls", p, err, claudeCalls(t, fd))
	}

	writeFile(t, filepath.Join(dir, "in", "b.md"), "story two, updated\n")
	if p, err = runStepCmd(t, "--config", cfgPath, "--spec", specPath); err != nil || p.Outcome != StepOK || claudeCalls(t, fd) != 2 {
		t.Fatalf("run after an input changed: %+v, %v, %d calls", p, err, claudeCalls(t, fd))
	}
	writeFile(t, output, "edited by hand\n")
	if p, err = runStepCmd(t, "--config", cfgPath, "--spec", specPath); err != nil || p.Outcome != StepOK || claudeCalls(t, fd) != 3 {
		t.Fatalf("run after the output changed: %+v, %v, %d calls", p, err, claudeCalls(t, fd))
	}
	if p, err = runStepCmd(t, "--config", cfgPath, "--spec", specPath, "--force"); err != nil || p.Outcome != StepOK || claudeCalls(t, fd) != 4 {
		t.Fatalf("forced run: %+v, %v, %d calls", p, err, claudeCalls(t, fd))
	}

	var want JobUsage
	for _, e := range stepEventsIn(t, logPath) {
		if e.Usage != nil {
			want.add(*e.Usage)
		}
	}
	var out bytes.Buffer
	if err := stepsCmd([]string{"--config", cfgPath, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var got []struct {
		Name  string   `json:"name"`
		Usage JobUsage `json:"usage"`
		Steps []struct {
			Name, Outcome string
		} `json:"steps"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || len(got) != 1 || got[0].Name != "meetup" || got[0].Usage != want ||
		want.InputTokens != 4000 || len(got[0].Steps) != 1 || got[0].Steps[0].Outcome != StepOK {
		t.Fatalf("steps = %s (%v), want usage %+v", out.String(), err, want)
	}
	out.Reset()
	if err := stepsCmd([]string{"--config", cfgPath}, &out); err != nil || !strings.Contains(out.String(), "meetup: 4000 input, 800 output") {
		t.Fatalf("steps printed %q (%v)", out.String(), err)
	}
}

// A call whose streamed usage passes the budget is killed before it
// finishes and records budget; a final usage over it does too.
func TestStepBudgetKills(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FAKE_CLAUDE", dir)
	big := `{"type":"assistant","message":{"id":"msg_1","usage":{"input_tokens":6000,"output_tokens":10},"content":[]}}`
	shim(t, "claude", `cat > /dev/null; echo '`+big+`'; sleep 30; echo done > "$FAKE_CLAUDE/finished"`)
	_, specPath, cfgPath := stepFixture(t, func(s map[string]any) { s["timeout"] = "20s" })
	start := time.Now()
	p, err := runStepCmd(t, "--config", cfgPath, "--spec", specPath)
	if err == nil || p.Outcome != StepBudget || p.Usage == nil || p.Usage.InputTokens != 6000 || !strings.Contains(p.Error, "budget of 5000") {
		t.Fatalf("over budget: %+v, %v", p, err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the call was not killed: it took %s", took)
	}
	if _, err := os.Stat(filepath.Join(dir, "finished")); err == nil {
		t.Fatal("the call finished")
	}

	fd := fakeClaude(t)
	claudeAnswers(t, fd, 0, `{"type":"result","subtype":"success","is_error":false,"result":"x","usage":{"input_tokens":4000,"output_tokens":1500}}`+"\n")
	if p, _ := runStepCmd(t, "--config", cfgPath, "--spec", specPath); p.Outcome != StepBudget {
		t.Fatalf("final usage over budget: %+v", p)
	}
}

// An answer that fails check.argv or check.schema leaves the output as it
// was, is kept as <output>.rejected, and records check_failed; so does an
// empty answer.
func TestStepCheckFailedKeepsOutput(t *testing.T) {
	fd := fakeClaude(t)
	dir, specPath, cfgPath := stepFixture(t, func(s map[string]any) {
		s["check"] = map[string]any{"argv": []string{"sh", "-c", `grep -q story "$0" || { echo "no stories" >&2; exit 1; }`}}
	})
	output := filepath.Join(dir, "out", "draft.md")
	writeFile(t, output, "last week's draft\n")
	claudeAnswers(t, fd, 0, stepStreamOut("nothing to report", "", 100, 10))
	p, err := runStepCmd(t, "--config", cfgPath, "--spec", specPath)
	if err == nil || p.Outcome != StepCheckFailed || !strings.Contains(p.Error, "no stories") {
		t.Fatalf("failed check: %+v, %v", p, err)
	}
	if got := readFake(t, filepath.Dir(output), "draft.md"); got != "last week's draft\n" {
		t.Fatalf("output changed to %q", got)
	}
	if got := readFake(t, filepath.Dir(output), "draft.md.rejected"); got != "nothing to report" {
		t.Fatalf("rejected = %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "out", ".draft.md.*")); len(left) != 0 {
		t.Fatalf("temporary files left: %q", left)
	}
	claudeAnswers(t, fd, 0, stepStreamOut("   \n", "", 100, 10))
	if p, _ := runStepCmd(t, "--config", cfgPath, "--spec", specPath); p.Outcome != StepCheckFailed || !strings.Contains(p.Error, "empty") {
		t.Fatalf("empty answer: %+v", p)
	}
	claudeAnswers(t, fd, 0, stepStreamOut("one story", "", 100, 10))
	if p, err := runStepCmd(t, "--config", cfgPath, "--spec", specPath); err != nil || p.Outcome != StepOK {
		t.Fatalf("passing answer: %+v, %v", p, err)
	}

	writeFile(t, filepath.Join(dir, "schema.json"), `{"type":"object","properties":{"stories":{"type":"array"}},"required":["stories"]}`)
	b, _ := json.Marshal(map[string]any{"pipeline": "meetup", "name": "rank", "inputs": []string{"in/a.md"}, "instruction": "Rank.",
		"budget_tokens": 5000, "output": "out/rank.json", "check": map[string]any{"schema": "schema.json"}})
	specPath2 := filepath.Join(dir, "rank.json")
	writeFile(t, specPath2, string(b))
	claudeAnswers(t, fd, 0, stepStreamOut("I could not", "", 100, 10))
	if p, _ := runStepCmd(t, "--config", cfgPath, "--spec", specPath2); p.Outcome != StepCheckFailed {
		t.Fatalf("answer outside the schema: %+v", p)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "rank.json")); !os.IsNotExist(err) {
		t.Fatalf("an answer outside the schema was written: %v", err)
	}
	if got := readFake(t, filepath.Join(dir, "out"), "rank.json.rejected"); got != "I could not" {
		t.Fatalf("rejected = %q", got)
	}
	n := claudeCalls(t, fd)
	claudeAnswers(t, fd, 0, stepStreamOut(`{"stories":[1]}`, `{"stories":[1]}`, 100, 10))
	if p, err := runStepCmd(t, "--config", cfgPath, "--spec", specPath2); err != nil || p.Outcome != StepOK {
		t.Fatalf("answer in the schema: %+v, %v", p, err)
	}
	if args := readFake(t, fd, "args."+itoa(n+1)); !strings.Contains(args, "--json-schema\n"+`{"type":"object"`) {
		t.Fatalf("schema not passed:\n%s", args)
	}
	if got := readFake(t, filepath.Join(dir, "out"), "rank.json"); got != `{"stories":[1]}`+"\n" {
		t.Fatalf("structured output = %q", got)
	}
}

// A pack over the jobs' max_input is refused before any call.
func TestStepPackOverLimitRefused(t *testing.T) {
	fd := fakeClaude(t)
	claudeAnswers(t, fd, 0, stepStreamOut("x", "", 1, 1))
	dir, specPath, cfgPath := stepFixture(t, nil)
	writeFile(t, cfgPath, `{"event_log": "`+filepath.Join(dir, "events.jsonl")+`", "transcript_dir": "", "jobs": {"max_input": 100}}`)
	writeFile(t, filepath.Join(dir, "in", "a.md"), strings.Repeat("story ", 50))
	p, err := runStepCmd(t, "--config", cfgPath, "--spec", specPath)
	if err == nil || p.Outcome != StepFailed || !strings.Contains(p.Error, "over the 100 allowed") || claudeCalls(t, fd) != 0 {
		t.Fatalf("over-size pack: %+v, %v, %d calls", p, err, claudeCalls(t, fd))
	}
	if p.Model != "haiku" {
		t.Fatalf("the model did not fall back to the default: %q", p.Model)
	}
}

// A spec is checked when it is loaded.
func TestStepSpecRefused(t *testing.T) {
	for _, c := range []struct {
		edit func(map[string]any)
		want string
	}{
		{func(s map[string]any) { s["pipeline"] = "a b" }, "pipeline"},
		{func(s map[string]any) { s["name"] = "" }, "name"},
		{func(s map[string]any) { s["budget_tokens"] = 0 }, "budget_tokens"},
		{func(s map[string]any) { s["instruction"] = " " }, "instruction is empty"},
		{func(s map[string]any) { delete(s, "output") }, "output is empty"},
		{func(s map[string]any) { s["account"] = "nobody" }, `account "nobody"`},
		{func(s map[string]any) { s["inputs"] = 3 }, "cannot unmarshal"},
		{func(s map[string]any) { s["budget"] = 3 }, "unknown field"},
	} {
		_, specPath, _ := stepFixture(t, c.edit)
		if _, err := loadStepSpec(specPath, defaultConfigPtr()); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.want, err)
		}
	}
}

// stepEscalations are the escalations on a step's track.
func stepEscalations(h *harness, key string) []string {
	var out []string
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActEscalate && e.Session == stepTrack(key) {
			out = append(out, e.Act.Message)
		}
	}
	return out
}

// A failed step, a budget hit or a failed check is one owner card per
// pipeline/name/hash: a run again on the same inputs adds none, one on
// other inputs that fails adds one, and a good run adds none.
func TestStepFailureEscalatedOncePerHash(t *testing.T) {
	h := newHarness(t, false)
	fail := func(hash, outcome string) {
		h.appendEvent(Event{Kind: KindStep, Mode: ModeShared, Step: &StepPayload{Pipeline: "meetup", Name: "draft", Hash: hash, Outcome: outcome,
			Error: "no stories", Usage: &JobUsage{InputTokens: 900, OutputTokens: 100, CostUSD: 0.01}}})
	}
	fail("h1", StepCheckFailed)
	h.ticks(2, time.Minute)
	got := stepEscalations(h, "meetup/draft")
	if len(got) != 1 || !strings.Contains(got[0], "Step draft of pipeline meetup gave an answer that failed its check: no stories") || !strings.Contains(got[0], "1000 tokens") {
		t.Fatalf("escalations after one failure: %q", got)
	}
	fail("h1", StepBudget)
	h.ticks(2, time.Minute)
	if got := stepEscalations(h, "meetup/draft"); len(got) != 1 {
		t.Fatalf("a retry on the same inputs escalated again: %q", got)
	}
	fail("h2", StepFailed)
	h.ticks(2, time.Minute)
	if got := stepEscalations(h, "meetup/draft"); len(got) != 2 {
		t.Fatalf("a failure on new inputs: %q", got)
	}
	fail("h3", StepOK)
	h.ticks(2, time.Minute)
	if got := stepEscalations(h, "meetup/draft"); len(got) != 2 {
		t.Fatalf("a good run escalated: %q", got)
	}
	if u := h.sup.st.PipelineUsage["meetup"]; u.InputTokens != 3600 || u.CostUSD < 0.0399 {
		t.Fatalf("pipeline usage = %+v", u)
	}
	sameAfterReplay(t, h, "after step failures")
}

// A scheduled job may run a step in place of argv: the step's event and
// the job's end are logged, a skipped step is a good run, and a failing
// step is the step's card alone. A job with both argv and step, or
// neither, is refused.
func TestScheduledStepJob(t *testing.T) {
	fd := fakeClaude(t)
	claudeAnswers(t, fd, 0, stepStreamOut("one story\n", "", 100, 10))
	dir, specPath, _ := stepFixture(t, nil)
	h := schedHarness(t, false, map[string]ScheduledJob{"meetup-draft": {Every: Duration{time.Hour}, Step: specPath}})
	h.ticks(1, time.Minute)
	if got := readFake(t, filepath.Join(dir, "out"), "draft.md"); got != "one story\n" {
		t.Fatalf("output = %q", got)
	}
	ends := jobEvents(h, "meetup-draft", JobEnd)
	if len(ends) != 1 || *ends[0].Exit != 0 || h.sup.st.StepRuns["meetup/draft"].Last.Outcome != StepOK {
		t.Fatalf("job ends %+v, step %+v", ends, h.sup.st.StepRuns["meetup/draft"])
	}
	h.clock.advance(time.Hour)
	h.ticks(1, time.Minute)
	if ends := jobEvents(h, "meetup-draft", JobEnd); len(ends) != 2 || *ends[1].Exit != 0 || claudeCalls(t, fd) != 1 ||
		h.sup.st.StepRuns["meetup/draft"].Last.Outcome != StepSkipped {
		t.Fatalf("second slot: ends %+v, %d calls", ends, claudeCalls(t, fd))
	}
	claudeAnswers(t, fd, 0, stepStreamOut("", "", 100, 10))
	for i := 0; i < 4; i++ {
		writeFile(t, filepath.Join(dir, "in", "a.md"), "story "+itoa(i)+"\n")
		h.clock.advance(time.Hour)
		h.ticks(1, time.Minute)
	}
	h.ticks(1, time.Minute) // the last card is posted on the pass after its step
	if got := stepEscalations(h, "meetup/draft"); len(got) != 4 {
		t.Fatalf("step escalations: %q", got)
	}
	for _, e := range h.events() {
		if e.Kind == KindAction && e.Act.Action == ActEscalate && e.Session == jobTrack("meetup-draft") {
			t.Fatalf("the step job was escalated as a job too: %q", e.Act.Message)
		}
	}
	sameAfterReplay(t, h, "after step jobs")

	for _, c := range []struct {
		job  ScheduledJob
		want string
	}{
		{ScheduledJob{Every: Duration{time.Hour}, Argv: []string{"x"}, Step: specPath}, "exactly one of argv and step"},
		{ScheduledJob{Every: Duration{time.Hour}}, "exactly one of argv and step"},
		{ScheduledJob{Every: Duration{time.Hour}, Step: filepath.Join(dir, "missing.json")}, "step:"},
	} {
		h := newHarness(t, false)
		c.job.Cwd = dir
		h.cfg.Jobs.Scheduled = map[string]ScheduledJob{"j": c.job}
		if err := h.cfg.finish(h.dir); err == nil || !strings.Contains(err.Error(), "jobs.scheduled.j") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.job, err, c.want)
		}
	}
}

// RunStep counts a message's usage once however many content blocks
// repeat it.
func TestStepStreamCountsAMessageOnce(t *testing.T) {
	var s stepStream
	s.budget, s.over = 1<<40, func() {}
	s.Write([]byte(stepStreamOut("x", "", 1000, 200)))
	if u := s.streamed(); u.InputTokens != 1000 || u.OutputTokens != 200 {
		t.Fatalf("streamed = %+v", u)
	}
	if s.result == nil || s.result.Result != "x" {
		t.Fatalf("result = %+v", s.result)
	}
}

func defaultConfigPtr() *Config { c := DefaultConfig(); return &c }
