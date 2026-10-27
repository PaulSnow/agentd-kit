package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeService is a target's version endpoint and a fleet's heartbeats.
type fakeService struct {
	mu      sync.Mutex
	version string
	beats   []map[string]any
}

func (f *fakeService) set(version string, beats ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version = version
	if beats != nil {
		f.beats = beats
	}
}

func (f *fakeService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/version":
		json.NewEncoder(w).Encode(map[string]any{"version": f.version})
	case "/api/heartbeats":
		json.NewEncoder(w).Encode(map[string]any{"heartbeats": f.beats})
	default:
		http.NotFound(w, r)
	}
}

func beat(member, version string) map[string]any {
	return map[string]any{"member": member, "version": version, "last_seen": "2026-10-22T00:00:00Z"}
}

// releaseHarness is a harness whose widgets deploys a service over ssh
// from host-a's node and a fleet of three, behind a gate.
func releaseHarness(t *testing.T) (*harness, *fakeService) {
	t.Helper()
	h := newHarness(t, false)
	svc := &fakeService{version: "v1.0.0", beats: []map[string]any{beat("op-a", "v1.0.0"), beat("op-b", "v1.0.0"), beat("op-c", "v1.0.0")}}
	srv := httptest.NewServer(svc)
	t.Cleanup(srv.Close)
	h.cfg.Hosts = map[string]HostConfig{"host-a": {}}
	r := h.cfg.Repositories["widgets"]
	r.Deploy = &DeployConfig{Clone: h.dir, Gate: ReleaseGateRules{Argv: []string{"scripts/parity.sh", "{tag}"}}, Targets: map[string]DeployTarget{
		"browser": {Host: "host-b", Via: ViaSSH, Node: "host-a", Check: DeployCheck{URL: srv.URL + "/api/version", JSONPath: "version"},
			Steps: []DeployStep{{Name: "install", Run: []string{"deploy", "{tag}"}, Rollback: []string{"rollback"}}}},
		"signers": {Kind: TargetFleet, Heartbeats: srv.URL + "/api/heartbeats", Threshold: 2},
	}}
	var errs []error
	h.cfg.checkDeploy("widgets", &r, func(_, p string) string { return p }, &errs)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	h.cfg.Repositories["widgets"] = r
	h.git.tags = []string{"v1.0.0"}
	return h, svc
}

// tag makes a new release tag and runs passes until its item opens.
func (h *harness) tag(tag, notes string) *Release {
	h.t.Helper()
	h.git.mu.Lock()
	h.git.tags = append(h.git.tags, tag)
	if h.git.tagNotes == nil {
		h.git.tagNotes = map[string]string{}
	}
	h.git.tagNotes[tag] = notes
	h.git.mu.Unlock()
	for i := 0; i < 3; i++ {
		h.ticks(2, h.cfg.Poll.Every.Duration)
		if r := h.sup.st.Releases[releaseKey("widgets", tag)]; r != nil {
			return r
		}
	}
	h.t.Fatalf("no release item opened for %s", tag)
	return nil
}

func (h *harness) releaseSteps(step string) []ReleasePayload {
	var out []ReleasePayload
	for _, e := range h.events() {
		if e.Kind == KindRelease && e.Release.Step == step {
			out = append(out, *e.Release)
		}
	}
	return out
}

func (h *harness) messages(text string) []string {
	var out []string
	for _, e := range h.events() {
		if e.Kind == KindMessage && strings.Contains(e.Message.Text, text) {
			out = append(out, e.Message.Text)
		}
	}
	return out
}

// stepDone logs host-a's node's result of a release step.
func (h *harness) stepResult(tag string, index, attempt int, outcome string, seq int64) {
	id := deployStepID("widgets", tag, "browser", index, attempt)
	h.sup.emit("", Event{Kind: KindNode, Mode: ModeShared, Node: &NodePayload{Host: "host-a", Seq: seq, At: h.clock.Now(), Result: &NodeResult{
		Command: id, Action: NodeDeployStep, Repository: "widgets", Path: "gitlab.example.com/example-org/widgets", Outcome: outcome, Error: map[bool]string{true: "step 1 (install): the run failed"}[outcome != NodeDone],
		Deploy: &DeployStepResult{Tag: tag, Target: "browser", Index: index, Attempt: attempt, Name: "install", RolledBack: outcome != NodeDone}}}})
}

