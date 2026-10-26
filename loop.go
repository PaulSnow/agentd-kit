package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The request loop (#53, #1): agentd carries each request of a repository
// with a sessions entry through review, judgment, fix rounds and merge
// itself, the same way every time, and calls Claude only for bounded
// judgment sessions. A delivered request, or an open one no session
// delivered, is reviewed at its head by a reviewer session; a round with
// findings is disposed by a judge session; adopted findings relaunch the
// work session as a fix round, whose next delivery moves the head and is
// reviewed again; a converged loop merges by policy. Each step agentd
// takes is a loop event; where a request stands is worked out each pass
// from those, its case's rounds, its CI and its judgment sessions, so a
// replay and agentd status agree with what the loop acts on. A judgment
// session that is stopped, crash-loops or finishes without its call
// fails its step only: that request waits for the owner's agentd loop
// retry, and the rest of the repository goes on.

// Loop steps.
const (
	LoopReview  = "review"  // a reviewer was started on the head
	LoopJudge   = "judge"   // a judge was started on a round
	LoopFix     = "fix"     // the work session was relaunched with a round's findings
	LoopApprove = "approve" // a request no session delivered converged at the head
	LoopFailed  = "failed"  // a judgment session failed its step
	LoopRetry   = "retry"   // the owner's word to take a failed step again
)

// Where a request stands in the loop.
const (
	PlaceReviewing = "reviewing"
	PlaceJudging   = "judging"
	PlaceFixing    = "fixing"
	PlaceCI        = "waiting for CI"
	PlaceMerging   = "merging"
	PlaceOwner     = "waiting for the owner"
)

func validLoopStep(step string) bool {
	switch step {
	case LoopReview, LoopJudge, LoopFix, LoopApprove, LoopFailed, LoopRetry:
		return true
	}
	return false
}

// LoopPayload is one step agentd took for a request. Failed names the step
// a failure is of.
type LoopPayload struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
	Case       string `json:"case,omitempty"`
	Head       string `json:"head,omitempty"`
	Step       string `json:"step"`
	Failed     string `json:"failed,omitempty"`
	Session    string `json:"session,omitempty"`
	Round      int    `json:"round,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// LoopRecord is a request's steps as the state holds them: the round each
// step was last taken for, and the failure the request waits on.
type LoopRecord struct {
	ReviewRound int
	ReviewHead  string
	JudgeRound  int
	FixRound    int
	FixHead     string
	Failed      *LoopPayload
	Failures    int // every failed step, so each is told to the owner
}

// requestsTrackPrefix begins the name of the track a repository's
// requests no session delivered are watched, approved and escalated on.
const requestsTrackPrefix = "requests:"

func requestsTrack(repo string) string { return requestsTrackPrefix + repo }

// FixRoundPointer is what a work session relaunched for a fix round reads
// before its facts.
const FixRoundPointer = "agentd: the judge adopted review findings on your request. The rounds in your facts list them with their dispositions and the plan; fix the adopted ones on the same branch, then mark STATUS.md DONE so agentd delivers and reviews again."

// applyLoop folds a loop event, copying the record it changes.
func (st *LogState) applyLoop(e Event) {
	p := e.Loop
	key := ciKey(p.Repository, p.Ref)
	if st.Loops == nil {
		st.Loops = map[string]LoopRecord{}
	}
	r := st.Loops[key]
	switch p.Step {
	case LoopReview:
		r.ReviewRound, r.ReviewHead = p.Round, p.Head
	case LoopJudge:
		r.JudgeRound = p.Round
	case LoopFix:
		r.FixRound, r.FixHead = p.Round, p.Head
	case LoopApprove:
		if st.Approvals == nil {
			st.Approvals = map[string]ApprovalRecord{}
		}
		st.Approvals[key] = ApprovalRecord{ApprovalPayload: ApprovalPayload{Repository: p.Repository, Ref: p.Ref, SHA: p.Head}, By: "agentd", At: e.Time}
	case LoopFailed:
		f := *p
		r.Failed = &f
		r.Failures++
	case LoopRetry:
		// The failed step is due again: its round counts as not taken.
		if f := r.Failed; f != nil {
			switch f.Failed {
			case LoopReview:
				r.ReviewRound = f.Round - 1
			case LoopJudge:
				r.JudgeRound = f.Round - 1
			}
			r.Failed = nil
		}
	}
	st.Loops[key] = r
}

// looped says whether agentd runs the repository's request loop: it has
// a sessions entry.
func looped(repo Repository) bool {
	return repo.Sessions != nil
}

// loopRequest is one open request of a looped repository: the work
// session that delivered it, or none, and the case its rounds are in.
type loopRequest struct {
	ciWatched
	Outside bool
	Case    string
	Branch  string // the branch its judgment sessions are named for
}

// loopRequests is every open request of a looped repository, delivered
// first, in the order the watcher lists them.
func loopRequests(cfg *Config, st *LogState) []loopRequest {
	delivered := watchedRequestsOf(cfg, st)
	var out []loopRequest
	for i, w := range append(delivered, outsideRequestsOf(cfg, st, delivered)...) {
		repo := cfg.Repositories[w.Repository]
		if !looped(repo) {
			continue
		}
		q := loopRequest{ciWatched: w, Outside: i >= len(delivered), Branch: "request-" + w.Ref[1:]}
		if q.Outside {
			q.Case = repoKey(repo.Forge, repo.Project) + w.Ref
		} else {
			sess := cfg.Sessions[w.Session]
			q.Case = cfg.caseOf(sess)
			if sess.Branch != "" {
				q.Branch = sess.Branch
			}
		}
		out = append(out, q)
	}
	return out
}

// loopPlace is where a request stands, and the step due, if any: Do is a
// loop step, or "end" for a judgment session whose part is over.
type loopPlace struct {
	Phase, Reason string
	Do            string
	Session       string // the judgment session Do names
	Failed        string // the step a failure is of
	Round         int
	Head          string
}

// judgment is the name of the request's reviewer or judge session.
func (q loopRequest) judgment(kind string) string { return runtimeName(q.Repository, q.Branch, kind) }

// placeOf works out where q stands from the state alone.
func placeOf(cfg *Config, st *LogState, q loopRequest) loopPlace {
	owner := func(format string, a ...any) loopPlace {
		return loopPlace{Phase: PlaceOwner, Reason: fmt.Sprintf(format, a...)}
	}
	key := ciKey(q.Repository, q.Ref)
	r, ok := st.CI[key]
	L := st.Loops[key]
	reviewer, judge := q.judgment(KindReviewer), q.judgment(KindJudge)
	_, reviewing := st.Runtime[reviewer]
	_, judging := st.Runtime[judge]
	switch {
	case q.Case == "":
		return owner("its session works no issue, so it has no case to keep review rounds in")
	case !ok || r.Head == "":
		return loopPlace{Phase: PlaceCI, Reason: "the CI watcher has not recorded its head yet"}
	case L.Failed != nil:
		return owner("%s; agentd loop retry %s %s takes the step again", L.Failed.Reason, q.Repository, q.Ref)
	}
	c := st.Cases[q.Case]
	if c == nil {
		c = &Case{}
	}
	var last *RoundRecord
	if n := len(c.Rounds); n > 0 {
		last = &c.Rounds[n-1]
	}
	if c.Closed {
		switch {
		case reviewing:
			return loopPlace{Phase: PlaceOwner, Reason: "its case is closed", Do: "end", Session: reviewer}
		case judging:
			return loopPlace{Phase: PlaceOwner, Reason: "its case is closed", Do: "end", Session: judge}
		}
		return owner("its case %s is closed", q.Case)
	}
	if stop := c.RoundStop; stop != nil {
		switch {
		case reviewing:
			return loopPlace{Phase: PlaceReviewing, Reason: "round recorded", Do: "end", Session: reviewer}
		case judging:
			return loopPlace{Phase: PlaceJudging, Reason: "round disposed", Do: "end", Session: judge}
		case stop.Reason != RoundsConverged:
			return owner("review rounds stopped at round %d, %s; agentd cases rounds %s N resets them", stop.Round, stop.Reason, q.Case)
		case L.ReviewHead != r.Head && (!q.Outside || st.approvedAt(q.Repository, q.Ref) != r.Head):
			// A head no round reviewed, pushed by a CI fix or the
			// request's author, merges only after the owner resets the
			// rounds, which reviews it.
			return owner("its head moved to %s after review converged at %s; agentd cases rounds %s N reviews it", shortHash(r.Head), shortHash(L.ReviewHead), q.Case)
		case q.Outside && st.approvedAt(q.Repository, q.Ref) != r.Head:
			return loopPlace{Phase: PlaceMerging, Reason: "review converged", Do: LoopApprove, Head: r.Head}
		case r.State == CIPending || r.State == CIUnknown:
			return loopPlace{Phase: PlaceCI, Reason: fmt.Sprintf("CI on %s is %s", shortHash(r.Head), r.State)}
		case r.State != CISuccess:
			return loopPlace{Phase: PlaceCI, Reason: fmt.Sprintf("CI on %s %s; review converged", shortHash(r.Head), r.State)}
		case !cfg.Repositories[q.Repository].Policy.Merge.Enabled:
			return owner("review converged and CI is green; %s's policy does not merge, so the owner does", q.Repository)
		}
		return loopPlace{Phase: PlaceMerging, Reason: "review converged and CI is green; merging by policy"}
	}
	if last != nil && len(last.Findings) > 0 && last.Dispositions == nil {
		switch {
		case reviewing:
			return loopPlace{Phase: PlaceJudging, Reason: "round recorded", Do: "end", Session: reviewer}
		case L.JudgeRound < last.N && judging:
			return loopPlace{Phase: PlaceJudging, Reason: "an earlier judge is still running", Do: "end", Session: judge}
		case L.JudgeRound < last.N:
			return loopPlace{Phase: PlaceJudging, Reason: fmt.Sprintf("round %d's findings await a judge", last.N), Do: LoopJudge, Session: judge, Round: last.N, Head: r.Head}
		case !judging && st.Mode == ModeShadow:
			return loopPlace{Phase: PlaceJudging, Reason: fmt.Sprintf("round %d; shadow mode starts no judge", last.N)}
		case !judging:
			return loopPlace{Phase: PlaceJudging, Do: LoopFailed, Failed: LoopJudge, Session: judge, Round: last.N,
				Reason: fmt.Sprintf("the judge of round %d ended without its dispositions", last.N)}
		}
		// A judge that asked the owner waits for the answer, DONE or not.
		if t := st.Tracks[judge]; t != nil && (t.State == BlockedOnOwner || t.Questions.Open || t.blocking()) {
			return owner("the judge of round %d asks the owner", last.N)
		}
		if why := judgmentFailed(st.Tracks[judge]); why != "" {
			return loopPlace{Phase: PlaceJudging, Do: LoopFailed, Failed: LoopJudge, Session: judge, Round: last.N, Reason: fmt.Sprintf("the judge of round %d %s", last.N, why)}
		}
		return loopPlace{Phase: PlaceJudging, Reason: fmt.Sprintf("round %d", last.N)}
	}
	if judging {
		return loopPlace{Phase: PlaceJudging, Reason: "round disposed", Do: "end", Session: judge}
	}
	if last != nil && adopts(last) {
		switch {
		case q.Outside:
			return owner("round %d adopted findings on a request no session delivered; its author fixes them", last.N)
		case L.FixRound < last.N:
			return loopPlace{Phase: PlaceFixing, Reason: fmt.Sprintf("round %d adopted findings; the work session is relaunched once it is done", last.N),
				Do: LoopFix, Session: q.Session, Round: last.N, Head: r.Head}
		case r.Head == L.FixHead:
			if f, ok := lastFixRound(st.Tracks[q.Session], q); ok && f.Redelivered && f.Pushed == L.FixHead {
				return owner("the fix round of round %d delivered no new commit", last.N)
			}
			return loopPlace{Phase: PlaceFixing, Reason: fmt.Sprintf("fix round %d: waiting for the work session's next delivery", last.N)}
		}
	}
	next := len(c.Rounds) + 1
	switch {
	case L.ReviewRound < next && reviewing:
		return loopPlace{Phase: PlaceReviewing, Reason: "an earlier reviewer is still running", Do: "end", Session: reviewer}
	case L.ReviewRound < next:
		return loopPlace{Phase: PlaceReviewing, Reason: fmt.Sprintf("round %d is due at %s", next, shortHash(r.Head)), Do: LoopReview, Session: reviewer, Round: next, Head: r.Head}
	case !reviewing && st.Mode == ModeShadow:
		return loopPlace{Phase: PlaceReviewing, Reason: fmt.Sprintf("round %d at %s; shadow mode starts no reviewer", next, shortHash(L.ReviewHead))}
	case !reviewing:
		return loopPlace{Phase: PlaceReviewing, Do: LoopFailed, Failed: LoopReview, Session: reviewer, Round: next, Reason: fmt.Sprintf("the reviewer of round %d ended without its findings", next)}
	}
	if t := st.Tracks[reviewer]; t != nil && (t.State == BlockedOnOwner || t.Questions.Open || t.blocking()) {
		return owner("the reviewer of round %d asks the owner", next)
	}
	if why := judgmentFailed(st.Tracks[reviewer]); why != "" {
		return loopPlace{Phase: PlaceReviewing, Do: LoopFailed, Failed: LoopReview, Session: reviewer, Round: next, Reason: fmt.Sprintf("the reviewer of round %d %s", next, why)}
	}
	return loopPlace{Phase: PlaceReviewing, Reason: fmt.Sprintf("round %d at %s", next, shortHash(L.ReviewHead))}
}

// lastFixRound is the latest fix round the work session was relaunched
// for on q.
func lastFixRound(t *Track, q loopRequest) (CIFixRecord, bool) {
	if t == nil {
		return CIFixRecord{}, false
	}
	fixes := t.ciFixes(q.Repository, q.Ref)
	for i := len(fixes) - 1; i >= 0; i-- {
		if fixes[i].Review {
			return fixes[i], true
		}
	}
	return CIFixRecord{}, false
}

// judgmentFailed is why a running reviewer or judge will not make its
// call, or "".
func judgmentFailed(t *Track) string {
	switch {
	case t == nil:
		return ""
	case t.State == Stopped:
		return "was stopped: " + t.Stopped
	case t.State == CrashLooping || t.Parked:
		return "crash-loops"
	case t.State == Failed:
		return "failed"
	case t.State == Done:
		return "finished without its call"
	}
	return ""
}

// adopts says whether a disposed round adopted a finding.
func adopts(r *RoundRecord) bool {
	for _, d := range r.Dispositions {
		if d.Disposition == DispositionAdopt {
			return true
		}
	}
	return false
}

// LoopView is a request's place in the loop, as agentd status shows it.
type LoopView struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
	Phase      string `json:"phase"`
	Reason     string `json:"reason,omitempty"`
}

// loopViews is every looped request's place.
func loopViews(cfg *Config, st *LogState) []LoopView {
	var out []LoopView
	for _, q := range loopRequests(cfg, st) {
		p := placeOf(cfg, st, q)
		out = append(out, LoopView{Repository: q.Repository, Ref: q.Ref, Phase: p.Phase, Reason: p.Reason})
	}
	return out
}

// runLoops takes the step due for each looped request, then starts the
// next queued case where there is room. A fix round is the work session's
// own step (fixRound), taken when it is done.
func (s *Supervisor) runLoops(now time.Time) {
	for _, q := range loopRequests(s.cfg, s.st) {
		if s.halt != nil {
			return
		}
		p := placeOf(s.cfg, s.st, q)
		lp := LoopPayload{Repository: q.Repository, Ref: q.Ref, Case: q.Case, Head: p.Head, Step: p.Do, Session: p.Session, Round: p.Round}
		switch p.Do {
		case "end":
			s.endJudgment(q.Repository, p.Session)
		case LoopReview, LoopJudge:
			kind := KindReviewer
			if p.Do == LoopJudge {
				kind = KindJudge
			}
			if s.shadow {
				s.emit("", Event{Kind: KindLoop, Loop: &lp})
				continue
			}
			if why := s.startJudgment(q, kind, p.Session); why != "" {
				s.skipOnce(requestsTrack(q.Repository), fmt.Sprintf("loop:%s:%s:%d", ciKey(q.Repository, q.Ref), p.Do, p.Round), fmt.Sprintf("not starting the %s of %s in %s: %s", kind, q.Ref, q.Repository, why), s.track(requestsTrack(q.Repository)), nil)
				continue
			}
			s.emit("", Event{Kind: KindLoop, Loop: &lp})
		case LoopApprove:
			s.emit("", Event{Kind: KindLoop, Loop: &lp})
		case LoopFailed:
			lp.Failed, lp.Reason = p.Failed, p.Reason
			if s.emit("", Event{Kind: KindLoop, Loop: &lp}); s.halt != nil {
				return
			}
			s.endJudgment(q.Repository, p.Session)
			t := s.track(requestsTrack(q.Repository))
			s.escalate(requestsTrack(q.Repository), fmt.Sprintf("loop-failed:%s:%d", ciKey(q.Repository, q.Ref), s.st.Loops[ciKey(q.Repository, q.Ref)].Failures),
				fmt.Sprintf("%s in %s: %s. The rest of %s's requests go on; agentd loop retry %s %s takes the step again.",
					q.Ref, q.Repository, p.Reason, q.Repository, q.Repository, q.Ref), t, now)
		}
	}
	s.endFinished()
	s.startNext(now)
}

// endFinished ends each work session agentd started in a looped
// repository once every request it delivered is merged or closed, and
// each reviewer or judge whose request left the loop, so its room under
// sessions.max goes to the next case and the judgment sessions.
func (s *Supervisor) endFinished() {
	current := map[string]bool{}
	for _, q := range loopRequests(s.cfg, s.st) {
		current[q.judgment(KindReviewer)], current[q.judgment(KindJudge)] = true, true
	}
	for _, name := range sortedKeys(s.st.Runtime) {
		p := s.st.Runtime[name]
		t := s.st.Tracks[name]
		if (p.Kind == KindReviewer || p.Kind == KindJudge) && p.By == "agentd" && !current[name] && !s.shadow {
			s.endJudgment(p.Repository, name)
			continue
		}
		if p.Kind != KindWork || !looped(s.cfg.Repositories[p.Repository]) || t == nil || len(t.Deliveries) == 0 || s.shadow {
			continue
		}
		open := false
		for _, d := range t.Deliveries {
			r, ok := s.st.CI[ciKey(p.Repository, d.Ref)]
			open = open || !ok || r.State != CIMerged && r.State != CIClosed
		}
		if !open {
			s.emit(name, Event{Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: newIntentID(), Action: SessionEnd, Repository: p.Repository, By: "agentd"}})
		}
	}
}

// startJudgment records the start of a reviewer or judge for q under the
// name the loop knows it by, and says why not when it could not.
func (s *Supervisor) startJudgment(q loopRequest, kind, name string) string {
	issue := 0
	if !q.Outside {
		issue = s.cfg.Sessions[q.Session].Issue
	}
	p := RuntimePayload{Kind: kind, Branch: q.Branch, Issue: issue, Head: s.st.CI[ciKey(q.Repository, q.Ref)].Head}
	if q.Outside {
		p.Request = q.Ref
	}
	return s.startRuntime(q.Repository, name, p)
}

// startRuntime records agentd's start of a session of repo, placed as the
// repository's policy says, and says why not when it holds no room or
// another start of the name holds.
func (s *Supervisor) startRuntime(repo, name string, p RuntimePayload) string {
	rules, _ := effectivePolicy(s.cfg, s.st, repo)
	if _, ok := s.st.Runtime[name]; ok {
		return "it is running already"
	}
	if c, ok := s.cfg.Sessions[name]; ok && !c.Runtime {
		return "a configured session has the name"
	}
	if n := len(sessionsOf(s.st.Runtime, repo)); n >= rules.Sessions.Max {
		return fmt.Sprintf("%s runs %d started sessions, its max", repo, n)
	}
	p.ID, p.Action, p.Repository, p.Max, p.By = newIntentID(), SessionStart, repo, rules.Sessions.Max, "agentd"
	if sp := s.cfg.Repositories[repo].Sessions; sp != nil {
		p.Host = sp.Host
	}
	if p.Host != "" && len(rules.Sessions.Accounts) == 0 {
		return repo + " places its sessions on " + p.Host + " and names no account for them"
	}
	switch {
	case len(rules.Sessions.Fallback) > 0 && p.Host == "":
		p.Place = true
	case len(rules.Sessions.Accounts) > 0:
		p.Account = rules.Sessions.Accounts[0]
	}
	if s.emit(name, Event{Kind: KindSession, Mode: ModeShared, Runtime: &p}); s.halt != nil {
		return s.halt.Error()
	}
	if got, ok := s.st.Runtime[name]; !ok || got.ID != p.ID {
		return "another start came first"
	}
	return ""
}

// endJudgment records the end of a reviewer or judge agentd started.
func (s *Supervisor) endJudgment(repo, name string) {
	if s.shadow {
		return
	}
	if _, ok := s.st.Runtime[name]; ok {
		s.emit(name, Event{Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: newIntentID(), Action: SessionEnd, Repository: repo, By: "agentd"}})
	}
}

// fixRound relaunches a work session that is done, or idle, for the round
// whose adopted findings its request waits on, and says whether it did.
// The session keeps its branch; its STATUS.md loses its DONE and its
// facts carry the rounds, so its next delivery moves the request's head.
func (s *Supervisor) fixRound(name string, sess Session, t *Track, ev Evidence, state State) bool {
	if state != Done && state != Idle || t.Limited {
		return false
	}
	for _, q := range loopRequests(s.cfg, s.st) {
		if q.Outside || q.Session != name {
			continue
		}
		p := placeOf(s.cfg, s.st, q)
		if p.Do != LoopFix || t.Delivered[q.Delivered] == "" {
			continue
		}
		id := newSessionID()
		cmd, facts := s.launchCommandWith(name, sess, t, id, "fix-round", nil)
		line, err := s.launchLine(sess, cmd)
		if err != nil {
			s.skipOnce(name, "account:"+name, "not starting a fix round: "+err.Error(), t, &SkipPayload{Account: sess.Account})
			return false
		}
		status := filepath.Join(sess.Workdir, "STATUS.md")
		err = s.act(name, ActFixRound, &ActPayload{Key: fmt.Sprintf("fix-round:%s:%s:%d", name, q.Ref, p.Round), Cwd: sess.Cwd, Respawn: ev.PaneDead, Account: sess.Account,
			Command: line, Transcript: id, Repository: q.Repository, Ref: q.Ref, Head: p.Head, Delivered: q.Delivered}, func() error {
			if b, err := os.ReadFile(status); err == nil && ParseStatus(string(b)).Done {
				if err := writeAtomic(status, []byte(undone(string(b)))); err != nil {
					return err
				}
			}
			switch {
			case ev.Alive:
				return s.tmux.Replace(name, sess.Cwd, line)
			case ev.PaneDead:
				return s.tmux.Respawn(name, sess.Cwd, line)
			}
			return s.tmux.NewSession(name, sess.Cwd, line, s.cfg.Policies.Restart.Width, s.cfg.Policies.Restart.Height)
		})
		if err != nil || s.halt != nil {
			return false
		}
		s.emit("", Event{Kind: KindLoop, Loop: &LoopPayload{Repository: q.Repository, Ref: q.Ref, Case: q.Case, Head: p.Head, Step: LoopFix, Session: name, Round: p.Round}})
		if s.shadow {
			return false
		}
		s.notePack(name, sess, facts)
		return true
	}
	return false
}

// startNext starts a work session, one a pass, on the top-ranked case the
// owner queued in a looped repository with room under its max, less one
// place kept for judgment sessions: pinned, or
// in a repository the week's priorities list; not started, not reserved
// and with no session.
func (s *Supervisor) startNext(now time.Time) {
	room := map[string]string{} // repository key to its name
	for _, name := range sortedKeys(s.cfg.Repositories) {
		repo := s.cfg.Repositories[name]
		rules, _ := effectivePolicy(s.cfg, s.st, name)
		// One place is kept for the judgment sessions the work asks for.
		if looped(repo) && len(sessionsOf(s.st.Runtime, name))+1 < rules.Sessions.Max {
			room[repoKey(repo.Forge, repo.Project)] = name
		}
	}
	if len(room) == 0 || s.shadow {
		return
	}
	pr, err := LoadPriorities(s.cfg.Tracking.Priorities)
	if err != nil {
		name := room[sortedKeys(room)[0]]
		s.skipOnce(requestsTrack(name), "loop-priorities:"+err.Error(), "not starting queued cases: "+err.Error(), s.track(requestsTrack(name)), nil)
		return
	}
	if pr == nil {
		return
	}
	cs := CasesFrom(s.cfg, s.st)
	RankCases(cs, pr)
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Rank < cs[j].Rank })
	for _, c := range cs {
		name, ok := room[c.Repository]
		it := s.st.Items[c.ID]
		if !ok || !c.Pinned && c.Priority == 0 || c.State != CaseNotStarted || len(c.Sessions) > 0 || it == nil || it.Kind != ItemIssue {
			continue
		}
		if rules, _ := effectivePolicy(s.cfg, s.st, name); rules.Scope.Reserved.matches(c.Issue, nil) {
			continue
		}
		branch := fmt.Sprintf("issue-%d", c.Issue)
		session := runtimeName(name, branch, KindWork)
		if why := s.startRuntime(name, session, RuntimePayload{Kind: KindWork, Branch: branch, Issue: c.Issue}); why != "" {
			s.skipOnce(requestsTrack(name), "loop-start:"+c.ID, fmt.Sprintf("not starting %s: %s", c.ID, why), s.track(requestsTrack(name)), nil)
			continue
		}
		return
	}
}

// RetryLoop is the owner's record that a request's failed step is taken
// again: agentd loop retry REPO REF.
func RetryLoop(cfg *Config, log *Log, events []Event, now time.Time, repo, ref string) error {
	st := NewLogState(cfg, ModeLive).Fold(events)
	if _, ok := cfg.Repositories[repo]; !ok {
		return fmt.Errorf("no repository %q is configured", repo)
	}
	if !requestRef.MatchString(ref) {
		return fmt.Errorf("request %q is not a reference such as !12 or #12", ref)
	}
	if st.Loops[ciKey(repo, ref)].Failed == nil {
		return fmt.Errorf("request %s in %s has no failed step to retry", ref, repo)
	}
	return log.Append(Event{V: EventVersion, Time: now, Kind: KindLoop, Mode: ModeOwner, Loop: &LoopPayload{Repository: repo, Ref: ref, Step: LoopRetry}})
}

// ReviewFact is the request a reviewer or judge agentd started serves:
// the head to review, which it checks out itself, and its target.
type ReviewFact struct {
	Request string `json:"request"`
	URL     string `json:"url,omitempty"`
	Head    string `json:"head"`
	Target  string `json:"target,omitempty"`
}

// reviewFact is the request the judgment session name serves, or nil.
func (s *Supervisor) reviewFact(name string) *ReviewFact {
	for _, q := range loopRequests(s.cfg, s.st) {
		if q.judgment(KindReviewer) != name && q.judgment(KindJudge) != name {
			continue
		}
		// The head the step was recorded for, which its round is
		// credited to, even when the request moved since.
		r := s.st.CI[ciKey(q.Repository, q.Ref)]
		head := r.Head
		if p, ok := s.st.Runtime[name]; ok && p.Head != "" {
			head = p.Head
		}
		return &ReviewFact{Request: q.Ref, URL: r.URL, Head: head, Target: r.Target}
	}
	return nil
}
