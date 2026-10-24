package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const mailFixture = `{"asOf": "2026-10-23T20:00:00-05:00", "since": "2026-10-16T20:00:00-05:00", "account": "owner@example.com", "searched": 12,
 "findings": [
  {"repo": "widgets", "subject": "Can the widgets ship on Friday?", "from": "user-c@example.com", "date": "2026-10-22", "threadId": "t1", "summary": "Asks whether the release can ship.", "suggested": "Reply with the date", "related": [17]},
  {"repo": "nowhere", "subject": "Weekly numbers", "from": "user-d@example.com", "date": "2026-10-21", "threadId": "t2", "summary": "The weekly report.", "suggested": "", "related": null}
 ]}`

// itemsFixture is the tracking fixture plus a mail findings file, and a
// supervisor-free state to refresh into.
func itemsFixture(t *testing.T) (Tracking, *Config) {
	tr := trackingFixture(t)
	tr.Mail = filepath.Join(filepath.Dir(tr.Snapshot), "mail-findings.json")
	writeFile(t, tr.Mail, mailFixture)
	cfg := DefaultConfig()
	cfg.Tracking = tr
	return tr, &cfg
}

// refresh applies one refresh to st, as the log would, and returns the
// events it made.
func refresh(t *testing.T, st *LogState, tr Tracking) ([]Event, error) {
	t.Helper()
	events, err := RefreshItems(st, tr, time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC))
	for _, e := range events {
		if cerr := e.check(); cerr != nil {
			t.Fatalf("refresh made an unreadable event: %v", cerr)
		}
		st.apply(e)
	}
	return events, err
}

// laterSnapshot rewrites the fixture snapshot as the next one: an hour
// later, with issue 19 filed and issue 18 commented on in between.
func laterSnapshot(t *testing.T, tr Tracking) {
	b, err := os.ReadFile(tr.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), `"AsOf": "2026-10-23T21:23:04.123456789-05:00"`, `"AsOf": "2026-10-23T22:23:04-05:00"`, 1)
	s = strings.Replace(s, `"UpdatedAt": "2026-10-21T10:00:00Z"`, `"UpdatedAt": "2026-10-24T03:00:00Z"`, 1)
	s = strings.Replace(s, `"Issues": [`, `"Issues": [
        {"Number": 19, "Title": "Widgets squeak", "Description": "", "State": "open", "Author": "user-c", "Labels": [], "CreatedAt": "2026-10-24T02:50:00Z", "UpdatedAt": "2026-10-24T02:50:00Z", "ClosedAt": null},`, 1)
	writeFile(t, tr.Snapshot, s)
}

// The first snapshot is the baseline; an issue filed before the next one
// becomes exactly one item, a comment on an open issue a discussion, and
// reading the same snapshot again adds nothing. Mail findings are items
// for the owner only.
func TestNewIssueBetweenSnapshotsIsOneItem(t *testing.T) {
	tr, cfg := itemsFixture(t)
	st := NewLogState(cfg, ModeLive)
	if _, err := refresh(t, st, tr); err != nil {
		t.Fatal(err)
	}
	for id, it := range st.Items {
		if !strings.HasPrefix(id, "mail:") {
			t.Fatalf("the baseline snapshot made item %+v", it)
		}
	}
	laterSnapshot(t, tr)
	if _, err := refresh(t, st, tr); err != nil {
		t.Fatal(err)
	}
	issue := st.Items["gitlab/example-org/widgets#19"]
	if issue == nil || issue.Kind != ItemIssue || issue.Title != "Widgets squeak" || issue.Author != "user-c" || issue.OwnerOnly ||
		issue.Link != "https://gitlab.com/example-org/widgets/-/issues/19" || issue.FirstSeen.IsZero() {
		t.Fatalf("issue 19 = %+v", issue)
	}
	discussions := 0
	for _, it := range st.Items {
		if it.Kind == ItemDiscussion {
			discussions++
			if !strings.HasPrefix(it.ID, "gitlab/example-org/widgets#18@") {
				t.Errorf("discussion %+v", it)
			}
		}
	}
	if discussions != 1 {
		t.Fatalf("%d discussions, want issue 18's", discussions)
	}
	n := len(st.Items)
	if events, err := refresh(t, st, tr); err != nil || len(events) != 0 || len(st.Items) != n {
		t.Fatalf("the same snapshot again: %d events, %d items, %v", len(events), len(st.Items), err)
	}
	q, r := st.Items["mail:t1"], st.Items["mail:t2"]
	if q == nil || q.Kind != ItemMailQuestion || !q.OwnerOnly || q.Repository != "gitlab/example-org/widgets" {
		t.Fatalf("mail question = %+v", q)
	}
	if r == nil || r.Kind != ItemMailReport || !r.OwnerOnly || r.Repository != "" {
		t.Fatalf("mail report = %+v", r)
	}
}

