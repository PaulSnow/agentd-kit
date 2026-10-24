package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// SessionStatus is one row of the status table, derived from the event log.
type SessionStatus struct {
	Name        string
	State       State
	Since       time.Time
	LastSeen    time.Time
	Head        string
	Reason      string
	Restarts    int
	Nudges      int
	Escalations int
	Delivered   []string
	Would       int
	Errors      int
	Blocked     []string // why a delivery is declined, per worktree not yet delivered
	GaveUp      []string // escalations given up on
	CutShort    []string // actions a crash of agentd cut short, and pending intents
	Context     int64    // the current transcript's context in tokens, or -1 when unknown
	Spend       int64    // input-equivalent tokens over every transcript, or -1 when unknown
	Guessed     bool     // a figure rests on a transcript agentd did not launch
	Rotations   int
	Stopped     string // why the budget stopped it
	// A coordinator: its role and repository, its wakes, its spend per
	// wake (-1 when unknown), and its latest wake.
	Role       string
	Repository string
	Wakes      int
	WakeTokens int64
	LastWake   time.Time
	WakeReason string
}

// Summarize folds the event log for mode through the one reader into one
// row per session: every configured session, whether or not it has events
// yet, and any other session the log names.
func Summarize(cfg *Config, events []Event, mode string) ([]SessionStatus, map[string]any) {
	return SummarizeState(cfg, NewLogState(cfg, mode).Fold(events))
}

