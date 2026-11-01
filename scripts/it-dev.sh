#!/usr/bin/env bash
# Integration test for `meterm dev` against a real sshd and tmux. Everything
# runs from a temp directory: its own sshd on a loopback port, its own keys, and
# its own tmux sockets. It needs no root and leaves the machine's ssh alone.
# Needs: sshd, tmux, python3, curl, jq.
set -euo pipefail

# Short and without symlinks: macOS's own temp path is long enough to take a ControlPath
# past its 104 bytes, and mktemp -d alone goes there whatever TMPDIR says.
W=$(mktemp -d /tmp/mtd.XXXXXX)
W=$(cd "$W" && pwd -P)
SSHD_PID=""
HTTP_PID=""
cleanup() {
  trap - EXIT
  tmux -L it-outer kill-server 2>/dev/null || true
  TMUX_TMPDIR=$W/remote tmux kill-server 2>/dev/null || true
  [ -z "$HTTP_PID" ] || kill "$HTTP_PID" 2>/dev/null || true
  [ -z "$SSHD_PID" ] || kill "$SSHD_PID" 2>/dev/null || true
  [ -z "${SILENT_PID:-}" ] || kill "$SILENT_PID" 2>/dev/null || true
  [ -z "${BAD_PID:-}" ] || kill "$BAD_PID" 2>/dev/null || true
  [ -z "${NT_PID:-}" ] || kill "$NT_PID" 2>/dev/null || true
  [ -z "${NT2_PID:-}" ] || kill "$NT2_PID" 2>/dev/null || true
  # Whatever still names this directory, on any way out: an sshd started partway through, a
  # shared connection (ssh: $W/ctl/... [mux]) outliving the session, a tail -f it was running.
  # The name is this run's own, so nothing else can match.
  local p
  for p in $(pgrep -f "$W/" 2>/dev/null); do
    [ "$p" = "$$" ] || kill "$p" 2>/dev/null || true
  done
  rm -rf "$W"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP
fail() {
  echo "FAIL: $*"
  echo "--- outer pane"; tmux -L it-outer capture-pane -p -t it 2>/dev/null || true
  echo "--- sshd log"; tail -n 20 "$W/sshd.log" 2>/dev/null || true
  exit 1
}
wait_for() { # <description> <command...>
  local what=$1 i; shift
  for i in $(seq 1 100); do "$@" >/dev/null 2>&1 && return 0; sleep 0.2; done
  fail "timed out waiting for $what"
}
free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }

export METERM_CONFIG=$W/hosts.yaml METERM_HISTORY=$W/history.json METERM_ACTIVITY_DIR=$W/activity METERM_NO_AUTOSYNC=1 METERM_TEAM_DIR=$W/team
export METERM_RUNTIME_DIR=$W/run
export TMUX_TMPDIR=$W/outer
mkdir -p "$W/outer" "$W/remote" "$W/proj" "$W/bin" "$W/ctl"
go build -o "$W/bin/meterm" ./cmd/meterm

PORT=$(free_port)
WEB_REMOTE=$(free_port)
WEB_LOCAL=$(free_port)
ssh-keygen -q -t ed25519 -N '' -f "$W/hostkey"
ssh-keygen -q -t ed25519 -N '' -f "$W/key"
cp "$W/key.pub" "$W/authorized_keys"; chmod 600 "$W/authorized_keys"
cat > "$W/sshd_config" <<EOF
Port $PORT
ListenAddress 127.0.0.1
HostKey $W/hostkey
AuthorizedKeysFile $W/authorized_keys
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
StrictModes no
PidFile $W/sshd.pid
SetEnv TMUX_TMPDIR=$W/remote PATH=$PATH
EOF
/usr/sbin/sshd -D -f "$W/sshd_config" -E "$W/sshd.log" &
SSHD_PID=$!

