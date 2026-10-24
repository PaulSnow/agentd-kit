package main

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The target's tip in the merge fixtures, and the policy file's path.
const (
	mergeTip        = "feed1234"
	mergePolicyPath = ciProject + "/repository/files/.agentd%2Fpolicy.json/raw"
)

// mergeHarness is ciHarness with request !1 green on c0ffee, the commit
// agentd's gate passed, widgets allowing merge, and its target branch
// granting a fast-forward merge in its own policy file: a request due to
// merge.
func mergeHarness(t *testing.T) (*harness, *fakeForgeAPI) {
	t.Helper()
	h, api := ciHarness(t)
	repo := h.cfg.Repositories["widgets"]
	repo.Allow = []string{"push", "open", "merge"}
	h.cfg.Repositories["widgets"] = repo
	api.pipelines("c0ffee", "success", nil, "success", nil)
	api.set(func() {
		api.gitlab[ciProject+"/repository/branches/main"] = `{"name":"main","commit":{"id":"` + mergeTip + `"}}`
		api.gitlab[mergePolicyPath+"|ref="+mergeTip] = `{"merge":"fast-forward"}`
		api.gitlab["="+ciProject] = `{"id":7,"merge_method":"ff"}`
		api.gitlab[ciProject+"/repository/merge_base"] = `{"id":"` + mergeTip + `"}`
	})
	return h, api
}

// mergeSkips is the reasons merges were declined, oldest first.
func (h *harness) mergeSkips() []string {
	var out []string
	for _, e := range h.events() {
		if e.Kind == KindSkip && e.Skip.Action == ActMerge {
			out = append(out, e.Skip.Reason)
		}
	}
	return out
}

// A granted, gated, green request that is a fast-forward of its target
// merges, once, through the forge client, and the merge is recorded.
func TestMergeGatedFastForwardMergesOnce(t *testing.T) {
	h, _ := mergeHarness(t)
	h.ticks(6, 5*time.Minute)
	want := []mergedRequest{{ref: "!1", target: "main", head: "c0ffee"}}
	if !reflect.DeepEqual(h.forge.merges, want) {
		t.Fatalf("merges = %+v, want %+v (skips %q)", h.forge.merges, want, h.mergeSkips())
	}
	if n := h.count(KindAction, ActMerge, "d"); n != 1 {
		t.Fatalf("merge actions = %d, want 1", n)
	}
	var m *ActPayload
	for _, e := range h.events() {
		if e.Kind == KindAction && actionOf(e) == ActMerge {
			m = e.Act
		}
	}
	if m.Repository != "widgets" || m.Ref != "!1" || m.Head != "c0ffee" || m.Target != "main" || m.SHA != mergeTip {
		t.Fatalf("merge = %+v", m)
	}
	if got := h.sup.Tracks()["d"].Merges[ciKey("widgets", "!1")]; got != "c0ffee" {
		t.Fatalf("merges recorded = %v", h.sup.Tracks()["d"].Merges)
	}
	sameAfterReplay(t, h, "after the merge")
}

// Shadow mode merges nothing. It opens nothing either, so the owner's
// mark-delivered names the request, and records why it would not merge.
func TestMergeShadowMergesNothing(t *testing.T) {
	h := newHarness(t, true, "d")
	doneWithDelivery(t, h, "d")
	h.cfg.Poll.CI = true
	repo := h.cfg.Repositories["widgets"]
	repo.Allow = []string{"push", "open", "merge"}
	h.cfg.Repositories["widgets"] = repo
	api := &fakeForgeAPI{gitlab: map[string]string{}}
	h.sup.api = api
	if err := MarkDelivered(h.cfg, h.sup.log, h.clock.Now(), "d", h.dir+"/d/wt", "!1"); err != nil {
		t.Fatal(err)
	}
	api.pipelines("c0ffee", "success", nil, "success", nil)
	h.ticks(6, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("shadow merged: %+v", h.forge.merges)
	}
	// The owner's mark-delivered ran no gate: the head is not gated.
	if s := h.mergeSkips(); h.count(KindWould, ActMerge, "d") != 0 || len(s) != 1 || !strings.Contains(s[0], "did not pass agentd's gate") {
		t.Fatalf("would-merge %d, skips %q", h.count(KindWould, ActMerge, "d"), s)
	}
}

