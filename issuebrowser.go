package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The issue browser: what a repository's pages add to the open list. Every
// issue ever listed from the forge, so the closed ones are a list of their
// own; the priority the owner sets on an issue; and which issues are
// umbrellas. It is the page process's own data, kept in a file beside the
// event log and never in it: the log's readers, the supervisor and the
// sessions' tool servers among them, refuse an entry they do not know, and
// nothing here is theirs to act on.

// issueListPages bounds one request's worth of a listing, after which what
// it found is recorded before it carries on.
const issueListPages = 20

// PriorityLevels are the priorities the owner can set, highest first.
var PriorityLevels = []string{"high", "medium", "low"}

// ListedIssue is an issue as the browser's listing read it, open or closed.
type ListedIssue struct {
	IndexPayload
	// ClosedAt is when it closed, zero while it is open; Parts and PartsDone
	// its sub-issues or task list and how many of them are done.
	ClosedAt  time.Time `json:"closed_at"`
	Parts     int       `json:"parts,omitempty"`
	PartsDone int       `json:"parts_done,omitempty"`
}

func (i *ListedIssue) closed() bool { return i.State == "closed" }

// same says whether two listings say the same of an issue.
func (i ListedIssue) same(o ListedIssue) bool {
	return i.IndexPayload.same(o.IndexPayload) && i.ClosedAt.Equal(o.ClosedAt) && i.Parts == o.Parts && i.PartsDone == o.PartsDone
}

// PriorityPayload is the priority the owner set on an issue; an empty
// Level clears it.
type PriorityPayload struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
	Level      string `json:"level,omitempty"`
}

func (p PriorityPayload) check() error {
	if p.Repository == "" || !issueRefPattern.MatchString(p.Ref) {
		return fmt.Errorf("a priority names its repository and an issue")
	}
	if p.Level != "" && !contains(PriorityLevels, p.Level) {
		return fmt.Errorf("a priority is one of %s, or none", strings.Join(PriorityLevels, ", "))
	}
	return nil
}

// priorityRank orders the levels for the priority sort, none last.
func priorityRank(level string) int {
	for k, l := range PriorityLevels {
		if l == level {
			return k
		}
	}
	return len(PriorityLevels)
}

// issueRecord is a line of the issue file: an issue as listed, or a
// priority set. The newest line about an issue stands.
type issueRecord struct {
	At       time.Time        `json:"at"`
	Issue    *ListedIssue     `json:"issue,omitempty"`
	Priority *PriorityPayload `json:"priority,omitempty"`
}

// apply brings the fold's browser maps up to date with r.
func (st *LogState) applyIssueRecord(r issueRecord) {
	if i := r.Issue; i != nil && i.Repository != "" {
		if st.ListedIssues == nil {
			st.ListedIssues = map[string]map[string]*ListedIssue{}
		}
		if st.ListedIssues[i.Repository] == nil {
			st.ListedIssues[i.Repository] = map[string]*ListedIssue{}
		}
		st.ListedIssues[i.Repository][i.Ref] = i
	}
	if p := r.Priority; p != nil && p.Repository != "" {
		if st.Priorities == nil {
			st.Priorities = map[string]map[string]string{}
		}
		if st.Priorities[p.Repository] == nil {
			st.Priorities[p.Repository] = map[string]string{}
		}
		if p.Level == "" {
			delete(st.Priorities[p.Repository], p.Ref)
		} else {
			st.Priorities[p.Repository][p.Ref] = p.Level
		}
	}
}

// issueFile is the issue file of an event log.
func issueFile(eventLog string) string { return eventLog + ".issues.jsonl" }

// loadIssues reads the source's issue file, which may not exist yet, and
// from then on records go to it. A line it cannot read, as the last after a
// crash may be, is passed over: the next listing or the owner restores it.
func (src *uiSource) loadIssues(path string) error {
	src.mu.Lock()
	defer src.mu.Unlock()
	src.issuePath = path
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var rec issueRecord
			if json.Unmarshal(line, &rec) == nil {
				src.st.applyIssueRecord(rec)
			}
		}
		if err != nil {
			return nil
		}
	}
}