func TestReleaseClosesOnlyWhenServiceAndFleetHold(t *testing.T) {
	h, svc := releaseHarness(t)
	every := h.cfg.Poll.Every.Duration
	h.ticks(2, every)
	if seen := h.releaseSteps(ReleaseSeen); len(seen) != 1 || len(seen[0].Tags) != 1 || len(h.sup.st.Releases) != 0 {
		t.Fatalf("the tags there at first are seen and open nothing: %+v, %d items", seen, len(h.sup.st.Releases))
	}
	r := h.tag("v1.1.0", "Release v1.1.0\n\ncompat: browser and signers together\n\x00")
	if r.Compat != "browser and signers together" {
		t.Fatalf("compat = %q", r.Compat)
	}
	h.ticks(3, every)
	if r.State != ReleaseDeploying || len(h.runner.gates) != 1 || h.runner.gates[0].argv[1] != "v1.1.0" || len(h.git.tagCopies) != 1 || h.git.removed != 1 {
		t.Fatalf("the gate runs once on a copy at the tag, then the item deploys: state %s, gates %+v, copies %v", r.State, h.runner.gates, h.git.tagCopies)
	}
	if g := h.releaseSteps(ReleaseGate); len(g) != 1 || g[0].Gate.Output != "gate output" {
		t.Fatalf("the gate's output is attached to the item: %+v", g)
	}
	// The item is among the open work items, with its fleet-shaped done_when.
	none := func(...string) bool { return false }
	if vs := WorkViews(h.cfg, h.sup.st, "widgets", false, h.clock.Now(), none); len(vs) != 1 || vs[0].ID != "release:v1.1.0" || vs[0].State != ReleaseDeploying ||
		!strings.Contains(vs[0].DoneWhen, "signers: at least 2 members' heartbeats on v1.1.0") || !strings.Contains(vs[0].DoneWhen, `version is "v1.1.0"`) ||
		!strings.Contains(vs[0].Progress, "compat: browser and signers together") || vs[0].Entry != "" {
		t.Fatalf("views = %+v", vs)
	}
	// The hub asks host-a's node for the first step.
	cmds := deployCommands(h.cfg, h.sup.st, newNodeRecord("host-a"))
	if len(cmds) != 1 || cmds[0].Deploy.SSH != "host-b" || strings.Join(cmds[0].Deploy.Run, " ") != "deploy v1.1.0" {
		t.Fatalf("commands = %+v", cmds)
	}
	if ops := h.messages("For the operators of widgets's signers"); len(ops) != 1 || !strings.Contains(ops[0], "Not on v1.1.0 yet: op-a, op-b, op-c") ||
		!strings.Contains(ops[0], "browser and signers together") {
		t.Fatalf("the operators are asked once, with the members behind and the compat rule: %q", ops)
	}
	// The step is done and the service runs the tag, but the fleet does not.
	h.stepResult("v1.1.0", 0, 0, NodeDone, 1)
	svc.set("v1.1.0", beat("op-a", "v1.1.0"), beat("op-b", "v1.0.0"), beat("op-c", "v1.0.0"))
	h.ticks(3, every)
	if r.State == ReleaseDone || !r.Targets["browser"].OK || r.Targets["browser"].StepsDone != 1 || r.Targets["signers"].OK {
		t.Fatalf("the service alone does not close the item: %s, %+v %+v", r.State, r.Targets["browser"], r.Targets["signers"])
	}
	if b := r.Targets["signers"].Behind; strings.Join(b, ",") != "op-b,op-c" {
		t.Fatalf("behind = %v", b)
	}
	// Past escalate_after the owner is told who is behind, once, and the
	// fleet is not claimed done.
	h.ticks(1, DefaultFleetEscalate)
	h.ticks(3, every)
	if esc := h.messages("is not running everywhere yet"); len(esc) != 1 || !strings.Contains(esc[0], "op-b, op-c") || r.State == ReleaseDone {
		t.Fatalf("escalation = %q, state %s", esc, r.State)
	}
	// The fleet reaches its threshold, but the service went back: still open.
	svc.set("v1.0.0", beat("op-a", "v1.1.0"), beat("op-b", "v1.1.0"), beat("op-c", "v1.0.0"))
	h.ticks(3, every)
	if r.State == ReleaseDone || r.Targets["browser"].OK || !r.Targets["signers"].OK {
		t.Fatalf("the fleet alone does not close the item: %s", r.State)
	}
	svc.set("v1.1.0")
	h.ticks(3, every)
	if r.State != ReleaseDone || len(h.releaseSteps(ReleaseRunning)) != 1 || len(h.messages("is running where it must")) != 1 {
		t.Fatalf("both checks hold: the item is done once: %s", r.State)
	}
	if vs := WorkViews(h.cfg, h.sup.st, "widgets", false, h.clock.Now(), none); len(vs) != 0 {
		t.Fatalf("a done release is still open: %+v", vs)
	}
	if len(h.messages("For the operators")) != 1 {
		t.Fatal("the operators were asked more than once")
	}
}

