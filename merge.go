package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Merge when green (#26, #1 decision D2, #35). agentd merges a request
// where its repository's policy enables merge (policy.go), narrowed by the
// repository's own policy file on the target branch, read at the target's
// tip, so a session cannot widen it from its branch. Each watched request
// is checked as the CI watcher records it; it merges when it targets one
// of the policy's targets, its author is one of its authors, it is not
// excluded, each named approver approved it, what the policy requires
// holds at its head (agentd's gate passed on it, CI succeeded on it, its
// review rounds converged), no blocking decision of its session is open,
// and, for a fast-forward, the head is a fast-forward of the target. The
// target must not have moved since that check. Otherwise why is recorded,
// once per head and reason, and nothing is done. A request is merged once,
// by the policy's method; there is no rebase or force.

// MergePolicyFile is the repository's own policy, read from the target
// branch: rules that narrow the owner's (policy.go).
const MergePolicyFile = ".agentd/policy.json"

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
	delivered := s.watchedRequests()
	outside := s.outsideRequests(delivered)
	for i, w := range append(delivered, outside...) {
		// The owner's policy decides what is worth asking the forge; the
		// repository's file, which may narrow it, is read at the target's
		// tip in the worker, every time, so a file that narrowed it once
		// does not hold it after it changes.
		pol := s.cfg.Repositories[w.Repository].Policy
		if !pol.Merge.Enabled {
			continue
		}
		t := s.track(w.Session)
		key := ciKey(w.Repository, w.Ref)
		if _, done := t.Merges[key]; done {
			continue
		}
		r, ok := s.st.CI[key]
		out := i >= len(delivered)
		// A request no session delivered is reviewed by its coordinator's
		// approve at its head; a delivered one by its case's rounds too.
		reviewed := ok && r.Head != "" && s.st.approvedAt(w.Repository, w.Ref) == r.Head || !out && s.reviewConverged(w.Session)
		require := requireOf(pol, pol.Merge.Require, out)
		if looped(s.cfg.Repositories[w.Repository]) && !contains(require, RequireReview) {
			// agentd reviews every request of a repository it runs the
			// loop for, and merges none before its rounds converge (#53).
			require = append(append([]string(nil), require...), RequireReview)
		}
		code, why := s.mergeBlocked(t, w, r, ok, require, reviewed, out, now)
		if code != "" {
			s.mergeSkip(w.Session, t, w, r.Head, code, why)
			continue
		}
		w, r := w, r
		s.spawn(w.Session, mergeKey(w.Repository, w.Ref), func(ws *Supervisor, t *Track) { ws.mergeRequest(w.Session, t, w, r, reviewed, out) })
	}
}

// requireOf is what must hold at a request's head before it merges: the
// policy's require for a request a session delivered. agentd's gate never
// ran on a request no session delivered, so for one of those green CI and
// its coordinator's approval are required instead, whatever require says.
// With ci.required, green CI is required of every request.
func requireOf(pol RepoPolicy, require []string, outside bool) []string {
	if outside {
		return []string{RequireCI, RequireReview}
	}
	if pol.CI.Required && !contains(require, RequireCI) {
		return append(append([]string(nil), require...), RequireCI)
	}
	return require
}

// reviewConverged says whether the case the session works stopped its
// review rounds converged.
func (s *Supervisor) reviewConverged(name string) bool {
	c := s.st.Cases[s.cfg.caseOf(s.cfg.Sessions[name])]
	return c != nil && c.RoundStop != nil && c.RoundStop.Reason == RoundsConverged
}

