// agentd keeps unattended Claude Code sessions going and delivers their
// work. Deterministic management runs here; models are asked only for
// judgment, through escalation.
//
//	agentd run [--config PATH] [--log PATH] [--shadow] [--once] [--every 5m]
//	agentd status [--config PATH] [--log PATH]
//	agentd config [--config PATH]
//	agentd mark-delivered [--config PATH] [--log PATH] SESSION WORKTREE REF
//	agentd clear [--config PATH] [--log PATH] SESSION
//	agentd inventory [--config PATH]
//	agentd report [--config PATH] [--log PATH] [--shadow] [--host NAME] --out PATH
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const defaultConfigPath = "~/.config/agentd-kit/agentd.json"

// version is agentd-kit's release version.
const version = "1.0.0"

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
	case "clear":
		err = clearCmd(os.Args[2:])
	case "inventory":
		err = inventoryCmd(os.Args[2:], os.Stdout)
	case "report":
		err = reportCmd(os.Args[2:])
	case "version", "--version":
		fmt.Println("agentd", version)
		return
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
  agentd clear [--config PATH] [--log PATH] SESSION
  agentd inventory [--config PATH]
  agentd report [--config PATH] [--log PATH] [--shadow] [--host NAME] --out PATH
  agentd version

run supervises the configured sessions; --shadow observes and logs what it
would do without acting. status prints the state table from the event log.
config prints the resolved configuration. mark-delivered records that a
worktree was already delivered as REF by other means, so run never delivers
it again. clear lifts a crash-looping session's park, so it may be
restarted again. inventory lists every repository in the tracking system's
manifest with its open work and clones, from the tracking snapshot. report
writes report.json for the tracking site from the event log, atomically.`)
}

func loadConfigFlag(fs *flag.FlagSet, args []string) (*Config, *string, error) {
	configPath := fs.String("config", defaultConfigPath, "configuration file")
	logPath := fs.String("log", "", "event log (default: event_log in the configuration)")
	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	cfg, err := LoadConfig(expandPath(*configPath, ""))
	if err != nil {
		return nil, nil, err
	}
	if *logPath != "" {
		cfg.EventLog = expandPath(*logPath, "")
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
	lock, err := lockInstance(realPath(cfg.EventLog) + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	log, events, err := openLoaded(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	to := cfg.Timeouts
	sup := NewSupervisor(cfg, log, RealClock{}, ExecTmux{Timeout: to.Tmux.Duration}, ExecGit{Timeout: to.Forge.Duration},
		ExecForge{Timeout: to.Forge.Duration}, ExecRunner{Timeout: to.Escalate.Duration}, *shadow)
	sup.Replay(events)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mode := "live"
	if *shadow {
		mode = "shadow"
	}
	fmt.Fprintf(os.Stderr, "agentd %s: %d sessions, every %s, log %s\n", mode, len(cfg.Sessions), cfg.PollInterval, cfg.EventLog)
	return sup.Run(ctx, *once)
}

// realPath resolves symlinks in path, so every name for one event log
// derives one lock. A log not created yet resolves through its directory;
// failing that, the path is made absolute.
func realPath(path string) string {
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(dir, filepath.Base(path))
	}
	if p, err := filepath.Abs(path); err == nil {
		return p
	}
	return path
}

// lockInstance takes an exclusive lock on path, creating it and its
// directory, without waiting. Two runs over one event log would each act on
// its sessions and double every restart, nudge and delivery; the second is
// refused. The lock lasts until the file is closed or the process exits.
func lockInstance(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another agentd run holds %s", path)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return f, nil
}

// openLoaded opens the event log and reads it through Load, which refuses a
// corrupt log and repairs a torn last line, saying so on stderr.
func openLoaded(path string) (*Log, []Event, error) {
	log, err := OpenLog(path)
	if err != nil {
		return nil, nil, err
	}
	events, torn, err := log.Load()
	if err != nil {
		log.Close()
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if torn > 0 {
		fmt.Fprintf(os.Stderr, "agentd: dropped a torn final line of %d bytes from %s\n", torn, path)
	}
	return log, events, nil
}

func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	events, _, err := ReadLog(cfg.EventLog)
	if err != nil {
		return err
	}
	rows, meta := Summarize(events)
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

// markDeliveredCmd is the command-line face of MarkDelivered.
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
	log, _, err := openLoaded(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	return MarkDelivered(cfg, log, time.Now(), rest[0], rest[1], rest[2])
}

// clearCmd is the command-line face of ClearSession.
func clearCmd(args []string) error {
	fs := flag.NewFlagSet("clear", flag.ContinueOnError)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("usage: agentd clear SESSION")
	}
	log, _, err := openLoaded(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	return ClearSession(cfg, log, time.Now(), rest[0])
}
