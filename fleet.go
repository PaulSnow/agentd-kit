package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Fleets (#67). A fleet is a target whose members are other people's
// machines: Go reads, on poll.every, what its heartbeats endpoint says
// each member runs and when it was last seen, and logs the reading when
// it changes. The reading is a fact: a card Go writes itself (topic
// fleet:TARGET) and part of the thread agents' fresh facts, so which
// members run which version is answered from state. A fleet whose
// members run more than one version is mixed.

// KindFleet is a fleet's reading.
const KindFleet = "fleet"

// FleetPayload is one reading of a fleet's heartbeats: each member's
// version and when it was last seen, or why they could not be read.
type FleetPayload struct {
	Repository string        `json:"repository"`
	Target     string        `json:"target"`
	Members    []FleetMember `json:"members,omitempty"`
	Mixed      bool          `json:"mixed,omitempty"`
	Error      string        `json:"error,omitempty"`
}

// FleetMember is one member as its heartbeats say.
type FleetMember struct {
	Member   string     `json:"member"`
	Version  string     `json:"version"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// FleetReading is a fleet's latest reading, when it was read and how many
// readings there have been.
type FleetReading struct {
	FleetPayload
	At  time.Time
	Rev int
}

// Fleet limits: the most members kept, the body read, and how long a
// reading that has not changed stands before it is logged again with the
// members' last-seen times.
const (
	fleetMembersMax = 500
	fleetBodyMax    = 4 << 20
	fleetRefresh    = time.Hour
	fleetTimeout    = 30 * time.Second
)

func fleetKey(repo, target string) string { return repo + "/" + target }

func (st *LogState) applyFleet(e Event) {
	if st.Fleets == nil {
		st.Fleets = map[string]*FleetReading{}
	}
	k := fleetKey(e.Fleet.Repository, e.Fleet.Target)
	rev := 1
	if prev := st.Fleets[k]; prev != nil {
		rev = prev.Rev + 1
	}
	st.Fleets[k] = &FleetReading{FleetPayload: *e.Fleet, At: e.Time, Rev: rev}
}

// versions counts the members on each version.
func (p *FleetPayload) versions() map[string]int {
	out := map[string]int{}
	for _, m := range p.Members {
		out[m.Version]++
	}
	return out
}

// onTag splits the members into those that run tag and those that do
// not, by name.
func (p *FleetPayload) onTag(tag string) (on int, behind []string) {
	for _, m := range p.Members {
		if versionIs(m.Version, tag) {
			on++
		} else {
			behind = append(behind, m.Member)
		}
	}
	return on, behind
}

// versionIs says whether a member's version is the tag: the same with or
// without a leading v, or the tag with a build suffix.
func versionIs(version, tag string) bool {
	v, t := strings.TrimPrefix(strings.TrimSpace(version), "v"), strings.TrimPrefix(tag, "v")
	return t != "" && (v == t || strings.HasPrefix(v, t+"+") || strings.HasPrefix(v, t+"-"))
}

// fleetHTTP is the client a fleet's heartbeats and a target's check are
// read with.
var fleetHTTP = &http.Client{Timeout: fleetTimeout}

// readFleet reads a fleet's heartbeats.
func readFleet(repo, target string, t DeployTarget) FleetPayload {
	p := FleetPayload{Repository: repo, Target: target}
	b, err := httpGet(t.Heartbeats)
	if err == nil {
		p.Members, err = parseHeartbeats(b, t.Fields)
	}
	if err != nil {
		p.Error = oneLine(err.Error(), 300)
		return p
	}
	p.Mixed = len(p.versions()) > 1
	return p
}

// httpGet reads u's body, at most fleetBodyMax, when it answers 200.
func httpGet(u string) ([]byte, error) {
	resp, err := fleetHTTP.Get(u)
	if err != nil {
		return nil, fmt.Errorf("could not be read: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, fleetBodyMax))
	if err != nil {
		return nil, fmt.Errorf("could not be read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return b, fmt.Errorf("status %d", resp.StatusCode)
	}
	return b, nil
}

// Heartbeat keys guessed when the target's fields name none.
var (
	heartbeatLists    = []string{"heartbeats", "members", "signers", "nodes", "data", "items", "results"}
	heartbeatMembers  = []string{"member", "name", "operator", "signer", "node", "id"}
	heartbeatVersions = []string{"version", "release", "build"}
	heartbeatSeen     = []string{"last_seen", "lastSeen", "seen", "timestamp", "time", "at", "updated"}
)

// parseHeartbeats reads a heartbeats body: a list of heartbeats, an
// object holding one, or an object of heartbeats by member. A member
// seen more than once is kept at its latest heartbeat.
func parseHeartbeats(b []byte, f HeartbeatFields) ([]FleetMember, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, errors.New("the heartbeats are not JSON")
	}
	// Each heartbeat, with the key it is under in an object by member.
	type heartbeat struct {
		key string
		o   map[string]any
	}
	var hbs []heartbeat
	var list []any
	switch x := v.(type) {
	case []any:
		list = x
	case map[string]any:
		keys := heartbeatLists
		if f.List != "" {
			keys = []string{f.List}
		}
		for _, k := range keys {
			if l, ok := x[k].([]any); ok {
				list = l
				break
			}
			if m, ok := x[k].(map[string]any); ok {
				x = m
				break
			}
		}
		if list == nil {
			for _, k := range sortedKeys(x) {
				if o, ok := x[k].(map[string]any); ok {
					hbs = append(hbs, heartbeat{k, o})
				}
			}
		}
	}
	for _, e := range list {
		if o, ok := e.(map[string]any); ok {
			hbs = append(hbs, heartbeat{"", o})
		}
	}
	if len(hbs) == 0 {
		return nil, errors.New("the heartbeats hold no list of members")
	}
	latest := map[string]FleetMember{}
	for _, h := range hbs {
		o := h.o
		m := FleetMember{Member: field(o, f.Member, heartbeatMembers), Version: field(o, f.Version, heartbeatVersions)}
		if m.Member == "" {
			m.Member = h.key
		}
		if m.Member == "" {
			continue
		}
		if t, ok := seenTime(field(o, f.Seen, heartbeatSeen)); ok {
			m.LastSeen = &t
		}
		if prev, ok := latest[m.Member]; ok && prev.LastSeen != nil && (m.LastSeen == nil || !m.LastSeen.After(*prev.LastSeen)) {
			continue
		}
		latest[m.Member] = m
	}
	if len(latest) == 0 {
		return nil, errors.New("the heartbeats name no member")
	}
	out := make([]FleetMember, 0, len(latest))
	for _, k := range sortedKeys(latest) {
		out = append(out, latest[k])
	}
	if len(out) > fleetMembersMax {
		out = out[:fleetMembersMax]
	}
	return out, nil
}

// field is o's value at key, or, with none named, at the first of guesses
// it has, as text.
func field(o map[string]any, key string, guesses []string) string {
	if key != "" {
		guesses = []string{key}
	}
	for _, k := range guesses {
		switch x := o[k].(type) {
		case string:
			return oneLine(x, 200)
		case json.Number:
			return x.String()
		}
	}
	return ""
}

// seenTime reads a heartbeat's time: RFC 3339, or Unix seconds or
// milliseconds.
func seenTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), true
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	if n > 1e12 {
		n /= 1000
	}
	return time.Unix(int64(n), 0).UTC(), true
}

// fleetChanged says whether a reading says something prev did not: a
// member's version, a member come or gone, the error, or, after
// fleetRefresh, the members' last-seen times.
func fleetChanged(prev *FleetReading, p FleetPayload, now time.Time) bool {
	if prev == nil || prev.Error != p.Error || len(prev.Members) != len(p.Members) || now.Sub(prev.At) >= fleetRefresh && p.Error == "" {
		return true
	}
	for i, m := range p.Members {
		if prev.Members[i].Member != m.Member || prev.Members[i].Version != m.Version {
			return true
		}
	}
	return false
}

// fleetStatus is a fleet's state in a line.
func fleetStatus(p *FleetPayload, threshold int) string {
	if p.Error != "" {
		return "heartbeats unreadable: " + p.Error
	}
	vs := p.versions()
	names := sortedKeys(vs)
	sort.SliceStable(names, func(i, j int) bool { return vs[names[i]] > vs[names[j]] })
	var parts []string
	for _, v := range names {
		label := v
		if label == "" {
			label = "no version"
		}
		parts = append(parts, fmt.Sprintf("%d on %s", vs[v], label))
	}
	state := "uniform"
	if p.Mixed {
		state = "mixed"
	}
	return fmt.Sprintf("%s: %s, of %d members (threshold %d)", state, strings.Join(parts, ", "), len(p.Members), threshold)
}

// fleetSummary lists the members, each with its version and when it was
// last seen.
func fleetSummary(p *FleetPayload) string {
	var parts []string
	for _, m := range p.Members {
		s := m.Member + " " + m.Version
		if m.LastSeen != nil {
			s += " (seen " + m.LastSeen.UTC().Format(time.RFC3339) + ")"
		}
		parts = append(parts, s)
	}
	return cutUTF8(strings.Join(parts, "; "), cardSummaryMax)
}

// fleetCard is the card Go writes for a fleet's reading.
func fleetCard(repo, target string, t DeployTarget, r *FleetReading) *Card {
	return &Card{Topic: topicFleet + target, Kind: CardFleet, Title: fmt.Sprintf("%s's %s fleet", repo, target),
		Status: cutUTF8(fleetStatus(&r.FleetPayload, t.Threshold), cardStatusMax), Summary: fleetSummary(&r.FleetPayload),
		Src: []string{"fleet:" + fleetKey(repo, target) + "@" + r.At.UTC().Format(time.RFC3339)}, InForce: []CardRule{}, Open: []CardQuestion{},
		KnownProblems: []CardProblem{}, Related: []string{}, Updated: r.At, Rev: r.Rev}
}

// pollFleet reads a fleet in a worker and logs the reading, and its card,
// when it changed.
func (s *Supervisor) pollFleet(repo, target string, t DeployTarget, prev *FleetReading) {
	p := readFleet(repo, target, t)
	now := s.clock.Now()
	if !fleetChanged(prev, p, now) {
		return
	}
	s.emit("", Event{Kind: KindFleet, Fleet: &p})
	rev := 1
	if prev != nil {
		rev = prev.Rev + 1
	}
	r := &FleetReading{FleetPayload: p, At: now, Rev: rev}
	s.emit("", Event{Kind: KindCard, Card: &CardPayload{Repository: repo, Topic: topicFleet + target, Card: fleetCard(repo, target, t, r), Through: now}})
}

// threadFleet is a fleet as a thread agent is given it: a live fact.
type threadFleet struct {
	Target    string         `json:"target"`
	Threshold int            `json:"threshold"`
	At        time.Time      `json:"read_at"`
	Mixed     bool           `json:"mixed,omitempty"`
	Versions  map[string]int `json:"versions,omitempty"`
	Members   []FleetMember  `json:"members,omitempty"`
	Error     string         `json:"error,omitempty"`
}

// threadRelease is a release item as a thread agent is given it.
type threadRelease struct {
	Tag     string   `json:"tag"`
	State   string   `json:"state"`
	Compat  string   `json:"compat,omitempty"`
	Targets []string `json:"targets"`
}

// threadFleetMembers bounds the members a thread agent is given a fleet
// with; versions counts them all.
const threadFleetMembers = 50

// fleetFacts are repo's fleets and its open and latest releases, as the
// log holds them.
func (s *Supervisor) fleetFacts(repo string, keep func(string) string) ([]threadFleet, []threadRelease) {
	d := s.cfg.Repositories[repo].Deploy
	if d == nil {
		return nil, nil
	}
	var fleets []threadFleet
	for _, tn := range sortedKeys(d.Targets) {
		t := d.Targets[tn]
		r := s.st.Fleets[fleetKey(repo, tn)]
		if t.Kind != TargetFleet || r == nil {
			continue
		}
		f := threadFleet{Target: tn, Threshold: t.Threshold, At: r.At, Mixed: r.Mixed, Versions: r.versions(), Error: keep(r.Error)}
		f.Members = r.Members
		if len(f.Members) > threadFleetMembers {
			f.Members = f.Members[:threadFleetMembers]
		}
		fleets = append(fleets, f)
	}
	var rels []threadRelease
	for _, r := range s.st.sortedReleases() {
		if r.Repository != repo || r.terminal() && len(rels) > 0 {
			continue
		}
		tr := threadRelease{Tag: r.Tag, State: r.State, Compat: keep(r.Compat)}
		for _, line := range releaseTargetLines(d, r) {
			tr.Targets = append(tr.Targets, keep(line))
		}
		rels = append(rels, tr)
		if len(rels) == 5 {
			break
		}
	}
	return fleets, rels
}
