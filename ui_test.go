package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite the UI's golden HTML in testdata/ui")

// fakeWhois names the login behind each caller's IP address, or fails
// with fail.
type fakeWhois struct {
	logins map[string]string
	fail   error
	calls  int
}

func (f *fakeWhois) Login(addr string) (string, error) {
	f.calls++
	if f.fail != nil {
		return "", f.fail
	}
	ip := addr[:strings.LastIndexByte(addr, ':')]
	if l, ok := f.logins[ip]; ok {
		return l, nil
	}
	return "", errors.New("no such peer")
}

// The UI listens on a documentation address; the owner calls from
// 192.0.2.20, a guest from 192.0.2.30.
const pageAddr = "192.0.2.10:8770"

var pageAt = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

// testUI serves sources to the owner, at pageAt, with times in UTC and
// a fixed form token, so its HTML is the same on every machine.
func testUI(t *testing.T, cfg UIConfig, sources ...*uiSource) (*UI, *fakeWhois) {
	t.Helper()
	whois := &fakeWhois{logins: map[string]string{"192.0.2.20": "owner-a", "192.0.2.30": "guest-b", "192.0.2.10": "owner-a", "127.0.0.1": "owner-a"}}
	cfg.Address, cfg.Whois, cfg.Owner = pageAddr, []string{"whois", "{addr}"}, "owner-a"
	u, err := NewUI(cfg, whois, sources, func() time.Time { return pageAt })
	if err != nil {
		t.Fatal(err)
	}
	u.loc, u.token = time.UTC, strings.Repeat("ab", 32)
	return u, whois
}

// get asks the UI for target from remote, with Host host.
func get(u *UI, method, target, host, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	req.RemoteAddr = remote
	w := httptest.NewRecorder()
	u.ServeHTTP(w, req)
	return w
}

