package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trackingFixture copies the fixture manifest and snapshot into a fresh
// directory and returns the Tracking that names them.
func trackingFixture(t *testing.T) Tracking {
	dir := t.TempDir()
	tr := Tracking{Manifest: filepath.Join(dir, "repositories.json"), Snapshot: filepath.Join(dir, "snapshot.json")}
	for src, dst := range map[string]string{"repositories.json": tr.Manifest, "snapshot.json": tr.Snapshot} {
		b, err := os.ReadFile(filepath.Join("testdata", "tracking", src))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, dst, string(b))
	}
	return tr
}

// A good manifest and snapshot join into one record per manifest
// repository, keyed by platform and path, with the snapshot's open work and
// each machine's clones.
func TestInventoryReadsTheTrackingFiles(t *testing.T) {
	inv, err := LoadInventory(trackingFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if inv.Host != "host-a" || inv.AsOf.IsZero() {
		t.Fatalf("snapshot header: host %q, as of %v", inv.Host, inv.AsOf)
	}
	if len(inv.Repos) != 4 {
		t.Fatalf("got %d repositories, want the manifest's 4", len(inv.Repos))
	}
	w := inv.Find("gitlab", "example-org/widgets")
	if w == nil {
		t.Fatal("widgets missing")
	}
	if w.Key != "gitlab/example-org/widgets" || w.Name != "widgets" || !w.Active || w.Audience != "team-a" {
		t.Fatalf("widgets record = %+v", w)
	}
	if len(w.OpenIssues) != 2 || w.OpenIssues[0].Number != 17 || len(w.OpenMRs) != 1 || w.OpenMRs[0].Number != 40 {
		t.Fatalf("widgets open work: issues %+v, requests %+v", w.OpenIssues, w.OpenMRs)
	}
	if len(w.Clones) != 2 || w.Clones[0].Host != "host-a" || w.Clones[1].FetchError == "" {
		t.Fatalf("widgets clones = %+v", w.Clones)
	}
	if !w.Tracked {
		t.Fatal("widgets is in the snapshot but not marked tracked")
	}
	// A manifest repository the snapshot has not fetched is listed, with no
	// work and untracked, never dropped.
	d := inv.Find("github", "example-user/doohickey")
	if d == nil || d.Tracked || len(d.OpenIssues) != 0 {
		t.Fatalf("doohickey = %+v", d)
	}
	if s := inv.Find("gitlab", "example-org/tools/sprockets"); s == nil || s.Active || s.Note != "kept for reference" {
		t.Fatalf("sprockets = %+v", s)
	}
	// Lookups ignore case, as the snapshot's own manifest keys do.
	if inv.Find("GitLab", "Example-Org/Widgets") != w {
		t.Fatal("lookup is case-sensitive")
	}
}

// A snapshot with no repositories yet, written with null lists as the
// tracking system writes them, is read; every manifest repository is
// listed, untracked.
func TestInventoryReadsAnEmptySnapshot(t *testing.T) {
	tr := trackingFixture(t)
	writeFile(t, tr.Snapshot, `{"AsOf": "2026-10-23T21:23:04Z", "Host": "host-a", "Repos": null, "Errors": null, "LocalWork": null, "Clones": null, "Platform": null, "Manifest": {}, "Scans": null}`)
	inv, err := LoadInventory(tr)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range inv.Repos {
		if r.Tracked {
			t.Errorf("%s tracked by an empty snapshot", r.Key)
		}
	}
}

// Each failure names the file and says what is wrong with it: missing,
// not JSON, or JSON of a shape agentd does not know.
func TestInventoryRefusesBadFiles(t *testing.T) {
	cases := []struct {
		name     string
		file     string // "manifest" or "snapshot"
		text     string // "" removes the file
		want     string
		noConfig bool
	}{
		{name: "missing manifest", file: "manifest", want: "does not exist"},
		{name: "missing snapshot", file: "snapshot", want: "does not exist"},
		{name: "malformed manifest", file: "manifest", text: `[{"name": "widgets",`, want: "not valid JSON"},
		{name: "malformed snapshot", file: "snapshot", text: `{"AsOf": `, want: "not valid JSON"},
		{name: "manifest is an object", file: "manifest", text: `{"repositories": []}`, want: "unknown shape"},
		{name: "manifest unknown platform", file: "manifest", text: `[{"name": "w", "platform": "bitbucket", "path": "o/w", "active": true, "audience": "a"}]`, want: `platform "bitbucket" is not github or gitlab`},
		{name: "manifest entry without path", file: "manifest", text: `[{"name": "w", "platform": "gitlab", "active": true, "audience": "a"}]`, want: "name and path must be set"},
		{name: "manifest duplicate", file: "manifest", text: `[{"name": "w", "platform": "gitlab", "path": "o/w", "active": true, "audience": "a"}, {"name": "w2", "platform": "gitlab", "path": "O/W", "active": true, "audience": "a"}]`, want: "twice"},
		{name: "snapshot without AsOf", file: "snapshot", text: `{"Host": "host-a", "Repos": []}`, want: "AsOf and Repos must be present"},
		{name: "snapshot repos not a list", file: "snapshot", text: `{"AsOf": "2026-10-23T21:23:04Z", "Host": "host-a", "Repos": {}}`, want: "unknown shape"},
		{name: "snapshot trailing data", file: "snapshot", text: `{"AsOf": "2026-10-23T21:23:04Z", "Host": "host-a", "Repos": []} {}`, want: "trailing data"},
		{name: "manifest is null", file: "manifest", text: `null`, want: "not a list"},
		{name: "snapshot without Repos", file: "snapshot", text: `{"AsOf": "2026-10-23T21:23:04Z", "Host": "host-a"}`, want: "AsOf and Repos must be present"},
		{name: "snapshot duplicate", file: "snapshot", text: `{"AsOf": "2026-10-23T21:23:04Z", "Host": "host-a", "Repos": [{"Info": {"Name": "w", "Platform": "gitlab", "FullPath": "o/w"}}, {"Info": {"Name": "w", "Platform": "gitlab", "FullPath": "o/W"}}]}`, want: "gitlab/o/w is listed twice"},
		{name: "snapshot unknown platform", file: "snapshot", text: `{"AsOf": "2026-10-23T21:23:04Z", "Host": "host-a", "Repos": [{"Info": {"Name": "w", "Platform": "bitbucket", "FullPath": "o/w"}}]}`, want: `platform "bitbucket"`},
		{name: "snapshot clone platform", file: "snapshot", text: `{"AsOf": "2026-10-23T21:23:04Z", "Host": "host-a", "Repos": null, "Clones": [{"Host": "host-a", "Path": "/srv/w", "Platform": "svn", "FullPath": "o/w"}]}`, want: `Clones[0]: platform "svn"`},
		{name: "snapshot unknown issue state", file: "snapshot", text: `{"AsOf": "2026-10-23T21:23:04Z", "Host": "host-a", "Repos": [{"Info": {"Name": "w", "Platform": "gitlab", "FullPath": "o/w"}, "Issues": [{"Number": 1, "State": "reopened"}]}]}`, want: `issue 1 has state "reopened"`},
		{name: "snapshot unknown request state", file: "snapshot", text: `{"AsOf": "2026-10-23T21:23:04Z", "Host": "host-a", "Repos": [{"Info": {"Name": "w", "Platform": "gitlab", "FullPath": "o/w"}, "MRs": [{"Number": 1, "State": "draft"}]}]}`, want: `request 1 has state "draft"`},
		{name: "not configured", noConfig: true, want: "tracking.manifest"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := trackingFixture(t)
			path := tr.Manifest
			if c.file == "snapshot" {
				path = tr.Snapshot
			}
			switch {
			case c.noConfig:
				tr = Tracking{}
			case c.text == "":
				os.Remove(path)
			default:
				writeFile(t, path, c.text)
			}
			_, err := LoadInventory(tr)
			if err == nil {
				t.Fatal("loaded")
			}
			// The temporary directory holds the test's name; leave it out.
			if !strings.Contains(strings.ReplaceAll(err.Error(), path, "PATH"), c.want) {
				t.Fatalf("error %q does not say %q", err, c.want)
			}
			if !c.noConfig && !strings.Contains(err.Error(), path) {
				t.Fatalf("error %q does not name %s", err, path)
			}
		})
	}
}

