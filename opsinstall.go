package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// Go installs itself (#68). The ops-serving agentd watches the tip of
// ops.branch on the clone's origin: when the forge of the repository
// ops.watch names reports its CI green, and it is neither installed nor
// proposed yet, Go proposes its install on the ops console, as the ops
// agent's ops_install would, and says so there. With ops.auto_install the
// install needs no yes: it is carried out at the next pass, as #63
// carries out an install, keeping the running binary as agentd.SHA and
// putting it back when agentd config or a restart fails; the console is
// told what was installed and what changed, or why it was put back. One
// install is proposed or under way at a time, and a tip once proposed is
// never proposed again, whatever came of it.

// watchInstall looks at the watched branch's tip once a poll.ci_every, in
// a worker. A tip already installed or proposed, not ahead of the
// installed commit, or whose CI is not green, is left; why is logged as
// an error when it is a failure, once until it changes.
func (s *Supervisor) watchInstall(now time.Time) {
	o := s.ops
	if o == nil || s.shadow || o.cfg.Ops.Watch == "" || s.work.busy("ops-watch") || s.work.busy("ops") {
		return
	}
	if !s.watchedAt.IsZero() && now.Sub(s.watchedAt) < s.cfg.Poll.CIEvery.Duration {
		return
	}
	s.watchedAt = now
	if s.watchErr == nil {
		s.watchErr = emptyValue()
	}
	proposed := map[string]bool{}
	for _, r := range s.st.Ops {
		if r.Op != OpsInstall {
			continue
		}
		if !r.Started.IsZero() && r.Ended.IsZero() {
			return // one install under way at a time
		}
		proposed[r.Commit] = true
	}
	repo := s.cfg.Repositories[o.cfg.Ops.Watch]
	env, branch, auto := *o, o.cfg.Ops.branch(), o.cfg.Ops.AutoInstall
	console := o.cfg.Ops.Console
	last := s.watchErr
	s.spawnShared("ops-watch", func(w *Supervisor) {
		fail := func(why string) {
			why = w.withheld(cutUTF8(why, 300))
			if last.Swap(why) != why {
				w.emit("", Event{Kind: KindError, Error: &ErrorPayload{Step: "ops watch of " + branch, Error: why}})
			}
		}
		if _, err := env.fetchBranch(); err != nil {
			fail(err.Error())
			return
		}
		tip, _ := env.git(env.cfg.Ops.Repo, "rev-parse", "--verify", "--quiet", "origin/"+branch+"^{commit}")
		installed := strings.TrimSuffix(binaryCommit(env.binary), "+modified")
		if tip == "" || proposed[tip] || installed == tip {
			return
		}
		// Never back: the tip must come after the installed commit.
		if installed != "" {
			if _, err := env.git(env.cfg.Ops.Repo, "merge-base", "--is-ancestor", installed, tip); err != nil {
				fail(fmt.Sprintf("the installed commit %s is not an ancestor of %s's tip %s, so agentd proposes no install of it", short(installed), branch, short(tip)))
				return
			}
		}
		green, detail, err := w.commitCI(repo, tip)
		if err != nil {
			fail("the CI of " + short(tip) + " could not be read: " + err.Error())
			return
		}
		if !green {
			return
		}
		why := fmt.Sprintf("the CI of %s is green at %s (%s)", branch, short(tip), detail)
		c, err := env.proposeInstall(OpsChange{Op: OpsInstall, Why: why}, opsArgs{Commit: tip, Why: why})
		if err != nil {
			fail(err.Error())
			return
		}
		last.Store("")
		c.Auto, c.Summary, c.Why = auto, w.withheld(c.Summary), w.withheld(c.Why)
		at := w.clock.Now()
		id := opsID(at, c)
		if w.emit("", Event{Kind: KindOps, Mode: ModeShared, Ops: &OpsPayload{ID: id, Step: OpsProposed, Change: &c}}); w.halt != nil {
			return
		}
		text := fmt.Sprintf("agentd proposes installing %s from %s: %s. Answer yes or no on %s%s.", short(tip), branch, c.Why, entryOps, id)
		if auto {
			text = fmt.Sprintf("agentd installs %s from %s without a yes (ops.auto_install): %s. It keeps the running binary and puts it back if a check or a restart fails; the outcome follows here.", short(tip), branch, c.Why)
		}
		w.opsTell(console, "I"+id, text)
	})
}

