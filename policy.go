package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Repository policy (#35, #3 sections 4 and 7). Each repository's rules
// that agentd checks mechanically are one typed policy in the owner's
// configuration. A repository may add rules of its own in
// MergePolicyFile on its target branch, read at the tip when a request is
// about to merge; they may only narrow the owner's policy. A key that
// would widen it is refused, and the owner hears of it once per file. The
// effective policy is the owner's narrowed by the file last read, which a
// shared policy event records, so every process reads the same.

// RepoPolicy is a repository's policy.
type RepoPolicy struct {
	Delivery             DeliveryRules `json:"delivery"`
	Gate                 GateRules     `json:"gate"`
	Merge                MergeRules    `json:"merge"`
	Review               ReviewRules   `json:"review"`
	CI                   CIRules       `json:"ci"`
	Scope                ScopeRules    `json:"scope"`
	Sessions             SessionRules  `json:"sessions"`
	ConfidentialPatterns []string      `json:"confidential_patterns"`
	// Feed is which of its sessions' turns its console receives (turns.go).
	Feed FeedRules `json:"feed"`
	// Design is whether a contained design goes on without a go (#68).
	Design DesignRules `json:"design"`
}

// DeliveryRules: Allow is the writes delivery may make, push and open.
type DeliveryRules struct {
	Allow []string `json:"allow"`
}

// GateRules is the argv agentd runs in the worktree, on the exact commit,
// before it pushes or merges; delivery pushes only when it exits 0. A
// repository that allows push or merge must have one. Timeout bounds it,
// by default DefaultGateTimeout. It runs with PATH, LANG and TMPDIR, HOME
// a fresh empty directory, and the variables Env names, nothing else; no
// agent socket, token, Claude setting or HOME may be named. The gate must
// be the target branch's: its script, when Argv names one in the
// worktree, and the files or directories Paths names, relative to the
// worktree, must be as the target branch has them.
type GateRules struct {
	Argv    []string `json:"argv"`
	Timeout Duration `json:"timeout"`
	Env     []string `json:"env"`
	Paths   []string `json:"paths"`
}

// Merge methods.
const (
	MergeFastForward = "fast-forward" // the target moves to the gated head, nothing new is written
	MergeCommit      = "merge"        // the forge's merge call, a merge commit
	MergeSquash      = "squash"       // the forge's merge call, squashed
	MergeByCommand   = "command"      // the repository's own merge command
)

// What a merge may require at the request's exact head.
const (
	RequireGate   = "gate"   // agentd's gate passed on it
	RequireCI     = "ci"     // its CI succeeded on it
	RequireReview = "review" // its session's case stopped its review rounds converged
)

// MergeRules is when agentd merges a request: Enabled, into one of
// Targets, by one of Authors (forge usernames), not Excluded, approved by
// each of Approvals, with everything Require names at its head, by
// Method; Command is the method command's argv, with {repository},
// {request}, {head} and {target} substituted.
type MergeRules struct {
	Enabled   bool     `json:"enabled"`
	Method    string   `json:"method"`
	Command   []string `json:"command"`
	Targets   []string `json:"targets"`
	Authors   []string `json:"authors"`
	Exclude   Matches  `json:"exclude"`
	Approvals []string `json:"approvals"`
	Require   []string `json:"require"`
}

// Matches names requests or issues by number or by label.
type Matches struct {
	Numbers []int    `json:"numbers"`
	Labels  []string `json:"labels"`
}

// matches says whether number, or one of labels, is named.
func (m Matches) matches(number int, labels []string) bool {
	for _, n := range m.Numbers {
		if n == number {
			return true
		}
	}
	for _, l := range labels {
		if contains(m.Labels, l) {
			return true
		}
	}
	return false
}

// ReviewRules: the models review rounds use, their Rounds.Max, and Others,
// review-only for requests by other authors, which merge.authors
// already keeps from merging: the loop reviews one updated within
// OthersMaxAge (#55).
type ReviewRules struct {
	ReviewerModel string      `json:"reviewer_model"`
	JudgeModel    string      `json:"judge_model"`
	Rounds        RoundsRules `json:"rounds"`
	Others        string      `json:"others"`
	OthersMaxAge  Duration    `json:"others_max_age"`
}

// RoundsRules caps a case's review rounds; 0 leaves policies.rounds.max.
type RoundsRules struct {
	Max int `json:"max"`
}

// ReviewOnly is ReviewRules.Others for requests reviewed, never merged.
const ReviewOnly = "review-only"

// DefaultOthersMaxAge is review.others_max_age when the policy sets none.
const DefaultOthersMaxAge = 14 * 24 * time.Hour

// CIRules: Required adds ci to every request's merge.require, and
// RetestOnBaseMove retests a request whose target moved (#36).
type CIRules struct {
	Required         bool `json:"required"`
	RetestOnBaseMove bool `json:"retest_on_base_move"`
}

// ScopeRules: Reserved issues agentd and its sessions never start.
type ScopeRules struct {
	Reserved Matches `json:"reserved"`
}

// SessionRules are what the repository's started sessions may use:
// Accounts, Models, at most Max at once, Budget caps on each and on all
// of them, and a Fallback order of accounts (#39 acts on it). WorkModel
// is the model its work sessions run, over policies.models (#52). With no
// Accounts, its sessions run under the default login.
type SessionRules struct {
	Accounts  []string    `json:"accounts"`
	Max       int         `json:"max"`
	Models    []string    `json:"models"`
	Budget    BudgetRules `json:"budget"`
	Fallback  []string    `json:"fallback"`
	WorkModel string      `json:"work_model"`
}

// BudgetRules caps a repository's sessions' spend: each one's, and all of
// them together. 0 is no cap beyond policies.budget.
type BudgetRules struct {
	SessionTokens int64 `json:"session_tokens"`
	RunTokens     int64 `json:"run_tokens"`
}

// checkPolicy validates a repository's policy and fills its defaults,
// naming each key it defaulted.
func (c *Config) checkPolicy(name string, repo *Repository, errs *[]error) {
	p := &repo.Policy
	field := "repositories." + name + ".policy"
	bad := func(format string, a ...any) { *errs = append(*errs, fmt.Errorf(field+"."+format, a...)) }
	def := func(key string) { repo.defaulted = append(repo.defaulted, key) }
	for _, a := range p.Delivery.Allow {
		if a != "push" && a != "open" && a != ActComment {
			bad("delivery.allow: %q is not push, open or comment", a)
		}
		if a == "push" && len(p.Gate.Argv) == 0 {
			bad("delivery.allow has push but gate.argv is empty: nothing is delivered without a gate")
		}
	}
	if p.Design.SpendLimitUSD < 0 {
		bad("design.spend_limit_usd must not be negative")
	}
	if p.Gate.Timeout.Duration < 0 {
		bad("gate.timeout must not be negative")
	}
	if p.Gate.Timeout.Duration == 0 {
		p.Gate.Timeout = Duration{DefaultGateTimeout}
		def("gate.timeout")
	}
	for _, v := range p.Gate.Env {
		if secretEnv(v) || v == "" || strings.Contains(v, "=") {
			bad("gate.env: %q may not be passed to a gate", v)
		}
	}
	for _, q := range p.Gate.Paths {
		if q == "" || filepath.IsAbs(q) || filepath.Clean(q) != q || q == "." || q == ".." || strings.HasPrefix(q, "../") || strings.ContainsAny(q, "*?[\\:") {
			bad("gate.paths: %q must be a clean path inside the worktree, without wildcards", q)
		}
	}
	m := &p.Merge
	if m.Method == "" {
		m.Method = MergeFastForward
		def("merge.method")
	}
	switch m.Method {
	case MergeFastForward, MergeCommit, MergeSquash:
		if len(m.Command) > 0 {
			bad("merge.command is for method command only")
		}
	case MergeByCommand:
		if len(m.Command) == 0 || m.Command[0] == "" {
			bad("merge.method command needs merge.command, an argv")
		}
	default:
		bad("merge.method %q is not fast-forward, merge, squash or command", m.Method)
	}
	if m.Require == nil {
		m.Require = []string{RequireGate, RequireCI}
		def("merge.require")
	}
	for _, r := range m.Require {
		if r != RequireGate && r != RequireCI && r != RequireReview {
			bad("merge.require: %q is not gate, ci or review", r)
		}
	}
	if m.Enabled {
		if len(p.Gate.Argv) == 0 {
			bad("merge.enabled but gate.argv is empty: nothing is merged without a gate")
		}
		if !contains(m.Require, RequireGate) {
			bad("merge.require must name gate: nothing is merged that agentd's gate did not pass")
		}
		if len(m.Targets) == 0 || len(m.Authors) == 0 {
			bad("merge.enabled needs targets and authors: merge never means any target or any author")
		}
	}
	for _, b := range m.Targets {
		if !plainBranch(b) {
			bad("merge.targets: %q is not a plain branch name", b)
		}
	}
	for _, list := range [][]string{m.Authors, m.Approvals, m.Exclude.Labels, p.Scope.Reserved.Labels, p.Sessions.Models} {
		for _, v := range list {
			if strings.TrimSpace(v) == "" {
				bad("names and labels must not be empty")
			}
		}
	}
	for _, n := range append(append([]int{}, m.Exclude.Numbers...), p.Scope.Reserved.Numbers...) {
		if n <= 0 {
			bad("merge.exclude and scope.reserved numbers must be 1 or more")
		}
	}
	if p.Review.Rounds.Max < 0 || p.Review.Rounds.Max > maxRounds {
		bad("review.rounds.max must be 0 to %d", maxRounds)
	}
	if o := p.Review.Others; o != "" && o != ReviewOnly {
		bad("review.others %q is not review-only", o)
	}
	if p.Review.OthersMaxAge.Duration < 0 {
		bad("review.others_max_age must not be negative")
	}
	if p.Review.OthersMaxAge.Duration == 0 {
		p.Review.OthersMaxAge = Duration{DefaultOthersMaxAge}
		def("review.others_max_age")
	}
	s := &p.Sessions
	if s.Max < 0 || s.Budget.SessionTokens < 0 || s.Budget.RunTokens < 0 {
		bad("sessions: max and budget must not be negative")
	}
	// A started session runs one of the models it may use.
	if len(s.Models) > 0 {
		g := c.Policies.Models
		for _, m := range []string{orModel(s.WorkModel, g.Work), orModel(p.Review.ReviewerModel, g.Reviewer), orModel(p.Review.JudgeModel, g.Judge)} {
			if m != "" && !contains(s.Models, m) {
				bad("sessions.models does not have %q, a model its sessions would run", m)
			}
		}
	}
	if s.Max == 0 {
		s.Max = DefaultMaxSessions
		def("sessions.max")
	}
	for _, a := range append(append([]string{}, s.Accounts...), s.Fallback...) {
		if _, ok := c.Accounts[a]; a != "" && !ok {
			bad("sessions: account %q is not in accounts", a)
		}
	}
	for _, a := range s.Fallback {
		if a == "" {
			bad("sessions.fallback: an account must be named; the default login is never a fallback")
		}
	}
	for i, pat := range p.ConfidentialPatterns {
		if _, err := regexp.Compile(pat); err != nil {
			bad("confidential_patterns[%d]: %v", i, err)
		}
	}
}

// roundsFor is the review rounds policy for the repository with key
// repo, as cases name it: policies.rounds, its max the owner's policy's
// when it sets one, lowered by the repository's own files.
func (c *Config) roundsFor(st *LogState, repo string) RoundsPolicy {
	// Repositories that share a project share its cases: the lowest of
	// their maxes holds.
	p := c.Policies.Rounds
	lowest := 0
	for _, name := range sortedKeys(c.Repositories) {
		r := c.Repositories[name]
		if repoKey(r.Forge, r.Project) != repo {
			continue
		}
		m := c.Policies.Rounds.Max
		if r.Policy.Review.Rounds.Max > 0 {
			m = r.Policy.Review.Rounds.Max
		}
		if eff, _ := effectivePolicy(c, st, name); eff.Review.Rounds.Max > 0 && eff.Review.Rounds.Max < m {
			m = eff.Review.Rounds.Max
		}
		if lowest == 0 || m < lowest {
			lowest = m
		}
	}
	if lowest > 0 {
		p.Max = lowest
	}
	return p
}

// screenPatterns is every pattern agentd screens with: the global ones and
// each repository's, so a repository's patterns screen everything agentd
// logs, which is stricter than its own sessions' text alone.
func screenPatterns(cfg *Config) []string {
	out := append([]string{}, cfg.ConfidentialPatterns...)
	names := make([]string, 0, len(cfg.Repositories))
	for n := range cfg.Repositories {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, cfg.Repositories[n].Policy.ConfidentialPatterns...)
	}
	return out
}

