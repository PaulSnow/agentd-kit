package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// releaseRun is what the release driver keeps in memory: when each of its
// reads last started, and the last error each logged, so a read that keeps
// failing is logged once.
type releaseRun struct {
	mu     sync.Mutex
	last   map[string]time.Time
	failed map[string]string
}

// due says whether the read key, every apart, is due now, and marks it
// started.
func (r *releaseRun) due(key string, every time.Duration, now time.Time) bool {
	if t, ok := r.last[key]; ok && now.Sub(t) < every {
		return false
	}
	r.last[key] = now
	return true
}

// failedOnce says whether err is new for key; "" clears it.
func (r *releaseRun) failedOnce(key, err string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed[key] == err {
		return false
	}
	r.failed[key] = err
	return err != ""
}

// driveReleases reads each deploying repository's tags and fleets on
// poll.every, and drives each open release item a step.
func (s *Supervisor) driveReleases(now time.Time) {
	if s.rel == nil {
		s.rel = &releaseRun{last: map[string]time.Time{}, failed: map[string]string{}}
	}
	every := s.cfg.Poll.Every.Duration
	for _, repo := range sortedKeys(s.cfg.Repositories) {
		d := s.cfg.Repositories[repo].Deploy
		if d == nil || s.halt != nil {
			continue
		}
		if s.rel.due("tags:"+repo, every, now) {
			known := copyMap(s.st.ReleaseTags[repo])
			first := s.st.ReleaseTags[repo] == nil
			s.spawnShared("release-tags:"+repo, func(w *Supervisor) { w.readTags(repo, d, known, first) })
		}
		for _, tn := range sortedKeys(d.Targets) {
			t := d.Targets[tn]
			if t.Kind != TargetFleet || !s.rel.due("fleet:"+fleetKey(repo, tn), every, now) {
				continue
			}
			var prev *FleetReading
			if r := s.st.Fleets[fleetKey(repo, tn)]; r != nil {
				c := *r
				prev = &c
			}
			s.spawnShared("fleet:"+fleetKey(repo, tn), func(w *Supervisor) { w.pollFleet(repo, tn, t, prev) })
		}
		for _, r := range s.st.sortedReleases() {
			if r.Repository == repo && !r.terminal() && s.halt == nil {
				s.driveRelease(r, d, now)
			}
		}
	}
}

func (s *Supervisor) releaseEmit(p ReleasePayload) {
	s.emit("", Event{Kind: KindRelease, Release: &p})
}

// readTags reads repo's tags in a worker. The first read records those
// there as seen, and opens no item for them; a later one opens an item
// for each new release tag, with its compat rule.
func (s *Supervisor) readTags(repo string, d *DeployConfig, known map[string]bool, first bool) {
	tags, err := s.git.Tags(d.Clone)
	msg := ""
	if err != nil {
		msg = oneLine(err.Error(), 300)
	}
	if s.rel.failedOnce("tags:"+repo, msg) {
		s.emit("", Event{Kind: KindError, Error: &ErrorPayload{Step: "reading " + repo + "'s release tags", Error: msg}})
	}
	if err != nil {
		return
	}
	var fresh []string
	for _, t := range tags {
		if d.tags.MatchString(t) && releaseTagPattern.MatchString(t) && !strings.Contains(t, "..") && !known[t] {
			fresh = append(fresh, t)
		}
	}
	if first {
		s.releaseEmit(ReleasePayload{Repository: repo, Step: ReleaseSeen, Tags: fresh})
		return
	}
	for _, t := range fresh {
		notes, _ := s.git.TagNotes(d.Clone, t)
		s.releaseEmit(ReleasePayload{Repository: repo, Tag: t, Step: ReleaseOpened, Compat: compatRule(notes, t)})
	}
}

// compatPattern finds a release's compatibility rule: a line "compat:"
// or "compatibility:", in its tag's notes or its CHANGELOG entry.
var compatPattern = regexp.MustCompile(`(?im)^[\s>*-]*(?:\*\*)?compat(?:ibility)?(?:\*\*)?\s*:\s*(?:\*\*)?\s*(.+)$`)

