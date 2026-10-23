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

`run` reads `~/.config/agentd-kit/agentd.json` (`--config` for another file), replays its event log, then passes over every configured session each `poll_interval`. `--shadow` observes everything and acts on nothing: each action is logged as a `would` event instead. `--once` makes one pass. `status` prints one row per session from the event log. `config` prints the resolved configuration. `mark-delivered SESSION WORKTREE REF` records a delivery made before `agentd` took over, so it is never repeated.

`run` is meant to live in a tmux session of its own, under `nice -n 19`. Only one `run` may use an event log: it holds an exclusive lock on the log's path, with symlinks resolved, plus `.lock` (by default `~/.local/state/agentd-kit/events.jsonl.lock`), and a second `run` exits with an error naming that file.

## Configuration

The configuration is JSON, not the YAML the issue sketched: `agentd` is standard library first, and Go has no YAML parser in it. Features live in this repository; the owner's specifics live outside it, in the config file. [`agentd.example.json`](agentd.example.json) shows every field; unset fields take the defaults in `config.go`, which are the Python supervisor's numbers. Relative paths in the file are relative to the file's own directory, not to where `agentd` runs; `~` expands to the home directory, and `~user` is refused. Every session's `workdir` and `cwd` must exist when the configuration is loaded, so a path that resolved wrongly fails at start, naming the session. An escalation command given as a bare name is found on `PATH`. Paths given on the command line (`--config`, `--log`, the `mark-delivered` worktree) are relative to the working directory.

