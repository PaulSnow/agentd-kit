package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// digestMessages are the digest entries agentd posted, in order.
func digestMessages(h *harness) []MessagePayload {
	var out []MessagePayload
	for _, e := range h.events() {
		if e.Kind == KindMessage && e.Message != nil && e.Message.Digest != "" {
			out = append(out, *e.Message)
		}
	}
	return out
}

// The home page lists every waiting decision across both logs and both
// repositories, oldest first, each with its recommendation and how long
// it has waited since it was first seen, not since the page loaded.
func TestDigestPageOldestFirst(t *testing.T) {
	u, _ := fixtureUI(t)
	v, err := u.indexView(pageAt)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i, d := range v.Digest {
		got = append(got, d.Src+"/"+d.Repository+"/"+d.Session+"/"+d.Kind)
		if i > 0 && d.Since.Before(v.Digest[i-1].Since) {
			t.Fatalf("not oldest first: %+v", v.Digest)
		}
		if d.Since.IsZero() || d.Waited != pageAt.Sub(d.Since) {
			t.Fatalf("%s waited %s since %s", got[i], d.Waited, d.Since)
		}
	}
	want := []string{"night/widgets/s-blocked/decision", "night/widgets/txt1/decision", "coord/gadgets/g1/decision"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("digest %v, want %v", got, want)
	}
	if d := v.Digest[0]; d.Recommended != "b" || d.Waited != 2*time.Hour || !strings.HasPrefix(d.Href, "/console/widgets-console#e-") {
		t.Fatalf("first %+v", d)
	}
	body := owner(t, u, "/status")
	digest, needs := strings.Index(body, `id="digest"`), strings.Index(body, `id="needs"`)
	if digest < 0 || needs < digest {
		t.Fatalf("digest section at %d, needs at %d", digest, needs)
	}
	sec := body[digest:needs]
	for _, w := range []string{"Waiting on you, oldest first", "Recommended: b", "waited 2h", "gadgets", "Release gadgets 2.0?"} {
		if !strings.Contains(sec, w) {
			t.Errorf("digest section lacks %q:\n%s", w, sec)
		}
	}
}

// With nothing waiting the section says so.
func TestDigestPageEmpty(t *testing.T) {
	h := newHarness(t, false)
	u, _ := testUI(t, UIConfig{}, newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a"))
	body := owner(t, u, "/status")
	i := strings.Index(body, `id="digest"`)
	if i < 0 || !strings.Contains(body[i:], "Nothing waits on you.") {
		t.Fatalf("no empty digest section")
	}
}

// One entry a day, after digest.at, to digest.console, from agentd; none
// after a restart the same day; a new one the next day, saying nothing
// waits when nothing does.
func TestDigestConsoleOncePerDay(t *testing.T) {
	h := newHarness(t, false)
	h.cfg.Digest = DigestConfig{Console: "widgets", At: "08:00"}
	h.ticks(8, time.Hour) // 00:00 to 07:00
	if m := digestMessages(h); len(m) != 0 {
		t.Fatalf("posted before digest.at: %+v", m)
	}
	h.ticks(3, time.Hour) // 08:00 to 10:00
	m := digestMessages(h)
	if len(m) != 1 || m[0].ID != "digest-2026-10-22" || m[0].Console != "widgets" || m[0].From != "agentd" || m[0].Kind != "info" || m[0].Text != digestNothing {
		t.Fatalf("messages %+v", m)
	}
	h.restart(false)
	h.ticks(3, time.Hour)
	if m := digestMessages(h); len(m) != 1 {
		t.Fatalf("a restart posted again: %+v", m)
	}
	h.clock.advance(22 * time.Hour) // the next day, 11:00
	h.ticks(1, time.Hour)
	if m := digestMessages(h); len(m) != 2 || m[1].ID != "digest-2026-10-23" {
		t.Fatalf("next day %+v", m)
	}
}

// No console, or shadow mode: nothing is posted.
func TestDigestConsoleOff(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		h := newHarness(t, shadow)
		if shadow {
			h.cfg.Digest.Console = "widgets"
		}
		h.ticks(12, time.Hour)
		if m := digestMessages(h); len(m) != 0 {
			t.Fatalf("shadow %v posted %+v", shadow, m)
		}
	}
}

