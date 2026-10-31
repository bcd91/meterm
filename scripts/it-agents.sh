#!/usr/bin/env bash
# Integration test for `meterm agents` against a real sshd. Everything runs from a
# temp directory: its own sshd on a loopback port, its own keys, its own HOME for the
# login, and a fake claude. It needs no root and leaves the machine's ssh alone.
# Needs: sshd, python3.
set -euo pipefail

W=$(mktemp -d /tmp/mta.XXXXXX)
W=$(cd "$W" && pwd -P)
SSHD_PID=""
cleanup() {
  for s in "$W"/tmp/meterm-cm-*/* "$W"/long*/meterm-cm-*/*; do
    [ -S "$s" ] && ssh -F "$W/ssh_config" -o ControlPath="$s" -O exit x 2>/dev/null || true
  done
  [ -z "$SSHD_PID" ] || kill "$SSHD_PID" 2>/dev/null || true
  rm -rf "$W"
}
trap cleanup EXIT
fail() { echo "FAIL: $*"; tail -n 20 "$W/sshd.log" 2>/dev/null || true; exit 1; }
free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }

go build -o "$W/meterm" ./cmd/meterm
M=$W/meterm
export METERM_CONFIG=$W/hosts.yaml METERM_ACTIVITY_DIR=$W/activity METERM_HISTORY=$W/history.json
export METERM_TEAM_DIR=$W/team METERM_RUNTIME_DIR=$W/run METERM_NO_AUTOSYNC=1
export TMPDIR=$W/tmp XDG_RUNTIME_DIR=
mkdir -p "$TMPDIR" "$W/home/.local/bin"

PORT=$(free_port); DEAD=$(free_port)
ssh-keygen -q -t ed25519 -N '' -f "$W/hostkey"
ssh-keygen -q -t ed25519 -N '' -f "$W/key"
cp "$W/key.pub" "$W/authorized_keys"; chmod 600 "$W/authorized_keys"
cat >"$W/sshd_config" <<CONF
Port $PORT
ListenAddress 127.0.0.1
HostKey $W/hostkey
AuthorizedKeysFile $W/authorized_keys
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
StrictModes no
PidFile $W/sshd.pid
LogLevel INFO
SetEnv HOME=$W/home PATH=/usr/bin:/bin
CONF
/usr/sbin/sshd -D -f "$W/sshd_config" -E "$W/sshd.log" &
SSHD_PID=$!
printf '[127.0.0.1]:%s %s\n' "$PORT" "$(cut -d' ' -f1,2 "$W/hostkey.pub")" >"$W/known_hosts"
cat >"$W/ssh_config" <<CONF
UserKnownHostsFile $W/known_hosts
StrictHostKeyChecking yes
IdentityFile $W/key
IdentitiesOnly yes
CONF
for i in $(seq 1 50); do grep -q 'Server listening' "$W/sshd.log" 2>/dev/null && break; sleep 0.1; done

cat >"$METERM_CONFIG" <<CONF
version: 1
hosts:
- {name: box, address: 127.0.0.1, port: $PORT, tags: [t], extra_args: [-F, $W/ssh_config]}
- {name: live, address: 127.0.0.1, port: $PORT, tags: [t], env: production, extra_args: [-F, $W/ssh_config]}
- {name: dead, address: 127.0.0.1, port: $DEAD, extra_args: [-F, $W/ssh_config]}
CONF

fake() { # what the fake claude does
  printf '#!/bin/sh\n[ "$1 $2" = "agents --json" ] || { echo "wrong args: $*" >&2; exit 9; }\n%s\n' "$1" >"$W/home/.local/bin/claude"
  chmod +x "$W/home/.local/bin/claude"
}
logins() { grep -c 'Accepted publickey' "$W/sshd.log" || true; }
run() { # <expected exit> <args...>: leaves stdout in $OUT
  local want=$1 rc=0; shift
  OUT=$("$M" agents "$@" 2>"$W/stderr") || rc=$?
  [ "$rc" = "$want" ] || fail "agents $*: exit $rc, wanted $want: $OUT $(cat "$W/stderr")"
}

echo "== finds claude in ~/.local/bin, with a non-login PATH"
fake "echo '[{\"pid\":7,\"cwd\":\"/src\",\"kind\":\"interactive\",\"startedAt\":1793311202058,\"sessionId\":\"s\",\"name\":\"n\",\"status\":\"busy\",\"extra\":1}]'"
run 0 box --format json
[ "$OUT" = '{"schema":1,"hosts":[{"host":"box","reachable":true,"error":"","agents":[{"cwd":"/src","kind":"interactive","startedAt":1793311202058,"pid":7,"status":"busy","sessionId":"s","name":"n"}]}]}' ] || fail "got $OUT"
echo ok

echo "== nothing is written, and a second look uses the same login"
[ ! -e "$METERM_ACTIVITY_DIR" ] && [ ! -e "$METERM_HISTORY" ] && [ ! -e "$METERM_TEAM_DIR" ] || fail "something was recorded"
before=$(logins)
run 0 box
run 0 box
[ "$(logins)" = "$before" ] || fail "logins went from $before to $(logins)"
SOCK=$(ls "$TMPDIR"/meterm-cm-*/*)
[ -S "$SOCK" ] || fail "no master socket: $SOCK"
[ "$(stat -f '%Lp' "$(dirname "$SOCK")" 2>/dev/null || stat -c '%a' "$(dirname "$SOCK")")" = 700 ] || fail "the directory isn't private"
echo "ok: one login for three looks, socket of ${#SOCK} bytes"