// Items, cursors and the owner's mark-seen live in the log: replay
// rebuilds them, so a restart neither repeats an item nor forgets that the
// owner saw it.
func TestMarkSeenSurvivesReplay(t *testing.T) {
	tr, cfg := itemsFixture(t)
	var buf bytes.Buffer
	log := NewLogWriter(&buf)
	st := NewLogState(cfg, ModeLive)
	for _, step := range []func(){func() {}, func() { laterSnapshot(t, tr) }} {
		step()
		events, err := RefreshItems(st, tr, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if err := log.Append(e); err != nil {
				t.Fatal(err)
			}
			st.apply(e)
		}
	}
	if err := MarkSeen(log, time.Now(), []string{"gitlab/example-org/widgets#19"}, ""); err != nil {
		t.Fatal(err)
	}
	events, _, err := ReadEvents(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{ModeLive, ModeShadow} {
		replayed := NewLogState(cfg, mode).Fold(events)
		if it := replayed.Items["gitlab/example-org/widgets#19"]; it == nil || !it.Seen {
			t.Fatalf("%s: issue 19 after replay = %+v", mode, it)
		}
		if again, err := RefreshItems(replayed, tr, time.Now()); err != nil || len(again) != 0 {
			t.Fatalf("%s: a refresh after replay made %d events, %v", mode, len(again), err)
		}
	}
	if err := MarkSeen(log, time.Now(), nil, ""); err == nil {
		t.Fatal("marked nothing seen")
	}
}

// A mail findings file that cannot be read is an error naming it, and the
// items already listed stay: a bad file never empties the list. The
// snapshot is still read.
func TestMalformedMailNeverEmptiesTheList(t *testing.T) {
	tr, cfg := itemsFixture(t)
	st := NewLogState(cfg, ModeLive)
	if _, err := refresh(t, st, tr); err != nil {
		t.Fatal(err)
	}
	before := len(st.Items)
	laterSnapshot(t, tr)
	for _, bad := range []string{`{"asOf": `, `{"asOf": "2026-10-25T00:00:00Z", "findings": [{"threadId": "t9"}]}`, `{"asOf": "2026-10-25T00:00:00Z", "findings": null}`, `[]`} {
		writeFile(t, tr.Mail, bad)
		_, err := refresh(t, st, tr)
		if err == nil || !strings.Contains(err.Error(), tr.Mail) {
			t.Fatalf("%s: error %v, want one naming %s", bad, err, tr.Mail)
		}
		if st.Items["mail:t1"] == nil || len(st.Items) < before {
			t.Fatalf("%s: items went from %d to %d", bad, before, len(st.Items))
		}
	}
	if st.Items["gitlab/example-org/widgets#19"] == nil {
		t.Fatal("the snapshot was not read beside the bad mail file")
	}
}

// agentd items lists the items, refreshing them first; --new leaves out
// those marked seen; --seen marks items seen.
func TestItemsCommand(t *testing.T) {
	tr, _ := itemsFixture(t)
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.jsonl")
	cfgPath := filepath.Join(dir, "agentd.json")
	writeFile(t, cfgPath, `{"event_log": "`+logPath+`", "transcript_dir": "", "tracking": {"manifest": "`+tr.Manifest+`", "snapshot": "`+tr.Snapshot+`", "mail": "`+tr.Mail+`"}}`)
	var out bytes.Buffer
	if err := itemsCmd([]string{"--config", cfgPath}, &out); err != nil {
		t.Fatal(err)
	}
	laterSnapshot(t, tr)
	out.Reset()
	if err := itemsCmd([]string{"--config", cfgPath}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Widgets squeak", "Can the widgets ship on Friday?", "mail_question", "owner only"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("items lack %q:\n%s", want, out.String())
		}
	}
	if err := itemsCmd([]string{"--config", cfgPath, "--seen", "gitlab/example-org/widgets#19,mail:t1"}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := itemsCmd([]string{"--config", cfgPath, "--new"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Widgets squeak") || strings.Contains(out.String(), "ship on Friday") || !strings.Contains(out.String(), "Weekly numbers") {
		t.Fatalf("--new after --seen:\n%s", out.String())
	}
	if err := itemsCmd([]string{"--config", cfgPath, "--seen", "nosuch#1"}, &out); err == nil {
		t.Fatal("marked an unknown item seen")
	}
	for _, args := range [][]string{{"--new", "--seen", "mail:t2"}, {"extra"}} {
		if err := itemsCmd(append([]string{"--config", cfgPath}, args...), &out); err == nil {
			t.Errorf("items %v accepted", args)
		}
	}
}

// An item seen again under a moved cursor is not listed twice: a mail file
// rewritten with a later asOf and the same thread, and two refreshes made
// from the same state, as when the command and a running agentd race.
func TestItemSeenAgainIsNotDuplicated(t *testing.T) {
	tr, cfg := itemsFixture(t)
	st := NewLogState(cfg, ModeLive)
	if _, err := refresh(t, st, tr); err != nil {
		t.Fatal(err)
	}
	writeFile(t, tr.Mail, strings.Replace(mailFixture, `"asOf": "2026-10-23T20:00:00-05:00"`, `"asOf": "2026-10-23T23:00:00-05:00"`, 1))
	events, err := refresh(t, st, tr)
	if err != nil || len(events) != 1 || events[0].Kind != KindCursor {
		t.Fatalf("the same threads again: %+v, %v", events, err)
	}
	laterSnapshot(t, tr)
	a, errA := RefreshItems(st, tr, time.Now())
	b, errB := RefreshItems(st, tr, time.Now())
	if errA != nil || errB != nil || len(a) == 0 || len(a) != len(b) {
		t.Fatalf("two refreshes: %d and %d events, %v %v", len(a), len(b), errA, errB)
	}
	n := len(st.Items)
	for _, e := range append(a, b...) {
		st.apply(e)
	}
	if got := len(st.Items) - n; got != len(a)-1 { // one of a's events is the cursor
		t.Fatalf("%d items from two racing refreshes, want %d", got, len(a)-1)
	}
	// A cursor never moves back: an older snapshot, restored, adds nothing.
	writeFile(t, tr.Snapshot, strings.Replace(mustRead(t, tr.Snapshot), `"AsOf": "2026-10-23T22:23:04-05:00"`, `"AsOf": "2026-10-22T22:23:04-05:00"`, 1))
	if events, err := refresh(t, st, tr); err != nil || len(events) != 0 {
		t.Fatalf("an older snapshot: %+v, %v", events, err)
	}
}

// Mail waits while the tracking files cannot be read: its items would
// have no repositories, and keep none for good.
func TestMailWaitsForTheInventory(t *testing.T) {
	tr, cfg := itemsFixture(t)
	writeFile(t, tr.Snapshot, `{`)
	st := NewLogState(cfg, ModeLive)
	if _, err := refresh(t, st, tr); err == nil || !strings.Contains(err.Error(), tr.Snapshot) {
		t.Fatalf("error = %v", err)
	}
	if len(st.Items) != 0 || !st.Cursors[SourceMail].IsZero() {
		t.Fatalf("items %v, mail cursor %v", st.Items, st.Cursors)
	}
}

// Items and cursors are shared facts and seen marks the owner's; no other
// mode is accepted for them, and shared for nothing else.
func TestItemEventModes(t *testing.T) {
	at := time.Now()
	for _, e := range []Event{
		{V: EventVersion, Time: at, Kind: KindItem, Mode: ModeLive, Item: &ItemPayload{ID: "x"}},
		{V: EventVersion, Time: at, Kind: KindCursor, Mode: ModeOwner, Cursor: &CursorPayload{Source: SourceMail}},
		{V: EventVersion, Time: at, Kind: KindSeen, Mode: ModeShared, Seen: &SeenPayload{IDs: []string{"x"}}},
		{V: EventVersion, Time: at, Kind: KindStop, Mode: ModeShared, Stop: &StopPayload{}},
	} {
		if err := e.check(); err == nil {
			t.Errorf("accepted a %s event in mode %s", e.Kind, e.Mode)
		}
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
