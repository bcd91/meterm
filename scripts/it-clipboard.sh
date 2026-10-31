#!/usr/bin/env bash
# What reaches the outer terminal through `meterm dev`: OSC 52 (clipboard) and
# notifications, sent from a pane of the remote tmux. The outer terminal is a pty
# that this script reads byte by byte, with TERM=xterm-256color as Ghostty's sessions
# get. Everything runs from a temp directory: its own sshd on a loopback port, keys,
# remote HOME and tmux socket directory. It prints what it found, and fails only when
# something differs from what the README says.
# Needs: sshd, tmux, python3.
set -euo pipefail

W=$(mktemp -d /tmp/mtc.XXXXXX)
W=$(cd "$W" && pwd -P)
SSHD_PID=""
cleanup() {
  TMUX_TMPDIR=$W/remote tmux kill-server 2>/dev/null || true
  [ -z "$SSHD_PID" ] || kill "$SSHD_PID" 2>/dev/null || true
  rm -rf "$W"
}
trap cleanup EXIT
free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }

go build -o "$W/meterm" ./cmd/meterm
export METERM_CONFIG=$W/hosts.yaml METERM_ACTIVITY_DIR=$W/activity METERM_HISTORY=$W/history.json
export METERM_TEAM_DIR=$W/team METERM_RUNTIME_DIR=$W/run METERM_NO_AUTOSYNC=1
mkdir -p "$W/remote" "$W/home" "$W/bin"
printf "PROMPT='READY> '\n" >"$W/home/.zshrc"
printf "PS1='READY> '\n" >"$W/home/.bashrc"

PORT=$(free_port)
ssh-keygen -q -t ed25519 -N '' -f "$W/hostkey"
ssh-keygen -q -t ed25519 -N '' -f "$W/key"
cp "$W/key.pub" "$W/authorized_keys"; chmod 600 "$W/authorized_keys"
TMUX_DIR=$(dirname "$(command -v tmux)")
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
SetEnv TMUX_TMPDIR=$W/remote HOME=$W/home PATH=$TMUX_DIR:/usr/bin:/bin
CONF
/usr/sbin/sshd -D -f "$W/sshd_config" -E "$W/sshd.log" &
SSHD_PID=$!
sleep 0.5
printf '[127.0.0.1]:%s %s\n' "$PORT" "$(cut -d' ' -f1,2 "$W/hostkey.pub")" >"$W/known_hosts"
cat >"$W/ssh_config" <<CONF
UserKnownHostsFile $W/known_hosts
StrictHostKeyChecking yes
IdentityFile $W/key
IdentitiesOnly yes
CONF
cat >"$METERM_CONFIG" <<CONF
version: 1
hosts:
- {name: box, address: 127.0.0.1, port: $PORT, extra_args: [-F, $W/ssh_config]}
CONF

cat >"$W/probe.py" <<'PY'
import os, pty, re, select, subprocess, sys, time

W = os.environ["W"]
OSC52 = rb"\x1b\]52;[a-z]*;TUVURVJ[A-Za-z0-9+/=]*(\x07|\x1b\\)"  # METERM, in base64
results = []

def session(outer_term, conf, script, expect, what, also=None):
    subprocess.run(["tmux", "kill-server"], env={**os.environ, "TMUX_TMPDIR": W + "/remote"}, stderr=subprocess.DEVNULL)
    time.sleep(0.2)
    with open(W + "/home/.tmux.conf", "w") as f:
        f.write(conf)
    with open(W + "/home/send.sh", "w") as f:
        f.write(script)
    pid, fd = pty.fork()
    if pid == 0:
        os.environ["TERM"] = outer_term
        os.execv(W + "/meterm", [W + "/meterm", "dev", "box", "--session", "probe", "--no-reconnect"])
    out = b""
    def read_for(seconds, until=None):
        nonlocal out
        end = time.time() + seconds
        while time.time() < end:
            r, _, _ = select.select([fd], [], [], 0.1)
            if r:
                try:
                    data = os.read(fd, 65536)
                except OSError:
                    return
                if not data:
                    return
                out += data
            if until and re.search(until, out):
                return
    # Keys sent before tmux is up are thrown away when it starts.
    read_for(15, rb"READY>")
    read_for(0.3)
    start = len(out)
    os.write(fd, b"sh ~/send.sh; printf 'DONE''MARK\\n'\r")
    read_for(6, rb"DONEMARK")
    read_for(0.5)
    got = out[start:]
    seen = bool(re.search(expect, got))
    results.append((what, seen, also(got) if also else ""))
    os.close(fd)
    try:
        os.kill(pid, 15)
        os.waitpid(pid, 0)
    except OSError:
        pass

