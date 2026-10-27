package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
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
	if i < 0 || i > j || !strings.Contains(body, `<span class="badge">delivered</span>`) || strings.Index(body, "Ship it on Friday") > i {
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
	if err := PostMessage(f.night, NewLogWriter(f.nightBuf), nil, pageAt, MessagePost{Console: "widgets-console", From: "embargoed-bot", Text: "Plain words"}); err != nil {
		t.Fatal(err)
	}
	if ms := messages(t, f.nightBuf); len(ms) != 2 || ms[1].Withheld != 1 || ms[1].From != "" || strings.Contains(f.nightBuf.String(), "embargoed-bot") {
		t.Fatalf("a confidential author was logged: %+v", ms)
	}
}

// --to types the text into that tmux session, by its exact name, after
// the checks a nudge makes, and records that it did; a name with no
// session, a pane not running Claude, or a dialog on screen is refused,
// and nothing is typed or recorded.
func TestConsolePostTo(t *testing.T) {
	h := newHarness(t, false, "a", "b-shell", "c-dialog", "d-dead", "e-trust")
	tmux := newFakeTmux()
	tmux.alive["a"], tmux.panes["a"] = true, "❯ "
	log := NewLogWriter(h.buf)
	if err := PostMessage(h.cfg, log, tmux, h.clock.Now(), MessagePost{Console: "widgets", Text: "Rebase on main,\r\nthen\tcarry on", To: "a"}); err != nil {
		t.Fatal(err)
	}
	if typed := h.typedBy(tmux, "a"); len(typed) != 1 || typed[0] != "Rebase on main, then carry on" {
		t.Fatalf("typed %q", typed)
	}
	ms := messages(t, h.buf)
	if len(ms) != 1 || ms[0].To != "a" || !ms[0].Typed {
		t.Fatalf("messages = %+v", ms)
	}
	tmux.alive["b-shell"], tmux.commands["b-shell"] = true, "bash"
	tmux.alive["c-dialog"], tmux.panes["c-dialog"] = true, "Do you want to proceed?\n❯ 1. Yes\n  2. No"
	tmux.alive["d-dead"], tmux.dead["d-dead"] = true, true
	// The trust prompt is refused by its question, whatever the dialog
	// patterns say (#43).
	tmux.alive["e-trust"], tmux.panes["e-trust"] = true, unnumberedTrustScreen("/work/e")
	h.cfg.Policies.Dialog.Patterns = nil
	for _, to := range []string{"nobody", "a*", "b-shell", "c-dialog", "d-dead", "e-trust"} {
		if err := PostMessage(h.cfg, log, tmux, h.clock.Now(), MessagePost{Console: "widgets", Text: "hello", To: to}); err == nil {
			t.Errorf("--to %s was accepted", to)
		}
		if n := tmux.count("type", to); n != 0 {
			t.Errorf("typed into %s", to)
		}
	}
	// A session agentd does not run, one on another machine say, is
	// refused by name, even when a tmux session here has the name (#66).
	tmux.alive["laptop"], tmux.panes["laptop"] = true, "❯ "
	if err := PostMessage(h.cfg, log, tmux, h.clock.Now(), MessagePost{Console: "widgets", Text: "go 1b", To: "laptop"}); err == nil || !strings.Contains(err.Error(), `"laptop"`) {
		t.Errorf("--to a session agentd does not run: %v", err)
	}
	if err := PostMessage(h.cfg, log, tmux, h.clock.Now(), MessagePost{Console: "widgets", Text: "stop\x1b[A", To: "a"}); err == nil {
		t.Error("a post with an Escape was typed")
	}
	if n := tmux.count("type", "a"); n != 1 {
		t.Errorf("typed into a %d times", n)
	}
	if n := len(messages(t, h.buf)); n != 1 {
		t.Fatalf("a refused post was recorded: %d messages", n)
	}
}

// typedBy is the text typed into name.
func (h *harness) typedBy(tmux *fakeTmux, name string) []string {
	var out []string
	for _, c := range tmux.calls {
		if c.op == "type" && c.name == name {
			out = append(out, c.args[0])
		}
	}
	return out
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
	var first InboxInstruction
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
	// Without an escalation command, a session must escalate to a console
	// the UI lists.
	lost := DefaultConfig()
	lost.TranscriptDir = ""
	lost.Policies.Escalate.DefaultConsole = "infra"
	lost.Sessions = map[string]Session{"ops-1": {Workdir: t.TempDir(), Cwd: t.TempDir()}}
	if err := lost.finish(t.TempDir()); err == nil || !strings.Contains(err.Error(), "sessions.ops-1") {
		t.Errorf("escalations shown nowhere: %v", err)
	}
	lost.Consoles = map[string]ConsoleConfig{"infra": {Title: "Infrastructure"}}
	if err := lost.finish(t.TempDir()); err != nil {
		t.Errorf("default console listed: %v", err)
	}
}