func TestFleetPollMarksMixed(t *testing.T) {
	h, svc := releaseHarness(t)
	svc.set("v1.0.0", beat("op-a", "v1.1.0"), beat("op-b", "v1.0.0"), beat("op-b", "v1.0.0"))
	h.ticks(2, time.Minute)
	f := h.sup.st.Fleets[fleetKey("widgets", "signers")]
	if f == nil || !f.Mixed || len(f.Members) != 2 || f.versions()["v1.1.0"] != 1 {
		t.Fatalf("fleet = %+v", f)
	}
	var card *Card
	for _, e := range h.events() {
		if e.Kind == KindCard && e.Card.Topic == "fleet:signers" {
			card = e.Card.Card
		}
	}
	if card == nil || card.Kind != CardFleet || !strings.HasPrefix(card.Status, "mixed: ") || !strings.Contains(card.Summary, "op-a v1.1.0") || cardKindOf(card.Topic) != CardFleet {
		t.Fatalf("card = %+v", card)
	}
	// An unchanged reading is not logged again.
	n := h.count(KindFleet, "", "")
	h.ticks(2, h.cfg.Poll.Every.Duration)
	if h.count(KindFleet, "", "") != n {
		t.Fatal("an unchanged reading was logged again")
	}
	fleets, _ := h.sup.fleetFacts("widgets", func(s string) string { return s })
	if len(fleets) != 1 || !fleets[0].Mixed || fleets[0].Threshold != 2 || len(fleets[0].Members) != 2 {
		t.Fatalf("the thread agent's facts = %+v", fleets)
	}
	// One fleet on one version is uniform.
	svc.set("v1.0.0", beat("op-a", "v1.0.0"), beat("op-b", "v1.0.0"))
	h.ticks(2, h.cfg.Poll.Every.Duration)
	if f := h.sup.st.Fleets[fleetKey("widgets", "signers")]; f.Mixed {
		t.Fatalf("fleet = %+v", f)
	}
}