app = r"printf '\033]52;c;TUVURVJN\007'" "\n"
wrapped = r"printf '\033Ptmux;\033\033]52;c;TUVURVJN\007\033\\'" "\n"
copymode = "clear; printf 'METERM\\n'; tmux copy-mode; tmux send-keys -X top-line; tmux send-keys -X start-of-line; " \
    "tmux send-keys -X begin-selection; tmux send-keys -X end-of-line; tmux send-keys -X copy-selection-and-cancel\n"
notify777 = r"printf '\033]777;notify;TITLE;BODY\007'" "\n"
notify9 = r"printf '\033]9;BODY\007'" "\n"
termname = "tmux display-message -p 'client=#{client_termname} features=#{client_termfeatures}'\n"

XT = "xterm-256color"
session(XT, "", app, OSC52, "app OSC 52, tmux defaults")
session(XT, "set -s set-clipboard on\n", app, OSC52, "app OSC 52, set-clipboard on")
session(XT, "", copymode, OSC52, "tmux's own copy-mode, tmux defaults")
session(XT, "set -s set-clipboard off\n", copymode, OSC52, "tmux's own copy-mode, set-clipboard off")
session(XT, "", wrapped, OSC52, "app OSC 52 in a passthrough wrapper, tmux defaults")
session(XT, "set -g allow-passthrough on\n", wrapped, OSC52, "app OSC 52 in a passthrough wrapper, allow-passthrough on")
session(XT, "", notify777, rb"\x1b\]777;", "OSC 777 notification, tmux defaults")
session(XT, "", notify9, rb"\x1b\]9;", "OSC 9 notification, tmux defaults")
session("xterm-ghostty", "", termname, rb"client=xterm-256color", "TERM=xterm-ghostty here reaches the host as xterm-256color")

# Whether the option can be had for one session, as `dev` would need.
env = {**os.environ, "TMUX_TMPDIR": W + "/remote"}
open(W + "/home/.tmux.conf", "w").close()
subprocess.run(["tmux", "kill-server"], env=env, stderr=subprocess.DEVNULL)
time.sleep(0.2)
subprocess.run(["tmux", "new-session", "-d", "-s", "one"], env=env, check=True)
subprocess.run(["tmux", "new-session", "-d", "-s", "two"], env=env, check=True)
subprocess.run(["tmux", "set-option", "-t", "one", "set-clipboard", "on"], env=env)
two = subprocess.run(["tmux", "show-options", "-s", "-v", "set-clipboard"], env=env, capture_output=True, text=True).stdout.strip()
results.append(("set-clipboard set for one session changes every session", two == "on", two))

want = {
    "app OSC 52, tmux defaults": False,
    "app OSC 52, set-clipboard on": True,
    "tmux's own copy-mode, tmux defaults": True,
    "tmux's own copy-mode, set-clipboard off": False,
    "app OSC 52 in a passthrough wrapper, tmux defaults": False,
    "app OSC 52 in a passthrough wrapper, allow-passthrough on": True,
    "OSC 777 notification, tmux defaults": False,
    "OSC 9 notification, tmux defaults": False,
    "TERM=xterm-ghostty here reaches the host as xterm-256color": True,
    "set-clipboard set for one session changes every session": True,
}
bad = 0
for what, seen, note in results:
    mark = "yes" if seen else "no "
    flag = "" if want[what] == seen else "   <-- README says otherwise"
    bad += want[what] != seen
    print(f"{mark}  {what}{flag}")
sys.exit(1 if bad else 0)
PY
W=$W python3 -I "$W/probe.py"
echo PASS
