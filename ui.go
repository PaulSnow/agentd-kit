package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The owner's web UI (#41): one server over every configured log, merged
// by repository. It replaces agentd page, and serves the consoles' view
// that #37 asked for.

// UIConfig is the owner's UI: where it listens, the Host names it answers
// to, and how it learns who is asking. Whois is an argv with {addr}, the
// caller's address, substituted; it prints the caller's login, either
// alone on its first line or as the UserProfile.LoginName of a JSON
// object. Owner is the login the UI serves; everyone else is refused.
// Logs are other agentd config files whose sessions, repositories and
// event logs are shown with this config's; Shadow adds each one's shadow
// run as one more source.
type UIConfig struct {
	Address string   `json:"address"`
	Hosts   []string `json:"hosts"`
	Whois   []string `json:"whois"`
	Owner   string   `json:"owner"`
	Logs    []string `json:"logs"`
	Shadow  bool     `json:"shadow"`
}

// Whois names the login behind a remote address.
type Whois interface {
	Login(addr string) (string, error)
}

// ExecWhois runs the configured whois command, bounded by Timeout.
type ExecWhois struct {
	Argv    []string
	Timeout time.Duration
}

func (w ExecWhois) Login(addr string) (string, error) {
	argv := make([]string, len(w.Argv))
	for i, a := range w.Argv {
		argv[i] = strings.ReplaceAll(a, "{addr}", addr)
	}
	out, err := runTimed(w.Timeout, "", nil, false, argv[0], argv[1:]...)
	if err != nil {
		return "", fmt.Errorf("whois %s: %w", addr, err)
	}
	return parseWhois(out)
}

// parseWhois reads a login from whois output: a JSON object's
// UserProfile.LoginName, or else the first line. Empty is an error.
func parseWhois(out []byte) (string, error) {
	var who struct {
		UserProfile struct {
			LoginName string `json:"LoginName"`
		} `json:"UserProfile"`
	}
	text := strings.TrimSpace(string(out))
	if strings.HasPrefix(text, "{") {
		if err := json.Unmarshal([]byte(text), &who); err != nil {
			return "", fmt.Errorf("whois output is not the JSON expected: %w", err)
		}
		text = who.UserProfile.LoginName
	} else if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	if text = strings.TrimSpace(text); text == "" {
		return "", errors.New("whois named no login")
	}
	return text, nil
}

// uiKeptMessages is how many console messages a source keeps for each
// repository, and as many of its sessions' turns besides; uiKeptEvents
// is how many events for each session; uiTurnRefs bounds the prompts
// read whose console message has not come, which a prompt the console
// rule does not post never has.
const (
	uiKeptMessages = 200
	uiKeptEvents   = 40
	uiTurnRefs     = 1024
)

// uiMessage is one line of a repository's console, or of a console
// without a repository: a feed post, an escalation of one of its
// sessions, an instruction of the owner's, a line of the old console's
// history, or a post (#42) of its kind.
type uiMessage struct {
	At      time.Time
	Kind    string // feed, escalation, instruction, imported, or a post's kind
	From    string
	Session string
	Text    string
	ID      string // an instruction's, whose steps the view reads from the fold
	// Ref is where a session's turn was read, when its turn is logged,
	// for its prompt page (#58).
	Ref *promptRef
	// Entry is its id as the owner responds to it, Writer the session
	// that wrote it, "" when agentd or the owner did, and Re the entry an
	// instruction responds to (#59).
	Entry  string
	Writer string
	Re     *EntryRe
}

// turn says whether m is a session's turn (#38).
func (m uiMessage) turn() bool { return m.Kind == MessagePrompt || m.Kind == MessageReply }

// uiSource is one log as the UI sees it: its config, its run mode, and a
// LogState brought up to date through apply with the lines appended since
// it last looked, so a request reads only what is new. It also keeps what
// the fold does not: each repository's latest console messages and each
// session's latest events.
type uiSource struct {
	Name string

	mu       sync.Mutex
	cfg      *Config
	log      *Log
	off      int64
	st       *LogState
	host     string
	messages map[string][]uiMessage // by repository or console without one, oldest first
	events   map[string][]Event     // by session, oldest first
	askedAt  map[string]time.Time   // when each decision was first seen, by session and id
	// turnRefs holds where each prompt logged was read, by the id of the
	// console message it is posted as, until that message comes.
	turnRefs map[string]promptRef
	// echo marks a shadow source over a live source's log: the owner's
	// records and the shared facts are the live source's to show.
	echo bool
	// owned is the owner's message each session started for one was
	// started for, until its first post, which is threaded under it (#65).
	owned map[string]string
	// now is the time a response is taken at; a test's clock, else the
	// wall clock.
	now func() time.Time
}

