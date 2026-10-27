package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

// Placement (#15; #6). A repository whose sessions entry names a host has
// its started sessions run there, through the host's node (#29), and not
// here. The start event names the host, and the log's fold holds the
// first start of a name until it is ended, whatever host a later start
// names: one session per repository, branch and kind, from the log alone.
// The hub reads the starts and ends from the log and asks the host's node
// for each in the node's answer, until the node returns its result. The
// node checks every command itself: the account must be logged in on the
// host, and agentd never falls back to another account or another host.
// A start keeps its host, so a host that stops reporting leaves its
// sessions unknown and nothing starts them elsewhere.

// Remote session states in agentd nodes. A session on a host that is
// unknown is unknown (NodeUnknown).
const (
	SessionStarting = "starting" // asked of its node, not yet done
	SessionRunning  = "running"
	SessionGone     = "gone" // started, and its node no longer runs it
	SessionFailed   = "failed"
	SessionStopping = "stopping" // ended, and its node not yet stopped it
)

// placements are the log's started sessions as the hub reads them: the
// fold every run shares (applySession), and the ends of sessions on hosts
// whose stop the node has not yet done.
type placements struct {
	st    *LogState
	stops map[string]remoteStop // by session name
}

// remoteStop is an ended session's stop, still to be done on its host.
type remoteStop struct {
	Host, Repository, End string
}

func newPlacements(cfg *Config) *placements {
	st := NewLogState(cfg, ModeLive)
	st.Runtime, st.Ended = map[string]RuntimePayload{}, map[string]string{}
	return &placements{st: st, stops: map[string]remoteStop{}}
}

// apply folds a session event, and a repository's policy, which its
// sessions' models come from.
func (p *placements) apply(e Event) {
	if e.Mode != ModeLive && e.Mode != ModeOwner && e.Mode != ModeShared && e.Mode != "" {
		return // as LogState.apply: a shadow run's records are its own
	}
	switch e.Kind {
	case KindSession:
		r, running := p.st.Runtime[e.Session]
		p.st.applySession(e)
		if e.Runtime != nil && e.Runtime.Action == SessionEnd && running && r.Host != "" {
			p.stops[e.Session] = remoteStop{Host: r.Host, Repository: r.Repository, End: e.Runtime.ID}
		}
	case KindPolicy:
		p.st.applyPolicy(e)
	case KindRelease:
		if e.Mode == ModeLive || e.Mode == ModeOwner { // a shadow run deploys nothing
			p.st.applyRelease(e)
		}
	case KindNode:
		if e.Node.Result != nil && e.Node.Result.Action == NodeDeployStep {
			p.st.applyDeployResult(e)
		}
	}
}

// settle drops the stops their nodes have done.
func (p *placements) settle(nodes map[string]*NodeRecord) {
	for name, s := range p.stops {
		if r := nodes[s.Host]; r != nil && r.Done[stopID(name, s)] {
			delete(p.stops, name)
		}
	}
}

func startID(name string, r RuntimePayload) string { return NodeSessionStart + ":" + name + ":" + r.ID }
func stopID(name string, s remoteStop) string      { return NodeSessionStop + ":" + name + ":" + s.End }

// on is the sessions placed on host, running or still to stop, by name.
func (p *placements) on(host string) []string {
	names := map[string]bool{}
	for n, r := range p.st.Runtime {
		if r.Host == host {
			names[n] = true
		}
	}
	for n, s := range p.stops {
		if s.Host == host {
			names[n] = true
		}
	}
	return sortedKeys(names)
}

// commands are what rec's host is to do for its sessions now: each stop
// not done, then each start not done unless a stop of its name is still
// to be done, which it waits for. One that failed is asked again after
// hub.retry_after.
func (p *placements) commands(cfg *Config, rec *NodeRecord, now time.Time) []NodeCommand {
	due := func(id string) bool {
		t, failed := rec.Failed[id]
		return !rec.Done[id] && (!failed || now.Sub(t) >= cfg.Hub.RetryAfter.Duration)
	}
	var out []NodeCommand
	for _, name := range sortedKeys(p.stops) {
		if s := p.stops[name]; s.Host == rec.Host && due(stopID(name, s)) {
			out = append(out, NodeCommand{ID: stopID(name, s), Action: NodeSessionStop, Session: name, Repository: s.Repository})
		}
	}
	for _, name := range sortedKeys(p.st.Runtime) {
		r := p.st.Runtime[name]
		repo, ok := cfg.Repositories[r.Repository]
		if _, stopping := p.stops[name]; r.Host != rec.Host || stopping || !ok || repo.Sessions == nil || !due(startID(name, r)) {
			continue
		}
		out = append(out, NodeCommand{ID: startID(name, r), Action: NodeSessionStart, Session: name, Start: r.ID,
			Repository: r.Repository, Path: layoutPath(repo), Kind: r.Kind, Branch: r.Branch, Target: repo.Sessions.Target, Issue: r.Issue,
			Account: r.Account, Model: sessionModel(cfg, p.st, runtimeSession(name, r, repo))})
	}
	return out
}

