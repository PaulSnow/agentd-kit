package main

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// coordHarness is the build host's supervisor (sessionsHarness) and a hub
// two nodes report to: thelio, the build host, and laptop, which only
// observes. The hub logs to its own file, which relay copies into the
// supervisor's log, as the one log the hub and the supervisor share.
type coordHarness struct {
	*harness
	hub     *hubHarness
	relayed int
	origin  string // the laptop clone's origin, a bare repository
	clone   string // the laptop's clone of widgets
}

const coordLayout = "gitlab.example.com/example-org/widgets"

func newCoordHarness(t *testing.T) *coordHarness {
	t.Helper()
	h := sessionsHarness(t)
	h.cfg.Hosts = map[string]HostConfig{"thelio": {}, "laptop": {Observe: true}}
	h.cfg.ConfidentialPatterns = append(h.cfg.ConfidentialPatterns, `secret-plan`)
	h.restart(false)
	h.sup.worktreePaths = func(dir, target string) []string {
		if strings.Contains(dir, "issue-12") {
			return []string{"spin.go"}
		}
		return nil
	}

	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.EventLog = filepath.Join(dir, "events.jsonl")
	cfg.Hub = HubConfig{Address: "127.0.0.1:0", SilentAfter: Duration{10 * time.Minute}, RetryAfter: Duration{30 * time.Minute}}
	cfg.ConfidentialPatterns = []string{`secret-plan`}
	cfg.Hosts = map[string]HostConfig{}
	for _, name := range []string{"thelio", "laptop"} {
		tf := filepath.Join(dir, name+".token")
		if err := os.WriteFile(tf, []byte("token-of-"+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.Hosts[name] = HostConfig{TokenFile: tf, Observe: name == "laptop"}
	}
	cfg.Repositories = map[string]Repository{
		"widgets": {Forge: "gitlab", Host: "gitlab.example.com", Transport: "https", Project: "example-org/widgets", CloneOn: []string{"thelio", "laptop"}},
	}
	log, err := OpenLog(cfg.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	hub := &hubHarness{t: t, cfg: &cfg, log: log, now: h.clock.Now(), nodes: map[string]*Node{}, gits: map[string]*fakeNodeGit{}, tmuxes: map[string]*fakeTmux{}}
	hub.startHub()
	for _, name := range []string{"thelio", "laptop"} {
		hub.gits[name], hub.tmuxes[name] = &fakeNodeGit{t: t}, newFakeTmux()
		nc := NodeConfig{Name: name, Hub: hub.srv.URL, ReposRoot: filepath.Join(dir, name, "repos"), Observe: name == "laptop"}
		hub.nodes[name] = NewNode(nc, "token-of-"+name, hub.gits[name], hub.tmuxes[name], hub.srv.Client(), hub.clock)
	}
	c := &coordHarness{harness: h, hub: hub, origin: filepath.Join(dir, "origin.git")}
	gitRun(t, dir, "init", "-q", "--bare", c.origin)
	c.clone = hub.makeClone("laptop", coordLayout)
	gitRun(t, c.clone, "branch", "-M", "main")
	gitRun(t, c.clone, "remote", "add", "origin", c.origin)
	gitRun(t, c.clone, "push", "-q", "-u", "origin", "main")
	return c
}

// report has both nodes report, then the supervisor read the log and
// compare, as a pass does.
func (c *coordHarness) report() {
	c.t.Helper()
	c.hub.now = c.clock.Now()
	c.hub.tick("thelio")
	c.hub.tick("laptop")
	events, _, err := ReadLog(c.hub.cfg.EventLog)
	if err != nil {
		c.t.Fatal(err)
	}
	w := NewLogWriter(c.buf)
	for _, e := range events[c.relayed:] {
		if err := w.Append(e); err != nil {
			c.t.Fatal(err)
		}
	}
	c.relayed = len(events)
	c.sup.ownerRecords()
	c.sup.coordinate(c.clock.Now())
}

func (c *coordHarness) overlaps() map[string]*Overlap {
	st := c.logState()
	if st.Coord == nil {
		return nil
	}
	return st.Coord.Overlaps
}

// A dirty laptop path that a running session's worktree also changes is
// exactly one card, and a start on that work waits on the laptop; once
// the laptop commits and pushes to another branch, sharing no path, the
// next report closes the card and the start goes ahead.
func TestLaptopOverlapHoldsAndReleasesStart(t *testing.T) {
	c := newCoordHarness(t)
	running := c.startSession("issue-12-spin", KindWork, 12)
	gitRun(t, c.clone, "checkout", "-q", "-b", "laptop-work")
	writeFile(t, filepath.Join(c.clone, "spin.go"), "package spin\n")
	c.report()

	got := c.overlaps()
	if len(got) != 1 {
		t.Fatalf("overlaps %+v, want one", got)
	}
	for _, o := range got {
		if o.Work != running || o.Why != "path" || !o.Holds || o.Sentence != "Your laptop's laptop-work edits spin.go; session "+running+" here changes the same file." {
			t.Fatalf("overlap %+v", o)
		}
	}
	held := runtimeName("widgets", "issue-12-again", KindWork)
	why := c.sup.startRuntime("widgets", held, RuntimePayload{Kind: KindWork, Branch: "issue-12-again", Issue: 12})
	if !strings.HasPrefix(why, "waits on your laptop: Your laptop's laptop-work edits spin.go") {
		t.Fatalf("start not held on the laptop: %q", why)
	}
	if len(c.overlaps()) != 1 {
		t.Fatalf("the held start made another card: %+v", c.overlaps())
	}
	u := harnessUI(t, c.harness)
	page := owner(t, u, "/repo/widgets")
	if n := strings.Count(page, `class="card decision-card overlap-card"`); n != 1 {
		t.Fatalf("%d overlap cards on the repository page", n)
	}
	if !strings.Contains(page, "laptop: laptop-work, 1 changed, 0 ahead") {
		t.Errorf("the page lacks the laptop's clone:\n%s", page)
	}
	if home := owner(t, u, "/"); !strings.Contains(home, "1 laptop overlap") {
		t.Errorf("the home page lacks the overlap count")
	}

	// The laptop commits, pushes to another branch, and leaves no local
	// branch the forge lacks.
	gitRun(t, c.clone, "checkout", "-q", "-b", "other")
	gitRun(t, c.clone, "add", "spin.go")
	gitRun(t, c.clone, "commit", "-q", "-m", "spin")
	gitRun(t, c.clone, "push", "-q", "-u", "origin", "other")
	gitRun(t, c.clone, "branch", "-q", "-D", "laptop-work")
	c.clock.advance(time.Minute)
	c.report()
	if got := c.overlaps(); len(got) != 0 {
		t.Fatalf("the card did not close: %+v", got)
	}
	if why := c.sup.startRuntime("widgets", held, RuntimePayload{Kind: KindWork, Branch: "issue-12-again", Issue: 12}); why != "" {
		t.Fatalf("the held start did not go ahead: %s", why)
	}
}

// An observe host's answer carries no command, though its clone is
// absent, and an observe node refuses any command it is sent.
func TestObserveHostGetsAndDoesNoCommands(t *testing.T) {
	c := newCoordHarness(t)
	if err := os.RemoveAll(c.clone); err != nil {
		t.Fatal(err)
	}
	c.report()
	c.report()
	if g := c.hub.gits["laptop"]; len(g.cloned) != 0 {
		t.Fatalf("the laptop cloned %v", g.cloned)
	}
	if g := c.hub.gits["thelio"]; len(g.cloned) != 1 {
		t.Fatalf("thelio, which does not observe, cloned %v", g.cloned)
	}
	_, reply := c.hub.hub.take("laptop", nil)
	if len(reply.Commands) != 0 {
		t.Fatalf("the observe host was sent %+v", reply.Commands)
	}
	n := c.hub.nodes["laptop"]
	for _, cmd := range []NodeCommand{
		{ID: "c1", Action: NodeClone, Repository: "widgets", Path: coordLayout, URL: "https://example.invalid/w.git"},
		{ID: "c2", Action: NodeSessionStart, Session: "s", Repository: "widgets", Path: coordLayout, Branch: "b", Target: "main"},
		{ID: "c3", Action: NodeSessionStop, Session: "s"},
		{ID: "c4", Action: NodeDeployStep},
	} {
		if r := n.do(context.Background(), cmd); r.Outcome != NodeRefused {
			t.Errorf("%s: %+v", cmd.Action, r)
		}
	}
	if _, err := os.Lstat(c.clone); err == nil {
		t.Fatal("the refused clone made a checkout")
	}
	var observe Config = *c.hub.cfg
	observe.Repositories = map[string]Repository{"widgets": {Forge: "gitlab", Host: "gitlab.example.com", Project: "example-org/widgets", CloneOn: []string{"laptop"},
		Sessions: &SessionsPolicy{Root: "/r", Clone: "/c", Target: "main", Host: "laptop"}}}
	var errs []error
	observe.checkNodeConfig(func(_, p string) string { return p }, &errs)
	if !strings.Contains(errorsText(errs), "only observes") {
		t.Fatalf("placing sessions on an observe host was not refused: %v", errs)
	}
}

func errorsText(errs []error) string {
	var b strings.Builder
	for _, e := range errs {
		b.WriteString(e.Error() + "\n")
	}
	return b.String()
}

// A laptop path, branch or directory that matches a confidential pattern
// is withheld before the hub logs the report.
func TestLaptopConfidentialPathWithheld(t *testing.T) {
	c := newCoordHarness(t)
	gitRun(t, c.clone, "checkout", "-q", "-b", "secret-plan-branch")
	writeFile(t, filepath.Join(c.clone, "secret-plan.md"), "x\n")
	writeFile(t, filepath.Join(c.clone, "open.go"), "x\n")
	c.report()
	b, err := os.ReadFile(c.hub.cfg.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret-plan") {
		t.Fatalf("the hub logged a confidential name:\n%s", b)
	}
	var clone *CloneState
	for _, p := range c.hub.nodeEvents() {
		if p.Host == "laptop" && p.Report != nil {
			for i := range p.Report.Clones {
				clone = &p.Report.Clones[i]
			}
		}
	}
	if clone == nil || clone.Branch != withheldPath || strings.Join(clone.Changed, ",") != "open.go,"+withheldPath && strings.Join(clone.Changed, ",") != withheldPath+",open.go" {
		t.Fatalf("clone %+v", clone)
	}
}

// A laptop silent for longer than hub.silent_after shows on the
// repository page as last seen, never as an error.
func TestSilentLaptopShowsLastSeen(t *testing.T) {
	c := newCoordHarness(t)
	c.report()
	c.clock.advance(time.Hour)
	u := harnessUI(t, c.harness)
	page := owner(t, u, "/repo/widgets")
	if !strings.Contains(page, "laptop: last seen 2026-") {
		t.Fatalf("the page lacks the laptop's last seen:\n%s", page)
	}
	if strings.Contains(strings.ToLower(page[strings.Index(page, `id="laptop"`):]), "error") {
		t.Fatalf("a silent laptop shows as an error")
	}
}

// "The laptop work is the item" links the laptop branch to the item: the
// item's start waits while the branch is unpushed, and continues it once
// the forge has it.
func TestLaptopWorkIsTheItem(t *testing.T) {
	c := newCoordHarness(t)
	it := &WorkItem{ID: "W1234abcd", Repository: "widgets", State: WorkOpen, Designer: true,
		Design: &WorkDesign{Round: 1, Brief: &DesignBrief{Design: []string{"Change spin.go and docs/spin.md (spin.go:12)."}}}}
	c.sup.st.Work = map[string]*WorkItem{it.ID: it}
	gitRun(t, c.clone, "checkout", "-q", "-b", "spin-faster")
	writeFile(t, filepath.Join(c.clone, "spin.go"), "package spin\n")
	gitRun(t, c.clone, "add", "spin.go")
	gitRun(t, c.clone, "commit", "-q", "-m", "spin")
	c.report()
	o := c.sup.st.Coord.Overlaps[overlapKey("laptop", "widgets", "spin-faster", it.ID)]
	if o == nil || o.Sentence != "Your laptop's spin-faster edits spin.go; item W1234abcd here plans the same file." {
		t.Fatalf("overlaps %+v", c.sup.st.Coord.Overlaps)
	}
	u := harnessUI(t, c.harness)
	if w := ownerPost(u, "/coord", url.Values{"src": {"main"}, "repository": {"widgets"}, "key": {o.Key}, "choice": {CoordIsItem}, "back": {"/repo/widgets"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("choice: %d %s", w.Code, w.Body.String())
	}
	c.sup.ownerRecords()
	if c.sup.st.Coord.Choices[o.Key] != CoordIsItem || c.sup.st.Coord.Links[it.ID].Laptop != "spin-faster" {
		t.Fatalf("not linked: %+v", c.sup.st.Coord)
	}
	if b := linkedBranch(c.cfg, c.sup.st, it.ID); b != "" {
		t.Fatalf("an unpushed branch was continued: %q", b)
	}
	if why := c.sup.laptopWait("widgets", "x", RuntimePayload{Kind: KindWork, Item: it.ID, Branch: "owner/x"}, nil); !strings.Contains(why, "spin-faster") {
		t.Fatalf("the item's start did not wait on the unpushed branch: %q", why)
	}
	gitRun(t, c.clone, "push", "-q", "-u", "origin", "spin-faster")
	c.clock.advance(time.Minute)
	c.report()
	if b := linkedBranch(c.cfg, c.sup.st, it.ID); b != "spin-faster" {
		t.Fatalf("the pushed branch is not the item's: %q", b)
	}
	if why := c.sup.laptopWait("widgets", "x", RuntimePayload{Kind: KindWork, Item: it.ID, Branch: "spin-faster"}, nil); why != "" {
		t.Fatalf("the linked, pushed branch still holds the item: %q", why)
	}
}

// The staking case: the laptop's requests-poll edits a file item
// W258e3837's brief plans, one card on the staking page.
func TestStakingOverlapCard(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Hosts = map[string]HostConfig{"thelio": {}, "laptop": {Observe: true}}
	cfg.Repositories = map[string]Repository{"staking": {Forge: "gitlab", Host: "gitlab.com", Project: "accumulatenetwork/core/staking", CloneOn: []string{"laptop"}}}
	now := time.Date(2026, 11, 1, 22, 0, 0, 0, time.UTC)
	st := NewLogState(&cfg, ModeLive)
	st.Work = map[string]*WorkItem{"W258e3837": {ID: "W258e3837", Repository: "staking", State: WorkOpen, Designer: true,
		Design: &WorkDesign{Brief: &DesignBrief{Exists: []string{"internal/pull/requests_indexing.go indexes the requests (line 40)."}, Where: "staking"}}}}
	st.apply(Event{V: EventVersion, Time: now, Kind: KindNode, Mode: ModeShared, Node: &NodePayload{Host: "laptop", Seq: 1, At: now, Report: &NodeReport{Clones: []CloneState{{
		Repository: "staking", Path: "gitlab.com/accumulatenetwork/core/staking", Exists: true, Git: true, Branch: "requests-poll",
		Changed: []string{"internal/pull/requests_indexing.go"}, Local: []LocalBranch{{Name: "requests-poll"}}}}}}})
	got := Overlaps(&cfg, st, nil, now)
	if len(got) != 1 {
		t.Fatalf("overlaps %+v", got)
	}
	for k, o := range got {
		st.apply(Event{V: EventVersion, Time: now, Kind: KindLaptop, Mode: ModeLive, Laptop: &LaptopPayload{Op: LaptopOpen, Repository: "staking", Key: k, Overlap: o}})
	}
	v := BuildRepoLevel(&cfg, st, "staking", nil, now)
	if len(v.Overlaps) != 1 || v.Overlaps[0].Sentence != "Your laptop's requests-poll edits internal/pull/requests_indexing.go; item W258e3837 here plans the same file." {
		t.Fatalf("staking page overlaps %+v", v.Overlaps)
	}
	if row := BuildHomeRow(&cfg, st, "staking", nil, now); row.Overlaps != 1 {
		t.Fatalf("home row %+v", row)
	}
}

// The node reports a clone's changed paths, a rename's both, untracked
// but not ignored, and its local branches the forge lacks with the paths
// each changes against the target; never contents.
func TestNodeScanReportsPaths(t *testing.T) {
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin.git")
	gitRun(t, dir, "init", "-q", "--bare", origin)
	clone := filepath.Join(dir, "clone")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, clone, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(clone, "a.go"), "a\n")
	writeFile(t, filepath.Join(clone, ".gitignore"), "*.log\n")
	gitRun(t, clone, "add", ".")
	gitRun(t, clone, "commit", "-q", "-m", "base")
	gitRun(t, clone, "remote", "add", "origin", origin)
	gitRun(t, clone, "push", "-q", "-u", "origin", "main")
	gitRun(t, clone, "checkout", "-q", "-b", "local-only")
	writeFile(t, filepath.Join(clone, "b.go"), "b\n")
	gitRun(t, clone, "add", "b.go")
	gitRun(t, clone, "commit", "-q", "-m", "b")
	gitRun(t, clone, "mv", "a.go", "c.go")
	writeFile(t, filepath.Join(clone, "new.go"), "n\n")
	writeFile(t, filepath.Join(clone, "x.log"), "ignored\n")
	c, err := ExecNodeGit{Timeout: 10 * time.Second}.Scan(clone, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Changed, ","); got != "c.go,a.go,new.go" {
		t.Errorf("changed %q", got)
	}
	if len(c.Local) != 1 || c.Local[0].Name != "local-only" || strings.Join(c.Local[0].Paths, ",") != "b.go" {
		t.Errorf("local %+v", c.Local)
	}
	if c.Branch != "local-only" || c.Dirty != 2 {
		t.Errorf("clone %+v", c)
	}
}

// A Claude Code session is mapped to the watched clone it runs in, with
// its branch; one outside every clone is reported without its directory.
func TestNodeReportsClaudeSessions(t *testing.T) {
	c := newCoordHarness(t)
	n := c.hub.nodes["laptop"]
	n.claude = func() ([]string, error) { return []string{filepath.Join(c.clone, "sub"), "/home/someone/private"}, nil }
	if err := os.MkdirAll(filepath.Join(c.clone, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.report()
	var rep *NodeReport
	for _, p := range c.hub.nodeEvents() {
		if p.Host == "laptop" && p.Report != nil {
			rep = p.Report
		}
	}
	if rep == nil || len(rep.Claude) != 2 || rep.Claude[0].Repository != "widgets" || rep.Claude[0].Branch != "main" ||
		rep.Claude[0].Cwd != coordLayout+"/sub" || !rep.Claude[1].Outside || rep.Claude[1].Cwd != "" {
		t.Fatalf("claude %+v", rep)
	}
}

// A report that finds everything as the last one the hub holds is sent
// as same: the host is seen again, and what it last reported stands.
func TestUnchangedReportIsSame(t *testing.T) {
	c := newCoordHarness(t)
	writeFile(t, filepath.Join(c.clone, "spin.go"), "package spin\n")
	c.report()
	c.clock.advance(time.Minute)
	c.report()
	var reps []*NodeReport
	for _, p := range c.hub.nodeEvents() {
		if p.Host == "laptop" && p.Report != nil {
			reps = append(reps, p.Report)
		}
	}
	if len(reps) != 2 || reps[0].Same || !reps[1].Same || len(reps[1].Clones) != 0 {
		t.Fatalf("reports %+v", reps)
	}
	c.hub.now = c.clock.Now()
	if row := c.hub.status()["laptop"]; len(row.Clones) != 1 || row.Clones[0].Dirty != 1 || row.State != NodeReporting {
		t.Fatalf("status %+v", row)
	}
	seen := c.logState().Coord.Seen["laptop"]
	if !seen.LastAt.Equal(c.clock.Now()) || len(seen.Report.Clones) != 1 || strings.Join(seen.Report.Clones[0].Changed, ",") != "spin.go" {
		t.Fatalf("seen %+v", seen)
	}
	writeFile(t, filepath.Join(c.clone, "more.go"), "package spin\n")
	c.report()
	if seen := c.logState().Coord.Seen["laptop"]; len(seen.Report.Clones[0].Changed) != 2 {
		t.Fatalf("a changed report was not sent whole: %+v", seen)
	}
}
