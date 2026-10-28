package main

import (
	"bytes"
	"encoding/json"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// boxRows are the rows of a box on a rendered page, each its data-*
// attributes, in the page's order.
func boxRows(t *testing.T, body, box string) []map[string]string {
	t.Helper()
	start := strings.Index(body, `data-box="`+box+`"`)
	if start < 0 {
		t.Fatalf("no %s box", box)
	}
	end := strings.Index(body[start:], "</ol>")
	attr := regexp.MustCompile(`data-([a-z]+)="([^"]*)"`)
	var rows []map[string]string
	for _, li := range regexp.MustCompile(`<li class="box-row[^>]*>`).FindAllString(body[start:start+end], -1) {
		row := map[string]string{}
		for _, m := range attr.FindAllStringSubmatch(li, -1) {
			row[m[1]] = html.UnescapeString(m[2])
		}
		rows = append(rows, row)
	}
	return rows
}

// scriptOrder sorts rows as ui.js sorts a box by each of sorts, run in
// node, and returns each sort's refs in order.
func scriptOrder(t *testing.T, rows []map[string]string, sorts []string) map[string][]string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node to run ui.js")
	}
	js, err := uiAssets.ReadFile("ui/ui.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	begin, end := strings.Index(s, "// box sorts: begin"), strings.Index(s, "// box sorts: end")
	if begin < 0 || end < begin {
		t.Fatal("ui.js marks no box sorts")
	}
	in, _ := json.Marshal(map[string]any{"rows": rows, "sorts": sorts})
	prog := s[begin:end] + `
var input = ` + string(in) + `;
var out = {};
input.sorts.forEach(function (by) {
  var rows = input.rows.map(function (d) { return { dataset: d }; });
  rows.sort(function (a, b) { return compareRows(by, a, b); });
  out[by] = rows.map(function (r) { return r.dataset.ref; });
});
console.log(JSON.stringify(out));
`
	file := filepath.Join(t.TempDir(), "sorts.js")
	if err := os.WriteFile(file, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command(node, file).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	var got map[string][]string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("node said %s", b)
	}
	return got
}

// The page's script and the Go sorts order the same rows the same way,
// every sort of both boxes; labels, milestones and authors compare
// without regard to case or locale, so "bug" comes before "Feature" on
// the page and on the command line alike.
func TestSortsAgreeWithScript(t *testing.T) {
	f := newLevelsFixture(t)
	for n, label := range map[string]string{"#11": "Feature", "#12": "bug", "#13": "Zeta"} {
		p := IndexPayload{Repository: "widgets", Ref: n, Title: "Labelled " + label, State: "open", Labels: []string{label}, Author: strings.ToLower(label), Opened: pageAt.Add(-time.Duration(len(label)) * time.Hour), Changed: pageAt.Add(-time.Hour)}
		if err := f.log.Append(Event{V: EventVersion, Time: pageAt.Add(-2 * time.Hour), Kind: KindIndex, Mode: ModeLive, Index: &p}); err != nil {
			t.Fatal(err)
		}
	}
	u, _ := testUI(t, UIConfig{}, f.src)
	body := owner(t, u, "/repo/widgets")
	v := BuildRepoLevel(f.cfg, f.src.st, "widgets", nil, pageAt)
	issues := scriptOrder(t, boxRows(t, body, "issues"), IssueSorts)
	for _, by := range IssueSorts {
		is := append([]LevelIssue(nil), v.Issues...)
		sortIssues(is, by)
		var want []string
		for _, i := range is {
			want = append(want, i.Ref)
		}
		if strings.Join(issues[by], " ") != strings.Join(want, " ") {
			t.Errorf("%s: the page %v, Go %v", by, issues[by], want)
		}
	}
	if l := strings.Join(issues["label"], " "); strings.Index(l, "#12") > strings.Index(l, "#11") {
		t.Errorf("label: bug after Feature: %s", l)
	}
	requests := scriptOrder(t, boxRows(t, body, "requests"), RequestSorts)
	for _, by := range RequestSorts {
		rs := append([]LevelRequest(nil), v.Requests...)
		sortRequests(rs, by)
		var want []string
		for _, r := range rs {
			want = append(want, r.Ref)
		}
		if strings.Join(requests[by], " ") != strings.Join(want, " ") {
			t.Errorf("requests %s: the page %v, Go %v", by, requests[by], want)
		}
	}
}

