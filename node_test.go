package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeNodeGit clones by making a git checkout at the path, recording the
// URL it was asked for; it scans with the real git, so a test's checkouts
// are read as a node reads them.
type fakeNodeGit struct {
	t      *testing.T
	cloned []string // url -> dir, as "url dir"
	fail   error
}

func (g *fakeNodeGit) Clone(_ context.Context, url, dir string) error {
	g.cloned = append(g.cloned, url+" "+dir)
	if g.fail != nil {
		return g.fail
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	gitRun(g.t, dir, "init", "-q")
	return nil
}

func (g *fakeNodeGit) Scan(dir string) (CloneState, error) {
	return ExecNodeGit{Timeout: 10 * time.Second}.Scan(dir)
}

// hubHarness is a hub on a test server and the nodes that report to it,
// each with its own token and repos root.
type hubHarness struct {
	t     *testing.T
	cfg   *Config
	log   *Log
	hub   *Hub
	srv   *httptest.Server
	now   time.Time
	nodes map[string]*Node
	gits  map[string]*fakeNodeGit
}

// newHubHarness configures hosts host-a and host-b, and widgets, which
// must be cloned on both, and gadgets, on host-b only.
func newHubHarness(t *testing.T) *hubHarness {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.EventLog = filepath.Join(dir, "events.jsonl")
	cfg.Hub = HubConfig{Address: "127.0.0.1:0", SilentAfter: Duration{10 * time.Minute}, RetryAfter: Duration{30 * time.Minute}}
	cfg.Hosts = map[string]HostConfig{}
	for _, h := range []string{"host-a", "host-b"} {
		tf := filepath.Join(dir, h+".token")
		if err := os.WriteFile(tf, []byte("token-of-"+h+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.Hosts[h] = HostConfig{TokenFile: tf}
	}
	cfg.Repositories = map[string]Repository{
		"widgets": {Forge: "github", Host: "github.com", Transport: "ssh", Project: "example-org/widgets", CloneOn: []string{"host-a", "host-b"}},
		"gadgets": {Forge: "gitlab", Host: "gitlab.com", Transport: "https", Project: "example-org/tools/gadgets", CloneOn: []string{"host-b"}},
	}
	log, err := OpenLog(cfg.EventLog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	h := &hubHarness{t: t, cfg: &cfg, log: log, now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), nodes: map[string]*Node{}, gits: map[string]*fakeNodeGit{}}
	h.startHub()
	for _, name := range []string{"host-a", "host-b"} {
		h.gits[name] = &fakeNodeGit{t: t}
		nc := NodeConfig{Name: name, Hub: h.srv.URL, ReposRoot: filepath.Join(dir, name, "repos"), OpenState: []string{".kickoff"}}
		h.nodes[name] = NewNode(nc, "token-of-"+name, h.gits[name], h.srv.Client(), h.clock)
	}
	return h
}

func (h *hubHarness) clock() time.Time { return h.now }

// startHub starts a hub over the harness's log, as agentd hub does: from
// what the log holds.
func (h *hubHarness) startHub() {
	h.t.Helper()
	if h.srv != nil {
		h.srv.Close()
	}
	events, _, err := h.log.Load()
	if err != nil {
		h.t.Fatal(err)
	}
	hub, err := NewHub(h.cfg, h.log, events, h.clock)
	if err != nil {
		h.t.Fatal(err)
	}
	h.hub = hub
	if h.srv == nil {
		h.srv = httptest.NewServer(hub)
		h.t.Cleanup(h.srv.Close)
	} else {
		h.srv = httptest.NewServer(hub)
		h.t.Cleanup(h.srv.Close)
		for _, n := range h.nodes {
			n.cfg.Hub = h.srv.URL
		}
	}
}

func (h *hubHarness) tick(name string) {
	h.t.Helper()
	if err := h.nodes[name].Tick(context.Background()); err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
}

// nodeEvents are the log's node events, in log order.
func (h *hubHarness) nodeEvents() []NodePayload {
	h.t.Helper()
	events, _, err := ReadLog(h.cfg.EventLog)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []NodePayload
	for _, e := range events {
		if e.Kind == KindNode {
			out = append(out, *e.Node)
		}
	}
	return out
}

func (h *hubHarness) status() map[string]NodeRow {
	h.t.Helper()
	events, _, err := ReadLog(h.cfg.EventLog)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]NodeRow{}
	for _, r := range NodeStatus(h.cfg, events, h.now) {
		out[r.Host] = r
	}
	return out
}

// makeClone puts a checkout of repo's layout path under node's root, with
// one commit, and returns its directory.
func (h *hubHarness) makeClone(node, layout string) string {
	h.t.Helper()
	dir := filepath.Join(h.nodes[node].cfg.ReposRoot, layout)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	gitRun(h.t, dir, "init", "-q")
	writeFile(h.t, filepath.Join(dir, "a"), "a\n")
	gitRun(h.t, dir, "add", "a")
	gitRun(h.t, dir, "commit", "-q", "-m", "base")
	return dir
}

// Each node's events land in the hub's log in the order the node made
// them, numbered from one per host, however the two nodes interleave; a
// hub restarted from the log continues each host's numbering.
func TestNodeEventsLandInOrder(t *testing.T) {
	h := newHubHarness(t)
	h.tick("host-a")
	h.tick("host-b")
	h.now = h.now.Add(time.Minute)
	h.tick("host-b")
	h.tick("host-a")
	h.startHub()
	h.now = h.now.Add(time.Minute)
	h.tick("host-a")
	seqs := map[string][]int64{}
	for _, p := range h.nodeEvents() {
		seqs[p.Host] = append(seqs[p.Host], p.Seq)
	}
	for host, s := range seqs {
		for i, n := range s {
			if n != int64(i+1) {
				t.Fatalf("%s: sequence %v is not 1, 2, ...", host, s)
			}
		}
	}
	if len(seqs["host-a"]) < 3 || len(seqs["host-b"]) < 2 {
		t.Fatalf("events per host: %v", seqs)
	}
}

// A node that lost the hub's reply sends its events again: the hub keeps
// one copy. A batch that skips a number is refused with the sequence the
// hub has, and the node renumbers and is taken.
func TestHubKeepsOneCopyAndRefusesAGap(t *testing.T) {
	h := newHubHarness(t)
	h.tick("host-a")
	n := len(h.nodeEvents())
	again := nodeRequest{Host: "host-a", Events: []NodePayload{{Host: "host-a", Seq: 1, At: h.now, Report: &NodeReport{}}}}
	if code, reply := h.post("token-of-host-a", again); code != http.StatusOK || reply.Acked != int64(n) {
		t.Fatalf("resend: %d acked %d, want 200 acked %d", code, reply.Acked, n)
	}
	if len(h.nodeEvents()) != n {
		t.Fatal("a resent event was appended again")
	}
	gap := nodeRequest{Host: "host-a", Events: []NodePayload{{Host: "host-a", Seq: int64(n + 5), At: h.now, Report: &NodeReport{}}}}
	if code, reply := h.post("token-of-host-a", gap); code != http.StatusConflict || reply.Acked != int64(n) {
		t.Fatalf("gap: %d acked %d, want 409 acked %d", code, reply.Acked, n)
	}
	if len(h.nodeEvents()) != n {
		t.Fatal("an event after a gap was appended")
	}
	// The node itself, its numbering ahead of the hub's, is put right.
	node := h.nodes["host-a"]
	node.seq += 4
	node.queue(NodePayload{Report: &NodeReport{}})
	if _, err := node.send(context.Background()); err != nil {
		t.Fatal(err)
	}
	ev := h.nodeEvents()
	if last := ev[len(ev)-1]; last.Seq != int64(len(ev)) {
		t.Fatalf("after renumbering the last event is %d of %d", last.Seq, len(ev))
	}
}

func (h *hubHarness) post(token string, req nodeRequest) (int, nodeReply) {
	h.t.Helper()
	b, _ := json.Marshal(req)
	r, _ := http.NewRequest(http.MethodPost, h.srv.URL+nodeReportPath, strings.NewReader(string(b)))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.srv.Client().Do(r)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var reply nodeReply
	json.NewDecoder(resp.Body).Decode(&reply)
	return resp.StatusCode, reply
}

// loseReply forwards each request and, while lose is set, loses the
// hub's answer, as a dropped connection does after the hub took the events.
type loseReply struct {
	next http.RoundTripper
	lose bool
}

func (l *loseReply) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := l.next.RoundTrip(r)
	if err == nil && l.lose {
		resp.Body.Close()
		return nil, errors.New("connection reset")
	}
	return resp, err
}

// A node whose answers are lost, though the hub took its events, and then
// heard again, leaves each event in the log once, with no gap; so does one
// whose hub was down.
func TestNodeAfterLostReplies(t *testing.T) {
	h := newHubHarness(t)
	rt := &loseReply{next: h.srv.Client().Transport}
	n := NewNode(h.nodes["host-a"].cfg, "token-of-host-a", h.gits["host-a"], &http.Client{Transport: rt}, h.clock)
	h.nodes["host-a"] = n
	h.tick("host-a")
	rt.lose = true
	for i := 0; i < 3; i++ {
		h.now = h.now.Add(time.Minute)
		if err := n.Tick(context.Background()); err == nil {
			t.Fatal("a lost answer is no error")
		}
	}
	rt.lose = false
	h.srv.Close()
	if err := n.Tick(context.Background()); err == nil {
		t.Fatal("a hub that is down is no error")
	}
	h.startHub()
	h.now = h.now.Add(time.Minute)
	rt.next = h.srv.Client().Transport
	h.tick("host-a")
	h.tick("host-a")
	ev := h.nodeEvents()
	for i, p := range ev {
		if p.Seq != int64(i+1) {
			t.Fatalf("event %d has number %d", i+1, p.Seq)
		}
	}
	clones := 0
	for _, p := range ev {
		if p.Result != nil {
			clones++
		}
	}
	if clones != 1 || len(h.gits["host-a"].cloned) != 1 {
		t.Fatalf("%d clone results, %d clones", clones, len(h.gits["host-a"].cloned))
	}
	if len(n.pending) != 0 {
		t.Fatalf("pending after the hub heard all: %+v", n.pending)
	}
}

// A batch with an event that names another host stops there: what came
// before it is taken, it and what follows are not.
func TestHubRefusesAnotherHostsEvent(t *testing.T) {
	h := newHubHarness(t)
	req := nodeRequest{Host: "host-a", Events: []NodePayload{
		{Host: "host-a", Seq: 1, At: h.now, Report: &NodeReport{}},
		{Host: "host-b", Seq: 2, At: h.now, Report: &NodeReport{}},
		{Host: "host-a", Seq: 3, At: h.now, Report: &NodeReport{}},
	}}
	if code, reply := h.post("token-of-host-a", req); code != http.StatusBadRequest || reply.Acked != 1 {
		t.Fatalf("%d acked %d", code, reply.Acked)
	}
	if ev := h.nodeEvents(); len(ev) != 1 || ev[0].Host != "host-a" {
		t.Fatalf("log: %+v", ev)
	}
}

// A node with no token, a wrong token, or another host's token for its
// own name is refused, and nothing it sent reaches the log.
func TestHubRefusesUnauthenticatedNode(t *testing.T) {
	h := newHubHarness(t)
	req := nodeRequest{Host: "host-a", Events: []NodePayload{{Host: "host-a", Seq: 1, At: h.now, Report: &NodeReport{}}}}
	if code, _ := h.post("", req); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := h.post("token-of-host-c", req); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", code)
	}
	if code, _ := h.post("token-of-host-b", req); code != http.StatusForbidden {
		t.Fatalf("host-b's token for host-a: %d", code)
	}
	stranger := NewNode(NodeConfig{Name: "host-a", Hub: h.srv.URL, ReposRoot: t.TempDir()}, "not-a-token", &fakeNodeGit{t: t}, h.srv.Client(), h.clock)
	if err := stranger.Tick(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a node with a wrong token: %v", err)
	}
	if ev := h.nodeEvents(); len(ev) != 0 {
		t.Fatalf("refused nodes' events reached the log: %+v", ev)
	}
	// Only the report path is served.
	resp, err := h.srv.Client().Get(h.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /: %d", resp.StatusCode)
	}
}

