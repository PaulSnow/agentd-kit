# The laptop node and the hub

agentd on the owner's laptop is a node that only observes (W8510403e, on
#29). It reports, for each repository the build host's config has it watch,
the clone's branch, head, distance from its upstream, the paths it holds
changes on (staged, unstaged, untracked but not ignored), the paths its
unpushed commits change, and its local branches origin lacks with the
paths each changes against the target; and the Claude Code sessions
running on the laptop, with the clone and branch each runs in. Paths and
branch names only, never a file's contents. The hub screens every report
with the confidential patterns before it logs it.

The coord-live supervisor compares that work with its own on every pass
and before it starts a session, a fix round or a go'd item. An overlap is
a card on the repository's page, counted on the home page, which you
answer hold, go on, or "the laptop work is the item". Until you answer go
on, agentd starts nothing on what the laptop holds.

The node asks; the hub never connects to the laptop. A laptop that is off
or asleep shows "laptop: last seen <time>".

## On the build host

1. Make one token per host, each the owner's alone:

       umask 077
       head -c 32 /dev/urandom | base64 > ~/.config/agentd-kit/hub-buildhost.token
       head -c 32 /dev/urandom | base64 > ~/.config/agentd-kit/hub-laptop.token

2. In `~/.config/agentd-kit/coord-live.json`, add the hub on the tailnet
   IP, the hosts, and the laptop to each repository it should watch:

       "hub": {"address": "HUB_ADDRESS:8737", "silent_after": "10m", "retry_after": "30m"},
       "hosts": {
         "buildhost": {"token_file": "~/.config/agentd-kit/hub-buildhost.token"},
         "laptop": {"token_file": "~/.config/agentd-kit/hub-laptop.token", "observe": true}
       },
       "repositories": {
         "widgets": {..., "clone_on": ["laptop"]},
         "agentd-kit": {..., "clone_on": ["laptop"]}
       }

   `HUB_ADDRESS` stands for the build host's tailnet address (as
   `tailscale ip -4` prints it there).

   `observe: true` means the hub sends the laptop no clone, session or
   deploy command, and no repository may place its sessions there.

3. Start the hub beside the supervisor, from the same config, so both
   use one event log: `deploy/systemd/agentd-hub.service`.

## On the laptop

1. Copy the laptop's token from the build host to
   `~/.config/agentd/hub.token`, mode 0600.

2. Write `~/.config/agentd/node.json`:

       {
         "node": {
           "name": "laptop",
           "hub": "http://HUB_ADDRESS:8737",
           "token_file": "~/.config/agentd/hub.token",
           "repos_root": "~/repos",
           "more_roots": ["~/go/src"],
           "every": "1m",
           "observe": true
         }
       }

   `HUB_ADDRESS` is the build host's tailnet address, as above.
   Clones are found at `<root>/<forge host>/<project>`, as
   `~/repos/gitlab.example.com/example-group/widgets`.

3. Install agentd and the unit, then start it:

       go install github.com/PaulSnow/agentd-kit@<the umbrella's commit>
       mv ~/go/bin/agentd-kit ~/go/bin/agentd
       mkdir -p ~/.config/systemd/user
       cp deploy/systemd/agentd-node.service ~/.config/systemd/user/
       systemctl --user daemon-reload
       systemctl --user enable --now agentd-node
       loginctl enable-linger "$USER"

4. On the build host, `agentd nodes -config ~/.config/agentd-kit/coord-live.json`
   shows the laptop reporting within one `every`.

## Documents and prompts (#95)

To let the build host send the laptop documents and run prompts there,
add `"errands": true` to the laptop's `node.json` and restart its node:

       systemctl --user restart agentd-node

`observe` stays: the laptop still takes no clone, session or deploy
command. Then, on the build host:

       agentd send laptop report.pdf
       agentd send -open laptop report.pdf
       agentd prompt laptop "What is in ~/Downloads?"

The first saves the file in the laptop's `~/agentd-inbox`, the second
opens it there as well. A prompt runs `claude -p` on the laptop with its
permission checks skipped, so it may do anything there, under the
laptop's Claude Code login. On the laptop,
`agentd send -config ~/.config/agentd/node.json hub notes.md` sends a
file to the build host's `~/agentd-inbox`. The unit needs the desktop
session's environment (`DISPLAY`, `DBUS_SESSION_BUS_ADDRESS`) for
`-open` and `-notify`; a desktop login imports it into the user's
systemd manager.
