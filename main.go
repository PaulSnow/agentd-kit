// agentd keeps unattended Claude Code sessions going and delivers their
// work. Deterministic management runs here; models are asked only for
// judgment, through escalation.
//
//	agentd run [--config PATH] [--log PATH] [--shadow] [--once] [--every 5m]
//	agentd status [--config PATH] [--log PATH] [--mode live|shadow]
//	agentd config [--config PATH]
//	agentd mark-delivered [--config PATH] [--log PATH] SESSION WORKTREE REF
//	agentd clear [--config PATH] [--log PATH] SESSION
//	agentd inventory [--config PATH]
//	agentd report [--config PATH] [--log PATH] [--shadow] [--host NAME] --out PATH
//	agentd page [--config PATH] [--log PATH] [--shadow]
//	agentd items [--config PATH] [--log PATH] [--new] [--seen ID,...]
//	agentd cases [--config PATH] [--log PATH] [--shadow] [--json]
//	agentd cases [--config PATH] [--log PATH] rule CASE restriction|waiver|close|reopen [TEXT]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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
	case "sessions":
		err = sessionsCmd(os.Args[2:])
	case "loop":
		err = loopCmd(os.Args[2:])
	case "console":
		err = consoleCmd(os.Args[2:])
	case "policy":
		err = policyCmd(os.Args[2:], os.Stdout)
	case "inventory":
		err = inventoryCmd(os.Args[2:], os.Stdout)
	case "report":
		err = reportCmd(os.Args[2:])
	case "ui":
		err = uiCmd(os.Args[2:])
	case "accounts":
		err = accountsCmd(os.Args[2:], os.Stdout)
	case "items":
		err = itemsCmd(os.Args[2:], os.Stdout)
	case "triage":
		err = triageCmd(os.Args[2:], os.Stdout)
	case "cases":
		err = casesCmd(os.Args[2:], os.Stdout)
	case "mcp":
		err = mcpCmd(os.Args[2:], os.Stdin, os.Stdout)
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
  agentd status [--config PATH] [--log PATH] [--mode live|shadow]
  agentd config [--config PATH]
  agentd mark-delivered [--config PATH] [--log PATH] SESSION WORKTREE REF
  agentd clear [--config PATH] [--log PATH] SESSION
  agentd sessions [--config PATH] [--log PATH] end NAME
  agentd loop [--config PATH] [--log PATH] retry REPOSITORY REQUEST
  agentd policy [--config PATH] [--log PATH] show REPO
  agentd console [--config PATH] [--log PATH] [--shadow] reply CONSOLE TEXT...
  agentd console [--config PATH] [--log PATH] [--repository REPO] import FILE
  agentd console post [--config PATH] [--log PATH] [--kind KIND] [--from NAME] [--to SESSION] CONSOLE TEXT...|-
  agentd console inbox [--config PATH] [--log PATH] [--from NAME] CONSOLE
  agentd inventory [--config PATH]
  agentd report [--config PATH] [--log PATH] [--shadow] [--host NAME] --out PATH
  agentd ui [--config PATH] [--log PATH]
  agentd accounts [--config PATH]
  agentd items [--config PATH] [--log PATH] [--new] [--seen ID,...]
  agentd triage [--config PATH] --items FILE
  agentd cases [--config PATH] [--log PATH] [--shadow] [--json]
  agentd cases [--config PATH] [--log PATH] rule CASE restriction|waiver|close|reopen [TEXT]
  agentd cases [--config PATH] [--log PATH] goal CASE FILE|-
  agentd mcp [--config PATH] [--log PATH] --session NAME
  agentd version

