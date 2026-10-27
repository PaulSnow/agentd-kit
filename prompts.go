package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A prompt's own page (#58): every prompt the console and a session's
// page show links to it, and it reads the prompt whole from the
// transcript when it is asked, by the turn's transcript and line, as a
// session's activity is read: never logged, screened whole, withheld
// whole on a match. When agentd's prompt points the session at files,
// the page shows them as they are now, each a regular file under the
// session's workdir, screened whole: what the session was actually told.

const (
	promptLineMax = 8 << 20 // bytes of a transcript line read as a prompt
	promptFileMax = 1 << 20 // bytes of a file a prompt points at shown
	promptLead    = 200     // bytes of a prompt's first sentence on a line
)

// promptRef is where a turn was read: its transcript and line.
type promptRef struct {
	Transcript string
	Line       int
	Role       string
}

// ID is the ref as the prompt page's path names it.
func (r promptRef) ID() string { return r.Transcript + "." + strconv.Itoa(r.Line) }

// transcriptID is a transcript's name as a prompt page's path may give it.
var transcriptID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// parsePromptID reads a prompt page's id, TRANSCRIPT.LINE.
func parsePromptID(id string) (promptRef, bool) {
	tr, n, ok := strings.Cut(id, ".")
	line, err := strconv.Atoi(n)
	if !ok || err != nil || line < 1 || !transcriptID.MatchString(tr) || strconv.Itoa(line) != n {
		return promptRef{}, false
	}
	return promptRef{Transcript: tr, Line: line}, true
}

// promptLabel is how a prompt shows on a line: Lead, its first sentence,
// then a link saying Link; agentd's own prompts say what they are
// instead, with no lead.
type promptLabel struct {
	Lead string
	Link string
}

// labelPrompt is the line for a prompt of role with text, which may be
// cut, under the policies pol.
func labelPrompt(pol Policies, role, text string) promptLabel {
	text = strings.TrimSpace(text)
	if role == TurnCompact || text == compactPosted || strings.HasPrefix(text, compactOpening) {
		return promptLabel{Link: "Context compacted"}
	}
	if rest, ok := strings.CutPrefix(text, DefaultRestartPrompt); ok {
		_, why := restartOf(pol, rest)
		label := "Restarted"
		if why != "" {
			label += " (" + why + ")"
		}
		return promptLabel{Link: label + ": " + strings.Join(restartFiles(why), ", ")}
	}
	for _, own := range []struct{ line, label string }{
		{pol.Answers.Pointer, "Answers delivered"},
		{pol.Instructions.Pointer, "Your message delivered"},
		{pol.Nudge.Message, "Nudged to carry on"},
		{pol.Nudge.ContinueMessage, "Told to go on: STATUS.md is not DONE"},
		{pol.Nudge.ResumeMessage, "Resumed after the usage limit"},
		{pol.CI.Pointer, "CI failed: fix it"},
	} {
		if own.line != "" && (strings.HasPrefix(text, own.line) || len(text) >= 40 && strings.HasPrefix(own.line, strings.TrimSuffix(text, "…"))) {
			return promptLabel{Link: own.label}
		}
	}
	return promptLabel{Lead: firstSentence(text), Link: "full prompt"}
}

// leadEnd is the end of a prompt's first sentence.
var leadEnd = regexp.MustCompile(`[.?!](\s|$)|\n`)

// firstSentence is text's first sentence, bounded.
func firstSentence(text string) string {
	if loc := leadEnd.FindStringIndex(text); loc != nil {
		text = text[:loc[0]+1]
	}
	return strings.TrimSpace(cutUTF8(text, promptLead))
}

// whyFact is why a restart's facts say it was, read from facts cut.
var whyFact = regexp.MustCompile(`\{"why":"([a-z-]{1,40})"`)

