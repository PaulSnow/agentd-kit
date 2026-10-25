package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A request the policy does not cover does not merge: by another author,
// into a target outside targets, excluded by number or label, or without
// a named approval at its head. Without green CI at its head it does not
// merge either (TestMergeNoPipelineIsNotGreen, TestMergeNeedsGreenAndGivesUp).
func TestMergeRefusedByPolicy(t *testing.T) {
	approvals := ciProject + "/merge_requests/1/approvals"
	for name, c := range map[string]struct {
		change func(m *MergeRules)
		api    func(api *fakeForgeAPI)
		want   string
	}{
		"author": {func(m *MergeRules) { m.Authors = []string{"someone-else"} }, nil, "its author widget-bot is not one"},
		"target": {func(m *MergeRules) { m.Targets = []string{"release"} }, nil, "its target main is not one"},
		"number": {func(m *MergeRules) { m.Exclude.Numbers = []int{1} }, nil, "excludes it"},
		"label":  {func(m *MergeRules) { m.Exclude.Labels = []string{"agentd"} }, nil, "excludes it"},
		"reserved": {func(m *MergeRules) {}, func(api *fakeForgeAPI) {
			api.gitlab[ciProject+"/merge_requests/1"] = strings.Replace(api.gitlab[ciProject+"/merge_requests/1"], `"labels":["agentd"]`, `"labels":["owner-only"]`, 1)
		}, "label the policy reserves"},
		"approval": {func(m *MergeRules) { m.Approvals = []string{"lead"} }, func(api *fakeForgeAPI) {
			api.gitlab[approvals] = `{"approved_by":[{"user":{"username":"someone"}}]}`
		}, "lead has not approved it"},
	} {
		t.Run(name, func(t *testing.T) {
			h, api := mergeHarness(t)
			setMerge(t, h, c.change)
			w := h.cfg.Repositories["widgets"]
			w.Policy.Scope.Reserved.Labels = []string{"owner-only"}
			h.cfg.Repositories["widgets"] = w
			if c.api != nil {
				c.api(api)
			}
			h.ticks(6, 5*time.Minute)
			if s := h.mergeSkips(); len(h.forge.merges) != 0 || len(s) != 1 || !strings.Contains(s[0], c.want) {
				t.Fatalf("merges = %+v, skips %q", h.forge.merges, s)
			}
			for _, call := range api.callList() {
				if name == "target" && strings.Contains(call, "policy.json") {
					t.Fatalf("read the file of a target outside the policy: %s", call)
				}
			}
		})
	}
	// With the approval given, it merges.
	h, api := mergeHarness(t)
	setMerge(t, h, func(m *MergeRules) { m.Approvals = []string{"lead"} })
	api.set(func() { api.gitlab[approvals] = `{"approved_by":[{"user":{"username":"lead"}}]}` })
	h.ticks(6, 5*time.Minute)
	if len(h.forge.merges) != 1 {
		t.Fatalf("approved: merges = %+v, skips %q", h.forge.merges, h.mergeSkips())
	}
}

// Each method merges through the forge client, asked with that method; only
// a fast-forward needs the head to be a fast-forward of the target.
func TestMergeEachMethod(t *testing.T) {
	for _, method := range []string{MergeFastForward, MergeCommit, MergeSquash, MergeByCommand} {
		t.Run(method, func(t *testing.T) {
			h, api := mergeHarness(t)
			setMerge(t, h, func(m *MergeRules) {
				m.Method = method
				if method == MergeByCommand {
					m.Command = []string{"merge-it", "{request}", "{head}"}
				}
			})
			if method != MergeFastForward {
				api.set(func() { api.gitlab[ciProject+"/repository/merge_base"] = `{"id":"0ld0ne"}` })
			}
			h.ticks(6, 5*time.Minute)
			want := []mergedRequest{{ref: "!1", target: "main", head: "c0ffee", method: method}}
			if !reflect.DeepEqual(h.forge.merges, want) {
				t.Fatalf("merges = %+v, skips %q", h.forge.merges, h.mergeSkips())
			}
			for _, e := range h.events() {
				if e.Kind == KindAction && actionOf(e) == ActMerge && e.Act.Method != method {
					t.Fatalf("merge recorded method %q", e.Act.Method)
				}
			}
		})
	}
}

