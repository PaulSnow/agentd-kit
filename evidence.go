package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Evidence is everything observed about a session in one pass. State
// transitions come from these fields and nothing else.
type Evidence struct {
	Alive       bool // the session exists and its pane is running
	PaneDead    bool // the session exists but its command exited, kept by remain-on-exit
	Exit        int  // the dead pane's exit status, or -1 when unknown
	DeadScreen  string
	Command     string // what the live pane runs, #{pane_current_command}
	PaneHash    string
	Limited     bool
	TrustPrompt bool
	TrustMenu   *TrustMenu // the trust prompt's menu, when it shows a whole one
	PanePath    string     // where the live pane runs, #{pane_current_path}
	Dialog      string     // the dialog pattern the visible screen matches, or ""
	DialogShape bool       // the visible screen has a dialog's shape, which alone may be declined
	Prompt      bool       // the screen ends at Claude Code's input prompt, with no turn running
	Status      StatusEvidence
	// StatusQuestions is the hash of STATUS.md's own Questions section;
	// Status.Questions is what still waits on the owner once the answers
	// are counted, questions.json's blocking decisions included.
	StatusQuestions string
	Transcript      TranscriptEvidence
}

// StatusEvidence is what the session's STATUS.md says.
type StatusEvidence struct {
	Exists    bool
	Head      string
	Done      bool
	Questions string // the Questions section body when it asks something, else ""
	Progress  Progress
}

// TranscriptEvidence is the last turn of the session's Claude Code
// transcript, when one was found cheaply.
type TranscriptEvidence struct {
	Found      bool
	Role       string
	StopReason string
	At         time.Time
}

// ReadStatus reads and interprets a STATUS.md, in the same terms the Python
// supervisor used: DONE as the last non-blank line, and a Questions section
// whose body is more than "none".
func ReadStatus(path string) (StatusEvidence, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return StatusEvidence{}, nil
	}
	if err != nil {
		return StatusEvidence{}, err
	}
	return ParseStatus(string(b)), nil
}

// statusLimit is the most of a STATUS.md the UI reads.
const statusLimit = 1 << 18

// readStatusRegular is ReadStatus for the UI (#41): the file is read only
// as a regular file, never through a link or from a FIFO, which would
// hold a request, and at most statusLimit bytes of it.
func readStatusRegular(path string) (StatusEvidence, error) {
	b, err := readRegular(path, statusLimit)
	if errors.Is(err, os.ErrNotExist) {
		return StatusEvidence{}, nil
	}
	if err != nil {
		return StatusEvidence{}, err
	}
	return ParseStatus(string(b)), nil
}

// A STATUS.md may hold several Questions sections (one per issue the session
// works through). Each is judged on its own: a line that says "none" asks
// nothing, a section ends only at a heading of its own level or higher (a
// subheading under it is part of the question), and rules and blank lines
// around the text do not change the escalation key. Lines inside a fenced
// code block are content, never headings.
var questionsHeading = regexp.MustCompile(`(?i)^(#+)\s*Questions\s*$`)
var anyHeading = regexp.MustCompile(`^(#+)\s`)
var thematicBreak = regexp.MustCompile(`^(-{3,}|\*{3,}|_{3,})$`)

// noQuestions matches the ways a session says it has none: "none", "- none",
// "(none yet)", "None.", "None open.", "No open questions", "n/a".
var noQuestions = regexp.MustCompile(`(?i)^[-*\s]*\(?\s*(none(\s+(yet|open|so\s+far))?|no(\s+open)?\s+questions?(\s+(yet|open))?|n/a)\s*\.?\)?\s*$`)

