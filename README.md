# agentd-kit

Version 1.0. `agentd` started as phase 1 of a supervisor inside [agentic-kit](https://github.com/PaulSnow/agentic-kit), a fork of Wolf McNally's agentic-coding-starter, and moved here to be its own product: a service for the whole machine that supervises sessions across many repositories, which does not fit a template that governs one repository. `agentd version` prints the version.

## agentd

`agentd` keeps unattended Claude Code sessions going and delivers their work. The deterministic management, watching, restarting, nudging, delivering and routing, runs in this program and runs the same way every time. A model is called in only for judgment, through escalation. It replaces the Python session supervisor in phases; this is phase 1: the event log, the session state machine and the supervisor's policies.

## Running

`agentd` needs Go 1.21 or newer and nothing else.

```bash
go build -o ~/.local/bin/agentd .
```

```bash
agentd run --shadow
```

```bash
agentd run
```

```bash
agentd status
```

`run` reads `~/.config/agentd-kit/agentd.json` (`--config` for another file), replays its event log, then passes over every configured session each `poll_interval`. `--shadow` observes everything and acts on nothing: each action is logged as a `would` event instead (a fetch, which writes nothing to the remote, is the one exception). `--once` makes one pass. `status` prints one row per session from the event log. `config` prints the resolved configuration. `mark-delivered SESSION WORKTREE REF` records a delivery made before `agentd` took over, so it is never repeated. `clear SESSION` lifts a crash-looping session's park.

`run` is meant to live in a tmux session of its own, under `nice -n 19`. Only one `run` may use an event log: it holds an exclusive lock on the log's path, with symlinks resolved, plus `.lock` (by default `~/.local/state/agentd-kit/events.jsonl.lock`), and a second `run` exits with an error naming that file.

## Configuration

The configuration is JSON, not the YAML the issue sketched: `agentd` is standard library first, and Go has no YAML parser in it. Features live in this repository; the owner's specifics live outside it, in the config file. [`agentd.example.json`](agentd.example.json) shows every field; unset fields take the defaults in `config.go`, which are the Python supervisor's numbers. Relative paths in the file are relative to the file's own directory, not to where `agentd` runs; `~` expands to the home directory, and `~user` is refused. Every session's `workdir` and `cwd` must exist when the configuration is loaded, so a path that resolved wrongly fails at start, naming the session. An escalation command given as a bare name is found on `PATH`. Paths given on the command line (`--config`, `--log`, the `mark-delivered` worktree) are relative to the working directory.

- `timeouts`: how long one external command may run: `tmux` (default `10s`) for each tmux call, `forge` (default `2m`) for each git, glab and gh call, `escalate` (default `2m`) for the escalation command. Each command runs in a process group of its own, and one still running at its timeout is killed with everything it started; its action fails with an `error` event and the pass goes on. git runs with `GIT_TERMINAL_PROMPT=0`, `GCM_INTERACTIVE=never` and empty `GIT_ASKPASS` and `SSH_ASKPASS`, so it fails rather than waiting for credentials.
- `confidential_patterns`: regular expressions, in Go's [RE2 syntax](https://pkg.go.dev/regexp/syntax) and case-sensitive unless they start with `(?i)`, for text `agentd` must never post. An escalation whose text matches one, including Questions copied from `STATUS.md`, is replaced by a generic message that names the session and says the details were withheld; a `skip` event with `filtered` records it, without the matched text, and the event log never holds the original. A `STATUS.md` first line that matches is logged as withheld. A merge request whose title or text matches is not delivered, and neither is a branch whose name, or any commit message in `origin/<target>..HEAD`, matches; these are screened before the branch is named anywhere. One skip and one escalation say so, without the text, and delivery waits until the text or the patterns change.
- `repositories`: each forge project `agentd` may write to, with its `forge` (`gitlab` or `github`), `project` path, `allow` list (`push`, `open`; there is no `merge`), the `console` escalations go to, the `assignee` for opened requests, and the `gate`: the argv `agentd` runs in the worktree before it pushes (a relative command such as `./bin/check` is the worktree's own), bounded by `gate_timeout` (default the forge timeout). A repository that allows `push` without a `gate` is refused at load.
- `accounts`: each Claude login a session may run under, by name, with its `config_dir`, its `token_file`, or both. A restart under an account sets `CLAUDE_CONFIG_DIR`, and for a token account `CLAUDE_CODE_OAUTH_TOKEN`, which the launching shell reads from the token file as the command starts: the token never appears in `agentd`'s memory, the event log, or any process's arguments. A token file that is missing, empty, or readable by group or others is refused; the session is not restarted, one skip and one escalation say why, and `agentd` never falls back to another account. An account's transcripts are read from its `config_dir`'s `projects` directory.
- `sessions`: each tmux session, its `workdir` (holding `BRIEF.md` and `STATUS.md`), its `cwd`, its `account` (one of `accounts`; unset means the default login), its `repository`, optional `deliver` specs, and an optional `hold` reason that stops delivery. A session naming an unknown account is refused at load.
- `policies.restart`: restart a gone session with `command`. By default that is `claude` under a login shell with a prompt telling it to read the session's `BRIEF.md` and `STATUS.md` and continue from `STATUS.md`: a fresh context, never `--continue`. It keeps Claude Code's permission prompts; add `--dangerously-skip-permissions` to your own `command` only if you choose to. Restarts back off exponentially from `backoff_initial` to `backoff_max`, counted within `window`. A session gone while its `STATUS.md` has open Questions is `blocked-on-owner` and never restarted. The command ends with Claude itself, so the pane dies when Claude exits; `agentd` starts sessions with tmux's `remain-on-exit`, so a dead pane keeps its exit status (`#{pane_dead_status}`) and last screen, and is restarted with `respawn-pane`, which tmux refuses for a live pane.
- Crash loops park. Each disappearance of a session `agentd` saw alive or restarted is a `crash` event with its cause: the exit status when tmux kept it, and the hash of the last screen. A restart that fails is a crash too, its error the cause. A crash whose cause repeats an earlier one, or the `max_crashes`-th crash (default 3) however far apart, parks the session as `crash-looping` with one escalation. A parked session is never restarted, nudged or typed into, and being seen alive again does not clear it: only `agentd clear SESSION`, which writes a `clear` event that a running `agentd` reads at its next pass.
- `policies.nudge`: type `message` into a session whose screen has not changed for `stall`, at most `max` times per idle episode, then escalate once. A session with open Questions is never nudged.
- `policies.usage_limit`: the screen pattern that means wait; when it clears, `resume_message` is typed once, and only into a session that can take it: never one that is DONE, has open Questions, or shows a dialog.
- `policies.trust_prompt`: the pattern and keys that answer the folder-trust prompt. The keys are sent at most once per screen hash, only when the prompt is on the visible screen, and never to a session with open Questions.
- `policies.dialog`: `patterns`, regular expressions for a Claude Code permission or confirmation dialog on the visible screen (by default `Do you want to`, a `❯ 1.` option menu, and `Enter to confirm`). A session showing one is `blocked-on-owner`, the reason names the pattern, one escalation is posted per screen, and nothing is typed or sent to it. A session whose screen has not changed for `stall` while its transcript's last turn is an assistant tool call is treated the same way: it is most likely waiting at a permission prompt.
- `policies.pane_commands`: the values of tmux's `#{pane_current_command}` that mean the pane runs Claude Code (default `claude`, what a native install reports; an npm install reports `node`). Before typing a line or sending keys, `agentd` asks tmux what the pane runs, and skips with one `skip` event naming the command when it is anything else, such as the shell left after Claude exited.
- `policies.escalate`: the command run once per escalation key, with `{console}` and `{message}` substituted.

A session's Questions section has one open escalation at a time. It opens when Questions become non-empty and the escalation is posted once. While they stay open, each change to their text posts one short `update` to the same console that names the session, says the questions changed and gives the new text (filtered like an escalation), at most one per change and one per pass; rewording never posts a new escalation. When the section empties, or the session is DONE, the escalation closes, and a later non-empty Questions section posts a new one. The episode number and the text last posted are rebuilt from the log, so a restart re-posts nothing and a change made while `agentd` was down is one update.

## States and evidence

A session is `starting`, `working`, `idle`, `blocked-on-owner`, `usage-limited`, `done`, `failed` or `crash-looping`. Every transition comes from evidence and names it in the log:

- whether the tmux session exists, by exact name, and whether its pane is dead, with its exit status;
- the hash of the visible screen plus the 40 lines of history above it, and how long since it changed, with a nudge's own echo not counted as change;
- the patterns (usage limit, trust prompt, dialogs) matched against the visible screen only, so a prompt that has scrolled away is never answered;
- what tmux says the pane is running, asked before anything is typed;
- `STATUS.md`: its first line, whether its last line is `DONE`, and whether its Questions section asks anything; a line inside a ```` ``` ```` or `~~~` fenced code block is content, never a heading, so a `# comment` in a snippet neither ends nor starts a section (a fence still open at the end of the file is taken as a typo, and the file is read without fences);
- the transcript's last turn, role and stop reason, read from the tail of the newest transcript for the session's directory when one exists. A fresh transcript keeps a static screen from counting as idle, as during a long tool call. Sessions sharing a directory share transcripts, so this is supporting evidence only.

## Event log

One JSONL file, append-only, one line per observation and action: `time`, `session`, `kind` and `details`. Every event's details carry the `mode` of the run that wrote it, `live` or `shadow`; `mark-delivered` and `clear` write `owner`, and both modes read those. Kinds are `start`, `stop`, `observe` (logged when the evidence changes), `transition`, `crash`, `clear`, `intent`, `action`, `would`, `skip`, `error` and `warning`. Every action is recorded before it is taken, as an `intent`, and its outcome after, as an `action` or `error`. When any append fails, the pass stops: the action whose intent could not be written is not taken, nothing more is done until the next pass, and `run --once` exits with the error. An `intent` with no outcome after it marks an action cut short by a crash of `agentd`; replay does not count it. The file is created readable by its owner only (`0600`). Each append holds an exclusive `flock` on the file, so `mark-delivered` never interleaves with `run`, and is synced to disk before the action counts as recorded; an append whose write or sync fails is cut back out of the file, so it never leaves a fragment. At startup a malformed line, or one without a `kind`, is corruption: `run` refuses to start and names the line. Only a malformed last line that lacks its newline is a write cut short; `run`, `mark-delivered` and every append cut it off first and log a `warning` with its length, so a new record never lands glued to the fragment. An `observe` event carries `unchanged_s` only once the screen has been seen; until then the time is unknown and the field is absent. The log is the only state: `run` rebuilds its counters, posted escalations and deliveries from it, and `status` is derived from it. Live and shadow never share state: a run replays only the events of its own mode, plus the owner's records, so a shadow run's `would` events never use up a live run's restarts, escalations or deliveries, and a live run's actions never shape the shadow's decisions. In shadow mode `would` events count toward the shadow's own counters, so the crash parking and the once-per-key rules show in the log as they would have run.

## Safety

- Process control is by exact session name only. The tmux surface has no listing, no pattern and no kill.
- Nothing is typed or sent as keys into a session that is DONE, has open Questions, is blocked on the owner or shows a dialog, or whose pane is not running Claude.
- Every forge write goes through one client wrapped with the per-repository allow-list. It can push and open; there is no merge call to authorize.
- No escalation, merge-request text, branch name or commit message that matches a `confidential_patterns` entry is posted or pushed, and none is logged.
- Escalations, Questions updates and deliveries are keyed and posted once, across restarts of `agentd`. Before pushing, the forge is asked for an existing request on the branch: an open one, or a merged one whose head commit is the worktree's current HEAD, or whose head the target already contains while HEAD has nothing the target lacks (the forge rebased it before merging). One found is recorded as the delivery, with the HEAD it covers, so a lost reply or a lost log never opens a duplicate. Commits count as ahead of the target only when the target lacks their changes, so a branch rebased onto a squash merge has nothing to deliver; a merged request for older commits does not count, so new commits on a reused branch name are delivered. `mark-delivered` accepts only a worktree named in the session's deliver specs.
- Delivery pushes only to the configured project: before anything else, the worktree's `origin` URL (https, ssh or `user@host:path`, credentials never logged) must name the repository's `project`; otherwise one skip says why and nothing is pushed until it does. The push names the exact commit that was checked, `git push origin <sha>:refs/heads/<branch>`, never whatever HEAD has become. Requests from forks are left out when looking for an existing request.
- Nothing is delivered without a gate `agentd` ran itself. Delivery runs the repository's `gate` on the worktree's exact HEAD and records a `gate` event with the command, the commit, the exit status and the duration. It pushes only when the gate exits 0 and the tree is still the one it ran on: HEAD unmoved and the worktree clean. A failing gate is a failed delivery step, retried on the backoff schedule and given up with one escalation; a tree that changed under the gate is skipped, and the next pass gates the new tree. Shadow mode logs the gate it `would` run and runs none.
- A worktree on the deliver target itself, or on a detached HEAD, is never pushed; one skip says why, and it is delivered once it moves to a branch.
- A failed action is not retried every pass: retries follow the restart backoff and stop after `max_restarts` tries, with one skip or escalation saying so.
- Every external command runs under a timeout, and when it hangs its whole process group is killed; a hung tmux is an error, never taken for a gone session.
- Shadow mode performs no tmux write, no push or open, and runs no escalation command; escalations and Questions updates are logged as `would` events. It does fetch, which changes only the worktree's remote-tracking refs and nothing on the remote, so its delivery decisions use current refs.

## Tests

```bash
./bin/check
```

`bin/check` runs `gofmt -l`, `go vet` and `go test -race`, and fails when Go is missing rather than passing without it. CI runs the same script on every push.

The tests run the supervisor against a fake tmux, forge, git and clock over simulated hours. Each failure the old tools showed is a test: a crash loop parks for the owner and escalates once; a session blocked on the owner is not nudged; an escalation is posted once, and reworded Questions are updates to it; nothing is ever killed; shadow mode never acts.
