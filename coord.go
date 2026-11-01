package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Coordination with the owner's laptop (W8510403e, on #29). The laptop
// runs a node that only observes: it reports each watched clone's branch,
// changed paths, unpushed paths and local branches, and the Claude Code
// sessions running there, and the hub screens and logs the report. On
// every pass, and before it starts any work, agentd compares that work on
// the laptop with its own here: running sessions' worktrees and branches,
// open requests' changed files, and the files open or designed items'
// briefs name. An overlap is a card on the repository's page, which the
// owner answers hold, go on, or "the laptop work is the item"; until go
// on, agentd starts nothing on what the laptop holds.

// KindLaptop is coordination's event: a request's changed files as read,
// an overlap opened or closed, or the owner's choice on one.
const KindLaptop = "laptop"

// Coordination ops.
const (
	LaptopFiles  = "files"
	LaptopOpen   = "open"
	LaptopClose  = "close"
	LaptopChoice = "choice"
)

// The owner's choices on an overlap's card.
const (
	CoordHold   = "hold"    // agentd's work waits, as with no choice
	CoordGoOn   = "go-on"   // agentd's work goes on despite this overlap
	CoordIsItem = "is-item" // the laptop work is the item: its session continues the laptop branch
)

// LaptopPayload is one coordination event. Files is a request's changed
// files at Version, its head (or its last change, when no head is known).
// Overlap is the overlap opened; Key the one closed or chosen on.
type LaptopPayload struct {
	Op         string   `json:"op"`
	Repository string   `json:"repository"`
	Key        string   `json:"key,omitempty"`
	Ref        string   `json:"ref,omitempty"`
	Version    string   `json:"version,omitempty"`
	Files      []string `json:"files,omitempty"`
	Error      string   `json:"error,omitempty"`
	Overlap    *Overlap `json:"overlap,omitempty"`
	Choice     string   `json:"choice,omitempty"`
}

func (p *LaptopPayload) check() error {
	switch {
	case p.Repository == "":
		return fmt.Errorf("a laptop event names its repository")
	case p.Op == LaptopFiles && p.Ref == "":
		return fmt.Errorf("a request's files name the request")
	case p.Op == LaptopOpen && (p.Overlap == nil || p.Overlap.Key == "" || p.Overlap.Key != p.Key):
		return fmt.Errorf("an overlap opened carries itself under its key")
	case (p.Op == LaptopClose || p.Op == LaptopChoice) && p.Key == "":
		return fmt.Errorf("a %s names the overlap's key", p.Op)
	case p.Op == LaptopChoice && p.Choice != CoordHold && p.Choice != CoordGoOn && p.Choice != CoordIsItem:
		return fmt.Errorf("a choice is %s, %s or %s", CoordHold, CoordGoOn, CoordIsItem)
	case p.Op != LaptopFiles && p.Op != LaptopOpen && p.Op != LaptopClose && p.Op != LaptopChoice:
		return fmt.Errorf("unknown laptop op %q", p.Op)
	}
	return nil
}

