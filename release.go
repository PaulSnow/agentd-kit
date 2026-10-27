package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Releases (#67). A repository that declares deploy names where its
// releases must run: targets of its own, which Go deploys through a node
// (#15, #29), and fleets, whose members are other people's machines Go
// can only read. When the repository gets a release tag Go opens the
// item "release TAG running". The repository's gate runs the system on a
// copy first; a tag whose gate fails is withdrawn and deploys nowhere.
// Then Go drives each of its own targets' steps on the node, each a
// checked action with its rollback, asks the fleet's operators once to
// upgrade, and checks every target on each pass: the item is done only
// when every check holds. Nothing an agent or a session says closes it.

// DeployConfig is a repository's deploy: the clone its tags are read
// from, the tags that are releases, the release gate, and the targets.
type DeployConfig struct {
	// Clone is where tags are read and the gate's copy is made; by
	// default the repository's sessions clone.
	Clone string `json:"clone"`
	// Tags is the regular expression a release tag matches; by default
	// DefaultReleaseTags.
	Tags    string                  `json:"tags"`
	Gate    ReleaseGateRules        `json:"gate"`
	Targets map[string]DeployTarget `json:"targets"`

	tags *regexp.Regexp
}

// ReleaseGateRules is the release gate (#67): the argv run in a copy of
// the repository at the tag, which runs the system, not the build: the
// repository's own scripts, against a copy of production. It runs as the
// delivery gate does, with PATH, LANG and TMPDIR, an empty HOME and the
// variables Env names; Timeout bounds it.
type ReleaseGateRules struct {
	Argv    []string `json:"argv"`
	Timeout Duration `json:"timeout"`
	Env     []string `json:"env"`
}

// DeployTarget is where a release must run. A service of ours (Kind "")
// is deployed by Steps on Host, through the node on Host (via node) or
// through the node that holds ssh access to it (via ssh); its Check says
// whether the tag runs there. A fleet is other people's machines: Go
// reads its Heartbeats, and it holds when Threshold members run the tag.
type DeployTarget struct {
	Kind string `json:"kind"`
	Host string `json:"host"`
	Via  string `json:"via"`
	// Node is, for an ssh target, the host whose node holds the ssh
	// access; agentd's own host only with OwnHost.
	Node    string       `json:"node"`
	OwnHost bool         `json:"own_host"`
	Check   DeployCheck  `json:"check"`
	Steps   []DeployStep `json:"steps"`
	// A fleet's heartbeats URL, how many members must run the tag, the
	// operators to ask, the fields a heartbeat is read by, when the owner
	// is told it is not there yet, and the command, {message} filled in,
	// that tells the operators.
	Heartbeats    string          `json:"heartbeats"`
	Threshold     int             `json:"threshold"`
	Operators     []string        `json:"operators"`
	Fields        HeartbeatFields `json:"fields"`
	EscalateAfter Duration        `json:"escalate_after"`
	Notify        []string        `json:"notify"`
}

// DeployCheck says whether a target runs the tag: URL's body contains
// Contains, or the JSON value at JSONPath (dotted keys) equals Value, by
// default the tag. Each may name {tag}.
type DeployCheck struct {
	URL      string `json:"url"`
	Contains string `json:"contains"`
	JSONPath string `json:"json_path"`
	Value    string `json:"value"`
}

// DeployStep is one checked action of a target's deployment: Run, then
// Check, each to exit 0; when either fails, Rollback. Each argv may name
// {tag}. Timeout bounds each command, by default DefaultStepTimeout.
type DeployStep struct {
	Name     string   `json:"name"`
	Run      []string `json:"run"`
	Check    []string `json:"check"`
	Rollback []string `json:"rollback"`
	Timeout  Duration `json:"timeout"`
}

// HeartbeatFields name what a fleet's heartbeats are read by: the key
// holding the list, when the body is an object, and each heartbeat's
// member, version and last-seen keys. Empty ones are guessed from common
// names.
type HeartbeatFields struct {
	List    string `json:"list"`
	Member  string `json:"member"`
	Version string `json:"version"`
	Seen    string `json:"seen"`
}

// Target kinds and routes.
const (
	TargetFleet = "fleet"
	ViaNode     = "node"
	ViaSSH      = "ssh"
)

// Release defaults.
const (
	DefaultReleaseTags     = `^v[0-9]`
	DefaultReleaseGateTime = 30 * time.Minute
	DefaultStepTimeout     = 10 * time.Minute
	DefaultFleetEscalate   = 24 * time.Hour
	releaseOutputMax       = 2000
)

var (
	sshHostPattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@-]*$`)
	releaseTagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/+-]{0,99}$`)
)

