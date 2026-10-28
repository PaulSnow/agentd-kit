package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// agentd asks the forges itself for what changed, every Poll.Every, in a
// worker: GitHub's notifications, and each active GitLab repository's
// issues and merge requests with the owner's to-do list. What it finds
// becomes items (#18's), with the same ids the tracking snapshot gives, so
// an issue both find is one item. Each source's cursor is an event, so a
// restart neither misses nor repeats; a source that fails is recorded and
// shown as unknown, and its cursor stays where it was.

// DefaultPollEvery is how often the forges are asked when poll.every is
// unset: every 15 minutes, the owner's decision.
const DefaultPollEvery = 15 * time.Minute

// Poll sources, as their cursors and poll records name them.
const (
	SourceGitHub = "github"
	SourceGitLab = "gitlab"
)

// githubNotifications asks for read notifications too, since the owner
// may read one on the site before agentd polls, and for a full page.
const githubNotifications = "/notifications?all=true&per_page=100"

// pollHeartbeat is the longest a source goes without a poll record while
// it is polled: quiet polls are tallied into the next record, so status
// can tell a source still polled from one no longer polled, and the calls
// of every poll are counted.
const pollHeartbeat = 6 * time.Hour

// pollOverlap is how far before the poll began the GitLab cursor is set,
// so a clock behind GitLab's misses nothing; what the overlap finds again
// has the same id.
const pollOverlap = 2 * time.Minute

// gitlabPages is how many pages of 100 one GitLab list may take.
const gitlabPages = 10

// PollConfig turns on agentd's own poll of the forges, per forge: each
// needs that forge's CLI logged in. Every is how often.
type PollConfig struct {
	Every  Duration `json:"every"`
	GitHub bool     `json:"github"`
	GitLab bool     `json:"gitlab"`
	// CI turns on the CI watcher (#25): each delivered request's pipeline
	// is asked about every CIEvery.
	CI      bool     `json:"ci"`
	CIEvery Duration `json:"ci_every"`
	// Index turns on the issue index (#69): each forge repository's open
	// issues and requests are listed every Every.
	Index bool `json:"index"`
}

