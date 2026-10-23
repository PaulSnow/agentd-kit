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
}

// Summarize folds the event log into one row per session.
func Summarize(events []Event) ([]SessionStatus, map[string]any) {
	rows := map[string]*SessionStatus{}
	meta := map[string]any{"events": len(events)}
	row := func(name string) *SessionStatus {
		r, ok := rows[name]
		if !ok {
			r = &SessionStatus{Name: name}
			rows[name] = r
		}
		return r
	}
	for _, e := range events {
		if e.Session == "" {
			switch e.Kind {
			case KindStart:
				meta["mode"] = detailString(e, "mode")
				meta["started"] = e.Time
				delete(meta, "stopped")
			case KindStop:
				meta["stopped"] = e.Time
			}
			continue
		}
		r := row(e.Session)
		switch e.Kind {
		case KindObserve:
			r.LastSeen = e.Time
			r.Head = detailString(e, "head")
			r.Reason = detailString(e, "reason")
			if st := detailString(e, "state"); st != "" && r.State == "" {
				r.State, r.Since = State(st), e.Time
			}
		case KindTransition:
			r.State, r.Since = State(detailString(e, "to")), e.Time
			r.Reason = detailString(e, "reason")
		case KindAction, KindWould:
			if e.Kind == KindWould {
				r.Would++
			}
			switch detailString(e, "action") {
			case ActRestart:
				r.Restarts++
			case ActNudge:
				r.Nudges++
			case ActEscalate:
				r.Escalations++
			case ActOpen:
				ref := detailString(e, "ref")
				if ref == "" {
					ref = "would-open"
				}
				r.Delivered = append(r.Delivered, ref)
			}
		case KindSkip:
			if detailString(e, "delivered") != "" {
				r.Delivered = append(r.Delivered, detailString(e, "outcome"))
			}
		case KindError:
			r.Errors++
		}
	}
	out := make([]SessionStatus, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, meta
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
	fmt.Fprintln(tw, "SESSION\tSTATE\tFOR\tSEEN\tRESTARTS\tNUDGES\tESCALATED\tDELIVERED\tWHY")
	for _, r := range rows {
		delivered := strings.Join(r.Delivered, ",")
		if delivered == "" {
			delivered = "-"
		}
		why := r.Reason
		if r.Errors > 0 {
			why = fmt.Sprintf("%s (%d errors)", why, r.Errors)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\n",
			r.Name, r.State, ago(now, r.Since), ago(now, r.LastSeen), r.Restarts, r.Nudges, r.Escalations, delivered, why)
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