// A node that stops reporting shows unknown, with what it last reported,
// never missing or idle; one that never reported is unknown too. The one
// still reporting is reporting.
func TestSilentNodeShowsUnknown(t *testing.T) {
	h := newHubHarness(t)
	h.makeClone("host-a", "github.com/example-org/widgets")
	h.tick("host-a")
	st := h.status()
	if st["host-a"].State != NodeReporting || st["host-b"].State != NodeUnknown {
		t.Fatalf("host-a %q, host-b (never reported) %q", st["host-a"].State, st["host-b"].State)
	}
	h.tick("host-b")
	h.now = h.now.Add(9 * time.Minute)
	h.tick("host-b")
	h.now = h.now.Add(2 * time.Minute)
	st = h.status()
	if st["host-a"].State != NodeUnknown || st["host-b"].State != NodeReporting {
		t.Fatalf("after 11 minutes host-a %q, host-b %q", st["host-a"].State, st["host-b"].State)
	}
	a := st["host-a"]
	if len(a.Clones) != 1 || a.Clones[0].Repository != "widgets" || !a.Clones[0].Exists {
		t.Fatalf("host-a's last state is kept: %+v", a.Clones)
	}
	var out strings.Builder
	events, _, _ := ReadLog(h.cfg.EventLog)
	PrintNodes(&out, NodeStatus(h.cfg, events, h.now), h.now)
	text := out.String()
	for _, want := range []string{"host-a", "unknown", "host-b", "reporting"} {
		if !strings.Contains(text, want) {
			t.Fatalf("nodes table lacks %q:\n%s", want, text)
		}
	}
	for _, never := range []string{"missing", "idle", "gone"} {
		if strings.Contains(text, never) {
			t.Fatalf("a silent node is shown %s:\n%s", never, text)
		}
	}
}