# meterm runs the ssh on PATH. This one keeps known_hosts out of the real home
# and turns on connection sharing, which `meterm dev` has to stay clear of.
cat > "$W/ssh_config" <<EOF
Host *
  StrictHostKeyChecking no
  UserKnownHostsFile $W/known_hosts
  ControlMaster auto
  ControlPath $W/ctl/%C
  ControlPersist 60
EOF
cat > "$W/bin/ssh" <<EOF
#!/bin/sh
exec /usr/bin/ssh -F "\${IT_SSH_CONFIG:-$W/ssh_config}" "\$@"
EOF
chmod +x "$W/bin/ssh"
export PATH=$W/bin:$PATH

meterm add box --address 127.0.0.1 --port "$PORT" --user "$(id -un)" --identity "$W/key" >/dev/null
meterm tunnel add box web -L "$WEB_LOCAL:localhost:$WEB_REMOTE" >/dev/null 2>&1

(cd "$W/proj" && exec python3 -m http.server --bind 127.0.0.1 "$WEB_REMOTE" >/dev/null 2>&1) &
HTTP_PID=$!
wait_for 'sshd' ssh -i "$W/key" -p "$PORT" -o BatchMode=yes -o ControlPath=none 127.0.0.1 true

remote_tmux() { TMUX_TMPDIR=$W/remote tmux "$@"; }
# The terminal is an outer tmux pane, so meterm and ssh see a real tty.
outer() { tmux -L it-outer -f /dev/null "$@"; }
run_dev() {
  outer new-session -d -s it -x 120 -y 30 "$W/bin/meterm dev $*; echo \$? > $W/rc; sleep 60"
}
rc_is() { [ "$(cat "$W/rc" 2>/dev/null)" = "$1" ]; }
stop_outer() { outer kill-server 2>/dev/null || true; rm -f "$W/rc"; sleep 0.3; }

echo '1. creates the session in the directory, named after it, with the tunnel up'
run_dev "box:$W/proj --tunnel web"
wait_for 'session proj' remote_tmux has-session -t =proj
wait_for 'tunnel' curl -fsS -o /dev/null "http://127.0.0.1:$WEB_LOCAL/"
in_project() { [ "$(remote_tmux display-message -p -t =proj: '#{pane_current_path}')" = "$(cd "$W/proj" && pwd -P)" ]; }
wait_for 'session in the project dir' in_project
[ "$(meterm activity --host box --format json | grep -c '"dev proj"')" -ge 1 ] || fail 'activity not recorded'
status_is() { # <kind> <name> <state>
  meterm status --format json | jq -e --arg k "$1" --arg n "$2" --arg s "$3" \
    'any(.entries[]; .kind == $k and .name == $n and .state == $s and .host == "box" and .route == "default")' >/dev/null
}
wait_for 'status: session connected' status_is session proj connected
wait_for 'status: tunnel connected' status_is tunnel web connected

echo '2. a dropped connection comes back to the same session, tunnel included'
remote_tmux set-environment -t =proj -g IT_ALIVE 1
PANE=$(remote_tmux display-message -p -t =proj: '#{pane_id}')
sleep 1
pkill -P "$SSHD_PID" || fail 'nothing to drop'
wait_for 'reconnect notice' sh -c "tmux -L it-outer capture-pane -p -t it | grep -q 'lost; reconnecting'"
meterm status | grep -qE 'proj +(reconnecting|connected)' || fail 'status lost the session during the drop'
wait_for 'client re-attached' sh -c "[ \"\$(TMUX_TMPDIR=$W/remote tmux display-message -p -t =proj: '#{session_attached}')\" = 1 ]"
[ "$(remote_tmux display-message -p -t =proj: '#{pane_id}')" = "$PANE" ] || fail 'a different pane after reconnecting'
[ "$(remote_tmux list-sessions | wc -l | tr -d " ")" = 1 ] || fail 'reconnecting made another session'
wait_for 'tunnel after drop' curl -fsS -o /dev/null "http://127.0.0.1:$WEB_LOCAL/"
wait_for 'status: back to connected' status_is session proj connected

