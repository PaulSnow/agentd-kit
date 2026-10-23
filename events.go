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

// Modes, in the mode detail every event carries. A run replays only the
// events of its own mode, and the owner's: live and shadow never share state.
const (
	ModeLive   = "live"
	ModeShadow = "shadow"
	ModeOwner  = "owner" // an owner's record, such as mark-delivered, read by both
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
	ActUpdate   = "update" // the open Questions escalation's text changed
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

// Append writes one event as one line and syncs it. It first repairs a
// torn final line, so the event never lands glued to a fragment that the
// next run would cut off with it.
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
	if l.f != nil {
		if _, err := l.repairTail(); err != nil {
			return err
		}
	}
	return l.write(append(b, '\n'))
}

// write writes and syncs, with the lock held. When the write or the sync
// fails, a log file is cut back to its size before, so a failed append, a
// full disk say, never leaves a fragment for the next append to follow.
func (l *Log) write(b []byte) error {
	size := int64(-1)
	if l.f != nil {
		info, err := l.f.Stat()
		if err != nil {
			return err
		}
		size = info.Size()
	}
	_, err := l.w.Write(b)
	if s, ok := l.w.(interface{ Sync() error }); ok && err == nil {
		err = s.Sync()
	}
	if err != nil && size >= 0 {
		if terr := l.f.Truncate(size); terr != nil {
			return errors.Join(err, fmt.Errorf("cutting back the failed append: %w", terr))
		}
	}
	return err
}

// Load reads every event in the open log under its lock, after repairing a
// torn final line. It returns the events and the length of the torn line it
// cut off, if any.
func (l *Log) Load() ([]Event, int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	unlock, err := l.lock()
	if err != nil {
		return nil, 0, err
	}
	defer unlock()
	torn, err := l.repairTail()
	if err != nil {
		return nil, 0, err
	}
	info, err := l.f.Stat()
	if err != nil {
		return nil, 0, err
	}
	events, _, err := ReadEvents(io.NewSectionReader(l.f, 0, info.Size()))
	return events, torn, err
}

// repairTail makes the file end at a line boundary, with the lock held. A
// final line without its newline is a write cut short: one that does not
// parse is cut off, with a warning event saying how long it was, and one
// that parses gets its newline. It returns the length cut off.
func (l *Log) repairTail() (int, error) {
	info, err := l.f.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	start := size // where the final line begins
	buf := make([]byte, 64*1024)
	for start > 0 {
		n := int64(len(buf))
		if n > start {
			n = start
		}
		if _, err := l.f.ReadAt(buf[:n], start-n); err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			start = start - n + int64(i) + 1
			break
		}
		start -= n
	}
	if start == size {
		return 0, nil
	}
	tail := make([]byte, size-start)
	if _, err := l.f.ReadAt(tail, start); err != nil {
		return 0, err
	}
	var e Event
	if err := json.Unmarshal(tail, &e); err == nil && e.Kind != "" {
		return 0, l.write([]byte{'\n'})
	}
	if err := l.f.Truncate(start); err != nil {
		return 0, err
	}
	torn := int(size - start)
	w, err := json.Marshal(Event{Time: time.Now(), Kind: KindWarning, Details: map[string]any{
		"warning": "dropped a torn final line of the event log", "bytes": torn}})
	if err != nil {
		return 0, err
	}
	return torn, l.write(append(w, '\n'))
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

// ReadEvents parses JSONL events from r. A malformed final line without its
// newline is a write cut short: it is left out and its length in bytes
// returned, so the caller can warn. Any other malformed line, a complete
// last line included, is corruption, and an error: a replay that skipped it
// could repeat the action it recorded.
func ReadEvents(r io.Reader) ([]Event, int, error) {
	var events []Event
	br := bufio.NewReader(r)
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return nil, 0, fmt.Errorf("reading event log: %w", err)
		}
		final := err == io.EOF // the line has no newline
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