// The real clients: GitLab's merge call squashes for squash, GitHub's merge
// call takes the method and the head, and a command runs with its
// placeholders filled and prompts off.
func TestExecForgeMergeMethods(t *testing.T) {
	f := ExecForge{Timeout: 10 * time.Second}
	out := filepath.Join(t.TempDir(), "argv")
	gitlab := Repository{Forge: "gitlab", Host: "gitlab.example.com", Project: "example-org/widgets", Policy: RepoPolicy{Merge: MergeRules{Method: MergeSquash}}}
	shim(t, "glab", `echo "$*" > `+out)
	if err := f.Merge(gitlab, "!3", "c0ffee", "main"); err != nil {
		t.Fatal(err)
	}
	if b, _ := readFile(out); !strings.HasSuffix(string(b), "merge_requests/3/merge -f sha=c0ffee -F squash=true\n") {
		t.Fatalf("glab = %q", b)
	}
	github := Repository{Forge: "github", Project: "example-org/gadgets", Policy: RepoPolicy{Merge: MergeRules{Method: MergeCommit}}}
	shim(t, "gh", `echo "$GH_PROMPT_DISABLED $*" > `+out)
	if err := f.Merge(github, "#4", "abc1234", "main"); err != nil {
		t.Fatal(err)
	}
	if b, _ := readFile(out); string(b) != "1 api --method PUT repos/example-org/gadgets/pulls/4/merge -f sha=abc1234 -f merge_method=merge\n" {
		t.Fatalf("gh = %q", b)
	}
	shim(t, "merge-it", `echo "$GH_PROMPT_DISABLED $*" > `+out)
	cmd := Repository{Forge: "github", Project: "example-org/gadgets", Policy: RepoPolicy{Merge: MergeRules{Method: MergeByCommand,
		Command: []string{"merge-it", "--repo", "{repository}", "{request}", "{head}", "{target}"}}}}
	if err := f.Merge(cmd, "#4", "abc1234", "main"); err != nil {
		t.Fatal(err)
	}
	if b, _ := readFile(out); string(b) != "1 --repo example-org/gadgets #4 abc1234 main\n" {
		t.Fatalf("command = %q", b)
	}
}

// Review in require holds a merge until the session's case stopped its
// review rounds converged.
func TestMergeRequiresReview(t *testing.T) {
	h, _ := mergeHarness(t)
	setMerge(t, h, func(m *MergeRules) { m.Require = []string{RequireGate, RequireCI, RequireReview} })
	d := h.cfg.Sessions["d"]
	d.Issue = 5
	h.cfg.Sessions["d"] = d
	h.ticks(3, 5*time.Minute)
	if s := h.mergeSkips(); len(h.forge.merges) != 0 || len(s) != 1 || !strings.Contains(s[0], "review rounds") {
		t.Fatalf("merges = %+v, skips %q", h.forge.merges, s)
	}
	h.sup.st.Cases[h.cfg.caseOf(d)] = &Case{RoundStop: &RoundStop{Round: 2, Reason: RoundsConverged}}
	h.ticks(3, 5*time.Minute)
	if len(h.forge.merges) != 1 {
		t.Fatalf("reviewed: merges = %+v, skips %q", h.forge.merges, h.mergeSkips())
	}
}

// policyEvents is the repository policy files the log recorded.
func (h *harness) policyEvents() []PolicyPayload {
	var out []PolicyPayload
	for _, e := range h.events() {
		if e.Kind == KindPolicy {
			out = append(out, *e.Policy)
		}
	}
	return out
}

