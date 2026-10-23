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
// exact session name; there is no listing, no pattern, and no kill.
type Tmux interface {
	HasSession(name string) (bool, error)
	CapturePane(name string, lines int) (string, error)
	NewSession(name, cwd, command string, width, height int) error
	// Type enters a line of text and submits it.
	Type(name, text string) error
	// Keys sends named keys such as Down and Enter, one at a time.
	Keys(name string, keys ...string) error
}

var plainName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// target is the exact-match form of a tmux target. tmux matches a bare
// name as a prefix; "=" forces equality, so "s4" never reaches "s4590", and
// the trailing ":" names the session's current window and pane, which
// capture-pane and send-keys need.
func target(name string) (string, error) {
	if !plainName.MatchString(name) {
		return "", fmt.Errorf("%q is not a plain session name", name)
	}
	return "=" + name + ":", nil
}

// ExecTmux runs the tmux binary.
type ExecTmux struct{}

func (ExecTmux) run(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).CombinedOutput()
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
	out, err := t.run("capture-pane", "-p", "-t", tg, "-S", "-"+strconv.Itoa(lines))
	if err != nil {
		return "", fmt.Errorf("capture-pane %s: %w: %s", name, err, strings.TrimSpace(out))
	}
	return out, nil
}

func (t ExecTmux) NewSession(name, cwd, command string, width, height int) error {
	if _, err := target(name); err != nil {
		return err
	}
	out, err := t.run("new-session", "-d", "-s", name, "-x", strconv.Itoa(width), "-y", strconv.Itoa(height), "-c", cwd, command)
	if err != nil {
		return fmt.Errorf("new-session %s: %w: %s", name, err, strings.TrimSpace(out))
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
