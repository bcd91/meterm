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
mkdir -p "$TMPDIR" "$W/home/.local/bin" "$W/bin"

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
KNOWN=$W/known_hosts
printf '[127.0.0.1]:%s %s\n[localhost]:%s %s\n' "$PORT" "$(cut -d' ' -f1,2 "$W/hostkey.pub")" "$PORT" "$(cut -d' ' -f1,2 "$W/hostkey.pub")" >"$KNOWN"
# meterm runs the ssh on PATH, and -J's hop runs the ssh that argv[0] says, so this holds
# for hops too. It keeps known_hosts out of the real home. `agents` has no -F to spare and
# won't take these options from extra_args, which is the point.
cat >"$W/bin/ssh" <<CONF
#!/bin/bash
exec -a ssh /usr/bin/ssh -o UserKnownHostsFile=$KNOWN -o IdentityFile=$W/key -o IdentitiesOnly=yes "\$@"
CONF
chmod +x "$W/bin/ssh"
export PATH=$W/bin:$PATH
for i in $(seq 1 50); do grep -q 'Server listening' "$W/sshd.log" 2>/dev/null && break; sleep 0.1; done

cat >"$METERM_CONFIG" <<CONF
version: 1
hosts:
- {name: box, address: 127.0.0.1, port: $PORT, tags: [t]}
- {name: live, address: localhost, port: $PORT, tags: [t], env: production}
- {name: dead, address: 127.0.0.1, port: $DEAD}
- {name: x-bare, address: 127.0.0.1, port: $PORT, extra_args: [127.0.0.1]}
- {name: x-proxy, address: 127.0.0.1, port: $PORT, extra_args: [-o, "ProxyCommand=touch $W/pwned"]}
- {name: x-agent, address: 127.0.0.1, port: $PORT, extra_args: [-A]}
- {name: x-master, address: 127.0.0.1, port: $PORT, extra_args: [-M, -S, $W/sock]}
- {name: x-ok, address: 127.0.0.1, port: $PORT, extra_args: [-o, ConnectTimeout=5, -C]}
- {name: via-live, address: 127.0.0.1, port: $PORT, tags: [j], proxy_jump: live}
- {name: via-user, address: 127.0.0.1, port: $PORT, tags: [j], proxy_jump: "bcd@live"}
- {name: via-addr, address: 127.0.0.1, port: $PORT, tags: [j], proxy_jump: "localhost:$PORT"}
- {name: via-uri, address: 127.0.0.1, port: $PORT, tags: [j], proxy_jump: "ssh://localhost:$PORT"}
- {name: via-route, address: 127.0.0.1, port: $DEAD, tags: [j], routes: [{name: r, proxy_jump: live}]}
- {name: hop, address: 127.0.0.1, port: $PORT}
- {name: via-hop, address: localhost, port: $PORT, proxy_jump: hop}
- {name: routed, address: 127.0.0.1, port: $DEAD, routes: [{name: r, address: 127.0.0.1, port: $PORT}]}
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

echo "== nothing is written, and every look is a login of its own"
[ ! -e "$METERM_ACTIVITY_DIR" ] && [ ! -e "$METERM_HISTORY" ] && [ ! -e "$METERM_TEAM_DIR" ] && [ ! -e "$W/routes.json" ] || fail "something was recorded"
before=$(logins)
run 0 box
run 0 box
[ "$(logins)" = "$((before + 2))" ] || fail "logins went from $before to $(logins), wanted two more"
pgrep -f "^ssh .*$W/key" >/dev/null && fail "an ssh stayed behind"
[ -z "$(ls "$TMPDIR")" ] || fail "left in TMPDIR: $(ls "$TMPDIR")"
echo "ok: two looks, two logins, nothing left running or on disk"

echo "== a revoked key stops working at the next look"
mv "$W/authorized_keys" "$W/authorized_keys.off"
run 2 box --format json
python3 -I -c 'import json,sys; h=json.loads(sys.argv[1])["hosts"][0]; assert h["agents"] is None and "refused the key" in h["error"], h' "$OUT" || fail "$OUT"
mv "$W/authorized_keys.off" "$W/authorized_keys"
echo ok

