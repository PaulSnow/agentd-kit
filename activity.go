package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// A session's activity (#56 C): the tools it ran lately, and the one it
// runs now, read from the tail of its transcript when a page asks, not
// logged, so the log does not grow with every tool call of every
// session. Each line is screened as the page shows it.

const (
	activityTail  = 512 << 10 // bytes of a transcript's end read for its activity
	activityShown = 30        // tools a session's page lists, the latest
)

// Activity is what a session did lately: its latest tools, oldest first,
// and the tool it runs now, if a call has no result yet.
type Activity struct {
	Ran []TurnPayload
	Now *TurnPayload
}

// readActivity reads the activity in the end of the transcript at path.
// A tool whose call is before the part read is not listed.
func readActivity(path string) (Activity, error) {
	f, err := os.Open(path)
	if err != nil {
		return Activity{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Activity{}, err
	}
	// A transcript unchanged since it was last read is not read again, so
	// a page refreshing every few seconds costs a stat per session.
	key := activityKey{path, info.Size(), info.ModTime()}
	activityMu.Lock()
	a, ok := activityRead[key.path]
	activityMu.Unlock()
	if ok && a.key == key {
		return a.Activity, nil
	}
	got, err := readActivityFrom(f, info)
	if err == nil {
		activityMu.Lock()
		if len(activityRead) >= activityCachedMax {
			clear(activityRead)
		}
		activityRead[key.path] = activityCached{key, got}
		activityMu.Unlock()
	}
	return got, err
}

// activityKey is a transcript as it was read: its path, size and time.
type activityKey struct {
	path string
	size int64
	mod  time.Time
}

// activityCached is a transcript's activity as last read.
type activityCached struct {
	key activityKey
	Activity
}

// activityRead holds each transcript's activity as last read, one per
// transcript path, at most activityCachedMax: when full it starts again,
// so transcripts left behind by rotations do not pile up.
const activityCachedMax = 256

var (
	activityMu   sync.Mutex
	activityRead = map[string]activityCached{}
)

// readActivityFrom reads the activity from the tail of f.
func readActivityFrom(f *os.File, info os.FileInfo) (Activity, error) {
	off := info.Size() - activityTail
	if off < 0 {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return Activity{}, err
	}
	r := bufio.NewReaderSize(f, 64<<10)
	if off > 0 {
		r.ReadBytes('\n') // a line cut by the seek
	}
	var t transcriptFile
	var calls []string // call ids in order, to find the one running now
	targets := map[string]TurnPayload{}
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 && err == nil {
			t.lines++
			for _, id := range toolCalls(line) {
				calls = append(calls, id)
			}
			for _, tn := range t.toolTurns(line) {
				if len(t.ran) == activityShown {
					t.ran = t.ran[1:]
				}
				t.ran = append(t.ran, tn)
			}
			for id, target := range t.tools {
				if _, ok := targets[id]; !ok {
					targets[id] = TurnPayload{Role: TurnTool, Text: target, At: lineTime(line)}
				}
			}
		}
		if err != nil {
			break
		}
	}
	a := Activity{Ran: t.ran}
	for i := len(calls) - 1; i >= 0; i-- {
		if _, open := t.tools[calls[i]]; open {
			now := targets[calls[i]]
			a.Now = &now
			break
		}
	}
	return a, nil
}

// toolCalls is the ids of the tool calls a transcript line makes.
func toolCalls(line []byte) []string {
	var l struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil {
		return nil
	}
	var ids []string
	for _, b := range l.Message.Content {
		if b.Type == "tool_use" && b.ID != "" {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

// lineTime is a transcript line's time, zero when it has none.
func lineTime(line []byte) time.Time {
	var l struct {
		Timestamp string `json:"timestamp"`
	}
	json.Unmarshal(line, &l)
	at, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
	return at
}

// sessionActivity reads session sess's activity, from its transcript;
// none when it has no transcript here, such as a session on another host.
func sessionActivity(cfg *Config, sess Session, t *Track) (Activity, bool) {
	path := sessionTranscript(cfg, sess, t)
	if path == "" {
		return Activity{}, false
	}
	a, err := readActivity(path)
	return a, err == nil
}

// sessionTranscript is the path of the transcript session sess was last
// launched with, else its newest, or "" when it has none here.
func sessionTranscript(cfg *Config, sess Session, t *Track) string {
	root := transcriptRootOf(cfg, sess)
	if root == "" || sess.Cwd == "" {
		return ""
	}
	dir := TranscriptDir(root, sess.Cwd)
	id := ""
	if t != nil {
		id = t.Transcript
	}
	if _, err := os.Stat(filepath.Join(dir, id+".jsonl")); id == "" || err != nil {
		id = newestTranscript(dir)
	}
	if id == "" {
		return ""
	}
	return filepath.Join(dir, id+".jsonl")
}