echo "== production: named it's asked, on a login of its own; by tag it's refused"
before=$(logins); n=$(ls "$TMPDIR"/meterm-cm-*/ | wc -l)
run 0 live
run 0 live
[ "$(logins)" = "$((before + 2))" ] || fail "reused a login for production"
[ "$(ls "$TMPDIR"/meterm-cm-*/ | wc -l)" = "$n" ] || fail "a master for production"
before=$(logins)
run 2 --tag t --format json
python3 -I -c '
import json, sys
h = {x["host"]: x for x in json.loads(sys.argv[1])["hosts"]}
assert h["box"]["error"] == "" and h["box"]["agents"]
assert h["live"]["reachable"] is False and h["live"]["agents"] is None and "refused" in h["live"]["error"]
' "$OUT" || fail "got $OUT"
[ "$(logins)" = "$before" ] || fail "something logged in"
echo ok

echo "== every failure is reported per host, and never as zero agents"
check() { # <what> <reachable> <text in the error>
  python3 -I -c '
import json, sys
h = json.loads(sys.argv[1])["hosts"][0]
assert h["agents"] is None and h["reachable"] == (sys.argv[2] == "true") and sys.argv[3] in h["error"], h
' "$OUT" "$2" "$3" || fail "$1: $OUT"
  echo "ok: $1"
}
run 2 dead --format json; check "nothing listening" false "connection refused"
fake 'echo "not json"'; run 2 box --format json; check "bad JSON" true "unexpected output"
fake 'exit 255'; run 2 box --format json; check "claude exits 255, which isn't ssh's" true "claude agents failed"
fake 'sleep 30'; run 2 box --format json --timeout 2s; check "a claude that hangs" false "timed out"
fake 'head -c 2000000 /dev/zero'; run 2 box --format json; check "too much output" true "more than meterm reads"
rm "$W/home/.local/bin/claude"; run 2 box --format json; check "no claude" true "claude not found"

echo "== hostile output reaches neither format"
fake "printf '[{\"name\":\"a\\\\u001b]52;c;aGk=\\\\u0007b\\\\u202ec\\\\u009b2Jd\",\"cwd\":\"/x\\\\u001b[2J\\\\ny\"}]'"
for f in text json; do
  run 0 box --format $f
  python3 -I -c '
import sys
s = sys.argv[1]
for c in ("\x1b", "\x07", "‮", "\u009b", "\\u001b", "\\u202e", "\\u0007", "\\u009b"):
    assert c not in s, (c, s)
' "$OUT" || fail "$f: $OUT"
done
echo ok

echo "== nothing to do without a selection"
run 1
run 1 no-such-host
run 1 --tag no-such-tag
rc=0; "$M" agents -- $'evil\e]0;x\a' 2>"$W/stderr" || rc=$?
[ "$rc" = 1 ] && ! grep -q $'\e' "$W/stderr" || fail "a hostile name came back raw"
echo ok

echo "== a long TMPDIR, and one with a space, still work"
for dir in "$W/long$(printf 'x%.0s' $(seq 1 120))" "$W/long with space"; do
  mkdir -p "$dir"
  fake "echo '[]'"
  OUT=$(TMPDIR=$dir "$M" agents box --format json) || fail "TMPDIR=$dir"
  [ "$OUT" = '{"schema":1,"hosts":[{"host":"box","reachable":true,"error":"","agents":[]}]}' ] || fail "$OUT"
done
echo ok
echo PASS