echo '3. detaching ends it, without retrying and with the tunnel closed'
outer send-keys -t it C-b d
wait_for 'exit after detach' rc_is 0
curl -fsS -o /dev/null --max-time 2 "http://127.0.0.1:$WEB_LOCAL/" 2>/dev/null && fail 'tunnel still open after detach'
stop_outer

echo '4. the same command lands in the same session'
run_dev "box:$W/proj"
wait_for 'attach' sh -c "[ \"\$(TMUX_TMPDIR=$W/remote tmux display-message -p -t =proj: '#{session_attached}')\" = 1 ]"
[ "$(remote_tmux list-sessions | wc -l | tr -d " ")" = 1 ] || fail 'second run made another session'
stop_outer

echo '5. a missing directory says so and does not retry'
run_dev "box:$W/nope"
wait_for 'exit 3' rc_is 3
outer capture-pane -p -t it | grep -q 'no directory' || fail 'no message about the directory'
stop_outer

echo '6. a host that cannot be reached fails at once'
meterm add dead --address 127.0.0.1 --port "$(free_port)" --user "$(id -un)" --identity "$W/key" >/dev/null
run_dev dead
wait_for 'exit 255' rc_is 255
stop_outer

echo '7. --no-reconnect stops at the first drop'
run_dev "box:$W/proj --no-reconnect"
wait_for 'attach' sh -c "[ \"\$(TMUX_TMPDIR=$W/remote tmux display-message -p -t =proj: '#{session_attached}')\" = 1 ]"
sleep 1
pkill -P "$SSHD_PID" || fail 'nothing to drop'
wait_for 'exit 255' rc_is 255
stop_outer

echo '8. `meterm tunnel up` stays up with a shared connection already open'
ssh -i "$W/key" -p "$PORT" -o BatchMode=yes -fN 127.0.0.1
outer new-session -d -s it -x 120 -y 30 "$W/bin/meterm tunnel up box/web; echo \$? > $W/rc; sleep 60"
wait_for 'tunnel' curl -fsS -o /dev/null "http://127.0.0.1:$WEB_LOCAL/"
sleep 1
[ ! -e "$W/rc" ] || fail 'tunnel up exited behind a ControlMaster'
stop_outer

echo '9. a host that accepts and never answers fails once, without calling it lost'
SILENT_PORT=$(free_port)
python3 -c 'import socket,sys
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("127.0.0.1",int(sys.argv[1]))); s.listen(5)
keep=[]
while True: keep.append(s.accept())' "$SILENT_PORT" &
SILENT_PID=$!
meterm add silent --address 127.0.0.1 --port "$SILENT_PORT" --user "$(id -un)" --identity "$W/key" >/dev/null
run_dev silent
wait_for 'exit 255 after ConnectTimeout' rc_is 255
kill "$SILENT_PID" 2>/dev/null || true
outer capture-pane -p -t it | grep -q 'reconnecting\|trying again' && fail 'retried a host it never got into'
stop_outer

echo '10. a key revoked during the session stops the retries and says why'
run_dev "box:$W/proj"
wait_for 'attach' sh -c "[ \"\$(TMUX_TMPDIR=$W/remote tmux display-message -p -t =proj: '#{session_attached}')\" = 1 ]"
cp "$W/authorized_keys" "$W/authorized_keys.bak"
: > "$W/authorized_keys"
pkill -P "$SSHD_PID" || fail 'nothing to drop'
wait_for 'exit 255' rc_is 255
outer capture-pane -p -t it | grep -q 'giving up on box: it refused the key' || fail 'no word about the revoked key'
cp "$W/authorized_keys.bak" "$W/authorized_keys"
stop_outer