echo "== extra_args can't make it something else"
before=$(logins)
for h in x-bare x-proxy x-agent x-master; do
  run 2 $h --format json
  python3 -I -c 'import json,sys; h=json.loads(sys.argv[1])["hosts"][0]; assert h["reachable"] is False and h["agents"] is None and "extra_args" in h["error"], h' "$OUT" || fail "$h: $OUT"
  echo "   $h: $(python3 -I -c 'import json,sys; print(json.loads(sys.argv[1])["hosts"][0]["error"][:70])' "$OUT")"
done
[ "$(logins)" = "$before" ] || fail "ssh ran for a host with hostile extra_args"
[ ! -e "$W/pwned" ] || fail "ProxyCommand ran"
run 0 x-ok
echo ok

echo "== production: named it's asked; by tag it's refused, by any route"
before=$(logins)
run 0 live
[ "$(logins)" = "$((before + 1))" ] || fail "named production wasn't asked"
before=$(logins)
run 2 --tag t --format json
python3 -I -c '
import json, sys
h = {x["host"]: x for x in json.loads(sys.argv[1])["hosts"]}
assert h["box"]["error"] == "" and h["box"]["agents"]
assert h["live"]["reachable"] is False and h["live"]["agents"] is None and "refused" in h["live"]["error"]
' "$OUT" || fail "got $OUT"
[ "$(logins)" = "$((before + 1))" ] || fail "something else logged in"
before=$(logins)
run 2 --tag j --format json
python3 -I -c '
import json, sys
hs = json.loads(sys.argv[1])["hosts"]
assert len(hs) == 5
for h in hs:
    assert "refused" in h["error"] and h["agents"] is None, h
' "$OUT" || fail "got $OUT"
[ "$(logins)" = "$before" ] || fail "logged in through production on a poll"
run 0 via-live
echo "ok: refused by tag through name, user@name, address, ssh:// and saved route; via-live named still works"

echo "== a look writes no state, and an unknown host key is an error"
sum=$(shasum "$KNOWN")
: >"$W/empty_known"
cp "$KNOWN" "$W/known.keep"; : >"$KNOWN"
run 2 box --format json
python3 -I -c 'import json,sys; h=json.loads(sys.argv[1])["hosts"][0]; assert h["agents"] is None and "trusted" in h["error"], h' "$OUT" || fail "$OUT"
[ ! -s "$KNOWN" ] || fail "a key was accepted into known_hosts"
cp "$W/known.keep" "$KNOWN"
[ "$(shasum "$KNOWN")" = "$sum" ] || fail "known_hosts changed"
run 0 routed
[ ! -e "$W/routes.json" ] || fail "routes.json was written"
[ "$(shasum "$KNOWN")" = "$sum" ] || fail "known_hosts changed through a route"
t0=$(python3 -I -c 'import time; print(time.time())')
python3 -I - "$W" <<'PY' &
import socket, sys, time
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("127.0.0.1", 0)); s.listen(50)
open(sys.argv[1] + "/silent.port", "w").write(str(s.getsockname()[1]))
keep = []
while True: keep.append(s.accept())
PY
SILENT_PID=$!
for i in $(seq 1 50); do [ -s "$W/silent.port" ] && break; sleep 0.1; done
SP=$(cat "$W/silent.port")
cat >>"$METERM_CONFIG" <<CONF
- {name: silent, address: 127.0.0.1, port: $SP, routes: [{name: a, address: 127.0.0.1, port: $SP}, {name: b, address: 127.0.0.1, port: $SP}, {name: c, address: 127.0.0.1, port: $SP}]}
CONF
run 2 silent --format json --timeout 1s
dt=$(python3 -I -c "import time; print(round(time.time()-$t0, 1))")
kill "$SILENT_PID" 2>/dev/null || true
python3 -I -c "import sys; assert $dt < 3, $dt" || fail "--timeout 1s took ${dt}s with routes that never greet"
echo "ok: no known_hosts or routes.json change; --timeout 1s took ${dt}s through silent routes"

echo "== options reach a jump hop"
before=$(logins)
run 0 via-hop
[ "$(logins)" = "$((before + 2))" ] || fail "wanted a login for the hop and one for the host"
# The hop's key unknown: an error, and nothing added, with BatchMode from the -F file.
: >"$KNOWN"
run 2 via-hop --format json
[ ! -s "$KNOWN" ] || fail "the hop's key was accepted"
cp "$W/known.keep" "$KNOWN"
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
