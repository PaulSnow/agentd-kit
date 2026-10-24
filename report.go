package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ReportSchema names the report's format. The tracking site reads only a
// schema it knows.
const ReportSchema = "agentd-report/1"

// Report is what agentd publishes through the tracking site: each
// repository's cases, what was delivered, and what waits on the owner,
// rebuilt from the event log alone. Nothing matching a confidential
// pattern is in it; an entry that would carry such text is left out whole,
// so the site never has it to hide. OwnerOnly marks an entry the site
// shows to the owner alone.
type Report struct {
	Schema       string       `json:"schema"`
	WrittenAt    time.Time    `json:"written_at"`
	Host         string       `json:"host"`
	Mode         string       `json:"mode"`
	Repositories []ReportRepo `json:"repositories"`
	Digest       string       `json:"digest"`
}

// ReportRepo is one repository's part of the report. Repository is its
// identity in the tracking system, platform/path; sessions that name no
// configured repository are grouped under "" and are for the owner only,
// since the site cannot tell who may see them.
type ReportRepo struct {
	Repository string           `json:"repository"`
	Name       string           `json:"name"`
	OwnerOnly  bool             `json:"owner_only"`
	Cases      []ReportCase     `json:"cases"`
	Deliveries []ReportDelivery `json:"deliveries"`
	Questions  []ReportQuestion `json:"questions"`
}

// ReportCase is one session's state, and the first line of its STATUS.md.
type ReportCase struct {
	Session   string    `json:"session"`
	State     string    `json:"state"`
	Since     time.Time `json:"since"`
	Head      string    `json:"head,omitempty"`
	OwnerOnly bool      `json:"owner_only"`
}

// ReportDelivery is a merge or pull request agentd opened, found, or was
// told of (mark-delivered).
type ReportDelivery struct {
	Session   string    `json:"session"`
	Ref       string    `json:"ref"`
	At        time.Time `json:"at"`
	OwnerOnly bool      `json:"owner_only"`
}

// ReportQuestion is a session's open Questions, as last posted to the
// owner. Questions are always for the owner only.
type ReportQuestion struct {
	Session   string    `json:"session"`
	Since     time.Time `json:"since"`
	Text      string    `json:"text"`
	OwnerOnly bool      `json:"owner_only"`
}

// reportSession is what BuildReport folds the log into for one session.
type reportSession struct {
	state      State
	since      time.Time
	head       string
	asks       bool
	question   string
	askedAt    time.Time
	deliveries []ReportDelivery
}