// clock is the time now, by src's clock.
func (src *uiSource) clock() time.Time {
	if src.now != nil {
		return src.now()
	}
	return time.Now()
}

func newUISource(name string, cfg *Config, log *Log, mode, host string) *uiSource {
	return &uiSource{Name: name, cfg: cfg, log: log, st: NewLogState(cfg, mode), host: host,
		messages: map[string][]uiMessage{}, events: map[string][]Event{}, askedAt: map[string]time.Time{}, turnRefs: map[string]promptRef{}, owned: map[string]string{}}
}

// catchUp applies the events appended since the last look, and keeps the
// messages and events among them.
func (src *uiSource) catchUp() error {
	events, off, err := src.log.Tail(src.off)
	if err != nil {
		return err
	}
	src.off = off
	src.st.Fold(events)
	cfg := withRuntime(src.cfg, src.st.Runtime)
	repoOf := func(session string) string {
		if s, ok := cfg.Sessions[session]; ok {
			return s.Repository
		}
		return src.st.Ended[session]
	}
	// add keeps m in time order, and the latest uiKeptMessages only, so
	// imported history never pushes out what is new. Sessions' turns are
	// kept apart, the latest uiKeptMessages of them, so a busy session
	// never pushes out the rest.
	add := func(repo string, m uiMessage) {
		if repo == "" {
			return
		}
		ms := src.messages[repo]
		i := len(ms)
		for i > 0 && ms[i-1].At.After(m.At) {
			i--
		}
		ms = append(ms[:i], append([]uiMessage{m}, ms[i:]...)...)
		n := 0
		for _, o := range ms {
			if o.turn() == m.turn() {
				n++
			}
		}
		if n > uiKeptMessages {
			for j, o := range ms {
				if o.turn() == m.turn() {
					ms = append(ms[:j], ms[j+1:]...)
					break
				}
			}
		}
		src.messages[repo] = ms
	}
	for _, e := range events {
		if e.Mode != src.st.Mode && e.Mode != ModeOwner && e.Mode != ModeShared && e.Mode != "" {
			continue
		}
		src.noteAsked(e)
		owners := e.Mode == ModeOwner || e.Mode == ModeShared || e.Mode == ""
		if e.Session != "" {
			es := append(src.events[e.Session], e)
			if len(es) > uiKeptEvents {
				es = es[len(es)-uiKeptEvents:]
			}
			src.events[e.Session] = es
		}
		// A prompt logged before #56 B that its rule rejects is shown as
		// none: its turn is no ref, and its message is dropped below.
		if e.Kind == KindTurn && e.Turn != nil && e.Session != "" && (e.Turn.Role == TurnPrompt && !loggedNoPrompt(e.Turn.Text) || e.Turn.Role == TurnCompact) {
			if len(src.turnRefs) >= uiTurnRefs {
				clear(src.turnRefs)
			}
			src.turnRefs[turnMessageID(e.Session, e.Turn.Transcript, e.Turn.Line)] = promptRef{e.Turn.Transcript, e.Turn.Line, e.Turn.Role}
		}
		if r := e.Runtime; e.Kind == KindSession && r != nil && r.Action == SessionStart && r.Owner != "" {
			src.owned[e.Session] = r.Owner
		}
		switch {
		case e.Feed != nil && (e.Feed.Phase == FeedIntent || e.Feed.Phase == FeedWould) && e.Feed.Text != "":
			add(e.Feed.Repository, uiMessage{At: e.Time, Kind: "feed", From: "agentd", Text: e.Feed.Text, Entry: entryKey(entryFeed, e.Feed.Key, e.Time, e.Feed.Text)})
		case src.echo && owners:
		case e.Feed != nil && e.Feed.Phase == FeedImported:
			at := e.Time
			if e.Feed.At != nil {
				at = *e.Feed.At
			}
			kind := "imported"
			if e.Feed.Kind != "" {
				kind = "imported-" + e.Feed.Kind
			}
			add(e.Feed.Repository, uiMessage{At: at, Kind: kind, From: e.Feed.From, Text: e.Feed.Text, Entry: entryKey(entryFeed, e.Feed.Key, at, e.Feed.Text)})
		case e.Act != nil && (e.Kind == KindAction || e.Kind == KindWould) && (e.Act.Action == ActEscalate || e.Act.Action == ActUpdate):
			m := uiMessage{At: e.Time, Kind: "escalation", From: "agentd", Session: e.Session, Text: e.Act.Message,
				Entry: entryKey(entryEscalation, "", e.Time, e.Session+"\n"+e.Act.Key)}
			// A session's repository has it; one that names none, its
			// console's repositories or the console without one it went to.
			if repo := repoOf(e.Session); repo != "" {
				add(repo, m)
			} else if _, ok := cfg.Consoles[e.Act.Console]; ok {
				add(e.Act.Console, m)
			} else {
				for _, r := range cfg.consoleRepos(e.Act.Console) {
					add(r, m)
				}
			}
		case e.Kind == KindCase && e.Case != nil && e.Case.Call != nil && e.Case.Call.Progress != nil && e.Case.Call.Refused == "":
			// A session's progress, reported through its tool (#59): an
			// entry of its own, labelled by the call, to respond to.
			p := e.Case.Call.Progress
			text := p.Summary
			if p.NextAction != "" {
				text += "\n\nNext: " + p.NextAction
			}
			m := uiMessage{At: e.Time, Kind: "progress", From: e.Session, Session: e.Session, Writer: e.Session, Text: text,
				Entry: entryKey(entryProgress, e.Case.Case+"-"+e.Case.Call.Hash, e.Time, text)}
			// A session of no repository's goes to its console.
			if repo := repoOf(e.Session); repo != "" {
				add(repo, m)
			} else if con := cfg.Console(e.Session); cfg.knownConsole(con) {
				if _, ok := cfg.Consoles[con]; ok {
					add(con, m)
				}
				for _, r := range cfg.consoleRepos(con) {
					add(r, m)
				}
			}
		case e.Kind == KindInstruction && e.Instruction != nil:
			m := uiMessage{At: e.Time, Kind: "instruction", From: e.Instruction.From, Text: e.Instruction.Text, ID: e.Instruction.ID,
				Entry: entryInstruction + e.Instruction.ID, Re: e.Instruction.Re}
			for _, r := range e.Instruction.Repositories {
				add(r, m)
			}
			add(e.Instruction.Console, m)
		case e.Kind == KindMessage && e.Message != nil && e.Message.Kind == MessagePrompt && loggedNoPrompt(e.Message.Text):
		case e.Kind == KindMessage && e.Message != nil:
			m := uiMessage{At: e.Time, Kind: e.Message.Kind, From: e.Message.From, Text: e.Message.Text, Entry: entryKey(entryMessage, e.Message.ID, e.Time, e.Message.Text)}
			// A session's own post or turn is the session's to answer.
			if _, ok := cfg.Sessions[e.Message.From]; ok {
				m.Writer = e.Message.From
			}
			// The first post of a session started for the owner's message
			// is threaded under that message (#65).
			if o := src.owned[e.Message.From]; o != "" && !m.turn() {
				m.Re = &EntryRe{ID: entryInstruction + o, Kind: "instruction", Session: e.Message.From}
				delete(src.owned, e.Message.From)
			}
			if m.turn() {
				m.Session = e.Message.From // a session's turn links to its page
				if ref, ok := src.turnRefs[e.Message.ID]; ok && m.Kind == MessagePrompt {
					m.Ref = &ref
					delete(src.turnRefs, e.Message.ID)
				}
			}
			if _, ok := cfg.Consoles[e.Message.Console]; ok {
				add(e.Message.Console, m)
			}
			for _, r := range cfg.consoleRepos(e.Message.Console) {
				add(r, m)
			}
		}
	}
	return nil
}

