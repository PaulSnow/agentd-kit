package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A change is made twice: once when the agent proposes it, to show the
// owner exactly what it does and to refuse one that could not be made,
// and once when the owner said yes, from what the log recorded, checked
// again against the files as they are then.

// opsArgs are a change tool's arguments.
type opsArgs struct {
	Supervisor string          `json:"supervisor"`
	Pointer    string          `json:"pointer"`
	Value      json.RawMessage `json:"value"`
	Delete     bool            `json:"delete"`
	Commit     string          `json:"commit"`
	Binary     string          `json:"binary"`
	Session    string          `json:"session"`
	Why        string          `json:"why"`
}

// opsWhyMax bounds a change's why.
const opsWhyMax = 500

// propose is the change op with args as the owner will be shown it, or
// why it cannot be made.
func (o *opsEnv) propose(op string, a opsArgs) (OpsChange, error) {
	c := OpsChange{Op: op, Supervisor: a.Supervisor, Why: oneLine(a.Why, opsWhyMax)}
	if strings.TrimSpace(a.Why) == "" {
		return c, errors.New("why is empty: say why the owner should make this change")
	}
	switch op {
	case OpsConfigSet:
		return o.proposeSet(c, a)
	case OpsRestart:
		s, ok := o.cfg.Ops.Supervisors[a.Supervisor]
		if !ok {
			_, _, err := o.supervisor(a.Supervisor)
			return c, err
		}
		_, pane := o.up(s)
		c.Summary = fmt.Sprintf("Restart %s (tmux session %s): Ctrl-C, then Up and Enter.", a.Supervisor, s.Tmux)
		// Down beyond doubt, and not this process: a shell waits in its pane.
		if a.Supervisor != o.self && o.down(s) {
			c.Auto = true
			c.Summary = fmt.Sprintf("Restart %s (tmux session %s), which is down (%s): Up and Enter. A supervisor that is down is restarted without a yes.", a.Supervisor, s.Tmux, oneLine(pane, 60))
		}
		return c, nil
	case OpsInstall:
		return o.proposeInstall(c, a)
	case OpsClear, OpsEnd:
		_, cfg, err := o.supervisor(a.Supervisor)
		if err != nil {
			return c, err
		}
		events, _, err := ReadLog(cfg.EventLog)
		if err != nil {
			return c, err
		}
		c.Session = a.Session
		if op == OpsClear {
			if _, ok := projected(cfg, events).Sessions[a.Session]; !ok {
				return c, fmt.Errorf("%s has no session %q", a.Supervisor, cutUTF8(a.Session, 60))
			}
			c.Summary = fmt.Sprintf("Clear session %s on %s: lift its park, so it may be restarted (agentd clear).", a.Session, a.Supervisor)
			return c, nil
		}
		if _, ok := NewLogState(cfg, ModeLive).Fold(events).Runtime[a.Session]; !ok {
			return c, fmt.Errorf("%s has no started session %q running; only a session agentd started is ended", a.Supervisor, cutUTF8(a.Session, 60))
		}
		c.Summary = fmt.Sprintf("End session %s on %s: stop it and take it off the supervised sessions, keeping its files (agentd sessions end).", a.Session, a.Supervisor)
		return c, nil
	case OpsRollback:
		c.Supervisor = ""
		for _, k := range o.keptBinaries() {
			if k == a.Binary {
				c.Kept = k
			}
		}
		if c.Kept == "" {
			return c, fmt.Errorf("%q is not a kept binary; they are %s", cutUTF8(a.Binary, 60), strings.Join(o.keptBinaries(), ", "))
		}
		c.Summary = fmt.Sprintf("Roll agentd back to %s: keep the installed binary, swap %s in, check every supervisor's config with it and restart them all.", c.Kept, c.Kept)
		return c, nil
	}
	return c, fmt.Errorf("no change %q", op)
}

