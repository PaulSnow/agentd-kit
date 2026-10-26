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

// noteTouch records the threads e acts on for a coordinator: its
// approvals, agentd's merges and the sessions it started or ended for an
// issue. Touches older than the wake margin are dropped.
func (st *LogState) noteTouch(e Event) {
	thread := ""
	switch {
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
		st.Touched = map[string]time.Time{}
	}
	for k, at := range st.Touched {
		if at.Before(e.Time.Add(-wakeMargin)) {
			delete(st.Touched, k)
		}
	}
	if e.Time.After(st.Touched[thread]) {
		st.Touched[thread] = e.Time
	}
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

// turnEnd is when the coordinator's turn after at ended: its first
// summary after at, else the next wake, else now, the turn still running.
func (t *Track) turnEnd(at, now time.Time) time.Time {
	for _, tn := range t.Turns {
		if tn.Role == TurnSummary && tn.At.After(at) {
			return tn.At
		}
	}
	for _, w := range t.Wakes {
		if w.At.After(at) {
			return w.At
		}
	}
	return now
}

// echo says whether it is the coordinator's own: written by one of its
// logins, or a discussion that moved within echo of its act on the
// thread.
func (s *Supervisor) echo(co *Coordinator, t *Track, it *Item, now time.Time) bool {
	if it.Author != "" && contains(co.Logins, it.Author) {
		return true
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
	// The forge's clock and agentd's differ a little, and a merge is
	// recorded after the forge made it: the window is either side.
	if touched, ok := s.st.Touched[thread]; ok && !at.Before(touched.Add(-win)) && !at.After(touched.Add(win)) {
		return true
	}
	for _, w := range t.Wakes {
		if w.Taken && contains(w.Threads, thread) && !at.Before(w.At) && !at.After(t.turnEnd(w.At, now).Add(win)) {
			return true
		}
	}
	return false
}
