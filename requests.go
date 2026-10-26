package main

import (
	"strconv"
	"time"
)

// Every request (#36, #3, #53). In a repository whose request loop agentd
// runs, the CI watcher lists the open requests each round and watches
// those no session delivered on the repository's requests track,
// requests:REPO: their CI events and records carry that track. The loop
// reviews them as it does a delivered request, and once review converged
// it approves the request at that head; a request agentd did not deliver
// merges only under the repository's policy with green CI and that
// approval at its head, since agentd's gate never ran on it.

// ApprovalPayload is a request's approval at Head: the loop's, held in
// the state, or, in an earlier version's approval event, a coordinator's,
// which still reads and is ignored.
type ApprovalPayload struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
	SHA        string `json:"sha"`
}

// ApprovalRecord is a request's latest approval, as the state holds it.
type ApprovalRecord struct {
	ApprovalPayload
	By string
	At time.Time
}

// approvedAt is the head the request's latest approval names, or "".
func (st *LogState) approvedAt(repo, ref string) string {
	return st.Approvals[ciKey(repo, ref)].SHA
}

// discussionsPage is how many reviews or discussions are read: one page.
// A full page may hide more, and holds the request as if one were open.
const discussionsPage = 100

// githubUnresolved says whether pull request n has an open discussion.
// REST has no thread state: a reviewer whose latest review that says
// something requests changes is the open discussion; a dismissal or a
// later approval takes it back. Branch protection's conversation
// resolution shows as mergeable_state blocked, which holds a merge too.
func githubUnresolved(get func(string, any) error, base, n string) (bool, error) {
	var reviews []struct {
		State string `json:"state"`
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := get(base+"/pulls/"+n+"/reviews?per_page="+strconv.Itoa(discussionsPage), &reviews); err != nil {
		return false, err
	}
	if len(reviews) >= discussionsPage {
		return true, nil
	}
	latest := map[string]string{}
	for _, r := range reviews {
		if r.State != "COMMENTED" && r.State != "PENDING" {
			latest[r.User.Login] = r.State
		}
	}
	for _, st := range latest {
		if st == "CHANGES_REQUESTED" {
			return true, nil
		}
	}
	return false, nil
}

// gitlabUnresolved says whether merge request iid has an open discussion:
// GitLab says its blocking discussions are not resolved, which it says
// only where the project requires threads resolved, or one of its
// resolvable threads is not resolved.
func gitlabUnresolved(get func(string, any) error, base, iid string, blocking *bool) (bool, error) {
	if blocking != nil && !*blocking {
		return true, nil
	}
	var ds []struct {
		Notes []struct {
			Resolvable bool `json:"resolvable"`
			Resolved   bool `json:"resolved"`
		} `json:"notes"`
	}
	if err := get(base+"/merge_requests/"+iid+"/discussions?per_page="+strconv.Itoa(discussionsPage), &ds); err != nil {
		return false, err
	}
	if len(ds) >= discussionsPage {
		return true, nil
	}
	for _, d := range ds {
		for _, n := range d.Notes {
			if n.Resolvable && !n.Resolved {
				return true, nil
			}
		}
	}
	return false, nil
}
