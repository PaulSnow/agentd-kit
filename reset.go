package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The idle reset (#96): a page session that should run on another account
// is moved there by agentd once nothing is active in it. agentd asks the
// session for a summary of its current work, ends it, starts a new one,
// which goes on the account placement picks, and gives the new one the
// summary. A session with active work is left alone, and its title says
// it needs a reset (#97).

// How the reset is paced. A session is idle once its transcript and its
// subagents' have been quiet for resetQuiet; a turn that never ended is
// taken as given up after resetStale. One session is not reset twice
// within resetGap, a failed try included, so a reset is never retried at
// every pass.
const (
	resetEvery   = 30 * time.Second
	resetQuiet   = 5 * time.Minute
	resetStale   = 30 * time.Minute
	resetGap     = time.Hour
	resetSummary = 5 * time.Minute
	resetPrompt  = 30 * time.Minute
)

// Reset steps.
const (
	resetAsking   = "summary"
	resetStarting = "restart"
	resetFeeding  = "feed"
	resetDone     = "done"
	resetFailed   = "failed"
)

// resetState is where a page session's move to another account stands.
type resetState struct {
	Step     string
	From, To string
	At       time.Time
	// Err is why it failed; File the summary it kept, "" when none.
	Err  string
	File string
}

// going says whether the reset is under way.
func (s *resetState) going() bool {
	return s != nil && s.Step != resetDone && s.Step != resetFailed
}

// pageSession is a session a page starts: a repository's, or the console
// for all of them.
type pageSession struct {
	name    string
	dir     string
	command []string
	cfg     *Config
}

// pageSessions are the page sessions now running.
func (u *UI) pageSessions() []pageSession {
	running := u.runningSessions()
	var out []pageSession
	for _, repo := range sortedKeys(u.workspaceRepos()) {
		if !running[workspaceSession(repo)] {
			continue
		}
		src, r, ok := u.workspacePick(repo)
		if dir := workspaceClone(r, u.home); ok && dir != "" {
			out = append(out, pageSession{workspaceSession(repo), dir, defaultWorkspaceCommand, src.cfg})
		}
	}
	if running[allConsoleSession] {
		out = append(out, u.consoleSession())
	}
	return out
}

// consoleSession is the console for all the work, as a page session.
func (u *UI) consoleSession() pageSession {
	return pageSession{allConsoleSession, u.allConsoleDir(), allConsoleCommand, u.sources[0].cfg}
}

// sgr is a terminal styling sequence.
var sgr = regexp.MustCompile("\x1b\\[[0-9;]*m")

// promptText is what stands at Claude Code's input prompt on a screen
// captured with its styling: the text the owner typed there, "" when the
// prompt is empty or shows only a suggestion, which is dim. found says
// the screen ends at a prompt at all.
func promptText(styled string) (text string, found bool) {
	lines := strings.Split(strings.TrimRight(styled, "\n \t"), "\n")
	for i, seen := len(lines)-1, 0; i >= 0 && seen < promptLines+promptAbove; i-- {
		plain := strings.Trim(sgr.ReplaceAllString(lines[i], ""), " \t │┃|")
		if plain == "" {
			continue
		}
		seen++
		rest, ok := strings.CutPrefix(plain, "❯")
		if !ok {
			if rest, ok = strings.CutPrefix(plain, ">"); !ok {
				continue
			}
		}
		if dialogOption.MatchString(strings.TrimLeft(rest, "  ")) {
			return "", false // a dialog's selected option, not the prompt
		}
		// What follows the prompt's mark, with its styling: dim text is a
		// suggestion, anything else is typed.
		_, after, _ := strings.Cut(lines[i], "❯")
		if after == "" {
			_, after, _ = strings.Cut(lines[i], ">")
		}
		dim := false
		var typed strings.Builder
		for after != "" {
			if loc := sgr.FindStringIndex(after); loc != nil && loc[0] == 0 {
				codes := strings.Split(strings.Trim(after[:loc[1]], "\x1b[m"), ";")
				for k := 0; k < len(codes); k++ {
					switch codes[k] {
					case "2":
						dim = true
					case "", "0", "22":
						dim = false
					case "38", "48", "58": // a colour: its numbers are not styles
						if k+1 < len(codes) && codes[k+1] == "5" {
							k += 2
						} else {
							k += 4
						}
					}
				}
				after = after[loc[1]:]
				continue
			}
			end := len(after)
			if loc := sgr.FindStringIndex(after); loc != nil {
				end = loc[0]
			}
			if !dim {
				typed.WriteString(after[:end])
			}
			after = after[end:]
		}
		return strings.Trim(typed.String(), " \t │┃|"), true
	}
	return "", false
}

// screenBusy says why the session's screen is not at an empty prompt, ""
// when it is: a turn running, a dialog, or text the owner typed.
func (u *UI) screenBusy(name string) string {
	styled, err := u.tmux("capture-pane", "-p", "-e", "-t", "="+name+":")
	if err != nil {
		return "its screen could not be read"
	}
	plain := sgr.ReplaceAllString(styled, "")
	var lines []string
	for _, l := range strings.Split(plain, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	// A running turn shows its spinner line above the prompt; an ended
	// turn's line has no ellipsis. Claude Code no longer says "esc to
	// interrupt" there, which promptRunning looks for.
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-promptLines-promptAbove; i-- {
		if promptRunning.MatchString(lines[i]) || spinnerLine.MatchString(lines[i]) {
			return "a turn is running"
		}
	}
	text, found := promptText(styled)
	switch {
	case !found:
		return "it is not at its prompt"
	case text != "":
		return "text is typed at its prompt"
	}
	return ""
}

// sessionTranscript is the transcript the page session writes now, and
// whether its place is known: its own, or the one a /clear typed into it
// began since. The file may not exist yet.
func (u *UI) sessionTranscript(s pageSession) (string, bool) {
	out, err := u.tmux("show-environment", "-t", "="+s.name, workspaceSessionEnv)
	_, id, ok := strings.Cut(strings.TrimSpace(out), "=")
	if err != nil || !ok || id == "" {
		return "", false
	}
	var account string
	if out, err := u.tmux("show-environment", "-t", "="+s.name, workspaceAccountEnv); err == nil {
		_, account, _ = strings.Cut(strings.TrimSpace(out), "=")
	}
	root := transcriptRootOf(s.cfg, Session{Account: account})
	if root == "" {
		return "", false
	}
	dir := TranscriptDir(root, s.dir)
	if out, err := u.tmux("display-message", "-p", "-t", "="+s.name+":", "#{session_created}"); err == nil {
		if sec, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil {
			if later := u.usage.clearedFrom(dir, id, time.Unix(sec, 0)); len(later) > 0 {
				id = later[len(later)-1]
			}
		}
	}
	return filepath.Join(dir, id+".jsonl"), true
}

// sessionBusy says why the page session is not idle, "" when it is: its
// screen is at an empty prompt, its last turn ended, and neither it nor a
// subagent of it has written its transcript within quiet. worked says the
// session has a transcript, so there is work to summarize.
func (u *UI) sessionBusy(s pageSession, now time.Time) (why string, worked bool) {
	if why := u.screenBusy(s.name); why != "" {
		return why, true
	}
	path, ok := u.sessionTranscript(s)
	if !ok {
		return "its transcript is not read", true
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		return "its transcript could not be read", true
	}
	if now.Sub(info.ModTime()) < u.resetQuiet {
		return "it was active within the last " + shortDuration(u.resetQuiet), true
	}
	subs, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "*.jsonl"))
	for _, p := range subs {
		if fi, err := os.Stat(p); err == nil && now.Sub(fi.ModTime()) < u.resetQuiet {
			return "an agent of it is active", true
		}
	}
	if last := lastTurn(path); last.Found && !(last.Role == "assistant" && last.StopReason == "end_turn") && now.Sub(info.ModTime()) < resetStale {
		return "its last turn has not ended", true
	}
	return "", true
}

