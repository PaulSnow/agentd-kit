package main

import (
	"encoding/json"
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
	cut   *BriefCut // set when Text was cut to policies.brief.max_bytes (#87)
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
	text, cut := capBrief(text, s.cfg.Policies.Brief.MaxBytes)
	return &Brief{Text: text, Parts: parts, cut: cut}, true
}

// parts is the brief's parts, none for no brief.
func (b *Brief) parts() []BriefPart {
	if b == nil {
		return nil
	}
	return b.Parts
}

// briefCut is the brief's cut, nil for none or no brief.
func (b *Brief) briefCut() *BriefCut {
	if b == nil {
		return nil
	}
	return b.cut
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

// BriefCut records a BRIEF.md cut to policies.brief.max_bytes (#87): the
// bytes it would have held, the bytes it holds, and what was dropped.
type BriefCut struct {
	Bytes   int      `json:"bytes"`
	Kept    int      `json:"kept"`
	Dropped []string `json:"dropped"`
}

// briefCutLine is the marker line that ends a cut brief.
func briefCutLine(c BriefCut) string {
	return fmt.Sprintf("[agentd cut this brief from %d to %d bytes, to policies.brief.max_bytes: dropped %s]", c.Bytes, c.Kept, strings.Join(c.Dropped, ", "))
}

// briefText is a started session's BRIEF.md: head, then facts as JSON.
func briefText(head string, facts map[string]any) []byte {
	return []byte(head + "\n\n" + compact(facts) + "\n")
}

// fitBrief is a started session's BRIEF.md under max bytes, before its
// marker line (#87), and the cut, nil when it fits or max is 0. It drops
// the lower-value parts in a fixed order, each only while still over:
// what repeats the head or the owner's message (the own item's design and
// words), the finished sessions, the running sessions' now, the earlier
// thread records oldest first, the other work items, then the cards,
// issues, fleets, releases and running sessions. The head, the owner's
// message, the agent's brief and the repository, branch, worktree and
// issue facts are never dropped: a brief they alone put over max is
// written whole, its marker saying so.
func fitBrief(head string, facts map[string]any, max int) ([]byte, *BriefCut) {
	full := briefText(head, facts)
	if max <= 0 || len(full) <= max {
		return full, nil
	}
	// A copy as plain JSON values, so any part can be dropped.
	var doc map[string]any
	if err := json.Unmarshal([]byte(compact(facts)), &doc); err != nil {
		return full, nil
	}
	cut := &BriefCut{Bytes: len(full)}
	text := full
	over := func() bool {
		text = briefText(head, doc)
		return len(text) > max
	}
	work, _ := doc["owner_work"].(map[string]any)
	items, _ := doc["work_items"].([]any)
	// The session's own item is the one its head is the design of, or the
	// first; items.json holds them all, whole.
	own := ""
	for i, it := range items {
		m, _ := it.(map[string]any)
		if b, _ := m["brief"].(string); i == 0 || m["designed"] == true && strings.TrimSpace(b) != "" && strings.Contains(head, strings.TrimSpace(b)) {
			own = itemID(it)
		}
	}
	steps := []func() bool{
		// What repeats what the brief holds already.
		func() bool {
			done := false
			for _, it := range items {
				m, _ := it.(map[string]any)
				if m == nil || itemID(it) != own {
					continue
				}
				if b, _ := m["brief"].(string); b != "" && strings.Contains(head, strings.TrimSpace(b)) {
					delete(m, "brief")
					cut.Dropped = append(cut.Dropped, "work_items["+own+"].brief (the head above)")
					done = true
				}
				msg, _ := work["message"].(map[string]any)
				if w, _ := m["owner_words"].(string); w != "" && msg != nil && msg["text"] == w {
					delete(m, "owner_words")
					cut.Dropped = append(cut.Dropped, "work_items["+own+"].owner_words (owner_work.message)")
					done = true
				}
			}
			return done
		},
		dropFact(work, "finished", "finished", cut),
		func() bool {
			done := false
			if rs, _ := work["running"].([]any); len(rs) > 0 {
				for _, r := range rs {
					if m, _ := r.(map[string]any); m != nil && m["now"] != nil {
						delete(m, "now")
						done = true
					}
				}
			}
			if done {
				cut.Dropped = append(cut.Dropped, "running[*].now")
			}
			return done
		},
	}
	for _, step := range steps {
		if !over() {
			break
		}
		step()
	}
	// The earlier thread records, oldest first, one at a time.
	if earlier, _ := work["earlier"].([]any); len(earlier) > 0 && over() {
		n := 0
		for n < len(earlier) && over() {
			n++
			work["earlier"] = earlier[n:]
		}
		if n == len(earlier) {
			delete(work, "earlier")
		}
		cut.Dropped = append(cut.Dropped, fmt.Sprintf("earlier[0..%d]", n-1))
	}
	// The other work items, last first.
	for i := len(items) - 1; i >= 0 && over(); i-- {
		if id := itemID(items[i]); id != own {
			items = append(items[:i], items[i+1:]...)
			doc["work_items"] = items
			cut.Dropped = append(cut.Dropped, "work_items["+id+"]")
		}
	}
	for _, k := range []string{"cards", "issues", "fleets", "releases", "running"} {
		if !over() {
			break
		}
		dropFact(work, k, k, cut)()
	}
	if over() {
		cut.Dropped = append(cut.Dropped, fmt.Sprintf("nothing more: the head and the work alone are %d bytes", len(text)))
	}
	cut.Kept = len(text)
	return []byte(string(text) + briefCutLine(*cut) + "\n"), cut
}

// dropFact is a step that drops key from m, recording name.
func dropFact(m map[string]any, key, name string, cut *BriefCut) func() bool {
	return func() bool {
		if _, ok := m[key]; !ok || m == nil {
			return false
		}
		delete(m, key)
		cut.Dropped = append(cut.Dropped, name)
		return true
	}
}

// itemID is a work item's id as decoded JSON holds it.
func itemID(v any) string {
	m, _ := v.(map[string]any)
	s, _ := m["id"].(string)
	return s
}

// capBrief cuts an assembled brief over max bytes at the end, on a UTF-8
// boundary, and ends it with the marker line (#87); nil when it fits.
func capBrief(text []byte, max int) ([]byte, *BriefCut) {
	if max <= 0 || len(text) <= max {
		return text, nil
	}
	kept := strings.TrimSuffix(cutUTF8(string(text), max-1), "…")
	cut := &BriefCut{Bytes: len(text), Kept: len(kept) + 1, Dropped: []string{fmt.Sprintf("the last %d bytes of the brief parts", len(text)-len(kept))}}
	return []byte(kept + "\n" + briefCutLine(*cut) + "\n"), cut
}
