package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// BriefPart is one part of a coordinator's brief (#50): its path and the
// hash of what was read from it.
type BriefPart struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

// assembleBrief reads a coordinator's brief parts in order: the brief
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

// writeBrief writes a coordinator's BRIEF.md from its parts before it is
// started or rotated, so a changed part reaches the next start, and says
// the parts' hashes. A session with no parts keeps its BRIEF.md as it is.
// A part that cannot be read refuses the launch: it is skipped, naming
// the part, and escalated once per cause; the brief written last stays.
// Shadow mode writes nothing.
func (s *Supervisor) writeBrief(name string, sess Session, t *Track, now time.Time) ([]BriefPart, bool) {
	if len(sess.Brief) == 0 {
		return nil, true
	}
	brief, parts, err := assembleBrief(sess.Brief)
	if err == nil && !s.shadow {
		err = writeAtomic(filepath.Join(sess.Workdir, "BRIEF.md"), brief)
	}
	if err != nil {
		key := "brief:" + name + ":" + shortHash(HashPane(err.Error()))
		s.skipOnce(name, key, "not starting: "+err.Error(), t, nil)
		s.escalate(name, key, fmt.Sprintf("%s is not started: %v. Fix the part, or the brief listed under its repository's coordinator; agentd starts it once the brief can be written.", name, err), t, now)
		return nil, false
	}
	// The pack written just before the launch has the parts too; the
	// launch's event records them for a replay.
	if !s.shadow {
		t.Brief = parts
	}
	return parts, true
}
