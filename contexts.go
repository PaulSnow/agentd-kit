package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// A long-running Claude session re-reads its whole context on every model
// call, so most of what an unattended session spends goes on re-reading a
// context it no longer needs: its progress is already in its progress file,
// the issues and the branches. agentd measures each session's context and
// spend from its transcript, restarts a session whose context has grown past
// its policy's threshold with a fresh context at its next checkpoint (a
// rotation), builds the fresh session's prompt from the recorded progress,
// and stops a session that reaches its budget.

// UsagePayload is what one transcript says it has cost so far: its calls,
// their tokens by kind, the context of its latest main-chain call (input,
// cache write and cache read), and the input-equivalent spend. Guessed marks
// a transcript agentd did not launch, taken as the newest in the session's
// directory. Each event carries the transcript's totals, so a replay or a
// re-read gives the same figures.
type UsagePayload struct {
	Transcript string `json:"transcript"`
	Guessed    bool   `json:"guessed,omitempty"`
	Calls      int    `json:"calls"`
	Input      int64  `json:"input"`
	CacheWrite int64  `json:"cache_write"`
	CacheRead  int64  `json:"cache_read"`
	Output     int64  `json:"output"`
	Context    int64  `json:"context"`
	Spend      int64  `json:"spend"`
}

// CheckpointPayload is a merge newly found at the head of the session's
// worktree: a checkpoint the session reached. The first one agentd sees for
// a session is its baseline, a merge from before agentd looked, at which
// nothing is rotated.
type CheckpointPayload struct {
	SHA       string    `json:"sha"`
	Subject   string    `json:"subject"`
	Committed time.Time `json:"committed"`
	Baseline  bool      `json:"baseline,omitempty"`
}

// launchWait is how long a launched session has to write its transcript
// before agentd decides the command did not pass the session id on, and
// guesses instead.
const launchWait = 10 * time.Minute

// Checkpoint is the latest checkpoint seen for a session, and when.
type Checkpoint struct {
	CheckpointPayload
	At time.Time
}

// spendOf is the input-equivalent spend: cache writes cost a quarter more
// than input, cache reads a tenth, and output five times.
func spendOf(u UsagePayload) int64 {
	return int64(math.Round(float64(u.Input) + 1.25*float64(u.CacheWrite) + 0.1*float64(u.CacheRead) + 5*float64(u.Output)))
}

// current is the usage of the transcript the session now runs, when known:
// the one agentd launched, or the guess, for a session it did not launch or
// whose launched transcript never appeared.
func (t *Track) current() (UsagePayload, bool) {
	u, ok := t.Usage[t.Measured]
	if !ok || t.Transcript != "" && t.Measured != t.Transcript && !u.Guessed {
		return UsagePayload{}, false
	}
	return u, true
}

// spend is the session's spend since the owner last cleared it: every
// transcript agentd launched it under, and, while it has launched none, the
// current guess. exact leaves the guess out, as live mode does. A guess
// agentd has since replaced with a launch, or another file that was newest
// for a while, is never counted.
func (t *Track) spend(exact bool) (int64, bool) {
	var n int64
	known := false
	for _, u := range t.Usage {
		if !u.Guessed {
			n, known = n+u.Spend, true
		}
	}
	if u := t.Usage[t.Measured]; t.guessed() && !exact {
		n, known = n+u.Spend, true
	}
	return n - t.SpendBase, known
}

// guessed says whether the session is measured by a guess.
func (t *Track) guessed() bool {
	u, ok := t.Usage[t.Measured]
	return ok && u.Guessed
}

// callUsage is one model call's tokens.
type callUsage struct{ in, cw, cr, out int64 }

func (c *callUsage) add(o callUsage, sign int64) {
	c.in += sign * o.in
	c.cw += sign * o.cw
	c.cr += sign * o.cr
	c.out += sign * o.out
}

// transcriptFile is how far one transcript file has been read and what it
// held. Claude Code writes a call that has several content blocks as
// several consecutive lines with the same message id; each call counts
// once, with its last line's usage, so only the last call is remembered.
type transcriptFile struct {
	off     int64
	partial []byte // a last line not yet ended
	lastID  string
	last    callUsage
	calls   int
	sum     callUsage
	context int64
	known   bool // a main-chain call has been read, so context is known
}

// usageReader reads transcripts in the workers, each file from where the
// last read stopped. The loop and its workers share it.
type usageReader struct {
	mu    sync.Mutex
	files map[string]*transcriptFile
}

func newUsageReader() *usageReader { return &usageReader{files: map[string]*transcriptFile{}} }

