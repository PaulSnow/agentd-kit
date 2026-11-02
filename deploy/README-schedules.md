# Moving outside schedules under agentd

Some agent work may still run from the login's crontab or from a user
systemd unit: nightly builds, a handful of daily scripts, a poller every few
minutes, and a long-running service that schedules its own tasks. Each one
runs unseen, and a failure shows up only in a log file someone has to open.
This document moves such jobs into `jobs.scheduled` (#86) one at a time, and
gives a way back for each. After the move, agentd starts each run, records
its start and end, keeps its output, and posts one card to the default
console when the job fails.

The job names below are generic: `nightly-a`, `nightly-b`, `crons-a` to
`crons-d`, `poller`, and `researcher-<task>`. Commands and paths are
placeholders. The real mapping from these names to the machine's jobs, with
their real commands, is kept outside the repository, next to the live
config. Fill in the placeholders from it, and only in the live config:

| Placeholder | What it is |
|---|---|
| `<repo-dir>` | the clone or directory a job works in (one per job; the cron line's `cd` or the script's directory) |
| `<script>` | the script or program the cron line runs |
| `<service-dir>` | the long-running service's `WorkingDirectory=` |
| `<venv>` | the service's virtualenv, when it has one |
| `<task>` | one of the service's own scheduled tasks |
| `<account>` | the agentd account whose Claude login the job runs under |
| `<jobs-log-dir>` | where run logs are kept, for example `~/.local/state/agentd-kit/jobs` |
| `<lock-dir>` | where the cron line's lock files are (often `/tmp`) |
| `<HH:MM>` | the time the cron line runs at |

## The fields

`at` is `HH:MM` daily in agentd's local time, the same clock cron uses. An
`at` slot that agentd was down for still runs if agentd comes back within the
hour, where cron would skip it. `every` replaces `*/N` lines. `argv` runs
through `sh -c 'exec "$@"'`, with no login shell and no `~/.profile`, the same
as cron. `env` sets what the cron line or `Environment=` set. `timeout` kills
the whole process group. `fail_after` is how many failed runs in a row post a
card (default 3). `output` and `keep` replace the `>> some.log 2>&1`
redirect: one file per run, and the newest `keep` are kept. See README.md,
"Scheduled jobs".

agentd runs a job with its own environment, minus the variables it drops.
It does not use cron's bare one. To keep a script's behavior the same, give
it cron's `PATH` (`/usr/bin:/bin`) in `env`. A secret other than the Claude
token, such as an API key a script uses, stays where it is now: in the
script's own file or wrapper. `agentd config` refuses an env name that
looks like a secret and a value that matches a confidential pattern.

A job that calls `claude` runs under `account`. agentd sets that account's
config directory and passes its token from the token file, so the token
appears on no command line. Without `account` the job uses agentd's own
login, as cron used the crontab owner's.

## The entries

These go in the live config's `jobs.scheduled`. The daily jobs use
`fail_after: 1`, because a daily job that fails once has lost its day. The
poller runs every few minutes and can fail once for reasons outside it, so
it keeps the default of 3. Every entry keeps its output.

```json
"jobs": {"scheduled": {
  "nightly-a": {
    "at": "<HH:MM>", "argv": ["<script>"], "cwd": "<repo-dir>",
    "account": "<account>", "timeout": "4h", "fail_after": 1,
    "env": {"PATH": "/usr/bin:/bin"}, "output": "<jobs-log-dir>", "keep": 14
  },
  "nightly-b": {
    "at": "<HH:MM>", "argv": ["<script>"], "cwd": "<repo-dir>",
    "account": "<account>", "timeout": "4h", "fail_after": 1,
    "env": {"PATH": "/usr/bin:/bin"}, "output": "<jobs-log-dir>", "keep": 14
  },
  "crons-a": {
    "at": "<HH:MM>", "argv": ["<script>"], "cwd": "<repo-dir>",
    "timeout": "1h", "fail_after": 1,
    "env": {"PATH": "/usr/bin:/bin"}, "output": "<jobs-log-dir>", "keep": 14
  },
  "crons-b": {
    "at": "<HH:MM>", "argv": ["<script>"],
    "timeout": "30m", "fail_after": 1,
    "env": {"PATH": "/usr/bin:/bin"}, "output": "<jobs-log-dir>", "keep": 14
  },
  "crons-c": {
    "at": "<HH:MM>", "argv": ["<script>"], "cwd": "<repo-dir>",
    "timeout": "30m", "fail_after": 1,
    "env": {"PATH": "/usr/bin:/bin"}, "output": "<jobs-log-dir>", "keep": 14
  },
  "crons-d": {
    "at": "<HH:MM>", "argv": ["<script>"], "cwd": "<repo-dir>",
    "account": "<account>", "timeout": "2h", "fail_after": 1,
    "env": {"PATH": "/usr/bin:/bin"}, "output": "<jobs-log-dir>", "keep": 14
  },
  "poller": {
    "every": "5m", "argv": ["flock", "-n", "<lock-dir>/poller.lock", "<script>"],
    "cwd": "<repo-dir>", "account": "<account>", "timeout": "1h",
    "env": {"PATH": "/usr/bin:/bin"}, "output": "<jobs-log-dir>", "keep": 288
  },
  "researcher-<task>": {
    "every": "6h", "argv": ["<venv>/bin/python", "-m", "<module>", "<run-one-task>", "<task>"], "cwd": "<service-dir>",
    "account": "<account>", "timeout": "1h",
    "env": {"PATH": "<venv>/bin:/usr/local/bin:/usr/bin:/bin", "PYTHONUNBUFFERED": "1"},
    "output": "<jobs-log-dir>", "keep": 28
  }
}}
```

A script that takes a lock (`flock -n`) keeps taking it. When the lock is
on the cron line itself, as for the poller, the entry carries it in `argv`.
A run that finds the lock held exits at once and does no work twice. agentd
also never starts a job while its previous run is still going: it records a
`skip` instead.

### A long-running service

A service that runs all the time and schedules its own tasks is not a cron
line. It moves only if it has a command that runs one task once and exits.
Each task then becomes its own job, `researcher-<task>`, at that task's
interval, and its environment comes from the unit's `Environment=` lines.
Take the intervals from the service's own config at move time.

What is lost by the move:

- Any schedule logic inside the service (for example, tasks that run only
  on some days) goes. A job runs its task at its `every`. If that logic
  matters, keep that task on the service.
- Anything the service serves while running, such as a dashboard, goes. If
  it is still needed, the service stays as it is until it can serve it on
  its own. Changing the service is out of scope here.
- The unit's `MemoryMax=` and `CPUQuota=` caps go. agentd's timeout still
  kills a task that runs too long.

For these reasons the service moves last.

## Moving one job

Move one job at a time, in the order below. For each:

1. **Copy it.** From `crontab -l` (or `systemctl --user cat <unit>`), take the
   schedule, the command, any `cd` or `WorkingDirectory=`, and any `VAR=` or
   `Environment=`. Fill in the entry's placeholders in the live config. Leave
   the cron line as it is for now.
2. **Check it.** `agentd config --config <live config>` must load without an
   error and print the job's resolved `cwd`, `output` and `keep`.
3. **Shadow it for one slot.** Copy the entry, with its account, into a
   scratch config that holds nothing else, plus an `event_log` of its own.
   Set its `at` to a minute in the last hour. Then:

       agentd run --shadow --once --config <scratch config>
       grep '"phase":"would"' <scratch event log>

   You should see one `would` event for the job and its slot. Shadow mode
   runs nothing.
4. **Take it off cron.** Comment out the cron line (`crontab -e`, put `#` in
   front). For a timer or a service: `systemctl --user disable --now <unit>`.
   Check that it is off:

       crontab -l | grep -v '^#' | grep -c '<script>'     # 0

5. **Turn it on in agentd.** Restart the live agentd: it reads its config
   only when it starts. A restart kills any scheduled run in flight and
   records it as `stopped`, so pick a time when none is running. On the
   owner's page, the job then shows under the accounts with its schedule.
6. **Confirm the first real run.** After the slot, check the following:
   - The live event log has a `start` and an `end` for the job:

         grep '"kind":"job"' <live event log> | grep '"name":"<job>"' | tail -2

   - The end has `"exit":0` and a `log` path.
   - The file at that path holds what the old `>> ….log` used to get.
   - The job's old log file got no new entry for that night.

The next night, check again. For a daily job, the first card comes after
one failed run. Use the card's log path to look.

### Rollback

1. Remove the job's entry from the live config and restart agentd, at a
   time when the job is not running: a restart kills a run in flight.
2. Restore the cron line (remove the `#`), or
   `systemctl --user enable --now <unit>`.
3. Check that it is back: `crontab -l | grep '<script>'` shows it
   uncommented, and the old log gets its next entry.

A rollback does not remove the kept run logs in `<jobs-log-dir>`. Delete
them when they are no longer wanted.

## The order

1. The daily scripts that make no Claude call and take no lock, earliest
   first, so the first move shows within a day.
2. The other daily scripts, one per day, even when two run minutes apart.
3. The first job with an account. It also shows that any other secret the
   script uses still comes from its own file.
4. The long nightlies (`nightly-a`, then `nightly-b`). They keep their locks.
5. The poller. With its lock in `argv`, a run from a cron line left on by
   mistake and a run from agentd cannot overlap.
6. The long-running service's tasks, last, under the conditions above.

## Cron and agentd never both run a job

- A job goes into the live config only after its cron line or unit is off
  (step 4 comes before step 5). Shadow mode in step 3 runs nothing.
- The jobs that lock keep their locks under agentd. A second run started by
  mistake exits at once.
- To check at any time: for each job in `jobs.scheduled`, no line in
  `crontab -l | grep -v '^#'` and no enabled timer or service
  (`systemctl --user list-timers`, `systemctl --user list-units`) names its
  script. Each night, the old log of a moved job gets nothing new, and
  `<jobs-log-dir>` gets one file per job.
