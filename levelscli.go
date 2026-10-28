package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// The CLI of the owner's levels (#69): the repository's and the issue's
// pages in the same sentences, and the console box's scoped messages.
// They read the log through the same source the UI serves from, so what
// they print and record is what the pages show and post.

// consoleSubcommands are the words agentd console takes before a
// repository; a repository named like one is refused, so neither is
// taken for the other.
var consoleSubcommands = []string{"reply", "post", "inbox", "import", "ask", "message"}

// cliSource is a UI source over the configuration's log, as the CLI
// reads and writes it.
func cliSource(cfg *Config, log *Log) *uiSource {
	src := newUISource("cli", cfg, log, ModeLive, "")
	return src
}

// levelCmd is agentd console REPO [--issues [--sort S] [--filter W]]
// [--history]: the repository's page, its issue box, or its steps, newest
// last.
func levelCmd(cfg *Config, repo string, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("console "+repo, flag.ContinueOnError)
	issues := fs.Bool("issues", false, "print the issue box")
	sortBy := fs.String("sort", IssueSorts[0], "the issue box's sort: "+strings.Join(IssueSorts, ", "))
	filter := fs.String("filter", "", "keep the issues holding these words, a label, #N or !N")
	history := fs.Bool("history", false, "print every step of the repository's issues and requests, oldest first")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("console %s takes no more arguments, got %q", repo, fs.Args())
	}
	if !contains(IssueSorts, *sortBy) {
		return fmt.Errorf("--sort is one of %s", strings.Join(IssueSorts, ", "))
	}
	if _, ok := cfg.Repositories[repo]; !ok {
		return fmt.Errorf("no repository %q", repo)
	}
	log, err := openView(cfg)
	if err != nil {
		return err
	}
	defer log.Close()
	src := cliSource(cfg, log)
	now := time.Now()
	c, rep, err := src.view(now)
	if err != nil {
		return err
	}
	v := BuildRepoLevel(c, src.st, repo, DecisionsFrom(c, src.st, rep), now)
	p := uiPage{Now: now, loc: time.Local}
	switch {
	case *history:
		printHistory(out, p, src.st, repo)
	case *issues:
		printIssueBox(out, p, v, *sortBy, *filter)
	default:
		fmt.Fprintln(out, v.Sentence)
		fmt.Fprintln(out)
		printIssueBox(out, p, v, *sortBy, *filter)
		fmt.Fprintln(out)
		printRequestBox(out, p, v.Requests)
		if talk := screenTalk(c, src.st.scopeConversation(repo, scopeShown)); len(talk) > 0 {
			fmt.Fprintln(out)
			printTalk(out, p, talk)
		}
	}
	return nil
}

// printIssueBox prints the issue box: its count, then a row and a
// sentence per issue, sorted and filtered as the page's script does.
func printIssueBox(w io.Writer, p uiPage, v RepoLevel, by, filter string) {
	is := append([]LevelIssue(nil), v.Issues...)
	sortIssues(is, by)
	shown := filterIssues(is, filter)
	fmt.Fprintf(w, "%d issues, %d need you, %d being worked, %d shown\n", v.Counts.Open, v.Counts.Waiting, v.Counts.Working, len(shown))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, i := range shown {
		mr := strings.Join(i.Requests, " ")
		if mr == "" {
			mr = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\tchanged %s\n", i.Ref, i.Word, oneLine(i.Title, 60), mr, p.Ago(i.Changed))
		if i.Sentence != "" {
			fmt.Fprintf(tw, "\t%s (%s)\n", i.Sentence, i.Writer)
		} else {
			fmt.Fprintf(tw, "\tNo sentence yet; the forge's title stands.\n")
		}
		if d := i.Decision; d != nil {
			var opts []string
			for _, o := range d.Options {
				opts = append(opts, o.Key)
			}
			fmt.Fprintf(tw, "\tAsks you (%s): %s [%s]\n", d.Entry, d.Asked, strings.Join(opts, ", "))
		}
	}
	tw.Flush()
}

// printRequestBox prints the MR box, in the loop's order.
func printRequestBox(w io.Writer, p uiPage, rs []LevelRequest) {
	fmt.Fprintf(w, "%d open requests\n", len(rs))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range rs {
		ci, issue := r.CI, r.Issue
		if ci == "" {
			ci = "not watched"
		}
		if issue == "" {
			issue = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\tfor %s\tCI %s\tchanged %s\n", r.Ref, oneLine(r.Title, 60), issue, ci, p.Ago(r.Changed))
		if r.Place != "" {
			fmt.Fprintf(tw, "\t%s\n", r.Place)
		}
	}
	tw.Flush()
}

// openView opens the configuration's log as the UI does, for a view: one
// that does not exist is an error, never a new empty log.
func openView(cfg *Config) (*Log, error) {
	if _, err := os.Stat(cfg.EventLog); err != nil {
		return nil, fmt.Errorf("the event log: %w", err)
	}
	return OpenLog(cfg.EventLog)
}

// screenTalk screens a conversation as the page's console box does.
func screenTalk(cfg *Config, talk []ScopeEntry) []ScopeEntry {
	secret := confidentialMatcher(cfg)
	for k := range talk {
		if secret(talk[k].Text) {
			talk[k].Text = withheldMessage
		}
		if secret(talk[k].Answer) {
			talk[k].Answer = withheldMessage
		}
	}
	return talk
}

// textFlags refuses a flag of fs written among the text, which flag
// parsing would take as words.
func textFlags(fs *flag.FlagSet) error {
	for _, a := range fs.Args() {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && fs.Lookup(name) != nil {
			return fmt.Errorf("%s comes after the text: put the flags first", a)
		}
	}
	return nil
}

// flagsFirst moves the flags of args before the positional arguments, so
// a flag after them is read as one; --config and --log take the argument
// after them unless written with =.
func flagsFirst(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			rest = append(rest, a)
			continue
		}
		flags = append(flags, a)
		if name := strings.TrimLeft(a, "-"); (name == "config" || name == "log") && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, rest...)
}

