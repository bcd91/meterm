#!/usr/bin/env bash
# What an agent, or anything else running as this user, can do to get around meterm. It
# tries each one for real, against its own sshd on a loopback port, with a temp HOME holding
# its own keys, hosts and team repository. It touches nothing of yours and connects nowhere
# else. Needs: sshd, python3, git.
#
# Each row says what happens today. A row that stops being true fails the script: if meterm
# ever blocks one of these, this table and the README have to change, and if a far-side limit
# stops holding, that's worse. "not blocked" is the expected result of most rows, on purpose.
set -euo pipefail

W=$(mktemp -d /tmp/mtb.XXXXXX)
W=$(cd "$W" && pwd -P)
ROOT=$(cd "$(dirname "$0")/.." && pwd -P)
SSHD_PID=""
# meterm starts a short background upload after each command. Files can't be swapped under it.
settle() { for _ in $(seq 1 200); do pgrep -f "^$W/meterm team background" >/dev/null || return 0; sleep 0.1; done; }
cleanup() {
  trap - EXIT
  settle
  [ -z "$SSHD_PID" ] || kill "$SSHD_PID" 2>/dev/null || true
  rm -rf "$W"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP
free_port() { python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }

(cd "$ROOT" && go build -o "$W/meterm" ./cmd/meterm)
M=$W/meterm
for v in $(env | sed -n 's/^\(METERM_[A-Za-z_]*\)=.*/\1/p'); do unset "$v"; done
unset XDG_CONFIG_HOME CLAUDECODE AI_AGENT CLAUDE_CODE_SESSION_ID
export HOME=$W/home GIT_CONFIG_NOSYSTEM=1
export TMPDIR=$W/tmp
mkdir -p "$HOME/.ssh" "$W/bin" "$TMPDIR" "$W/far"
chmod 700 "$HOME/.ssh"
git config --global user.email agent@example.com
git config --global user.name agent
git config --global init.defaultBranch main

PORT=$(free_port)
ssh-keygen -q -t ed25519 -N '' -f "$W/hostkey"
ssh-keygen -q -t ed25519 -N '' -f "$HOME/.ssh/id_ed25519"
ssh-keygen -q -t ed25519 -N '' -f "$W/agent_key"

# The README's worked example, with a file in place of logger so it can be read back.
cat >"$W/far/agent-gate" <<GATE
#!/bin/sh
# Runs for every login with this key, whatever the client asked for.
printf '%s\n' "\$SSH_CONNECTION: \$SSH_ORIGINAL_COMMAND" >>"$W/far/gate.log"
case "\$SSH_ORIGINAL_COMMAND" in
  uptime|"df -h") exec \$SSH_ORIGINAL_COMMAND ;;
esac
echo "agent-gate: refused" >&2
exit 1
GATE
chmod +x "$W/far/agent-gate"
{
  cat "$HOME/.ssh/id_ed25519.pub"
  printf 'restrict,command="%s" %s\n' "$W/far/agent-gate" "$(cat "$W/agent_key.pub")"
} >"$W/authorized_keys"
chmod 600 "$W/authorized_keys"

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
SetEnv HOME=$W/far PATH=/usr/bin:/bin
CONF
/usr/sbin/sshd -D -f "$W/sshd_config" -E "$W/sshd.log" &
SSHD_PID=$!
for _ in $(seq 1 50); do grep -q 'Server listening' "$W/sshd.log" 2>/dev/null && break; sleep 0.1; done

KNOWN=$W/known_hosts
printf '[127.0.0.1]:%s %s\n[localhost]:%s %s\n' "$PORT" "$(cut -d' ' -f1,2 "$W/hostkey.pub")" "$PORT" "$(cut -d' ' -f1,2 "$W/hostkey.pub")" >"$KNOWN"
# Plain ssh, for the rows that go round meterm: no config of yours, no known_hosts of yours, no
# key of yours. Which key each row uses is said on its own command line.
PLAIN=(/usr/bin/ssh -F /dev/null -o UserKnownHostsFile="$KNOWN" -o IdentitiesOnly=yes -o BatchMode=yes -o IdentityAgent=none)
# OpenSSH reads ~/.ssh from the password database, not from $HOME, and the -F file that `agents`
# writes Includes it. So this takes every Include out, and adds what the run needs. Nothing of
# the real ~/.ssh or /etc/ssh is read: check_isolated below proves it.
cat >"$W/bin/ssh" <<CONF
#!/bin/bash
cfg=/dev/null; args=()
while [ \$# -gt 0 ]; do
  case "\$1" in
    -F) cfg=\$2; shift 2 ;;
    *) args+=("\$1"); shift ;;
  esac
