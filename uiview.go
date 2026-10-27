package main

import (
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The UI's views (#41), built from each source's state as it is now:
// what waits on the owner is worked out from the current state alone,
// never from the history of escalations, so an item goes when its
// condition clears.

//go:embed ui/ui.html
var uiHTML string

// uiTemplates are the views' templates; ctx pairs a view with one of its
// parts for a template the part is rendered by.
var uiTemplates = template.Must(template.New("ui").Funcs(template.FuncMap{
	"markdown": markdown,
	"pct":      func(v *float64) string { return fmt.Sprintf("%.0f%%", *v) },
	"yes":      func(b *bool) bool { return b != nil && *b },
	"ctx":      func(p, v any) map[string]any { return map[string]any{"P": p, "V": v} },
}).Parse(uiHTML))

// uiPage is what every view carries: the form token, the time the view
// was built and the zone it shows times in, and the path a form sends the
// owner back to.
type uiPage struct {
	Title string
	Token string
	Back  string
	Now   time.Time
	loc   *time.Location
}

// When is t as the owner reads it, or "-".
func (p uiPage) When(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.In(p.loc).Format("2006-01-02 15:04")
}

// Ago is how long before the view t was, shortly: "5m", "3h", "2d".
func (p uiPage) Ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := p.Now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// Tokens is a token count, shortly: "950", "12k", "1.2M".
func (uiPage) Tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

// UIDecision is one open decision a session raised, with the owner's
// deadline on it. A withheld decision is shown by its id and type only,
// with Withheld saying why, and no form.
type UIDecision struct {
	Session    string
	Repository string
	Decision
	OwnerDeadline *DeadlineRecord
	Withheld      string
}

// Why a decision is withheld.
const (
	withheldSecret  = "The details were withheld because they matched a confidential pattern. Look at the session directly."
	withheldChanged = "STATUS.md's Questions changed since agentd last read them; they are shown after its next pass."
)

// DecisionsFrom lists the decisions the report's sessions wait on the
// owner for, each session's in the order it raised them: STATUS.md's
// Questions first, as a text decision whose context is the section read
// from STATUS.md now, offered only while it is the section agentd last
// saw, so an answer is to the text the owner read; then those of
// questions.json, then those raised through the tools. A decision already
// answered is not listed, nor is any of a done session's, nor STATUS.md's
// Questions once the file asks none. Every text is screened again.
func DecisionsFrom(cfg *Config, st *LogState, rep Report) []UIDecision {
	secret := confidentialMatcher(cfg)
	repoOf := map[string]string{}
	for _, rr := range rep.Repositories {
		for _, c := range rr.Cases {
			repoOf[c.Session] = uiRepoName(rr)
		}
	}
	var out []UIDecision
	for _, n := range sortedKeys(repoOf) {
		t := st.Tracks[n]
		if t == nil || t.State == Done {
			continue
		}
		var ds []UIDecision
		sess, configured := cfg.Sessions[n]
		if t.StatusQuestions != "" && configured {
			d := UIDecision{Decision: statusDecisionFor(t.StatusQuestions)}
			sts, err := readStatusRegular(filepath.Join(sess.Workdir, "STATUS.md"))
			switch q := sts.Questions; {
			case err == nil && (q == "" || sts.Done):
				d.ID = "" // it asks nothing now
			case err != nil || shortHash(HashPane(q)) != t.StatusQuestions:
				d.Withheld = withheldChanged
			default:
				d.Context = q
			}
			if d.ID != "" {
				ds = append(ds, d)
			}
		}
		for _, o := range t.open() {
			ds = append(ds, UIDecision{Decision: o})
		}
		for _, d := range ds {
			if _, ok := t.answer(d.ID); ok {
				continue
			}
			d.Session, d.Repository = n, repoOf[n]
			if dl, ok := t.deadline(d.Decision); ok {
				d.OwnerDeadline = &dl
			}
			if d.Withheld == "" && (d.Decision.Withheld || secret(decisionTexts(d.Decision)...)) {
				d.Withheld = withheldSecret
			}
			if d.Withheld != "" {
				d.Decision = Decision{ID: d.ID, Type: d.Type, Blocking: d.Blocking}
			}
			out = append(out, d)
		}
	}
	return out
}

// uiRepoName is the name a repository is shown and merged by: its
// configured name, or its key when no config names it.
func uiRepoName(rr ReportRepo) string {
	if rr.Name != "" {
		return rr.Name
	}
	if rr.Repository != "" {
		return rr.Repository
	}
	return "(no repository)"
}

// uiNeed is one thing waiting on the owner: an open decision, or a
// session that cannot go on without the owner.
type uiNeed struct {
	Src        string
	Repository string
	Session    string
	Kind       string // decision, blocked, usage-limited, crash-looping, stopped, failed
	Since      time.Time
	Text       string // a session's reason
	Decision   *UIDecision
}

// needs lists what waits on the owner in src: every open decision, and
// every session of the report blocked on the owner, usage-limited,
// crash-looping, stopped or failed. A done session is never listed, and
// a session blocked on its Questions or with a blocking decision open is
// listed by its decisions alone.
func (src *uiSource) needs(cfg *Config, rep Report, asked map[string]time.Time) []uiNeed {
	secret := confidentialMatcher(cfg)
	var out []uiNeed
	decided := map[string]bool{}
	for _, d := range DecisionsFrom(cfg, src.st, rep) {
		d := d
		// A blocking decision, or STATUS.md's Questions, is what the
		// session waits on; one that does not block leaves a dialog
		// its own card.
		if d.Blocking || strings.HasPrefix(d.ID, statusDecision) {
			decided[d.Session] = true
		}
		t := src.st.Tracks[d.Session]
		since, ok := asked[d.Session+" "+d.ID]
		if !ok {
			since = t.Since
		}
		out = append(out, uiNeed{Src: src.Name, Repository: d.Repository, Session: d.Session, Kind: "decision", Since: since, Decision: &d})
	}
	for _, rr := range rep.Repositories {
		for _, c := range rr.Cases {
			t := src.st.Tracks[c.Session]
			if t == nil || t.State == Done {
				continue
			}
			kind := ""
			switch {
			case t.Stopped != "":
				kind = "stopped"
			case t.State == BlockedOnOwner && !decided[c.Session] && t.StatusQuestions == "":
				// A dialog or a tool waiting. Blocked on STATUS.md's
				// Questions, it is listed by them until they are
				// answered, and then the answer is on its way.
				kind = "blocked"
			case t.State == UsageLimited, t.State == CrashLooping, t.State == Failed:
				kind = string(t.State)
			}
			if kind == "" {
				continue
			}
			why := t.Reason
			if t.Stopped != "" {
				why = "stopped: " + t.Stopped
			}
			if secret(why) {
				why = "The reason was withheld because it matched a confidential pattern."
			}
			out = append(out, uiNeed{Src: src.Name, Repository: uiRepoName(rr), Session: c.Session, Kind: kind, Since: t.Since, Text: why})
		}
	}
	return out
}

// uiRun is one source's run, for the top bar.
type uiRun struct {
	Src        string
	Mode       string
	Started    time.Time
	Stopped    time.Time
	Spend      int64
	SpendKnown bool
	Cap        int64
	StopAt     *time.Time
	Digest     string
}

// uiRow is one session as a repository's card lists it.
type uiRow struct {
	Src        string
	Name       string
	State      string
	Since      time.Time
	Head       string
	Context    int64
	HasContext bool
	Spend      int64
	HasSpend   bool
	LastTurn   time.Time
}

// uiRequest is one open request of a repository: delivered by a session,
// or an outside one the repository's loop watches (#36, #53).
type uiRequest struct {
	Src        string
	Ref        string
	Session    string
	URL        string
	CI         string
	Verdict    string
	Jobs       []string
	Outside    bool
	Author     string
	Draft      bool
	Unresolved bool
}

// uiItems are a source's unseen items of one repository.
type uiItems struct {
	Src   string
	Items []ReportItem
}

// uiRepo is one repository's card, merged across the sources.
type uiRepo struct {
	Name     string
	NeedsYou int
	Working  []uiRow
	Finished []uiRow
	Requests []uiRequest
	Items    []uiItems
	Console  bool // a source configures it, so it has a console view
}

// uiAccount is an account's latest usage reading across the sources
// (#39): Over says a utilization is at or over the limit, Stale that the
// reading is too old to place a session by.
type uiAccount struct {
	AccountRecord
	Name  string
	Over  bool
	Stale bool
	Error string // the latest error since the numbers, screened
}

// uiOtherConsole is a console without a repository (#42), merged across
// the sources: its title and its latest message.
type uiOtherConsole struct {
	Name  string
	Title string
	Last  time.Time
}

// uiIndex is the index view.
type uiIndex struct {
	uiPage
	Runs     []uiRun
	Accounts []uiAccount
	Jobs     []uiJob
	Needs    []uiNeed
	Repos    []*uiRepo
	Consoles []*uiOtherConsole // listed after the repositories, by name
}

// view brings src up to date and reads what every view needs from it:
// the config with the sessions agentd started, the report, and
// when each open decision was first seen.
func (src *uiSource) view(now time.Time) (*Config, Report, error) {
	if err := src.catchUp(); err != nil {
		return nil, Report{}, err
	}
	cfg := withRuntime(src.cfg, src.st.Runtime)
	return cfg, ReportFrom(cfg, src.st, src.host, now), nil
}

// row is session c of src as a card lists it.
func (src *uiSource) row(c ReportCase) uiRow {
	t := src.st.Tracks[c.Session]
	live := src.st.Mode == ModeLive
	r := uiRow{Src: src.Name, Name: c.Session, State: c.State, Since: c.Since, Head: c.Head}
	if u, ok := t.current(); ok {
		r.Context, r.HasContext = u.Context, true
	}
	r.Spend, r.HasSpend = t.spend(live)
	if n := len(t.Turns); n > 0 {
		r.LastTurn = t.Turns[n-1].At
	}
	return r
}

// indexView is the index: the runs, what needs the owner, and every
// repository's card, merged by name across the sources.
func (u *UI) indexView(now time.Time) (*uiIndex, error) {
	v := &uiIndex{uiPage: uiPage{Title: "Overview", Token: u.token, Back: "/", Now: now, loc: u.loc}}
	repos := map[string]*uiRepo{}
	get := func(name string) *uiRepo {
		r := repos[name]
		if r == nil {
			r = &uiRepo{Name: name}
			repos[name] = r
		}
		return r
	}
	consoles := map[string]*uiOtherConsole{}
	for _, src := range u.sources {
		if err := src.addIndex(v, get, now); err != nil {
			return nil, err
		}
		src.addConsoles(consoles)
	}
	for _, c := range consoles {
		v.Consoles = append(v.Consoles, c)
	}
	sort.Slice(v.Consoles, func(i, j int) bool { return v.Consoles[i].Name < v.Consoles[j].Name })
	for _, n := range v.Needs {
		get(n.Repository).NeedsYou++
	}
	sort.Slice(v.Accounts, func(i, j int) bool { return v.Accounts[i].Name < v.Accounts[j].Name })
	sortNeeds(v.Needs)
	for _, r := range repos {
		for _, rows := range [][]uiRow{r.Working, r.Finished} {
			sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
		}
		v.Repos = append(v.Repos, r)
	}
	sort.Slice(v.Repos, func(i, j int) bool {
		a, b := v.Repos[i], v.Repos[j]
		if a.NeedsYou != b.NeedsYou {
			return a.NeedsYou > b.NeedsYou
		}
		if aw, bw := len(a.Working), len(b.Working); (aw > 0) != (bw > 0) {
			return aw > 0
		}
		return a.Name < b.Name
	})
	return v, nil
}

// addConsoles adds src's consoles without a repository to consoles, by
// name: the first title given, and the latest message.
func (src *uiSource) addConsoles(consoles map[string]*uiOtherConsole) {
	src.mu.Lock()
	defer src.mu.Unlock()
	for name, con := range src.cfg.Consoles {
		c := consoles[name]
		if c == nil {
			c = &uiOtherConsole{Name: name}
			consoles[name] = c
		}
		if c.Title == "" {
			c.Title = con.Title
		}
		if ms := src.messages[name]; len(ms) > 0 && ms[len(ms)-1].At.After(c.Last) {
			c.Last = ms[len(ms)-1].At
		}
	}
}

// sortNeeds puts blocking decisions and stuck sessions first, then the
// longest waiting.
func sortNeeds(ns []uiNeed) {
	urgent := func(n uiNeed) bool { return n.Decision == nil || n.Decision.Blocking }
	sort.SliceStable(ns, func(i, j int) bool {
		if a, b := urgent(ns[i]), urgent(ns[j]); a != b {
			return a
		}
		return ns[i].Since.Before(ns[j].Since)
	})
}

// addIndex adds src's run, needs and repositories to the index.
func (src *uiSource) addIndex(v *uiIndex, get func(string) *uiRepo, now time.Time) error {
	src.mu.Lock()
	defer src.mu.Unlock()
	cfg, rep, err := src.view(now)
	if err != nil {
		return err
	}
	live := src.st.Mode == ModeLive
	run := uiRun{Src: src.Name, Mode: src.st.Mode, Started: src.st.Started, Stopped: src.st.Stopped,
		Cap: cfg.Policies.Budget.RunTokens, StopAt: cfg.Policies.Budget.StopAt, Digest: rep.OwnerDigest}
	for _, t := range src.st.Tracks {
		if sp, ok := t.spend(live); ok {
			run.Spend, run.SpendKnown = run.Spend+sp+t.SpendBase, true
		}
	}
	v.Runs = append(v.Runs, run)
	src.addAccounts(v, cfg, now)
	src.addJobs(v, cfg)
	v.Needs = append(v.Needs, src.needs(cfg, rep, src.askedAt)...)
	for _, rr := range rep.Repositories {
		r := get(uiRepoName(rr))
		if _, ok := cfg.Repositories[rr.Name]; ok {
			r.Console = true
		}
		for _, c := range rr.Cases {
			row := src.row(c)
			switch {
			case c.State == string(Done):
				r.Finished = append(r.Finished, row)
			default:
				r.Working = append(r.Working, row)
			}
		}
		for _, d := range rr.Deliveries {
			if d.CI == CIMerged || d.CI == CIClosed {
				continue
			}
			r.Requests = append(r.Requests, uiRequest{Src: src.Name, Ref: d.Ref, Session: d.Session, CI: d.CI, Verdict: d.CIVerdict, Jobs: d.CIJobs})
		}
		var unseen []ReportItem
		for _, it := range rr.Items {
			if src.echo {
				break // the live source over the same log lists them
			}
			if !it.Seen {
				unseen = append(unseen, it)
			}
		}
		if len(unseen) > 0 {
			r.Items = append(r.Items, uiItems{Src: src.Name, Items: unseen})
		}
	}
	// The outside requests each repository's loop watches, by repository
	// name.
	secret := confidentialMatcher(cfg)
	looping := loopingOf(cfg)
	for _, key := range sortedKeys(src.st.CI) {
		rec := src.st.CI[key]
		track, ok := looping[rec.Repository]
		if !ok || rec.Session != track || rec.State == CIMerged || rec.State == CIClosed || rec.Delivered != "" {
			continue
		}
		if _, ok := cfg.Repositories[rec.Repository]; !ok || secret(rec.Repository, rec.Ref, rec.Author) {
			continue
		}
		r := get(rec.Repository)
		r.Console = true
		r.Requests = append(r.Requests, uiRequest{Src: src.Name, Ref: rec.Ref, URL: rec.URL, CI: rec.State, Verdict: rec.Verdict, Jobs: rec.Jobs,
			Outside: true, Author: rec.Author, Draft: rec.Draft, Unresolved: rec.Unresolved})
	}
	return nil
}

// noteAsked records when each decision e raises was first seen.
func (src *uiSource) noteAsked(e Event) {
	note := func(session, id string) {
		if k := session + " " + id; session != "" && id != "" {
			if _, ok := src.askedAt[k]; !ok {
				src.askedAt[k] = e.Time
			}
		}
	}
	switch {
	case e.Decision != nil:
		for _, d := range e.Decision.Decisions {
			note(e.Session, d.ID)
		}
	case e.Case != nil && e.Case.Call != nil && e.Case.Call.Raised != nil:
		note(e.Session, e.Case.Call.Raised.ID)
	case e.Act != nil && e.Act.Raised != nil:
		note(e.Session, e.Act.Raised.ID)
	case e.Observe != nil && e.Observe.StatusQuestions != "":
		note(e.Session, statusDecisionFor(e.Observe.StatusQuestions).ID)
	}
}

// uiMessageView is a console message as the view shows it.
type uiMessageView struct {
	uiMessage
	Src  string
	Body template.HTML
	More template.HTML // the whole text of a long one, folded (#56)
	// An instruction's steps (#56), the tools the session ran on it, and
	// the reply threaded under it.
	Steps     []uiStep
	Activity  []uiStep
	Reply     template.HTML
	ReplyMore template.HTML
	ReplyBy   string
	Replied   time.Time
}

// uiStep is one step of an owner instruction, as the page shows it.
type uiStep struct {
	Text   string
	At     time.Time
	Failed bool
	Wait   bool
	Exit   *int
}

// foldAt is how much of a message the page shows before the fold.
const foldAt = 1200

// folded renders text: whole when it is short; else its start, cut at a
// line, with the whole text to show under a fold.
func folded(text string) (body, more template.HTML) {
	if len(text) <= foldAt {
		return markdown(text), ""
	}
	start := cutUTF8(text, foldAt)
	if i := strings.LastIndex(start, "\n"); i > foldAt/2 {
		start = text[:i] + "\n\n…"
	}
	return markdown(start), markdown(text)
}

// takesLine says whether a message routed to a session with track t will
// be typed into it soon: it is working or idle, or not yet seen.
func takesLine(t *Track) bool {
	return t == nil || t.State == "" || t.State == Working || t.State == Idle || t.State == Starting
}

// instructionSteps is what became of instruction in, as the run's fold
// holds it: received, routed or started, delivered, taken, or waiting
// and why, or failed and why.
func instructionSteps(st *LogState, in *Instruction) []uiStep {
	steps := []uiStep{{Text: "received", At: in.At}}
	if !in.Route {
		return append(steps, uiStep{Text: "recorded before agentd routed instructions; not delivered to any session", Wait: true})
	}
	if in.Session != "" {
		if in.Started {
			steps = append(steps, uiStep{Text: "agentd started " + in.Session + " for it"})
		} else {
			steps = append(steps, uiStep{Text: "routed to " + in.Session})
		}
	}
	switch {
	case in.Failed != "":
		return append(steps, uiStep{Text: "failed: " + in.Failed, Failed: true})
	case in.Waiting != "":
		steps = append(steps, uiStep{Text: "waiting: " + in.Waiting, Wait: true})
	case in.Session != "" && in.Delivered.IsZero():
		why := "waiting for " + in.Session + " to take a line; it is " + string(stateOf(st.Tracks[in.Session]))
		if t := st.Tracks[in.Session]; t != nil && t.State == BlockedOnOwner {
			why = "waiting: " + in.Session + " waits on your answer to its questions; answer them, and agentd hands it the message"
		}
		steps = append(steps, uiStep{Text: why, Wait: true})
	}
	if !in.Delivered.IsZero() {
		steps = append(steps, uiStep{Text: "delivered to " + in.Session, At: in.Delivered})
		if in.Taken.IsZero() {
			steps = append(steps, uiStep{Text: "waiting for " + in.Session + " to start on it", Wait: true})
		}
	}
	if !in.Taken.IsZero() {
		steps = append(steps, uiStep{Text: in.Session + " started on it", At: in.Taken})
		if in.Replied.IsZero() {
			steps = append(steps, uiStep{Text: "waiting for " + in.Session + "'s reply", Wait: true})
		}
	}
	if !in.Replied.IsZero() {
		steps = append(steps, uiStep{Text: in.Session + " replied", At: in.Replied})
	}
	return steps
}

// uiConsole is a repository's console view, or a console's without a
// repository (Other): its messages newest last, its open decisions, and
// the reply box.
type uiConsole struct {
	uiPage
	Repository string
	Other      bool
	Messages   []uiMessageView
	Needs      []uiNeed
	ReplySrc   string // the source a reply is recorded in, or "" when none configures the repository
	// Live: a message waits on its session's answer, so the page
	// refreshes every few seconds.
	Live bool
}

// withheldMessage stands in for a message that matches a confidential
// pattern.
const withheldMessage = "(withheld: it matched a confidential pattern)"

// consoleView is repo's console, nil when no source knows the repository.
func (u *UI) consoleView(repo string, now time.Time) (*uiConsole, error) {
	v := &uiConsole{uiPage: uiPage{Title: repo + " console", Token: u.token, Back: "/console/" + repo, Now: now, loc: u.loc}, Repository: repo}
	known, loops := false, false
	for _, src := range u.sources {
		err := func() error {
			src.mu.Lock()
			defer src.mu.Unlock()
			cfg, rep, err := src.view(now)
			if err != nil {
				return err
			}
			if con, ok := cfg.Consoles[repo]; ok {
				known, v.Other = true, true
				if con.Title != "" {
					v.Title = con.Title + " console"
				}
				if src.st.Mode == ModeLive && v.ReplySrc == "" {
					v.ReplySrc = src.Name
				}
			} else if _, ok := cfg.Repositories[repo]; ok {
				known = true
				// The reply goes to the run of the repository's loop,
				// whose sessions read its inbox, else to the first live
				// run.
				l := looped(cfg.Repositories[repo])
				if src.st.Mode == ModeLive && (v.ReplySrc == "" || l && !loops) {
					v.ReplySrc, loops = src.Name, l
				}
			}
			secret := confidentialMatcher(cfg)
			for _, m := range src.messages[repo] {
				body, more := folded(m.Text)
				if m.Text == "" || secret(m.Text) {
					body, more = template.HTML(template.HTMLEscapeString(withheldMessage)), ""
				}
				mv := uiMessageView{uiMessage: m, Src: src.Name, Body: body, More: more}
				if in := src.st.Instructions[m.ID]; in != nil && m.Kind == "instruction" {
					mv.Steps = instructionSteps(src.st, in)
					for _, tn := range in.Activity {
						text := tn.Text
						if tn.Withheld > 0 || secret(text) {
							text = withheldMessage
						}
						mv.Activity = append(mv.Activity, uiStep{Text: text, At: tn.At, Failed: tn.Failed, Exit: tn.Exit})
					}
					v.Live = v.Live || in.open(src.st, now) || in.Route && in.Failed == "" && in.Waiting == "" && in.Delivered.IsZero() && now.Sub(in.At) < ChatIdle && takesLine(src.st.Tracks[in.Session])
					if !in.Replied.IsZero() {
						mv.ReplyBy, mv.Replied = in.Session, in.Replied
						mv.Reply, mv.ReplyMore = folded(in.Reply)
						if in.Reply == "" || in.ReplyWithheld || secret(in.Reply) {
							mv.Reply, mv.ReplyMore = template.HTML(template.HTMLEscapeString(withheldMessage)), ""
						}
					}
				}
				v.Messages = append(v.Messages, mv)
			}
			for _, n := range src.needs(cfg, rep, src.askedAt) {
				if n.Repository == repo {
					v.Needs = append(v.Needs, n)
				}
			}
			return nil
		}()
		if err != nil {
			return nil, err
		}
	}
	if !known && len(v.Messages) == 0 {
		return nil, nil
	}
	sort.SliceStable(v.Messages, func(i, j int) bool { return v.Messages[i].At.Before(v.Messages[j].At) })
	sortNeeds(v.Needs)
	return v, nil
}

// uiEvent is one line of a session's timeline.
type uiEvent struct {
	At   time.Time
	Kind string
	Text string
}

// uiTurn is one of a session's latest turns.
type uiTurn struct {
	Role string
	At   time.Time
	Body template.HTML
	More template.HTML // the whole text of a long one, folded
}

// uiSessionPage is a session's view: its row, its latest turns, its
// STATUS.md rendered, its open needs, and its latest events.
type uiSessionPage struct {
	uiPage
	Src        string
	Repository string
	Row        uiRow
	Reason     string
	Turns      []uiTurn
	Status     template.HTML
	StatusNote string
	Needs      []uiNeed
	Events     []uiEvent
	// MessageTo is the repository or console a message to the session is
	// recorded for, "" when the session takes none here (#56).
	MessageTo string
}

// uiTurns is how many of a session's latest turns its view shows.
const uiTurns = 12

// sessionView is session name of src, nil when the report does not show
// it.
func (u *UI) sessionView(src *uiSource, name string, now time.Time) (*uiSessionPage, error) {
	src.mu.Lock()
	defer src.mu.Unlock()
	cfg, rep, err := src.view(now)
	if err != nil {
		return nil, err
	}
	v := &uiSessionPage{uiPage: uiPage{Title: name, Token: u.token, Back: "/session/" + src.Name + "/" + name, Now: now, loc: u.loc}, Src: src.Name}
	found := false
	for _, rr := range rep.Repositories {
		for _, c := range rr.Cases {
			if c.Session == name {
				v.Repository, v.Row, found = uiRepoName(rr), src.row(c), true
			}
		}
	}
	if !found {
		return nil, nil
	}
	if sess, ok := cfg.Sessions[name]; ok && sess.Role == "" && src.st.Mode == ModeLive && !src.echo {
		v.MessageTo = sess.Repository
		if v.MessageTo == "" {
			v.MessageTo = cfg.Console(name)
		}
	}
	secret := confidentialMatcher(cfg)
	t := src.st.Tracks[name]
	if !secret(t.Reason) {
		v.Reason = t.Reason
	}
	turns := t.Turns
	if len(turns) > uiTurns {
		turns = turns[len(turns)-uiTurns:]
	}
	for _, tr := range turns {
		body, more := folded(tr.Text)
		if tr.Withheld || secret(tr.Text) {
			body, more = template.HTML(template.HTMLEscapeString(withheldMessage)), ""
		}
		v.Turns = append(v.Turns, uiTurn{Role: tr.Role, At: tr.At, Body: body, More: more})
	}
	if sess, ok := cfg.Sessions[name]; ok && sess.Workdir != "" {
		b, err := readRegular(filepath.Join(sess.Workdir, "STATUS.md"), statusLimit)
		switch {
		case errors.Is(err, os.ErrNotExist):
			v.StatusNote = "No STATUS.md."
		case err != nil:
			v.StatusNote = "STATUS.md could not be read."
		case len(b) == 0:
			v.StatusNote = "No STATUS.md."
		case secret(string(b)):
			v.StatusNote = "STATUS.md was withheld because it matched a confidential pattern."
		default:
			v.Status = markdown(string(b))
		}
	}
	for _, n := range src.needs(cfg, rep, src.askedAt) {
		if n.Session == name {
			v.Needs = append(v.Needs, n)
		}
	}
	es := src.events[name]
	for i := len(es) - 1; i >= 0; i-- {
		text := eventText(es[i])
		if secret(text) {
			text = withheldMessage
		}
		v.Events = append(v.Events, uiEvent{At: es[i].Time, Kind: es[i].Kind, Text: text})
	}
	return v, nil
}

// eventText is a line saying what e did, for a timeline.
func eventText(e Event) string {
	join := func(parts ...string) string {
		var kept []string
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				kept = append(kept, p)
			}
		}
		return strings.Join(kept, " · ")
	}
	switch {
	case e.Transition != nil:
		return join(e.Transition.From+" → "+e.Transition.To, e.Transition.Reason)
	case e.Act != nil:
		return join(e.Act.Action, e.Act.Key, e.Act.Ref, e.Act.Reason, e.Act.Message)
	case e.Observe != nil:
		return join(e.Observe.State, e.Observe.Reason)
	case e.Skip != nil:
		return join(e.Skip.Action, e.Skip.Key, e.Skip.Reason)
	case e.Crash != nil:
		return fmt.Sprintf("crash %d", e.Crash.Crashes)
	case e.Error != nil:
		return join(e.Error.Step, e.Error.Error)
	case e.Turn != nil:
		return e.Turn.Role
	case e.Usage != nil:
		return fmt.Sprintf("context %d, spend %d", e.Usage.Context, e.Usage.Spend)
	case e.Checkpoint != nil:
		return join(shortSHA(e.Checkpoint.SHA), e.Checkpoint.Subject)
	case e.Decision != nil:
		return fmt.Sprintf("%d decisions", len(e.Decision.Decisions))
	case e.Answer != nil:
		return "answered " + e.Answer.ID
	case e.CI != nil:
		return join(e.CI.Ref, e.CI.State, e.CI.Verdict)
	}
	return ""
}

// shortSHA is a commit's first twelve characters.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// addAccounts merges src's account readings into the index: the newest
// reading of each account stands.
func (src *uiSource) addAccounts(v *uiIndex, cfg *Config, now time.Time) {
	secret := confidentialMatcher(cfg)
	for name, r := range src.st.Accounts {
		a := uiAccount{AccountRecord: r, Name: name}
		if r.ErrorAt.After(r.At) && !secret(r.Error) {
			a.Error = r.Error
		}
		if high, ok := r.usageHigh(); ok && cfg.Usage.Limit > 0 && high >= cfg.Usage.Limit {
			a.Over = true
		}
		a.Stale = r.At.IsZero() || cfg.Usage.Every.Duration > 0 && now.Sub(r.At) > usageStale*cfg.Usage.Every.Duration
		i := 0
		for i < len(v.Accounts) && v.Accounts[i].Name != name {
			i++
		}
		switch {
		case i == len(v.Accounts):
			v.Accounts = append(v.Accounts, a)
		case r.At.After(v.Accounts[i].At):
			v.Accounts[i] = a
		}
	}
}
