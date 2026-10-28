package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// agentd clear writes the owner's clear for a configured session, and
// refuses one that is not configured.
func TestClearCommand(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	if err := os.MkdirAll(filepath.Join(dir, "w"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "agentd.json")
	writeFile(t, cfgPath, `{"event_log": "`+logPath+`", "transcript_dir": "", "policies": {"escalate": {"command": ["post", "{message}"]}}, "sessions": {"s1": {"workdir": "w", "cwd": "w"}}}`)
	if err := clearCmd([]string{"--config", cfgPath, "s2"}); err == nil {
		t.Fatal("cleared an unconfigured session")
	}
	if err := clearCmd([]string{"--config", cfgPath, "s1"}); err != nil {
		t.Fatal(err)
	}
	events, _, err := ReadLog(logPath)
	if err != nil || len(events) != 1 || events[0].Kind != KindClear || events[0].Session != "s1" || events[0].Mode != ModeOwner {
		t.Fatalf("events = %+v, %v", events, err)
	}
}

// agentd console reply applies a reply as its console's tool hands it on,
// and refuses one without a console and text.
func TestConsoleCommand(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	cfgPath := filepath.Join(dir, "agentd.json")
	writeFile(t, cfgPath, `{"event_log": "`+logPath+`", "transcript_dir": "", "repositories": {"widgets": {"forge": "gitlab", "host": "gitlab.example.com", "project": "example-org/widgets"}}}`)
	log, _, err := openLoaded(logPath)
	if err != nil {
		t.Fatal(err)
	}
	it := ItemPayload{ID: "gitlab/example-org/widgets#4", Kind: "issue", Repository: "gitlab/example-org/widgets", Title: "Widgets wobble", SourceTime: time.Now()}
	if err := log.Append(Event{V: EventVersion, Time: time.Now(), Kind: KindItem, Mode: ModeShared, Item: &it}); err != nil {
		t.Fatal(err)
	}
	log.Close()
	if err := consoleCmd([]string{"--config", cfgPath, "reply", "widgets"}); err == nil {
		t.Fatal("a reply with no text was applied")
	}
	if err := consoleCmd([]string{"--config", cfgPath, "reply", "widgets", "seen", it.ID}); err != nil {
		t.Fatal(err)
	}
	events, _, err := ReadLog(logPath)
	if err != nil || len(events) != 2 || events[1].Kind != KindSeen || events[1].Seen.From != "console:widgets" {
		t.Fatalf("events = %+v, %v", events, err)
	}
}

// --config and --log may come anywhere after the command, as status
// takes them: every console form, issue and the other commands read
// the configuration named after their own words (#69). A missing file
// there is the proof it was read; "--" ends the flags.
func TestConfigAfterSubcommand(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lanterns.json")
	for _, args := range [][]string{
		{"console", "ask", "home", "hi", "--config", p},
		{"console", "ask", "lanterns#5", "trim", "the", "wicks", "--config=" + p},
		{"console", "reply", "--entry", "x:I1", "--choice", "drop", "--config", p},
		{"console", "message", "--to", "lamplighter", "hi", "--config", p},
		{"console", "lanterns", "--issues", "--config", p},
		{"console", "post", "lanterns", "hi", "--config", p},
		{"console", "inbox", "lanterns", "--config", p},
		{"console", "reply", "lanterns", "seen", "x1", "--config", p},
		{"issue", "lanterns", "5", "--config", p},
		{"status", "--log", "x.jsonl", "--config", p},
		{"sessions", "end", "lamplighter", "--config", p},
		{"loop", "retry", "lanterns", "!3", "--config", p},
		{"releases", "--config", p},
		{"policy", "show", "lanterns", "--config", p},
		{"cases", "--json", "--config", p},
		{"context", "stats", "lanterns", "--config", p},
	} {
		handled, err := runCommand(args[0], args[1:])
		if !handled || err == nil || !strings.Contains(err.Error(), p) {
			t.Errorf("%q: handled %v, err %v, want the missing %s", args, handled, err, p)
		}
	}
	for args, want := range map[string]string{
		"ask home hi --config P":        "--config P ask home hi",
		"ask --log=L home hi -config P": "--log=L -config P ask home hi",
		"post lanterns -- --config P":   "post lanterns -- --config P",
		"post lanterns hi --config":     "post lanterns hi --config",
		"reply lanterns try --log L":    "--log L reply lanterns try",
	} {
		if got := strings.Join(configFirst(strings.Fields(args)), " "); got != want {
			t.Errorf("%q: %q, want %q", args, got, want)
		}
	}
	if !strings.Contains(usageText, "quote it with the words around it") {
		t.Error("the usage does not say how to write --config as text")
	}
	if _, err := runCommand("no-such-command", nil); err != nil {
		t.Fatal(err)
	}
	if handled, _ := runCommand("no-such-command", nil); handled {
		t.Fatal("an unknown command was handled")
	}
}