// proposeSet shows a config set: the file edited in its text, checked as
// agentd config checks it, and the diff.
func (o *opsEnv) proposeSet(c OpsChange, a opsArgs) (OpsChange, error) {
	s, ok := o.cfg.Ops.Supervisors[a.Supervisor]
	if !ok {
		_, _, err := o.supervisor(a.Supervisor)
		return c, err
	}
	toks, err := pointerTokens(a.Pointer)
	if err != nil {
		return c, err
	}
	for _, t := range toks {
		if redactedKeys.MatchString(t) {
			return c, fmt.Errorf("%s names a credential or the path to one; the owner edits those by hand", cutUTF8(a.Pointer, 120))
		}
	}
	if a.Delete == (len(a.Value) > 0) {
		return c, errors.New("give a value to set, or delete: true, not both")
	}
	c.Pointer, c.Delete = a.Pointer, a.Delete
	if !a.Delete {
		c.Value = a.Value
	}
	src, err := os.ReadFile(s.Config)
	if err != nil {
		return c, err
	}
	// A value that holds a credential's key, or a delete of one that
	// does, could not be shown to the owner.
	changed := []byte(c.Value)
	if c.Delete {
		if doc, err := decodeNumbers(src); err == nil {
			changed, _ = json.Marshal(pointerGet(doc, toks))
		}
	}
	if v, err := decodeNumbers(changed); err == nil && hasRedactedKey(v) {
		return c, fmt.Errorf("the change at %s holds a credential or the path to one; the owner edits those by hand", cutUTF8(a.Pointer, 120))
	}
	out, err := setConfigText(src, c.Pointer, c.Value, c.Delete)
	if err != nil {
		return c, err
	}
	if err := checkConfigText(s.Config, out); err != nil {
		return c, fmt.Errorf("the config would not pass agentd config, so nothing was proposed: %v", err)
	}
	diff, err := diffLines(filepath.Base(s.Config), src, out)
	if err != nil {
		return c, err
	}
	if diff == "" {
		return c, fmt.Errorf("%s is that already; nothing would change", a.Pointer)
	}
	c.Base, c.Result, c.Diff = fileHash(src), fileHash(out), redactLines(diff, confidentialMatcher(o.cfg))
	var with []string
	for _, n := range o.cfg.Ops.names() {
		if realPath(o.cfg.Ops.Supervisors[n].Config) == realPath(s.Config) {
			with = append(with, n)
		}
	}
	what := "Delete " + c.Pointer
	if !c.Delete {
		what = "Set " + c.Pointer + " to " + oneLine(string(c.Value), 80)
	}
	c.Summary = fmt.Sprintf("%s in %s's config (%s), keeping a timestamped backup, then restart %s.", what, a.Supervisor, filepath.Base(s.Config), strings.Join(with, " and "))
	return c, nil
}

// checkConfigText checks text as the config at path, with the check
// agentd config makes: written beside it, so its relative paths resolve
// as the file's own, and loaded.
func checkConfigText(path string, text []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".check-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(text); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	_, err = LoadConfig(tmp.Name())
	if err != nil {
		// The error names the check's file; the owner knows the config's.
		return errors.New(strings.ReplaceAll(err.Error(), tmp.Name(), path))
	}
	return nil
}

// onMain is commit sha, in full, when the clone's main holds it.
func (o *opsEnv) onMain(sha string) (string, error) {
	if !shaRef.MatchString(sha) {
		return "", fmt.Errorf("%q is not a commit's hex id", cutUTF8(sha, 60))
	}
	full, err := o.git(o.cfg.Ops.Repo, "rev-parse", "--verify", "--quiet", sha+"^{commit}")
	if err != nil || full == "" {
		return "", fmt.Errorf("the clone has no commit %s", sha)
	}
	if _, err := o.git(o.cfg.Ops.Repo, "merge-base", "--is-ancestor", full, "origin/"+o.cfg.Ops.branch()); err != nil {
		return "", fmt.Errorf("commit %s is not on the clone's %s (origin/%s)", short(full), o.cfg.Ops.branch(), o.cfg.Ops.branch())
	}
	return full, nil
}

