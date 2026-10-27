package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// A release's steps on a node (#67). The hub asks the node that runs a
// target's steps for the first step not done, as it asks for a clone. The
// node runs it only when its own configuration names the target in
// node.deploy and, for a step over ssh, the host in node.ssh: what a node
// does is the node's owner's to allow, whatever the hub asks. A step is
// its run, then its check; when either fails, its rollback runs, and the
// result says what failed and whether the rollback did.

// NodeDeployStep is the action of a release's step.
const NodeDeployStep = "deploy-step"

// DeployStepCommand is one step of a target's deployment, as the hub asks
// for it: the release's tag, the target, the step's place and the
// owner's retry it belongs to, and its commands, the tag filled in. SSH,
// when set, is the host the commands run on, over ssh from the node.
type DeployStepCommand struct {
	Tag      string   `json:"tag"`
	Target   string   `json:"target"`
	Index    int      `json:"index"`
	Attempt  int      `json:"attempt"`
	Name     string   `json:"name,omitempty"`
	Via      string   `json:"via"`
	SSH      string   `json:"ssh,omitempty"`
	Run      []string `json:"run"`
	Check    []string `json:"check,omitempty"`
	Rollback []string `json:"rollback,omitempty"`
	Timeout  Duration `json:"timeout"`
}

// DeployStepResult is what the node did with a step: the step, which of
// its commands failed, whether the rollback ran and held, and the tail
// of the output.
type DeployStepResult struct {
	Tag        string `json:"tag"`
	Target     string `json:"target"`
	Index      int    `json:"index"`
	Attempt    int    `json:"attempt"`
	Name       string `json:"name,omitempty"`
	FailedAt   string `json:"failed_at,omitempty"` // run or check
	RolledBack bool   `json:"rolled_back,omitempty"`
	Rollback   string `json:"rollback,omitempty"` // why the rollback did not hold
	Output     string `json:"output,omitempty"`
	// Error is the result's, copied in when the fold keeps a failure.
	Error string `json:"error,omitempty"`
}

// deployStep runs a release's step, if the node allows it.
func (n *Node) deployStep(ctx context.Context, c NodeCommand, r *NodeResult, refuse func(string) *NodeResult) *NodeResult {
	d := c.Deploy
	if d == nil || len(d.Run) == 0 {
		return refuse("the step names no command")
	}
	r.Deploy = &DeployStepResult{Tag: d.Tag, Target: d.Target, Index: d.Index, Attempt: d.Attempt, Name: d.Name}
	switch {
	case !releaseTagPattern.MatchString(d.Tag) || strings.Contains(d.Tag, ".."):
		return refuse(fmt.Sprintf("%q is not a tag", d.Tag))
	case !contains(n.cfg.Deploy, c.Repository+"/"+d.Target):
		return refuse("node.deploy does not name " + c.Repository + "/" + d.Target)
	case d.Via == ViaSSH && (d.SSH == "" || !contains(n.cfg.SSH, d.SSH)):
		return refuse(fmt.Sprintf("this node holds no ssh access to %q: node.ssh does not name it", d.SSH))
	case d.Via != ViaSSH && d.SSH != "":
		return refuse("a step not over ssh names an ssh host")
	}
	dir := n.cfg.ReposRoot
	if d.Via != ViaSSH {
		if p, err := n.local(c.Path); err == nil {
			if fi, err := os.Stat(p); err == nil && fi.IsDir() {
				dir = p // a step of the node's own host runs in its clone
			}
		}
	}
	timeout := d.Timeout.Duration
	if timeout <= 0 {
		timeout = DefaultStepTimeout
	}
	var out strings.Builder
	run := func(what string, argv []string) bool {
		if len(argv) == 0 {
			return true
		}
		if d.SSH != "" {
			argv = sshArgv(d.SSH, argv)
		}
		exit, output, err := n.run(dir, argv, timeout)
		fmt.Fprintf(&out, "%s: exit %d\n%s\n", what, exit, strings.TrimSpace(output))
		if err != nil {
			fmt.Fprintf(&out, "%s: %v\n", what, err)
		}
		return err == nil && exit == 0
	}
	r.Deploy.FailedAt = "run"
	if run("run", d.Run) {
		r.Deploy.FailedAt = "check"
		if run("check", d.Check) {
			r.Outcome, r.Deploy.FailedAt, r.Deploy.Output = NodeDone, "", tailOf(out.String(), releaseOutputMax)
			return r
		}
	}
	r.Outcome, r.Error = NodeFailed, "step "+stepLabel(d.Index, d.Name)+": the "+r.Deploy.FailedAt+" failed"
	if len(d.Rollback) > 0 {
		r.Deploy.RolledBack = run("rollback", d.Rollback)
		if !r.Deploy.RolledBack {
			r.Deploy.Rollback = "the rollback failed too"
		}
	}
	r.Deploy.Output = tailOf(out.String(), releaseOutputMax)
	return r
}

// sshArgv runs argv on host over ssh, each word quoted for its shell.
func sshArgv(host string, argv []string) []string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return []string{"ssh", "-o", "BatchMode=yes", "--", host, strings.Join(q, " ")}
}

// stepLabel names a step: its place, from 1, and its name.
func stepLabel(i int, name string) string {
	if name == "" {
		return fmt.Sprint(i + 1)
	}
	return fmt.Sprintf("%d (%s)", i+1, name)
}

// tailOf is the last max bytes of s.
func tailOf(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	s = s[len(s)-max:]
	for i := 0; i < len(s) && i < 4; i++ { // not in the middle of a rune
		if s[i]&0xC0 != 0x80 {
			return "…" + s[i:]
		}
	}
	return "…" + s
}

// execStep runs argv in dir, bounded by timeout, with the node's
// environment: a step deploys, and needs what the node's owner has.
func execStep(dir string, argv []string, timeout time.Duration) (int, string, error) {
	return ExecRunner{}.Gate(dir, argv, os.Environ(), timeout)
}