- `timeouts`: how long one external command may run: `tmux` (default `10s`) for each tmux call, `forge` (default `2m`) for each git, glab and gh call, `escalate` (default `2m`) for the escalation command. Each command runs in a process group of its own, and one still running at its timeout is killed with everything it started; its action fails with an `error` event and the pass goes on. git runs with `GIT_TERMINAL_PROMPT=0`, `GCM_INTERACTIVE=never` and empty `GIT_ASKPASS` and `SSH_ASKPASS`, so it fails rather than waiting for credentials.
- `confidential_patterns`: regular expressions, in Go's [RE2 syntax](https://pkg.go.dev/regexp/syntax) and case-sensitive unless they start with `(?i)`, for text `agentd` must never post. An escalation whose text matches one, including Questions copied from `STATUS.md`, is replaced by a generic message that names the session and says the details were withheld; a `skip` event with `filtered` records it, without the matched text, and the event log never holds the original. A `STATUS.md` first line that matches is logged as withheld. A merge request whose title or text matches is not delivered: one skip and one escalation say so, and delivery waits until the text or the patterns change.
- `repositories`: each forge project `agentd` may write to, with its `forge` (`gitlab` or `github`), `project` path, `allow` list (`push`, `open`; there is no `merge`), the `console` escalations go to, and the `assignee` for opened requests.
- `sessions`: each tmux session, its `workdir` (holding `BRIEF.md` and `STATUS.md`), its `cwd`, its `repository`, optional `deliver` specs, and an optional `hold` reason that stops delivery.
- `policies.restart`: restart a gone session with `command` (by default `claude --continue` under a login shell, which keeps Claude Code's permission prompts; add `--dangerously-skip-permissions` to your own `command` only if you choose to); exponential backoff from `backoff_initial` to `backoff_max`; after `max_restarts` within `window` the breaker trips, restarts stop and one escalation is posted. The breaker clears when the session is next seen alive. The command ends with Claude itself, so the tmux session ends when Claude exits and the policy sees it; a crashed session's last screen is not kept, and the event log is the record of what happened.
- `policies.nudge`: type `message` into a session whose screen has not changed for `stall`, at most `max` times per idle episode, then escalate once. A session with open Questions is never nudged.
- `policies.usage_limit`: the screen pattern that means wait; when it clears, `resume_message` is typed once.
- `policies.trust_prompt`: the pattern and keys that answer the folder-trust prompt.
- `policies.escalate`: the command run once per escalation key, with `{console}` and `{message}` substituted.

## States and evidence

A session is `starting`, `working`, `idle`, `blocked-on-owner`, `usage-limited`, `done`, `failed` or `crash-looping`. Every transition comes from evidence and names it in the log:

- whether the tmux session exists, by exact name;
- the hash of the last 40 screen lines and how long since it changed, with a nudge's own echo not counted as change;
- `STATUS.md`: its first line, whether its last line is `DONE`, and whether its Questions section asks anything; a line inside a ```` ``` ```` or `~~~` fenced code block is content, never a heading, so a `# comment` in a snippet neither ends nor starts a section (a fence still open at the end of the file is taken as a typo, and the file is read without fences);
- the transcript's last turn, role and stop reason, read from the tail of the newest transcript for the session's directory when one exists. A fresh transcript keeps a static screen from counting as idle, as during a long tool call. Sessions sharing a directory share transcripts, so this is supporting evidence only.

## Event log

One JSONL file, append-only, one line per observation and action: `time`, `session`, `kind` and `details`. Kinds are `start`, `stop`, `observe` (logged when the evidence changes), `transition`, `action`, `would`, `skip`, `error` and `warning`. The file is created readable by its owner only (`0600`). Each append holds an exclusive `flock` on the file, so `mark-delivered` never interleaves with `run`, and is synced to disk before the action counts as recorded; an append whose write or sync fails is cut back out of the file, so it never leaves a fragment. At startup a malformed line, or one without a `kind`, is corruption: `run` refuses to start and names the line. Only a malformed last line that lacks its newline is a write cut short; `run`, `mark-delivered` and every append cut it off first and log a `warning` with its length, so a new record never lands glued to the fragment. An `observe` event carries `unchanged_s` only once the screen has been seen; until then the time is unknown and the field is absent. The log is the only state: `run` rebuilds its counters, posted escalations and deliveries from it, and `status` is derived from it. In shadow mode `would` events count toward the shadow's own counters, so the breaker and the once-per-key rules show in the log as they would have run.

## Safety

- Process control is by exact session name only. The tmux surface has no listing, no pattern and no kill.
- Every forge write goes through one client wrapped with the per-repository allow-list. It can push and open; there is no merge call to authorize.
- No escalation or merge-request text that matches a `confidential_patterns` entry is posted, and none is logged.
- Escalations and deliveries are keyed and posted once, across restarts of `agentd`. Before pushing, the forge is asked for an existing request on the branch: an open one, or a merged one whose head commit is the worktree's current HEAD, or whose head the target already contains while HEAD has nothing the target lacks (the forge rebased it before merging). One found is recorded as the delivery, with the HEAD it covers, so a lost reply or a lost log never opens a duplicate. Commits count as ahead of the target only when the target lacks their changes, so a branch rebased onto a squash merge has nothing to deliver; a merged request for older commits does not count, so new commits on a reused branch name are delivered. `mark-delivered` accepts only a worktree named in the session's deliver specs.
- A worktree on the deliver target itself, or on a detached HEAD, is never pushed; one skip says why, and it is delivered once it moves to a branch.
- A failed action is not retried every pass: retries follow the restart backoff and stop after `max_restarts` tries, with one skip or escalation saying so.
- Every external command runs under a timeout, and when it hangs its whole process group is killed; a hung tmux is an error, never taken for a gone session.
- Shadow mode performs no tmux write, no fetch, push or open, and runs no escalation command.

## Tests

```bash
./bin/check
```

`bin/check` runs `gofmt -l`, `go vet` and `go test -race`, and fails when Go is missing rather than passing without it. CI runs the same script on every push.

The tests run the supervisor against a fake tmux, forge, git and clock over simulated hours. Each failure the old tools showed is a test: a crash loop stops after N restarts and escalates; a session blocked on the owner is not nudged; an escalation is posted once; nothing is ever killed; shadow mode never acts.