// owner is the page at target as the owner is served it.
func owner(t *testing.T, u *UI, target string) string {
	t.Helper()
	w := get(u, "GET", target, pageAddr, "192.0.2.20:51000")
	if w.Code != http.StatusOK {
		t.Fatalf("%s: %d\n%s", target, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// postTo sends a form to path from remote, with origin and token.
func postTo(u *UI, path, origin, token string, form url.Values, remote string) *httptest.ResponseRecorder {
	form.Set("token", token)
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.Host = pageAddr
	req.RemoteAddr = remote
	w := httptest.NewRecorder()
	u.ServeHTTP(w, req)
	return w
}

// ownerPost sends a form to path as the owner, from the UI itself.
func ownerPost(u *UI, path string, form url.Values) *httptest.ResponseRecorder {
	return postTo(u, path, "http://"+pageAddr, u.token, form, "192.0.2.20:51000")
}

// uiFixture is two configs, as a night build and a second run run them,
// each with its own log. The night config's widgets has a working
// session, one blocked on a decision, one done, one whose dialog cleared,
// one whose Questions say "None open.", one asking a text question in
// STATUS.md, and a new item; its console has a feed post, an instruction
// and imported history. The second config runs widgets' request loop,
// with an outside request, a usage-limited session in widgets, and a
// session of gadgets with a yes/no decision.
type uiFixture struct {
	night, coord       *Config
	nightBuf, coordBuf *bytes.Buffer
	nightSrc, coordSrc *uiSource
}

func newUIFixture(t *testing.T) *uiFixture {
	t.Helper()
	f := &uiFixture{nightBuf: &bytes.Buffer{}, coordBuf: &bytes.Buffer{}}
	repos := func() map[string]Repository {
		return map[string]Repository{
			"widgets": {Forge: "gitlab", Project: "example-org/widgets", Console: "widgets-console"},
			"gadgets": {Forge: "github", Project: "example-org/gadgets"},
		}
	}
	dir := t.TempDir()
	workdir := func(name, status string) string {
		d := filepath.Join(dir, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if status != "" {
			writeFile(t, filepath.Join(d, "STATUS.md"), status)
		}
		return d
	}
	night := DefaultConfig()
	night.ConfidentialPatterns = []string{"(?i)embargoed"}
	night.Policies.Budget.RunTokens = 5_000_000
	night.Repositories = repos()
	night.Sessions = map[string]Session{
		"w17":       {Workdir: workdir("w17", "# Widgets blue\n\nItem: #17, **painting**.\n\n- [x] primer\n- [ ] paint\n\n## Questions\n\nNone.\n"), Repository: "widgets"},
		"s-blocked": {Workdir: workdir("s-blocked", ""), Repository: "widgets"},
		"done1":     {Workdir: workdir("done1", "# Done\n\n## Questions\n\nNone.\n\nDONE\n"), Repository: "widgets"},
		"w18":       {Workdir: workdir("w18", ""), Repository: "widgets"},
		"quiet1":    {Workdir: workdir("quiet1", "# Quiet\n\n## Questions\n\nNone open.\n"), Repository: "widgets"},
		"txt1":      {Workdir: workdir("txt1", "# Logo\n\n## Questions\n\nWhich colour should the **logo** be?\n"), Repository: "widgets"},
	}
	coord := DefaultConfig()
	coord.Repositories = repos()
	// coord runs widgets' request loop, which watches its outside requests.
	cw := coord.Repositories["widgets"]
	cw.Sessions = &SessionsPolicy{Root: t.TempDir(), Clone: t.TempDir(), Target: "main"}
	coord.Repositories["widgets"] = cw
	coord.Sessions = map[string]Session{
		"lim1": {Workdir: workdir("lim1", ""), Repository: "widgets"},
		"g1":   {Workdir: workdir("g1", ""), Repository: "gadgets"},
	}
	f.night, f.coord = &night, &coord
	at := func(d time.Duration) time.Time { return pageAt.Add(-d) }
	ev := func(t0 time.Time, session, kind, mode string) Event {
		return Event{V: EventVersion, Time: t0, Session: session, Kind: kind, Mode: mode}
	}
	to := func(t0 time.Time, session string, state State, reason string) Event {
		e := ev(t0, session, KindTransition, ModeLive)
		e.Transition = &TransitionPayload{To: string(state), Reason: reason}
		return e
	}
	observe := func(t0 time.Time, session string, o ObservePayload) Event {
		e := ev(t0, session, KindObserve, ModeLive)
		e.Observe = &o
		return e
	}
	hash := func(s string) string { return shortHash(HashPane(s)) }
	start := ev(at(6*time.Hour), "", KindStart, ModeLive)
	start.Start = &StartPayload{Sessions: []string{"w17"}}
	decision := ev(at(2*time.Hour), "s-blocked", KindDecision, ModeLive)
	decision.Decision = &DecisionPayload{Hash: "h1", Decisions: []Decision{{ID: "Q1", Type: DecisionChooseOne, Question: "Which <b>colour</b>?",
		Context: "</pre><script>y()</script>", Recommendation: "b", Blocking: true,
		Options: []DecisionOption{{Key: "a", Label: "Blue", Consequence: "old logo"}, {Key: "b", Label: "Green", Consequence: "new guide"}}}}}
	doneDecision := ev(at(3*time.Hour), "done1", KindDecision, ModeLive)
	doneDecision.Decision = &DecisionPayload{Hash: "h2", Decisions: []Decision{{ID: "Q1", Type: DecisionYesNo, Question: "Ship done1?", Blocking: true}}}
	escalate := ev(at(4*time.Hour), "w18", KindAction, ModeLive)
	escalate.Act = &ActPayload{Action: ActEscalate, Key: "dialog:w18:1", Message: "w18 shows a dialog: **Allow** the tool?"}
	feed := ev(at(5*time.Hour), "", KindFeed, ModeLive)
	feed.Feed = &FeedPayload{Phase: FeedIntent, Key: "deliver:w17:!4", Repository: "widgets", Console: "widgets-console", Text: "w17 delivered !4"}
	instruction := ev(at(90*time.Minute), "", KindInstruction, ModeOwner)
	instruction.Instruction = &InstructionPayload{ID: "I1", Repositories: []string{"widgets"}, Text: "Ship it on Friday", From: "console:widgets-console", Hash: hash("Ship it on Friday")}
	old := at(72 * time.Hour)
	imported := ev(at(30*time.Minute), "", KindFeed, ModeOwner)
	imported.Feed = &FeedPayload{Phase: FeedImported, Key: importKey("widgets", old, "Old console line"), Repository: "widgets", Text: "Old console line", At: &old, From: "owner-a"}
	item := ev(at(10*time.Minute), "", KindItem, ModeShared)
	item.Item = &ItemPayload{ID: "gitlab/example-org/widgets#19", Kind: ItemIssue, Repository: "gitlab/example-org/widgets",
		Title: "<iframe src=x>", Link: "https://gitlab.example/example-org/widgets/-/issues/19", SourceTime: at(10 * time.Minute)}
	turn := ev(at(20*time.Minute), "w17", KindTurn, ModeLive)
	turn.Turn = &TurnPayload{Transcript: "t17", Line: 3, Role: TurnSummary, Text: "Primed the widgets; **paint** next.", At: at(20 * time.Minute)}
	usage := ev(at(20*time.Minute), "w17", KindUsage, ModeLive)
	usage.Usage = &UsagePayload{Transcript: "t17", Account: "a", Calls: 4, Context: 120_000, Spend: 450_000}
	five, week0, week1, week2 := 30.0, 39.0, 40.0, 42.0
	weekReset := pageAt.Add(3 * 24 * time.Hour)
	read1 := ev(at(30*time.Minute), "", KindAccount, ModeShared)
	read1.Account = &AccountPayload{Account: "a", Method: UsageEndpoint, FiveHour: &five, Weekly: &week1, WeeklyReset: &weekReset}
	read0 := ev(at(70*time.Minute), "", KindAccount, ModeShared)
	read0.Account = &AccountPayload{Account: "a", Method: UsageEndpoint, FiveHour: &five, Weekly: &week0, WeeklyReset: &weekReset}
	read2 := ev(at(5*time.Minute), "", KindAccount, ModeShared)
	read2.Account = &AccountPayload{Account: "a", Method: UsageEndpoint, FiveHour: &five, Weekly: &week2, WeeklyReset: &weekReset}
	for _, e := range []Event{
		start,
		to(at(5*time.Hour), "w17", Working, "transcript moved"),
		observe(at(5*time.Hour), "w17", ObservePayload{State: string(Working), Alive: true, Head: `Widgets blue "><b>bold</b>`}),
		to(at(2*time.Hour), "s-blocked", Idle, "turn ended"),
		decision,
		to(at(3*time.Hour), "done1", Done, "STATUS.md says DONE"),
		doneDecision,
		to(at(4*time.Hour), "w18", BlockedOnOwner, "a dialog"),
		escalate,
		to(at(3*time.Hour), "w18", Working, "transcript moved"),
		to(at(time.Hour), "quiet1", Working, "transcript moved"),
		observe(at(time.Hour), "quiet1", ObservePayload{State: string(Working), Alive: true, StatusQuestions: hash("None open.")}),
		to(at(45*time.Minute), "txt1", BlockedOnOwner, "open Questions"),
		observe(at(45*time.Minute), "txt1", ObservePayload{State: string(BlockedOnOwner), Alive: true, Questions: true,
			StatusQuestions: hash("Which colour should the **logo** be?")}),
		feed, instruction, imported, item, turn, usage, read0, read1, read2,
	} {
		if err := NewLogWriter(f.nightBuf).Append(e); err != nil {
			t.Fatalf("%+v: %v", e, err)
		}
	}
	// An earlier version's coordinator, cw, left its claim and its turn
	// in the log: read, and shown nowhere (#53).
	claim := ev(at(6*time.Hour), "cw", KindCoordinator, ModeShared)
	claim.Coordinator = &CoordinatorPayload{Repository: "widgets"}
	ci := ev(at(15*time.Minute), requestsTrack("widgets"), KindCI, ModeLive)
	ci.CI = &CIPayload{Repository: "widgets", Ref: "!9", URL: "https://gitlab.example/example-org/widgets/-/merge_requests/9", Head: "abc", State: CIPending, Author: "contributor-c", Unresolved: true}
	ask := ev(at(25*time.Minute), "g1", KindDecision, ModeLive)
	ask.Decision = &DecisionPayload{Hash: "h3", Decisions: []Decision{{ID: "Q1", Type: DecisionYesNo, Question: "Release gadgets 2.0?", Why: "the changelog is ready"}}}
	cstart := ev(at(6*time.Hour), "", KindStart, ModeLive)
	cstart.Start = &StartPayload{Sessions: []string{"cw"}}
	for _, e := range []Event{
		cstart, claim,
		to(at(6*time.Hour), "cw", Idle, "turn ended"),
		to(at(40*time.Minute), "lim1", UsageLimited, "usage limit until 14:00"),
		to(at(time.Hour), "g1", Working, "transcript moved"),
		ask, ci,
	} {
		if err := NewLogWriter(f.coordBuf).Append(e); err != nil {
			t.Fatalf("%+v: %v", e, err)
		}
	}
	f.nightSrc = newUISource("night", f.night, NewLogWriter(f.nightBuf), ModeLive, "host-a")
	f.coordSrc = newUISource("coord", f.coord, NewLogWriter(f.coordBuf), ModeLive, "host-a")
	return f
}

// fixtureUI is the UI over the fixture's two logs.
func fixtureUI(t *testing.T) (*UI, *uiFixture) {
	t.Helper()
	f := newUIFixture(t)
	u, _ := testUI(t, UIConfig{}, f.nightSrc, f.coordSrc)
	return u, f
}

// needKeys are the index's needs as source/session/kind.
func needKeys(t *testing.T, u *UI) []string {
	t.Helper()
	v, err := u.indexView(pageAt)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range v.Needs {
		k := n.Src + "/" + n.Session + "/" + n.Kind
		if n.Decision != nil {
			k += ":" + n.Decision.ID
		}
		out = append(out, k)
	}
	return out
}

// Two logs are merged into one view: one top-bar chip per source, and one
// card per repository holding both sources' sessions, each linked to its
// own source.
func TestUIMergesLogs(t *testing.T) {
	u, _ := fixtureUI(t)
	v, err := u.indexView(pageAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Runs) != 2 || v.Runs[0].Src != "night" || v.Runs[1].Src != "coord" || v.Runs[0].Spend != 450_000 || v.Runs[0].Cap != 5_000_000 {
		t.Fatalf("runs %+v", v.Runs)
	}
	var widgets, gadgets *uiRepo
	for _, r := range v.Repos {
		switch r.Name {
		case "widgets":
			widgets = r
		case "gadgets":
			gadgets = r
		}
	}
	if widgets == nil || gadgets == nil || len(v.Repos) != 2 {
		t.Fatalf("repositories %+v", v.Repos)
	}
	names := func(rows []uiRow) string {
		var out []string
		for _, r := range rows {
			out = append(out, r.Src+"/"+r.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(widgets.Working); got != "coord/lim1,night/quiet1,night/s-blocked,night/txt1,night/w17,night/w18" {
		t.Fatalf("widgets working: %s", got)
	}
	if names(widgets.Finished) != "night/done1" || names(gadgets.Working) != "coord/g1" {
		t.Fatalf("widgets %+v, gadgets %+v", widgets, gadgets)
	}
	if len(widgets.Requests) != 1 || !widgets.Requests[0].Outside || widgets.Requests[0].Author != "contributor-c" || !widgets.Requests[0].Unresolved {
		t.Fatalf("widgets requests %+v", widgets.Requests)
	}
	if len(widgets.Items) != 1 || widgets.Items[0].Src != "night" || widgets.Items[0].Items[0].ID != "gitlab/example-org/widgets#19" {
		t.Fatalf("widgets items %+v", widgets.Items)
	}
	body := owner(t, u, "/")
	for _, want := range []string{`href="/session/night/w17"`, `href="/session/coord/g1"`, `href="/console/widgets"`, "1 finished", "context 120k", "spent 450k of 5.0M"} {
		if !strings.Contains(body, want) {
			t.Errorf("index lacks %q", want)
		}
	}
}

// Needs you is worked out from the state as it is now: a cleared dialog
// is gone though it was escalated, a done session's decision is never
// listed, Questions that say "None open." are none, and what is left are
// the open decisions and the usage-limited session, blocking first.
func TestUINeedsFromCurrentState(t *testing.T) {
	u, _ := fixtureUI(t)
	got := strings.Join(needKeys(t, u), ",")
	if want := "night/s-blocked/decision:Q1,night/txt1/decision:" + statusDecision + shortHash(HashPane("Which colour should the **logo** be?")) +
		",coord/lim1/usage-limited,coord/g1/decision:Q1"; got != want {
		t.Fatalf("needs %s\nwant  %s", got, want)
	}
	body := owner(t, u, "/")
	if strings.Contains(body, "Ship done1?") || strings.Contains(body, "None open.") {
		t.Fatalf("a done session's decision or no question is listed:\n%s", body)
	}
}

// Choices are answered by buttons and a text question by a box; an
// answer goes to the source that holds the decision, and the decision
// leaves Needs you once answered.
func TestUIAnswersByButtonAndBox(t *testing.T) {
	u, f := fixtureUI(t)
	body := owner(t, u, "/")
	for _, want := range []string{
		`<button type="submit" name="choice" value="b" class="recommended">`,
		`<button type="submit" name="answer" value="yes">Yes</button>`,
		`<textarea name="text"`,
		"Which colour should the <strong>logo</strong> be?",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("index lacks %q", want)
		}
	}
	if n := strings.Count(body, `<textarea name="text"`); n != 1 {
		t.Fatalf("%d text boxes, want the text question's only", n)
	}
	if w := ownerPost(u, "/answer", url.Values{"src": {"night"}, "session": {"s-blocked"}, "id": {"Q1"}, "choice": {"b"}, "back": {"/"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("choice: %d %s", w.Code, w.Body.String())
	}
	if w := ownerPost(u, "/answer", url.Values{"src": {"coord"}, "session": {"g1"}, "id": {"Q1"}, "answer": {"yes"}, "back": {"/console/gadgets"}}); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/console/gadgets" {
		t.Fatalf("yes: %d %s", w.Code, w.Header().Get("Location"))
	}
	if !strings.Contains(f.nightBuf.String(), `"choice":"b"`) || strings.Contains(f.nightBuf.String(), `"answer":true`) || !strings.Contains(f.coordBuf.String(), `"answer":true`) {
		t.Fatal("an answer went to the wrong log")
	}
	text := url.Values{"src": {"night"}, "session": {"txt1"}, "id": {statusDecision + shortHash(HashPane("Which colour should the **logo** be?"))}, "text": {"Green"}}
	if w := ownerPost(u, "/answer", text); w.Code != http.StatusSeeOther {
		t.Fatalf("text: %d %s", w.Code, w.Body.String())
	}
	if got := strings.Join(needKeys(t, u), ","); got != "coord/lim1/usage-limited" {
		t.Fatalf("needs after the answers: %s", got)
	}
	// A dialog after the answer is the owner's again.
	dialog := Event{V: EventVersion, Time: pageAt, Session: "s-blocked", Kind: KindTransition, Mode: ModeLive,
		Transition: &TransitionPayload{From: string(Idle), To: string(BlockedOnOwner), Reason: "screen shows a permission or confirmation dialog"}}
	if err := NewLogWriter(f.nightBuf).Append(dialog); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(needKeys(t, u), ","); got != "coord/lim1/usage-limited,night/s-blocked/blocked" {
		t.Fatalf("needs after a dialog: %s", got)
	}
	// One source's decision is not answered through another.
	if w := ownerPost(u, "/answer", url.Values{"src": {"coord"}, "session": {"s-blocked"}, "id": {"Q1"}, "choice": {"a"}}); w.Code != http.StatusBadRequest {
		t.Fatalf("an answer through the wrong source: %d", w.Code)
	}
	for _, bad := range []url.Values{
		{"session": {"g1"}, "id": {"Q1"}, "answer": {"yes"}},
		{"src": {"elsewhere"}, "session": {"g1"}, "id": {"Q1"}, "answer": {"yes"}},
		{"src": {"coord"}, "id": {"Q1"}, "answer": {"yes"}},
		{"src": {"coord"}, "session": {"g1"}, "id": {"Q1"}, "answer": {"maybe"}},
	} {
		if w := ownerPost(u, "/answer", bad); w.Code != http.StatusBadRequest {
			t.Errorf("%v: %d", bad, w.Code)
		}
	}
}

// The console shows the repository's messages from every source in time
// order, imported history first, and a reply is recorded as the owner's
// instruction from the UI, in the run of the repository's loop, whose
// sessions read it.
func TestUIConsoleReply(t *testing.T) {
	u, f := fixtureUI(t)
	body := owner(t, u, "/console/widgets")
	var at []int
	for _, s := range []string{"Old console line", "w17 delivered !4", "<strong>Allow</strong> the tool?", "Ship it on Friday"} {
		at = append(at, strings.Index(body, s))
	}
	for i := range at {
		if at[i] < 0 || i > 0 && at[i] < at[i-1] {
			t.Fatalf("messages not newest last %v:\n%s", at, body)
		}
	}
	if !strings.Contains(body, `<input type="hidden" name="src" value="coord"><input type="hidden" name="repository" value="widgets">`) || !strings.Contains(body, `<form id="composer" class="composer" method="post" action="/respond" enctype="multipart/form-data">`) || !strings.Contains(body, "Which &lt;b&gt;colour&lt;/b&gt;?") {
		t.Fatalf("console lacks the reply box or the decision:\n%s", body)
	}
	w := ownerPost(u, "/reply", url.Values{"src": {"coord"}, "repository": {"widgets"}, "text": {"Paint the **logo** green too"}, "back": {"/console/widgets"}})
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/console/widgets" {
		t.Fatalf("reply: %d %s", w.Code, w.Body.String())
	}
	st := NewLogState(f.coord, ModeLive)
	events, _, _ := NewLogWriter(f.coordBuf).Tail(0)
	st.Fold(events)
	var got *Instruction
	for _, in := range st.Instructions {
		if in.From == "ui" {
			got = in
		}
	}
	if got == nil || got.Text != "Paint the **logo** green too" || strings.Join(got.Repositories, ",") != "widgets" {
		t.Fatalf("instruction %+v", got)
	}
	if body := owner(t, u, "/console/widgets"); !strings.Contains(body, "Paint the <strong>logo</strong> green too") {
		t.Fatalf("the reply is not shown:\n%s", body)
	}
	for _, bad := range []url.Values{
		{"src": {"night"}, "repository": {"widgets"}, "text": {"  "}},
		{"src": {"night"}, "repository": {"elsewhere"}, "text": {"x"}},
		{"src": {"night"}, "repository": {"widgets"}, "text": {strings.Repeat("x", maxReply+1)}},
	} {
		if w := ownerPost(u, "/reply", bad); w.Code != http.StatusBadRequest {
			t.Errorf("%v: %d", bad, w.Code)
		}
	}
	if w := get(u, "GET", "/console/elsewhere", pageAddr, "192.0.2.20:51000"); w.Code != http.StatusNotFound {
		t.Fatalf("an unknown console: %d", w.Code)
	}
}

// The session view shows the session's turn, its STATUS.md rendered and
// its events, newest first.
func TestUISessionView(t *testing.T) {
	u, _ := fixtureUI(t)
	body := owner(t, u, "/session/night/w17")
	for _, want := range []string{"Primed the widgets; <strong>paint</strong> next.", "<h3>Widgets blue</h3>", "<li>[x] primer</li>", "→ working · transcript moved", "context 120000, spend 450000"} {
		if !strings.Contains(body, want) {
			t.Errorf("session lacks %q", want)
		}
	}
	if strings.Index(body, "context 120000") > strings.Index(body, "transcript moved") {
		t.Fatal("events are not newest first")
	}
	for _, target := range []string{"/session/night/nobody", "/session/elsewhere/w17", "/session/coord/w17", "/session/night"} {
		if w := get(u, "GET", target, pageAddr, "192.0.2.20:51000"); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", target, w.Code)
		}
	}
}

// A STATUS.md that matches a confidential pattern is withheld whole.
func TestUISessionStatusWithheld(t *testing.T) {
	u, f := fixtureUI(t)
	writeFile(t, filepath.Join(f.night.Sessions["w17"].Workdir, "STATUS.md"), "# Plan\n\nThe embargoed launch.\n")
	body := owner(t, u, "/session/night/w17")
	if strings.Contains(body, "launch") || !strings.Contains(body, "STATUS.md was withheld") {
		t.Fatalf("STATUS.md not withheld:\n%s", body)
	}
}

// The owner is served; anyone else whois names, or that whois cannot
// name, is refused with 403, before any of the logs is read.
func TestUIServesOnlyTheOwner(t *testing.T) {
	u, _ := fixtureUI(t)
	owner(t, u, "/")
	for _, target := range []string{"/", "/console/widgets", "/session/night/w17", "/ui.css"} {
		for _, remote := range []string{"192.0.2.30:51000", "192.0.2.99:51000"} {
			if w := get(u, "GET", target, pageAddr, remote); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "widgets") {
				t.Errorf("%s from %s: %d\n%s", target, remote, w.Code, w.Body.String())
			}
		}
	}
	reply := url.Values{"src": {"night"}, "repository": {"widgets"}, "text": {"x"}}
	if w := postTo(u, "/reply", "http://"+pageAddr, u.token, reply, "192.0.2.30:51000"); w.Code != http.StatusForbidden {
		t.Fatalf("a guest's reply: %d", w.Code)
	}
}

// A request with another Host, or from the UI's own address or this
// machine's loopback, is refused before whois is asked.
func TestUIRefusesOtherHostsAndItself(t *testing.T) {
	u, whois := testUI(t, UIConfig{}, newUIFixture(t).nightSrc)
	for _, c := range []struct{ host, remote string }{
		{"rebound.example:8770", "192.0.2.20:51000"},
		{"", "192.0.2.20:51000"},
		{pageAddr, "192.0.2.10:40000"},
		{pageAddr, "127.0.0.1:40000"},
		{pageAddr, "[::1]:40000"},
		{pageAddr, "[::ffff:127.0.0.1]:40000"},
		{pageAddr, "[::ffff:192.0.2.10]:40000"},
		{pageAddr, ""},
		{pageAddr, "192.0.2.20"},
	} {
		if w := get(u, "GET", "/", c.host, c.remote); w.Code != http.StatusForbidden {
			t.Errorf("host %q from %s: %d", c.host, c.remote, w.Code)
		}
	}
	if whois.calls != 0 {
		t.Fatalf("whois was asked %d times", whois.calls)
	}
}

// The Host check ignores case; whois failing for the owner's address is a
// refusal; and one whois answer serves an address for whoisTTL.
func TestUIHostCaseAndWhois(t *testing.T) {
	u, whois := testUI(t, UIConfig{Hosts: []string{"Page.Example:8770"}}, newUIFixture(t).nightSrc)
	if w := get(u, "GET", "/", "page.example:8770", "192.0.2.20:51000"); w.Code != http.StatusOK {
		t.Fatalf("lower-case host: %d", w.Code)
	}
	get(u, "GET", "/", "page.example:8770", "192.0.2.20:51001")
	if whois.calls != 1 {
		t.Fatalf("whois asked %d times within its TTL", whois.calls)
	}
	now := pageAt
	u.now = func() time.Time { return now }
	now = now.Add(whoisTTL + time.Second)
	whois.fail = errors.New("whois: no answer")
	if w := get(u, "GET", "/", "page.example:8770", "192.0.2.20:51000"); w.Code != http.StatusForbidden || whois.calls != 2 {
		t.Fatalf("whois error for the owner: %d after %d calls", w.Code, whois.calls)
	}
	// A failure is not kept: the next request asks again, and gets in.
	whois.fail = nil
	if w := get(u, "GET", "/", "page.example:8770", "192.0.2.20:51000"); w.Code != http.StatusOK || whois.calls != 3 {
		t.Fatalf("after a whois error: %d after %d calls", w.Code, whois.calls)
	}
}

// Each path takes its own methods; the CSP allows the UI's own stylesheet
// and script only, both served from the binary.
func TestUIMethodsAndAssets(t *testing.T) {
	u, _ := fixtureUI(t)
	for _, c := range []struct {
		method, target string
		code           int
	}{
		{"POST", "/", http.StatusMethodNotAllowed},
		{"POST", "/console/widgets", http.StatusMethodNotAllowed},
		{"GET", "/answer", http.StatusMethodNotAllowed},
		{"GET", "/reply", http.StatusMethodNotAllowed},
		{"POST", "/ui.css", http.StatusMethodNotAllowed},
		{"GET", "/other", http.StatusNotFound},
		{"GET", "/page", http.StatusNotFound},
	} {
		if w := get(u, c.method, c.target, pageAddr, "192.0.2.20:51000"); w.Code != c.code {
			t.Errorf("%s %s: %d", c.method, c.target, w.Code)
		}
	}
	w := get(u, "GET", "/", pageAddr, "192.0.2.20:51000")
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "style-src 'self'", "script-src 'self'", "form-action 'self'", "base-uri 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	// A browser sends a form's POST with Origin null under no-referrer,
	// which checkPost would refuse.
	if rp := w.Header().Get("Referrer-Policy"); rp != "same-origin" {
		t.Fatalf("Referrer-Policy %q", rp)
	}
	if strings.Contains(csp, "unsafe") {
		t.Fatalf("CSP allows unsafe sources: %q", csp)
	}
	for path, ctype := range map[string]string{"/ui.css": "text/css; charset=utf-8", "/ui.js": "text/javascript; charset=utf-8"} {
		w := get(u, "GET", path, pageAddr, "192.0.2.20:51000")
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != ctype || w.Body.Len() < 500 {
			t.Errorf("%s: %d %q %d bytes", path, w.Code, w.Header().Get("Content-Type"), w.Body.Len())
		}
	}
	if strings.Contains(w.Body.String(), "<style") || strings.Contains(w.Body.String(), " style=") || regexp.MustCompile(`<script>`).MatchString(w.Body.String()) {
		t.Fatal("the page carries inline style or script the CSP refuses")
	}
}

// A hostile session's head, decision, item title or message is shown as
// text, never as markup.
func TestUIEscapesEverything(t *testing.T) {
	u, _ := fixtureUI(t)
	for _, target := range []string{"/", "/console/widgets", "/session/night/w17"} {
		body := owner(t, u, target)
		for _, bad := range []string{"<script>y()", "<b>", "<iframe"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s holds %q", target, bad)
			}
		}
	}
	body := owner(t, u, "/")
	for _, want := range []string{"&lt;iframe src=x&gt;", "&#34;&gt;&lt;b&gt;bold&lt;/b&gt;", "&lt;/pre&gt;&lt;script&gt;y()&lt;/script&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("index lacks %q", want)
		}
	}
}

// Marking items seen needs the owner, the UI's own Origin and its token,
// and names the source the items are in; it goes back to a path of the
// UI's own only.
func TestUIMarksSeen(t *testing.T) {
	u, f := fixtureUI(t)
	origin := "http://" + pageAddr
	ids := func() url.Values { return url.Values{"src": {"night"}, "id": {"gitlab/example-org/widgets#19"}} }
	for _, c := range []struct {
		name, origin, token, remote string
	}{
		{"no origin", "", u.token, "192.0.2.20:51000"},
		{"another origin", "http://evil.example", u.token, "192.0.2.20:51000"},
		{"no token", origin, "", "192.0.2.20:51000"},
		{"another token", origin, strings.Repeat("0", 64), "192.0.2.20:51000"},
		{"a guest", origin, u.token, "192.0.2.30:51000"},
		{"this machine", origin, u.token, "127.0.0.1:51000"},
	} {
		if w := postTo(u, "/seen", c.origin, c.token, ids(), c.remote); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", c.name, w.Code)
		}
	}
	if strings.Contains(f.nightBuf.String(), `"kind":"seen"`) {
		t.Fatal("a refused POST marked items seen")
	}
	for _, bad := range []url.Values{{"src": {"night"}}, {"id": {"gitlab/example-org/widgets#19"}}, {"src": {"coord"}, "id": {"gitlab/example-org/widgets#19"}}} {
		if w := ownerPost(u, "/seen", bad); w.Code != http.StatusBadRequest {
			t.Errorf("%v: %d", bad, w.Code)
		}
	}
	for back, want := range map[string]string{"/console/widgets": "/console/widgets", "//evil.example/x": "/", "https://evil.example/": "/", "": "/", "javascript:alert(1)": "/"} {
		form := ids()
		form.Set("back", back)
		w := ownerPost(u, "/seen", form)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
			t.Errorf("back %q: %d to %q", back, w.Code, w.Header().Get("Location"))
		}
	}
	if !strings.Contains(f.nightBuf.String(), `"from":"192.0.2.20:51000"`) {
		t.Fatal("the mark does not say where it came from")
	}
	if v, _ := u.indexView(pageAt); len(v.Repos[0].Items)+len(v.Repos[1].Items) != 0 {
		t.Fatal("a seen item is still listed")
	}
}

