package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// errandRec is what a node did with errands beyond saving files.
type errandRec struct {
	mu       sync.Mutex
	opened   []string
	notified []string
	prompts  [][]string
	dirs     []string
}

func (r *errandRec) snapshot() (opened, notified []string, prompts [][]string, dirs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.opened...), append([]string(nil), r.notified...), append([][]string(nil), r.prompts...), append([]string(nil), r.dirs...)
}

// startErrands makes name's node take errands into a new inbox, recording
// what it opens, notifies and prompts, and runs its errand loop until the
// test ends. Its prompts answer "the answer" and exit 3.
func startErrands(t *testing.T, h *hubHarness, name string) (*errandRec, string) {
	t.Helper()
	n := h.nodes[name]
	inbox := t.TempDir()
	n.cfg.Errands, n.cfg.Inbox = true, inbox
	rec := &errandRec{}
	n.open = func(p string) error {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.opened = append(rec.opened, p)
		return nil
	}
	n.notify = func(_, body string) error {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.notified = append(rec.notified, body)
		return nil
	}
	n.prompt = func(_ context.Context, dir string, argv []string, _ time.Duration) (int, string, error) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		rec.prompts, rec.dirs = append(rec.prompts, argv), append(rec.dirs, dir)
		return 3, "the answer\n", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.errandLoop(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return rec, inbox
}

type sendAnswer struct {
	ID   string      `json:"id"`
	Name string      `json:"name"`
	Done *ErrandDone `json:"done"`
}

// localSend sends body as name to host through the hub's local side, as
// agentd send does, waiting up to wait for the host to have it.
func localSend(t *testing.T, h *hubHarness, host, name, body string, wait time.Duration, extra url.Values) (int, sendAnswer) {
	t.Helper()
	srv := httptest.NewServer(h.hub.localHandler())
	defer srv.Close()
	q := url.Values{"host": {host}, "name": {name}, "wait": {wait.String()}}
	for k, v := range extra {
		q[k] = v
	}
	resp, err := srv.Client().Post(srv.URL+localSendPath+"?"+q.Encode(), "application/octet-stream", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var a sendAnswer
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, a
}

// localPrompt runs a prompt through the hub's local side, as agentd
// prompt does.
func localPrompt(t *testing.T, h *hubHarness, req map[string]any) (int, ErrandDone) {
	t.Helper()
	srv := httptest.NewServer(h.hub.localHandler())
	defer srv.Close()
	b, _ := json.Marshal(req)
	resp, err := srv.Client().Post(srv.URL+localPromptPath, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d ErrandDone
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, d
}

func readInbox(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func spoolFiles(t *testing.T, h *hubHarness, host string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(hubSpool(h.cfg), host, "*"))
	return m
}

// A document sent to a host lands in its inbox once, whole; a name already
// there gets a numbered copy; it is opened or notified only when asked;
// a name with a path lands in the inbox all the same; and the hub keeps
// nothing once it is delivered.
func TestDocumentReachesNodeInbox(t *testing.T) {
	h := newHubHarness(t)
	rec, inbox := startErrands(t, h, "host-a")

	code, a := localSend(t, h, "host-a", "report.pdf", "first report", 10*time.Second, nil)
	if code != http.StatusOK || a.Done == nil || !a.Done.OK || a.Done.Path != filepath.Join(inbox, "report.pdf") {
		t.Fatalf("send: %d %+v", code, a.Done)
	}
	if got := readInbox(t, a.Done.Path); got != "first report" {
		t.Errorf("saved %q", got)
	}
	opened, notified, _, _ := rec.snapshot()
	if len(opened) != 0 || len(notified) != 0 {
		t.Errorf("opened %v, notified %v, with neither asked", opened, notified)
	}

	_, a = localSend(t, h, "host-a", "report.pdf", "second report", 10*time.Second, url.Values{"open": {"1"}})
	second := filepath.Join(inbox, "report (2).pdf")
	if a.Done == nil || a.Done.Path != second || readInbox(t, second) != "second report" || readInbox(t, filepath.Join(inbox, "report.pdf")) != "first report" {
		t.Fatalf("a clash: %+v", a.Done)
	}
	if opened, _, _, _ := rec.snapshot(); !reflect.DeepEqual(opened, []string{second}) {
		t.Errorf("opened %v", opened)
	}

	_, a = localSend(t, h, "host-a", "../../etc/passwd", "not a password", 10*time.Second, url.Values{"notify": {"1"}})
	if a.Done == nil || a.Done.Path != filepath.Join(inbox, "passwd") {
		t.Fatalf("a name with a path: %+v", a.Done)
	}
	if _, notified, _, _ := rec.snapshot(); !reflect.DeepEqual(notified, []string{filepath.Join(inbox, "passwd")}) {
		t.Errorf("notified %v", notified)
	}
	if n := h.hub.errands.pending("host-a"); n != 0 {
		t.Errorf("%d errands left", n)
	}
	if left := spoolFiles(t, h, "host-a"); len(left) != 0 {
		t.Errorf("the spool keeps %v", left)
	}
}

// A document for a node that is away waits in the spool, through a hub
// restart, and arrives when the node asks.
func TestDocumentWaitsForAbsentNode(t *testing.T) {
	h := newHubHarness(t)
	code, a := localSend(t, h, "host-b", "notes.md", "# Notes", 0, nil)
	if code != http.StatusOK || a.Done != nil || a.ID == "" {
		t.Fatalf("queued: %d %+v", code, a)
	}
	if left := spoolFiles(t, h, "host-b"); len(left) != 2 {
		t.Fatalf("spool %v", left)
	}
	h.startHub()
	if n := h.hub.errands.pending("host-b"); n != 1 {
		t.Fatalf("after a restart the hub holds %d errands", n)
	}
	_, inbox := startErrands(t, h, "host-b")
	want := filepath.Join(inbox, "notes.md")
	for i := 0; i < 250; i++ {
		if _, err := os.Stat(want); err == nil && h.hub.errands.pending("host-b") == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := readInbox(t, want); got != "# Notes" {
		t.Errorf("arrived %q", got)
	}
	if n := h.hub.errands.pending("host-b"); n != 0 {
		t.Errorf("%d errands left", n)
	}
}

// A node sends documents to the hub's inbox with its token; a clash gets
// a numbered copy, and a request without a token is refused.
func TestDocumentToHubInbox(t *testing.T) {
	h := newHubHarness(t)
	h.cfg.Hub.Inbox = t.TempDir()
	f := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(f, []byte("from the laptop"), 0o644); err != nil {
		t.Fatal(err)
	}
	n := h.nodes["host-a"]
	for _, want := range []string{"notes.md", "notes (2).md"} {
		path, err := n.sendToHub(context.Background(), f)
		if err != nil || path != filepath.Join(h.cfg.Hub.Inbox, want) || readInbox(t, path) != "from the laptop" {
			t.Fatalf("sent as %q, %v; want %s", path, err, want)
		}
	}
	resp, err := http.Post(h.srv.URL+uploadPath+"?name=x", "application/octet-stream", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a token: %s", resp.Status)
	}
}

// A prompt runs on the node in the directory asked, permission checks
// skipped, the model and the text after the command; its answer and exit
// status come back. A directory the node lacks fails, and a host the hub
// does not have is refused.
func TestPromptRunsOnNode(t *testing.T) {
	h := newHubHarness(t)
	rec, _ := startErrands(t, h, "host-a")
	code, d := localPrompt(t, h, map[string]any{"host": "host-a", "text": "What is on the desktop?", "dir": "~", "model": "opus", "timeout_sec": 60})
	if code != http.StatusOK || !d.OK || d.Exit != 3 || d.Output != "the answer\n" {
		t.Fatalf("prompt: %d %+v", code, d)
	}
	home, _ := os.UserHomeDir()
	_, _, prompts, dirs := rec.snapshot()
	want := []string{"claude", "-p", "--dangerously-skip-permissions", "--model", "opus", "What is on the desktop?"}
	if len(prompts) != 1 || !reflect.DeepEqual(prompts[0], want) || dirs[0] != home {
		t.Errorf("ran %v in %v", prompts, dirs)
	}
	if _, d := localPrompt(t, h, map[string]any{"host": "host-a", "text": "hi", "dir": "/no/such/dir"}); d.OK || !strings.Contains(d.Error, "has no directory /no/such/dir") {
		t.Errorf("a missing directory: %+v", d)
	}
	if code, _ := localPrompt(t, h, map[string]any{"host": "elsewhere", "text": "hi"}); code != http.StatusNotFound {
		t.Errorf("an unknown host: %d", code)
	}
	if code, _ := localPrompt(t, h, map[string]any{"host": "host-a", "text": "  "}); code != http.StatusBadRequest {
		t.Errorf("no text: %d", code)
	}
}

// A node whose configuration does not take errands asks for none, so its
// documents wait; another host's token cannot fetch them, and no token
// is refused.
func TestNodeWithoutErrandsTakesNothing(t *testing.T) {
	h := newHubHarness(t)
	done := make(chan struct{})
	go func() { h.nodes["host-a"].errandLoop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the errand loop of a node without errands runs")
	}
	_, a := localSend(t, h, "host-a", "plan.txt", "plan", 0, nil)
	if n := h.hub.errands.pending("host-a"); n != 1 || a.Done != nil {
		t.Fatalf("%d pending, %+v", n, a.Done)
	}
	get := func(token string) int {
		req, _ := http.NewRequest(http.MethodGet, h.srv.URL+errandPrefix+a.ID+"/file", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("token-of-host-b"); code != http.StatusNotFound {
		t.Errorf("another host's token: %d", code)
	}
	if code := get(""); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code := get("token-of-host-a"); code != http.StatusOK {
		t.Errorf("its own token: %d", code)
	}
}

// The hub's local socket is its owner's alone, replaces a socket left
// before, refuses anything else in its place, and carries agentd send's
// requests.
func TestHubSocketIsOwnerOnly(t *testing.T) {
	h := newHubHarness(t)
	sock := filepath.Join(t.TempDir(), "hub.sock")
	for i := 0; i < 2; i++ {
		l, err := listenLocal(sock)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(sock)
		if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
			t.Fatalf("socket %v %v", info.Mode(), err)
		}
		if i == 1 {
			srv := &http.Server{Handler: h.hub.localHandler()}
			go srv.Serve(l)
			defer srv.Close()
			resp, err := localClient(sock).Post("http://hub"+localSendPath+"?host=elsewhere&name=x", "application/octet-stream", strings.NewReader("x"))
			if _, err := localAnswer(resp, err, sock); err == nil || !strings.Contains(err.Error(), `"elsewhere" is not one of the hub's hosts`) {
				t.Errorf("through the socket: %v", err)
			}
		} else {
			// Leave the socket behind, as a hub that died would.
			l.(*net.UnixListener).SetUnlinkOnClose(false)
			l.Close()
			if _, err := os.Lstat(sock); err != nil {
				t.Fatal(err)
			}
		}
	}
	plain := filepath.Join(t.TempDir(), "hub.sock")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenLocal(plain); err == nil || !strings.Contains(err.Error(), "is not a socket") {
		t.Errorf("a file in its place: %v", err)
	}
}

func TestInboxName(t *testing.T) {
	for in, want := range map[string]string{"report.pdf": "report.pdf", "../../etc/passwd": "passwd", "a/b/c.txt": "c.txt", "": "document", "..": "document", `..\..\x.doc`: "x.doc"} {
		if got := inboxName(in); got != want {
			t.Errorf("inboxName(%q) = %q, want %q", in, got, want)
		}
	}
}