// Overlap is laptop work that meets agentd's here: on a host, in a
// repository, the laptop's branch (Laptop) and the build host's work it
// meets, a session, request, item or start, by Work. Branch and Issue
// are that work's; Paths the paths both touch; Why what made it one:
// "branch", "path" or "issue". Holds says the laptop holds uncommitted or
// unpushed changes on it. Asked is when a start was last held on it, for
// a start's overlap.
type Overlap struct {
	Key        string    `json:"key"`
	Host       string    `json:"host"`
	Repository string    `json:"repository"`
	Laptop     string    `json:"laptop"`
	Work       string    `json:"work"`
	WorkKind   string    `json:"work_kind"`
	Item       string    `json:"item,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	Issue      int       `json:"issue,omitempty"`
	Paths      []string  `json:"paths,omitempty"`
	Why        string    `json:"why"`
	Holds      bool      `json:"holds,omitempty"`
	Sentence   string    `json:"sentence"`
	Asked      time.Time `json:"asked,omitempty"`
}

// Work kinds an overlap meets.
const (
	WorkKindSession = "session"
	WorkKindRequest = "request"
	WorkKindItem    = "item"
	WorkKindStart   = "start"
)

// startOverlapKept is how long a held start's card stays without the
// start being tried again.
const startOverlapKept = time.Hour

// overlapKey is an overlap's stable key: the host, the repository, the
// laptop's branch and the work it meets.
func overlapKey(host, repo, laptop, work string) string {
	return host + "|" + repo + "|" + laptop + "|" + work
}

// LaptopSeen is an observing host's latest report, and when the hub took
// it.
type LaptopSeen struct {
	Host   string
	LastAt time.Time
	Report NodeReport
}

// CoordState is coordination's part of the fold: each host's latest
// report, the requests' files, the open overlaps, the owner's choices on
// them, and the laptop branches the owner said are an item's, by item.
type CoordState struct {
	Seen     map[string]*LaptopSeen
	Files    map[string]LaptopPayload // by repository and ref
	Overlaps map[string]*Overlap
	Choices  map[string]string
	Links    map[string]Overlap
}

func (st *LogState) coord() *CoordState {
	if st.Coord == nil {
		st.Coord = &CoordState{Seen: map[string]*LaptopSeen{}, Files: map[string]LaptopPayload{}, Overlaps: map[string]*Overlap{},
			Choices: map[string]string{}, Links: map[string]Overlap{}}
	}
	return st.Coord
}

// applyNodeReport keeps each host's latest report.
func (st *LogState) applyNodeReport(e Event) {
	if e.Node == nil || e.Node.Report == nil {
		return
	}
	if e.Node.Report.Same {
		if seen := st.coord().Seen[e.Node.Host]; seen != nil {
			c := *seen
			c.LastAt = e.Time
			st.Coord.Seen[e.Node.Host] = &c
		}
		return
	}
	st.coord().Seen[e.Node.Host] = &LaptopSeen{Host: e.Node.Host, LastAt: e.Time, Report: *e.Node.Report}
}

func (st *LogState) applyLaptop(e Event) {
	p, c := e.Laptop, st.coord()
	switch p.Op {
	case LaptopFiles:
		c.Files[filesKey(p.Repository, p.Ref)] = *p
	case LaptopOpen:
		o := *p.Overlap
		c.Overlaps[p.Key] = &o
	case LaptopClose:
		delete(c.Overlaps, p.Key)
		if c.Choices[p.Key] != CoordIsItem {
			delete(c.Choices, p.Key)
		}
	case LaptopChoice:
		c.Choices[p.Key] = p.Choice
		if o := c.Overlaps[p.Key]; o != nil && p.Choice == CoordIsItem && o.Item != "" {
			c.Links[o.Item] = *o
		} else if o != nil && o.Item != "" && c.Links[o.Item].Key == p.Key {
			delete(c.Links, o.Item)
		}
	}
}

func filesKey(repo, ref string) string { return repo + "\x00" + ref }

// observing says whether host is one agentd only watches.
func observing(cfg *Config, host string) bool { return cfg.Hosts[host].Observe }

// withheldPath is what a report holds in place of a path, branch or
// directory that matched a confidential pattern; it meets nothing.
const withheldPath = "(withheld: matches a confidential pattern)"

// screenNodeReport withholds every path, branch name and directory in a
// report that matches a confidential pattern, before the hub logs it.
func screenNodeReport(cfg *Config, r *NodeReport) {
	secret := confidentialMatcher(cfg)
	one := func(s string) string {
		if s != "" && secret(s) {
			return withheldPath
		}
		return s
	}
	all := func(ps []string) {
		for i := range ps {
			ps[i] = one(ps[i])
		}
	}
	for i := range r.Clones {
		c := &r.Clones[i]
		c.Branch, c.Error, c.Root = one(c.Branch), one(c.Error), one(c.Root)
		for k := range c.Pushed {
			c.Pushed[k] = one(c.Pushed[k])
		}
		all(c.Changed)
		all(c.Unpushed)
		all(c.OpenState)
		for k := range c.Local {
			c.Local[k].Name = one(c.Local[k].Name)
			all(c.Local[k].Paths)
		}
	}
	for i := range r.Claude {
		c := &r.Claude[i]
		c.Cwd, c.Branch = one(c.Cwd), one(c.Branch)
		all(c.Changed)
	}
	r.ClaudeError = one(r.ClaudeError)
	for i := range r.Sessions {
		r.Sessions[i].Error = one(r.Sessions[i].Error)
	}
}

// laptopWork is one piece of work on an observing host: a branch of a
// clone, a local branch, or a Claude Code session's worktree, with the
// paths it touches and whether it holds changes not on the forge.
type laptopWork struct {
	Host, Repository, Branch string
	Paths                    []string
	Holds                    bool
	Pushed                   bool // the branch is on the forge with nothing unpushed
}

// detachedHead is a clone's work with no branch checked out.
const detachedHead = "a detached HEAD"

// laptopWorks are the observing hosts' work, by repository, from their
// latest reports.
func laptopWorks(cfg *Config, c *CoordState) []laptopWork {
	var out []laptopWork
	if c == nil {
		return nil
	}
	for _, h := range sortedKeys(c.Seen) {
		if !observing(cfg, h) {
			continue
		}
		rep := c.Seen[h].Report
		byKey := map[string]int{}
		add := func(w laptopWork) {
			if w.Branch == withheldPath {
				return
			}
			k := w.Repository + "\x00" + w.Branch
			if i, ok := byKey[k]; ok {
				o := &out[i]
				o.Paths = unionPaths(o.Paths, w.Paths)
				o.Holds = o.Holds || w.Holds
				if w.Holds {
					o.Pushed = false // a session's worktree that holds nothing leaves it as it is
				}
				return
			}
			w.Host = h
			w.Paths = unionPaths(nil, w.Paths)
			byKey[k] = len(out)
			out = append(out, w)
		}
		for _, cl := range rep.Clones {
			if !cl.Exists || !cl.Git {
				continue
			}
			local := map[string]bool{} // the local branches that hold something
			for _, b := range cl.Local {
				local[b.Name] = local[b.Name] || len(b.Paths) > 0 || b.NoBase
			}
			branch := cl.Branch
			if branch == "" {
				branch = detachedHead // its changes still meet paths
			}
			paths := unionPaths(cl.Changed, cl.Unpushed)
			unpushed := cl.Ahead > 0 || local[cl.Branch]
			add(laptopWork{Repository: cl.Repository, Branch: branch, Paths: paths, Holds: len(cl.Changed) > 0 || unpushed,
				Pushed: !unpushed && cl.Upstream && len(cl.Changed) == 0 && cl.Branch != ""})
			for _, b := range cl.Local {
				// One whose commits the target has, merged, holds
				// nothing; a squash-merged one is go on's to clear.
				add(laptopWork{Repository: cl.Repository, Branch: b.Name, Paths: b.Paths, Holds: len(b.Paths) > 0 || b.NoBase})
			}

		}
		for _, s := range rep.Claude {
			if s.Outside || s.Repository == "" || s.Branch == "" {
				continue
			}
			add(laptopWork{Repository: s.Repository, Branch: s.Branch, Paths: s.Changed, Holds: len(s.Changed) > 0})
		}
	}
	return out
}

// unionPaths is a and b's paths, once each, sorted, none withheld.
func unionPaths(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, ps := range [][]string{a, b} {
		for _, p := range ps {
			if p != "" && p != withheldPath && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// hereWork is a piece of agentd's work here that laptop work may meet.
type hereWork struct {
	ID, Kind, Repository, Branch, Item string
	Issue                              int
	Paths                              []string
	Verb                               string // what it does to the paths: "changes", "plans"
	Asked                              time.Time
}

// hereWorks is agentd's work in its repositories: running sessions not on
// an observing host, with the paths their worktrees change; open requests
// no session here works, with their files; and open or designed items no
// running session works, with the paths their briefs name.
func hereWorks(cfg *Config, st *LogState, worktree func(name string, p RuntimePayload) []string) []hereWork {
	var out []hereWork
	sessionBranch := map[string]bool{}
	sessionItem := map[string]bool{}
	for _, name := range sortedKeys(st.Runtime) {
		p := st.Runtime[name]
		if p.Host != "" && observing(cfg, p.Host) || p.Kind != KindWork && p.Kind != "" {
			continue
		}
		w := hereWork{ID: name, Kind: WorkKindSession, Repository: p.Repository, Branch: p.Branch, Issue: p.Issue, Item: p.Item, Verb: "changes"}
		if p.Item != "" {
			w.ID = p.Item
			sessionItem[p.Item] = true
		}
		if worktree != nil {
			w.Paths = worktree(name, p)
		}
		if p.Branch != "" {
			sessionBranch[p.Repository+"\x00"+p.Branch] = true
		}
		out = append(out, w)
	}
	c := st.Coord
	for _, repo := range sortedKeys(st.Index) {
		for _, ref := range sortedKeys(st.Index[repo]) {
			i := st.Index[repo][ref]
			if !strings.HasPrefix(ref, "!") || i.Gone || sessionBranch[repo+"\x00"+i.Branch] {
				continue
			}
			w := hereWork{ID: ref, Kind: WorkKindRequest, Repository: repo, Branch: i.Branch, Verb: "changes"}
			if c != nil {
				w.Paths = c.Files[filesKey(repo, ref)].Files
			}
			out = append(out, w)
		}
	}
	for _, id := range sortedKeys(st.Work) {
		it := st.Work[id]
		if sessionItem[id] || it.State == WorkDone || it.State == WorkDeclined {
			continue
		}
		w := hereWork{ID: id, Kind: WorkKindItem, Repository: it.Repository, Issue: it.Issue, Item: id, Verb: "plans"}
		if it.Design != nil && it.Design.Brief != nil {
			w.Paths = briefPaths(*it.Design.Brief)
		}
		out = append(out, w)
	}
	return out
}

// pathToken is a word that may be a repository-relative path: a file
// name with an extension, or one with a directory.
var pathToken = regexp.MustCompile(`[A-Za-z0-9_.\-]+(?:/[A-Za-z0-9_.\-]+)*`)

// briefPaths are the words of a brief that look like paths: with a
// slash, or a dot before a short extension; a line number after a colon
// is left off.
func briefPaths(b DesignBrief) []string {
	text := strings.Join(append(append(append([]string{}, b.Exists...), b.Missing...), b.Design...), "\n") + "\n" + b.Where
	seen := map[string]bool{}
	var out []string
	for _, t := range pathToken.FindAllString(text, -1) {
		t = strings.Trim(t, ".-")
		if t == "" || seen[t] || strings.HasPrefix(t, "http") {
			continue
		}
		dot := strings.LastIndex(t, ".")
		ext := ""
		if dot > 0 {
			ext = t[dot+1:]
		}
		if !strings.Contains(t, "/") && (ext == "" || len(ext) > 5 || !hasLetter(ext)) {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func hasLetter(s string) bool {
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}

// branchNamesIssue says whether a branch's name names issue n: issue-N,
// N-…, …-N, or N between dashes, slashes or underscores.
func branchNamesIssue(branch string, n int) bool {
	if n <= 0 {
		return false
	}
	want := strconv.Itoa(n)
	for _, f := range strings.FieldsFunc(branch, func(r rune) bool { return r == '-' || r == '/' || r == '_' }) {
		if f == want {
			return true
		}
	}
	return false
}

// meet is the overlap laptop work l makes with work h, or nil: the same
// repository and the same branch, a shared path, or a laptop branch that
// names h's issue.
func meet(l laptopWork, h hereWork) *Overlap {
	if l.Repository != h.Repository {
		return nil
	}
	o := &Overlap{Key: overlapKey(l.Host, l.Repository, l.Branch, h.ID), Host: l.Host, Repository: l.Repository, Laptop: l.Branch,
		Work: h.ID, WorkKind: h.Kind, Item: h.Item, Branch: h.Branch, Issue: h.Issue, Holds: l.Holds, Asked: h.Asked}
	if shared := sharedPaths(l.Paths, h.Paths); len(shared) > 0 {
		o.Why, o.Paths = "path", shared
	} else if h.Branch != "" && l.Branch == h.Branch {
		o.Why = "branch"
	} else if branchNamesIssue(l.Branch, h.Issue) {
		o.Why = "issue"
	} else {
		return nil
	}
	o.Sentence = overlapSentence(*o, h)
	return o
}

func sharedPaths(a, b []string) []string {
	in := map[string]bool{}
	for _, p := range b {
		in[p] = true
	}
	var out []string
	for _, p := range a {
		if in[p] && p != withheldPath {
			out = append(out, p)
		}
	}
	return out
}

// overlapSentence is the card's sentence: "Your laptop's requests-poll
// edits internal/pull/requests_indexing.go; item W258e3837 here plans the
// same file."
func overlapSentence(o Overlap, h hereWork) string {
	who := "Your " + o.Host + "'s " + o.Laptop
	what := map[string]string{WorkKindSession: "session ", WorkKindRequest: "request ", WorkKindItem: "item ", WorkKindStart: "the start of "}[h.Kind] + h.ID
	switch o.Why {
	case "path":
		if len(o.Paths) == 1 {
			return fmt.Sprintf("%s edits %s; %s here %s the same file.", who, o.Paths[0], what, h.Verb)
		}
		return fmt.Sprintf("%s edits %s and %d more; %s here %s the same files.", who, o.Paths[0], len(o.Paths)-1, what, h.Verb)
	case "branch":
		return fmt.Sprintf("%s is the branch %s here works on too.", who, what)
	}
	return fmt.Sprintf("%s names issue #%d, which %s here works.", who, o.Issue, what)
}

// Overlaps are every overlap of the observing hosts' work with agentd's
// here, by key; held starts' overlaps still standing are kept, until
// startOverlapKept after the start was last tried.
func Overlaps(cfg *Config, st *LogState, worktree func(string, RuntimePayload) []string, now time.Time) map[string]*Overlap {
	out := map[string]*Overlap{}
	lws := laptopWorks(cfg, st.Coord)
	if len(lws) == 0 {
		return out
	}
	// A worktree is read only where the laptop has paths to meet.
	withPaths := map[string]bool{}
	for _, l := range lws {
		withPaths[l.Repository] = withPaths[l.Repository] || len(l.Paths) > 0
	}
	read := func(name string, p RuntimePayload) []string {
		if worktree == nil || !withPaths[p.Repository] {
			return nil
		}
		return worktree(name, p)
	}
	hws := hereWorks(cfg, st, read)
	if st.Coord != nil {
		for _, k := range sortedKeys(st.Coord.Overlaps) {
			o := st.Coord.Overlaps[k]
			// A held start's card stands while the laptop still holds it,
			// a fix round's too, whose session runs: it is closed only
			// when what it waits on ends, or the start is not tried again.
			if o.WorkKind == WorkKindStart && now.Sub(o.Asked) < startOverlapKept {
				hws = append(hws, hereWork{ID: o.Work, Kind: WorkKindStart, Repository: o.Repository, Branch: o.Branch, Issue: o.Issue, Item: o.Item, Paths: o.Paths, Verb: "would change", Asked: o.Asked})
			}
		}
	}
	for _, l := range lws {
		for _, h := range hws {
			if o := meet(l, h); o != nil {
				if h.Kind == WorkKindStart && !l.Holds {
					continue // a start waits only on what the laptop holds
				}
				if had := out[o.Key]; had != nil && had.WorkKind != WorkKindStart {
					continue // the work itself, once it runs, is the card
				}
				out[o.Key] = o
			}
		}
	}
	return out
}

// sameOverlap says whether two overlaps say the same, the time a start
// was asked aside.
func sameOverlap(a, b *Overlap) bool {
	x, y := *a, *b
	x.Asked, y.Asked = time.Time{}, time.Time{}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return string(ja) == string(jb)
}

// coordinate logs the overlaps that opened, changed or closed since the
// state's, so each is one card that closes itself when it ends.
func (s *Supervisor) coordinate(now time.Time) {
	if s.shadow || s.worker {
		return
	}
	c := s.st.coord()
	want := Overlaps(s.cfg, s.st, s.worktreeFor, now)
	for _, k := range sortedKeys(want) {
		o := want[k]
		if had := c.Overlaps[k]; had != nil && sameOverlap(had, o) {
			continue
		}
		if s.emit("", Event{Kind: KindLaptop, Laptop: &LaptopPayload{Op: LaptopOpen, Repository: o.Repository, Key: k, Overlap: o}}); s.halt != nil {
			return
		}
	}
	for _, k := range sortedKeys(c.Overlaps) {
		if want[k] == nil {
			o := c.Overlaps[k]
			if s.emit("", Event{Kind: KindLaptop, Laptop: &LaptopPayload{Op: LaptopClose, Repository: o.Repository, Key: k}}); s.halt != nil {
				return
			}
		}
	}
}

// worktreeFor is the paths session name's worktree changes: its
// uncommitted changes and its commits against origin/target. A session
// on another host has none read here.
func (s *Supervisor) worktreeFor(name string, p RuntimePayload) []string {
	if p.Host != "" {
		return nil
	}
	sess, ok := withRuntime(s.cfg, s.st.Runtime).Sessions[name]
	if !ok || sess.Cwd == "" {
		return nil
	}
	target := "main"
	if sp := s.cfg.Repositories[p.Repository].Sessions; sp != nil && sp.Target != "" {
		target = sp.Target
	}
	if s.worktreePaths != nil {
		return s.worktreePaths(sess.Cwd, target)
	}
	return worktreeChanged(s.cfg.Timeouts.Forge.Duration, sess.Cwd, target)
}

// worktreeChanged are the paths a worktree changes, read-only.
func worktreeChanged(timeout time.Duration, dir, target string) []string {
	run := func(args ...string) string {
		out, err := runTimed(timeout, dir, append([]string{"GIT_OPTIONAL_LOCKS=0", "LC_ALL=C"}, gitEnv...), false, "git", append(append([]string{}, gitSafe...), args...)...)
		if err != nil {
			return ""
		}
		return string(out)
	}
	changed, _ := statusPaths(run("status", "--porcelain=v1", "-z", "--untracked-files=normal"))
	var committed []string
	if plainBranch(target) {
		committed = nulPaths(run("diff", "--name-only", "-z", "refs/remotes/origin/"+target+"...HEAD", "--"))
	}
	return unionPaths(changed, committed)
}

// laptopWait is why work about to start must wait on the laptop, "" when
// it need not: an open overlap of it the owner did not answer go on, or
// laptop work that holds changes on its branch, its issue or the paths it
// would change. A start held with no overlap open gets one, so the owner
// has a card to answer.
func (s *Supervisor) laptopWait(repo, name string, p RuntimePayload, paths []string) string {
	now := s.clock.Now()
	s.coordinate(now)
	c := s.st.coord()
	work := name
	if p.Item != "" {
		work = p.Item
	}
	for _, k := range sortedKeys(c.Overlaps) {
		o := c.Overlaps[k]
		if o.Repository != repo || c.Choices[k] == CoordGoOn {
			continue
		}
		involved := o.Work == work || o.Work == name || p.Request != "" && o.Work == p.Request ||
			p.Issue > 0 && o.Issue == p.Issue || p.Branch != "" && o.Branch == p.Branch
		if !involved || !o.Holds {
			continue // a card of what the laptop does not hold only tells
		}
		if o.WorkKind == WorkKindStart && now.Sub(o.Asked) > startOverlapKept/6 && !s.shadow {
			again := *o
			again.Asked = now
			s.emit("", Event{Kind: KindLaptop, Laptop: &LaptopPayload{Op: LaptopOpen, Repository: repo, Key: k, Overlap: &again}})
		}
		return "waits on your laptop: " + o.Sentence
	}
	if p.Item != "" {
		if it := s.st.Work[p.Item]; it != nil && it.Design != nil && it.Design.Brief != nil {
			paths = unionPaths(paths, briefPaths(*it.Design.Brief))
		}
	}
	h := hereWork{ID: work, Kind: WorkKindStart, Repository: repo, Branch: p.Branch, Issue: p.Issue, Item: p.Item, Paths: paths, Verb: "would change", Asked: now}
	for _, l := range laptopWorks(s.cfg, c) {
		if !l.Holds {
			continue
		}
		o := meet(l, h)
		if o == nil || c.Choices[o.Key] == CoordGoOn {
			continue
		}
		if !s.shadow {
			s.emit("", Event{Kind: KindLaptop, Laptop: &LaptopPayload{Op: LaptopOpen, Repository: repo, Key: o.Key, Overlap: o}})
		}
		return "waits on your laptop: " + o.Sentence
	}
	return ""
}

// linkedBranch is the laptop branch the owner said is item's work, once
// the forge has it with nothing left unpushed: the item's session
// continues it. "" when there is none, or it is not pushed yet.
func linkedBranch(cfg *Config, st *LogState, item string) string {
	if st.Coord == nil || item == "" {
		return ""
	}
	o, ok := st.Coord.Links[item]
	if !ok || !plainBranch(o.Laptop) {
		return ""
	}
	for _, l := range laptopWorks(cfg, st.Coord) {
		if l.Host == o.Host && l.Repository == o.Repository && l.Branch == o.Laptop {
			if l.Pushed {
				return o.Laptop
			}
			return ""
		}
	}
	// Checked out no longer: pushed when its clone says the forge has it
	// with nothing unpushed.
	if seen := st.Coord.Seen[o.Host]; seen != nil {
		for _, c := range seen.Report.Clones {
			if c.Repository != o.Repository {
				continue
			}
			for _, b := range c.Pushed {
				if b == o.Laptop {
					return o.Laptop
				}
			}
		}
	}
	return "" // not reported as pushed: deleted, withheld, unpushed, or its clone unread
}

// readRequestFiles reads, in a worker, the changed files of each open
// request in a repository where an observing host reports work, once per
// head, so a laptop path is compared with them.
func (s *Supervisor) readRequestFiles(now time.Time) {
	if s.shadow || s.api == nil || s.st.Coord == nil {
		return
	}
	repos := map[string]bool{}
	for _, l := range laptopWorks(s.cfg, s.st.Coord) {
		if len(l.Paths) > 0 {
			repos[l.Repository] = true
		}
	}
	for _, repo := range sortedKeys(repos) {
		r, ok := s.cfg.Repositories[repo]
		if !ok || !indexed(r) {
			continue
		}
		for _, ref := range sortedKeys(s.st.Index[repo]) {
			i := s.st.Index[repo][ref]
			if !strings.HasPrefix(ref, "!") || i.Gone {
				continue
			}
			version := s.st.CI[ciKey(repo, ref)].Head
			if version == "" {
				version = i.Changed.UTC().Format(time.RFC3339)
			}
			if had, ok := s.st.Coord.Files[filesKey(repo, ref)]; ok && had.Version == version {
				continue
			}
			repo, ref, n := repo, ref, i.Number()
			s.spawnShared("request-files:"+repo+ref, func(w *Supervisor) {
				files, err := requestFiles(w.api.At(r.Host), r, n)
				p := LaptopPayload{Op: LaptopFiles, Repository: repo, Ref: ref, Version: version}
				if err != nil {
					p.Error = w.withheld(oneLine(err.Error(), 300))
				}
				for _, f := range files {
					if w.confidentialMatch(f) == 0 {
						p.Files = append(p.Files, f)
					}
				}
				w.emit("", Event{Kind: KindLaptop, Laptop: &p})
			})
		}
	}
}

// requestFiles are the paths request n changes, old and new, from the
// forge: the first 100 on GitHub, and GitLab's changes.
func requestFiles(api ForgeAPI, r Repository, n int) ([]string, error) {
	var out []string
	if r.Forge == "gitlab" {
		resp, err := api.GitLab(fmt.Sprintf("/projects/%s/merge_requests/%d/changes", url.PathEscape(r.Project), n))
		if err != nil {
			return nil, err
		}
		if resp.Status != 200 {
			return nil, statusError("GitLab merge request changes", resp)
		}
		var v struct {
			Changes []struct {
				Old string `json:"old_path"`
				New string `json:"new_path"`
			} `json:"changes"`
		}
		if err := json.Unmarshal(resp.Body, &v); err != nil {
			return nil, err
		}
		for _, c := range v.Changes {
			out = append(out, c.Old, c.New)
		}
		return unionPaths(nil, out), nil
	}
	resp, err := api.GitHub(fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100", r.Project, n), nil)
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		return nil, statusError("GitHub pull request files", resp)
	}
	var v []struct {
		Name     string `json:"filename"`
		Previous string `json:"previous_filename"`
	}
	if err := json.Unmarshal(resp.Body, &v); err != nil {
		return nil, err
	}
	for _, f := range v {
		out = append(out, f.Name, f.Previous)
	}
	return unionPaths(nil, out), nil
}

// LevelOverlap is an overlap's card on a repository's page.
type LevelOverlap struct {
	Key      string `json:"key"`
	Sentence string `json:"sentence"`
	Choice   string `json:"choice,omitempty"`
	Holds    bool   `json:"holds"`
	Item     string `json:"item,omitempty"`
}

// LevelLaptop is one observing host's clone of a repository on its page:
// branch, changed-file count, ahead, and when the host last reported, or
// that it is silent.
type LevelLaptop struct {
	Host    string    `json:"host"`
	Root    string    `json:"root,omitempty"`
	Branch  string    `json:"branch"`
	Changed int       `json:"changed"`
	Ahead   int       `json:"ahead"`
	Local   int       `json:"local"`
	LastAt  time.Time `json:"last_at"`
	Silent  bool      `json:"silent"`
	// None marks a host that reported no clone of the repository.
	None bool `json:"none,omitempty"`
}

// repoOverlaps are repo's overlaps' cards, by key.
func repoOverlaps(st *LogState, repo string) []LevelOverlap {
	if st.Coord == nil {
		return nil
	}
	var out []LevelOverlap
	for _, k := range sortedKeys(st.Coord.Overlaps) {
		o := st.Coord.Overlaps[k]
		if o.Repository != repo {
			continue
		}
		out = append(out, LevelOverlap{Key: k, Sentence: o.Sentence, Choice: st.Coord.Choices[k], Holds: o.Holds, Item: o.Item})
	}
	return out
}

// repoLaptops are the observing hosts' clones of repo, each with when the
// host last reported: one silent for hub.silent_after reads "laptop: last
// seen <time>", never an error.
func repoLaptops(cfg *Config, st *LogState, repo string, now time.Time) []LevelLaptop {
	var out []LevelLaptop
	for _, h := range sortedKeys(cfg.Hosts) {
		if !observing(cfg, h) {
			continue
		}
		var seen *LaptopSeen
		if st.Coord != nil {
			seen = st.Coord.Seen[h]
		}
		if seen == nil {
			out = append(out, LevelLaptop{Host: h, Silent: true, None: true})
			continue
		}
		silent := now.Sub(seen.LastAt) > cfg.Hub.SilentAfter.Duration
		any := false
		for _, c := range seen.Report.Clones {
			if c.Repository != repo || !c.Exists {
				continue
			}
			any = true
			l := LevelLaptop{Host: h, Root: c.Root, Branch: c.Branch, Changed: len(c.Changed), Ahead: c.Ahead, Local: len(c.Local), LastAt: seen.LastAt, Silent: silent}
			if l.Branch == "" {
				l.Branch = "detached"
			}
			out = append(out, l)
		}
		if !any {
			out = append(out, LevelLaptop{Host: h, LastAt: seen.LastAt, Silent: silent, None: true})
		}
	}
	return out
}

// overlapCount is how many overlaps repo has open.
func overlapCount(st *LogState, repo string) int { return len(repoOverlaps(st, repo)) }

// RecordCoordChoice records the owner's choice on an open overlap.
func RecordCoordChoice(log *Log, st *LogState, now time.Time, key, choice string) error {
	if st.Coord == nil || st.Coord.Overlaps[key] == nil {
		return fmt.Errorf("no open overlap %q", key)
	}
	o := st.Coord.Overlaps[key]
	if choice == CoordIsItem && o.Item == "" {
		return fmt.Errorf("the overlap is with %s, no item", o.Work)
	}
	e := Event{V: EventVersion, Time: now.UTC(), Kind: KindLaptop, Mode: ModeOwner, Laptop: &LaptopPayload{Op: LaptopChoice, Repository: o.Repository, Key: key, Choice: choice}}
	if err := e.check(); err != nil {
		return err
	}
	return log.Append(e)
}