// A request whose head moved after the gate, to a commit agentd's gate
// never passed, does not merge, however green CI is on it.
func TestMergeHeadMovedAfterGate(t *testing.T) {
	h, api := mergeHarness(t)
	api.pipelines("beef00", "success", nil, "success", nil)
	h.ticks(6, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merges = %+v", h.forge.merges)
	}
	if s := h.mergeSkips(); len(s) != 1 || !strings.Contains(s[0], "did not pass agentd's gate") {
		t.Fatalf("skips = %q", s)
	}
}

// The head the forge reports when the merge is checked must still be the
// head the CI watcher saw green and agentd's gate passed.
func TestMergeHeadMovedSinceTheRecord(t *testing.T) {
	h, api := mergeHarness(t)
	h.ticks(1, 5*time.Minute)
	h.ticks(1, 5*time.Minute) // the CI record of c0ffee
	api.set(func() {
		// The request moves on before the merge check runs.
		api.gitlab[ciProject+"/merge_requests/1"] = strings.Replace(api.gitlab[ciProject+"/merge_requests/1"], `"sha":"c0ffee"`, `"sha":"beef00"`, 1)
	})
	h.sup.mergeRequest("d", h.sup.track("d"), ciWatched{Session: "d", Repository: "widgets", Ref: "!1", Delivered: "d:" + h.dir + "/d/wt"}, h.sup.st.CI[ciKey("widgets", "!1")])
	if len(h.forge.merges) != 0 {
		t.Fatalf("merges = %+v", h.forge.merges)
	}
	if s := h.mergeSkips(); len(s) == 0 || !strings.Contains(s[len(s)-1], "its head is now beef00") {
		t.Fatalf("skips = %q", s)
	}
}

// A head that is not a fast-forward of the target does not merge.
func TestMergeNotFastForward(t *testing.T) {
	h, api := mergeHarness(t)
	api.set(func() { api.gitlab[ciProject+"/repository/merge_base"] = `{"id":"0ld0ne"}` })
	h.ticks(6, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merges = %+v", h.forge.merges)
	}
	if s := h.mergeSkips(); len(s) != 1 || !strings.Contains(s[0], "not a fast-forward") {
		t.Fatalf("skips = %q", s)
	}
}

// A project whose merge method is not ff never merges: GitLab would make a
// merge commit.
func TestMergeGitLabNeedsFastForwardMethod(t *testing.T) {
	h, api := mergeHarness(t)
	api.set(func() { api.gitlab["="+ciProject] = `{"id":7,"merge_method":"merge"}` })
	h.ticks(6, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merges = %+v", h.forge.merges)
	}
	if s := h.mergeSkips(); len(s) != 1 || !strings.Contains(s[0], `merge method is "merge"`) {
		t.Fatalf("skips = %q", s)
	}
}

// Without the grant, nothing merges: neither where the configuration does
// not allow merge, nor where the target branch's own policy file does not
// grant it.
func TestMergeNeedsBothGrants(t *testing.T) {
	h, api := mergeHarness(t)
	repo := h.cfg.Repositories["widgets"]
	repo.Allow = []string{"push", "open"}
	h.cfg.Repositories["widgets"] = repo
	h.ticks(6, 5*time.Minute)
	if len(h.forge.merges) != 0 || len(h.mergeSkips()) != 0 {
		t.Fatalf("merges = %+v, skips %q", h.forge.merges, h.mergeSkips())
	}
	for _, c := range api.callList() {
		if strings.Contains(c, "policy.json") {
			t.Fatalf("asked the policy file without the configuration's grant: %s", c)
		}
	}

	for name, body := range map[string]string{"absent": fakeNotFound, "other": `{"merge":"squash"}`, "torn": `{"merge":`} {
		t.Run(name, func(t *testing.T) {
			h, api := mergeHarness(t)
			api.set(func() { api.gitlab[mergePolicyPath+"|ref="+mergeTip] = body })
			h.ticks(6, 5*time.Minute)
			if len(h.forge.merges) != 0 {
				t.Fatalf("merges = %+v", h.forge.merges)
			}
			if s := h.mergeSkips(); len(s) != 1 || !strings.Contains(s[0], "does not grant merge") {
				t.Fatalf("skips = %q", s)
			}
		})
	}
}