// shortDuration is d as the page says it.
func shortDuration(d time.Duration) string {
	if d >= time.Minute {
		return fmt.Sprintf("%d minutes", int(d/time.Minute))
	}
	return d.String()
}

// summaryAsk is what the old session is asked. The mark at its end finds
// the reply in the transcript.
const summaryAsk = `agentd: this session is about to be restarted, to move it from the Claude account %s to %s. Reply with a summary of the current work that a fresh session can carry on from: the goal, what is done, what is in progress, the next steps, the questions open for the owner, and the branches, files and commands that matter. Reply with the summary alone and use no tools. (handover %s)`

// summaryFeed is the one line the new session is given: where the summary
// is, which it reads itself. The same line is typed when the owner gives
// it again with Give summary.
const summaryFeed = `agentd: this session replaces an earlier one that agentd restarted to move it to another Claude account. The summary that session left of its current work is in the file %s. Read it, say in a line or two where the work stands, and carry on from it when the owner next asks; start nothing by yourself.`

// handoverFile is where the summary the session name last left is kept for
// the new session to read: one known file per page session, so the owner
// can have it read again (#96).
func (u *UI) handoverFile(name string) string {
	return filepath.Join(u.handoverRoot, name+".md")
}

// keepHandover writes text as the summary the session name left, in a
// folder only this user may use: one that is a link, or another user's,
// is refused, since /tmp is shared.
func (u *UI) keepHandover(name, text string) (string, error) {
	if err := os.MkdirAll(u.handoverRoot, 0o700); err != nil {
		return "", err
	}
	fi, err := os.Lstat(u.handoverRoot)
	if err != nil {
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() || fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is not a folder of this user's alone", u.handoverRoot)
	}
	path := u.handoverFile(name)
	tmp, err := os.CreateTemp(u.handoverRoot, name+".*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
}

// hasHandover says whether a summary is kept for the session name.
func (u *UI) hasHandover(name string) bool {
	fi, err := os.Lstat(u.handoverFile(name))
	return err == nil && fi.Mode().IsRegular()
}

// giveHandover types the line that has the session name read its kept
// summary. It refuses unless the session is at an empty prompt, so it
// never types into a turn, a dialog or the owner's draft.
func (u *UI) giveHandover(name string) error {
	if !u.hasHandover(name) {
		return errNoHandover
	}
	if !u.sessionExists(name) {
		return errNoSession
	}
	if why := u.screenBusy(name); why != "" {
		return fmt.Errorf("the session is not at an empty prompt: %s", why)
	}
	return u.typeLine(name, fmt.Sprintf(summaryFeed, u.handoverFile(name)))
}

var (
	errNoHandover = errors.New("no summary is kept for this session")
	errNoSession  = errors.New("no session is running; open the page to start one, then give it the summary")
)

// typeLine types one line at the session's prompt and enters it. The
// Enter follows a moment later, or it is read as part of a paste.
func (u *UI) typeLine(name, text string) error {
	if out, err := u.tmux("send-keys", "-t", "="+name+":", "-l", "--", text); err != nil {
		return fmt.Errorf("typing: %w: %s", err, strings.TrimSpace(out))
	}
	time.Sleep(u.resetPause)
	if out, err := u.tmux("send-keys", "-t", "="+name+":", "C-m"); err != nil {
		return fmt.Errorf("entering: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// summaryReply reads the reply to the ask marked mark from the transcripts
// in dir written since the ask: the text of the assistant's turn after it,
// once that turn has ended. A /clear, or a session id agentd does not
// know, is found the same way.
func summaryReply(dir, mark string, since time.Time) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if info, err := e.Info(); err != nil || info.ModTime().Before(since.Add(-time.Second)) {
			continue
		}
		if text, ok := replyIn(filepath.Join(dir, e.Name()), mark); ok {
			return text, true
		}
	}
	return "", false
}

// replyIn is summaryReply for one transcript, read from its tail.
func replyIn(path, mark string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > 4*transcriptTail {
		f.Seek(info.Size()-4*transcriptTail, io.SeekStart)
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", false
	}
	var parts []string
	asked, ended := false, false
	for _, raw := range strings.Split(string(b), "\n") {
		var line struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			Message     struct {
				StopReason string          `json:"stop_reason"`
				Content    json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(raw), &line) != nil || line.IsSidechain {
			continue
		}
		switch line.Type {
		case "user":
			if strings.Contains(raw, mark) {
				asked, ended, parts = true, false, nil
			} else if asked {
				ended = false // the turn goes on: a tool's result
			}
		case "assistant":
			if !asked {
				continue
			}
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(line.Message.Content, &blocks) == nil {
				for _, bl := range blocks {
					if bl.Type == "text" && strings.TrimSpace(bl.Text) != "" {
						parts = append(parts, strings.TrimSpace(bl.Text))
					}
				}
			}
			ended = line.Message.StopReason == "end_turn"
		}
	}
	if !asked || !ended || len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n\n"), true
}

// handoverDir is where the summaries are kept, for the owner to read.
func (u *UI) handoverDir(cfg *Config) string {
	if cfg.EventLog != "" {
		return filepath.Join(filepath.Dir(cfg.EventLog), "handover")
	}
	return filepath.Join(u.home, ".local", "state", "agentd-kit", "handover")
}

// setReset records where name's reset stands.
func (u *UI) setReset(name string, st resetState) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.resets == nil {
		u.resets = map[string]*resetState{}
	}
	u.resets[name] = &st
}

// resetOf is where name's reset stands, nil when it never had one.
func (u *UI) resetOf(name string) *resetState {
	u.mu.Lock()
	defer u.mu.Unlock()
	if st := u.resets[name]; st != nil {
		c := *st
		return &c
	}
	return nil
}

// resetIdle looks at each page session that should be on another account
// and starts the reset of those that are idle, each on its own.
func (u *UI) resetIdle(now time.Time) {
	if u.cfg.ManualReset {
		return
	}
	pick, _ := u.pageAccount(now)
	if pick == "" {
		return
	}
	for _, s := range u.pageSessions() {
		on := u.sessionAccount(s.name)
		to := u.moveTo(on, pick)
		if to == "" {
			continue
		}
		if st := u.resetOf(s.name); st.going() || st != nil && time.Since(st.At) < u.resetGap {
			continue
		}
		why, worked := u.sessionBusy(s, time.Now())
		if why != "" {
			continue
		}
		u.setReset(s.name, resetState{Step: resetAsking, From: on, To: to, At: time.Now()})
		go u.handover(s, on, to, worked)
	}
}

// resetLoop runs resetIdle until ctx ends.
func (u *UI) resetLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(resetEvery):
		}
		u.resetIdle(u.now())
	}
}