// The UI's source is one projection: each view applies only the lines
// appended since the last.
func TestUISourceFollowsTheLog(t *testing.T) {
	f := newUIFixture(t)
	u, _ := testUI(t, UIConfig{}, f.nightSrc)
	owner(t, u, "/")
	n := f.nightSrc.st.Events
	e := Event{V: EventVersion, Time: pageAt, Session: "w18", Kind: KindTransition, Mode: ModeLive, Transition: &TransitionPayload{From: "working", To: string(Failed), Reason: "exited 1"}}
	if err := NewLogWriter(f.nightBuf).Append(e); err != nil {
		t.Fatal(err)
	}
	if body := owner(t, u, "/"); f.nightSrc.st.Events != n+1 || !strings.Contains(body, "exited 1") {
		t.Fatalf("after one more line: %d events applied, want %d", f.nightSrc.st.Events, n+1)
	}
}

// The UI refuses to start without one IP address to listen on, an owner,
// a whois command and a log.
func TestUIConfigIsChecked(t *testing.T) {
	src := newUIFixture(t).nightSrc
	for _, c := range []struct {
		cfg     UIConfig
		sources []*uiSource
	}{
		{UIConfig{Address: "0.0.0.0:8770", Whois: []string{"w"}, Owner: "o"}, []*uiSource{src}},
		{UIConfig{Address: "page.example:8770", Whois: []string{"w"}, Owner: "o"}, []*uiSource{src}},
		{UIConfig{Address: pageAddr, Owner: "o"}, []*uiSource{src}},
		{UIConfig{Address: pageAddr, Whois: []string{"w"}}, []*uiSource{src}},
		{UIConfig{Address: pageAddr, Whois: []string{"w"}, Owner: "o"}, nil},
	} {
		if _, err := NewUI(c.cfg, &fakeWhois{}, c.sources, time.Now); err == nil {
			t.Errorf("%+v accepted", c.cfg)
		}
	}
}