done
clean=\$(mktemp "$W/sshcfg.XXXXXX")
grep -v '^Include ' "\$cfg" >"\$clean" || true
exec -a ssh /usr/bin/ssh -F "\$clean" -o UserKnownHostsFile=$KNOWN -o IdentityFile=$HOME/.ssh/id_ed25519 -o IdentitiesOnly=yes -o IdentityAgent=none "\${args[@]}"
CONF
chmod +x "$W/bin/ssh"
unset SSH_AUTH_SOCK
check_isolated() {
  printf 'Include ~/.ssh/config\nInclude /etc/ssh/ssh_config\n' >"$W/include_probe"
  local read
  read=$("$W/bin/ssh" -F "$W/include_probe" -G -v 127.0.0.1 2>&1 | sed -n 's/^debug1: Reading configuration data //p')
  case "$read" in "$W"/sshcfg.*) ;; *) echo "FAIL: the ssh wrapper read: $read"; exit 1 ;; esac
}
check_isolated
export PATH=$W/bin:$PATH

CFG=$("$M" list --format json >/dev/null 2>&1; python3 -I -c '
import os, sys
h = os.environ["HOME"]
print(os.path.join(h, "Library/Application Support") if sys.platform == "darwin" else os.path.join(h, ".config"))')/meterm
mkdir -p "$CFG"
HOSTS=$CFG/hosts.yaml
LOG=$CFG/activity/events.jsonl
cat >"$HOSTS" <<CONF
version: 1
hosts:
- {name: box, address: 127.0.0.1, port: $PORT, tags: [t]}
- {name: live, address: localhost, port: $PORT, tags: [t], env: production}
- {name: narrow, address: 127.0.0.1, port: $PORT, identity_file: $W/agent_key, extra_args: [-o, IdentitiesOnly=yes]}
CONF

git init -q --bare "$W/team.git"
"$M" team git add ACME "$W/team.git" >/dev/null 2>&1
"$M" team share box --yes >/dev/null 2>&1
"$M" exec box -- uptime >/dev/null 2>&1
"$M" exec box -- uptime >/dev/null 2>&1
"$M" team sync --yes >/dev/null 2>&1
settle

count() { [ -f "$1" ] && wc -l <"$1" | tr -d ' ' || echo 0; }
FAILED=0
ROWS=$W/rows
: >"$ROWS"
row() { # <what was tried> <expected> <actual> <evidence>
  local mark=" "
  if [ "$2" != "$3" ]; then mark="!"; FAILED=$((FAILED + 1)); fi
  printf '%s|%s|%s|%s\n' "$mark" "$1" "$3" "$4" >>"$ROWS"
}
# Stands in for what a tampered log has to survive, each from the same starting point.
snapshot() { settle; rm -rf "$W/keep"; mkdir -p "$W/keep"; cp -R "$CFG/." "$W/keep/"; }
restore() { settle; rm -rf "$CFG"; mkdir -p "$CFG"; cp -R "$W/keep/." "$CFG/"; }
verify() { "$M" activity --verify >"$W/out" 2>&1; }

# ---- walking around it ------------------------------------------------------------------

before=$(count "$LOG")
out=$("${PLAIN[@]}" -i "$HOME/.ssh/id_ed25519" -p "$PORT" 127.0.0.1 'echo reached-$(id -un)' 2>&1) && rc=0 || rc=$?
case "$out" in reached-*) a="not blocked" ;; *) a=blocked ;; esac
[ "$(count "$LOG")" = "$before" ] && n="not logged" || n=logged
row 'run ssh directly with a key from ~/.ssh' "not blocked" "$a" "exit $rc, \"$out\", $n"

