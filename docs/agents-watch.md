# Watching agents: the `meterm agents --watch` stream

`meterm agents --watch` looks at the hosts you choose again and again, and says which Claude
Code sessions there are waiting for someone. It's meant to be run by a status bar or a
notifier as its own child process, reading stdout line by line. This page is the contract for
what it prints with `--format json`.

```sh
meterm agents devbox buildbox --watch --format json    # each host about every 60s
meterm agents --tag agents --watch --interval 30s      # readable lines, for a terminal
```

Hosts are chosen as for `meterm agents`: by name, `--tag` or `--group`, never all of them,
and a production host (or one reached through production) only when it's named. Picked up by
a tag or group, it's listed once as `refused` and never asked.

## How it looks

- **One fresh login per look.** Each look runs ssh, logs in, runs `claude agents --json` and
  logs out, exactly as `meterm agents` does. No connection is kept or shared between looks, so
  every look is in the host's sshd log, and a key revoked there stops the next one.
- **Each host on its own schedule**, about every `--interval` (60 s by default, 15 s at least),
  plus up to a tenth at random, so hosts watched together drift apart rather than being logged
  in to at the same moment. The first looks are spread over a tenth of the interval, at most
  10 s. `-j` (8) is how many looks can run at once.
- **A host that doesn't answer is asked less often.** The first failure is tried again after
  the interval; from the second failure in a row on, the wait doubles each time, up to 10
  minutes (with the jitter taken off the top, so never past it). The first good answer brings
  it back to the interval. Nothing is
  sent to a host but that ssh attempt: no ping, no wake-on-LAN. A host with routes has its
  routes dialled first, as every meterm command does (a TCP connection and the SSH greeting,
  no login). Whether a sleeping host wakes for an ssh attempt is the host's setting; set
  `agents_watch: false` on one that mustn't be woken (below).
- **A host that refuses the key is asked from every minute up.** When the host itself (not a
  jump host) says "Permission denied", whatever key the look offered, the look is an `error`
  ("not let in"), and the next one waits at least a minute, doubling to 10, whatever
  `--interval` is: at most 4 failed logins in any 10 minutes, under fail2ban's default of 5.
  OpenSSH 9.8 and later also turn an address away for a while after a failed login
  (`PerSourcePenalties`, 5 seconds per failure by default), which has run out long before the
  next try. It keeps being asked, as the key may be put back;
  a watcher key that never got in is the exception (below). A `-q` in the host's `extra_args`
  is left out of every look, as it would hide the refusal. If your ssh config gives the host
  another name (`HostName`), an ordinary look's refusal names that, isn't recognised, and the
  host is `unreachable` and backed off as one, from the interval up.
- **It stops on SIGINT, SIGTERM or SIGHUP**, ends any look in flight, prints a `stopped` line
  and exits 0. If stdout is closed (the reader went away), it stops the same way, ending the
  looks in flight, says so on stderr and exits 3; it never dies of SIGPIPE halfway. As the
  text form writes only when something changes, it doesn't wait for a write to find out: it
  polls stdout once a second for the reader having gone (on Linux and macOS; `agents` doesn't
  run on Windows). Started with stdout closed (`>&-`), on `/dev/null` or read-only, it doesn't
  start at all: exit 3, as it would log in to its hosts for nobody. It runs
  no daemon and leaves no file: each look's ssh config is a temp file removed when the look
  ends. The host list is read once, at the start; restart it to pick up changes.
- **A reader that stops reading can't hold up a stop.** Lines go out through a queue; when it's
  full, looks wait for it, and a signal still ends the watch. On the way out it waits for
  stdout to take the last lines for as long as it keeps taking them, up to 10 seconds; once
  2 seconds pass with none taken, or at 10, it says "stdout wasn't being read" and exits 3
  without them.
- **It never pauses.** Started from meterm's picker, other commands wait for Enter before
  going back; a watch doesn't, whatever it ends with.
- **It remembers nothing between runs.** Started again, it raises every wait that's still
  going on as new (`needs_input_started`), and its counts start at zero.

## The lines

Every line is one JSON object, with `schema` (1 for now) and `type`. A reader must skip a
`type` it doesn't know: new ones may be added. `schema` goes up only when a field changes
meaning; new fields don't need it to. Times are RFC 3339 in UTC.

### `start`, once

```json
{"schema":1,"type":"start","at":"2026-11-14T08:00:00Z","interval_s":60,"timeout_s":8,
 "hosts":["buildbox","devbox"]}
```

### `look`, one per host per look

