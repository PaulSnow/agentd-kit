package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// PageConfig is the owner's page: where it listens, the Host names it
// answers to, and how it learns who is asking. Whois is an argv with {addr},
// the caller's address, substituted; it prints the caller's login, either
// alone on its first line or as the UserProfile.LoginName of a JSON object.
// Owner is the login the page serves; everyone else is refused.
type PageConfig struct {
	Address string   `json:"address"`
	Hosts   []string `json:"hosts"`
	Whois   []string `json:"whois"`
	Owner   string   `json:"owner"`
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

// PageData is everything one view is rendered from: the inventory, when
// the tracking files are configured, the report and what needs the owner,
// both from the page's state, the decisions sessions raised, and the
// items the owner has not yet seen.
type PageData struct {
	Inventory *Inventory
	Report    Report
	Needs     []PageNeed
	Decisions []PageDecision
	New       []*Item
}

// PageDecision is one open decision a session raised, with the owner's
// answer to it so far. A withheld decision matched a confidential pattern:
// only its id and type are shown, and no form.
type PageDecision struct {
	Session    string
	Repository string
	Decision
	Answered *AnswerRecord
	Withheld bool
}

// DecisionsFrom lists the open decisions of the report's sessions, each
// session's in the order it raised them: STATUS.md's Questions first, as
// a text decision whose context is the text the report shows, then those
// of questions.json. Every text is screened again.
func DecisionsFrom(cfg *Config, st *LogState, rep Report) []PageDecision {
	secret := confidentialMatcher(cfg)
	repoOf := map[string]string{}
	asked := map[string]string{}
	for _, rr := range rep.Repositories {
		for _, c := range rr.Cases {
			repoOf[c.Session] = rr.Name
		}
		for _, q := range rr.Questions {
			asked[q.Session] = q.Text
		}
	}
	names := make([]string, 0, len(repoOf))
	for n := range repoOf {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []PageDecision
	for _, n := range names {
		t := st.Tracks[n]
		if t == nil {
			continue
		}
		ds := t.Decisions
		if t.StatusQuestions != "" {
			d, _ := t.decision(statusDecision + t.StatusQuestions)
			d.Context = asked[n]
			ds = append([]Decision{d}, ds...)
		}
		for _, d := range ds {
			pd := PageDecision{Session: n, Repository: repoOf[n], Decision: d}
			if a, ok := t.answer(d.ID); ok {
				pd.Answered = &a
			}
			texts := []string{d.Question, d.Context, d.Why, d.IfWrong, d.Recommendation}
			for _, o := range d.Options {
				texts = append(texts, o.Label, o.Consequence)
			}
			// STATUS.md's Questions with no text in the report were
			// withheld when they were posted, or were never posted.
			if secret(texts...) || strings.HasPrefix(d.ID, statusDecision) && d.Context == "" {
				pd.Decision = Decision{ID: d.ID, Type: d.Type, Blocking: d.Blocking}
				pd.Withheld = true
			}
			out = append(out, pd)
		}
	}
	return out
}

// PageNeed is one thing waiting on the owner: a session's open Questions,
// a session parked or blocked for the owner, or an escalation.
type PageNeed struct {
	Kind       string // questions, parked, escalation
	Session    string
	Repository string
	Since      time.Time
	Text       string
}

// needsWindow is how far back the page lists escalations under Needs you.
const needsWindow = 24 * time.Hour

// NeedsFrom lists what waits on the owner, from st and the report folded
// from it: every open Questions section, every session parked until the
// owner clears it (crash-looping, or stopped by its budget), and the
// escalations of the last needsWindow other than Questions.
// Only the report's sessions count, so a session the report leaves out as
// confidential is left out here too, and every text is screened again.
func NeedsFrom(cfg *Config, st *LogState, rep Report, now time.Time) []PageNeed {
	secret := confidentialMatcher(cfg)
	repoOf := map[string]string{}
	asked := map[string]bool{}
	var needs []PageNeed
	for _, rr := range rep.Repositories {
		for _, c := range rr.Cases {
			repoOf[c.Session] = rr.Name
		}
		for _, q := range rr.Questions {
			asked[q.Session] = true
			needs = append(needs, PageNeed{Kind: "questions", Session: q.Session, Repository: rr.Name, Since: q.Since, Text: q.Text})
		}
	}
	names := make([]string, 0, len(repoOf))
	for n := range repoOf {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t := st.Tracks[n]
		if t == nil {
			continue
		}
		// Open Questions whose text was withheld still wait on the owner:
		// the generic message is what was posted.
		if t.Questions.Open && !asked[n] {
			needs = append(needs, PageNeed{Kind: "questions", Session: n, Repository: repoOf[n], Since: t.AskedAt,
				Text: "open Questions; the details were withheld because they matched a confidential pattern. Look at the session directly."})
		}
		parked := t.State == CrashLooping || t.Stopped != ""
		if why := t.Reason; parked && !secret(why, t.Stopped) {
			if t.Stopped != "" {
				why = "stopped: " + t.Stopped
			}
			needs = append(needs, PageNeed{Kind: "parked", Session: n, Repository: repoOf[n], Since: t.Since, Text: why})
		}
		for _, e := range t.Escalated {
			// A parked session is listed once, as parked.
			if parked || now.Sub(e.At) > needsWindow || strings.HasPrefix(e.Key, "questions:") || secret(e.Message) {
				continue
			}
			needs = append(needs, PageNeed{Kind: "escalation", Session: n, Repository: repoOf[n], Since: e.At, Text: e.Message})
		}
	}
	sort.SliceStable(needs, func(i, j int) bool { return needs[i].Since.After(needs[j].Since) })
	return needs
}

// pageState is the page's projection of the log: one LogState, brought up
// to date through apply with the lines appended since it last looked, so
// a request reads only what is new.
type pageState struct {
	mu   sync.Mutex
	cfg  *Config
	log  *Log
	off  int64
	st   *LogState
	host string
	tr   bool // the tracking files are configured
}

func newPageState(cfg *Config, log *Log, mode, host string) *pageState {
	return &pageState{cfg: cfg, log: log, st: NewLogState(cfg, mode), host: host,
		tr: cfg.Tracking.Manifest != "" || cfg.Tracking.Snapshot != ""}
}

// load brings the state up to date and reads the page's data from it.
func (ps *pageState) load() (PageData, error) {
	var data PageData
	if ps.tr {
		inv, err := LoadInventory(ps.cfg.Tracking)
		if err != nil {
			return data, err
		}
		data.Inventory = inv
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if err := ps.catchUp(); err != nil {
		return data, err
	}
	now := time.Now()
	data.Report = ReportFrom(ps.cfg, ps.st, ps.host, now)
	data.Needs = NeedsFrom(ps.cfg, ps.st, data.Report, now)
	data.Decisions = DecisionsFrom(ps.cfg, ps.st, data.Report)
	data.New = ps.st.SortedItems(true)
	SortByUrgency(data.New)
	return data, nil
}

// catchUp applies the events appended since the last look.
func (ps *pageState) catchUp() error {
	events, off, err := ps.log.Tail(ps.off)
	if err != nil {
		return err
	}
	ps.off = off
	ps.st.Fold(events)
	return nil
}

// markSeen records the owner marking items seen from the page. Only items
// the state holds are marked; an unknown id is an error and marks nothing.
func (ps *pageState) markSeen(ids []string, from string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if err := ps.catchUp(); err != nil {
		return err
	}
	for _, id := range ids {
		if ps.st.Items[id] == nil {
			return fmt.Errorf("no item %q", id)
		}
	}
	return MarkSeen(ps.log, time.Now(), ids, from)
}

// answer records the owner's answer to one of session's open decisions,
// checked against the decision as the log holds it now.
func (ps *pageState) answer(session string, a AnswerPayload) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if err := ps.catchUp(); err != nil {
		return err
	}
	return RecordAnswer(ps.log, ps.st, time.Now(), session, a)
}

// whoisTTL is how long the page trusts one whois answer for an address,
// and whoisSlots how many whois commands may run at once: a burst of
// requests runs no burst of commands.
const (
	whoisTTL   = time.Minute
	whoisSlots = 4
)

type whoisAnswer struct {
	login string
	at    time.Time
}

// Page serves the owner's view. Every request is checked before anything
// is read: its Host must be one the page answers to, it must not come
// from the page's own address or loopback (a process on this machine
// would otherwise borrow the machine's identity), and whois must name the
// owner. A POST must also come from the page itself: its Origin must be
// the page's, and it must carry the token the page put in its form.
type Page struct {
	cfg    PageConfig
	whois  Whois
	load   func() (PageData, error)
	mark   func(ids []string, from string) error
	answer func(session string, a AnswerPayload) error
	self   net.IP // the address the page listens on
	now    func() time.Time
	token  string // the form token, new each time the page starts

	mu      sync.Mutex
	answers map[string]whoisAnswer // by caller IP
	slots   chan struct{}
}

// NewPage builds the page's handler.
func NewPage(cfg PageConfig, whois Whois, load func() (PageData, error), mark func(ids []string, from string) error,
	answer func(session string, a AnswerPayload) error, now func() time.Time) (*Page, error) {
	host, _, err := net.SplitHostPort(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("page.address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() {
		return nil, fmt.Errorf("page.address %q must be one IP address, not a name or every address", cfg.Address)
	}
	if cfg.Owner == "" || len(cfg.Whois) == 0 {
		return nil, errors.New("page.owner and page.whois must be set")
	}
	if len(cfg.Hosts) == 0 {
		cfg.Hosts = []string{cfg.Address}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return &Page{cfg: cfg, whois: whois, load: load, mark: mark, answer: answer, self: ip, now: now, token: hex.EncodeToString(b),
		answers: map[string]whoisAnswer{}, slots: make(chan struct{}, whoisSlots)}, nil
}

// login is whois's answer for the caller, from the cache while it is
// fresh. A failure is logged once per address per whoisTTL.
func (p *Page) login(remote string, ip net.IP) (string, error) {
	key := ip.String()
	p.mu.Lock()
	a, ok := p.answers[key]
	p.mu.Unlock()
	if ok && p.now().Sub(a.at) < whoisTTL {
		return a.login, nil
	}
	select {
	case p.slots <- struct{}{}:
	default:
		return "", errors.New("too many whois commands running")
	}
	login, err := p.whois.Login(remote)
	<-p.slots
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentd page: refused %s: %v\n", key, err)
	}
	p.mu.Lock()
	if len(p.answers) > 1024 {
		p.answers = map[string]whoisAnswer{}
	}
	// A failure is not kept: one timeout must not lock the owner out for
	// whoisTTL. The slots bound how often whois runs.
	if err == nil {
		p.answers[key] = whoisAnswer{login: login, at: p.now()}
	}
	p.mu.Unlock()
	return login, err
}

func (p *Page) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	if !p.hostAllowed(r.Host) {
		http.Error(w, "unknown host", http.StatusForbidden)
		return
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(remote)
	if err != nil || ip == nil || ip.Equal(p.self) || ip.IsLoopback() || ip.IsUnspecified() {
		http.Error(w, "refused: the page does not answer its own machine", http.StatusForbidden)
		return
	}
	login, err := p.login(r.RemoteAddr, ip)
	if err != nil || login != p.cfg.Owner {
		http.Error(w, "refused: only the owner may use this page", http.StatusForbidden)
		return
	}
	switch {
	case r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
	case r.URL.Path == "/seen" && r.Method == http.MethodPost:
		p.serveSeen(w, r)
		return
	case r.URL.Path == "/answer" && r.Method == http.MethodPost:
		p.serveAnswer(w, r)
		return
	case r.URL.Path == "/" || r.URL.Path == "/seen" || r.URL.Path == "/answer":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	default:
		http.NotFound(w, r)
		return
	}
	data, err := p.load()
	if err != nil {
		http.Error(w, "the page's data could not be read; see agentd's output", http.StatusInternalServerError)
		fmt.Fprintf(os.Stderr, "agentd page: %v\n", err)
		return
	}
	view := buildView(data, parseViewQuery(r.URL.Query()), p.now())
	view.Token = p.token
	h.Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, view); err != nil {
		fmt.Fprintf(os.Stderr, "agentd page: %v\n", err)
	}
}

// maxSeen is the most items one POST may mark seen.
const maxSeen = 500

// serveSeen marks the items a POST names seen, then sends the owner back
// to the view they came from. The POST must name the page as its Origin
// and carry the page's token.
func (p *Page) serveSeen(w http.ResponseWriter, r *http.Request) {
	if !p.checkPost(w, r) {
		return
	}
	ids := r.PostForm["id"]
	if len(ids) == 0 || len(ids) > maxSeen {
		http.Error(w, fmt.Sprintf("name between 1 and %d items", maxSeen), http.StatusBadRequest)
		return
	}
	if err := p.mark(ids, r.RemoteAddr); err != nil {
		http.Error(w, "the items could not be marked seen; see agentd's output", http.StatusBadRequest)
		fmt.Fprintf(os.Stderr, "agentd page: mark seen: %v\n", err)
		return
	}
	p.back(w, r)
}

// checkPost reads a POST's form once it is known to come from the page:
// its Origin is the page's and it carries the page's token. It answers a
// POST that does not, and says whether to go on.
func (p *Page) checkPost(w http.ResponseWriter, r *http.Request) bool {
	if o := r.Header.Get("Origin"); o == "" || !strings.EqualFold(o, "http://"+r.Host) {
		http.Error(w, "refused: a POST must come from the page", http.StatusForbidden)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return false
	}
	if subtle.ConstantTimeCompare([]byte(r.PostForm.Get("token")), []byte(p.token)) != 1 {
		http.Error(w, "refused: the form's token is not the page's", http.StatusForbidden)
		return false
	}
	return true
}

// back sends the owner back to the view the form came from, rebuilt from
// its parameters, never a target the form names.
func (p *Page) back(w http.ResponseWriter, r *http.Request) {
	back, _ := url.ParseQuery(r.PostForm.Get("view"))
	http.Redirect(w, r, "/"+parseViewQuery(back).link(nil), http.StatusSeeOther)
}

// serveAnswer records the owner's answer to one decision, then sends the
// owner back. The form names the session and the decision, and holds
// answer (yes or no), choice, text or other, and a note; the answer is
// checked against the decision as the log holds it before it is
// recorded, with the address it came from.
func (p *Page) serveAnswer(w http.ResponseWriter, r *http.Request) {
	if !p.checkPost(w, r) {
		return
	}
	f := func(k string) string { return strings.TrimSpace(r.PostForm.Get(k)) }
	session := f("session")
	a := AnswerPayload{ID: f("id"), Choice: f("choice"), Text: f("text"), Other: f("other"), Note: f("note"), From: r.RemoteAddr}
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
	if err := p.answer(session, a); err != nil {
		http.Error(w, "the answer was not recorded: "+err.Error(), http.StatusBadRequest)
		return
	}
	p.back(w, r)
}

// hostAllowed checks the Host header against the configured names, so a
// page reached through another name (a rebinding DNS name) is refused.
func (p *Page) hostAllowed(host string) bool {
	for _, h := range p.cfg.Hosts {
		if strings.EqualFold(host, h) {
			return true
		}
	}
	return false
}

// viewQuery is the view's sort and filters, all carried in the URL so a
// link opens the same view.
type viewQuery struct {
	RepoSort   string // needs, recent, work, name
	Show       string // open, all, hidden
	IssueSort  string // needs, oldest, recent, number
	HideRepos  map[string]bool
	HideIssues map[string]bool // key#number
}

var (
	repoSorts  = []string{"needs", "recent", "work", "name"}
	shows      = []string{"open", "all", "hidden"}
	issueSorts = []string{"needs", "oldest", "recent", "number"}
)

// oneOf returns v when it is one of allowed, else the first, the default.
func oneOf(v string, allowed []string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return allowed[0]
}

func parseViewQuery(q url.Values) viewQuery {
	v := viewQuery{RepoSort: oneOf(q.Get("sort"), repoSorts), Show: oneOf(q.Get("show"), shows),
		IssueSort: oneOf(q.Get("isort"), issueSorts), HideRepos: map[string]bool{}, HideIssues: map[string]bool{}}
	for _, k := range q["hide"] {
		v.HideRepos[k] = true
	}
	for _, k := range q["hidei"] {
		v.HideIssues[k] = true
	}
	return v
}

// link is the query for v, with change applied to a copy of it.
func (v viewQuery) link(change func(*viewQuery)) string {
	c := v
	c.HideRepos, c.HideIssues = map[string]bool{}, map[string]bool{}
	for k := range v.HideRepos {
		c.HideRepos[k] = true
	}
	for k := range v.HideIssues {
		c.HideIssues[k] = true
	}
	if change != nil {
		change(&c)
	}
	q := url.Values{}
	q.Set("sort", c.RepoSort)
	q.Set("show", c.Show)
	q.Set("isort", c.IssueSort)
	for _, set := range []struct {
		name string
		keys map[string]bool
	}{{"hide", c.HideRepos}, {"hidei", c.HideIssues}} {
		keys := make([]string, 0, len(set.keys))
		for k := range set.keys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			q.Add(set.name, k)
		}
	}
	return "?" + q.Encode()
}

// pageView is what the template renders.
type pageView struct {
	Now       time.Time
	Token     string // the form token
	Needs     []PageNeed
	Decisions []PageDecision
	New       []*Item
	ViewQ     string // the view's query, for the form to come back to
	AsOf      time.Time
	Digest    string
	Query     viewQuery
	Repos     []pageRepo
	Hidden    int
	Links     []pageLink // the sort and show controls
	Untraced  bool       // no tracking files: repositories come from the log only
}

type pageLink struct {
	Label, Href string
	Current     bool
}

type pageRepo struct {
	Key, Name string
	NeedsYou  int
	OpenWork  int
	Activity  time.Time
	Hidden    bool
	HideLink  string
	Cases     []ReportCase
	Issues    []pageIssue
	// HiddenIssues counts the issues hidden in this repository, and
	// ShowIssues shows them again.
	HiddenIssues int
	ShowIssues   string
}

type pageIssue struct {
	Number   int
	Title    string
	Created  time.Time
	Updated  time.Time
	HideLink string
}

// buildView joins the inventory's repositories and issues with the
// report's sessions and questions, then sorts and filters them as q says.
// Hiding changes the view only. A hidden repository that needs the owner
// is shown all the same.
func buildView(data PageData, q viewQuery, now time.Time) pageView {
	v := pageView{Now: now, Digest: data.Report.Digest, Query: q, Untraced: data.Inventory == nil, Needs: data.Needs,
		Decisions: data.Decisions, New: data.New,
		ViewQ: strings.TrimPrefix(q.link(nil), "?")}
	repos := map[string]*pageRepo{}
	var order []string
	get := func(key, name string) *pageRepo {
		r := repos[key]
		if r == nil {
			if name == "" {
				name = "(no repository)"
			}
			r = &pageRepo{Key: key, Name: name}
			repos[key] = r
			order = append(order, key)
		}
		return r
	}
	if inv := data.Inventory; inv != nil {
		v.AsOf = inv.AsOf
		for _, rec := range inv.Repos {
			r := get(rec.Key, rec.Name)
			r.OpenWork = len(rec.OpenIssues) + len(rec.OpenMRs)
			for _, is := range rec.OpenIssues {
				r.Issues = append(r.Issues, pageIssue{Number: is.Number, Title: is.Title, Created: is.CreatedAt, Updated: is.UpdatedAt})
				if is.UpdatedAt.After(r.Activity) {
					r.Activity = is.UpdatedAt
				}
			}
		}
	}
	for _, rr := range data.Report.Repositories {
		r := get(rr.Repository, rr.Name)
		r.Cases = rr.Cases
		needs := map[string]bool{}
		for _, c := range rr.Cases {
			if c.State == string(BlockedOnOwner) {
				needs[c.Session] = true
			}
			if c.Since.After(r.Activity) {
				r.Activity = c.Since
			}
		}
		for _, qu := range rr.Questions {
			if qu.Since.After(r.Activity) {
				r.Activity = qu.Since
			}
		}
		// Whatever Needs you lists for the repository counts: questions,
		// parked and stopped sessions, escalations.
		for _, nd := range data.Needs {
			if nd.Repository == rr.Name {
				needs[nd.Session] = true
			}
		}
		r.NeedsYou = len(needs)
	}
	for _, key := range order {
		r := repos[key]
		r.Hidden = q.HideRepos[key]
		k := key
		if r.Hidden {
			r.HideLink = q.link(func(c *viewQuery) { delete(c.HideRepos, k) })
		} else {
			r.HideLink = q.link(func(c *viewQuery) { c.HideRepos[k] = true })
		}
		var issues []pageIssue
		var hidden []string
		for _, is := range r.Issues {
			ik := fmt.Sprintf("%s#%d", key, is.Number)
			if q.HideIssues[ik] {
				r.HiddenIssues++
				hidden = append(hidden, ik)
				continue
			}
			is.HideLink = q.link(func(c *viewQuery) { c.HideIssues[ik] = true })
			issues = append(issues, is)
		}
		if len(hidden) > 0 {
			r.ShowIssues = q.link(func(c *viewQuery) {
				for _, ik := range hidden {
					delete(c.HideIssues, ik)
				}
			})
		}
		r.Issues = issues
		sortIssues(r.Issues, q.IssueSort)
		active := r.OpenWork > 0 || len(r.Cases) > 0 || r.NeedsYou > 0
		switch {
		case q.Show == "hidden" && !r.Hidden:
		case q.Show == "open" && !active:
		case q.Show != "hidden" && r.Hidden && r.NeedsYou == 0:
			v.Hidden++ // one the filter would show, but for hiding
		default:
			v.Repos = append(v.Repos, *r)
		}
	}
	sortRepos(v.Repos, q.RepoSort)
	for _, s := range repoSorts {
		s := s
		v.Links = append(v.Links, pageLink{Label: "sort: " + s, Href: q.link(func(c *viewQuery) { c.RepoSort = s }), Current: q.RepoSort == s})
	}
	for _, s := range shows {
		s := s
		v.Links = append(v.Links, pageLink{Label: "show: " + s, Href: q.link(func(c *viewQuery) { c.Show = s }), Current: q.Show == s})
	}
	for _, s := range issueSorts {
		s := s
		v.Links = append(v.Links, pageLink{Label: "issues: " + s, Href: q.link(func(c *viewQuery) { c.IssueSort = s }), Current: q.IssueSort == s})
	}
	return v
}

// sortRepos orders repositories as by says, each order falling back to
// name, then key.
func sortRepos(rs []pageRepo, by string) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		switch by {
		case "needs":
			if a.NeedsYou != b.NeedsYou {
				return a.NeedsYou > b.NeedsYou
			}
		case "recent":
			if !a.Activity.Equal(b.Activity) {
				return a.Activity.After(b.Activity)
			}
		case "work":
			if a.OpenWork != b.OpenWork {
				return a.OpenWork > b.OpenWork
			}
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Key < b.Key
	})
}

