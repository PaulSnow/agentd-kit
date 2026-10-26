package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Session turns on consoles (#38): each prompt a session was given and the
// text it ended a turn with is read from its transcript (#31), kept on its
// Track for rotation and the views, and posted to the session's console as a message (#42),
// so no outside program need tail the transcripts. A turn is posted when it
// is logged, which is once by its line, so a restart of agentd posts none
// again; the message's id comes from the session, transcript and line.

// Which turns a session's console receives.
const (
	TurnsOff     = "off"
	TurnsPrompts = "prompts" // the prompts only
	TurnsAll     = "all"     // the prompts and the replies
)

// Message kinds of a posted turn. Only agentd posts them.
const (
	MessagePrompt = "prompt"
	MessageReply  = "reply"
)

// validMessageKind says whether a message event's kind is a post's or a
// turn's.
func validMessageKind(k string) bool {
	return messageKinds[k] || k == MessagePrompt || k == MessageReply
}

// turnBacklog is how old a turn may be when it is read and still be
// posted: a transcript agentd reads for the first time, of a session it
// did not launch or one configured since, logs its earlier turns but
// does not post them all at once.
const turnBacklog = time.Hour

// FeedRules is a repository's or a session's say over its console: Turns
// is off, prompts or all, and empty leaves it to the next rule.
type FeedRules struct {
	Turns string `json:"turns"`
}

// SessionPolicy is a session's own rules, beside its repository's.
type SessionPolicy struct {
	Feed FeedRules `json:"feed"`
}

// DefaultTurnLength is how much of a turn is posted, in bytes, unless
// policies.feed.turn_length says otherwise.
const DefaultTurnLength = 300

// checkTurns checks every turns rule and the turn length, which can be no
// more than what is read of a turn.
func (c *Config) checkTurns(errs *[]error) {
	check := func(field, v string) {
		switch v {
		case "", TurnsOff, TurnsPrompts, TurnsAll:
		default:
			*errs = append(*errs, fmt.Errorf("%s %q is not off, prompts or all", field, v))
		}
	}
	for _, name := range sortedKeys(c.Repositories) {
		check("repositories."+name+".policy.feed.turns", c.Repositories[name].Policy.Feed.Turns)
	}
	for _, name := range sortedKeys(c.Sessions) {
		check("sessions."+name+".policy.feed.turns", c.Sessions[name].Policy.Feed.Turns)
	}
	if n := c.Policies.Feed.TurnLength; n < 40 || n > turnRead {
		*errs = append(*errs, fmt.Errorf("policies.feed.turn_length %d is not between 40 and %d", n, turnRead))
	}
}

// TurnsOf is which turns of session name its console receives: its own
// rule; else its repository's; else all.
func (c *Config) TurnsOf(name string) string {
	sess := c.Sessions[name]
	if v := sess.Policy.Feed.Turns; v != "" {
		return v
	}
	if v := c.Repositories[sess.Repository].Policy.Feed.Turns; v != "" {
		return v
	}
	return TurnsAll
}

// postTurn posts a turn of session name to its console, as its rule says,
// once the turn is logged: from the text read, screened whole, then cut at
// the turn length. A crash between the two loses the post; it never
// repeats it. A turn read more than turnBacklog after it was written is
// not posted. Shadow mode posts nothing, since a message is shared
// and the live run posts the same turn; nor does a session whose console
// neither a repository nor a consoles entry has, which no page shows.
func (s *Supervisor) postTurn(name string, tn TurnPayload) {
	kind := MessagePrompt
	switch rule := s.cfg.TurnsOf(name); {
	case s.shadow || rule == TurnsOff:
		return
	case tn.Role == TurnSummary && rule == TurnsAll:
		kind = MessageReply
	case tn.Role != TurnPrompt:
		return
	}
	// A turn without a time is as old as a backlog's, and so is one
	// written while agentd was stopped for longer than turnBacklog.
	if s.clock.Now().Sub(tn.At) > turnBacklog {
		return
	}
	console := s.cfg.Console(name)
	if !s.cfg.knownConsole(console) {
		return
	}
	m := &MessagePayload{ID: "T" + shortHash(HashPane(name + "\n" + tn.Transcript + "\n" + fmt.Sprint(tn.Line)))[:8],
		Console: console, Kind: kind, From: name, Text: cutUTF8(tn.Text, s.cfg.Policies.Feed.TurnLength)}
	// The session's name stays, as it does in every event of the session.
	if i := s.confidentialMatch(tn.Text, name); i > 0 {
		m.Text, m.Withheld = "", i
	}
	s.emit("", Event{Kind: KindMessage, Mode: ModeShared, Message: m})
}

const (
	turnText  = 300          // bytes of a prompt or summary logged, cut after it is screened whole
	turnRead  = 4 * turnText // bytes of a prompt or summary read, so a term across the cut is screened
	turnsKept = 16           // a session's latest turns its Track keeps
	turnsRead = 256          // turns a transcript reader holds until they are logged
)

// Turn roles.
const (
	TurnPrompt  = "prompt"  // what the session was asked
	TurnSummary = "summary" // the text it ended a turn with
)

// TurnPayload is one prompt or end-of-turn summary of a session's
// transcript, at its line, so each is logged once however often the
// transcript is read. Text is bounded; Withheld is the number of the
// confidential pattern it matched, when it did, and Text is then empty.
type TurnPayload struct {
	Transcript string    `json:"transcript"`
	Line       int       `json:"line"`
	Role       string    `json:"role"`
	Text       string    `json:"text,omitempty"`
	At         time.Time `json:"at"`
	Withheld   int       `json:"withheld,omitempty"`
}

// TurnRecord is a turn as a Track holds it.
type TurnRecord struct {
	Role     string    `json:"role"`
	Text     string    `json:"text,omitempty"`
	Withheld bool      `json:"withheld,omitempty"`
	At       time.Time `json:"at"`
}

// turnOf reads a transcript line as a turn: a main-chain user prompt in
// the user's own words (not a tool result or a meta line), or a main-chain
// assistant line that ends its turn with text.
func turnOf(line []byte, n int) (TurnPayload, bool) {
	var l struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		IsMeta      bool   `json:"isMeta"`
		Timestamp   string `json:"timestamp"`
		Message     struct {
			StopReason string          `json:"stop_reason"`
			Content    json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil || l.IsSidechain || l.IsMeta {
		return TurnPayload{}, false
	}
	var role string
	switch {
	case l.Type == "user":
		role = TurnPrompt
	case l.Type == "assistant" && l.Message.StopReason == "end_turn":
		role = TurnSummary
	default:
		return TurnPayload{}, false
	}
	var text string
	if json.Unmarshal(l.Message.Content, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			return TurnPayload{}, false
		}
		var parts []string
		for _, b := range blocks {
			switch b.Type {
			case "text":
				parts = append(parts, b.Text)
			case "tool_result", "tool_use":
				return TurnPayload{}, false
			}
		}
		text = strings.Join(parts, "\n")
	}
	if text = strings.TrimSpace(text); text == "" {
		return TurnPayload{}, false
	}
	at, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
	return TurnPayload{Line: n, Role: role, Text: cutUTF8(text, turnRead), At: at}, true
}

// noteTurn keeps a turn read from the transcript until it is logged; the
// oldest go first when a transcript gains more than turnsRead at once.
func (f *transcriptFile) noteTurn(line []byte) {
	if tn, ok := turnOf(line, f.lines); ok {
		if len(f.turns) == turnsRead {
			f.turns = f.turns[1:]
		}
		f.turns = append(f.turns, tn)
	}
}

// turns are the turns of transcript id in dir read so far, after line.
func (r *usageReader) turns(dir, id string, after int) []TurnPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.files[filepath.Join(dir, id+".jsonl")]
	if f == nil {
		return nil
	}
	var out []TurnPayload
	for _, tn := range f.turns {
		if tn.Line > after {
			tn.Transcript = id
			out = append(out, tn)
		}
	}
	return out
}

// applyTurn folds a logged turn into the session's Track.
func (t *Track) applyTurn(tn *TurnPayload) {
	if t.TurnLines == nil {
		t.TurnLines = map[string]int{}
	}
	if tn.Line > t.TurnLines[tn.Transcript] {
		t.TurnLines[tn.Transcript] = tn.Line
	}
	t.TurnCount++
	// A turn ended since the last nudge answered it, however soon after:
	// the idle clock and the nudge count start again from its end.
	if tn.Role == TurnSummary && t.Nudges > 0 && tn.At.After(t.LastNudge) {
		t.Nudges, t.HashSince = 0, tn.At
	}
	r := TurnRecord{Role: tn.Role, Text: tn.Text, Withheld: tn.Withheld > 0, At: tn.At}
	if len(t.Turns) == turnsKept {
		t.Turns = t.Turns[1:len(t.Turns):len(t.Turns)]
	}
	t.Turns = append(t.Turns[:len(t.Turns):len(t.Turns)], r)
}
