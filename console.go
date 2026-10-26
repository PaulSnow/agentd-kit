package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Console posts and inbox (#42): the sessions' reports
// to a console, and their reading of the owner's replies there, go through
// agentd, so no outside console program is needed. A post is a shared
// message event in the configuration's log; the UI shows it in its
// console. The owner's replies are the instructions RecordInstruction
// logs; inbox prints those a reader has not read and records them read.
// A console without a repository is declared under consoles: it is
// listed by the UI, takes posts and replies, and its replies are
// instructions for no repository.

// ConsoleConfig is a console that is not a repository's: its title, and
// the globs of the sessions whose escalations go to it when they name no
// repository.
type ConsoleConfig struct {
	Title    string   `json:"title"`
	Sessions []string `json:"sessions"`
}

// MessagePayload is a post to a console: its id, kind and author, and its
// text, left out when it matches a confidential pattern (Withheld, from
// 1, says which). To is the tmux session it was also typed into.
type MessagePayload struct {
	ID       string `json:"id"`
	Console  string `json:"console"`
	Kind     string `json:"kind"`
	From     string `json:"from"`
	Text     string `json:"text,omitempty"`
	Withheld int    `json:"withheld,omitempty"`
	To       string `json:"to,omitempty"`
	Typed    bool   `json:"typed,omitempty"`
}

// InboxPayload records that Reader read the instructions IDs of Console.
type InboxPayload struct {
	Console string   `json:"console"`
	Reader  string   `json:"reader"`
	IDs     []string `json:"ids"`
}

// MessageLimit is the longest post, in bytes.
const MessageLimit = 16 << 10

// messageKinds are the kinds a post may have; info is the default.
var messageKinds = map[string]bool{"info": true, "decision": true, "delivered": true, "ci": true, "finished": true}

// MessagePost is what agentd console post was asked to record.
type MessagePost struct {
	Console string
	Kind    string
	From    string
	Text    string
	To      string
}

// checkConsoles checks the consoles without a repository: a name no
// repository or repository's console has, and globs that compile. Without
// an escalation command the UI is the only place an escalation shows, so
// a session must escalate to a console it lists.
func (c *Config) checkConsoles(errs *[]error) {
	if len(c.Policies.Escalate.Command) == 0 {
		for _, name := range sortedKeys(c.Sessions) {
			if con := c.Console(name); !c.knownConsole(con) {
				*errs = append(*errs, fmt.Errorf("sessions.%s escalates to console %q, which no repository or consoles entry has; without policies.escalate.command its escalations would show nowhere", name, con))
			}
		}
	}
	for name, con := range c.Consoles {
		if name == "" || strings.TrimSpace(name) != name {
			*errs = append(*errs, fmt.Errorf("consoles: %q is not a console name", name))
			continue
		}
		for repo := range c.Repositories {
			if repo == name || c.RepositoryConsole(repo) == name {
				*errs = append(*errs, fmt.Errorf("consoles.%s: repository %s's console has that name", name, repo))
				break
			}
		}
		for i, g := range con.Sessions {
			if _, err := path.Match(g, ""); err != nil || g == "" {
				*errs = append(*errs, fmt.Errorf("consoles.%s.sessions[%d] %q is not a glob", name, i, g))
			}
		}
	}
}

// consoleOf is the console without a repository whose sessions name
// matches, the first by name, or "".
func (c *Config) consoleOf(session string) string {
	names := make([]string, 0, len(c.Consoles))
	for n := range c.Consoles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, g := range c.Consoles[n].Sessions {
			if ok, _ := path.Match(g, session); ok {
				return n
			}
		}
	}
	return ""
}

// consoleRepos are the repositories whose console is name, sorted.
func (c *Config) consoleRepos(name string) []string {
	var repos []string
	for repo := range c.Repositories {
		if c.RepositoryConsole(repo) == name {
			repos = append(repos, repo)
		}
	}
	sort.Strings(repos)
	return repos
}

// knownConsole says whether name is a repository's console or one
// without a repository.
func (c *Config) knownConsole(name string) bool {
	if _, ok := c.Consoles[name]; ok {
		return true
	}
	return len(c.consoleRepos(name)) > 0
}