echo '11. a drop while the host is down keeps trying until it comes back'
run_dev "box:$W/proj"
wait_for 'attach' sh -c "[ \"\$(TMUX_TMPDIR=$W/remote tmux display-message -p -t =proj: '#{session_attached}')\" = 1 ]"
OLD_SSHD=$SSHD_PID; SSHD_PID=""
kill -STOP "$OLD_SSHD"              # no new connections while its children go
pkill -P "$OLD_SSHD" || fail 'nothing to drop'
kill -CONT "$OLD_SSHD"; kill "$OLD_SSHD"
wait_for 'still trying' sh -c "tmux -L it-outer capture-pane -p -t it | grep -q 'still no connection to box'"
/usr/sbin/sshd -D -f "$W/sshd_config" -E "$W/sshd.log" &
SSHD_PID=$!
for _ in $(seq 1 200); do
  [ "$(remote_tmux display-message -p -t =proj: '#{session_attached}' 2>/dev/null)" = 1 ] && break
  sleep 0.2
done
[ "$(remote_tmux display-message -p -t =proj: '#{session_attached}')" = 1 ] || fail 'did not come back'
[ ! -e "$W/rc" ] || fail 'gave up while the host was down'
stop_outer

echo '12. directories with the same name get sessions of their own'
stop_outer
remote_tmux kill-server 2>/dev/null || true
sleep 0.3
mkdir -p "$W/client-a/api" "$W/client-b/api"
attached() { [ "$(remote_tmux list-clients | wc -l | tr -d " ")" = 1 ]; }
current() { remote_tmux list-clients -F '#{session_name}' | head -1; }
real() { (cd "$1" && pwd -P); }
run_dev "box:$W/client-a/api"; wait_for 'first session' attached
[ "$(current)" = api ] || fail "first directory got $(current), want api"
stop_outer; wait_for 'detached' sh -c "[ \"\$(TMUX_TMPDIR=$W/remote tmux list-clients | wc -l | tr -d ' ')\" = 0 ]"
run_dev "box:$W/client-b/api"; wait_for 'second session' attached
B=$(current)
[ "$B" != api ] || fail 'second directory attached to the first one'"'"'s session'
[ "$(remote_tmux display-message -p -t "=$B:" '#{pane_current_path}')" = "$(real "$W/client-b/api")" ] || fail 'second session in the wrong directory'
stop_outer; wait_for 'detached' sh -c "[ \"\$(TMUX_TMPDIR=$W/remote tmux list-clients | wc -l | tr -d ' ')\" = 0 ]"
run_dev "box:$W/client-a/api"; wait_for 'back to the first' attached
[ "$(current)" = api ] || fail 'the first directory lost its session name'
stop_outer; wait_for 'detached' sh -c "[ \"\$(TMUX_TMPDIR=$W/remote tmux list-clients | wc -l | tr -d ' ')\" = 0 ]"
run_dev "box:$W/client-b/api"; wait_for 'back to the second' attached
[ "$(current)" = "$B" ] || fail 'the second directory got another session'
[ "$(remote_tmux list-sessions | wc -l | tr -d " ")" = 2 ] || fail 'more than two sessions'
stop_outer; wait_for 'detached' sh -c "[ \"\$(TMUX_TMPDIR=$W/remote tmux list-clients | wc -l | tr -d ' ')\" = 0 ]"
remote_tmux kill-session -t =api
run_dev "box:$W/client-b/api"; wait_for 'own session after the other left' attached
[ "$(current)" = "$B" ] || fail 'it moved off its own session once the name was free'
stop_outer