// compatRule is the compat line of notes: the tag's own notes first, then
// the CHANGELOG section that names the tag.
func compatRule(notes, tag string) string {
	tagNotes, changelog, _ := strings.Cut(notes, "\x00")
	if m := compatPattern.FindStringSubmatch(tagNotes); m != nil {
		return oneLine(strings.Trim(m[1], "* "), 300)
	}
	if sec := changelogSection(changelog, tag); sec != "" {
		if m := compatPattern.FindStringSubmatch(sec); m != nil {
			return oneLine(strings.Trim(m[1], "* "), 300)
		}
	}
	return ""
}

// changelogSection is the section of a CHANGELOG whose heading names tag,
// with or without its v, up to the next heading of its level or above.
func changelogSection(text, tag string) string {
	bare := strings.TrimPrefix(tag, "v")
	names := regexp.MustCompile(`(^|[^0-9A-Za-z.])v?` + regexp.QuoteMeta(bare) + `($|[^0-9A-Za-z.])`)
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		level := len(l) - len(strings.TrimLeft(l, "#"))
		if level == 0 || !names.MatchString(l) {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if lv := len(lines[j]) - len(strings.TrimLeft(lines[j], "#")); lv > 0 && lv <= level {
				end = j
				break
			}
		}
		return strings.Join(lines[i:end], "\n")
	}
	return ""
}

// driveRelease takes release r a step: its gate while it is built; while
// it deploys, its checks, the fleet's members behind, the operators' and
// the owner's messages; and its end when every check holds.
func (s *Supervisor) driveRelease(r *Release, d *DeployConfig, now time.Time) {
	key := releaseKey(r.Repository, r.Tag)
	every := s.cfg.Poll.Every.Duration
	if r.State == ReleaseBuilt {
		switch {
		case len(d.Gate.Argv) == 0:
			s.releaseEmit(ReleasePayload{Repository: r.Repository, Tag: r.Tag, Step: ReleaseGate, Gate: &ReleaseGateResult{Passed: true, None: true}})
		case s.shadow:
			// A shadow run runs no gate: the live run does.
		case s.rel.due("gate:"+key, every, now):
			repo, tag := r.Repository, r.Tag
			s.spawnShared("release-gate:"+key, func(w *Supervisor) { w.runReleaseGate(repo, tag, d) })
		}
		return
	}
	// The checks of the targets of ours, read in a worker.
	if s.rel.due("check:"+key, every, now) {
		prev := map[string]ReleaseTarget{}
		for tn, t := range r.Targets {
			prev[tn] = *t
		}
		repo, tag := r.Repository, r.Tag
		s.spawnShared("release-check:"+key, func(w *Supervisor) { w.checkTargets(repo, tag, d, prev) })
	}
	// The fleets, from their readings.
	for _, tn := range sortedKeys(d.Targets) {
		t := d.Targets[tn]
		if t.Kind != TargetFleet {
			continue
		}
		f := s.st.Fleets[fleetKey(r.Repository, tn)]
		if f == nil {
			continue
		}
		on, behind := f.onTag(r.Tag)
		rt := r.at(tn)
		if f.Error == "" && (on != rt.On || strings.Join(behind, ",") != strings.Join(rt.Behind, ",")) {
			s.releaseEmit(ReleasePayload{Repository: r.Repository, Tag: r.Tag, Step: ReleaseBehind, Target: tn, Behind: behind, On: on})
		}
		ok := f.Error == "" && on >= t.Threshold
		seen := fmt.Sprintf("%d of %d members on %s, threshold %d", on, len(f.Members), r.Tag, t.Threshold)
		if f.Error != "" {
			seen = "the heartbeats could not be read: " + f.Error
		}
		if ok != rt.OK || seen != rt.Seen {
			s.releaseEmit(ReleasePayload{Repository: r.Repository, Tag: r.Tag, Step: ReleaseCheck, Target: tn, OK: ok, Seen: seen})
		}
	}
	if s.halt != nil {
		return
	}
	s.tellOperators(r, d)
	s.releaseBlocked(r, d)
	s.escalateFleet(r, d, now)
	if s.halt == nil && releaseHolds(r, d) {
		s.releaseEmit(ReleasePayload{Repository: r.Repository, Tag: r.Tag, Step: ReleaseRunning, Text: strings.Join(releaseTargetLines(d, r), "; ")})
		s.releaseMessage(r.Repository, r.Tag, "running", fmt.Sprintf("Release %s of %s is running where it must: %s.", r.Tag, r.Repository, strings.Join(releaseTargetLines(d, r), "; ")))
	}
}

