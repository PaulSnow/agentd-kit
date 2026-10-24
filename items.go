package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Item kinds: what changed that the owner may want to know.
const (
	ItemIssue        = "issue"
	ItemMergeRequest = "merge_request"
	ItemDiscussion   = "discussion" // an open issue or request updated since the owner last looked
	ItemMailQuestion = "mail_question"
	ItemMailReport   = "mail_report"
)

// MailFindings is the tracking system's mail findings file: the threads
// in the owner's mail that hold questions or reports about the work.
type MailFindings struct {
	AsOf     time.Time     `json:"asOf"`
	Since    time.Time     `json:"since"`
	Account  string        `json:"account"`
	Searched int           `json:"searched"`
	Findings []MailFinding `json:"findings"`
}

// MailFinding is one mail thread the tracking system found.
type MailFinding struct {
	Repo      string `json:"repo"`
	Subject   string `json:"subject"`
	From      string `json:"from"`
	Date      string `json:"date"`
	ThreadID  string `json:"threadId"`
	Summary   string `json:"summary"`
	Suggested string `json:"suggested"`
	Related   []int  `json:"related"`
}

// ItemPayload is one new item, in #7's shape: its id, which stays the same
// however often it is seen, its kind, repository (platform/path, or "" for
// mail that names none), title, link, when the source says it happened,
// who wrote it, and whether only the owner may see it. Everything derived
// from mail is for the owner only.
type ItemPayload struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Repository string    `json:"repository,omitempty"`
	Title      string    `json:"title"`
	Link       string    `json:"link,omitempty"`
	SourceTime time.Time `json:"source_time"`
	Author     string    `json:"author,omitempty"`
	OwnerOnly  bool      `json:"owner_only"`
}

// SeenPayload is the owner marking items seen.
type SeenPayload struct {
	IDs []string `json:"ids"`
	// From is the address the page was asked from, when it was.
	From string `json:"from,omitempty"`
}

// CursorPayload is how far a source has been read: the snapshot's AsOf,
// or the mail findings', so a restart neither misses nor repeats items.
type CursorPayload struct {
	Source string    `json:"source"`
	AsOf   time.Time `json:"as_of"`
}

// Item is an item as the log holds it: when agentd first saw it, and
// whether the owner has marked it seen.
type Item struct {
	ItemPayload
	FirstSeen time.Time
	Seen      bool
	// Triage is the item's latest triage; it holds for the item's text
	// while its Hash is the item's. TriageFailed is when a triage of it
	// last failed.
	Triage       *TriageResult
	TriageFailed time.Time
}

// itemLink is the forge page for an issue (or a request when mr is set).
// The tracking files name the platform, not the host, so a repository on a
// self-hosted forge gets a link to the public one: a known gap.
func itemLink(platform, path string, number int, mr bool) string {
	switch {
	case platform == "gitlab" && mr:
		return fmt.Sprintf("https://gitlab.com/%s/-/merge_requests/%d", path, number)
	case platform == "gitlab":
		return fmt.Sprintf("https://gitlab.com/%s/-/issues/%d", path, number)
	case mr:
		return fmt.Sprintf("https://github.com/%s/pull/%d", path, number)
	}
	return fmt.Sprintf("https://github.com/%s/issues/%d", path, number)
}