// handover moves the idle page session s from the account on to the one
// placement picks, to as it stood: it asks for the summary, keeps it,
// ends the session, starts the new one and gives it the summary. A step
// that fails is recorded with what is left: until the session is ended
// it is as it was, and after that the summary is kept in its file.
func (u *UI) handover(s pageSession, on, to string, worked bool) {
	st := resetState{Step: resetAsking, From: on, To: to, At: time.Now()}
	fail := func(format string, args ...any) {
		st.Step, st.Err, st.At = resetFailed, fmt.Sprintf(format, args...), time.Now()
		u.setReset(s.name, st)
		fmt.Fprintf(os.Stderr, "agentd ui: resetting %s to %s: %s\n", s.name, to, st.Err)
	}
	var summary string
	if worked {
		path, ok := u.sessionTranscript(s)
		if !ok {
			fail("its transcript is not read")
			return
		}
		mark, err := newUUID()
		if err != nil {
			fail("%v", err)
			return
		}
		mark = mark[:8]
		asked := time.Now()
		if err := u.typeLine(s.name, fmt.Sprintf(summaryAsk, on, to, mark)); err != nil {
			fail("the summary was not asked for: %v", err)
			return
		}
		for until := asked.Add(u.resetSummary); ; {
			time.Sleep(u.resetPoll)
			if got, ok := summaryReply(filepath.Dir(path), mark, asked); ok {
				summary = got
				break
			}
			if !u.sessionExists(s.name) {
				fail("the session ended before it gave a summary")
				return
			}
			if time.Now().After(until) {
				fail("no summary was returned within %s; the session is as it was", shortDuration(u.resetSummary))
				return
			}
		}
		dir := u.handoverDir(s.cfg)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fail("the summary was not kept: %v", err)
			return
		}
		st.File = filepath.Join(dir, s.name+"-"+asked.UTC().Format("20060102-150405")+".md")
		head := fmt.Sprintf("Summary %s left on %s, before agentd moved it from the account %s to %s.\n\n", s.name, asked.In(u.loc).Format("2006-01-02 15:04"), on, to)
		if err := os.WriteFile(st.File, []byte(head+summary+"\n"), 0o600); err != nil {
			st.File = ""
			fail("the summary was not kept: %v", err)
			return
		}
		if _, err := u.keepHandover(s.name, head+summary+"\n"); err != nil {
			fail("the summary was not kept for the new session: %v; the session is as it was", err)
			return
		}
		// The owner may have taken the session up while it wrote.
		for i := 0; ; i++ {
			why := u.screenBusy(s.name)
			if why == "" {
				break
			}
			if i >= 10 {
				fail("the session was taken up again (%s); it is as it was, and its summary is kept", why)
				return
			}
			time.Sleep(u.resetPoll)
		}
	}
	st.Step = resetStarting
	u.setReset(s.name, st)
	// Ended and started under the one lock, so a page's terminal that sees
	// the old one end finds the new one and reconnects to it.
	u.sessionsMu.Lock()
	out, err := u.tmux("kill-session", "-t", "="+s.name)
	if err == nil {
		err = u.startSession(s.name, s.dir, s.command)
		u.sessionsMu.Unlock()
		if err != nil {
			fail("the new session did not start: %v; open its page to start one", err)
			return
		}
	} else {
		u.sessionsMu.Unlock()
		fail("the session was not ended: %v: %s", err, strings.TrimSpace(out))
		return
	}
	if now := u.sessionAccount(s.name); now != "" {
		st.To = now
	}
	if summary != "" {
		st.Step = resetFeeding
		u.setReset(s.name, st)
		for until := time.Now().Add(u.resetPrompt); ; {
			time.Sleep(u.resetPoll)
			if u.screenBusy(s.name) == "" {
				break
			}
			if !u.sessionExists(s.name) {
				fail("the new session ended before it took the summary; it is kept in %s: start the session, then Give summary", u.handoverFile(s.name))
				return
			}
			if time.Now().After(until) {
				fail("the new session did not come to its prompt within %s (a trust question, say); the summary is kept in %s: once the session is at its prompt, Give summary has it read it", shortDuration(u.resetPrompt), u.handoverFile(s.name))
				return
			}
		}
		if err := u.giveHandover(s.name); err != nil {
			fail("the summary is kept in %s, not given: %v; Give summary tries again", u.handoverFile(s.name), err)
			return
		}
	}
	st.Step, st.At = resetDone, time.Now()
	u.setReset(s.name, st)
}

