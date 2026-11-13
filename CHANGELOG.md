# Changelog

What changed between releases that you'd notice in use. Newest first.

## Unreleased (after v0.23.1)

### Keys that moved

The picker (`meterm`) and the file manager (`meterm files`) share one key
map now, so some keys moved. `?` in the picker lists the picker's first.

| Where | In v0.23.1 | Now |
|---|---|---|
| picker | `h` `l` `b` `u` `←` `→` turned the page | they don't; `pgup` `pgdn` a page, `ctrl+u` `ctrl+d` half a page |
| picker | `m` connect in tmux | `M` |
| picker | `R` route check | `C`; `R` renames the host |
| picker | `esc` dropped the marks, then quit | `esc` clears the filter, then the marks, and never quits (`q`, `ctrl+c` do) |
| picker | `y` at the delete question deleted at once | `y` counts once the question has been up 150ms |
| file manager | `R` reload, `r` rename | `r` reload, `R` rename |
| file manager | `a` select all | `*` |
| file manager | `pgup` `pgdn` half a page | a page; `ctrl+u` `ctrl+d` half a page |

### Picker

- A table, one line per host, in columns that fit the terminal (name, env,
  address, group, tags, last connected), dropping the least useful columns
  first on a narrow one; the selected host's details on the line under it.
  `s` cycles the order: recent, name, group, env. Under 40×10 it says the
  terminal is too small, and any key pressed then (`y` included) cancels a
  question that was open.
