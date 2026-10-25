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
	Items      []ReportItem     `json:"items"`
}

// ReportItem is a new item (#18): an issue, a request, a discussion or a
// mail finding, with its triage while it holds for the item's text. Only
// these fields are published; who wrote it is not. An item from mail is
// for the owner only, and so is one of a repository agentd is not
// configured for, since the site cannot tell who may see it.
type ReportItem struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Title      string    `json:"title"`
	Link       string    `json:"link,omitempty"`
	SourceTime time.Time `json:"source_time"`
	FirstSeen  time.Time `json:"first_seen"`
	Seen       bool      `json:"seen"`
	NeedsOwner string    `json:"needs_owner,omitempty"`
	Urgency    string    `json:"urgency,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	OwnerOnly  bool      `json:"owner_only"`
}

// allOwnerOnly says whether every entry of rr is for the owner only.
func allOwnerOnly(rr ReportRepo) bool {
	for _, c := range rr.Cases {
		if !c.OwnerOnly {
			return false
		}
	}
	for _, d := range rr.Deliveries {
		if !d.OwnerOnly {
			return false
		}
	}
	for _, it := range rr.Items {
		if !it.OwnerOnly {
			return false
		}
	}
	return true // questions are always the owner's
}

// fromMail says whether an item is derived from mail, by any of the marks
// a mail finding carries: everything from mail is for the owner only.
func fromMail(it ItemPayload) bool {
	return it.OwnerOnly || it.Kind == ItemMailQuestion || it.Kind == ItemMailReport || strings.HasPrefix(it.ID, "mail:")
}

// reportItemWindow is how long an item the owner has seen stays in the
// report; one not yet seen stays until it is.
const reportItemWindow = 7 * 24 * time.Hour

// ReportCase is one session's state, and the first line of its STATUS.md.
type ReportCase struct {
	Session   string    `json:"session"`
	State     string    `json:"state"`
	Since     time.Time `json:"since"`
	Head      string    `json:"head,omitempty"`
	OwnerOnly bool      `json:"owner_only"`
	// A coordinator: its role, its wakes, and its spend per wake when
	// known.
	Role       string `json:"role,omitempty"`
	Wakes      int    `json:"wakes,omitempty"`
	WakeTokens *int64 `json:"wake_tokens,omitempty"`
}

// ReportDelivery is a merge or pull request agentd opened, found, or was
// told of (mark-delivered).
type ReportDelivery struct {
	Session   string    `json:"session"`
	Ref       string    `json:"ref"`
	At        time.Time `json:"at"`
	OwnerOnly bool      `json:"owner_only"`
	// CI is the request's pipeline as the watcher last saw it, its
	// verdict on a failure and the failing jobs; "" when not watched.
	CI        string   `json:"ci,omitempty"`
	CIVerdict string   `json:"ci_verdict,omitempty"`
	CIJobs    []string `json:"ci_jobs,omitempty"`
}

// ReportQuestion is a session's open Questions, the STATUS.md text last
// posted to the owner. Questions are always for the owner only.
type ReportQuestion struct {
	Session   string    `json:"session"`
	Since     time.Time `json:"since"`
	Text      string    `json:"text"`
	OwnerOnly bool      `json:"owner_only"`
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
	st := NewLogState(cfg, mode).Fold(events)
	return ReportFrom(withRuntime(cfg, st.Runtime), st, host, now)
}

// confidentialMatcher says whether any of the texts matches one of the
// configuration's confidential patterns as they are now.
func confidentialMatcher(cfg *Config) func(texts ...string) bool {
	var confidential []*regexp.Regexp
	for _, p := range screenPatterns(cfg) {
		confidential = append(confidential, regexp.MustCompile(p))
	}
	return func(texts ...string) bool {
		for _, re := range confidential {
			for _, t := range texts {
				if re.MatchString(t) {
					return true
				}
			}
		}
		return false
	}
}

// ReportFrom is BuildReport over a state already folded, which it reads
// and does not change, as the page's does.
func ReportFrom(cfg *Config, st *LogState, host string, now time.Time) Report {
	secret := confidentialMatcher(cfg)
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
	mode := st.Mode
	repos := map[string]*ReportRepo{}
	// entry is the report's entry for the repository with key, which the
	// configuration names name ("" when it does not), nil when its name or
	// identity matches a confidential pattern.
	entry := func(key, name string, ownerOnly bool) *ReportRepo {
		if secret(name, key) {
			return nil
		}
		rr := repos[key]
		if rr == nil {
			rr = &ReportRepo{Repository: key, Name: name, OwnerOnly: ownerOnly,
				Cases: []ReportCase{}, Deliveries: []ReportDelivery{}, Questions: []ReportQuestion{}, Items: []ReportItem{}}
			repos[key] = rr
		}
		return rr
	}
	// configured names the configured repository with each key, the first
	// by name when several share one (the key leaves out the host);
	// keyOwnerOnly says whether any of them is for the owner only, and
	// keySecret whether any of their names matches a confidential pattern,
	// which leaves the shared entry out whole.
	configured, keyOwnerOnly, keySecret := map[string]string{}, map[string]bool{}, map[string]bool{}
	repoNames := make([]string, 0, len(cfg.Repositories))
	for name := range cfg.Repositories {
		repoNames = append(repoNames, name)
	}
	sort.Strings(repoNames)
	for _, name := range repoNames {
		r := cfg.Repositories[name]
		key := repoKey(r.Forge, r.Project)
		if _, ok := configured[key]; !ok {
			configured[key] = name
		}
		keyOwnerOnly[key] = keyOwnerOnly[key] || r.OwnerOnly
		keySecret[key] = keySecret[key] || secret(name)
	}
	// group is the entry for the configured repository name.
	group := func(name string) *ReportRepo {
		r, ok := cfg.Repositories[name]
		if !ok {
			return entry("", "", true)
		}
		key := repoKey(r.Forge, r.Project)
		if keySecret[key] {
			return nil
		}
		return entry(key, configured[key], keyOwnerOnly[key])
	}
	var names []string
	for n := range st.Tracks {
		if _, ok := cfg.Sessions[n]; ok { // a session no longer configured is not reported
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var public, all digestCounts
	count := func(state string, delivered, asked, items int, ownerOnly bool) {
		all.add(state, delivered, asked, items)
		if !ownerOnly {
			public.add(state, delivered, asked, items)
		}
	}
	for _, it := range st.SortedItems(false) {
		if it.Seen && now.Sub(it.FirstSeen) > reportItemWindow {
			continue
		}
		var rr *ReportRepo
		if name, ok := configured[it.Repository]; ok {
			rr = group(name)
		} else {
			rr = entry(it.Repository, "", true)
		}
		ri := ReportItem{ID: it.ID, Kind: it.Kind, Title: it.Title, Link: it.Link, SourceTime: it.SourceTime, FirstSeen: it.FirstSeen,
			Seen: it.Seen, OwnerOnly: fromMail(it.ItemPayload)}
		if tr := it.Triaged(); tr != nil {
			ri.NeedsOwner, ri.Urgency, ri.Summary = tr.NeedsOwner, tr.Urgency, tr.Summary
		}
		if rr == nil || secret(ri.ID, ri.Title, ri.Link, ri.Summary) {
			continue
		}
		ri.OwnerOnly = ri.OwnerOnly || rr.OwnerOnly
		rr.Items = append(rr.Items, ri)
		count("", 0, 0, 1, ri.OwnerOnly)
	}
	for _, n := range names {
		rs := st.Tracks[n]
		if secret(n) {
			continue
		}
		rr := group(cfg.Sessions[n].Repository)
		if rr != nil && rs.State != "" {
			c := ReportCase{Session: n, State: string(rs.State), Since: rs.Since, Head: rs.Head, OwnerOnly: rr.OwnerOnly}
			if secret(c.Head) {
				c.Head = ""
			}
			if cfg.Sessions[n].Role == RoleCoordinator {
				c.Role, c.Wakes = RoleCoordinator, rs.WakeCount
				if per, ok := rs.wakeTokens(st.Mode == ModeLive); ok {
					c.WakeTokens = &per
				}
			}
			rr.Cases = append(rr.Cases, c)
			count(c.State, 0, 0, 0, c.OwnerOnly)
		}
		for _, d := range rs.Deliveries {
			repo := d.Repository
			if repo == "" {
				repo = deliveredTo(n, d.Delivered)
			}
			dr := group(repo)
			if dr == nil || secret(d.Ref) {
				continue
			}
			rd := ReportDelivery{Session: n, Ref: d.Ref, At: d.At, OwnerOnly: dr.OwnerOnly}
			if c, ok := st.CI[ciKey(repo, d.Ref)]; ok {
				rd.CI, rd.CIVerdict, rd.CIJobs = c.State, c.Verdict, c.Jobs
				// No one watching, or a record of a head before the one
				// delivered since: unknown, never an old green.
				if c.State != CIMerged && c.State != CIClosed && (!cfg.Poll.CI || !st.Stopped.IsZero() || rs.ciStale(c)) {
					rd.CI, rd.CIVerdict, rd.CIJobs = CIUnknown, "", nil
				}
			}
			// A CI fix pushed to the request is the same delivery again.
			again := false
			for i, o := range dr.Deliveries {
				if o.Session == n && o.Ref == d.Ref {
					rd.At, dr.Deliveries[i], again = o.At, rd, true
				}
			}
			if again {
				continue
			}
			dr.Deliveries = append(dr.Deliveries, rd)
			count("", 1, 0, 0, dr.OwnerOnly)
		}
		if rr != nil && rs.Questions.Open && rs.Asked != "" && !secret(rs.Asked) {
			rr.Questions = append(rr.Questions, ReportQuestion{Session: n, Since: rs.AskedAt, Text: rs.Asked, OwnerOnly: true})
			count("", 0, 1, 0, true)
		}
	}
	report := Report{Schema: ReportSchema, WrittenAt: now, Host: host, Mode: mode, Repositories: []ReportRepo{},
		Digest: public.digest(), OwnerDigest: all.digest()}
	for _, rr := range repos {
		if len(rr.Cases)+len(rr.Deliveries)+len(rr.Questions)+len(rr.Items) > 0 {
			// A repository whose every entry is the owner's is too: a mail
			// item alone does not show the site that something is there.
			rr.OwnerOnly = rr.OwnerOnly || allOwnerOnly(*rr)
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
	items     int
}

// add counts a case in state (when not ""), deliveries, questions and
// items.
func (d *digestCounts) add(state string, delivered, asked, items int) {
	if d.states == nil {
		d.states = map[State]int{}
	}
	if state != "" {
		d.states[State(state)]++
	}
	d.delivered += delivered
	d.asked += asked
	d.items += items
}

// digest is one plain summary: sessions by state, the deliveries, the
// questions waiting on the owner, and the new items.
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
	return fmt.Sprintf("%s; %s delivered; %s waiting on the owner; %s.", line, plural(d.delivered, "request"), plural(d.asked, "question"),
		plural(d.items, "new item"))
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
