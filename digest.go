package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// The owner's daily digest (#88): every decision waiting on the owner,
// across all repositories, oldest first, each with its recommended answer
// and how long it has waited. The home page shows it, `agentd digest`
// prints it, and once a day after digest.at agentd posts it as one info
// entry on digest.console. A decision of a kind digest.defaults names
// takes its default once that long has passed since it was first seen,
// unless the owner set its own deadline on it, which wins.

// Kinds of waiting decision.
const (
	DigestDecision = "decision" // a session's decision: raised, questions.json or STATUS.md's Questions
	DigestCI       = "ci"       // agentd's own C-numbered decision on a request's CI
	DigestFinding  = "finding"  // a judge's finding deferred to the owner
	DigestDesign   = "design"   // a designer's brief waiting on go, change or decline
	DigestWork     = "work"     // a work item's question, or its claim of done
	DigestRequest  = "request"  // a request the loop waits on the owner for
	DigestOps      = "ops"      // a change proposed on the ops console
)

// digestDefaultKinds are the kinds a default deadline may be set on: the
// only ones whose decisions carry a default (Decision.Default).
var digestDefaultKinds = []string{DigestDecision, DigestCI, DigestFinding}

// DigestConfig is the owner's daily digest: the console its entry goes
// to ("" for none), the local time after which it is posted each day
// (HH:MM, "08:00" when empty), and how long after a decision of a kind
// was first seen its default is applied (none when not set).
type DigestConfig struct {
	Console  string              `json:"console"`
	At       string              `json:"at"`
	Defaults map[string]Duration `json:"defaults"`
}

// digestAtDefault is when the entry is posted when digest.at is empty.
const digestAtDefault = "08:00"

// at is the time of day the entry is posted after: hour and minute.
func (d DigestConfig) at() (int, int) {
	s := d.At
	if s == "" {
		s = digestAtDefault
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		t, _ = time.Parse("15:04", digestAtDefault)
	}
	return t.Hour(), t.Minute()
}

// checkDigest refuses a digest.at that is no HH:MM, a default on a kind
// that carries none or is unknown, a duration that is not positive, and
// a console no repository or consoles entry has.
func (c *Config) checkDigest(errs *[]error) {
	d := c.Digest
	if d.At != "" {
		if _, err := time.Parse("15:04", d.At); err != nil {
			*errs = append(*errs, fmt.Errorf("digest.at %q is not a local time HH:MM", d.At))
		}
	}
	for _, k := range sortedKeys(d.Defaults) {
		if !contains(digestDefaultKinds, k) {
			*errs = append(*errs, fmt.Errorf("digest.defaults.%s: only %s decisions carry a default; a %q one never takes one", k, strings.Join(digestDefaultKinds, ", "), k))
			continue
		}
		if d.Defaults[k].Duration <= 0 {
			*errs = append(*errs, fmt.Errorf("digest.defaults.%s must be a positive duration", k))
		}
	}
	if d.Console != "" && !c.knownConsole(d.Console) {
		*errs = append(*errs, fmt.Errorf("digest.console %q is a console no repository or consoles entry has", d.Console))
	}
}

// DigestEntry is one decision waiting on the owner: its kind, the
// repository it is in ("" for none), its console entry and session, what
// it asks, the recommended answer when there is one, when it was first
// seen and how long it has waited since, and the default it takes and
// when, when one will be applied.
type DigestEntry struct {
	Kind        string        `json:"kind"`
	Repository  string        `json:"repository,omitempty"`
	Entry       string        `json:"entry"`
	Session     string        `json:"session,omitempty"`
	Asked       string        `json:"asked"`
	Recommended string        `json:"recommended,omitempty"`
	Since       time.Time     `json:"since"`
	Waited      time.Duration `json:"waited"`
	Default     string        `json:"default,omitempty"`
	DefaultAt   *time.Time    `json:"default_at,omitempty"`
}

// decisionKind is the kind of a session's decision d.
func decisionKind(d Decision) string {
	switch {
	case d.Finding != "":
		return DigestFinding
	case ciRaisedID.MatchString(d.ID):
		return DigestCI
	}
	return DigestDecision
}

// digestKind is the kind of need n.
func digestKind(n repoNeed) string {
	switch e := n.Entry; {
	case n.Decision != nil:
		return decisionKind(*n.Decision)
	case strings.HasPrefix(e, entryDesign):
		return DigestDesign
	case strings.HasPrefix(e, entryWork):
		return DigestWork
	case strings.HasPrefix(e, entryOps):
		return DigestOps
	case strings.HasPrefix(e, entryDecision):
		return DigestDecision
	}
	return DigestRequest
}