// proposeInstall shows an install: the commit, on main, and the commits
// it brings over the installed one.
func (o *opsEnv) proposeInstall(c OpsChange, a opsArgs) (OpsChange, error) {
	c.Supervisor = ""
	if o.binary == "" {
		return c, errors.New("agentd does not know the binary to replace; set ops.binary")
	}
	if _, err := o.git(o.cfg.Ops.Repo, "fetch", "--quiet", "origin", o.cfg.Ops.branch()); err != nil {
		return c, err
	}
	full, err := o.onMain(a.Commit)
	if err != nil {
		return c, err
	}
	c.Commit = full
	subject, _ := o.git(o.cfg.Ops.Repo, "log", "-1", "--format=%s", full)
	cur := strings.TrimSuffix(binaryCommit(o.binary), "+modified")
	if cur == full {
		return c, fmt.Errorf("commit %s is installed already", short(full))
	}
	log := ""
	if cur != "" {
		log, _ = o.git(o.cfg.Ops.Repo, "log", "--oneline", "--no-decorate", "-n", "40", cur+".."+full)
	}
	if log == "" {
		log, _ = o.git(o.cfg.Ops.Repo, "log", "--oneline", "--no-decorate", "-n", "1", full)
	}
	installed := "an unknown commit"
	if cur != "" {
		installed = short(cur)
	}
	c.Diff = redactLines("installed: "+installed+"\nto install: "+short(full)+"\n\n"+log, confidentialMatcher(o.cfg))
	c.Summary = fmt.Sprintf("Install commit %s (%s) from %s: build it, keep the running binary as %s.%s, swap it in, check every supervisor's config with it and restart them all, this process last.",
		short(full), oneLine(subject, 100), o.cfg.Ops.branch(), filepath.Base(o.binary), keptSuffix(cur))
	return c, nil
}

// keptSuffix is the suffix the binary of commit cur is kept under.
func keptSuffix(cur string) string {
	if len(cur) >= 12 {
		return cur[:12]
	}
	return "unknown-…"
}

// execute carries out change r, which the owner approved, and says what
// came of it; self says this process restarts to finish it, and the next
// one reports.
func (o *opsEnv) execute(ctx context.Context, r OpsRecord) (outcome string, self bool, err error) {
	switch r.Op {
	case OpsConfigSet:
		return o.executeSet(r)
	case OpsRestart:
		if err := o.restart(r.Supervisor); err != nil {
			if err == errSelf {
				return fmt.Sprintf("restarting %s, this process", r.Supervisor), true, nil
			}
			return "", false, err
		}
		return r.Supervisor + " is up again", false, nil
	case OpsInstall:
		if _, err := o.git(o.cfg.Ops.Repo, "fetch", "--quiet", "origin", o.cfg.Ops.branch()); err != nil {
			return "", false, err
		}
		full, err := o.onMain(r.Commit)
		if err != nil || full != r.Commit {
			return "", false, fmt.Errorf("nothing was installed: %v", err)
		}
		dir, err := os.MkdirTemp("", "agentd-install-")
		if err != nil {
			return "", false, err
		}
		defer os.RemoveAll(dir)
		built := filepath.Join(dir, "agentd")
		if err := o.build(ctx, o.cfg.Ops.Repo, full, built); err != nil {
			return "", false, fmt.Errorf("the build of %s failed, so nothing was installed: %v", short(full), err)
		}
		return o.swapIn(built, "commit "+short(full))
	case OpsRollback:
		if base := filepath.Base(o.binary) + "."; !strings.HasPrefix(r.Kept, base) || !keptName.MatchString(strings.TrimPrefix(r.Kept, base)) {
			return "", false, fmt.Errorf("%q is not a kept binary's name", cutUTF8(r.Kept, 60))
		}
		src := filepath.Join(filepath.Dir(o.binary), r.Kept)
		dir, err := os.MkdirTemp("", "agentd-rollback-")
		if err != nil {
			return "", false, err
		}
		defer os.RemoveAll(dir)
		// A copy, since keeping the installed one may name the same file.
		held := filepath.Join(dir, "agentd")
		if err := copyFileTo(src, held); err != nil {
			return "", false, fmt.Errorf("nothing was rolled back: %v", err)
		}
		return o.swapIn(held, r.Kept)
	case OpsClear, OpsEnd:
		_, cfg, err := o.supervisor(r.Supervisor)
		if err != nil {
			return "", false, err
		}
		log, events, err := openLoaded(cfg.EventLog)
		if err != nil {
			return "", false, err
		}
		defer log.Close()
		if r.Op == OpsClear {
			if err := ClearSession(projected(cfg, events), log, o.now(), r.Session); err != nil {
				return "", false, err
			}
			return fmt.Sprintf("cleared %s on %s; %s restarts it at its next pass", r.Session, r.Supervisor, r.Supervisor), false, nil
		}
		if err := EndSession(cfg, log, events, o.now(), r.Session); err != nil {
			return "", false, err
		}
		return fmt.Sprintf("ended %s on %s; its files are kept", r.Session, r.Supervisor), false, nil
	}
	return "", false, fmt.Errorf("no change %q", r.Op)
}

