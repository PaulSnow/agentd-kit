// agentd keeps unattended Claude Code sessions going and delivers their
// work. Deterministic management runs here; models are asked only for
// judgment, through escalation.
//
//	agentd run [--config PATH] [--log PATH] [--shadow] [--once] [--every 5m]
//	agentd status [--config PATH] [--log PATH]
//	agentd config [--config PATH]
//	agentd mark-delivered [--config PATH] [--log PATH] SESSION WORKTREE REF
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const defaultConfigPath = "~/.config/agentic-kit/agentd.json"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "status":
		err = statusCmd(os.Args[2:])
	case "config":
		err = configCmd(os.Args[2:])
	case "mark-delivered":
		err = markDeliveredCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentd:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  agentd run [--config PATH] [--log PATH] [--shadow] [--once] [--every 5m]
  agentd status [--config PATH] [--log PATH]
  agentd config [--config PATH]
  agentd mark-delivered [--config PATH] [--log PATH] SESSION WORKTREE REF

run supervises the configured sessions; --shadow observes and logs what it
would do without acting. status prints the state table from the event log.
config prints the resolved configuration. mark-delivered records that a
worktree was already delivered as REF by other means, so run never delivers
it again.`)
}

func loadConfigFlag(fs *flag.FlagSet, args []string) (*Config, *string, error) {
	configPath := fs.String("config", defaultConfigPath, "configuration file")
	logPath := fs.String("log", "", "event log (default: event_log in the configuration)")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	cfg, err := LoadConfig(expandPath(*configPath))
	if err != nil {
		return nil, nil, err
	}
	if *logPath != "" {
		cfg.EventLog = expandPath(*logPath)
	}
	return cfg, logPath, nil
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	shadow := fs.Bool("shadow", false, "observe and log, act on nothing")
	once := fs.Bool("once", false, "one pass, then exit")
	every := fs.Duration("every", 0, "poll interval (default: poll_interval in the configuration)")
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	if *every > 0 {
		cfg.PollInterval = Duration{*every}
	}
	events, bad, err := ReadLog(cfg.EventLog)
	if err != nil {
		return err
	}
	if bad > 0 {
		fmt.Fprintf(os.Stderr, "agentd: %d malformed lines in %s ignored\n", bad, cfg.EventLog)
	}
	log, err := OpenLog(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	sup := NewSupervisor(cfg, log, RealClock{}, ExecTmux{}, ExecGit{}, ExecForge{}, ExecRunner{}, *shadow)
	sup.Replay(events)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mode := "live"
	if *shadow {
		mode = "shadow"
	}
	fmt.Fprintf(os.Stderr, "agentd %s: %d sessions, every %s, log %s\n", mode, len(cfg.Sessions), cfg.PollInterval, cfg.EventLog)
	sup.Run(ctx, *once)
	return nil
}

func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	events, bad, err := ReadLog(cfg.EventLog)
	if err != nil {
		return err
	}
	rows, meta := Summarize(events)
	if bad > 0 {
		meta["malformed"] = bad
	}
	PrintStatus(os.Stdout, rows, meta, time.Now())
	return nil
}

func configCmd(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(cfg)
}

// markDeliveredCmd records a delivery made before agentd took over, as the
// action event run would have written, so the worktree is never delivered
// twice.
func markDeliveredCmd(args []string) error {
	fs := flag.NewFlagSet("mark-delivered", flag.ContinueOnError)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 3 {
		return fmt.Errorf("usage: agentd mark-delivered SESSION WORKTREE REF")
	}
	session, worktree, ref := rest[0], expandPath(rest[1]), rest[2]
	if _, ok := cfg.Sessions[session]; !ok {
		return fmt.Errorf("session %q is not configured", session)
	}
	log, err := OpenLog(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	return log.Append(Event{Time: time.Now(), Session: session, Kind: KindAction, Details: map[string]any{
		"action": ActOpen, "delivered": session + ":" + worktree, "ref": ref, "by": "mark-delivered"}})
}