// markSeen records the owner marking items seen. Only items the state
// holds are marked; an unknown id is an error and marks nothing.
func (src *uiSource) markSeen(ids []string, from string) error {
	src.mu.Lock()
	defer src.mu.Unlock()
	if err := src.catchUp(); err != nil {
		return err
	}
	for _, id := range ids {
		if src.st.Items[id] == nil {
			return fmt.Errorf("no item %q", id)
		}
	}
	return MarkSeen(src.log, time.Now(), ids, from)
}

// answer records the owner's answer to one of session's open decisions,
// checked against the decision as the log holds it now.
func (src *uiSource) answer(session string, a AnswerPayload) error {
	src.mu.Lock()
	defer src.mu.Unlock()
	if err := src.catchUp(); err != nil {
		return err
	}
	return RecordAnswer(src.log, src.st, time.Now(), session, a)
}

// deadline records the owner's deadline on one of session's open
// decisions, checked against the decision as the log holds it now.
func (src *uiSource) deadline(session, id string, at time.Time) error {
	src.mu.Lock()
	defer src.mu.Unlock()
	if err := src.catchUp(); err != nil {
		return err
	}
	return RecordDeadline(src.log, src.st, time.Now(), session, id, at)
}

// reply records the owner's console reply as an instruction for repo,
// addressed to session to when it is set; started sessions count.
func (src *uiSource) reply(repo, to, text string, files []stagedAttachment) error {
	src.mu.Lock()
	defer src.mu.Unlock()
	if err := src.catchUp(); err != nil {
		return err
	}
	return UIInstruction(withRuntime(src.cfg, src.st.Runtime), src.log, time.Now(), repo, to, text, files)
}