// refresh folds what the log gained since the hub last read it. Node
// events are the hub's own, folded as it took them.
func (h *Hub) refresh() error {
	events, off, err := h.log.Tail(h.off)
	if err != nil {
		return err
	}
	h.off = off
	for _, e := range events {
		h.places.apply(e)
	}
	return nil
}

// startSession starts a session the hub placed here, if it is safe: a
// session name and plain branches, a repository the node watches and has
// a clone of, an account logged in on this host, and no session of the
// name running. It writes the brief, adds the worktree unless an earlier
// start left one, and starts tmux there under the account.
func (n *Node) startSession(c NodeCommand, r *NodeResult, refuse func(string) *NodeResult) *NodeResult {
	switch {
	case !sessionName.MatchString(c.Session):
		return refuse(fmt.Sprintf("%q is not a session name", c.Session))
	case !plainBranch(c.Branch) || !plainBranch(c.Target):
		return refuse(fmt.Sprintf("branch %q or target %q is not a plain branch", c.Branch, c.Target))
	case !n.watches(c.Repository, c.Path):
		return refuse("the node does not watch " + c.Repository + " at " + c.Path)
	case len(n.cfg.Launch) == 0:
		return refuse("node.launch is not set")
	case n.cfg.SessionsRoot == "":
		return refuse("node.sessions_root is not set")
	}
	acct, ok := n.cfg.Accounts[c.Account]
	if !ok {
		return refuse(fmt.Sprintf("account %s is not an account on %s", c.Account, n.cfg.Name))
	}
	if info, err := os.Stat(acct.ConfigDir); acct.ConfigDir == "" || err != nil || !info.IsDir() {
		return refuse(fmt.Sprintf("account %s is not logged in on %s: its config_dir %q is not there", c.Account, n.cfg.Name, acct.ConfigDir))
	}
	clone, err := n.local(c.Path)
	if err != nil {
		return refuse(err.Error())
	}
	if info, err := os.Stat(clone); err != nil || !info.IsDir() {
		return refuse("no clone of " + c.Repository + " at " + c.Path)
	}
	workdir := filepath.Join(n.cfg.SessionsRoot, c.Session)
	mark := filepath.Join(workdir, nodeMark)
	if alive, err := n.tmux.HasSession(c.Session); err != nil || alive {
		if err != nil {
			r.Outcome, r.Error = NodeFailed, err.Error()
			return r
		}
		// This start's own session, whose start reported an error after
		// tmux came up: it is done.
		if b, err := os.ReadFile(mark); err == nil && strings.TrimSpace(string(b)) == c.Start && c.Start != "" {
			r.Outcome = NodeDone
			return r
		}
		return refuse("a session named " + c.Session + " is already running")
	}
	work := filepath.Join(workdir, "work")
	brief, status := filepath.Join(workdir, "BRIEF.md"), filepath.Join(workdir, "STATUS.md")
	rep := strings.NewReplacer("{brief}", brief, "{status}", status)
	argv := withModel(n.cfg.Launch, c.Model)
	for i, a := range argv {
		argv[i] = rep.Replace(a)
	}
	line, err := accountLine(c.Account, acct, shellJoin(argv))
	if err != nil {
		return refuse(err.Error())
	}
	facts := map[string]any{"repository": c.Repository, "kind": c.Kind, "branch": c.Branch, "target": c.Target, "worktree": work}
	if c.Issue > 0 {
		facts["issue"] = c.Issue
	}
	err = os.MkdirAll(workdir, 0o755)
	if err == nil {
		err = writeAtomic(mark, []byte(c.Start+"\n"))
	}
	if err == nil {
		err = writeStartFiles(workdir, c.Kind, "", facts)
	}
	if _, serr := os.Stat(work); err == nil && errors.Is(serr, os.ErrNotExist) {
		err = n.git.AddWorktree(clone, work, c.Branch, c.Target, c.Kind != KindWork)
	}
	if err == nil {
		err = n.tmux.NewSession(c.Session, work, line, n.width, n.height)
	}
	if err != nil {
		r.Outcome, r.Error = NodeFailed, err.Error()
		return r
	}
	r.Outcome = NodeDone
	return r
}

// nodeMark is the file in a session's workdir that says this node started
// it, holding the start's id: the node stops no session without it.
const nodeMark = ".agentd-node"