// resetNote is what a page says of where its session's account stands:
// the words beside its title, "" for none, and their explanation. on is
// the account it runs on and to the one it should move to, "" when none.
func (u *UI) resetNote(s pageSession, on, to string) (text, title string) {
	st := u.resetOf(s.name)
	switch {
	case st.going():
		step := map[string]string{resetAsking: "asking it for a summary of its work", resetStarting: "starting the new session", resetFeeding: "giving the new session the summary once it is at its prompt"}[st.Step]
		return "Resetting → " + st.To, "agentd is moving this session from " + st.From + " to " + st.To + ": " + step + "."
	case to == "":
		return "", ""
	case u.cfg.ManualReset:
		return "Needs a reset → " + to, "New sessions go on " + to + ", whose week resets first. Close session and start a new one to move this one there."
	case st != nil && st.Step == resetFailed && time.Since(st.At) < u.resetGap:
		return "Needs a reset → " + to + " (the last try failed)", "agentd tried to move this session to " + st.To + ": " + st.Err + ". It tries again in an hour; Close session moves it now, without a summary."
	case st != nil && st.Step == resetFailed && time.Since(st.At) < u.resetGap && u.hasHandover(s.name):
		// Moved, but the summary did not reach the new session.
		return "Summary not given", "agentd moved this session to " + st.To + ", but " + st.Err + "."
	}
	why, _ := u.sessionBusy(s, time.Now())
	if why == "" {
		return "Reset → " + to + " next", "This session is idle and on " + on + "; new sessions go on " + to + ". agentd asks it for a summary and restarts it on " + to + " within the minute."
	}
	return "Needs a reset → " + to, "This session is on " + on + "; new sessions go on " + to + ", whose week resets first. It has active work (" + why + "), so agentd waits: once it is idle, agentd asks it for a summary and restarts it on " + to + ". Close session moves it now, without a summary."
}