func TestParseHeartbeats(t *testing.T) {
	for _, tc := range []struct {
		body string
		f    HeartbeatFields
		want string
	}{
		{`[{"name":"a","version":"v1","lastSeen":1761091200}]`, HeartbeatFields{}, "a v1 2025-10-22T00:00:00Z"},
		{`{"signers":{"a":{"version":"v2","last_seen":"2026-10-22T00:00:00Z"}}}`, HeartbeatFields{}, "a v2 2026-10-22T00:00:00Z"},
		{`{"rows":[{"op":"a","ver":"v3","t":"2026-10-22T00:00:00Z"},{"op":"a","ver":"v2","t":"2026-10-21T00:00:00Z"}]}`, HeartbeatFields{List: "rows", Member: "op", Version: "ver", Seen: "t"}, "a v3 2026-10-22T00:00:00Z"},
	} {
		ms, err := parseHeartbeats([]byte(tc.body), tc.f)
		if err != nil || len(ms) != 1 || fmt.Sprintf("%s %s %s", ms[0].Member, ms[0].Version, ms[0].LastSeen.Format(time.RFC3339)) != tc.want {
			t.Errorf("%s: %+v %v", tc.body, ms, err)
		}
	}
	if _, err := parseHeartbeats([]byte(`{"ok":true}`), HeartbeatFields{}); err == nil {
		t.Error("a body with no members was read")
	}
	if !versionIs("3.38.2", "v3.38.2") || !versionIs("v3.38.2+abc", "v3.38.2") || versionIs("v3.38.20", "v3.38.2") {
		t.Error("versionIs")
	}
}

func TestReleaseGateFailureWithdraws(t *testing.T) {
	h, _ := releaseHarness(t)
	every := h.cfg.Poll.Every.Duration
	h.runner.gateExit = 3
	h.ticks(2, every)
	r := h.tag("v1.1.0", "")
	h.ticks(3, every)
	if r.State != ReleaseOut || len(h.releaseSteps(ReleaseWithdrawn)) != 1 {
		t.Fatalf("state = %s", r.State)
	}
	if told := h.messages("is withdrawn: its gate (scripts/parity.sh {tag}) failed, exit 3"); len(told) != 1 || !strings.Contains(told[0], "Nothing deploys it") {
		t.Fatalf("the owner is told: %q", told)
	}
	if cmds := deployCommands(h.cfg, h.sup.st, newNodeRecord("host-a")); len(cmds) != 0 {
		t.Fatalf("a withdrawn tag deploys: %+v", cmds)
	}
	if len(h.messages("For the operators")) != 0 || len(h.releaseSteps(ReleaseCheck)) != 0 || len(h.runner.gates) != 1 {
		t.Fatal("a withdrawn tag went past built")
	}
}

func TestReleaseStepFailureBlocksUntilRetry(t *testing.T) {
	h, _ := releaseHarness(t)
	every := h.cfg.Poll.Every.Duration
	h.ticks(2, every)
	r := h.tag("v1.1.0", "")
	h.ticks(2, every)
	h.stepResult("v1.1.0", 0, 0, NodeFailed, 1)
	h.ticks(2, every)
	if r.State != ReleaseBlocked || len(h.messages("is blocked: browser's step 1 (install) failed")) != 1 {
		t.Fatalf("state = %s, %q", r.State, h.messages("blocked"))
	}
	rec := newNodeRecord("host-a")
	for _, e := range h.events() {
		if e.Kind == KindNode {
			rec.apply(e)
		}
	}
	if cmds := deployCommands(h.cfg, h.sup.st, rec); len(cmds) != 0 {
		t.Fatalf("a failed step is asked again without the owner: %+v", cmds)
	}
	retry := Event{V: EventVersion, Time: h.clock.Now(), Kind: KindRelease, Mode: ModeOwner, Release: &ReleasePayload{Repository: "widgets", Tag: "v1.1.0", Step: ReleaseRetry}}
	if err := retry.check(); err != nil {
		t.Fatal(err)
	}
	h.sup.st.apply(retry)
	cmds := deployCommands(h.cfg, h.sup.st, rec)
	if r.State != ReleaseDeploying || len(cmds) != 1 || cmds[0].Deploy.Attempt != 1 {
		t.Fatalf("the owner's retry asks for the step again: %s %+v", r.State, cmds)
	}
}

