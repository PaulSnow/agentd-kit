package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// syncWriter records syncs, and can fail them.
type syncWriter struct {
	bytes.Buffer
	syncs int
	fail  error
}

func (w *syncWriter) Sync() error { w.syncs++; return w.fail }

// Every append reaches the disk before Append returns: it is synced, and a
// failed sync is the append's error.
func TestLogAppendSyncs(t *testing.T) {
	w := &syncWriter{}
	l := NewLogWriter(w)
	if err := l.Append(Event{Kind: KindStart}); err != nil || w.syncs != 1 {
		t.Fatalf("append: %v, syncs %d", err, w.syncs)
	}
	w.fail = errors.New("disk gone")
	if err := l.Append(Event{Kind: KindStop}); err == nil {
		t.Fatal("a failed sync was not reported")
	}
}

// The event log is private to its owner, and another process's append (as
// mark-delivered makes) waits for the lock rather than interleaving.
func TestLogFileIsPrivateAndLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	l, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	other, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.Append(Event{Kind: KindStart}) }()
	select {
	case err := <-done:
		t.Fatalf("append did not wait for the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	syscall.Flock(int(other.Fd()), syscall.LOCK_UN)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("append never took the lock")
	}
}

// A malformed line before the end of the log is corruption, and startup
// stops on it rather than replaying a log with a hole in it.
func TestMalformedMiddleLineStopsStartup(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	writeFile(t, logPath, `{"time":"2026-10-22T00:00:00Z","kind":"start"}`+"\n"+`{"kind":`+"\n"+`{"time":"2026-10-22T00:01:00Z","kind":"stop"}`+"\n")
	if _, _, err := ReadLog(logPath); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("ReadLog: %v, want an error at line 2", err)
	}
	cfgPath := filepath.Join(dir, "agentd.json")
	writeFile(t, cfgPath, `{"event_log": "`+logPath+`", "transcript_dir": ""}`)
	if err := runCmd([]string{"--config", cfgPath, "--once"}); err == nil {
		t.Fatal("run started over a corrupt log")
	}
}

// A torn final line, a write cut short, is dropped from the log with a
// warning event, and the log stays readable after later appends.
func TestTornFinalLineIsDroppedWithWarning(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	writeFile(t, logPath, `{"time":"2026-10-22T00:00:00Z","kind":"start"}`+"\n"+`{"time":"2026-10-22T00:01:00Z","kind":"obs`)
	cfgPath := filepath.Join(dir, "agentd.json")
	writeFile(t, cfgPath, `{"event_log": "`+logPath+`", "transcript_dir": ""}`)
	if err := runCmd([]string{"--config", cfgPath, "--once"}); err != nil {
		t.Fatal(err)
	}
	events, torn, err := ReadLog(logPath)
	if err != nil || torn != 0 {
		t.Fatalf("log after the run: torn %d, %v", torn, err)
	}
	warned := false
	for _, e := range events {
		if e.Kind == KindWarning {
			n, ok := detailInt(e, "bytes")
			warned = ok && n == len(`{"time":"2026-10-22T00:01:00Z","kind":"obs`)
		}
	}
	if !warned || events[0].Kind != KindStart || len(events) != 4 {
		t.Fatalf("events = %+v", events)
	}
}

// A missing number is not a zero.
func TestDetailIntMissingIsUnknown(t *testing.T) {
	e := Event{Details: map[string]any{"n": float64(0)}}
	if n, ok := detailInt(e, "n"); !ok || n != 0 {
		t.Fatalf("present zero = %d, %v", n, ok)
	}
	if _, ok := detailInt(e, "missing"); ok {
		t.Fatal("a missing detail read as a number")
	}
}

// When the screen has never been seen, how long it has been unchanged is
// unknown, and the observe event leaves it out instead of logging the
// largest duration there is.
func TestUnchangedUnknownIsAbsent(t *testing.T) {
	h := newHarness(t, false, "gone")
	h.status("gone", working)
	h.tmux.newErr["gone"] = errors.New("tmux: server not running")
	h.ticks(1, 5*time.Minute)
	seen := 0
	for _, e := range h.events() {
		if e.Kind != KindObserve {
			continue
		}
		seen++
		if _, ok := e.Details["unchanged_s"]; ok {
			t.Fatalf("unchanged_s logged for a screen never seen: %v", e.Details)
		}
	}
	if seen == 0 {
		t.Fatal("no observe event")
	}
}