// noneSaid matches a parenthesised line that says none are open and why:
// "(none open; the three were answered on ...)", "(no open questions;
// answered)" (#54). "(none of these ...)" asks something, so the none must
// end at punctuation; a question mark, or a word that asks the owner for
// something (noneAsks), makes it a question whatever it begins with.
var noneSaid = regexp.MustCompile(`(?i)^[-*\s]*\(\s*(none(\s+(yet|open|so\s+far))?|no(\s+open)?\s+questions?(\s+(yet|open))?)\s*([;:,.\x{2013}\x{2014}]|\s-\s)[^?]*\)\s*\.?$`)
var listOrFence = regexp.MustCompile("^\\s*([-*+]\\s|\\d+[.)]\\s|```|~~~)")
var noneAsks = regexp.MustCompile(`(?i)\b(but|need|needs|please|must|until|block|blocked|blocking|waiting|approve)\b`)

// noneFirst matches a line whose first word is none, said as an answer:
// "none. Owner asked ...", "None so far: ...", "- (none, ...". "None of
// the tests pass" and "none-blocking" say something else.
var noneFirst = regexp.MustCompile(`(?i)^[-*\s]*\(?\s*none(\s+(yet|open|so\s+far))?\s*([.,;:!)\x{2013}\x{2014}]|\s-\s|$)`)

// noneParagraphAsks is what makes a none paragraph ask the owner something
// after all: a question mark, or a request (#69).
var noneParagraphAsks = regexp.MustCompile(`(?i)\?|\b(please|must|blocked|choose|say\s+if|tell\s+me)\b|\bowner\s*:`)

// ParseStatus interprets STATUS.md text.
func ParseStatus(text string) StatusEvidence {
	ev := StatusEvidence{Exists: true, Progress: ParseProgress(text)}
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) == 0 {
		return ev
	}
	ev.Head = lines[0]
	if len(ev.Head) > 160 {
		ev.Head = ev.Head[:160]
	}
	ev.Done = lines[len(lines)-1] == "DONE"
	q, closed := parseQuestions(text, true)
	if !closed {
		q, _ = parseQuestions(text, false)
	}
	ev.Questions = q
	return ev
}

// parseQuestions joins the Questions sections of text. With fences set,
// lines inside code fences are content; closed is false when a fence is
// still open at the end, which is a typo the caller reads again without
// fences rather than a block that swallows every later heading.
func parseQuestions(text string, fences bool) (questions string, closed bool) {
	var found, section []string
	level := 0  // the open Questions section's heading level, or 0 outside one
	fence := "" // the fence the line is inside, such as "```", or ""
	flush := func() {
		// A DONE line under the heading is the end of the file, not a
		// question, and a line that says none are open asks nothing: only
		// the lines left are the section's questions. That holds for a
		// line within a question too, such as an option "- none", which
		// leaves the question asked. A parenthesised reason wrapped onto
		// the lines after it is judged as the one line it is.
		var kept, lead []string
		for i := 0; i < len(section); i++ {
			t, j := strings.TrimSpace(section[i]), i
			if strings.HasPrefix(strings.TrimLeft(t, "-* "), "(") && !strings.Contains(t, ")") {
				// Only up to a closing parenthesis, over plain lines: one
				// never closed, or a list item or fence before the close,
				// leaves it judged alone, swallowing nothing after it.
				for k := i + 1; k < len(section); k++ {
					if listOrFence.MatchString(section[k]) {
						break
					}
					if strings.Contains(section[k], ")") {
						for j < k {
							j++
							t += " " + strings.TrimSpace(section[j])
						}
						break
					}
				}
			}
			if t == "DONE" || noQuestions.MatchString(t) || noneSaid.MatchString(t) && !noneAsks.MatchString(t) {
				i = j
				continue
			}
			// A section whose first word is none: its paragraph, the plain
			// lines up to a list item, a fence or a line asking a question,
			// asks nothing unless it asks the owner something (#69). It is
			// kept as the lead-in of a question after it.
			if i == 0 && noneFirst.MatchString(t) {
				k := j + 1
				for ; k < len(section) && !listOrFence.MatchString(section[k]) && !strings.HasSuffix(strings.TrimRight(section[k], ")*_ "), "?"); k++ {
					t += " " + section[k]
				}
				if !noneParagraphAsks.MatchString(t) {
					lead = section[:k]
					i = k - 1
					continue
				}
			}
			kept = append(kept, section[i])
		}
		if len(kept) > 0 {
			kept = append(lead, kept...)
		}
		if joined := strings.Join(kept, "\n"); joined != "" {
			found = append(found, joined)
		}
		section = nil
	}
	for _, line := range strings.Split(text, "\n") {
		if fence == "" {
			if m := questionsHeading.FindStringSubmatch(line); m != nil && (level == 0 || len(m[1]) <= level) {
				flush()
				level = len(m[1])
				continue
			}
			if m := anyHeading.FindStringSubmatch(line); m != nil && level > 0 && len(m[1]) <= level {
				flush()
				level = 0
				continue
			}
		}
		if f, rest := fenceOf(line); fences && f != "" {
			if fence == "" {
				fence = f
			} else if f[0] == fence[0] && len(f) >= len(fence) && strings.TrimSpace(rest) == "" {
				fence = ""
			}
		}
		l := strings.TrimSpace(line)
		if level == 0 || l == "" || thematicBreak.MatchString(strings.ReplaceAll(l, " ", "")) {
			continue
		}
		section = append(section, l)
	}
	flush()
	return strings.Join(found, "\n"), fence == ""
}

