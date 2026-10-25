package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Every request (#36, #3). In a repository with a coordinator, the CI
// watcher lists the open requests each round and watches those no
// session delivered for the coordinator: their CI events and records
// carry the coordinator's session. The coordinator approves a request at
// a head with approve(request, sha); a request agentd did not deliver
// merges only under the repository's policy with green CI and that
// approval at its head, since agentd's gate never ran on it. A red one is
// due for the coordinator, and the pack lists them all.

// ToolApprove is the coordinator's tool that approves a request at a head.
const ToolApprove = "approve"

// ApprovalPayload is a coordinator's approval of a request at Head.
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

// shaPrefix is what approve takes as a head: 7 to 40 hex digits.
var shaPrefix = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// callApprove checks and records a coordinator's approve: a request of
// its own repository the CI watcher recorded, at its recorded head, given
// whole or by a prefix of 7 or more. A refused call is answered and not
// logged.
func (srv *MCPServer) callApprove(st *LogState, args json.RawMessage) (string, bool) {
	refuse := func(format string, a ...any) (string, bool) {
		return compact(map[string]any{"refused": fmt.Sprintf(format, a...)}), true
	}
	var a struct {
		Request string `json:"request"`
		SHA     string `json:"sha"`
	}
	if ps := srv.decodeArgs(args, &a); len(ps) > 0 {
		return refuse("%s", ps[0].reply())
	}
	own := srv.sess.Repository
	if !requestRef.MatchString(a.Request) {
		return refuse("request %q is not a reference such as !12 or #12", a.Request)
	}
	if !shaPrefix.MatchString(a.SHA) {
		return refuse("sha %q is not 7 to 40 hex digits", a.SHA)
	}
	r, ok := st.CI[ciKey(own, a.Request)]
	switch {
	case !ok || r.Head == "":
		return refuse("agentd has no record of request %s in %s yet; approve it once its CI is recorded", a.Request, own)
	case r.State == CIMerged || r.State == CIClosed:
		return refuse("request %s is %s", a.Request, r.State)
	case !strings.HasPrefix(r.Head, a.SHA):
		return refuse("request %s's head is %s, not %s: review the head as it is now", a.Request, shortHash(r.Head), a.SHA)
	}
	p := &ApprovalPayload{Repository: own, Ref: a.Request, SHA: r.Head}
	if err := srv.log.Append(Event{V: EventVersion, Time: srv.now(), Session: srv.name, Kind: KindApproval, Mode: ModeShared, Approval: p}); err != nil {
		return refuse("agentd could not record the approval: %v", err)
	}
	return compact(map[string]any{"approved": true, "request": a.Request, "sha": r.Head}), false
}

// outsideRecords is each open request of repo no session delivered, by
// key, as the watcher last recorded it for the coordinator.
func (s *Supervisor) outsideRecords(repo string) []CIRecord {
	coord, ok := s.coordinated()[repo]
	if !ok {
		return nil
	}
	var out []CIRecord
	for _, key := range sortedKeys(s.st.CI) {
		if r := s.st.CI[key]; r.Repository == repo && r.Session == coord && r.State != CIMerged && r.State != CIClosed {
			out = append(out, r)
		}
	}
	return out
}

// packExcerpt bounds a red request's log excerpt in the pack.
const packExcerpt = 600

// PackRequest is a request of the repository no session delivered, as the
// coordinator's pack lists it.
type PackRequest struct {
	Ref        string   `json:"ref"`
	URL        string   `json:"url,omitempty"`
	Author     string   `json:"author,omitempty"`
	Head       string   `json:"head,omitempty"`
	Target     string   `json:"target,omitempty"`
	State      string   `json:"state"`
	Jobs       []string `json:"jobs,omitempty"`
	Excerpt    string   `json:"excerpt,omitempty"`
	Draft      bool     `json:"draft,omitempty"`
	Unresolved bool     `json:"unresolved,omitempty"`
	Approved   bool     `json:"approved"`
}

// packRequests is the repository's requests no session delivered.
func (s *Supervisor) packRequests(repo string) []PackRequest {
	var out []PackRequest
	for _, r := range s.outsideRecords(repo) {
		out = append(out, PackRequest{Ref: r.Ref, URL: r.URL, Author: r.Author, Head: r.Head, Target: r.Target, State: r.State,
			Jobs: r.Jobs, Excerpt: cutUTF8(r.Excerpt, packExcerpt), Draft: r.Draft, Unresolved: r.Unresolved,
			Approved: r.Head != "" && s.st.approvedAt(r.Repository, r.Ref) == r.Head})
	}
	return out
}

// ciReason is the due reason of a red request no session delivered.
func ciReason(r CIRecord) string {
	return "ci: " + r.Ref + " failed at " + shortHash(r.Head)
}
