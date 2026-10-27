package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"time"
)

// agentd context (#60): a repository's cards, and how agents use them.

// contextFollowUp is how long after a state or find call a history call
// from the same agent counts as the same question going to history.
const contextFollowUp = 5 * time.Minute

// ContextStats is how a repository's agents used the context tools, and
// what the curators and the expert cost, over a span of the log.
type ContextStats struct {
	Repository string `json:"repository"`
	Since      string `json:"since"`
	// Questions start at a state or find call, or at a history call no
	// state call led to; FromState are those no history call followed
	// within contextFollowUp.
	Questions int            `json:"questions"`
	FromState int            `json:"from_state"`
	StateRate float64        `json:"state_rate"`
	Calls     map[string]int `json:"calls"`
	HistoryBy map[string]int `json:"history_by_why"`
	// Curators: runs, refusals, failures, and tokens a run (median, max).
	Curations     int   `json:"curations"`
	Refused       int   `json:"refused"`
	Failed        int   `json:"failed"`
	CuratorMedian int64 `json:"curator_tokens_median"`
	CuratorMax    int64 `json:"curator_tokens_max"`
	CuratorTotal  int64 `json:"curator_tokens_total"`
	Cards         int   `json:"cards"`
	ExpertCalls   int   `json:"expert_calls"`
	ExpertTokens  int64 `json:"expert_tokens"`
}

// contextStats measures events for repo from since on.
func contextStats(events []Event, repo string, since time.Time) ContextStats {
	s := ContextStats{Repository: repo, Since: since.UTC().Format(time.RFC3339), Calls: map[string]int{}, HistoryBy: map[string]int{}}
	type open struct {
		at      time.Time
		history bool
	}
	asking := map[string]*open{} // each agent's question in progress
	closeQ := func(q *open) {
		if q != nil && !q.history {
			s.FromState++
		}
	}
	var curator []int64
	cards := map[string]bool{}
	for _, e := range events {
		if e.Time.Before(since) {
			continue
		}
		if p := e.Card; p != nil && p.Repository == repo {
			s.Curations++
			switch {
			case p.Refused != "":
				s.Refused++
			case p.Error != "":
				s.Failed++
			case p.Card != nil:
				cards[p.Topic] = true
			}
			if p.Usage != nil {
				curator = append(curator, p.Usage.tokens())
				s.CuratorTotal += p.Usage.tokens()
			}
		}
		q := e.Query
		if q == nil || q.Repository != repo || q.Error != "" {
			continue
		}
		s.Calls[q.Tool]++
		switch q.Tool {
		case ToolState, ToolFind:
			closeQ(asking[e.Session])
			asking[e.Session] = &open{at: e.Time}
			s.Questions++
		case ToolHistory:
			s.HistoryBy[q.Why]++
			if a := asking[e.Session]; a != nil && e.Time.Sub(a.at) <= contextFollowUp {
				a.history = true
			} else {
				closeQ(a)
				asking[e.Session] = &open{at: e.Time, history: true}
				s.Questions++
			}
		case ToolAskExpert:
			s.ExpertCalls++
			if q.Usage != nil {
				s.ExpertTokens += q.Usage.tokens()
			}
		}
	}
	for _, a := range asking {
		closeQ(a)
	}
	if s.Questions > 0 {
		s.StateRate = float64(s.FromState) / float64(s.Questions)
	}
	if len(curator) > 0 {
		sort.Slice(curator, func(i, j int) bool { return curator[i] < curator[j] })
		s.CuratorMedian, s.CuratorMax = curator[len(curator)/2], curator[len(curator)-1]
	}
	s.Cards = len(cards)
	return s
}

// contextCmd is agentd context: stats measures a repository's use of the
// context tools; card prints a topic's card, or the repository's.
func contextCmd(args []string, w io.Writer) error {
	usage := errors.New("usage: agentd context [--config PATH] [--log PATH] stats REPO [--since 24h] | card REPO [TOPIC]")
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return usage
	}
	if _, ok := cfg.Repositories[rest[1]]; !ok {
		return fmt.Errorf("no repository %q", rest[1])
	}
	log, err := OpenLog(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	events, _, err := log.Load()
	if err != nil {
		return err
	}
	switch rest[0] {
	case "stats":
		sub := flag.NewFlagSet("context stats", flag.ContinueOnError)
		span := sub.Duration("since", 24*time.Hour, "how far back to measure")
		if err := sub.Parse(rest[2:]); err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, compact(contextStats(events, rest[1], time.Now().Add(-*span))))
		return err
	case "card":
		if len(rest) > 3 {
			return usage
		}
		cs := NewContextStore(projected(cfg, events))
		for _, e := range events {
			cs.Apply(e)
		}
		topic := TopicRepo
		if len(rest) == 3 {
			topic = rest[2]
		}
		c := cs.Card(rest[1], topic)
		if c == nil {
			return fmt.Errorf("%s has no card for %s; history holds %d records on it", rest[1], topic, len(cs.History(rest[1]).OnTopic(topic)))
		}
		_, err = fmt.Fprintln(w, compact(c))
		return err
	}
	return usage
}