// The config's ui block takes other configs as logs, relative to it; a
// leftover page block is refused; and the sources are named after their
// config files, each with its shadow when ui.shadow is set.
func TestUIConfigLogs(t *testing.T) {
	dir := t.TempDir()
	minimal := func(name, extra string) string {
		p := filepath.Join(dir, name)
		writeFile(t, p, `{"event_log": "`+strings.TrimSuffix(name, ".json")+`.jsonl"`+extra+`}`)
		return p
	}
	minimal("coord.json", "")
	main := minimal("night.json", `, "ui": {"address": "`+pageAddr+`", "whois": ["w"], "owner": "o", "logs": ["coord.json"], "shadow": true}`)
	cfg, err := LoadConfig(main)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.UI.Logs) != 1 || cfg.UI.Logs[0] != filepath.Join(dir, "coord.json") {
		t.Fatalf("logs %v", cfg.UI.Logs)
	}
	sources, logs, err := uiSources(cfg, "host-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range logs {
		l.Close()
	}
	var got []string
	for _, s := range sources {
		got = append(got, s.Name+":"+s.st.Mode)
	}
	if strings.Join(got, ",") != "night:live,night-shadow:shadow,coord:live,coord-shadow:shadow" {
		t.Fatalf("sources %v", got)
	}
	old := minimal("old.json", `, "page": {"address": "`+pageAddr+`"}`)
	if _, err := LoadConfig(old); err == nil || !strings.Contains(err.Error(), "page") {
		t.Fatalf("a page block: %v", err)
	}
}