- Health checks follow the routes `connect` tries, where v0.23.1 dialled
  the saved address and port and didn't check a host behind a jump host at
  all: each of a host's routes in the order connect tries them (the one
  that worked last first), dialling the first hop and wanting an SSH
  greeting. They dial the saved address themselves, not through `ssh`, so
  ssh_config plays no part: a name only a `Host` alias resolves, or a
  `ProxyCommand` or `ProxyJump` set there, isn't followed, and such a host
  can show down while `connect` works. `●` up; `◐` the jump host answered, the host behind it isn't
  checked; `✕` down; `?` checking, or the port took the connection but no
  SSH greeting came within 2s (sslh, a slow server); `◌` not checked (a
  host meterm won't connect to as it stands). The line under the table
  says which way and when ("up via vpn, 2m ago", "via bastion: down:
  connection refused, 30s ago"). Hosts are checked again every 5 minutes
  and when you come back to the picker; older marks are dim. `route check`
  marks the route connect would use the same way.
- When a command started from the picker ends, the footer says how: a
  session whose last command exited non-zero ("session on X ended, exit 1"),
  ssh's own 255 ("connection lost, or never made"), a failure.
- An ENV column: `⚑prod`, `⚑staging`, `dev`, `test`, and "env not set"
  under the table. An env meterm doesn't know ("prodution") counts as
  production, shows `⚑prod?`, and is flagged as unrecognised, here and in
  `meterm list`.
- A name with hidden characters (`zw\u200bsp`) is spelled out and quoted in
  its row, so it can't pass for the plain one, and the line under the table
  says so. A name with a word that mixes Latin, Cyrillic or Greek letters
  is flagged there too. `x`, `R` and `L` put a name with hidden characters, spaces or
  quotes in the palette as a reference,
  `‹zw\u200bsp›`, that stands for exactly that host. Quotes you type in the
  palette are taken as typed, as in v0.23.1.
- A host whose group no longer exists says so under the table.
- Deleting a jump host lists the hosts that go through it.
- `?` lists the keys that moved first, every key, the marks, and the last
  message in full.
- The picker's private temporary directory (`meterm-tui-<pid>-…`) is
  removed when its terminal closes (SIGHUP) or it's sent SIGTERM, not only
  when it quits. One left by a picker that was killed outright is removed
  by the next picker, once that PID isn't running and the directory holds
  only what a picker puts there (not on Windows).

### Safety prompts

v0.23.1 asked about nothing but deleting a host in the picker.

- The picker asks before anything that runs on a production host, or on a
  host reached through one (a production jump host): connect, a login,
  exec, a tunnel, logs, files, audit and the rest. A tunnel whose name or
  note says it leads to production asks too. Only `y` (or `Y`) goes ahead,
  and only once the question has been up 150ms; a key typed sooner is
  ignored, and after that `enter` cancels, like any other key.
  The question shows the command it would run.
- On the command line, `exec --all`/`--tag`/`--group` asks about the
  production hosts it swept in (hosts named outright aren't asked about
  again), and so do `cp --sync … --delete` and `tunnel enable` on
  production. Only `y` (or `Y`) goes ahead; `--yes` goes ahead without asking and
  warns. With stdin not a terminal `exec` and `cp` warn on stderr and go
  on; `tunnel enable`, which leaves a tunnel open from every login, refuses
  without `--yes`, as `group rm` does. `tunnel enable` on a host that isn't
  production, for a tunnel that leads there, says the tunnel leads to
  production.
- The picker's `y` reaches the command it starts as a one-use token: a
  random value in a 0600 file in the picker's private temp directory, which
  the command checks and deletes before anything else runs. An environment
  variable alone (`METERM_FROM_TUI`) never skips a question. Any process
  running as you can still forge the token: it stops accidents, not someone
  who means it.
- `meterm group rm` asks when removing the group changes hosts' env (a
  group that made them production, say), listing the hosts and the change;
  `--yes` skips the question, and without a terminal it refuses, naming
  the hosts and the change. Before, it took production away silently.
- **Answering no exits 1.** Every question answered anything but yes ends
  the command with `meterm: cancelled: …` on stderr (`web1 not removed`,
  `nothing ran`, `tunnel not enabled`) and exit status 1: `rm` (which
  exited 0 in v0.23.1), `group rm`, `exec` and `cp` on production,
  `tunnel enable`, `files`, `keys revoke`, `team push`, `team approve`,
  `team remove`, and the relay commands. A script that relied on exit 0
  after "no" must check for 1. From the picker, it shows as cancelled, not
  failed.

### Evidence and activity

- Every connect, exec, cp, files, sftp, logs, dev and tunnel start on a
  production host says how it went ahead, first in its
  detail: `(production, asked)`, `(production, picker asked)`,
  `(production, --yes)`, `(production, no prompt: not a terminal)`,
  `(production, named on the command line)` or `(production, picker didn't
  ask)`; `(via production, …)` for a host behind a production jump host and
  `(tunnel to production, …)` for a tunnel that leads there.
- New in the log: `dev` sessions and `tunnel enable`/`disable` with how
  they ended; `edit` with what it changed (env production→dev, address…),
  `rename` old→new, `rm` and the picker's delete; a group's env change on
  each host it reaches.
- `exec` with one argument logs `df (shell line)` instead of counting the
  line's words as arguments.
- Redaction also masks openssl's `-k` password and `-K` key, `vault login`
  tokens, and HashiCorp Vault tokens by their shape (`hvs.…`).
- `meterm activity` on a terminal gives the detail the rest of the line:
  HOST is capped, a long detail loses its middle (keeping how it ended, a
  file manager's word for the file, such as `unchanged`, and never half a
  `[REDACTED]`), a row too wide for a narrow terminal has ACTOR, USER, HOST
  and ACTION cut to fit, production notes are shortened (`⚑picker`,
  `⚑named`, `⚑via picker`), and TARGET shows from 140 columns. `--full`
  wraps each detail whole; piped output is never cut. `--host` takes the
  name as shown.

### Export

- `meterm export` (ssh_config and `--format ansible`) also leaves out a host
  whose name holds a character that draws as nothing (Hangul fillers, the
  combining grapheme joiner and the like), as it already did for control
  and format characters.
- `meterm import` skips, and names, a host whose fields hold control or
  invisible characters.

### CLI

- A host can be named as it's shown when its saved name has hidden
  characters; a name two hosts show as is an error naming both.
- `add`, `edit`, `rename` and `import` refuse control and invisible
  characters (zero-width space, joiner and non-joiner, bidi controls, soft
  hyphen, BOM) in names, addresses, users, groups, tags and jump hosts.
  Notes and tunnel notes may hold the zero-width joiner and non-joiner,
  which emoji (`👨‍💻`, `🏳️‍🌈`) and Persian need; every other control or
  invisible character is refused there too. A host name, group, route or
  tunnel name is refused when one word in it mixes Latin with Cyrillic or
  Greek letters (`pаypal` with a Cyrillic `а`); words are split at `-`,
  `_`, `.`, digits and anything else that isn't a letter, so `db-москва`
  and `api-μ` are fine. `add`, `edit`, `rename`, `group add`, `tunnel add`
  and `import` apply this; a look-alike name already saved still works and
  can still be edited.
- **Team pulls keep and flag, and never drop a host silently.** A team host
  that v0.23.1 accepted but these rules refuse (an invisible character, a
  word mixing alphabets) is still pulled and kept, and the pull prints a
  warning naming the host and the field, as a host saved locally with one still
  loads; fix it with `meterm edit`. `team push`, `team approve` and `team
  resolve --theirs` take such a host too, so a member can still sync a host
  someone else created. A team host with what v0.23.1 refused as well
  (control characters such as escape sequences and line breaks, bidi
  controls, ssh options that run commands) is not applied: `team pull` and
  `team sync` report it as `rejected`, `team status` lists it, `list` and the
  picker count it in the team reminder ("1 refused"), and the command
  exits non-zero. Before, a rejected host was only reported and the
  command exited 0.
- `add` and `edit` refuse an address, user or jump that ssh would read as an
  option or split (`--address=-oProxyCommand=…`), and an identity file
  starting with `-`, naming the flag. Before, only `connect` refused them.
- `meterm list` spells out a name or jump with a hidden character, lines up
  Thai and emoji names, and shows `pay (no such group)` (`group_missing` in
  JSON) for a host whose group was removed. JSON output spells C1 and bidi
  controls and invisible characters as `\u` escapes.
- `meterm --config <file> connect <host>` (and `--debug`, or either right
  after `connect`) works; it looked for a host called `--config`.
- Commands other than the picker start at once on a terminal that doesn't
  answer a colour query; they waited 5 seconds. The picker itself still
  waits up to 5 seconds there unless `$COLORFGBG` is set.
- Team commands run quickly one after another no longer collide with the
  background sync on the team's clone ("cannot lock ref", "index.lock: File
  exists"): git work on a clone is one at a time, and a command that can't
  get it within 10s says "team sync busy … retry" (not "you are not in a
  team called …"), and `team list` shows that team as "(busy, try again)"
  and exits non-zero rather than leaving it out. A git lock file in the
  clone that is older than 30s (by its time) is removed, saying "removed a
  git lock older than 30s: <path>"; a newer one may be a live git meterm didn't start, and
  is left. A git of your own in that clone that holds a lock longer (a
  `git commit` waiting in its editor) would lose it.

### File manager

What `meterm files` does to the files it touches was reworked; up to
v0.23.1 an overwrite truncated the target and wrote into it.

- **Overwrites are atomic.** A copy is written to a hidden temporary file
  next to the target (`.name.meterm-….part`) and renamed over it only when
  complete; a cancelled or failed copy removes the temporary file and leaves
  the target as it was. On a host the temporary file is made inside a
  private directory of its own next to the target (`.meterm-…/`, made
  `0700` and checked first, removed after): SFTP can't give a file its mode
  as it creates it, so it starts `0666` less the server's umask, and loose
  in the target's directory anyone who could read that directory could have
  opened it before its mode was set and read the new contents. A directory
  that can't be made private, where a new file would start more open than
  the copy, counts as a temporary file that can't be made (asked about or
  stopped, below). A new file doesn't replace one that appeared since the
  copy was worked out: refused in one step on Linux and by OpenSSH's
  server, elsewhere checked just before the rename. On a server without `posix-rename` the old file
  is moved aside, the new one moved in, and only then the old one removed;
  if that fails part way, one of the two is always left and the message
  says where. The temporary name cuts the file's name to 64 bytes, so a
  long name (a 78-character Thai one is 234 bytes) can still be
  overwritten, on eCryptfs's 143-byte limit too.
- **No room is not a reason to truncate.** On this machine, if the
  temporary file can't be made for want of space, inodes or quota, the copy
  stops with the target as it was; on a host, where SFTP says only
  "failure", it asks first (below). If only the rename fails (a bind-mounted file, such as a
  container's `/etc/hosts`), the target is untouched and is written in
  place, as v0.23.1 did, and the copy says "couldn't replace … in one
  step; wrote it in place".
- **The temporary file is only touched through its handle.** Owner and
  mode are set and read back on the open file, never by name, and it isn't
  renamed into place unless its name still leads to it (the same inode
  here; a regular file of the size written on a host): in a directory
  others can write, a swapped-in symlink isn't followed or moved over the
  target.
- **What an overwrite keeps:** the old file's permission bits, setuid,
  setgid and sticky (set only on the finished copy, just before the rename),
  and its owner and group where they can be given (as root, or a group
  you're in). A new file is never more open than the one it replaces: if the
  host won't set permissions, the old file is left. A file we may not write
  is never renamed over: it's written in place, which a read-only file
  refuses, changing nothing. On a host uid 0 isn't taken for root, since a
  server that sends no owners shows 0 for everyone. It's a new file, so ACLs and extended
  attributes are not kept, and other hard links keep the old content. Hard
  links are only seen on this machine; SFTP doesn't report them, so on a
  host they split unasked.
- **Questions before losing something.** When owner or group can't be
  kept, or a file has other hard links, it asks once per copy: write in
  place (keeps them, not atomic), replace anyway, or skip. Where no
  temporary file can be made (a read-only directory, or on this machine a
  read-only filesystem with the file bind-mounted into it) it asks before
  writing in place. A host that only says "failure" (read-only, full disk
  and quota look the same over SFTP) is asked about too, the question
  saying that on a full disk the file may be left partly written. A write in place that fails or is cancelled says the
  file may be partly written.
- **A stop says what it left.** A cancel or failure names the file it was
  writing as unchanged, not created, or partly written; a temporary file
  that couldn't be removed (the connection went) is named, as v0.23.1 named
  an unfinished new file it couldn't remove; a copy that stops part way
  says which files it had already copied. A listing that fails after it no
  longer hides those words.
- **A file never replaces a directory, nor a directory a file.** That's
  refused when the copy is worked out, with nothing copied and nothing
  asked. Before, on a server without `posix-rename`, the directory was moved
  aside to `.name.meterm-….old` and the file put in its place.
- **Symlinks are followed visibly.** An overwrite through a link writes the
  file it leads to and keeps the link; the question says so (`viaimg (->
  linktarget, the file it points to will be overwritten)`) and so does the
  status after. A link that leads nowhere is not replaced.
- **Questions act on what they named.** A delete, rename, mkdir or overwrite
  question keeps the directory and paths it was opened on, and says which
  directory; if a listing of another directory lands while it's open, it's
  cancelled ("cancelled: the directory changed while asking") and keys
  wait for enter or esc, so an answer already typed doesn't run as a
  command. Before, `h` then `d` on a slow host could delete the same name
  in the parent.
- **Rename** refuses a name that's taken, atomically where the server can;
  enter on a box you didn't change renames nothing; a name with hidden
  characters starts the box empty, saying so.
- **Production.** `meterm files` on a production host asks first (`--yes`
  skips; with stdin not a terminal it warns and goes on). From the picker
  it isn't asked twice, but only on the picker's one-time token, not on an
  environment variable anything could set. The host's pane is marked
  `⚑PROD` and its delete and overwrite questions say PRODUCTION. The log's
  `files` line says how it went ahead: `(production, asked)`, `(production,
  picker asked)`, `(production, --yes)` or `(production, no prompt: not a
  terminal)`.
- **A host that stops answering** no longer freezes it: the progress line
  says "no data for 12s", `esc` cancels, and `esc` again drops the
  connection and quits, saying what may be left behind (a copy writing a
  file in place first lets that file finish, so it takes one `esc` more).
- **The activity log** has each copy, delete, rename and new directory on
  the host as a start, written before anything is touched and carrying the
  production note, and an `outcome` event pointing back at it (`ref_seq`,
  `exit_code` 0 when it did what was asked, otherwise no exit code and its
  word: `failed`, `partial`, `cancelled`, `stopped`, `aborted`), the same
  model as `connect`, `exec` and `cp`, so `meterm activity` shows EXIT and
  TOOK on the operation's own row. What only the end knows (written in
  place, an error, what went before a failed delete, what was copied before
  a stop, a temporary file left behind) and the answers to questions asked
  part way are events of their own, each leading with how it went
  (`failed:`, `partial:`, `asked:` …). Deleting something already gone says
  so. A copy whose only file was skipped now logs its end too.
- A name from the host with an escape sequence, control or invisible
  character in it is shown quoted with each such character spelled out
  (`"ev\x1b]0;x\a name\u202etxt.exe"`), as the picker shows such a host
  name: in its row, the delete question, the pane's path, rename, mkdir,
  copy and delete messages, `?`, and the activity log, which keeps the exact name rather than `�`. A clean
  name is shown as it is. `?` shows what the last operation did, in full.
