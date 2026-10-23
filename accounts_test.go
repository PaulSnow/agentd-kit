package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const token = "sk-ant-oat01-not-a-real-token"

// accountHarness gives session s1 an account with a config directory and,
// when mode is not zero, a token file with that mode.
func accountHarness(t *testing.T, mode os.FileMode) (*harness, Account) {
	h := newHarness(t, false, "s1")
	acct := Account{ConfigDir: filepath.Join(h.dir, "claude-b")}
	if mode != 0 {
		acct.TokenFile = filepath.Join(h.dir, "token-b")
		writeFile(t, acct.TokenFile, token+"\n")
		if err := os.Chmod(acct.TokenFile, mode); err != nil {
			t.Fatal(err)
		}
	}
	h.cfg.Accounts = map[string]Account{"b": acct}
	s := h.cfg.Sessions["s1"]
	s.Account = "b"
	h.cfg.Sessions["s1"] = s
	h.status("s1", working)
	return h, acct
}

// A restarted session runs under its account: CLAUDE_CONFIG_DIR set, and
// the OAuth token read from its file when the command launches, never
// written into the command tmux is given or into the log.
func TestRestartUsesTheSessionsAccount(t *testing.T) {
	h, acct := accountHarness(t, 0o600)
	h.ticks(1, 5*time.Minute)
	var cmd string
	for _, c := range h.tmux.calls {
		if c.op == "new-session" {
			cmd = c.args[1]
		}
	}
	if !strings.Contains(cmd, "CLAUDE_CONFIG_DIR=") || !strings.Contains(cmd, acct.TokenFile) || strings.Contains(cmd, token) {
		t.Fatalf("restart command = %q", cmd)
	}
	if strings.Contains(h.buf.String(), token) || !strings.Contains(h.buf.String(), "CLAUDE_CONFIG_DIR") {
		t.Fatalf("log:\n%s", h.buf.String())
	}
	// The command line sets both for the program it starts.
	line, err := h.sup.launchLine(h.cfg.Sessions["s1"], []string{"sh", "-c", `echo "$CLAUDE_CONFIG_DIR|$CLAUDE_CODE_OAUTH_TOKEN"`})
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", line).Output()
	if err != nil || strings.TrimSpace(string(out)) != acct.ConfigDir+"|"+token {
		t.Fatalf("launched with %q, %v", out, err)
	}
}

// A token file others can read is refused: no restart under it, none under
// any other account, one skip and one escalation.
func TestReadableTokenFileIsRefused(t *testing.T) {
	h, _ := accountHarness(t, 0o644)
	h.ticks(6, 5*time.Minute)
	if got := restarts(h, "s1"); got != 0 {
		t.Fatalf("restarts = %d with a readable token file", got)
	}
	if len(h.runner.calls) != 1 || !strings.Contains(h.runner.calls[0][4], "token") || h.count(KindSkip, "", "s1") != 1 {
		t.Fatalf("escalations = %v, skips = %d", h.runner.calls, h.count(KindSkip, "", "s1"))
	}
}

// An unknown account, or one naming neither a config directory nor a
// token file, is refused at load.
func TestAccountsAreValidated(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "w"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "c.json")
	for name, text := range map[string]string{
		"unknown": `{"policies": {"escalate": {"command": ["post"]}}, "sessions": {"s": {"workdir": "w", "cwd": "w", "account": "nope"}}}`,
		"empty":   `{"policies": {"escalate": {"command": ["post"]}}, "accounts": {"a": {}}}`,
	} {
		writeFile(t, path, text)
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "account") {
			t.Errorf("%s: %v", name, err)
		}
	}
	writeFile(t, path, `{"policies": {"escalate": {"command": ["post"]}}, "accounts": {"a": {"config_dir": "claude-a", "token_file": "~/tok"}}, "sessions": {"s": {"workdir": "w", "cwd": "w", "account": "a"}}}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if a := cfg.Accounts["a"]; a.ConfigDir != filepath.Join(dir, "claude-a") || !filepath.IsAbs(a.TokenFile) {
		t.Fatalf("account paths not expanded: %+v", a)
	}
}
