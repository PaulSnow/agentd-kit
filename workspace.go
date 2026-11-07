package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// The workspace: a repository's page is a live Claude Code session in its
// clone, in a terminal in the browser, beside the session's tokens and the
// repository's issues. The session is a tmux session, so it outlives the
// page; the terminal is the page's window onto it. The page shows what the
// session itself shows and reads nothing into it.

// workspaceSessionEnv is the tmux environment variable naming the session's
// Claude Code session id, which finds its transcript.
const workspaceSessionEnv = "AGENTD_SESSION"

// defaultWorkspaceCommand starts a repository's session; {session_id} is
// replaced. It runs under a login shell, which sets the path.
var defaultWorkspaceCommand = []string{"bash", "-lc", `exec claude --session-id "$1"`, "agentd", "{session_id}"}

// terminalEnded is the close code saying the session itself has ended, so
// the page offers a new one and does not start one by reconnecting.
const terminalEnded = 4000

// workspaceSession is repo's tmux session name.
func workspaceSession(repo string) string {
	return "repo-" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, repo)
}

// workspaceClone is the folder of repo's clone, "" when none is found: the
// one its sessions use, the conventional one, or one elsewhere under the
// owner's repositories whose origin is the project.
func workspaceClone(r Repository, home string) string {
	if r.Sessions != nil && r.Sessions.Clone != "" {
		return r.Sessions.Clone
	}
	host := r.Host
	if host == "" {
		host = map[string]string{"gitlab": "gitlab.com", "github": "github.com"}[r.Forge]
	}
	isGit := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, ".git"))
		return err == nil
	}
	if dir := filepath.Join(home, "repos", host, filepath.FromSlash(r.Project)); isGit(dir) {
		return dir
	}
	found, _ := filepath.Glob(filepath.Join(home, "repos", host, "*", filepath.Base(r.Project)))
	for _, dir := range found {
		if !isGit(dir) {
			continue
		}
		out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
		if err == nil && originIs(strings.TrimSpace(string(out)), host, r.Project) {
			return dir
		}
	}
	return ""
}

// tmux runs a tmux command, on the test's own server when it has one.
func (u *UI) tmux(args ...string) (string, error) {
	out, err := runTimed(10*time.Second, "", nil, true, "tmux", append(append([]string{}, u.tmuxArgs...), args...)...)
	return string(out), err
}

// newUUID is a random version 4 UUID.
func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// sessionExists says whether the tmux session is running.
func (u *UI) sessionExists(name string) bool {
	_, err := u.tmux("has-session", "-t", "="+name)
	return err == nil
}

// ensureSession starts repo's session in dir if it is not running.
func (u *UI) ensureSession(name, dir string) error {
	u.sessionsMu.Lock()
	defer u.sessionsMu.Unlock()
	if u.sessionExists(name) {
		return nil
	}
	id, err := newUUID()
	if err != nil {
		return err
	}
	command := u.cfg.SessionCommand
	if len(command) == 0 {
		command = defaultWorkspaceCommand
	}
	args := []string{"new-session", "-d", "-s", name, "-c", dir, "-x", "160", "-y", "48", "-e", workspaceSessionEnv + "=" + id, "--"}
	for _, a := range command {
		args = append(args, strings.ReplaceAll(a, "{session_id}", id))
	}
	if out, err := u.tmux(args...); err != nil {
		return fmt.Errorf("starting the session: %w: %s", err, strings.TrimSpace(out))
	}
	// The page says what tmux's status line would.
	if out, err := u.tmux("set-option", "-t", "="+name+":", "status", "off"); err != nil {
		fmt.Fprintf(os.Stderr, "agentd ui: hiding %s's status line: %v: %s\n", name, err, strings.TrimSpace(out))
	}
	return nil
}

// sessionTokens is what repo's running session has used, and whether one is
// running; its transcript may not exist until the first reply.
func (u *UI) sessionTokens(cfg *Config, repo, dir string) (int64, bool) {
	name := workspaceSession(repo)
	out, err := u.tmux("show-environment", "-t", "="+name, workspaceSessionEnv)
	if err != nil {
		return 0, false
	}
	_, id, ok := strings.Cut(strings.TrimSpace(out), "=")
	if !ok || id == "" {
		return 0, true
	}
	root := transcriptRootOf(cfg, Session{})
	if root == "" {
		return 0, true
	}
	got, known, err := u.usage.measure(TranscriptDir(root, dir), id)
	if err != nil || !known {
		return 0, true
	}
	return got.Spend, true
}