// The sorts sort by what they name: newest and oldest by the number,
// working by how long its session has run, has an MR by where its
// request stands in the loop.
func TestBoxSortsByWhatTheyName(t *testing.T) {
	day := func(n int) time.Time { return time.Date(2026, 10, n, 0, 0, 0, 0, time.UTC) }
	is := []LevelIssue{
		{Ref: "#3", Number: 3, Opened: day(9), Working: true, RunSince: day(20), Requests: []string{"!1"}, MRRank: 3},
		{Ref: "#8", Number: 8, Opened: day(1), Working: true, RunSince: day(10), Requests: []string{"!2"}, MRRank: 0},
		{Ref: "#5", Number: 5, Opened: day(5)},
	}
	order := func(by string) string {
		s := append([]LevelIssue(nil), is...)
		sortIssues(s, by)
		var refs []string
		for _, i := range s {
			refs = append(refs, i.Ref)
		}
		return strings.Join(refs, " ")
	}
	for by, want := range map[string]string{"newest": "#8 #5 #3", "oldest": "#3 #5 #8", "working": "#8 #3 #5", "has-mr": "#8 #3 #5"} {
		if got := order(by); got != want {
			t.Errorf("%s: %s, want %s", by, got, want)
		}
	}
}

// The command line has what the page has: the MR box's sorts, an issue's
// labels, milestone and author, a row's labels, a request's author; the
// counts agree in number; the filter finds an item or a session by its id.
func TestLevelsCLIParity(t *testing.T) {
	f := newLevelsFixture(t)
	cfgPath := levelsCLI(t, f)
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		ok, err := levelConsoleCmd(append([]string{"--config", cfgPath}, args...), &out)
		if !ok || err != nil {
			t.Fatalf("%v: %v %v", args, ok, err)
		}
		return out.String()
	}
	page := run("widgets", "--mr-sort", "newest")
	for _, want := range []string{"1 needs you", "[perf]", "by ann", "2 open requests"} {
		if !strings.Contains(page, want) {
			t.Errorf("console widgets lacks %q:\n%s", want, page)
		}
	}
	if i, j := strings.Index(page, "!10 "), strings.Index(page, "!9 "); i < 0 || j < 0 || i > j {
		t.Errorf("the MR box sorted newest has !9 before !10:\n%s", page)
	}
	if box := run("widgets", "--issues", "--filter", "wa1"); !strings.Contains(box, "#5") || strings.Contains(box, "#6") {
		t.Errorf("the filter does not find the item by its id:\n%s", box)
	}
	var out bytes.Buffer
	if err := issueCmd([]string{"--config", cfgPath, "widgets", "4"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"labels: perf", "milestone: v2", "opened by ann"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("issue widgets 4 lacks %q:\n%s", want, out.String())
		}
	}
}

// The pages: home's waits-on-you number opens the repository sorted by
// needs me; a request row names its author; the issue box's counts agree
// in number; the issue page's history is the issue's every step; the
// filter text holds the item's and the session's ids.
func TestLevelPagesSmallThings(t *testing.T) {
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{}, f.src)
	if body := owner(t, u, "/"); !strings.Contains(body, `<a href="/repo/widgets?sort=needs-me"><strong>1</strong></a>`) {
		t.Errorf("home's number is no link:\n%s", body)
	}
	body := owner(t, u, "/repo/widgets")
	for _, want := range []string{"by ann", "1 needs you", "2 open requests"} {
		if !strings.Contains(body, want) {
			t.Errorf("the repository page lacks %q", want)
		}
	}
	if !regexp.MustCompile(`data-ref="#5"[^>]*data-text="[^"]*wa1[^"]*w4`).MatchString(body) {
		t.Error("#5's filter text holds no item or session id")
	}
	if body := owner(t, u, "/repo/widgets/issue/5"); !strings.Contains(body, `<a href="?steps=all">history</a>`) {
		t.Error("the issue's history crumb is not its every step")
	}
}
