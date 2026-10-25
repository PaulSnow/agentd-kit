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
	trustOption = regexp.MustCompile(`^(❯\s*)?(\d)\.\s+(\S.*)$`)
	trustFooter = regexp.MustCompile(`(?i)enter to confirm`)
	trustYes    = regexp.MustCompile(`(?i)^yes\b`)
)

// trustMenu reads the dialog's frame from screen: the options below the
// question, a cursor on one of them, and the footer. It is nil when the
// screen does not show all three.
func trustMenu(screen string) *TrustMenu {
	m := &TrustMenu{Selected: -1, Accept: -1}
	footer := false
	var accepts []int
	for _, line := range strings.Split(screen, "\n") {
		line = strings.Trim(line, " \t│┃|")
		if o := trustOption.FindStringSubmatch(line); o != nil && o[2] == fmt.Sprint(len(m.Options)+1) {
			if o[1] != "" {
				m.Selected = len(m.Options)
			}
			if trustYes.MatchString(o[3]) {
				accepts = append(accepts, len(m.Options))
			}
			m.Options = append(m.Options, o[3])
		} else if strings.HasPrefix(line, "/") && !strings.ContainsAny(line, " \t") {
			m.Dirs = append(m.Dirs, filepath.Clean(line))
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
// is that one.
func trustDirOK(sess Session, path string, m *TrustMenu) bool {
	cwd := filepath.Clean(sess.Cwd)
	if path == "" || filepath.Clean(path) != cwd {
		return false
	}
	for _, d := range m.Dirs {
		if d != cwd {
			return false
		}
	}
	return true
}

// claudeTrusts reads Claude Code's state file for whether it trusts dir:
// yes or no, and known false when the file is missing, unreadable or does
// not name the directory's choice.
func claudeTrusts(stateFile, dir string) (trusted, known bool) {
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return false, false
	}
	var st struct {
		Projects map[string]struct {
			HasTrustDialogAccepted *bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(b, &st) != nil {
		return false, false
	}
	p, ok := st.Projects[filepath.Clean(dir)]
	if !ok {
		return false, true
	}
	if p.HasTrustDialogAccepted == nil {
		return false, true
	}
	return *p.HasTrustDialogAccepted, true
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
// ahead. The launch goes on, and the dialog is answered when it shows.
func (s *Supervisor) noteTrust(name string, sess Session, t *Track) {
	if a := s.cfg.Accounts[sess.Account]; s.home == "" && (sess.Account == "" || a.ConfigDir == "") {
		return
	}
	if trusted, known := claudeTrusts(s.claudeStateFile(sess), sess.Cwd); trusted || !known {
		return
	}
	s.escalate(name, "untrusted:"+sess.Cwd, fmt.Sprintf("Claude Code does not trust %s yet; %s is launching there and agentd will answer its folder-trust dialog for this configured directory. "+
		"To trust it ahead, start claude in %s once and choose \"Yes, I trust this folder\".", sess.Cwd, name, sess.Cwd), t, s.clock.Now())
}
