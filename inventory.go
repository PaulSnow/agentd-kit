package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Tracking names the owner's tracking system's output files. agentd reads
// them and fetches none of their data again: the manifest is the canonical
// repository list, and the snapshot holds each repository's issues, merge
// requests and commits and every machine's clones.
type Tracking struct {
	Manifest string `json:"manifest"`
	Snapshot string `json:"snapshot"`
}

// ManifestRepo is one entry of the tracking system's repository manifest.
type ManifestRepo struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Path     string `json:"path"`
	Active   bool   `json:"active"`
	Audience string `json:"audience"`
	Note     string `json:"note,omitempty"`
}

// Snapshot is the tracking system's snapshot file. Its field names are the
// tracking system's own; a field agentd does not know refuses the file, so
// a change of format is noticed rather than half read.
type Snapshot struct {
	AsOf      time.Time         `json:"AsOf"`
	Host      string            `json:"Host"`
	Repos     []SnapRepo        `json:"Repos"`
	Errors    []json.RawMessage `json:"Errors"` // null in every snapshot seen; kept as written
	LocalWork []LocalWork       `json:"LocalWork"`
	Clones    []SnapClone       `json:"Clones"`
	Platform  []SnapPushed      `json:"Platform"`
	Manifest  map[string]bool   `json:"Manifest"`
	Scans     []SnapScan        `json:"Scans"`
}

// SnapRepo is one repository's forge data in the snapshot.
type SnapRepo struct {
	Info    SnapInfo     `json:"Info"`
	Issues  []SnapIssue  `json:"Issues"`
	MRs     []SnapMR     `json:"MRs"`
	Commits []SnapCommit `json:"Commits"`
}

// SnapInfo names a snapshot repository.
type SnapInfo struct {
	Name     string `json:"Name"`
	Platform string `json:"Platform"`
	FullPath string `json:"FullPath"`
	Audience string `json:"Audience"`
}

// SnapIssue is an issue as the snapshot records it.
type SnapIssue struct {
	Number      int        `json:"Number"`
	Title       string     `json:"Title"`
	Description string     `json:"Description"`
	State       string     `json:"State"`
	Author      string     `json:"Author"`
	Labels      []string   `json:"Labels"`
	CreatedAt   time.Time  `json:"CreatedAt"`
	UpdatedAt   time.Time  `json:"UpdatedAt"`
	ClosedAt    *time.Time `json:"ClosedAt"`
}

// SnapMR is a merge or pull request as the snapshot records it.
type SnapMR struct {
	Number          int        `json:"Number"`
	Title           string     `json:"Title"`
	Description     string     `json:"Description"`
	State           string     `json:"State"`
	Author          string     `json:"Author"`
	SourceBranch    string     `json:"SourceBranch"`
	TargetBranch    string     `json:"TargetBranch"`
	RelatedIssueIDs []int      `json:"RelatedIssueIDs"`
	CreatedAt       time.Time  `json:"CreatedAt"`
	UpdatedAt       time.Time  `json:"UpdatedAt"`
	MergedAt        *time.Time `json:"MergedAt"`
}

// SnapCommit is a recent commit as the snapshot records it.
type SnapCommit struct {
	SHA         string    `json:"SHA"`
	Message     string    `json:"Message"`
	AuthorName  string    `json:"AuthorName"`
	AuthorEmail string    `json:"AuthorEmail"`
	CommittedAt time.Time `json:"CommittedAt"`
}

// LocalWork is a checkout holding work not on its forge: dirty files,
// unpushed commits, stashes or worktrees.
type LocalWork struct {
	Host       string   `json:"Host"`
	Path       string   `json:"Path"`
	Branch     string   `json:"Branch"`
	Dirty      int      `json:"Dirty"`
	Unpushed   int      `json:"Unpushed"`
	UnpushedOn []string `json:"UnpushedOn"`
	NoRemote   bool     `json:"NoRemote"`
	Stashes    int      `json:"Stashes"`
	Worktrees  []string `json:"Worktrees"`
	LastCommit string   `json:"LastCommit"`
}

// SnapClone is one clone of a forge repository on one machine.
type SnapClone struct {
	Host       string `json:"Host"`
	Path       string `json:"Path"`
	Platform   string `json:"Platform"`
	FullPath   string `json:"FullPath"`
	Branch     string `json:"Branch,omitempty"`
	FetchError string `json:"FetchError,omitempty"`
}