// recordIssues appends recs to the issue file and folds them, under src.mu.
func (src *uiSource) recordIssues(recs []issueRecord) error {
	if src.issuePath != "" {
		f, err := os.OpenFile(src.issuePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		w := bufio.NewWriter(f)
		enc := json.NewEncoder(w)
		for _, r := range recs {
			if err := enc.Encode(r); err != nil {
				f.Close()
				return err
			}
		}
		if err := w.Flush(); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	for _, r := range recs {
		src.st.applyIssueRecord(r)
	}
	return nil
}

// setPriority records the owner's priority on an issue the source knows of.
func (src *uiSource) setPriority(repo, ref, level string) error {
	p := PriorityPayload{Repository: repo, Ref: ref, Level: level}
	if err := p.check(); err != nil {
		return err
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if err := src.catchUp(); err != nil {
		return err
	}
	st := src.st
	if st.Index[repo][ref] == nil && st.Closed[repo][ref] == nil && st.ListedIssues[repo][ref] == nil {
		return fmt.Errorf("no issue %s in %s", ref, repo)
	}
	return src.recordIssues([]issueRecord{{At: src.clock().UTC(), Priority: &p}})
}

// umbrellaTitle is an issue's title saying it is an umbrella or an epic.
var umbrellaTitle = regexp.MustCompile(`(?i)\bumbrella\b|^\s*epic\b`)

// isUmbrella says whether an issue gathers others, as neither forge marks
// that in one way: a label or title saying so, or GitHub sub-issues. A
// GitLab task list does not count alone, as any issue may keep a checklist.
func isUmbrella(forge, title string, labels []string, parts int) bool {
	if forge == "github" && parts > 0 || umbrellaTitle.MatchString(title) {
		return true
	}
	for _, l := range labels {
		if i := strings.LastIndex(l, ":"); i >= 0 {
			l = l[i+1:]
		}
		switch strings.ToLower(strings.TrimSpace(l)) {
		case "umbrella", "epic", "tracking":
			return true
		}
	}
	return false
}

// parseGitLabIssues reads a page of GitLab's issue list; a state other than
// "" says what the list holds, else each issue says.
func parseGitLabIssues(b []byte, state string) ([]ListedIssue, error) {
	var is []struct {
		IID       int                     `json:"iid"`
		Title     string                  `json:"title"`
		State     string                  `json:"state"`
		Labels    []string                `json:"labels"`
		Milestone *struct{ Title string } `json:"milestone"`
		Author    struct{ Username string }
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
		ClosedAt  time.Time `json:"closed_at"`
		Tasks     struct {
			Count int `json:"count"`
			Done  int `json:"completed_count"`
		} `json:"task_completion_status"`
	}
	if err := json.Unmarshal(b, &is); err != nil {
		return nil, err
	}
	out := make([]ListedIssue, 0, len(is))
	for _, i := range is {
		q := IndexPayload{Ref: "#" + strconv.Itoa(i.IID), Title: i.Title, State: listedState(state, i.State), Labels: i.Labels, Author: i.Author.Username,
			Opened: i.CreatedAt, Changed: i.UpdatedAt}
		if i.Milestone != nil {
			q.Milestone = i.Milestone.Title
		}
		out = append(out, ListedIssue{IndexPayload: q, ClosedAt: i.ClosedAt, Parts: i.Tasks.Count, PartsDone: i.Tasks.Done})
	}
	return out, nil
}

// parseGitHubIssues reads a page of GitHub's issue list, which holds
// requests too.
func parseGitHubIssues(b []byte, state string) ([]ListedIssue, error) {
	var is []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Milestone   *struct{ Title string } `json:"milestone"`
		User        struct{ Login string }  `json:"user"`
		Draft       bool                    `json:"draft"`
		CreatedAt   time.Time               `json:"created_at"`
		UpdatedAt   time.Time               `json:"updated_at"`
		ClosedAt    time.Time               `json:"closed_at"`
		PullRequest *struct{}               `json:"pull_request"`
		Subs        struct {
			Total int `json:"total"`
			Done  int `json:"completed"`
		} `json:"sub_issues_summary"`
	}
	if err := json.Unmarshal(b, &is); err != nil {
		return nil, err
	}
	out := make([]ListedIssue, 0, len(is))
	for _, i := range is {
		q := IndexPayload{Ref: "#" + strconv.Itoa(i.Number), Title: i.Title, State: listedState(state, i.State), Author: i.User.Login, Opened: i.CreatedAt, Changed: i.UpdatedAt}
		if i.PullRequest != nil {
			q.Ref, q.Draft = "!"+strconv.Itoa(i.Number), i.Draft
		}
		for _, l := range i.Labels {
			q.Labels = append(q.Labels, l.Name)
		}
		if i.Milestone != nil {
			q.Milestone = i.Milestone.Title
		}
		out = append(out, ListedIssue{IndexPayload: q, ClosedAt: i.ClosedAt, Parts: i.Subs.Total, PartsDone: i.Subs.Done})
	}
	return out, nil
}

// listedState is an issue's state: the list's when it has one, else the
// issue's own, where GitLab's opened is open.
func listedState(list, own string) string {
	switch {
	case list != "":
		return list
	case own == "opened":
		return "open"
	}
	return own
}

// trimListed bounds what a listing keeps of each issue.
func trimListed(p *IndexPayload) {
	p.Repository = "" // set by the caller
	p.Title = cutUTF8(p.Title, indexTitle)
	if len(p.Labels) > indexLabels {
		p.Labels = p.Labels[:indexLabels]
	}
	sort.Strings(p.Labels)
}

// listIssues lists r's issues of any state changed since since, the oldest
// change first, at most issueListPages pages; more says there may be others.
// On an error it returns the pages it read as well, which hold.
func listIssues(api ForgeAPI, r Repository, since time.Time) (out []ListedIssue, more bool, err error) {
	after := ""
	if !since.IsZero() {
		after = url.QueryEscape(since.UTC().Format(time.RFC3339))
	}
	for n := 1; n <= issueListPages; n++ {
		var resp APIResponse
		var page []ListedIssue
		if r.Forge == "gitlab" {
			q := fmt.Sprintf("/projects/%s/issues?state=all&order_by=updated_at&sort=asc&per_page=%d&page=%d", url.PathEscape(r.Project), indexPerPage, n)
			if after != "" {
				q += "&updated_after=" + after
			}
			resp, err = api.GitLab(q)
		} else {
			q := fmt.Sprintf("/repos/%s/issues?state=all&sort=updated&direction=asc&per_page=%d&page=%d", r.Project, indexPerPage, n)
			if after != "" {
				q += "&since=" + after
			}
			resp, err = api.GitHub(q, nil)
		}
		if err != nil {
			return out, false, err
		}
		if resp.Status != http.StatusOK {
			return out, false, statusError("issues", resp)
		}
		if r.Forge == "gitlab" {
			page, err = parseGitLabIssues(resp.Body, "")
		} else {
			page, err = parseGitHubIssues(resp.Body, "")
		}
		if err != nil {
			return out, false, fmt.Errorf("issues: %w", err)
		}
		for _, p := range page {
			if p.Ref[0] == '#' {
				trimListed(&p.IndexPayload)
				out = append(out, p)
			}
		}
		if len(page) < indexPerPage {
			return out, false, nil
		}
	}
	return out, true, nil
}

// issueReading is how a repository's listing last went, for its closed list.
type issueReading struct {
	Read  time.Time // when a listing last reached the end
	Error string    // why the last one failed, "" when it did not
}

// issueListTries is how many times running a listing may fail without
// getting further before it waits for the next poll.
const issueListTries = 3

// readIssues brings repo's listed issues up to date from the last change
// the file holds, recording as it goes, until the forge has no more. What a
// failed listing read holds, and it carries on from there.
func (src *uiSource) readIssues(api ForgeAPI, repo string, r Repository) {
	src.mu.Lock()
	secret := confidentialMatcher(src.cfg)
	src.mu.Unlock()
	note := func(msg string) {
		src.mu.Lock()
		defer src.mu.Unlock()
		if src.issueReads == nil {
			src.issueReads = map[string]issueReading{}
		}
		k := src.issueReads[repo]
		if msg == "" {
			k.Read = src.clock()
		}
		k.Error = msg
		src.issueReads[repo] = k
	}
	failed := 0
	for {
		src.mu.Lock()
		var since time.Time
		known := src.st.ListedIssues[repo]
		for _, i := range known {
			if i.Changed.After(since) {
				since = i.Changed
			}
		}
		src.mu.Unlock()
		if !since.IsZero() {
			since = since.Add(-time.Second) // issues changed in the same second as the last
		}
		list, more, listErr := listIssues(api.At(r.Host), r, since)
		var recs []issueRecord
		src.mu.Lock()
		for k := range list {
			i := list[k]
			i.Repository = repo
			if secret(append([]string{i.Title, i.Milestone, i.Author}, i.Labels...)...) {
				i.Title, i.Milestone, i.Author, i.Labels, i.Withheld = "", "", "", nil, 1
			}
			if o := src.st.ListedIssues[repo][i.Ref]; o != nil && o.same(i) {
				continue
			}
			recs = append(recs, issueRecord{At: src.clock().UTC(), Issue: &i})
		}
		err := src.recordIssues(recs)
		src.mu.Unlock()
		if err != nil {
			note(oneLine(err.Error(), 300))
			return
		}
		if listErr != nil {
			if len(recs) > 0 {
				failed = 0
			}
			if failed++; failed >= issueListTries {
				note(oneLine(listErr.Error(), 300))
				return
			}
			continue
		}
		// A page of issues all at one last change would list the same again.
		if !more || len(recs) == 0 {
			note("")
			return
		}
	}
}

// readAllIssues lists every repository the workspace serves once, one after
// another, starting from what the tracking system follows now: its issues,
// then its open requests.
func (u *UI) readAllIssues(api ForgeAPI) {
	u.loadCatalog()
	u.readRepos(api, sortedKeys(u.workspaceRepos()))
}

// pollIssues keeps the listed issues up to date until ctx ends.
func (u *UI) pollIssues(ctx context.Context, api ForgeAPI, every time.Duration) {
	for {
		u.readAllIssues(api)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// eachClosed calls fn on each of repo's closed issues, those the listing
// holds as closed and those only seen to leave the open list, with when
// each closed. One the open list holds is open, whatever else says.
func (st *LogState) eachClosed(repo string, fn func(i IndexPayload, l *ListedIssue, at time.Time)) {
	listed := st.ListedIssues[repo]
	for ref, l := range listed {
		if l.closed() && st.Index[repo][ref] == nil {
			at := l.ClosedAt
			if at.IsZero() {
				at = l.Changed
			}
			fn(l.IndexPayload, l, at)
		}
	}
	for ref, i := range st.Closed[repo] {
		if ref[0] == '#' && st.Index[repo][ref] == nil && listed[ref] == nil {
			fn(i.IndexPayload, nil, i.Closed)
		}
	}
}

// closedCount is how many closed issues of repo are known.
func closedCount(st *LogState, repo string) int {
	n := 0
	st.eachClosed(repo, func(IndexPayload, *ListedIssue, time.Time) { n++ })
	return n
}

// closedRows are repo's closed issues as rows, the latest closed first.
func closedRows(cfg *Config, st *LogState, repo string, now time.Time) []LevelIssue {
	rows := []LevelIssue{}
	st.eachClosed(repo, func(i IndexPayload, _ *ListedIssue, at time.Time) {
		li := plainRow(cfg, st, repo, i)
		li.ClosedAt, li.Word = at, closedWord(at, now)
		rows = append(rows, li)
	})
	sort.Slice(rows, func(a, b int) bool {
		if !rows[a].ClosedAt.Equal(rows[b].ClosedAt) {
			return rows[a].ClosedAt.After(rows[b].ClosedAt)
		}
		return rows[a].Number > rows[b].Number
	})
	return rows
}

// setBrowserFields sets what the box shows of any issue, open or closed.
func (i *LevelIssue) setBrowserFields(cfg *Config, st *LogState, x *IndexIssue) {
	i.Priority = st.Priorities[i.Repository][i.Ref]
	if l := st.ListedIssues[i.Repository][i.Ref]; l != nil {
		i.Parts, i.PartsDone = l.Parts, l.PartsDone
	}
	i.Umbrella = isUmbrella(cfg.Repositories[i.Repository].Forge, x.Title, x.Labels, i.Parts)
}

// PriorityRank is the row's place in the priority sort, for the script.
func (i LevelIssue) PriorityRank() int { return priorityRank(i.Priority) }

// ClosedUnix is when it closed, for the page's script, 0 for an open one.
func (i LevelIssue) ClosedUnix() int64 {
	if i.ClosedAt.IsZero() {
		return 0
	}
	return i.ClosedAt.Unix()
}

// IsClosed says whether the row is a closed issue.
func (i LevelIssue) IsClosed() bool { return !i.ClosedAt.IsZero() }

// ClosedSorts are the closed list's sorts, the first the default.
var ClosedSorts = []string{"recently-closed", "oldest-closed", "newest", "oldest", "last-change", "stale", "priority", "umbrella", "label", "milestone", "author"}

// sortLabels are the sorts as the owner reads them.
var sortLabels = map[string]string{
	"needs-me": "Needs me first", "priority": "Priority, highest first", "umbrella": "Umbrellas first",
	"last-change": "Recently changed", "stale": "Least recently changed",
	"newest": "Newest opened", "oldest": "Oldest opened",
	"recently-closed": "Recently closed", "oldest-closed": "Oldest closed",
	"working": "Being worked first", "has-mr": "With a request first",
	"label": "Label", "milestone": "Milestone", "author": "Author",
	"loop": "Where it stands in the loop", "ci-failing": "CI failing first",
	"repo-session": "Running sessions first", "repo-name": "Name", "repo-open": "Most open issues", "repo-closed": "Most closed issues",
	"repo-tokens": "Most tokens", "repo-changed": "Recently changed", "repo-mrs": "Failing merge requests first",
	"repo-newest": "Newest repository first", "repo-oldest": "Oldest repository first",
}

// SortLabel is sort s as the owner reads it.
func (p *uiPage) SortLabel(s string) string { return sortLabels[s] }

// uiClosedPage is a repository's closed issues, a list of its own with
// its own sorts and filters, shown in the workspace's issue panel.
type uiClosedPage struct {
	uiPage
	Repository  string
	URL         string
	Src         string
	Rows        []LevelIssue
	OpenCount   int
	ClosedSorts []string
	// Reading is how the listing last went: whether the list is whole.
	Reading issueReading
}

// closedView is repo's closed list, nil when no source configures it.
func (u *UI) closedView(repo string, now time.Time) (*uiClosedPage, error) {
	var v *uiClosedPage
	ok, err := u.withWorkspace(repo, func(src *uiSource, cfg *Config, st *LogState) {
		v = &uiClosedPage{uiPage: uiPage{Title: repo + " closed issues", Token: u.token, Back: "/r/" + url.PathEscape(repo) + "/closed", Now: now, loc: u.loc, Static: true},
			Repository: repo, URL: webURL(cfg.Repositories[repo], 0, 0), Src: src.Name, Rows: closedRows(cfg, st, repo, now),
			OpenCount: openCount(st, repo), ClosedSorts: ClosedSorts, Reading: src.issueReads[repo]}
	})
	if !ok || err != nil {
		return nil, err
	}
	return v, nil
}

// serveIssuePriority records the priority the owner set on an issue.
func (u *UI) serveIssuePriority(w http.ResponseWriter, r *http.Request) {
	if !u.checkPost(w, r) {
		return
	}
	src := u.postSource(w, r)
	if src == nil {
		return
	}
	repo, ref, level := r.PostForm.Get("repository"), r.PostForm.Get("ref"), r.PostForm.Get("level")
	if served, _, ok := u.workspacePick(repo); !ok || served != src {
		http.Error(w, "no such repository", http.StatusBadRequest)
		return
	}
	if err := src.setPriority(repo, ref, level); err != nil {
		http.Error(w, "the priority was not recorded: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