```json
{"schema":1,"type":"look","host":"devbox","looked_at":"2026-11-14T08:00:03Z","took_ms":412,
 "result":"ok","error":"",
 "sessions":[{"cwd":"/home/dev/src/api","kind":"background","startedAt":1794590755329,
   "id":"a1b2c3d4","state":"blocked","pid":4101,"status":"waiting",
   "waitingFor":"permission prompt","sessionId":"a1b2c3d4-…","name":"fix-login",
   "event":"needs_input","agent_kind":"claude-code","source":"claude agents --json",
   "confidence":"high","needs_input_since":"2026-11-14T08:00:03Z"}],
 "last_ok_at":"2026-11-14T08:00:03Z","age_s":0,"failing_since":null,
 "next_look_at":"2026-11-14T08:01:06Z","stale_after":"2026-11-14T08:01:29Z",
 "looks":1,"failures":0}
```

| Field | Meaning |
|---|---|
| `host` | The host as meterm names it (`user@name` when it's not the saved login). |
| `looked_at` | When the answer came. `null` for a host that isn't looked at (`refused`, `opted_out`). |
| `took_ms` | How long the look took, route and login included. |
| `result` | `ok`, `unreachable`, `error`, `refused` or `opted_out`: see below. |
| `error` | Empty when `ok`, otherwise why, in meterm's own words. It never quotes the host. |
| `sessions` | When `ok`: the sessions, `[]` if there are none. Otherwise **`null`**: a host that couldn't say isn't a host with nothing running. |
| `last_ok_at` | When this host last answered in this run; `null` if it hasn't. |
| `last_sessions` | On a failed look: what it said then. Left out when there's nothing. |
| `age_s` | How old the newest answer about this host's sessions is: 0 on `ok`, seconds since `last_ok_at` on a failure, `null` if there's none. |
| `failing_since` | The first failed look of the failures still going on; `null` when the host answers. |
| `next_look_at` | When the next look is due. |
| `stale_after` | The latest the next `look` for this host can come: `next_look_at`, plus the wait for a slot when more hosts are watched than `-j` (at most ⌈(hosts − j) / j⌉ other looks), plus a look's own run (timeout + 2 s) for each, plus 15 s. With no newer `look` for this host by then, the watch has stopped or is stuck: show what you have as stale, not as current. |
| `looks`, `failures` | Looks at this host in this run, and how many failed. Each look is an sshd login there. |
| `stopped_asking` | `true` when the host won't be asked again in this run; `next_look_at` and `stale_after` are `null` then. Only for a refused watcher key, so far: see below. Left out otherwise. **A seat should show it prominently**, not as one more error: nothing about that host will change until someone fixes the key and restarts the watch. |

`result`:

| `result` | Means |
|---|---|
| `ok` | The host answered. `sessions` is the list. |
| `unreachable` | ssh didn't get in: no route, connection refused, timed out, a host key that changed, or a jump host that refused its key. Nothing is known. |
| `error` | The host was reached and gave no list (no `claude`, or one too old for `--json`, or output that isn't one), it refused the key ("not let in"), or it wasn't asked (an unsafe `extra_args`, a missing `agents_identity` file). Nothing is known. |
| `refused` | Production, or reached through production, and not named. Said once, at the start. Never asked. |
| `opted_out` | `agents_watch: false`. Said once, at the start. Never asked. |

Each session has the fields of `meterm agents` (see the README) and of
[agent events](agent-events.md), and two more:

- `needs_input_since`: when the watch raised `needs_input` for it. That's when a look first
  saw it, so the wait began up to an interval earlier.
- `pending: true`: it says `needs_input` but has no process, and has been seen so only once.
  It isn't raised yet: see below.

### `change`, when a session starts or stops waiting

```json
{"schema":1,"type":"change","host":"devbox","at":"2026-11-14T08:00:03Z",
 "change":"needs_input_started","why":"pid","session":{…as in sessions…}}
{"schema":1,"type":"change","host":"devbox","at":"2026-11-14T08:04:05Z",
 "change":"needs_input_ended","why":"changed","session":{…},"waited_s":242}
```

| `change` | `why` | Means |
|---|---|---|
| `needs_input_started` | `pid` | A session with a process says it's waiting for someone. Tell them. |
| `needs_input_started` | `seen_twice` | A session with no process has said so in two answers from the host in a row (a look that fails in between doesn't count). Its `confidence` is `low`. |
| `needs_input_ended` | `changed` | It's still there and says something else now: answered, or it moved on. A row that says something nobody knows (`event: unknown`) doesn't end a wait: it stays raised until the row says something known. |
| `needs_input_ended` | `gone` | It's no longer listed. |

`waited_s` is how long it was raised, give or take an interval. The `change` lines of a look
come just before its `look` line, ends before starts. A host that stops answering doesn't end
anything: its sessions are unknown, not answered, until it answers again.

### `stopped`, last

```json
{"schema":1,"type":"stopped","at":"2026-11-14T09:30:00Z"}
```

If the stream ends without it, the watch was killed: treat every host as unknown.

## Finding the session

A background session has an `id`; `meterm dev HOST -- claude attach <id>` is one way in. An
interactive one has no `id`, and the watch doesn't make one up. It has what Claude gives: its
`kind`, `pid`, `cwd`, `name` and `sessionId`, which is enough to find the tmux window or the
`meterm dev` session it runs in.

## False alarms, and the one this holds back

When a background session's worker dies, Claude Code keeps listing it as `state: blocked`,
with no `pid`, for about 6 to 10 seconds, until its supervisor starts the worker again (seen on
2.1.323). Taken at its word that's `needs_input`, and it's false. So a session with no `pid` that
says `needs_input` is held back: it's `pending` on the first look and raised on the second if it
still says so (`why: seen_twice`, confidence low). Two answers, that is: a look that fails in
between neither counts nor starts the count again. One that was already raised stays raised
through such a look, rather than ending and starting again. A row with a `pid` is raised at once.

Other false alarms aren't held back. Claude also sets `waitingFor` for dialogs a person may
already be looking at, its first prompt if started without one, and an expired login. Each of
those is raised like any wait.

## What it costs

- **On each host, per look:** one sshd login and logout, and one run of `claude agents --json`.
  At the default interval that's 1,440 logins a day per host. Measured on Debian 13: 2 journal
  lines per look that the account itself can read; with the root-side lines (accepted key, PAM
  session open and close, logind) probably 6 to 8, which is not yet measured. A look with no
  terminal isn't recorded in wtmp, so `last`, `wtmpdb` and `meterm logins` don't show looks.
- **Looking changes nothing there**, as far as was measured on Claude Code 2.1.323: no
  supervisor is started by `--json`, and polling every 60 s didn't keep a finished session from
  retiring after 61 minutes.
- **On this machine:** an ssh process per look, nothing kept. Looks are counted in the stream
  (`looks`, `failures`) and nowhere else: they aren't in the activity log, which is permanent
  and shared with the team, and 1,440 entries a day per host would bury it. `meterm exec` stays
  the recorded way to run something on a host.

## What it can't see

- **Windows hosts.** The command needs `sh`, so a Windows host comes back as `error`. Nor does
  `agents` run on a Windows client.
- **An interactive session at the folder-trust question** isn't listed by Claude at all.
- **Whether someone is already looking at a session.** `claude agents --json` doesn't say
  whether one is attached, so a wait in a session in front of you is raised too.
- **Waits shorter than the interval** may come and go between looks and never be seen.
- **Anything Claude doesn't report.** What's listed is what the account's own `claude` says;
  that account can change what it says. It's declared, not observed.
- **Production**, unless named. "No waits" on a stream without production covers only the
  hosts in it.

## A key that can only look

Every look logs in with your key, so whatever runs `meterm agents --watch` as you could log in
with that key in other ways too. That can't be helped on this machine: anything running as you
here, an agent included, can read your keys. The limit has to be on the host's side. A host's
`agents_identity` (on the host, its group or `defaults` in `hosts.yaml`) is a key `agents` and
`agents --watch` log in with instead of `identity_file`, and nothing else uses it:

```sh
meterm keygen --file ~/.ssh/meterm_agents   # no passphrase (a look can't type one), mode
                                           # 0600, and kept out of ssh-agent (below)
# hosts.yaml:  - {name: devbox, address: 192.0.2.10, agents_identity: ~/.ssh/meterm_agents}
meterm agents key devbox            # prints the line; meterm installs nothing
# ...put the line in devbox's authorized_keys, then:
meterm agents key devbox --check    # exit 0: limited. 2: NOT limited. 1: can't tell
```

`meterm agents key` prints one `authorized_keys` line for it, and names the account to put it
in (the one a look logs in as, as `ssh -G` works it out):

```
restrict,no-pty,no-port-forwarding,no-agent-forwarding,no-X11-forwarding,no-user-rc,command="sh -c '…claude agents --json…'" ssh-ed25519 AAAA… meterm-agents-watcher
```

- `command=` is the same fixed command a look sends, so sshd runs it whatever the client asks
  for, and it never reads `SSH_ORIGINAL_COMMAND`: nothing a client sends gets into it.
- `restrict` turns off ptys, forwarding and `~/.ssh/rc`, now and for options sshd adds later.
  An sshd older than 7.2 doesn't know it and refuses the line, and so the key.
- Put it where the account can't change it, such as a root-owned `AuthorizedKeysFile`
  (`/etc/ssh/authorized_keys/%u`). In the account's own `~/.ssh/authorized_keys`, a process
  running as that account can edit the line, though only to do what it could do already.

**It has to be a key of its own.** sshd uses the first `authorized_keys` line a key matches.
If the watcher key is also a key you log in with, either an earlier plain line wins and the
"watcher" key runs anything, or the limited line wins and your own logins are limited. Nothing
shows it, as the forced command is what a look sends anyway. So `agents key` refuses a key
that's also:

- the host's `identity_file`;
- an `IdentityFile` that `ssh -G` names for the host: your ssh config's, or ssh's own defaults
  such as `~/.ssh/id_ed25519`. Paths are expanded as ssh does for `IdentityFile`: `~`,
  `${VAR}`, and `%d %u %h %r %p %l %L %i %k %%`. A path it can't work out (another token, an
  unset variable, `~user`), and a key whose public half it can't read (no `.pub`, and a
  passphrase on the key), are named on stderr as unchecked;
- in ssh-agent (`ssh-add -L`), or in the agent an `IdentityAgent` in your config names, such
  as 1Password's or Secretive's: take it out with `ssh-add -d`. An agent that's there but can't
  be asked is named as unchecked; a locked agent answers as one with no keys, and can't be
  told apart from one.

It gives no line for a host your ssh config reaches through a `ProxyCommand`: no watcher look
could use it (below).

It also refuses a watcher key with a certificate beside it (`KEY-cert.pub`): ssh offers the
certificate with the key on its own, and a host that trusts the certificate's CA lets it in
without reading `authorized_keys`, so without the line's limits.

**Then check it.** `meterm agents key HOST --check` tells you whether the line that's in place
is meterm's. No probe from outside can prove a key is limited (a forced command can take stdin
and say nothing, or run what's sent quietly and then answer as meterm's would; `permitopen`
can refuse only the ports a probe tries), so the answer rests on the line itself:

