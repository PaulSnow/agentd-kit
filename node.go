package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
)

// Node and hub (#29; #4 §2, #6). agentd runs on every machine as a node
// that watches its own clones, and one machine runs the hub, which holds
// the event log and the policy. Not every machine can reach every other,
// but every node can reach the hub, so the node asks: on each tick it
// sends the hub its new events, numbered per host, and the hub's answer is
// the node interface, what the node is to watch and the commands the hub's
// record allows. A node acts on nothing else, and on a command only once
// it has checked it is safe: it creates what is missing and never deletes,
// resets, pulls, rebases, moves or changes remotes. A host that stops
// reporting shows as unknown, with what it last reported.

// HubConfig is the hub: the one IP address it listens on, on the private
// network; how long a host may be silent before it shows as unknown; and
// how long after a failed clone the hub asks for it again.
type HubConfig struct {
	Address     string   `json:"address"`
	SilentAfter Duration `json:"silent_after"`
	RetryAfter  Duration `json:"retry_after"`
}

// HostConfig is a machine a node runs on. TokenFile holds the token that
// node authenticates with, its own: the hub knows the host from its token,
// so one node cannot report as another.
type HostConfig struct {
	TokenFile string `json:"token_file"`
}

// NodeConfig is this machine's node: its host name, the hub's URL, its
// token file, the root its clones are laid out under (#4 §2:
// ReposRoot/<forge host>/<project>), how often it reports, and the paths
// inside a checkout that mean it holds open methodology state.
type NodeConfig struct {
	Name      string   `json:"name"`
	Hub       string   `json:"hub"`
	TokenFile string   `json:"token_file"`
	ReposRoot string   `json:"repos_root"`
	Every     Duration `json:"every"`
	OpenState []string `json:"open_state"`
	// CloneTimeout bounds one clone, which takes longer than a scan.
	CloneTimeout Duration `json:"clone_timeout"`
}

// Hub and node defaults.
const (
	DefaultSilentAfter  = 10 * time.Minute
	DefaultRetryAfter   = 30 * time.Minute
	DefaultNodeEvery    = time.Minute
	DefaultCloneTimeout = 30 * time.Minute
	nodeReportPath      = "/v1/report"
	nodeBodyMax         = 1 << 20
)

// KindNode is a node's event, as the hub appended it.
const KindNode = "node"

// NodePayload is one event a node sent: its host, its number in the
// host's sequence, the node's time, and either a report or a command's
// result.
type NodePayload struct {
	Host   string      `json:"host"`
	Seq    int64       `json:"seq"`
	At     time.Time   `json:"at"`
	Report *NodeReport `json:"report,omitempty"`
	Result *NodeResult `json:"result,omitempty"`
}

// NodeReport is the state of the clones a node watches, as it scanned them.
type NodeReport struct {
	Clones []CloneState `json:"clones"`
}