run supervises the configured sessions; --shadow observes and logs what it
would do without acting. status prints the state table from the event log.
config prints the resolved configuration. mark-delivered records that a
worktree was already delivered as REF by other means, so run never delivers
it again. clear lifts a crash-looping session's park, so it may be
restarted again. sessions end stops a session agentd started and takes it
off the supervised sessions, keeping its files. loop retry takes a request's failed review or judge step again.
policy show prints a repository's effective policy, each key with
where it came from: the owner's configuration, a default, or the
repository's own .agentd/policy.json as last read. console reply applies the owner's
reply in a console, as its tool hands it on: "SESSION ID ANSWER" answers a
decision, "seen ID..." marks items seen. inventory lists every repository in the tracking system's
manifest with its open work and clones, from the tracking snapshot. report
writes report.json for the tracking site from the event log, atomically.
items lists what is new since the owner last looked: issues, requests and
discussions from the tracking snapshots, and mail findings; --seen marks
items seen. ui serves the owner's UI on ui.address, to the owner only:
this config's log and ui.logs' merged by repository, with what needs the
owner, each repository's console and each session; its writes are answers,
deadlines, items marked seen and console replies. accounts reads every
configured account's plan usage now and prints it: the five-hour and weekly
utilization, their resets, and overage; it logs nothing. console import appends
the old console's history in FILE, one JSON object per line, once: at or
ts (Unix seconds), from, kind and text. console post records a post in a
console of this config's log, of KIND info (the default), decision,
delivered, ci or finished, from NAME (default this tmux session, else
cli), TEXT or - for standard input, at most 16 KiB; --to also types it
into that tmux session. console inbox prints the owner's replies for a
console that NAME has not read, one JSON object a line, and records them
read. triage tries the triage job on the items in
FILE with one call (retried once on failure) and prints its checked answer and cost; it logs nothing.
cases lists the cases, one per issue, ranked: the owner's pins and the
repositories' order from tracking.priorities when that file exists, then
what needs the owner, triage urgency and age; rule records the owner's
one-shot restriction or waiver on a case, or closes or reopens it; goal
records the done-when items of the issue body in FILE (- for standard
input) as the case's goal, A1 on. mcp serves the session's tools,
report_progress and request_step, on standard input and output for its
case alone; the restart command's {mcp} passes it to a fresh session.`)
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
	if cfg.Poll.GitHub || cfg.Poll.GitLab {
		sup.api = ExecForgeAPI{Timeout: to.Forge.Duration}
	}
	sup.home, _ = os.UserHomeDir()
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
	modeFlag := fs.String("mode", "", "live or shadow (default: the latest run's mode)")
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	events, _, err := ReadLog(cfg.EventLog)
	if err != nil {
		return err
	}
	mode := *modeFlag
	switch mode {
	case "":
		mode = statusMode(events)
	case ModeLive, ModeShadow:
	default:
		return fmt.Errorf("--mode must be live or shadow")
	}
	rows, meta := Summarize(cfg, events, mode)
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
	log, events, err := openLoaded(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	return MarkDelivered(projected(cfg, events), log, time.Now(), rest[0], rest[1], rest[2])
}

