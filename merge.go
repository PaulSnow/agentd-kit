package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Merge when green (#26, #1 decision D2). agentd merges a request only as
// a fast-forward of a gated tree, and only where the repository grants it
// twice: its configuration allows merge, and its own policy file on the
// target branch, read at the target's tip, says merge: fast-forward, so a
// session cannot grant itself a merge from its branch. Each watched
// request is checked as the CI watcher records it; it merges when its
// head is the commit agentd's gate passed and CI succeeded on, no
// blocking decision of its session is open, the head
// is a fast-forward of the target, and the target has not moved since
// that check. Otherwise why is recorded, once per head and reason, and
// nothing is done. A request is merged once; there is no squash, rebase
// or force.

// MergePolicyFile is the repository's own merge policy, read from the
// target branch: {"merge": "fast-forward"} grants a fast-forward merge.
const MergePolicyFile = ".agentd/policy.json"

// MergeFastForward is the one merge a policy file may grant.
const MergeFastForward = "fast-forward"

// sourceMerge is the merge check's entry among the poll worker's due times.
const sourceMerge = "merge"

// mergeKey keys a request's merge, its failures and its refusals.
func mergeKey(repo, ref string) string { return "merge:" + ciKey(repo, ref) }

// mergeGreen checks every watched request a repository allows to merge,
// every poll.ci_every while the CI watcher runs: the CI watcher's records
// are what says a request is green on its head. What the state alone can
// settle is settled here; the forge is asked in a worker per request.
func (s *Supervisor) mergeGreen(now time.Time) {
	if s.api == nil || !s.cfg.Poll.CI {
		return
	}
	s.work.mu.Lock()
	next := s.work.nextPoll[sourceMerge]
	if !now.Before(next) {
		s.work.nextPoll[sourceMerge] = now.Add(s.cfg.Poll.CIEvery.Duration)
	}
	s.work.mu.Unlock()
	if now.Before(next) {
		return
	}
	for _, w := range s.watchedRequests() {
		if !contains(s.cfg.Repositories[w.Repository].Allow, "merge") {
			continue
		}
		t := s.track(w.Session)
		key := ciKey(w.Repository, w.Ref)
		if _, done := t.Merges[key]; done {
			continue
		}
		r, ok := s.st.CI[key]
		code, why := s.mergeBlocked(t, w, r, ok, now)
		if code != "" {
			s.mergeSkip(w.Session, t, w, r.Head, code, why)
			continue
		}
		w, r := w, r
		s.spawn(w.Session, mergeKey(w.Repository, w.Ref), func(ws *Supervisor, t *Track) { ws.mergeRequest(w.Session, t, w, r) })
	}
}

// mergeBlocked is why the state alone says a request may not merge now,
// as a short code and a reason, or "" when the forge is to be asked.
func (s *Supervisor) mergeBlocked(t *Track, w ciWatched, r CIRecord, ok bool, now time.Time) (string, string) {
	switch {
	case !ok || r.Head == "":
		return "no-ci", "" // the CI watcher has not recorded it yet
	case r.Session != w.Session || r.Delivered != w.Delivered || t.ciStale(r):
		return "stale", "" // the watcher records the delivery at its next round
	case r.State == CIPending:
		return "ci-pending", "" // CI is running
	case r.State != CISuccess:
		return "ci-" + r.State, fmt.Sprintf("CI on %s is %s, not green", shortHash(r.Head), r.State)
	case !contains(t.Gated, r.Head):
		return "ungated", fmt.Sprintf("its head %s did not pass agentd's gate", shortHash(r.Head))
	case t.Questions.Open || t.blocking():
		return "decision", "its session has an open blocking decision"
	}
	fkey := mergeKey(w.Repository, w.Ref) + ":" + r.Head
	if s.failed(t, fkey) {
		// A skip, not a decision: the request is still open, for the
		// owner to merge, and its errors are in the log.
		return "gave-up", fmt.Sprintf("the merge call failed %d times on %s; not retrying", len(t.Failures[fkey]), shortHash(r.Head))
	}
	if !s.retryDue(t, fkey, now) {
		return "backoff", ""
	}
	return "", ""
}

// blocking says whether one of the session's blocking decisions waits on
// the owner.
func (t *Track) blocking() bool {
	for _, d := range t.open() {
		if d.Blocking && t.waits(d) {
			return true
		}
	}
	return false
}

// mergeSkip records why a request is not merged, once per head and
// reason. A backoff, whose reason is the failure already logged, is not.
func (s *Supervisor) mergeSkip(name string, t *Track, w ciWatched, head, code, why string) {
	if why == "" {
		return
	}
	s.skipOnce(name, mergeKey(w.Repository, w.Ref)+":"+head+":"+code, fmt.Sprintf("not merging %s in %s: %s", w.Ref, w.Repository, why), t,
		&SkipPayload{Action: ActMerge, Repository: w.Repository, Head: head})
}

// mergeView is what the forge says of a request about to merge: its
// head, target and whether it is open, and how to ask about the target.
type mergeView struct {
	head, target, url string
	open              bool
	held              string // why the forge itself holds it, a draft, or ""
	// tip is the target branch's commit, policy the merge policy file at
	// a commit (nil when there is none), and fastForward why head is not
	// a fast-forward of tip, or "".
	tip         func(target string) (string, error)
	policy      func(sha string) ([]byte, error)
	fastForward func(tip, head string) (string, error)
}

// mergeRequest asks the forge whether the request may merge, and merges it
// through the allow-listed forge client when every check holds.
func (s *Supervisor) mergeRequest(name string, t *Track, w ciWatched, r CIRecord) {
	repo := s.cfg.Repositories[w.Repository]
	api := s.api.At(repo.Host)
	view := func() (mergeView, error) {
		if repo.Forge == "github" {
			return githubMergeView(api, repo.Project, w.Ref)
		}
		return gitlabMergeView(api, repo.Project, w.Ref)
	}
	v, err := view()
	skip := func(code, why string) { s.mergeSkip(name, t, w, r.Head, code, why) }
	unknown := func(err error) { skip("unknown", "the forge could not be asked: "+s.withheld(err.Error())) }
	if err != nil {
		unknown(err)
		return
	}
	if !v.open {
		return // merged or closed: the CI watcher records which
	}
	if v.head != r.Head {
		skip("head-moved:"+v.head, fmt.Sprintf("its head is now %s, not %s, which was gated and green", shortHash(v.head), shortHash(r.Head)))
		return
	}
	if v.held != "" {
		skip("held", v.held)
		return
	}
	tip, err := v.tip(v.target)
	if err != nil {
		unknown(err)
		return
	}
	b, err := v.policy(tip)
	if err != nil {
		unknown(err)
		return
	}
	// Keys are matched exactly: json.Unmarshal into a struct would take
	// "MERGE" too.
	var pol map[string]any
	if b == nil || json.Unmarshal(b, &pol) != nil || pol["merge"] != MergeFastForward {
		skip("no-grant", fmt.Sprintf("%s on %s at %s does not grant merge: %s", MergePolicyFile, v.target, shortHash(tip), MergeFastForward))
		return
	}
	why, err := v.fastForward(tip, v.head)
	if err != nil {
		unknown(err)
		return
	}
	if why != "" {
		skip("not-ff", why)
		return
	}
	// The request and its target are asked again just before the merge:
	// a request retargeted or moved on, or a target that moved since the
	// check, is checked again at the next round.
	again, err := view()
	if err != nil {
		unknown(err)
		return
	}
	if !again.open || again.head != v.head || again.target != v.target || again.held != "" {
		skip("request-changed", "the request changed during the check")
		return
	}
	now, err := v.tip(v.target)
	if err != nil {
		unknown(err)
		return
	}
	if now != tip {
		skip("target-moved", fmt.Sprintf("%s moved from %s to %s since the check", v.target, shortHash(tip), shortHash(now)))
		return
	}
	fkey := mergeKey(w.Repository, w.Ref) + ":" + r.Head
	s.act(name, ActMerge, &ActPayload{Key: fkey, Repository: w.Repository, Ref: w.Ref, URL: v.url, Head: v.head, Target: v.target, SHA: tip}, func() error {
		return s.forge.MergeAllowed(w.Repository, w.Ref, v.head, v.target)
	})
}

// gitlabMergeView asks GitLab about merge request !n. GitLab's merge call
// fast-forwards only under the project's ff merge method; under any other
// it would make a merge commit, and where the project squashes every
// merge it would write a commit nobody gated, so such a project never
// merges. A draft is held.
func gitlabMergeView(api ForgeAPI, project, ref string) (mergeView, error) {
	base := "/projects/" + url.PathEscape(project)
	get := func(path string, v any) error {
		return ciGet(func() (APIResponse, error) { return api.GitLab(path) }, "glab api "+path, v)
	}
	var mr struct {
		State        string `json:"state"`
		WebURL       string `json:"web_url"`
		SHA          string `json:"sha"`
		TargetBranch string `json:"target_branch"`
		Draft        bool   `json:"draft"`
	}
	if err := get(base+"/merge_requests/"+ref[1:], &mr); err != nil {
		return mergeView{}, err
	}
	v := mergeView{head: mr.SHA, target: mr.TargetBranch, url: mr.WebURL, open: mr.State == "opened"}
	if mr.Draft {
		v.held = "it is a draft"
	}
	v.tip = func(target string) (string, error) {
		var b struct {
			Commit struct {
				ID string `json:"id"`
			} `json:"commit"`
		}
		if err := get(base+"/repository/branches/"+url.PathEscape(target), &b); err != nil {
			return "", err
		}
		if b.Commit.ID == "" {
			return "", fmt.Errorf("glab api: branch %s has no commit", target)
		}
		return b.Commit.ID, nil
	}
	v.policy = func(sha string) ([]byte, error) {
		return policyFile(func() (APIResponse, error) {
			return api.GitLab(base + "/repository/files/" + url.PathEscape(MergePolicyFile) + "/raw?ref=" + url.QueryEscape(sha))
		})
	}
	v.fastForward = func(tip, head string) (string, error) {
		var p struct {
			MergeMethod  string `json:"merge_method"`
			SquashOption string `json:"squash_option"`
		}
		if err := get(base, &p); err != nil {
			return "", err
		}
		if p.MergeMethod != "ff" {
			return fmt.Sprintf("the project's merge method is %q, not ff: GitLab would not fast-forward", p.MergeMethod), nil
		}
		// Under always, GitLab squashes whatever the merge call says.
		if p.SquashOption == "always" {
			return "the project squashes every merge: GitLab would not fast-forward the gated commits", nil
		}
		var mb struct {
			ID string `json:"id"`
		}
		if err := get(base+"/repository/merge_base?refs%5B%5D="+url.QueryEscape(tip)+"&refs%5B%5D="+url.QueryEscape(head), &mb); err != nil {
			return "", err
		}
		if mb.ID != tip {
			return fmt.Sprintf("%s is not a fast-forward of %s", shortHash(head), shortHash(tip)), nil
		}
		return "", nil
	}
	return v, nil
}

// githubMergeView is gitlabMergeView for GitHub, where the merge is a
// move of the target branch without force: a fast-forward or nothing.
func githubMergeView(api ForgeAPI, project, ref string) (mergeView, error) {
	base := "/repos/" + project
	get := func(path string, v any) error {
		return ciGet(func() (APIResponse, error) { return api.GitHub(path, nil) }, "gh api "+path, v)
	}
	var pr struct {
		State          string `json:"state"`
		Merged         bool   `json:"merged"`
		Draft          bool   `json:"draft"`
		MergeableState string `json:"mergeable_state"`
		HTMLURL        string `json:"html_url"`
		Head           struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := get(base+"/pulls/"+ref[1:], &pr); err != nil {
		return mergeView{}, err
	}
	v := mergeView{head: pr.Head.SHA, target: pr.Base.Ref, url: pr.HTMLURL, open: pr.State == "open" && !pr.Merged}
	// Moving the branch skips what GitHub's own merge would ask, so only a
	// request GitHub says it would merge as it stands is not held: clean,
	// or with hooks, or unstable (a check that is not required fails).
	// A state GitHub has not computed yet, unknown after every push, is
	// held until a later round finds it computed.
	switch pr.MergeableState {
	case "clean", "has_hooks", "unstable":
	default:
		v.held = fmt.Sprintf("GitHub says it is %q, not clean", pr.MergeableState)
	}
	if pr.Draft {
		v.held = "it is a draft"
	}
	v.tip = func(target string) (string, error) {
		var r struct {
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if err := get(base+"/git/ref/heads/"+escapeRef(target), &r); err != nil {
			return "", err
		}
		if r.Object.SHA == "" {
			return "", fmt.Errorf("gh api: branch %s has no commit", target)
		}
		return r.Object.SHA, nil
	}
	v.policy = func(sha string) ([]byte, error) {
		return policyFile(func() (APIResponse, error) {
			return api.GitHub(base+"/contents/"+MergePolicyFile+"?ref="+url.QueryEscape(sha), map[string]string{"Accept": "application/vnd.github.raw+json"})
		})
	}
	v.fastForward = func(tip, head string) (string, error) {
		var c struct {
			Status   string `json:"status"`
			BehindBy int    `json:"behind_by"`
		}
		if err := get(base+"/compare/"+tip+"..."+head, &c); err != nil {
			return "", err
		}
		if c.Status != "ahead" || c.BehindBy != 0 {
			return fmt.Sprintf("%s is %s of %s, not a fast-forward", shortHash(head), c.Status, shortHash(tip)), nil
		}
		return "", nil
	}
	return v, nil
}

// escapeRef escapes each segment of a branch name for a GitHub path, so a
// # or % in it names the branch, not a fragment or another ref.
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// policyFile reads the merge policy file: nil when the target has none.
func policyFile(call func() (APIResponse, error)) ([]byte, error) {
	resp, err := call()
	if err != nil {
		return nil, err
	}
	switch resp.Status {
	case http.StatusOK:
		if resp.Body == nil {
			return []byte{}, nil
		}
		return resp.Body, nil
	case http.StatusNotFound:
		return nil, nil
	}
	return nil, statusError(MergePolicyFile, resp)
}