// openPTY opens a pseudo-terminal, returning its two ends.
func openPTY() (*os.File, *os.File, error) {
	ptm, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, ptm.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		ptm.Close()
		return nil, nil, e
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, ptm.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		ptm.Close()
		return nil, nil, e
	}
	pts, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		ptm.Close()
		return nil, nil, err
	}
	return ptm, pts, nil
}

// setPTYSize tells the terminal its size.
func setPTYSize(ptm *os.File, cols, rows int) {
	size := struct{ rows, cols, x, y uint16 }{uint16(rows), uint16(cols), 0, 0}
	syscall.Syscall(syscall.SYS_IOCTL, ptm.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size)))
}

// attachSession attaches to the tmux session on a new pseudo-terminal.
func (u *UI) attachSession(name string) (*os.File, *exec.Cmd, error) {
	ptm, pts, err := openPTY()
	if err != nil {
		return nil, nil, err
	}
	defer pts.Close()
	cmd := exec.Command("tmux", append(append([]string{}, u.tmuxArgs...), "attach-session", "-t", "="+name)...)
	for _, e := range os.Environ() {
		// The page may itself run inside tmux, which would refuse to nest.
		if k, _, _ := strings.Cut(e, "="); k != "TMUX" && k != "TMUX_PANE" && k != "TERM" {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pts, pts, pts
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		ptm.Close()
		return nil, nil, err
	}
	return ptm, cmd, nil
}

// wsMagic is the WebSocket handshake's fixed string (RFC 6455).
const wsMagic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsMaxFrame bounds what a browser may send in one frame.
const wsMaxFrame = 1 << 20

// WebSocket opcodes.
const (
	wsText   = 1
	wsBinary = 2
	wsClose  = 8
	wsPing   = 9
	wsPong   = 10
)

// wsConn is the server's end of a WebSocket connection.
type wsConn struct {
	conn net.Conn
	r    *bufio.Reader
	mu   sync.Mutex
}

// write sends one unmasked frame, as a server's are.
func (c *wsConn) write(op byte, p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	head := []byte{0x80 | op}
	switch {
	case len(p) < 126:
		head = append(head, byte(len(p)))
	case len(p) < 1<<16:
		head = append(head, 126, byte(len(p)>>8), byte(len(p)))
	default:
		head = append(head, 127)
		head = binary.BigEndian.AppendUint64(head, uint64(len(p)))
	}
	c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := c.conn.Write(append(head, p...)); err != nil {
		return err
	}
	return nil
}

// closeWith sends a close frame with code.
func (c *wsConn) closeWith(code uint16) {
	c.write(wsClose, binary.BigEndian.AppendUint16(nil, code))
}

// read returns the next frame. A browser's frames are masked and, for what
// the terminal sends, whole; anything else ends the connection.
func (c *wsConn) read() (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.r, head[:]); err != nil {
		return 0, nil, err
	}
	if head[0]&0x80 == 0 || head[1]&0x80 == 0 {
		return 0, nil, errors.New("a fragmented or unmasked frame")
	}
	n := uint64(head[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > wsMaxFrame {
		return 0, nil, errors.New("a frame too long")
	}
	var mask [4]byte
	if _, err := io.ReadFull(c.r, mask[:]); err != nil {
		return 0, nil, err
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(c.r, p); err != nil {
		return 0, nil, err
	}
	for i := range p {
		p[i] ^= mask[i%4]
	}
	return head[0] & 0x0f, p, nil
}

// upgradeWebSocket answers the handshake and takes over the connection.
func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		http.Error(w, "this address takes a WebSocket", http.StatusBadRequest)
		return nil, errors.New("not a WebSocket")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot take over the connection", http.StatusInternalServerError)
		return nil, errors.New("no hijacker")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Time{}) // the server's limits are for pages
	sum := sha1.Sum([]byte(key + wsMagic))
	reply := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n"
	if _, err := conn.Write([]byte(reply)); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{conn: conn, r: rw.Reader}, nil
}

// serveTerminal connects a browser to repo's session, starting it if it is
// not running. This is a shell on the machine: beyond the owner check every
// page has, it must come from the UI's own page, with the UI's token.
func (u *UI) serveTerminal(w http.ResponseWriter, r *http.Request, repo string) {
	if o := r.Header.Get("Origin"); o == "" || !strings.EqualFold(o, "http://"+r.Host) {
		http.Error(w, "refused: a terminal must come from the UI", http.StatusForbidden)
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(u.token)) != 1 {
		http.Error(w, "refused: stale page; reload it", http.StatusForbidden)
		return
	}
	src := u.levelPick(repo)
	if src == nil {
		http.NotFound(w, r)
		return
	}
	dir := workspaceClone(src.cfg.Repositories[repo], u.home)
	if dir == "" {
		http.Error(w, "no clone of "+repo+" was found", http.StatusNotFound)
		return
	}
	name := workspaceSession(repo)
	if err := u.ensureSession(name, dir); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ptm, cmd, err := u.attachSession(name)
	if err != nil {
		http.Error(w, "attaching: "+err.Error(), http.StatusInternalServerError)
		return
	}
	done := func() {
		ptm.Close()
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		cmd.Wait()
	}
	ws, err := upgradeWebSocket(w, r)
	if err != nil {
		done()
		return
	}
	defer ws.conn.Close()
	defer done()
	go func() { // the browser's keys and size
		defer ptm.Close()
		for {
			op, p, err := ws.read()
			if err != nil {
				return
			}
			switch op {
			case wsBinary:
				if _, err := ptm.Write(p); err != nil {
					return
				}
			case wsText:
				var size struct{ Cols, Rows int }
				if json.Unmarshal(p, &size) == nil && size.Cols > 0 && size.Rows > 0 && size.Cols < 1000 && size.Rows < 1000 {
					setPTYSize(ptm, size.Cols, size.Rows)
				}
			case wsPing:
				ws.write(wsPong, p)
			case wsClose:
				return
			}
		}
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := ptm.Read(buf)
		if n > 0 && ws.write(wsBinary, buf[:n]) != nil {
			return
		}
		if err != nil {
			break
		}
	}
	if !u.sessionExists(name) {
		ws.closeWith(terminalEnded)
	}
}

// plainRow is an issue as the workspace's lists show it: what the forge
// says and what the owner set, read into no further.
func plainRow(cfg *Config, st *LogState, repo string, i IndexPayload) LevelIssue {
	x := IndexIssue{IndexPayload: i}
	li := LevelIssue{Repository: repo, Ref: i.Ref, Number: x.Number(), Title: i.Title, Labels: i.Labels, Milestone: i.Milestone, Author: i.Author,
		Opened: i.Opened, Changed: i.Changed, URL: webURL(cfg.Repositories[repo], '#', x.Number()), MRRank: len(loopRanks) + 1, Plain: true}
	li.setBrowserFields(cfg, st, &x)
	if i.Withheld > 0 {
		li.Title = "(withheld: it matched a confidential pattern)"
	}
	return li
}

// openRows are repo's open issues, the latest change first.
func openRows(cfg *Config, st *LogState, repo string) []LevelIssue {
	rows := []LevelIssue{}
	for _, l := range st.ListedIssues[repo] {
		if !l.closed() {
			rows = append(rows, plainRow(cfg, st, repo, l.IndexPayload))
		}
	}
	sortIssues(rows, "last-change")
	return rows
}

// openCount is how many open issues of repo are listed.
func openCount(st *LogState, repo string) int {
	n := 0
	for _, l := range st.ListedIssues[repo] {
		if !l.closed() {
			n++
		}
	}
	return n
}

// WorkspaceSorts are the open issue list's sorts, the first the default.
var WorkspaceSorts = []string{"last-change", "priority", "umbrella", "stale", "newest", "oldest", "label", "milestone", "author"}

// uiWorkspace is a repository's page.
type uiWorkspace struct {
	uiPage
	Repository  string
	URL         string
	Src         string
	Host        string
	Clone       string
	Rows        []LevelIssue
	ClosedCount int
	Sorts       []string
	Reading     issueReading
}

// TerminalURL is the page's WebSocket address, with the UI's token.
func (p *uiWorkspace) TerminalURL() string {
	return "/r/" + url.PathEscape(p.Repository) + "/terminal?token=" + url.QueryEscape(p.Token)
}

// workspaceView is repo's page, nil when no source serves it.
func (u *UI) workspaceView(repo string, now time.Time) (*uiWorkspace, error) {
	var v *uiWorkspace
	ok, err := u.levelSource(repo, now, func(src *uiSource, cfg *Config, st *LogState, ds []UIDecision) {
		host, _ := os.Hostname()
		v = &uiWorkspace{uiPage: uiPage{Title: repo, Token: u.token, Back: "/r/" + url.PathEscape(repo), Now: now, loc: u.loc},
			Repository: repo, URL: webURL(cfg.Repositories[repo], 0, 0), Src: src.Name, Host: host,
			Clone: workspaceClone(cfg.Repositories[repo], u.home), Rows: openRows(cfg, st, repo), ClosedCount: closedCount(st, repo),
			Sorts: WorkspaceSorts, Reading: src.issueReads[repo]}
	})
	if !ok || err != nil {
		return nil, err
	}
	return v, nil
}

// serveTokens answers the page's token line.
func (u *UI) serveTokens(w http.ResponseWriter, repo string) {
	src := u.levelPick(repo)
	if src == nil {
		http.Error(w, "no such repository", http.StatusNotFound)
		return
	}
	spend, running := u.sessionTokens(src.cfg, repo, workspaceClone(src.cfg.Repositories[repo], u.home))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"tokens": tokens(spend), "running": running})
}