// SnapPushed is when a repository was last pushed to, by its forge.
type SnapPushed struct {
	Platform string `json:"Platform"`
	FullPath string `json:"FullPath"`
	PushedAt string `json:"PushedAt"`
}

// SnapScan is when a machine's clones were last scanned.
type SnapScan struct {
	Host string    `json:"Host"`
	AsOf time.Time `json:"AsOf"`
}

// RepoRecord is the inventory's record for one repository: the manifest's
// entry, joined with the snapshot's open work and clones. Tracked says the
// snapshot holds the repository's forge data; one it does not hold is
// listed all the same, with no work, never dropped.
type RepoRecord struct {
	Key        string // platform/path, lower case: the repository's identity
	Name       string
	Platform   string
	Path       string
	Active     bool
	Audience   string
	Note       string
	Tracked    bool
	OpenIssues []SnapIssue
	OpenMRs    []SnapMR
	Clones     []SnapClone
}

// Inventory is every repository in the manifest, in key order, as the
// snapshot taken on Host at AsOf saw it.
type Inventory struct {
	AsOf  time.Time
	Host  string
	Repos []*RepoRecord
	Scans []SnapScan
	byKey map[string]*RepoRecord
}

// repoKey is a repository's identity: its platform and project path, in
// lower case, as the snapshot's own manifest keys them.
func repoKey(platform, path string) string {
	return strings.ToLower(platform + "/" + path)
}

// Find returns the record for platform and path, or nil.
func (inv *Inventory) Find(platform, path string) *RepoRecord {
	return inv.byKey[repoKey(platform, path)]
}