// accountTitle is how a session's title names its account (#97).
func accountTitle(account string) string {
	if account == "default login" {
		return account
	}
	return "account " + account
}

// accountInfo is what the page's token line says of a session's account:
// the account, the one it should move to, the note beside the title, and
// whether a summary of an earlier reset is kept.
func (u *UI) accountInfo(s pageSession, now time.Time) map[string]any {
	on, to := u.sessionMove(s.name, now)
	text, title := u.resetNote(s, on, to)
	titled := ""
	if on != "" {
		titled = accountTitle(on)
	}
	return map[string]any{"account": on, "account_title": titled, "move_to": to, "note": text, "note_title": title, "summary": u.hasHandover(s.name)}
}

// serveHandover answers the summary the session name last left when
// agentd reset it, as plain text. It asks the page's token.
func (u *UI) serveHandover(w http.ResponseWriter, r *http.Request, name string) {
	if !u.checkQueryToken(w, r) {
		return
	}
	if !u.hasHandover(name) {
		http.Error(w, errNoHandover.Error(), http.StatusNotFound)
		return
	}
	b, err := os.ReadFile(u.handoverFile(name))
	if err != nil {
		http.Error(w, "reading the summary: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

// serveGive is the page's Give summary: a POST from the UI's own page with
// its token, which has the session read the summary kept for it. The page
// is sent back to back, with the outcome said there.
func (u *UI) serveGive(w http.ResponseWriter, r *http.Request, name, back string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !u.checkPost(w, r) {
		return
	}
	err := u.giveHandover(name)
	switch {
	case errors.Is(err, errNoHandover), errors.Is(err, errNoSession):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, "not given: "+err.Error()+"; clear it, then Give summary again", http.StatusConflict)
		return
	}
	if st := u.resetOf(name); st != nil && st.Step == resetFailed {
		st.Step, st.Err, st.At = resetDone, "", time.Now()
		u.setReset(name, *st)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// checkQueryToken says whether a GET carries the page's token, and refuses
// it if not.
func (u *UI) checkQueryToken(w http.ResponseWriter, r *http.Request) bool {
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(u.token)) != 1 {
		http.Error(w, "refused: stale page; reload it", http.StatusForbidden)
		return false
	}
	return true
}
