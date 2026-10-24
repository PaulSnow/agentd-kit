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

const githubNotifications = "/notifications"

// PollConfig turns on agentd's own poll of the forges, per forge: each
// needs that forge's CLI logged in. Every is how often.
type PollConfig struct {
	Every  Duration `json:"every"`
	GitHub bool     `json:"github"`
	GitLab bool     `json:"gitlab"`
}

// PollPayload is one poll of a source that changed something: it found
// items or moved its cursor, or it failed, or it worked again after
// failing. A poll that found nothing new makes no event. Calls is how many
// API calls it made.
type PollPayload struct {
	Source string `json:"source"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
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
// logins: agentd holds no credentials of its own.
type ForgeAPI interface {
	GitHub(path string, header map[string]string) (APIResponse, error)
	GitLab(path string) (APIResponse, error)
}

// ExecForgeAPI calls gh api and glab api, each bounded by Timeout.
type ExecForgeAPI struct{ Timeout time.Duration }

func (a ExecForgeAPI) GitHub(path string, header map[string]string) (APIResponse, error) {
	args := []string{"api", "--include", "--method", "GET", path}
	for k, v := range header {
		args = append(args, "-H", k+": "+v)
	}
	out, err := runTimed(a.Timeout, "", nil, false, "gh", args...)
	// gh exits non-zero on a 304; the reply it printed still says so.
	resp, perr := parseIncluded(out)
	if perr == nil && (err == nil || resp.Status == http.StatusNotModified) {
		return resp, nil
	}
	if err == nil {
		err = perr
	}
	return resp, fmt.Errorf("gh api %s: %w", path, err)
}

func (a ExecForgeAPI) GitLab(path string) (APIResponse, error) {
	out, err := runTimed(a.Timeout, "", nil, false, "glab", "api", "--include", path)
	resp, perr := parseIncluded(out)
	if err == nil {
		err = perr
	}
	if err != nil {
		return resp, fmt.Errorf("glab api %s: %w", path, err)
	}
	return resp, nil
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
	return pollGitLab(s.api, s.st, inv, now)
}

// recordPoll logs what a poll begun at start found: its items, its cursor,
// and a poll record when it changed something or its health changed; and
// when the source is next due.
func (s *Supervisor) recordPoll(src string, r pollResult, start time.Time) {
	wait := s.cfg.Poll.Every.Duration
	if r.wait > wait {
		wait = r.wait
	}
	s.work.mu.Lock()
	s.work.nextPoll[src] = start.Add(wait)
	s.work.mu.Unlock()
	p := PollPayload{Source: src, OK: r.err == nil, Calls: r.calls}
	if r.err != nil {
		p.Error = r.err.Error()
	}
	for _, it := range r.items {
		if _, ok := s.st.Items[it.ID]; ok {
			continue
		}
		it := it
		s.emit("", Event{Kind: KindItem, Mode: ModeShared, Item: &it})
		p.Items++
	}
	moved := r.err == nil && r.cursor.After(s.st.Cursors[src])
	if moved {
		s.emit("", Event{Kind: KindCursor, Mode: ModeShared, Cursor: &CursorPayload{Source: src, AsOf: r.cursor}})
	}
	prev, seen := s.st.Polls[src]
	if p.Items > 0 || moved || !seen || prev.OK != p.OK || prev.Error != p.Error {
		s.emit("", Event{Kind: KindPoll, Poll: &p})
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
		r.err = fmt.Errorf("GitHub notifications: status %d", resp.Status)
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
		if _, held := st.Items[id]; !held {
			r.calls++
			resp, err := api.GitHub(strings.TrimPrefix(n.Subject.URL, "https://api.github.com"), nil)
			if err == nil && resp.Status != http.StatusOK {
				err = fmt.Errorf("status %d", resp.Status)
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
// The new cursor is when the poll began, so what changes while it runs is
// asked for again next time, and found again under the same id. Any call
// that fails makes the whole poll unknown and leaves the cursor, so a
// failure never leaves a gap. The first poll is the baseline: it sets the
// cursor only.
func pollGitLab(api ForgeAPI, st *LogState, inv *Inventory, now time.Time) pollResult {
	since := st.Cursors[SourceGitLab]
	if since.IsZero() {
		return pollResult{cursor: now}
	}
	var r pollResult
	get := func(path string, into any) bool {
		r.calls++
		resp, err := api.GitLab(path)
		if err == nil && resp.Status != http.StatusOK {
			err = fmt.Errorf("GitLab %s: status %d", path, resp.Status)
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
			if !get(fmt.Sprintf("/projects/%s/%s?%s", url.PathEscape(repo.Path), kind, q), &list) {
				return pollResult{calls: r.calls, err: r.err}
			}
			for _, w := range list {
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
	r.cursor = now
	return r
}