func TestSSHTargetRefusedWithoutNode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Hosts = map[string]HostConfig{"host-a": {}}
	cfg.Node.Name = "host-a"
	check := func(t DeployTarget) []error {
		r := Repository{Deploy: &DeployConfig{Clone: "/clone", Targets: map[string]DeployTarget{"browser": t}}}
		var errs []error
		cfg.checkDeploy("widgets", &r, func(_, p string) string { return p }, &errs)
		return errs
	}
	ok := DeployTarget{Host: "host-b", Via: ViaSSH, Node: "host-a", OwnHost: true, Check: DeployCheck{URL: "https://example.com/api/version", JSONPath: "version"}}
	if errs := check(ok); len(errs) != 0 {
		t.Fatal(errs)
	}
	for name, mod := range map[string]func(*DeployTarget){
		"no node":         func(t *DeployTarget) { t.Node = "" },
		"node not a host": func(t *DeployTarget) { t.Node = "laptop" },
		"agentd's host":   func(t *DeployTarget) { t.OwnHost = false },
		"node target":     func(t *DeployTarget) { t.Via, t.Host = ViaNode, "host-b" },
	} {
		tt := ok
		mod(&tt)
		if errs := check(tt); len(errs) != 1 || !strings.Contains(errs[0].Error(), "node") && !strings.Contains(errs[0].Error(), "host") {
			t.Errorf("%s: %v", name, errs)
		}
	}
	// The node holds no ssh access to host-b: it refuses the step.
	var ran [][]string
	n := NewNode(NodeConfig{Name: "host-a", Deploy: []string{"widgets/browser"}}, "", nil, nil, nil, time.Now)
	n.run = func(dir string, argv []string, timeout time.Duration) (int, string, error) {
		ran = append(ran, argv)
		return 0, "", nil
	}
	c := NodeCommand{ID: "x", Action: NodeDeployStep, Repository: "widgets", Path: "p",
		Deploy: &DeployStepCommand{Tag: "v1.1.0", Target: "browser", Via: ViaSSH, SSH: "host-b", Run: []string{"deploy", "v1.1.0"}}}
	if res := n.do(context.Background(), c); res.Outcome != NodeRefused || !strings.Contains(res.Error, "no ssh access") || len(ran) != 0 {
		t.Fatalf("result = %+v, ran %v", res, ran)
	}
	n.cfg.SSH = []string{"host-b"}
	c.Deploy.Target = "other"
	if res := n.do(context.Background(), c); res.Outcome != NodeRefused || !strings.Contains(res.Error, "node.deploy") {
		t.Fatalf("result = %+v", res)
	}
	c.Deploy.Target = "browser"
	if res := n.do(context.Background(), c); res.Outcome != NodeDone || len(ran) != 1 || ran[0][0] != "ssh" || ran[0][len(ran[0])-1] != "'deploy' 'v1.1.0'" {
		t.Fatalf("result = %+v, ran %v", res, ran)
	}
}

func TestNodeStepRollsBackAFailedCheck(t *testing.T) {
	var ran []string
	n := NewNode(NodeConfig{Name: "host-b", Deploy: []string{"widgets/browser"}, ReposRoot: t.TempDir()}, "", nil, nil, nil, time.Now)
	n.run = func(dir string, argv []string, timeout time.Duration) (int, string, error) {
		ran = append(ran, argv[0])
		if argv[0] == "check" {
			return 1, "wrong period", nil
		}
		return 0, "", nil
	}
	c := NodeCommand{ID: "x", Action: NodeDeployStep, Repository: "widgets", Path: "p",
		Deploy: &DeployStepCommand{Tag: "v1.1.0", Target: "browser", Via: ViaNode, Name: "install", Run: []string{"run"}, Check: []string{"check"}, Rollback: []string{"rollback"}}}
	res := n.do(context.Background(), c)
	if res.Outcome != NodeFailed || res.Deploy.FailedAt != "check" || !res.Deploy.RolledBack || strings.Join(ran, ",") != "run,check,rollback" || !strings.Contains(res.Deploy.Output, "wrong period") {
		t.Fatalf("result = %+v %+v, ran %v", res, res.Deploy, ran)
	}
}