// filePatterns are the confidential patterns repositories' own files
// added, compiled once as each file's read is applied, in the order of
// their keys: they screen after the configuration's.
func filePatterns(st *LogState) []*regexp.Regexp {
	if st == nil || len(st.filePats) == 0 {
		return nil
	}
	var out []*regexp.Regexp
	for _, k := range sortedKeys(st.filePats) {
		out = append(out, st.filePats[k]...)
	}
	return out
}

// policyKey keys a repository's file on one target branch.
func policyKey(repo, target string) string { return repo + "\x00" + target }

// PolicyPayload is a repository's own policy file as read at its target's
// tip: the file, compact, its hash, and the keys of it that applied and
// that were refused. A file that matched a confidential pattern is not
// kept; it is refused whole.
type PolicyPayload struct {
	Repository string          `json:"repository"`
	Target     string          `json:"target"`
	Tip        string          `json:"tip"`
	Hash       string          `json:"hash"`
	File       json.RawMessage `json:"file,omitempty"`
	Applied    []string        `json:"applied,omitempty"`
	Refused    []string        `json:"refused,omitempty"`
	Withheld   int             `json:"withheld,omitempty"`
}

// policyFileMax bounds the repository file agentd reads.
const policyFileMax = 16 * 1024

// narrowPolicy applies a repository's own file to the owner's policy: each
// key that narrows it applies, each that would widen it, or that the
// owner's policy has not, is refused. A file that is no JSON object is
// refused whole, as "file".
func narrowPolicy(owner RepoPolicy, file []byte) (RepoPolicy, []string, []string) {
	eff := clonePolicy(owner)
	var sections map[string]json.RawMessage
	if len(file) > policyFileMax || json.Unmarshal(file, &sections) != nil || sections == nil {
		return eff, nil, []string{"file"}
	}
	var applied, refused []string
	verdict := func(key string, ok bool) {
		if ok {
			applied = append(applied, key)
		} else {
			refused = append(refused, key)
		}
	}
	for _, sec := range sortedKeys(sections) {
		raw := sections[sec]
		if sec == "confidential_patterns" {
			var pats []string
			ok := strictJSON(raw, &pats) == nil
			for _, p := range pats {
				if _, err := regexp.Compile(p); err != nil {
					ok = false
				}
			}
			if ok {
				eff.ConfidentialPatterns = union(eff.ConfidentialPatterns, pats)
			}
			verdict(sec, ok)
			continue
		}
		var keys map[string]json.RawMessage
		if json.Unmarshal(raw, &keys) != nil || keys == nil {
			verdict(sec, false)
			continue
		}
		for _, k := range sortedKeys(keys) {
			verdict(sec+"."+k, narrowKey(&eff, sec+"."+k, keys[k]))
		}
	}
	return eff, applied, refused
}

