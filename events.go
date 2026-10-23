package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event is one line of the event log: an observation, a transition, an
// action taken, an action shadow mode would have taken, or an error.
type Event struct {
	Time    time.Time      `json:"time"`
	Session string         `json:"session,omitempty"`
	Kind    string         `json:"kind"`
	Details map[string]any `json:"details,omitempty"`
}

// Event kinds.
const (
	KindStart      = "start"      // agentd started: mode, config, sessions
	KindStop       = "stop"       // agentd stopped
	KindObserve    = "observe"    // evidence for a session changed
	KindTransition = "transition" // the session's state changed
	KindAction     = "action"     // an action was taken
	KindWould      = "would"      // shadow mode: the action that would have been taken
	KindSkip       = "skip"       // an action a policy declined, with the reason
	KindError      = "error"      // observation or action failed
)

// Action names, used in the details of action and would events.
const (
	ActRestart  = "restart"
	ActNudge    = "nudge"
	ActResume   = "resume"
	ActTrust    = "trust"
	ActFetch    = "fetch"
	ActPush     = "push"
	ActOpen     = "open"
	ActEscalate = "escalate"
)

// Log appends events to a JSONL file. It is safe for concurrent use.
type Log struct {
	mu sync.Mutex
	w  io.Writer
	c  io.Closer
}

// OpenLog opens path for appending, creating its directory.
func OpenLog(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Log{w: f, c: f}, nil
}

// NewLogWriter logs to any writer; tests use a buffer.
func NewLogWriter(w io.Writer) *Log { return &Log{w: w} }

// Append writes one event as one line.
func (l *Log) Append(e Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.w.Write(append(b, '\n'))
	return err
}

// Close closes the underlying file, if any.
func (l *Log) Close() error {
	if l.c == nil {
		return nil
	}
	return l.c.Close()
}

// ReadLog reads every well-formed event in path, in order. A missing file is
// an empty log. Malformed lines are counted, not fatal: the log is
// append-only and a torn last line must not hide the rest.
func ReadLog(path string) ([]Event, int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	return ReadEvents(f)
}

// ReadEvents parses JSONL events from r.
func ReadEvents(r io.Reader) ([]Event, int, error) {
	var events []Event
	bad := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil || e.Kind == "" {
			bad++
			continue
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		return events, bad, fmt.Errorf("reading event log: %w", err)
	}
	return events, bad, nil
}

// detailString reads a string detail, or "".
func detailString(e Event, key string) string {
	if v, ok := e.Details[key].(string); ok {
		return v
	}
	return ""
}

// detailInt reads a numeric detail, or 0. JSON numbers decode as float64.
func detailInt(e Event, key string) int {
	switch v := e.Details[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}