// A clone the policy requires on a node and its report says is missing is
// created there, from the configured URL at the layout's path, and
// reported. A clone the node has already gets no command.
func TestRequiredMissingCloneIsCreated(t *testing.T) {
	h := newHubHarness(t)
	h.makeClone("host-b", "github.com/example-org/widgets")
	h.tick("host-a")
	h.tick("host-b")
	if got := h.gits["host-a"].cloned; len(got) != 1 || got[0] != "git@github.com:example-org/widgets.git "+filepath.Join(h.nodes["host-a"].cfg.ReposRoot, "github.com/example-org/widgets") {
		t.Fatalf("host-a cloned %v", got)
	}
	if got := h.gits["host-b"].cloned; len(got) != 1 || !strings.HasPrefix(got[0], "https://gitlab.com/example-org/tools/gadgets.git ") {
		t.Fatalf("host-b cloned %v; widgets it had already", got)
	}
	var done []string
	for _, p := range h.nodeEvents() {
		if p.Result != nil && p.Result.Outcome == NodeDone {
			done = append(done, p.Host+" "+p.Result.Repository)
		}
	}
	if strings.Join(done, ",") != "host-a widgets,host-b gadgets" {
		t.Fatalf("results: %v", done)
	}
	// In the log, host-a's events are in the order it made them: its
	// report, the clone's result, then the report after it.
	var order []string
	for _, p := range h.nodeEvents() {
		if p.Host != "host-a" {
			continue
		}
		kind := "report"
		if p.Result != nil {
			kind = "result"
		}
		order = append(order, kind)
	}
	if strings.Join(order, ",") != "report,result,report" {
		t.Fatalf("host-a's events: %v", order)
	}
	st := h.status()
	for _, c := range st["host-a"].Clones {
		if c.Repository == "widgets" && !c.Exists {
			t.Fatal("host-a's report after the clone still says missing")
		}
	}
	// Ticks after that ask for nothing more.
	h.tick("host-a")
	if len(h.gits["host-a"].cloned) != 1 {
		t.Fatalf("cloned again: %v", h.gits["host-a"].cloned)
	}
}

