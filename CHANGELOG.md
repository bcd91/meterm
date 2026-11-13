# Changelog

TODO: this needs rewriting against v0.23.1 before a release. It was written for the
branch that diverged before v0.21; v0.21-v0.23.1 were merged in since, so some entries may be
released, or differ in what the merge kept.

What changed between releases that you'd notice in use. Newest first.

## Unreleased (after v0.23.1)

### Keys that changed

The picker (`meterm`) and the file manager (`meterm files`) now share one
key map, so some keys moved. `?` in the picker lists these first.

| Where | Was | Now |
|---|---|---|
| picker | `m` connect in tmux | `M` |
| picker | `R` route check | `C` (`R` is now rename) |
| picker | `esc` quit | `esc` clears the filter, then the marks; it never quits (`q` and `ctrl+c` do) |
| picker | `enter` or `y` at a production or delete question | only `y`, and not in the question's first 150ms; `enter` cancels |
| picker | `h` `l` `b` `f` `u` `d` turned the page | `pgup` `pgdn` a page, `ctrl+u` `ctrl+d` half a page (`f` is files, `d` delete) |
| file manager | `R` reload, `r` rename | `r` reload, `R` rename |
| file manager | `a` select all | `*` |
| file manager | `pgup` `pgdn` half a page | a page (`ctrl+u` `ctrl+d` are half a page) |

### Picker

- One line per host, in columns that fit the terminal; the selected host's
  details on the line under the table.
