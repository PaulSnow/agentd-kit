package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	Alive       bool
	PaneHash    string
	Limited     bool
	TrustPrompt bool
	Status      StatusEvidence
	Transcript  TranscriptEvidence
}

// StatusEvidence is what the session's STATUS.md says.
type StatusEvidence struct {
	Exists    bool
	Head      string
	Done      bool
	Questions string // the Questions section body when it asks something, else ""
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

// A STATUS.md may hold several Questions sections (one per issue the session
// works through). Each is judged on its own: a section that says "none" asks
// nothing, a section ends only at a heading of its own level or higher (a
// subheading under it is part of the question), and rules and blank lines
// around the text do not change the escalation key. Lines inside a fenced
// code block are content, never headings.
var questionsHeading = regexp.MustCompile(`(?i)^(#+)\s*Questions\s*$`)
var anyHeading = regexp.MustCompile(`^(#+)\s`)
var thematicBreak = regexp.MustCompile(`^(-{3,}|\*{3,}|_{3,})$`)

// noQuestions matches the ways a session says it has none: "none", "- none",
// "(none yet)", "None.", "n/a".
var noQuestions = regexp.MustCompile(`(?i)^[-*\s]*\(?\s*(none(\s+yet)?|n/a)\s*\.?\)?\s*$`)

// ParseStatus interprets STATUS.md text.
func ParseStatus(text string) StatusEvidence {
	ev := StatusEvidence{Exists: true}
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
	var questions, section []string
	level := 0  // the open Questions section's heading level, or 0 outside one
	fence := "" // the fence the line is inside, such as "```", or ""
	flush := func() {
		if joined := strings.Join(section, "\n"); joined != "" && !noQuestions.MatchString(joined) {
			questions = append(questions, joined)
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
		if f, rest := fenceOf(line); f != "" {
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
	ev.Questions = strings.Join(questions, "\n")
	return ev
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
