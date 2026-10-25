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
	"math"
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
// repository, and uiKeptEvents how many events for each session.
const (
	uiKeptMessages = 200
	uiKeptEvents   = 40
)

// uiMessage is one line of a repository's console: a feed post, an
// escalation of one of its sessions, an instruction of the owner's, or a
// line of the old console's history.
type uiMessage struct {
	At      time.Time
	Kind    string // feed, escalation, instruction, imported
	From    string
	Session string
	Text    string
}

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
	messages map[string][]uiMessage // by repository, oldest first
	events   map[string][]Event     // by session, oldest first
	askedAt  map[string]time.Time   // when each decision was first seen, by session and id
	// echo marks a shadow source over a live source's log: the owner's
	// records and the shared facts are the live source's to show.
	echo bool
}

func newUISource(name string, cfg *Config, log *Log, mode, host string) *uiSource {
	return &uiSource{Name: name, cfg: cfg, log: log, st: NewLogState(cfg, mode), host: host,
		messages: map[string][]uiMessage{}, events: map[string][]Event{}, askedAt: map[string]time.Time{}}
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
	// imported history never pushes out what is new.
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
		if len(ms) > uiKeptMessages {
			ms = ms[len(ms)-uiKeptMessages:]
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
		switch {
		case e.Feed != nil && (e.Feed.Phase == FeedIntent || e.Feed.Phase == FeedWould) && e.Feed.Text != "":
			add(e.Feed.Repository, uiMessage{At: e.Time, Kind: "feed", From: "agentd", Text: e.Feed.Text})
		case src.echo && owners:
		case e.Feed != nil && e.Feed.Phase == FeedImported:
			at := e.Time
			if e.Feed.At != nil {
				at = *e.Feed.At
			}
			add(e.Feed.Repository, uiMessage{At: at, Kind: "imported", From: e.Feed.From, Text: e.Feed.Text})
		case e.Act != nil && (e.Kind == KindAction || e.Kind == KindWould) && (e.Act.Action == ActEscalate || e.Act.Action == ActUpdate):
			add(repoOf(e.Session), uiMessage{At: e.Time, Kind: "escalation", From: "agentd", Session: e.Session, Text: e.Act.Message})
		case e.Kind == KindInstruction && e.Instruction != nil:
			for _, r := range e.Instruction.Repositories {
				add(r, uiMessage{At: e.Time, Kind: "instruction", From: e.Instruction.From, Text: e.Instruction.Text})
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

// reply records the owner's console reply as an instruction for repo.
func (src *uiSource) reply(repo, text string) error {
	src.mu.Lock()
	defer src.mu.Unlock()
	if err := src.catchUp(); err != nil {
		return err
	}
	return UIInstruction(src.cfg, src.log, src.st, time.Now(), repo, text)
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
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
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
		"/seen": u.serveSeen, "/answer": u.serveAnswer, "/deadline": u.serveDeadline, "/reply": u.serveReply,
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
		var v *uiConsole
		if v, err = u.consoleView(strings.TrimPrefix(path, "/console/"), now); err == nil && v == nil {
			http.NotFound(w, r)
			return
		}
		view = v
	default:
		parts := strings.Split(strings.TrimPrefix(path, "/session/"), "/")
		src := u.source(parts[0])
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
	a := AnswerPayload{ID: f("id"), Choice: f("choice"), Text: f("text"), Other: f("other"), Note: f("note"), From: r.RemoteAddr}
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
		http.Error(w, "answer is yes or no", http.StatusBadRequest)
		return
	}
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
	if !u.checkPost(w, r) {
		return
	}
	src := u.postSource(w, r)
	if src == nil {
		return
	}
	repo, text := strings.TrimSpace(r.PostForm.Get("repository")), r.PostForm.Get("text")
	if repo == "" || strings.TrimSpace(text) == "" || len(text) > maxReply {
		http.Error(w, fmt.Sprintf("name the repository and write a reply of at most %d bytes", maxReply), http.StatusBadRequest)
		return
	}
	if err := src.reply(repo, text); err != nil {
		http.Error(w, "the reply was not recorded: "+err.Error(), http.StatusBadRequest)
		return
	}
	u.back(w, r)
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