// narrowKey applies one key of a repository file to eff, if it narrows it.
func narrowKey(eff *RepoPolicy, key string, raw json.RawMessage) bool {
	var strs []string
	var b bool
	var n int64
	var m Matches
	subset := func(have *[]string) bool {
		if strictJSON(raw, &strs) != nil || !isSubset(strs, *have) || len(strs) == 0 && key == "sessions.models" {
			return false
		}
		*have = strs
		return true
	}
	more := func(have *[]string) bool {
		if strictJSON(raw, &strs) != nil || hasEmpty(strs) {
			return false
		}
		*have = union(*have, strs)
		return true
	}
	matches := func(have *Matches) bool {
		if strictJSON(raw, &m) != nil || hasEmpty(m.Labels) {
			return false
		}
		for _, x := range m.Numbers {
			if x <= 0 {
				return false
			}
		}
		have.Labels = union(have.Labels, m.Labels)
		for _, x := range m.Numbers {
			if !containsInt(have.Numbers, x) {
				have.Numbers = append(have.Numbers, x)
			}
		}
		return true
	}
	lower := func(have *int64) bool {
		if strictJSON(raw, &n) != nil || n <= 0 || *have > 0 && n > *have {
			return false
		}
		*have = n
		return true
	}
	// An empty list is the same as none.
	same := func(have any) bool {
		v := reflect.New(reflect.TypeOf(have).Elem())
		got, want := v.Elem(), reflect.ValueOf(have).Elem()
		if strictJSON(raw, v.Interface()) != nil {
			return false
		}
		if got.Kind() == reflect.Slice && got.Len() == 0 && want.Len() == 0 {
			return true
		}
		return reflect.DeepEqual(got.Interface(), want.Interface())
	}
	switch key {
	case "gate.argv":
		return same(&eff.Gate.Argv)
	case "gate.timeout":
		return same(&eff.Gate.Timeout)
	case "gate.env":
		return same(&eff.Gate.Env)
	case "gate.paths":
		return same(&eff.Gate.Paths)
	case "merge.enabled":
		if strictJSON(raw, &b) != nil || b && !eff.Merge.Enabled {
			return false
		}
		eff.Merge.Enabled = b
		return true
	case "merge.method":
		return same(&eff.Merge.Method)
	case "merge.command":
		return same(&eff.Merge.Command)
	case "merge.targets":
		return subset(&eff.Merge.Targets)
	case "merge.authors":
		return subset(&eff.Merge.Authors)
	case "merge.exclude":
		return matches(&eff.Merge.Exclude)
	case "merge.approvals":
		return more(&eff.Merge.Approvals)
	case "merge.require":
		if strictJSON(raw, &strs) != nil {
			return false
		}
		for _, r := range strs {
			if r != RequireGate && r != RequireCI && r != RequireReview {
				return false
			}
		}
		eff.Merge.Require = union(eff.Merge.Require, strs)
		return true
	case "review.reviewer_model":
		return same(&eff.Review.ReviewerModel)
	case "review.judge_model":
		return same(&eff.Review.JudgeModel)
	case "review.rounds":
		var r RoundsRules
		// Under an owner's 0, policies.rounds.max holds, which roundsFor
		// takes when it is lower.
		if strictJSON(raw, &r) != nil || r.Max <= 0 || r.Max > maxRounds || eff.Review.Rounds.Max > 0 && r.Max > eff.Review.Rounds.Max {
			return false
		}
		eff.Review.Rounds = r
		return true
	case "review.others":
		var o string
		if strictJSON(raw, &o) != nil || o != ReviewOnly && o != eff.Review.Others {
			return false
		}
		eff.Review.Others = o
		return true
	case "review.others_max_age":
		var d Duration
		if strictJSON(raw, &d) != nil || d.Duration <= 0 || eff.Review.OthersMaxAge.Duration > 0 && d.Duration > eff.Review.OthersMaxAge.Duration {
			return false
		}
		eff.Review.OthersMaxAge = d
		return true
	case "design.auto":
		if strictJSON(raw, &b) != nil || b && !eff.Design.Auto {
			return false
		}
		eff.Design.Auto = b
		return true
	case "design.spend_limit_usd":
		var f float64
		if strictJSON(raw, &f) != nil || f < 0 || f > eff.Design.SpendLimitUSD {
			return false
		}
		eff.Design.SpendLimitUSD = f
		return true
	case "ci.required", "ci.retest_on_base_move":
		have := &eff.CI.Required
		if key == "ci.retest_on_base_move" {
			have = &eff.CI.RetestOnBaseMove
		}
		if strictJSON(raw, &b) != nil || !b && *have {
			return false
		}
		*have = b
		return true
	case "scope.reserved":
		return matches(&eff.Scope.Reserved)
	case "sessions.accounts":
		return subset(&eff.Sessions.Accounts)
	case "sessions.models":
		// No list to narrow is any model, and an empty one would read
		// the same.
		if len(eff.Sessions.Models) == 0 {
			return false
		}
		return subset(&eff.Sessions.Models)
	case "sessions.fallback":
		return same(&eff.Sessions.Fallback)
	case "sessions.work_model":
		return same(&eff.Sessions.WorkModel)
	case "sessions.max":
		max := int64(eff.Sessions.Max)
		if !lower(&max) {
			return false
		}
		eff.Sessions.Max = int(max)
		return true
	case "sessions.budget":
		var bud BudgetRules
		if strictJSON(raw, &bud) != nil {
			return false
		}
		next := eff.Sessions.Budget
		for _, c := range []struct {
			have *int64
			want int64
		}{{&next.SessionTokens, bud.SessionTokens}, {&next.RunTokens, bud.RunTokens}} {
			if c.want == 0 {
				continue
			}
			if c.want < 0 || *c.have > 0 && c.want > *c.have {
				return false
			}
			*c.have = c.want
		}
		eff.Sessions.Budget = next
		return true
	}
	return false
}

