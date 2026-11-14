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
- **It stops on SIGINT, SIGTERM or SIGHUP**, ends any look in flight, prints a `stopped` line
  and exits 0. It runs no daemon and leaves no file: each look's ssh config is a temp file
  removed when the look ends. The host list is read once, at the start; restart it to pick up
  changes.

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
| `stale_after` | `next_look_at` plus the timeout and 15 s. With no newer `look` for this host by then, the watch has stopped or is stuck: show what you have as stale, not as current. |
| `looks`, `failures` | Looks at this host in this run, and how many failed. Each look is an sshd login there. |

`result`:

| `result` | Means |
|---|---|
| `ok` | The host answered. `sessions` is the list. |
| `unreachable` | ssh didn't get in: no route, refused, timed out, a key it won't take, a host key that changed. Nothing is known. |
| `error` | The host was reached and gave no list (no `claude`, or one too old for `--json`, or output that isn't one), or it wasn't asked (an unsafe `extra_args`, a missing `agents_identity` file). Nothing is known. |
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
| `needs_input_started` | `seen_twice` | A session with no process has said so on two looks in a row. Its `confidence` is `low`. |
| `needs_input_ended` | `changed` | It's still there and says something else now: answered, or it moved on. |
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
meterm keygen --file ~/.ssh/meterm_agents
# hosts.yaml:  - {name: devbox, address: 192.0.2.10, agents_identity: ~/.ssh/meterm_agents}
meterm agents key devbox        # prints the line; meterm installs nothing
```

`meterm agents key` prints one `authorized_keys` line for it:

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
- It refuses a key that's also the host's `identity_file`: sshd takes the first line a key
  matches, so one key can't be both limited and not.

With `agents_identity` set, a look offers that key first, with `IdentitiesOnly=yes` so the
keys in ssh-agent aren't tried. Keys named in your `~/.ssh/config` or the host's `extra_args`
are still offered after it, so if the host doesn't take the watcher key, the look may get in
with another. If the file isn't there, the host isn't asked (`error`), rather than looked at
with some other key. Jump hosts on the way are logged in to as always, with your usual keys:
the watcher key is for the host at the end.

## Settings

| `hosts.yaml` | Where | Meaning |
|---|---|---|
| `agents_identity: PATH` | host, group, `defaults` | The key `agents` and `agents --watch` use. Kept out of team sharing, like `identity_file`. |
| `agents_watch: false` | host, group | `--watch` doesn't look at the host at all. A single `meterm agents HOST` still does: that's someone asking. |

A meterm older than the one that added these ignores them, and drops them if it saves
`hosts.yaml`.