// A failed clone is reported and not asked for again until retry_after
// has passed.
func TestFailedCloneWaitsBeforeRetry(t *testing.T) {
	h := newHubHarness(t)
	h.gits["host-a"].fail = os.ErrPermission
	h.tick("host-a")
	h.now = h.now.Add(time.Minute)
	h.tick("host-a")
	if n := len(h.gits["host-a"].cloned); n != 1 {
		t.Fatalf("clone tried %d times within retry_after", n)
	}
	var failed int
	for _, p := range h.nodeEvents() {
		if p.Result != nil && p.Result.Outcome == NodeFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("%d failed results", failed)
	}
	h.now = h.now.Add(30 * time.Minute)
	h.gits["host-a"].fail = nil
	h.tick("host-a")
	if n := len(h.gits["host-a"].cloned); n != 2 {
		t.Fatalf("clone tried %d times after retry_after", n)
	}
}

// A dirty clone, one with a stash, and one holding open methodology state
// are reported as they are and nothing in them changes: no command is
// given for a clone that exists.
func TestDirtyCloneIsReportedAndUntouched(t *testing.T) {
	h := newHubHarness(t)
	dir := h.makeClone("host-a", "github.com/example-org/widgets")
	writeFile(t, filepath.Join(dir, "a"), "changed\n")
	writeFile(t, filepath.Join(dir, "new"), "untracked\n")
	writeFile(t, filepath.Join(dir, ".kickoff", "budget"), "3\n")
	before := treeSnapshot(t, dir)
	h.tick("host-a")
	h.now = h.now.Add(time.Minute)
	h.tick("host-a")
	if after := treeSnapshot(t, dir); after != before {
		t.Fatalf("the dirty clone changed:\n%s\n---\n%s", before, after)
	}
	if got := h.gits["host-a"].cloned; len(got) != 0 {
		t.Fatalf("cloned %v", got)
	}
	var c *CloneState
	for i, x := range h.status()["host-a"].Clones {
		if x.Repository == "widgets" {
			c = &h.status()["host-a"].Clones[i]
		}
	}
	if c == nil || !c.Exists || !c.Git || c.Dirty != 3 || c.Branch != "main" || len(c.Head) != 40 || strings.Join(c.OpenState, ",") != ".kickoff" {
		t.Fatalf("reported %+v", c)
	}
	for _, p := range h.nodeEvents() {
		if p.Result != nil {
			t.Fatalf("a command reached host-a: %+v", p.Result)
		}
	}
}

