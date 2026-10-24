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
}

// Summarize folds the event log for mode through the one reader into one
// row per session: every configured session, whether or not it has events
// yet, and any other session the log names.
func Summarize(cfg *Config, events []Event, mode string) ([]SessionStatus, map[string]any) {
	st := NewLogState(cfg, mode).Fold(events)
	meta := map[string]any{"events": st.Events, "mode": mode}
	if !st.Started.IsZero() {
		meta["started"] = st.Started
	}
	if !st.Stopped.IsZero() {
		meta["stopped"] = st.Stopped
	}
	for _, n := range cfg.SessionNames() {
		st.track(n)
	}
	pending := map[string][]string{}
	for _, e := range st.CutShort() {
		pending[e.Session] = append(pending[e.Session], e.Act.Action+" "+e.Act.Key+" (no outcome yet: running, or cut short)")
	}
	out := make([]SessionStatus, 0, len(st.Tracks))
	for name, t := range st.Tracks {
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
