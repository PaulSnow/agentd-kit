package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Claude Code asks, in a directory it does not yet trust, whether to trust
// it, and has worded and ordered that menu more than one way: "Yes,
// proceed" first and selected, or "No, exit" first and selected. agentd
// answers the menu by the option's text, never its position, and only in
// the directory it launches the session in (#32).

// TrustMenu is the folder-trust dialog as the screen shows it: its
// options in order, the selected one and the accepting one (-1 when there
// is none, or more than one), and the directories it prints.
type TrustMenu struct {
	Options  []string
	Selected int
	Accept   int
	Dirs     []string
}

var (
	// The cursor is ❯, or > on a terminal without Unicode.
	trustOption = regexp.MustCompile(`^([❯›>]\s*)?(\d)\.\s+(\S.*)$`)
	trustFooter = regexp.MustCompile(`(?i)enter to confirm`)
	trustYes    = regexp.MustCompile(`(?i)^yes\b`)
)

// trustMenu reads the dialog's frame from screen: the options below the
// last line question matches, a cursor on one of them, and the footer, so
// a menu left above it on the screen is not read as its. It is nil when
// the screen does not show all three. The directories are the absolute
// paths, a whole line each, anywhere on the screen (~ under home): one
// left over from before only makes agentd refuse.
func trustMenu(screen string, question *regexp.Regexp, home string) *TrustMenu {
	lines := strings.Split(screen, "\n")
	from := -1
	for i, line := range lines {
		line = strings.Trim(line, " \t│┃|")
		if question.MatchString(line) {
			from = i
		}
	}
	if from < 0 {
		return nil
	}
	m := &TrustMenu{Selected: -1, Accept: -1}
	for _, line := range lines {
		line = strings.Trim(line, " \t│┃|")
		if strings.HasPrefix(line, "~/") && home != "" {
			line = filepath.Join(home, line[2:])
		}
		if strings.HasPrefix(line, "/") && !strings.ContainsAny(line, " \t") {
			m.Dirs = append(m.Dirs, filepath.Clean(line))
		}
	}
	footer := false
	var accepts []int
	for _, line := range lines[from+1:] {
		line = strings.Trim(line, " \t│┃|")
		o := trustOption.FindStringSubmatch(line)
		if o != nil && o[2] == "1" && !footer {
			// A new run of options: only the last before the footer counts.
			m.Options, m.Selected, accepts = nil, -1, nil
		}
		if o != nil && o[2] == fmt.Sprint(len(m.Options)+1) && !footer {
			if o[1] != "" {
				m.Selected = len(m.Options)
			}
			if trustYes.MatchString(o[3]) {
				accepts = append(accepts, len(m.Options))
			}
			m.Options = append(m.Options, o[3])
		}
		footer = footer || trustFooter.MatchString(line)
	}
	if len(m.Options) < 2 || m.Selected < 0 || !footer {
		return nil
	}
	if len(accepts) == 1 { // two accepting options: which is meant is unclear
		m.Accept = accepts[0]
	}
	return m
}

// trustKeys is what to send to menu m: the confirm keys when the
// accepting option is selected, else only the arrows that move the cursor
// to it, so the confirm keys go only on a later screen that shows it
// selected. Why says what is not sent and why, when nothing is.
func trustKeys(m *TrustMenu, confirm []string) (keys []string, why string) {
	switch {
	case m == nil:
		return nil, "the folder-trust prompt shows no menu with a cursor and the Enter to confirm footer"
	case m.Accept < 0:
		return nil, "the folder-trust menu has no single option that accepts trust"
	case m.Selected == m.Accept:
		return confirm, ""
	}
	key, n := "Down", m.Accept-m.Selected
	if n < 0 {
		key, n = "Up", -n
	}
	for i := 0; i < n; i++ {
		keys = append(keys, key)
	}
	return keys, ""
}

// trustDirOK says whether the dialog is for the directory agentd launches
// the session in: the pane runs there, and any directory the dialog prints
// is that one. Paths compare with symbolic links resolved, as tmux and
// Claude Code report them (realPath).
func trustDirOK(sess Session, path string, m *TrustMenu) bool {
	cwd := realPath(sess.Cwd)
	if path == "" || realPath(path) != cwd {
		return false
	}
	for _, d := range m.Dirs {
		if realPath(d) != cwd {
			return false
		}
	}
	return true
}

// claudeTrusts reads Claude Code's state file for whether it trusts dir,
// which it does when dir or a directory above it was accepted. Known is
// false when the file is missing or unreadable, which says nothing.
func claudeTrusts(stateFile, dir string) (trusted, known bool) {
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return false, false
	}
	var st struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(b, &st) != nil {
		return false, false
	}
	for _, d := range []string{filepath.Clean(dir), realPath(dir)} {
		for {
			if st.Projects[d].HasTrustDialogAccepted {
				return true, true
			}
			up := filepath.Dir(d)
			if up == d {
				break
			}
			d = up
		}
	}
	return false, true
}

// claudeStateFile is the state file of the login a session runs under:
// the account's config directory's, or the home directory's.
func (s *Supervisor) claudeStateFile(sess Session) string {
	if a, ok := s.cfg.Accounts[sess.Account]; ok && sess.Account != "" && a.ConfigDir != "" {
		return filepath.Join(a.ConfigDir, ".claude.json")
	}
	return filepath.Join(s.home, ".claude.json")
}

// noteTrust tells the owner, once per directory, that Claude Code does not
// trust the directory a session is being launched in, and how to trust it
// ahead. The launch goes on, and the dialog is answered when it shows. The
// note is once per session and directory: two sessions sharing one each
// say it.
func (s *Supervisor) noteTrust(name string, sess Session, t *Track) {
	if a := s.cfg.Accounts[sess.Account]; s.home == "" && (sess.Account == "" || a.ConfigDir == "") {
		return
	}
	if _, err := s.launchLine(sess, nil); err != nil {
		return // the launch is skipped for its account, and says why
	}
	if trusted, known := claudeTrusts(s.claudeStateFile(sess), sess.Cwd); trusted || !known {
		return
	}
	s.escalate(name, "untrusted:"+sess.Cwd, fmt.Sprintf("Claude Code does not trust %s yet; %s is launching there and agentd will answer its folder-trust dialog for this configured directory. "+
		"To trust it ahead, start claude in %s once and choose \"Yes, I trust this folder\".", sess.Cwd, name, sess.Cwd), t, s.clock.Now())
}