// stopSession stops a session the hub placed here: one the hub has it
// watch and this node started, its mark in its workdir. One not running
// is stopped already.
func (n *Node) stopSession(c NodeCommand, r *NodeResult, refuse func(string) *NodeResult) *NodeResult {
	if !sessionName.MatchString(c.Session) {
		return refuse(fmt.Sprintf("%q is not a session name", c.Session))
	}
	alive, err := n.tmux.HasSession(c.Session)
	if err == nil && alive {
		if _, serr := os.Stat(filepath.Join(n.cfg.SessionsRoot, c.Session, nodeMark)); n.cfg.SessionsRoot == "" || serr != nil || !contains(n.tmuxes, c.Session) {
			return refuse(c.Session + " is not a session this node started")
		}
		err = n.tmux.Stop(c.Session)
	}
	if err != nil {
		r.Outcome, r.Error = NodeFailed, err.Error()
		return r
	}
	r.Outcome = NodeDone
	return r
}

// watches says whether the hub has the node watch repo at path.
func (n *Node) watches(repo, path string) bool {
	for _, w := range n.watch {
		if w == (NodeWatch{Repository: repo, Path: path}) {
			return true
		}
	}
	return false
}

// SessionRow is a session placed on a host: where, under which account,
// and its state, with the node's reason when it failed.
type SessionRow struct {
	Name, Repository, Host, Account, State, Error string
}

// RemoteSessionStatus is every session the log places on a host, by name.
func RemoteSessionStatus(cfg *Config, events []Event, now time.Time) []SessionRow {
	p := newPlacements(cfg)
	for _, e := range events {
		p.apply(e)
	}
	nodes := foldNodes(events)
	p.settle(nodes)
	known := func(host string) *NodeRecord {
		if r := nodes[host]; r != nil && now.Sub(r.LastAt) <= cfg.Hub.SilentAfter.Duration {
			return r
		}
		return nil
	}
	var out []SessionRow
	names := map[string]bool{}
	for n, r := range p.st.Runtime {
		if r.Host != "" {
			names[n] = true
		}
	}
	for n := range p.stops {
		names[n] = true
	}
	for _, name := range sortedKeys(names) {
		r, running := p.st.Runtime[name]
		row := SessionRow{Name: name, Repository: r.Repository, Host: r.Host, Account: r.Account}
		s, stopping := p.stops[name]
		if stopping && (!running || r.Host == "") {
			row.Repository, row.Host, row.Account = s.Repository, s.Host, ""
		}
		rec := known(row.Host)
		last := NodeResult{}
		if rec != nil {
			last = rec.Last[name]
		}
		t := TmuxState{}
		if rec != nil {
			t = rec.Sessions[name]
		}
		switch {
		case rec == nil:
			row.State = NodeUnknown
		case stopping:
			row.State = SessionStopping
			if last.Command == stopID(name, s) && last.Outcome != NodeDone {
				row.Error = last.Error
			}
		case t.Error != "":
			row.State, row.Error = NodeUnknown, t.Error
		case rec.Done[startID(name, r)] && t.Alive:
			row.State = SessionRunning
		case rec.Done[startID(name, r)]:
			row.State = SessionGone
		case last.Command == startID(name, r) && last.Outcome != NodeDone:
			row.State, row.Error = SessionFailed, last.Error
		default:
			row.State = SessionStarting
		}
		out = append(out, row)
	}
	return out
}

// PrintSessions writes the placed sessions' table, if there are any.
func PrintSessions(w io.Writer, rows []SessionRow) {
	if len(rows) == 0 {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nSESSION\tHOST\tACCOUNT\tREPOSITORY\tSTATE")
	for _, r := range rows {
		state := r.State
		if r.Error != "" {
			state += ": " + r.Error
		}
		acct := r.Account
		if acct == "" {
			acct = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Host, acct, r.Repository, state)
	}
	tw.Flush()
}

// checkPlacement validates each repository's sessions host: a configured
// host with the repository's clone, and no placement by usage, which reads
// accounts on this machine.
func (c *Config) checkPlacement(errs *[]error) {
	for _, name := range sortedKeys(c.Repositories) {
		r := c.Repositories[name]
		if r.Sessions == nil || r.Sessions.Host == "" {
			continue
		}
		h := r.Sessions.Host
		switch {
		case !mapHas(c.Hosts, h):
			*errs = append(*errs, fmt.Errorf("repositories.%s.sessions.host %q is not in hosts", name, h))
		case !contains(r.CloneOn, h):
			*errs = append(*errs, fmt.Errorf("repositories.%s.sessions.host %q is not in its clone_on: the node adds sessions' worktrees from its clone", name, h))
		}
		if len(r.Policy.Sessions.Accounts) == 0 {
			*errs = append(*errs, fmt.Errorf("repositories.%s: sessions.host needs policy.sessions.accounts: a session on a host runs under an account logged in there", name))
		}
		if len(r.Policy.Sessions.Fallback) > 0 {
			*errs = append(*errs, fmt.Errorf("repositories.%s: sessions.host and policy.sessions.fallback do not go together: placement by usage reads accounts on this machine", name))
		}
	}
}

func mapHas[V any](m map[string]V, k string) bool {
	_, ok := m[k]
	return ok
}
