package main

import (
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
