package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// modelOf is the model a launch command passes after --model, or "".
func modelOf(cmd []string) string {
	for i, part := range cmd {
		if part == "--model" && i+1 < len(cmd) {
			return cmd[i+1]
		}
	}
	return ""
}

// Each kind of session runs its own model, its repository's over every
// repository's, passed to the launch command; a kind with none runs the
// command's default (#52).
func TestModelReachesTheCommandPerKind(t *testing.T) {
	h := repoHarness(t)
	h.cfg.Policies.Restart.Command = []string{"claude", "--model", "{model}", "{prompt}"}
	h.cfg.Policies.Models = ModelsPolicy{Work: "sonnet", Reviewer: "sonnet", Judge: "opus"}
	w := h.cfg.Repositories["widgets"]
	w.Policy.Sessions.WorkModel = "haiku"
	w.Policy.Review.JudgeModel = "sonnet"
	h.cfg.Repositories["widgets"] = w
	started := func(kind, repo string) Session {
		r := h.cfg.Repositories[repo]
		r.Sessions = &SessionsPolicy{Root: h.dir}
		return runtimeSession("x", RuntimePayload{Repository: repo, Kind: kind, Branch: "night/1-x"}, r)
	}
	sess := func(role, repo string) Session {
		s := h.cfg.Sessions["a"]
		s.Role, s.Repository = role, repo
		return s
	}
	for _, c := range []struct {
		name string
		sess Session
		want string
	}{
		{"started work", started(KindWork, "widgets"), "haiku"},
		{"started reviewer", started(KindReviewer, "readonly"), "sonnet"},
		{"started judge", started(KindJudge, "widgets"), "sonnet"},
		{"started judge elsewhere", started(KindJudge, "readonly"), "opus"},
		{"work", sess("", "widgets"), "haiku"},
		{"work elsewhere", sess("", "readonly"), "sonnet"},
		{"work of no repository", sess("", ""), "sonnet"},
		{"reviewer", sess(RoleReviewer, "widgets"), "sonnet"},
		{"judge", sess(RoleJudge, "widgets"), "sonnet"},
		{"judge elsewhere", sess(RoleJudge, "readonly"), "opus"},
	} {
		cmd, _ := h.sup.launchCommand(c.name, c.sess, newTrack(), "id-1", "restart")
		if got := modelOf(cmd); got != c.want {
			t.Errorf("%s: model %q, want %q (%q)", c.name, got, c.want, cmd)
		}
	}
	// A kind with no model leaves out --model and its value.
	h.cfg.Policies.Models = ModelsPolicy{}
	cmd, _ := h.sup.launchCommand("d", sess("", "readonly"), newTrack(), "id-1", "restart")
	if len(cmd) != 2 || cmd[0] != "claude" || !strings.HasPrefix(cmd[1], "You were restarted") {
		t.Fatalf("command with no model = %q", cmd)
	}
}

// With no model, only a --model argument is left out; any other {model}
// becomes empty, so a positional argument after it keeps its place.
func TestWithModel(t *testing.T) {
	for _, c := range []struct {
		cmd   []string
		model string
		want  []string
	}{
		{[]string{"claude", "--model", "{model}", "p"}, "haiku", []string{"claude", "--model", "haiku", "p"}},
		{[]string{"claude", "--model", "{model}", "p"}, "", []string{"claude", "p"}},
		{[]string{"claude", "--model={model}", "p"}, "", []string{"claude", "p"}},
		{[]string{"claude", "--model={model}", "p"}, "opus", []string{"claude", "--model=opus", "p"}},
		{[]string{"sh", "-c", "s", "x", "{model}", "{prompt}"}, "", []string{"sh", "-c", "s", "x", "", "{prompt}"}},
		{[]string{"claude", "p"}, "haiku", []string{"claude", "p"}},
	} {
		if got := withModel(c.cmd, c.model); !reflect.DeepEqual(got, c.want) {
			t.Errorf("withModel(%q, %q) = %q, want %q", c.cmd, c.model, got, c.want)
		}
	}
}