1. **Probes, with the watcher key alone.** It asks for `echo meterm-watcher-check-<random>`, as
   the command and on stdin; then for a forward both ways: `ssh -W 127.0.0.1:PORT` to the
   host's own sshd, and `ssh -N -R 127.0.0.1:0:127.0.0.1:1 -o ExitOnForwardFailure=yes`. If the
   word comes back, or either forward works (anything through `-W`, the host trying the
   connection for it, or "Allocated port" for `-R`), the key isn't limited: exit 2.
2. **The line, with your ordinary login.** It logs in a second way, as `meterm agents` would
   with no watcher key (your own key; this is a login with it, like any other), runs one fixed
   command that prints `~/.ssh/authorized_keys` and `authorized_keys2` (at most 256 KiB each),
   and finds the lines for the watcher key by key type and blob. Any line for it whose options
   aren't byte for byte the ones `meterm agents key HOST` prints: exit 2, naming the
   difference ("has permitopen=…", "lacks restrict", "a different command").

**Exit 0** means: the installed `authorized_keys` line for this key is exactly meterm's line,
it's the only one, and the probes found no way through. It's the line's `restrict` that turns
off forwarding, a pty, agent and X11 forwarding and `~/.ssh/rc` for the key. sshd-wide settings
aren't read: `Match` blocks, `ForceCommand`, `AuthorizedKeysCommand`, an `AuthorizedKeysFile`
elsewhere.