// releaseHolds says whether every target's check holds.
func releaseHolds(r *Release, d *DeployConfig) bool {
	for tn := range d.Targets {
		if t := r.Targets[tn]; t == nil || !t.OK {
			return false
		}
	}
	return true
}

// runReleaseGate runs the release gate in a worker, in a copy of the
// repository at the tag, and logs its outcome with its output; a gate
// that fails withdraws the tag, and the owner is told. One that could not
// run is logged and tried again on the next poll.
func (s *Supervisor) runReleaseGate(repo, tag string, d *DeployConfig) {
	res := &ReleaseGateResult{}
	tmp, err := os.MkdirTemp("", "agentd-release-")
	if err != nil {
		s.emit("", Event{Kind: KindError, Error: &ErrorPayload{Step: "release gate of " + repo + " " + tag, Error: err.Error()}})
		return
	}
	defer os.RemoveAll(tmp)
	dir, home := filepath.Join(tmp, "copy"), filepath.Join(tmp, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		s.emit("", Event{Kind: KindError, Error: &ErrorPayload{Step: "release gate of " + repo + " " + tag, Error: err.Error()}})
		return
	}
	if err := s.git.TagWorktree(d.Clone, dir, tag); err != nil {
		s.emit("", Event{Kind: KindError, Error: &ErrorPayload{Step: "copying " + repo + " at " + tag + " for its release gate", Error: oneLine(err.Error(), 300)}})
		return
	}
	defer s.git.RemoveWorktree(d.Clone, dir)
	env := []string{"HOME=" + home}
	for _, name := range append(append([]string{}, gateBaseEnv...), d.Gate.Env...) {
		if v, ok := os.LookupEnv(name); ok && !secretEnv(name) {
			env = append(env, name+"="+v)
		}
	}
	exit, out, err := s.runner.Gate(dir, withTag(d.Gate.Argv, tag), env, d.Gate.Timeout.Duration)
	res.Exit, res.Output = exit, s.withheld(tailOf(out, releaseOutputMax))
	if err != nil {
		res.Error = s.withheld(oneLine(err.Error(), 300))
	}
	res.Passed = err == nil && exit == 0
	s.releaseEmit(ReleasePayload{Repository: repo, Tag: tag, Step: ReleaseGate, Gate: res})
	if res.Passed {
		return
	}
	why := fmt.Sprintf("exit %d", exit)
	if res.Error != "" {
		why = res.Error
	}
	text := fmt.Sprintf("Release %s of %s is withdrawn: its gate (%s) failed, %s. Nothing deploys it. The gate's output ends: %s",
		tag, repo, strings.Join(d.Gate.Argv, " "), why, cutUTF8(res.Output, 600))
	s.releaseEmit(ReleasePayload{Repository: repo, Tag: tag, Step: ReleaseWithdrawn, Text: text})
	s.releaseMessage(repo, tag, "withdrawn", text)
}

// checkTargets reads each target of ours' check in a worker, and logs
// those that changed since prev.
func (s *Supervisor) checkTargets(repo, tag string, d *DeployConfig, prev map[string]ReleaseTarget) {
	for _, tn := range sortedKeys(d.Targets) {
		t := d.Targets[tn]
		if !t.own() {
			continue
		}
		ok, seen := runDeployCheck(t.Check, tag)
		seen = s.withheld(seen)
		if p := prev[tn]; p.OK != ok || p.Seen != seen || p.CheckedAt.IsZero() {
			s.releaseEmit(ReleasePayload{Repository: repo, Tag: tag, Step: ReleaseCheck, Target: tn, OK: ok, Seen: seen})
		}
	}
}

