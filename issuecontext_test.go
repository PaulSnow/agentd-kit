package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// issue_body quotes the owner's words from their records, lists the facts
// and records, and gives each topic's decisions so far from its card; an
// id history lacks is refused.
func TestIssueBody(t *testing.T) {
	h := caseHarness(t)
	h.appendEvent(Event{Kind: KindInstruction, Mode: ModeOwner, Instruction: &InstructionPayload{ID: "I4c94", Repositories: []string{"widgets"}, Text: "Why would the go code not\ncreate a session?", From: "owner"}})
	h.appendEvent(Event{Kind: KindCI, Mode: ModeLive, Session: "s1", CI: &CIPayload{Repository: "widgets", Ref: "!12", State: "failed", Cause: "test"}})
	ci := onTopic(h, "!12", "failed")
	h.appendEvent(Event{Kind: KindCard, Mode: ModeLive, Card: &CardPayload{Repository: "widgets", Topic: "#7", Card: &Card{Topic: "#7", Kind: CardIssue, Status: "open", Summary: "s", Rev: 1,
		InForce: []CardRule{{ID: "R1", Rule: "replies are routed, never only recorded", Src: []string{"I4c94"}}},
		Open:    []CardQuestion{{ID: "Q2", Question: "Merge the follow-up now?", Options: []string{"yes", "no"}, Src: []string{ci}}}}}})
	srv := h.queryServer()
	rs := session(t, srv, toolCall(1, ToolIssueBody, map[string]any{"title": "Replies are lost", "owner_records": []string{"I4c94"},
		"facts": []string{"the reply sat unread for 2 hours"}, "record_ids": []string{ci}, "topics": []string{"#7"}}))
	got := replyJSON(t, rs[0])
	body := got["body"].(string)
	for _, want := range []string{"Filed by an agent", "> Why would the go code not\n> create a session?\n", "— instruction `I4c94`", "- the reply sat unread for 2 hours",
		"- `" + ci + "` ci", "**#7**, in force, `R1`: replies are routed, never only recorded (from `I4c94`)", "**#7**, open, `Q2`: Merge the follow-up now? (yes / no)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body lacks %q:\n%s", want, body)
		}
	}
	rs = session(t, srv, toolCall(2, ToolIssueBody, map[string]any{"title": "x", "facts": []string{}, "record_ids": []string{"Nope1"}}))
	if !rs[0].Result.IsError || !strings.Contains(rs[0].Result.Content[0].Text, "no record Nope1") {
		t.Fatalf("unknown id: %+v", rs[0])
	}
}

// A thread's outcome is posted on its issue as a labelled comment when the
// repository allows comments, and logged; not otherwise, nor in a shadow
// run, nor when it matches a confidential pattern.
func TestPostOutcome(t *testing.T) {
	h := caseHarness(t)
	o := ThreadOutcome{Repository: "widgets", Ref: "#7", Kind: OutcomeDecision, Text: "Merge the follow-up once CI is green.", Thread: "T91c2", Records: []string{"A3"}}
	if err := h.sup.PostOutcome(o); err == nil || !strings.Contains(err.Error(), "does not allow comment") {
		t.Fatalf("without comment allowed: %v", err)
	}
	r := h.cfg.Repositories["widgets"]
	r.Policy.Delivery.Allow = append(r.Policy.Delivery.Allow, ActComment)
	h.cfg.Repositories["widgets"] = r
	h.sup.forge.Repos = h.cfg.Repositories
	if err := h.sup.PostOutcome(o); err != nil {
		t.Fatal(err)
	}
	want := "#7 **Decision** (written by an agent, from console thread `T91c2`, not by the owner): Merge the follow-up once CI is green.\n\nSources: `A3`"
	if len(h.forge.comments) != 1 || h.forge.comments[0] != want {
		t.Fatalf("comments %q", h.forge.comments)
	}
	// The refused attempt is logged too: its intent and its error.
	if h.count(KindAction, ActComment, "") != 1 || h.count(KindIntent, ActComment, "") != 2 || h.count(KindError, ActComment, "") != 1 {
		t.Fatal("the comment is not logged")
	}
	for _, bad := range []ThreadOutcome{{Repository: "widgets", Ref: "7", Kind: OutcomeResult, Text: "x", Thread: "T1"}, {Repository: "widgets", Ref: "#7", Kind: "maybe", Text: "x", Thread: "T1"},
		{Repository: "widgets", Ref: "#7", Kind: OutcomeResult, Text: "", Thread: "T1"}, {Repository: "widgets", Ref: "#7", Kind: OutcomeResult, Text: "x"}} {
		if h.sup.PostOutcome(bad) == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	h.sup.confidential = append(h.sup.confidential, regexp.MustCompile(`secret-[0-9]+`))
	if err := h.sup.PostOutcome(ThreadOutcome{Repository: "widgets", Ref: "#7", Kind: OutcomeResult, Text: "secret-9", Thread: "T2"}); err == nil || len(h.forge.comments) != 1 {
		t.Fatalf("a confidential outcome: %v", err)
	}
	h.restart(true)
	h.sup.forge.Repos = h.cfg.Repositories
	if err := h.sup.PostOutcome(o); err != nil || len(h.forge.comments) != 1 || h.count(KindWould, ActComment, "") != 1 {
		t.Fatalf("shadow: %v %d", err, len(h.forge.comments))
	}
}

// The forge comment goes through gh's and glab's API with the body as a
// raw field.
func TestExecForgeComment(t *testing.T) {
	dir := t.TempDir()
	for _, cli := range []string{"gh", "glab"} {
		shim(t, cli, fmt.Sprintf(`printf '%%s\n' "$@" > %q`, filepath.Join(dir, cli)))
	}
	f := ExecForge{Timeout: 5 * time.Second}
	if err := f.Comment(Repository{Forge: "github", Project: "example-org/widgets"}, "#7", "@file done"); err != nil {
		t.Fatal(err)
	}
	if err := f.Comment(Repository{Forge: "gitlab", Host: "gitlab.example.com", Project: "example-org/widgets"}, "!12", "done"); err != nil {
		t.Fatal(err)
	}
	gh, _ := os.ReadFile(filepath.Join(dir, "gh"))
	glab, _ := os.ReadFile(filepath.Join(dir, "glab"))
	if string(gh) != "api\n--method\nPOST\nrepos/example-org/widgets/issues/7/comments\n-f\nbody=@file done\n" ||
		string(glab) != "api\n--hostname\ngitlab.example.com\n--method\nPOST\nprojects/example-org%2Fwidgets/merge_requests/12/notes\n-f\nbody=done\n" {
		t.Fatalf("gh %q glab %q", gh, glab)
	}
}