echo '13. SIGUSR1 redoes a connection at once, in the same session'
# Options first: on BSD anything after the name is more names, so -P would be ignored,
# and this would pick up every meterm on the machine.
meterm_pid() { pgrep -x -P "$(outer list-panes -t it -F '#{pane_pid}')" meterm; }
logins() { grep -c 'Accepted publickey' "$W/sshd.log"; }
run_dev "box:$W/proj"
wait_for 'attach' sh -c "[ \"\$(TMUX_TMPDIR=$W/remote tmux display-message -p -t =proj: '#{session_attached}')\" = 1 ]"
PANE=$(remote_tmux display-message -p -t =proj: '#{pane_id}')
sleep 2 # the second login, that finds out whether tmux is there
BEFORE=$(logins)
kill -USR1 "$(meterm_pid)"
for _ in $(seq 1 50); do [ "$(logins)" -gt "$BEFORE" ] && break; sleep 0.2; done
[ "$(logins)" -gt "$BEFORE" ] || fail 'no new login within 10s of the signal'
wait_for 're-attached' sh -c "[ \"\$(TMUX_TMPDIR=$W/remote tmux display-message -p -t =proj: '#{session_attached}')\" = 1 ]"
[ "$(remote_tmux display-message -p -t =proj: '#{pane_id}')" = "$PANE" ] || fail 'a different pane after the signal'
[ ! -e "$W/rc" ] || fail 'exited on the signal'
stop_outer

echo '14. a meterm not holding a session on for reconnects ignores it'
run_dev "box:$W/proj --no-reconnect"
wait_for 'attach' sh -c "[ \"\$(TMUX_TMPDIR=$W/remote tmux display-message -p -t =proj: '#{session_attached}')\" = 1 ]"
BEFORE=$(logins)
kill -USR1 "$(meterm_pid)"
sleep 2
[ "$(logins)" = "$BEFORE" ] && [ ! -e "$W/rc" ] || fail '--no-reconnect reacted to the signal'
stop_outer

echo '15. the wrong host key on one route moves on to the next, with a warning'
# Host keys are checked for real here, in a home of its own, since a route's
# keys get carried into ~/.ssh/known_hosts.
mkdir -p "$W/home/.ssh"
ssh-keygen -q -t ed25519 -N '' -f "$W/otherkey"
BAD_PORT=$(free_port)
sed -e "s/^Port .*/Port $BAD_PORT/" -e "s#^HostKey .*#HostKey $W/otherkey#" -e "s#^PidFile .*#PidFile $W/bad.pid#" "$W/sshd_config" > "$W/bad_config"
/usr/sbin/sshd -D -f "$W/bad_config" -E "$W/bad.log" &
BAD_PID=$!
GOOD_KEY=$(cut -d' ' -f1,2 "$W/hostkey.pub")
printf '[127.0.0.1]:%s %s\n[127.0.0.1]:%s %s\n' "$BAD_PORT" "$GOOD_KEY" "$PORT" "$GOOD_KEY" > "$W/home/.ssh/known_hosts"
cat > "$W/strict_config" <<EOF
Host *
  StrictHostKeyChecking yes
  UserKnownHostsFile $W/home/.ssh/known_hosts
EOF
meterm add moved --address 127.0.0.1 --port "$BAD_PORT" --user "$(id -un)" --identity "$W/key" >/dev/null
meterm route add moved good --address 127.0.0.1 --port "$PORT" --direct >/dev/null
wait_for 'second sshd' sh -c "ssh-keyscan -p $BAD_PORT 127.0.0.1 2>/dev/null | grep -q ."
outer new-session -d -s it -x 120 -y 30 "env HOME=$W/home IT_SSH_CONFIG=$W/strict_config $W/bin/meterm dev moved:$W/proj --no-reconnect; echo \$? > $W/rc; sleep 60"
wait_for 'attach over the good route' sh -c "[ \"\$(TMUX_TMPDIR=$W/remote tmux display-message -p -t =proj: '#{session_attached}')\" = 1 ]"
[ "$(grep -c 'Accepted publickey' "$W/bad.log")" = 0 ] || fail 'logged in to the machine with the wrong key'
stop_outer

echo '16. with no route left, the wrong host key stops it'
meterm add liar --address 127.0.0.1 --port "$BAD_PORT" --user "$(id -un)" --identity "$W/key" >/dev/null
outer new-session -d -s it -x 120 -y 30 "env HOME=$W/home IT_SSH_CONFIG=$W/strict_config $W/bin/meterm dev liar; echo \$? > $W/rc; sleep 60"
wait_for 'exit 255' rc_is 255
[ "$(grep -c 'Accepted publickey' "$W/bad.log")" = 0 ] || fail 'logged in to the machine with the wrong key'
outer capture-pane -p -t it | grep -q 'Host key verification failed' || fail 'ssh said nothing about the key'
kill "$BAD_PID" 2>/dev/null || true
stop_outer