// executeSet writes an approved config set: on the file it was shown
// against, the same edit, checked again, after a timestamped backup;
// then the supervisors that run the file are restarted.
func (o *opsEnv) executeSet(r OpsRecord) (string, bool, error) {
	s, ok := o.cfg.Ops.Supervisors[r.Supervisor]
	if !ok {
		return "", false, fmt.Errorf("no supervisor %q any more", r.Supervisor)
	}
	src, err := os.ReadFile(s.Config)
	if err != nil {
		return "", false, err
	}
	if fileHash(src) != r.Base {
		return "", false, fmt.Errorf("%s changed since the diff was shown, so nothing was written; ask for the change again", filepath.Base(s.Config))
	}
	out, err := setConfigText(src, r.Pointer, r.Value, r.Delete)
	if err != nil {
		return "", false, err
	}
	if fileHash(out) != r.Result {
		return "", false, errors.New("the edit no longer makes the file that was shown, so nothing was written")
	}
	if err := checkConfigText(s.Config, out); err != nil {
		return "", false, fmt.Errorf("the config would not pass agentd config, so nothing was written: %v", err)
	}
	info, err := os.Stat(s.Config)
	if err != nil {
		return "", false, err
	}
	backup, err := writeBackup(s.Config+".bak-ops-"+o.now().UTC().Format("20060102T150405Z"), src, info.Mode().Perm())
	if err != nil {
		return "", false, fmt.Errorf("the backup could not be written, so nothing was: %v", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Config), "."+filepath.Base(s.Config)+".new-")
	if err != nil {
		return "", false, err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(out)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), info.Mode().Perm())
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), s.Config)
	}
	if werr != nil {
		return "", false, fmt.Errorf("%s could not be written: %v", filepath.Base(s.Config), werr)
	}
	outcome := fmt.Sprintf("wrote %s (the old one is kept as %s)", filepath.Base(s.Config), filepath.Base(backup))
	done, self, _, err := o.restartAll(s.Config, "")
	if len(done) > 0 {
		outcome += "; restarted " + strings.Join(done, ", ")
	}
	if err != nil {
		return "", false, fmt.Errorf("%s; but the restart failed: %v", outcome, err)
	}
	if self {
		outcome += "; " + o.self + ", this process, restarts last"
	}
	return outcome, self, nil
}

