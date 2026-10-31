#!/usr/bin/env bash
# Integration test for `meterm tunnel enable` on macOS, against the real launchd and
# a real sshd. Run it by hand, on a Mac, logged in at the screen.
#
# What it touches, all of it undone on exit: one job in your gui domain with a label
# only this run uses, loaded from a temp HOME, so your real ~/Library/LaunchAgents is
# never written to; and its own sshd on a loopback port, with its own keys. The job
# is given its own activity, team and status directories, and autosync is off.
# macOS may show a "background item added" notice once, for the job.
# Needs: sshd, python3, curl.
set -euo pipefail

[ "$(uname)" = Darwin ] || { echo "skip: macOS only"; exit 0; }
launchctl print "gui/$(id -u)" >/dev/null 2>&1 || { echo "skip: no gui domain (nobody logged in at the screen)"; exit 0; }

W=$(mktemp -d /tmp/mtl.XXXXXX)
W=$(cd "$W" && pwd -P)
SSHD_PID=""
HTTP_PID=""
TARGET=""
cleanup() {
  [ -z "$TARGET" ] || launchctl bootout "$TARGET" >/dev/null 2>&1 || true
  [ -z "$HTTP_PID" ] || kill "$HTTP_PID" 2>/dev/null || true
  [ -z "$SSHD_PID" ] || kill "$SSHD_PID" 2>/dev/null || true
  rm -rf "$W"
}
trap cleanup EXIT
fail() {
  echo "FAIL: $*"
  [ -z "$TARGET" ] || launchctl print "$TARGET" 2>&1 | sed -n 1,30p || true
  cat "$W"/home/Library/Logs/meterm/*.log 2>/dev/null | tail -n 20 || true
  tail -n 10 "$W/sshd.log" 2>/dev/null || true
  exit 1
}
wait_for() { # <seconds> <description> <command...>
  local n=$1 what=$2 i; shift 2
  for ((i = 0; i < n * 5; i++)); do "$@" >/dev/null 2>&1 && return 0; sleep 0.2; done
  fail "timed out waiting for $what"
}
free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }

go build -o "$W/bin/meterm" ./cmd/meterm
M=$W/bin/meterm
REAL_LAUNCH_AGENTS=$(ls -a "$HOME/Library/LaunchAgents" 2>/dev/null | shasum)

export HOME=$W/home
export METERM_CONFIG=$W/hosts.yaml METERM_ACTIVITY_DIR=$W/activity METERM_HISTORY=$W/history.json
export METERM_TEAM_DIR=$W/team METERM_RUNTIME_DIR=$W/run METERM_NO_AUTOSYNC=1
mkdir -p "$HOME" "$W/www"
echo meterm-it-ok >"$W/www/ok.txt"

SSH_PORT=$(free_port); WEB_REMOTE=$(free_port); WEB_LOCAL=$(free_port)
ssh-keygen -q -t ed25519 -N '' -f "$W/hostkey"
ssh-keygen -q -t ed25519 -N '' -f "$W/key"
cp "$W/key.pub" "$W/authorized_keys"; chmod 600 "$W/authorized_keys"
cat >"$W/sshd_config" <<CONF
Port $SSH_PORT
ListenAddress 127.0.0.1
HostKey $W/hostkey
AuthorizedKeysFile $W/authorized_keys
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
StrictModes no
PidFile $W/sshd.pid
Banner $W/banner
CONF
# What a hostile server can say before authentication: something ssh would read as a
# failure that can't pass, and an escape sequence for whoever cats the log.
printf 'Unable to negotiate with you\n\033]52;c;aGk=\007\n' >"$W/banner"
/usr/sbin/sshd -D -f "$W/sshd_config" -E "$W/sshd.log" &
SSHD_PID=$!
(cd "$W/www" && exec python3 -I -m http.server "$WEB_REMOTE" --bind 127.0.0.1 >/dev/null 2>&1) &
HTTP_PID=$!
printf '[127.0.0.1]:%s %s\n' "$SSH_PORT" "$(cut -d' ' -f1,2 "$W/hostkey.pub")" >"$W/known_hosts"
cat >"$W/ssh_config" <<CONF
UserKnownHostsFile $W/known_hosts
StrictHostKeyChecking yes
IdentityFile $W/key
IdentitiesOnly yes
CONF

BOX="it-launchd-$$"
# A name with everything XML and a shell care about. It has to come back as itself.
NAME="w<e&b>'\$(touch \$HOME/pwned)\""
SPEC="$BOX/$NAME"
write_hosts() { # with or without the tunnel
  {
    printf 'version: 1\nhosts:\n- name: %s\n  address: 127.0.0.1\n  port: %s\n  extra_args: [-F, %s/ssh_config]\n' "$BOX" "$SSH_PORT" "$W"
    if [ "$1" = with ]; then
      printf '  forwards:\n  - {name: %s, type: L, local_port: %s, remote_host: 127.0.0.1, remote_port: %s}\n' \
        "$(NAME=$NAME python3 -I -c 'import json, os; print(json.dumps(os.environ["NAME"]))')" "$WEB_LOCAL" "$WEB_REMOTE"
    fi
  } >"$METERM_CONFIG"
}
write_hosts with

status_state() { "$M" status --format json | python3 -I -c '
import json, sys
for e in json.load(sys.stdin)["entries"]:
    print(e["state"], e["pid"])'; }
is_state() { status_state | grep -q "^$1 "; }
job_pid() { status_state | awk '$1=="connected"{print $2}'; }
through() { [ "$(curl -s --max-time 2 "http://127.0.0.1:$WEB_LOCAL/ok.txt")" = meterm-it-ok ]; }

echo "== enable: $SPEC"
"$M" tunnel enable "$SPEC"
PLIST=$(ls "$HOME"/Library/LaunchAgents/*.plist)
LABEL=$(basename "$PLIST" .plist)
TARGET="gui/$(id -u)/$LABEL"
plutil -lint "$PLIST" >/dev/null || fail "plutil refused the plist"
wait_for 20 "the tunnel to connect" is_state connected
wait_for 10 "the tunnel to carry traffic" through
[ ! -e "$HOME/pwned" ] || fail "the name ran as a command"
echo "ok: status says connected, and traffic gets through"

launchctl print "$TARGET" | grep -q 'state = running' || fail "launchd doesn't say running"
launchctl print "$TARGET" | grep -q 'SSH_AUTH_SOCK' || fail "the job has no SSH_AUTH_SOCK"
SOCK=$(launchctl getenv SSH_AUTH_SOCK || true)
if [ -n "$SOCK" ]; then
  rc=0; SSH_AUTH_SOCK=$SOCK ssh-add -l >/dev/null 2>&1 || rc=$?
  [ "$rc" != 2 ] || fail "launchd's agent socket doesn't answer"
  echo "ok: the job has launchd's SSH_AUTH_SOCK, and that agent answers"
fi
[ -n "$(ls "$METERM_ACTIVITY_DIR" 2>/dev/null)" ] || fail "the job's activity went somewhere else"

echo "== a killed meterm comes back"
wait_for 20 "a state file saying connected" is_state connected
PID=$(job_pid)
kill -9 "$PID"
back() { p=$(job_pid); [ -n "$p" ] && [ "$p" != "$PID" ]; }
wait_for 50 "launchd to restart it" back
wait_for 10 "the tunnel to carry traffic again" through
echo "ok: pid $PID was killed and pid $(job_pid) took over"

echo "== a hostile banner neither stops it nor reaches the log"
grep -q 'Unable to negotiate' "$HOME"/Library/Logs/meterm/*.log || fail "the banner wasn't seen, so this proves nothing"
is_state connected || fail "the banner stopped it"
if LC_ALL=C grep -q "$(printf '\033')" "$HOME"/Library/Logs/meterm/*.log; then fail "an escape sequence reached the log"; fi
echo ok

echo "== SIGTERM from outside: it comes back"
wait_for 20 "a state file saying connected" is_state connected
PID=$(job_pid)
kill -TERM "$PID"
back() { p=$(job_pid); [ -n "$p" ] && [ "$p" != "$PID" ]; }
wait_for 50 "launchd to restart it" back
wait_for 10 "the tunnel to carry traffic" through
echo "ok: pid $PID got SIGTERM and pid $(job_pid) took over"

echo "== a broken config, across launchd's restarts, then fixed: it comes back"
wait_for 20 "a state file saying connected" is_state connected
PID=$(job_pid)
printf 'hosts: [ {{{ not yaml' >"$METERM_CONFIG"
kill -9 "$PID"
echo "   waiting past launchd's 30 seconds with the file broken"
sleep 36
failing() { launchctl print "$TARGET" | grep -q 'last exit code = 1'; }
wait_for 40 "a failed start" failing
launchctl print "$TARGET" | grep -q 'last exit code = 0' && fail "it exited 0 on a broken file"
if through; then fail "traffic with a broken config?"; fi
"$M" status 2>&1 | grep -q 'stopped' && echo "   status: $("$M" status 2>&1 | tail -1 | cut -c1-110)" || true
write_hosts with
wait_for 50 "the tunnel to return once the file is fixed" through
echo "ok: exit 1 while broken, and back on its own once fixed"

echo "== the same with a file that's empty at login"
wait_for 20 "a state file saying connected" is_state connected
PID=$(job_pid)
printf 'version: 1\nhosts: []\n' >"$METERM_CONFIG"
kill -9 "$PID"
sleep 36
wait_for 40 "a failed start" failing
write_hosts with
wait_for 50 "the tunnel to return" through
echo ok

echo "== the status directory purged: launchd says it's running"
wait_for 20 "a state file saying connected" is_state connected
PID=$(job_pid)
mv "$METERM_RUNTIME_DIR" "$METERM_RUNTIME_DIR.gone"
through || fail "the tunnel should be untouched"
is_state running || fail "status doesn't say it's running: $("$M" status | tail -1)"
pkill -9 -P "$PID" ssh
wait_for 20 "the directory to come back" test -d "$METERM_RUNTIME_DIR"
wait_for 20 "a state file saying connected" is_state connected
echo ok

echo "== the binary moved: status says launchd can't start it"
wait_for 20 "a state file saying connected" is_state connected
PID=$(job_pid)
mv "$M" "$M.moved"
kill -9 "$PID"
sleep 3
"$M.moved" status | grep -q "isn't there" || fail "status: $("$M.moved" status | tail -1)"
echo "   status: $("$M.moved" status | tail -1 | cut -c1-110)"
mv "$M.moved" "$M"
wait_for 50 "the tunnel to return with the binary" through
echo ok

echo "== a failed re-enable brings the old job back, and a job with its file gone still stops"
wait_for 20 "a state file saying connected" is_state connected
PID=$(job_pid)
cp "$PLIST" "$W/plist.keep"
rm -f "$PLIST"
"$M" tunnel disable "$SPEC" || fail "disable with the file gone"
gone() { ! launchctl print "$TARGET" >/dev/null 2>&1; }
wait_for 10 "launchd to let go of the job" gone
wait_for 10 "the port to close" sh -c "! curl -s --max-time 1 http://127.0.0.1:$WEB_LOCAL/ok.txt"
"$M" tunnel enable "$SPEC" 2>/dev/null
wait_for 20 "the tunnel" through
# Something launchd will refuse, with a job running: a read-only place for the new file
# can't be used, so this goes through a job file launchd won't take after a changed env.
wait_for 20 "a state file saying connected" is_state connected
PID=$(job_pid)
chmod 500 "$HOME/Library/LaunchAgents"
rc=0; METERM_TEAM_DIR=$W/team2 "$M" tunnel enable "$SPEC" 2>"$W/enable.err" || rc=$?
chmod 700 "$HOME/Library/LaunchAgents"
[ "$rc" != 0 ] || fail "enable should have failed: nowhere to write"
[ "$(job_pid)" = "$PID" ] || fail "a failed enable disturbed the running job"
through || fail "a failed enable took the tunnel down"
echo "ok: $(cut -c1-110 "$W/enable.err")"

echo "== a tunnel that's gone stops for good"
wait_for 20 "a state file saying connected" is_state connected
PID=$(job_pid)
write_hosts without
pkill -9 -P "$PID" ssh
stopped() { ! launchctl print "$TARGET" | grep -q 'state = running'; }
wait_for 20 "it to give up" stopped
launchctl print "$TARGET" | grep -Eq 'last exit (code|reason) = 0' || fail "it didn't exit 0"
grep -q 'no such tunnel any more' "$HOME"/Library/Logs/meterm/*.log || fail "the log doesn't say why"
is_state stopped || fail "status doesn't show it stopped"
echo "   waiting past launchd's 30 seconds to see that it stays stopped"
sleep 40
stopped || fail "launchd restarted it"
echo "ok: stopped, says why in the log, and meterm status shows it"

echo "== enable again restarts it"
write_hosts with
"$M" tunnel enable "$SPEC"
wait_for 20 "the tunnel to connect" is_state connected
wait_for 10 "the tunnel to carry traffic" through
wait_for 20 "a state file saying connected" is_state connected
PID=$(job_pid)
"$M" tunnel enable "$SPEC" 2>/dev/null
[ "$(job_pid)" = "$PID" ] || fail "enabling what's already enabled dropped the tunnel"
echo "ok"

echo "== disable"
"$M" tunnel disable "$SPEC"
[ ! -e "$PLIST" ] || fail "the job file stayed"
wait_for 10 "launchd to let go of the job" gone
wait_for 10 "the port to close" sh -c "! curl -s --max-time 1 http://127.0.0.1:$WEB_LOCAL/ok.txt"
[ "$("$M" status --format json)" = '{"schema":1,"entries":[]}' ] || fail "status isn't empty: $("$M" status --format json)"
TARGET=""

REAL_HOME=$(dscl . -read "/Users/$(id -un)" NFSHomeDirectory | awk '{print $2}')
[ "$(ls -a "$REAL_HOME/Library/LaunchAgents" 2>/dev/null | shasum)" = "$REAL_LAUNCH_AGENTS" ] || fail "your real LaunchAgents changed"
echo "ok: real LaunchAgents unchanged"
echo PASS
