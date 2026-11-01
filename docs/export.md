# Export

What meterm knows, in forms a dashboard or an insights tool can rely on. Read these rather than
meterm's files, which can change. All of them only read. `facts` and the local log use no
network, and `--no-fetch` means none for a team's log.

The rule for every format: new fields can appear at any time, so ignore what you don't know. A
field changing meaning raises `schema`. Every string has control, bidi and other invisible
characters replaced by U+FFFD. It's still text from a log, or from a machine, so show it as text.

These are facts about use, not proof. See [What meterm can't stop](../README.md#what-meterm-cant-stop).

## Activity: `meterm activity --format json -n 0`

An array of events, oldest first. `--host H` keeps one host. `-n` keeps the most recent N, and
the default is 50, so pass `-n 0` for all of it. An array has nowhere to put a `schema`, and its
shape is the one below.

```json
[{"device":"7f3a9c01d2e4b568","seq":12,"time":"2026-10-31T16:30:01.123456Z",
  "user":"alice@laptop","action":"exec","host":"web1","target":"deploy@10.0.0.5",
  "detail":"df (+1 args)","prev_hash":"…","hash":"…",
  "actor_type":"agent","agent_kind":"claude-code","agent_session_id":"sess-…",
  "attribution_source":"declared","prev_ext":"…","ext_hash":"…"},
 {"device":"7f3a9c01d2e4b568","seq":13,"time":"2026-10-31T16:30:02.5Z",
  "user":"alice@laptop","action":"outcome","host":"web1","target":"deploy@10.0.0.5",
  "detail":"seq 12: exit 0, 1.4s","prev_hash":"…","hash":"…",
  "ref_seq":12,"exit_code":0,"duration_ms":1377,
  "actor_type":"agent","agent_kind":"claude-code","agent_session_id":"sess-…",
  "attribution_source":"declared","prev_ext":"…","ext_hash":"…"}]
```

| Field | Type | Meaning |
|---|---|---|
| `device`, `seq` | string, number | Together, unique everywhere. `seq` goes up by one in a device's log. |
| `time` | RFC 3339, UTC | When it was logged. |
| `user` | string | The local account and machine. |
| `action` | string | `connect`, `exec`, `cp`, `sync`, `sftp`, `logs`, `tunnel`, `outcome`, `log-repair` and others. Show one you don't know. |
| `host`, `target` | string | The saved name, and the `user@address` it resolved to. |
| `detail` | string, optional | The program and argument count, or paths. No command text unless `--log-args` was used, and that's redacted. Cut at 4096 characters. |
| `prev_hash`, `hash` | string | The chain every version checks. |
| `ref_seq` | number, optional | On an `outcome`: the `seq` it reports on. |
| `exit_code` | number, optional | On an `outcome`. 255 can be ssh's own failure. **Missing isn't 0**: it timed out, was killed, or failed in a way with no code. |
| `duration_ms` | number, optional | On an `outcome`. |
| `actor_type` | string, optional | `agent` or `unknown`. `human` is reserved and isn't written today. Missing on events from before this field. |
| `agent_kind` | string, optional | `claude-code`, another `AI_AGENT` kind in lowercase letters and hyphens, or `other`. |
| `agent_session_id` | string, optional | The agent's session, if it's a plain short word. It reaches the team's log for shared hosts. `METERM_NO_AGENT_SESSION=1` leaves it out. |
| `attribution_source` | string, optional | `declared`, meaning what the process's environment claims, which anything can set. Never read it as proof. |
| `prev_ext`, `ext_hash` | string, optional | The chain over the newer fields. |

A start with no `outcome` means nobody can say how it ended: it's still running, or it was killed
or the machine stopped. `dev`, `tunnel` and `files` sessions never have one. Don't show that as a
success or a failure.

Hashes are as stored, but strings have been cleaned, so a line with anything to clean won't match
its hash if you recompute it. Ask `--verify`, not the hashes.

### `meterm activity --verify --format json`

```json
{"schema":1,"ok":true,"events":42,"broken":null,"torn_tail":null,"repairs":[],
 "sink":{"kind":"file","target":"/mnt/logs/laptop.jsonl","state":"ok","sent_seq":42,
  "last_ok":"2026-10-31T16:30:02Z","failing_since":null,"last_error":"",
  "copy":"compared","differences":[]}}
```

`broken`, when set, is `{"line","seq","reason"}`. `torn_tail`, when set, is `{"line","bytes"}`: a last line
cut short by a crash, which doesn't affect `ok`. `repairs` holds the `seq` of each `log-repair` event. `sink` is null with none configured. Its `kind` is
`file`, `syslog` or `invalid`, its `state` is `ok`, `behind`, `failing` or `differs`, its `copy` is
`compared`, `not comparable` or `unreadable`, and each difference is `{"seq","reason"}`.

Exit code: 0 all's well, or the sink is just `behind`; 1 the chain doesn't hold, or the log can't be
read; 2 the chain holds, and the sink is `failing` or `differs`. `ok` is about the chain only.

## Team log: `meterm team log --format json --no-fetch -n 0`

An array of the same events, each with `uploader_email`, `uploader_uid`, `verified` and, when that's
false, `integrity_problem`, oldest first, from everyone in the team, for shared hosts only. It uses this machine's clone as it is, with no network, and
fails rather than cloning if there isn't one. So it's what this machine last saw. Without `--no-fetch`
it fetches first. `--host`, `--user` and `--since` also apply. `team audits --no-fetch` is the same for audits.

The team's log is append-only and comes from everyone, so one bad event would otherwise fail this for
the whole team for good. So the exit code is 0 whatever's in it, an event that doesn't check out has
`verified: false`, and stderr says how many. `--format text` and `csv` exit 1 for that, and `team log --verify`
is the gate for a script. An event uploaded by a version that doesn't know the newer fields won't have
them, and that doesn't count against it.

### `meterm team log --verify --format json`

```json
{"schema":1,"ok":true,"events":120,"broken":null,"ext_gaps":0,"device":"7f3a9c01d2e4b568","compared":42,
 "differences":[],"not_in_team_copy":0}
```

`ok` depends on `broken` and `differences`. `ext_gaps` counts places where an event's back-pointer leads to
one with no exit code or actor to check, and this machine's events that have those here and not in the team's
copy: uploaded by an older meterm, or stripped. `compared` is how many of the
team's events came from this machine, and `differences` is where this machine's log has lost or changed them,
each `{"seq","reason"}`. `not_in_team_copy` counts events here, on hosts shared now, that the upload has gone past
and the team hasn't got: from before a host was shared, or removed there. With `--no-fetch` it's compared with
this machine's clone, which is no harder to change than the log.

## Facts: `meterm facts --format json`

History, routes and what's known of each server. It reads only, uses no network, starts no sync and picks
no route. Identity files, extra args, notes and team URLs stay out.

```json
{"schema":1,"generated":"2026-10-31T16:30:00Z",
 "sources":{"history":"ok","routes":"ok","server_os":"missing"},
 "hosts":[{"name":"web1","target":"deploy@10.0.0.5","port":22,"group":"prod","tags":["web"],
  "env":"production","production":true,"touches_production":true,"team_shared":false,
  "proxy_jump":"bastion",
  "routes":[{"name":"vpn","address":"100.64.0.5","port":0,"proxy_jump":""}],
  "last_route":"vpn","last_connected":"2026-10-30T09:00:00Z","connections":17,
  "server":{"software":"OpenSSH_9.6","windows":false,"checked":"2026-10-01T00:00:00Z","stale":false}}]}
```

| Field | Meaning |
|---|---|
| `sources.*` | `ok`, `missing` or `unreadable`, for each file behind this. Without the file, "no connections" means "can't say". |
| `target`, `port` | The host's own address. A route may be what's actually used. |
| `env` | `production`, `staging`, `dev`, `test`, or empty when unset, which says nothing. |
| `production` | This host is production. |
| `touches_production` | Reaching it goes to or through production, by any route. |
| `routes[].port` | 0: the host's own. An empty `address` or `proxy_jump` is explained in the README's routes section. |
| `last_route` | The route that worked last, or empty. |
| `last_connected`, `connections` | Null and 0 with no connection on record here. |
| `server` | Null until meterm has had reason to look. `software` is what the server announced. `stale` says the address or route has changed since. |

## Looks at a host

These go to a host, and each is a look: one fixed read-only command, production refused unless
named, `extra_args` on an allowlist, a Windows client refused, nothing in the activity log. Each shows
in that host's sshd log instead. Exit codes: 0 every host answered, 1 nothing was asked, 2 the report is
on stdout and at least one host has an `error`. `error` is in meterm's words, never the host's.

### `meterm logins <host> --format json`

```json
{"schema":1,"hosts":[{"host":"web1","reachable":true,"error":"",
  "since":"2026-10-01T00:00:01Z",
  "logins":[{"time":"2026-10-31T16:30:01Z","end":null,"user":"deploy","tty":"pts/0",
    "source":"192.0.2.9","still_logged_in":true,"matched":true,
    "event":{"device":"7f3a9c01d2e4b568","seq":12,"action":"connect","time":"2026-10-31T16:30:00Z"}}],
  "unparsed":0,"truncated":false}]}
```

`logins` is null when `error` is set: a host that can't say isn't one nobody logged in to. `since` is how
far back wtmp goes. `source` is an address, or empty for a local login. `end` is null while logged in
or when `last` doesn't say. `unparsed` counts lines that weren't the known shape. `truncated` says it stopped at
500 records. `matched` says this machine's log has an event for that host and account within two minutes.

**Limits.** wtmp mostly holds sessions with a terminal, so `ssh host command`, `scp` and `meterm exec` often
aren't there. Those, and how someone logged in, are in `auth.log`, which takes root. No match doesn't mean an
intruder, as it could be someone else's meterm or a plain `ssh`. A match doesn't prove it was that event. It
was tested against a stand-in for `last` written from util-linux's format, and macOS and BSD `last` don't
take these flags and return an error.

### `meterm auth-methods <host> --format json`

```json
{"schema":1,"hosts":[{"host":"web1","reachable":true,"error":"",
  "methods":["publickey","password"],"password_login":true,
  "keyboard_interactive":false,"none_accepted":false}]}
```

`methods` is null when `error` is set. The other three are null when it can't be said, and **null isn't
false**: a jump host refusing tells nothing of the host behind it, nor does anything said before the host key was
checked, or a disconnect. `password_login` is also null if
the server's list had a name that couldn't be read. `keyboard_interactive` usually means passwords too.
`none_accepted` says it let this connection in with no authentication at all, in which case `true` ran there.
This is the server's own statement of what's on offer to this account from this address. No key or password is
offered to it. A jump host on the way is logged in to as usual.

## Agents: `meterm agents --format json`

See [agent-events.md](agent-events.md).
