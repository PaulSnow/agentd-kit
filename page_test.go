package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fakeWhois names the login behind each caller's IP address, or fails
// with fail.
type fakeWhois struct {
	logins map[string]string
	fail   error
	calls  int
}

func (f *fakeWhois) Login(addr string) (string, error) {
	f.calls++
	if f.fail != nil {
		return "", f.fail
	}
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
	needs := []PageNeed{{Kind: "questions", Session: "<script>alert(1)</script>", Repository: "widgets", Since: pageAt.Add(-time.Hour), Text: "Blue or </pre><script>x()</script>?"}}
	items := []*Item{{ItemPayload: ItemPayload{ID: "gitlab/example-org/widgets#19", Kind: ItemIssue, Repository: "gitlab/example-org/widgets",
		Title: "<iframe src=x>", Link: "https://gitlab.com/example-org/widgets/-/issues/19", SourceTime: pageAt.Add(-10 * time.Minute)}}}
	return PageData{Inventory: inv, Report: report, Needs: needs, New: items}
}

// fakeMarks records what the page marked seen, and from where.
type fakeMarks struct {
	ids  []string
	from string
}

func testPage(t *testing.T) (*Page, *fakeWhois) {
	p, whois, _ := testPageMarks(t, PageConfig{})
	return p, whois
}

// testPageMarks is testPage with cfg's Hosts, and what it marks seen.
func testPageMarks(t *testing.T, cfg PageConfig) (*Page, *fakeWhois, *fakeMarks) {
	t.Helper()
	whois := &fakeWhois{logins: map[string]string{"192.0.2.20": "owner-a", "192.0.2.30": "guest-b", "192.0.2.10": "owner-a", "127.0.0.1": "owner-a"}}
	marks := &fakeMarks{}
	mark := func(ids []string, from string) error {
		marks.ids, marks.from = append(marks.ids, ids...), from
		return nil
	}
	cfg.Address, cfg.Whois, cfg.Owner = pageAddr, []string{"whois", "{addr}"}, "owner-a"
	p, err := NewPage(cfg, whois, func() (PageData, error) { return testPageData(), nil }, mark, func() time.Time { return pageAt })
	if err != nil {
		t.Fatal(err)
	}
	return p, whois, marks
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
		{pageAddr, "[::ffff:127.0.0.1]:40000"},
		{pageAddr, "[::ffff:192.0.2.10]:40000"},
		{pageAddr, ""},
		{pageAddr, "192.0.2.20"},
	} {
		if w := get(p, "GET", "/", c.host, c.remote); w.Code != http.StatusForbidden {
			t.Errorf("host %q from %s: %d", c.host, c.remote, w.Code)
		}
	}
	if whois.calls != 0 {
		t.Fatalf("whois was asked %d times", whois.calls)
	}
}

// The Host check ignores case; whois failing for the owner's address is a
// refusal; and one whois answer serves an address for whoisTTL.
func TestPageHostCaseAndWhois(t *testing.T) {
	p, whois, _ := testPageMarks(t, PageConfig{Hosts: []string{"Page.Example:8770"}})
	if w := get(p, "GET", "/", "page.example:8770", "192.0.2.20:51000"); w.Code != http.StatusOK {
		t.Fatalf("lower-case host: %d", w.Code)
	}
	get(p, "GET", "/", "page.example:8770", "192.0.2.20:51001")
	if whois.calls != 1 {
		t.Fatalf("whois asked %d times within its TTL", whois.calls)
	}
	now := pageAt
	p.now = func() time.Time { return now }
	now = now.Add(whoisTTL + time.Second)
	whois.fail = errors.New("whois: no answer")
	if w := get(p, "GET", "/", "page.example:8770", "192.0.2.20:51000"); w.Code != http.StatusForbidden || whois.calls != 2 {
		t.Fatalf("whois error for the owner: %d after %d calls", w.Code, whois.calls)
	}
}

// Only GET and HEAD of the page and POST of /seen are served.
func TestPageMethods(t *testing.T) {
	p, _ := testPage(t)
	if w := get(p, "POST", "/", pageAddr, "192.0.2.20:51000"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", w.Code)
	}
	if w := get(p, "GET", "/other", pageAddr, "192.0.2.20:51000"); w.Code != http.StatusNotFound {
		t.Fatalf("other path: %d", w.Code)
	}
	w := get(p, "GET", "/", pageAddr, "192.0.2.20:51000")
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "base-uri 'none'") {
		t.Fatalf("CSP = %q", csp)
	}
}

// A hostile session name, issue title, first line or question is shown as
// text, never as markup.
func TestPageEscapesEverything(t *testing.T) {
	p, _ := testPage(t)
	body := get(p, "GET", "/?show=all", pageAddr, "192.0.2.20:51000").Body.String()
	for _, bad := range []string{"<script>", "<img", "<b>bold", "</pre><script>", "<iframe"} {
		if strings.Contains(body, bad) {
			t.Errorf("page holds %q:\n%s", bad, body)
		}
	}
	for _, want := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "&lt;img src=x onerror=alert(1)&gt;", "&lt;b&gt;bold&lt;/b&gt;", "&lt;iframe src=x&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
}