// mergeBlocked is why the state alone says a request may not merge now,
// as a short code and a reason, or "" when the forge is to be asked.
// Outside says no session delivered it: its coordinator's decisions are
// not about it.
func (s *Supervisor) mergeBlocked(t *Track, w ciWatched, r CIRecord, ok bool, require []string, reviewed, outside bool, now time.Time) (string, string) {
	switch {
	case !ok || r.Head == "":
		return "no-ci", "" // the CI watcher has not recorded it yet: it knows the head
	case r.Session != w.Session || r.Delivered != w.Delivered || t.ciStale(r):
		return "stale", "" // the watcher records the delivery at its next round
	case r.Draft:
		return "draft", "it is a draft"
	}
	if code, why := missing(t, r, require, reviewed); code != "" {
		return code, why
	}
	if r.Unresolved {
		return "unresolved", "it has an open discussion"
	}
	if !outside && (t.Questions.Open || t.blocking()) {
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

// missing is what require names that does not hold at the request's
// head, as a short code and a reason, or "".
func missing(t *Track, r CIRecord, require []string, reviewed bool) (string, string) {
	switch {
	case contains(require, RequireCI) && r.State == CIPending:
		return "ci-pending", "" // CI is running
	case contains(require, RequireCI) && r.State != CISuccess:
		return "ci-" + r.State, fmt.Sprintf("CI on %s is %s, not green", shortHash(r.Head), r.State)
	case contains(require, RequireGate) && !contains(t.Gated, r.Head):
		return "ungated", fmt.Sprintf("its head %s did not pass agentd's gate", shortHash(r.Head))
	case contains(require, RequireReview) && !reviewed:
		return "unreviewed", fmt.Sprintf("neither its case's review rounds converged nor its coordinator approved it at %s", shortHash(r.Head))
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
	author            string
	labels            []string
	unresolved        bool // an open discussion, read as the CI watcher reads it
	// tip is the target branch's commit, policy the repository's policy
	// file at a commit (nil when there is none), fastForward why head is
	// not a fast-forward of tip, or "", and approvers who approved the
	// request at head.
	tip         func(target string) (string, error)
	policy      func(sha string) ([]byte, error)
	fastForward func(tip, head string) (string, error)
	approvers   func(head string) ([]string, error)
}

// mergeRequest asks the forge whether the request may merge, and merges it
// through the allow-listed forge client when every check holds.
func (s *Supervisor) mergeRequest(name string, t *Track, w ciWatched, r CIRecord, reviewed, outside bool) {
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
	if v.unresolved {
		skip("unresolved", "it has an open discussion")
		return
	}
	// A target outside the owner's is not merged into, and its file is
	// not read: a session could point its request at a branch it writes.
	if owner := s.cfg.Repositories[w.Repository].Policy.Merge; !contains(owner.Targets, v.target) {
		skip("target", fmt.Sprintf("its target %s is not one the policy merges into: %s", v.target, strings.Join(owner.Targets, ", ")))
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
	pol := s.notePolicy(name, t, w.Repository, v.target, tip, b)
	if s.halt != nil {
		return
	}
	m := pol.Merge
	number, _ := strconv.Atoi(w.Ref[1:])
	switch {
	case !m.Enabled:
		skip("disabled", fmt.Sprintf("%s on %s at %s turns merge off", MergePolicyFile, v.target, shortHash(tip)))
		return
	case !contains(m.Targets, v.target):
		skip("target", fmt.Sprintf("its target %s is not one the policy merges into: %s", v.target, strings.Join(m.Targets, ", ")))
		return
	case !contains(m.Authors, v.author):
		skip("author", fmt.Sprintf("its author %s is not one whose requests the policy merges", s.withheld(v.author)))
		return
	case m.Exclude.matches(number, v.labels):
		skip("excluded", "the policy excludes it, by its number or a label")
		return
	case pol.Scope.Reserved.matches(0, v.labels):
		skip("reserved", "it carries a label the policy reserves")
		return
	}
	if code, why := missing(t, r, requireOf(pol, m.Require, outside), reviewed); code != "" {
		skip(code, why)
		return
	}
	if len(m.Approvals) > 0 {
		approved, err := v.approvers(v.head)
		if err != nil {
			unknown(err)
			return
		}
		for _, a := range m.Approvals {
			if !contains(approved, a) {
				skip("approval", fmt.Sprintf("%s has not approved it at %s", a, shortHash(v.head)))
				return
			}
		}
	}
	// A green run judged the head against the base it ran on; once the
	// target moved on, the merge result is tested again first, once per
	// head and tip. A base that could not be asked counts as moved.
	if pol.CI.RetestOnBaseMove && r.Base != "" && r.Base != tip {
		moved := fmt.Sprintf("%s is at %s, not %s, the base CI ran on for %s", v.target, shortHash(tip), shortHash(r.Base), shortHash(v.head))
		if repo.Forge == "github" {
			skip("base-moved", moved+", and GitHub has no call that tests the merge result again")
			return
		}
		rkey := mergeKey(w.Repository, w.Ref) + ":retest:" + v.head + ":" + tip
		switch {
		case t.Unretestable[rkey]:
			// The forge cannot test the merge result, so asking again
			// before the tip moves is no use: the owner is told once per
			// head what would let it merge (#46).
			s.escalate(name, mergeKey(w.Repository, w.Ref)+":empty:"+v.head, fmt.Sprintf("%s in %s cannot merge: %s, and GitLab answers that a new pipeline for it would be empty, "+
				"so its CI rules make no merge-request pipelines and the merge result cannot be tested. "+
				"To let it merge, merge %s into its branch as a merge commit (its branch pipeline then runs on the new tip), "+
				"add merge-request pipeline rules to its CI, or turn off ci.retest_on_base_move.", w.Ref, w.Repository, moved, v.target), t, s.clock.Now())
			skip("retest-empty", moved+"; a new pipeline for it would be empty, and the owner is told")
			return
		case t.Retests[rkey]:
			skip("retest-waiting", moved+"; waiting for the new pipeline")
			return
		case s.failed(t, rkey):
			skip("retest-gave-up", fmt.Sprintf("%s; asking for a new pipeline failed %d times, not retrying", moved, len(t.Failures[rkey])))
			return
		case !s.retryDue(t, rkey, s.clock.Now()):
			return
		}
		s.act(name, ActRetest, &ActPayload{Key: rkey, Repository: w.Repository, Ref: w.Ref, URL: v.url, Head: v.head, Target: v.target, SHA: tip}, func() error {
			return s.forge.RetestAllowed(w.Repository, w.Ref)
		})
		return
	}
	if m.Method == MergeFastForward {
		why, err := v.fastForward(tip, v.head)
		if err != nil {
			unknown(err)
			return
		}
		if why != "" {
			skip("not-ff", why)
			return
		}
	}
	// The request and its target are asked again just before the merge:
	// a request retargeted or moved on, or a target that moved since the
	// check, is checked again at the next round.
	again, err := view()
	if err != nil {
		unknown(err)
		return
	}
	if !again.open || again.head != v.head || again.target != v.target || again.held != "" || again.unresolved {
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
	s.act(name, ActMerge, &ActPayload{Key: fkey, Repository: w.Repository, Ref: w.Ref, URL: v.url, Head: v.head, Target: v.target, SHA: tip, Method: m.Method}, func() error {
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
		State        string   `json:"state"`
		WebURL       string   `json:"web_url"`
		SHA          string   `json:"sha"`
		TargetBranch string   `json:"target_branch"`
		Draft        bool     `json:"draft"`
		Labels       []string `json:"labels"`
		Author       struct {
			Username string `json:"username"`
		} `json:"author"`
		BlockingDiscussionsResolved *bool `json:"blocking_discussions_resolved"`
	}
	if err := get(base+"/merge_requests/"+ref[1:], &mr); err != nil {
		return mergeView{}, err
	}
	v := mergeView{head: mr.SHA, target: mr.TargetBranch, url: mr.WebURL, open: mr.State == "opened", author: mr.Author.Username, labels: mr.Labels}
	if v.open {
		var err error
		if v.unresolved, err = gitlabUnresolved(get, base, ref[1:], mr.BlockingDiscussionsResolved); err != nil {
			return mergeView{}, err
		}
	}
	// GitLab keeps an approval across a push unless the project resets
	// approvals, so its approval is not tied to the head.
	v.approvers = func(string) ([]string, error) {
		var a struct {
			ApprovedBy []struct {
				User struct {
					Username string `json:"username"`
				} `json:"user"`
			} `json:"approved_by"`
		}
		if err := get(base+"/merge_requests/"+ref[1:]+"/approvals", &a); err != nil {
			return nil, err
		}
		var out []string
		for _, x := range a.ApprovedBy {
			out = append(out, x.User.Username)
		}
		return out, nil
	}
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
		User           struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := get(base+"/pulls/"+ref[1:], &pr); err != nil {
		return mergeView{}, err
	}
	v := mergeView{head: pr.Head.SHA, target: pr.Base.Ref, url: pr.HTMLURL, open: pr.State == "open" && !pr.Merged, author: pr.User.Login}
	if v.open {
		var err error
		if v.unresolved, err = githubUnresolved(get, base, ref[1:]); err != nil {
			return mergeView{}, err
		}
	}
	for _, l := range pr.Labels {
		v.labels = append(v.labels, l.Name)
	}
	// An approval counts at the commit it was given on, while it is its
	// reviewer's latest say: a later request for changes, or a dismissal,
	// takes it back. More reviews than a page holds is an error, not a
	// silent miss.
	v.approvers = func(head string) ([]string, error) {
		var reviews []struct {
			State    string `json:"state"`
			CommitID string `json:"commit_id"`
			User     struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := get(base+"/pulls/"+ref[1:]+"/reviews?per_page=100", &reviews); err != nil {
			return nil, err
		}
		if len(reviews) >= 100 {
			return nil, fmt.Errorf("gh api: pull request %s has 100 reviews or more; agentd reads one page", ref)
		}
		latest := map[string]int{} // each reviewer's latest review that says something
		for i, r := range reviews {
			if r.State != "COMMENTED" && r.State != "PENDING" {
				latest[r.User.Login] = i
			}
		}
		var out []string
		for _, login := range sortedKeys(latest) {
			if r := reviews[latest[login]]; r.State == "APPROVED" && r.CommitID == head {
				out = append(out, login)
			}
		}
		return out, nil
	}
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