// checkDeploy checks repository name's deploy and fills in its defaults.
func (c *Config) checkDeploy(name string, r *Repository, path func(field, p string) string, errs *[]error) {
	d := r.Deploy
	if d == nil {
		return
	}
	field := "repositories." + name + ".deploy"
	bad := func(format string, a ...any) { *errs = append(*errs, fmt.Errorf(field+format, a...)) }
	switch {
	case d.Clone != "":
		d.Clone = path(field+".clone", d.Clone)
	case r.Sessions != nil && r.Sessions.Host == "":
		d.Clone = r.Sessions.Clone
	default:
		bad(".clone: name the clone its tags are read from")
	}
	if d.Tags == "" {
		d.Tags = DefaultReleaseTags
	}
	re, err := regexp.Compile(d.Tags)
	if err != nil {
		bad(".tags: %v", err)
	}
	d.tags = re
	if d.Gate.Timeout.Duration < 0 {
		bad(".gate.timeout must not be negative")
	}
	if d.Gate.Timeout.Duration == 0 {
		d.Gate.Timeout = Duration{DefaultReleaseGateTime}
	}
	for _, v := range d.Gate.Env {
		if secretEnv(v) {
			bad(".gate.env: %q may not be passed to a gate", v)
		}
	}
	if len(d.Targets) == 0 {
		bad(".targets: name at least one")
	}
	for _, tn := range sortedKeys(d.Targets) {
		t := d.Targets[tn]
		tf := ".targets." + tn
		if !sessionName.MatchString(tn) {
			bad("%s: a target's name is letters, digits, - and _", tf)
		}
		switch t.Kind {
		case TargetFleet:
			if !httpURL(t.Heartbeats) {
				bad("%s.heartbeats must be an http or https URL", tf)
			}
			if t.Threshold < 1 {
				bad("%s.threshold must be at least 1", tf)
			}
			if t.Via != "" || len(t.Steps) > 0 || t.Check.URL != "" {
				bad("%s: a fleet's members are not ours: it has no via, steps or check", tf)
			}
			if t.EscalateAfter.Duration == 0 {
				t.EscalateAfter = Duration{DefaultFleetEscalate}
			}
		case "":
			c.checkOwnTarget(t, func(format string, a ...any) { bad(tf+format, a...) })
			for i := range t.Steps {
				if t.Steps[i].Timeout.Duration <= 0 {
					t.Steps[i].Timeout = Duration{DefaultStepTimeout}
				}
			}
			if t.Check.JSONPath != "" && t.Check.Value == "" {
				t.Check.Value = "{tag}"
			}
		default:
			bad("%s.kind must be fleet, or absent for a target of ours", tf)
		}
		d.Targets[tn] = t
	}
}

// checkOwnTarget checks a target Go deploys: its route, its check and
// its steps.
func (c *Config) checkOwnTarget(t DeployTarget, bad func(format string, a ...any)) {
	if !sshHostPattern.MatchString(t.Host) {
		bad(".host must name the host the target runs on")
	}
	switch t.Via {
	case ViaNode:
		if _, ok := c.Hosts[t.Host]; !ok {
			bad(".host: %q has no agentd node: it is not in hosts", t.Host)
		}
	case ViaSSH:
		if _, ok := c.Hosts[t.Node]; t.Node == "" || !ok {
			bad(".node: an ssh target names the node that holds the ssh access, a host in hosts; %q is none", t.Node)
		} else if t.Node == c.Node.Name && !t.OwnHost {
			bad(".node: %q is agentd's own host; set own_host to deploy over ssh from it", t.Node)
		}
	default:
		bad(".via must be node or ssh")
	}
	if !httpURL(strings.ReplaceAll(t.Check.URL, "{tag}", "v0")) {
		bad(".check.url must be an http or https URL")
	}
	if (t.Check.Contains == "") == (t.Check.JSONPath == "") {
		bad(".check names contains or json_path, not both")
	}
	for i, st := range t.Steps {
		if len(st.Run) == 0 {
			bad(".steps[%d].run is empty", i)
		}
	}
}

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// own says whether t is a target Go deploys.
func (t DeployTarget) own() bool { return t.Kind == "" }

