package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeWhois names the login behind each caller's IP address.
type fakeWhois struct {
	logins map[string]string
	calls  int
}

func (f *fakeWhois) Login(addr string) (string, error) {
	f.calls++
	ip := addr[:strings.LastIndexByte(addr, ':')]
	if l, ok := f.logins[ip]; ok {
		return l, nil
	}
	return "", errors.New("no such peer")
}

// The page listens on a documentation address; the owner calls from
// 192.0.2.20, a guest from 192.0.2.30.
const pageAddr = "192.0.2.10:8770"

var pageAt = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func testPageData() PageData {
	inv := &Inventory{AsOf: pageAt, Host: "host-a", byKey: map[string]*RepoRecord{}}
	add := func(r *RepoRecord) {
		inv.Repos = append(inv.Repos, r)
		inv.byKey[r.Key] = r
	}
	add(&RepoRecord{Key: "github/example-org/gadgets", Name: "gadgets", Platform: "github", Path: "example-org/gadgets", Active: true, Tracked: true,
		OpenIssues: []SnapIssue{{Number: 3, Title: "Gadget docs", State: "open", CreatedAt: pageAt.Add(-72 * time.Hour), UpdatedAt: pageAt.Add(-30 * time.Minute)}}})
	add(&RepoRecord{Key: "gitlab/example-org/quiet", Name: "quiet", Platform: "gitlab", Path: "example-org/quiet", Active: true, Tracked: true})
	add(&RepoRecord{Key: "gitlab/example-org/widgets", Name: "widgets", Platform: "gitlab", Path: "example-org/widgets", Active: true, Tracked: true,
		OpenIssues: []SnapIssue{
			{Number: 17, Title: "<img src=x onerror=alert(1)>", State: "open", CreatedAt: pageAt.Add(-48 * time.Hour), UpdatedAt: pageAt.Add(-30 * time.Hour)},
			{Number: 18, Title: "Paint widgets blue", State: "open", CreatedAt: pageAt.Add(-96 * time.Hour), UpdatedAt: pageAt.Add(-2 * time.Hour)},
		}})
	report := Report{Schema: ReportSchema, Digest: "2 sessions", Repositories: []ReportRepo{{
		Repository: "gitlab/example-org/widgets", Name: "widgets",
		Cases: []ReportCase{
			{Session: "<script>alert(1)</script>", State: string(BlockedOnOwner), Since: pageAt.Add(-time.Hour), Head: `"><b>bold</b>`},
			{Session: "w17", State: string(Working), Since: pageAt.Add(-3 * time.Hour)},
		},
		Questions: []ReportQuestion{{Session: "<script>alert(1)</script>", Since: pageAt.Add(-time.Hour), Text: "Blue or </pre><script>x()</script>?", OwnerOnly: true}},
	}}}
	return PageData{Inventory: inv, Report: report}
}

func testPage(t *testing.T) (*Page, *fakeWhois) {
	t.Helper()
	whois := &fakeWhois{logins: map[string]string{"192.0.2.20": "owner-a", "192.0.2.30": "guest-b", "192.0.2.10": "owner-a"}}
	p, err := NewPage(PageConfig{Address: pageAddr, Whois: []string{"whois", "{addr}"}, Owner: "owner-a"}, whois,
		func() (PageData, error) { return testPageData(), nil }, func() time.Time { return pageAt })
	if err != nil {
		t.Fatal(err)
	}
	return p, whois
}

// get asks the page for target from remote, with Host host.
func get(p *Page, method, target, host, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	req.RemoteAddr = remote
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	return w
}

// The owner is served; anyone else whois names, or that whois cannot
// name, is refused with 403.
func TestPageServesOnlyTheOwner(t *testing.T) {
	p, _ := testPage(t)
	if w := get(p, "GET", "/", pageAddr, "192.0.2.20:51000"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "widgets") {
		t.Fatalf("owner: %d\n%s", w.Code, w.Body.String())
	}
	for _, remote := range []string{"192.0.2.30:51000", "192.0.2.99:51000"} {
		if w := get(p, "GET", "/", pageAddr, remote); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "widgets") {
			t.Errorf("%s: %d\n%s", remote, w.Code, w.Body.String())
		}
	}
}

// A request with another Host, or from the page's own address or this
// machine's loopback, is refused before whois is asked.
func TestPageRefusesOtherHostsAndItself(t *testing.T) {
	p, whois := testPage(t)
	for _, c := range []struct{ host, remote string }{
		{"rebound.example:8770", "192.0.2.20:51000"},
		{"", "192.0.2.20:51000"},
		{pageAddr, "192.0.2.10:40000"},
		{pageAddr, "127.0.0.1:40000"},
		{pageAddr, "[::1]:40000"},
	} {
		if w := get(p, "GET", "/", c.host, c.remote); w.Code != http.StatusForbidden {
			t.Errorf("host %q from %s: %d", c.host, c.remote, w.Code)
		}
	}
	if whois.calls != 0 {
		t.Fatalf("whois was asked %d times", whois.calls)
	}
}

