package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// messages are the message events of a log, in order.
func messages(t *testing.T, buf *bytes.Buffer) []*MessagePayload {
	t.Helper()
	events, _, err := NewLogWriter(buf).Tail(0)
	if err != nil {
		t.Fatal(err)
	}
	var out []*MessagePayload
	for _, e := range events {
		if e.Kind == KindMessage {
			if e.Mode != ModeShared || e.Session != "" {
				t.Fatalf("a message event %+v", e)
			}
			out = append(out, e.Message)
		}
	}
	return out
}

// A post is recorded in its console with its kind and author, shown in
// that console newest last, and refused when it names no console, an
// unknown kind, or is empty or too long.
func TestConsolePostShownInItsConsole(t *testing.T) {
	u, f := fixtureUI(t)
	log := NewLogWriter(f.nightBuf)
	at := pageAt.Add(-time.Minute)
	if err := PostMessage(f.night, log, nil, at, MessagePost{Console: "widgets-console", Kind: "delivered", From: "w17", Text: "Delivered **!4** for review"}); err != nil {
		t.Fatal(err)
	}
	if err := PostMessage(f.night, log, nil, at, MessagePost{Console: "widgets-console", From: "w17", Text: "Starting the paint"}); err != nil {
		t.Fatal(err)
	}
	ms := messages(t, f.nightBuf)
	if len(ms) != 2 || ms[0].Kind != "delivered" || ms[0].From != "w17" || ms[0].Console != "widgets-console" || ms[1].Kind != "info" || ms[0].ID == "" || ms[0].ID == ms[1].ID {
		t.Fatalf("messages = %+v", ms)
	}
	body := owner(t, u, "/console/widgets")
	i, j := strings.Index(body, "Delivered <strong>!4</strong> for review"), strings.Index(body, "Starting the paint")
	if i < 0 || j < i || !strings.Contains(body, `<span class="badge">delivered</span>`) || strings.LastIndex(body, "Ship it on Friday") > i {
		t.Fatalf("the posts are not shown newest last with their kind:\n%s", body)
	}
	for _, bad := range []MessagePost{
		{Console: "nowhere", Text: "x"},
		{Console: "widgets-console", Kind: "gossip", Text: "x"},
		{Console: "widgets-console", Text: "  "},
		{Console: "widgets-console", Text: strings.Repeat("x", MessageLimit+1)},
	} {
		if err := PostMessage(f.night, log, nil, at, bad); err == nil {
			t.Errorf("%+v was recorded", bad)
		}
	}
	if n := len(messages(t, f.nightBuf)); n != 2 {
		t.Fatalf("a refused post was recorded: %d messages", n)
	}
}

