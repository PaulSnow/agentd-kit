package main

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Tmux is the slice of tmux the supervisor uses. Every method takes one
// exact session name; there is no listing and no pattern. The only kills
// are Replace, of the one pane of a session being rotated, and Stop, of a
// session that reached its budget.
type Tmux interface {
	HasSession(name string) (bool, error)
	// CapturePane returns the visible screen and, when lines is more than
	// zero, that many lines of history above it.
	CapturePane(name string, lines int) (string, error)
	// Pane reports what the session's pane is running.
	Pane(name string) (PaneInfo, error)
	NewSession(name, cwd, command string, width, height int) error
	// Respawn starts command again in the session's dead pane. tmux refuses
	// it for a pane that is still running, so it cannot replace a live one.
	Respawn(name, cwd, command string) error
	// Replace kills what the session's pane runs and starts command in it:
	// a rotation, which keeps the session.
	Replace(name, cwd, command string) error
	// Stop ends the session.
	Stop(name string) error
	// Type enters a line of text and submits it.
	Type(name, text string) error
	// Keys sends named keys such as Down and Enter, one at a time.
	Keys(name string, keys ...string) error
}

// PaneInfo is what tmux reports about a session's pane.
type PaneInfo struct {
	Command   string // #{pane_current_command}
	Dead      bool   // #{pane_dead}: the command exited and remain-on-exit kept the pane
	Status    int    // #{pane_dead_status}, when HasStatus
	HasStatus bool
}

var plainName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// target is the exact-match form of a tmux target. tmux matches a bare
// name as a prefix; "=" forces equality, so "w1" never reaches "w17", and
// the trailing ":" names the session's current window and pane, which
// capture-pane and send-keys need.
func target(name string) (string, error) {
	if !plainName.MatchString(name) {
		return "", fmt.Errorf("%q is not a plain session name", name)
	}
	return "=" + name + ":", nil
}

// ExecTmux runs the tmux binary, each call bounded by Timeout.
type ExecTmux struct{ Timeout time.Duration }

func (t ExecTmux) run(args ...string) (string, error) {
	out, err := runTimed(t.Timeout, "", nil, true, "tmux", args...)
	return string(out), err
}

func (t ExecTmux) HasSession(name string) (bool, error) {
	tg, err := target(name)
	if err != nil {
		return false, err
	}
	_, err = t.run("has-session", "-t", tg)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, nil
	}
	return err == nil, err
}

func (t ExecTmux) CapturePane(name string, lines int) (string, error) {
	tg, err := target(name)
	if err != nil {
		return "", err
	}
	args := []string{"capture-pane", "-p", "-t", tg}
	if lines > 0 {
		args = append(args, "-S", "-"+strconv.Itoa(lines))
	}
	out, err := t.run(args...)
	if err != nil {
		return "", fmt.Errorf("capture-pane %s: %w: %s", name, err, strings.TrimSpace(out))
	}
	return out, nil
}

// Pane asks tmux what the session's active pane is running.
func (t ExecTmux) Pane(name string) (PaneInfo, error) {
	tg, err := target(name)
	if err != nil {
		return PaneInfo{}, err
	}
	out, err := t.run("display-message", "-p", "-t", tg, "#{pane_current_command}\t#{pane_dead}\t#{pane_dead_status}")
	if err != nil {
		return PaneInfo{}, fmt.Errorf("display-message %s: %w: %s", name, err, strings.TrimSpace(out))
	}
	return parsePaneInfo(out), nil
}

// parsePaneInfo reads display-message's command, dead flag and exit status.
func parsePaneInfo(out string) PaneInfo {
	f := strings.Split(strings.TrimRight(out, "\n"), "\t")
	info := PaneInfo{Command: f[0]}
	if len(f) > 1 {
		info.Dead = f[1] == "1"
	}
	if len(f) > 2 && info.Dead {
		if n, err := strconv.Atoi(strings.TrimSpace(f[2])); err == nil {
			info.Status, info.HasStatus = n, true
		}
	}
	return info
}

// NewSession starts the session with remain-on-exit set in the same tmux
// command list, before the command can exit, so a crashed pane stays with
// its exit status and last screen.
func (t ExecTmux) NewSession(name, cwd, command string, width, height int) error {
	tg, err := target(name)
	if err != nil {
		return err
	}
	out, err := t.run("new-session", "-d", "-s", name, "-x", strconv.Itoa(width), "-y", strconv.Itoa(height), "-c", cwd, command,
		";", "set-option", "-w", "-t", tg, "remain-on-exit", "on")
	if err != nil {
		return fmt.Errorf("new-session %s: %w: %s", name, err, strings.TrimSpace(out))
	}
	return nil
}

// Respawn runs respawn-pane without -k, which tmux refuses for a pane that
// is still running: it can only replace a dead one.
func (t ExecTmux) Respawn(name, cwd, command string) error {
	tg, err := target(name)
	if err != nil {
		return err
	}
	if out, err := t.run("respawn-pane", "-t", tg, "-c", cwd, command); err != nil {
		return fmt.Errorf("respawn-pane %s: %w: %s", name, err, strings.TrimSpace(out))
	}
	return nil
}

// Replace runs respawn-pane -k: the pane's command is killed and command
// started in its place, in the same session and pane.
func (t ExecTmux) Replace(name, cwd, command string) error {
	tg, err := target(name)
	if err != nil {
		return err
	}
	if out, err := t.run("respawn-pane", "-k", "-t", tg, "-c", cwd, command); err != nil {
		return fmt.Errorf("respawn-pane -k %s: %w: %s", name, err, strings.TrimSpace(out))
	}
	return nil
}

// Stop runs kill-session on the one session of that exact name.
func (t ExecTmux) Stop(name string) error {
	if _, err := target(name); err != nil {
		return err
	}
	if out, err := t.run("kill-session", "-t", "="+name); err != nil {
		return fmt.Errorf("kill-session %s: %w: %s", name, err, strings.TrimSpace(out))
	}
	return nil
}

// Type sends the text literally, waits, then sends Return. A Return that
// arrives in the same instant as the text is read as part of a paste and
// becomes a newline inside the prompt instead of submitting it.
func (t ExecTmux) Type(name, text string) error {
	tg, err := target(name)
	if err != nil {
		return err
	}
	if out, err := t.run("send-keys", "-t", tg, "-l", text); err != nil {
		return fmt.Errorf("send-keys %s: %w: %s", name, err, strings.TrimSpace(out))
	}
	time.Sleep(time.Second)
	if out, err := t.run("send-keys", "-t", tg, "C-m"); err != nil {
		return fmt.Errorf("send-keys %s: %w: %s", name, err, strings.TrimSpace(out))
	}
	return nil
}

// Keys sends each key on its own, a second apart.
func (t ExecTmux) Keys(name string, keys ...string) error {
	tg, err := target(name)
	if err != nil {
		return err
	}
	for i, k := range keys {
		if i > 0 {
			time.Sleep(time.Second)
		}
		if out, err := t.run("send-keys", "-t", tg, k); err != nil {
			return fmt.Errorf("send-keys %s: %w: %s", name, err, strings.TrimSpace(out))
		}
	}
	return nil
}