// emptyValue is an atomic.Value holding "".
func emptyValue() *atomic.Value {
	v := &atomic.Value{}
	v.Store("")
	return v
}

// opsTell posts text to the ops console as agentd's, screened.
func (s *Supervisor) opsTell(console, id, text string) {
	m := &MessagePayload{ID: id, Console: console, Kind: "info", From: "agentd", Text: text}
	if i := s.confidentialMatch(text); i > 0 {
		m.Text, m.Withheld = "", i
	}
	s.emit("", Event{Kind: KindMessage, Mode: ModeShared, Message: m})
}

// installTold tells the ops console what came of an install made without
// a yes: what was installed and what changed, or why it was put back.
func (s *Supervisor) installTold(r *OpsRecord, step, outcome string) {
	branch := s.cfg.Ops.branch()
	text := fmt.Sprintf("agentd installed %s from %s (ops.auto_install): %s", short(r.Commit), branch, outcome)
	if step != OpsDone {
		text = fmt.Sprintf("agentd's install of %s from %s (ops.auto_install) failed: %s", short(r.Commit), branch, outcome)
	}
	if r.Diff != "" {
		text += "\n\nWhat changed:\n\n```\n" + strings.ReplaceAll(r.Diff, "```", "'''") + "\n```"
	}
	s.opsTell(s.cfg.Ops.Console, "E"+r.ID, text)
}

// CI conclusions a check run may end with and count as green.
var greenConclusions = map[string]bool{"success": true, "neutral": true, "skipped": true}

// commitCI says whether the forge of repo reports commit sha's CI green,
// and what it saw: every GitHub check run completed and passing, or, with
// none, its combined status a success; or the GitLab commit's last
// pipeline a success. An error is a forge that could not be asked.
func (s *Supervisor) commitCI(repo Repository, sha string) (bool, string, error) {
	if s.api == nil || repo.Project == "" {
		return false, "", errors.New("no forge to ask")
	}
	api := s.api.At(repo.Host)
	if repo.Forge == "gitlab" {
		resp, err := api.GitLab(fmt.Sprintf("/projects/%s/repository/commits/%s", url.PathEscape(repo.Project), sha))
		var v struct {
			LastPipeline *struct {
				Status string `json:"status"`
			} `json:"last_pipeline"`
		}
		if err := getJSON(resp, err, "the forge", &v); err != nil {
			return false, "", err
		}
		if v.LastPipeline == nil {
			return false, "no pipeline", nil
		}
		return v.LastPipeline.Status == "success", "its pipeline is " + v.LastPipeline.Status, nil
	}
	resp, err := api.GitHub(fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=100", repo.Project, sha), nil)
	var v struct {
		Total int `json:"total_count"`
		Runs  []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := getJSON(resp, err, "the forge", &v); err != nil {
		return false, "", err
	}
	if v.Total == 0 {
		resp, err := api.GitHub(fmt.Sprintf("/repos/%s/commits/%s/status", repo.Project, sha), nil)
		var st struct {
			State string `json:"state"`
			Total int    `json:"total_count"`
		}
		if err := getJSON(resp, err, "the forge", &st); err != nil {
			return false, "", err
		}
		if st.Total == 0 {
			return false, "no check runs and no statuses", nil
		}
		return st.State == "success", fmt.Sprintf("its %d statuses are %s", st.Total, st.State), nil
	}
	if len(v.Runs) < v.Total {
		return false, fmt.Sprintf("%d check runs, more than the %d read", v.Total, len(v.Runs)), nil
	}
	for _, r := range v.Runs {
		if r.Status != "completed" || !greenConclusions[r.Conclusion] {
			return false, fmt.Sprintf("%s is %s %s", r.Name, r.Status, r.Conclusion), nil
		}
	}
	names := make([]string, 0, len(v.Runs))
	for _, r := range v.Runs {
		names = append(names, r.Name)
	}
	b, _ := json.Marshal(names)
	return true, fmt.Sprintf("%d check runs passed: %s", len(v.Runs), cutUTF8(string(b), 200)), nil
}