// via is the host whose node runs t's steps.
func (t DeployTarget) runsOn() string {
	if t.Via == ViaSSH {
		return t.Node
	}
	return t.Host
}

// withTag fills {tag} into each of argv.
func withTag(argv []string, tag string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = strings.ReplaceAll(a, "{tag}", tag)
	}
	return out
}

// KindRelease is a step of a release item.
const KindRelease = "release"

// Release steps.
const (
	ReleaseSeen      = "seen"      // the tags there when Go began watching the repository: no item for them
	ReleaseOpened    = "opened"    // a new release tag: the item is open, built
	ReleaseGate      = "gate"      // the release gate's outcome, its output attached
	ReleaseWithdrawn = "withdrawn" // the gate failed: nothing deploys the tag
	ReleaseCheck     = "check"     // a target's check changed: whether the tag runs there, and what was seen
	ReleaseBehind    = "behind"    // the fleet members not on the tag changed
	ReleaseTold      = "told"      // the fleet's operators were asked to upgrade
	ReleaseEscalate  = "escalate"  // the owner was told what is not there yet
	ReleaseRetry     = "retry"     // the owner asked for a failed step again
	ReleasePosted    = "posted"    // a message about it was posted, once per key
	ReleaseRunning   = "running"   // every check holds: the item is done
)

var releaseSteps = []string{ReleaseSeen, ReleaseOpened, ReleaseGate, ReleaseWithdrawn, ReleaseCheck, ReleaseBehind, ReleaseTold, ReleaseEscalate, ReleaseRetry, ReleasePosted, ReleaseRunning}

// Release states.
const (
	ReleaseBuilt     = "built"     // tagged; the gate has not passed
	ReleaseDeploying = "deploying" // the gate passed: Go deploys and checks
	ReleaseBlocked   = "blocked"   // a step of ours failed and was rolled back: the owner is told
	ReleaseDone      = "running"   // every target's check holds
	ReleaseOut       = "withdrawn" // the gate failed
)

// ReleasePayload is one step of a release item.
type ReleasePayload struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag,omitempty"`
	Step       string `json:"step"`
	// Tags are the tags a seen step found already there.
	Tags []string `json:"tags,omitempty"`
	// Compat is the release's compatibility rule, from its tag's notes
	// or its CHANGELOG entry.
	Compat string `json:"compat,omitempty"`
	// A gate's outcome.
	Gate *ReleaseGateResult `json:"gate,omitempty"`
	// A check's target, whether it holds, and what was seen.
	Target string `json:"target,omitempty"`
	OK     bool   `json:"ok,omitempty"`
	Seen   string `json:"seen,omitempty"`
	// The fleet's members not on the tag, and those that are.
	Behind []string `json:"behind,omitempty"`
	On     int      `json:"on,omitempty"`
	// Text is what was posted, or why it was not; Key the message's.
	Text  string `json:"text,omitempty"`
	Key   string `json:"key,omitempty"`
	Error string `json:"error,omitempty"`
}