// The node checks a command itself: a clone into a path that exists, or
// outside its root, or for a repository it was not told to watch there, is
// refused and nothing is done.
func TestNodeRefusesUnsafeCommands(t *testing.T) {
	h := newHubHarness(t)
	n := h.nodes["host-a"]
	dir := h.makeClone("host-a", "github.com/example-org/widgets")
	n.watch = []NodeWatch{{Repository: "widgets", Path: "github.com/example-org/widgets"}}
	cases := []NodeCommand{
		{ID: "c1", Action: NodeClone, Repository: "widgets", Path: "github.com/example-org/widgets", URL: "u"},
		{ID: "c2", Action: NodeClone, Repository: "widgets", Path: "../escape", URL: "u"},
		{ID: "c3", Action: NodeClone, Repository: "gadgets", Path: "gitlab.com/x", URL: "u"},
		{ID: "c4", Action: "delete", Repository: "widgets", Path: "github.com/example-org/widgets"},
	}
	for _, c := range cases {
		if r := n.do(context.Background(), c); r.Outcome != NodeRefused {
			t.Fatalf("%s: %+v", c.ID, r)
		}
	}
	if len(h.gits["host-a"].cloned) != 0 {
		t.Fatalf("cloned %v", h.gits["host-a"].cloned)
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
}

// ExecNodeGit clones from a URL and scans a checkout without taking git's
// optional locks, so a scan writes nothing into the clone.
func TestExecNodeGitClonesAndScans(t *testing.T) {
	root := t.TempDir()
	origin, seed := filepath.Join(root, "origin.git"), filepath.Join(root, "seed")
	gitRun(t, root, "init", "-q", "--bare", origin)
	gitRun(t, root, "clone", "-q", origin, seed)
	writeFile(t, filepath.Join(seed, "a"), "a\n")
	gitRun(t, seed, "add", "a")
	gitRun(t, seed, "commit", "-q", "-m", "base")
	gitRun(t, seed, "push", "-q", "origin", "HEAD")
	g := ExecNodeGit{Timeout: 10 * time.Second, CloneTimeout: 10 * time.Second}
	// A clone that fails leaves nothing at its path, nor beside it.
	bad := filepath.Join(root, "repos", "host", "bad")
	if err := g.Clone(context.Background(), filepath.Join(root, "no-such.git"), bad); err == nil {
		t.Fatal("a clone of nothing succeeded")
	}
	if left, _ := os.ReadDir(filepath.Dir(bad)); len(left) != 0 {
		t.Fatalf("a failed clone left %v", left)
	}
	// So does one killed at its timeout, and one whose node was stopped.
	quick := ExecNodeGit{Timeout: 10 * time.Second, CloneTimeout: time.Nanosecond}
	if err := quick.Clone(context.Background(), origin, bad); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("a clone past its timeout: %v", err)
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Clone(stopped, origin, bad); err == nil {
		t.Fatal("a clone of a stopped node succeeded")
	}
	if left, _ := os.ReadDir(filepath.Dir(bad)); len(left) != 0 {
		t.Fatalf("a killed clone left %v", left)
	}
	dst := filepath.Join(root, "repos", "host", "clone")
	if err := g.Clone(context.Background(), origin, dst); err != nil {
		t.Fatal(err)
	}
	c, err := g.Scan(dst)
	if err != nil || !c.Git || c.Dirty != 0 || len(c.Head) != 40 || c.Branch == "" {
		t.Fatalf("scan: %+v, %v", c, err)
	}
	writeFile(t, filepath.Join(dst, "a"), "b\n")
	if c, _ := g.Scan(dst); c.Dirty != 1 {
		t.Fatalf("dirty: %+v", c)
	}
	plain := filepath.Join(root, "plain")
	os.MkdirAll(plain, 0o755)
	if c, err := g.Scan(plain); err != nil || c.Git {
		t.Fatalf("a directory that is no checkout: %+v, %v", c, err)
	}
}

// treeSnapshot lists every file under dir with its content and mode, and
// git's index, so a test sees any change at all.
func treeSnapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b.WriteString(rel + " " + info.Mode().String() + " " + info.ModTime().String())
		if info.Mode().IsRegular() {
			c, _ := os.ReadFile(p)
			b.Write(c)
		}
		b.WriteString("\n")
		return nil
	})
	return b.String()
}

