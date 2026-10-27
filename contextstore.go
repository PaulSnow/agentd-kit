package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Event kinds of the repository's context (#60).
const (
	KindCard   = "card"   // a curator's revision of a topic's card, or the diff Go refused, or the call that failed
	KindThread = "thread" // an issue's or request's thread as the forge has it: what is new since agentd last read it
	KindQuery  = "query"  // an agent's call on the context tools: what it asked, why, and the answer's size
)

// CardPayload is one curation of a topic's card.
type CardPayload struct {
	Repository string `json:"repository"`
	Topic      string `json:"topic"`
	Card       *Card  `json:"card,omitempty"`
	Refused    string `json:"refused,omitempty"`
	Error      string `json:"error,omitempty"`
	// Unchanged says the curator found nothing to change.
	Unchanged bool `json:"unchanged,omitempty"`
	// Through is the newest history record the curator was given: the
	// next curation sends what is newer.
	Through    time.Time `json:"through"`
	Records    []string  `json:"records,omitempty"`
	InputBytes int       `json:"input_bytes,omitempty"`
	Model      string    `json:"model,omitempty"`
	Usage      *JobUsage `json:"usage,omitempty"`
}

// ThreadPayload is an issue's or request's thread: its title, state,
// labels and body, and the comments agentd had not read before, each
// screened.
type ThreadPayload struct {
	Repository string          `json:"repository"`
	Ref        string          `json:"ref"`
	Title      string          `json:"title,omitempty"`
	State      string          `json:"state,omitempty"`
	Labels     []string        `json:"labels,omitempty"`
	Body       string          `json:"body,omitempty"`
	Author     string          `json:"author,omitempty"`
	Withheld   int             `json:"withheld,omitempty"`
	Comments   []ThreadComment `json:"comments,omitempty"`
	Error      string          `json:"error,omitempty"`
}

// ThreadComment is one comment of a thread.
type ThreadComment struct {
	ID       string    `json:"id"`
	Author   string    `json:"author,omitempty"`
	At       time.Time `json:"at"`
	Body     string    `json:"body,omitempty"`
	Withheld int       `json:"withheld,omitempty"`
}