// The entry holds the page's list in its order, each line with its
// recommendation, its wait and its default deadline; a confidential term
// never reaches the entry, the command or the page.
func TestDigestEntryAndCommand(t *testing.T) {
	h, _ := mcpHarness(t)
	h.cfg.ConfidentialPatterns = []string{`(?i)embargoed-[0-9]+`}
	h.cfg.Digest = DigestConfig{Console: "widgets", At: "02:00", Defaults: map[string]Duration{DigestDecision: {48 * time.Hour}}}
	h.alive("s1", "waiting")
	h.status("s1", working)
	first := mustRaise(t, h, decisionArgs(DecisionYesNo, "Should the widget spin faster?", map[string]any{"default": "no"}))
	h.ticks(1, time.Hour)
	mustRaise(t, h, decisionArgs(DecisionText, "What should embargoed-7 say?", nil))
	h.ticks(3, time.Hour)
	ms := digestMessages(h)
	if len(ms) != 1 {
		t.Fatalf("messages %+v", ms)
	}
	lines := strings.Split(ms[0].Text, "\n")
	if len(lines) != 3 || lines[0] != "Waiting on you, oldest first (2):" {
		t.Fatalf("entry %q", ms[0].Text)
	}
	defaultAt := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04")
	want := "[decision] widgets: Should the widget spin faster?. Recommended: Take the first. Waited 2h0m. Default no at " + defaultAt
	if lines[1] != want {
		t.Fatalf("line %q\nwant %q", lines[1], want)
	}
	if !strings.Contains(lines[2], withheldSecret) {
		t.Fatalf("second line %q", lines[2])
	}
	var out bytes.Buffer
	now := h.clock.Now()
	if err := writeDigest(&out, h.cfg, h.events(), ModeLive, now, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "embargoed") || !strings.Contains(out.String(), "Should the widget spin faster?") {
		t.Fatalf("command printed %q", out.String())
	}
	out.Reset()
	if err := writeDigest(&out, h.cfg, h.events(), ModeLive, now, true); err != nil {
		t.Fatal(err)
	}
	var es []DigestEntry
	if err := json.Unmarshal(out.Bytes(), &es); err != nil || len(es) != 2 || strings.Contains(out.String(), "embargoed") {
		t.Fatalf("json %v: %s", err, out.String())
	}
	if es[0].Entry != entryDecision+"s1:"+first && !strings.HasSuffix(es[0].Entry, first) {
		t.Fatalf("first entry %+v", es[0])
	}
	if es[0].DefaultAt == nil || es[0].Default != "no" || es[1].DefaultAt != nil {
		t.Fatalf("defaults %+v", es)
	}
	u, _ := testUI(t, UIConfig{}, newUISource("night", h.cfg, NewLogWriter(h.buf), ModeLive, "host-a"))
	if body := owner(t, u, "/status"); strings.Contains(body, "embargoed") || !strings.Contains(body, "default no at") {
		t.Fatalf("page leaks or lacks the default")
	}
}

// What does not fit the entry is left out whole and counted.
func TestDigestTextCut(t *testing.T) {
	var es []DigestEntry
	for i := 0; i < 50; i++ {
		es = append(es, DigestEntry{Kind: DigestWork, Repository: "widgets", Asked: strings.Repeat("x", 100)})
	}
	text := digestText(es, time.UTC, 1000)
	if len(text) > 1000 || !strings.HasSuffix(text, "more; see the home page or agentd digest.") {
		t.Fatalf("cut %d bytes: %q", len(text), text)
	}
}