out=$("$M" export 2>&1) && rc=0 || rc=$?
FRAG=$(printf '%s\n' "$out" | sed -n 's/^Wrote //p' | head -1)
if [ -f "$FRAG" ] && out=$(/usr/bin/ssh -F "$FRAG" -o UserKnownHostsFile="$KNOWN" -o IdentitiesOnly=yes -o BatchMode=yes -o IdentityAgent=none -i "$HOME/.ssh/id_ed25519" box 'echo via-export' 2>&1); then a="not blocked"; else a=blocked; fi
row 'meterm export, then ssh with what it wrote' "not blocked" "$a" "\"$out\" using a file meterm wrote under HOME"

grep -q 'env: production' "$HOSTS" && a="not blocked" || a=blocked
row 'read hosts.yaml' "not blocked" "$a" "$(grep -c 'address:' "$HOSTS") hosts and which one is production, in a plain file"

before=$(count "$LOG")
out=$("${PLAIN[@]}" -i "$HOME/.ssh/id_ed25519" -p "$PORT" localhost 'echo production-reached' 2>&1) && a="not blocked" || a=blocked
[ "$(count "$LOG")" = "$before" ] && n="not logged" || n=logged
row 'connect to a production host with plain ssh' "not blocked" "$a" "\"$out\", $n"

out=$("$M" exec live -- echo production-through-meterm 2>&1 | head -1) || true
case "$out" in *production-through-meterm*) a="not blocked" ;; *) a=blocked ;; esac
row 'meterm exec on a production host, named' "not blocked" "$a" "ran, and logged: the skill asks for approval, the command doesn't"

out=$("$M" agents --tag t --format json 2>/dev/null) || true
python3 -I -c 'import json,sys; h={x["host"]:x for x in json.loads(sys.argv[1])["hosts"]}; sys.exit(0 if "refused" in h["live"]["error"] else 1)' "$out" && a=refused || a="not refused"
row 'meterm agents --tag reaching production' "refused" "$a" "a guide: the next rows go round it"

snapshot
python3 -I -c 'import re,sys; p=sys.argv[1]; s=open(p).read(); t=re.sub(r"\n\s*env: production","",s); assert t!=s; open(p,"w").write(t)' "$HOSTS"
out=$("$M" agents --tag t --format json 2>/dev/null) || true
python3 -I -c 'import json,sys; h={x["host"]:x for x in json.loads(sys.argv[1])["hosts"]}; sys.exit(1 if "refused" in h["live"]["error"] else 0)' "$out" && a="not blocked" || a=blocked
row 'edit hosts.yaml to drop env: production, then the same' "not blocked" "$a" "the label is in a file this account owns"
restore

# ---- the log ----------------------------------------------------------------------------

snapshot
python3 -I -c '
import sys
p = sys.argv[1]; ls = open(p).read().splitlines()
ls[0] = ls[0].replace("uptime", "nothing")
open(p, "w").write("\n".join(ls) + "\n")' "$LOG" && a="not blocked" || a=blocked
verify && d="not detected" || d=detected
row 'edit a line of the activity log' "not blocked, detected" "$a, $d" "$(tail -1 "$W/out")"
restore

snapshot
rm -f "$LOG" && a="not blocked" || a=blocked
verify && d="not detected" || d=detected
row 'delete the activity log file' "not blocked, detected" "$a, $d" "$(tail -1 "$W/out")"
restore

snapshot
python3 -I -c '
import json, sys
p = sys.argv[1]; ev = [json.loads(x) for x in open(p) if x.strip()]
for k in ("ref_seq", "exit_code", "duration_ms", "actor_type", "agent_kind", "agent_session_id", "attribution_source", "prev_ext", "ext_hash"):
    ev[-1].pop(k, None)
open(p, "w").write("".join(json.dumps(e, separators=(",", ":"), ensure_ascii=False) + "\n" for e in ev))' "$LOG" && a="not blocked" || a=blocked
verify && d="not detected" || d=detected
row 'strip the exit code and actor from the last line' "not blocked, detected" "$a, $d" "$(tail -1 "$W/out" | cut -c1-90)"
restore

