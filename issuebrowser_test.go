package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func closeIssue(t *testing.T, f *levelsFixture, ref, title string, labels []string, closed time.Time) {
	t.Helper()
	i := ListedIssue{IndexPayload: IndexPayload{Repository: "widgets", Ref: ref, Title: title, State: "closed", Labels: labels, Author: "bob",
		Opened: closed.Add(-48 * time.Hour), Changed: closed}, ClosedAt: closed}
	f.src.mu.Lock()
	defer f.src.mu.Unlock()
	if err := f.src.recordIssues([]issueRecord{{At: pageAt, Issue: &i}}); err != nil {
		t.Fatal(err)
	}
}

// Closed issues are a list of their own: the open page holds none and
// counts them, the closed page holds every one and no open one, with its
// own sorts agreeing between Go and the page, and a closed issue has a page.
func TestClosedList(t *testing.T) {
	f := newLevelsFixture(t)
	closeIssue(t, f, "#101", "Closed long ago", []string{"zeta"}, pageAt.Add(-90*24*time.Hour))
	closeIssue(t, f, "#102", "Closed lately", []string{"alpha"}, pageAt.Add(-24*time.Hour))
	u, _ := testUI(t, UIConfig{}, f.src)
	open := owner(t, u, "/repo/widgets")
	if strings.Index(open, `id="issues"`) > strings.Index(open, `id="requests"`) || strings.Index(open, `id="issues"`) > strings.Index(open, `id="needs"`) {
		t.Errorf("the issue list is not the first thing after the sentence")
	}
	if strings.Contains(open, `data-ref="#101"`) || !strings.Contains(open, `data-ref="#4"`) || !strings.Contains(open, `/r/widgets/closed">Closed <span class="muted">2</span>`) {
		t.Errorf("open page: a closed row, no open row, or no closed count")
	}
	closed := owner(t, u, "/r/widgets/closed")
	if strings.Contains(closed, `data-ref="#4"`) || !strings.Contains(closed, `data-ref="#101"`) || !strings.Contains(closed, "data-static") {
		t.Errorf("closed page: an open row, no closed row, or it refreshes")
	}
	if strings.Index(closed, `data-ref="#102"`) > strings.Index(closed, `data-ref="#101"`) {
		t.Errorf("the latest closed does not come first")
	}
	if !strings.Contains(closed, "Still reading them from the forge") {
		t.Errorf("a list not yet read whole does not say so")
	}
	for _, s := range ClosedSorts {
		if !strings.Contains(closed, `<option value="`+s+`">`) || sortLabels[s] == "" {
			t.Errorf("sort %s missing or unlabelled", s)
		}
	}
	for _, s := range []string{"needs-me", "working", "has-mr"} {
		if strings.Contains(closed, `<option value="`+s+`">`) {
			t.Errorf("sort %s means nothing on closed issues", s)
		}
	}
	if body := owner(t, u, "/repo/widgets/issue/101"); !strings.Contains(body, "Closed long ago") || !strings.Contains(body, "closed ") {
		t.Errorf("no page for a closed issue")
	}
	if w := get(u, "GET", "/r/nobody/closed", pageAddr, "192.0.2.20:51000"); w.Code != http.StatusNotFound {
		t.Errorf("unknown repository's closed list: %d", w.Code)
	}
	orders := scriptOrder(t, boxRows(t, closed, "closed"), ClosedSorts)
	v := closedRows(f.cfg, f.src.st, "widgets", pageAt)
	for _, by := range ClosedSorts {
		is := append([]LevelIssue{}, v...)
		sortIssues(is, by)
		for k, i := range is {
			if orders[by][k] != i.Ref {
				t.Errorf("%s: the page has %v, Go has %s at %d", by, orders[by], i.Ref, k)
			}
		}
	}
}

// An issue the open list holds is not also listed as closed.
func TestReopenedIssueIsOpenOnly(t *testing.T) {
	f := newLevelsFixture(t)
	closeIssue(t, f, "#4", "Was closed", nil, pageAt.Add(-24*time.Hour))
	u, _ := testUI(t, UIConfig{}, f.src)
	if body := owner(t, u, "/r/widgets/closed"); strings.Contains(body, `data-ref="#4"`) {
		t.Fatal("an open issue is listed as closed")
	}
}