// SnapshotItems are the items an inventory's snapshot holds that are newer
// than since, the AsOf of the snapshot read before it: issues and requests
// created since, and open ones updated since, as discussions, for the
// repositories in the manifest only. A zero since is the first snapshot,
// which is only a baseline: it yields nothing. A discussion names no
// author: the snapshot knows who opened the issue, not who changed it.
func SnapshotItems(inv *Inventory, since time.Time) []ItemPayload {
	if since.IsZero() {
		return nil
	}
	var out []ItemPayload
	for _, r := range inv.snapshot.Repos {
		key := repoKey(r.Info.Platform, r.Info.FullPath)
		if inv.byKey[key] == nil {
			continue // the manifest is the list
		}
		add := func(kind, sigil string, number int, title, author string, created, updated time.Time, open, mr bool) {
			switch {
			case created.After(since):
				out = append(out, ItemPayload{ID: fmt.Sprintf("%s%s%d", key, sigil, number), Kind: kind, Repository: key, Title: title,
					Link: itemLink(r.Info.Platform, r.Info.FullPath, number, mr), SourceTime: created, Author: author})
			case open && updated.After(since):
				out = append(out, ItemPayload{ID: fmt.Sprintf("%s%s%d@%s", key, sigil, number, updated.UTC().Format(time.RFC3339)), Kind: ItemDiscussion,
					Repository: key, Title: title, Link: itemLink(r.Info.Platform, r.Info.FullPath, number, mr), SourceTime: updated})
			}
		}
		for _, is := range r.Issues {
			add(ItemIssue, "#", is.Number, is.Title, is.Author, is.CreatedAt, is.UpdatedAt, workStates[is.State], false)
		}
		for _, mr := range r.MRs {
			add(ItemMergeRequest, "!", mr.Number, mr.Title, mr.Author, mr.CreatedAt, mr.UpdatedAt, workStates[mr.State], true)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// MailItems are the mail findings as items, every one for the owner only.
// A finding whose summary or subject asks something is a question; any
// other is a report. Its repository is the manifest repository of the
// finding's name, or none when no one repository has that name. An item
// keeps the kind and title it was first seen with.
func MailItems(m *MailFindings, inv *Inventory) ([]ItemPayload, error) {
	var out []ItemPayload
	for i, f := range m.Findings {
		if f.ThreadID == "" || f.Subject == "" {
			return nil, fmt.Errorf("finding %d: threadId and subject must be set", i)
		}
		at, err := time.Parse("2006-01-02", f.Date)
		if err != nil {
			return nil, fmt.Errorf("finding %d: date %q is not YYYY-MM-DD", i, f.Date)
		}
		kind := ItemMailReport
		if strings.Contains(f.Summary, "?") || strings.Contains(f.Subject, "?") {
			kind = ItemMailQuestion
		}
		repo, matches := "", 0
		for _, r := range inv.Repos {
			if strings.EqualFold(r.Name, f.Repo) {
				repo, matches = r.Key, matches+1
			}
		}
		if matches != 1 {
			repo = ""
		}
		out = append(out, ItemPayload{ID: "mail:" + f.ThreadID, Kind: kind, Repository: repo, Title: f.Subject, SourceTime: at, Author: f.From, OwnerOnly: true})
	}
	return out, nil
}

// ItemsSource names the sources items are read from, as their cursors do.
const (
	SourceSnapshot = "snapshot"
	SourceMail     = "mail"
)

// RefreshItems reads the tracking files tr names and returns the events
// that bring st up to date: an item event for each item st does not hold
// yet, and a cursor for each source read that moved forward. The first
// snapshot read sets the cursor only. A file that cannot be read is an
// error and yields no events from it, so a bad file never empties or
// rewrites the list. The mail findings need the inventory, for their
// repositories, so they wait while the tracking files cannot be read.
func RefreshItems(st *LogState, tr Tracking, now time.Time) ([]Event, error) {
	var events []Event
	var errs []error
	queued := map[string]bool{}
	add := func(items []ItemPayload) {
		for _, it := range items {
			if _, ok := st.Items[it.ID]; ok || queued[it.ID] {
				continue
			}
			queued[it.ID] = true
			it := it
			events = append(events, Event{V: EventVersion, Time: now, Kind: KindItem, Mode: ModeShared, Item: &it})
		}
	}
	inv, err := LoadInventory(tr)
	if err != nil {
		errs = append(errs, err)
	} else if since := st.Cursors[SourceSnapshot]; inv.AsOf.After(since) {
		add(SnapshotItems(inv, since))
		events = append(events, Event{V: EventVersion, Time: now, Kind: KindCursor, Mode: ModeShared, Cursor: &CursorPayload{Source: SourceSnapshot, AsOf: inv.AsOf}})
	}
	if tr.Mail != "" && inv != nil {
		var m MailFindings
		if err := readStrict(tr.Mail, &m); err != nil {
			errs = append(errs, fmt.Errorf("tracking mail findings %w", err))
		} else if m.AsOf.IsZero() || m.Findings == nil {
			errs = append(errs, fmt.Errorf("tracking mail findings %s: unknown shape: asOf and findings must be present", tr.Mail))
		} else if m.AsOf.After(st.Cursors[SourceMail]) {
			items, err := MailItems(&m, inv)
			if err != nil {
				errs = append(errs, fmt.Errorf("tracking mail findings %s: unknown shape: %w", tr.Mail, err))
			} else {
				add(items)
				events = append(events, Event{V: EventVersion, Time: now, Kind: KindCursor, Mode: ModeShared, Cursor: &CursorPayload{Source: SourceMail, AsOf: m.AsOf}})
			}
		}
	}
	return events, errors.Join(errs...)
}

// MarkSeen records the owner marking items seen, as an owner record both
// modes read; from is the page's caller, or empty.
func MarkSeen(log *Log, now time.Time, ids []string, from string) error {
	if len(ids) == 0 {
		return errors.New("name at least one item to mark seen")
	}
	return log.Append(Event{V: EventVersion, Time: now, Kind: KindSeen, Mode: ModeOwner, Seen: &SeenPayload{IDs: ids, From: from}})
}

// SortedItems lists st's items, the newest first, unseen ones only when
// unseen is set.
func (st *LogState) SortedItems(unseen bool) []*Item {
	var out []*Item
	for _, it := range st.Items {
		if !unseen || !it.Seen {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].SourceTime.Equal(out[j].SourceTime) {
			return out[i].SourceTime.After(out[j].SourceTime)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Triaged is the item's triage, while it is for the item's text.
func (it *Item) Triaged() *TriageResult {
	if it.Triage != nil && it.Triage.Hash == ItemHash(it.ItemPayload) {
		return it.Triage
	}
	return nil
}

// urgencyRank orders items by their triage's urgency. An untriaged item
// ranks with today's, neither buried nor put first.
func urgencyRank(it *Item) int {
	tr := it.Triaged()
	switch {
	case tr == nil:
		return 1
	case tr.Urgency == "now":
		return 0
	case tr.Urgency == "today":
		return 1
	}
	return 2
}

// SortByUrgency orders items by urgency, then as they were: #7's order
// for what is new.
func SortByUrgency(items []*Item) {
	sort.SliceStable(items, func(i, j int) bool { return urgencyRank(items[i]) < urgencyRank(items[j]) })
}

// itemsCmd brings the items up to date from the tracking files, then
// lists them, or marks the ones --seen names.
func itemsCmd(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("items", flag.ContinueOnError)
	unseen := fs.Bool("new", false, "only the items not yet marked seen")
	seen := fs.String("seen", "", "mark these items seen, comma-separated ids")
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	if fs.NArg() > 0 || *seen != "" && *unseen {
		return errors.New("usage: agentd items [--new] | agentd items --seen ID,...")
	}
	log, events, err := openLoaded(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	st := NewLogState(cfg, ModeLive).Fold(events)
	if *seen != "" {
		ids := strings.Split(*seen, ",")
		for _, id := range ids {
			if st.Items[id] == nil {
				return fmt.Errorf("no item %q", id)
			}
		}
		return MarkSeen(log, time.Now(), ids, "")
	}
	fresh, rerr := RefreshItems(st, cfg.Tracking, time.Now())
	for _, e := range fresh {
		if err := log.Append(e); err != nil {
			return err
		}
		st.apply(e)
	}
	items := st.SortedItems(*unseen)
	SortByUrgency(items)
	PrintItems(w, items)
	return rerr
}

// PrintItems writes the items table.
func PrintItems(w io.Writer, items []*Item) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN\tKIND\tREPOSITORY\tTITLE\tFROM\tSEEN\tNEEDS\tID\tSUMMARY")
	for _, it := range items {
		repo, seen := it.Repository, ""
		if repo == "" {
			repo = "-"
		}
		if it.Seen {
			seen = "seen"
		}
		title := it.Title
		if it.OwnerOnly {
			title += " (owner only)"
		}
		// Mail findings carry a day, not a time.
		when := it.SourceTime.Local().Format("2006-01-02 15:04")
		if it.Kind == ItemMailQuestion || it.Kind == ItemMailReport {
			when = it.SourceTime.UTC().Format("2006-01-02")
		}
		needs, summary := "-", ""
		if tr := it.Triaged(); tr != nil {
			needs, summary = tr.NeedsOwner+"/"+tr.Urgency, tr.Summary
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", when, it.Kind, repo, title, it.Author, seen, needs, it.ID, summary)
	}
	tw.Flush()
}
