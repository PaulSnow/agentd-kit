package main

import (
	"fmt"
	"sort"
	"strings"
)

// Go keeps its own room (#68): a start that finds its repository at
// sessions.max ends a finished session agentd started there, the oldest
// first, and logs which and why, so no start waits on the owner to end
// one. A finished session is a work session that wrote DONE, or stopped,
// crash-looped or was parked, with every request it delivered merged or
// closed and no work item of the owner's held. A session at work, one
// whose request is still in review (its fix rounds are its own), a
// reviewer or judge (the loop ends those), and a chat session are never
// ended for room: when every place is held so, the start is refused and
// the owner told, as before.

// finishedFor says why a started session may be ended for room, "" when
// it may not.
func (s *Supervisor) finishedFor(name string) string {
	p, ok := s.st.Runtime[name]
	t := s.st.Tracks[name]
	if !ok || p.Kind != KindWork || t == nil || len(s.st.itemsOf(name)) > 0 {
		return ""
	}
	var how string
	switch {
	case t.State == Done:
		how = "wrote DONE"
	case t.Parked:
		how = "is parked"
	case t.State == Stopped || t.State == CrashLooping:
		how = "stopped (" + string(t.State) + ")"
	default:
		return ""
	}
	refs := []string{}
	for _, d := range t.Deliveries {
		r, ok := s.st.CI[ciKey(p.Repository, d.Ref)]
		if !ok || r.State != CIMerged && r.State != CIClosed {
			return ""
		}
		refs = append(refs, d.Ref)
	}
	if len(refs) == 0 {
		return how + ", with no request delivered"
	}
	sort.Strings(refs)
	return fmt.Sprintf("%s, with every request it delivered merged or closed (%s)", how, strings.Join(refs, ", "))
}

// makeRoom ends the oldest finished session of repo to make room for the
// start of name under max, telling the repository's console which and
// why. It says whether it ended one.
func (s *Supervisor) makeRoom(repo, name string, max int) bool {
	if s.shadow {
		return false
	}
	type cand struct {
		name, why string
	}
	var cands []cand
	for _, n := range sessionsOf(s.st.Runtime, repo) {
		if why := s.finishedFor(n); why != "" {
			cands = append(cands, cand{n, why})
		}
	}
	if len(cands) == 0 {
		return false
	}
	sort.SliceStable(cands, func(i, j int) bool { return s.st.StartedAt[cands[i].name].Before(s.st.StartedAt[cands[j].name]) })
	c := cands[0]
	why := fmt.Sprintf("agentd ended %s, which %s, to make room for %s under %s's sessions.max (%d); its workdir and worktree stay.", c.name, c.why, name, repo, max)
	if s.emit(c.name, Event{Kind: KindSession, Mode: ModeShared, Runtime: &RuntimePayload{ID: newIntentID(), Action: SessionEnd, Repository: repo, By: "agentd", Why: why}}); s.halt != nil {
		return false
	}
	m := &MessagePayload{ID: "R" + shortHash(HashPane(c.name + "\n" + name))[:8], Console: s.cfg.RepositoryConsole(repo), Kind: "info", From: "agentd", Text: why}
	if i := s.confidentialMatch(why); i > 0 {
		m.Text, m.Withheld = "", i
	}
	if s.emit("", Event{Kind: KindMessage, Mode: ModeShared, Message: m}); s.halt != nil {
		return false
	}
	s.refreshRuntime()
	return true
}