// workspaceRepo is a repository on the home page.
type workspaceRepo struct {
	Repository string
	URL        string
	Open       int
	Closed     int
	Running    bool
	Tokens     int64
}

// uiHome lists the repositories.
type uiHome struct {
	uiPage
	Repos []workspaceRepo
}

// TokenText is n tokens as the page shows them.
func (p *uiHome) TokenText(n int64) string { return tokens(n) }

// homeView is the home page.
func (u *UI) homeView(now time.Time) *uiHome {
	v := &uiHome{uiPage: uiPage{Title: "Repositories", Token: u.token, Back: "/", Now: now, loc: u.loc}}
	seen := map[string]bool{}
	for _, src := range u.sources {
		for _, repo := range sortedKeys(src.cfg.Repositories) {
			if !seen[repo] && u.levelPick(repo) != nil {
				seen[repo] = true
			}
		}
	}
	for _, repo := range sortedKeys(seen) {
		src := u.levelPick(repo)
		src.mu.Lock()
		row := workspaceRepo{Repository: repo, URL: webURL(src.cfg.Repositories[repo], 0, 0), Open: openCount(src.st, repo), Closed: closedCount(src.st, repo)}
		cfg := src.cfg
		src.mu.Unlock()
		row.Tokens, row.Running = u.sessionTokens(cfg, repo, workspaceClone(cfg.Repositories[repo], u.home))
		v.Repos = append(v.Repos, row)
	}
	return v
}

// serveWorkspace serves /r/REPO and what belongs to it.
func (u *UI) serveWorkspace(w http.ResponseWriter, r *http.Request) {
	repo, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/r/"), "/")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := u.now()
	var name string
	var view any
	var err error
	switch rest {
	case "terminal":
		u.serveTerminal(w, r, repo)
		return
	case "tokens":
		u.serveTokens(w, repo)
		return
	case "":
		// The terminal library writes its colours as a style element.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; img-src 'self' blob:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		name = "workspace"
		var v *uiWorkspace
		if v, err = u.workspaceView(repo, now); err == nil && v == nil {
			http.NotFound(w, r)
			return
		}
		view = v
	case "closed":
		name = "closed-issues"
		var v *uiClosedPage
		if v, err = u.closedView(repo, now); err == nil && v == nil {
			http.NotFound(w, r)
			return
		}
		view = v
	default:
		http.NotFound(w, r)
		return
	}
	u.render(w, name, view, err)
}
