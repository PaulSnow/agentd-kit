package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func listIssue(t *testing.T, f *levelsFixture, ref, title, state string) {
	t.Helper()
	i := ListedIssue{IndexPayload: IndexPayload{Repository: "widgets", Ref: ref, Title: title, State: state, Author: "al", Opened: pageAt, Changed: pageAt}}
	f.src.mu.Lock()
	defer f.src.mu.Unlock()
	if err := f.src.recordIssues([]issueRecord{{At: pageAt, Issue: &i}}); err != nil {
		t.Fatal(err)
	}
}

// Home lists the repositories, and a repository's page is the terminal
// beside its issues, with none of the status the fold works out.
func TestWorkspacePage(t *testing.T) {
	f := newLevelsFixture(t)
	listIssue(t, f, "#201", "An open one", "open")
	listIssue(t, f, "#202", "A closed one", "closed")
	u, _ := testUI(t, UIConfig{}, f.src)
	u.home = t.TempDir()
	u.tmuxArgs = []string{"-L", "agentd-test-none", "-f", "/dev/null"}
	home := owner(t, u, "/")
	for _, want := range []string{`href="/r/widgets"`, "no session", `href="/status"`} {
		if !strings.Contains(home, want) {
			t.Errorf("home lacks %q", want)
		}
	}
	if strings.Contains(home, "Waits on you") {
		t.Error("home still reads status")
	}
	if err := os.MkdirAll(filepath.Join(u.home, "repos", "github.com", "example-org", "widgets", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	page := owner(t, u, "/r/widgets")
	for _, want := range []string{`id="terminal"`, `/r/widgets/terminal?token=`, "An open one", `/r/widgets/closed`, `id="tokens"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, not := range []string{"A closed one", "needs you", "row-sentence", "decision-card", `id="needs"`, `id="requests"`, "Being worked"} {
		if strings.Contains(page, not) {
			t.Errorf("page shows %q", not)
		}
	}
	if !strings.Contains(owner(t, u, "/r/widgets/closed"), "A closed one") {
		t.Error("no closed issue on the closed list")
	}
	orders := scriptOrder(t, boxRows(t, page, "panel"), WorkspaceSorts)
	if len(orders) != len(WorkspaceSorts) {
		t.Fatalf("orders: %v", orders)
	}
	for _, s := range WorkspaceSorts {
		if sortLabels[s] == "" {
			t.Errorf("sort %s has no label", s)
		}
	}
	if status := owner(t, u, "/status"); !strings.Contains(status, `class="home-table"`) {
		t.Error("the status page went away")
	}
	for _, asset := range []string{"/xterm.js", "/xterm.css", "/addon-fit.js"} {
		if w := get(u, "GET", asset, pageAddr, "192.0.2.20:51000"); w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Errorf("%s: %d", asset, w.Code)
		}
	}
}

// The clone is found where it is conventional, or by its origin elsewhere,
// and a folder of the same name with another origin is not it.
func TestWorkspaceClone(t *testing.T) {
	home := t.TempDir()
	r := Repository{Forge: "github", Project: "example-org/widgets"}
	if got := workspaceClone(r, home); got != "" {
		t.Fatalf("found %q in nothing", got)
	}
	git := func(dir, origin string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "init", "-q")
		gitRun(t, dir, "remote", "add", "origin", origin)
	}
	other := filepath.Join(home, "repos", "github.com", "someone", "widgets")
	git(other, "git@github.com:someone/widgets.git")
	if got := workspaceClone(r, home); got != "" {
		t.Fatalf("took another project's clone: %q", got)
	}
	fork := filepath.Join(home, "repos", "github.com", "fork", "widgets")
	git(fork, "git@github.com:example-org/widgets.git")
	if got := workspaceClone(r, home); got != fork {
		t.Fatalf("by origin: %q", got)
	}
	usual := filepath.Join(home, "repos", "github.com", "example-org", "widgets")
	git(usual, "git@github.com:example-org/widgets.git")
	if got := workspaceClone(r, home); got != usual {
		t.Fatalf("conventional: %q", got)
	}
	if got := workspaceClone(Repository{Forge: "github", Project: "x/y", Sessions: &SessionsPolicy{Clone: "/the/clone"}}, home); got != "/the/clone" {
		t.Fatalf("sessions' clone: %q", got)
	}
}

// wsClient is the browser's end of a terminal, for the test.
type wsClient struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialTerminal(t *testing.T, addr, target, origin string) (*wsClient, int) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n", target)
	if origin != "" {
		fmt.Fprintf(conn, "Origin: %s\r\n", origin)
	}
	fmt.Fprint(conn, "\r\n")
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusSwitchingProtocols && resp.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept %q", resp.Header.Get("Sec-WebSocket-Accept"))
	}
	return &wsClient{conn, r}, resp.StatusCode
}

func (c *wsClient) send(t *testing.T, op byte, p []byte) {
	t.Helper()
	var mask [4]byte
	rand.Read(mask[:])
	frame := []byte{0x80 | op, 0x80 | byte(len(p))}
	frame = append(frame, mask[:]...)
	for i, b := range p {
		frame = append(frame, b^mask[i%4])
	}
	if _, err := c.conn.Write(frame); err != nil {
		t.Fatal(err)
	}
}

// next is the next frame from the server.
func (c *wsClient) next() (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.r, head[:]); err != nil {
		return 0, nil, err
	}
	n := int(head[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return 0, nil, err
		}
		n = int(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return 0, nil, err
		}
		n = int(binary.BigEndian.Uint64(b[:]))
	}
	p := make([]byte, n)
	_, err := io.ReadFull(c.r, p)
	return head[0] & 0x0f, p, err
}

// The terminal is a real session: refused without the UI's origin or token
// or for anyone but the owner, it carries keys and output both ways, the
// session outlives the connection, and its end closes the terminal with the
// code that stops the page reconnecting.
func TestTerminal(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{SessionCommand: []string{"cat"}}, f.src)
	u.home = t.TempDir()
	sock := fmt.Sprintf("agentd-test-%d", time.Now().UnixNano())
	u.tmuxArgs = []string{"-L", sock, "-f", "/dev/null"}
	t.Cleanup(func() { u.tmux("kill-server") })
	if err := os.MkdirAll(filepath.Join(u.home, "repos", "github.com", "example-org", "widgets", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	remote := "192.0.2.20:51000"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr, r.Host = remote, pageAddr
		u.ServeHTTP(w, r)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	origin := "http://" + pageAddr
	target := "/r/widgets/terminal?token=" + u.token
	name := workspaceSession("widgets")

	if _, code := dialTerminal(t, addr, target, ""); code != http.StatusForbidden {
		t.Errorf("no origin: %d", code)
	}
	if _, code := dialTerminal(t, addr, target, "http://evil.example"); code != http.StatusForbidden {
		t.Errorf("another origin: %d", code)
	}
	if _, code := dialTerminal(t, addr, "/r/widgets/terminal?token=wrong", origin); code != http.StatusForbidden {
		t.Errorf("wrong token: %d", code)
	}
	remote = "192.0.2.30:51000"
	if _, code := dialTerminal(t, addr, target, origin); code != http.StatusForbidden {
		t.Errorf("another login: %d", code)
	}
	remote = "192.0.2.20:51000"
	if u.sessionExists(name) {
		t.Fatal("a refused terminal started a session")
	}

	c, code := dialTerminal(t, addr, target, origin)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %d", code)
	}
	if !u.sessionExists(name) {
		t.Fatal("no session")
	}
	c.send(t, wsText, []byte(`{"cols":100,"rows":30}`))
	c.send(t, wsBinary, []byte("hello-terminal\r"))
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var seen strings.Builder
	for !strings.Contains(seen.String(), "hello-terminal") {
		op, p, err := c.next()
		if err != nil {
			t.Fatalf("read after %q: %v", seen.String(), err)
		}
		if op == wsBinary {
			seen.Write(p)
		}
	}
	c.conn.Close()
	if !u.sessionExists(name) {
		t.Fatal("the session ended with the connection")
	}
	if _, running := u.sessionTokens(f.cfg, "widgets", u.home); !running {
		t.Error("tokens say no session")
	}

	c, code = dialTerminal(t, addr, target, origin)
	if code != http.StatusSwitchingProtocols {
		t.Fatalf("again: %d", code)
	}
	if out, err := u.tmux("kill-session", "-t", "="+name); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		op, p, err := c.next()
		if err != nil {
			t.Fatalf("no close frame: %v", err)
		}
		if op == wsClose {
			if len(p) < 2 || binary.BigEndian.Uint16(p) != terminalEnded {
				t.Fatalf("close %v", p)
			}
			break
		}
	}
}

// ui/xterm.js is @xterm/xterm 6.0.0's lib/xterm.js without its source map
// line. bin/private-check passes over it for project names, so it must not
// change without this hash changing with it.
func TestVendoredTerminalLibrary(t *testing.T) {
	b, err := uiAssets.ReadFile("ui/xterm.js")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != "98d0973151aff2991d335b1adbbdac2e14da26341abe329d677d4c0034402bdf" {
		t.Fatalf("ui/xterm.js changed: %s", got)
	}
}