// attachment is the attachment id of src's log and where it is stored,
// when the log holds it.
func (src *uiSource) attachment(id string) (AttachmentPayload, string, bool) {
	src.mu.Lock()
	defer src.mu.Unlock()
	if err := src.catchUp(); err != nil {
		return AttachmentPayload{}, "", false
	}
	for _, in := range src.st.Instructions {
		for _, a := range in.Attachments {
			if a.ID == id {
				return a, attachmentPath(src.cfg, a), true
			}
		}
	}
	return AttachmentPayload{}, "", false
}

// whoisTTL is how long the UI trusts one whois answer for an address, and
// whoisSlots how many whois commands may run at once: a burst of requests
// runs no burst of commands.
const (
	whoisTTL   = time.Minute
	whoisSlots = 4
)

type whoisAnswer struct {
	login string
	at    time.Time
}

// UI serves the owner's view. Every request is checked before anything is
// read: its Host must be one the UI answers to, it must not come from the
// UI's own address or loopback (a process on this machine would otherwise
// borrow the machine's identity), and whois must name the owner. A POST
// must also come from the UI itself: its Origin must be the UI's, and it
// must carry the token the UI put in its form.
type UI struct {
	cfg     UIConfig
	whois   Whois
	sources []*uiSource
	self    net.IP // the address the UI listens on
	now     func() time.Time
	loc     *time.Location // the zone times are shown in
	token   string         // the form token, new each time the UI starts

	mu      sync.Mutex
	answers map[string]whoisAnswer // by caller IP
	slots   chan struct{}
}