snapshot
python3 -I -c 'import sys; p=sys.argv[1]; ls=open(p).read().splitlines(); open(p,"w").write("\n".join(ls[:-2])+"\n")' "$LOG"
rm -f "$CFG/activity/head" && a="not blocked" || a=blocked
verify && d="not detected" || d=detected
row 'cut the last lines and delete head' "not blocked, detected" "$a, $d" "$(tail -1 "$W/out" | cut -c1-90)"
"$M" exec box -- uptime >/dev/null 2>&1
verify && d="not detected" || d=detected
row '  the same, after the next command' "detected" "$d" "the upload cursor says how far it got, and numbering carries on from there"
restore

snapshot
python3 -I -c 'import sys; p=sys.argv[1]; ls=open(p).read().splitlines(); open(p,"w").write("\n".join(ls[:-2])+"\n")' "$LOG"
rm -f "$CFG/activity/head" "$CFG"/activity/uploaded-seq.*
"$M" exec box -- uptime >/dev/null 2>&1
verify && d="not detected" || d=detected
row 'cut lines, delete head and the cursor, run a command' "not detected" "$d" "a log-repair event is the only mark, and it stays"
restore

snapshot
# Recomputes every hash, with the same logic as the code, and the witness beside the log.
python3 -I - "$CFG/activity" <<'PY'
import hashlib, json, sys
d = sys.argv[1]
evs = [json.loads(x) for x in open(d + "/events.jsonl") if x.strip()]
def h(fs): return hashlib.sha256("".join("%d:%s|" % (len(f.encode()), f) for f in fs).encode()).hexdigest()
prev = ext = ""
for i, e in enumerate(evs):
    if i == 0: e["detail"] = "nothing to see"
    e["prev_hash"] = prev
    if prev == "" or ext or e.get("ext_hash"): e["prev_ext"] = ext
    if not e.get("prev_ext"): e.pop("prev_ext", None)
    # Go prints times as they're stored, so a round trip through JSON keeps them.
    e["hash"] = h([e["device"], str(e["seq"]), e["time"], e["user"], e["action"], e["host"], e["target"], e.get("detail", ""), e["prev_hash"]])
    e["ext_hash"] = h(["meterm-ext-1", e["hash"], e.get("prev_ext", ""), str(e["ref_seq"]) if e.get("ref_seq") else "",
                       str(e["exit_code"]) if "exit_code" in e else "", str(e["duration_ms"]) if "duration_ms" in e else "",
                       e.get("actor_type", ""), e.get("agent_kind", ""), e.get("agent_session_id", ""), e.get("attribution_source", "")])
    prev, ext = e["hash"], e["ext_hash"]
open(d + "/events.jsonl", "w").write("".join(json.dumps(e, separators=(",", ":"), ensure_ascii=False) + "\n" for e in evs))
open(d + "/head", "w").write("%d %s %s\n" % (evs[-1]["seq"], prev, ext))
PY
verify && d="not detected" || d=detected
row 'rewrite the log, every hash and the witness too' "not blocked, not detected" "not blocked, $d" "--verify: $(tail -1 "$W/out")"
"$M" team log --verify >"$W/out" 2>&1 && d="not detected" || d=detected
row '  the same, against the team copy (team log --verify)' "detected" "$d" "$(tail -1 "$W/out" | cut -c1-90)"
restore

snapshot
rm -rf "$CFG/activity" && a="not blocked" || a=blocked
verify && d="not detected" || d=detected
"$M" team log -n 0 >"$W/out2" 2>&1 || true
row 'delete the whole activity directory' "not blocked, not detected" "$a, $d" "--verify says OK on nothing; the team keeps $(grep -c exec "$W/out2") exec lines"
restore

# ---- the copies ------------------------------------------------------------------------

