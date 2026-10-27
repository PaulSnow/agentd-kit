package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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
// policies.feed.turn_length says otherwise: a reply whole, as it is logged
// (#56); the page folds a long one.
const DefaultTurnLength = replyText

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
	case tn.Role != TurnPrompt && tn.Role != TurnCompact:
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
	m := &MessagePayload{ID: turnMessageID(name, tn.Transcript, tn.Line),
		Console: console, Kind: kind, From: name, Text: cutUTF8(tn.Text, s.cfg.Policies.Feed.TurnLength)}
	// The session's name stays, as it does in every event of the session.
	if i := s.confidentialMatch(tn.Text, name); i > 0 {
		m.Text, m.Withheld = "", i
	}
	// A compaction's summary is no prompt: the console says what it is,
	// and its page shows it (#58).
	if tn.Role == TurnCompact {
		m.Text, m.Withheld = compactPosted, 0
	}
	s.emit("", Event{Kind: KindMessage, Mode: ModeShared, Message: m})
}

// turnMessageID is the id of the console message session name's turn at
// line of transcript is posted as: the console finds the turn by it.
func turnMessageID(name, transcript string, line int) string {
	return "T" + shortHash(HashPane(name + "\n" + transcript + "\n" + fmt.Sprint(line)))[:8]
}

const (
	turnText  = 300         // bytes of a prompt logged, cut after it is screened whole
	replyText = 16 << 10    // bytes of a summary logged: a reply is shown whole (#56)
	turnRead  = 5 * 4 << 10 // bytes of a prompt or summary read, so a term across the cut is screened
	turnsKept = 16          // a session's latest turns its Track keeps
	turnsRead = 256         // turns a transcript reader holds until they are logged
)

// Turn roles.
const (
	TurnPrompt  = "prompt"  // what the session was asked
	TurnSummary = "summary" // the text it ended a turn with
	TurnTool    = "tool"    // a tool it ran: its one-line target and outcome (#56)
	TurnCompact = "compact" // the summary a compaction began its context with (#58): no prompt
)

// compactOpening is how Claude Code begins a compaction's summary, read
// as one where the line does not say so.
const compactOpening = "This session is being continued from a previous conversation"

// compactPosted is what the console is told of a compaction.
const compactPosted = "Context compacted."

// toolsPending bounds the tool calls a transcript reader holds awaiting
// their results, and toolsRead the tools run it holds until they are
// logged, apart from the prompts and summaries.
const (
	toolsPending = 64
	toolsRead    = 64
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
	// A tool's outcome: Failed when its result is an error, with the
	// exit code a command's result names.
	Failed bool `json:"failed,omitempty"`
	Exit   *int `json:"exit,omitempty"`
}

// TurnRecord is a turn as a Track holds it.
type TurnRecord struct {
	// Where it was read, so its page reads it whole (#58).
	Transcript string    `json:"transcript,omitempty"`
	Line       int       `json:"line,omitempty"`
	Role       string    `json:"role"`
	Text       string    `json:"text,omitempty"`
	Withheld   bool      `json:"withheld,omitempty"`
	At         time.Time `json:"at"`
}

// harnessLine is the start of a user entry Claude Code wrote itself, not a
// prompt anyone typed: a <task-notification>, a <system-reminder>, a
// <bash-input> or <bash-stdout>, a <command-name> and the like (#56):
// a text that opens with a tag it also closes, and ends with a closing
// tag, so nothing typed follows it.
var harnessLine = regexp.MustCompile(`^<([a-z][a-z0-9_-]*)>(?s:.*)</[a-z][a-z0-9_-]*>$`)

// injected says whether text is an entry the harness wrote, not a prompt.
func injected(text string) bool {
	m := harnessLine.FindStringSubmatch(text)
	return m != nil && strings.Contains(text, "</"+m[1]+">")
}

// turnOf reads a transcript line as a turn, its text cut at turnRead.
func turnOf(line []byte, n int) (TurnPayload, bool) {
	tn, ok := wholeTurn(line, n)
	tn.Text = cutUTF8(tn.Text, turnRead)
	return tn, ok
}

// wholeTurn reads a transcript line as a turn, its text whole: a
// main-chain user prompt in the user's own words (not a tool result, a
// meta line or an entry the harness injected), a compaction's summary,
// or a main-chain assistant line that ends its turn with text.
func wholeTurn(line []byte, n int) (TurnPayload, bool) {
	var l struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		IsMeta      bool   `json:"isMeta"`
		IsCompact   bool   `json:"isCompactSummary"`
		Timestamp   string `json:"timestamp"`
		Message     struct {
			StopReason string          `json:"stop_reason"`
			Content    json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil || l.IsSidechain || l.IsMeta && !l.IsCompact {
		return TurnPayload{}, false
	}
	var role string
	switch {
	case l.Type == "user" && l.IsCompact:
		role = TurnCompact
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
				// A block the harness injected beside a typed one is dropped,
				// not the typed one with it.
				if !injected(strings.TrimSpace(b.Text)) {
					parts = append(parts, b.Text)
				}
			case "tool_result", "tool_use":
				return TurnPayload{}, false
			}
		}
		text = strings.Join(parts, "\n")
	}
	if text = strings.TrimSpace(text); text == "" || role == TurnPrompt && injected(text) {
		return TurnPayload{}, false
	}
	if role == TurnPrompt && strings.HasPrefix(text, compactOpening) {
		role = TurnCompact
	}
	at, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
	return TurnPayload{Line: n, Role: role, Text: text, At: at}, true
}

