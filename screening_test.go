package main

import (
	"strings"
	"testing"
	"time"
)

// A branch name or a commit message bound for the forge that matches a
// confidential pattern is not pushed: one skip and one escalation say so
// without the text, the log never holds it, and delivery goes ahead once
// the text no longer matches.
func TestConfidentialCommitsAreNotPushed(t *testing.T) {
	for what, set := range map[string]func(h *harness, text string){
		"message": func(h *harness, text string) { h.git.messages = "Add the page\n\n" + text + "\n" },
		"branch":  func(h *harness, text string) { h.git.branch = "feature-" + strings.ReplaceAll(text, " ", "-") },
	} {
		h := newHarness(t, false, "d")
		h.cfg.ConfidentialPatterns = []string{`(?i)nightingale`}
		h.restart(false)
		doneWithDelivery(t, h, "d")
		set(h, "for Nightingale launch")
		h.ticks(4, 5*time.Minute)
		if len(h.forge.pushes) != 0 || len(h.runner.gates) != 0 {
			t.Fatalf("%s: pushes %v, gates %d", what, h.forge.pushes, len(h.runner.gates))
		}
		if strings.Contains(strings.ToLower(h.buf.String()), "nightingale") {
			t.Fatalf("%s: the event log carries the confidential text", what)
		}
		if got := h.count(KindSkip, "", "d"); got != 1 {
			t.Fatalf("%s: skips = %d, want 1", what, got)
		}
		if len(h.runner.calls) != 1 || strings.Contains(strings.ToLower(h.runner.calls[0][4]), "nightingale") || !strings.Contains(h.runner.calls[0][4], "confidential") {
			t.Fatalf("%s: escalations = %v", what, h.runner.calls)
		}
		set(h, "for the launch")
		h.ticks(1, 5*time.Minute)
		if len(h.forge.pushes) != 1 {
			t.Fatalf("%s: pushes = %v once the text was cleaned", what, h.forge.pushes)
		}
	}
}

// The real client reads every full commit message ahead of the target.
func TestExecGitMessages(t *testing.T) {
	shim(t, "git", `echo "$*"`)
	got, err := ExecGit{Timeout: 10 * time.Second}.Messages(t.TempDir(), "main")
	if err != nil || strings.TrimSpace(got) != "log --format=%B origin/main..HEAD" {
		t.Fatalf("messages ran %q, %v", got, err)
	}
}