// A repository's own file that narrows sessions.models past a chosen
// model holds: its sessions run the list's first model instead.
func TestNarrowedModelsHold(t *testing.T) {
	h := repoHarness(t)
	h.cfg.Policies.Restart.Command = []string{"claude", "--model", "{model}", "{prompt}"}
	h.cfg.Policies.Models = ModelsPolicy{Work: "opus", Reviewer: "sonnet", Judge: "haiku"}
	w := h.cfg.Repositories["widgets"]
	w.Policy.Sessions.Models = []string{"opus", "sonnet", "haiku"}
	w.Policy.Merge.Targets = []string{"main"}
	h.cfg.Repositories["widgets"] = w
	h.sup.st.Policies = map[string]PolicyPayload{policyKey("widgets", "main"): {
		Repository: "widgets", Target: "main", File: []byte(`{"sessions": {"models": ["haiku", "sonnet"]}}`),
	}}
	sess := h.cfg.Sessions["a"]
	sess.Role, sess.Repository = "", "widgets"
	if m := sessionModel(h.cfg, h.sup.st, sess); m != "haiku" {
		t.Errorf("work model = %q, want haiku", m)
	}
	sess.Role, sess.Issue = RoleReviewer, 7
	if m := sessionModel(h.cfg, h.sup.st, sess); m != "sonnet" {
		t.Errorf("reviewer model = %q, want sonnet", m)
	}
}

// With restarts and rotation off nothing is launched, so a model set with
// no {model} in the command loads; with rotation on it is refused.
func TestModelWithRestartOffLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	writeFile(t, path, `{"policies": {"restart": {"enabled": false, "command": ["claude", "{prompt}"]}, "context": {"enabled": false}, "models": {"work": "haiku"}}}`)
	if _, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, `{"policies": {"restart": {"enabled": false, "command": ["claude", "{prompt}"]}, "models": {"work": "haiku"}}}`)
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("a model rotation would not run was accepted")
	}
}

// agentd status shows the model each session runs.
func TestStatusShowsModel(t *testing.T) {
	h := repoHarness(t)
	w := h.cfg.Repositories["widgets"]
	w.Policy.Sessions.WorkModel = "haiku"
	h.cfg.Repositories["widgets"] = w
	h.ticks(1, 0)
	rows, _ := SummarizeState(h.cfg, h.sup.st)
	got := map[string]string{}
	for _, r := range rows {
		got[r.Name] = r.Model
	}
	want := map[string]string{"a": "haiku", "b": "haiku", "d": "default"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	var out strings.Builder
	PrintStatus(&out, rows, nil, h.clock.Now())
	if !strings.Contains(out.String(), "MODEL") || !strings.Contains(out.String(), "haiku") || !strings.Contains(out.String(), "default") {
		t.Fatalf("status:\n%s", out.String())
	}
}

// The default restart command passes --model only when the session has
// one, run through bash as tmux runs it.
func TestDefaultCommandPassesModel(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nfor a in \"$@\"; do printf '[%s]' \"$a\"; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := DefaultConfig().Policies.Restart.Command
	for model, want := range map[string]string{
		"haiku": "[--model][haiku][--session-id][id-1][--mcp-config=m][the prompt]",
		"":      "[--session-id][id-1][--mcp-config=m][the prompt]",
	} {
		argv := strings.NewReplacer("{session_id}", "id-1", "{mcp}", "m", "{prompt}", "the prompt").Replace(strings.Join(withModel(cmd, model), "\x00"))
		parts := strings.Split(argv, "\x00")
		c := exec.Command(bash, append([]string{"-c"}, parts[2:]...)...) // -c, not -lic: no profile
		c.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := c.Output()
		if err != nil || string(out) != want {
			t.Errorf("model %q: %s, %v; want %s", model, out, err, want)
		}
	}
}