**Exit 1**, can't tell: the key isn't in those two files (sshd may take keys from an
`AuthorizedKeysFile` elsewhere or an `AuthorizedKeysCommand`, which meterm can't see), it's
there more than once, a file can't be read, the ordinary login fails, the host refuses the
watcher key or doesn't answer, more than 64 KiB of output, ssh stopped by a signal, a forward
that failed without saying why.

Each login is held to `--timeout`. Like a look, the check isn't in the activity log. Run it
after installing the line, and after anything that changes `authorized_keys` or `sshd_config`
there.

**This machine has to be able to use it.** ssh passes over a key it can't open without a
passphrase (a look can't type one) or one that others can read (mode `0644`, `0640`), and the
host then says it refused the login, which would send you to fix the wrong machine. So
`agents key`, `--check` and every look check the key here first, and stop with "this machine
can't use the watcher key: …" without contacting the host. Make it with no passphrase and mode
`0600`; the host is what limits it. A look stopped this way is an `error` and is tried again at
the next look; it never stops the watch asking.

With `agents_identity` set, a look offers that key and no other:

- `-i` that key with `IdentitiesOnly=yes`, in place of the host's `identity_file`. A key
  that `-i`, `-o IdentityFile`, `IdentitiesOnly`, `IdentityAgent` or `CertificateFile` in the
  host's `extra_args` names is dropped for the look.
- The look's ssh config leaves out your own `~/.ssh/config` and `/etc/ssh/ssh_config` for that
  host (`Match originalhost *,!ADDRESS` around the `Include`s), so an `IdentityFile`,
  `IdentityAgent` or `CertificateFile` there isn't added either.
- What they say about the host is otherwise kept. Before each such look, `ssh -G` reads them as
  an ordinary look would, and these are written into the look's config for that host
  (`Match originalhost ADDRESS`): `HostName`, `User`, `Port`, `ProxyJump`,
  `UserKnownHostsFile`, `GlobalKnownHostsFile`, `HostKeyAlias`, `AddressFamily`,
  `BindAddress` and `BindInterface`. What meterm says itself (its user, port, jump hosts) still
  comes first.
- **A host with a `ProxyCommand` isn't looked at.** A look doesn't run one, and going straight
  to the host's `HostName` instead would be another route than yours (an `ssh -W` bastion, a
  SOCKS proxy, cloudflared, SSM, Teleport). The look is an `error` ("the host is reached
  through a ProxyCommand…"); use `ProxyJump` for that host, or take `agents_identity` off it.
- Not kept, and not refused: `LocalCommand` and `KnownHostsCommand` (programs a look doesn't
  run; `PermitLocalCommand` is off for a look anyway), `IdentityAgent` and `CertificateFile`.
  `StrictHostKeyChecking` is always `yes` for a look. Anything else in your config for that
  host doesn't apply to a watcher look.
- A carried value with characters that can't go in a config line as one word fails the look
  (`error`) rather than being changed. `ssh -G` prints a known-hosts path with a space in it
  unquoted, so it's read as two paths; a host key then isn't found, and the look fails.
- ssh-agent is still asked, for that key only, in case it holds it (which `agents key` refuses,
  above).
- If the host refuses the key, the look fails with `error` ("the host refused the watcher
  key"), whichever user ssh logged in as. What `--watch` does next depends on whether the key
  has got in during this run:
  - **Never:** the key may not be set up there. Once it has been refused 3 times in a row over
    at least 5 minutes (with the backoff below, that's the 4th refusal, about 7 minutes after
    the first), it stops asking that host for the rest of the run (`stopped_asking: true`); one
    refusal alone can be a passing thing (a home on NFS, an LDAP hiccup, `authorized_keys`
    being rewritten, a confirm in an agent). Asking on would only pile up failed logins: every try would be a failed login, and enough of those get an
    address banned by tools such as fail2ban. Fix the key, then start the watch again. The
    watch keeps running even when no host is left to ask, so a seat that restarts it whenever
    it ends doesn't bring the failed logins back.
  - **It got in before:** this may pass (a host restarting, `authorized_keys` being edited), so
    it's asked again, backing off like a host that doesn't answer but from at least a minute,
    whatever `--interval` is: 1, 2, 4, 8, then every 10 minutes (plus up to a tenth). That's at
    most 4 tries in any 10 minutes (at 0, 1, 3 and 7 minutes after the first refusal, then the
    next at 15), under fail2ban's default of 5 failures in 10 minutes. One refused try is one
    failed login there: the look offers that one key and nothing else.
  A jump host refusing its own key isn't this; it's `unreachable` and backs off as usual.
- If the file isn't there, or its path has characters that don't show, the host isn't asked
  (`error`), rather than looked at with some other key.
- Jump hosts on the way read your ssh config and log in with your usual keys, as always: the
  watcher key is for the host at the end. A jump host on the very same address as that host
  is the one case where they can't be told apart, and it gets the watcher look's settings
  too.

## Settings

| `hosts.yaml` | Where | Meaning |
|---|---|---|
| `agents_identity: PATH` | host, group, `defaults` | The key `agents` and `agents --watch` use. Kept out of team sharing, like `identity_file`. |
| `agents_watch: false` | host, group | `--watch` doesn't look at the host at all. A single `meterm agents HOST` still does: that's someone asking. |

A meterm older than the one that added these ignores them, and drops them if it saves
`hosts.yaml`. Then, with nothing said: `meterm agents` logs in with your ordinary key again,
and a host with `agents_watch: false` is watched. This meterm keeps no record of what the file
said before, so it can't notice either. After running an older meterm, check that both are
still in `hosts.yaml`.