echo '17. status: a kill -9 is dropped, the end of a session leaves nothing'
[ "$(meterm status --format json | jq '.entries | length')" = 0 ] || fail 'status still shows ended processes'
run_dev "box:$W/proj"
wait_for 'status: connected' status_is session proj connected
kill -9 "$(meterm_pid)"
wait_for 'dead process dropped' sh -c "[ \"\$($W/bin/meterm status --format json | jq '.entries | length')\" = 0 ]"
[ -z "$(ls "$W/run")" ] || fail 'state files left behind'
stop_outer
meterm status --format json --watch --interval 200ms > "$W/watch.out" &
WATCH_PID=$!
run_dev "box:$W/proj"
wait_for 'watch: session connected' grep -q '"state":"connected"' "$W/watch.out"
stop_outer
wait_for 'watch: nothing held' sh -c "tail -n 1 $W/watch.out | jq -e '.entries == []'"
kill "$WATCH_PID"

echo '18. with no tmux on the host, a kick leaves the plain shell alone'
NOTMUX_PORT=$(free_port)
mkdir -p "$W/notmux"
ln -s /bin/sh /bin/bash "$W/notmux/"
sed -e "s/^Port .*/Port $NOTMUX_PORT/" -e "s#^PidFile .*#PidFile $W/nt.pid#" -e "s#^SetEnv .*#SetEnv PATH=$W/notmux#" "$W/sshd_config" > "$W/nt_config"
/usr/sbin/sshd -D -f "$W/nt_config" -E "$W/nt.log" &
NT_PID=$!
meterm add plain --address 127.0.0.1 --port "$NOTMUX_PORT" --user "$(id -un)" --identity "$W/key" >/dev/null
wait_for 'third sshd' sh -c "ssh-keyscan -p $NOTMUX_PORT 127.0.0.1 2>/dev/null | grep -q ."
outer new-session -d -s it -x 120 -y 30 "env SHELL=/bin/sh $W/bin/meterm dev plain; echo \$? > $W/rc; sleep 60"
wait_for 'the plain shell' sh -c "tmux -L it-outer capture-pane -p -t it | grep -q 'plain shell'"
outer send-keys -t it "echo \$\$ > $W/shell.pid" Enter
wait_for 'the shell pid' test -s "$W/shell.pid"
SHELL_PID=$(cat "$W/shell.pid")
sleep 2 # the second login, that finds out there's no tmux
BEFORE=$(grep -c 'Accepted publickey' "$W/nt.log")
kill -USR1 "$(meterm_pid)"
sleep 2
kill -0 "$SHELL_PID" 2>/dev/null || fail 'the signal killed the plain shell'
[ "$(grep -c 'Accepted publickey' "$W/nt.log")" = "$BEFORE" ] || fail 'the signal reconnected a session with no tmux'
outer capture-pane -p -t it | grep -q 'reconnecting' && fail 'said it was reconnecting'
kill "$NT_PID" 2>/dev/null || true
stop_outer

echo '19. a log path runs nothing on the host'
mkdir -p "$W/logs"
echo 'from the log' > "$W/logs/app.log"
for evil in "\$(touch $W/pwned)" "\`touch $W/pwned\`" "x; touch $W/pwned"; do
  meterm logs box "$W/logs/$evil" >/dev/null 2>&1 || true
