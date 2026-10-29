package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
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
	Mail     string `json:"mail"` // the mail findings file, beside the snapshot
	// Priorities is the weekly review's file: each repository's priority
	// this week and the owner's pinned cases. agentd cases ranks by it.
	Priorities string `json:"priorities"`
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
// tracking system's own. The file is external, so a field agentd does not
// know is ignored and named (#75): a new field never stops a poll. A value
// agentd cannot read, a missing AsOf or Repos, or an unknown platform or
// state still refuses the file.
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
	// Unknown is, by file path, the fields the manifest and the snapshot
	// carry that agentd does not know, as dotted names.
	Unknown map[string][]string

	snapshot *Snapshot // as read, for the items it holds
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
// is an error naming the file. A field it does not know is not: it is
// ignored, and named in Unknown.
func LoadInventory(tr Tracking) (*Inventory, error) {
	if tr.Manifest == "" || tr.Snapshot == "" {
		return nil, errors.New("tracking.manifest and tracking.snapshot must be set in the configuration")
	}
	var manifest []ManifestRepo
	unknown := map[string][]string{}
	mu, err := readTolerant(tr.Manifest, &manifest)
	if err != nil {
		return nil, fmt.Errorf("tracking manifest %w", err)
	}
	if manifest == nil {
		return nil, fmt.Errorf("tracking manifest %s: unknown shape: not a list", tr.Manifest)
	}
	// The snapshot is read once and decoded twice, so both decodings see
	// the same file even when the tracking system replaces it meanwhile.
	b, err := readFile(tr.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("tracking snapshot %w", err)
	}
	var snap Snapshot
	su, err := decodeTolerant(tr.Snapshot, b, &snap)
	if err != nil {
		return nil, fmt.Errorf("tracking snapshot %w", err)
	}
	for path, names := range map[string][]string{tr.Manifest: mu, tr.Snapshot: su} {
		if len(names) > 0 {
			unknown[path] = names
		}
	}
	// Repos may be null, as the tracking system writes an empty list, but
	// it must be there: a file without it is not a snapshot.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, fmt.Errorf("tracking snapshot %s: unknown shape: %w", tr.Snapshot, err)
	}
	if _, ok := keys["Repos"]; !ok || snap.AsOf.IsZero() {
		return nil, fmt.Errorf("tracking snapshot %s: unknown shape: AsOf and Repos must be present", tr.Snapshot)
	}
	inv := &Inventory{AsOf: snap.AsOf, Host: snap.Host, Scans: snap.Scans, byKey: map[string]*RepoRecord{}, Unknown: unknown, snapshot: &snap}
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
	b, err := readFile(path)
	if err != nil {
		return err
	}
	return decodeStrict(path, b, v)
}

// readFile reads path, saying plainly when it does not exist.
func readFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s does not exist", path)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return b, nil
}

// decodeStrict is readStrict's decoding of b, read from path.
func decodeStrict(path string, b []byte, v any) error {
	if err := checkJSON(path, b); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: unknown shape: %w", path, err)
	}
	return nil
}

// checkJSON says whether b, read from path, is one JSON value.
func checkJSON(path string, b []byte) error {
	if !json.Valid(b) {
		// json.Valid also fails on a second value after the first, which
		// Decode would accept; say which it is.
		var first json.RawMessage
		if dec := json.NewDecoder(bytes.NewReader(b)); dec.Decode(&first) == nil {
			return fmt.Errorf("%s: unknown shape: trailing data after the first JSON value", path)
		}
		return fmt.Errorf("%s is not valid JSON", path)
	}
	return nil
}

// readTolerant decodes the one JSON value in path into v as readStrict
// does, but for an external file, written by another program: a field v
// does not have is ignored, and named in the sorted list it returns.
func readTolerant(path string, v any) ([]string, error) {
	b, err := readFile(path)
	if err != nil {
		return nil, err
	}
	return decodeTolerant(path, b, v)
}

// decodeTolerant is readTolerant's decoding of b, read from path.
func decodeTolerant(path string, b []byte, v any) ([]string, error) {
	if err := checkJSON(path, b); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return nil, fmt.Errorf("%s: unknown shape: %w", path, err)
	}
	found := map[string]bool{}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := unknownFields(dec, reflect.TypeOf(v), "", found); err != nil {
		return nil, fmt.Errorf("%s: unknown shape: %w", path, err)
	}
	return sortedKeys(found), nil
}

// unknownFields reads the next value from dec and adds to found the
// dotted name of each object key in it that t, the Go type it decodes
// into, has no field for: "Behind", "Repos[].Behind". A nil t is a value
// nothing decodes into, whose keys are not looked at. Only names are
// kept, never values.
func unknownFields(dec *json.Decoder, t reflect.Type, at string, found map[string]bool) error {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('['):
		var elem reflect.Type
		if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			elem = t.Elem()
		}
		for dec.More() {
			if err := unknownFields(dec, elem, at+"[]", found); err != nil {
				return err
			}
		}
	case json.Delim('{'):
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := k.(string)
			name := key
			if at != "" {
				name = at + "." + key
			}
			var ft reflect.Type
			switch {
			case t == nil:
			case t.Kind() == reflect.Struct:
				if ft = jsonField(t, key); ft == nil {
					found[name] = true
				}
			case t.Kind() == reflect.Map:
				ft, name = t.Elem(), at // a map's keys are data, not names
			}
			if err := unknownFields(dec, ft, name, found); err != nil {
				return err
			}
		}
	default:
		return nil
	}
	_, err = dec.Token() // the closing ] or }
	return err
}

// jsonField is the type of the field of struct t that encoding/json
// decodes key into, or nil. As encoding/json does, a key matches a field's
// JSON name, or its Go name when it has none, ignoring case.
func jsonField(t reflect.Type, key string) reflect.Type {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if strings.EqualFold(name, key) {
			return f.Type
		}
	}
	return nil
}

// ignoredFields writes, once in a process, each unknown field an external
// file was read with, at info: a new field is told, not repeated at every
// poll.
type ignoredFields struct {
	mu   sync.Mutex
	seen map[string]bool // "path: field"
	w    io.Writer
}

func newIgnoredFields(w io.Writer) *ignoredFields {
	return &ignoredFields{seen: map[string]bool{}, w: w}
}

// note writes one line naming the fields of names not yet told for path,
// the file what names: "tracking snapshot".
func (f *ignoredFields) note(what, path string, names []string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var fresh []string
	for _, n := range names {
		if k := path + ": " + n; !f.seen[k] {
			f.seen[k] = true
			fresh = append(fresh, n)
		}
	}
	if len(fresh) > 0 {
		fmt.Fprintf(f.w, "agentd: info: %s %s: ignored unknown fields %s\n", what, path, strings.Join(fresh, ", "))
	}
}

// noteInventory tells f the fields the tracking manifest and snapshot of
// inv were read with that agentd does not know.
func (f *ignoredFields) noteInventory(tr Tracking, inv *Inventory) {
	f.note("tracking manifest", tr.Manifest, inv.Unknown[tr.Manifest])
	f.note("tracking snapshot", tr.Snapshot, inv.Unknown[tr.Snapshot])
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
	newIgnoredFields(os.Stderr).noteInventory(cfg.Tracking, inv)
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