// PostMessage records p as a message event, screened, after typing its
// text into p.To when it names a session: by its exact name, only into a
// pane running Claude Code that shows no dialog, as a nudge is typed.
// Nothing is recorded when the post or the typing is refused.
func PostMessage(cfg *Config, log *Log, tmux Tmux, now time.Time, p MessagePost) error {
	if !cfg.knownConsole(p.Console) {
		return fmt.Errorf("no console %q", p.Console)
	}
	if p.Kind == "" {
		p.Kind = "info"
	}
	if !messageKinds[p.Kind] {
		return fmt.Errorf("kind %q is not info, decision, delivered, ci or finished", p.Kind)
	}
	text := strings.TrimSpace(p.Text)
	if text == "" {
		return errors.New("an empty post")
	}
	if len(text) > MessageLimit {
		return fmt.Errorf("a post of %d bytes; at most %d", len(text), MessageLimit)
	}
	if p.From == "" {
		p.From = "cli"
	}
	m := &MessagePayload{Console: p.Console, Kind: p.Kind, From: p.From, Text: text}
	for i, pat := range screenPatterns(cfg) {
		if regexp.MustCompile(pat).MatchString(text + "\n" + p.From) {
			m.Text, m.From, m.Withheld = "", "", i+1
			break
		}
	}
	m.ID = "M" + shortHash(HashPane(now.UTC().Format(time.RFC3339Nano) + "\n" + p.Console + "\n" + p.From + "\n" + text))[:8]
	if p.To != "" {
		line, err := typedLine(text)
		if err != nil {
			return err
		}
		if err := typeTo(cfg, tmux, p.To, line); err != nil {
			return err
		}
		m.To, m.Typed = p.To, true
	}
	return log.Append(Event{V: EventVersion, Time: now, Kind: KindMessage, Mode: ModeShared, Message: m})
}

// typedLine is text as one typed line: its line breaks and tabs become
// spaces, since a typed line break or a carriage return would submit the
// prompt early. Any other control character, an Escape that would
// interrupt the session among them, is refused.
func typedLine(text string) (string, error) {
	var b strings.Builder
	for _, r := range text {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
			return "", fmt.Errorf("the post holds control character %U, which would be typed as a key", r)
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " "), nil
}

// typeTo types text into tmux session name, checked as a nudge is: the
// session is there by its exact name, its pane runs Claude Code, and its
// screen shows no dialog, which a typed line would answer.
func typeTo(cfg *Config, tmux Tmux, name, text string) error {
	if tmux == nil {
		return errors.New("no tmux to type into")
	}
	if strings.ContainsAny(name, "*?[]:.") {
		return fmt.Errorf("%q is not a session's exact name", name)
	}
	if ok, err := tmux.HasSession(name); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("no tmux session %q", name)
	}
	info, err := tmux.Pane(name)
	if err != nil {
		return err
	}
	claude := false
	for _, c := range cfg.Policies.PaneCommands {
		claude = claude || info.Command == c
	}
	if !claude || info.Dead {
		return fmt.Errorf("%s's pane runs %q, not Claude Code; not typing into it", name, info.Command)
	}
	screen, err := tmux.CapturePane(name, 0)
	if err != nil {
		return err
	}
	if dialogShape(screen) {
		return fmt.Errorf("%s shows a dialog; not typing into it", name)
	}
	if cfg.Policies.Trust.Pattern != "" && questionLine(regexp.MustCompile(`^(?:`+cfg.Policies.Trust.Pattern+`)$`), screen) {
		return fmt.Errorf("%s shows Claude Code's folder-trust prompt; not typing into it", name)
	}
	for _, p := range cfg.Policies.Dialog.Patterns {
		if regexp.MustCompile(p).MatchString(screen) {
			return fmt.Errorf("%s shows a dialog; not typing into it", name)
		}
	}
	return tmux.Type(name, text)
}

// ConsoleInbox prints the owner's instructions for console that reader
// has not read, oldest first, one compact JSON object a line, and records
// them read. It says how many it printed.
func ConsoleInbox(cfg *Config, log *Log, events []Event, now time.Time, console, reader string, w io.Writer) (int, error) {
	if !cfg.knownConsole(console) {
		return 0, fmt.Errorf("no console %q", console)
	}
	if reader == "" {
		reader = "cli"
	}
	repos := map[string]bool{}
	for _, r := range cfg.consoleRepos(console) {
		repos[r] = true
	}
	read := map[string]bool{}
	var unread []InboxInstruction
	for _, e := range events {
		switch {
		case e.Kind == KindInbox && e.Inbox != nil && e.Inbox.Console == console && e.Inbox.Reader == reader:
			for _, id := range e.Inbox.IDs {
				read[id] = true
			}
		case e.Kind == KindInstruction && e.Instruction != nil:
			in := e.Instruction
			mine := in.Console == console
			for _, r := range in.Repositories {
				mine = mine || repos[r]
			}
			if mine {
				unread = append(unread, InboxInstruction{ID: in.ID, Text: in.Text, Withheld: in.Withheld > 0, From: in.From, At: e.Time})
			}
		}
	}
	var ids []string
	for _, in := range unread {
		if read[in.ID] {
			continue
		}
		line, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		if _, err := fmt.Fprintf(w, "%s\n", line); err != nil {
			return 0, err
		}
		read[in.ID] = true
		ids = append(ids, in.ID)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return len(ids), log.Append(Event{V: EventVersion, Time: now, Kind: KindInbox, Mode: ModeShared, Inbox: &InboxPayload{Console: console, Reader: reader, IDs: ids}})
}

// tmuxSessionName is the name of the tmux session this process runs in,
// or "" outside tmux.
var tmuxSessionName = func() string {
	if os.Getenv("TMUX") == "" {
		return ""
	}
	args := []string{"display-message", "-p"}
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		args = append(args, "-t", pane)
	}
	out, err := runTimed(10*time.Second, "", nil, false, "tmux", append(args, "#S")...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
