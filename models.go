package main

import "strings"

// orModel is a repository's model for a kind of session, or else every
// repository's.
func orModel(repo, all string) string {
	if repo != "" {
		return repo
	}
	return all
}

// allowedModel is m, or, when the policy's sessions.models leaves it out,
// the list's first: a repository's own file may narrow the list past a
// model the owner chose, and the narrowing holds. "" stays the command's
// default.
func allowedModel(p RepoPolicy, m string) string {
	if m == "" || len(p.Sessions.Models) == 0 || contains(p.Sessions.Models, m) {
		return m
	}
	return p.Sessions.Models[0]
}

// sessionModel is the model a session runs (#52): its repository's
// policy's for its kind, or else policies.models'. A reviewer or a judge,
// started or configured, runs the reviewer or judge model; any other
// session is a work session. "" is the restart command's own default.
func sessionModel(cfg *Config, st *LogState, sess Session) string {
	var p RepoPolicy
	if sess.Repository != "" {
		p, _ = effectivePolicy(cfg, st, sess.Repository)
	}
	g := cfg.Policies.Models
	switch sess.Role {
	case RoleReviewer:
		return allowedModel(p, orModel(p.Review.ReviewerModel, g.Reviewer))
	case RoleJudge:
		return allowedModel(p, orModel(p.Review.JudgeModel, g.Judge))
	}
	return allowedModel(p, orModel(p.Sessions.WorkModel, g.Work))
}

// withModel substitutes {model} in a launch command. With no model, a
// "--model {model}" pair or a "--model={model}" is left out, so the
// command's own default runs; any other {model} becomes "", which keeps
// the arguments after it in place.
func withModel(cmd []string, model string) []string {
	out := make([]string, 0, len(cmd))
	for _, part := range cmd {
		switch {
		case model != "" || !strings.Contains(part, "{model}"):
			out = append(out, strings.ReplaceAll(part, "{model}", model))
		case part == "--model={model}":
		case part == "{model}" && len(out) > 0 && out[len(out)-1] == "--model":
			out = out[:len(out)-1]
		default:
			out = append(out, strings.ReplaceAll(part, "{model}", ""))
		}
	}
	return out
}

// modelsSet names a model setting the config has, or is "" when every
// session runs the command's default.
func (c *Config) modelsSet() string {
	if g := c.Policies.Models; g != (ModelsPolicy{}) {
		return "policies.models"
	}
	for _, n := range sortedKeys(c.Repositories) {
		p := c.Repositories[n].Policy
		if p.Sessions.WorkModel != "" || p.Review.ReviewerModel != "" || p.Review.JudgeModel != "" {
			return "repositories." + n + ".policy's model"
		}
	}
	return ""
}