// order is the repository names as the page lists them.
func order(body string) []string {
	var names []string
	for _, m := range regexp.MustCompile(`<h2 class="repo">([^ <]+) `).FindAllStringSubmatch(body, -1) {
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
	// A hidden issue can be shown again.
	if !strings.Contains(body, `<a href="?isort=oldest&amp;show=open&amp;sort=needs">show them</a>`) {
		t.Fatalf("no link shows the hidden issue:\n%s", body)
	}
	// A hidden repository the filter would not show anyway is not counted.
	if body := get(p, "GET", "/?hide=gitlab/example-org/quiet", pageAddr, "192.0.2.20:51000").Body.String(); strings.Contains(body, "hidden repositories") {
		t.Fatalf("an inactive hidden repository counted:\n%s", body)
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
		if _, err := NewPage(cfg, &fakeWhois{}, nil, nil, time.Now); err == nil {
			t.Errorf("%+v accepted", cfg)
		}
	}
}

// Sorting by name sorts by the name shown, not the key.
func TestPageSortsByName(t *testing.T) {
	inv := &Inventory{byKey: map[string]*RepoRecord{}}
	for _, r := range []*RepoRecord{
		{Key: "github/example-org/zz", Name: "alpha", OpenIssues: []SnapIssue{{Number: 1}}},
		{Key: "gitlab/example-org/aa", Name: "zeta", OpenIssues: []SnapIssue{{Number: 1}}},
	} {
		inv.Repos = append(inv.Repos, r)
		inv.byKey[r.Key] = r
	}
	v := buildView(PageData{Inventory: inv}, parseViewQuery(url.Values{"sort": {"name"}}), pageAt)
	if len(v.Repos) != 2 || v.Repos[0].Name != "alpha" || v.Repos[1].Name != "zeta" {
		t.Fatalf("by name: %+v", v.Repos)
	}
}

// The page lists what needs the owner first, then what is new, then the
// repositories.
func TestPageSectionsInOrder(t *testing.T) {
	p, _ := testPage(t)
	body := get(p, "GET", "/", pageAddr, "192.0.2.20:51000").Body.String()
	needs, fresh, repos := strings.Index(body, "Needs you"), strings.Index(body, "New since last visit"), strings.Index(body, "<nav>")
	if needs < 0 || fresh < needs || repos < fresh {
		t.Fatalf("sections out of order (%d, %d, %d):\n%s", needs, fresh, repos, body)
	}
	if !strings.Contains(body, "Blue or &lt;/pre&gt;") || !strings.Contains(body, `value="gitlab/example-org/widgets#19"`) {
		t.Fatalf("the question or the new item is missing:\n%s", body)
	}
}

// post sends the mark-seen form from remote, with origin and token.
func post(p *Page, origin, token string, form url.Values, remote string) *httptest.ResponseRecorder {
	form.Set("token", token)
	req := httptest.NewRequest("POST", "/seen", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.Host = pageAddr
	req.RemoteAddr = remote
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	return w
}

// Marking items seen needs the owner, the page's own Origin and the token
// the page put in its form; it goes back to the same view, rebuilt.
func TestPageMarksSeen(t *testing.T) {
	p, _, marks := testPageMarks(t, PageConfig{})
	body := get(p, "GET", "/", pageAddr, "192.0.2.20:51000").Body.String()
	m := regexp.MustCompile(`name="token" value="([0-9a-f]{64})"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no token in the form:\n%s", body)
	}
	token, origin := m[1], "http://"+pageAddr
	ids := url.Values{"id": {"gitlab/example-org/widgets#19"}}
	for _, c := range []struct {
		name, origin, token, remote string
	}{
		{"no origin", "", token, "192.0.2.20:51000"},
		{"another origin", "http://evil.example", token, "192.0.2.20:51000"},
		{"no token", origin, "", "192.0.2.20:51000"},
		{"another token", origin, strings.Repeat("0", 64), "192.0.2.20:51000"},
		{"a guest", origin, token, "192.0.2.30:51000"},
		{"this machine", origin, token, "127.0.0.1:51000"},
	} {
		if w := post(p, c.origin, c.token, url.Values{"id": ids["id"]}, c.remote); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", c.name, w.Code)
		}
	}
	if len(marks.ids) != 0 {
		t.Fatalf("a refused POST marked %v", marks.ids)
	}
	if w := post(p, origin, token, url.Values{}, "192.0.2.20:51000"); w.Code != http.StatusBadRequest {
		t.Fatalf("no ids: %d", w.Code)
	}
	w := post(p, origin, token, url.Values{"id": ids["id"], "view": {"sort=name&hide=x&next=//evil.example"}}, "192.0.2.20:51000")
	if w.Code != http.StatusSeeOther || marks.from != "192.0.2.20:51000" || len(marks.ids) != 1 {
		t.Fatalf("owner's POST: %d, marked %v from %q", w.Code, marks.ids, marks.from)
	}
	if loc := w.Header().Get("Location"); loc != "/?hide=x&isort=needs&show=open&sort=name" {
		t.Fatalf("back to %q", loc)
	}
}

// The page's state is one projection: each load applies only the lines
// appended since the last, and a mark from the page is applied too.
func TestPageStateFollowsTheLog(t *testing.T) {
	cfg := DefaultConfig()
	var buf bytes.Buffer
	log := NewLogWriter(&buf)
	at := pageAt
	item := func(id string) Event {
		return Event{V: EventVersion, Time: at, Kind: KindItem, Mode: ModeShared, Item: &ItemPayload{ID: id, Kind: ItemIssue, SourceTime: at}}
	}
	if err := log.Append(item("gitlab/example-org/widgets#1")); err != nil {
		t.Fatal(err)
	}
	ps := newPageState(&cfg, log, ModeLive, "host-a")
	data, err := ps.load()
	if err != nil || len(data.New) != 1 {
		t.Fatalf("first load: %v, %v", data.New, err)
	}
	log.Append(item("gitlab/example-org/widgets#2"))
	if data, _ = ps.load(); len(data.New) != 2 || ps.st.Events != 2 {
		t.Fatalf("second load: %d new, %d events applied", len(data.New), ps.st.Events)
	}
	if err := ps.markSeen([]string{"gitlab/example-org/widgets#9"}, "192.0.2.20:1"); err == nil {
		t.Fatal("marked an unknown item")
	}
	if err := ps.markSeen([]string{"gitlab/example-org/widgets#1"}, "192.0.2.20:1"); err != nil {
		t.Fatal(err)
	}
	if data, _ = ps.load(); len(data.New) != 1 || ps.st.Events != 3 {
		t.Fatalf("after the mark: %d new, %d events applied", len(data.New), ps.st.Events)
	}
	if !strings.Contains(buf.String(), `"from":"192.0.2.20:1"`) {
		t.Fatalf("the mark does not say where it came from:\n%s", buf.String())
	}
}

// Needs you lists open Questions, parked sessions and the day's other
// escalations, of the report's sessions only, screened again.
func TestNeedsFrom(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ConfidentialPatterns = []string{"(?i)embargoed"}
	st := NewLogState(&cfg, ModeLive)
	w1 := st.track("w1")
	w1.State, w1.Since, w1.Reason = CrashLooping, pageAt.Add(-time.Hour), "parked after 3 crashes"
	w1.Escalated = []EscalationRecord{
		{Key: "tool-wait:w1:1", Message: "w1 waits on a tool", At: pageAt.Add(-2 * time.Hour)},
		{Key: "tool-wait:w1:0", Message: "old", At: pageAt.Add(-48 * time.Hour)},
		{Key: "questions:w1", Message: "w1 is blocked on you", At: pageAt.Add(-time.Hour)},
		{Key: "deliver:x", Message: "the embargoed thing", At: pageAt.Add(-time.Hour)},
	}
	hidden := st.track("secret-session")
	hidden.State, hidden.Reason = CrashLooping, "parked"
	rep := Report{Repositories: []ReportRepo{{Name: "widgets", Cases: []ReportCase{{Session: "w1"}},
		Questions: []ReportQuestion{{Session: "w1", Text: "Blue?", Since: pageAt.Add(-30 * time.Minute)}}}}}
	// An escalation posted is kept on its Track, the latest few only.
	for i := 0; i < keptEscalations+2; i++ {
		st.apply(Event{V: EventVersion, Time: pageAt, Kind: KindAction, Mode: ModeLive, Session: "w2",
			Act: &ActPayload{Action: ActEscalate, Key: fmt.Sprintf("stall:w2:%d", i), Message: "w2 stalled"}})
	}
	if e := st.Tracks["w2"].Escalated; len(e) != keptEscalations || e[len(e)-1].Key != fmt.Sprintf("stall:w2:%d", keptEscalations+1) || e[0].Message != "w2 stalled" {
		t.Fatalf("escalations kept: %+v", e)
	}
	needs := NeedsFrom(&cfg, st, rep, pageAt)
	var got []string
	for _, n := range needs {
		got = append(got, n.Kind+":"+n.Session+":"+n.Text)
	}
	want := "questions:w1:Blue?,parked:w1:parked after 3 crashes,escalation:w1:w1 waits on a tool"
	if strings.Join(got, ",") != want {
		t.Fatalf("needs = %s\nwant    %s", strings.Join(got, ","), want)
	}
}
