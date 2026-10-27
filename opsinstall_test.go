package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// opsWatchH is newOpsH whose live.json watches main's CI on the
// agentd-kit repository's forge, installing without a yes with auto.
func opsWatchH(t *testing.T, auto bool) (*opsH, *fakeForgeAPI) {
	t.Helper()
	h := newOpsH(t)
	a := "false"
	if auto {
		a = "true"
	}
	writeOps(t, filepath.Join(h.dir, "live.json"), `{
  "event_log": "live.jsonl",
  "repositories": {"agentd-kit": {"forge": "github", "project": "example-org/agentd-kit"}},
  "ops": {
    "supervisors": {
      "live": {"config": "live.json", "tmux": "agentd-live"},
      "page": {"config": "page.json", "tmux": "agentd-page"}
    },
    "repo": "clone",
    "binary": "bin/agentd",
    "branch": "main",
    "watch": "agentd-kit",
    "auto_install": `+a+`
  }
}
`, 0o600)
	cfg, err := LoadConfig(filepath.Join(h.dir, "live.json"))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg, h.env.cfg = cfg, cfg
	api := &fakeForgeAPI{github: map[string]string{}}
	h.restartOps()
	h.sup.api = api
	return h, api
}

// checkRuns is GitHub's check runs of a commit, each name's conclusion.
func checkRuns(conclusions ...string) string {
	runs := []string{}
	for i, c := range conclusions {
		runs = append(runs, `{"name":"check-`+itoa(i)+`","status":"completed","conclusion":"`+c+`"}`)
	}
	return `{"total_count":` + itoa(len(runs)) + `,"check_runs":[` + strings.Join(runs, ",") + `]}`
}

// installs are the install changes of the log, by commit.
func (h *opsH) installs() []*OpsRecord {
	var out []*OpsRecord
	st := h.logState()
	for _, id := range sortedKeys(st.Ops) {
		if r := st.Ops[id]; r.Op == OpsInstall {
			out = append(out, r)
		}
	}
	return out
}

// Main's tip with its CI not green is not proposed; once green, Go
// proposes its install on the ops console, once, and waits on the
// owner's yes.
func TestOpsWatchProposesGreenTip(t *testing.T) {
	h, api := opsWatchH(t, false)
	tip := h.commitMain()
	path := "/repos/example-org/agentd-kit/commits/" + tip + "/check-runs?per_page=100"
	api.github[path] = checkRuns("success", "in_progress")
	h.ticks(2, 10*time.Minute)
	if rs := h.installs(); len(rs) != 0 {
		t.Fatalf("proposed on a CI not green: %+v", rs[0])
	}
	api.github[path] = checkRuns("success", "skipped")
	h.ticks(3, 10*time.Minute)
	rs := h.installs()
	if len(rs) != 1 || rs[0].Commit != tip || rs[0].Auto || !rs[0].open() || !strings.Contains(rs[0].Why, "CI of main is green") {
		t.Fatalf("installs = %+v", rs)
	}
	told := 0
	for _, e := range h.events() {
		if e.Kind == KindMessage && e.Message.Console == "ops" && strings.Contains(e.Message.Text, "agentd proposes installing "+short(tip)) {
			told++
		}
	}
	if told != 1 || h.builds != 0 {
		t.Fatalf("told %d times, %d builds", told, h.builds)
	}
}