// whois output is a login on its first line, or the JSON a private
// network's whois prints; anything else names no one.
func TestParseWhois(t *testing.T) {
	for out, want := range map[string]string{
		"owner-a\n":       "owner-a",
		"owner-a\nmore\n": "owner-a",
		`{"Node": {"Name": "host-b"}, "UserProfile": {"LoginName": "owner-a"}}`: "owner-a",
	} {
		if got, err := parseWhois([]byte(out)); err != nil || got != want {
			t.Errorf("%q: %q, %v", out, got, err)
		}
	}
	for _, out := range []string{"", "  \n", `{"UserProfile": {}}`, `{"UserProfile": `} {
		if got, err := parseWhois([]byte(out)); err == nil {
			t.Errorf("%q named %q", out, got)
		}
	}
	login, err := ExecWhois{Argv: []string{"sh", "-c", `echo "login-for-$1"`, "sh", "{addr}"}, Timeout: 5 * time.Second}.Login("192.0.2.20:51000")
	if err != nil || login != "login-for-192.0.2.20:51000" {
		t.Fatalf("exec whois: %q, %v", login, err)
	}
}

// The old console's history is imported once, in time order, screened:
// a second import of the same lines adds nothing, and a file with a bad
// line adds nothing at all.
func TestConsoleImport(t *testing.T) {
	f := newUIFixture(t)
	log := NewLogWriter(f.nightBuf)
	events, _, _ := log.Tail(0)
	lines := `{"at": "2026-10-20T09:00:00Z", "from": "owner-a", "text": "Second line"}
{"at": "2026-10-19T09:00:00Z", "from": "bot", "text": "First line"}

{"at": "2026-10-21T09:00:00Z", "from": "owner-a", "text": "The embargoed plan"}
`
	added, skipped, err := ImportConsole(f.night, log, events, pageAt, "widgets", strings.NewReader(lines))
	if err != nil || added != 3 || skipped != 0 {
		t.Fatalf("import: %d added, %d skipped, %v", added, skipped, err)
	}
	if strings.Contains(f.nightBuf.String(), "embargoed") {
		t.Fatal("a confidential line was imported")
	}
	events, _, _ = log.Tail(0)
	if added, skipped, err = ImportConsole(f.night, log, events, pageAt, "widgets", strings.NewReader(lines)); err != nil || added != 0 || skipped != 3 {
		t.Fatalf("again: %d added, %d skipped, %v", added, skipped, err)
	}
	size := f.nightBuf.Len()
	for _, bad := range []string{
		`{"at": "2026-10-22T09:00:00Z", "text": "New"}` + "\nnot json\n",
		`{"at": "2026-10-22T09:00:00Z", "text": "New", "repository": "elsewhere"}`,
		`{"text": "no time"}`,
		`{"at": "2026-10-22T09:00:00Z", "text": "New", "from": 3}`,
	} {
		if _, _, err := ImportConsole(f.night, log, events, pageAt, "widgets", strings.NewReader(bad)); err == nil {
			t.Errorf("%q imported", bad)
		}
	}
	if f.nightBuf.Len() != size {
		t.Fatal("a refused import appended lines")
	}
	u, _ := testUI(t, UIConfig{}, newUISource("night", f.night, log, ModeLive, "host-a"))
	body := owner(t, u, "/console/widgets")
	first, second, old := strings.Index(body, "First line"), strings.Index(body, "Second line"), strings.Index(body, "Old console line")
	if old < 0 || second > old || first > second || first < 0 || !strings.Contains(body, withheldMessage) {
		t.Fatalf("imported lines out of order (%d, %d, %d):\n%s", first, second, old, body)
	}
	// Old history never pushes what is new out of the console.
	var many strings.Builder
	for i := 0; i < uiKeptMessages+50; i++ {
		many.WriteString(`{"at": "2026-09-01T09:00:00Z", "text": "old ` + strconv.Itoa(i) + `"}` + "\n")
	}
	events, _, _ = log.Tail(0)
	if _, _, err := ImportConsole(f.night, log, events, pageAt, "widgets", strings.NewReader(many.String())); err != nil {
		t.Fatal(err)
	}
	if body := owner(t, u, "/console/widgets"); !strings.Contains(body, "w17 delivered !4") || !strings.Contains(body, "Ship it on Friday") {
		t.Fatalf("new messages pushed out:\n%s", body)
	}
	// The fold ignores imported lines: the feed is as it was.
	st := NewLogState(f.night, ModeLive)
	events, _, _ = log.Tail(0)
	if st.Fold(events); len(st.Feed.Fed) != 1 {
		t.Fatalf("feed fold: %v", st.Feed.Fed)
	}
}