// tolerantFixture is trackingFixture with fields agentd does not know: the
// tracking system's "Behind" at the snapshot's top level and in its first
// repository, and an "owner" in the manifest's first entry.
func tolerantFixture(t *testing.T) Tracking {
	tr := trackingFixture(t)
	for path, edits := range map[string][][2]string{
		tr.Snapshot: {{`"Host": "host-a",`, `"Host": "host-a", "Behind": 3,`}, {`"Info": {"Name": "widgets"`, `"Behind": 2, "Info": {"Name": "widgets"`}},
		tr.Manifest: {{`"audience": "team-a"}`, `"audience": "team-a", "owner": "user-a"}`}},
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, e := range edits {
			if !strings.Contains(s, e[0]) {
				t.Fatalf("%s has no %s", path, e[0])
			}
			s = strings.Replace(s, e[0], e[1], 1)
		}
		writeFile(t, path, s)
	}
	return tr
}

// The tracking files are the tracking system's, not agentd's: a field
// agentd does not know is ignored, and named, never a refusal (#75).
func TestInventoryToleratesUnknownFields(t *testing.T) {
	want, err := LoadInventory(trackingFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	tr := tolerantFixture(t)
	inv, err := LoadInventory(tr)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Repos) != len(want.Repos) {
		t.Fatalf("got %d repositories, want %d", len(inv.Repos), len(want.Repos))
	}
	for i, r := range inv.Repos {
		w := want.Repos[i]
		if r.Key != w.Key || r.Tracked != w.Tracked || len(r.OpenIssues) != len(w.OpenIssues) || len(r.OpenMRs) != len(w.OpenMRs) || len(r.Clones) != len(w.Clones) {
			t.Fatalf("record %d = %+v, want %+v", i, r, w)
		}
	}
	if len(want.Unknown) != 0 {
		t.Fatalf("the plain fixture has unknown fields %v", want.Unknown)
	}
	if got := strings.Join(inv.Unknown[tr.Snapshot], ","); got != "Behind,Repos[].Behind" {
		t.Fatalf("snapshot unknown fields = %q", got)
	}
	if got := strings.Join(inv.Unknown[tr.Manifest], ","); got != "[].owner" {
		t.Fatalf("manifest unknown fields = %q", got)
	}
}

// agentd inventory prints one row per repository from the configured
// files, and refuses with the file's name when one is bad.
func TestInventoryCommand(t *testing.T) {
	tr := trackingFixture(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agentd.json")
	writeFile(t, cfgPath, `{"event_log": "`+filepath.Join(dir, "events.jsonl")+`", "transcript_dir": "", "tracking": {"manifest": "`+tr.Manifest+`", "snapshot": "`+tr.Snapshot+`"}}`)
	var out bytes.Buffer
	if err := inventoryCmd([]string{"--config", cfgPath}, &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"snapshot of host-a", "gitlab/example-org/widgets", "host-a,host-b", "github/example-user/doohickey", "not in snapshot"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) != 6 { // header line, column heads, four repositories
		t.Errorf("got %d lines:\n%s", len(lines), text)
	}
	writeFile(t, tr.Snapshot, `{`)
	if err := inventoryCmd([]string{"--config", cfgPath}, &out); err == nil || !strings.Contains(err.Error(), tr.Snapshot) {
		t.Fatalf("bad snapshot: %v", err)
	}
}
