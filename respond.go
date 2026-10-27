package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Responding to an entry (#59): every line of a console is an entry, with
// an id made of what the log already holds, and the owner responds to one
// in its own context. The response names the entry's id only: Go fills
// in what it is about from the log (EntryRe), never the browser, so a
// "yes" means yes to that entry. An answer to a decision is recorded as
// the answer page records it, settling that decision only; the response
// is also an instruction carrying the entry, handed to the session that
// wrote it, or, when agentd wrote it, routed as a console reply is.

// EntryRe is the entry a response is about, as Go fills it from the log:
// its id, kind, the session it is of and when it came; its text as the
// page showed it, screened; and for a decision, its id, question and
// options.
type EntryRe struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Session  string    `json:"session,omitempty"`
	At       time.Time `json:"at"`
	Decision string    `json:"decision,omitempty"`
	Question string    `json:"question,omitempty"`
	Options  []string  `json:"options,omitempty"`
	Text     string    `json:"text,omitempty"`
}

// EntryAnswer is the structured answer a response gave a decision, as the
// session receives it: what AnswerPayload holds, without the decision's
// id, which re names, or agentd's own bookkeeping.
type EntryAnswer struct {
	Answer  *bool           `json:"answer,omitempty"`
	Choice  string          `json:"choice,omitempty"`
	Choices []string        `json:"choices,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
	Text    string          `json:"text,omitempty"`
}

// entryAnswer is a's answer as a response carries it, nil when a gives
// none that settles.
func entryAnswer(a AnswerPayload) *EntryAnswer {
	if !a.Binding() {
		return nil
	}
	return &EntryAnswer{Answer: a.Answer, Choice: a.Choice, Choices: a.Choices, Value: a.Value, Text: a.Text}
}

// Entry ids: a kind's prefix, then what the log identifies it by.
const (
	entryMessage     = "m:" // a message: a post, or a session's turn
	entryFeed        = "f:" // a feed line, or one of the old console's
	entryEscalation  = "e:" // agentd escalating a session
	entryInstruction = "i:" // the owner's own message
	entryDecision    = "d:" // a session's decision: d:SESSION:ID
)

// entryKey is an entry's id: prefix and the id the log gives it, or a
// hash of when it came and what it says, for a line the log gives none.
func entryKey(prefix, id string, at time.Time, text string) string {
	if id == "" {
		id = shortHash(HashPane(at.UTC().Format(time.RFC3339Nano) + "\n" + text))[:12]
	}
	return prefix + id
}

// decisionEntry is the entry id of session's decision id.
func decisionEntry(session, id string) string { return entryDecision + session + ":" + id }

// decisionOptions are what a decision of its type may be answered with,
// as re names them: none for a number, a date or a text.
func decisionOptions(d Decision) []string {
	switch d.Type {
	case DecisionYesNo:
		return []string{"yes", "no"}
	case DecisionChooseOne, DecisionChooseMany:
		var keys []string
		for _, o := range d.Options {
			keys = append(keys, o.Key)
		}
		return keys
	}
	return nil
}

// uiDecisionEntry is a session's decision as an entry of its console:
// open, or answered, with the answer.
type uiDecisionEntry struct {
	UIDecision
	At     time.Time
	Answer *AnswerRecord
}

// decisionEntries are the decisions of repo's sessions as entries: each
// open one, and each answered one the session still holds, by when it was
// first seen.
func (src *uiSource) decisionEntries(cfg *Config, rep Report, repo string) []uiDecisionEntry {
	secret := confidentialMatcher(cfg)
	var out []uiDecisionEntry
	for _, d := range DecisionsFrom(cfg, src.st, rep) {
		if d.Repository == repo {
			out = append(out, uiDecisionEntry{UIDecision: d, At: src.askedAt[d.Session+" "+d.ID]})
		}
	}
	for _, rr := range rep.Repositories {
		if uiRepoName(rr) != repo {
			continue
		}
		for _, c := range rr.Cases {
			t := src.st.Tracks[c.Session]
			if t == nil {
				continue
			}
			for _, id := range sortedKeys(t.Answers) {
				a := t.Answers[id]
				d, ok := t.decision(id)
				if !ok || !a.Binding() {
					continue
				}
				e := uiDecisionEntry{UIDecision: UIDecision{Session: c.Session, Repository: repo, Decision: d}, At: src.askedAt[c.Session+" "+id], Answer: &a}
				if d.Withheld || secret(decisionTexts(d)...) {
					e.Withheld = withheldSecret
					e.Decision = Decision{ID: d.ID, Type: d.Type, Blocking: d.Blocking}
				}
				if e.At.IsZero() {
					e.At = a.At
				}
				out = append(out, e)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// entryOf finds entry id of repo's console as the page shows it now:
// what it is about, the session that wrote it ("" for agentd or the
// owner), and the decision it is, when it is one.
func (src *uiSource) entryOf(cfg *Config, rep Report, repo, id string) (EntryRe, string, *Decision, error) {
	secret := confidentialMatcher(cfg)
	if strings.HasPrefix(id, entryDecision) {
		for _, d := range src.decisionEntries(cfg, rep, repo) {
			if decisionEntry(d.Session, d.ID) != id {
				continue
			}
			re := EntryRe{ID: id, Kind: "decision", Session: d.Session, At: d.At, Decision: d.ID}
			if d.Withheld == "" {
				re.Question, re.Options, re.Text = d.Question, decisionOptions(d.Decision), d.Context
			}
			dec := d.Decision
			return re, d.Session, &dec, nil
		}
		return EntryRe{}, "", nil, fmt.Errorf("%s has no decision %q on its console", repo, id)
	}
	for _, m := range src.messages[repo] {
		if m.Entry != id {
			continue
		}
		re := EntryRe{ID: id, Kind: m.Kind, Session: m.Session, At: m.At, Text: m.Text}
		if re.Session == "" {
			re.Session = m.Writer
		}
		if m.Text == "" || secret(m.Text) {
			re.Text = ""
		}
		return re, m.Writer, nil, nil
	}
	return EntryRe{}, "", nil, fmt.Errorf("%s's console holds no entry %q now", repo, id)
}

// respond records the owner's response to entry id of repo's console:
// an answer to it, when it is a decision and the response gives one,
// then the response as an instruction carrying the entry, for the
// session that wrote it, else routed as a reply to the console is.
func (src *uiSource) respond(repo, id, text string, a *AnswerPayload) error {
	src.mu.Lock()
	defer src.mu.Unlock()
	now := time.Now()
	cfg, rep, err := src.view(now)
	if err != nil {
		return err
	}
	re, writer, dec, err := src.entryOf(cfg, rep, repo, id)
	if err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	var answer *EntryAnswer
	if a != nil && dec != nil {
		a.ID = dec.ID
		if !a.Binding() && a.Other == "" && text != "" {
			// Words alone answer a text decision; any other they
			// answer in the owner's own words, which settles nothing.
			if dec.Type == DecisionText {
				a.Text = text
			} else {
				a.Other = text
			}
		}
		if a.Binding() && a.Text != text {
			a.Note = text
		}
		if a.Binding() || a.Other != "" {
			if err := RecordAnswer(src.log, src.st, now, writer, *a); err != nil {
				return err
			}
			answer = entryAnswer(*a)
		}
	} else if a != nil {
		return errors.New("only a decision takes an answer")
	}
	if text == "" && answer == nil {
		return errors.New("an empty response")
	}
	to := ""
	if writer != "" {
		if sess, ok := cfg.Sessions[writer]; ok && sess.Role == "" {
			to = writer
		}
	}
	if err := src.catchUp(); err != nil {
		return err
	}
	return respondInstruction(cfg, src.log, now, repo, to, text, &re, answer)
}

// respondInstruction records a response as the owner's instruction for
// repo, or for a console without one, addressed to session to when set.
func respondInstruction(cfg *Config, log *Log, now time.Time, repo, to, text string, re *EntryRe, answer *EntryAnswer) error {
	var repos []string
	console := repo
	if _, ok := cfg.Consoles[repo]; !ok {
		if _, ok := cfg.Repositories[repo]; !ok {
			return fmt.Errorf("no repository %q", repo)
		}
		repos, console = []string{repo}, ""
	}
	return recordInstructionRe(cfg, log, now, repos, console, to, "ui", text, re, answer)
}