// sortIssues orders issues within a repository. Until issues carry their
// cases' questions, needs-you first is by number.
func sortIssues(is []pageIssue, by string) {
	sort.SliceStable(is, func(i, j int) bool {
		a, b := is[i], is[j]
		switch by {
		case "oldest":
			if !a.Created.Equal(b.Created) {
				return a.Created.Before(b.Created)
			}
		case "recent":
			if !a.Updated.Equal(b.Updated) {
				return a.Updated.After(b.Updated)
			}
		}
		return a.Number < b.Number
	})
}

var pageTemplate = template.Must(template.New("page").Funcs(template.FuncMap{
	"when": func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.Local().Format("2006-01-02 15:04")
	},
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>agentd</title>
<style>
body { font: 15px/1.4 system-ui, sans-serif; margin: 1rem; max-width: 70rem; }
nav a { margin-right: .6rem; } nav a.current { font-weight: bold; text-decoration: none; }
section { border-top: 1px solid #ccc; padding: .5rem 0; } h2 { font-size: 1.05rem; margin: .2rem 0; }
.needs { color: #a40000; font-weight: bold; } .muted { color: #666; } pre { white-space: pre-wrap; margin: .2rem 0 .2rem 1rem; }
table { border-collapse: collapse; } td { padding: .1rem .6rem .1rem 0; vertical-align: top; }
</style></head><body>
<h1>agentd</h1>
<p class="muted">{{.Digest}}{{if not .AsOf.IsZero}} Tracking snapshot of {{when .AsOf}}.{{end}}{{if .Untraced}} No tracking files are configured; repositories come from the event log only.{{end}}</p>
<section id="needs"><h2>Needs you</h2>
{{range .Needs}}<div><span class="needs">{{.Session}}</span> <span class="muted">{{.Repository}}, {{.Kind}}, since {{when .Since}}</span></div><pre>{{.Text}}</pre>
{{else}}<p class="muted">Nothing waits on you.</p>{{end}}
</section>
{{if .Decisions}}<section id="decisions"><h2>Decisions</h2>
{{range .Decisions}}<div><span class="needs">{{.Session}}</span> <span class="muted">{{.Repository}}, {{.ID}}, {{.Type}}{{if .Blocking}}, blocking{{end}}</span></div>
{{if .Withheld}}<p class="muted">The details were withheld because they matched a confidential pattern, or were never posted. Look at the session directly.</p>
{{else}}<pre>{{.Question}}</pre>{{if .Context}}<pre class="muted">{{.Context}}</pre>{{end}}
{{if .Why}}<p class="muted">Why: {{.Why}}</p>{{end}}{{if .IfWrong}}<p class="muted">If wrong: {{.IfWrong}}</p>{{end}}{{if .Recommendation}}<p>Recommended: {{.Recommendation}}</p>{{end}}
{{with .Answered}}<p>Answered {{when .At}}:{{with .Answer}} {{if .}}yes{{else}}no{{end}}{{end}} {{.Choice}} {{.Text}} {{.Other}}{{if .Delivered}} <span class="muted">(delivered)</span>{{end}}</p>{{end}}
<form method="post" action="/answer"><input type="hidden" name="token" value="{{$.Token}}"><input type="hidden" name="view" value="{{$.ViewQ}}"><input type="hidden" name="session" value="{{.Session}}"><input type="hidden" name="id" value="{{.ID}}">
{{if eq .Type "yes_no"}}<label><input type="radio" name="answer" value="yes"> yes</label> <label><input type="radio" name="answer" value="no"> no</label>
{{else if eq .Type "choose_one"}}{{range .Options}}<div><label><input type="radio" name="choice" value="{{.Key}}"> {{.Key}}: {{.Label}}</label> <span class="muted">{{.Consequence}}</span></div>{{end}}
{{else}}<div><textarea name="text" rows="3" cols="60" maxlength="2000"></textarea></div>{{end}}
<div><textarea name="other" rows="1" cols="60" maxlength="2000" placeholder="or in your own words, which does not settle it"></textarea></div>
<div><input name="note" size="60" maxlength="500" placeholder="a note"> <button type="submit">Answer</button></div></form>{{end}}
{{end}}</section>{{end}}
<section id="new"><h2>New since last visit</h2>
{{if .New}}<form method="post" action="/seen"><input type="hidden" name="token" value="{{.Token}}"><input type="hidden" name="view" value="{{.ViewQ}}">
<table>{{range .New}}<tr><td><input type="checkbox" name="id" value="{{.ID}}" checked></td><td class="muted">{{when .SourceTime}}</td><td>{{.Kind}}</td><td>{{.Repository}}</td><td>{{if .Link}}<a href="{{.Link}}">{{.Title}}</a>{{else}}{{.Title}}{{end}}</td><td class="muted">{{.Author}}</td>{{with .Triaged}}<td>{{.NeedsOwner}}/{{.Urgency}}</td><td>{{.Summary}}</td>{{else}}<td class="muted">untriaged</td><td></td>{{end}}</tr>{{end}}</table>
<button type="submit">Mark the ticked items seen</button></form>
{{else}}<p class="muted">Nothing new.</p>{{end}}
</section>
<nav>{{range .Links}}<a href="{{.Href}}"{{if .Current}} class="current"{{end}}>{{.Label}}</a>{{end}}</nav>
{{if .Hidden}}<p class="muted">{{.Hidden}} hidden repositories.</p>{{end}}
{{range .Repos}}<section>
<h2 class="repo">{{.Name}} <span class="muted">{{.Key}}</span>{{if .NeedsYou}} <span class="needs">needs you: {{.NeedsYou}}</span>{{end}} <a href="{{.HideLink}}">{{if .Hidden}}show{{else}}hide{{end}}</a></h2>
{{if .Cases}}<table>{{range .Cases}}<tr><td>{{.Session}}</td><td>{{.State}}</td><td class="muted">since {{when .Since}}</td><td>{{.Head}}</td></tr>{{end}}</table>{{end}}
{{if .Issues}}<table>{{range .Issues}}<tr><td>#{{.Number}}</td><td>{{.Title}}</td><td class="muted">updated {{when .Updated}}</td><td><a href="{{.HideLink}}">hide</a></td></tr>{{end}}</table>{{end}}
{{if .HiddenIssues}}<p class="muted">{{.HiddenIssues}} hidden issues. <a href="{{.ShowIssues}}">show them</a></p>{{end}}
</section>{{else}}<p>Nothing to show.</p>{{end}}
</body></html>
`))

// pageCmd serves the owner's page until interrupted.
func pageCmd(args []string) error {
	fs := flag.NewFlagSet("page", flag.ContinueOnError)
	shadow := fs.Bool("shadow", false, "show what the shadow run saw and would have done")
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	mode := ModeLive
	if *shadow {
		mode = ModeShadow
	}
	host, _ := os.Hostname()
	log, err := OpenLog(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	ps := newPageState(cfg, log, mode, host)
	if _, err := ps.load(); err != nil {
		return err
	}
	page, err := NewPage(cfg.Page, ExecWhois{Argv: cfg.Page.Whois, Timeout: cfg.Timeouts.Tmux.Duration}, ps.load, ps.markSeen, ps.answer, time.Now)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.Page.Address, Handler: page, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, MaxHeaderBytes: 1 << 16}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	fmt.Fprintf(os.Stderr, "agentd page: serving %s for %s\n", cfg.Page.Address, cfg.Page.Owner)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
