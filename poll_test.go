package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeForgeAPI answers GitHub's notifications and issues, and GitLab's
// lists, from what the test sets, honouring If-Modified-Since as GitHub
// does.
type fakeForgeAPI struct {
	mu           sync.Mutex
	notes        string            // the notifications body
	lastModified string            // its Last-Modified
	pollInterval string            // X-Poll-Interval, if any
	github       map[string]string // other GitHub paths: body
	gitlab       map[string]string // GitLab path prefix: body
	githubErr    error
	gitlabErr    error
	calls        []string
}

func (f *fakeForgeAPI) GitHub(path string, header map[string]string) (APIResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "github "+path+" "+header["If-Modified-Since"])
	if f.githubErr != nil {
		return APIResponse{}, f.githubErr
	}
	if path != githubNotifications {
		if b, ok := f.github[path]; ok {
			return APIResponse{Status: http.StatusOK, Header: http.Header{}, Body: []byte(b)}, nil
		}
		return APIResponse{Status: http.StatusNotFound, Header: http.Header{}}, nil
	}
	h := http.Header{}
	h.Set("Last-Modified", f.lastModified)
	if f.pollInterval != "" {
		h.Set("X-Poll-Interval", f.pollInterval)
	}
	if ims := header["If-Modified-Since"]; ims != "" && ims == f.lastModified {
		return APIResponse{Status: http.StatusNotModified, Header: h}, nil
	}
	return APIResponse{Status: http.StatusOK, Header: h, Body: []byte(f.notes)}, nil
}

func (f *fakeForgeAPI) GitLab(path string) (APIResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "gitlab "+path)
	if f.gitlabErr != nil {
		return APIResponse{}, f.gitlabErr
	}
	for prefix, body := range f.gitlab {
		if strings.HasPrefix(path, prefix) {
			return APIResponse{Status: http.StatusOK, Body: []byte(body)}, nil
		}
	}
	return APIResponse{Status: http.StatusOK, Body: []byte("[]")}, nil
}

func (f *fakeForgeAPI) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeForgeAPI) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// pollHarness is a harness with no sessions, so every event is the poll's,
// over the tracking fixture, with the forges named polled.
func pollHarness(t *testing.T, github, gitlab bool) (*harness, *fakeForgeAPI) {
	t.Helper()
	h := newHarness(t, false)
	h.cfg.Tracking = trackingFixture(t)
	h.cfg.Poll.GitHub, h.cfg.Poll.GitLab = github, gitlab
	api := &fakeForgeAPI{lastModified: "Thu, 22 Oct 2026 00:00:00 GMT", notes: "[]"}
	h.sup.api = api
	return h, api
}

func (h *harness) itemEvents(id string) int {
	n := 0
	for _, e := range h.events() {
		if e.Kind == KindItem && e.Item.ID == id {
			n++
		}
	}
	return n
}

func TestGitLabNewIssueIsOneItem(t *testing.T) {
	h, api := pollHarness(t, false, true)
	h.ticks(1, 15*time.Minute) // the baseline: a cursor, no items, no calls
	if n := h.count(KindItem, "", ""); n != 0 || h.sup.st.Cursors[SourceGitLab].IsZero() {
		t.Fatalf("baseline: %d items, cursor %v", n, h.sup.st.Cursors[SourceGitLab])
	}
	created := h.clock.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	api.set(func() {
		api.gitlab = map[string]string{"/projects/example-org%2Fwidgets/issues": `[{"iid": 7, "title": "Widgets wobble", "created_at": "` + created +
			`", "updated_at": "` + created + `", "author": {"username": "user-a"}}]`}
	})
	h.ticks(3, 15*time.Minute)
	id := "gitlab/example-org/widgets#7"
	if n := h.itemEvents(id); n != 1 {
		t.Fatalf("item events for %s = %d, want 1", id, n)
	}
	if it := h.sup.st.Items[id]; it.Kind != ItemIssue || it.Author != "user-a" || it.Link != "https://gitlab.com/example-org/widgets/-/issues/7" {
		t.Fatalf("item = %+v", it.ItemPayload)
	}
	// Only the active GitLab repository is asked, with the cursor.
	for _, c := range api.callList() {
		if strings.Contains(c, "sprockets") {
			t.Fatalf("an inactive repository was asked: %s", c)
		}
		if strings.Contains(c, "/issues") && !strings.Contains(c, "updated_after=") {
			t.Fatalf("asked without the cursor: %s", c)
		}
	}
	// An issue only updated since the cursor is a discussion.
	updated := h.clock.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	api.set(func() {
		api.gitlab = map[string]string{"/projects/example-org%2Fwidgets/merge_requests": `[{"iid": 3, "title": "Old request", "created_at": "2026-01-01T00:00:00Z", "updated_at": "` +
			updated + `", "author": {"username": "user-b"}}]`}
	})
	h.ticks(1, 15*time.Minute)
	if n := h.itemEvents("gitlab/example-org/widgets!3@" + updated); n != 1 {
		t.Fatalf("discussion events = %d", n)
	}
}

