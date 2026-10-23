package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
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
	KindWarning    = "warning"    // agentd itself noticed something: a torn log line dropped
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

// Log appends events to a JSONL file. It is safe for concurrent use, and
// across processes: each append holds an exclusive flock on the file, so
// mark-delivered cannot interleave with run, and is synced before it returns.
type Log struct {
	mu sync.Mutex
	w  io.Writer
	f  *os.File // the log file, when there is one
}

// OpenLog opens path for appending, creating its directory and the file,
// readable by its owner only.
func OpenLog(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{w: f, f: f}, nil
}

// NewLogWriter logs to any writer; tests use a buffer. A writer with a Sync
// method is synced after each append.
func NewLogWriter(w io.Writer) *Log { return &Log{w: w} }

// lock takes the file's exclusive lock, waiting for another process's
// append to finish, and returns its release.
func (l *Log) lock() (func(), error) {
	if l.f == nil {
		return func() {}, nil
	}
	fd := int(l.f.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("locking the event log: %w", err)
	}
	return func() { syscall.Flock(fd, syscall.LOCK_UN) }, nil
}

// Append writes one event as one line and syncs it.
func (l *Log) Append(e Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, err := l.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := l.w.Write(append(b, '\n')); err != nil {
		return err
	}
	if s, ok := l.w.(interface{ Sync() error }); ok {
		return s.Sync()
	}
	return nil
}

// Load reads every event in the open log under its lock, and repairs a torn
// final line so the next append starts a line of its own: a malformed final
// line is cut off, and a well-formed one missing its newline gets it. It
// returns the events and the length of the line it cut off.
func (l *Log) Load() ([]Event, int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, err := l.lock()
	if err != nil {
		return nil, 0, err
	}
	defer unlock()
	info, err := l.f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := info.Size()
	events, torn, err := ReadEvents(io.NewSectionReader(l.f, 0, size))
	if err != nil {
		return nil, 0, err
	}
	if torn > 0 {
		if err := l.f.Truncate(size - int64(torn)); err != nil {
			return nil, 0, err
		}
		size -= int64(torn)
	}
	if size > 0 {
		last := make([]byte, 1)
		if _, err := l.f.ReadAt(last, size-1); err != nil {
			return nil, 0, err
		}
		if last[0] != '\n' {
			if _, err := l.f.Write([]byte{'\n'}); err != nil {
				return nil, 0, err
			}
		}
	}
	return events, torn, l.f.Sync()
}

// Close closes the underlying file, if any.
func (l *Log) Close() error {
	if l.f == nil {
		return nil
	}
	return l.f.Close()
}

// ReadLog reads every event in path, in order, without changing it. A
// missing file is an empty log. See ReadEvents for malformed lines.
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

// ReadEvents parses JSONL events from r. A malformed final line is a write
// cut short: it is left out and its length in bytes returned, so the caller
// can warn. A malformed line anywhere else is corruption, and an error: a
// replay that skipped it could repeat the action it recorded.
func ReadEvents(r io.Reader) ([]Event, int, error) {
	var events []Event
	br := bufio.NewReader(r)
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, 0, fmt.Errorf("reading event log: %w", err)
		}
		final := err == io.EOF
		if !final {
			if _, perr := br.Peek(1); perr == io.EOF {
				final = true
			}
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var e Event
			if jerr := json.Unmarshal(line, &e); jerr != nil || e.Kind == "" {
				if final {
					return events, len(line), nil
				}
				return nil, 0, fmt.Errorf("event log line %d is malformed; fix or remove it before starting", n)
			}
			events = append(events, e)
		}
		if final {
			return events, 0, nil
		}
	}
}

// detailString reads a string detail, or "".
func detailString(e Event, key string) string {
	if v, ok := e.Details[key].(string); ok {
		return v
	}
	return ""
}

// detailInt reads a numeric detail. ok is false when the detail is missing
// or not a number, which is unknown, not zero. JSON numbers decode as float64.
func detailInt(e Event, key string) (int, bool) {
	switch v := e.Details[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}
