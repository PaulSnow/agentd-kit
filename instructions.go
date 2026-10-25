package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The owner's reply in a console that is neither an answer nor a seen mark
// is an instruction for the repositories that console feeds (#33). It is
// due for each one's coordinator, which it wakes once, and its pack shows
// it until the coordinator has handled a wake that carried it.

// InstructionPayload is an owner instruction: its id, the repositories it
// is for, and its text, left out when it matches a confidential pattern
// (Withheld, from 1, says which). Hash is the text's, so the same words
// sent twice are recorded once; a withheld text has none. Console names
// a console without a repository (#42) the instruction came from: it is
// for no repository, so no coordinator, and inbox alone reads it.
type InstructionPayload struct {
	ID           string   `json:"id"`
	Repositories []string `json:"repositories"`
	Console      string   `json:"console,omitempty"`
	Text         string   `json:"text,omitempty"`
	Withheld     int      `json:"withheld,omitempty"`
	Hash         string   `json:"hash,omitempty"`
	From         string   `json:"from"`
}

// Instruction is an instruction as the log holds it, with when it came.
type Instruction struct {
	InstructionPayload
	At time.Time
}

// PackInstruction is an instruction as a coordinator's pack shows it.
type PackInstruction struct {
	ID       string    `json:"id"`
	Text     string    `json:"text,omitempty"`
	Withheld bool      `json:"withheld,omitempty"`
	From     string    `json:"from"`
	At       time.Time `json:"at"`
}

// instructionReason begins an instruction's due reason and key.
const instructionReason = "instruction: "

// decisionLike is a word that reads as a decision's id, with or without
// the version a fed line names.
var decisionLike = regexp.MustCompile(`^[A-Za-z]\d+(@\S*)?$`)

// instructionKey is the instruction id a due key names.
func instructionKey(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, instructionReason)
	if !ok {
		return "", false
	}
	id, _, _ := strings.Cut(rest, "@")
	return id, true
}

