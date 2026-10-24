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
	// Digest summarizes the entries anyone may see; OwnerDigest, for the
	// owner only, summarizes them all.
	Digest      string `json:"digest"`
	OwnerDigest string `json:"owner_digest"`
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

// ReportQuestion is a session's open Questions, the STATUS.md text last
// posted to the owner. Questions are always for the owner only.
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
	filtered   map[string]bool // escalation keys whose text was withheld when posted
	deliveries []reportDelivery
}

// reportDelivery is a delivery and the repository it went to.
type reportDelivery struct {
	ReportDelivery
	repository string
}

// Questions escalations and updates start with one of these, after the
// session's name, ahead of the text from STATUS.md.
var questionPrefixes = []string{" is blocked on you. Questions from STATUS.md:\n", " changed its open Questions in STATUS.md; they now read:\n"}

// questionText is the STATUS.md text a Questions escalation carried.
func questionText(session, message string) string {
	for _, p := range questionPrefixes {
		if strings.HasPrefix(message, session+p) {
			return message[len(session+p):]
		}
	}
	return message
}

// BuildReport folds events into the report for mode: the events of that
// mode and the owner's, as a run of that mode replays them, so a live
// report never shows what a shadow run only would have done. Only
// configured sessions and repositories are reported. Every text is
// screened against the confidential patterns as configured now: a matching
// first line is left out, and any other entry with matching text, its
// session's name or its repository's, is left out whole.
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
	// deliveredTo is the repository of the deliver spec a delivered key,
	// session:worktree, names.
	deliveredTo := func(session, key string) string {
		for _, d := range cfg.Sessions[session].Deliver {
			if session+":"+d.Worktree == key {
				return d.Repository
			}
		}
		return ""
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
		if _, ok := cfg.Sessions[e.Session]; !ok {
			continue // a session no longer configured is not reported
		}
		if m := eventMode(e, started); m != mode && m != ModeOwner {
			continue
		}
		rs := sessions[e.Session]
		if rs == nil {
			rs = &reportSession{filtered: map[string]bool{}}
			sessions[e.Session] = rs
		}
		deliver := func(ref string) {
			repo := detailString(e, "repository")
			if repo == "" {
				repo = deliveredTo(e.Session, detailString(e, "delivered"))
			}
			rs.deliveries = append(rs.deliveries, reportDelivery{ReportDelivery{Session: e.Session, Ref: ref, At: e.Time}, repo})
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
				ref := detailString(e, "ref")
				if ref == "" && e.Kind == KindWould {
					ref = "would-open"
				}
				if ref != "" {
					deliver(ref)
				}
			case ActEscalate, ActUpdate:
				if key := detailString(e, "key"); strings.HasPrefix(key, "questions:") {
					rs.question, rs.askedAt = questionText(e.Session, detailString(e, "message")), e.Time
					if rs.filtered[key] {
						rs.question = "" // the generic message, not the questions
					}
				}
			}
		case KindSkip:
			if k := detailString(e, "filtered"); k != "" {
				rs.filtered[k] = true
			}
			if detailString(e, "delivered") != "" {
				// The outcome is the request found already open or
				// merged, or failed or nothing, which deliver nothing.
				if ref := detailString(e, "outcome"); ref != "" && ref != "failed" && ref != "nothing" {
					deliver(ref)
				}
			}
		}
	}

	repos := map[string]*ReportRepo{}
	// group is the report's entry for the configured repository name, nil
	// when its name or identity matches a confidential pattern.
	group := func(name string) *ReportRepo {
		key, ownerOnly := "", true
		r, ok := cfg.Repositories[name]
		if ok {
			key, ownerOnly = repoKey(r.Forge, r.Project), r.OwnerOnly
		} else {
			name = ""
		}
		if secret(name, key) {
			return nil
		}
		rr := repos[key]
		if rr == nil {
			rr = &ReportRepo{Repository: key, Name: name, OwnerOnly: ownerOnly,
				Cases: []ReportCase{}, Deliveries: []ReportDelivery{}, Questions: []ReportQuestion{}}
			repos[key] = rr
		}
		return rr
	}
	names := make([]string, 0, len(sessions))
	for n := range sessions {
		names = append(names, n)
	}
	sort.Strings(names)
	var public, all digestCounts
	count := func(state string, delivered, asked int, ownerOnly bool) {
		all.add(state, delivered, asked)
		if !ownerOnly {
			public.add(state, delivered, asked)
		}
	}
	for _, n := range names {
		rs := sessions[n]
		if secret(n) {
			continue
		}
		rr := group(cfg.Sessions[n].Repository)
		if rr != nil && rs.state != "" {
			c := ReportCase{Session: n, State: string(rs.state), Since: rs.since, Head: rs.head, OwnerOnly: rr.OwnerOnly}
			if secret(c.Head) {
				c.Head = ""
			}
			rr.Cases = append(rr.Cases, c)
			count(c.State, 0, 0, c.OwnerOnly)
		}
		for _, d := range rs.deliveries {
			dr := group(d.repository)
			if dr == nil || secret(d.Ref) {
				continue
			}
			d.OwnerOnly = dr.OwnerOnly
			dr.Deliveries = append(dr.Deliveries, d.ReportDelivery)
			count("", 1, 0, d.OwnerOnly)
		}
		if rr != nil && rs.asks && rs.question != "" && !secret(rs.question) {
			rr.Questions = append(rr.Questions, ReportQuestion{Session: n, Since: rs.askedAt, Text: rs.question, OwnerOnly: true})
			count("", 0, 1, true)
		}
	}
	report := Report{Schema: ReportSchema, WrittenAt: now, Host: host, Mode: mode, Repositories: []ReportRepo{},
		Digest: public.digest(), OwnerDigest: all.digest()}
	for _, rr := range repos {
		if len(rr.Cases)+len(rr.Deliveries)+len(rr.Questions) > 0 {
			report.Repositories = append(report.Repositories, *rr)
		}
	}
	sort.Slice(report.Repositories, func(i, j int) bool {
		return report.Repositories[i].Repository < report.Repositories[j].Repository
	})
	return report
}

// digestCounts is what a digest summarizes.
type digestCounts struct {
	states    map[State]int
	delivered int
	asked     int
}

// add counts a case in state (when not ""), deliveries and questions.
func (d *digestCounts) add(state string, delivered, asked int) {
	if d.states == nil {
		d.states = map[State]int{}
	}
	if state != "" {
		d.states[State(state)]++
	}
	d.delivered += delivered
	d.asked += asked
}

// digest is one plain summary: sessions by state, the deliveries, and the
// questions waiting on the owner.
func (d digestCounts) digest() string {
	total := 0
	var parts []string
	for _, st := range []State{Working, Starting, Idle, BlockedOnOwner, UsageLimited, Done, Failed, CrashLooping} {
		if d.states[st] > 0 {
			total += d.states[st]
			parts = append(parts, fmt.Sprintf("%d %s", d.states[st], st))
		}
	}
	line := fmt.Sprintf("%d sessions", total)
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, ", ")
	}
	return fmt.Sprintf("%s; %s delivered; %s waiting on the owner.", line, plural(d.delivered, "request"), plural(d.asked, "question"))
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