// The owner's priority is kept, sorts first, can be cleared, and works on
// a closed issue; a level or an issue agentd does not know is refused.
func TestIssuePriority(t *testing.T) {
	f := newLevelsFixture(t)
	closeIssue(t, f, "#101", "Closed", nil, pageAt.Add(-24*time.Hour))
	u, _ := testUI(t, UIConfig{}, f.src)
	set := func(ref, level string) int {
		return ownerPost(u, "/priority", url.Values{"src": {f.src.Name}, "repository": {"widgets"}, "ref": {ref}, "level": {level}}).Code
	}
	for ref, level := range map[string]string{"#6": "high", "#4": "low", "#101": "medium"} {
		if c := set(ref, level); c != http.StatusNoContent {
			t.Fatalf("%s: %d", ref, c)
		}
	}
	if c := set("#6", "urgent"); c != http.StatusBadRequest {
		t.Errorf("unknown level: %d", c)
	}
	if c := set("#9999", "high"); c != http.StatusBadRequest {
		t.Errorf("unknown issue: %d", c)
	}
	if c := set("!9", "high"); c != http.StatusBadRequest {
		t.Errorf("a request: %d", c)
	}
	body := owner(t, u, "/repo/widgets")
	rows := map[string]map[string]string{}
	for _, r := range append(boxRows(t, body, "issues"), boxRows(t, owner(t, u, "/r/widgets/closed"), "closed")...) {
		rows[r["ref"]] = r
	}
	if rows["#6"]["priority"] != "0" || rows["#4"]["priority"] != "2" || rows["#101"]["priority"] != "1" || rows["#5"]["priority"] != "3" {
		t.Fatalf("ranks: #6 %s #4 %s #101 %s #5 %s", rows["#6"]["priority"], rows["#4"]["priority"], rows["#101"]["priority"], rows["#5"]["priority"])
	}
	if !strings.Contains(body, `<option value="high" selected>`) {
		t.Error("the set level is not selected")
	}
	if c := set("#6", ""); c != http.StatusNoContent {
		t.Fatalf("clear: %d", c)
	}
	for _, r := range boxRows(t, owner(t, u, "/repo/widgets"), "issues") {
		if r["ref"] == "#6" && r["priority"] != "3" {
			t.Fatalf("not cleared: %s", r["priority"])
		}
	}
	if body := owner(t, u, "/repo/widgets/issue/4"); !strings.Contains(body, `<option value="low" selected>`) {
		t.Error("the issue page does not show its priority")
	}
}