// A repository file that narrows the policy applies: here it excludes the
// request's label, so it does not merge. It is recorded once, and nobody
// is asked about it.
func TestPolicyFileNarrows(t *testing.T) {
	h, api := mergeHarness(t)
	api.set(func() {
		api.gitlab[mergePolicyPath+"|ref="+mergeTip] = `{"merge": {"exclude": {"labels": ["agentd"]}, "require": ["ci"]}}`
	})
	h.ticks(6, 5*time.Minute)
	if s := h.mergeSkips(); len(h.forge.merges) != 0 || len(s) != 1 || !strings.Contains(s[0], "excludes it") {
		t.Fatalf("merges = %+v, skips %q", h.forge.merges, s)
	}
	ps := h.policyEvents()
	if len(ps) != 1 || ps[0].Tip != mergeTip || ps[0].Target != "main" || !reflect.DeepEqual(ps[0].Applied, []string{"merge.exclude", "merge.require"}) || len(ps[0].Refused) != 0 {
		t.Fatalf("policy events = %+v", ps)
	}
	if n := h.count(KindAction, ActEscalate, ""); n != 0 {
		t.Fatalf("escalations = %d", n)
	}
	eff, _ := effectivePolicy(h.cfg, h.sup.st, "widgets")
	if !reflect.DeepEqual(eff.Merge.Exclude.Labels, []string{"agentd"}) || !reflect.DeepEqual(eff.Merge.Require, []string{RequireGate, RequireCI}) {
		t.Fatalf("effective = %+v", eff.Merge)
	}
	if h.cfg.Repositories["widgets"].Policy.Merge.Exclude.Labels != nil {
		t.Fatal("narrowing changed the owner's policy")
	}
	sameAfterReplay(t, h, "after the policy file")
}

// A key that would widen the policy is refused and goes to the owner once,
// as an escalation naming it; the keys that narrow still apply.
func TestPolicyFileWideningRefused(t *testing.T) {
	h, api := mergeHarness(t)
	api.set(func() {
		api.gitlab[mergePolicyPath+"|ref="+mergeTip] = `{"merge": {"authors": ["widget-bot", "stranger"], "exclude": {"labels": ["hold"]}}, "gate": {"argv": ["true"]}}`
	})
	h.ticks(8, 5*time.Minute)
	ps := h.policyEvents()
	if len(ps) != 1 || !reflect.DeepEqual(ps[0].Refused, []string{"gate.argv", "merge.authors"}) || !reflect.DeepEqual(ps[0].Applied, []string{"merge.exclude"}) {
		t.Fatalf("policy events = %+v", ps)
	}
	var esc []string
	for _, e := range h.events() {
		if e.Kind == KindAction && actionOf(e) == ActEscalate && strings.HasPrefix(e.Act.Key, "policy:") {
			esc = append(esc, e.Act.Message)
		}
	}
	if len(esc) != 1 || !strings.Contains(esc[0], "gate.argv, merge.authors") {
		t.Fatalf("escalations = %q", esc)
	}
	// The owner's authors stand, and the request has no hold label.
	if len(h.forge.merges) != 1 {
		t.Fatalf("merges = %+v, skips %q", h.forge.merges, h.mergeSkips())
	}
}