// With ops.auto_install a green tip is installed without a yes; an
// install whose agentd config check fails is put back, the console is
// told why, and the tip is not proposed again.
func TestOpsAutoInstallRollsBackOnFailedCheck(t *testing.T) {
	h, api := opsWatchH(t, true)
	h.built = "#!/bin/sh\necho 'config: policies are wrong' >&2\nexit 1\n"
	tip := h.commitMain()
	api.github["/repos/example-org/agentd-kit/commits/"+tip+"/check-runs?per_page=100"] = checkRuns("success")
	h.ticks(4, 10*time.Minute)
	rs := h.installs()
	if len(rs) != 1 || !rs[0].Auto || rs[0].Answer != "" || rs[0].Ended.IsZero() || !rs[0].Failed || !strings.Contains(rs[0].Outcome, "is back") {
		t.Fatalf("installs = %+v", rs)
	}
	if b, _ := os.ReadFile(h.binary); string(b) != opsPassing || h.builds != 1 {
		t.Fatalf("the binary is %q after %d builds", b, h.builds)
	}
	var told []string
	for _, e := range h.events() {
		if e.Kind == KindMessage && e.Message.Console == "ops" && e.Message.From == "agentd" {
			told = append(told, e.Message.Text)
		}
	}
	if len(told) != 2 || !strings.Contains(told[0], "without a yes") || !strings.Contains(told[1], "failed") || !strings.Contains(told[1], "What changed") {
		t.Fatalf("told = %q", told)
	}
	h.ticks(3, 10*time.Minute)
	if rs = h.installs(); len(rs) != 1 || h.builds != 1 {
		t.Fatalf("the tip was proposed again: %d installs, %d builds", len(rs), h.builds)
	}
	// The page shows it answered by the policy, not the owner.
	src := h.page()
	src.catchUp()
	es := src.opsEntries(h.cfg, "ops", func(...string) bool { return false })
	if len(es) != 1 || es[0].open || es[0].Answer == nil || !strings.Contains(es[0].Answer.Text, "ops.auto_install") {
		t.Fatalf("entries = %+v", es)
	}
}

// A forge that cannot be asked is logged once, not each look; a tip with
// no check runs is green by its combined status; an auto install whose
// policy is off waits on the owner's yes.
func TestOpsWatchEdges(t *testing.T) {
	h, api := opsWatchH(t, true)
	tip := h.commitMain()
	api.githubErr = errors.New("HTTP 403: resource not accessible")
	h.ticks(4, 10*time.Minute)
	errs := 0
	for _, e := range h.events() {
		if e.Kind == KindError && strings.HasPrefix(e.Error.Step, "ops watch") && strings.Contains(e.Error.Error, "403") {
			errs++
		}
	}
	if errs != 1 || len(h.installs()) != 0 {
		t.Fatalf("%d errors logged, %d installs", errs, len(h.installs()))
	}

	// Statuses alone: green. The policy is off by the time it would run.
	api.githubErr = nil
	api.github["/repos/example-org/agentd-kit/commits/"+tip+"/check-runs?per_page=100"] = `{"total_count":0,"check_runs":[]}`
	api.github["/repos/example-org/agentd-kit/commits/"+tip+"/status"] = `{"state":"success","total_count":2}`
	h.sup.cfg.Ops.AutoInstall = false
	h.ticks(3, 10*time.Minute)
	rs := h.installs()
	if len(rs) != 1 || !strings.Contains(rs[0].Why, "2 statuses are success") || !rs[0].open() || h.builds != 0 {
		t.Fatalf("installs = %+v, %d builds", *rs[0], h.builds)
	}
	src := h.page()
	src.catchUp()
	if es := src.opsEntries(h.sup.cfg, "ops", func(...string) bool { return false }); len(es) != 1 || !es[0].open {
		t.Fatalf("the install does not wait on a yes: %+v", es)
	}
	// An install proposed while auto_install was on needs a yes once it
	// is off.
	r := &OpsRecord{OpsChange: OpsChange{Op: OpsInstall, Auto: true}}
	if cfg := (&Config{Ops: OpsConfig{Watch: "agentd-kit"}}); r.noYes(cfg) {
		t.Fatal("an auto install runs with auto_install off")
	}
	if cfg := (&Config{Ops: OpsConfig{Watch: "agentd-kit", AutoInstall: true}}); !r.noYes(cfg) {
		t.Fatal("an auto install waits with auto_install on")
	}

}
