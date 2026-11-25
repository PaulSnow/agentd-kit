package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A kit says how a kind of development is run, beginning with the roles its
// agents take (#103). It is a manifest, kit.json, at the root of the kit's
// own repository, and a repository in agentd's configuration may name the
// kit it is developed under (Repository.Kit).
//
// Phase 1 only reads and checks a manifest: LoadConfig loads every kit a
// repository names and refuses one that does not hold together, so a bad
// manifest fails at start. Nothing else uses a kit yet. Kit roles are not
// agentd's session roles (Session.Role, reviewer and judge).

// KitFile is the manifest's name at the root of a kit's repository.
const KitFile = "kit.json"

// Kit is a manifest, decoded strictly.
type Kit struct {
	// Name is the kit's name, e.g. "software".
	Name string `json:"name"`
	// About says in a sentence or two what kind of development it runs.
	About string `json:"about,omitempty"`
	// Roles are the kinds of agent the kit starts.
	Roles []KitRole `json:"roles"`

	dir string // the manifest's directory, which a role's Brief is relative to
}

// KitRole is one kind of agent a kit starts. Brief, when set, is the file
// that briefs it, relative to the manifest's directory, e.g. an agent
// definition under .claude/agents.
type KitRole struct {
	Name  string `json:"name"`
	About string `json:"about,omitempty"`
	Brief string `json:"brief,omitempty"`
}

// loadKit reads the manifest at path, a kit.json or the directory of the
// kit's repository holding one, strictly: an unknown key is refused. It
// then checks it, and every error names the path.
func loadKit(path string) (*Kit, error) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, KitFile)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var k Kit
	if err := dec.Decode(&k); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s: more than one JSON value", path)
	}
	k.dir = filepath.Dir(path)
	if err := k.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &k, nil
}

// check refuses a manifest that does not hold together. It reports every
// fault, each naming its field.
func (k *Kit) check() error {
	var errs []error
	bad := func(field, format string, a ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, a...)))
	}
	if strings.TrimSpace(k.Name) == "" {
		bad("name", "empty")
	}
	if len(k.Roles) == 0 {
		bad("roles", "none: a kit holds at least one role")
	}
	seen := map[string]bool{}
	for i, r := range k.Roles {
		field := fmt.Sprintf("roles[%d]", i)
		switch {
		case r.Name == "":
			bad(field+".name", "empty")
		case !sessionName.MatchString(r.Name):
			bad(field+".name", "%q must be letters, digits, _ and -", r.Name)
		case seen[r.Name]:
			bad(fmt.Sprintf("roles.%q", r.Name), "duplicate")
		}
		seen[r.Name] = true
		if r.Brief != "" {
			if err := k.checkBrief(r.Brief); err != nil {
				bad(field+".brief", "%v", err)
			}
		}
	}
	return errors.Join(errs...)
}

// checkBrief checks that a role's brief is a file in the kit, relative to
// the manifest's directory and not outside it.
func (k *Kit) checkBrief(p string) error {
	clean := filepath.Clean(filepath.FromSlash(p))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%q must be a path inside the kit, relative to %s", p, KitFile)
	}
	info, err := os.Stat(filepath.Join(k.dir, clean))
	switch {
	case err != nil:
		return fmt.Errorf("%q does not exist in %s", p, k.dir)
	case info.IsDir():
		return fmt.Errorf("%q is a directory, not a file", p)
	}
	return nil
}