// CloneState is one watched clone. Path is relative to the node's repos
// root. A path that exists but is no git checkout is Exists without Git.
// Ahead and Behind count against the branch's upstream, when it has one.
// OpenState names the open-state paths the checkout holds.
type CloneState struct {
	Repository string   `json:"repository"`
	Path       string   `json:"path"`
	Exists     bool     `json:"exists"`
	Git        bool     `json:"git,omitempty"`
	Branch     string   `json:"branch,omitempty"`
	Head       string   `json:"head,omitempty"`
	Dirty      int      `json:"dirty,omitempty"`
	Stashes    int      `json:"stashes,omitempty"`
	Upstream   bool     `json:"upstream,omitempty"`
	Ahead      int      `json:"ahead,omitempty"`
	Behind     int      `json:"behind,omitempty"`
	OpenState  []string `json:"open_state,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// NodeResult is what a node did with one command.
type NodeResult struct {
	Command    string `json:"command"`
	Action     string `json:"action"`
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Outcome    string `json:"outcome"`
	Error      string `json:"error,omitempty"`
}

// Node command actions and outcomes.
const (
	NodeClone   = "clone"
	NodeDone    = "done"
	NodeRefused = "refused" // the node's own check declined it
	NodeFailed  = "failed"
)

// nodeRequest is a node's report to the hub: its events, oldest first.
// One with no events asks only for the hub's answer.
type nodeRequest struct {
	Host   string        `json:"host"`
	Events []NodePayload `json:"events"`
}

// nodeReply is the hub's answer: the last of the host's events it holds,
// the clones the host is to watch, and the commands its record allows.
type nodeReply struct {
	Acked    int64         `json:"acked"`
	Watch    []NodeWatch   `json:"watch"`
	Commands []NodeCommand `json:"commands"`
	Error    string        `json:"error,omitempty"`
}

// NodeWatch is a clone a node watches: a repository and its layout path.
type NodeWatch struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
}

// NodeCommand is something the hub's record allows a node to do.
type NodeCommand struct {
	ID         string `json:"id"`
	Action     string `json:"action"`
	Repository string `json:"repository"`
	Path       string `json:"path"`
	URL        string `json:"url,omitempty"`
}

func (p *NodePayload) check() error {
	if p.Host == "" || p.Seq < 1 || (p.Report == nil) == (p.Result == nil) {
		return errors.New("a node event names its host and a sequence number from 1, and carries a report or a result")
	}
	return nil
}

// layoutPath is where a repository's clone lives under a repos root: its
// forge host, then its project path (#4 §2).
func layoutPath(r Repository) string { return r.Host + "/" + r.Project }

// watchList is what host is to watch: every repository whose clone_on
// names it, by name.
func watchList(cfg *Config, host string) []NodeWatch {
	var out []NodeWatch
	for _, name := range sortedKeys(cfg.Repositories) {
		r := cfg.Repositories[name]
		for _, h := range r.CloneOn {
			if h == host {
				out = append(out, NodeWatch{Repository: name, Path: layoutPath(r)})
				break
			}
		}
	}
	return out
}

// NodeRecord is the hub's record of one host, folded from its events.
type NodeRecord struct {
	Host   string
	Seq    int64
	LastAt time.Time // the hub's time of the host's last event
	// ReportSeq is the number of the latest report, which Clones are.
	ReportSeq int64
	Clones    map[string]CloneState
	// Tried is when each repository's last clone that was not done
	// came back, by the hub's time.
	Tried map[string]time.Time
}

func newNodeRecord(host string) *NodeRecord {
	return &NodeRecord{Host: host, Clones: map[string]CloneState{}, Tried: map[string]time.Time{}}
}

func (r *NodeRecord) apply(e Event) {
	p := e.Node
	r.Seq, r.LastAt = p.Seq, e.Time
	switch {
	case p.Report != nil:
		r.ReportSeq = p.Seq
		r.Clones = map[string]CloneState{}
		for _, c := range p.Report.Clones {
			r.Clones[c.Repository] = c
		}
	case p.Result != nil && p.Result.Action == NodeClone:
		if p.Result.Outcome == NodeDone {
			delete(r.Tried, p.Result.Repository)
		} else {
			r.Tried[p.Result.Repository] = e.Time
		}
	}
}

// foldNodes is every host's record from the log's node events.
func foldNodes(events []Event) map[string]*NodeRecord {
	out := map[string]*NodeRecord{}
	for _, e := range events {
		if e.Kind != KindNode || e.Node == nil {
			continue
		}
		r := out[e.Node.Host]
		if r == nil {
			r = newNodeRecord(e.Node.Host)
			out[e.Node.Host] = r
		}
		r.apply(e)
	}
	return out
}

// commands are what the record allows rec's host to do now: a clone of a
// repository its latest report says is absent, unless the last try of it
// failed within retry. A clone that exists, whatever its state, gets
// nothing.
func commands(cfg *Config, rec *NodeRecord, now time.Time) []NodeCommand {
	var out []NodeCommand
	for _, w := range watchList(cfg, rec.Host) {
		c, ok := rec.Clones[w.Repository]
		if !ok || c.Exists || c.Path != w.Path {
			continue
		}
		if t, tried := rec.Tried[w.Repository]; tried && now.Sub(t) < cfg.Hub.RetryAfter.Duration {
			continue
		}
		out = append(out, NodeCommand{ID: NodeClone + ":" + w.Repository + ":" + strconv.FormatInt(rec.ReportSeq, 10),
			Action: NodeClone, Repository: w.Repository, Path: w.Path, URL: pushURL(cfg.Repositories[w.Repository])})
	}
	return out
}

// Hub serves nodes' reports and appends their events to the log.
type Hub struct {
	cfg    *Config
	log    *Log
	now    func() time.Time
	tokens map[string][]byte // host -> token

	mu    sync.Mutex
	nodes map[string]*NodeRecord
}

// NewHub is a hub over log, which already holds events. Its address must
// be one IP address, so it listens on the private network alone, and every
// host's token file must be the owner's alone and hold a token.
func NewHub(cfg *Config, log *Log, events []Event, now func() time.Time) (*Hub, error) {
	host, _, err := net.SplitHostPort(cfg.Hub.Address)
	if err != nil {
		return nil, fmt.Errorf("hub.address: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		return nil, fmt.Errorf("hub.address %q must be one IP address, not a name or every address", cfg.Hub.Address)
	}
	if len(cfg.Hosts) == 0 {
		return nil, errors.New("hosts: the hub has no host to serve")
	}
	tokens := map[string][]byte{}
	var errs []error
	for _, name := range sortedKeys(cfg.Hosts) {
		tok, err := readToken(cfg.Hosts[name].TokenFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("hosts.%s.token_file: %w", name, err))
			continue
		}
		for other, t := range tokens {
			if subtle.ConstantTimeCompare(t, tok) == 1 {
				errs = append(errs, fmt.Errorf("hosts.%s and hosts.%s have the same token; each host needs its own", other, name))
			}
		}
		tokens[name] = tok
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Hub{cfg: cfg, log: log, now: now, tokens: tokens, nodes: foldNodes(events)}, nil
}

// readToken is the token in path, which checkTokenFile accepts.
func readToken(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("not set")
	}
	if err := checkTokenFile(path); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b), nil
}

// host is the host whose token r carries, or "".
func (h *Hub) host(r *http.Request) string {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return ""
	}
	found := ""
	for _, name := range sortedKeys(h.tokens) {
		if subtle.ConstantTimeCompare(h.tokens[name], []byte(tok)) == 1 {
			found = name
		}
	}
	return found
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != nodeReportPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	host := h.host(r)
	if host == "" {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, nodeBodyMax))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	var req nodeRequest
	if err := decodeStrict("request", b, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Host != host {
		http.Error(w, "the token is not "+req.Host+"'s", http.StatusForbidden)
		return
	}
	code, reply := h.take(host, req.Events)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(reply)
}

// take appends host's events that continue its sequence, in order. One the
// hub has already is skipped, so a node that lost an answer may send again;
// one that skips a number stops the batch with 409, and those after it are
// not taken. The answer names the last event the hub holds.
func (h *Hub) take(host string, events []NodePayload) (int, nodeReply) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := h.nodes[host]
	if rec == nil {
		rec = newNodeRecord(host)
		h.nodes[host] = rec
	}
	code, msg := http.StatusOK, ""
	for _, p := range events {
		if p.Seq <= rec.Seq {
			continue
		}
		if p.Seq != rec.Seq+1 {
			code, msg = http.StatusConflict, fmt.Sprintf("event %d does not follow %d", p.Seq, rec.Seq)
			break
		}
		if p.Host != host {
			code, msg = http.StatusBadRequest, "an event names another host"
			break
		}
		e := Event{V: EventVersion, Time: h.now().UTC(), Kind: KindNode, Mode: ModeShared, Node: &p}
		if err := e.check(); err != nil {
			code, msg = http.StatusBadRequest, err.Error()
			break
		}
		if err := h.log.Append(e); err != nil {
			code, msg = http.StatusInternalServerError, "the hub could not append to its log"
			break
		}
		rec.apply(e)
	}
	reply := nodeReply{Acked: rec.Seq, Watch: watchList(h.cfg, host), Error: msg}
	if code == http.StatusOK {
		reply.Commands = commands(h.cfg, rec, h.now())
	}
	return code, reply
}

// NodeGit is the git a node runs: a clone of a URL into a new directory,
// and a scan that changes nothing.
type NodeGit interface {
	Clone(ctx context.Context, url, dir string) error
	Scan(dir string) (CloneState, error)
}

// ExecNodeGit runs git, each scan's call bounded by Timeout and a clone by
// CloneTimeout.
type ExecNodeGit struct{ Timeout, CloneTimeout time.Duration }

// Clone clones url into a new directory beside dir, bounded by
// CloneTimeout, and renames it to dir only once the clone is whole. A clone
// that fails or is killed at its timeout leaves no partial checkout at dir
// for a scan to take as the clone: its directory, which this call made, is
// removed, and nothing else. A node told to stop, ending ctx, kills the
// clone's process group and removes it all the same.
func (g ExecNodeGit) Clone(ctx context.Context, url, dir string) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".agentd-clone-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	into := filepath.Join(tmp, "clone")
	ctx, cancel := context.WithTimeout(ctx, g.CloneTimeout)
	defer cancel()
	cmd := groupCommand(ctx, tmp, "git", append(append([]string{}, gitSafe...), "clone", "-q", "--", url, into)...)
	cmd.Env = append(append(os.Environ(), gitEnv...), "LC_ALL=C")
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("git clone of %s stopped: %w", url, ctx.Err())
		}
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return fmt.Errorf("git clone of %s: %s", url, msg)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("something came to %s during the clone", dir)
	}
	return os.Rename(into, dir)
}

// Scan reads dir's branch, head, changes, stashes and distance from its
// upstream. It takes none of git's optional locks, so not even the index
// is refreshed. A directory that is not a checkout's top is no checkout.
func (g ExecNodeGit) Scan(dir string) (CloneState, error) {
	var c CloneState
	run := func(args ...string) (string, error) {
		out, err := runTimed(g.Timeout, dir, append([]string{"GIT_OPTIONAL_LOCKS=0", "LC_ALL=C"}, gitEnv...), false, "git", append(append([]string{}, gitSafe...), args...)...)
		if err != nil {
			return "", fmt.Errorf("git %s in %s: %s", strings.Join(args, " "), dir, exitMessage(err))
		}
		return strings.TrimSpace(string(out)), nil
	}
	top, err := run("rev-parse", "--show-toplevel")
	if err != nil {
		if strings.Contains(err.Error(), "not a git repository") {
			return c, nil
		}
		return c, err // a checkout git refused or did not answer for
	}
	if same, _ := sameDir(top, dir); !same {
		return c, nil
	}
	c.Git = true
	if c.Branch, err = run("symbolic-ref", "-q", "--short", "HEAD"); err != nil {
		c.Branch = "" // detached
	}
	c.Head, _ = run("rev-parse", "-q", "--verify", "HEAD")
	status, err := run("status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return c, err
	}
	c.Dirty = countLines(status)
	stashes, err := run("stash", "list")
	if err != nil {
		return c, err
	}
	c.Stashes = countLines(stashes)
	if lr, err := run("rev-list", "--left-right", "--count", "@{upstream}...HEAD"); err == nil {
		if f := strings.Fields(lr); len(f) == 2 {
			c.Upstream = true
			c.Behind, _ = strconv.Atoi(f[0])
			c.Ahead, _ = strconv.Atoi(f[1])
		}
	}
	return c, nil
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// sameDir says whether a and b are one directory.
func sameDir(a, b string) (bool, error) {
	ia, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	ib, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(ia, ib), nil
}

// Node is this machine's node: it scans what the hub has it watch,
// reports, and does the hub's commands it finds safe.
type Node struct {
	cfg    NodeConfig
	token  string
	git    NodeGit
	client *http.Client
	now    func() time.Time

	known   bool          // acked is the hub's: no post since has failed
	acked   int64         // the last of the host's events the hub holds
	seq     int64         // the last number given to an event
	pending []NodePayload // the events the hub does not hold, in order
	watch   []NodeWatch
}

// NewNode is a node reporting to cfg.Hub with token.
func NewNode(cfg NodeConfig, token string, git NodeGit, client *http.Client, now func() time.Time) *Node {
	return &Node{cfg: cfg, token: token, git: git, client: client, now: now}
}

// Tick reports what the node watches and does the commands the hub
// answers with, reporting their results and the clones after them. Events
// the hub did not take are kept and sent with the next tick. After a post
// that failed, the node first asks the hub what it holds, since it may
// have taken the events before its answer was lost.
func (n *Node) Tick(ctx context.Context) error {
	if !n.known {
		reply, _, err := n.post(ctx, nil)
		if err != nil {
			return err
		}
		n.known, n.watch = true, reply.Watch
		n.ack(reply.Acked)
		n.renumber(reply.Acked)
	}
	n.queueReport()
	reply, err := n.send(ctx)
	if err != nil {
		return err
	}
	if len(reply.Commands) == 0 {
		return nil
	}
	for _, c := range reply.Commands {
		n.queue(NodePayload{Result: n.do(ctx, c)})
	}
	n.queueReport()
	_, err = n.send(ctx)
	return err
}

// queueReport queues a scan of the watched clones. A report the hub does
// not hold is replaced, so a node the hub cannot hear keeps one report and
// its results, never results only: only the newest state is worth sending.
// The hub holds none of the pending events (Tick), so they are numbered
// again from its last.
func (n *Node) queueReport() {
	kept := n.pending[:0]
	for _, p := range n.pending {
		if p.Report == nil {
			kept = append(kept, p)
		}
	}
	n.pending = kept
	n.renumber(n.acked)
	n.queue(NodePayload{Report: &NodeReport{Clones: n.scan()}})
}

func (n *Node) queue(p NodePayload) {
	n.seq++
	p.Host, p.Seq, p.At = n.cfg.Name, n.seq, n.now().UTC()
	n.pending = append(n.pending, p)
}

// renumber numbers the pending events from after.
func (n *Node) renumber(after int64) {
	for i := range n.pending {
		n.pending[i].Seq = after + int64(i) + 1
	}
	n.seq = after + int64(len(n.pending))
}

// send posts the pending events and drops those the hub holds. A gap is
// put right by numbering from the hub's last, which the hub's refusal
// names. A post with no answer leaves the node not knowing what the hub
// holds.
func (n *Node) send(ctx context.Context) (nodeReply, error) {
	reply, code, err := n.post(ctx, n.pending)
	if code == http.StatusConflict {
		n.ack(reply.Acked)
		n.renumber(reply.Acked)
		reply, code, err = n.post(ctx, n.pending)
	}
	if code == http.StatusOK || code == http.StatusConflict {
		n.ack(reply.Acked)
	}
	if err != nil {
		n.known = code == http.StatusConflict
		return reply, err
	}
	n.watch = reply.Watch
	return reply, nil
}

// ack records that the hub holds the host's events up to acked, and drops
// them.
func (n *Node) ack(acked int64) {
	n.acked = acked
	kept := n.pending[:0]
	for _, p := range n.pending {
		if p.Seq > acked {
			kept = append(kept, p)
		}
	}
	n.pending = kept
}

func (n *Node) post(ctx context.Context, events []NodePayload) (nodeReply, int, error) {
	var reply nodeReply
	b, err := json.Marshal(nodeRequest{Host: n.cfg.Name, Events: events})
	if err != nil {
		return reply, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(n.cfg.Hub, "/")+nodeReportPath, bytes.NewReader(b))
	if err != nil {
		return reply, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return reply, 0, fmt.Errorf("hub: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, nodeBodyMax))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusConflict {
		if err := json.Unmarshal(body, &reply); err != nil {
			// Its numbers unknown, the answer is no answer: nothing is
			// acknowledged or numbered again from it.
			return nodeReply{}, 0, fmt.Errorf("hub: answer is not JSON: %w", err)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return reply, resp.StatusCode, fmt.Errorf("hub: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return reply, resp.StatusCode, nil
}

// scan is the state of each watched clone.
func (n *Node) scan() []CloneState {
	out := make([]CloneState, 0, len(n.watch))
	for _, w := range n.watch {
		c := CloneState{Repository: w.Repository, Path: w.Path}
		dir, err := n.local(w.Path)
		if err != nil {
			c.Error = err.Error()
			out = append(out, c)
			continue
		}
		if _, err := os.Lstat(dir); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				c.Exists, c.Error = true, err.Error()
			}
			out = append(out, c)
			continue
		}
		s, err := n.git.Scan(dir)
		s.Repository, s.Path, s.Exists = w.Repository, w.Path, true
		if err != nil {
			s.Error = err.Error()
		}
		for _, o := range n.cfg.OpenState {
			if _, err := os.Lstat(filepath.Join(dir, o)); err == nil {
				s.OpenState = append(s.OpenState, o)
			}
		}
		out = append(out, s)
	}
	return out
}

// local is a layout path under the node's root, refusing one that would
// leave it.
func (n *Node) local(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("path %q is not inside the repos root", rel)
	}
	return filepath.Join(n.cfg.ReposRoot, rel), nil
}

// do carries out one command, if it is safe: a clone, of a repository the
// node watches at that path, into a path where nothing is. Anything else
// is refused and nothing is done.
func (n *Node) do(ctx context.Context, c NodeCommand) *NodeResult {
	r := &NodeResult{Command: c.ID, Action: c.Action, Repository: c.Repository, Path: c.Path}
	refuse := func(why string) *NodeResult {
		r.Outcome, r.Error = NodeRefused, why
		return r
	}
	if c.Action != NodeClone {
		return refuse("the node does no " + c.Action)
	}
	watched := false
	for _, w := range n.watch {
		watched = watched || w == NodeWatch{Repository: c.Repository, Path: c.Path}
	}
	if !watched {
		return refuse("the node does not watch " + c.Repository + " at " + c.Path)
	}
	dir, err := n.local(c.Path)
	if err != nil {
		return refuse(err.Error())
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		return refuse("something is already at " + c.Path)
	}
	if c.URL == "" {
		return refuse("no URL")
	}
	if err := n.git.Clone(ctx, c.URL, dir); err != nil {
		r.Outcome, r.Error = NodeFailed, err.Error()
		return r
	}
	r.Outcome = NodeDone
	return r
}

// Node states in agentd nodes. A host is never shown as missing or idle:
// it reports, or what it last said is all that is known.
const (
	NodeReporting = "reporting"
	NodeUnknown   = "unknown"
)

// NodeRow is one host's state: reporting, or unknown when it has been
// silent for hub.silent_after or never reported, with what it last
// reported.
type NodeRow struct {
	Host   string
	State  string
	LastAt time.Time
	Clones []CloneState
}

// NodeStatus is every configured host's row, and any other host's the log
// holds, by name.
func NodeStatus(cfg *Config, events []Event, now time.Time) []NodeRow {
	recs := foldNodes(events)
	hosts := map[string]bool{}
	for h := range cfg.Hosts {
		hosts[h] = true
	}
	for h := range recs {
		hosts[h] = true
	}
	var out []NodeRow
	for _, h := range sortedKeys(hosts) {
		row := NodeRow{Host: h, State: NodeUnknown}
		if r := recs[h]; r != nil {
			row.LastAt = r.LastAt
			if now.Sub(r.LastAt) <= cfg.Hub.SilentAfter.Duration {
				row.State = NodeReporting
			}
			for _, name := range sortedKeys(r.Clones) {
				row.Clones = append(row.Clones, r.Clones[name])
			}
		}
		out = append(out, row)
	}
	return out
}

// cloneText is a clone's state in a word or a few.
func cloneText(c CloneState) string {
	switch {
	case c.Error != "":
		return "error: " + c.Error
	case !c.Exists:
		return "absent"
	case !c.Git:
		return "not a checkout"
	}
	var parts []string
	if c.Dirty > 0 {
		parts = append(parts, fmt.Sprintf("dirty %d", c.Dirty))
	}
	if c.Stashes > 0 {
		parts = append(parts, fmt.Sprintf("stashes %d", c.Stashes))
	}
	if c.Ahead > 0 || c.Behind > 0 {
		parts = append(parts, fmt.Sprintf("ahead %d behind %d", c.Ahead, c.Behind))
	}
	if len(c.OpenState) > 0 {
		parts = append(parts, "open "+strings.Join(c.OpenState, ","))
	}
	if len(parts) == 0 {
		return "clean"
	}
	return strings.Join(parts, ", ")
}

// PrintNodes writes the hosts table, one row per clone, a host that
// reported none on a row of its own. An unknown host's clones are as it
// last reported them.
func PrintNodes(w io.Writer, rows []NodeRow, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tSTATE\tLAST REPORT\tREPOSITORY\tBRANCH\tCLONE")
	for _, r := range rows {
		last := "never"
		if !r.LastAt.IsZero() {
			last = now.Sub(r.LastAt).Round(time.Second).String() + " ago"
		}
		if len(r.Clones) == 0 {
			fmt.Fprintf(tw, "%s\t%s\t%s\t-\t-\t-\n", r.Host, r.State, last)
		}
		for _, c := range r.Clones {
			branch := c.Branch
			if branch == "" {
				branch = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Host, r.State, last, c.Repository, branch, cloneText(c))
		}
	}
	tw.Flush()
}

// checkNodeConfig validates the hub, the hosts, the node and every
// repository's clone_on, filling in defaults. Token files are read when
// the hub or the node starts, not here: each machine has its own.
func (c *Config) checkNodeConfig(path func(field, p string) string, errs *[]error) {
	if c.Hub.SilentAfter.Duration <= 0 || c.Hub.RetryAfter.Duration <= 0 {
		*errs = append(*errs, errors.New("hub.silent_after and hub.retry_after must be positive"))
	}
	for name, h := range c.Hosts {
		if !sessionName.MatchString(name) {
			*errs = append(*errs, fmt.Errorf("hosts.%q: a host name is letters, digits, - and _", name))
		}
		h.TokenFile = path("hosts."+name+".token_file", h.TokenFile)
		c.Hosts[name] = h
	}
	for _, name := range sortedKeys(c.Repositories) {
		for _, h := range c.Repositories[name].CloneOn {
			if _, ok := c.Hosts[h]; !ok {
				*errs = append(*errs, fmt.Errorf("repositories.%s.clone_on: %q is not in hosts", name, h))
			}
		}
	}
	n := &c.Node
	if n.Every.Duration <= 0 || n.CloneTimeout.Duration <= 0 {
		*errs = append(*errs, errors.New("node.every and node.clone_timeout must be positive"))
	}
	n.TokenFile = path("node.token_file", n.TokenFile)
	n.ReposRoot = path("node.repos_root", n.ReposRoot)
	for _, o := range n.OpenState {
		if !filepath.IsLocal(o) {
			*errs = append(*errs, fmt.Errorf("node.open_state: %q is not a path inside a checkout", o))
		}
	}
	if n.Hub != "" {
		u, err := url.Parse(n.Hub)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.Path != "" && u.Path != "/" {
			*errs = append(*errs, fmt.Errorf("node.hub %q must be the hub's http or https URL, with no path", n.Hub))
		}
	}
}

// hubCmd serves the nodes until interrupted.
func hubCmd(args []string) error {
	fs := flag.NewFlagSet("hub", flag.ContinueOnError)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	log, events, err := openLoaded(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	hub, err := NewHub(cfg, log, events, time.Now)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.Hub.Address, Handler: hub, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: time.Minute, WriteTimeout: time.Minute}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	fmt.Fprintf(os.Stderr, "agentd hub: serving %d hosts on %s\n", len(cfg.Hosts), cfg.Hub.Address)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// nodeCmd reports to the hub every node.every until interrupted, or once.
func nodeCmd(args []string) error {
	fs := flag.NewFlagSet("node", flag.ContinueOnError)
	once := fs.Bool("once", false, "one report, then exit")
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	nc := cfg.Node
	if nc.Name == "" || nc.Hub == "" {
		return errors.New("node.name and node.hub must be set")
	}
	tok, err := readToken(nc.TokenFile)
	if err != nil {
		return fmt.Errorf("node.token_file: %w", err)
	}
	n := NewNode(nc, string(tok), ExecNodeGit{Timeout: cfg.Timeouts.Forge.Duration, CloneTimeout: nc.CloneTimeout.Duration}, &http.Client{Timeout: time.Minute}, time.Now)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		if err := n.Tick(ctx); err != nil {
			if *once {
				return err
			}
			fmt.Fprintln(os.Stderr, "agentd node:", err)
		} else if *once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(nc.Every.Duration):
		}
	}
}

// nodesCmd prints every host's state from the hub's log.
func nodesCmd(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("nodes", flag.ContinueOnError)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	events, torn, err := ReadLog(cfg.EventLog)
	if err != nil {
		return err
	}
	if torn > 0 {
		fmt.Fprintf(os.Stderr, "agentd: ignored a torn final line of %d bytes in %s\n", torn, cfg.EventLog)
	}
	now := time.Now()
	PrintNodes(w, NodeStatus(cfg, events, now), now)
	return nil
}