// The forge client refuses a merge the repository does not allow, even
// when asked directly.
func TestMergeAllowListed(t *testing.T) {
	f := &fakeForge{}
	a := AllowedForge{Repos: map[string]Repository{"widgets": {Forge: "gitlab", Allow: []string{"push", "open"}}}, Forge: f}
	if err := a.MergeAllowed("widgets", "!1", "c0ffee", "main"); err == nil || len(f.merges) != 0 {
		t.Fatalf("merge allowed without the grant: %v %+v", err, f.merges)
	}
}

// A target that moved between the check and the merge does not merge.
func TestMergeTargetMoved(t *testing.T) {
	h, api := mergeHarness(t)
	key := ciProject + "/repository/branches/main"
	api.set(func() {
		// The check sees feed1234; the second look, just before the
		// merge, sees the target moved on.
		api.gitlabSeq = map[string][]string{key: {`{"commit":{"id":"` + mergeTip + `"}}`}}
		api.gitlab[key] = `{"commit":{"id":"a1b2c3d4"}}`
	})
	h.ticks(3, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merges = %+v", h.forge.merges)
	}
	s := h.mergeSkips()
	if len(s) != 1 || !strings.Contains(s[0], "moved from feed1234 to a1b2c3d4") {
		t.Fatalf("skips = %q", s)
	}
}

// An open blocking decision of the session holds the merge.
func TestMergeWaitsForBlockingDecision(t *testing.T) {
	h, _ := mergeHarness(t)
	h.sup.track("d").Raised = []Decision{{ID: "D1", Type: DecisionText, Question: "Ship it?", Blocking: true}}
	h.ticks(3, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merges = %+v", h.forge.merges)
	}
	if s := h.mergeSkips(); len(s) != 1 || !strings.Contains(s[0], "blocking decision") {
		t.Fatalf("skips = %q", s)
	}
}

// CI that is not green on the head holds the merge; a failed merge call is
// retried within the restart policy, then given up on.
func TestMergeNeedsGreenAndGivesUp(t *testing.T) {
	h, api := mergeHarness(t)
	api.pipelines("c0ffee", "running", nil, "success", nil)
	h.ticks(3, 5*time.Minute)
	if len(h.forge.merges) != 0 {
		t.Fatalf("merged on pending CI: %+v", h.forge.merges)
	}
	if s := h.mergeSkips(); len(s) != 1 || !strings.Contains(s[0], "is pending, not green") {
		t.Fatalf("skips = %q", s)
	}
	api.pipelines("c0ffee", "success", nil, "success", nil)
	h.forge.mergeErr = errors.New("merge refused")
	h.ticks(40, 30*time.Minute)
	max := h.cfg.Policies.Restart.MaxRestarts
	if len(h.forge.merges) != max {
		t.Fatalf("merge calls = %d, want %d", len(h.forge.merges), max)
	}
	s := h.mergeSkips()
	if !strings.Contains(s[len(s)-1], "failed") || !strings.Contains(s[len(s)-1], "not retrying") {
		t.Fatalf("skips = %q", s)
	}
}