// NewUI builds the UI's handler over sources, the first the main config's.
func NewUI(cfg UIConfig, whois Whois, sources []*uiSource, now func() time.Time) (*UI, error) {
	host, _, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("ui.address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		return nil, fmt.Errorf("ui.address %q must be one IP address, not a name or every address", cfg.Address)
	}
	if cfg.Owner == "" || len(cfg.Whois) == 0 {
		return nil, errors.New("ui.owner and ui.whois must be set")
	}
	if len(sources) == 0 {
		return nil, errors.New("the UI has no log to show")
	}
	if len(cfg.Hosts) == 0 {
		cfg.Hosts = []string{cfg.Address}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return &UI{cfg: cfg, whois: whois, sources: sources, self: ip, now: now, loc: time.Local, token: hex.EncodeToString(b),
		answers: map[string]whoisAnswer{}, slots: make(chan struct{}, whoisSlots)}, nil
}

// source is the source named name, or nil.
func (u *UI) source(name string) *uiSource {
	for _, s := range u.sources {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// login is whois's answer for the caller, from the cache while it is
// fresh. A failure is logged once per address per whoisTTL.
func (u *UI) login(remote string, ip net.IP) (string, error) {
	key := ip.String()
	u.mu.Lock()
	a, ok := u.answers[key]
	u.mu.Unlock()
	if ok && u.now().Sub(a.at) < whoisTTL {
		return a.login, nil
	}
	select {
	case u.slots <- struct{}{}:
	default:
		return "", errors.New("too many whois commands running")
	}
	login, err := u.whois.Login(remote)
	<-u.slots
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentd ui: refused %s: %v\n", key, err)
	}
	u.mu.Lock()
	if len(u.answers) > 1024 {
		u.answers = map[string]whoisAnswer{}
	}
	// A failure is not kept: one timeout must not lock the owner out for
	// whoisTTL. The slots bound how often whois runs.
	if err == nil {
		u.answers[key] = whoisAnswer{login: login, at: u.now()}
	}
	u.mu.Unlock()
	return login, err
}

//go:embed ui/ui.css ui/ui.js
var uiAssets embed.FS

func (u *UI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src 'self' blob:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	// same-origin, not no-referrer: under no-referrer a browser sends a
	// form's POST with Origin null, and checkPost refuses it. Other sites
	// still get no referrer, and links out carry rel=noreferrer.
	h.Set("Referrer-Policy", "same-origin")
	if !u.hostAllowed(r.Host) {
		http.Error(w, "unknown host", http.StatusForbidden)
		return
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(remote)
	if err != nil || ip == nil || ip.Equal(u.self) || ip.IsLoopback() || ip.IsUnspecified() {
		http.Error(w, "refused: the UI does not answer its own machine", http.StatusForbidden)
		return
	}
	login, err := u.login(r.RemoteAddr, ip)
	if err != nil || login != u.cfg.Owner {
		http.Error(w, "refused: only the owner may use this UI", http.StatusForbidden)
		return
	}
	path := r.URL.Path
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	posts := map[string]func(http.ResponseWriter, *http.Request){
		"/seen": u.serveSeen, "/answer": u.serveAnswer, "/deadline": u.serveDeadline, "/reply": u.serveReply, "/respond": u.serveRespond,
	}
	switch {
	case path == "/ui.css" || path == "/ui.js":
		if !get {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		b, _ := uiAssets.ReadFile("ui" + path)
		if path == "/ui.css" {
			h.Set("Content-Type", "text/css; charset=utf-8")
		} else {
			h.Set("Content-Type", "text/javascript; charset=utf-8")
		}
		w.Write(b)
	case posts[path] != nil:
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		posts[path](w, r)
	case path == "/accounts.json":
		if !get {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		u.serveAccounts(w)
	case strings.HasPrefix(path, "/attachment/"):
		if !get {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		u.serveAttachment(w, r)
	case path == "/" || strings.HasPrefix(path, "/console/") || strings.HasPrefix(path, "/session/"):
		if !get {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		u.serveView(w, r)
	default:
		http.NotFound(w, r)
	}
}

// serveView renders the index, a repository's console or a session.
func (u *UI) serveView(w http.ResponseWriter, r *http.Request) {
	now := u.now()
	var view any
	var name string
	var err error
	switch path := r.URL.Path; {
	case path == "/":
		name = "index"
		view, err = u.indexView(now)
	case strings.HasPrefix(path, "/console/"):
		name = "console"
		repo, entries := strings.CutSuffix(strings.TrimPrefix(path, "/console/"), "/entries")
		var v *uiConsole
		if v, err = u.consoleView(repo, now); err == nil && v == nil {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		var before time.Time
		if ms, perr := strconv.ParseInt(q.Get("at"), 10, 64); perr == nil {
			before = time.UnixMilli(ms)
		}
		if entries && err == nil {
			u.serveEntries(w, v, q.Get("before"), before, q.Get("have"))
			return
		}
		// The page carries the latest entries only; older ones come as
		// the owner scrolls up, so a long console stays light (#59).
		if v != nil {
			v.Entries, v.More = window(v.Entries, q.Get("before"), before, consoleWindow)
			for _, e := range v.Entries {
				html, _, rerr := renderEntry(v, e)
				if rerr != nil {
					err = rerr
					break
				}
				v.Rendered = append(v.Rendered, template.HTML(html))
			}
		}
		view = v
	default:
		parts := strings.Split(strings.TrimPrefix(path, "/session/"), "/")
		src := u.source(parts[0])
		if len(parts) == 4 && parts[2] == "prompt" && src != nil {
			name = "prompt"
			var v *uiPromptPage
			if v, err = u.promptView(src, parts[1], parts[3], now); err == nil && v == nil {
				http.NotFound(w, r)
				return
			}
			view = v
			break
		}
		if len(parts) != 2 || src == nil {
			http.NotFound(w, r)
			return
		}
		name = "session"
		var v *uiSessionPage
		if v, err = u.sessionView(src, parts[1], now); err == nil && v == nil {
			http.NotFound(w, r)
			return
		}
		view = v
	}
	if err != nil {
		http.Error(w, "the UI's data could not be read; see agentd's output", http.StatusInternalServerError)
		fmt.Fprintf(os.Stderr, "agentd ui: %v\n", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := uiTemplates.ExecuteTemplate(w, name, view); err != nil {
		fmt.Fprintf(os.Stderr, "agentd ui: %v\n", err)
	}
}

// uiEntriesDoc is a console's box as its page polls it (#59): the latest
// entries, or those before one, each rendered as the page renders it,
// with a hash that says when it changed; whether older ones remain; the
// open decisions; and the session chips.
type uiEntriesDoc struct {
	Entries []uiEntryDoc `json:"entries"`
	More    bool         `json:"more"`
	Open    []string     `json:"open"`
	Chips   string       `json:"chips"`
}

// uiEntryDoc is one entry of a uiEntriesDoc.
type uiEntryDoc struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
	HTML string `json:"html"`
}

// maxHave bounds the have list a poll may send, in bytes.
const maxHave = 16 << 10

// renderEntry is entry e of console v as the box shows it, and its hash,
// which the entry carries as data-hash so a page can name it in a poll.
func renderEntry(v *uiConsole, e uiEntry) (string, string, error) {
	var b strings.Builder
	if err := uiTemplates.ExecuteTemplate(&b, "entry", map[string]any{"P": v, "V": e}); err != nil {
		return "", "", err
	}
	hash := shortHash(HashPane(b.String()))
	return strings.Replace(b.String(), `data-hash=""`, `data-hash="`+hash+`"`, 1), hash, nil
}

// serveEntries answers a console page's poll with its box as JSON: the
// latest consoleWindow entries, or those before the entry before, or
// before the time at when the console no longer holds that entry. An
// entry the page has as it is, by have's ID.HASH list, comes without its
// HTML, so a poll of a quiet console is small.
func (u *UI) serveEntries(w http.ResponseWriter, v *uiConsole, before string, at time.Time, have string) {
	v.Entries, v.More = window(v.Entries, before, at, consoleWindow)
	known := map[string]bool{}
	if len(have) <= maxHave {
		for _, h := range strings.Split(have, ",") {
			known[h] = true
		}
	}
	doc := uiEntriesDoc{Entries: []uiEntryDoc{}, More: v.More, Open: v.Open}
	if doc.Open == nil {
		doc.Open = []string{}
	}
	for _, e := range v.Entries {
		html, hash, err := renderEntry(v, e)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agentd ui: %v\n", err)
			http.Error(w, "the console could not be rendered", http.StatusInternalServerError)
			return
		}
		d := uiEntryDoc{ID: e.DOMID, Hash: hash, HTML: html}
		if known[d.ID+"."+d.Hash] {
			d.HTML = ""
		}
		doc.Entries = append(doc.Entries, d)
	}
	var b strings.Builder
	if err := uiTemplates.ExecuteTemplate(&b, "chips", v); err == nil {
		doc.Chips = b.String()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(doc)
}

// maxSeen is the most items one POST may mark seen.
const maxSeen = 500

// checkPost reads a POST's form once it is known to come from the UI: its
// Origin is the UI's and it carries the UI's token. It answers a POST that
// does not, and says whether to go on.
func (u *UI) checkPost(w http.ResponseWriter, r *http.Request) bool {
	if o := r.Header.Get("Origin"); o == "" || !strings.EqualFold(o, "http://"+r.Host) {
		http.Error(w, "refused: a POST must come from the UI", http.StatusForbidden)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return false
	}
	return u.checkToken(w, r)
}

// maxUpload bounds a message's multipart body: its files, and room for
// its fields and the parts' headers.
const maxUpload = maxAttachments*maxAttachment + 1<<20

// checkPostFiles is checkPost for a form that may carry files (#61): a
// multipart one is read whole, at most maxUpload, its larger files
// spooled to temporary files the request removes when it ends; then its
// token, then its files are checked. Nothing is stored until all pass.
func (u *UI) checkPostFiles(w http.ResponseWriter, r *http.Request) ([]stagedAttachment, bool) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return nil, u.checkPost(w, r)
	}
	if o := r.Header.Get("Origin"); o == "" || !strings.EqualFold(o, "http://"+r.Host) {
		http.Error(w, "refused: a POST must come from the UI", http.StatusForbidden)
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, fmt.Sprintf("refused: a message carries at most %d files of at most %d MB each", maxAttachments, maxAttachment>>20), http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "bad form", http.StatusBadRequest)
		}
		return nil, false
	}
	if !u.checkToken(w, r) {
		return nil, false
	}
	files, err := checkAttachments(r.MultipartForm.File["file"])
	if err != nil {
		http.Error(w, "refused: "+err.Error()+"; nothing was sent", http.StatusBadRequest)
		return nil, false
	}
	return files, true
}

// checkToken checks a read form's token is the UI's.
func (u *UI) checkToken(w http.ResponseWriter, r *http.Request) bool {
	if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("token")), []byte(u.token)) != 1 {
		http.Error(w, "refused: the form's token is not the UI's", http.StatusForbidden)
		return false
	}
	return true
}

// postSource is the source a checked POST names, or nil after answering
// that it names none.
func (u *UI) postSource(w http.ResponseWriter, r *http.Request) *uiSource {
	src := u.source(r.PostForm.Get("src"))
	if src == nil {
		http.Error(w, "the form names no source the UI shows", http.StatusBadRequest)
	}
	return src
}

// back sends the owner back to the view the form came from: a path of
// the UI's own, never another site.
func (u *UI) back(w http.ResponseWriter, r *http.Request) {
	back := r.PostForm.Get("back")
	if p, err := url.Parse(back); err != nil || p.Scheme != "" || p.Host != "" || !strings.HasPrefix(back, "/") ||
		strings.HasPrefix(back, "//") || strings.ContainsAny(back, "\\\r\n") {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// serveSeen marks the items a POST names seen, then sends the owner back.
func (u *UI) serveSeen(w http.ResponseWriter, r *http.Request) {
	if !u.checkPost(w, r) {
		return
	}
	src := u.postSource(w, r)
	if src == nil {
		return
	}
	ids := r.PostForm["id"]
	if len(ids) == 0 || len(ids) > maxSeen {
		http.Error(w, fmt.Sprintf("name between 1 and %d items", maxSeen), http.StatusBadRequest)
		return
	}
	if err := src.markSeen(ids, r.RemoteAddr); err != nil {
		http.Error(w, "the items could not be marked seen; see agentd's output", http.StatusBadRequest)
		fmt.Fprintf(os.Stderr, "agentd ui: mark seen: %v\n", err)
		return
	}
	u.back(w, r)
}

// serveAnswer records the owner's answer to one decision, then sends the
// owner back. The form names the source, the session and the decision,
// and holds answer (yes or no), choice, choices, value or text, and other
// and a note; the answer is checked against the decision as the log
// holds it before it is recorded, with the address it came from.
func (u *UI) serveAnswer(w http.ResponseWriter, r *http.Request) {
	if !u.checkPost(w, r) {
		return
	}
	src := u.postSource(w, r)
	if src == nil {
		return
	}
	f := func(k string) string { return strings.TrimSpace(r.PostForm.Get(k)) }
	session := f("session")
	a, err := answerForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.ID, a.Text = f("id"), f("text")
	if session == "" || a.ID == "" {
		http.Error(w, "name the session and the decision", http.StatusBadRequest)
		return
	}
	if err := src.answer(session, a); err != nil {
		http.Error(w, "the answer was not recorded: "+err.Error(), http.StatusBadRequest)
		return
	}
	u.back(w, r)
}

// answerForm reads the answer a checked POST gives a decision: answer
// (yes or no), choice, choices, value, other and a note, with the address
// it came from. The decision and its text the caller reads.
func answerForm(r *http.Request) (AnswerPayload, error) {
	f := func(k string) string { return strings.TrimSpace(r.PostForm.Get(k)) }
	a := AnswerPayload{Choice: f("choice"), Other: f("other"), Note: f("note"), From: r.RemoteAddr}
	for _, k := range r.PostForm["choices"] {
		a.Choices = append(a.Choices, strings.TrimSpace(k))
	}
	// A value is a number when it reads as one, else a date's text; the
	// check against the decision says whether it is the decision's type.
	if v := f("value"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) {
			a.Value = json.RawMessage(strconv.FormatFloat(n, 'f', -1, 64))
		} else {
			a.Value, _ = json.Marshal(v)
		}
	}
	switch f("answer") {
	case "":
	case "yes", "no":
		yes := f("answer") == "yes"
		a.Answer = &yes
	default:
		return a, errors.New("answer is yes or no")
	}
	return a, nil
}

// serveRespond records the owner's response to one entry of a console
// (#59), then sends the owner back: the form names the source, the
// repository and the entry, and holds the owner's words and, for a
// decision, an answer as /answer takes it. Without an entry it is a
// reply to the console, to the session the form names, if any.
func (u *UI) serveRespond(w http.ResponseWriter, r *http.Request) {
	files, ok := u.checkPostFiles(w, r)
	if !ok {
		return
	}
	src := u.postSource(w, r)
	if src == nil {
		return
	}
	repo, entry, text := strings.TrimSpace(r.PostForm.Get("repository")), strings.TrimSpace(r.PostForm.Get("entry")), r.PostForm.Get("text")
	// A quick word the popover offers leads the owner's own.
	if q := strings.TrimSpace(r.PostForm.Get("quick")); q != "" {
		if t := strings.TrimSpace(text); t != "" {
			q += ". " + t
		}
		text = q
	}
	if repo == "" || len(text) > maxReply {
		http.Error(w, fmt.Sprintf("name the repository and write at most %d bytes", maxReply), http.StatusBadRequest)
		return
	}
	if entry == "" {
		if strings.TrimSpace(text) == "" && len(files) == 0 {
			http.Error(w, "write a message", http.StatusBadRequest)
			return
		}
		if err := src.reply(repo, strings.TrimSpace(r.PostForm.Get("session")), text, files); err != nil {
			http.Error(w, "the message was not recorded: "+err.Error(), http.StatusBadRequest)
			return
		}
		u.back(w, r)
		return
	}
	a, err := answerForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var ap *AnswerPayload
	if a.Binding() || a.Other != "" || strings.HasPrefix(entry, entryDecision) {
		ap = &a
	}
	if err := src.respond(repo, entry, strings.TrimSpace(r.PostForm.Get("session")), text, ap, files); err != nil {
		http.Error(w, "the response was not recorded: "+err.Error(), http.StatusBadRequest)
		return
	}
	u.back(w, r)
}

// serveDeadline records the deadline the owner set on one decision, then
// sends the owner back. The form names the source, the session and the
// decision and holds the deadline, a local date and time as the browser
// writes it, or RFC 3339.
func (u *UI) serveDeadline(w http.ResponseWriter, r *http.Request) {
	if !u.checkPost(w, r) {
		return
	}
	src := u.postSource(w, r)
	if src == nil {
		return
	}
	f := func(k string) string { return strings.TrimSpace(r.PostForm.Get(k)) }
	session, id, v := f("session"), f("id"), f("deadline")
	at, err := time.ParseInLocation("2006-01-02T15:04", v, u.loc)
	if err != nil {
		at, err = time.Parse(time.RFC3339, v)
	}
	if session == "" || id == "" || err != nil {
		http.Error(w, "name the session and the decision, and give the deadline as a date and time", http.StatusBadRequest)
		return
	}
	if err := src.deadline(session, id, at); err != nil {
		http.Error(w, "the deadline was not recorded: "+err.Error(), http.StatusBadRequest)
		return
	}
	u.back(w, r)
}

// maxReply is the longest console reply, in bytes.
const maxReply = 4000

// serveReply records a console reply as the owner's instruction for the
// repository the form names, then sends the owner back to its console.
func (u *UI) serveReply(w http.ResponseWriter, r *http.Request) {
	files, ok := u.checkPostFiles(w, r)
	if !ok {
		return
	}
	src := u.postSource(w, r)
	if src == nil {
		return
	}
	repo, to, text := strings.TrimSpace(r.PostForm.Get("repository")), strings.TrimSpace(r.PostForm.Get("session")), r.PostForm.Get("text")
	if repo == "" || strings.TrimSpace(text) == "" && len(files) == 0 || len(text) > maxReply {
		http.Error(w, fmt.Sprintf("name the repository and write a reply of at most %d bytes", maxReply), http.StatusBadRequest)
		return
	}
	if err := src.reply(repo, to, text, files); err != nil {
		http.Error(w, "the reply was not recorded: "+err.Error(), http.StatusBadRequest)
		return
	}
	u.back(w, r)
}

// serveAttachment serves /attachment/SRC/ID, a file the owner sent, from
// where agentd stored it, as the type its bytes read as: only to the
// owner, as every page, and never as a page of the UI's that runs
// anything.
func (u *UI) serveAttachment(w http.ResponseWriter, r *http.Request) {
	srcName, id, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/attachment/"), "/")
	src := u.source(srcName)
	if src == nil || id == "" {
		http.NotFound(w, r)
		return
	}
	a, path, ok := src.attachment(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "the file is gone from agentd's state directory", http.StatusNotFound)
		return
	}
	defer f.Close()
	h := w.Header()
	// A PDF opens in the browser's viewer; nothing else of it runs.
	h.Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; object-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'self'")
	h.Set("Content-Type", a.Type)
	if a.Type == "text/plain" {
		h.Set("Content-Type", "text/plain; charset=utf-8")
	}
	h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": a.Name}))
	h.Set("Cache-Control", "private, max-age=86400, immutable")
	http.ServeContent(w, r, "", time.Time{}, f)
}

// hostAllowed checks the Host header against the configured names, so a
// UI reached through another name (a rebinding DNS name) is refused.
func (u *UI) hostAllowed(host string) bool {
	for _, h := range u.cfg.Hosts {
		if strings.EqualFold(host, h) {
			return true
		}
	}
	return false
}

// uiSources opens the logs the UI shows: cfg's, then each of ui.logs'
// configs', each with its shadow run after it when ui.shadow is set. A
// source is named after its config file, made unique. It returns the
// logs to close.
func uiSources(cfg *Config, host string) ([]*uiSource, []*Log, error) {
	cfgs := []*Config{cfg}
	for _, p := range cfg.UI.Logs {
		c, err := LoadConfig(p)
		if err != nil {
			return nil, nil, err
		}
		cfgs = append(cfgs, c)
	}
	var sources []*uiSource
	var logs []*Log
	used := map[string]bool{}
	for _, c := range cfgs {
		log, err := OpenLog(c.EventLog)
		if err != nil {
			for _, l := range logs {
				l.Close()
			}
			return nil, nil, err
		}
		logs = append(logs, log)
		base := strings.TrimSuffix(filepath.Base(c.Path), filepath.Ext(c.Path))
		if base == "" || base == "." {
			base = "agentd"
		}
		name := base
		for i := 2; used[name]; i++ {
			name = base + "-" + strconv.Itoa(i)
		}
		used[name] = true
		sources = append(sources, newUISource(name, c, log, ModeLive, host))
		if cfg.UI.Shadow {
			used[name+"-shadow"] = true
			shadow := newUISource(name+"-shadow", c, log, ModeShadow, host)
			shadow.echo = true
			sources = append(sources, shadow)
		}
	}
	return sources, logs, nil
}

// uiCmd serves the owner's UI until interrupted.
func uiCmd(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	sources, logs, err := uiSources(cfg, host)
	if err != nil {
		return err
	}
	defer func() {
		for _, l := range logs {
			l.Close()
		}
	}()
	ui, err := NewUI(cfg.UI, ExecWhois{Argv: cfg.UI.Whois, Timeout: cfg.Timeouts.Tmux.Duration}, sources, time.Now)
	if err != nil {
		return err
	}
	if _, err := ui.indexView(time.Now()); err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.UI.Address, Handler: ui, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 1 << 16}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	fmt.Fprintf(os.Stderr, "agentd ui: serving %s for %s, %d sources\n", cfg.UI.Address, cfg.UI.Owner, len(sources))
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