// BuildDigest is every decision waiting on the owner as st holds it at
// now, oldest first: each configured repository's needs (repoNeeds) and
// the open decisions of sessions in none, then the ops changes waiting
// on a yes. decisions are the sessions' open decisions (DecisionsFrom).
// Every text is screened again; a matching one is withheld.
func BuildDigest(cfg *Config, st *LogState, decisions []UIDecision, now time.Time) []DigestEntry {
	secret := confidentialMatcher(cfg)
	var out []DigestEntry
	add := func(repo string, n repoNeed) {
		e := DigestEntry{Kind: digestKind(n), Repository: repo, Entry: n.Entry, Session: n.Session, Asked: n.Asked,
			Recommended: strings.TrimPrefix(n.Checked, "Recommended: ")}
		if !strings.HasPrefix(n.Checked, "Recommended: ") {
			e.Recommended = ""
		}
		if n.Decision != nil && n.Decision.Recommendation != "" && !n.Decision.Withheld {
			e.Recommended = oneLine(n.Decision.Recommendation, 300)
		}
		e.Since = needSince(st, repo, n)
		if !e.Since.IsZero() {
			e.Waited = now.Sub(e.Since)
		}
		if n.Decision != nil {
			if t := st.Tracks[n.Session]; t != nil {
				if at, _, ok := t.effectiveDeadline(*n.Decision, e.Kind, cfg); ok {
					e.Default, e.DefaultAt = n.Decision.Default, &at
				}
			}
		}
		if secret(e.Asked, e.Recommended, e.Default, e.Repository, e.Session) {
			e.Asked, e.Recommended, e.Default = withheldSecret, "", ""
			if secret(e.Repository, e.Session) {
				e.Repository, e.Session = "", ""
			}
		}
		out = append(out, e)
	}
	for _, repo := range sortedKeys(cfg.Repositories) {
		for _, n := range repoNeeds(cfg, st, repo, decisions, now) {
			add(repo, n)
		}
	}
	for _, d := range decisions {
		if _, ok := cfg.Repositories[d.Repository]; !ok {
			add("", repoNeed{LevelDecision: sessionDecision(d)})
		}
	}
	for _, id := range sortedKeys(st.Ops) {
		r := st.Ops[id]
		if r.Answer != "" || !r.Ended.IsZero() || !r.Started.IsZero() || r.noYes(cfg) {
			continue
		}
		add("", repoNeed{LevelDecision: LevelDecision{Entry: entryOps + id, Asked: oneLine(r.Summary, 400)}})
	}
	sortDigest(out)
	return out
}

// sortDigest orders entries oldest first, ties by entry.
func sortDigest(es []DigestEntry) {
	sort.SliceStable(es, func(i, j int) bool {
		if !es[i].Since.Equal(es[j].Since) {
			return es[i].Since.Before(es[j].Since)
		}
		return es[i].Entry < es[j].Entry
	})
}

// needSince is when n began waiting on the owner, as the log holds it,
// never now: a session decision's first sighting; a design's latest
// round; a work item's question since it was blocked, or its claim since
// it was made or refuted; a request's place since the loop moved to it;
// an ops change since it was proposed. Zero when the log holds none.
func needSince(st *LogState, repo string, n repoNeed) time.Time {
	e := n.Entry
	switch {
	case n.Decision != nil:
		if t := st.Tracks[n.Session]; t != nil {
			if at, ok := t.firstSeen(*n.Decision); ok {
				return at
			}
			// Raised before first sightings were folded: its case's record.
			for _, c := range st.Cases {
				for _, r := range c.Decisions {
					if r.Session == n.Session && r.ID == n.Decision.ID {
						return r.At
					}
				}
			}
			return t.Since
		}
	case strings.HasPrefix(e, entryDesign):
		if it := st.Work[strings.TrimPrefix(e, entryDesign)]; it != nil {
			return firstNonZero(it.Designed, it.Progress, it.Opened)
		}
	case strings.HasPrefix(e, entryWork):
		if it := st.Work[strings.TrimPrefix(e, entryWork)]; it != nil {
			switch {
			case it.Question != "":
				return firstNonZero(it.Progress, it.Opened)
			case it.Refuted == it.Claim && !it.RefutedAt.IsZero():
				return it.RefutedAt
			}
			return firstNonZero(it.Claimed, it.Progress, it.Opened)
		}
	case strings.HasPrefix(e, entryOps):
		if r := st.Ops[strings.TrimPrefix(e, entryOps)]; r != nil {
			return r.Proposed
		}
	case strings.HasPrefix(e, entryLoop):
		return st.Loops[ciKey(repo, strings.TrimPrefix(e, entryLoop))].PlaceSince
	}
	return time.Time{}
}

// firstNonZero is the first of ts that is not zero.
func firstNonZero(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// waitedText is how long d is, shortly: "3d4h", "5h12m", "7m".
func waitedText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%dd%dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
}