// What a repository file may and may not do to each kind of key.
func TestNarrowPolicy(t *testing.T) {
	owner := RepoPolicy{
		Delivery: DeliveryRules{Allow: []string{"push", "open"}},
		Gate:     GateRules{Argv: []string{"./bin/check"}, Timeout: Duration{DefaultGateTimeout}},
		Merge:    MergeRules{Enabled: true, Method: MergeFastForward, Targets: []string{"main", "next"}, Authors: []string{"bot"}, Require: []string{RequireGate}},
		Review:   ReviewRules{Rounds: RoundsRules{Max: 3}},
		Sessions: SessionRules{Accounts: []string{"a", "b"}, Max: 4, Budget: BudgetRules{SessionTokens: 1000}},
	}
	for file, want := range map[string]bool{
		`{"delivery": {"allow": ["open"]}}`:                                     false,
		`{"merge": {"enabled": false}}`:                                         true,
		`{"merge": {"targets": ["main"]}}`:                                      true,
		`{"merge": {"targets": ["main", "other"]}}`:                             false,
		`{"merge": {"method": "squash"}}`:                                       false,
		`{"merge": {"method": "fast-forward"}}`:                                 true,
		`{"merge": {"approvals": ["lead"]}}`:                                    true,
		`{"merge": {"require": ["review"]}}`:                                    true,
		`{"merge": {"require": ["luck"]}}`:                                      false,
		`{"merge": {"unknown": 1}}`:                                             false,
		`{"review": {"rounds": {"max": 2}}}`:                                    true,
		`{"review": {"rounds": {"max": 5}}}`:                                    false,
		`{"review": {"rounds": {"max": 50}}}`:                                   false,
		`{"MERGE": {"enabled": false}}`:                                         false,
		`{"merge": {"Enabled": false}}`:                                         false,
		`{"gate": {"env": []}}`:                                                 true,
		`{"review": {"others": "review-only"}}`:                                 true,
		`{"ci": {"required": true}}`:                                            true,
		`{"scope": {"reserved": {"numbers": [9]}}}`:                             true,
		`{"sessions": {"accounts": ["a"]}}`:                                     true,
		`{"sessions": {"accounts": ["c"]}}`:                                     false,
		`{"sessions": {"max": 2}}`:                                              true,
		`{"sessions": {"max": 9}}`:                                              false,
		`{"sessions": {"budget": {"session_tokens": 500, "run_tokens": 9000}}}`: true,
		`{"sessions": {"budget": {"session_tokens": 5000}}}`:                    false,
		`{"sessions": {"models": ["haiku"]}}`:                                   false,
		`{"confidential_patterns": ["secret-[0-9]+"]}`:                          true,
		`{"confidential_patterns": ["("]}`:                                      false,
		`{"gate": {"timeout": "1m"}}`:                                           false,
	} {
		eff, applied, refused := narrowPolicy(owner, []byte(file))
		if got := len(applied) == 1 && len(refused) == 0; got != want {
			t.Errorf("%s: applied %v, refused %v", file, applied, refused)
		}
		if !want && !reflect.DeepEqual(eff, clonePolicy(owner)) {
			t.Errorf("%s: a refused key changed the policy: %+v", file, eff)
		}
	}
	if _, _, refused := narrowPolicy(owner, []byte(`{"merge":`)); !reflect.DeepEqual(refused, []string{"file"}) {
		t.Errorf("torn file refused %v", refused)
	}
	if owner.Merge.Approvals != nil || len(owner.Merge.Targets) != 2 {
		t.Error("narrowing changed the owner's policy")
	}
}

// start_session refuses an issue the policy reserves.
func TestStartSessionRefusesReserved(t *testing.T) {
	h := sessionsHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Scope.Reserved.Numbers = []int{7}
	h.cfg.Repositories["widgets"] = w
	reply, refused := h.startSession(map[string]any{"branch": "fix-7", "issue": 7})
	if r, _ := reply["refused"].(string); !refused || !strings.Contains(r, "reserved") {
		t.Fatalf("reply = %v, refused %v", reply, refused)
	}
	if _, refused := h.startSession(map[string]any{"branch": "fix-8", "issue": 8}); refused {
		t.Fatal("an issue not reserved was refused")
	}
}

// The coordinator's pack carries the effective policy, without the
// confidential patterns.
func TestPackCarriesPolicy(t *testing.T) {
	h := coordHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Merge = MergeRules{Enabled: true, Method: MergeSquash, Targets: []string{"main"}, Authors: []string{"widget-bot"}}
	w.Policy.ConfidentialPatterns = []string{"secret-[0-9]+"}
	h.cfg.Repositories["widgets"] = w
	h.sup.st.applyPolicy(Event{Policy: &PolicyPayload{Repository: "widgets", Target: "main", Tip: "feed1234", File: json.RawMessage(`{"merge":{"exclude":{"numbers":[12]}}}`), Applied: []string{"merge.exclude"}}})
	p, b := h.sup.buildPack("widgets", nil, h.clock.Now(), false)
	if p.Policy.Merge.Method != MergeSquash || !reflect.DeepEqual(p.Policy.Merge.Exclude.Numbers, []int{12}) {
		t.Fatalf("pack policy = %+v", p.Policy.Merge)
	}
	if !bytes.Contains(b, []byte(`"policy":{`)) || bytes.Contains(b, []byte("secret-")) {
		t.Fatalf("pack = %s", b)
	}
}