// The hub asks the node that holds the ssh access for each step in turn,
// from what the log holds, and the node's results land in the log; a
// shadow run's release deploys nothing.
func TestHubDrivesReleaseStepsOnTheNode(t *testing.T) {
	h := newHubHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Deploy = &DeployConfig{Clone: "/clone", Targets: map[string]DeployTarget{"browser": {Host: "host-c", Via: ViaSSH, Node: "host-a",
		Check: DeployCheck{URL: "https://example.com/api/version", JSONPath: "version", Value: "{tag}"},
		Steps: []DeployStep{{Name: "install", Run: []string{"install", "{tag}"}}, {Name: "restart", Run: []string{"restart"}}}}}}
	h.cfg.Repositories["widgets"] = w
	n := h.nodes["host-a"]
	n.cfg.Deploy, n.cfg.SSH = []string{"widgets/browser"}, []string{"host-c"}
	var ran []string
	n.run = func(dir string, argv []string, timeout time.Duration) (int, string, error) {
		ran = append(ran, argv[len(argv)-1])
		return 0, "", nil
	}
	appendRelease := func(mode, step string, gate *ReleaseGateResult) {
		if err := h.log.Append(Event{V: EventVersion, Time: h.now, Kind: KindRelease, Mode: mode, Release: &ReleasePayload{Repository: "widgets", Tag: "v2.0.0", Step: step, Gate: gate}}); err != nil {
			t.Fatal(err)
		}
	}
	appendRelease(ModeShadow, ReleaseOpened, nil)
	appendRelease(ModeShadow, ReleaseGate, &ReleaseGateResult{Passed: true})
	h.tick("host-a")
	h.tick("host-a")
	if len(ran) != 0 {
		t.Fatalf("a shadow run's release deployed: %v", ran)
	}
	appendRelease(ModeLive, ReleaseOpened, nil)
	appendRelease(ModeLive, ReleaseGate, &ReleaseGateResult{Passed: true})
	for i := 0; i < 4; i++ {
		h.tick("host-a")
	}
	if strings.Join(ran, ",") != "'install' 'v2.0.0','restart'" {
		t.Fatalf("ran = %v", ran)
	}
	events, _, err := ReadLog(h.cfg.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	st := NewLogState(h.cfg, ModeLive).Fold(events)
	if r := st.Releases[releaseKey("widgets", "v2.0.0")]; r == nil || r.Targets["browser"].StepsDone != 2 {
		t.Fatalf("release = %+v", r)
	}
}

// The owner opens an item for a tag there before Go read the clone, and
// retries a blocked one, through the log as agentd releases does.
func TestOwnerOpensAndRetriesARelease(t *testing.T) {
	h, _ := releaseHarness(t)
	path := filepath.Join(t.TempDir(), "events.jsonl")
	log, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	h.git.tagNotes = map[string]string{"v1.0.0": "\x00# Changes\n\n## v1.0.0\n\n- compat: signers first\n\n## v0.9.0\n- compat: none\n"}
	if err := OpenRelease(h.cfg, log, nil, h.git, h.clock.Now(), "widgets", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := OpenRelease(h.cfg, log, nil, h.git, h.clock.Now(), "widgets", "nightly"); err == nil {
		t.Fatal("a tag deploy.tags does not match was opened")
	}
	events, _, err := log.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := OpenRelease(h.cfg, log, events, h.git, h.clock.Now(), "widgets", "v1.0.0"); err == nil {
		t.Fatal("an open release was opened again")
	}
	st := NewLogState(h.cfg, ModeLive).Fold(events)
	if r := st.Releases[releaseKey("widgets", "v1.0.0")]; r == nil || r.Compat != "signers first" {
		t.Fatalf("release = %+v", r)
	}
	if err := RetryRelease(h.cfg, log, events, h.clock.Now(), "widgets", "v1.0.0"); err == nil {
		t.Fatal("a release with no failed step was retried")
	}
}