snapshot
"$M" team autosync off >/dev/null 2>&1 && a="not blocked" || a=blocked
before=$(cat "$CFG"/team/git/*/events/*.jsonl | wc -l | tr -d ' ')
"$M" exec box -- uptime >/dev/null 2>&1
settle
after=$(cat "$CFG"/team/git/*/events/*.jsonl | wc -l | tr -d ' ')
[ "$before" = "$after" ] && n="nothing uploaded" || n="uploaded"
row 'meterm team autosync off' "not blocked" "$a" "$n; the team sees silence, nothing more"
restore

snapshot
METERM_NO_AUTOSYNC=1 "$M" exec box -- uptime >/dev/null 2>&1 && a="not blocked" || a=blocked
row 'METERM_NO_AUTOSYNC=1' "not blocked" "$a" "no upload for that command"
restore

snapshot
before=$(count "$LOG")
METERM_ACTIVITY_DIR=$W/elsewhere "$M" exec box -- uptime >/dev/null 2>&1 && a="not blocked" || a=blocked
[ "$(count "$LOG")" = "$before" ] && [ -s "$W/elsewhere/events.jsonl" ] && n="the event went there, the real log has nothing" || n="the real log has it"
row 'METERM_ACTIVITY_DIR=somewhere else' "not blocked" "$a" "$n"
verify && d="--verify OK" || d="--verify fails"
row '  the same, seen from the real log' "--verify OK" "$d" "nothing there to say it happened"
restore

snapshot
sed_cfg=$W/other.yaml
printf 'version: 1\nhosts:\n- {name: quiet, address: 127.0.0.1, port: %s}\n' "$PORT" >"$sed_cfg"
METERM_CONFIG=$sed_cfg METERM_ACTIVITY_DIR=$W/elsewhere2 "$M" exec quiet -- uptime >/dev/null 2>&1 && a="not blocked" || a=blocked
row 'METERM_CONFIG=a hosts file of its own' "not blocked" "$a" "any host, any log, none of it the user's"
restore

snapshot
out=$(env -u CLAUDECODE -u AI_AGENT "$M" exec box -- uptime >/dev/null 2>&1; CLAUDECODE=1 CLAUDE_CODE_SESSION_ID=not-really "$M" exec box -- uptime >/dev/null 2>&1; "$M" activity --format json -n 0)
python3 -I -c '
import json, sys
ev = [e for e in json.loads(sys.argv[1]) if e["action"] == "exec"]
a, b = ev[-2], ev[-1]
sys.exit(0 if a["actor_type"] == "unknown" and b["actor_type"] == "agent" and b["agent_session_id"] == "not-really" and b["attribution_source"] == "declared" else 1)' "$out" && a="not blocked" || a=blocked
row 'set or clear the actor variables' "not blocked" "$a" "an agent reads as unknown, a person as an agent: attribution_source says declared"
restore

snapshot
S=$W/sink.jsonl
printf 'activity:\n  sink: %s\n' "$S" >>"$HOSTS"
"$M" exec box -- uptime >/dev/null 2>&1
[ -s "$S" ] || row 'a sink receives events' "yes" "no" ""
python3 -I -c 'import sys; p=sys.argv[1]; s=open(p).read(); open(p,"w").write(s[:s.index("activity:")])' "$HOSTS"
before=$(count "$S")
"$M" exec box -- uptime >/dev/null 2>&1
[ "$(count "$S")" = "$before" ] && a="not blocked" || a=blocked
row 'remove activity.sink from hosts.yaml' "not blocked" "$a" "the copy just stops; a path this account can write, it can also empty"
restore

# ---- what does hold: the far end ---------------------------------------------------------

G=$W/far/gate.log
NARROW=("${PLAIN[@]}" -i "$W/agent_key" -p "$PORT")
out=$("${NARROW[@]}" 127.0.0.1 uptime 2>&1) && a=allowed || a=refused
row 'restricted key: the one command it may run' "allowed" "$a" "$(printf '%s' "$out" | cut -c1-40)..."
for attempt in 'id' 'cat /etc/hosts' 'uptime; touch '"$W"'/pwned' 'uptime && touch '"$W"'/pwned' '$(touch '"$W"'/pwned)' 'sh -c "touch '"$W"'/pwned"'; do
  "${NARROW[@]}" 127.0.0.1 "$attempt" >"$W/out" 2>&1 && a=allowed || a=blocked
  [ -e "$W/pwned" ] && a="BROKEN: it ran"
  row "restricted key: $(printf '%s' "$attempt" | sed "s#$W#W#g" | cut -c1-34)" "blocked" "$a" "$(tail -1 "$W/out")"
done
"${NARROW[@]}" -tt 127.0.0.1 </dev/null >"$W/out" 2>&1 && a=allowed || a=blocked
row 'restricted key: an interactive shell' "blocked" "$a" "$(tail -1 "$W/out" | tr -d '\r' | cut -c1-60)"
"${NARROW[@]}" -W "127.0.0.1:$PORT" 127.0.0.1 </dev/null >"$W/out" 2>&1 && rc=0 || rc=$?
grep -q '^SSH-2' "$W/out" && a="not blocked" || a=blocked
row 'restricted key: tunnel to another port (-W)' "blocked" "$a" "$(tail -1 "$W/out" | cut -c1-60)"
"${NARROW[@]}" -N -o ExitOnForwardFailure=yes -R 0:127.0.0.1:1 127.0.0.1 >"$W/out" 2>&1 && a="not blocked" || a=blocked
row 'restricted key: reverse tunnel (-R)' "blocked" "$a" "$(tail -1 "$W/out" | cut -c1-60)"
/usr/bin/scp -F /dev/null -o UserKnownHostsFile="$KNOWN" -o IdentitiesOnly=yes -o BatchMode=yes -o IdentityAgent=none -i "$W/agent_key" -P "$PORT" "$W/hostkey.pub" 127.0.0.1:/tmp/mtb-not-here >"$W/out" 2>&1 && a="not blocked" || a=blocked
[ ! -e /tmp/mtb-not-here ] || { rm -f /tmp/mtb-not-here; a="not blocked"; }
row 'restricted key: scp a file there' "blocked" "$a" "no file written"
# Clearing every trace on this side changes nothing about the record over there.
tries=$(wc -l <"$G" | tr -d ' ')
rm -rf "$CFG/activity"
[ "$(wc -l <"$G" | tr -d ' ')" = "$tries" ] && [ "$tries" -gt 0 ] && a=kept || a=lost
row 'far-side record after wiping the local log' "kept" "$a" "$tries attempts in the gate's log on the far side"

out=$("$M" exec narrow -- uptime 2>&1 | head -1) || true
case "$out" in *load*) a=allowed ;; *) a=refused ;; esac
row 'meterm exec through it: one argument' "allowed" "$a" "a one-word command line reaches the gate as written"
out=$("$M" exec narrow -- 'df -h' 2>&1 | head -1) || true
case "$out" in *Filesystem*) a=allowed ;; *) a=refused ;; esac
row "meterm exec through it: 'df -h' as one string" "allowed" "$a" "one string is sent as written"
"$M" exec narrow -- df -h >"$W/out" 2>&1 && a=allowed || a=refused
row 'meterm exec through it: df -h as two arguments' "allowed" "$a" "plain words aren't quoted, so it reads the same"
"$M" exec narrow -- df '-h; touch '"$W"'/pwned' >"$W/out" 2>&1 && a=allowed || a=refused
[ -e "$W/pwned" ] && a="BROKEN: it ran"
row 'meterm exec through it: something else' "refused" "$a" "the gate decides, whatever came through meterm"

echo
python3 -I - "$ROWS" <<'PY'
import sys
rows = [l.rstrip("\n").split("|", 3) for l in open(sys.argv[1])]
w = max(len(r[1]) for r in rows)
v = max(len(r[2]) for r in rows)
print(f"  {'WHAT WAS TRIED':<{w}}  {'RESULT':<{v}}  EVIDENCE")
for m, what, result, ev in rows:
    print(f"{m} {what:<{w}}  {result:<{v}}  {ev}")
PY
echo
if [ "$FAILED" != 0 ]; then
  echo "FAIL: $FAILED row(s) marked ! aren't what the README says"
  tail -n 10 "$W/sshd.log"
  exit 1
fi
echo "PASS: every row is what the README says"