// ReleaseGateResult is a release gate's run: its exit status, whether it
// passed, its output's tail, and why it could not run.
type ReleaseGateResult struct {
	Exit   int    `json:"exit"`
	Passed bool   `json:"passed"`
	None   bool   `json:"none,omitempty"` // no gate is configured
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (p *ReleasePayload) check() error {
	if p.Repository == "" || !contains(releaseSteps, p.Step) || (p.Step == ReleaseSeen) != (p.Tag == "") {
		return errors.New("a release event names its repository, a step, and its tag unless it lists the tags seen")
	}
	if (p.Step == ReleaseGate) != (p.Gate != nil) || (p.Step == ReleaseCheck || p.Step == ReleaseBehind) && p.Target == "" {
		return errors.New("a release gate carries its outcome, and a check or behind names its target")
	}
	return nil
}

// Release is a release item as the log holds it.
type Release struct {
	Repository, Tag, Compat string
	Opened                  time.Time
	State                   string
	Gate                    *ReleaseGateResult
	GatedAt                 time.Time
	Targets                 map[string]*ReleaseTarget
	Told, Escalated         bool
	Retries                 int
	Posted                  map[string]bool
	Done                    time.Time
}

// ReleaseTarget is one target of a release: its check, the steps done
// and the one that failed, and for a fleet, who is not on the tag.
type ReleaseTarget struct {
	OK        bool
	Seen      string
	CheckedAt time.Time
	StepsDone int
	Failed    *DeployStepResult
	Behind    []string
	On        int
}

// terminal says whether r is closed.
func (r *Release) terminal() bool { return r.State == ReleaseDone || r.State == ReleaseOut }

func releaseKey(repo, tag string) string { return repo + " " + tag }

// at is r's target name, as folded, or a zero one.
func (r *Release) at(name string) ReleaseTarget {
	if t := r.Targets[name]; t != nil {
		return *t
	}
	return ReleaseTarget{}
}

func (r *Release) target(name string) *ReleaseTarget {
	t := r.Targets[name]
	if t == nil {
		t = &ReleaseTarget{}
		r.Targets[name] = t
	}
	return t
}

// applyRelease folds a release event.
func (st *LogState) applyRelease(e Event) {
	p := e.Release
	if st.Releases == nil {
		st.Releases, st.ReleaseTags = map[string]*Release{}, map[string]map[string]bool{}
	}
	tags := st.ReleaseTags[p.Repository]
	if tags == nil {
		tags = map[string]bool{}
		st.ReleaseTags[p.Repository] = tags
	}
	if p.Step == ReleaseSeen {
		for _, t := range p.Tags {
			tags[t] = true
		}
		return
	}
	k := releaseKey(p.Repository, p.Tag)
	r := st.Releases[k]
	if p.Step == ReleaseOpened {
		tags[p.Tag] = true
		if r == nil {
			st.Releases[k] = &Release{Repository: p.Repository, Tag: p.Tag, Compat: p.Compat, Opened: e.Time, State: ReleaseBuilt,
				Targets: map[string]*ReleaseTarget{}, Posted: map[string]bool{}}
		}
		return
	}
	if r == nil {
		return
	}
	switch p.Step {
	case ReleaseGate:
		r.Gate, r.GatedAt = p.Gate, e.Time
		if p.Gate.Passed && r.State == ReleaseBuilt {
			r.State = ReleaseDeploying
		}
	case ReleaseWithdrawn:
		r.State = ReleaseOut
	case ReleaseCheck:
		t := r.target(p.Target)
		t.OK, t.Seen, t.CheckedAt = p.OK, p.Seen, e.Time
	case ReleaseBehind:
		t := r.target(p.Target)
		t.Behind, t.On = p.Behind, p.On
	case ReleaseTold:
		r.Told = true
	case ReleaseEscalate:
		r.Escalated = true
	case ReleaseRetry:
		if r.State == ReleaseBlocked {
			r.Retries++
			r.State = ReleaseDeploying
			for _, t := range r.Targets {
				t.Failed = nil
			}
		}
	case ReleasePosted:
		r.Posted[p.Key] = true
	case ReleaseRunning:
		r.State, r.Done = ReleaseDone, e.Time
	}
}

// applyDeployResult folds a node's result of a release step into its
// release: a step done counts, and one failed blocks the release.
func (st *LogState) applyDeployResult(e Event) {
	res := e.Node.Result
	d := res.Deploy
	if d == nil {
		return
	}
	r := st.Releases[releaseKey(res.Repository, d.Tag)]
	if r == nil || r.terminal() || d.Attempt != r.Retries {
		return
	}
	t := r.target(d.Target)
	switch {
	case res.Outcome == NodeDone && d.Index == t.StepsDone:
		t.StepsDone++
	case res.Outcome != NodeDone && t.Failed == nil:
		f := *d
		f.Error = res.Error
		if f.Error == "" {
			f.Error = res.Outcome
		}
		t.Failed = &f
		if r.State == ReleaseDeploying {
			r.State = ReleaseBlocked
		}
	}
}

// sortedReleases are the releases, the newest first.
func (st *LogState) sortedReleases() []*Release {
	out := make([]*Release, 0, len(st.Releases))
	for _, r := range st.Releases {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Opened.Equal(out[j].Opened) {
			return out[i].Opened.After(out[j].Opened)
		}
		return releaseKey(out[i].Repository, out[i].Tag) < releaseKey(out[j].Repository, out[j].Tag)
	})
	return out
}

