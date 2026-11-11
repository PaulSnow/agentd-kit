package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	for _, want := range []string{`href="/r/widgets"`, `href="/status"`} {
		if !strings.Contains(home, want) {
			t.Errorf("home lacks %q", want)
		}
	}
	if strings.Contains(home, "Waits on you") {
		t.Error("home still reads status")
	}
	if strings.Contains(home, `class="session-dot"`) {
		t.Error("home marks a repository with no session as running")
	}
	rows := []workspaceRepo{{Repository: "a"}, {Repository: "z", Running: true}, {Repository: "b"}, {Repository: "m", Running: true}}
	sortWorkspaceRepos(rows)
	if got := rows[0].Repository + rows[1].Repository + rows[2].Repository + rows[3].Repository; got != "mzab" {
		t.Errorf("order %s", got)
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

// A terminal's files are stored by their bytes and answered by path, for
// the page to paste into the session; one that is not a file agentd takes,
// one without the token or from another page, and a GET are refused.
func TestTerminalFiles(t *testing.T) {
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{}, f.src)
	u.home = t.TempDir()
	u.tmuxArgs = []string{"-L", "agentd-test-none", "-f", "/dev/null"}
	if err := os.MkdirAll(filepath.Join(u.home, "repos", "github.com", "example-org", "widgets", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	page := owner(t, u, "/r/widgets")
	for _, want := range []string{`data-files="/r/widgets/files"`, `id="term-image"`, `data-token="` + u.token + `"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	if home := owner(t, u, "/"); !strings.Contains(home, `data-files="/all/files"`) {
		t.Error("the console takes no files")
	}
	shot := testPNG(t)
	for _, path := range []string{"/r/widgets/files", "/all/files"} {
		w := multipartTo(u, path, u.token, nil, upload{name: "shot.png", body: shot})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var got struct{ Paths []string }
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Paths) != 1 {
			t.Fatalf("%s: %s", path, w.Body.String())
		}
		if !strings.HasSuffix(got.Paths[0], ".png") || !strings.HasPrefix(got.Paths[0], filepath.Dir(f.cfg.EventLog)) {
			t.Errorf("%s stored at %s", path, got.Paths[0])
		}
		if b, err := os.ReadFile(got.Paths[0]); err != nil || !bytes.Equal(b, shot) {
			t.Errorf("%s: the stored file is not the image: %v", path, err)
		}
	}
	if w := multipartTo(u, "/r/widgets/files", u.token, nil, upload{name: "x.png", body: []byte("PK\x03\x04 not an image")}); w.Code != http.StatusBadRequest {
		t.Errorf("not an image: %d", w.Code)
	}
	if w := multipartTo(u, "/r/widgets/files", "wrong", nil, upload{name: "shot.png", body: shot}); w.Code != http.StatusForbidden {
		t.Errorf("wrong token: %d", w.Code)
	}
	if w := multipartFrom(u, "/r/widgets/files", "http://evil.example", u.token, nil, upload{name: "shot.png", body: shot}); w.Code != http.StatusForbidden {
		t.Errorf("another origin: %d", w.Code)
	}
	if w := multipartTo(u, "/r/widgets/files", u.token, nil); w.Code != http.StatusBadRequest {
		t.Errorf("no file: %d", w.Code)
	}
	if w := get(u, "GET", "/r/widgets/files", pageAddr, "192.0.2.20:51000"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", w.Code)
	}
	if w := multipartTo(u, "/r/no-such/files", u.token, nil, upload{name: "shot.png", body: shot}); w.Code != http.StatusNotFound {
		t.Errorf("no such repository: %d", w.Code)
	}
}

// Next is the following repository with a running session, by name in
// any case, going round; never the page's own, and none when no other
// runs. The page links back to the repositories either way.
func TestNextRunning(t *testing.T) {
	repos := []string{"sprockets", "Gadgets", "gears", "nuts", "zeta"}
	on := func(names ...string) func(string) bool {
		return func(n string) bool {
			for _, m := range names {
				if m == n {
					return true
				}
			}
			return false
		}
	}
	for _, c := range []struct {
		current string
		running []string
		want    string
	}{
		{"gears", []string{"gears", "sprockets"}, "sprockets"},
		{"sprockets", []string{"gears", "sprockets"}, "gears"},
		{"Gadgets", []string{"gears", "zeta"}, "gears"},
		{"nuts", []string{"Gadgets"}, "Gadgets"},
		{"sprockets", []string{"sprockets"}, ""},
		{"zeta", nil, ""},
		{"rivets", []string{"nuts"}, "nuts"},
	} {
		if got := nextRunning(append([]string(nil), repos...), on(c.running...), c.current); got != c.want {
			t.Errorf("after %s with %v running: %q, want %q", c.current, c.running, got, c.want)
		}
	}
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{}, f.src)
	u.home = t.TempDir()
	u.tmuxArgs = []string{"-L", "agentd-test-none", "-f", "/dev/null"}
	page := owner(t, u, "/r/widgets")
	for _, want := range []string{`id="repo-nav"`, `<a href="/">`, `id="repo-next"`, "No other session running"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s", want)
		}
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
	if home := owner(t, u, "/"); !strings.Contains(home, `class="session-dot"`) {
		t.Error("home does not mark the repository with a running session")
	}
	if out, err := u.tmux("show-options", "-t", "="+name+":", "status"); err != nil || !strings.Contains(out, "off") {
		t.Errorf("tmux's status line is not off: %v: %s", err, out)
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
	screen := func(token string) (int, string) {
		resp, err := http.Get(srv.URL + "/r/widgets/screen?token=" + token)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, text := screen(u.token); code != http.StatusOK || !strings.Contains(text, "hello-terminal") {
		t.Errorf("the screen: %d %q", code, text)
	}
	if code, _ := screen("wrong"); code != http.StatusForbidden {
		t.Errorf("the screen with a wrong token: %d", code)
	}
	if !strings.Contains(owner(t, u, "/r/widgets"), `data-screen="/r/widgets/screen"`) {
		t.Error("the page has no screen to read")
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

// A session the page starts goes on the account placement would pick of
// every configured one, the one whose week resets first, under that
// account's login; the page says which.
func TestPageSessionAccount(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	f := newLevelsFixture(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	f.cfg.Accounts = map[string]Account{"a": {ConfigDir: dirA}, "b": {ConfigDir: dirB}}
	f.cfg.Usage.Enabled = true
	read := func(account string, weekly float64, reset time.Time) {
		five := 1.0
		if err := f.log.Append(Event{V: EventVersion, Time: pageAt.Add(-time.Minute), Kind: KindAccount, Mode: ModeShared,
			Account: &AccountPayload{Account: account, Method: UsageEndpoint, FiveHour: &five, Weekly: &weekly, WeeklyReset: &reset}}); err != nil {
			t.Fatal(err)
		}
	}
	read("a", 40, pageAt.Add(96*time.Hour))
	read("b", 5, pageAt.Add(48*time.Hour))
	u, _ := testUI(t, UIConfig{}, f.src)
	u.home = t.TempDir()
	u.tmuxArgs = []string{"-L", fmt.Sprintf("agentd-test-acct-%d", time.Now().UnixNano()), "-f", "/dev/null"}
	t.Cleanup(func() { u.tmux("kill-server") })
	if account, _ := u.pageAccount(pageAt); account != "b" {
		t.Fatalf("picked %q, want b, whose week resets first", account)
	}
	name := workspaceSession("widgets")
	if err := u.ensureSession(name, t.TempDir(), []string{"sh", "-c", `echo "dir=$CLAUDE_CONFIG_DIR"; sleep 60`}); err != nil {
		t.Fatal(err)
	}
	if got := u.sessionAccount(name); got != "b" {
		t.Errorf("session account %q", got)
	}
	var screen string
	for i := 0; i < 50 && !strings.Contains(screen, "dir="); i++ {
		time.Sleep(20 * time.Millisecond)
		screen, _ = u.tmux("capture-pane", "-p", "-t", "="+name+":")
	}
	if !strings.Contains(screen, "dir="+dirB) {
		t.Errorf("not under b's login: %q", screen)
	}
	if err := os.MkdirAll(filepath.Join(u.home, "repos", "github.com", "example-org", "widgets", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	page := owner(t, u, "/r/widgets")
	if !strings.Contains(page, `<span id="term-account">, account b</span>`) {
		t.Error("the page does not say the account")
	}
	if u.sessionAccount("repo-none") != "" {
		t.Error("an account for a session not running")
	}
	if !strings.Contains(page, `data-close="/r/widgets/close"`) {
		t.Error("the page has no Close session")
	}
	if !strings.Contains(page, `<span class="topbar-title topbar-repo">widgets</span>`) || strings.Contains(owner(t, u, "/"), "topbar-repo") {
		t.Error("only the repository page shows its repository in the top bar's repository style")
	}
	if strings.Contains(page, "Needs a reset") || strings.Contains(owner(t, u, "/"), ">reset</span>") {
		t.Error("a session on the right account is said to need a reset")
	}
	read("a", 40, pageAt.Add(24*time.Hour))
	if page := owner(t, u, "/r/widgets"); !strings.Contains(page, `title="New sessions go on a, whose week resets first. Close session and start a new one to move this one there.">Needs a reset → a</span>`) {
		t.Error("the page does not say the session needs a reset")
	}
	if home := owner(t, u, "/"); !strings.Contains(home, `title="Its session is on b; new sessions go on a, whose week resets first. Open it and use Close session.">reset</span>`) {
		t.Error("home does not mark the session that needs a reset")
	}
	var tok struct {
		Account string `json:"account"`
		MoveTo  string `json:"move_to"`
	}
	if err := json.Unmarshal([]byte(owner(t, u, "/r/widgets/tokens")), &tok); err != nil || tok.Account != "b" || tok.MoveTo != "a" {
		t.Errorf("tokens %+v %v", tok, err)
	}
	if got := u.moveTo("default login", "a"); got != "a" {
		t.Errorf("default login, no account on it: %q", got)
	}
	f.cfg.Accounts["a"] = Account{ConfigDir: filepath.Join(u.home, ".claude")}
	if got := u.moveTo("default login", "a"); got != "" {
		t.Errorf("default login is a's: %q", got)
	}

	// Close session ends it, from the UI's page with its token only.
	if w := postTo(u, "/r/widgets/close", "http://evil.example", u.token, url.Values{}, "192.0.2.20:51000"); w.Code != http.StatusForbidden || !u.sessionExists(name) {
		t.Errorf("another origin: %d", w.Code)
	}
	if w := postTo(u, "/r/widgets/close", "http://"+pageAddr, "wrong", url.Values{}, "192.0.2.20:51000"); w.Code != http.StatusForbidden || !u.sessionExists(name) {
		t.Errorf("a wrong token: %d", w.Code)
	}
	if w := ownerPost(u, "/r/widgets/close", url.Values{}); w.Code != http.StatusNoContent || u.sessionExists(name) {
		t.Errorf("close: %d %s", w.Code, w.Body.String())
	}
	if w := ownerPost(u, "/r/widgets/close", url.Values{}); w.Code != http.StatusNotFound {
		t.Errorf("close again: %d", w.Code)
	}
	if w := ownerPost(u, "/all/close", url.Values{}); w.Code != http.StatusNotFound {
		t.Errorf("close the console that is not running: %d", w.Code)
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

// The console for all the work is on the home page: it waits to be opened,
// takes the same checks as any terminal, and home reattaches once it runs.
func TestAllConsole(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	f := newLevelsFixture(t)
	u, _ := testUI(t, UIConfig{SessionCommand: []string{"cat"}}, f.src)
	u.home = t.TempDir()
	u.tmuxArgs = []string{"-L", fmt.Sprintf("agentd-test-all-%d", time.Now().UnixNano()), "-f", "/dev/null"}
	t.Cleanup(func() { u.tmux("kill-server") })
	home := owner(t, u, "/")
	if !strings.Contains(home, `/all/terminal?token=`) || !strings.Contains(home, `data-wait="Open the console"`) {
		t.Error("home does not offer the console")
	}
	if w := get(u, "GET", "/all", pageAddr, "192.0.2.20:51000"); w.Code != http.StatusNotFound {
		t.Errorf("the console has a page of its own: %d", w.Code)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr, r.Host = "192.0.2.20:51000", pageAddr
		u.ServeHTTP(w, r)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	if _, code := dialTerminal(t, addr, "/all/terminal?token=wrong", "http://"+pageAddr); code != http.StatusForbidden {
		t.Errorf("wrong token: %d", code)
	}
	if _, code := dialTerminal(t, addr, "/all/terminal?token="+u.token, "http://evil.example"); code != http.StatusForbidden {
		t.Errorf("another origin: %d", code)
	}
	if u.sessionExists(allConsoleSession) {
		t.Fatal("a refused terminal started the console")
	}
	if _, code := dialTerminal(t, addr, "/all/terminal?token="+u.token, "http://"+pageAddr); code != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %d", code)
	}
	if !u.sessionExists(allConsoleSession) {
		t.Fatal("the console did not start")
	}
	if home := owner(t, u, "/"); strings.Contains(home, "data-wait") {
		t.Error("home does not reattach to the running console")
	}
}

// Every repository tracking follows is served, once, with its clone and its
// issues; one it has retired is not, and agentd's own status does not take
// them on. Two with one name stay apart.
func TestTrackedRepositories(t *testing.T) {
	f := newLevelsFixture(t)
	dir := t.TempDir()
	clone := filepath.Join(dir, "somewhere", "tracked-one")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	f.cfg.Tracking.Manifest = filepath.Join(dir, "repositories.json")
	f.cfg.Tracking.Snapshot = filepath.Join(dir, "snapshot.json")
	writeFile(t, f.cfg.Tracking.Manifest, `[{"name":"tracked-one","platform":"github","path":"example-org/tracked-one","active":true,"description":"What tracked-one is for."},
{"name":"retired-one","platform":"github","path":"example-org/retired-one","active":false},
{"name":"Widgets","platform":"github","path":"Example-Org/Widgets","active":true,"description":"What widgets is for."},
{"name":"dupe","platform":"gitlab","path":"a/dupe","active":true},
{"name":"dupe","platform":"gitlab","path":"b/dupe","active":true}]`)
	writeFile(t, f.cfg.Tracking.Snapshot, fmt.Sprintf(`{"AsOf":"2026-11-01T00:00:00Z","Host":"host-a","Repos":[],"Clones":[{"Host":%q,"Path":%q,"Platform":"github","FullPath":"example-org/tracked-one"}]}`, host, clone))
	u, _ := testUI(t, UIConfig{}, f.src)
	u.home = t.TempDir()
	u.tmuxArgs = []string{"-L", "agentd-test-none", "-f", "/dev/null"}
	u.loadCatalog()
	home := owner(t, u, "/")
	for _, want := range []string{`href="/r/tracked-one"`, `href="/r/widgets"`, `href="/r/dupe"`, `href="/r/b-dupe"`} {
		if !strings.Contains(home, want) {
			t.Errorf("home lacks %s", want)
		}
	}
	for _, want := range []string{`aria-describedby="about-tracked-one"`, "<strong>GitHub</strong> example-org/tracked-one", "What tracked-one is for.", "What widgets is for.", "<strong>GitLab</strong> b/dupe", "No description yet."} {
		if !strings.Contains(home, want) {
			t.Errorf("home lacks %s", want)
		}
	}
	if strings.Contains(home, "retired-one") || strings.Contains(home, `href="/r/Widgets"`) {
		t.Error("home lists a retired repository, or one the configuration names twice")
	}
	if page := owner(t, u, "/r/tracked-one"); !strings.Contains(page, clone) || !strings.Contains(page, `id="terminal"`) {
		t.Error("a tracked repository's page lacks its clone or terminal")
	}
	var asked []string
	fail := ""
	body := `[{"number":7,"title":"A tracked issue","state":"open","user":{"login":"al"},"created_at":"2026-09-01T00:00:00Z","updated_at":"2026-10-02T00:00:00Z"}]`
	u.readAllIssues(listedAPI{asked: &asked, body: func() string { return body }, fail: &fail})
	if page := owner(t, u, "/r/tracked-one"); !strings.Contains(page, "A tracked issue") {
		t.Error("a tracked repository's issues are not listed")
	}
	if w := ownerPost(u, "/priority", url.Values{"src": {f.src.Name}, "repository": {"tracked-one"}, "ref": {"#7"}, "level": {"high"}}); w.Code != http.StatusNoContent {
		t.Errorf("priority: %d %s", w.Code, w.Body.String())
	}
	if status := owner(t, u, "/status"); strings.Contains(status, "tracked-one") {
		t.Error("agentd's status took on a tracked repository")
	}
	if w := get(u, "GET", "/r/retired-one", pageAddr, "192.0.2.20:51000"); w.Code != http.StatusNotFound {
		t.Errorf("a retired repository's page: %d", w.Code)
	}
}

// The repository list's sorts order as they say, with the name breaking ties.
func TestRepoSorts(t *testing.T) {
	for _, s := range RepoSorts {
		if sortLabels[s] == "" {
			t.Errorf("sort %s has no label", s)
		}
	}
	row := func(name, open, closed, tokens, running, changed string) map[string]string {
		return map[string]string{"ref": name, "name": name, "open": open, "closedcount": closed, "tokens": tokens, "running": running, "changed": changed}
	}
	rows := []map[string]string{
		row("beta", "5", "1", "0", "", "100"),
		row("Alpha", "5", "9", "0", "", "300"),
		row("gamma", "1", "2", "700", "1", "200"),
		row("delta", "0", "0", "900", "1", "0"),
	}
	rows[0]["mrfail"] = "1"
	rows[2]["mrpass"] = "3"
	rows[3]["mrpass"] = "1"
	got := scriptOrder(t, rows, RepoSorts)
	for by, want := range map[string]string{
		"repo-session": "delta gamma Alpha beta",
		"repo-name":    "Alpha beta delta gamma",
		"repo-open":    "Alpha beta gamma delta",
		"repo-closed":  "Alpha gamma beta delta",
		"repo-tokens":  "delta gamma Alpha beta",
		"repo-changed": "Alpha gamma beta delta",
		"repo-mrs":     "beta gamma delta Alpha",
	} {
		if s := strings.Join(got[by], " "); s != want {
			t.Errorf("%s: %s, want %s", by, s, want)
		}
	}
}
