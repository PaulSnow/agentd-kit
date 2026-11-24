package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The software kit's manifest in testdata loads, by its file or by its
// directory, and holds its roles.
func TestSoftwareKitLoads(t *testing.T) {
	for _, p := range []string{"testdata/kits/software/kit.json", "testdata/kits/software"} {
		k, err := loadKit(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if k.Name != "software" {
			t.Fatalf("%s: name %q", p, k.Name)
		}
		var names []string
		for _, r := range k.Roles {
			names = append(names, r.Name)
		}
		if got := strings.Join(names, ","); got != "phase-planner,plan-reviewer,phase-coder,code-critic" {
			t.Fatalf("%s: roles %s", p, got)
		}
	}
}

// writeKit writes a manifest, and a brief file brief.md beside it, into a
// new directory, and returns the manifest's path.
func writeKit(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "brief.md"), "# brief\n")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, KitFile)
	writeFile(t, p, manifest)
	return p
}

// A manifest that does not hold together is refused, and the error names
// the field at fault.
func TestKitRefusals(t *testing.T) {
	for _, c := range []struct{ name, manifest, field string }{
		{"unknown field", `{"name": "k", "roles": [{"name": "a"}], "budget": 5}`, `"budget"`},
		{"unknown role field", `{"name": "k", "roles": [{"name": "a", "tokens": 5}]}`, `"tokens"`},
		{"empty name", `{"name": " ", "roles": [{"name": "a"}]}`, "name: empty"},
		{"no roles", `{"name": "k", "roles": []}`, "roles: none"},
		{"empty role name", `{"name": "k", "roles": [{"name": "a"}, {"name": ""}]}`, "roles[1].name: empty"},
		{"duplicate role", `{"name": "k", "roles": [{"name": "coder"}, {"name": "coder"}]}`, `roles."coder": duplicate`},
		{"bad role name", `{"name": "k", "roles": [{"name": "a"}, {"name": "a b"}]}`, "roles[1].name"},
		{"missing brief", `{"name": "k", "roles": [{"name": "a", "brief": "brief.md"}, {"name": "b", "brief": "none.md"}]}`, "roles[1].brief"},
		{"brief is a directory", `{"name": "k", "roles": [{"name": "a", "brief": "sub"}]}`, "roles[0].brief"},
		{"brief outside the kit", `{"name": "k", "roles": [{"name": "a", "brief": "../brief.md"}]}`, "roles[0].brief"},
		{"two values", `{"name": "k", "roles": [{"name": "a"}]} {}`, "more than one"},
	} {
		p := writeKit(t, c.manifest)
		_, err := loadKit(p)
		if err == nil {
			t.Fatalf("%s: loaded", c.name)
		}
		if !strings.Contains(err.Error(), c.field) || !strings.Contains(err.Error(), p) {
			t.Fatalf("%s: error %q does not name %s and the path", c.name, err, c.field)
		}
	}
	// Every fault is reported, not only the first.
	_, err := loadKit(writeKit(t, `{"name": "", "roles": [{"name": "x", "brief": "none.md"}, {"name": "x"}]}`))
	for _, want := range []string{"name: empty", "roles[0].brief", `roles."x": duplicate`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v lacks %s", err, want)
		}
	}
	if _, err := loadKit(filepath.Join(t.TempDir(), KitFile)); err == nil {
		t.Fatal("a missing manifest loaded")
	}
}

// A repository may name a kit: LoadConfig accepts one with none, loads one
// that names a good kit, relative to the config, and refuses one whose kit
// is bad, naming the repository's kit field.
func TestConfigLoadsKits(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "c.json")
	repo := func(kit string) string {
		return `{"repositories": {"r": {"forge": "gitlab", "project": "a/b"` + kit + `}}}`
	}
	writeFile(t, cfg, repo(""))
	c, err := LoadConfig(cfg)
	if err != nil {
		t.Fatalf("no kit: %v", err)
	}
	if r := c.Repositories["r"]; r.Kit != "" || r.kit != nil {
		t.Fatalf("no kit: got %q %v", r.Kit, r.kit)
	}

	good, err := filepath.Abs("testdata/kits/software")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, cfg, repo(`, "kit": "`+good+`"`))
	c, err = LoadConfig(cfg)
	if err != nil {
		t.Fatalf("good kit: %v", err)
	}
	if k := c.Repositories["r"].kit; k == nil || k.Name != "software" {
		t.Fatalf("good kit not loaded: %v", k)
	}

	bad := writeKit(t, `{"name": "k", "roles": []}`)
	rel, err := filepath.Rel(dir, bad)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, cfg, repo(`, "kit": "`+rel+`"`))
	if _, err := LoadConfig(cfg); err == nil || !strings.Contains(err.Error(), "repositories.r.kit:") || !strings.Contains(err.Error(), "roles: none") {
		t.Fatalf("bad kit: %v", err)
	}
	writeFile(t, cfg, repo(`, "kit": "missing"`))
	if _, err := LoadConfig(cfg); err == nil || !strings.Contains(err.Error(), "repositories.r.kit:") {
		t.Fatalf("missing kit: %v", err)
	}
}