// The hub refuses an address that is a name or every address, and a
// host's token file readable by others.
func TestHubConfigRefusals(t *testing.T) {
	h := newHubHarness(t)
	for _, addr := range []string{":7450", "0.0.0.0:7450", "hub.example:7450"} {
		cfg := *h.cfg
		cfg.Hub.Address = addr
		if _, err := NewHub(&cfg, h.log, nil, h.clock); err == nil {
			t.Fatalf("address %q accepted", addr)
		}
	}
	os.Chmod(h.cfg.Hosts["host-a"].TokenFile, 0o644)
	if _, err := NewHub(h.cfg, h.log, nil, h.clock); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("token readable by others: %v", err)
	}
}

// Config: clone_on names configured hosts; hub and node durations are
// positive.
func TestNodeConfigValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) error {
		p := filepath.Join(dir, "agentd.json")
		writeFile(t, p, body)
		_, err := LoadConfig(p)
		return err
	}
	if err := write(`{"hosts":{"host-a":{"token_file":"a.token"}},"repositories":{"widgets":{"forge":"github","project":"example-org/widgets","clone_on":["host-b"]}}}`); err == nil || !strings.Contains(err.Error(), "clone_on") {
		t.Fatalf("unknown host in clone_on: %v", err)
	}
	if err := write(`{"hub":{"silent_after":"0s"}}`); err == nil || !strings.Contains(err.Error(), "hub") {
		t.Fatalf("zero silent_after: %v", err)
	}
	if err := write(`{"hosts":{"host a":{"token_file":"a.token"}}}`); err == nil {
		t.Fatal("a host name with a space accepted")
	}
	if err := write(`{"hosts":{"host-a":{"token_file":"a.token"}},"repositories":{"widgets":{"forge":"github","project":"example-org/widgets","clone_on":["host-a"]}}}`); err != nil {
		t.Fatal(err)
	}
}

// A node event is a shared fact for no one session, naming its host, a
// sequence number and exactly one of a report and a result.
func TestNodeEventCheck(t *testing.T) {
	ok := Event{V: EventVersion, Kind: KindNode, Mode: ModeShared, Node: &NodePayload{Host: "host-a", Seq: 1, Report: &NodeReport{}}}
	if err := ok.check(); err != nil {
		t.Fatal(err)
	}
	bad := []Event{
		{V: EventVersion, Kind: KindNode, Mode: ModeLive, Node: &NodePayload{Host: "host-a", Seq: 1, Report: &NodeReport{}}},
		{V: EventVersion, Kind: KindNode, Mode: ModeShared, Session: "s", Node: &NodePayload{Host: "host-a", Seq: 1, Report: &NodeReport{}}},
		{V: EventVersion, Kind: KindNode, Mode: ModeShared, Node: &NodePayload{Seq: 1, Report: &NodeReport{}}},
		{V: EventVersion, Kind: KindNode, Mode: ModeShared, Node: &NodePayload{Host: "host-a", Report: &NodeReport{}}},
		{V: EventVersion, Kind: KindNode, Mode: ModeShared, Node: &NodePayload{Host: "host-a", Seq: 1}},
		{V: EventVersion, Kind: KindNode, Mode: ModeShared, Node: &NodePayload{Host: "host-a", Seq: 1, Report: &NodeReport{}, Result: &NodeResult{}}},
	}
	for i, e := range bad {
		if err := e.check(); err == nil {
			t.Fatalf("bad %d accepted", i)
		}
	}
}
