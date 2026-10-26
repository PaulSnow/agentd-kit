package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BriefPart is one part of a configured session's brief (#50): its path and the
// hash of what was read from it.
type BriefPart struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

// assembleBrief reads a session's brief parts in order: the brief
// they make, each part's trailing space trimmed and a blank line between
// them, and each part's hash. A part that cannot be read is an error
// naming its path.
func assembleBrief(paths []string) ([]byte, []BriefPart, error) {
	var b strings.Builder
	parts := make([]BriefPart, 0, len(paths))
	for i, p := range paths {
		text, err := os.ReadFile(p)
		if err != nil {
			var pe *os.PathError
			if errors.As(err, &pe) {
				err = pe.Err
			}
			return nil, nil, fmt.Errorf("brief part %s cannot be read: %w", p, err)
		}
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(strings.TrimRight(string(text), " \t\r\n"))
		parts = append(parts, BriefPart{Path: p, Hash: shortHash(HashPane(string(text)))})
	}
	b.WriteString("\n")
	return []byte(b.String()), parts, nil
}

// Brief is a session's brief as read from its parts for a launch.
type Brief struct {
	Text  []byte
	Parts []BriefPart
}

// readBrief reads a session's brief parts, sessions.NAME.brief, before it
// is started or rotated (verb says which). A session with no parts has
// none, and its BRIEF.md is left as it is. A part that cannot be read refuses the
// launch: it is skipped, naming the part, and escalated once per cause;
// the brief written last stays.
func (s *Supervisor) readBrief(name string, sess Session, t *Track, now time.Time, verb string) (*Brief, bool) {
	if len(sess.Brief) == 0 {
		return nil, true
	}
	text, parts, err := assembleBrief(sess.Brief)
	if err != nil {
		key := "brief:" + name + ":" + shortHash(HashPane(err.Error()))
		s.skipOnce(name, key, "not "+verb+": "+err.Error(), t, nil)
		s.escalate(name, key, fmt.Sprintf("%s is not %s: %v. Fix the part, or the brief listed under sessions.%s; agentd goes ahead once the brief can be written.", name, verb, err, name), t, now)
		return nil, false
	}
	return &Brief{Text: text, Parts: parts}, true
}

// parts is the brief's parts, none for no brief.
func (b *Brief) parts() []BriefPart {
	if b == nil {
		return nil
	}
	return b.Parts
}

// write writes the brief as workdir's BRIEF.md, inside the launch's action
// so a launch that does not happen leaves the brief as it was. Shadow mode
// never runs it.
func (b *Brief) write(workdir string) error {
	if b == nil {
		return nil
	}
	return writeAtomic(filepath.Join(workdir, "BRIEF.md"), b.Text)
}