// printTalk prints a scope's conversation, newest last.
func printTalk(w io.Writer, p uiPage, talk []ScopeEntry) {
	fmt.Fprintln(w, "Console")
	for _, e := range talk {
		fmt.Fprintf(w, "  %s  you: %s\n", p.When(e.At), oneLine(e.Text, 200))
		if e.Answer != "" {
			fmt.Fprintf(w, "    %s\n", oneLine(e.Answer, 400))
		}
		fmt.Fprintf(w, "    (%s)\n", e.State)
	}
}

// printHistory prints every step the fold keeps of repo's issues and
// requests, oldest first.
func printHistory(w io.Writer, p uiPage, st *LogState, repo string) {
	var steps []IssueStep
	for k, s := range st.Steps {
		if strings.HasPrefix(k, repo+" ") {
			steps = append(steps, s...)
		}
	}
	sort.SliceStable(steps, func(a, b int) bool {
		if !steps[a].At.Equal(steps[b].At) {
			return steps[a].At.Before(steps[b].At)
		}
		return steps[a].Ref < steps[b].Ref
	})
	secret := confidentialMatcher(st.cfg)
	for _, s := range steps {
		text := s.Text
		if secret(text) {
			text = "(withheld: it matched a confidential pattern)"
		}
		fmt.Fprintf(w, "%s  %s  %s\n", p.When(s.At), s.Ref, text)
	}
}

// issueCmd is agentd issue REPO N [--all]: the issue's page.
func issueCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("issue", flag.ContinueOnError)
	all := fs.Bool("all", false, "every step, not the latest only")
	cfg, _, err := loadConfigFlag(fs, flagsFirst(args))
	if err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return errors.New("usage: agentd issue [--all] REPO N")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(rest[1], "#"))
	if err != nil || n <= 0 {
		return fmt.Errorf("%q is not an issue's number", rest[1])
	}
	if _, ok := cfg.Repositories[rest[0]]; !ok {
		return fmt.Errorf("no repository %q", rest[0])
	}
	log, err := openView(cfg)
	if err != nil {
		return err
	}
	defer log.Close()
	src := cliSource(cfg, log)
	now := time.Now()
	c, rep, err := src.view(now)
	if err != nil {
		return err
	}
	v := BuildIssueLevel(c, src.st, rest[0], n, DecisionsFrom(c, src.st, rep), *all, now)
	if !v.Known {
		return fmt.Errorf("agentd knows nothing of %s#%d", rest[0], n)
	}
	printIssueLevel(out, uiPage{Now: now, loc: time.Local}, v)
	if talk := screenTalk(c, src.st.scopeConversation(Scope{Repository: rest[0], Issue: n}.String(), scopeShown)); len(talk) > 0 {
		fmt.Fprintln(out)
		printTalk(out, uiPage{Now: now, loc: time.Local}, talk)
	}
	return nil
}