// A console without a repository is listed by the UI after the
// repositories, takes posts and replies, and a reply there is an
// instruction for no repository, which inbox reads.
func TestExtraConsoleInUI(t *testing.T) {
	u, f := fixtureUI(t)
	f.night.Consoles = map[string]ConsoleConfig{"chat": {Title: "General chat"}}
	if err := PostMessage(f.night, NewLogWriter(f.nightBuf), nil, pageAt.Add(-time.Minute), MessagePost{Console: "chat", Kind: "finished", From: "host-a", Text: "Backups are done"}); err != nil {
		t.Fatal(err)
	}
	// An escalation of a session with no repository shows in the console
	// it went to.
	esc := Event{V: EventVersion, Time: pageAt.Add(-time.Minute), Session: "ops-1", Kind: KindAction, Mode: ModeLive,
		Act: &ActPayload{Action: ActEscalate, Key: "k", Console: "chat", Message: "ops-1 needs a disk"}}
	if err := NewLogWriter(f.nightBuf).Append(esc); err != nil {
		t.Fatal(err)
	}
	body := owner(t, u, "/")
	if i, j := strings.Index(body, `href="/console/gadgets"`), strings.Index(body, `href="/console/chat"`); j < 0 || i > j || !strings.Contains(body, "General chat") {
		t.Fatalf("the console is not listed after the repositories:\n%s", body)
	}
	body = owner(t, u, "/console/chat")
	if !strings.Contains(body, "Backups are done") || !strings.Contains(body, "ops-1 needs a disk") || !strings.Contains(body, `name="repository" value="chat"`) {
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

// A reply in a console without a repository is an instruction for no
// repository.
func TestExtraConsoleReplyForNoRepository(t *testing.T) {
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
	byText := map[string]*FeedPayload{}
	for _, e := range events {
		if e.Kind == KindFeed && e.Feed.Phase == FeedImported {
			byText[e.Feed.Text] = e.Feed
		}
	}
	blue, now := byText["Use the blue paint"], byText["Painting now"]
	if blue == nil || now == nil || !blue.At.Equal(time.Unix(1761350400, 250_000_000)) || blue.Kind != "decision" || blue.Repository != "widgets" || now.Repository != "chat" || now.From != "w17" {
		t.Fatalf("imported = %+v, %+v", blue, now)
	}
	added, skipped, err = ImportConsole(f.night, NewLogWriter(f.nightBuf), events, pageAt, "widgets", strings.NewReader(in))
	if err != nil || added != 0 || skipped != 2 {
		t.Fatalf("second import = %d, %d, %v", added, skipped, err)
	}
	if _, _, err := ImportConsole(f.night, NewLogWriter(f.nightBuf), events, pageAt, "widgets", strings.NewReader(`{"ts": 1761350400000, "text": "ms"}`)); err == nil || !strings.Contains(err.Error(), "Unix seconds") {
		t.Fatalf("a ts in milliseconds: %v", err)
	}
}

// import takes the outside console program's lines as they are (#44): a
// line posted with --to keeps whom it was typed to, a field import does
// not use, such as edited, is ignored, and a line that is not JSON, has a
// field of the wrong type, or lacks its text is still refused.
func TestConsoleImportIgnoresUnusedFields(t *testing.T) {
	_, f := fixtureUI(t)
	in := `{"ts": 1761350400, "from": "owner-a", "kind": "info", "text": "Paint it blue", "to": "w17"}
{"ts": 1761350460, "from": "owner-a", "kind": "info", "text": "Paint it green", "edited": 1761350470}
`
	events, _, _ := NewLogWriter(f.nightBuf).Tail(0)
	added, skipped, err := ImportConsole(f.night, NewLogWriter(f.nightBuf), events, pageAt, "widgets", strings.NewReader(in))
	if err != nil || added != 2 || skipped != 0 {
		t.Fatalf("import = %d, %d, %v", added, skipped, err)
	}
	events, _, _ = NewLogWriter(f.nightBuf).Tail(0)
	to := map[string]string{}
	for _, e := range events {
		if e.Kind == KindFeed && e.Feed.Phase == FeedImported && strings.HasPrefix(e.Feed.Text, "Paint") {
			to[e.Feed.Text] = e.Feed.To
		}
	}
	if !reflect.DeepEqual(to, map[string]string{"Paint it blue": "w17", "Paint it green": ""}) {
		t.Fatalf("imported to = %v", to)
	}
	// A to that matches a confidential pattern withholds the line.
	if _, _, err := ImportConsole(f.night, NewLogWriter(f.nightBuf), events, pageAt, "widgets", strings.NewReader(`{"ts": 1761350520, "from": "owner-a", "text": "hello", "to": "embargoed-w1"}`)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.nightBuf.String(), "embargoed") {
		t.Fatal("a confidential to was logged")
	}
	events, _, _ = NewLogWriter(f.nightBuf).Tail(0)
	if p := events[len(events)-1].Feed; p == nil || p.Withheld == 0 || p.To != "" || p.From != "" || p.Text != "" {
		t.Fatalf("withheld line = %+v", p)
	}
	for name, line := range map[string]string{
		"not JSON":   `ts 1761350400 text hello`,
		"wrong type": `{"ts": 1761350400, "text": "hello", "to": 3}`,
		"no text":    `{"ts": 1761350400, "from": "owner-a", "edited": true}`,
		"no time":    `{"from": "owner-a", "text": "hello", "to": "w17"}`,
	} {
		if _, _, err := ImportConsole(f.night, NewLogWriter(f.nightBuf), events, pageAt, "widgets", strings.NewReader(line)); err == nil || !strings.Contains(err.Error(), "line 1") {
			t.Errorf("%s: %v", name, err)
		}
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

// post and inbox refuse the flags of reply and import, and a post on
// standard input over 1 MiB.
func TestConsolePostRefusesMistakes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agentd.json")
	writeFile(t, cfgPath, `{"event_log": "`+filepath.Join(dir, "events.jsonl")+`", "transcript_dir": "", "consoles": {"chat": {"title": "General chat"}}}`)
	for _, args := range [][]string{
		{"--config", cfgPath, "--shadow", "post", "chat", "hello"},
		{"--config", cfgPath, "--repository", "chat", "inbox", "chat"},
	} {
		if err := consoleCmd(args); err == nil || !strings.Contains(err.Error(), "neither --shadow nor --repository") {
			t.Errorf("%v: %v", args, err)
		}
	}
	big := strings.NewReader(strings.Repeat("x", 1<<20+1))
	if err := consolePostCmd("post", []string{"--config", cfgPath, "--from", "a", "chat", "-"}, big, io.Discard); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Errorf("a post over 1 MiB: %v", err)
	}
	if events, _, err := ReadLog(filepath.Join(dir, "events.jsonl")); err != nil || len(events) != 0 {
		t.Fatalf("events = %+v, %v", events, err)
	}
}

// An escalation of a session with no repository whose default console is
// a repository's shows in that repository's console, and an imported
// line shows its kind.
func TestConsoleShowsDefaultConsoleEscalationAndImportedKind(t *testing.T) {
	u, f := fixtureUI(t)
	esc := Event{V: EventVersion, Time: pageAt.Add(-time.Minute), Session: "ops-1", Kind: KindAction, Mode: ModeLive,
		Act: &ActPayload{Action: ActEscalate, Key: "k", Console: "widgets-console", Message: "ops-1 needs a disk"}}
	if err := NewLogWriter(f.nightBuf).Append(esc); err != nil {
		t.Fatal(err)
	}
	events, _, _ := NewLogWriter(f.nightBuf).Tail(0)
	if _, _, err := ImportConsole(f.night, NewLogWriter(f.nightBuf), events, pageAt, "widgets", strings.NewReader(`{"ts": 1761350400, "from": "w17", "kind": "decision", "text": "Use the blue paint"}`)); err != nil {
		t.Fatal(err)
	}
	body := owner(t, u, "/console/widgets")
	if !strings.Contains(body, "ops-1 needs a disk") || !strings.Contains(body, `<span class="badge">imported-decision</span>`) {
		t.Fatalf("the console lacks the escalation or the imported kind:\n%s", body)
	}
}