// deployStepID is the node command that runs a release's step: the same
// however often it is asked for, and new for each retry.
func deployStepID(repo, tag, target string, index, attempt int) string {
	return fmt.Sprintf("%s:%s:%s:%s:%d:%d", NodeDeployStep, repo, tag, target, index, attempt)
}

// deployCommands are what rec's host is to run for the releases now: for
// each release deploying, each of its targets whose steps run on the
// host, its first step not done, unless one failed. A step the node
// refused or that failed waits for the owner's retry.
func deployCommands(cfg *Config, st *LogState, rec *NodeRecord) []NodeCommand {
	var out []NodeCommand
	for _, r := range st.sortedReleases() {
		repo, ok := cfg.Repositories[r.Repository]
		if !ok || repo.Deploy == nil || r.State != ReleaseDeploying {
			continue
		}
		for _, tn := range sortedKeys(repo.Deploy.Targets) {
			t := repo.Deploy.Targets[tn]
			if !t.own() || t.runsOn() != rec.Host {
				continue
			}
			for i, step := range t.Steps {
				id := deployStepID(r.Repository, r.Tag, tn, i, r.Retries)
				if rec.Done[id] {
					continue
				}
				if _, failed := rec.Failed[id]; !failed {
					out = append(out, NodeCommand{ID: id, Action: NodeDeployStep, Repository: r.Repository, Path: layoutPath(repo),
						Deploy: &DeployStepCommand{Tag: r.Tag, Target: tn, Index: i, Attempt: r.Retries, Name: step.Name, Via: t.Via, SSH: sshHost(t),
							Run: withTag(step.Run, r.Tag), Check: withTag(step.Check, r.Tag), Rollback: withTag(step.Rollback, r.Tag), Timeout: step.Timeout}})
				}
				break
			}
		}
	}
	return out
}

func sshHost(t DeployTarget) string {
	if t.Via == ViaSSH {
		return t.Host
	}
	return ""
}