// restartOf reads what follows the restart prompt: the facts, when they
// are there whole, and why the session was restarted, from them or from
// the pointers before them.
func restartOf(pol Policies, rest string) (*restartFacts, string) {
	var facts *restartFacts
	if i := strings.Index(rest, `{"why"`); i >= 0 {
		var f restartFacts
		if json.NewDecoder(strings.NewReader(rest[i:])).Decode(&f) == nil {
			facts = &f
		}
	}
	if facts != nil && facts.Why != "" {
		return facts, facts.Why
	}
	// Facts cut where the log cut the prompt still name why first.
	if m := whyFact.FindStringSubmatch(rest); m != nil {
		return facts, m[1]
	}
	rest = strings.TrimSpace(rest)
	starts := func(pointer string) bool {
		return pointer != "" && rest != "" && (strings.HasPrefix(rest, pointer) || strings.HasPrefix(pointer, strings.TrimSuffix(rest, "…")))
	}
	switch {
	case starts(pol.Answers.Pointer):
		return facts, "answered"
	case starts(pol.Instructions.Pointer):
		return facts, "instructed"
	case starts(pol.CI.Pointer):
		return facts, "ci-fix"
	case starts(FixRoundPointer):
		return facts, "fix-round"
	}
	return facts, ""
}

// restartFiles names the files a restart prompt points at.
func restartFiles(why string) []string {
	files := []string{"brief", "progress"}
	if strings.Contains(why, "answered") {
		files = append(files, "answers")
	}
	if strings.Contains(why, "instructed") {
		files = append(files, "instructions")
	}
	return files
}

// uiPromptPage is a prompt's page: the prompt whole, read now, and the
// files it points at as they are now.
type uiPromptPage struct {
	uiPage
	Src     string
	Session string
	Label   string
	At      time.Time
	Note    string        // why the prompt is not shown, when it is not
	Body    template.HTML // the prompt, rendered
	Facts   template.HTML // a restart's facts, pretty
	Files   []uiPromptFile
}

// uiPromptFile is a file a prompt points at, as the page shows it.
type uiPromptFile struct {
	Name string // brief, progress, answers or instructions
	Path string // as the prompt names it
	Body template.HTML
	Note string // why it is not shown, when it is not
}

