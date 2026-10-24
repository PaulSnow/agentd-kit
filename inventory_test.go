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
	if s := inv.Find("gitlab", "example-org/tools/sprockets"); s == nil || s.Active {
		t.Fatalf("sprockets = %+v", s)
	}
	// Lookups ignore case, as the snapshot's own manifest keys do.
	if inv.Find("GitLab", "Example-Org/Widgets") != w {
		t.Fatal("lookup is case-sensitive")
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
		{name: "manifest unknown field", file: "manifest", text: `[{"name": "w", "platform": "gitlab", "path": "o/w", "active": true, "audience": "a", "owner": "x"}]`, want: `unknown field "owner"`},
		{name: "manifest unknown platform", file: "manifest", text: `[{"name": "w", "platform": "bitbucket", "path": "o/w", "active": true, "audience": "a"}]`, want: "platform"},
		{name: "manifest entry without path", file: "manifest", text: `[{"name": "w", "platform": "gitlab", "active": true, "audience": "a"}]`, want: "path"},
		{name: "manifest duplicate", file: "manifest", text: `[{"name": "w", "platform": "gitlab", "path": "o/w", "active": true, "audience": "a"}, {"name": "w2", "platform": "gitlab", "path": "O/W", "active": true, "audience": "a"}]`, want: "twice"},
		{name: "snapshot unknown field", file: "snapshot", text: `{"AsOf": "2026-10-23T21:23:04Z", "Host": "host-a", "Repos": [], "Version": 2}`, want: `unknown field "Version"`},
		{name: "snapshot without AsOf", file: "snapshot", text: `{"Host": "host-a", "Repos": []}`, want: "AsOf"},
		{name: "snapshot repos not a list", file: "snapshot", text: `{"AsOf": "2026-10-23T21:23:04Z", "Host": "host-a", "Repos": {}}`, want: "unknown shape"},
		{name: "snapshot trailing data", file: "snapshot", text: `{"AsOf": "2026-10-23T21:23:04Z", "Host": "host-a", "Repos": []} {}`, want: "trailing"},
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
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not say %q", err, c.want)
			}
			if !c.noConfig && !strings.Contains(err.Error(), path) {
				t.Fatalf("error %q does not name %s", err, path)
			}
		})
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