// QueryPayload is one call on the context tools.
type QueryPayload struct {
	Repository string   `json:"repository"`
	Tool       string   `json:"tool"`
	Topic      string   `json:"topic,omitempty"`
	Q          string   `json:"q,omitempty"`
	IDs        []string `json:"ids,omitempty"`
	Why        string   `json:"why,omitempty"`
	Question   string   `json:"question,omitempty"`
	// Hit says state answered: a card was there.
	Hit     bool      `json:"hit,omitempty"`
	Records int       `json:"records"`
	Bytes   int       `json:"bytes"`
	Tokens  int       `json:"tokens"`
	Model   string    `json:"model,omitempty"`
	Usage   *JobUsage `json:"usage,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// History reasons: why an agent reads history rather than state.
const (
	WhySpec             = "spec"
	WhyDebugDecision    = "debug-decision"
	WhyVerifyClaim      = "verify-claim"
	WhyMissingFromState = "missing-from-state"
)

var historyWhys = []string{WhySpec, WhyDebugDecision, WhyVerifyClaim, WhyMissingFromState}

// threadText is the most of an issue's body or a comment history keeps.
const threadText = 8000

// curation is a topic's last curation as the store folded it.
type curation struct {
	Through time.Time
	At      time.Time
	Refused string
	Failed  bool
	// Refusals is how many curations in a row were refused.
	Refusals int
}

// contextFlag is an agent's word that state lacked what it needed.
type contextFlag struct {
	At       time.Time
	Session  string
	Question string
}

// ContextStore is every repository's history and cards, folded from the
// log as it grows: agentd's run and each tool server keep one.
type ContextStore struct {
	cfg      *Config
	src      *histSource
	hist     map[string]*History
	cards    map[string]map[string]*Card
	curated  map[string]map[string]curation
	flags    map[string]map[string][]contextFlag
	threads  map[string]map[string]time.Time // when each thread was last read
	issues   map[string]string               // repo|ref: the hash of the issue as last read
	screen   []*regexp.Regexp
	policies *LogState // the repositories' own files, for the patterns they add
	off      int64
	newCards []string // repo|topic of cards revised since the repository's card was curated
	expert   map[string][]expertSpend
	runs     map[string][]time.Time // each repository's curations
}

func NewContextStore(cfg *Config) *ContextStore {
	cs := &ContextStore{cfg: cfg, src: newHistSource(cfg), hist: map[string]*History{}, cards: map[string]map[string]*Card{},
		curated: map[string]map[string]curation{}, flags: map[string]map[string][]contextFlag{}, threads: map[string]map[string]time.Time{}, expert: map[string][]expertSpend{},
		issues: map[string]string{}, policies: NewLogState(cfg, ModeLive), runs: map[string][]time.Time{}}
	for _, p := range screenPatterns(cfg) {
		cs.screen = append(cs.screen, regexp.MustCompile(p))
	}
	return cs
}

// Follow folds what the log gained since the store last read it.
func (cs *ContextStore) Follow(l *Log) error {
	events, off, err := l.Tail(cs.off)
	for _, e := range events {
		cs.Apply(e)
	}
	cs.off = off
	return err
}

// History is repo's history, made when first asked.
func (cs *ContextStore) History(repo string) *History {
	h := cs.hist[repo]
	if h == nil {
		h = newHistory(repo)
		cs.hist[repo] = h
	}
	return h
}

// Card is topic's latest card in repo, or nil.
func (cs *ContextStore) Card(repo, topic string) *Card { return cs.cards[repo][topic] }

// Cards are repo's latest cards by topic.
func (cs *ContextStore) Cards(repo string) map[string]*Card { return cs.cards[repo] }

func nested[V any](m map[string]map[string]V, k string) map[string]V {
	if m[k] == nil {
		m[k] = map[string]V{}
	}
	return m[k]
}

// Apply folds one event.
func (cs *ContextStore) Apply(e Event) {
	switch {
	case e.Card != nil:
		p := e.Card
		c := curation{Through: p.Through, At: e.Time, Refused: p.Refused, Failed: p.Error != ""}
		if p.Refused != "" {
			c.Refusals = cs.curated[p.Repository][p.Topic].Refusals + 1
		}
		if p.Card != nil && p.Topic != TopicRepo {
			nested(cs.cards, p.Repository)[p.Topic] = p.Card
			cs.newCards = append(cs.newCards, p.Repository+"|"+p.Topic)
		}
		if p.Topic == TopicRepo && p.Error == "" {
			if p.Card != nil {
				nested(cs.cards, p.Repository)[p.Topic] = p.Card
			}
			// The cards it was sent are seen, whatever it made of them.
			for _, id := range p.Records {
				cs.newCards = dropKey(cs.newCards, p.Repository+"|"+strings.TrimPrefix(id, "card:"))
			}
		}
		nested(cs.curated, p.Repository)[p.Topic] = c
		runs := cs.runs[p.Repository]
		for len(runs) > 0 && e.Time.Sub(runs[0]) >= 24*time.Hour {
			runs = runs[1:]
		}
		cs.runs[p.Repository] = append(runs, e.Time)
	case e.Thread != nil:
		t := e.Thread
		nested(cs.threads, t.Repository)[t.Ref] = e.Time
		if t.Error == "" {
			for _, r := range threadRecords(e.Time, t, cs.issues) {
				cs.History(t.Repository).add(r)
			}
		}
	case e.Query != nil:
		q := e.Query
		if q.Tool == ToolHistory && q.Why == WhyMissingFromState && q.Topic != "" && cardKindOf(q.Topic) != "" {
			f := nested(cs.flags, q.Repository)
			f[q.Topic] = append(f[q.Topic], contextFlag{At: e.Time, Session: e.Session, Question: q.Question})
		}
		if q.Tool == ToolAskExpert && q.Usage != nil {
			cs.expert[q.Repository] = append(cs.expert[q.Repository], expertSpend{At: e.Time, Tokens: q.Usage.tokens()})
		}
	case e.Policy != nil:
		cs.policies.applyPolicy(e)
	default:
		for _, r := range cs.src.records(e) {
			cs.History(r.repo).add(r)
		}
	}
}

func dropKey(list []string, key string) []string {
	var out []string
	for _, k := range list {
		if k != key {
			out = append(out, k)
		}
	}
	return out
}

// threadRecords are a thread's records, each at the time it was read, so
// a curation windowed by time never misses one written before it: the
// issue or request, a new record whenever it differs from its last read
// (issues holds each one's hash), and each comment, with the time the
// forge gives it in its text.
func threadRecords(at time.Time, t *ThreadPayload, issues map[string]string) []*HistRecord {
	kind := HistIssue
	if strings.HasPrefix(t.Ref, "!") {
		kind = HistRequest
	}
	var out []*HistRecord
	if t.Title != "" || t.Body != "" || t.Withheld > 0 {
		text := t.Body
		if t.Withheld > 0 {
			text = fmt.Sprintf("(withheld: matches confidential pattern %d)", t.Withheld)
		}
		text = strings.TrimSpace(text + "\n\nstate: " + t.State + "; labels: " + strings.Join(t.Labels, ", ") + "; by " + t.Author)
		topics := []string{t.Ref}
		for _, l := range t.Labels {
			topics = append(topics, topicLabel+strings.ToLower(l))
		}
		key, hash := t.Repository+"|"+t.Ref, shortHash(HashPane(t.Title+"|"+text))
		if issues[key] != hash {
			issues[key] = hash
			out = append(out, &HistRecord{ID: histID(kind, key+"|"+hash+"|"+at.UTC().Format(time.RFC3339Nano)), Kind: kind, At: at,
				Title: strings.TrimSpace(t.Ref + " " + t.Title), Text: text, Topics: topicsOf("", topics, t.Title, t.Body), Source: "forge:" + t.Ref})
		}
	}
	for _, c := range t.Comments {
		text := c.Body
		if c.Withheld > 0 {
			text = fmt.Sprintf("(withheld: matches confidential pattern %d)", c.Withheld)
		}
		text = strings.TrimSpace(c.Author+" at "+c.At.UTC().Format(time.RFC3339)) + ": " + text
		out = append(out, &HistRecord{ID: "C" + c.ID, Kind: HistComment, At: at, Text: text,
			Topics: topicsOf("", []string{t.Ref}, c.Body), Source: "forge:" + t.Ref + "#comment-" + c.ID})
	}
	return out
}

// patterns are every pattern the store screens with: the configuration's
// and those the repositories' own files add.
func (cs *ContextStore) patterns() []*regexp.Regexp {
	return append(append([]*regexp.Regexp(nil), cs.screen...), filePatterns(cs.policies)...)
}

// withheldBy is the number, from 1, of the first pattern any text
// matches, or 0.
func withheldBy(pats []*regexp.Regexp, texts ...string) int {
	for i, re := range pats {
		for _, t := range texts {
			if re.MatchString(t) {
				return i + 1
			}
		}
	}
	return 0
}

// threadAge is how soon a thread is read again.
const threadAge = 5 * time.Minute

// ThreadDue says whether ref's thread was not read within threadAge.
func (cs *ContextStore) ThreadDue(repo, ref string, now time.Time) bool {
	at, ok := cs.threads[repo][ref]
	return !ok || now.Sub(at) >= threadAge
}

// threadRead is what reading a thread needs from the store, copied, so a
// worker reads it while the loop goes on.
type threadRead struct {
	repo  string
	r     Repository
	ref   string
	seen  map[string]bool // the record ids history holds on ref
	since time.Time       // its last read, less a minute
	pats  []*regexp.Regexp
}

// threadRead is ref's read as the store has it now.
func (cs *ContextStore) threadRead(repo, ref string) threadRead {
	tr := threadRead{repo: repo, r: cs.cfg.Repositories[repo], ref: ref, seen: map[string]bool{}, pats: cs.patterns()}
	for _, r := range cs.History(repo).OnTopic(ref) {
		tr.seen[r.ID] = true
	}
	if at, ok := cs.threads[repo][ref]; ok {
		tr.since = at.Add(-time.Minute)
	}
	return tr
}

// ReadThread reads ref's thread from the forge and returns the payload to
// log: the issue as it is, screened, and the comments history lacks.
func (cs *ContextStore) ReadThread(api ForgeAPI, repo, ref string) *ThreadPayload {
	return cs.threadRead(repo, ref).read(api)
}

func (tr threadRead) read(api ForgeAPI) *ThreadPayload {
	p := &ThreadPayload{Repository: tr.repo, Ref: tr.ref}
	if tr.r.Project == "" || api == nil || len(tr.ref) < 2 {
		p.Error = "no forge to read " + tr.ref + " from"
		return p
	}
	n, err := strconv.Atoi(tr.ref[1:])
	if err != nil {
		p.Error = "not a reference: " + tr.ref
		return p
	}
	var comments []ThreadComment
	if tr.r.Forge == "gitlab" {
		comments, err = readGitLabThread(api.At(tr.r.Host), tr.r.Project, n, tr.ref[0] == '!', p)
	} else {
		comments, err = readGitHubThread(api.At(tr.r.Host), tr.r.Project, n, tr.since, p)
	}
	if err != nil {
		p.Error = oneLine(err.Error(), 300)
		return p
	}
	if p.Withheld = withheldBy(tr.pats, p.Title, p.Body); p.Withheld > 0 {
		p.Title, p.Body = "", ""
	}
	if withheldBy(tr.pats, p.Author) > 0 {
		p.Author = ""
	}
	var labels []string
	for _, l := range p.Labels {
		if withheldBy(tr.pats, l) == 0 {
			labels = append(labels, l)
		}
	}
	p.Labels = labels
	p.Body = cutUTF8(p.Body, threadText)
	for _, c := range comments {
		if tr.seen["C"+c.ID] {
			continue
		}
		if c.Withheld = withheldBy(tr.pats, c.Body); c.Withheld > 0 {
			c.Body = ""
		}
		if withheldBy(tr.pats, c.Author) > 0 {
			c.Author = ""
		}
		c.Body = cutUTF8(c.Body, threadText)
		p.Comments = append(p.Comments, c)
	}
	return p
}

// threadComments is the most comments one read asks for.
const threadComments = 100

func getJSON(resp APIResponse, err error, what string, v any) error {
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return statusError(what, resp)
	}
	return json.Unmarshal(resp.Body, v)
}

func readGitHubThread(api ForgeAPI, project string, n int, since time.Time, p *ThreadPayload) ([]ThreadComment, error) {
	var issue struct {
		Title  string                  `json:"title"`
		Body   string                  `json:"body"`
		State  string                  `json:"state"`
		User   struct{ Login string }  `json:"user"`
		Labels []struct{ Name string } `json:"labels"`
	}
	base := fmt.Sprintf("/repos/%s/issues/%d", project, n)
	resp, err := api.GitHub(base, nil)
	if err := getJSON(resp, err, "GitHub issue", &issue); err != nil {
		return nil, err
	}
	p.Title, p.Body, p.State, p.Author = issue.Title, issue.Body, issue.State, issue.User.Login
	for _, l := range issue.Labels {
		p.Labels = append(p.Labels, l.Name)
	}
	var cs []struct {
		ID        int64                  `json:"id"`
		Body      string                 `json:"body"`
		CreatedAt time.Time              `json:"created_at"`
		User      struct{ Login string } `json:"user"`
	}
	path := fmt.Sprintf("%s/comments?per_page=%d", base, threadComments)
	if !since.IsZero() {
		path += "&since=" + since.UTC().Format(time.RFC3339) // only the comments since: a long thread's new ones are past the first page
	}
	resp, err = api.GitHub(path, nil)
	if err := getJSON(resp, err, "GitHub comments", &cs); err != nil {
		return nil, err
	}
	var out []ThreadComment
	for _, c := range cs {
		out = append(out, ThreadComment{ID: strconv.FormatInt(c.ID, 10), Author: c.User.Login, At: c.CreatedAt, Body: c.Body})
	}
	return out, nil
}

func readGitLabThread(api ForgeAPI, project string, n int, mr bool, p *ThreadPayload) ([]ThreadComment, error) {
	kind := "issues"
	if mr {
		kind = "merge_requests"
	}
	base := fmt.Sprintf("/projects/%s/%s/%d", url.PathEscape(project), kind, n)
	var issue struct {
		Title       string                    `json:"title"`
		Description string                    `json:"description"`
		State       string                    `json:"state"`
		Labels      []string                  `json:"labels"`
		Author      struct{ Username string } `json:"author"`
	}
	resp, err := api.GitLab(base)
	if err := getJSON(resp, err, "GitLab "+kind, &issue); err != nil {
		return nil, err
	}
	p.Title, p.Body, p.State, p.Labels, p.Author = issue.Title, issue.Description, issue.State, issue.Labels, issue.Author.Username
	var notes []struct {
		ID        int64                     `json:"id"`
		Body      string                    `json:"body"`
		System    bool                      `json:"system"`
		CreatedAt time.Time                 `json:"created_at"`
		Author    struct{ Username string } `json:"author"`
	}
	// The newest first, so a long thread's new notes are on the first page.
	resp, err = api.GitLab(fmt.Sprintf("%s/notes?sort=desc&order_by=created_at&per_page=%d", base, threadComments))
	if err := getJSON(resp, err, "GitLab notes", &notes); err != nil {
		return nil, err
	}
	var out []ThreadComment
	for i := len(notes) - 1; i >= 0; i-- {
		if c := notes[i]; !c.System {
			out = append(out, ThreadComment{ID: strconv.FormatInt(c.ID, 10), Author: c.Author.Username, At: c.CreatedAt, Body: c.Body})
		}
	}
	return out, nil
}
