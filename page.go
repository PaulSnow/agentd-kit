package main

import (
	"context"
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
// the tracking files are configured, and the report folded from the log.
type PageData struct {
	Inventory *Inventory
	Report    Report
}

// Page serves the owner's read-only view. Every request is checked before
// anything is read: its Host must be one the page answers to, it must not
// come from the page's own address (a process on this machine would
// otherwise borrow the machine's identity), and whois must name the owner.
type Page struct {
	cfg   PageConfig
	whois Whois
	load  func() (PageData, error)
	self  net.IP // the address the page listens on
	now   func() time.Time
}

// NewPage builds the page's handler.
func NewPage(cfg PageConfig, whois Whois, load func() (PageData, error), now func() time.Time) (*Page, error) {
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
	return &Page{cfg: cfg, whois: whois, load: load, self: ip, now: now}, nil
}

func (p *Page) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	if !p.hostAllowed(r.Host) {
		http.Error(w, "unknown host", http.StatusForbidden)
		return
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(remote); err != nil || ip == nil || ip.Equal(p.self) || ip.IsLoopback() {
		http.Error(w, "refused: the page does not answer its own machine", http.StatusForbidden)
		return
	}
	login, err := p.whois.Login(r.RemoteAddr)
	if err != nil || login != p.cfg.Owner {
		http.Error(w, "refused: only the owner may use this page", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "this page is read-only", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/" {
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
	h.Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, view); err != nil {
		fmt.Fprintf(os.Stderr, "agentd page: %v\n", err)
	}
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

// encode is the query for v, with change applied to a copy of it.
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
	Now      time.Time
	AsOf     time.Time
	Digest   string
	Query    viewQuery
	Repos    []pageRepo
	Hidden   int
	Links    []pageLink // the sort and show controls
	Untraced bool       // no tracking files: repositories come from the log only
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
	Questions []ReportQuestion
	Issues    []pageIssue
	// HiddenIssues counts the issues hidden in this repository.
	HiddenIssues int
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
	v := pageView{Now: now, Digest: data.Report.Digest, Query: q, Untraced: data.Inventory == nil}
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
		r.Cases, r.Questions = rr.Cases, rr.Questions
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
			needs[qu.Session] = true
			if qu.Since.After(r.Activity) {
				r.Activity = qu.Since
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
		for _, is := range r.Issues {
			ik := fmt.Sprintf("%s#%d", key, is.Number)
			if q.HideIssues[ik] {
				r.HiddenIssues++
				continue
			}
			is.HideLink = q.link(func(c *viewQuery) { c.HideIssues[ik] = true })
			issues = append(issues, is)
		}
		r.Issues = issues
		sortIssues(r.Issues, q.IssueSort)
		active := r.OpenWork > 0 || len(r.Cases) > 0 || r.NeedsYou > 0
		switch {
		case q.Show == "hidden" && !r.Hidden:
		case q.Show != "hidden" && r.Hidden && r.NeedsYou == 0:
			v.Hidden++
		case q.Show == "open" && !active:
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

// sortRepos orders repositories by key, each order falling back to name.
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
<nav>{{range .Links}}<a href="{{.Href}}"{{if .Current}} class="current"{{end}}>{{.Label}}</a>{{end}}</nav>
{{if .Hidden}}<p class="muted">{{.Hidden}} hidden repositories.</p>{{end}}
{{range .Repos}}<section>
<h2>{{.Name}} <span class="muted">{{.Key}}</span>{{if .NeedsYou}} <span class="needs">needs you: {{.NeedsYou}}</span>{{end}} <a href="{{.HideLink}}">{{if .Hidden}}show{{else}}hide{{end}}</a></h2>
{{range .Questions}}<div class="needs">{{.Session}} asks, since {{when .Since}}:</div><pre>{{.Text}}</pre>{{end}}
{{if .Cases}}<table>{{range .Cases}}<tr><td>{{.Session}}</td><td>{{.State}}</td><td class="muted">since {{when .Since}}</td><td>{{.Head}}</td></tr>{{end}}</table>{{end}}
{{if .Issues}}<table>{{range .Issues}}<tr><td>#{{.Number}}</td><td>{{.Title}}</td><td class="muted">updated {{when .Updated}}</td><td><a href="{{.HideLink}}">hide</a></td></tr>{{end}}</table>{{end}}
{{if .HiddenIssues}}<p class="muted">{{.HiddenIssues}} hidden issues.</p>{{end}}
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
	load := func() (PageData, error) {
		var data PageData
		if cfg.Tracking.Manifest != "" || cfg.Tracking.Snapshot != "" {
			inv, err := LoadInventory(cfg.Tracking)
			if err != nil {
				return data, err
			}
			data.Inventory = inv
		}
		events, _, err := ReadLog(cfg.EventLog)
		if err != nil {
			return data, err
		}
		data.Report = BuildReport(cfg, events, mode, host, time.Now())
		return data, nil
	}
	page, err := NewPage(cfg.Page, ExecWhois{Argv: cfg.Page.Whois, Timeout: cfg.Timeouts.Tmux.Duration}, load, time.Now)
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