// A post that matches a confidential pattern is recorded without its
// text, and the console shows it withheld.
func TestConsolePostWithheld(t *testing.T) {
	u, f := fixtureUI(t)
	if err := PostMessage(f.night, NewLogWriter(f.nightBuf), nil, pageAt.Add(-time.Minute), MessagePost{Console: "widgets-console", Text: "The embargoed launch moved"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.nightBuf.String(), "embargoed launch") {
		t.Fatal("the confidential text was logged")
	}
	if ms := messages(t, f.nightBuf); len(ms) != 1 || ms[0].Withheld != 1 || ms[0].Text != "" {
		t.Fatalf("messages = %+v", ms)
	}
	if body := owner(t, u, "/console/widgets"); !strings.Contains(body, "withheld: it matched a confidential pattern") {
		t.Fatalf("the withheld post is not shown:\n%s", body)
	}
}

// --to types the text into that tmux session, by its exact name, after
// the checks a nudge makes, and records that it did; a name with no
// session, a pane not running Claude, or a dialog on screen is refused,
// and nothing is typed or recorded.
func TestConsolePostTo(t *testing.T) {
	h := newHarness(t, false, "a")
	tmux := newFakeTmux()
	tmux.alive["a"], tmux.panes["a"] = true, "❯ "
	log := NewLogWriter(h.buf)
	if err := PostMessage(h.cfg, log, tmux, h.clock.Now(), MessagePost{Console: "widgets", Text: "Rebase on main, then carry on", To: "a"}); err != nil {
		t.Fatal(err)
	}
	if n := tmux.count("type", "a"); n != 1 {
		t.Fatalf("typed %d times", n)
	}
	ms := messages(t, h.buf)
	if len(ms) != 1 || ms[0].To != "a" || !ms[0].Typed {
		t.Fatalf("messages = %+v", ms)
	}
	tmux.alive["b-shell"], tmux.commands["b-shell"] = true, "bash"
	tmux.alive["c-dialog"], tmux.panes["c-dialog"] = true, "Do you want to proceed?\n❯ 1. Yes\n  2. No"
	for _, to := range []string{"nobody", "a*", "b-shell", "c-dialog"} {
		if err := PostMessage(h.cfg, log, tmux, h.clock.Now(), MessagePost{Console: "widgets", Text: "hello", To: to}); err == nil {
			t.Errorf("--to %s was accepted", to)
		}
		if n := tmux.count("type", to); n != 0 {
			t.Errorf("typed into %s", to)
		}
	}
	if n := len(messages(t, h.buf)); n != 1 {
		t.Fatalf("a refused post was recorded: %d messages", n)
	}
}

// inbox prints the owner's replies for a console once to each reader,
// as compact JSON lines, and records them read; the second call prints
// nothing, and another reader still has them.
func TestConsoleInboxOnce(t *testing.T) {
	h := newHarness(t, false, "a")
	for _, text := range []string{"Paint it **green**", "Ship on Friday"} {
		if err := reply(h, "widgets", text); err != nil {
			t.Fatal(err)
		}
		h.clock.advance(time.Minute)
	}
	if err := reply(h, "readonly", "Not for widgets"); err != nil {
		t.Fatal(err)
	}
	inbox := func(reader string) []string {
		t.Helper()
		var out bytes.Buffer
		if _, err := ConsoleInbox(h.cfg, NewLogWriter(h.buf), h.events(), h.clock.Now(), "widgets", reader, &out); err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(out.String()), "\n")
	}
	got := inbox("a")
	if len(got) != 2 {
		t.Fatalf("inbox = %q", got)
	}
	var first PackInstruction
	if err := json.Unmarshal([]byte(got[0]), &first); err != nil || first.Text != "Paint it **green**" || first.From != "console:widgets" || first.ID == "" {
		t.Fatalf("first line %q: %+v %v", got[0], first, err)
	}
	if !strings.Contains(got[1], "Ship on Friday") {
		t.Fatalf("second line %q", got[1])
	}
	if again := inbox("a"); len(again) != 1 || again[0] != "" {
		t.Fatalf("inbox again = %q", again)
	}
	if other := inbox("cli"); len(other) != 2 {
		t.Fatalf("another reader's inbox = %q", other)
	}
	if err := reply(h, "widgets", "One more"); err != nil {
		t.Fatal(err)
	}
	if got := inbox("a"); len(got) != 1 || !strings.Contains(got[0], "One more") {
		t.Fatalf("inbox after a new reply = %q", got)
	}
	var out bytes.Buffer
	if _, err := ConsoleInbox(h.cfg, NewLogWriter(h.buf), h.events(), h.clock.Now(), "nowhere", "a", &out); err == nil {
		t.Fatal("an unknown console's inbox was read")
	}
}

// A console without a repository is configured by name with a title and
// its sessions; a name a repository or its console has is refused.
func TestExtraConsoleConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TranscriptDir = ""
	cfg.Repositories = map[string]Repository{"widgets": {Forge: "gitlab", Project: "example-org/widgets", Console: "widgets-console"}}
	cfg.Consoles = map[string]ConsoleConfig{"chat": {Title: "General chat", Sessions: []string{"chat-*"}}}
	cfg.Sessions = map[string]Session{"chat-1": {Workdir: t.TempDir(), Cwd: t.TempDir()}}
	cfg.Policies.Escalate.DefaultConsole = "infra"
	if err := cfg.finish(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Console("chat-1"); got != "chat" {
		t.Fatalf("chat-1's console = %q", got)
	}
	for _, name := range []string{"widgets", "widgets-console"} {
		bad := DefaultConfig()
		bad.TranscriptDir = ""
		bad.Repositories = cfg.Repositories
		bad.Consoles = map[string]ConsoleConfig{name: {}}
		if err := bad.finish(t.TempDir()); err == nil || !strings.Contains(err.Error(), "consoles."+name) {
			t.Errorf("console %q: %v", name, err)
		}
	}
	bad := DefaultConfig()
	bad.TranscriptDir = ""
	bad.Consoles = map[string]ConsoleConfig{"chat": {Sessions: []string{"[x"}}}
	if err := bad.finish(t.TempDir()); err == nil {
		t.Error("a bad glob was accepted")
	}
}