func TestGitHubNotModifiedMakesNoEvent(t *testing.T) {
	h, api := pollHarness(t, true, false)
	h.ticks(1, 15*time.Minute) // baseline: 200, the cursor
	before := len(h.events())
	h.ticks(3, 15*time.Minute)
	if after := len(h.events()); after != before {
		t.Fatalf("unchanged polls made %d events", after-before)
	}
	calls := api.callList()
	if len(calls) != 4 || !strings.HasSuffix(calls[3], api.lastModified) {
		t.Fatalf("calls = %q", calls)
	}
}

// GitHub's X-Poll-Interval is honoured when it is longer than the policy's.
func TestGitHubPollInterval(t *testing.T) {
	h, api := pollHarness(t, true, false)
	api.pollInterval = "3600"
	h.ticks(4, 15*time.Minute)
	if n := len(api.callList()); n != 1 {
		t.Fatalf("calls within the poll interval = %d", n)
	}
	h.ticks(1, 15*time.Minute)
	if n := len(api.callList()); n != 2 {
		t.Fatalf("calls after the poll interval = %d", n)
	}
}

func TestGitHubNotificationsBecomeItems(t *testing.T) {
	h, api := pollHarness(t, true, false)
	h.ticks(1, 15*time.Minute)
	upd := h.clock.Now().UTC().Format(time.RFC3339)
	api.set(func() {
		api.lastModified = "Thu, 22 Oct 2026 00:20:00 GMT"
		api.notes = `[{"id": "1", "updated_at": "` + upd + `", "subject": {"title": "New gadget bug", "type": "Issue", "url": "https://api.github.com/repos/example-org/gadgets/issues/5"}, "repository": {"full_name": "example-org/gadgets"}},
			{"id": "2", "updated_at": "` + upd + `", "subject": {"title": "Old gadget request", "type": "PullRequest", "url": "https://api.github.com/repos/example-org/gadgets/pulls/2"}, "repository": {"full_name": "example-org/gadgets"}},
			{"id": "3", "updated_at": "` + upd + `", "subject": {"title": "Elsewhere", "type": "Issue", "url": "https://api.github.com/repos/someone/else/issues/1"}, "repository": {"full_name": "someone/else"}},
			{"id": "4", "updated_at": "` + upd + `", "subject": {"title": "CI", "type": "CheckSuite", "url": ""}, "repository": {"full_name": "example-org/gadgets"}}]`
		api.github = map[string]string{
			"/repos/example-org/gadgets/issues/5": `{"created_at": "` + upd + `", "user": {"login": "user-c"}}`,
			"/repos/example-org/gadgets/pulls/2":  `{"created_at": "2026-01-01T00:00:00Z", "user": {"login": "user-d"}}`,
		}
	})
	h.ticks(1, 15*time.Minute)
	if it := h.sup.st.Items["github/example-org/gadgets#5"]; it == nil || it.Kind != ItemIssue || it.Author != "user-c" {
		t.Fatalf("new issue item = %+v", it)
	}
	if it := h.sup.st.Items["github/example-org/gadgets!2@"+upd]; it == nil || it.Kind != ItemDiscussion {
		t.Fatalf("discussion item missing: %v", h.sup.st.Items)
	}
	if n := h.count(KindItem, "", ""); n != 2 {
		t.Fatalf("items = %d, want 2 (a repository outside the manifest and a check suite are not items)", n)
	}
	// Seen again with a later Last-Modified, the new issue is not a second item.
	api.set(func() { api.lastModified = "Thu, 22 Oct 2026 00:40:00 GMT" })
	h.ticks(1, 15*time.Minute)
	if n := h.itemEvents("github/example-org/gadgets#5"); n != 1 {
		t.Fatalf("item events = %d", n)
	}
}

func TestPollCursorSurvivesRestart(t *testing.T) {
	h, api := pollHarness(t, true, true)
	h.ticks(1, 15*time.Minute)
	gitlabCursor := h.sup.st.Cursors[SourceGitLab]
	h.restart(false)
	h.sup.api = api
	h.ticks(1, 15*time.Minute)
	calls := api.callList()
	if last := calls[len(calls)-1]; strings.HasPrefix(last, "github") && !strings.HasSuffix(last, api.lastModified) {
		t.Fatalf("GitHub asked without its cursor after a restart: %q", last)
	}
	want := "updated_after=" + gitlabCursor.UTC().Format(time.RFC3339)
	found := false
	for _, c := range calls {
		if strings.Contains(c, "/issues") && strings.Contains(c, strings.ReplaceAll(want, ":", "%3A")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("GitLab asked without its cursor %s after a restart: %q", want, calls)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "github") && strings.HasSuffix(c, " ") {
			if c != calls[0] {
				t.Fatalf("GitHub asked unconditionally after the baseline: %q", calls)
			}
		}
	}
}