// The index, console and session pages match their golden HTML. Run
// go test -run TestUIGolden -update to rewrite them after a change.
func TestUIGolden(t *testing.T) {
	u, _ := fixtureUI(t)
	for name, target := range map[string]string{"index": "/", "console": "/console/widgets", "session": "/session/night/w17"} {
		body := owner(t, u, target)
		path := filepath.Join("testdata", "ui", name+".html")
		if *updateGolden {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update to write it)", err)
		}
		if body != string(want) {
			t.Errorf("%s differs from %s (run with -update to rewrite it):\n%s", target, path, body)
		}
	}
}

// /accounts.json is the account reports (#64): the readings and what
// the sessions charged to each account.
func TestUIAccountsJSON(t *testing.T) {
	u, _ := fixtureUI(t)
	var reps []AccountReport
	if err := json.Unmarshal([]byte(owner(t, u, "/accounts.json")), &reps); err != nil {
		t.Fatal(err)
	}
	if len(reps) != 1 || reps[0].Account != "a" || reps[0].Weekly == nil || *reps[0].Weekly != 42 || reps[0].ThisWeek.Total != 450_000 || len(reps[0].Week) != 3 {
		t.Fatalf("reports %+v", reps)
	}
}

// The page is built for a phone: a viewport meta, no fixed width over
// 20rem anywhere in the stylesheet, long text wrapping, preformatted text
// wrapping too, and light and dark colours from one token set.
func TestUIFitsAPhone(t *testing.T) {
	u, _ := fixtureUI(t)
	for _, target := range []string{"/", "/console/widgets", "/session/night/w17"} {
		if body := owner(t, u, target); !strings.Contains(body, `<meta name="viewport" content="width=device-width, initial-scale=1">`) {
			t.Errorf("%s has no viewport meta", target)
		}
	}
	css, err := uiAssets.ReadFile("ui/ui.css")
	if err != nil {
		t.Fatal(err)
	}
	text := string(css)
	for _, m := range regexp.MustCompile(`(?:^|[\s;{])((?:min-)?width|flex(?:-basis)?)\s*:\s*([^;}]+)`).FindAllStringSubmatch(text, -1) {
		for _, v := range regexp.MustCompile(`([\d.]+)(rem|em|px)`).FindAllStringSubmatch(m[2], -1) {
			n, _ := strconv.ParseFloat(v[1], 64)
			if v[2] == "px" && n > 320 || v[2] != "px" && n > 20 {
				t.Errorf("%s: %s is wider than a phone", m[1], m[2])
			}
		}
	}
	for _, want := range []string{"overflow-wrap: anywhere", "white-space: pre-wrap", "flex-wrap: wrap", "@media (prefers-color-scheme: dark)", "max-width: 100%"} {
		if !strings.Contains(text, want) {
			t.Errorf("the stylesheet lacks %q", want)
		}
	}
	if strings.Contains(text, "white-space: nowrap") && !regexp.MustCompile(`\.badge \{[^}]*white-space: nowrap`).MatchString(text) {
		t.Error("something other than a badge refuses to wrap")
	}
}