// Priorities and listed issues live through a restart, a torn last line
// costs only itself, and nothing of them reaches the event log, whose
// other readers would refuse an entry they do not know.
func TestIssueFile(t *testing.T) {
	f := newLevelsFixture(t)
	path := filepath.Join(t.TempDir(), "log.jsonl.issues.jsonl")
	if err := f.src.loadIssues(path); err != nil {
		t.Fatal(err)
	}
	before := f.buf.Len()
	closeIssue(t, f, "#101", "Closed", nil, pageAt.Add(-24*time.Hour))
	if err := f.src.setPriority("widgets", "#101", "high"); err != nil {
		t.Fatal(err)
	}
	if err := f.src.setPriority("widgets", "#4", "low"); err != nil {
		t.Fatal(err)
	}
	if err := f.src.setPriority("widgets", "#4", ""); err != nil {
		t.Fatal(err)
	}
	if f.buf.Len() != before {
		t.Error("the browser wrote to the event log")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString(`{"at":"2026-10-0`)
	file.Close()
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	again := newUISource("again", f.cfg, f.log, ModeLive, "host-a")
	if err := again.loadIssues(path); err != nil {
		t.Fatal(err)
	}
	if again.st.ListedIssues["widgets"]["#101"] == nil || again.st.Priorities["widgets"]["#101"] != "high" {
		t.Fatalf("lost: %+v %+v", again.st.ListedIssues, again.st.Priorities)
	}
	if _, set := again.st.Priorities["widgets"]["#4"]; set {
		t.Error("a cleared priority came back")
	}
}

func TestIsUmbrella(t *testing.T) {
	cases := []struct {
		forge, title string
		labels       []string
		parts        int
		want         bool
	}{
		{"gitlab", "Umbrella: mainnet", nil, 0, true},
		{"gitlab", "AIP-53 — umbrella (issues)", nil, 0, true},
		{"gitlab", "Epic: mining", nil, 0, true},
		{"gitlab", "Plain", []string{"epic"}, 0, true},
		{"gitlab", "Plain", []string{"type::Epic"}, 0, true},
		{"github", "Plain", []string{"tracking"}, 0, true},
		{"github", "Plain", nil, 3, true},
		{"gitlab", "Plain", nil, 3, false}, // only a checklist
		{"github", "Plain", nil, 0, false},
		{"gitlab", "Epicycles in the sim", nil, 0, false},
		{"gitlab", "The umbrellas page", nil, 0, false},
		{"gitlab", "Plain", []string{"epicenter"}, 0, false},
	}
	for _, c := range cases {
		if got := isUmbrella(c.forge, c.title, c.labels, c.parts); got != c.want {
			t.Errorf("%+v: %v", c, got)
		}
	}
}

// Rows mark umbrellas, with their progress where the listing gives one.
func TestUmbrellaRow(t *testing.T) {
	f := newLevelsFixture(t)
	p := IndexPayload{Repository: "widgets", Ref: "#70", Title: "Umbrella: everything", State: "open", Author: "al", Opened: pageAt, Changed: pageAt}
	if err := f.log.Append(Event{V: EventVersion, Time: pageAt.Add(-time.Hour), Kind: KindIndex, Mode: ModeLive, Index: &p}); err != nil {
		t.Fatal(err)
	}
	f.src.mu.Lock()
	err := f.src.recordIssues([]issueRecord{{At: pageAt, Issue: &ListedIssue{IndexPayload: p, Parts: 21, PartsDone: 3}}})
	f.src.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := testUI(t, UIConfig{}, f.src)
	body := owner(t, u, "/repo/widgets")
	if !strings.Contains(body, "umbrella 3/21") {
		t.Error("no umbrella badge with progress")
	}
	for _, r := range boxRows(t, body, "issues") {
		if want := r["ref"] == "#70"; (r["umbrella"] == "1") != want {
			t.Errorf("%s umbrella=%q", r["ref"], r["umbrella"])
		}
	}
}

// fakeListAPI answers either forge with page.
type fakeListAPI struct {
	page func(string) (APIResponse, error)
}

func (f fakeListAPI) GitHub(q string, _ map[string]string) (APIResponse, error) { return f.page(q) }
func (f fakeListAPI) GitLab(q string) (APIResponse, error)                      { return f.page(q) }
func (f fakeListAPI) At(string) ForgeAPI                                        { return f }

// A listing of either forge reads every issue's state and parts, leaves out
// requests, and asks only for what changed since.
func TestListIssues(t *testing.T) {
	for _, forge := range []string{"gitlab", "github"} {
		t.Run(forge, func(t *testing.T) {
			var asked []string
			page := func(q string) (APIResponse, error) {
				asked = append(asked, q)
				var b strings.Builder
				b.WriteString("[")
				n := indexPerPage
				if strings.Contains(q, "page=2") {
					n = 2
				}
				for k := 0; k < n; k++ {
					if k > 0 {
						b.WriteString(",")
					}
					id := k + 1
					if strings.Contains(q, "page=2") {
						id += indexPerPage
					}
					at := "2026-10-01T00:00:00Z"
					if forge == "gitlab" {
						fmt.Fprintf(&b, `{"iid":%d,"title":"t%d","state":"closed","labels":["z","a"],"author":{"username":"u"},"created_at":%q,"updated_at":%q,"closed_at":%q,"task_completion_status":{"count":4,"completed_count":1}}`, id, id, at, at, at)
					} else if k == 0 {
						fmt.Fprintf(&b, `{"number":%d,"title":"a request","state":"closed","user":{"login":"u"},"created_at":%q,"updated_at":%q,"closed_at":%q,"pull_request":{}}`, id, at, at, at)
					} else {
						fmt.Fprintf(&b, `{"number":%d,"title":"t%d","state":"closed","labels":[{"name":"a"}],"user":{"login":"u"},"created_at":%q,"updated_at":%q,"closed_at":%q,"sub_issues_summary":{"total":4,"completed":1}}`, id, id, at, at, at)
					}
				}
				b.WriteString("]")
				return APIResponse{Status: http.StatusOK, Body: []byte(b.String())}, nil
			}
			since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			list, more, err := listIssues(fakeListAPI{page: page}, Repository{Forge: forge, Project: "example-org/widgets"}, since)
			if err != nil || more {
				t.Fatal(more, err)
			}
			want := indexPerPage + 2
			if forge == "github" {
				want -= 2 // a request on each page
			}
			if len(asked) != 2 || len(list) != want {
				t.Fatalf("%d pages, %d issues", len(asked), len(list))
			}
			if !strings.Contains(asked[0], "state=all") || !strings.Contains(asked[0], "2026-09-01T00%3A00%3A00Z") {
				t.Errorf("asked %s", asked[0])
			}
			got := list[0]
			if got.State != "closed" || got.ClosedAt.IsZero() || got.Parts != 4 || got.PartsDone != 1 || got.Labels[0] != "a" {
				t.Errorf("%+v", got)
			}
		})
	}
}

// listedAPI lists repo's issues from a slice, the way a forge pages them.
type listedAPI struct {
	asked *[]string
	body  func() string
	fail  *string
}

func (a listedAPI) answer(q string) (APIResponse, error) {
	*a.asked = append(*a.asked, q)
	if *a.fail != "" {
		return APIResponse{}, fmt.Errorf("%s", *a.fail)
	}
	return APIResponse{Status: http.StatusOK, Body: []byte(a.body())}, nil
}
func (a listedAPI) GitHub(q string, _ map[string]string) (APIResponse, error) { return a.answer(q) }
func (a listedAPI) GitLab(q string) (APIResponse, error)                      { return a.answer(q) }
func (a listedAPI) At(string) ForgeAPI                                        { return a }

// The page's listing records each issue once, carries on from the last
// change it holds, says the list is whole once it has read to the end, says
// so when a read fails, and holds nothing of a confidential issue.
func TestReadIssues(t *testing.T) {
	f := newLevelsFixture(t)
	f.cfg.ConfidentialPatterns = []string{"secretword"}
	u, _ := testUI(t, UIConfig{}, f.src)
	var asked []string
	fail := ""
	body := `[{"number":70,"title":"Done","state":"closed","labels":[{"name":"epic"}],"user":{"login":"al"},"created_at":"2026-09-01T00:00:00Z","updated_at":"2026-10-02T00:00:00Z","closed_at":"2026-10-02T00:00:00Z"},` +
		`{"number":71,"title":"the secretword issue","state":"closed","user":{"login":"al"},"created_at":"2026-09-01T00:00:00Z","updated_at":"2026-10-01T00:00:00Z","closed_at":"2026-10-01T00:00:00Z"}]`
	api := listedAPI{asked: &asked, body: func() string { return body }, fail: &fail}
	u.readAllIssues(api)
	got := f.src.st.ListedIssues["widgets"]["#70"]
	if got == nil || !got.closed() || got.Title != "Done" {
		t.Fatalf("not listed: %+v", got)
	}
	if s := f.src.st.ListedIssues["widgets"]["#71"]; s == nil || s.Title != "" || s.Withheld == 0 {
		t.Fatalf("confidential: %+v", s)
	}
	page := owner(t, u, "/r/widgets/closed")
	if strings.Contains(page, "secretword") || !strings.Contains(page, "(withheld") {
		t.Error("a confidential title shows")
	}
	if !strings.Contains(page, "Every closed issue, read from the forge") {
		t.Error("a list read whole does not say so")
	}
	if !strings.Contains(page, `class="badge umbrella"`) {
		t.Error("an epic label is no umbrella")
	}
	u.readAllIssues(api)
	if last := asked[len(asked)-1]; !strings.Contains(last, "since=2026-10-01T23%3A59%3A59Z") {
		t.Fatalf("the next listing does not carry on: %s", last)
	}
	fail = "the forge is down"
	u.readAllIssues(api)
	if page := owner(t, u, "/r/widgets/closed"); !strings.Contains(page, "The last read of the forge failed: the forge is down") {
		t.Error("a failed read is not told")
	}
}

// A listing that fails partway keeps the pages it read and tries again from
// there, so one hung call costs no more than itself.
func TestReadIssuesKeepsWhatItRead(t *testing.T) {
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{}, f.src)
	issue := func(n int, at string) string {
		return fmt.Sprintf(`{"number":%d,"title":"t","state":"closed","user":{"login":"u"},"created_at":%q,"updated_at":%q,"closed_at":%q}`, n, at, at, at)
	}
	fullPage := func() string {
		var is []string
		for n := 1; n <= indexPerPage; n++ {
			is = append(is, issue(n, "2026-09-01T00:00:00Z"))
		}
		return "[" + strings.Join(is, ",") + "]"
	}
	calls, hung := 0, true
	api := fakeListAPI{page: func(q string) (APIResponse, error) {
		calls++
		if strings.Contains(q, "since=") {
			if hung {
				hung = false
				return APIResponse{}, fmt.Errorf("hung")
			}
			return APIResponse{Status: http.StatusOK, Body: []byte("[" + issue(500, "2026-10-01T00:00:00Z") + "]")}, nil
		}
		if strings.Contains(q, "page=2") {
			return APIResponse{}, fmt.Errorf("hung")
		}
		return APIResponse{Status: http.StatusOK, Body: []byte(fullPage())}, nil
	}}
	u.readAllIssues(api)
	if n := len(f.src.st.ListedIssues["widgets"]); n != indexPerPage+1 {
		t.Fatalf("%d issues after %d calls", n, calls)
	}
	if page := owner(t, u, "/r/widgets/closed"); !strings.Contains(page, "Every closed issue") {
		t.Error("a list read whole after a failure does not say so")
	}
}