done
[ ! -e "$W/pwned" ] || fail 'a log path ran on the host'
meterm logs list box "\$(touch $W/pwned)" >/dev/null 2>&1 || true
[ ! -e "$W/pwned" ] || fail 'a log directory ran on the host'
meterm logs box "$W/logs/ap*.log" | grep 'from the log' >/dev/null || fail 'a glob stopped working'
[ "$(meterm activity --host box --format json | jq '[.[] | select(.action == "logs")] | length')" -ge 5 ] || fail 'logs not in the activity log'

echo '20. exec: several arguments stay arguments, one is a command line'
meterm exec box -- echo "\$(touch $W/pwned)" >/dev/null
[ ! -e "$W/pwned" ] || fail 'an exec argument ran on the host'
meterm exec box -- sh -c 'echo first; echo second' | grep second >/dev/null || fail 'sh -c lost its script'
meterm exec box -- 'echo piped | tr a-z A-Z' | grep PIPED >/dev/null || fail 'one argument is not a command line'

echo '21. the activity log keeps the program, not its arguments'
meterm exec box -- echo KEEP-THIS-OUT >/dev/null
meterm activity --host box --format json | grep 'KEEP-THIS-OUT' >/dev/null && fail 'arguments in the activity log by default'
meterm activity --host box --format json | grep 'echo (+1 args)' >/dev/null || fail 'the program is not in the activity log'
meterm exec box --log-args -- echo PUT-THIS-IN >/dev/null
meterm activity --host box --format json | grep 'PUT-THIS-IN' >/dev/null || fail '--log-args did not record them'
meterm activity --verify | grep 'chain intact' >/dev/null || fail 'the chain broke'

echo '22. on a terminal, what a host prints can only show text'
printf 'plain line\n\033]52;c;ZXZpbA==\007after osc\n\033[2Jafter clear\n\033[31mred\033[0m\n' > "$W/logs/evil.log"
on_tty() { # util-linux's script and the BSD one take different arguments
  if script --version >/dev/null 2>&1; then script -qfec "$*" /dev/null; else script -q /dev/null sh -c "$*" </dev/null; fi
}
for run in "meterm logs box $W/logs/evil.log" "meterm exec box -- cat $W/logs/evil.log" ; do
  on_tty "env PATH=$PATH $W/bin/$run" > "$W/tty.out" 2>&1
  grep -q 'after osc' "$W/tty.out" || fail "$run: lost text"
  grep -q 'red' "$W/tty.out" || fail "$run: lost colours"
  python3 - "$W/tty.out" <<'PY' || fail "$run: an escape sequence got through"
import sys
data = open(sys.argv[1], 'rb').read()
sys.exit(1 if b'\x1b]52' in data or b'\x1b[2J' in data else 0)
PY
done
on_tty "env PATH=$PATH $W/bin/meterm logs box --raw $W/logs/evil.log" > "$W/tty.out" 2>&1
grep -qa ']52;' "$W/tty.out" || fail '--raw dropped them'
meterm logs box "$W/logs/evil.log" | grep -a ']52;' >/dev/null || fail 'a pipe should get what was sent'
meterm exec box --format json -- cat "$W/logs/evil.log" | jq -e '.[0].stdout | test("]52;")' >/dev/null || fail 'json should hold what was sent'
: > "$W/logs/live.log"
outer new-session -d -s it -x 120 -y 30 "$W/bin/meterm logs box -f $W/logs/live.log; sleep 60"
sleep 1
printf '\033]0;title\007live line one\n' >> "$W/logs/live.log"
wait_for 'a followed line' sh -c "tmux -L it-outer capture-pane -p -t it | grep -q 'live line one'"
stop_outer