// SummarizeState is Summarize over a state already folded, which it reads
// and does not change.
func SummarizeState(cfg *Config, st *LogState) ([]SessionStatus, map[string]any) {
	mode := st.Mode
	meta := map[string]any{"events": st.Events, "mode": mode}
	if !st.Started.IsZero() {
		meta["started"] = st.Started
	}
	if !st.Stopped.IsZero() {
		meta["stopped"] = st.Stopped
	}
	// A source no longer polled is left out.
	polls := map[string]PollRecord{}
	for src, p := range st.Polls {
		if src == SourceGitHub && cfg.Poll.GitHub || src == SourceGitLab && cfg.Poll.GitLab {
			polls[src] = p
		}
	}
	if len(polls) > 0 {
		meta["polls"] = polls
		meta["poll_stale"] = pollHeartbeat + 2*cfg.Poll.Every.Duration
	}
	if cfg.Poll.CI && len(st.CI) > 0 {
		meta["ci"] = st.CI
	}
	tracks := copyMap(st.Tracks)
	for _, n := range cfg.SessionNames() {
		if tracks[n] == nil {
			tracks[n] = newTrack()
		}
	}
	pending := map[string][]string{}
	for _, e := range st.CutShort() {
		pending[e.Session] = append(pending[e.Session], e.Act.Action+" "+e.Act.Key+" (no outcome yet: running, or cut short)")
	}
	out := make([]SessionStatus, 0, len(tracks))
	for name, t := range tracks {
		r := SessionStatus{Name: name, State: t.State, Since: t.Since, LastSeen: t.LastSeen, Head: t.Head, Reason: t.Reason,
			Restarts: t.RestartCount, Nudges: t.NudgeCount, Escalations: t.Escalations, Delivered: t.DeliveredRefs,
			Would: t.Would, Errors: t.Errors, GaveUp: t.GaveUp, CutShort: append(append([]string(nil), t.CutShort...), pending[name]...),
			Context: -1, Spend: -1, Guessed: t.guessed(), Rotations: t.Rotations, Stopped: t.Stopped}
		if u, ok := t.current(); ok {
			r.Context = u.Context
		}
		if sp, ok := t.spend(mode == ModeLive); ok {
			r.Spend = sp
		}
		if sess := cfg.Sessions[name]; sess.Role == RoleCoordinator {
			r.Role, r.Repository, r.Wakes, r.WakeTokens = sess.Role, sess.Repository, t.WakeCount, -1
			if n, ok := t.wakeTokens(mode == ModeLive); ok {
				r.WakeTokens = n
			}
			if w, ok := t.lastWake(); ok {
				r.LastWake, r.WakeReason = w.At, w.Reason
			}
		}
		for _, d := range cfg.Sessions[name].Deliver {
			if why := t.Blocked[d.Worktree]; why != "" && t.Delivered[name+":"+d.Worktree] == "" {
				r.Blocked = append(r.Blocked, why)
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, meta
}

// statusMode is the mode status shows by default: the latest run's.
func statusMode(events []Event) string {
	m := ModeLive
	for _, e := range events {
		if e.Kind == KindStart {
			m = e.Mode
		}
	}
	return m
}

// PrintStatus writes the table.
func PrintStatus(w io.Writer, rows []SessionStatus, meta map[string]any, now time.Time) {
	if mode, ok := meta["mode"].(string); ok {
		line := fmt.Sprintf("agentd %s", mode)
		if st, ok := meta["started"].(time.Time); ok {
			line += ", started " + st.Local().Format("2006-01-02 15:04")
		}
		if st, ok := meta["stopped"].(time.Time); ok {
			line += ", stopped " + st.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "%s; %d events\n", line, meta["events"])
	}
	// A quiet poll makes no event, but a source still polled has a record
	// at least every pollHeartbeat: an older one means no one is polling
	// it, so it is unknown, never "nothing new".
	// A request's CI as the watcher last saw it; once agentd has stopped,
	// no one is watching, so it is unknown.
	if ci, ok := meta["ci"].(map[string]CIRecord); ok {
		stopped, _ := meta["stopped"].(time.Time)
		keys := make([]string, 0, len(ci))
		for k := range ci {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			c := ci[k]
			line := fmt.Sprintf("ci %s %s (%s): %s", c.Repository, c.Ref, c.Session, c.State)
			switch {
			case !stopped.IsZero() && c.State != CIMerged && c.State != CIClosed:
				line = fmt.Sprintf("ci %s %s (%s): unknown, agentd stopped %s ago; last %s", c.Repository, c.Ref, c.Session, ago(now, stopped), c.State)
			case c.State == CIUnknown:
				line += ": " + c.Error
			case c.State == CIFailed:
				line += fmt.Sprintf(" (%s: %s)", c.Verdict, strings.Join(c.Jobs, ", "))
			}
			fmt.Fprintf(w, "%s, since %s ago\n", line, ago(now, c.At))
		}
	}
	if polls, ok := meta["polls"].(map[string]PollRecord); ok {
		stale, _ := meta["poll_stale"].(time.Duration)
		stopped, _ := meta["stopped"].(time.Time)
		srcs := make([]string, 0, len(polls))
		for src := range polls {
			srcs = append(srcs, src)
		}
		sort.Strings(srcs)
		for _, src := range srcs {
			p := polls[src]
			switch {
			case !p.OK:
				fmt.Fprintf(w, "poll %s: unknown since %s ago (%d calls in %d polls): %s\n", src, ago(now, p.At), p.Calls, p.Polls, p.Error)
			case !stopped.IsZero():
				fmt.Fprintf(w, "poll %s: unknown, agentd stopped %s ago\n", src, ago(now, stopped))
			case stale > 0 && now.Sub(p.At) > stale:
				fmt.Fprintf(w, "poll %s: unknown, no poll recorded for %s\n", src, ago(now, p.At))
			default:
				fmt.Fprintf(w, "poll %s: ok, last record %s ago (%d calls in %d polls, %d new items)\n", src, ago(now, p.At), p.Calls, p.Polls, p.Items)
			}
		}
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tSTATE\tFOR\tSEEN\tCONTEXT\tSPEND\tRESTARTS\tROTATED\tNUDGES\tESCALATED\tDELIVERED\tWHY")
	for _, r := range rows {
		delivered := strings.Join(r.Delivered, ",")
		if delivered == "" {
			delivered = "-"
		}
		why := r.Reason
		if r.Errors > 0 {
			why = fmt.Sprintf("%s (%d errors)", why, r.Errors)
		}
		for _, b := range r.Blocked {
			why += "; delivery last declined: " + b
		}
		for _, g := range r.GaveUp {
			why += "; gave up escalating " + g
		}
		for _, c := range r.CutShort {
			why += "; no outcome: " + c
		}
		if r.Guessed {
			why += "; tokens from the newest transcript in its directory, a guess"
		}
		if r.Role == RoleCoordinator {
			per := "tokens per wake unknown"
			if r.WakeTokens >= 0 {
				per = tokens(r.WakeTokens) + " tokens per wake"
			}
			why += fmt.Sprintf("; coordinator of %s: wakes %d, %s", r.Repository, r.Wakes, per)
			if !r.LastWake.IsZero() {
				why += fmt.Sprintf(", quiet %s since the last (%s)", ago(now, r.LastWake), r.WakeReason)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\n",
			r.Name, r.State, ago(now, r.Since), ago(now, r.LastSeen), tokens(r.Context), tokens(r.Spend), r.Restarts, r.Rotations,
			r.Nudges, r.Escalations, delivered, why)
	}
	tw.Flush()
}

func ago(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// tokens abbreviates a token count, or "?" when it is unknown.
func tokens(n int64) string {
	switch {
	case n < 0:
		return "?"
	case n < 10000:
		return fmt.Sprint(n)
	case n < 10000000:
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprintf("%dM", n/1000000)
}
