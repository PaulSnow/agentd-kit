package main

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// Merge requests on the owner's pages (#94): each repository's open merge
// or pull requests, read from its forge with its issues, each with what its
// CI says of its head commit: passing, failing, running, or none. A
// request's CI is asked again only when its head moved or its CI had not
// passed or failed. They are kept in memory: a restart reads them again at
// its first poll.

// ListedRequest is one open request as the pages show it.
type ListedRequest struct {
	Ref      string // !12 on GitLab, #12 on GitHub
	Title    string
	URL      string
	Author   string
	Draft    bool
	Head     string
	Opened   time.Time
	Changed  time.Time
	CI       string // CISuccess, CIFailed, CIPending, CINone or CIUnknown
	Withheld bool   // the title matched a confidential pattern
}

// CIText says what the request's CI is, in a word or two.
func (r ListedRequest) CIText() string {
	switch r.CI {
	case CISuccess:
		return "CI passing"
	case CIFailed:
		return "CI failing"
	case CIPending:
		return "CI running"
	case CINone:
		return "no CI"
	}
	return "CI unknown"
}

// requestReading is a repository's open requests as last read.
type requestReading struct {
	Read  time.Time // when a read last succeeded
	Error string    // why the last read failed, "" when it did not
	List  []ListedRequest
}

// counts is how many of the requests pass and fail CI, and how many are
// neither.
func (r requestReading) counts() (pass, fail, other int) {
	for _, q := range r.List {
		switch q.CI {
		case CISuccess:
			pass++
		case CIFailed:
			fail++
		default:
			other++
		}
	}
	return pass, fail, other
}

// listRequests lists r's open requests, at most 100, without their CI.
func listRequests(api ForgeAPI, r Repository) ([]ListedRequest, error) {
	var out []ListedRequest
	if r.Forge == "gitlab" {
		var mrs []struct {
			IID       int       `json:"iid"`
			Title     string    `json:"title"`
			WebURL    string    `json:"web_url"`
			Draft     bool      `json:"draft"`
			SHA       string    `json:"sha"`
			CreatedAt time.Time `json:"created_at"`
			UpdatedAt time.Time `json:"updated_at"`
			Author    struct {
				Username string `json:"username"`
			} `json:"author"`
		}
		path := fmt.Sprintf("/projects/%s/merge_requests?state=opened&per_page=100", url.PathEscape(r.Project))
		resp, err := api.GitLab(path)
		if err := getJSON(resp, err, "merge requests", &mrs); err != nil {
			return nil, err
		}
		for _, m := range mrs {
			out = append(out, ListedRequest{Ref: "!" + strconv.Itoa(m.IID), Title: m.Title, URL: m.WebURL, Author: m.Author.Username,
				Draft: m.Draft, Head: m.SHA, Opened: m.CreatedAt, Changed: m.UpdatedAt})
		}
		return out, nil
	}
	var prs []struct {
		Number    int       `json:"number"`
		Title     string    `json:"title"`
		HTMLURL   string    `json:"html_url"`
		Draft     bool      `json:"draft"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	resp, err := api.GitHub(fmt.Sprintf("/repos/%s/pulls?state=open&per_page=100", r.Project), nil)
	if err := getJSON(resp, err, "pull requests", &prs); err != nil {
		return nil, err
	}
	for _, p := range prs {
		out = append(out, ListedRequest{Ref: "#" + strconv.Itoa(p.Number), Title: p.Title, URL: p.HTMLURL, Author: p.User.Login,
			Draft: p.Draft, Head: p.Head.SHA, Opened: p.CreatedAt, Changed: p.UpdatedAt})
	}
	return out, nil
}

// requestCI is what the forge says of q's CI: on GitLab its head pipeline;
// on GitHub its head commit's check runs, failing when any failed and
// running when any has not finished, or with none its combined status. A
// forge that could not be asked is unknown.
func requestCI(api ForgeAPI, r Repository, q ListedRequest) string {
	if r.Forge == "gitlab" {
		var mr struct {
			HeadPipeline *struct {
				Status string `json:"status"`
			} `json:"head_pipeline"`
		}
		resp, err := api.GitLab(fmt.Sprintf("/projects/%s/merge_requests/%s", url.PathEscape(r.Project), q.Ref[1:]))
		if getJSON(resp, err, "a merge request", &mr) != nil {
			return CIUnknown
		}
		if mr.HeadPipeline == nil {
			return CINone
		}
		return gitlabState(mr.HeadPipeline.Status)
	}
	if q.Head == "" {
		return CIUnknown
	}
	var runs struct {
		Total int `json:"total_count"`
		Runs  []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	resp, err := api.GitHub(fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=100", r.Project, q.Head), nil)
	if getJSON(resp, err, "check runs", &runs) != nil {
		return CIUnknown
	}
	if runs.Total == 0 {
		var st struct {
			State string `json:"state"`
			Total int    `json:"total_count"`
		}
		resp, err := api.GitHub(fmt.Sprintf("/repos/%s/commits/%s/status", r.Project, q.Head), nil)
		if getJSON(resp, err, "statuses", &st) != nil {
			return CIUnknown
		}
		switch {
		case st.Total == 0:
			return CINone
		case st.State == "success":
			return CISuccess
		case st.State == "pending":
			return CIPending
		}
		return CIFailed
	}
	state := CISuccess
	for _, run := range runs.Runs {
		switch {
		case run.Status != "completed":
			state = CIPending
		case !greenConclusions[run.Conclusion]:
			return CIFailed
		}
	}
	if len(runs.Runs) < runs.Total && state == CISuccess {
		return CIPending // more runs than were read: not known to pass
	}
	return state
}

// readRequests brings repo's open requests up to date. A failed read keeps
// the list it had and says why.
func (src *uiSource) readRequests(api ForgeAPI, repo string, r Repository) {
	src.mu.Lock()
	secret := confidentialMatcher(src.cfg)
	prev := map[string]ListedRequest{}
	for _, q := range src.requests[repo].List {
		prev[q.Ref] = q
	}
	src.mu.Unlock()
	api = api.At(r.Host)
	list, err := listRequests(api, r)
	if err != nil {
		src.mu.Lock()
		k := src.requests[repo]
		k.Error = oneLine(err.Error(), 300)
		src.setRequests(repo, k)
		src.mu.Unlock()
		return
	}
	for i := range list {
		q := &list[i]
		if o, ok := prev[q.Ref]; ok && o.Head == q.Head && q.Head != "" && (o.CI == CISuccess || o.CI == CIFailed) {
			q.CI = o.CI
		} else {
			q.CI = requestCI(api, r, *q)
		}
		q.Title = cutUTF8(q.Title, indexTitle)
		if secret(q.Title, q.Author) {
			q.Title, q.Author, q.Withheld = "", "", true
		}
	}
	sort.SliceStable(list, func(a, b int) bool { return list[a].Changed.After(list[b].Changed) })
	src.mu.Lock()
	src.setRequests(repo, requestReading{Read: src.clock(), List: list})
	src.mu.Unlock()
}

// setRequests records repo's reading; src.mu is held.
func (src *uiSource) setRequests(repo string, k requestReading) {
	if src.requests == nil {
		src.requests = map[string]requestReading{}
	}
	src.requests[repo] = k
}