// PollPayload is a poll of a source that changed something: it found
// items or moved its cursor, or it failed, or it worked again after
// failing; or the heartbeat after pollHeartbeat of quiet polls. A quiet
// poll makes no event of its own: Polls counts it into the next record,
// and Calls is the API calls of those polls.
type PollPayload struct {
	Source string `json:"source"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Polls  int    `json:"polls"`
	Calls  int    `json:"calls"`
	Items  int    `json:"items"`
}

// PollRecord is a source's latest poll record, and when.
type PollRecord struct {
	PollPayload
	At time.Time
}

// APIResponse is one forge API reply: its status, headers and body.
type APIResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// ForgeAPI makes read-only forge API calls through the forge CLIs' own
// logins: agentd holds no credentials of its own. At is the same API on
// a repository's host, "" for the CLI's own.
type ForgeAPI interface {
	GitHub(path string, header map[string]string) (APIResponse, error)
	GitLab(path string) (APIResponse, error)
	At(host string) ForgeAPI
}

// ExecForgeAPI calls gh api and glab api, each bounded by Timeout, on Host
// when it is set.
type ExecForgeAPI struct {
	Timeout time.Duration
	Host    string
}

func (a ExecForgeAPI) At(host string) ForgeAPI { a.Host = host; return a }

func (a ExecForgeAPI) GitHub(path string, header map[string]string) (APIResponse, error) {
	args := []string{"api", "--include", "--method", "GET", path}
	if a.Host != "" {
		args = append(args, "--hostname", a.Host)
	}
	for k, v := range header {
		args = append(args, "-H", k+": "+v)
	}
	out, err := runTimed(a.Timeout, "", nil, false, "gh", args...)
	return included("gh api "+path, out, err)
}

func (a ExecForgeAPI) GitLab(path string) (APIResponse, error) {
	args := []string{"api", "--include", path}
	if a.Host != "" {
		args = append(args, "--hostname", a.Host)
	}
	out, err := runTimed(a.Timeout, "", nil, false, "glab", args...)
	return included("glab api "+path, out, err)
}

// included is the reply a CLI printed. The CLIs exit non-zero on a 304 or
// an error status, and the reply they printed still says which, so the
// caller reports its status; with no reply, the error carries what the CLI
// said on stderr.
func included(what string, out []byte, err error) (APIResponse, error) {
	resp, perr := parseIncluded(out)
	if perr == nil {
		return resp, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
		err = fmt.Errorf("%w: %s", err, bytes.TrimSpace(ee.Stderr))
	}
	if err == nil {
		err = perr
	}
	return resp, fmt.Errorf("%s: %w", what, err)
}

// statusError is a reply that is neither 200 nor 304: its status and the
// start of what the forge said.
func statusError(what string, resp APIResponse) error {
	msg := strings.Join(strings.Fields(string(resp.Body)), " ")
	if len(msg) > 120 {
		msg = msg[:120] + "..."
	}
	return fmt.Errorf("%s: status %d %s", what, resp.Status, msg)
}

// parseIncluded reads a reply printed with --include: the status line,
// the headers, a blank line, then the body.
func parseIncluded(out []byte) (APIResponse, error) {
	r := bufio.NewReader(bytes.NewReader(out))
	line, err := r.ReadString('\n')
	if err != nil {
		return APIResponse{}, errors.New("no status line in the reply")
	}
	f := strings.Fields(line)
	if len(f) < 2 {
		return APIResponse{}, fmt.Errorf("status line %q", strings.TrimSpace(line))
	}
	status, err := strconv.Atoi(f[1])
	if err != nil {
		return APIResponse{}, fmt.Errorf("status line %q", strings.TrimSpace(line))
	}
	mh, err := textproto.NewReader(r).ReadMIMEHeader()
	if err != nil && len(mh) == 0 && status != http.StatusNotModified {
		return APIResponse{}, fmt.Errorf("headers: %w", err)
	}
	var body bytes.Buffer
	body.ReadFrom(r)
	return APIResponse{Status: status, Header: http.Header(mh), Body: body.Bytes()}, nil
}

// pollResult is what one poll of one source found: its items, its new
// cursor (zero when it did not move), the least wait before the next poll
// the forge asks for, the calls made, and the error that made it unknown.
type pollResult struct {
	items  []ItemPayload
	cursor time.Time
	wait   time.Duration
	calls  int
	err    error
}

// pollForges starts the poll of each source that is due, in a worker. A
// source's next poll is due Every after its last, or later when the forge
// asks for a longer interval; a restarted agentd polls at once.
func (s *Supervisor) pollForges(now time.Time) {
	if s.api == nil {
		return
	}
	for _, src := range []string{SourceGitHub, SourceGitLab} {
		s.work.mu.Lock()
		next := s.work.nextPoll[src]
		s.work.mu.Unlock()
		if src == SourceGitHub && !s.cfg.Poll.GitHub || src == SourceGitLab && !s.cfg.Poll.GitLab || now.Before(next) {
			continue
		}
		src := src
		s.spawnShared("poll:"+src, func(w *Supervisor) {
			r := w.pollSource(src, now)
			w.recordPoll(src, r, now)
		})
	}
}

// pollSource polls one source. The tracking manifest is the list of
// repositories: without it a source is unknown.
func (s *Supervisor) pollSource(src string, now time.Time) pollResult {
	inv, err := LoadInventory(s.cfg.Tracking)
	if err != nil {
		return pollResult{err: err}
	}
	if src == SourceGitHub {
		return pollGitHub(s.api, s.st, inv)
	}
	s.work.mu.Lock()
	since := s.work.liveCursor[src]
	s.work.mu.Unlock()
	if c := s.st.Cursors[src]; c.IsZero() || c.After(since) {
		since = c
	}
	return pollGitLab(s.api, since, inv, now)
}

// recordPoll logs what a poll begun at start found: its items and its
// cursor, and a poll record when it changed something, its health changed
// or the heartbeat is due, tallying the quiet polls since the last; and
// when the source is next due. A poll that failed writes no items, even
// those it found before the failure: its cursor stays, so the next poll
// finds them again. The GitHub cursor is Last-Modified and moves whenever
// GitHub sends a new one. The GitLab cursor moves with every poll that
// worked, but in memory: its event is written only with a record, so quiet
// polls write nothing, and a restart asks again from the last record's.
// The tally is in memory too: a restart loses the quiet polls since the
// last record from the count.
func (s *Supervisor) recordPoll(src string, r pollResult, start time.Time) {
	wait := s.cfg.Poll.Every.Duration
	if r.wait > wait {
		wait = r.wait
	}
	s.work.mu.Lock()
	s.work.nextPoll[src] = start.Add(wait)
	p := s.work.pollTally[src]
	p.Polls++
	p.Calls += r.calls
	s.work.pollTally[src] = p
	s.work.mu.Unlock()
	p.Source, p.OK = src, r.err == nil
	if r.err != nil {
		// Screened here, not in emit, so a withheld error compares equal
		// from one poll to the next.
		p.Error = s.withheld(r.err.Error())
	}
	for _, it := range r.items {
		if !s.st.itemChanged(it) || r.err != nil {
			continue
		}
		it := it
		s.emit("", Event{Kind: KindItem, Mode: ModeShared, Item: &it})
		p.Items++
	}
	prev, seen := s.st.Polls[src]
	record := p.Items > 0 || !seen || prev.OK != p.OK || prev.Error != p.Error || start.Sub(prev.At) >= pollHeartbeat
	if r.err == nil && !r.cursor.IsZero() {
		s.work.mu.Lock()
		s.work.liveCursor[src] = r.cursor
		s.work.mu.Unlock()
	}
	moved := r.err == nil && r.cursor.After(s.st.Cursors[src]) && (src == SourceGitHub || record || s.st.Cursors[src].IsZero())
	if moved {
		s.emit("", Event{Kind: KindCursor, Mode: ModeShared, Cursor: &CursorPayload{Source: src, AsOf: r.cursor}})
	}
	if record || moved {
		s.emit("", Event{Kind: KindPoll, Poll: &p})
		s.work.mu.Lock()
		delete(s.work.pollTally, src)
		s.work.mu.Unlock()
	}
}

// githubSubject is the part of a GitHub issue or pull request a new
// notification needs: when it was opened, and by whom.
type githubSubject struct {
	CreatedAt time.Time `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

// pollGitHub asks for the owner's notifications changed since the cursor,
// GitHub's Last-Modified of the last poll. A 304 means nothing changed: it
// costs no rate limit and makes no event. A notification of an issue or
// pull request is a discussion of it, unless st does not hold the issue
// yet and it was opened since the cursor: then it is a new issue or
// request, which asks GitHub for its opening time. The first poll is the
// baseline: it sets the cursor only.
func pollGitHub(api ForgeAPI, st *LogState, inv *Inventory) pollResult {
	since := st.Cursors[SourceGitHub]
	header := map[string]string{}
	if !since.IsZero() {
		header["If-Modified-Since"] = since.UTC().Format(http.TimeFormat)
	}
	r := pollResult{calls: 1}
	resp, err := api.GitHub(githubNotifications, header)
	if err != nil {
		r.err = err
		return r
	}
	if n, err := strconv.Atoi(resp.Header.Get("X-Poll-Interval")); err == nil {
		r.wait = time.Duration(n) * time.Second
	}
	switch resp.Status {
	case http.StatusNotModified:
		return r
	case http.StatusOK:
	default:
		r.err = statusError("GitHub notifications", resp)
		return r
	}
	cursor, err := http.ParseTime(resp.Header.Get("Last-Modified"))
	if err != nil {
		r.err = fmt.Errorf("GitHub notifications: Last-Modified %q: %w", resp.Header.Get("Last-Modified"), err)
		return r
	}
	if since.IsZero() {
		r.cursor = cursor
		return r
	}
	var notes []struct {
		UpdatedAt time.Time `json:"updated_at"`
		Subject   struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Type  string `json:"type"`
		} `json:"subject"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(resp.Body, &notes); err != nil {
		r.err = fmt.Errorf("GitHub notifications: %w", err)
		return r
	}
	for _, n := range notes {
		mr := n.Subject.Type == "PullRequest"
		if !mr && n.Subject.Type != "Issue" || !n.UpdatedAt.After(since) {
			continue
		}
		key := repoKey("github", n.Repository.FullName)
		num, err := strconv.Atoi(n.Subject.URL[strings.LastIndexByte(n.Subject.URL, '/')+1:])
		if inv.byKey[key] == nil || err != nil {
			continue // the manifest is the list
		}
		sigil, kind := "#", ItemIssue
		if mr {
			sigil, kind = "!", ItemMergeRequest
		}
		id := fmt.Sprintf("%s%s%d", key, sigil, num)
		it := ItemPayload{ID: id, Kind: kind, Repository: key, Title: n.Subject.Title, Link: itemLink("github", n.Repository.FullName, num, mr)}
		var sub githubSubject
		if held, ok := st.Items[id]; ok {
			if held.SourceTime.After(since) {
				continue // opened since the cursor: already its new item
			}
		} else {
			r.calls++
			resp, err := api.GitHub(strings.TrimPrefix(n.Subject.URL, "https://api.github.com"), nil)
			if err == nil && resp.Status != http.StatusOK {
				err = statusError("status", resp)
			}
			if err == nil {
				err = json.Unmarshal(resp.Body, &sub)
			}
			if err != nil {
				r.err = fmt.Errorf("GitHub %s: %w", id, err)
				return r
			}
		}
		if sub.CreatedAt.After(since) {
			it.SourceTime, it.Author = sub.CreatedAt, sub.User.Login
		} else {
			it.ID, it.Kind, it.SourceTime = id+"@"+n.UpdatedAt.UTC().Format(time.RFC3339), ItemDiscussion, n.UpdatedAt
		}
		r.items = append(r.items, it)
	}
	r.cursor = cursor
	return r
}

// gitlabWork is an issue or merge request as GitLab lists it.
type gitlabWork struct {
	IID       int       `json:"iid"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Author    struct {
		Username string `json:"username"`
	} `json:"author"`
}

// gitlabItem is the item for an issue or request seen since the cursor: new
// when it was opened since, otherwise a discussion of it.
func gitlabItem(key, path string, w gitlabWork, mr bool, since time.Time) ItemPayload {
	sigil, kind := "#", ItemIssue
	if mr {
		sigil, kind = "!", ItemMergeRequest
	}
	id := fmt.Sprintf("%s%s%d", key, sigil, w.IID)
	it := ItemPayload{ID: id, Kind: kind, Repository: key, Title: w.Title, Link: itemLink("gitlab", path, w.IID, mr), SourceTime: w.CreatedAt, Author: w.Author.Username}
	if !w.CreatedAt.After(since) {
		it.ID, it.Kind, it.SourceTime, it.Author = id+"@"+w.UpdatedAt.UTC().Format(time.RFC3339), ItemDiscussion, w.UpdatedAt, ""
	}
	return it
}

// pollGitLab asks each active GitLab repository of the manifest for the
// issues and merge requests updated since the cursor, and the owner's
// pending to-do list for to-dos made since, on any manifest repository.
// The new cursor is pollOverlap before the poll began, so what changes
// while it runs, or in a skew between the clocks, is asked for again next
// time, and found again under the same id. Any call
// that fails makes the whole poll unknown and leaves the cursor, so a
// failure never leaves a gap. Each list is read page by page, up to
// gitlabPages pages; a list longer than that makes the poll unknown rather
// than leave out what is past it. The first poll is the baseline: it sets
// the cursor only.
func pollGitLab(api ForgeAPI, since time.Time, inv *Inventory, now time.Time) pollResult {
	cursor := now.Add(-pollOverlap)
	if since.IsZero() {
		return pollResult{cursor: cursor}
	}
	var r pollResult
	get := func(path string, into any) bool {
		r.calls++
		resp, err := api.GitLab(path)
		if err == nil && resp.Status != http.StatusOK {
			err = statusError("GitLab "+path, resp)
		}
		if err == nil {
			if err = json.Unmarshal(resp.Body, into); err != nil {
				err = fmt.Errorf("GitLab %s: %w", path, err)
			}
		}
		r.err = err
		return err == nil
	}
	q := url.Values{"updated_after": {since.UTC().Format(time.RFC3339)}, "per_page": {"100"}}.Encode()
	for _, repo := range inv.Repos {
		if repo.Platform != "gitlab" || !repo.Active {
			continue
		}
		for _, kind := range []string{"issues", "merge_requests"} {
			var list []gitlabWork
			for page := 1; ; page++ {
				if page > gitlabPages {
					return pollResult{calls: r.calls, err: fmt.Errorf("GitLab %s %s: more than %d updated since %s", repo.Key, kind, gitlabPages*100, since.UTC().Format(time.RFC3339))}
				}
				var one []gitlabWork
				if !get(fmt.Sprintf("/projects/%s/%s?%s&page=%d", url.PathEscape(repo.Path), kind, q, page), &one) {
					return pollResult{calls: r.calls, err: r.err}
				}
				list = append(list, one...)
				if len(one) < 100 {
					break
				}
			}
			for _, w := range list {
				if !w.UpdatedAt.After(since) {
					continue // the overlap again: nothing since
				}
				r.items = append(r.items, gitlabItem(repo.Key, repo.Path, w, kind == "merge_requests", since))
			}
		}
	}
	var todos []struct {
		CreatedAt  time.Time `json:"created_at"`
		TargetType string    `json:"target_type"`
		Author     struct {
			Username string `json:"username"`
		} `json:"author"`
		Project struct {
			Path string `json:"path_with_namespace"`
		} `json:"project"`
		Target gitlabWork `json:"target"`
	}
	if !get("/todos?state=pending&per_page=100", &todos) {
		return pollResult{calls: r.calls, err: r.err}
	}
	for _, td := range todos {
		key := repoKey("gitlab", td.Project.Path)
		mr := td.TargetType == "MergeRequest"
		if inv.byKey[key] == nil || !mr && td.TargetType != "Issue" || !td.CreatedAt.After(since) {
			continue
		}
		it := gitlabItem(key, td.Project.Path, td.Target, mr, since)
		if it.Kind == ItemDiscussion {
			it.Author = td.Author.Username
		}
		r.items = append(r.items, it)
	}
	r.cursor = cursor
	return r
}
