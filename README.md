# meterm

You have a dozen servers and every one of them needs a different IP, a
different user, a different key, and half of them are only reachable through
a bastion. `meterm` remembers all of that so you don't have to — give each
machine a short name once, then just `meterm <name>`.

## Install

**From source** (needs Go):

```sh
go build -o meterm ./cmd/meterm
mv meterm /usr/local/bin/   # or anywhere on your $PATH
```

**Pre-built binary** — every `vX.Y.Z` tag publishes macOS/Linux/Windows
(amd64 + arm64) archives to GitHub Releases via goreleaser
(`.github/workflows/release.yml`). Download one from the
[releases page](https://github.com/bcd91/meterm/releases), or:

```sh
sh skills/meterm/scripts/install.sh   # latest release, checksum checked, into ~/.local/bin
```

**For AI agents** — [`skills/meterm`](skills/meterm/SKILL.md) is a skill that tells
Claude Code when to use meterm, which commands need the user's approval, and how to
install it. Copy that folder to `~/.claude/skills/meterm`.

## Quick start

```sh
# add a bastion, and a group that jumps through it
meterm add bastion --address 203.0.113.9 --user jump --identity ~/.ssh/bastion_key
meterm group add prod --user deploy --identity ~/.ssh/prod_key --proxy-jump bastion

# add servers that inherit the group's user/key/jump host
meterm add web1 --address 10.0.1.10 --group prod --tags web
meterm add db1  --address 10.0.1.20 --group prod --tags db --port 2222

# say which machines are production (or put --env on the group)
meterm edit web1 --env production

# browse and connect
meterm                 # interactive picker: type to fuzzy-filter, enter to connect
meterm connect web1    # or connect directly by name
```

Only `--address` is required — everything else (user, port, identity file,
jump host) falls back from **host → group → global defaults**, so a whole
environment's worth of machines can share one set of credentials and you only
override what's different per host.

## Interactive picker

Running `meterm` with no arguments opens a searchable list of every saved
host, sorted by most-recently-connected, framed in a bordered box with a
legend underneath it. Each row shows a live status dot (● green = port
open, ● red = unreachable, ○ = still checking, ◌ = behind a `proxy_jump`
so a direct check would be meaningless — not dialed) plus a ⚷ once
`copy-id` has installed a key on that host. After the name, `⇄2` means two
other routes, `⚑prod` a production host, `⇶1` one saved tunnel (red when it
reaches production), `⧉ACME`
the team it's shared with (type `acme` to filter to it), `↑` a
team change not pushed yet and `✗` a team conflict:

| Key     | Action                       |
|---------|------------------------------|
| type    | fuzzy-filter the list        |
| enter   | connect (asks which account when the host has `users`) |
| `a`     | add a new host                |
| `e`     | edit the selected host        |
| `f`     | two-pane file manager on the selected host |
| `S`     | CPU/memory/disk/network snapshot of the selected host |
| `i`     | install your public key on the selected host (copy-id) |
| `K`     | generate a new key and set it as the selected host's identity |
| `d`     | delete the selected host (asks to confirm) |
| `r`     | refresh (reload hosts + re-check status) |
| `m`     | connect inside tmux (`connect --tmux`): resume a session or start one |
| `t`     | open one of the host's tunnels (a picker when it has several; production ones are flagged) |
| `space` | mark hosts, then `x` |
| `x`     | exec on the marked hosts (or the selected one): opens the palette with `exec <hosts> -- ` |
| `A`     | audit the selected host |
| `L`     | list the host's `/var/log`, then a palette ready for the file to tail |
| `R`     | check which route to the host answers right now |
| `w`     | who can log in: the host's authorized keys (`keys list`) |
| `T`     | team status |
| `:`     | command palette — see below   |
| `esc`   | drop the marks (when any), otherwise quit |
| `q`     | quit                          |

`?` shows every key. The newer ones are capitals where the lowercase letter
already moves the list (`l`, `k`, `u`, ...).

### Command palette

`:` opens a prompt where you can type **any** meterm subcommand — the picker
re-execs itself with what you type, so nothing on the CLI is out of reach
from inside the TUI:

```
: cp web1:/etc/nginx/nginx.conf ./nginx.conf
: exec --tag web -- systemctl restart nginx
: group list
: import
```

`tab` completes commands, subcommands and host names (`host:` for `cp`,
`host/tunnel` for `tunnel up`). A command that needs a host — `connect`,
`edit`, `files`, `stats`, `copy-id`, `logs`, `logs list`, `tunnel up`, and
`exec` with no hosts before `--` — gets the selected one when you leave it
out, and the line under the prompt always shows exactly what Enter will run.
Commands where the host is optional (`audit`, `keys list`, `route check`)
keep their CLI meaning, every host, and nothing destructive is ever aimed at
the selection for you.

It hands the command the real terminal (so wizards, ssh/sftp sessions, and
exec's live output all work exactly as they would standalone), pauses for a
keypress when it's done, then returns you to the list — which reloads in
case what you ran changed the inventory.

## Commands

```
meterm                          interactive picker
meterm connect <name> [-- args] connect (aliases: c, ssh); extra args go straight to ssh
meterm connect deploy@<name>    log in as another account (save them: edit --users deploy,root)
meterm connect <name> --tmux    inside tmux: pick up an existing session or start one
meterm dev <name>[:<dir>]       back to work on a host: named tmux session in <dir>,
                                reconnecting on drops, after sleep, and on SIGUSR1
                                (--tunnel <name>|all, --session s)
meterm dev <name>[:<dir>] -- <cmd>   the same, with cmd running once in a new window of
                                the session (the exit code is the session's, not cmd's)
meterm add <name> [flags]       add a host (omit flags for a guided prompt)
meterm edit <name> [flags]      edit a host (prompts: enter keeps a value, - clears it;
                                or e.g. --proxy-jump "" to drop a jump host)
meterm rm <name>                remove a host
meterm list [--group g] [--tag t] [--team T]

meterm group add <name> [flags] create/update a group (shared user/key/port/jump host/tags)
meterm group list
meterm group rm <name>

meterm import [path]            pull hosts out of an existing ~/.ssh/config
meterm import tailscale         the machines in your tailnet (local tailscale CLI; --tag, --online, -u user)
meterm import ansible <file>    an Ansible inventory, INI or YAML (vars and groups → address/user/port/jump/tags)
meterm export [--write-include] render meterm's hosts as a plain ssh_config
meterm export --format ansible [-o file]   ...or as an Ansible YAML inventory (groups from groups and tags)
                                 fragment, so scp/rsync/git/VS Code Remote-SSH
                                 can use the same names

meterm cp <src> <dst> [-r]      upload/download a file or dir over scp
meterm cp --sync <src> <dst> [-- rsync-flags]   same, but incremental/resumable via rsync

meterm exec [host...] -- <cmd>  run a command on several hosts at once (--tag/--group/--all, -j N, --timeout, --format json)
meterm tunnel add|list|rm|up    named port-forwards, opened on demand (up --keep reconnects)
meterm relay init|enroll|invite|accept|status|remove   reach a machine behind NAT through a relay host (reverse SSH tunnel)
meterm route add|list|rm|check  other ways to reach a host (public forward, LAN, another jump), used when its own address doesn't answer

meterm history                  show recently connected hosts, most recent first
meterm activity [--host H] [--verify] [--format text|json]   what meterm was used to do, as a tamper-evident log
meterm rename <old> <new>       rename a host, keeping history and jump-host references
meterm files <host> [dir]       two-pane file manager: copy between here and the host (--shell: plain sftp prompt)

meterm keygen [--file p] [--for name]   generate an ed25519 key, optionally set as a host's identity
meterm copy-id <name> [--identity p]    install your public key on a host (jump-host aware)
meterm copy-id bs@<name> --via dev      for an account on a server without password logins: through
                                        dev (sudo), written as bs; checks/offers AllowUsers, sshd -t
                                        (Windows OpenSSH servers detected: user or administrators file)
meterm keys list [--by-key]             who can log in where (authorized_keys, read-only)
meterm keys revoke --comment bob@laptop take a key away from every host (preview, backup, lockout guard)

meterm stats <name>             CPU/memory/disk/network snapshot from a host (Linux + macOS/BSD)
meterm agents [host...] [--tag t] [--group g] [--format text|json] [--timeout 8s] [-j N]   Claude Code sessions on hosts; read-only, not logged

meterm logs <name> [path...] [-f] [-n N]        tail log files (no path: list /var/log)
meterm logs list <name> [dir]                   see what log files actually exist (default dir: /var/log)

meterm audit [host...] [--tag/--group/--all] [--probe] [--format text|json]   security baseline, read-only
meterm report --client <name> [--title T] [--out file] [--format html|text|json]   same baseline, as a file to hand a client
```

### Terminals with their own TERM (Ghostty, kitty, WezTerm, …)

Newer terminals set `TERM` to a name of their own (`xterm-ghostty`,
`xterm-kitty`, …) that most servers have no terminfo for, so `htop`, `vim`
and friends fail there with "Error opening terminal". Their own fixes
(Ghostty's `ssh-env`, kitty's ssh kitten) only wrap an `ssh` typed in a
shell, which meterm doesn't go through — so `meterm connect` and `files
--shell` send `xterm-256color` instead, which every server knows and which
those terminals emulate. Set `METERM_TERM=keep` to send your `TERM`
unchanged (for hosts where you've installed the terminfo, e.g. `infocmp -x
xterm-ghostty | meterm connect web1 -- tic -x -`), or `METERM_TERM=<name>`
to send a name of your choice. `meterm dev` does the same.

### Clipboard and notifications through `meterm dev`

`meterm dev` puts you in tmux on the host, and what a program sends to the terminal has
to get past that tmux. meterm doesn't filter any of it on the way. This is what comes out
at your terminal with tmux's default settings and `TERM=xterm-256color`, as measured by
`scripts/it-clipboard.sh` with tmux 3.8:

| From a pane on the host | Reaches your terminal? |
|---|---|
| Copying in tmux's own copy mode | yes |
| A program's OSC 52 (`vim`'s or `nvim`'s osc52, `printf '\e]52;…'`) | no |
| The same with `set -s set-clipboard on` in the host's `tmux.conf` | yes |
| OSC 52 in a passthrough wrapper, with `set -g allow-passthrough on` | yes, and not without it |
| OSC 777 and OSC 9 notifications | no |

The usual fix is the `set-clipboard on` line. `meterm dev` doesn't do it for you:
`set-clipboard` belongs to the whole tmux server (setting it for one session changes every
session, which the script also checks), so that would change how all your work on that
host behaves. It also lets anything printed in that tmux, such as a hostile file you `cat`,
write to your clipboard. That's for you to choose per host. Older tmux may differ: before
3.2 the terminal support copy mode needs isn't built in.

## Moving files

`cp` takes exactly one saved-host endpoint, written as
`<name>:<path>`; the other side is a plain local path. Direction (upload vs.
download) is inferred from which side is the host:

```sh
meterm cp web1:/etc/nginx/nginx.conf ./nginx.conf   # download
meterm cp ./deploy.tar.gz web1:/tmp/                # upload
meterm cp -r web1:/var/log/myapp ./logs             # download a directory

meterm cp --sync ./dist/ web1:/var/www/app/ -- -avz --delete   # rsync, custom flags
```

It reuses the host's saved user/port/identity/jump-host, exactly like
`connect` does. For poking around without knowing exact paths up front, use
`meterm files web1` (below), or `meterm files --shell web1` for OpenSSH's
plain sftp prompt. (`meterm sync` and `meterm sftp` still work for now, but
are deprecated in favour of these.)

## File manager

`meterm files <host>` (or `f` on a host in the picker) opens a two-pane file
manager, vifm / Midnight Commander style: this machine on the left, the host
on the right. Select files on one side and copy them to the directory open on
the other.

```sh
meterm files WEB                         # starts in your home on WEB, and the current directory here
meterm files WEB /var/www --local ~/site
```

| Key | Does |
|---|---|
| `tab` | switch side |
| `h j k l` / arrows, `enter`, `backspace` | move, open a directory, go up |
| `/` (then `n`) | find by name as you type (next match) |
| `space`, `a` | select / unselect, select all / none |
| `c` / `F5` | copy the selection (or the file under the cursor) to the other side |
| `d` / `F8` | delete, recursively, after a `y` |
| `r` / `F6`, `m` / `F7` | rename, new directory |
| `.`, `~`, `R`, `q` | hidden files, home, reload, quit |
| `esc` | cancel a running copy |

- **Copying over existing files asks first:** overwrite them, or skip them.
- **Progress:** a running copy shows a progress bar and its speed.
- **Cancelling:** a cancelled copy removes the half-written file.
- **Permissions:** permission bits are kept, so scripts stay executable.
- **Symlinks:** a symlink to a file is copied as the file; a symlink to a
  directory is skipped, to avoid loops.

The host side is SFTP through your own `ssh` (`ssh -s host sftp`), so jump
hosts, keys, the agent and `known_hosts` behave exactly as for
`meterm connect`. Uploads, downloads, deletes, renames and new directories on
the host go into the activity log (and to the team, for shared hosts). Use
`meterm cp --sync` (rsync) instead for very large trees you want to resume.

## Running commands on a fleet

`exec` runs the same command on every matching host in parallel and streams
each host's output back with a colored `[name]` prefix:

```sh
meterm exec --tag web -- systemctl restart nginx
meterm exec --group prod -- uptime
meterm exec web1 web2 -- df -h
meterm exec --all -j 20 -- uptime   # cap concurrency at 20
meterm exec --all --timeout 30s --format json -- uptime   # for scripts and CI
```

On a terminal, escape sequences other than colours are dropped from what hosts send,
in `exec`, `logs` and `stats` alike, so a line of output can't set your clipboard or
move your cursor. Bidi controls go too, so it can't reorder what you read. `--raw` shows them as they are; pipes and `--format json` always get
what was sent. An interactive session (`connect`, `dev`) isn't filtered, since
full-screen programs are made of those sequences.

Several arguments reach the host exactly as given, each quoted, so `$(...)` or `;`
in one is only text. One argument is a command line for the host's shell, so pipes
and the like go in one string: `meterm exec web1 -- 'df -h | grep /srv'`. Scripts
that put shell syntax in separate arguments, or quotes inside one
(`-- sh -c "'a; b'"`), need to change: `-- sh -c 'a; b'` is what that means now. A
Windows server gets the arguments joined with spaces, as before, because cmd.exe
doesn't read these quotes, so don't pass one text you don't trust.

`--tag`/`--group` are repeatable and can combine with explicit host names;
matches are deduped. When everything has finished it prints a summary table
(host, exit code, time) and exits nonzero if any host failed — so it can gate
a CI step. `--timeout` gives up on a host that hangs. `--format json` captures
each host's stdout, stderr, exit code and duration instead of streaming, and
prints one JSON array; an exit code of 255 is ssh itself failing (unreachable,
auth, host key), not your command. Because it doesn't attach a terminal, it's for
non-interactive commands on hosts with key-based auth already working — it
won't sit there waiting on a password prompt, it'll just fail fast.

## Bringing an inventory in and out

meterm doesn't have to be where hosts start. `import tailscale` reads
`tailscale status --json` on this machine (no API key) and offers each device
as a host at its 100.x address, tagged `tailscale`, `os:<os>` and its ACL
tags; `--tag tag:server` and `--online` narrow it down. `import ansible`
takes an existing inventory. Every import skips a host whose name or address
is already saved, so it's safe to re-run.

```sh
meterm import tailscale -u ubuntu --tag tag:server
meterm import ansible ./inventory/hosts.ini --yes
meterm export --format ansible -o inventory.yml   # then: ansible -i inventory.yml web -m ping
```

The Ansible export uses each host's *effective* settings (group and default
user, port, key, jump chain as `-J`), and makes an Ansible group per meterm
group and per tag (`exposes:web` becomes `exposes_web`).

## Tunnels

A tunnel is a port-forward saved on a host under a name, opened when you need
it rather than on every connect. It goes through the host's jump hosts, and a
team host's tunnels are shared with the team on `meterm team push`.

```sh
meterm tunnel add prod-db pg -L 5432:localhost:5432   # localhost:5432 here → :5432 on prod-db
meterm tunnel add bastion socks -D 1080                # SOCKS proxy
meterm tunnel add web1 hook -R 8080:localhost:3000     # web1:8080 → your laptop's :3000
meterm tunnel list
meterm tunnel up prod-db/pg                            # hold it open; Ctrl-C to close
meterm tunnel up prod-db bastion/socks --keep          # several at once, reconnecting on drops
meterm tunnel rm prod-db pg
```

`up` runs one `ssh -N` per host and fails at once if a local port is already
taken (`ExitOnForwardFailure`), instead of looking connected while forwarding
nothing. Forwards saved with `add --forward-local` have no name and still open
with every `meterm connect`.

**Say what a tunnel is.** `--note "Billing API (prod)"` is shown in
`tunnel list`, and is required on team hosts — teammates open those tunnels
too, and a name like `billing` doesn't tell them it's production. `tunnel
add` looks (read-only) at what's at the far end: the systemd service or
Docker container listening on the port when it's the server's own, or
whether another host answers as seen from the server. It warns when
nothing does — usually a mistyped port — and on a personal host uses what
it found as the note. Tunnels on a production host, or whose name or note
says prod/production, are marked ⚠ in `tunnel list`, and `tunnel up` says so
before opening them.

## Saying what's production

`env` says what a machine is: `production`, `staging`, `dev` or `test`.

```sh
meterm edit web1 --env production        # this host
meterm group add live --env production   # every host in the group that has no env of its own
meterm edit web2 --env staging           # a host's env wins over its group's
meterm edit web1 --env ""                # take it off again
```

meterm doesn't guess it from a host's name or tags: an unlabelled host isn't
treated as production. `list` shows an ENV column once a host has one, and
`list --format json` gives it as `env`, the host's or else its group's, so
desktop tools and scripts have one answer to ask for. A host's env is shared
with its team; a group's isn't, because groups stay on each machine. A value
typed by hand into `hosts.yaml` that meterm doesn't know counts as production,
so a typo can't turn a warning off. A team copy with such a value is refused.

### In the background (Linux and macOS)

`meterm tunnel enable <host>/<tunnel>` writes a job that holds one tunnel open, starts
it, and brings it back at each login: a systemd user unit on Linux, a LaunchAgent on
macOS. `meterm status` shows how it's doing, and `meterm tunnel disable <host>/<tunnel>`
stops it and removes the job.

The unit runs the binary you enabled it with, by its full path, so a warning appears
if that isn't `/usr/bin/meterm`. It logs in with no terminal, so the key has to be
in your agent or have no passphrase; until then it keeps trying. If the tunnel or its
host has been removed or turned unsafe in a host file that can be read, it stops and stays
stopped, and `systemctl --user status` shows why. Anything else is retried: a host file that
can't be parsed or has no hosts in it, anything ssh said (a server can say what it likes
before login), and a SIGTERM from outside. Being stopped with `systemctl --user stop` isn't
restarted, though it shows as failed. The tunnel's start
is one entry in the activity log, however long that takes. A tunnel on a production host,
or whose name or note mentions production, warns you when enabled.

**On macOS** the job is `~/Library/LaunchAgents/com.github.bcd91.meterm.tunnel.<name>-<hash>.plist`,
loaded with `launchctl bootstrap gui/<uid>`, and its output goes to
`~/Library/Logs/meterm/<the same name>.log`. It behaves as the Linux unit does, with these
differences:

- launchd can't be told to leave one exit code alone, only to restart on anything but
  success. So a job that has nothing left to hold exits with 0, and stays stopped until
  the next login or `tunnel enable`. The log says why, and `meterm status` lists it as
  `stopped`.
- `enable` puts back what was there if it fails: a new job file is removed, or the previous
  job returns. Enabling what's already enabled doesn't drop the tunnel. `disable` works with
  the job file gone, and won't say Disabled unless launchd says so.
- It runs the binary by the path you started it with and not through symlinks, so
  Homebrew's `/opt/homebrew/bin/meterm` keeps working through an upgrade. A binary in a
  temp directory gets a warning.
- ssh is the system's, and its agent is launchd's `SSH_AUTH_SOCK`, so the key has to be in
  that agent (`ssh-add --apple-use-keychain`), have no passphrase, or be reached through
  `IdentityAgent` in your ssh config. You're warned if this shell uses a different agent.
- `$PATH` is the system's, then `/opt/homebrew/bin` and `/usr/local/bin`, for a
  `ProxyCommand` that lives there. The `METERM_*` directories set at enable time carry
  over, as `--config` does.
- It runs in your login session: it stops when you log out and comes back when you log in,
  as a systemd user unit does without lingering. With nobody logged in at the screen,
  `enable` fails. macOS may show a "background item added" notice, and the job can be
  switched off under Login Items in System Settings, which `enable` then reports.
- The log file isn't rotated, and meterm won't: a tunnel failing for months adds some tens of
  megabytes, and it's safe to empty. What ssh prints goes in with escape sequences removed.

## Routes: more than one way in

A host can have routes besides its own address — a public port forward next
to a VPN address, another jump host, the LAN address for when you're in the
office. Whenever meterm connects, it checks the first hop of each (the host,
or the first jump host on the way) for an SSH server's greeting, its own address
first and then the routes in order, and uses the first that gives one. Hosts
that jump through it follow automatically: give an office's jump host a
second route and every machine behind it gets one too.

```sh
meterm route add devbox public --address 203.0.113.7 --port 5000   # e.g. a port forward on the office firewall
meterm route add db1 office-lan --address 192.168.1.20 --direct     # straight in when you're on the LAN
meterm route list
meterm route check            # which first hops answer, and which route would be used
```

- The route that worked last time is tried first (remembered in
  `routes.json` next to the host file), so the usual case costs one connect;
  a route that stops answering costs about 2 s before the next is tried.
  `meterm connect` says when it isn't using the host's own address.
- A route reaches **the same machine**, so its host key is the same: when
  the host's own address is already trusted in `~/.ssh/known_hosts`, meterm
  adds the same keys under the route's address (no new trust, and batch
  commands like `exec` don't stop at an unknown-host prompt). If a route
  really led to a different machine, ssh would report a changed key and refuse;
  `dev` and `tunnel up` warn and try the next route, which ssh checks the same
  way, and `connect` stops there.
- Only the first hop can be checked without logging in; past it, a route
  fails just as ssh would.
- Routes are shared with the team like the rest of the host. Everyone needs
  meterm 0.12+: an older version keeps working but drops a host's routes if
  it pushes that host.
- `list` shows `(+N route)` without checking anything; `list --check` and
  `--format json` (field `route`) show the route in use.

## Relays: reaching machines nobody can connect to

A machine behind NAT, CGNAT or a closed cloud firewall can still be reached:
it keeps one outbound SSH connection to a **relay** — any saved host everyone
can reach — and gets a route through it. Only OpenSSH and systemd are used.

```sh
meterm relay init DEVBOX-1              # once per relay (sudo there)
meterm relay enroll GW1 --via DEVBOX-1  # per machine (sudo on both)
meterm relay status                      # which tunnels are up
meterm relay remove GW1
```

- **On the relay**, `init` creates the account `meterm-relay` (no password,
  no shell) and appends a `Match User meterm-relay` block to the end of
  `sshd_config` allowing reverse forwards on localhost only; it's added to
  any `AllowUsers` line. The new config is checked with `sshd -t` and put
  back if that fails. Each machine's key is added as
  `restrict,port-forwarding,permitlisten="localhost:<port>"`: it can listen
  on its own port on the relay's localhost and do nothing else.
- **On the machine**, `enroll` creates a key in `/etc/meterm-relay`, writes
  the relay's host keys there — taken from your own `known_hosts`, so the
  machine trusts exactly what you already trust, never blindly — and
  installs a systemd unit that keeps `ssh -N -R` open and reconnects. The
  machine tries the relay's routes first (typically its public way in),
  then its own address, and uses the first it can actually reach.
- **Here**, the machine gets a route `relay-<relay>` (127.0.0.1:<port> via
  the relay), used like any route when its own address doesn't answer, and
  shared with the team.
- Run `enroll` while the machine can still be reached some other way.
  Without passwordless sudo, run it in a terminal: sudo asks there.
- `route check` can only see that the relay answers; `relay status` checks
  that each tunnel is actually listening there.

**A machine you can't reach at all** (behind NAT, no address) but that can
reach the relay is added the other way round:

```sh
meterm relay invite NEWBOX --via DEVBOX-1    # writes meterm-invite-NEWBOX.sh
# someone at NEWBOX runs:  sudo sh meterm-invite-NEWBOX.sh
# it prints a line to send back:
meterm relay accept NEWBOX <reply>
```

- The script holds no secrets: the relay's addresses and host keys (as you
  already trust them), the tunnel port, and your public key. It installs
  that key for the login user (by default, whoever runs it with sudo) —
  written as that user, never through a symlink — creates the tunnel's key
  on the machine (the private half never leaves it) and starts the tunnel,
  which keeps retrying until you accept.
- The reply is untrusted input: every field is checked, and the port and
  relay come from your own record of the invite, not from the reply.
  `accept` shows the machine's host key fingerprints; have the person read
  theirs out (`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`) before you
  say yes. That check is what makes the first connection trustworthy.
- `accept` authorizes the tunnel key on the relay, adds the host keys to
  your `known_hosts`, saves the machine (127.0.0.1:<port> via the relay)
  and logs in once to prove it all works. `relay status` lists invites still
  waiting for a reply.

## Who can log in: keys

```sh
meterm keys list --tag prod             # every key in authorized_keys, per host
meterm keys list --all --by-key         # each key once, with the hosts it opens
meterm keys revoke --comment bob@laptop # remove it everywhere (asks first)
meterm keys revoke --tag prod --key ~/offboarding/bob.pub --yes
```

`list` reads the login user's `~/.ssh/authorized_keys`, and root's and every
`/home` user's too when `sudo -n` works (it says when it couldn't look). Keys
this machine holds are marked `[you]`.

`revoke` matches by key (`--key file.pub`), `--fingerprint`, or exact
`--comment`, shows every line it will remove, and asks. Each file is copied
to `authorized_keys.meterm-bak-<time>` first and rewritten in place, so owner
and mode don't change; other users' files are edited with `sudo -n`. It
refuses, unless `--force`, to remove a key this machine holds or the last key
in the file meterm logs in with — either would lock you out.

`meterm team remove <email>` takes someone out of the team (owners only;
your own email leaves). It can't reach into the servers by itself — their key
still works until it's revoked — so `--revoke-keys` follows up with a revoke
of keys commented with their email on every team host.

## Setting up a brand-new host

No key yet, or the host doesn't trust any of your keys? Two commands (or
`K` then `i` in the picker) get you from nothing to `meterm connect`:

```sh
meterm keygen --for web1        # generate ~/.ssh/id_ed25519, set it as web1's identity
meterm copy-id web1             # install the new public key (you'll be asked for
                                 # web1's current password this one time)
meterm connect web1             # now key-based, no password
```

`copy-id` is idempotent (safe to run again) and, like `connect`, uses the
host's resolved port/jump-host — it isn't a thin wrapper around the system
`ssh-copy-id`, it reimplements the same trick (pipe the key over stdin,
since ssh reads the password prompt from the terminal directly, not stdin)
so proxy-jump hosts work the same as everything else in meterm.

## Logs

```sh
meterm logs web1                       # what's in /var/log?
meterm logs list web1 /var/log/nginx   # ...or another directory

meterm logs web1 /var/log/syslog       # last 200 lines
meterm logs web1 /var/log/syslog -f    # follow it live (Ctrl-C to stop)
meterm logs web1 '/var/log/nginx/api_*.access.log'   # several files at once
```

## Security audit

`meterm audit` checks every internet-facing host against the same baseline and
tells you who has to fix what. It is **read-only**: one script is piped to
`sh -s` over SSH and changes nothing; it never prompts for a password and never
uses `sudo` except `sudo -n` (non-interactive) for a couple of read-only status
commands. Anything that needs root and can't be read is reported as **could not
be checked** — never as passing.

```sh
meterm audit --all
meterm audit WEB GW1 --format json          # for other tools
```

Baseline (the same for every host; exposure only decides which controls apply):

| Control | Looks at |
|---|---|
| `os-support` | release still gets security updates (Ubuntu/Debian date table; unlisted = unknown) |
| `patching` | pending updates, reboot required, days since boot |
| `auto-updates` | `unattended-upgrades` enabled |
| `ssh-auth` | `PasswordAuthentication`, `PermitRootLogin` (first occurrence wins, like sshd) |
| `brute-force` | `fail2ban` or `crowdsec` running |
| `firewall` | `ufw`/`firewalld` active |
| `exposed-ports` | anything bound to all interfaces beyond 22/80/443; databases are recognised even when Docker publishes them on a random host port |
| `web-hardening` | `server_tokens off`, a `limit_req_zone` exists |
| `observability` | access logs readable, `$request_time` logged, retention ≥ 30 days |
| `tls-renewal` | `certbot.timer` active (unknown if certificates are managed elsewhere) |
| `framework-cves` | Next.js version inside each container against known RCE / auth-bypass / source-exposure fixes (a floor: run `npm audit` in the repo for the full picture) |
| `container-privileges` | containers running `--privileged` or holding `/var/run/docker.sock` |
| `db-superuser` | an app's `postgres://` user is the database superuser (only user names are read, never passwords) |
| `container-egress` | the host reaches the internet and nothing (`DOCKER-USER` DROP/REJECT, or an upstream firewall) stops containers calling out |
| `disk-space` | any filesystem ≥ 80% full (≥ 90% is high), by space or by inodes |
| `time-sync` | the clock is NTP-synchronised (`timedatectl`) |
| `failed-units` | systemd services in the failed state |
| `cert-expiry` | Let's Encrypt certificates in `/etc/letsencrypt/live` expiring within 30 days (14 is high, expired is critical) |

`framework-cves` through `container-egress` look *inside* what the host runs, for the marks of a
compromised web app rather than a misconfigured server, such as an outdated
Next.js container used to mine and to exfiltrate its environment. To do that they reach a little further than the rest:
`docker exec` runs `sh` inside each container to read
`node_modules/next/package.json`, and the host makes one
HTTP request to `connectivitycheck.gstatic.com` to learn whether it can reach
the internet. Nothing is changed.

The last four are day-to-day operations rather than security: the things a
team checks every morning. `cert-expiry` reads certificates with `sudo -n`
only when `/etc/letsencrypt/live` isn't readable; certificates kept elsewhere
(nginx-proxy-manager, Caddy, a load balancer) show as n/a.

**Exposure.** Tag hosts with how they face the internet:
`exposes:ssh`, `exposes:web`, or `internal`. A host with none of these is
assumed to expose both, so a forgotten host gets the strict treatment.

```sh
meterm edit WEB --tags acme,web,exposes:ssh,exposes:web
```

**Hand-off.** The report ends with **ACTIONS BY OWNER**: failures regrouped by
the role who can act (sysadmin with root, network/cloud firewall, application
owner) so it can be forwarded as-is.

**Probing (opt-in).** "Bound to all interfaces" does not prove a port is reachable
from the internet: it also covers a VPN (Tailscale), a private VPC and the LAN, and
a cloud or router firewall may block the public side. `--probe` makes a plain TCP
connect (no data sent) from *this* machine to those ports — and to 22/80/443, so
an `exposes:ssh` tag can be checked against reality:

```sh
meterm audit GW1 --probe                              # uses the host's own public address
meterm audit WEB --probe --probe-addr WEB=203.0.113.10  # a host behind NAT needs the address the internet sees
```

A host reached over Tailscale or a jump host is probed at its **own public address**
(read from its interfaces), not at the inventory address — probing that would only
test the VPN. A host with no public address of its own (behind NAT) needs
`--probe-addr`. The report lists what was reachable and what was not.

Two limits: the result is from one network, so "reachable" may mean "open to your
office IP" rather than to the world (repeat from a phone hotspot to tell); and from
inside the network you are auditing it says nothing about the internet. Only probe
hosts you own.

### Watching for changes (cron)

`meterm audit watch` runs the audit, compares it with the previous run, and
posts what changed to a webhook: a new or worse failure, new evidence on an
existing one (another open port, another dropped file), a host that can no
longer be audited, and fixes. Nothing is sent when nothing changed; the first
run sends the current failures.

```sh
meterm audit watch --tag acme --notify https://hooks.slack.com/services/…
# cron, every morning at 07:00, quiet unless something changed:
0 7 * * * meterm audit watch --tag acme -q --notify https://discord.com/api/webhooks/…
```

`--notify` understands Slack and Discord incoming webhooks; any other URL gets
`{"text": …, "changes": […]}` as JSON, which n8n, Zapier or Make can forward to
email or LINE. Use `--name` to keep separate histories (e.g. one watch per
client). If you're signed in to a team, the latest result of every shared host
is also stored in the team: `meterm team audits` shows it to everyone.

### Client reports (MSP / consulting)

If you run `audit` for more than one client or company, `meterm report` is
`audit` shaped for handing the result to *them* instead of reading it
yourself: a self-contained HTML file (no external CSS/JS — safe to email or
hand over as-is), not a terminal dump.

A "client" is just a tag by convention — `client:<name>` — so this needs no
separate place to manage clients and every existing selector (`--tag`,
`--group`) still works:

```sh
meterm edit web1 --tags client:acme,exposes:web
meterm report --client acme                                      # writes meterm-report-acme-<date>.html
meterm report --client acme --title "Acme Corp — Q4 2026" --out ~/reports/acme-q4.html
meterm report --client acme --format json | jq .                 # or text/json instead, to stdout
```

Everything `audit` supports (`--probe`, `--probe-addr`, `-j`) works the same
way here — `report` just wraps the same baseline and selection logic in a
different output.

When the hosts are shared with a team, the report ends with **who accessed
them** over the last 30 days (person, how many actions of each kind, when
last), from the team activity log — `--access-days N` to change the window,
`0` to leave it out. For the raw log, `meterm team log --format csv` (add
`--user someone@example.com` for one person) opens in any spreadsheet.

## Host stats

`meterm stats web1` (or `S` in the picker) SSHes in and dumps uptime/load,
CPU core count, memory, disk usage, and network interfaces — one host at a
time, on demand, not run automatically for every host the way the status
dot is. It's aware of both Linux (`free`, `nproc`, `ip`) and macOS/BSD
(`vm_stat`, `sysctl`, `ifconfig`) remotes, picking whichever tool exists.
Like `exec`, it's non-interactive, so it needs key-based auth already
working — it won't hang on a password prompt.

## Live status

`meterm status` shows what running `meterm dev` and `meterm tunnel up` processes
hold open: which host and route, and whether each is connecting, connected,
reconnecting, or failing. It reads local files only, so it's cheap to call from a
status bar. `meterm connect` isn't tracked.

```
meterm status                               # a table
meterm status --format json                 # {"schema":1,"entries":[…]}
meterm status --format json --watch         # one JSON line at the start and per change
```

Each process keeps one file in `$XDG_RUNTIME_DIR/meterm` (`$METERM_RUNTIME_DIR` if
set, or a directory of your own under the temp directory without either) and removes
it when it ends. On macOS it's `~/Library/Caches/meterm/run` whatever those say, so a
tunnel started by launchd and a `status` typed in a terminal always find each other. A
process killed outright is dropped by the next `meterm status`. On macOS a tunnel from
`tunnel enable` with no state file shows too, as launchd sees it: `running` (with its pid; the
file was purged and comes back at the next change), `stopped`, saying why if it knows, such as a
binary that has moved, or `unknown` if launchctl couldn't say. Under `sudo` with `HOME` kept,
the directory is one of root's own under the temp directory, not the user's. All the strings
in both formats have control, bidi and invisible characters replaced.
Tunnels and sessions learn they got in through ssh's `LocalCommand`, so one set in your
own ssh config is replaced while they run.

## Claude Code sessions on your hosts

`meterm agents` lists the Claude Code sessions running on the hosts you choose.

```sh
meterm agents t480                       # a table
meterm agents t480 web1 --format json    # for a status bar or a script
meterm agents --tag laptop --timeout 4s -j 4
```

It runs one fixed, read-only command, `claude agents --json`, and looks for `claude` in
`~/.local/bin` and the other usual places too, since a non-login ssh often hasn't got them
on `PATH`. Nothing you type or that's saved about a host goes into that command. You have to
say which hosts; with none it's an error, and there's no `--all`. A production host is asked
only when you name it: picked up by `--tag` or `--group`, it's listed as refused. The same goes
for a host reached through production, by any of its routes, whether a hop is a saved name,
`user@name` or just a production host's address.

A host's `extra_args` have to pass an allowlist, or that host is reported as an error and
no ssh runs: `-p`, `-i`, `-l`, a few plain flags, and `-o` with connection settings such as
`Port`, `User`, `IdentityFile` or `ConnectTimeout`. A bare word in there would become ssh's
destination, and `-o ProxyCommand`, `-A` or `-M` would make this more than one fixed command.

```json
{"schema":1,"hosts":[{"host":"t480","reachable":true,"error":"","agents":[
  {"cwd":"/home/a/src","kind":"interactive","startedAt":1793311202058,"pid":812,
   "status":"busy","sessionId":"…","name":"fix login"}]}]}
```

- `agents` holds the fields `cwd`, `kind`, `startedAt` (Unix milliseconds), `id`, `state`,
  `pid`, `status`, `waitingFor`, `sessionId` and `name`, each only when the host sent it.
  Anything else is dropped. A known field of the wrong type makes the whole host an error.
- `reachable` says the command ran on the host.
- `error` is empty when the host answered, and in meterm's own words otherwise: it never
  quotes the host. When it's set, `agents` is `null`, so a host that can't say doesn't read
  as a host with no agents. That covers no connection, a refused key, a timeout, no `claude`,
  output that isn't what's expected, and more than 1 MiB of it.
- Every string has control, C1, bidi, format and other invisible characters replaced by
  U+FFFD, in both formats, and is cut at 1024 characters.

| Exit code | Meaning | stdout |
|---|---|---|
| 0 | Every host answered. Having no agents is an answer. | the report |
| 1 | Nothing was asked: bad flags, no selection, an unknown host, or a tag or group that matched nothing | nothing |
| 2 | At least one host has an `error`, a refused production host included | the report, complete |

**It isn't in the activity log.** That log is tamper-evident and shared with the team, so
nothing can be taken out later, and a status bar asking every 30 seconds would bury
everything else in it. So `agents` writes no activity entry and no history, and doesn't
start a team sync. What shows instead is on each host: every look is a login of its own,
which sshd logs. Sharing a connection between looks would have hidden those, and let a
revoked key keep working, so none is shared, on any host. `meterm exec` remains the command
that's recorded.

A look leaves nothing on this machine. It doesn't remember which route worked or carry a
host key to a route's address as other commands do, and a host key that isn't trusted yet
is that host's error rather than something to accept: run `meterm connect` once to trust
it. `--timeout` covers picking a route as well as the login. For the length of the run
there's one file in a private temp directory, the ssh config behind `-F` that hands the same
options (`BatchMode`, strict host keys, no forwarding, no shared connection) to jump hops,
which get none of a command line's `-o`. It includes your own ssh config after them, so
`ProxyCommand` or `IdentityFile` there still apply: that file is yours to trust.

## Status and history

`meterm list --check` TCP-pings each host's port and adds a STATUS column of
`up`/`down`/`via-jump` (this is what the picker does live in the background,
without you asking). A host with a `proxy_jump` is reported `via-jump`
rather than dialed directly — from outside that jump host's network a
direct dial to a private address almost always fails even when the host is
perfectly reachable through the jump, so a bare "down" there would lie.
Every host also gets a LAST CONNECTED column, backed by the same history
`connect` and `files --shell` update on every use — see it on its own with `meterm
history`.

Host add/edit flags: `--address --user --port --identity --group --tags
--proxy-jump --notes --forward-local LOCAL:HOST:REMOTE`.

`--proxy-jump` accepts either another saved host's name (resolved to its own
address/user/port/key automatically, and chainable — a jump host can itself
have a jump host) or a raw `user@host` spec.

## Scripting / AI agents

meterm is a normal CLI — no daemon, no API — so a script or an AI agent
drives it the same way a person would, just without the interactive bits.
A few things make that reliable:

- **Always pass the flags that make a command non-interactive.** A few
  commands fall back to a prompt when required info is missing, which hangs
  forever with no TTY attached:
  - `add <name> --address <addr>` — passing both a name and `--address`
    skips the add wizard entirely. Calling `add` with either one missing
    starts the interactive wizard instead.
  - `rm <name> --yes` — skips the "Remove host ...? [y/N]" confirmation.
  - `import --all` (or `--yes`) — skips the per-host "Import ...? [Y/n]"
    confirmation during `ssh config` import.
  - Bare `meterm` with no subcommand opens the interactive picker (a TUI) —
    it detects a non-TTY stdin and prints `--help` instead rather than
    hanging, but a script should just call a subcommand directly instead of
    relying on that fallback.
- **Use `--format json` for machine-readable output.** `list`, `history`,
  and `audit` all support it, with a stable, documented field schema (see
  each command's `--help`) instead of the tabwriter-aligned columns meant
  for a terminal — those columns can shift width release to release.
- **`meterm agents --format json`** is safe to poll, and has its own exit codes: see
  [Claude Code sessions on your hosts](#claude-code-sessions-on-your-hosts).
- **`exec`, `stats`, and `cp` are already non-interactive**
  by design — they need working key-based auth ahead of time and fail fast
  with a clear error instead of blocking on a password prompt.
- **Exit codes are meaningful.** A failed command exits non-zero and writes
  the error to stderr (`meterm: <message>`); `exec` additionally forwards
  the *remote* command's own exit code, so `meterm exec host -- some-check`
  behaves like running `some-check` directly for anything that branches on
  `$?`.
- **`meterm --debug`** (or `METERM_DEBUG=1`) traces every git command the
  team commands run, on stderr, and how each ended — safe to paste into a
  bug report.
- **`meterm --version`** reports the build's version — useful for an agent
  to confirm which release it's talking to before relying on a flag or
  output shape that changed between versions.

## Activity log

Every `connect`, `exec`, `cp` and `files` session appends one line to a local
activity log: when, which local account, which saved host and what it
resolved to, and the program or paths involved. `logs` is in there too. `meterm agents`
isn't: see [Claude Code sessions on your hosts](#claude-code-sessions-on-your-hosts).

The log can't forget, and it goes to the team for shared hosts, so a command's
arguments stay out of it by default: `exec` records `mysql (+3 args)`, and
`connect -- -t cmd` records `(+2 args)`. `--log-args` on either puts the arguments in,
redacted, for when you need the record to say exactly what ran.

```sh
meterm activity                     # most recent 50
meterm activity --host WEB -n 0      # everything on one host
meterm activity --verify            # check nobody edited or removed old entries
```

Each entry stores the hash of the one before it, so changing or deleting an
earlier entry breaks the chain from that point on, and `--verify` names the
first line that no longer fits. That makes tampering *evident*, not
impossible: anyone with the file can rebuild the whole chain. The team
repository keeps every member's copy, in its history, for that reason.

A line can't be removed later without breaking the chain, and shared hosts'
lines go to the team, so what looks like a credential is masked as
`[REDACTED]` before it's written: `-pSECRET` after mysql and sshpass,
`--password x`, `TOKEN=x`, `Authorization:` and `Cookie:` headers, passwords and
tokens in URLs, private key blocks, and well-known token formats. It's pattern
matching, so it leans towards masking too much and still can't catch everything:
a password given as a plain argument to a program it doesn't know stays as
typed. Pass secrets through a file or the environment on the host instead.

The log only sees what goes through meterm. Someone running plain `ssh` is
not recorded, so treat it as a record of the team's work, not as access
enforcement (that needs every connection to go through a proxy, as Teleport
does).

## Teams

Share hosts and their activity log with the people you work with. A team
lives in a **Git repository you control**. Use a repository you already have (GitHub, GitLab, Gitea, a bare repo over
SSH). No meterm account, no third-party server, and every change is a commit:

```sh
meterm team git add ACME git@github.com:acme/meterm-team.git   # creates the team if the repo is empty
meterm team share --tag acme                 # or names, --group, --all
meterm team invite bob@example.com --role admin   # records the role; grant repo access on the Git host
# bob: meterm team git add ACME git@github.com:acme/meterm-team.git
meterm team push                            # share later edits (preview first)
meterm team status                          # what's waiting, conflicts, queued activity
meterm team log --host WEB --since 168h      # who did what, from every member
meterm team remove bob@example.com --revoke-keys   # offboarding: out of the team and off the servers
```

Hosts are `hosts/<name>.yaml`, audits `audits/<name>.json`, and each device
appends its activity to its own `events/<device>.jsonl`, so concurrent pushes
don't conflict. Access is the repository's: who can clone reads the team, who
can push changes it; roles in `meterm-team.yaml` are honoured by meterm, but
only the Git host enforces them — so after `team remove`, also take the
person's access to the repository away. meterm keeps its own clone and commits
as your `git config user.email`. A Git team is as private as its repository
(there's no extra encryption layer).

### How syncing works

**What syncs, and when:**

| | When | Why |
|---|---|---|
| Activity | Automatically, right after each `connect`/`exec`/`cp`/`files` | Append-only, can't conflict; an audit log is only useful if it's complete and current |
| Team → you (pull) | Automatically, at most every 5 minutes while you use meterm (`list`, the picker) | Never overwrites your own unpushed edits |
| You → team (push) | Only when you run `team push` (or `share`), after a field-by-field preview | One typo would otherwise reach everyone at once |

Autosync runs as a short background process, so it never slows a command
down, and it waits quietly while you're offline. Turn it off with
`meterm team autosync off`. When something needs you, `meterm list` and the
picker show a one-line reminder (`⚠ team ACME: 1 not pushed — run meterm team status`).

- **What is shared:** address, user, port, group, tags, jump host, port
  forwards, extra args and notes. Private keys and `identity_file` never
  leave your machine: everyone keeps their own key for each host.
- **What a teammate can't make you run:** a shared host only ever reaches
  ssh as data. Addresses, users and jump hosts must be plain host names, IP
  addresses or `[user@]host[:port]`; nothing starting with `-`. A team
  host's extra ssh arguments are limited to connection tuning (`-4`, `-6`,
  `-C`, and `-o` options like `ServerAliveInterval`, `ConnectTimeout`,
  `Ciphers`): never `ProxyCommand`, `LocalCommand`, `KnownHostsCommand`,
  `PKCS11Provider`, `StrictHostKeyChecking`, `UserKnownHostsFile`, `-F`
  or agent forwarding. Names, notes and tags can't contain control
  characters (terminal escapes). A pulled host that breaks these rules
  isn't applied (`team status` shows it as rejected), a local one that does
  isn't pushed, and meterm refuses to connect to any host — or through any
  jump host — that fails them. Your personal hosts' extra args are yours to
  choose.
- **Roles:** `owner` (everything), `admin` (edit hosts, invite), `member`
  (use the hosts, add to the log). meterm honours them; the Git host's
  permissions are what enforce them.
- **Members propose, owners and admins approve.** A member's `team share`
  or `team push` goes to the repository's `meterm-proposals` branch instead:
  nobody pulls it, so nobody sees or reaches the host until it's approved.

  ```sh
  meterm team proposals          # what's waiting, who proposed it, how many warnings
  meterm team approve NEW-BOX    # full preview, checked again, then applied for everyone
  meterm team reject NEW-BOX     # drop it; the team is unchanged
  ```

  The preview warns about what a hostile proposal relies on going unread: an
  address no other team host uses, a changed address, jump host or route on
  an existing host, port forwards and ssh options. **Protect the team's
  branch** on the Git host (only owners and admins may push to it), or a
  member with plain `git` could still write it directly. "Proposed by" is the
  proposer's git email: what they say, not proof.
- **Editing a team host** (`edit`, `rename`, `rm`) changes only your copy
  until you `team push`. To drop a team host from your machine without
  removing it for everyone, use `meterm team ignore <host>`.
- **Conflicts and name clashes:** `team status` lists them.
  - `team resolve <host> --theirs` takes the team's copy and keeps your key.
    Use it when your personal host with the same name is the same machine.
  - `team resolve <host> --mine` pushes your copy instead (admins and owners).
  - If it's a different machine that only shares the name, run
    `meterm rename <host> <new>` instead.
- **Activity:** only events on hosts shared with that team are uploaded.
  Activity on personal hosts stays on your machine. `team log` re-checks
  every event's hash chain, so an edited or deleted entry shows. Anyone who
  can push could rewrite history, so protect the branch (no force-push) on
  the Git host. Keep in mind that `exec` records the
  command line, so don't put secrets in it on shared hosts.

Earlier versions also had a hosted cloud backend. It's gone (v0.17.0): a
team that was on it was moved to Git with `team git migrate` beforehand, and
meterm deletes the old sign-in and device keys from the machine (and its
keychain) the first time you run a team command.

## Where things live

Hosts are stored in a plain YAML file at `$XDG_CONFIG_HOME/meterm/hosts.yaml`
(macOS: `~/Library/Application Support/meterm/hosts.yaml`) — no daemon, no
database, easy to back up or put in a private dotfiles repo. Override the
location with `--config` or `$METERM_CONFIG`. Connection history lives next
to it in `history.json` (override with `$METERM_HISTORY`), and the activity
log in `activity/` (override with `$METERM_ACTIVITY_DIR`).

Nothing is ever stored in `~/.ssh/config` unless you run `meterm export
--write-include`, which is entirely optional and only adds one `Include`
line.

## Shell completion

```sh
meterm completion zsh > "${fpath[1]}/_meterm"   # or bash/fish/powershell
```
