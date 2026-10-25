package main

import (
	"fmt"
	"time"
)

// Session turns on consoles (#38): each prompt a session was given and the
// text it ended a turn with, read from its transcript for the coordinator's
// pack (#31), is also posted to the session's console as a message (#42),
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
		if co := c.Repositories[name].Coordinator; co != nil {
			check("repositories."+name+".coordinator.policy.feed.turns", co.Policy.Feed.Turns)
		}
	}
	for _, name := range sortedKeys(c.Sessions) {
		if c.Sessions[name].Role != RoleCoordinator {
			check("sessions."+name+".policy.feed.turns", c.Sessions[name].Policy.Feed.Turns)
		}
	}
	if n := c.Policies.Feed.TurnLength; n < 40 || n > turnRead {
		*errs = append(*errs, fmt.Errorf("policies.feed.turn_length %d is not between 40 and %d", n, turnRead))
	}
}

// TurnsOf is which turns of session name its console receives: its own
// rule; else none of a coordinator's, whose turns its pack already sums
// up; else its repository's; else all.
func (c *Config) TurnsOf(name string) string {
	sess := c.Sessions[name]
	if v := sess.Policy.Feed.Turns; v != "" {
		return v
	}
	if sess.Role == RoleCoordinator {
		return TurnsOff
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