// strictJSON decodes raw into v, refusing unknown fields and trailing data.
func strictJSON(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// clonePolicy copies a policy, its lists too, so narrowing never changes
// the configuration's.
func clonePolicy(p RepoPolicy) RepoPolicy {
	b, _ := json.Marshal(p)
	var c RepoPolicy
	json.Unmarshal(b, &c)
	return c
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func isSubset(xs, of []string) bool {
	for _, x := range xs {
		if !contains(of, x) {
			return false
		}
	}
	return true
}

func union(a, b []string) []string {
	out := append([]string{}, a...)
	for _, x := range b {
		if !contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func hasEmpty(xs []string) bool {
	for _, x := range xs {
		if strings.TrimSpace(x) == "" {
			return true
		}
	}
	return false
}

func containsInt(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// effectivePolicy is a repository's policy as it stands: the owner's,
// narrowed by each of the repository's own files last read, one per
// target branch, and those reads. What a session may start, spend and
// see reads it; a merge reads its own target's file alone (effectiveFor).
func effectivePolicy(cfg *Config, st *LogState, repo string) (RepoPolicy, []PolicyPayload) {
	eff := cfg.Repositories[repo].Policy
	if st == nil {
		return eff, nil
	}
	var files []PolicyPayload
	targets := eff.Merge.Targets
	for _, k := range sortedKeys(st.Policies) {
		if f := st.Policies[k]; f.Repository == repo && contains(targets, f.Target) {
			eff = narrowBy(eff, f)
			files = append(files, f)
		}
	}
	return eff, files
}

// effectiveFor is the policy for a merge into target: the owner's
// narrowed by target's own file as last read.
func effectiveFor(cfg *Config, st *LogState, repo, target string) RepoPolicy {
	eff := cfg.Repositories[repo].Policy
	if f, ok := st.Policies[policyKey(repo, target)]; ok {
		eff = narrowBy(eff, f)
	}
	return eff
}

// narrowBy narrows p by a file's read. A file withheld for matching a
// confidential pattern cannot be read, so it turns merge and design.auto off: what it
// says is unknown, and it may narrow.
func narrowBy(p RepoPolicy, f PolicyPayload) RepoPolicy {
	if f.Withheld > 0 {
		p = clonePolicy(p)
		p.Merge.Enabled, p.Design.Auto = false, false
		return p
	}
	if len(f.File) == 0 {
		return p
	}
	eff, _, _ := narrowPolicy(p, f.File)
	return eff
}

// applyPolicy folds a repository file's read, and compiles the
// confidential patterns it adds.
func (st *LogState) applyPolicy(e Event) {
	if st.Policies == nil {
		st.Policies = map[string]PolicyPayload{}
	}
	if st.filePats == nil {
		st.filePats = map[string][]*regexp.Regexp{}
	}
	p := *e.Policy
	key := policyKey(p.Repository, p.Target)
	st.Policies[key] = p
	delete(st.filePats, key)
	if st.cfg == nil || len(p.File) == 0 {
		return
	}
	owner := st.cfg.Repositories[p.Repository].Policy
	eff, _, _ := narrowPolicy(owner, p.File)
	for _, pat := range eff.ConfidentialPatterns {
		if re, err := regexp.Compile(pat); err == nil && !contains(owner.ConfidentialPatterns, pat) {
			st.filePats[key] = append(st.filePats[key], re)
		}
	}
}

// notePolicy records what the repository's own file on target at tip
// says, once per file and target: the keys applied and refused. A read
// that refused keys, or a file withheld, goes to the owner once, as the
// read is recorded. It returns the policy for a merge into target.
func (s *Supervisor) notePolicy(name string, t *Track, repo, target, tip string, file []byte) RepoPolicy {
	hash := ""
	if file != nil {
		hash = shortHash(HashPane(string(file)))
	}
	p := PolicyPayload{Repository: repo, Target: target, Tip: tip, Hash: hash}
	if file != nil {
		// The file is screened with the configuration's patterns alone,
		// never with those a file added, so a file cannot withhold
		// itself. The file is kept, compact, so any process narrows by it
		// the same way; one that is no JSON, or too long, is refused whole.
		if n := s.configMatch(string(file)); n > 0 {
			p.Refused, p.Withheld = []string{"file"}, n
		} else {
			_, p.Applied, p.Refused = narrowPolicy(s.cfg.Repositories[repo].Policy, file)
			var buf bytes.Buffer
			if len(file) <= policyFileMax && json.Compact(&buf, file) == nil {
				p.File = buf.Bytes()
			}
		}
	}
	// A file is screened at every read, so a pattern changed since lets
	// it be read, or withholds it, at once.
	if prev, ok := s.st.Policies[policyKey(repo, target)]; !ok || prev.Hash != p.Hash || (prev.Withheld > 0) != (p.Withheld > 0) {
		if s.emit("", Event{Kind: KindPolicy, Mode: ModeShared, Policy: &p}); s.halt != nil {
			return effectiveFor(s.cfg, s.st, repo, target)
		}
		switch {
		case p.Withheld > 0:
			s.escalate(name, "policy:"+repo+":"+target+":"+hash, fmt.Sprintf("%s's own %s on %s at %s matches confidential pattern %d, so agentd cannot read it, and merges into %s are off until it changes. Change the file, or the patterns.",
				repo, MergePolicyFile, target, shortHash(tip), p.Withheld, target), t, s.clock.Now())
		case len(p.Refused) > 0:
			s.escalate(name, "policy:"+repo+":"+target+":"+hash, fmt.Sprintf("%s's own %s on %s at %s would widen your policy for it, or names keys that are yours alone, so these keys are refused: %s. "+
				"A repository's file may only narrow the policy in your configuration, and never delivery.allow; the keys that narrow it apply. Change the file, or your configuration.",
				repo, MergePolicyFile, target, shortHash(tip), strings.Join(p.Refused, ", "))+removedNote(p.Refused), t, s.clock.Now())
		}
	}
	return effectiveFor(s.cfg, s.st, repo, target)
}

// removedNote names what replaced a key the coordinator's removal took
// away (#53), when a repository's file still has it.
func removedNote(refused []string) string {
	if !contains(refused, "sessions.coordinator_model") {
		return ""
	}
	return " sessions.coordinator_model was removed in #53: agentd runs the repository's request loop itself, with no coordinator; review.reviewer_model and review.judge_model choose its judgment sessions' models."
}

// policyKeys are the keys of a policy, section.key, as JSON names them.
func policyKeys(p RepoPolicy) map[string]any {
	b, _ := json.Marshal(p)
	var sections map[string]json.RawMessage
	json.Unmarshal(b, &sections)
	out := map[string]any{}
	for sec, raw := range sections {
		var keys map[string]any
		if json.Unmarshal(raw, &keys) != nil {
			var v any
			json.Unmarshal(raw, &v)
			out[sec] = v
			continue
		}
		for k, v := range keys {
			out[sec+"."+k] = v
		}
	}
	return out
}

// PolicyKey is one key of an effective policy, its value and where it
// came from: owner, default, or repository at a tip.
type PolicyKey struct {
	Key    string `json:"key"`
	Value  any    `json:"value"`
	Source string `json:"source"`
}

// PolicyView is what agentd policy show prints: each key, and each of
// the repository's own files as last read.
type PolicyView struct {
	Repository string       `json:"repository"`
	Keys       []PolicyKey  `json:"keys"`
	Files      []PolicyFile `json:"files,omitempty"`
}

// PolicyFile is a repository's own file as policy show reports it.
type PolicyFile struct {
	Target   string   `json:"target"`
	Tip      string   `json:"tip"`
	Refused  []string `json:"refused,omitempty"`
	Withheld bool     `json:"withheld,omitempty"`
}

// showPolicy attributes each key of repo's effective policy. Which keys a
// file applied is worked out again against the configuration as it is.
func showPolicy(cfg *Config, st *LogState, repo string) PolicyView {
	r := cfg.Repositories[repo]
	eff := r.Policy
	source := map[string]string{}
	v := PolicyView{Repository: repo}
	_, files := effectivePolicy(cfg, st, repo)
	for _, f := range files {
		v.Files = append(v.Files, PolicyFile{Target: f.Target, Tip: f.Tip, Refused: f.Refused, Withheld: f.Withheld > 0})
		from := "repository " + MergePolicyFile + " on " + f.Target + " at " + shortHash(f.Tip)
		if f.Withheld > 0 {
			source["merge.enabled"] = from + " (withheld)"
		} else if len(f.File) > 0 {
			var applied []string
			eff, applied, _ = narrowPolicy(eff, f.File)
			for _, k := range applied {
				source[k] = from
			}
			continue
		}
		eff = narrowBy(eff, f)
	}
	keys := policyKeys(eff)
	for _, k := range sortedKeys(keys) {
		src := "owner"
		switch {
		case source[k] != "":
			src = source[k]
		case contains(r.defaulted, k):
			src = "default"
		}
		v.Keys = append(v.Keys, PolicyKey{Key: k, Value: keys[k], Source: src})
	}
	return v
}

// policyCmd is agentd policy show REPO: the repository's effective policy,
// as compact JSON, each key with its source. The repository's own file is
// as the log last recorded it, a shared fact of any run.
func policyCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("policy", flag.ContinueOnError)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 2 || rest[0] != "show" {
		return fmt.Errorf("usage: agentd policy show REPO")
	}
	if _, ok := cfg.Repositories[rest[1]]; !ok {
		return fmt.Errorf("repository %q is not configured", rest[1])
	}
	events, _, err := ReadLog(cfg.EventLog)
	if err != nil {
		return err
	}
	b, err := json.Marshal(showPolicy(cfg, NewLogState(cfg, ModeLive).Fold(events), rest[1]))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(b))
	return err
}