// agentd policy show prints each key of the effective policy with where it
// came from: the owner's configuration, a default, or the repository's file.
func TestPolicyShowAttributesEachKey(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	path := filepath.Join(dir, "c.json")
	writeFile(t, path, `{"event_log": "`+logPath+`", "repositories": {"widgets": {"forge": "gitlab", "project": "example-org/widgets", "policy": {"gate": {"argv": ["make"]}, "merge": {"enabled": true, "targets": ["main"], "authors": ["widget-bot"]}}}}}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	log, err := OpenLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	p := &PolicyPayload{Repository: "widgets", Target: "main", Tip: "feed1234abcd", Hash: "h1", File: json.RawMessage(`{"merge":{"targets":["main"]}}`), Applied: []string{"merge.targets"}}
	if err := log.Append(Event{V: EventVersion, Time: time.Now(), Kind: KindPolicy, Mode: ModeShared, Policy: p}); err != nil {
		t.Fatal(err)
	}
	log.Close()
	_ = cfg
	var out bytes.Buffer
	if err := policyCmd([]string{"--config", path, "show", "widgets"}, &out); err != nil {
		t.Fatal(err)
	}
	var v PolicyView
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	src := map[string]string{}
	for _, k := range v.Keys {
		src[k.Key] = k.Source
	}
	for key, want := range map[string]string{
		"merge.authors": "owner", "gate.argv": "owner", "merge.method": "default", "gate.timeout": "default",
		"merge.targets": "repository .agentd/policy.json on main at feed1234",
	} {
		if !strings.HasPrefix(src[key], want) {
			t.Errorf("%s from %q, want %q", key, src[key], want)
		}
	}
	if len(v.Files) != 1 || v.Files[0].Tip != "feed1234abcd" {
		t.Fatalf("files = %+v", v.Files)
	}
	if err := policyCmd([]string{"--config", path, "show", "nowhere"}, &out); err == nil {
		t.Fatal("an unknown repository was shown")
	}
}

// A repository's budget caps each of its sessions and all of them,
// leaving other repositories' sessions alone.
func TestRepositoryBudget(t *testing.T) {
	h := newHarness(t, false, "a", "b", "c")
	h.cfg.TranscriptDir = filepath.Join(h.dir, "projects")
	c := h.cfg.Sessions["c"]
	c.Repository = "readonly"
	h.cfg.Sessions["c"] = c
	w := h.cfg.Repositories["widgets"]
	w.Policy.Sessions.Budget = BudgetRules{SessionTokens: 1000, RunTokens: 1500}
	h.cfg.Repositories["widgets"] = w
	h.ticks(1, time.Minute)
	for n, spend := range map[string]int64{"a": 800, "b": 800, "c": 5000} {
		id := h.sup.Tracks()[n].Transcript
		appendFile(t, filepath.Join(TranscriptDir(h.cfg.TranscriptDir, h.cfg.Sessions[n].Cwd), id+".jsonl"), usageLine("m", spend, 0, 0, 0, false))
	}
	h.ticks(3, time.Minute)
	if h.count(KindAction, ActStop, "a") != 1 || h.count(KindAction, ActStop, "b") != 1 || h.count(KindAction, ActStop, "c") != 0 ||
		!strings.Contains(h.sup.Tracks()["a"].Stopped, "widgets's sessions' spend") {
		t.Fatalf("stops a %d, b %d, c %d, why %q", h.count(KindAction, ActStop, "a"), h.count(KindAction, ActStop, "b"), h.count(KindAction, ActStop, "c"), h.sup.Tracks()["a"].Stopped)
	}

	h2 := newHarness(t, false, "a")
	h2.cfg.TranscriptDir = filepath.Join(h2.dir, "projects")
	w = h2.cfg.Repositories["widgets"]
	w.Policy.Sessions.Budget = BudgetRules{SessionTokens: 1000}
	h2.cfg.Repositories["widgets"] = w
	h2.ticks(1, time.Minute)
	id := h2.sup.Tracks()["a"].Transcript
	appendFile(t, filepath.Join(TranscriptDir(h2.cfg.TranscriptDir, h2.cfg.Sessions["a"].Cwd), id+".jsonl"), usageLine("m", 1200, 0, 0, 0, false))
	h2.ticks(3, time.Minute)
	if !strings.Contains(h2.sup.Tracks()["a"].Stopped, "widgets's cap of 1000 for each session") {
		t.Fatalf("why %q", h2.sup.Tracks()["a"].Stopped)
	}
}

// Every repository's confidential patterns, and those its own file adds,
// screen what agentd logs; a repository's review rounds cap its cases.
func TestRepositoryPatternsAndRounds(t *testing.T) {
	h := newHarness(t, false, "a")
	w := h.cfg.Repositories["readonly"]
	w.Policy.ConfidentialPatterns = []string{"hush-[0-9]+"}
	w.Policy.Review.Rounds.Max = 7
	h.cfg.Repositories["readonly"] = w
	h.sup = NewSupervisor(h.cfg, NewLogWriter(h.buf), h.clock, h.tmux, h.git, h.forge, h.runner, false)
	if got := h.sup.withheld("a hush-12 b"); !strings.Contains(got, "withheld") {
		t.Fatalf("configured pattern: %q", got)
	}
	if got := h.sup.withheld("a sekrit-12 b"); strings.Contains(got, "withheld") {
		t.Fatalf("withheld before the file: %q", got)
	}
	ww := h.cfg.Repositories["widgets"]
	ww.Policy.Merge.Targets = []string{"main"}
	h.cfg.Repositories["widgets"] = ww
	h.sup.st.applyPolicy(Event{Policy: &PolicyPayload{Repository: "widgets", Target: "main", Tip: "t", File: json.RawMessage(`{"confidential_patterns":["sekrit-[0-9]+"],"review":{"rounds":{"max":2}}}`), Applied: []string{"confidential_patterns", "review.rounds"}}})
	if got := h.sup.withheld("a sekrit-12 b"); !strings.Contains(got, "withheld") {
		t.Fatalf("the file's pattern: %q", got)
	}
	if got := h.cfg.roundsFor(h.sup.st, repoKey("gitlab", "example-org/readonly")).Max; got != 7 {
		t.Fatalf("readonly rounds = %d", got)
	}
	// The file lowers widgets' rounds below policies.rounds.max.
	if got := h.cfg.roundsFor(h.sup.st, repoKey("gitlab", "example-org/widgets")).Max; got != 2 {
		t.Fatalf("widgets rounds = %d", got)
	}
}

// A file that turned merge off, or required more, is read again at every
// round, so once it changes the merge goes ahead.
func TestPolicyFileLiftedIsReadAgain(t *testing.T) {
	for name, body := range map[string]string{"off": `{"merge":{"enabled":false}}`, "review": `{"merge":{"require":["review"]}}`} {
		t.Run(name, func(t *testing.T) {
			h, api := mergeHarness(t)
			api.set(func() { api.gitlab[mergePolicyPath+"|ref="+mergeTip] = body })
			h.ticks(4, 5*time.Minute)
			if len(h.forge.merges) != 0 {
				t.Fatalf("merged under %s", body)
			}
			api.set(func() { api.gitlab[mergePolicyPath+"|ref="+mergeTip] = `{}` })
			h.ticks(4, 5*time.Minute)
			if len(h.forge.merges) != 1 || len(h.policyEvents()) != 2 {
				t.Fatalf("merges = %+v, policy events %+v, skips %q", h.forge.merges, h.policyEvents(), h.mergeSkips())
			}
		})
	}
}

// A file that matches a confidential pattern cannot be read: merges into
// its target are off, the owner hears once, and nothing of the file is
// logged. A pattern the file itself adds never withholds it.
func TestPolicyFileWithheld(t *testing.T) {
	h, api := mergeHarness(t)
	h.cfg.ConfidentialPatterns = []string{"hush-[0-9]+"}
	h.sup = NewSupervisor(h.cfg, NewLogWriter(h.buf), h.clock, h.tmux, h.git, h.forge, h.runner, false)
	h.sup.home = h.dir
	h.sup.api = api
	api.set(func() { api.gitlab[mergePolicyPath+"|ref="+mergeTip] = `{"merge":{"exclude":{"labels":["hush-1"]}}}` })
	h.ticks(6, 5*time.Minute)
	ps := h.policyEvents()
	if len(h.forge.merges) != 0 || len(ps) != 1 || ps[0].Withheld != 1 || ps[0].File != nil || strings.Contains(h.buf.String(), "hush-1") {
		t.Fatalf("merges %+v, policy events %+v", h.forge.merges, ps)
	}
	if s := h.mergeSkips(); len(s) != 1 || !strings.Contains(s[0], "turns merge off") {
		t.Fatalf("skips = %q", s)
	}
	if n := h.count(KindAction, ActEscalate, ""); n != 1 {
		t.Fatalf("escalations = %d", n)
	}
	// The pattern removed, the same file is read at the next check.
	h.cfg.ConfidentialPatterns = nil
	h.restart(false)
	h.sup.api = api
	h.ticks(6, 5*time.Minute)
	if ps := h.policyEvents(); len(h.forge.merges) != 1 || len(ps) != 2 || ps[1].Withheld != 0 {
		t.Fatalf("after the pattern went: merges %+v, policy events %+v", h.forge.merges, ps)
	}

	h, api = mergeHarness(t)
	api.set(func() {
		api.gitlab[mergePolicyPath+"|ref="+mergeTip] = `{"merge":{"exclude":{"labels":["agentd"]}},"confidential_patterns":["agentd"]}`
	})
	h.ticks(8, 5*time.Minute)
	if len(h.forge.merges) != 0 || len(h.policyEvents()) != 1 || h.policyEvents()[0].Withheld != 0 {
		t.Fatalf("a file withheld itself: merges %+v, policy events %+v", h.forge.merges, h.policyEvents())
	}
}

// Each target's file is recorded once per file, and a merge reads its own
// target's file alone: release's file does not hold main's merges, while
// what sessions read is narrowed by both. A target outside the owner's
// targets is not read at all.
func TestPolicyFilePerTarget(t *testing.T) {
	h, api := mergeHarness(t)
	setMerge(t, h, func(m *MergeRules) { m.Targets = []string{"main", "release"} })
	api.set(func() { api.gitlab[mergePolicyPath+"|ref=0ther"] = `{"merge":{"enabled":false}}` })
	h.sup.notePolicy("d", h.sup.track("d"), "widgets", "release", "0ther", []byte(`{"merge":{"enabled":false}}`))
	h.ticks(6, 5*time.Minute)
	h.sup.notePolicy("d", h.sup.track("d"), "widgets", "release", "0ther", []byte(`{"merge":{"enabled":false}}`))
	if len(h.forge.merges) != 1 || len(h.policyEvents()) != 2 {
		t.Fatalf("merges %+v, policy events %+v", h.forge.merges, h.policyEvents())
	}
	if effectiveFor(h.cfg, h.sup.st, "widgets", "release").Merge.Enabled || !effectiveFor(h.cfg, h.sup.st, "widgets", "main").Merge.Enabled {
		t.Fatal("per-target policies mixed")
	}
	// What sessions read is narrowed by every file.
	if eff, files := effectivePolicy(h.cfg, h.sup.st, "widgets"); eff.Merge.Enabled || len(files) != 2 {
		t.Fatalf("effective = %+v, files %d", eff.Merge, len(files))
	}
}

// GitHub: an approval counts on the head, while it is its reviewer's
// latest say.
func TestMergeGitHubApprovals(t *testing.T) {
	reviews := "/repos/example-org/gadgets/pulls/4/reviews?per_page=100"
	api := &fakeForgeAPI{github: map[string]string{
		"/repos/example-org/gadgets/pulls/4": `{"state":"open","mergeable_state":"clean","user":{"login":"widget-bot"},"labels":[{"name":"agentd"}],"head":{"sha":"abc1234def"},"base":{"ref":"main"}}`,
		reviews: `[{"state":"APPROVED","commit_id":"abc1234def","user":{"login":"lead"}},
			{"state":"APPROVED","commit_id":"0ld","user":{"login":"stale"}},
			{"state":"APPROVED","commit_id":"abc1234def","user":{"login":"fickle"}},
			{"state":"CHANGES_REQUESTED","commit_id":"abc1234def","user":{"login":"fickle"}},
			{"state":"COMMENTED","commit_id":"abc1234def","user":{"login":"lead"}}]`,
	}}
	v, err := githubMergeView(api, "example-org/gadgets", "#4")
	if err != nil || v.author != "widget-bot" || !reflect.DeepEqual(v.labels, []string{"agentd"}) {
		t.Fatalf("view = %+v, %v", v, err)
	}
	if got, err := v.approvers(v.head); err != nil || !reflect.DeepEqual(got, []string{"lead"}) {
		t.Fatalf("approvers = %v, %v", got, err)
	}
}