// sessionsCmd is the command-line face of EndSession.
func sessionsCmd(args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 2 || rest[0] != "end" {
		return fmt.Errorf("usage: agentd sessions end NAME")
	}
	log, events, err := openLoaded(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	return EndSession(cfg, log, events, time.Now(), rest[1])
}

// loopCmd is agentd loop retry REPOSITORY REQUEST.
func loopCmd(args []string) error {
	fs := flag.NewFlagSet("loop", flag.ContinueOnError)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 3 || rest[0] != "retry" {
		return fmt.Errorf("usage: agentd loop retry REPOSITORY REQUEST")
	}
	log, events, err := openLoaded(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	return RetryLoop(cfg, log, events, time.Now(), rest[1], rest[2])
}

// consoleCmd is the command-line face of ConsoleReply, ImportConsole,
// PostMessage and ConsoleInbox. The decisions are read as the live run
// holds them, or with --shadow the shadow run, as the page reads them.
func consoleCmd(args []string) error {
	if len(args) > 0 && (args[0] == "post" || args[0] == "inbox") {
		return consolePostCmd(args[0], args[1:], os.Stdin, os.Stdout)
	}
	fs := flag.NewFlagSet("console", flag.ContinueOnError)
	shadow := fs.Bool("shadow", false, "answer the decisions the shadow run holds")
	repo := fs.String("repository", "", "import: the repository or console the lines are for, unless a line names its own")
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	mode := ModeLive
	if *shadow {
		mode = ModeShadow
	}
	rest := fs.Args()
	if len(rest) > 0 && (rest[0] == "post" || rest[0] == "inbox") {
		if *shadow || *repo != "" {
			return fmt.Errorf("console %s takes neither --shadow nor --repository", rest[0])
		}
		return consolePostCmd(rest[0], append(configArgs(fs), rest[1:]...), os.Stdin, os.Stdout)
	}
	importing := len(rest) == 2 && rest[0] == "import"
	if !importing && (len(rest) < 3 || rest[0] != "reply") {
		return fmt.Errorf("usage: agentd console reply CONSOLE TEXT... | agentd console --repository REPO import FILE | agentd console post CONSOLE TEXT|- | agentd console inbox CONSOLE")
	}
	log, events, err := openLoaded(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	if importing {
		f, err := os.Open(rest[1])
		if err != nil {
			return err
		}
		defer f.Close()
		added, skipped, err := ImportConsole(cfg, log, events, time.Now(), *repo, f)
		fmt.Printf("imported %d lines, %d already imported\n", added, skipped)
		return err
	}
	st := NewLogState(cfg, mode).Fold(events)
	return ConsoleReply(cfg, log, st, time.Now(), rest[1], strings.Join(rest[2:], " "))
}

// configArgs are the --config and --log flags fs was given, to pass on.
func configArgs(fs *flag.FlagSet) []string {
	var out []string
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" || f.Name == "log" {
			out = append(out, "--"+f.Name, f.Value.String())
		}
	})
	return out
}

// consolePostCmd is agentd console post and agentd console inbox (#42),
// which write to the log of the configuration given. Their author and
// reader is --from, by default the tmux session this runs in, else cli.
func consolePostCmd(verb string, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("console "+verb, flag.ContinueOnError)
	from := fs.String("from", "", "who posts or reads (default: this tmux session's name, else cli)")
	var kind, to *string
	if verb == "post" {
		kind = fs.String("kind", "info", "info, decision, delivered, ci or finished")
		to = fs.String("to", "", "also type the text into this tmux session, by its exact name")
	}
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return err
	}
	if *from == "" {
		if *from = tmuxSessionName(); *from == "" {
			*from = "cli"
		}
	}
	rest := fs.Args()
	if verb == "inbox" && len(rest) != 1 || verb == "post" && len(rest) < 2 {
		return fmt.Errorf("usage: agentd console post [--kind KIND] [--from NAME] [--to SESSION] CONSOLE TEXT...|- | agentd console inbox [--from NAME] CONSOLE")
	}
	log, events, err := openLoaded(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	if verb == "inbox" {
		_, err := ConsoleInbox(cfg, log, events, time.Now(), rest[0], *from, stdout)
		return err
	}
	text := strings.Join(rest[1:], " ")
	if len(rest) == 2 && rest[1] == "-" {
		b, err := io.ReadAll(io.LimitReader(stdin, 1<<20+1))
		if err != nil {
			return err
		}
		if len(b) > 1<<20 {
			return fmt.Errorf("a post of over 1 MiB on standard input; at most %d bytes", MessageLimit)
		}
		text = string(b)
	}
	return PostMessage(cfg, log, ExecTmux{Timeout: cfg.Timeouts.Tmux.Duration}, time.Now(), MessagePost{Console: rest[0], Kind: *kind, From: *from, Text: text, To: *to})
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
	log, events, err := openLoaded(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	return ClearSession(projected(cfg, events), log, time.Now(), rest[0])
}
