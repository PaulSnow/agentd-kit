package main

import (
	"errors"
	"fmt"
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
// sent twice are recorded once.
type InstructionPayload struct {
	ID           string   `json:"id"`
	Repositories []string `json:"repositories"`
	Text         string   `json:"text,omitempty"`
	Withheld     int      `json:"withheld,omitempty"`
	Hash         string   `json:"hash"`
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
// the same console, while an earlier copy is not yet handled by every
// coordinator it is for, are not recorded again.
func RecordInstruction(cfg *Config, log *Log, st *LogState, now time.Time, console, text string) error {
	var repos []string
	for name := range cfg.Repositories {
		if cfg.RepositoryConsole(name) == console {
			repos = append(repos, name)
		}
	}
	sort.Strings(repos)
	if len(repos) == 0 {
		return fmt.Errorf("console %q feeds no repository, so it takes no instruction", console)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("an empty instruction")
	}
	from := "console:" + console
	hash := shortHash(HashPane(text))
	for _, in := range st.Instructions {
		if in.Hash != hash || in.From != from || strings.Join(in.Repositories, ",") != strings.Join(repos, ",") {
			continue
		}
		for _, r := range repos {
			if !st.instructionHandled(cfg, r, in.ID, false) {
				return nil // still waiting: recorded once
			}
		}
	}
	p := &InstructionPayload{ID: "I" + shortHash(HashPane(now.UTC().Format(time.RFC3339Nano) + "\n" + from + "\n" + text))[:8],
		Repositories: repos, Text: text, Hash: hash, From: from}
	for i, pat := range cfg.ConfidentialPatterns {
		if regexp.MustCompile(pat).MatchString(text) {
			p.Text, p.Withheld = "", i+1
			break
		}
	}
	return log.Append(Event{V: EventVersion, Time: now, Kind: KindInstruction, Mode: ModeOwner, Instruction: p})
}