- Health checks go the way `connect` would: over a host's routes in the order
  connect tries them (the one that worked last first), dialling each route's
  first hop — the host, or for a host behind a jump host, the jump host — and
  wanting an SSH greeting. Past the first hop nothing is checked. The line
  under the table says how and when ("up via vpn, 2m ago", "via bastion:
  down: connection refused, 30s ago"). A host whose jump host is down is
  down, not "not checked"; `◌` is only a host meterm won't connect to. Hosts
  are checked again every 5 minutes and when you come back to the picker;
  older marks are dim. `route check`'s "← used" picks the same way.
- A port that takes the connection but sends no SSH greeting within 2s (a
  multiplexer such as sslh, a slow banner) shows `?` and "unsure: port open,
  no SSH greeting in 2s", not `✕` "down: no answer".
- A session whose last command exited non-zero says "session on X ended,
  exit 1", not "failed". ssh's own 255 says the connection was lost or never
  made.
- Deleting a host is in the activity log, as `meterm rm` now is too.
- `dev` and `test` hosts have a badge; an env meterm doesn't know
  ("prodution") is treated as production and flagged as unrecognised, here
  and in `meterm list`.
- A host whose name holds control characters can be renamed, edited and
  removed by the name as shown; `edit` cleans its values. `x` and `R` put
  such a name in the palette Go-quoted (`exec "zw\u200bsp" -- `), so they act
  on the selected host and never on another that shows the same.
- Deleting a jump host, here or with `meterm rm`, lists the hosts that go
  through it, whose jump would then be looked up by ssh itself.
- `tunnel enable` asks before a production host, as `t` does.
- A `y` to the picker's production question is passed to the command it
  starts as a one-use token (a random value in a 0600 file in the picker's
  private temp dir), which the command checks and deletes; its log says
  "production, picker asked". Before, `METERM_FROM_TUI=1` alone made `exec`,
  `cp --sync --delete` and `tunnel enable` skip their question and log
  "production, asked", from any script that set it. Now that variable skips
  nothing. Any process running as you can still forge the token: it stops
  accidents, not someone who means it.
- The detail line says when a host's name has hidden characters, and spells
  it out (`name has hidden characters: "web1\u200b"`).
- While a filter is typed, the footer shows the filter's keys; the detail
  line says "last connected 5m ago".
- `?` lists first the paging keys that changed meaning (`d` deletes, `f`
  opens files).

### Commands

- `meterm activity` gives the detail the rest of the line, never cuts it when
  piped, and wraps it with `--full`. On a terminal HOST is capped (24
  columns, 16 under 120) and a long detail loses its middle: it keeps the
  outcome up to the first `:` or `;` (`exit 1 after 0.2s;`, `failed:`), then
  the start and end of the rest. A cut production note is written short
  (`⚑asked`, `⚑picker`, `⚑--yes`, `⚑no prompt`). A detail left fewer than
  24 cells goes on its own line under its row. TARGET (user@address) is
  shown from 140 columns and with `--full`. `--host` takes the name as
  shown.
- `exec`, `cp`, `connect`, `dev` and `sftp` log a line when they start and
  one with how they ended (exit code, how long); `edit` logs what it changed
  (env production→dev, address, …), `rename` old→new, `rm` the removal.
  `tunnel enable` and `tunnel disable` are logged with how they ended, and
  enable with its production note.
- `exec --all`/`--tag`/`--group`, `cp --sync … --delete` and `tunnel
  enable` ask before production hosts (y only; `enter` cancels). `--yes`
  skips the question and warns. With stdin not a terminal they don't wait:
  they warn on stderr and go on. The log says which: "production, asked",
  "production, picker asked", "production, --yes", "production, no prompt:
  not a terminal". `exec` asks only about production hosts that `--all`,
  `--tag` or `--group` swept in, not ones named.
- `add` and `edit` refuse an address, user or jump that ssh would read as an
  option or split (`--address=-oProxyCommand=…`, `--user=-o…`), and an
  identity file starting with `-`, naming the flag. Before, they were saved
  and only `connect` refused them.
- The activity log masks openssl's `-k` password and `-K` key.
- A name two hosts show as (`zwsp` and `zw\u200bsp`) is an error naming both,
  even when one is called exactly that. Names, notes and tags from this
  machine or a team can no longer hold invisible characters (zero-width,
  format, bidi, soft hyphen, BOM).
- Emoji joined with a zero-width joiner (👨‍👩‍👧) are shown one by one:
  terminals disagree on how wide the joined picture is, and rows overflowed.
- `meterm export` leaves out, and names, any host whose name, address, user,
  identity file or jump ssh_config would split or read as a pattern. This
  fixes an injection: names were written into the ssh_config as they were,
  in v0.20.0 and before, so a host whose name held a line break (typed into
  `hosts.yaml` by hand, say; team copies were already refused) could add a
  ProxyCommand line.
  `--format ansible` leaves out the same hosts: it wrote hidden and control
  characters as they were, and a jump with spaces into
  `ansible_ssh_common_args`.
- `meterm list` spells out a name or jump with a hidden character
  (`"web1\u200b"`), so it can't read as another host's row.
- `exec` labels a host with a hidden character in its name by its exact
  name.
- `meterm list` lines up Thai and emoji names; its JSON (and every other
  command's) spells C1 controls, bidi controls and invisible characters as
  `\u` escapes. `group list` shows group names and an unknown env safely.
- Commands start at once on a terminal that doesn't answer a colour query
  (they waited 5 seconds).

### File manager

What `meterm files` does to the files it touches was reworked; up to
v0.23.1 an overwrite truncated the target and wrote into it.

- **Overwrites are atomic.** A copy is written to a hidden temporary file
  next to the target (`.name.meterm-….part`) and renamed over it only when
  complete; a cancelled or failed copy removes the temporary file and leaves
  the target as it was. A new file never replaces one that appeared since
  the copy was worked out. On a server without `posix-rename` the old file
  is moved aside, the new one moved in, and only then the old one removed;
  if that fails part way, one of the two is always left and the message
  says where. The temporary name cuts the file's name to 64 bytes, so a
  long name (a 78-character Thai one is 234 bytes) can still be
  overwritten, on eCryptfs's 143-byte limit too.
- **No room is not a reason to truncate.** If the temporary file can't be
  made for want of space, inodes or quota, the copy stops with the target
  as it was. If only the rename fails (a bind-mounted file, such as a
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
  connection and quits, saying what may be left behind.
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
- Names from the host are shown with escape sequences, control and invisible
  characters dropped (tabs and line breaks as spaces);
  `?` shows what the last operation did, in full.