// The page is read-only, and serves only its one view.
func TestPageIsReadOnly(t *testing.T) {
	p, _ := testPage(t)
	if w := get(p, "POST", "/", pageAddr, "192.0.2.20:51000"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", w.Code)
	}
	if w := get(p, "GET", "/other", pageAddr, "192.0.2.20:51000"); w.Code != http.StatusNotFound {
		t.Fatalf("other path: %d", w.Code)
	}
	w := get(p, "GET", "/", pageAddr, "192.0.2.20:51000")
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("CSP = %q", csp)
	}
}

// A hostile session name, issue title, first line or question is shown as
// text, never as markup.
func TestPageEscapesEverything(t *testing.T) {
	p, _ := testPage(t)
	body := get(p, "GET", "/?show=all", pageAddr, "192.0.2.20:51000").Body.String()
	for _, bad := range []string{"<script>", "<img", "<b>bold", "</pre><script>"} {
		if strings.Contains(body, bad) {
			t.Errorf("page holds %q:\n%s", bad, body)
		}
	}
	for _, want := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "&lt;img src=x onerror=alert(1)&gt;", "&lt;b&gt;bold&lt;/b&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
}

// order is the repository names as the page lists them.
func order(body string) []string {
	var names []string
	for _, m := range regexp.MustCompile(`<h2>([^ <]+) `).FindAllStringSubmatch(body, -1) {
		names = append(names, m[1])
	}
	return names
}

// Repositories sort by needs-you first by default, or by name, activity or
// open work; show open work by default, all, or hidden only. A hidden
// repository that needs the owner is shown anyway, and an issue can be
// hidden. Every control is a link carrying the whole view.
func TestPageSortsAndHides(t *testing.T) {
	p, _ := testPage(t)
	view := func(q string) string {
		return strings.Join(order(get(p, "GET", "/"+q, pageAddr, "192.0.2.20:51000").Body.String()), ",")
	}
	for q, want := range map[string]string{
		"":                                 "widgets,gadgets",
		"?sort=name":                       "gadgets,widgets",
		"?sort=recent":                     "gadgets,widgets",
		"?sort=work&show=all":              "widgets,gadgets,quiet",
		"?show=all&sort=name":              "gadgets,quiet,widgets",
		"?hide=github/example-org/gadgets": "widgets",
		"?hide=gitlab/example-org/widgets": "widgets,gadgets",
		"?show=hidden&hide=gitlab/example-org/quiet": "quiet",
		"?sort=bogus": "widgets,gadgets",
	} {
		if got := view(q); got != want {
			t.Errorf("%q: %s, want %s", q, got, want)
		}
	}
	body := get(p, "GET", "/?hidei=gitlab/example-org/widgets%2318&isort=oldest", pageAddr, "192.0.2.20:51000").Body.String()
	if strings.Contains(body, "Paint widgets blue") || !strings.Contains(body, "1 hidden issue") {
		t.Fatalf("hidden issue shown:\n%s", body)
	}
	body = get(p, "GET", "/?isort=oldest", pageAddr, "192.0.2.20:51000").Body.String()
	if strings.Index(body, "#18") > strings.Index(body, "#17") {
		t.Fatalf("oldest first puts #18 after #17")
	}
	body = get(p, "GET", "/?isort=number", pageAddr, "192.0.2.20:51000").Body.String()
	if strings.Index(body, "#17") > strings.Index(body, "#18") {
		t.Fatalf("by number puts #17 after #18")
	}
	if !strings.Contains(body, `href="?hide=github%2Fexample-org%2Fgadgets&amp;isort=number&amp;show=open&amp;sort=needs"`) {
		t.Fatalf("hide link does not carry the view:\n%s", body)
	}
}

// whois output is a login on its first line, or the JSON a tailnet's whois
// prints; anything else names no one.
func TestParseWhois(t *testing.T) {
	for out, want := range map[string]string{
		"owner-a\n":       "owner-a",
		"owner-a\nmore\n": "owner-a",
		`{"Node": {"Name": "host-b"}, "UserProfile": {"LoginName": "owner-a"}}`: "owner-a",
	} {
		if got, err := parseWhois([]byte(out)); err != nil || got != want {
			t.Errorf("%q: %q, %v", out, got, err)
		}
	}
	for _, out := range []string{"", "  \n", `{"UserProfile": {}}`, `{"UserProfile": `} {
		if got, err := parseWhois([]byte(out)); err == nil {
			t.Errorf("%q named %q", out, got)
		}
	}
	login, err := ExecWhois{Argv: []string{"sh", "-c", `echo "login-for-$1"`, "sh", "{addr}"}, Timeout: 5 * time.Second}.Login("192.0.2.20:51000")
	if err != nil || login != "login-for-192.0.2.20:51000" {
		t.Fatalf("exec whois: %q, %v", login, err)
	}
}

// The page refuses to start without one IP address to listen on, an owner
// and a whois command.
func TestPageConfigIsChecked(t *testing.T) {
	for _, cfg := range []PageConfig{
		{Address: "0.0.0.0:8770", Whois: []string{"w"}, Owner: "o"},
		{Address: "page.example:8770", Whois: []string{"w"}, Owner: "o"},
		{Address: pageAddr, Owner: "o"},
		{Address: pageAddr, Whois: []string{"w"}},
	} {
		if _, err := NewPage(cfg, &fakeWhois{}, nil, time.Now); err == nil {
			t.Errorf("%+v accepted", cfg)
		}
	}
}