// runDeployCheck reads c's URL and says whether it shows tag, and what
// was seen.
func runDeployCheck(c DeployCheck, tag string) (bool, string) {
	u := strings.ReplaceAll(c.URL, "{tag}", tag)
	b, err := httpGet(u)
	if err != nil {
		return false, u + " " + oneLine(err.Error(), 200)
	}
	if c.JSONPath == "" {
		want := strings.ReplaceAll(c.Contains, "{tag}", tag)
		if strings.Contains(string(b), want) {
			return true, fmt.Sprintf("%s contains %q", u, want)
		}
		return false, fmt.Sprintf("%s does not contain %q", u, want)
	}
	want := strings.ReplaceAll(c.Value, "{tag}", tag)
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return false, u + " is not JSON"
	}
	for _, k := range strings.Split(c.JSONPath, ".") {
		o, ok := v.(map[string]any)
		if !ok {
			return false, fmt.Sprintf("%s has no %s", u, c.JSONPath)
		}
		v = o[k]
	}
	got := fmt.Sprint(v)
	if v == nil {
		return false, fmt.Sprintf("%s has no %s", u, c.JSONPath)
	}
	if got == want {
		return true, fmt.Sprintf("%s %s is %q", u, c.JSONPath, got)
	}
	return false, fmt.Sprintf("%s %s is %q, not %q", u, c.JSONPath, oneLine(got, 100), want)
}

// tellOperators asks each fleet's operators, once a release, to run its
// tag: with the fleet's notify command when it has one, and on the
// repository's console in any case.
func (s *Supervisor) tellOperators(r *Release, d *DeployConfig) {
	if r.Told || s.shadow {
		return
	}
	var lines []string
	var notify [][]string
	for _, tn := range sortedKeys(d.Targets) {
		t := d.Targets[tn]
		if t.Kind != TargetFleet {
			continue
		}
		rt := r.at(tn)
		text := fmt.Sprintf("For the operators of %s's %s: release %s is out; please run it.", r.Repository, tn, r.Tag)
		if r.Compat != "" {
			text += " Compatibility: " + r.Compat + "."
		}
		if len(rt.Behind) > 0 {
			text += " Not on " + r.Tag + " yet: " + strings.Join(rt.Behind, ", ") + "."
		}
		lines = append(lines, text)
		if len(t.Notify) > 0 {
			argv := make([]string, len(t.Notify))
			for i, a := range t.Notify {
				argv[i] = strings.ReplaceAll(a, "{message}", text)
			}
			notify = append(notify, argv)
		}
	}
	if len(lines) == 0 {
		return
	}
	text := strings.Join(lines, " ")
	if s.confidentialMatch(text) > 0 {
		notify = nil // what was withheld from the console is withheld from the operators
	}
	key := releaseKey(r.Repository, r.Tag)
	if len(notify) == 0 {
		s.releaseEmit(ReleasePayload{Repository: r.Repository, Tag: r.Tag, Step: ReleaseTold, Text: "posted on the console"})
		s.releaseMessage(r.Repository, r.Tag, "operators", text)
		return
	}
	repo, tag := r.Repository, r.Tag
	s.spawnShared("release-told:"+key, func(w *Supervisor) {
		var errs []string
		for _, argv := range notify {
			if err := w.runner.Run(argv); err != nil {
				errs = append(errs, oneLine(err.Error(), 200))
			}
		}
		p := ReleasePayload{Repository: repo, Tag: tag, Step: ReleaseTold, Text: "sent with the fleet's notify command and posted on the console"}
		if len(errs) > 0 {
			p.Error = w.withheld(strings.Join(errs, "; "))
		}
		w.releaseEmit(p)
		w.releaseMessage(repo, tag, "operators", text)
	})
}