// printIssueLevel prints an issue's page.
func printIssueLevel(w io.Writer, p uiPage, v IssueLevel) {
	fmt.Fprintf(w, "%s %s (%s)\n", v.Ref, v.Title, v.Word)
	if v.Sentence != "" {
		fmt.Fprintf(w, "%s (%s, %s ago)\n", v.Sentence, v.Writer, p.Ago(v.SentenceAt))
	} else {
		fmt.Fprintln(w, "No sentence yet; the forge's title stands.")
	}
	var refs []string
	for _, r := range v.Refs {
		s := r.Kind + " " + r.Name
		if r.Note != "" {
			s += " (" + r.Note + ")"
		}
		refs = append(refs, s)
	}
	if len(refs) > 0 {
		fmt.Fprintln(w, "Refs: "+strings.Join(refs, ", "))
	}
	fmt.Fprintln(w)
	if len(v.Decisions) == 0 {
		fmt.Fprintln(w, "Nothing on this issue waits on you.")
	}
	for _, d := range v.Decisions {
		fmt.Fprintf(w, "Asks you (%s): %s\n", d.Entry, d.Asked)
		if d.CameBack != "" {
			fmt.Fprintf(w, "  What came back: %s\n", d.CameBack)
		}
		if d.Checked != "" {
			fmt.Fprintf(w, "  What Go checked: %s\n", d.Checked)
		}
		for _, o := range d.Options {
			fmt.Fprintf(w, "  %s: %s\n", o.Key, o.Consequence)
		}
		fmt.Fprintf(w, "  Answer: agentd console reply --entry %s --choice KEY [WORDS]\n", d.Entry)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "What has been done")
	for _, s := range v.Steps {
		ref := ""
		if s.Ref != v.Ref {
			ref = s.Ref + " "
		}
		fmt.Fprintf(w, "  %s  %s%s\n", p.When(s.At), ref, s.Text)
	}
	if len(v.Steps) == 0 {
		fmt.Fprintln(w, "  Nothing yet.")
	}
	if v.More > 0 {
		fmt.Fprintf(w, "  (%d earlier: --all prints every entry)\n", v.More)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Next")
	for _, n := range v.Next {
		fmt.Fprintln(w, "  "+n)
	}
}

// consoleAskCmd is agentd console ask SCOPE TEXT...: the console box's
// message at a level, recorded as the page records it.
func consoleAskCmd(cfg *Config, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("console ask", flag.ContinueOnError)
	now := fs.Bool("now", false, "at an issue: type it into its session at once, not at its next turn")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := textFlags(fs); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return errors.New("usage: agentd console ask [--now] home|REPO|REPO#N TEXT...")
	}
	c, err := ParseScope(cfg, fs.Arg(0))
	if err != nil {
		return err
	}
	log, _, err := openLoaded(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	what, err := cliSource(cfg, log).scopeReply(c, strings.Join(fs.Args()[1:], " "), *now, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "recorded at %s, %s\n", c, what)
	return nil
}

// consoleEntryCmd is agentd console reply --entry ID [--choice KEY |
// --yes | --no] TEXT...: a response to an entry, as its card's buttons and
// words record it.
func consoleEntryCmd(cfg *Config, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("console reply", flag.ContinueOnError)
	entry := fs.String("entry", "", "the entry's id: b:ITEM, w:ITEM, x:ID, d:SESSION:ID, n:SESSION:KIND or i:ID; any other (m:, f:, e:, p:, o:) with --repository")
	choice := fs.String("choice", "", "the option chosen")
	yes := fs.Bool("yes", false, "answer a yes/no decision yes")
	no := fs.Bool("no", false, "answer a yes/no decision no")
	repo := fs.String("repository", "", "the entry's repository, when Go cannot tell it from the entry")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := textFlags(fs); err != nil {
		return err
	}
	if *entry == "" || *yes && *no || fs.NArg() == 0 && *choice == "" && !*yes && !*no {
		return errors.New("usage: agentd console reply --entry ID [--choice KEY | --yes | --no] [--repository REPO] TEXT...")
	}
	log, _, err := openLoaded(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	src := cliSource(cfg, log)
	if *repo == "" {
		src.mu.Lock()
		err := src.catchUp()
		*repo = entryRepository(withRuntime(cfg, src.st.Runtime), src.st, *entry)
		src.mu.Unlock()
		if err != nil {
			return err
		}
		if *repo == "" {
			return fmt.Errorf("say the repository of %s with --repository", *entry)
		}
	}
	var a *AnswerPayload
	switch {
	case *choice != "":
		a = &AnswerPayload{Choice: *choice}
	case *yes || *no:
		v := *yes
		a = &AnswerPayload{Answer: &v}
	case strings.HasPrefix(*entry, entryDecision):
		a = &AnswerPayload{}
	}
	if err := src.respond(*repo, *entry, "", strings.Join(fs.Args(), " "), a, nil); err != nil {
		return err
	}
	fmt.Fprintf(out, "recorded the response to %s\n", *entry)
	return nil
}

// entryRepository is the repository an entry is of, as the log says, or
// "".
func entryRepository(cfg *Config, st *LogState, entry string) string {
	switch {
	case strings.HasPrefix(entry, entryWork), strings.HasPrefix(entry, entryDesign):
		if it := st.Work[entry[2:]]; it != nil {
			return it.Repository
		}
	case strings.HasPrefix(entry, entryDecision), strings.HasPrefix(entry, entryNeed):
		session, _, _ := strings.Cut(entry[2:], ":")
		if s, ok := cfg.Sessions[session]; ok {
			return s.Repository
		}
		return st.Ended[session]
	case strings.HasPrefix(entry, entryInstruction), strings.HasPrefix(entry, entryNext):
		if in := st.Instructions[entry[2:]]; in != nil && len(in.Repositories) > 0 {
			return in.Repositories[0]
		}
	}
	return ""
}

// consoleMessageCmd is agentd console message --to SESSION [--now]
// TEXT...: a message to a session. One working an issue gets it as that
// issue's console box sends it, at its next turn unless --now; any other
// as its own box on its page sends it.
func consoleMessageCmd(cfg *Config, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("console message", flag.ContinueOnError)
	to := fs.String("to", "", "the session")
	now := fs.Bool("now", false, "type it at once, not at the session's next turn")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := textFlags(fs); err != nil {
		return err
	}
	if *to == "" || fs.NArg() == 0 {
		return errors.New("usage: agentd console message --to SESSION [--now] TEXT...")
	}
	log, _, err := openLoaded(cfg.EventLog)
	if err != nil {
		return err
	}
	defer log.Close()
	src := cliSource(cfg, log)
	src.mu.Lock()
	defer src.mu.Unlock()
	if err := src.catchUp(); err != nil {
		return err
	}
	c := withRuntime(cfg, src.st.Runtime)
	sess, ok := c.Sessions[*to]
	if !ok {
		return fmt.Errorf("no session %q", *to)
	}
	text := strings.Join(fs.Args(), " ")
	if sess.Issue > 0 {
		if err := ScopeInstruction(c, log, time.Now(), Scope{Repository: sess.Repository, Issue: sess.Issue}, *to, text, *now, nil); err != nil {
			return err
		}
		when := "at its next turn"
		if *now {
			when = "now"
		}
		fmt.Fprintf(out, "recorded for %s, %s#%d, %s\n", *to, sess.Repository, sess.Issue, when)
		return nil
	}
	if *now {
		return fmt.Errorf("%s works no issue, so its message goes as its own box sends it, when it can take a line: leave out --now", *to)
	}
	if err := UIInstruction(c, log, time.Now(), sess.Repository, *to, text, nil); err != nil {
		return err
	}
	fmt.Fprintf(out, "recorded for %s\n", *to)
	return nil
}

// entryFlag says whether args, after reply, hold --entry among the flags
// before the text.
func entryFlag(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return false
		}
		name, _, value := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch {
		case name == "entry":
			return true
		case (name == "choice" || name == "repository") && !value:
			i++ // its value
		}
	}
	return false
}

// levelConsoleCmd takes agentd console's level forms, and says whether
// args was one: REPO, ask, message, and reply --entry.
func levelConsoleCmd(args []string, out io.Writer) (bool, error) {
	fs := flag.NewFlagSet("console", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg, _, err := loadConfigFlag(fs, args)
	if err != nil {
		return false, nil // the console's own forms say what is wrong
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return false, nil
	}
	if _, ok := cfg.Repositories[rest[0]]; ok && contains(consoleSubcommands, rest[0]) {
		return true, fmt.Errorf("repository %q is named like agentd console's %s: rename it in the configuration", rest[0], rest[0])
	}
	switch {
	case rest[0] == "ask":
		return true, consoleAskCmd(cfg, rest[1:], out)
	case rest[0] == "message":
		return true, consoleMessageCmd(cfg, rest[1:], out)
	case rest[0] == "reply" && entryFlag(rest[1:]):
		return true, consoleEntryCmd(cfg, rest[1:], out)
	case !contains(consoleSubcommands, rest[0]):
		if _, ok := cfg.Repositories[rest[0]]; ok {
			return true, levelCmd(cfg, rest[0], rest[1:], out)
		}
	}
	return false, nil
}