// BuildReport folds events into the report for mode: the events of that
// mode and the owner's, as a run of that mode replays them, so a live
// report never shows what a shadow run only would have done.
func BuildReport(cfg *Config, events []Event, mode, host string, now time.Time) Report {
	var confidential []*regexp.Regexp
	for _, p := range cfg.ConfidentialPatterns {
		confidential = append(confidential, regexp.MustCompile(p))
	}
	secret := func(texts ...string) bool {
		for _, re := range confidential {
			for _, t := range texts {
				if re.MatchString(t) {
					return true
				}
			}
		}
		return false
	}
	sessions := map[string]*reportSession{}
	started := ""
	for _, e := range events {
		if e.Session == "" {
			if e.Kind == KindStart {
				started = detailString(e, "mode")
			}
			continue
		}
		if m := eventMode(e, started); m != mode && m != ModeOwner {
			continue
		}
		rs := sessions[e.Session]
		if rs == nil {
			rs = &reportSession{}
			sessions[e.Session] = rs
		}
		switch e.Kind {
		case KindObserve:
			if st := detailString(e, "state"); st != "" && rs.state == "" {
				rs.state, rs.since = State(st), e.Time
			}
			rs.head = detailString(e, "head")
			asks, _ := e.Details["questions"].(bool)
			done, _ := e.Details["done"].(bool)
			if rs.asks = asks && !done; !rs.asks {
				rs.question = ""
			}
		case KindTransition:
			rs.state, rs.since = State(detailString(e, "to")), e.Time
		case KindAction, KindWould:
			if e.Kind == KindWould && mode != ModeShadow {
				continue
			}
			switch detailString(e, "action") {
			case ActOpen:
				if ref := detailString(e, "ref"); ref != "" {
					rs.deliveries = append(rs.deliveries, ReportDelivery{Session: e.Session, Ref: ref, At: e.Time})
				}
			case ActEscalate, ActUpdate:
				if strings.HasPrefix(detailString(e, "key"), "questions:") {
					rs.question, rs.askedAt = detailString(e, "message"), e.Time
				}
			}
		case KindSkip:
			if detailString(e, "delivered") != "" {
				// The outcome is the request found already open or
				// merged, or failed or nothing, which deliver nothing.
				if ref := detailString(e, "outcome"); ref != "" && ref != "failed" && ref != "nothing" {
					rs.deliveries = append(rs.deliveries, ReportDelivery{Session: e.Session, Ref: ref, At: e.Time})
				}
			}
		}
	}

	repos := map[string]*ReportRepo{}
	names := make([]string, 0, len(sessions))
	for n := range sessions {
		names = append(names, n)
	}
	sort.Strings(names)
	counts := map[State]int{}
	var delivered, asked int
	for _, n := range names {
		rs := sessions[n]
		key, name, ownerOnly := "", "", true
		if r, ok := cfg.Repositories[cfg.Sessions[n].Repository]; ok {
			key, name, ownerOnly = repoKey(r.Forge, r.Project), cfg.Sessions[n].Repository, r.OwnerOnly
		}
		rr := repos[key]
		if rr == nil {
			rr = &ReportRepo{Repository: key, Name: name, OwnerOnly: ownerOnly,
				Cases: []ReportCase{}, Deliveries: []ReportDelivery{}, Questions: []ReportQuestion{}}
			repos[key] = rr
		}
		if rs.state != "" {
			c := ReportCase{Session: n, State: string(rs.state), Since: rs.since, Head: rs.head, OwnerOnly: ownerOnly}
			if secret(c.Head) {
				c.Head = ""
			}
			rr.Cases = append(rr.Cases, c)
			counts[rs.state]++
		}
		for _, d := range rs.deliveries {
			if !secret(d.Ref) {
				d.OwnerOnly = ownerOnly
				rr.Deliveries = append(rr.Deliveries, d)
				delivered++
			}
		}
		if rs.asks && rs.question != "" && !secret(rs.question) {
			rr.Questions = append(rr.Questions, ReportQuestion{Session: n, Since: rs.askedAt, Text: rs.question, OwnerOnly: true})
			asked++
		}
	}
	report := Report{Schema: ReportSchema, WrittenAt: now, Host: host, Mode: mode, Repositories: []ReportRepo{}}
	for _, rr := range repos {
		report.Repositories = append(report.Repositories, *rr)
	}
	sort.Slice(report.Repositories, func(i, j int) bool {
		return report.Repositories[i].Repository < report.Repositories[j].Repository
	})
	report.Digest = reportDigest(counts, delivered, asked)
	return report
}

// reportDigest is the report's one plain summary: sessions by state, the
// deliveries, and the questions waiting on the owner.
func reportDigest(counts map[State]int, delivered, asked int) string {
	total := 0
	var parts []string
	for _, st := range []State{Working, Starting, Idle, BlockedOnOwner, UsageLimited, Done, Failed, CrashLooping} {
		if counts[st] > 0 {
			total += counts[st]
			parts = append(parts, fmt.Sprintf("%d %s", counts[st], st))
		}
	}
	line := fmt.Sprintf("%d sessions", total)
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, ", ")
	}
	return fmt.Sprintf("%s; %s delivered; %s waiting on the owner.", line, plural(delivered, "request"), plural(asked, "question"))
}

// plural is n with noun, made plural unless n is one.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// WriteReport writes r to path atomically: to a temporary file in the same
// directory, synced, then renamed over path, so the site reads the old
// report or the new one and never part of either. The file is readable by
// its owner only, since it holds the owner's questions.
func WriteReport(path string, r Report) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".report-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("writing report %s: %w", path, err)
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// reportCmd writes the report for the tracking site from the event log.
func reportCmd(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	out := fs.String("out", "", "where to write report.json")
	host := fs.String("host", "", "the host named in the report (default: this machine's name)")
	shadow := fs.Bool("shadow", false, "report what the shadow run saw and would have done")
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	if *out == "" {
		return errors.New("usage: agentd report --out PATH")
	}
	if *host == "" {
		if *host, err = os.Hostname(); err != nil {
			return err
		}
	}
	events, _, err := ReadLog(cfg.EventLog)
	if err != nil {
		return err
	}
	mode := ModeLive
	if *shadow {
		mode = ModeShadow
	}
	return WriteReport(expandPath(*out, ""), BuildReport(cfg, events, mode, *host, time.Now()))
}
