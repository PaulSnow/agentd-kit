package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
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
	return finishedWhy(s.cfg, s.st, name, s.clock.Now(), s.work.busy)
}

// finishedWhy is finishedFor from the state alone at now; busy, when
// given, says whether a worker runs a keyed job, as a delivery.
func finishedWhy(cfg *Config, st *LogState, name string, now time.Time, busy func(string) bool) string {
	p, ok := st.Runtime[name]
	t := st.Tracks[name]
	if !ok || p.Kind != KindWork || t == nil || len(st.itemsOf(name)) > 0 {
		return ""
	}
	// A message routed to it and not yet delivered waits on it: the
	// owner may have sent it there to reopen it.
	if len(st.freshInstructions(name, false, now)) > 0 {
		return ""
	}
	// Each delivery it makes after DONE has its outcome, and none runs.
	for _, d := range withRuntime(cfg, st.Runtime).Sessions[name].Deliver {
		key := name + ":" + d.Worktree
		if t.Delivered[key] == "" || busy != nil && busy("deliver:"+key) {
			return ""
		}
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
		r, ok := st.CI[ciKey(p.Repository, d.Ref)]
		if !ok || r.State != CIMerged && r.State != CIClosed {
			return ""
		}
		refs = append(refs, d.Ref)
	}
	if len(refs) == 0 {
		// Only one that wrote DONE is finished with nothing delivered:
		// one stopped or parked waits on the owner's clear.
		if t.State != Done {
			return ""
		}
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
