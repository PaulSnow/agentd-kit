package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Only one agentd run may supervise an event log: a second exits with an
// error naming the lock, and the lock is free again once the first is gone.
func TestRunIsSingleInstance(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "state", "events.jsonl")
	cfgPath := filepath.Join(dir, "agentd.json")
	writeFile(t, cfgPath, `{"event_log": "`+logPath+`", "transcript_dir": ""}`)

	held, err := lockInstance(logPath + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	err = runCmd([]string{"--config", cfgPath, "--once"})
	if err == nil || !strings.Contains(err.Error(), logPath+".lock") {
		t.Fatalf("second run: %v, want an error naming the lock", err)
	}
	if _, err := lockInstance(logPath + ".lock"); err == nil {
		t.Fatal("a second lock was granted in the same process")
	}
	held.Close()
	if err := runCmd([]string{"--config", cfgPath, "--once"}); err != nil {
		t.Fatalf("run after the lock was released: %v", err)
	}
}

// The lock follows the log through symlinks: a run given a link to the log,
// or a log in a linked directory, still meets the run that holds it.
func TestInstanceLockFollowsSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "state")
	writeFile(t, filepath.Join(real, "events.jsonl"), "")
	if err := os.Symlink(filepath.Join(real, "events.jsonl"), filepath.Join(dir, "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	for name, logPath := range map[string]string{
		"linked file":              filepath.Join(dir, "link.jsonl"),
		"new file in a linked dir": filepath.Join(dir, "linkdir", "new.jsonl"),
	} {
		target := filepath.Join(real, filepath.Base(logPath))
		if name == "linked file" {
			target = filepath.Join(real, "events.jsonl")
		}
		held, err := lockInstance(target + ".lock")
		if err != nil {
			t.Fatal(err)
		}
		cfgPath := filepath.Join(dir, "agentd.json")
		writeFile(t, cfgPath, `{"event_log": "`+logPath+`", "transcript_dir": ""}`)
		if err := runCmd([]string{"--config", cfgPath, "--once"}); err == nil || !strings.Contains(err.Error(), target+".lock") {
			t.Errorf("%s: run = %v, want refused by the lock on %s", name, err, target+".lock")
		}
		held.Close()
	}
}
