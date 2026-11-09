package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Remote errands (#95): documents between the hub's machine and a node's,
// and prompts run on a node. They ride the connection the node already
// makes to the hub, so the hub never reaches into the node's machine: the
// node holds a request open (errandWaitPath) and the hub answers it as soon
// as it has an errand for that host. A node takes errands only when its
// own configuration says so (node.errands); observe keeps refusing the
// hub's clone, session and deploy commands.
//
// On the hub's machine, agentd send and agentd prompt reach the hub through
// a Unix socket only its owner can open (hub.socket). A document waits in
// the hub's spool (hub.spool) until the node has it, so a node that is off
// gets it when it is back; the event log holds none of it.

const (
	errandWaitPath = "/v1/errand"  // GET: the next errand, held open up to errandWaitMax
	errandPrefix   = "/v1/errand/" // GET ID/file: a document's bytes; POST ID/done: how it ended
	uploadPath     = "/v1/upload"  // POST ?name=N: a document for the hub's inbox
	errandWaitMax  = 40 * time.Second
	// errandFileMax bounds one document.
	errandFileMax = 200 << 20
	// errandLease is how long a document a node fetched may go undone
	// before it is offered again.
	errandLease = 10 * time.Minute
	// DefaultPromptTimeout bounds a prompt nobody gave a timeout.
	DefaultPromptTimeout = 30 * time.Minute
	// DefaultInbox is where documents land, on either side.
	DefaultInbox = "~/agentd-inbox"
	// DefaultHubSocket is the hub's local socket.
	DefaultHubSocket = "~/.local/state/agentd-kit/hub.sock"
	// hubHost names the hub in agentd send, on a node's machine.
	hubHost = "hub"
)

// Errand kinds.
const (
	ErrandDocument = "document"
	ErrandPrompt   = "prompt"
)

// DefaultPromptCommand runs a prompt: Claude Code in print mode, its
// permission checks skipped, as the owner chose (#95). The model, when
// asked for, and the prompt's text follow it.
var DefaultPromptCommand = []string{"claude", "-p", "--dangerously-skip-permissions"}

// Errand is what the hub asks a node to do.
type Errand struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// A document: its name, size and SHA-256, and whether the node opens
	// it or shows a notification when it is saved.
	Name   string `json:"name,omitempty"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Open   bool   `json:"open,omitempty"`
	Notify bool   `json:"notify,omitempty"`
	// A prompt: its text, the directory it runs in ("" is the home
	// directory), the model and how long it may run.
	Text       string `json:"text,omitempty"`
	Dir        string `json:"dir,omitempty"`
	Model      string `json:"model,omitempty"`
	TimeoutSec int64  `json:"timeout_sec,omitempty"`
}

// ErrandDone is how an errand ended: a document's path where it was saved,
// a prompt's exit status and output. Error says what failed; with OK, it
// is a failure after the errand itself was done, such as opening a saved
// document.
type ErrandDone struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Path   string `json:"path,omitempty"`
	Exit   int    `json:"exit"`
	Output string `json:"output,omitempty"`
}

// pendingErrand is an errand the hub holds for a host.
type pendingErrand struct {
	Errand
	host  string
	data  string    // a document's bytes, in the spool
	taken time.Time // when a node fetched it; zero until then
	done  chan ErrandDone
}

// errandQueue is the hub's errands, per host, in the order they came.
type errandQueue struct {
	spool string
	now   func() time.Time

	mu     sync.Mutex
	byHost map[string][]*pendingErrand
	wake   map[string]chan struct{}
}

// newErrandQueue is a queue over spool, holding the documents a hub before
// it left there.
func newErrandQueue(spool string, now func() time.Time) (*errandQueue, error) {
	q := &errandQueue{spool: spool, now: now, byHost: map[string][]*pendingErrand{}, wake: map[string]chan struct{}{}}
	if err := os.MkdirAll(spool, 0o700); err != nil {
		return nil, fmt.Errorf("hub.spool: %w", err)
	}
	hosts, err := os.ReadDir(spool)
	if err != nil {
		return nil, fmt.Errorf("hub.spool: %w", err)
	}
	for _, h := range hosts {
		if !h.IsDir() {
			continue
		}
		metas, _ := filepath.Glob(filepath.Join(spool, h.Name(), "*.json"))
		sort.Strings(metas)
		for _, m := range metas {
			b, err := os.ReadFile(m)
			var e Errand
			if err != nil || json.Unmarshal(b, &e) != nil || e.Kind != ErrandDocument {
				continue
			}
			data := strings.TrimSuffix(m, ".json") + ".data"
			if _, err := os.Stat(data); err != nil {
				continue
			}
			q.byHost[h.Name()] = append(q.byHost[h.Name()], &pendingErrand{Errand: e, host: h.Name(), data: data, done: make(chan ErrandDone, 1)})
		}
	}
	return q, nil
}

func newErrandID(now time.Time) string {
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%d-%s", now.UnixNano(), hex.EncodeToString(b))
}

// addDocument spools a document from r for host and queues it.
func (q *errandQueue) addDocument(host, name string, r io.Reader, open, notify bool) (*pendingErrand, error) {
	name = inboxName(name)
	dir := filepath.Join(q.spool, host)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	e := Errand{ID: newErrandID(q.now()), Kind: ErrandDocument, Name: name, Open: open, Notify: notify}
	data := filepath.Join(dir, e.ID+".data")
	f, err := os.OpenFile(data, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum), io.LimitReader(r, errandFileMax+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > errandFileMax {
		err = fmt.Errorf("%s is larger than %d MB", name, errandFileMax>>20)
	}
	if err != nil {
		os.Remove(data)
		return nil, err
	}
	e.Size, e.SHA256 = n, hex.EncodeToString(sum.Sum(nil))
	b, _ := json.Marshal(e)
	if err := os.WriteFile(filepath.Join(dir, e.ID+".json"), b, 0o600); err != nil {
		os.Remove(data)
		return nil, err
	}
	p := &pendingErrand{Errand: e, host: host, data: data, done: make(chan ErrandDone, 1)}
	q.add(p)
	return p, nil
}

// addPrompt queues a prompt for host. It is held in memory alone: whoever
// asked waits for its answer.
func (q *errandQueue) addPrompt(host string, e Errand) *pendingErrand {
	e.ID, e.Kind = newErrandID(q.now()), ErrandPrompt
	if e.TimeoutSec <= 0 {
		e.TimeoutSec = int64(DefaultPromptTimeout / time.Second)
	}
	p := &pendingErrand{Errand: e, host: host, done: make(chan ErrandDone, 1)}
	q.add(p)
	return p
}

func (q *errandQueue) add(p *pendingErrand) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.byHost[p.host] = append(q.byHost[p.host], p)
	if ch, ok := q.wake[p.host]; ok {
		close(ch)
		delete(q.wake, p.host)
	}
}

// next is host's first errand no node is doing, waiting for one up to
// wait; nil when none came. A document fetched over errandLease ago and
// not done is offered again; a prompt taken and not answered within its
// timeout and errandLease ends as failed.
func (q *errandQueue) next(ctx context.Context, host string, wait time.Duration) *pendingErrand {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		q.mu.Lock()
		now := q.now()
		var found *pendingErrand
		for _, p := range append([]*pendingErrand(nil), q.byHost[host]...) {
			switch {
			case p.taken.IsZero(), p.Kind == ErrandDocument && now.Sub(p.taken) > errandLease:
				if found == nil {
					p.taken = now
					found = p
				}
			case p.Kind == ErrandPrompt && now.Sub(p.taken) > time.Duration(p.TimeoutSec)*time.Second+errandLease:
				q.removeLocked(p, ErrandDone{Error: "the node took the prompt and never answered"})
			}
		}
		if found != nil {
			q.mu.Unlock()
			return found
		}
		ch, ok := q.wake[host]
		if !ok {
			ch = make(chan struct{})
			q.wake[host] = ch
		}
		q.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		}
	}
}

// get is host's errand id.
func (q *errandQueue) get(host, id string) *pendingErrand {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, p := range q.byHost[host] {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// finish ends host's errand id with d, for whoever waits on it; false when
// the hub holds no such errand.
func (q *errandQueue) finish(host, id string, d ErrandDone) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, p := range q.byHost[host] {
		if p.ID == id {
			q.removeLocked(p, d)
			return true
		}
	}
	return false
}

// cancel drops p when no node has taken it yet, for an asker that stopped
// waiting on a prompt.
func (q *errandQueue) cancel(p *pendingErrand) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if p.taken.IsZero() {
		q.removeLocked(p, ErrandDone{Error: "cancelled"})
	}
}

func (q *errandQueue) removeLocked(p *pendingErrand, d ErrandDone) {
	list := q.byHost[p.host]
	for i, x := range list {
		if x == p {
			q.byHost[p.host] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	if p.data != "" {
		os.Remove(p.data)
		os.Remove(strings.TrimSuffix(p.data, ".data") + ".json")
	}
	select {
	case p.done <- d:
	default:
	}
}

// pending counts host's errands.
func (q *errandQueue) pending(host string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.byHost[host])
}

// inboxName is name as a file name of its own: its last element, never
// empty, a dot name or a path.
func inboxName(name string) string {
	name = filepath.Base(filepath.Clean("/" + strings.ReplaceAll(name, "\\", "/")))
	if name == "/" || name == "." || name == ".." || name == "" {
		return "document"
	}
	return name
}

// saveInbox saves r as name in dir, a numbered name ("report (2).pdf") when
// one is there already: nothing is overwritten, and a file appears only
// whole. When size is not negative, r must be size bytes with SHA-256 sum.
func saveInbox(dir, name string, r io.Reader, size int64, sum string) (string, error) {
	name = inboxName(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".agentd-incoming-")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, errandFileMax+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return "", err
	case n > errandFileMax:
		return "", fmt.Errorf("%s is larger than %d MB", name, errandFileMax>>20)
	case size >= 0 && (n != size || hex.EncodeToString(h.Sum(nil)) != sum):
		return "", fmt.Errorf("%s arrived as %d bytes that do not match the %d sent", name, n, size)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return "", err
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if stem == "" {
		stem, ext = name, ""
	}
	for i := 1; i <= 1000; i++ {
		cand := name
		if i > 1 {
			cand = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		path := filepath.Join(dir, cand)
		err := os.Link(tmp, path)
		if err == nil {
			return path, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("%s: a thousand copies are there already", name)
}

// serveErrands answers a node's errand requests, as host: the next
// errand, a document's bytes, how one ended, and a document for the
// hub's inbox. It says whether the path was one of them.
func (h *Hub) serveErrands(w http.ResponseWriter, r *http.Request, host string) bool {
	switch {
	case r.URL.Path == errandWaitPath:
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return true
		}
		p := h.errands.next(r.Context(), host, errandWaitMax)
		if p == nil {
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p.Errand)
	case strings.HasPrefix(r.URL.Path, errandPrefix):
		id, what, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, errandPrefix), "/")
		p := h.errands.get(host, id)
		if p == nil {
			http.Error(w, "no such errand", http.StatusNotFound)
			return true
		}
		switch {
		case what == "file" && r.Method == http.MethodGet && p.Kind == ErrandDocument:
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeFile(w, r, p.data)
		case what == "done" && r.Method == http.MethodPost:
			var d ErrandDone
			b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, nodeBodyMax))
			if err == nil {
				err = json.Unmarshal(b, &d)
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return true
			}
			h.errands.finish(host, id, d)
			fmt.Fprintf(os.Stderr, "agentd hub: %s %s %s on %s: %s\n", p.Kind, p.ID, p.Name, host, doneText(d))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	case r.URL.Path == uploadPath:
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return true
		}
		path, err := saveInbox(hubInbox(h.cfg), r.URL.Query().Get("name"), http.MaxBytesReader(w, r.Body, errandFileMax), -1, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return true
		}
		fmt.Fprintf(os.Stderr, "agentd hub: %s sent %s\n", host, path)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ErrandDone{OK: true, Path: path})
	default:
		return false
	}
	return true
}

// doneText says how an errand ended, in a line.
func doneText(d ErrandDone) string {
	switch {
	case !d.OK:
		return "failed: " + d.Error
	case d.Path != "" && d.Error != "":
		return "saved as " + d.Path + ", but " + d.Error
	case d.Path != "":
		return "saved as " + d.Path
	}
	return "exit " + strconv.Itoa(d.Exit)
}

func hubInbox(cfg *Config) string {
	if cfg.Hub.Inbox != "" {
		return cfg.Hub.Inbox
	}
	return expandPath(DefaultInbox, "")
}

func hubSocket(cfg *Config) string {
	if cfg.Hub.Socket != "" {
		return cfg.Hub.Socket
	}
	return expandPath(DefaultHubSocket, "")
}

// hubSpool is where documents wait for a node: hub.spool, else hub-spool
// beside the event log.
func hubSpool(cfg *Config) string {
	if cfg.Hub.Spool != "" {
		return cfg.Hub.Spool
	}
	return filepath.Join(filepath.Dir(cfg.EventLog), "hub-spool")
}

// Local requests, from agentd send and agentd prompt on the hub's machine.
const (
	localSendPath   = "/local/send"
	localPromptPath = "/local/prompt"
	// errandAnswerGrace is how much longer than a prompt's timeout its
	// asker waits, for the node to notice and answer.
	errandAnswerGrace = 2 * time.Minute
)

// localHandler serves agentd send and agentd prompt on the hub's socket.
func (h *Hub) localHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(localSendPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		host := q.Get("host")
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if _, ok := h.cfg.Hosts[host]; !ok {
			http.Error(w, fmt.Sprintf("%q is not one of the hub's hosts", host), http.StatusNotFound)
			return
		}
		p, err := h.errands.addDocument(host, q.Get("name"), r.Body, q.Get("open") == "1", q.Get("notify") == "1")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		wait, _ := time.ParseDuration(q.Get("wait"))
		answer := map[string]any{"id": p.ID, "name": p.Name}
		select {
		case d := <-p.done:
			answer["done"] = d
		case <-time.After(wait):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(answer)
	})
	mux.HandleFunc(localPromptPath, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Host string `json:"host"`
			Errand
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, nodeBodyMax)).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
			http.Error(w, "a prompt names its host and has text", http.StatusBadRequest)
			return
		}
		if _, ok := h.cfg.Hosts[req.Host]; !ok {
			http.Error(w, fmt.Sprintf("%q is not one of the hub's hosts", req.Host), http.StatusNotFound)
			return
		}
		p := h.errands.addPrompt(req.Host, req.Errand)
		var d ErrandDone
		select {
		case d = <-p.done:
		case <-time.After(time.Duration(p.TimeoutSec)*time.Second + errandAnswerGrace):
			h.errands.cancel(p)
			d = ErrandDone{Error: fmt.Sprintf("%s did not answer within %s", req.Host, time.Duration(p.TimeoutSec)*time.Second+errandAnswerGrace)}
		case <-r.Context().Done():
			h.errands.cancel(p)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(d)
	})
	return mux
}

// listenLocal listens on the hub's socket, which only its owner may open:
// a socket left by a hub before is replaced, anything else there refused.
func listenLocal(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("hub.socket: %s is there and is not a socket", path)
		}
		os.Remove(path)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("hub.socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// errandLoop asks the hub for errands until ctx ends, doing each in its
// own goroutine, so a long prompt holds back nothing. A node whose
// configuration does not take errands asks for none.
func (n *Node) errandLoop(ctx context.Context) {
	if !n.cfg.Errands {
		return
	}
	for ctx.Err() == nil {
		e, err := n.nextErrand(ctx)
		if err != nil {
			if ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "agentd node: errands:", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
			}
			continue
		}
		if e != nil {
			go func(e Errand) {
				d := n.doErrand(ctx, e)
				if err := n.errandDone(ctx, e.ID, d); err != nil {
					fmt.Fprintln(os.Stderr, "agentd node: errand", e.ID+":", err)
				}
			}(*e)
		}
	}
}

// hubRequest is a request to the hub with the node's token.
func (n *Node) hubRequest(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(n.cfg.Hub, "/")+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	return n.errandClient().Do(req)
}

// errandClient is the node's client without a whole-request timeout,
// since a document or a held request may outlast it.
func (n *Node) errandClient() *http.Client {
	c := *n.client
	c.Timeout = 0
	return &c
}

// nextErrand is the hub's next errand for the node, nil when none came.
func (n *Node) nextErrand(ctx context.Context) (*Errand, error) {
	ctx, cancel := context.WithTimeout(ctx, errandWaitMax+20*time.Second)
	defer cancel()
	resp, err := n.hubRequest(ctx, http.MethodGet, errandWaitPath, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		var e Errand
		if err := json.NewDecoder(io.LimitReader(resp.Body, nodeBodyMax)).Decode(&e); err != nil {
			return nil, fmt.Errorf("hub: errand is not JSON: %w", err)
		}
		return &e, nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return nil, fmt.Errorf("hub: %s: %s", resp.Status, strings.TrimSpace(string(b)))
}

// doErrand does e.
func (n *Node) doErrand(ctx context.Context, e Errand) ErrandDone {
	switch e.Kind {
	case ErrandDocument:
		return n.fetchDocument(ctx, e)
	case ErrandPrompt:
		return n.runPrompt(ctx, e)
	}
	return ErrandDone{Error: "this node does not know errands of kind " + strconv.Quote(e.Kind)}
}

// fetchDocument saves e's document in the node's inbox, then opens it or
// shows a notification when asked.
func (n *Node) fetchDocument(ctx context.Context, e Errand) ErrandDone {
	resp, err := n.hubRequest(ctx, http.MethodGet, errandPrefix+url.PathEscape(e.ID)+"/file", nil)
	if err != nil {
		return ErrandDone{Error: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ErrandDone{Error: "hub: " + resp.Status}
	}
	path, err := saveInbox(n.inbox(), e.Name, resp.Body, e.Size, e.SHA256)
	if err != nil {
		return ErrandDone{Error: err.Error()}
	}
	d := ErrandDone{OK: true, Path: path}
	var after []string
	if e.Open && n.open != nil {
		if err := n.open(path); err != nil {
			after = append(after, "opening it failed: "+err.Error())
		}
	}
	if e.Notify && n.notify != nil {
		if err := n.notify(e.Name+" arrived", path); err != nil {
			after = append(after, "the notification failed: "+err.Error())
		}
	}
	d.Error = strings.Join(after, "; ")
	return d
}

// inbox is where documents land on the node.
func (n *Node) inbox() string {
	if n.cfg.Inbox != "" {
		return n.cfg.Inbox
	}
	return expandPath(DefaultInbox, "")
}

// runPrompt runs e's prompt in its directory, the home directory when it
// names none.
func (n *Node) runPrompt(ctx context.Context, e Errand) ErrandDone {
	dir := expandPath(e.Dir, "")
	if e.Dir == "" {
		dir = expandPath("~", "")
	}
	if !filepath.IsAbs(dir) {
		return ErrandDone{Error: fmt.Sprintf("the directory %q is not absolute on %s", e.Dir, n.cfg.Name)}
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return ErrandDone{Error: fmt.Sprintf("%s has no directory %s", n.cfg.Name, dir)}
	}
	argv := append([]string{}, n.cfg.Prompt...)
	if len(argv) == 0 {
		argv = append(argv, DefaultPromptCommand...)
	}
	if e.Model != "" {
		argv = append(argv, "--model", e.Model)
	}
	argv = append(argv, e.Text)
	timeout := time.Duration(e.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = DefaultPromptTimeout
	}
	code, out, err := n.prompt(ctx, dir, argv, timeout)
	if err != nil {
		return ErrandDone{Error: err.Error(), Output: out}
	}
	return ErrandDone{OK: true, Exit: code, Output: out}
}

// execPrompt runs argv in dir, in a process group of its own killed at
// timeout, with no input; it keeps the last GateOutputMax bytes of its
// output.
func execPrompt(ctx context.Context, dir string, argv []string, timeout time.Duration) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := groupCommand(ctx, dir, argv[0], argv[1:]...)
	out := &tailBuffer{max: GateOutputMax}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	if err != nil && ctx.Err() != nil {
		return 0, string(out.b), fmt.Errorf("the prompt was stopped after %s", timeout)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), string(out.b), nil
	}
	return 0, string(out.b), err
}

// errandDone tells the hub how errand id ended.
func (n *Node) errandDone(ctx context.Context, id string, d ErrandDone) error {
	b, _ := json.Marshal(d)
	resp, err := n.hubRequest(ctx, http.MethodPost, errandPrefix+url.PathEscape(id)+"/done", bytes.NewReader(b))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("hub: %s", resp.Status)
	}
	return nil
}

// sendToHub sends the file at path to the hub's inbox, and answers where
// the hub saved it.
func (n *Node) sendToHub(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	resp, err := n.hubRequest(ctx, http.MethodPost, uploadPath+"?name="+url.QueryEscape(filepath.Base(path)), f)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, nodeBodyMax))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("hub: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var d ErrandDone
	if err := json.Unmarshal(b, &d); err != nil {
		return "", fmt.Errorf("hub: answer is not JSON: %w", err)
	}
	return d.Path, nil
}

// openDetached opens path in its usual viewer, leaving it running.
func openDetached(path string) error {
	cmd := exec.Command("xdg-open", path)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// desktopNotify shows a desktop notification: by notify-send when the
// machine has it, else by the notification service on the session bus.
func desktopNotify(title, body string) error {
	if _, err := exec.LookPath("notify-send"); err == nil {
		return exec.Command("notify-send", "-a", "agentd", title, body).Run()
	}
	return exec.Command("gdbus", "call", "--session", "--dest", "org.freedesktop.Notifications",
		"--object-path", "/org/freedesktop/Notifications", "--method", "org.freedesktop.Notifications.Notify",
		"agentd", "0", "", title, body, "[]", "{}", "10000").Run()
}

// localClient reaches the hub's socket.
func localClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}}
}

// localAnswer is the body of a local request's answer, or an error naming
// what the hub said.
func localAnswer(resp *http.Response, err error, socket string) ([]byte, error) {
	if err != nil {
		return nil, fmt.Errorf("the hub's socket %s: %w (is agentd hub running here?)", socket, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4*GateOutputMax))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return b, nil
}

// sendCmd sends files: from the hub's machine to a host's inbox, or, as
// host "hub", from a node's machine to the hub's inbox.
func sendCmd(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	open := fs.Bool("open", false, "open each file on the host once it arrives")
	notify := fs.Bool("notify", false, "show a desktop notification on the host when each arrives")
	wait := fs.Duration("wait", 2*time.Minute, "how long to wait for the host to have each file")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: agentd send [-open] [-notify] [-wait D] [-config F] HOST FILE...\n  HOST is a host of the hub, or hub to send from a node's machine to the hub's inbox.")
		fs.PrintDefaults()
	}
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return errors.New("send names a host and at least one file")
	}
	host, files := fs.Arg(0), fs.Args()[1:]
	ctx := context.Background()
	if host == hubHost {
		n, err := nodeFromConfig(cfg)
		if err != nil {
			return err
		}
		for _, f := range files {
			path, err := n.sendToHub(ctx, f)
			if err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
			fmt.Fprintf(w, "%s: the hub saved it as %s\n", f, path)
		}
		return nil
	}
	socket := hubSocket(cfg)
	client := localClient(socket)
	for _, f := range files {
		file, err := os.Open(f)
		if err != nil {
			return err
		}
		q := url.Values{"host": {host}, "name": {filepath.Base(f)}, "wait": {wait.String()}}
		if *open {
			q.Set("open", "1")
		}
		if *notify {
			q.Set("notify", "1")
		}
		resp, err := client.Post("http://hub"+localSendPath+"?"+q.Encode(), "application/octet-stream", file)
		b, err := localAnswer(resp, err, socket)
		file.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		var a struct {
			Done *ErrandDone `json:"done"`
		}
		if err := json.Unmarshal(b, &a); err != nil {
			return fmt.Errorf("hub: answer is not JSON: %w", err)
		}
		switch d := a.Done; {
		case d == nil:
			fmt.Fprintf(w, "%s: queued for %s, which has not taken it yet; it arrives when %s's node next asks\n", f, host, host)
		case !d.OK:
			return fmt.Errorf("%s: %s did not save it: %s", f, host, d.Error)
		case d.Error != "":
			fmt.Fprintf(w, "%s: on %s as %s, but %s\n", f, host, d.Path, d.Error)
		default:
			fmt.Fprintf(w, "%s: on %s as %s\n", f, host, d.Path)
		}
	}
	return nil
}

// promptCmd runs a prompt on a host and prints its answer; a prompt that
// exits non-zero is an error.
func promptCmd(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("prompt", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory on the host the prompt runs in (default: its home)")
	model := fs.String("model", "", "model (default: the host's Claude Code default)")
	timeout := fs.Duration("timeout", DefaultPromptTimeout, "how long the prompt may run")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: agentd prompt [-dir D] [-model M] [-timeout D] [-config F] HOST TEXT...\n  TEXT - reads the prompt from standard input.")
		fs.PrintDefaults()
	}
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return errors.New("prompt names a host and the prompt's text")
	}
	host, text := fs.Arg(0), strings.Join(fs.Args()[1:], " ")
	if text == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		text = string(b)
	}
	if *timeout <= 0 {
		return errors.New("-timeout must be positive")
	}
	body, _ := json.Marshal(map[string]any{"host": host, "text": text, "dir": *dir, "model": *model, "timeout_sec": int64(timeout.Seconds())})
	socket := hubSocket(cfg)
	resp, err := localClient(socket).Post("http://hub"+localPromptPath, "application/json", bytes.NewReader(body))
	b, err := localAnswer(resp, err, socket)
	if err != nil {
		return err
	}
	var d ErrandDone
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("hub: answer is not JSON: %w", err)
	}
	fmt.Fprint(w, d.Output)
	if d.Output != "" && !strings.HasSuffix(d.Output, "\n") {
		fmt.Fprintln(w)
	}
	switch {
	case !d.OK:
		return fmt.Errorf("the prompt on %s failed: %s", host, d.Error)
	case d.Exit != 0:
		return fmt.Errorf("the prompt on %s exited %d", host, d.Exit)
	}
	return nil
}