// releasesCmd prints the release items, or with retry asks for a release's
// failed step again.
func releasesCmd(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("releases", flag.ContinueOnError)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	log, events, err := openLoaded(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	switch rest := fs.Args(); {
	case len(rest) == 0:
		PrintReleases(w, cfg, NewLogState(cfg, ModeLive).Fold(events))
		return nil
	case len(rest) == 3 && rest[0] == "retry":
		return RetryRelease(cfg, log, events, time.Now(), rest[1], rest[2])
	case len(rest) == 3 && rest[0] == "open":
		return OpenRelease(cfg, log, events, ExecGit{Timeout: cfg.Timeouts.Forge.Duration}, time.Now(), rest[1], rest[2])
	}
	return errors.New("usage: agentd releases [open|retry REPOSITORY TAG]")
}

// OpenRelease records the owner's opening of a release item for a tag Go
// did not see made: one there when Go first read the clone.
func OpenRelease(cfg *Config, log *Log, events []Event, g Git, now time.Time, repo, tag string) error {
	d := cfg.Repositories[repo].Deploy
	if d == nil {
		return fmt.Errorf("repository %q has no deploy", repo)
	}
	if !d.tags.MatchString(tag) || !releaseTagPattern.MatchString(tag) || strings.Contains(tag, "..") {
		return fmt.Errorf("%q is not a release tag of %s (deploy.tags is %s)", tag, repo, d.Tags)
	}
	if NewLogState(cfg, ModeLive).Fold(events).Releases[releaseKey(repo, tag)] != nil {
		return fmt.Errorf("release %s of %s is open already", tag, repo)
	}
	notes, err := g.TagNotes(d.Clone, tag)
	if err != nil {
		return fmt.Errorf("tag %s in %s: %w", tag, d.Clone, err)
	}
	return log.Append(Event{V: EventVersion, Time: now, Kind: KindRelease, Mode: ModeOwner, Release: &ReleasePayload{Repository: repo, Tag: tag, Step: ReleaseOpened, Compat: compatRule(notes, tag)}})
}

// RetryRelease records the owner's retry of a blocked release: its steps
// are asked for again, from the one that failed.
func RetryRelease(cfg *Config, log *Log, events []Event, now time.Time, repo, tag string) error {
	st := NewLogState(cfg, ModeLive).Fold(events)
	r := st.Releases[releaseKey(repo, tag)]
	if r == nil || r.State != ReleaseBlocked {
		return fmt.Errorf("release %s of %s has no failed step to retry", tag, repo)
	}
	return log.Append(Event{V: EventVersion, Time: now, Kind: KindRelease, Mode: ModeOwner, Release: &ReleasePayload{Repository: repo, Tag: tag, Step: ReleaseRetry}})
}

// PrintReleases prints each release item: its tag, state, compat rule,
// gate and targets.
func PrintReleases(w io.Writer, cfg *Config, st *LogState) {
	rels := st.sortedReleases()
	if len(rels) == 0 {
		fmt.Fprintln(w, "no release items")
		return
	}
	for _, r := range rels {
		fmt.Fprintf(w, "%s %s: %s (opened %s)\n", r.Repository, r.Tag, r.State, r.Opened.UTC().Format(time.RFC3339))
		if r.Compat != "" {
			fmt.Fprintf(w, "  compat: %s\n", r.Compat)
		}
		if g := r.Gate; g != nil {
			switch {
			case g.None:
				fmt.Fprintln(w, "  gate: none configured")
			case g.Passed:
				fmt.Fprintln(w, "  gate: passed")
			default:
				fmt.Fprintf(w, "  gate: failed, exit %d %s\n", g.Exit, g.Error)
			}
		}
		if d := cfg.Repositories[r.Repository].Deploy; d != nil {
			for _, line := range releaseTargetLines(d, r) {
				fmt.Fprintf(w, "  %s\n", line)
			}
		}
	}
}

// releaseDoneWhen is a release item's done_when, as the owner reads it:
// every target's check, the fleets' by their thresholds.
func releaseDoneWhen(d *DeployConfig, tag string) string {
	var parts []string
	for _, tn := range sortedKeys(d.Targets) {
		t := d.Targets[tn]
		switch {
		case t.Kind == TargetFleet:
			parts = append(parts, fmt.Sprintf("%s: at least %d members' heartbeats on %s", tn, t.Threshold, tag))
		case t.Check.JSONPath != "":
			parts = append(parts, fmt.Sprintf("%s: %s %s is %q", tn, strings.ReplaceAll(t.Check.URL, "{tag}", tag), t.Check.JSONPath, strings.ReplaceAll(t.Check.Value, "{tag}", tag)))
		default:
			parts = append(parts, fmt.Sprintf("%s: %s contains %q", tn, strings.ReplaceAll(t.Check.URL, "{tag}", tag), strings.ReplaceAll(t.Check.Contains, "{tag}", tag)))
		}
	}
	return strings.Join(parts, "; ")
}

// releaseViews are st's release items on console as work items (#66):
// the item "release TAG running", its state, its done_when and each
// target's last check, the gate and the compat rule as its progress.
func releaseViews(cfg *Config, st *LogState, console string, all bool, now time.Time, secret func(...string) bool) []WorkView {
	var out []WorkView
	rels := st.sortedReleases()
	for i := len(rels) - 1; i >= 0; i-- { // oldest first, as work items are
		r := rels[i]
		d := cfg.Repositories[r.Repository].Deploy
		if d == nil || console != "" && r.Repository != console && cfg.RepositoryConsole(r.Repository) != console || !all && r.terminal() {
			continue
		}
		v := WorkView{ID: "release:" + r.Tag, Repository: r.Repository, Title: "release " + r.Tag + " running", State: r.State, Opened: r.Opened,
			Age: ago(now, r.Opened), DoneWhen: releaseDoneWhen(d, r.Tag), LastCheck: strings.Join(releaseTargetLines(d, r), "; ")}
		var progress []string
		switch g := r.Gate; {
		case g == nil:
			progress = append(progress, "the gate has not run")
		case g.None:
			progress = append(progress, "no gate configured")
		case g.Passed:
			progress = append(progress, "the gate passed")
		default:
			progress = append(progress, fmt.Sprintf("the gate failed, exit %d: withdrawn", g.Exit))
		}
		if r.Compat != "" {
			progress = append(progress, "compat: "+r.Compat)
		}
		v.Progress = strings.Join(progress, "; ")
		if secret(v.DoneWhen, v.LastCheck, v.Progress) {
			v.DoneWhen, v.LastCheck, v.Progress = withheldSecret, "", ""
		}
		out = append(out, v)
	}
	return out
}