// A shadow source over a live source's log shows its own run, not the
// owner's records and shared facts again: an instruction and an item are
// listed once.
func TestUIShadowEcho(t *testing.T) {
	f := newUIFixture(t)
	shadow := newUISource("night-shadow", f.night, NewLogWriter(f.nightBuf), ModeShadow, "host-a")
	shadow.echo = true
	u, _ := testUI(t, UIConfig{}, f.nightSrc, shadow)
	if body := owner(t, u, "/console/widgets"); strings.Count(body, "Ship it on Friday") != 1 || strings.Count(body, "Old console line") != 1 {
		t.Fatalf("console repeats the owner's lines:\n%s", body)
	}
	if body := owner(t, u, "/"); strings.Count(body, "1 new item") != 1 {
		t.Fatalf("items listed twice:\n%s", body)
	}
}

// A STATUS.md that is a FIFO or a link is not read: the session view
// says so, and nothing hangs, the index neither, where a session's
// Questions are read again.
func TestUISessionStatusNotRegular(t *testing.T) {
	u, f := fixtureUI(t)
	wd := f.night.Sessions["w17"].Workdir
	status := filepath.Join(wd, "STATUS.md")
	if err := os.Remove(status); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(status, 0o600); err != nil {
		t.Fatal(err)
	}
	if body := owner(t, u, "/session/night/w17"); !strings.Contains(body, "STATUS.md could not be read.") {
		t.Fatalf("a FIFO:\n%s", body)
	}
	if err := os.Remove(status); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "elsewhere")
	writeFile(t, secret, "# Elsewhere\n\nNot the session's.\n")
	if err := os.Symlink(secret, status); err != nil {
		t.Fatal(err)
	}
	if body := owner(t, u, "/session/night/w17"); strings.Contains(body, "Not the session") || !strings.Contains(body, "STATUS.md could not be read.") {
		t.Fatalf("a link:\n%s", body)
	}
}