// LoadInventory reads the manifest and the snapshot that tr names and joins
// them. Either file missing, not JSON, or of a shape agentd does not know
// is an error naming the file.
func LoadInventory(tr Tracking) (*Inventory, error) {
	if tr.Manifest == "" || tr.Snapshot == "" {
		return nil, errors.New("tracking.manifest and tracking.snapshot must be set in the configuration")
	}
	var manifest []ManifestRepo
	if err := readStrict(tr.Manifest, &manifest); err != nil {
		return nil, fmt.Errorf("tracking manifest %w", err)
	}
	if manifest == nil {
		return nil, fmt.Errorf("tracking manifest %s: unknown shape: not a list", tr.Manifest)
	}
	var snap Snapshot
	if err := readStrict(tr.Snapshot, &snap); err != nil {
		return nil, fmt.Errorf("tracking snapshot %w", err)
	}
	// Repos may be null, as the tracking system writes an empty list, but
	// it must be there: a file without it is not a snapshot.
	var keys map[string]json.RawMessage
	if err := readStrict(tr.Snapshot, &keys); err != nil {
		return nil, fmt.Errorf("tracking snapshot %w", err)
	}
	if _, ok := keys["Repos"]; !ok || snap.AsOf.IsZero() {
		return nil, fmt.Errorf("tracking snapshot %s: unknown shape: AsOf and Repos must be present", tr.Snapshot)
	}
	inv := &Inventory{AsOf: snap.AsOf, Host: snap.Host, Scans: snap.Scans, byKey: map[string]*RepoRecord{}}
	var errs []error
	for i, m := range manifest {
		switch {
		case !forgePlatform(m.Platform):
			errs = append(errs, fmt.Errorf("entry %d: platform %q is not github or gitlab", i, m.Platform))
			continue
		case m.Name == "" || m.Path == "":
			errs = append(errs, fmt.Errorf("entry %d: name and path must be set", i))
			continue
		}
		key := repoKey(m.Platform, m.Path)
		if _, dup := inv.byKey[key]; dup {
			errs = append(errs, fmt.Errorf("entry %d: %s is listed twice", i, key))
			continue
		}
		r := &RepoRecord{Key: key, Name: m.Name, Platform: m.Platform, Path: m.Path, Active: m.Active, Audience: m.Audience, Note: m.Note}
		inv.byKey[key] = r
		inv.Repos = append(inv.Repos, r)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("tracking manifest %s: unknown shape: %w", tr.Manifest, err)
	}
	seen := map[string]bool{}
	for i, sr := range snap.Repos {
		key := repoKey(sr.Info.Platform, sr.Info.FullPath)
		switch {
		case !forgePlatform(sr.Info.Platform):
			errs = append(errs, fmt.Errorf("Repos[%d]: platform %q is not github or gitlab", i, sr.Info.Platform))
			continue
		case seen[key]:
			errs = append(errs, fmt.Errorf("Repos[%d]: %s is listed twice", i, key))
			continue
		}
		seen[key] = true
		r := inv.byKey[key]
		for _, is := range sr.Issues {
			open, known := workStates[is.State]
			if !known {
				errs = append(errs, fmt.Errorf("Repos[%d]: issue %d has state %q", i, is.Number, is.State))
			}
			if r != nil && open {
				r.OpenIssues = append(r.OpenIssues, is)
			}
		}
		for _, mr := range sr.MRs {
			open, known := workStates[mr.State]
			if !known {
				errs = append(errs, fmt.Errorf("Repos[%d]: request %d has state %q", i, mr.Number, mr.State))
			}
			if r != nil && open {
				r.OpenMRs = append(r.OpenMRs, mr)
			}
		}
		if r == nil {
			continue // the manifest is the list; the snapshot may lag it
		}
		r.Tracked = true
		sort.Slice(r.OpenIssues, func(i, j int) bool { return r.OpenIssues[i].Number < r.OpenIssues[j].Number })
		sort.Slice(r.OpenMRs, func(i, j int) bool { return r.OpenMRs[i].Number < r.OpenMRs[j].Number })
	}
	for i, c := range snap.Clones {
		if !forgePlatform(c.Platform) {
			errs = append(errs, fmt.Errorf("Clones[%d]: platform %q is not github or gitlab", i, c.Platform))
		}
		if r := inv.byKey[repoKey(c.Platform, c.FullPath)]; r != nil {
			r.Clones = append(r.Clones, c)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("tracking snapshot %s: unknown shape: %w", tr.Snapshot, err)
	}
	for _, r := range inv.Repos {
		sort.SliceStable(r.Clones, func(i, j int) bool { return r.Clones[i].Host < r.Clones[j].Host })
	}
	sort.Slice(inv.Repos, func(i, j int) bool { return inv.Repos[i].Key < inv.Repos[j].Key })
	return inv, nil
}

// forgePlatform says whether p is a platform the tracking system fetches.
func forgePlatform(p string) bool { return p == "github" || p == "gitlab" }

// workStates are the issue and request states the tracking system writes,
// and whether each is open. Any other state is an unknown shape.
var workStates = map[string]bool{"open": true, "opened": true, "closed": false, "merged": false, "locked": false}

// readStrict decodes the one JSON value in path into v, refusing fields v
// does not have and anything after the value. Its errors start with the
// path and say which way the file is wrong.
func readStrict(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s does not exist", path)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !json.Valid(b) {
		// json.Valid also fails on a second value after the first, which
		// Decode below would accept; say which it is.
		var first json.RawMessage
		if dec := json.NewDecoder(bytes.NewReader(b)); dec.Decode(&first) == nil {
			return fmt.Errorf("%s: unknown shape: trailing data after the first JSON value", path)
		}
		return fmt.Errorf("%s is not valid JSON", path)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: unknown shape: %w", path, err)
	}
	return nil
}

// inventoryCmd prints the inventory: one row per manifest repository with
// its open issues and requests and the machines that have a clone.
func inventoryCmd(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("inventory", flag.ContinueOnError)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	inv, err := LoadInventory(cfg.Tracking)
	if err != nil {
		return err
	}
	PrintInventory(w, inv)
	return nil
}

// PrintInventory writes the inventory table.
func PrintInventory(w io.Writer, inv *Inventory) {
	fmt.Fprintf(w, "snapshot of %s at %s; %d repositories\n", inv.Host, inv.AsOf.Local().Format("2006-01-02 15:04"), len(inv.Repos))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPOSITORY\tACTIVE\tISSUES\tREQUESTS\tCLONES\tNOTE")
	for _, r := range inv.Repos {
		var hosts []string
		for _, c := range r.Clones {
			if len(hosts) == 0 || hosts[len(hosts)-1] != c.Host {
				hosts = append(hosts, c.Host)
			}
		}
		clones := strings.Join(hosts, ",")
		if clones == "" {
			clones = "-"
		}
		note := r.Note
		if !r.Tracked {
			note = strings.TrimPrefix(note+"; not in snapshot", "; ")
		}
		active := "yes"
		if !r.Active {
			active = "no"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\n", r.Key, active, len(r.OpenIssues), len(r.OpenMRs), clones, note)
	}
	tw.Flush()
}
