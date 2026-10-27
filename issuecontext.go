package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Issues carry their own context (#60 §4). An agent filing an issue asks
// Go for its body, which quotes the owner's words from their records,
// lists what was observed and the records it rests on, and gives the
// decisions so far from the cards. A console thread (#59) that ends with a
// decision or an outcome about an issue has Go post it there as a short
// comment, labelled as an agent's; the issue's thread then holds its own
// history, which the next read of it brings into the index.

// ToolIssueBody is the context tool that writes an issue's body.
const ToolIssueBody = "issue_body"

// issueBodyMax bounds an issue body Go writes.
const issueBodyMax = 12000

// issueBodyArgs are issue_body's arguments.
type issueBodyArgs struct {
	Title  string   `json:"title"`
	Owner  []string `json:"owner_records"`
	Facts  []string `json:"facts"`
	IDs    []string `json:"record_ids"`
	Topics []string `json:"topics"`
}

func issueBodySchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"title":{"type":"string"},` +
		`"owner_records":{"type":"array","items":{"type":"string"},"description":"ids of the records holding the owner's words, quoted whole"},` +
		`"facts":{"type":"array","items":{"type":"string"},"description":"what was observed, one fact each"},` +
		`"record_ids":{"type":"array","items":{"type":"string"},"description":"the other records the issue rests on"},` +
		`"topics":{"type":"array","items":{"type":"string"},"description":"the topics whose decisions so far the body lists, #N, !N or session:NAME"}},` +
		`"required":["title","facts"],"additionalProperties":false}`)
}

// issueBody writes an issue's body from the store: the owner's words
// quoted, the facts, the records with their ids, and each topic's rules
// in force and open questions, each with its sources. It refuses ids the
// history does not hold, so a body never cites what cannot be read.
func issueBody(cs *ContextStore, repo string, a issueBodyArgs, withheld func(string) string) (string, []problem) {
	h := cs.History(repo)
	var problems []problem
	if strings.TrimSpace(a.Title) == "" {
		problems = append(problems, problem{format: "title is empty"})
	}
	if len(a.Owner)+len(a.IDs) > historyIDs || len(a.Topics) > expertCards || len(a.Facts) > 30 {
		problems = append(problems, problem{format: "at most %d record ids, %d topics and 30 facts", args: []any{historyIDs, expertCards}})
	}
	for _, id := range append(append([]string{}, a.Owner...), a.IDs...) {
		if h.Record(id) == nil {
			problems = append(problems, problem{format: "history holds no record %s", args: []any{id}})
		}
	}
	if len(problems) > 0 {
		return "", problems
	}
	var b strings.Builder
	b.WriteString("_Filed by an agent with agentd's repository context (#60). The owner's words are quoted from their records; the rest is the agent's._\n")
	if len(a.Owner) > 0 {
		b.WriteString("\n## The owner's words\n")
		for _, id := range a.Owner {
			r := h.Record(id)
			b.WriteString("\n")
			for _, line := range strings.Split(strings.TrimSpace(cutUTF8(r.Text, 3000)), "\n") {
				b.WriteString("> " + line + "\n")
			}
			fmt.Fprintf(&b, ">\n> — %s `%s`, %s\n", r.Kind, r.ID, r.At.UTC().Format("2006-01-02 15:04Z"))
		}
	}
	b.WriteString("\n## What was observed\n\n")
	for _, f := range a.Facts {
		b.WriteString("- " + oneLine(withheld(f), 600) + "\n")
	}
	if len(a.IDs) > 0 {
		b.WriteString("\n## Records\n\n")
		for _, id := range a.IDs {
			r := h.Record(id)
			fmt.Fprintf(&b, "- `%s` %s, %s: %s\n", r.ID, r.Kind, r.At.UTC().Format("2006-01-02 15:04Z"), r.summary())
		}
	}
	var decisions []string
	for _, t := range a.Topics {
		c := cs.Card(repo, t)
		if c == nil {
			continue
		}
		for _, r := range c.InForce {
			decisions = append(decisions, fmt.Sprintf("- **%s**, in force, `%s`: %s (from %s)", t, r.ID, oneLine(r.Rule, 400), codeList(r.Src)))
		}
		for _, q := range c.Open {
			line := fmt.Sprintf("- **%s**, open, `%s`: %s", t, q.ID, oneLine(q.Question, 400))
			if len(q.Options) > 0 {
				line += " (" + strings.Join(q.Options, " / ") + ")"
			}
			decisions = append(decisions, line+" (from "+codeList(q.Src)+")")
		}
	}
	if len(decisions) > 0 {
		b.WriteString("\n## Decisions so far\n\n" + strings.Join(decisions, "\n") + "\n")
	}
	return cutUTF8(b.String(), issueBodyMax), nil
}

func codeList(ids []string) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = "`" + id + "`"
	}
	return strings.Join(out, ", ")
}

// ThreadOutcome is how a console thread (#59) ended about an issue or a
// request: a decision taken, or the outcome of the work it asked for.
type ThreadOutcome struct {
	Repository string   `json:"repository"`
	Ref        string   `json:"ref"`  // #N or !N
	Kind       string   `json:"kind"` // OutcomeDecision or OutcomeResult
	Text       string   `json:"text"`
	Thread     string   `json:"thread"`
	Records    []string `json:"records,omitempty"` // the records it rests on
}

// Thread outcome kinds.
const (
	OutcomeDecision = "decision"
	OutcomeResult   = "outcome"
)

// outcomeMax bounds an outcome's text: the comment is short.
const outcomeMax = 1500

// outcomeComment is the comment an outcome is posted as.
func outcomeComment(o ThreadOutcome) string {
	label := "Outcome"
	if o.Kind == OutcomeDecision {
		label = "Decision"
	}
	body := fmt.Sprintf("**%s** (written by an agent, from console thread `%s`, not by the owner): %s", label, o.Thread, strings.TrimSpace(o.Text))
	if len(o.Records) > 0 {
		body += "\n\nSources: " + codeList(o.Records)
	}
	return body
}

// ActComment is a comment agentd posted on an issue or a request.
const ActComment = "comment"

// PostOutcome posts a thread's outcome on its issue or request, where the
// repository's delivery allows comments, and logs it: intent, then the
// action or the error. A shadow run logs what it would post. An outcome
// whose words match a confidential pattern is refused.
func (s *Supervisor) PostOutcome(o ThreadOutcome) error {
	switch {
	case o.Kind != OutcomeDecision && o.Kind != OutcomeResult:
		return fmt.Errorf("an outcome is a %s or an %s, not %q", OutcomeDecision, OutcomeResult, o.Kind)
	case len(o.Ref) < 2 || o.Ref[0] != '#' && o.Ref[0] != '!' || strings.Trim(o.Ref[1:], "0123456789") != "":
		return fmt.Errorf("%q is not an issue or request reference", o.Ref)
	case strings.TrimSpace(o.Text) == "" || len(o.Text) > outcomeMax:
		return fmt.Errorf("an outcome's text is 1 to %d bytes", outcomeMax)
	case o.Thread == "":
		return errors.New("an outcome names its thread")
	}
	body := outcomeComment(o)
	if i := s.confidentialMatch(body); i > 0 {
		return fmt.Errorf("the outcome matches confidential pattern %d: post it by hand, if at all", i)
	}
	a := &ActPayload{Repository: o.Repository, Ref: o.Ref, Key: "outcome:" + o.Thread + ":" + o.Ref, Message: body}
	return s.act("", ActComment, a, func() error { return s.forge.CommentAllowed(o.Repository, o.Ref, body) })
}

// CommentAllowed comments on an issue or request if the repository's
// delivery allows comments.
func (a AllowedForge) CommentAllowed(repo, ref, body string) error {
	if err := a.allowed(repo, ActComment); err != nil {
		return err
	}
	return a.Forge.Comment(a.Repos[repo], ref, body)
}

// Comment posts body on ref through the forge's API: an issue's or a
// pull request's comments on GitHub, an issue's or merge request's notes
// on GitLab.
func (f ExecForge) Comment(repo Repository, ref, body string) error {
	n, err := strconv.Atoi(strings.TrimLeft(ref, "#!"))
	if err != nil {
		return fmt.Errorf("not a reference: %q", ref)
	}
	// -f sends the body as it is: a raw field, never read from a file.
	var args []string
	switch repo.Forge {
	case "github":
		args = glabArgs(repo, "--method", "POST", fmt.Sprintf("repos/%s/issues/%d/comments", repo.Project, n), "-f", "body="+body)
		_, err = runTimed(f.Timeout, "", nil, true, "gh", args...)
	case "gitlab":
		kind := "issues"
		if ref[0] == '!' {
			kind = "merge_requests"
		}
		args = glabArgs(repo, "--method", "POST", fmt.Sprintf("projects/%s/%s/%d/notes", url.PathEscape(repo.Project), kind, n), "-f", "body="+body)
		_, err = runTimed(f.Timeout, "", nil, true, "glab", args...)
	default:
		return fmt.Errorf("unknown forge %q", repo.Forge)
	}
	if err != nil {
		return fmt.Errorf("comment on %s: %s", ref, exitMessage(err))
	}
	return nil
}
