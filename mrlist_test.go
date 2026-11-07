package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// routedAPI answers each path with the body of the first prefix it starts
// with, and counts the calls.
type routedAPI struct {
	routes map[string]string
	asked  *[]string
}

func (a routedAPI) answer(q string) (APIResponse, error) {
	*a.asked = append(*a.asked, q)
	best := ""
	for p := range a.routes {
		if strings.HasPrefix(q, p) && len(p) > len(best) {
			best = p
		}
	}
	if best == "" {
		return APIResponse{}, fmt.Errorf("no route for %s", q)
	}
	return APIResponse{Status: http.StatusOK, Body: []byte(a.routes[best])}, nil
}
func (a routedAPI) GitHub(q string, _ map[string]string) (APIResponse, error) { return a.answer(q) }
func (a routedAPI) GitLab(q string) (APIResponse, error)                      { return a.answer(q) }
func (a routedAPI) At(string) ForgeAPI                                        { return a }

// A GitHub repository's open requests are read with what CI says of each
// head: a failed run fails it, a run still going is running, all green
// passes, and with no runs the combined status speaks. A settled one is not
// asked again while its head stays; a confidential title is withheld. The
// repository page lists them with their CI, and home counts them.
func TestReadRequests(t *testing.T) {
	f := newLevelsFixture(t)
	f.cfg.ConfidentialPatterns = []string{"secretword"}
	u, _ := testUI(t, UIConfig{}, f.src)
	u.home = t.TempDir()
	u.tmuxArgs = []string{"-L", "agentd-test-none", "-f", "/dev/null"}
	pr := func(n int, title, sha, updated string) string {
		return fmt.Sprintf(`{"number":%d,"title":%q,"html_url":"https://github.com/example-org/widgets/pull/%d","draft":false,"user":{"login":"al"},"created_at":"2026-10-01T00:00:00Z","updated_at":%q,"head":{"sha":%q}}`, n, title, n, updated, sha)
	}
	runs := func(states ...string) string {
		var r []string
		for _, s := range states {
			st, concl, _ := strings.Cut(s, "/")
			r = append(r, fmt.Sprintf(`{"status":%q,"conclusion":%q}`, st, concl))
		}
		return fmt.Sprintf(`{"total_count":%d,"check_runs":[%s]}`, len(r), strings.Join(r, ","))
	}
	var asked []string
	api := routedAPI{asked: &asked, routes: map[string]string{
		"/repos/example-org/widgets/pulls?state=open": "[" + strings.Join([]string{
			pr(1, "Passes", "aaa", "2026-10-05T00:00:00Z"),
			pr(2, "Fails", "bbb", "2026-10-04T00:00:00Z"),
			pr(3, "Runs", "ccc", "2026-10-03T00:00:00Z"),
			pr(4, "Has statuses only", "ddd", "2026-10-02T00:00:00Z"),
			pr(5, "the secretword change", "eee", "2026-10-01T00:00:00Z"),
		}, ",") + "]",
		"/repos/example-org/widgets/commits/aaa/check-runs": runs("completed/success", "completed/skipped"),
		"/repos/example-org/widgets/commits/bbb/check-runs": runs("completed/success", "completed/failure", "in_progress/"),
		"/repos/example-org/widgets/commits/ccc/check-runs": runs("completed/success", "queued/"),
		"/repos/example-org/widgets/commits/ddd/check-runs": runs(),
		"/repos/example-org/widgets/commits/ddd/status":     `{"state":"failure","total_count":2}`,
		"/repos/example-org/widgets/commits/eee/check-runs": runs(),
		"/repos/example-org/widgets/commits/eee/status":     `{"state":"pending","total_count":0}`,
		"/repos/example-org/widgets/issues":                 `[]`,
	}}
	u.readAllIssues(api)
	got := f.src.requests["widgets"]
	if got.Error != "" || got.Read.IsZero() {
		t.Fatalf("reading: %+v", got)
	}
	var ci []string
	for _, q := range got.List {
		ci = append(ci, q.Ref+" "+q.CI)
	}
	if s := strings.Join(ci, ", "); s != "#1 success, #2 failed, #3 pending, #4 failed, #5 none" {
		t.Fatalf("CI: %s", s)
	}
	if q := got.List[4]; !q.Withheld || q.Title != "" || q.Author != "" {
		t.Errorf("a confidential request: %+v", q)
	}
	if pass, fail, other := got.counts(); pass != 1 || fail != 2 || other != 2 {
		t.Errorf("counts %d %d %d", pass, fail, other)
	}

	// Again, with nothing moved: the settled ones are not asked again.
	asked = nil
	u.readAllIssues(api)
	for _, q := range asked {
		if strings.Contains(q, "/commits/aaa/") || strings.Contains(q, "/commits/bbb/") || strings.Contains(q, "/commits/ddd/") {
			t.Errorf("asked again of a settled head: %s", q)
		}
	}
	if !strings.Contains(strings.Join(asked, " "), "/commits/ccc/") {
		t.Error("a running one was not asked again")
	}

	page := owner(t, u, "/r/widgets")
	for _, want := range []string{`id="merge-requests"`, "CI passing", "CI failing", "CI running", "no CI", `href="https://github.com/example-org/widgets/pull/2"`, "title withheld"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	if strings.Contains(page, "secretword") {
		t.Error("the page shows a confidential title")
	}
	home := owner(t, u, "/")
	for _, want := range []string{`data-mrpass="1" data-mrfail="2"`, `title="5 open merge requests: 1 passing CI, 2 failing CI, 2 with CI running, none or unknown"`, `<span class="mr-pass">1</span>/<span class="mr-fail">2</span>`} {
		if !strings.Contains(home, want) {
			t.Errorf("home lacks %s", want)
		}
	}

	// A failed read keeps the list and says why.
	delete(api.routes, "/repos/example-org/widgets/pulls?state=open")
	u.readAllIssues(api)
	if got := f.src.requests["widgets"]; got.Error == "" || len(got.List) != 5 {
		t.Errorf("after a failed read: %q, %d", got.Error, len(got.List))
	}
	if page := owner(t, u, "/r/widgets"); !strings.Contains(page, "The last read of the forge failed") {
		t.Error("the page does not say the read failed")
	}
}

// GitLab: the head pipeline's status, and none without one.
func TestReadRequestsGitLab(t *testing.T) {
	var asked []string
	api := routedAPI{asked: &asked, routes: map[string]string{
		"/projects/example-org%2Fgadgets/merge_requests?state=opened": `[{"iid":7,"title":"Seven","web_url":"https://gitlab.com/example-org/gadgets/-/merge_requests/7","draft":true,"sha":"s7","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-02T00:00:00Z","author":{"username":"bo"}},` +
			`{"iid":8,"title":"Eight","web_url":"https://gitlab.com/example-org/gadgets/-/merge_requests/8","sha":"s8","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-03T00:00:00Z","author":{"username":"bo"}}]`,
		"/projects/example-org%2Fgadgets/merge_requests/7": `{"head_pipeline":{"status":"success"}}`,
		"/projects/example-org%2Fgadgets/merge_requests/8": `{"head_pipeline":null}`,
	}}
	r := Repository{Forge: "gitlab", Project: "example-org/gadgets"}
	list, err := listRequests(api, r)
	if err != nil || len(list) != 2 {
		t.Fatalf("%v %d", err, len(list))
	}
	if got := requestCI(api, r, list[0]); got != CISuccess {
		t.Errorf("!7: %s", got)
	}
	if got := requestCI(api, r, list[1]); got != CINone {
		t.Errorf("!8: %s", got)
	}
	if !list[0].Draft || list[0].Ref != "!7" || list[0].Author != "bo" {
		t.Errorf("!7: %+v", list[0])
	}
}