// GitHub: the pull request's head and base, the base's tip, the policy
// file at it, asked raw, and the compare that says fast-forward.
func TestMergeGitHubView(t *testing.T) {
	api := &fakeForgeAPI{github: map[string]string{
		"/repos/example-org/gadgets/pulls/4":                                     `{"state":"open","merged":false,"html_url":"https://github.com/example-org/gadgets/pull/4","head":{"sha":"abc1234def"},"base":{"ref":"main"}}`,
		"/repos/example-org/gadgets/git/ref/heads/main":                          `{"object":{"sha":"0123456789"}}`,
		"/repos/example-org/gadgets/contents/.agentd/policy.json?ref=0123456789": `{"merge":"fast-forward"}`,
		"/repos/example-org/gadgets/compare/0123456789...abc1234def":             `{"status":"ahead","behind_by":0}`,
	}}
	v, err := githubMergeView(api, "example-org/gadgets", "#4")
	if err != nil || !v.open || v.head != "abc1234def" || v.target != "main" {
		t.Fatalf("view = %+v, %v", v, err)
	}
	tip, err := v.tip("main")
	if err != nil || tip != "0123456789" {
		t.Fatalf("tip = %q, %v", tip, err)
	}
	if b, err := v.policy(tip); err != nil || string(b) != `{"merge":"fast-forward"}` {
		t.Fatalf("policy = %q, %v", b, err)
	}
	if b, err := v.policy("other"); err != nil || b != nil {
		t.Fatalf("absent policy = %q, %v", b, err)
	}
	if why, err := v.fastForward(tip, v.head); err != nil || why != "" {
		t.Fatalf("fast-forward = %q, %v", why, err)
	}
	api.github["/repos/example-org/gadgets/compare/0123456789...abc1234def"] = `{"status":"diverged","behind_by":2}`
	if why, _ := v.fastForward(tip, v.head); !strings.Contains(why, "not a fast-forward") {
		t.Fatalf("diverged = %q", why)
	}
	api.github["/repos/example-org/gadgets/pulls/4"] = `{"state":"closed","merged":true}`
	if v, err := githubMergeView(api, "example-org/gadgets", "#4"); err != nil || v.open {
		t.Fatalf("merged = %+v, %v", v, err)
	}
}

// The real clients merge only as a fast-forward: GitLab's merge call with
// the head pinned and no squash, GitHub's branch moved without force,
// with prompts off.
func TestExecForgeMergeIsFastForward(t *testing.T) {
	f := ExecForge{Timeout: 10 * time.Second}
	out := filepath.Join(t.TempDir(), "argv")
	shim(t, "glab", `echo "$GH_PROMPT_DISABLED $NO_PROMPT $GIT_TERMINAL_PROMPT $*" > `+out)
	if err := f.Merge(Repository{Forge: "gitlab", Host: "gitlab.example.com", Project: "example-org/widgets"}, "!3", "c0ffee", "main"); err != nil {
		t.Fatal(err)
	}
	want := "1 1 0 api --hostname gitlab.example.com -X PUT projects/example-org%2Fwidgets/merge_requests/3/merge -f sha=c0ffee -F squash=false\n"
	if b, _ := readFile(out); string(b) != want {
		t.Fatalf("glab = %q, want %q", b, want)
	}
	shim(t, "gh", `echo "$GH_PROMPT_DISABLED $*" > `+out)
	if err := f.Merge(Repository{Forge: "github", Project: "example-org/gadgets"}, "#4", "abc1234", "main"); err != nil {
		t.Fatal(err)
	}
	want = "1 api --method PATCH repos/example-org/gadgets/git/refs/heads/main -f sha=abc1234 -F force=false\n"
	if b, _ := readFile(out); string(b) != want {
		t.Fatalf("gh = %q, want %q", b, want)
	}
	shim(t, "gh", `echo "Update is not a fast forward" >&2; exit 1`)
	if err := f.Merge(Repository{Forge: "github", Project: "example-org/gadgets"}, "#4", "abc1234", "main"); err == nil {
		t.Fatal("a refused merge succeeded")
	}
}

// merge is an allowed write, and like push needs a gate.
func TestMergeConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.json")
	writeFile(t, path, `{"repositories": {"widgets": {"forge": "gitlab", "project": "example-org/widgets", "allow": ["open", "merge"], "gate": ["./bin/check"]}}}`)
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("merge refused: %v", err)
	}
	writeFile(t, path, `{"repositories": {"widgets": {"forge": "gitlab", "project": "example-org/widgets", "allow": ["merge"]}}}`)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "allows merge but has no gate") {
		t.Fatalf("merge without a gate: %v", err)
	}
}
