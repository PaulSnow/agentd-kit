package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// open_terminal (Wcace1380) only proposes: a terminal is an ops change the
// owner is shown, host, target and command, and opens on the owner's yes
// alone. The window is for the owner to type in; no agent runs anything
// in it.

// openTerminalDescription is open_terminal's description, on the ops
// server and on a session's.
const openTerminalDescription = "Propose opening a terminal window on a host's desktop (one of the hub's hosts, as agentd nodes lists them), " +
	"logged into target (a host name or ssh alias that host reaches, or local for a shell on the host itself), " +
	"optionally running command there first, such as a setup step the owner must do by hand. " +
	"The window is for the owner, a human, to type in: it is never a way for you to run anything, and you see nothing of what happens in it. " +
	"The owner is shown the host, target and command and it opens only on the owner's yes."

// openTerminalProps are open_terminal's arguments but why.
const openTerminalProps = `"host":{"type":"string","description":"the host whose desktop shows the window"},` +
	`"target":{"type":"string","description":"a host name or ssh alias the window logs into, or local"},` +
	`"command":{"type":"string","description":"shell text run in the window first, shown to the owner before the yes; the window stays open after it"}`

// proposeTerminal is a terminal on host into target as the owner will be
// shown it, or why it cannot be opened.
func proposeTerminal(cfg *Config, c OpsChange, host, target, command string) (OpsChange, error) {
	c.Supervisor = ""
	if _, ok := cfg.Hosts[host]; !ok {
		names := make([]string, 0, len(cfg.Hosts))
		for h := range cfg.Hosts {
			names = append(names, h)
		}
		sort.Strings(names)
		if len(names) == 0 {
			return c, fmt.Errorf("%q is not a host: this config names no hosts", cutUTF8(host, 60))
		}
		return c, fmt.Errorf("%q is not a host; the hosts are %s", cutUTF8(host, 60), strings.Join(names, ", "))
	}
	if err := checkTerminal(target, command); err != nil {
		return c, err
	}
	c.Host, c.Target, c.Command = host, target, command
	into := "logged into " + target
	if target == TerminalLocal {
		into = "with a shell on " + host
	}
	c.Summary = fmt.Sprintf("Open a terminal window on %s's desktop, %s, for you to use (agentd terminal %s %s).", host, into, host, target)
	if command != "" {
		c.Summary += " It runs this first, then stays open: " + strconv.Quote(command)
	}
	return c, nil
}

// executeTerminal asks the hub for the terminal the owner said yes to and
// says what was launched. Its command reaches the node through the hub,
// never the hub's log.
func (o *opsEnv) executeTerminal(r OpsRecord) (string, bool, error) {
	if err := checkTerminal(r.Target, r.Command); err != nil {
		return "", false, err
	}
	ask := o.terminal
	if ask == nil {
		ask = requestTerminal
	}
	by := r.By
	if by == "" {
		by = "ops console"
	}
	d, err := ask(hubSocket(o.cfg), r.Host, Errand{Target: r.Target, Command: r.Command, By: by}, DefaultTerminalWait)
	if err != nil {
		return "", false, err
	}
	if !d.OK {
		return "", false, fmt.Errorf("no terminal on %s: %s", r.Host, d.Error)
	}
	return fmt.Sprintf("opened %s on %s into %s", d.Emulator, r.Host, r.Target), false, nil
}

// sessionTerminal is open_terminal on a session's server: the same change,
// proposed on the ops console in the session's name, for the owner's yes.
func (srv *MCPServer) sessionTerminal(args json.RawMessage) (string, bool) {
	refuse := func(err error) (string, bool) {
		return compact(map[string]any{"refused": srv.withheld(cutUTF8(err.Error(), 2000))}), true
	}
	if srv.cfg.opsConsole() == "" {
		return refuse(errors.New("this agentd has no ops console to show the owner a terminal on; ask the owner with raise_decision instead"))
	}
	var a opsArgs
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	if p := srv.decodeArgs(args, &a); p != nil {
		return compact(map[string]any{"refused": p[0].reply()}), true
	}
	c := OpsChange{Op: OpsTerminal, Why: oneLine(a.Why, opsWhyMax), By: srv.name}
	if strings.TrimSpace(a.Why) == "" {
		return refuse(errors.New("why is empty: say why the owner should open this terminal"))
	}
	c, err := proposeTerminal(srv.cfg, c, a.Host, a.Target, a.Command)
	if err != nil {
		return refuse(err)
	}
	c.Summary = fmt.Sprintf("%s asks: %s", srv.name, c.Summary)
	return srv.recordOps(c)
}