// fenceOf returns the code fence a line starts with, a run of three or more
// backticks or tildes indented at most three spaces, and the text after it.
func fenceOf(line string) (string, string) {
	l := strings.TrimLeft(line, " ")
	if len(line)-len(l) > 3 {
		return "", ""
	}
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(l) && l[n] == c {
			n++
		}
		if n >= 3 {
			return l[:n], l[n:]
		}
	}
	return "", ""
}

// HashPane hashes screen text, so the log never carries the screen itself.
func HashPane(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// The parts of a Claude Code screen that move while nothing else does: the
// spinner glyph and its rotating verb, elapsed-time counters, token counts
// and clock times. A session at a dialog under a running subagent shows
// all of them ticking. Timings in a session's own output, such as a test
// run's, read as fixed too: they are no progress of their own.
var (
	spinnerLine  = regexp.MustCompile(`(?m)^(\s*)[·✢✳✶✻✽*](\s+)[\p{L}-]+…`)
	elapsedCount = regexp.MustCompile(`\b(?:\d+h\s?)?(?:\d+m\s?)?\d+(?:\.\d+)?s\b|\b\d+h\s?\d+m\b`)
	tokenCount   = regexp.MustCompile(`[↑↓]?\s*\d[\d,]*(?:\.\d+)?k?\s+tokens\b`)
	// A clock time stands after a space or a bracket, so a file:line:col
	// position is left as it is.
	clockTime = regexp.MustCompile(`(^|[\s(\[])\d{1,2}:\d{2}(?::\d{2})?\b`)
)

// stillText is screen with what moves on its own replaced by fixed text,
// so a screen that differs only in its counters reads the same.
func stillText(screen string) string {
	screen = spinnerLine.ReplaceAllString(screen, "$1*$2…")
	screen = elapsedCount.ReplaceAllString(screen, "#s")
	screen = tokenCount.ReplaceAllString(screen, "# tokens")
	return clockTime.ReplaceAllString(screen, "$1#:#")
}

// StillHash hashes screen text without its counters and spinners: the hash
// stillness is judged by, and the one a typing action compares before it
// types.
func StillHash(screen string) string { return HashPane(stillText(screen)) }

// dialogOption is a dialog's numbered option, the selected one marked.
var dialogOption = regexp.MustCompile(`^(?:❯\s*)?(\d)\.\s+\S`)

// dialogShape says whether the screen shows a dialog's shape wherever it
// comes from, the main session or a subagent: a question line, not a quote
// or a session's own ⏺ line, followed within a few lines by options
// numbered 1 and 2, one of them under the ❯ cursor. A question the session
// asks in prose, with a numbered list after it, has no cursor.
func dialogShape(screen string) bool {
	lines := strings.Split(screen, "\n")
	for i := range lines {
		lines[i] = strings.Trim(lines[i], " \t│┃|")
	}
	for i, line := range lines {
		if !strings.HasSuffix(line, "?") || strings.HasPrefix(line, ">") || strings.HasPrefix(line, "⏺") {
			continue
		}
		next, cursor := 1, false
		for j := i + 1; j < len(lines) && j <= i+4; j++ {
			m := dialogOption.FindStringSubmatch(lines[j])
			if m == nil || m[1] != fmt.Sprint(next) {
				if next > 1 {
					break
				}
				continue
			}
			cursor = cursor || strings.HasPrefix(lines[j], "❯")
			if next++; next > 2 && cursor {
				return true
			}
		}
	}
	return false
}

// inputPrompt is Claude Code's input prompt, in its box or not, empty or
// showing its placeholder suggestion: text typed at it, a draft or a line
// whose Enter did not take, is not idle. promptRunning is the line a
// running turn shows.
var (
	inputPrompt   = regexp.MustCompile(`^[>❯›](\s+Try ".*)?$`)
	promptRunning = regexp.MustCompile(`(?i)esc to interrupt`)
)

// promptLines is how far from the screen's last line the prompt may be:
// the box's border and the mode line below it. A running turn's line is
// looked for promptAbove lines further up, where it shows above the box,
// so session output quoting it higher up does not hide the prompt.
const (
	promptLines = 6
	promptAbove = 4
)

// idlePrompt says whether the screen ends at the input prompt with no
// turn running: a prompt line among its last few lines, and no running
// turn's line near it. A quoted prompt higher up, in the
// scrollback, is not the prompt.
func idlePrompt(screen string) bool {
	var lines []string
	for _, l := range strings.Split(screen, "\n") {
		if l = strings.Trim(l, " \t│┃|"); l != "" {
			lines = append(lines, l)
		}
	}
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-promptLines-promptAbove; i-- {
		if promptRunning.MatchString(lines[i]) {
			return false
		}
	}
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-promptLines; i-- {
		if inputPrompt.MatchString(lines[i]) && !dialogOption.MatchString(strings.TrimLeft(lines[i], ">❯› ")) {
			return true
		}
	}
	return false
}

// shortHash abbreviates a hash for keys and the status table.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// TranscriptDir is where Claude Code keeps the transcripts of sessions run
// from cwd: every character that is not a letter or digit becomes "-".
func TranscriptDir(root, cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return filepath.Join(root, b.String())
}

// ReadTranscript finds the newest transcript under dir and reads its last
// turn from the tail of the file. Several sessions run from one cwd share a
// directory, so the newest file may be another session's; the evidence is
// reported as found, and used only as a sign of recent activity.
func ReadTranscript(dir string) TranscriptEvidence {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return TranscriptEvidence{}
	}
	var newest string
	var newestAt time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == "" || info.ModTime().After(newestAt) {
			newest, newestAt = filepath.Join(dir, e.Name()), info.ModTime()
		}
	}
	if newest == "" {
		return TranscriptEvidence{}
	}
	return lastTurn(newest)
}

const transcriptTail = 256 * 1024

// lastTurn reads the final user or assistant line of a transcript.
func lastTurn(path string) TranscriptEvidence {
	f, err := os.Open(path)
	if err != nil {
		return TranscriptEvidence{}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return TranscriptEvidence{}
	}
	start := info.Size() - transcriptTail
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return TranscriptEvidence{}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return TranscriptEvidence{}
	}
	lines := strings.Split(string(b), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if start > 0 && i == 0 {
			break // a partial first line
		}
		var line struct {
			Type      string `json:"type"`
			Timestamp string `json:"timestamp"`
			Message   struct {
				StopReason string `json:"stop_reason"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &line); err != nil {
			continue
		}
		if line.Type != "user" && line.Type != "assistant" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, line.Timestamp)
		if err != nil {
			continue
		}
		return TranscriptEvidence{Found: true, Role: line.Type, StopReason: line.Message.StopReason, At: at}
	}
	return TranscriptEvidence{}
}