// releaseBlocked tells the owner, once a failure, of a step of ours that
// failed: what failed, whether it was rolled back, and how to retry.
func (s *Supervisor) releaseBlocked(r *Release, d *DeployConfig) {
	if r.State != ReleaseBlocked {
		return
	}
	for _, tn := range sortedKeys(r.Targets) {
		f := r.Targets[tn].Failed
		if f == nil {
			continue
		}
		back := "it had no rollback"
		switch {
		case f.RolledBack:
			back = "it was rolled back"
		case f.Rollback != "":
			back = f.Rollback
		}
		text := fmt.Sprintf("Release %s of %s is blocked: %s's step %s failed (%s); %s. Retry it with agentd release retry %s %s. Its output ends: %s",
			r.Tag, r.Repository, tn, stepLabel(f.Index, f.Name), f.Error, back, r.Repository, r.Tag, cutUTF8(f.Output, 600))
		s.releaseMessage(r.Repository, r.Tag, fmt.Sprintf("blocked:%s:%d:%d", tn, f.Index, f.Attempt), text)
	}
}

// escalateFleet tells the owner, once a release, when a fleet is not on
// the tag a fleet's escalate_after after the gate passed, naming the
// members that are not. It never says the fleet is done: only the
// fleet's check can.
func (s *Supervisor) escalateFleet(r *Release, d *DeployConfig, now time.Time) {
	if r.Escalated || r.State == ReleaseBuilt {
		return
	}
	var lines []string
	for _, tn := range sortedKeys(d.Targets) {
		t := d.Targets[tn]
		rt := r.at(tn)
		if t.Kind != TargetFleet || rt.OK || now.Sub(r.GatedAt) < t.EscalateAfter.Duration {
			continue
		}
		line := fmt.Sprintf("%s has %d members on %s, short of its threshold %d", tn, rt.On, r.Tag, t.Threshold)
		if len(rt.Behind) > 0 {
			line += "; not on it: " + strings.Join(rt.Behind, ", ")
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return
	}
	text := fmt.Sprintf("Release %s of %s is not running everywhere yet: %s. Its operators were asked; agentd cannot upgrade their machines.", r.Tag, r.Repository, strings.Join(lines, "; "))
	if r.Compat != "" {
		text += " Compatibility: " + r.Compat + "."
	}
	s.releaseEmit(ReleasePayload{Repository: r.Repository, Tag: r.Tag, Step: ReleaseEscalate, Text: text})
	s.releaseMessage(r.Repository, r.Tag, "escalate", text)
}

// releaseMessage posts text on r's repository's console once per key, as
// agentd: the page shows it, and the escalation command, when there is
// one, is not run for it.
func (s *Supervisor) releaseMessage(repo, tag, key, text string) {
	if r := s.st.Releases[releaseKey(repo, tag)]; r != nil && r.Posted[key] {
		return
	}
	if s.releaseEmit(ReleasePayload{Repository: repo, Tag: tag, Step: ReleasePosted, Key: key}); s.halt != nil {
		return
	}
	id := "R" + shortHash(HashPane(releaseKey(repo, tag) + "\n" + key))[:8]
	m := &MessagePayload{ID: id, Console: s.cfg.RepositoryConsole(repo), Kind: "info", From: "agentd", Text: text}
	if i := s.confidentialMatch(text); i > 0 {
		m.Text, m.Withheld = "", i
	}
	s.emit("", Event{Kind: KindMessage, Mode: ModeShared, Message: m})
}

// releaseTargetLines are each target's state in a line.
func releaseTargetLines(d *DeployConfig, r *Release) []string {
	var out []string
	for _, tn := range sortedKeys(d.Targets) {
		t := d.Targets[tn]
		rt := r.Targets[tn]
		var line string
		switch {
		case rt == nil:
			line = tn + ": not checked yet"
		case rt.Failed != nil:
			line = fmt.Sprintf("%s: step %s failed: %s", tn, stepLabel(rt.Failed.Index, rt.Failed.Name), rt.Failed.Error)
		case rt.OK:
			line = tn + ": holds (" + rt.Seen + ")"
		case rt.Seen != "":
			line = tn + ": not yet (" + rt.Seen + ")"
		default:
			line = tn + ": not checked yet"
		}
		if t.own() && len(t.Steps) > 0 && rt != nil && rt.Failed == nil {
			line += fmt.Sprintf(", %d of %d steps done", rt.StepsDone, len(t.Steps))
		}
		out = append(out, line)
	}
	return out
}