// swapIn installs the binary at src, what it is, over the installed one:
// the installed one is kept first, the swap is one rename, then every
// supervisor's config is checked with it and every supervisor restarted,
// this process last. On any failure the kept binary is put back, and
// the supervisors already restarted are restarted on it.
func (o *opsEnv) swapIn(src, what string) (string, bool, error) {
	kept, err := o.keep()
	if err != nil {
		return "", false, fmt.Errorf("the installed binary could not be kept, so nothing was installed: %v", err)
	}
	keptPath := filepath.Join(filepath.Dir(o.binary), kept)
	if err := copyFileTo(src, o.binary); err != nil {
		return "", false, fmt.Errorf("the swap failed, so nothing was installed: %v", err)
	}
	if o.swapped != nil {
		o.swapped(kept)
	}
	back := func(why error, restarted []string) (string, bool, error) {
		if err := copyFileTo(keptPath, o.binary); err != nil {
			return "", false, fmt.Errorf("%v; and putting %s back failed: %v; the binary is %s now", why, kept, err, what)
		}
		var again, failed []string
		for _, name := range restarted {
			if err := o.restart(name); err != nil {
				failed = append(failed, name)
			} else {
				again = append(again, name)
			}
		}
		msg := fmt.Sprintf("%v; %s is back", why, kept)
		if len(again) > 0 {
			msg += ", and " + strings.Join(again, ", ") + " restarted on it"
		}
		if len(failed) > 0 {
			msg += "; " + strings.Join(failed, ", ") + " did not come up on it"
		}
		return "", false, errors.New(msg)
	}
	if err := o.checkConfigs(); err != nil {
		return back(err, nil)
	}
	done, self, failing, err := o.restartAll("", kept)
	if err != nil {
		if failing != "" {
			done = append(done, failing)
		}
		return back(err, done)
	}
	outcome := fmt.Sprintf("installed %s; the previous binary is kept as %s; every config passed agentd config", what, kept)
	if len(done) > 0 {
		outcome += "; restarted " + strings.Join(done, ", ")
	}
	if self {
		outcome += "; " + o.self + ", this process, restarts last"
	}
	return outcome, self, nil
}

// writeBackup writes b to path, or to path.N for the first N free, never
// over a file, and says which.
func writeBackup(path string, b []byte, mode os.FileMode) (string, error) {
	for n := 0; n < 100; n++ {
		p := path
		if n > 0 {
			p = fmt.Sprintf("%s.%d", path, n)
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(b)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		return p, werr
	}
	return "", fmt.Errorf("%s and 99 more exist", path)
}

// pointerGet is the value at toks in doc, or nil.
func pointerGet(doc any, toks []string) any {
	for _, tok := range toks {
		switch x := doc.(type) {
		case map[string]any:
			doc = x[tok]
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(x) {
				return nil
			}
			doc = x[i]
		default:
			return nil
		}
	}
	return doc
}

// opsResolved is what the next process says of a change the last one
// left: one it was restarting itself to finish is done, now it is up;
// any other was cut short, and when it had swapped the binary, the kept
// one is put back.
func (o *opsEnv) opsResolved(r *OpsRecord) (string, string) {
	if r.Restarting != "" {
		commit := short(buildCommit())
		if commit == "" {
			commit = "an unknown commit"
		}
		return OpsDone, fmt.Sprintf("%s; %s is up again, on %s", r.Restarting, o.self, commit)
	}
	why := fmt.Sprintf("agentd stopped while it was carrying this out (started %s)", r.Started.UTC().Format(time.RFC3339))
	if r.Swapped != "" && o.binary != "" {
		if base := filepath.Base(o.binary) + "."; strings.HasPrefix(r.Swapped, base) && keptName.MatchString(strings.TrimPrefix(r.Swapped, base)) {
			if err := copyFileTo(filepath.Join(filepath.Dir(o.binary), r.Swapped), o.binary); err != nil {
				return OpsFailed, fmt.Sprintf("%s, after swapping the binary; putting %s back failed: %v", why, r.Swapped, err)
			}
			return OpsFailed, fmt.Sprintf("%s, after swapping the binary: %s is back; the supervisors restarted since run the new one until restarted with ops_restart", why, r.Swapped)
		}
	}
	return OpsFailed, why + "; ops_status and ops_tail show where it stands"
}