// read reads what was appended to path since the last read. A file that
// shrank was replaced, and is read again from the start.
func (r *usageReader) read(path string) (*transcriptFile, error) {
	f := r.files[path]
	if f == nil {
		f = &transcriptFile{}
		r.files[path] = f
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < f.off {
		*f = transcriptFile{}
	}
	if _, err := fh.Seek(f.off, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(fh)
	if err != nil {
		return nil, err
	}
	f.off += int64(len(b))
	data := append(f.partial, b...)
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		f.partial = data
		return f, nil
	}
	f.partial = append([]byte(nil), data[end+1:]...)
	for _, line := range bytes.Split(data[:end], []byte("\n")) {
		f.note(line)
	}
	return f, nil
}

// note counts one transcript line, if it is an assistant call with usage.
func (f *transcriptFile) note(line []byte) {
	var l struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		RequestID   string `json:"requestId"`
		Message     struct {
			ID    string `json:"id"`
			Usage *struct {
				Input      int64 `json:"input_tokens"`
				CacheWrite int64 `json:"cache_creation_input_tokens"`
				CacheRead  int64 `json:"cache_read_input_tokens"`
				Output     int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil || l.Type != "assistant" || l.Message.Usage == nil {
		return
	}
	u := callUsage{l.Message.Usage.Input, l.Message.Usage.CacheWrite, l.Message.Usage.CacheRead, l.Message.Usage.Output}
	id := l.Message.ID
	if id == "" {
		id = l.RequestID
	}
	if id != "" && id == f.lastID {
		f.sum.add(f.last, -1)
	} else {
		f.calls++
	}
	f.lastID, f.last = id, u
	f.sum.add(u, 1)
	if !l.IsSidechain {
		f.context, f.known = u.in+u.cw+u.cr, true
	}
}

// measure totals transcript id in dir, with its subagents' transcripts. It
// is unknown (false) until the main transcript has a main-chain call.
func (r *usageReader) measure(dir, id string) (UsagePayload, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	main, err := r.read(filepath.Join(dir, id+".jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return UsagePayload{}, false, nil
	}
	if err != nil {
		return UsagePayload{}, false, err
	}
	if !main.known {
		return UsagePayload{}, false, nil
	}
	sum, calls := main.sum, main.calls
	subs, _ := filepath.Glob(filepath.Join(dir, id, "subagents", "*.jsonl"))
	sort.Strings(subs)
	for _, p := range subs {
		f, err := r.read(p)
		if err != nil {
			continue // a subagent file that went away: its calls are not counted
		}
		sum.add(f.sum, 1)
		calls += f.calls
	}
	u := UsagePayload{Transcript: id, Calls: calls, Input: sum.in, CacheWrite: sum.cw, CacheRead: sum.cr, Output: sum.out, Context: main.context}
	u.Spend = spendOf(u)
	return u, true, nil
}

// newestTranscript is the id of the newest transcript in dir, or "".
func newestTranscript(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var newest string
	var at time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if info, err := e.Info(); err == nil && (newest == "" || info.ModTime().After(at)) {
			newest, at = strings.TrimSuffix(e.Name(), ".jsonl"), info.ModTime()
		}
	}
	return newest
}

// transcriptRoot is where the session's account keeps its transcripts, or
// "" when transcripts are not read.
func (s *Supervisor) transcriptRoot(sess Session) string {
	root := s.cfg.TranscriptDir
	if root == "" {
		return ""
	}
	// An account with its own config directory keeps its transcripts there.
	if a := s.cfg.Accounts[sess.Account]; sess.Account != "" && a.ConfigDir != "" {
		root = filepath.Join(a.ConfigDir, "projects")
	}
	return root
}

// measure runs in a worker: it reads what the session's transcript gained
// since the last read, and looks for a new merge at the head of its
// worktree, and logs each when it changed. A transcript or worktree that
// cannot be read leaves the figures unknown.
func (s *Supervisor) measure(name string, sess Session, t *Track) {
	if root := s.transcriptRoot(sess); root != "" {
		dir := TranscriptDir(root, sess.Cwd)
		id, guessed := t.Transcript, false
		if _, err := os.Stat(filepath.Join(dir, id+".jsonl")); id == "" || err != nil && t.Launched.Before(s.clock.Now().Add(-launchWait)) {
			// Not launched by agentd, or by a command that does not pass
			// the session id on: the newest transcript is a guess.
			id, guessed = newestTranscript(dir), true
		}
		if id != "" {
			if u, ok, err := s.usage.measure(dir, id); err == nil && ok {
				// A guess that lands on a transcript agentd launched is
				// that transcript, measured exactly.
				u.Guessed = guessed && (t.Usage[id].Transcript == "" || t.Usage[id].Guessed)
				if prev, seen := t.Usage[id]; !seen || prev != u || t.Measured != id {
					s.emit(name, Event{Kind: KindUsage, Usage: &u})
				}
			}
		}
	}
	// Only a newer merge is a checkpoint: a checkout of an older branch
	// goes back to an earlier merge, which is not one reached now.
	// A merge the session makes mid-step, of its base into its branch or
	// by a pull, is none either: only a subject the policy's pattern
	// matches is a checkpoint.
	sha, subject, at, err := s.git.LastMerge(sess.Cwd)
	if err == nil && sha != "" && sha != t.Checkpoint.SHA && !at.Before(t.Checkpoint.Committed) && s.checkpointRe.MatchString(subject) {
		s.emit(name, Event{Kind: KindCheckpoint, Checkpoint: &CheckpointPayload{SHA: sha, Subject: subject, Committed: at,
			Baseline: t.Checkpoint.SHA == ""}})
	}
}

// rotate restarts a session whose context is over its policy's threshold
// with a fresh context, at a checkpoint: once Grace has passed since a merge
// it reached after it was launched, so the session has recorded the merge
// and moved on from it. It is never mid-step, never for a session waiting on
// the owner, and never twice for one merge. Live mode acts only on a
// transcript it launched; shadow mode also records what it would do on a
// guess. A rotation is not a crash and not a restart.
func (s *Supervisor) rotate(name string, sess Session, t *Track, ev Evidence, now time.Time) bool {
	pol := s.cfg.Policies.Context
	if !pol.Enabled || !ev.Alive {
		return false
	}
	if t.Transcript != "" && t.guessed() {
		// The launch's transcript never appeared: the restart command
		// does not pass {session_id} on, and live mode acts on no guess.
		key := "transcript:" + name + ":" + t.Transcript
		s.skipOnce(name, key, "launched transcript "+t.Transcript+" never appeared; not rotating or counting a guess", t, nil)
		s.escalate(name, key, fmt.Sprintf("%s: the transcript agentd launched it under never appeared, so its context and spend are guesses and it is not rotated or held to its budget. Make the restart command pass --session-id {session_id}.", name), t, now)
	}
	u, ok := t.current()
	if !ok || u.Context <= pol.Threshold || u.Guessed && !s.shadow {
		return false
	}
	cp := t.Checkpoint
	if cp.SHA == "" || cp.Baseline || !cp.At.After(t.Launched) || t.RotatedFor == cp.SHA || now.Sub(cp.At) < pol.Grace.Duration {
		return false
	}
	key := "rotate:" + name + ":" + cp.SHA
	if s.failed(t, key) {
		s.escalate(name, key, fmt.Sprintf("%s could not be rotated at %s after %d tries; its context stays at %d tokens. See agentd status.",
			name, shortHash(cp.SHA), len(t.Failures[key]), u.Context), t, now)
		return false
	}
	if !s.retryDue(t, key, now) {
		return false
	}
	id := newSessionID()
	line, err := s.launchLine(sess, s.launchCommand(sess, t, id, "rotation"))
	if err != nil {
		s.skipOnce(name, "account:"+name, "not rotating: "+err.Error(), t, &SkipPayload{Account: sess.Account})
		return false
	}
	err = s.act(name, ActRotate, &ActPayload{Key: key, Cwd: sess.Cwd, Account: sess.Account, Command: line, Transcript: id,
		Context: u.Context, Checkpoint: cp.SHA}, func() error {
		return s.tmux.Replace(name, sess.Cwd, line)
	})
	return err == nil && s.halt == nil && !s.shadow
}

// overBudget says why the session must stop, or "": the stop time has come,
// or the session's spend or the run's (every session's) has reached its
// cap. Live mode counts only transcripts it launched.
func (s *Supervisor) overBudget(t *Track, now time.Time) string {
	b := s.cfg.Policies.Budget
	if b.StopAt != nil && !now.Before(*b.StopAt) {
		return "stop time " + b.StopAt.Format(time.RFC3339) + " reached"
	}
	if sp, ok := t.spend(!s.shadow); b.SessionTokens > 0 && ok && sp >= b.SessionTokens {
		return fmt.Sprintf("session spend %d reached its cap of %d", sp, b.SessionTokens)
	}
	if b.RunTokens > 0 {
		var total int64
		for _, o := range s.st.Tracks {
			if sp, ok := o.spend(!s.shadow); ok {
				total += sp + o.SpendBase // a clear lifts a session's cap, not the run's
			}
		}
		if total >= b.RunTokens {
			return fmt.Sprintf("run spend %d reached its cap of %d", total, b.RunTokens)
		}
	}
	return ""
}

// stopForBudget stops a session that reached its budget or the stop time,
// once: its tmux session is ended, by its exact name, and it is neither
// restarted nor typed into until the owner clears it. A finished session is
// left to deliver.
func (s *Supervisor) stopForBudget(name string, t *Track, ev Evidence, now time.Time) {
	if t.Stopped != "" || ev.Status.Done {
		return
	}
	why := s.overBudget(t, now)
	key := "stop:" + name
	if why == "" || !s.retryDue(t, key, now) {
		return
	}
	s.act(name, ActStop, &ActPayload{Key: key, Reason: why}, func() error {
		if ev.Alive || ev.PaneDead {
			return s.tmux.Stop(name)
		}
		return nil
	})
}

// newSessionID is a fresh Claude Code session id, a random UUID, so agentd
// knows which transcript the session it launches writes.
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the system has no randomness left to give
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Progress is what a progress file's table of items says: the current item
// (the first not finished) and its state, and the items after it.
type Progress struct {
	Item  string
	State string
	Next  []string
}

// finishedState is a state cell that says the item needs nothing more.
var finishedState = regexp.MustCompile(`(?i)^(merged|done|deferred|closed|dropped|skipped)\b`)

// progressCell bounds a cell carried into a prompt.
const progressCell = 120

// ParseProgress reads the first markdown table of text whose header has a
// State column; its first column names the item. Items whose state starts
// with merged, done, deferred, closed, dropped or skipped are finished.
func ParseProgress(text string) Progress {
	var p Progress
	lines := strings.Split(text, "\n")
	cells := func(l string) []string {
		l = strings.TrimSpace(l)
		l = strings.TrimSuffix(strings.TrimPrefix(l, "|"), "|")
		out := strings.Split(l, "|")
		for i := range out {
			out[i] = cutUTF8(strings.TrimSpace(out[i]), progressCell)
		}
		return out
	}
	for i := 0; i+1 < len(lines); i++ {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "|") || !strings.HasPrefix(strings.TrimSpace(lines[i+1]), "|-") &&
			!strings.HasPrefix(strings.TrimSpace(lines[i+1]), "| -") && !strings.HasPrefix(strings.TrimSpace(lines[i+1]), "|:") {
			continue
		}
		col := -1
		for j, c := range cells(lines[i]) {
			if strings.EqualFold(c, "state") {
				col = j
			}
		}
		if col < 1 {
			continue
		}
		for _, l := range lines[i+2:] {
			if !strings.HasPrefix(strings.TrimSpace(l), "|") {
				break
			}
			row := cells(l)
			if len(row) <= col || row[0] == "" || finishedState.MatchString(row[col]) {
				continue
			}
			switch {
			case p.Item == "":
				p.Item, p.State = row[0], row[col]
			case len(p.Next) < 3:
				p.Next = append(p.Next, row[0])
			}
		}
		return p
	}
	return p
}

// restartFacts is what agentd hands a fresh session about where it stands,
// as one compact JSON document: data, not instructions.
type restartFacts struct {
	Why       string   `json:"why"` // rotation, restart, or answered: restarted with the owner's answers
	Brief     string   `json:"brief"`
	Progress  string   `json:"progress"`
	Item      string   `json:"item,omitempty"`
	State     string   `json:"state,omitempty"`
	LastMerge string   `json:"last_merge,omitempty"`
	Next      []string `json:"next,omitempty"`
	Answers   string   `json:"answers,omitempty"` // the owner's answers, when restarted with them
}

// restartPrompt is the fresh session's prompt: the fixed instruction, then
// the facts agentd recorded, from the progress file and the last merge.
func (s *Supervisor) restartPrompt(sess Session, t *Track, why string) string {
	f := restartFacts{Why: why, Brief: sess.Workdir + "/BRIEF.md", Progress: sess.Workdir + "/STATUS.md"}
	if why == "answered" {
		f.Answers = sess.Workdir + "/" + AnswersFile
	}
	if b, err := os.ReadFile(f.Progress); err == nil {
		p := ParseProgress(string(b))
		f.Item, f.State, f.Next = p.Item, p.State, p.Next
	}
	if cp := t.Checkpoint; cp.SHA != "" {
		f.LastMerge = cutUTF8(shortHash(cp.SHA)+" "+cp.Subject, progressCell)
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(f)
	return DefaultRestartPrompt + " " + strings.TrimSuffix(b.String(), "\n")
}

// launchCommand is the restart policy's command for a fresh session, with
// {brief}, {status}, {session_id} and {prompt} substituted.
func (s *Supervisor) launchCommand(sess Session, t *Track, id, why string) []string {
	r := strings.NewReplacer("{brief}", sess.Workdir+"/BRIEF.md", "{status}", sess.Workdir+"/STATUS.md",
		"{session_id}", id, "{prompt}", s.restartPrompt(sess, t, why))
	cmd := make([]string, len(s.cfg.Policies.Restart.Command))
	for i, part := range s.cfg.Policies.Restart.Command {
		cmd[i] = r.Replace(part)
	}
	return cmd
}
