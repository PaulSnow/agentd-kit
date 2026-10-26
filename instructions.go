package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"
)

// The owner's reply in a console that is neither an answer nor a seen mark
// is an instruction for the repositories that console feeds (#33). It is
// recorded, and read with agentd console inbox; nothing types it into a
// session (#53).

// InstructionPayload is an owner instruction: its id, the repositories it
// is for, and its text, left out when it matches a confidential pattern
// (Withheld, from 1, says which). Hash is the text's, and makes the id; a
// withheld text has none.
// Console names a console without a repository (#42) the instruction came
// from: it is for no repository.
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

// InboxInstruction is an instruction as agentd console inbox shows it.
type InboxInstruction struct {
	ID       string    `json:"id"`
	Text     string    `json:"text,omitempty"`
	Withheld bool      `json:"withheld,omitempty"`
	From     string    `json:"from"`
	At       time.Time `json:"at"`
}

// decisionLike is a word that reads as a decision's id, with or without
// the version a fed line names.
var decisionLike = regexp.MustCompile(`^[A-Za-z]\d+(@\S*)?$`)

// RecordInstruction records text as the owner's instruction from console,
// for every repository that console feeds, screened. Each reply is
// recorded, the same words twice too: nothing carries an instruction to a
// session, so no earlier copy is waiting to be carried.
func RecordInstruction(cfg *Config, log *Log, now time.Time, console, text string) error {
	if _, ok := cfg.Consoles[console]; ok {
		return recordInstruction(cfg, log, now, nil, console, "console:"+console, text)
	}
	repos := cfg.consoleRepos(console)
	if len(repos) == 0 {
		return fmt.Errorf("console %q feeds no repository, so it takes no instruction", console)
	}
	return recordInstruction(cfg, log, now, repos, "", "console:"+console, text)
}

// UIInstruction records text as the owner's instruction from the UI for
// one repository (#41), or for a console without one (#42), as a console
// reply is recorded.
func UIInstruction(cfg *Config, log *Log, now time.Time, repo, text string) error {
	if _, ok := cfg.Consoles[repo]; ok {
		return recordInstruction(cfg, log, now, nil, repo, "ui", text)
	}
	if _, ok := cfg.Repositories[repo]; !ok {
		return fmt.Errorf("no repository %q", repo)
	}
	return recordInstruction(cfg, log, now, []string{repo}, "", "ui", text)
}

// recordInstruction records text as the owner's instruction for repos, or
// for console when it has none, screened, from where it was typed.
func recordInstruction(cfg *Config, log *Log, now time.Time, repos []string, console, from, text string) error {
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
	return log.Append(Event{V: EventVersion, Time: now, Kind: KindInstruction, Mode: ModeOwner, Instruction: p})
}

// ImportLine is one message of the old console's history, as agentd
// console import reads it: one JSON object per line. The outside console
// program's lines (#42) give their time as TS, Unix seconds, a Kind, and
// To, the session a line posted with --to was typed to; a field of theirs
// import does not use, such as edited, is ignored (#44). Repository may
// name a console without a repository too.
type ImportLine struct {
	At         time.Time `json:"at"`
	TS         *float64  `json:"ts,omitempty"`
	From       string    `json:"from"`
	Kind       string    `json:"kind,omitempty"`
	Text       string    `json:"text"`
	Repository string    `json:"repository,omitempty"`
	To         string    `json:"to,omitempty"`
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
		if err := json.Unmarshal([]byte(text), &l); err != nil {
			return 0, 0, fmt.Errorf("line %d: %w", n, err)
		}
		if l.Repository == "" {
			l.Repository = repo
		}
		_, isRepo := cfg.Repositories[l.Repository]
		if _, isConsole := cfg.Consoles[l.Repository]; !isRepo && !isConsole {
			return 0, 0, fmt.Errorf("line %d: no repository or console %q", n, l.Repository)
		}
		if l.TS != nil && l.At.IsZero() {
			if !(*l.TS > 0 && *l.TS < 1e11) {
				return 0, 0, fmt.Errorf("line %d: ts %v is not a time in Unix seconds", n, *l.TS)
			}
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
		p := &FeedPayload{Phase: FeedImported, Key: key, Repository: l.Repository, Text: l.Text, At: &at, From: l.From, Kind: l.Kind, To: l.To}
		for i, pat := range pats {
			if regexp.MustCompile(pat).MatchString(l.Text + "\n" + l.From + "\n" + l.To) {
				p.Text, p.From, p.To, p.Withheld = "", "", "", i+1
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