// A kind's default deadline applies the default that long after the
// decision was first seen, not before; never with digest.defaults unset;
// never over an owner's answer; and the owner's own deadline wins.
func TestDigestKindDefault(t *testing.T) {
	raiseOne := func(t *testing.T, defaults map[string]Duration) (*harness, string) {
		h, _ := mcpHarness(t)
		h.cfg.Digest.Defaults = defaults
		h.alive("s1", "waiting")
		h.status("s1", working)
		return h, mustRaise(t, h, decisionArgs(DecisionYesNo, "Should the widget spin faster?", map[string]any{"default": "no"}))
	}
	day2 := map[string]Duration{DigestDecision: {48 * time.Hour}}

	t.Run("applied after the kind's deadline", func(t *testing.T) {
		h, id := raiseOne(t, day2)
		h.ticks(47, time.Hour)
		if _, ok := h.sup.Tracks()["s1"].Answers[id]; ok {
			t.Fatal("applied before 48h")
		}
		h.ticks(3, time.Hour)
		a, ok := h.sup.Tracks()["s1"].Answers[id]
		if !ok || !a.ByDefault || a.Note != defaultKindNote+DigestDecision {
			t.Fatalf("answer %+v", a)
		}
	})
	t.Run("unset", func(t *testing.T) {
		h, id := raiseOne(t, nil)
		h.ticks(72, time.Hour)
		if _, ok := h.sup.Tracks()["s1"].Answers[id]; ok {
			t.Fatal("applied with digest.defaults unset")
		}
	})
	t.Run("other kind only", func(t *testing.T) {
		h, id := raiseOne(t, map[string]Duration{DigestCI: {time.Hour}})
		h.ticks(6, time.Hour)
		if _, ok := h.sup.Tracks()["s1"].Answers[id]; ok {
			t.Fatal("a ci deadline applied to a decision")
		}
	})
	t.Run("owner answered in words", func(t *testing.T) {
		h, id := raiseOne(t, day2)
		h.ticks(1, time.Hour)
		yes := true
		if err := RecordAnswer(NewLogWriter(h.buf), h.logState(), h.clock.Now(), "s1", AnswerPayload{ID: id, Answer: &yes, Note: "yes, spin it"}); err != nil {
			t.Fatal(err)
		}
		h.ticks(60, time.Hour)
		a := h.sup.Tracks()["s1"].Answers[id]
		if a.ByDefault || a.Answer == nil || !*a.Answer {
			t.Fatalf("the owner's answer was overridden: %+v", a)
		}
	})
	t.Run("owner's deadline wins", func(t *testing.T) {
		h, id := raiseOne(t, day2)
		if err := RecordDeadline(NewLogWriter(h.buf), h.logState(), h.clock.Now(), "s1", id, h.clock.Now().Add(72*time.Hour)); err != nil {
			t.Fatal(err)
		}
		h.ticks(60, time.Hour)
		if _, ok := h.sup.Tracks()["s1"].Answers[id]; ok {
			t.Fatal("the kind's deadline beat the owner's later one")
		}
		h.ticks(14, time.Hour)
		if a := h.sup.Tracks()["s1"].Answers[id]; !a.ByDefault || a.Note != defaultNote {
			t.Fatalf("answer %+v", a)
		}
	})
}

// The config check refuses a default on a kind that carries none or is
// unknown, a duration that is not positive, a console nobody has and a
// digest.at that is no time.
func TestDigestConfigRefusals(t *testing.T) {
	for _, tc := range []struct {
		d    DigestConfig
		want string
	}{
		{DigestConfig{Defaults: map[string]Duration{DigestDesign: {time.Hour}}}, `digest.defaults.design`},
		{DigestConfig{Defaults: map[string]Duration{"colour": {time.Hour}}}, `digest.defaults.colour`},
		{DigestConfig{Defaults: map[string]Duration{DigestDecision: {0}}}, `must be a positive duration`},
		{DigestConfig{Console: "nowhere"}, `digest.console "nowhere"`},
		{DigestConfig{At: "8am"}, `digest.at "8am"`},
	} {
		cfg := DefaultConfig()
		cfg.Repositories = map[string]Repository{"widgets": {Forge: "gitlab", Project: "example-org/widgets", Console: "widgets"}}
		cfg.Digest = tc.d
		if err := cfg.finish(t.TempDir()); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v, want %q", tc.d, err, tc.want)
		}
	}
	cfg := DefaultConfig()
	cfg.Repositories = map[string]Repository{"widgets": {Forge: "gitlab", Project: "example-org/widgets", Console: "widgets"}}
	cfg.Digest = DigestConfig{Console: "widgets", At: "07:30", Defaults: map[string]Duration{DigestDecision: {48 * time.Hour}, DigestCI: {time.Hour}, DigestFinding: {time.Hour}}}
	if err := cfg.finish(t.TempDir()); err != nil {
		t.Fatalf("a good digest refused: %v", err)
	}
}