// promptView is the prompt id of session name of src, nil when there is
// no such session or id.
func (u *UI) promptView(src *uiSource, name, id string, now time.Time) (*uiPromptPage, error) {
	ref, ok := parsePromptID(id)
	if !ok {
		return nil, nil
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	cfg, _, err := src.view(now)
	if err != nil {
		return nil, err
	}
	sess, ok := cfg.Sessions[name]
	if !ok {
		return nil, nil
	}
	v := &uiPromptPage{uiPage: uiPage{Title: name + ": prompt", Token: u.token, Back: "/session/" + src.Name + "/" + name + "/prompt/" + id, Now: now, loc: u.loc},
		Src: src.Name, Session: name, Label: "Prompt"}
	root := transcriptRootOf(cfg, sess)
	if root == "" || sess.Cwd == "" {
		v.Note = "No transcript of this session is readable here."
		return v, nil
	}
	line, err := transcriptLine(filepath.Join(TranscriptDir(root, sess.Cwd), ref.Transcript+".jsonl"), ref.Line)
	tn, ok := wholeTurn(line, ref.Line)
	switch {
	case errors.Is(err, os.ErrNotExist):
		v.Note = "The transcript is gone."
		return v, nil
	case err != nil:
		v.Note = "The transcript could not be read: " + err.Error()
		return v, nil
	case !ok || tn.Role != TurnPrompt && tn.Role != TurnCompact:
		v.Note = "That line of the transcript is no prompt."
		return v, nil
	}
	secret := confidentialMatcher(cfg)
	v.At = tn.At
	if secret(tn.Text) {
		v.Note = "The prompt was withheld because it matched a confidential pattern."
		return v, nil
	}
	pol := cfg.Policies
	l := labelPrompt(pol, tn.Role, tn.Text)
	if l.Lead == "" {
		v.Label = l.Link
	}
	text, files := tn.Text, []uiPromptFile(nil)
	if rest, ok := strings.CutPrefix(text, DefaultRestartPrompt); ok {
		facts, why := restartOf(pol, rest)
		if i := strings.Index(rest, `{"why"`); i >= 0 && facts != nil {
			text = DefaultRestartPrompt + rest[:i]
			v.Facts = prettyJSON([]byte(rest[i:]))
		}
		if facts != nil {
			files = append(files, uiPromptFile{Name: "brief", Path: facts.Brief}, uiPromptFile{Name: "progress", Path: facts.Progress})
		}
		if strings.Contains(why, "answered") {
			files = append(files, uiPromptFile{Name: "answers", Path: AnswersFile})
		}
		if strings.Contains(why, "instructed") {
			files = append(files, uiPromptFile{Name: "instructions", Path: InstructionsFile})
		}
	} else {
		switch l.Link {
		case "Answers delivered":
			files = append(files, uiPromptFile{Name: "answers", Path: AnswersFile})
		case "Your message delivered":
			files = append(files, uiPromptFile{Name: "instructions", Path: InstructionsFile})
		}
	}
	v.Body = markdown(text)
	for _, f := range files {
		v.Files = append(v.Files, promptFile(sess.Workdir, f, secret))
	}
	return v, nil
}

// promptFile reads f, as it is now, for a prompt's page: only a regular
// file under workdir, reached through no link, screened whole.
func promptFile(workdir string, f uiPromptFile, secret func(...string) bool) uiPromptFile {
	path, err := underWorkdir(workdir, f.Path)
	if err != nil {
		f.Note = err.Error()
		return f
	}
	b, err := readRegular(path, promptFileMax+1)
	switch {
	case errors.Is(err, os.ErrNotExist):
		f.Note = "There is no such file now."
	case err != nil:
		f.Note = "It could not be read: " + err.Error()
	case len(b) > promptFileMax:
		f.Note = fmt.Sprintf("It is larger than %d KiB, so it is not shown.", promptFileMax>>10)
	case secret(string(b)):
		f.Note = "It was withheld because it matched a confidential pattern."
	case strings.EqualFold(filepath.Ext(path), ".json"):
		f.Body = prettyJSON(b)
	case strings.EqualFold(filepath.Ext(path), ".md"):
		f.Body = markdown(string(b))
	default:
		f.Body = template.HTML("<pre>" + template.HTMLEscapeString(string(b)) + "</pre>")
	}
	return f
}

// underWorkdir is path, relative to workdir or absolute, when it names a
// file under workdir that no link on the way leads to.
func underWorkdir(workdir, path string) (string, error) {
	if workdir == "" || strings.TrimSpace(path) == "" {
		return "", errors.New("It is not named, or the session has no workdir.")
	}
	workdir = filepath.Clean(workdir)
	if !filepath.IsAbs(path) {
		path = filepath.Join(workdir, path)
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(workdir, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("It is not under the session's workdir, so it is not shown.")
	}
	// Each directory on the way is a directory, not a link; the file
	// itself readRegular opens only when it is no link.
	dir := workdir
	parts := strings.Split(rel, string(filepath.Separator))
	for _, p := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, p)
		if fi, err := os.Lstat(dir); err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return "", errors.New("It is reached through a link or no directory, so it is not shown.")
		}
	}
	return path, nil
}

// prettyJSON renders a JSON document indented, or as it is when it is
// not one.
func prettyJSON(b []byte) template.HTML {
	var out bytes.Buffer
	if err := json.Indent(&out, bytes.TrimSpace(b), "", "  "); err != nil {
		out.Reset()
		out.Write(b)
	}
	return template.HTML(`<pre class="json">` + template.HTMLEscapeString(out.String()) + "</pre>")
}

// transcriptLine reads line n of the transcript at path, counted as the
// transcript reader counts them: each line a newline ends, from 1.
func transcriptLine(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	for i := 1; ; i++ {
		var line []byte
		for {
			chunk, err := r.ReadSlice('\n')
			if i == n {
				if len(line)+len(chunk) > promptLineMax {
					return nil, fmt.Errorf("the line is larger than %d MiB", promptLineMax>>20)
				}
				line = append(line, chunk...)
			}
			if err == nil {
				break
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			if err == io.EOF {
				return nil, errors.New("the transcript has no such line")
			}
			return nil, err
		}
		if i == n {
			return bytes.TrimSuffix(line, []byte("\n")), nil
		}
	}
}