// The index reads STATUS.md's Questions as a regular file only: a FIFO
// holds no request, and the decision is shown as changed.
func TestUIQuestionsNotRegular(t *testing.T) {
	u, f := fixtureUI(t)
	status := filepath.Join(f.night.Sessions["txt1"].Workdir, "STATUS.md")
	if err := os.Remove(status); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(status, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { done <- get(u, "GET", "/", pageAddr, "192.0.2.20:51000").Body.String() }()
	select {
	case body := <-done:
		if !strings.Contains(body, "changed since agentd last read them") {
			t.Fatalf("index:\n%s", body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a FIFO for STATUS.md holds the index")
	}
}

// A session at a dialog with a decision open that does not block is
// listed twice: by its decision, and as blocked.
func TestUIDialogBesideNonBlockingDecision(t *testing.T) {
	u, f := fixtureUI(t)
	dialog := Event{V: EventVersion, Time: pageAt, Session: "g1", Kind: KindTransition, Mode: ModeLive,
		Transition: &TransitionPayload{From: string(Working), To: string(BlockedOnOwner), Reason: "screen shows a permission or confirmation dialog"}}
	if err := NewLogWriter(f.coordBuf).Append(dialog); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(needKeys(t, u), ",")
	if !strings.Contains(got, "coord/g1/blocked") || !strings.Contains(got, "coord/g1/decision:Q1") {
		t.Fatalf("needs %s", got)
	}
}