// A console without a repository is listed by the UI after the
// repositories, takes posts and replies, and a reply there is an
// instruction for no coordinator: it wakes none, and inbox reads it.
func TestExtraConsoleInUI(t *testing.T) {
	u, f := fixtureUI(t)
	f.night.Consoles = map[string]ConsoleConfig{"chat": {Title: "General chat"}}
	if err := PostMessage(f.night, NewLogWriter(f.nightBuf), nil, pageAt.Add(-time.Minute), MessagePost{Console: "chat", Kind: "finished", From: "host-a", Text: "Backups are done"}); err != nil {
		t.Fatal(err)
	}
	body := owner(t, u, "/")
	if i, j := strings.Index(body, `href="/console/gadgets"`), strings.Index(body, `href="/console/chat"`); j < 0 || i > j || !strings.Contains(body, "General chat") {
		t.Fatalf("the console is not listed after the repositories:\n%s", body)
	}
	body = owner(t, u, "/console/chat")
	if !strings.Contains(body, "Backups are done") || !strings.Contains(body, `name="repository" value="chat"`) {
		t.Fatalf("the console lacks its post or its reply box:\n%s", body)
	}
	w := ownerPost(u, "/reply", url.Values{"src": {"night"}, "repository": {"chat"}, "text": {"Rotate the logs tonight"}, "back": {"/console/chat"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("reply: %d %s", w.Code, w.Body.String())
	}
	if body := owner(t, u, "/console/chat"); !strings.Contains(body, "Rotate the logs tonight") {
		t.Fatalf("the reply is not shown:\n%s", body)
	}
	events, _, _ := NewLogWriter(f.nightBuf).Tail(0)
	st := NewLogState(f.night, ModeLive).Fold(events)
	var got *Instruction
	for _, in := range st.Instructions {
		if in.Console == "chat" {
			got = in
		}
	}
	if got == nil || len(got.Repositories) != 0 || got.Text != "Rotate the logs tonight" {
		t.Fatalf("instruction %+v", got)
	}
	var out bytes.Buffer
	if _, err := ConsoleInbox(f.night, NewLogWriter(f.nightBuf), events, pageAt, "chat", "cli", &out); err != nil || !strings.Contains(out.String(), "Rotate the logs tonight") {
		t.Fatalf("inbox %q %v", out.String(), err)
	}
}

// A reply in a console without a repository wakes no coordinator.
func TestExtraConsoleReplyWakesNoCoordinator(t *testing.T) {
	h := instrHarness(t)
	h.cfg.Consoles = map[string]ConsoleConfig{"chat": {Title: "General chat"}}
	h.ticks(2, time.Minute)
	if err := reply(h, "chat", "Rotate the logs tonight"); err != nil {
		t.Fatal(err)
	}
	ins := instructions(h)
	if len(ins) != 1 || ins[0].Console != "chat" || len(ins[0].Repositories) != 0 || ins[0].From != "console:chat" {
		t.Fatalf("instructions = %+v", ins)
	}
	h.ticks(3, time.Minute)
	if w := h.wakes(); len(w) != 0 {
		t.Fatalf("wakes = %+v", w)
	}
}

// Without an escalation command, escalations and feed lines are logged
// and shown by the UI, and no command runs.
func TestFeedAndEscalationsWithoutACommand(t *testing.T) {
	h := feedHarness(t, false, "d", "e")
	h.cfg.Policies.Escalate.Command = nil
	if err := h.cfg.finish(h.dir); err != nil {
		t.Fatalf("finish without a command: %v", err)
	}
	h.restart(false)
	h.alive("d", "waiting")
	h.ticks(1, 5*time.Minute)
	doneWithDelivery(t, h, "e")
	h.alive("e", "waiting")
	h.status("d", "# d\n\n## Questions\n\nShould the widget squeak?\n")
	h.ticks(3, 5*time.Minute)
	if len(h.runner.calls) != 0 {
		t.Fatalf("commands ran: %v", h.runner.calls)
	}
	var fed, escalated int
	for _, e := range h.events() {
		if e.Kind == KindFeed && e.Feed.Phase == FeedPosted {
			fed++
		}
		if e.Kind == KindAction && e.Act.Action == ActEscalate && e.Session == "d" {
			escalated++
		}
	}
	if fed == 0 || escalated != 1 {
		t.Fatalf("fed %d, escalated %d", fed, escalated)
	}
	h.restart(false)
	h.ticks(2, 5*time.Minute)
	if n := h.count(KindAction, ActEscalate, "d"); n != 1 {
		t.Fatalf("escalated %d times after a restart", n)
	}
	src := newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a")
	u, _ := testUI(t, UIConfig{}, src)
	body := owner(t, u, "/console/widgets")
	if !strings.Contains(body, "Should the widget squeak?") || !strings.Contains(body, "e delivered") {
		t.Fatalf("the console lacks the escalation or the feed line:\n%s", body)
	}
}

// import also reads the outside console program's lines: ts in Unix
// seconds, from, kind and text.
func TestConsoleImportOutsideLines(t *testing.T) {
	_, f := fixtureUI(t)
	f.night.Consoles = map[string]ConsoleConfig{"chat": {Title: "General chat"}}
	in := `{"ts": 1761350400.25, "from": "owner-a", "kind": "decision", "text": "Use the blue paint"}
{"ts": 1761350460, "from": "w17", "kind": "info", "text": "Painting now", "repository": "chat"}
`
	events, _, _ := NewLogWriter(f.nightBuf).Tail(0)
	added, skipped, err := ImportConsole(f.night, NewLogWriter(f.nightBuf), events, pageAt, "widgets", strings.NewReader(in))
	if err != nil || added != 2 || skipped != 0 {
		t.Fatalf("import = %d, %d, %v", added, skipped, err)
	}
	events, _, _ = NewLogWriter(f.nightBuf).Tail(0)
	var got []*FeedPayload
	for _, e := range events {
		if e.Kind == KindFeed && e.Feed.Phase == FeedImported && e.Feed.From != "owner-a" || e.Kind == KindFeed && e.Feed.Text == "Use the blue paint" {
			got = append(got, e.Feed)
		}
	}
	want := time.Unix(1761350400, 250_000_000)
	if len(got) < 2 || !got[len(got)-2].At.Equal(want) || got[len(got)-2].Kind != "decision" || got[len(got)-1].Repository != "chat" {
		t.Fatalf("imported = %+v", got)
	}
	added, skipped, err = ImportConsole(f.night, NewLogWriter(f.nightBuf), events, pageAt, "widgets", strings.NewReader(in))
	if err != nil || added != 0 || skipped != 2 {
		t.Fatalf("second import = %d, %d, %v", added, skipped, err)
	}
}

// agentd console post and inbox write to the log of the configuration
// given: a post from standard input with its kind and author, a reply in
// a console without a repository, and inbox printing it once.
func TestConsolePostAndInboxCommands(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	cfgPath := filepath.Join(dir, "agentd.json")
	writeFile(t, cfgPath, `{"event_log": "`+logPath+`", "transcript_dir": "", "consoles": {"chat": {"title": "General chat"}}}`)
	if err := consolePostCmd("post", []string{"--config", cfgPath, "--kind", "finished", "--from", "host-a", "chat", "-"}, strings.NewReader("Backups are done\n"), io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := consoleCmd([]string{"--config", cfgPath, "post", "--from", "host-a", "nowhere", "hello"}); err == nil {
		t.Fatal("a post to no console was recorded")
	}
	if err := consoleCmd([]string{"--config", cfgPath, "reply", "chat", "Rotate", "the", "logs"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for i := 0; i < 2; i++ {
		if err := consolePostCmd("inbox", []string{"--config", cfgPath, "--from", "host-a", "chat"}, nil, &out); err != nil {
			t.Fatal(err)
		}
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 1 || !strings.Contains(lines[0], `"text":"Rotate the logs"`) {
		t.Fatalf("inbox printed %q", out.String())
	}
	events, _, err := ReadLog(logPath)
	if err != nil || len(events) != 3 || events[0].Message == nil || events[0].Message.Kind != "finished" || events[0].Message.From != "host-a" || events[0].Message.Text != "Backups are done" ||
		events[1].Instruction == nil || events[2].Inbox == nil || events[2].Inbox.Reader != "host-a" {
		t.Fatalf("events = %+v, %v", events, err)
	}
}