echo '23. dev -- command: once, in a window of the session, with arguments intact'
mkdir -p "$W/cmdproj"
cat > "$W/bin/it-record" <<IT_EOF
#!/bin/sh
for a in "\$@"; do printf '%s\\n' "\$a"; done >> "$W/cmd.log"
pwd > "$W/cmd.pwd"
exec sleep 300
IT_EOF
chmod +x "$W/bin/it-record"
cat > "$W/run-cmd" <<IT_EOF
#!/bin/sh
"$W/bin/meterm" dev "box:$W/cmdproj" -- "$W/bin/it-record" one 'two words' '\$(touch $W/pwned)'
echo \$? > "$W/rc"
sleep 60
IT_EOF
chmod +x "$W/run-cmd"
outer new-session -d -s it -x 120 -y 30 "$W/run-cmd"
wait_for 'the command ran' test -s "$W/cmd.pwd"
[ "$(cat "$W/cmd.pwd")" = "$(cd "$W/cmdproj" && pwd -P)" ] || fail 'not in the directory'
[ "$(sed -n 2p "$W/cmd.log")" = 'two words' ] || fail 'arguments not intact'
[ "$(sed -n 3p "$W/cmd.log")" = "\$(touch $W/pwned)" ] || fail 'argument changed'
[ ! -e "$W/pwned" ] || fail 'an argument ran on the host'
[ "$(remote_tmux list-windows -t =cmdproj: | wc -l | tr -d " ")" = 2 ] || fail 'not a window of its own in the session'
wait_for 'attached' sh -c "[ \"\$(TMUX_TMPDIR=$W/remote tmux display-message -p -t =cmdproj: '#{session_attached}')\" = 1 ]"
meterm activity --host box --format json | grep 'dev cmdproj: .*it-record (+3 args)' >/dev/null || fail 'activity should say the program'
meterm activity --host box --format json | grep 'two words' >/dev/null && fail 'arguments in the activity log'
LINES_BEFORE=$(wc -l < "$W/cmd.log")
sleep 3 # the second login, that finds out there's tmux
LOGINS_BEFORE=$(logins)
kill -USR1 "$(meterm_pid)"
wait_for 'it to log in again' sh -c "[ \"\$(grep -c 'Accepted publickey' $W/sshd.log)\" -gt $LOGINS_BEFORE ]"
sleep 2
wait_for 'back in the session' sh -c "[ \"\$(TMUX_TMPDIR=$W/remote tmux display-message -p -t =cmdproj: '#{session_attached}')\" = 1 ]"
[ "$(wc -l < "$W/cmd.log")" = "$LINES_BEFORE" ] || fail 'a reconnect started it again'
[ "$(remote_tmux list-windows -t =cmdproj: | wc -l | tr -d " ")" = 2 ] || fail 'a reconnect opened another window'
outer send-keys -t it C-b d
wait_for 'detach exits 0' rc_is 0
stop_outer
remote_tmux kill-session -t =cmdproj
case "$(meterm dev "box:$W/cmdproj" box2 -- x 2>&1 || true)" in *'accepts 1 target'*) ;; *) fail 'two targets accepted' ;; esac
case "$(meterm dev box -- 2>&1 || true)" in *'no command given'*) ;; *) fail 'an empty command accepted' ;; esac

echo '24. dev -- command with no tmux: the exit code is the command'"'"'s'
NT2_PORT=$(free_port)
sed -e "s/^Port .*/Port $NT2_PORT/" -e "s#^PidFile .*#PidFile $W/nt2.pid#" "$W/nt_config" > "$W/nt2_config"
/usr/sbin/sshd -D -f "$W/nt2_config" -E "$W/nt2.log" &
NT2_PID=$!
meterm add plain2 --address 127.0.0.1 --port "$NT2_PORT" --user "$(id -un)" --identity "$W/key" >/dev/null
wait_for 'fourth sshd' sh -c "ssh-keyscan -p $NT2_PORT 127.0.0.1 2>/dev/null | grep -q ."
cat > "$W/run-cmd" <<IT_EOF
#!/bin/sh
"$W/bin/meterm" dev plain2 -- /bin/sh -c 'exit 7'
echo \$? > "$W/rc"
sleep 60
IT_EOF
outer new-session -d -s it -x 120 -y 30 "$W/run-cmd"
wait_for 'the exit code of the command' rc_is 7
stop_outer
kill "$NT2_PID" 2>/dev/null || true

echo 'PASS'