// Line is the entry as the console entry and the command print it:
// "[kind] repo: question. Recommended: …. Waited 3d4h. Default X at T".
func (e DigestEntry) Line(loc *time.Location) string {
	repo := e.Repository
	if repo == "" {
		repo = "(no repository)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s: %s", e.Kind, repo, strings.TrimRight(e.Asked, ". "))
	if e.Recommended != "" {
		fmt.Fprintf(&b, ". Recommended: %s", strings.TrimRight(e.Recommended, ". "))
	}
	if e.Since.IsZero() {
		b.WriteString(". Waited: not known")
	} else {
		fmt.Fprintf(&b, ". Waited %s", waitedText(e.Waited))
	}
	if e.DefaultAt != nil {
		fmt.Fprintf(&b, ". Default %s at %s", e.Default, e.DefaultAt.In(loc).Format("2006-01-02 15:04"))
	}
	return b.String()
}

// digestNothing is the entry on a day nothing waits on the owner, posted
// so the owner knows the digest ran.
const digestNothing = "Nothing waits on you."

// digestText is the console entry of es, at most limit bytes: a heading
// and one line per entry, oldest first; what does not fit is left out
// whole, and a last line says how many were.
func digestText(es []DigestEntry, loc *time.Location, limit int) string {
	if len(es) == 0 {
		return digestNothing
	}
	head := fmt.Sprintf("Waiting on you, oldest first (%d):", len(es))
	b := head
	more := func(n int) string { return fmt.Sprintf("\n…and %d more; see the home page or agentd digest.", n) }
	for i, e := range es {
		line := "\n" + oneLine(e.Line(loc), 1000)
		// The last line needs no room for the count of those left out.
		room := len(more(len(es)))
		if i == len(es)-1 {
			room = 0
		}
		if len(b)+len(line)+room > limit {
			return b + more(len(es)-i)
		}
		b += line
	}
	return b
}

// digestLimit bounds the console entry's text, under MessageLimit.
const digestLimit = 8 << 10

// digestDay is the local day of now a digest is posted for, YYYY-MM-DD,
// and whether it is due: past digest.at that day.
func digestDay(cfg DigestConfig, now time.Time) (string, bool) {
	h, m := cfg.at()
	at := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	return now.Format("2006-01-02"), !now.Before(at)
}

// driveDigest posts the day's digest on digest.console once, after
// digest.at in the clock's zone (the page's): one info entry from agentd,
// the list screened and cut. The day it was posted for is folded from the
// entry, so a restart that day posts nothing more. Nothing is posted with
// no digest.console, or in shadow mode.
func (s *Supervisor) driveDigest(now time.Time) {
	dc := s.cfg.Digest
	if dc.Console == "" || s.shadow || s.halt != nil {
		return
	}
	day, due := digestDay(dc, now)
	if !due || s.st.DigestDay == day {
		return
	}
	cfg := withRuntime(s.cfg, s.st.Runtime)
	rep := ReportFrom(cfg, s.st, "", now)
	es := BuildDigest(cfg, s.st, DecisionsFrom(cfg, s.st, rep), now)
	text := digestText(es, now.Location(), digestLimit)
	m := &MessagePayload{ID: "digest-" + day, Console: dc.Console, Kind: "info", From: "agentd", Text: text, Digest: day}
	if i := s.confidentialMatch(text); i > 0 {
		m.Text, m.Withheld = "", i
	}
	s.emit("", Event{Kind: KindMessage, Mode: ModeShared, Message: m})
}

// digestCmd prints today's digest from the event log: one line per
// decision waiting on the owner, oldest first, or JSON with --json.
func digestCmd(args []string) error {
	fs := flag.NewFlagSet("digest", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the digest as JSON")
	shadow := fs.Bool("shadow", false, "the digest of what the shadow run saw")
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	events, _, err := ReadLog(cfg.EventLog)
	if err != nil {
		return err
	}
	mode := ModeLive
	if *shadow {
		mode = ModeShadow
	}
	return writeDigest(os.Stdout, cfg, events, mode, time.Now(), *asJSON)
}

// writeDigest writes the digest of events at now to w.
func writeDigest(w io.Writer, cfg *Config, events []Event, mode string, now time.Time, asJSON bool) error {
	if cfg == nil {
		return errors.New("no configuration")
	}
	st := NewLogState(cfg, mode).Fold(events)
	c := withRuntime(cfg, st.Runtime)
	es := BuildDigest(c, st, DecisionsFrom(c, st, ReportFrom(c, st, "", now)), now)
	if asJSON {
		if es == nil {
			es = []DigestEntry{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(es)
	}
	_, err := io.WriteString(w, digestText(es, now.Location(), 1<<30)+"\n")
	return err
}
