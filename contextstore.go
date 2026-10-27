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
	screen   []*regexp.Regexp
	off      int64
	newCards []string // repo|topic of cards revised since the repository's card was curated
	expert   map[string][]expertSpend
}

func NewContextStore(cfg *Config) *ContextStore {
	cs := &ContextStore{cfg: cfg, src: newHistSource(cfg), hist: map[string]*History{}, cards: map[string]map[string]*Card{},
		curated: map[string]map[string]curation{}, flags: map[string]map[string][]contextFlag{}, threads: map[string]map[string]time.Time{}, expert: map[string][]expertSpend{}}
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
		if p.Card != nil {
			nested(cs.cards, p.Repository)[p.Topic] = p.Card
			if p.Topic != TopicRepo {
				cs.newCards = append(cs.newCards, p.Repository+"|"+p.Topic)
			} else {
				cs.newCards = dropRepo(cs.newCards, p.Repository)
			}
		}
		nested(cs.curated, p.Repository)[p.Topic] = c
	case e.Thread != nil:
		nested(cs.threads, e.Thread.Repository)[e.Thread.Ref] = e.Time
		if e.Thread.Error == "" {
			for _, r := range threadRecords(e.Time, e.Thread) {
				cs.History(e.Thread.Repository).add(r)
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
	default:
		for _, r := range cs.src.records(e) {
			cs.History(r.repo).add(r)
		}
	}
}

func dropRepo(list []string, repo string) []string {
	var out []string
	for _, k := range list {
		if !strings.HasPrefix(k, repo+"|") {
			out = append(out, k)
		}
	}
	return out
}

// threadRecords are a thread's records: the issue or request as it is
// now, which is a new record whenever it changed, and each comment.
func threadRecords(at time.Time, t *ThreadPayload) []*HistRecord {
	kind := HistIssue
	if strings.HasPrefix(t.Ref, "!") {
		kind = HistRequest
	}
	var out []*HistRecord
	if t.Title != "" || t.Body != "" {
		text := t.Body
		if t.Withheld > 0 {
			text = fmt.Sprintf("(withheld: matches confidential pattern %d)", t.Withheld)
		}
		text = strings.TrimSpace(text + "\n\nstate: " + t.State + "; labels: " + strings.Join(t.Labels, ", ") + "; by " + t.Author)
		topics := []string{t.Ref}
		for _, l := range t.Labels {
			topics = append(topics, topicLabel+strings.ToLower(l))
		}
		out = append(out, &HistRecord{ID: histID(kind, t.Repository+t.Ref+"|"+shortHash(HashPane(t.Title+"|"+text))), Kind: kind, At: at,
			Title: t.Ref + " " + t.Title, Text: text, Topics: topicsOf("", topics, t.Title, t.Body), Source: "forge:" + t.Ref})
	}
	for _, c := range t.Comments {
		text := c.Body
		if c.Withheld > 0 {
			text = fmt.Sprintf("(withheld: matches confidential pattern %d)", c.Withheld)
		}
		if c.Author != "" {
			text = c.Author + ": " + text
		}
		out = append(out, &HistRecord{ID: "C" + c.ID, Kind: HistComment, At: c.At, Text: text,
			Topics: topicsOf("", []string{t.Ref}, c.Body), Source: "forge:" + t.Ref + "#comment-" + c.ID})
	}
	return out
}

// withheldBy is the number, from 1, of the first pattern text matches, or 0.
func (cs *ContextStore) withheldBy(text string) int {
	for i, re := range cs.screen {
		if re.MatchString(text) {
			return i + 1
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

// ReadThread reads ref's thread from the forge and returns the payload to
// log: the issue as it is, screened, and the comments history lacks.
func (cs *ContextStore) ReadThread(api ForgeAPI, repo, ref string) *ThreadPayload {
	seen := map[string]bool{}
	for _, r := range cs.History(repo).OnTopic(ref) {
		seen[r.ID] = true
	}
	return cs.readThread(api, repo, ref, seen)
}

// readThread is ReadThread with the record ids history holds on ref given,
// so a worker reads nothing else of the store.
func (cs *ContextStore) readThread(api ForgeAPI, repo, ref string, seen map[string]bool) *ThreadPayload {
	p := &ThreadPayload{Repository: repo, Ref: ref}
	r, ok := cs.cfg.Repositories[repo]
	if !ok || api == nil || len(ref) < 2 {
		p.Error = "no forge to read " + ref + " from"
		return p
	}
	n, err := strconv.Atoi(ref[1:])
	if err != nil {
		p.Error = "not a reference: " + ref
		return p
	}
	var comments []ThreadComment
	if r.Forge == "gitlab" {
		comments, err = readGitLabThread(api.At(r.Host), r.Project, n, ref[0] == '!', p)
	} else {
		comments, err = readGitHubThread(api.At(r.Host), r.Project, n, p)
	}
	if err != nil {
		p.Error = oneLine(err.Error(), 300)
		return p
	}
	if p.Withheld = cs.withheldBy(p.Title + "\n" + p.Body); p.Withheld > 0 {
		p.Body = ""
	}
	p.Body = cutUTF8(p.Body, threadText)
	for _, c := range comments {
		if seen["C"+c.ID] {
			continue
		}
		if c.Withheld = cs.withheldBy(c.Body); c.Withheld > 0 {
			c.Body = ""
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

func readGitHubThread(api ForgeAPI, project string, n int, p *ThreadPayload) ([]ThreadComment, error) {
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
	resp, err = api.GitHub(fmt.Sprintf("%s/comments?per_page=%d", base, threadComments), nil)
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
	resp, err = api.GitLab(fmt.Sprintf("%s/notes?sort=asc&per_page=%d", base, threadComments))
	if err := getJSON(resp, err, "GitLab notes", &notes); err != nil {
		return nil, err
	}
	var out []ThreadComment
	for _, c := range notes {
		if !c.System {
			out = append(out, ThreadComment{ID: strconv.FormatInt(c.ID, 10), Author: c.Author.Username, At: c.CreatedAt, Body: c.Body})
		}
	}
	return out, nil
}