// instructionsFor is repo's instructions, oldest first.
func (st *LogState) instructionsFor(repo string) []*Instruction {
	var out []*Instruction
	for _, in := range st.Instructions {
		for _, r := range in.Repositories {
			if r == repo {
				out = append(out, in)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// instructionHandled says whether repo's coordinator has handled a wake
// that carried the instruction: one before its latest, or its latest when
// handled says it was.
func (st *LogState) instructionHandled(cfg *Config, repo, id string, handled bool) bool {
	co := cfg.Repositories[repo].Coordinator
	if co == nil {
		return false
	}
	t := st.Tracks[co.Session]
	if t == nil {
		return false
	}
	n, ok := t.Carried[id]
	return ok && (n < t.WakeCount || handled)
}

// instructionCarried says whether a wake of coordinator session carried
// the instruction.
func (st *LogState) instructionCarried(session, id string) bool {
	t := st.Tracks[session]
	if t == nil {
		return false
	}
	_, ok := t.Carried[id]
	return ok
}

// packInstructions is repo's instructions its coordinator has not handled.
func (s *Supervisor) packInstructions(repo string, handled bool) []PackInstruction {
	var out []PackInstruction
	for _, in := range s.st.instructionsFor(repo) {
		if !s.st.instructionHandled(s.cfg, repo, in.ID, handled) {
			out = append(out, PackInstruction{ID: in.ID, Text: in.Text, Withheld: in.Withheld > 0, From: in.From, At: in.At})
		}
	}
	return out
}

// RecordInstruction records text as the owner's instruction from console,
// for every repository that console feeds, screened. The same words from
// the same console, while a coordinator it is for has not yet been woken
// with an earlier copy, are not recorded again.
func RecordInstruction(cfg *Config, log *Log, st *LogState, now time.Time, console, text string) error {
	if _, ok := cfg.Consoles[console]; ok {
		return recordInstruction(cfg, log, st, now, nil, console, "console:"+console, text)
	}
	repos := cfg.consoleRepos(console)
	if len(repos) == 0 {
		return fmt.Errorf("console %q feeds no repository, so it takes no instruction", console)
	}
	return recordInstruction(cfg, log, st, now, repos, "", "console:"+console, text)
}

// UIInstruction records text as the owner's instruction from the UI for
// one repository (#41), or for a console without one (#42), as a console
// reply is recorded.
func UIInstruction(cfg *Config, log *Log, st *LogState, now time.Time, repo, text string) error {
	if _, ok := cfg.Consoles[repo]; ok {
		return recordInstruction(cfg, log, st, now, nil, repo, "ui", text)
	}
	if _, ok := cfg.Repositories[repo]; !ok {
		return fmt.Errorf("no repository %q", repo)
	}
	return recordInstruction(cfg, log, st, now, []string{repo}, "", "ui", text)
}

// recordInstruction records text as the owner's instruction for repos, or
// for console when it has none, screened, from where it was typed.
func recordInstruction(cfg *Config, log *Log, st *LogState, now time.Time, repos []string, console, from, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("an empty instruction")
	}
	p := &InstructionPayload{Repositories: repos, Console: console, Text: text, Hash: shortHash(HashPane(text)), From: from}
	for i, pat := range screenPatterns(cfg) {
		if regexp.MustCompile(pat).MatchString(text) {
			// Nothing of a withheld text is logged, not even its hash,
			// so it is never taken for a copy.
			p.Text, p.Hash, p.Withheld = "", "", i+1
			break
		}
	}
	p.ID = "I" + shortHash(HashPane(now.UTC().Format(time.RFC3339Nano) + "\n" + from + "\n" + p.Hash))[:8]
	for _, in := range st.Instructions {
		if p.Hash == "" || in.Hash != p.Hash || in.From != from || in.Console != console || strings.Join(in.Repositories, ",") != strings.Join(repos, ",") {
			continue
		}
		// A copy no wake has carried yet is still waiting. A repository
		// with no coordinator never carries it, so it keeps none waiting;
		// once a wake carried it, the same words are the owner's again.
		for _, r := range repos {
			if co := cfg.Repositories[r].Coordinator; co != nil && !st.instructionCarried(co.Session, in.ID) {
				return nil // still waiting: recorded once
			}
		}
	}
	return log.Append(Event{V: EventVersion, Time: now, Kind: KindInstruction, Mode: ModeOwner, Instruction: p})
}

// ImportLine is one message of the old console's history, as agentd
// console import reads it: one JSON object per line. The outside console
// program's lines (#42) give their time as TS, Unix seconds, and a Kind.
// Repository may name a console without a repository too.
type ImportLine struct {
	At         time.Time `json:"at"`
	TS         *float64  `json:"ts,omitempty"`
	From       string    `json:"from"`
	Kind       string    `json:"kind,omitempty"`
	Text       string    `json:"text"`
	Repository string    `json:"repository,omitempty"`
}

// importKey identifies an imported line, so a second import skips it.
func importKey(repo string, at time.Time, text string) string {
	return "imported:" + shortHash(HashPane(repo+"\n"+at.UTC().Format(time.RFC3339Nano)+"\n"+text))
}

// ImportConsole appends each line of r, the old console's history, as a
// feed event of phase imported for repo, or the line's own repository,
// with its time and author (#41). A line already imported, by repository,
// time and text, is skipped; every text is screened. Nothing is appended
// unless every line reads.
func ImportConsole(cfg *Config, log *Log, events []Event, now time.Time, repo string, r io.Reader) (added, skipped int, err error) {
	have := map[string]bool{}
	for _, e := range events {
		if e.Kind == KindFeed && e.Feed != nil && e.Feed.Phase == FeedImported {
			have[e.Feed.Key] = true
		}
	}
	var lines []ImportLine
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var l ImportLine
		dec := json.NewDecoder(strings.NewReader(text))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&l); err != nil {
			return 0, 0, fmt.Errorf("line %d: %w", n, err)
		}
		if l.Repository == "" {
			l.Repository = repo
		}
		_, repo := cfg.Repositories[l.Repository]
		if _, con := cfg.Consoles[l.Repository]; !repo && !con {
			return 0, 0, fmt.Errorf("line %d: no repository or console %q", n, l.Repository)
		}
		if l.TS != nil && l.At.IsZero() && *l.TS > 0 && *l.TS < 1e11 {
			sec, frac := math.Modf(*l.TS)
			l.At = time.Unix(int64(sec), int64(math.Round(frac*1e9))).UTC()
		}
		if l.At.IsZero() || strings.TrimSpace(l.Text) == "" {
			return 0, 0, fmt.Errorf("line %d: a line needs its time and text", n)
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	pats := screenPatterns(cfg)
	for _, l := range lines {
		key := importKey(l.Repository, l.At, l.Text)
		if have[key] {
			skipped++
			continue
		}
		have[key] = true
		at := l.At
		p := &FeedPayload{Phase: FeedImported, Key: key, Repository: l.Repository, Text: l.Text, At: &at, From: l.From, Kind: l.Kind}
		for i, pat := range pats {
			if regexp.MustCompile(pat).MatchString(l.Text + "\n" + l.From) {
				p.Text, p.From, p.Withheld = "", "", i+1
				break
			}
		}
		if err := log.Append(Event{V: EventVersion, Time: now, Kind: KindFeed, Mode: ModeOwner, Feed: p}); err != nil {
			return added, skipped, err
		}
		added++
	}
	return added, skipped, nil
}