func TestFailingSourceIsUnknown(t *testing.T) {
	h, api := pollHarness(t, true, true)
	h.ticks(1, 15*time.Minute)
	api.set(func() { api.gitlabErr = errors.New("glab: not logged in") })
	h.ticks(3, 15*time.Minute)
	p := h.sup.st.Polls[SourceGitLab]
	if p.OK || !strings.Contains(p.Error, "not logged in") {
		t.Fatalf("gitlab poll = %+v", p)
	}
	failures := 0
	for _, e := range h.events() {
		if e.Kind == KindPoll && e.Poll.Source == SourceGitLab && !e.Poll.OK {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("failure events = %d, want 1 while the error stays the same", failures)
	}
	_, meta := Summarize(h.cfg, h.events(), ModeLive)
	var b strings.Builder
	PrintStatus(&b, nil, meta, h.clock.Now())
	if !strings.Contains(b.String(), "gitlab: unknown") || !strings.Contains(b.String(), "not logged in") {
		t.Fatalf("status:\n%s", b.String())
	}
	// The cursor did not move while it failed, so nothing is missed.
	cursor := h.sup.st.Cursors[SourceGitLab]
	api.set(func() { api.gitlabErr = nil })
	h.ticks(1, 15*time.Minute)
	if !h.sup.st.Polls[SourceGitLab].OK || !h.sup.st.Cursors[SourceGitLab].After(cursor) {
		t.Fatalf("after recovery: %+v", h.sup.st.Polls[SourceGitLab])
	}
	found := false
	for _, c := range api.callList() {
		if strings.Contains(c, strings.ReplaceAll(cursor.UTC().Format(time.RFC3339), ":", "%3A")) {
			found = true
		}
	}
	if !found {
		t.Fatal("the poll after recovery did not ask from the cursor before the failure")
	}
	// No inventory: GitLab is unknown, not "nothing new".
	h2, _ := pollHarness(t, false, true)
	h2.cfg.Tracking.Manifest = h2.dir + "/missing.json"
	h2.ticks(1, 15*time.Minute)
	if p := h2.sup.st.Polls[SourceGitLab]; p.OK || p.Error == "" {
		t.Fatalf("gitlab without an inventory = %+v", p)
	}
}

func TestPollCallsAreCounted(t *testing.T) {
	h, api := pollHarness(t, false, true)
	h.ticks(2, 15*time.Minute)
	var p *PollPayload
	for _, e := range h.events() {
		if e.Kind == KindPoll {
			p = e.Poll
		}
	}
	// One active GitLab repository: issues, merge requests, and the to-do list.
	if p == nil || p.Calls != 3 || len(api.callList()) != 3 {
		t.Fatalf("poll = %+v, calls %q", p, api.callList())
	}
	_, meta := Summarize(h.cfg, h.events(), ModeLive)
	var b strings.Builder
	PrintStatus(&b, nil, meta, h.clock.Now())
	if !strings.Contains(b.String(), "gitlab: ok") || !strings.Contains(b.String(), "3 calls") {
		t.Fatalf("status:\n%s", b.String())
	}
}

// A to-do on a manifest repository's request is an item; one on another
// repository is not.
func TestGitLabTodos(t *testing.T) {
	h, api := pollHarness(t, false, true)
	h.ticks(1, 15*time.Minute)
	upd := h.clock.Now().UTC().Format(time.RFC3339)
	api.set(func() {
		api.gitlab = map[string]string{"/todos": fmt.Sprintf(`[{"id": 1, "created_at": %q, "target_type": "MergeRequest", "author": {"username": "user-e"},
			"project": {"path_with_namespace": "example-org/tools/sprockets"}, "target": {"iid": 9, "title": "Review me", "created_at": "2026-01-01T00:00:00Z", "updated_at": %q}},
			{"id": 2, "created_at": %q, "target_type": "Issue", "author": {"username": "user-f"},
			"project": {"path_with_namespace": "someone/else"}, "target": {"iid": 1, "title": "Not ours", "created_at": %q, "updated_at": %q}}]`, upd, upd, upd, upd, upd)}
	})
	h.ticks(1, 15*time.Minute)
	if it := h.sup.st.Items["gitlab/example-org/tools/sprockets!9@"+upd]; it == nil || it.Author != "user-e" {
		t.Fatalf("to-do item missing: %v", h.sup.st.Items)
	}
	if n := h.count(KindItem, "", ""); n != 1 {
		t.Fatalf("items = %d, want 1", n)
	}
}

// The CLIs' --include output: a status line, headers, a blank line, the body.
func TestParseIncluded(t *testing.T) {
	r, err := parseIncluded([]byte("HTTP/2.0 200 OK\r\nLast-Modified: Thu, 22 Oct 2026 00:00:00 GMT\r\nX-Poll-Interval: 60\r\n\r\n[{\"id\": \"1\"}]"))
	if err != nil || r.Status != 200 || r.Header.Get("X-Poll-Interval") != "60" || string(r.Body) != `[{"id": "1"}]` {
		t.Fatalf("200 = %+v, %v", r, err)
	}
	r, err = parseIncluded([]byte("HTTP/2.0 304 Not Modified\r\nLast-Modified: Thu, 22 Oct 2026 00:00:00 GMT\r\n\r\n"))
	if err != nil || r.Status != 304 {
		t.Fatalf("304 = %+v, %v", r, err)
	}
	if _, err := parseIncluded([]byte("gh: not logged in\n")); err == nil {
		t.Fatal("an error message parsed as a reply")
	}
}
