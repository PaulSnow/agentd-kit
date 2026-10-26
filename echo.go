package main

import (
	"strconv"
	"strings"
	"time"
)

// Echoes (#49): what a coordinator did comes back to it from the forge as
// items, and a wake for its own approval, merge or comment costs a whole
// pack for nothing. An item is its own when one of its logins wrote it,
// or when it is a discussion whose thread moved within the echo window of
// an act on that thread: an approval, agentd's merge, a session started
// or ended for the issue, or the turn that handled a wake carrying it.

// threadOf is the forge thread an item id names: the id without the
// update time a discussion's carries.
func threadOf(id string) string {
	if i := strings.LastIndexByte(id, '@'); i >= 0 {
		return id[:i]
	}
	return id
}

// refThread is the thread of a request or issue ref in repo: requests
// take "!", as the forge items do on either forge.
func (st *LogState) refThread(repo, sigil string, number int) string {
	r, ok := st.cfg.Repositories[repo]
	if !ok || number <= 0 {
		return ""
	}
	return repoKey(r.Forge, r.Project) + sigil + strconv.Itoa(number)
}

// echoSkew is how far before an act its thread may move and still be its
// echo: the forge's clock and agentd's differ a little, and a merge is
// logged after the forge made it. An outsider's move earlier than that is
// theirs.
const echoSkew = 30 * time.Second

// noteTouch records the threads e acts on for a coordinator: its
// approvals, agentd's merges and the sessions it started or ended for an
// issue, before e is applied. Touches older than the wake margin, or the
// echo window when that is longer, are dropped.
func (st *LogState) noteTouch(e Event) {
	thread := ""
	switch {
	case e.Kind == KindSession && e.Runtime != nil && e.Runtime.Action == SessionEnd:
		if r, ok := st.Runtime[e.Session]; ok {
			thread = st.refThread(r.Repository, "#", r.Issue)
		}
	case e.Kind == KindApproval && e.Approval != nil:
		n, _ := strconv.Atoi(strings.TrimLeft(e.Approval.Ref, "!#"))
		thread = st.refThread(e.Approval.Repository, "!", n)
	case e.Kind == KindAction && e.Act != nil && e.Act.Action == ActMerge:
		n, _ := strconv.Atoi(strings.TrimLeft(e.Act.Ref, "!#"))
		thread = st.refThread(e.Act.Repository, "!", n)
	case e.Kind == KindSession && e.Runtime != nil && e.Runtime.Issue > 0:
		thread = st.refThread(e.Runtime.Repository, "#", e.Runtime.Issue)
	}
	if thread == "" {
		return
	}
	if st.Touched == nil {
		st.Touched = map[string][]time.Time{}
	}
	keep := max(wakeMargin, st.cfg.Policies.Coordinator.Echo.Duration)
	for k, ats := range st.Touched {
		var live []time.Time
		for _, at := range ats {
			if !at.Before(e.Time.Add(-keep)) {
				live = append(live, at)
			}
		}
		if live == nil {
			delete(st.Touched, k)
		} else {
			st.Touched[k] = live
		}
	}
	st.Touched[thread] = append(st.Touched[thread], e.Time)
}

// wakeThreads are the threads of the items a wake covered, from its keys.
func wakeThreads(keys []string) []string {
	var out []string
	for _, k := range keys {
		if !strings.HasPrefix(k, "item: ") {
			continue
		}
		id := strings.TrimPrefix(k, "item: ")
		if i := strings.LastIndexByte(id, '@'); i >= 0 {
			id = id[:i] // the key's own time
		}
		out = append(out, threadOf(id))
	}
	return out
}

// turnEnd is when the coordinator's turn for the wake at at ended: the
// first summary after its pointer's prompt, or after at when no prompt
// was read, but no later than the stall after at, as handled has it, nor
// the next wake. A turn that left no summary, or ran without transcripts,
// does not keep its threads quiet past that.
func (t *Track) turnEnd(at time.Time, stall time.Duration) time.Time {
	end := at.Add(stall)
	for _, w := range t.Wakes {
		if w.At.After(at) && w.At.Before(end) {
			end = w.At
		}
	}
	from := at
	for _, tn := range t.Turns {
		if tn.Role == TurnPrompt && !tn.At.Before(at) {
			from = tn.At
			break
		}
	}
	for _, tn := range t.Turns {
		if tn.Role == TurnSummary && tn.At.After(from) && tn.At.Before(end) {
			return tn.At
		}
	}
	return end
}

// echo says whether it is the coordinator's own: written by one of its
// logins, or a discussion that moved within echo of its act on the
// thread. A discussion names no author, so an outsider's move inside the
// window is taken for the coordinator's too.
func (s *Supervisor) echo(co *Coordinator, t *Track, it *Item) bool {
	for _, l := range co.Logins {
		if it.Author != "" && strings.EqualFold(it.Author, l) {
			return true
		}
	}
	if it.Kind != ItemDiscussion {
		return false
	}
	win := s.cfg.Policies.Coordinator.Echo.Duration
	at := it.SourceTime
	if at.IsZero() {
		at = it.FirstSeen
	}
	thread := threadOf(it.ID)
	for _, touched := range s.st.Touched[thread] {
		if !at.Before(touched.Add(-echoSkew)) && !at.After(touched.Add(win)) {
			return true
		}
	}
	stall := s.cfg.Policies.Nudge.Stall.Duration
	for _, w := range t.Wakes {
		if w.Taken && contains(w.Threads, thread) && !at.Before(w.At) && !at.After(t.turnEnd(w.At, stall).Add(win)) {
			return true
		}
	}
	return false
}