// noteTurn keeps a turn read from the transcript until it is logged; the
// oldest go first when a transcript gains more than turnsRead at once. A
// tool call is kept by its id until its result says how it went.
func (f *transcriptFile) noteTurn(line []byte) {
	keep := func(tn TurnPayload) {
		if len(f.turns) == turnsRead {
			f.turns = f.turns[1:]
		}
		f.turns = append(f.turns, tn)
	}
	if tn, ok := turnOf(line, f.lines); ok {
		keep(tn)
	}
	for _, tn := range f.toolTurns(line) {
		if len(f.ran) == toolsRead {
			f.ran = f.ran[1:]
		}
		f.ran = append(f.ran, tn)
	}
}

// exitCode reads the exit code a failed command's result begins with.
var exitCode = regexp.MustCompile(`^Exit code (\d+)`)

// toolTurns reads a main-chain line's tool calls, kept until their
// results, and its tool results, each a tool turn: the call's one-line
// target and the outcome.
func (f *transcriptFile) toolTurns(line []byte) []TurnPayload {
	var l struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		Timestamp   string `json:"timestamp"`
		Message     struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil || l.IsSidechain || (l.Type != "user" && l.Type != "assistant") {
		return nil
	}
	var blocks []struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		IsError   bool            `json:"is_error"`
		Content   json.RawMessage `json:"content"`
	}
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return nil
	}
	at, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
	var out []TurnPayload
	for _, b := range blocks {
		switch {
		case b.Type == "tool_use" && b.ID != "":
			if f.tools == nil {
				f.tools = map[string]string{}
			}
			if len(f.tools) < toolsPending {
				f.tools[b.ID] = toolTarget(b.Name, b.Input)
			}
		case b.Type == "tool_result" && b.ToolUseID != "":
			target, ok := f.tools[b.ToolUseID]
			if !ok {
				continue
			}
			delete(f.tools, b.ToolUseID)
			tn := TurnPayload{Line: f.lines, Role: TurnTool, Text: target, At: at, Failed: b.IsError}
			if m := exitCode.FindStringSubmatch(resultText(b.Content)); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil {
					tn.Exit = &n
					tn.Failed = tn.Failed || n != 0
				}
			}
			out = append(out, tn)
		}
	}
	return out
}

// resultText is the start of a tool result's text, a string or blocks.
func resultText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil && len(blocks) > 0 {
		return blocks[0].Text
	}
	return ""
}

// toolTargetLength bounds a tool line's text.
const toolTargetLength = 160

// toolTarget is a tool call as one line, as read: the tool and what it ran on, the
// command's first line, the file, the pattern or the description.
func toolTarget(name string, input json.RawMessage) string {
	var in map[string]any
	json.Unmarshal(input, &in)
	target := ""
	for _, k := range []string{"command", "file_path", "notebook_path", "path", "pattern", "url", "query", "description", "skill"} {
		if v, ok := in[k].(string); ok && strings.TrimSpace(v) != "" {
			target = v
			break
		}
	}
	// Cut at what is read, not shown: the log cuts it at
	// toolTargetLength once it is screened whole.
	target, _, _ = strings.Cut(strings.TrimSpace(target), "\n")
	if target == "" {
		return cutUTF8(name, turnRead)
	}
	return cutUTF8(name+": "+target, turnRead)
}

// turns are the turns of transcript id in dir read so far, after line,
// in the transcript's order.
func (r *usageReader) turns(dir, id string, after int) []TurnPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.files[filepath.Join(dir, id+".jsonl")]
	if f == nil {
		return nil
	}
	var out []TurnPayload
	for _, list := range [][]TurnPayload{f.turns, f.ran} {
		for _, tn := range list {
			if tn.Line > after {
				tn.Transcript = id
				out = append(out, tn)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// applyTurn folds a logged turn into the session's Track. A tool turn
// only moves the line read: it is the instructions' activity, not a turn
// rotation or the nudges count. A compaction is kept to show, and counts
// as no turn.
func (t *Track) applyTurn(tn *TurnPayload) {
	if t.TurnLines == nil {
		t.TurnLines = map[string]int{}
	}
	if tn.Line > t.TurnLines[tn.Transcript] {
		t.TurnLines[tn.Transcript] = tn.Line
	}
	if tn.Role == TurnTool {
		return
	}
	if tn.Role != TurnCompact {
		t.TurnCount++
	}
	// A turn ended since the last nudge answered it, however soon after:
	// the idle clock and the nudge count start again from its end.
	if tn.Role == TurnSummary && t.Nudges > 0 && tn.At.After(t.LastNudge) {
		t.Nudges, t.HashSince = 0, tn.At
	}
	r := TurnRecord{Transcript: tn.Transcript, Line: tn.Line, Role: tn.Role, Text: tn.Text, Withheld: tn.Withheld > 0, At: tn.At}
	if len(t.Turns) == turnsKept {
		t.Turns = t.Turns[1:len(t.Turns):len(t.Turns)]
	}
	t.Turns = append(t.Turns[:len(t.Turns):len(t.Turns)], r)
}
