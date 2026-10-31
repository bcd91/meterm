#!/usr/bin/env bash
# Integration test for `meterm copy-id --via` against a real sshd. It changes
# the machine's sshd config and adds accounts: run it only on a throwaway CI
# runner (with passwordless sudo), never on a real server.
set -euo pipefail
trap 'echo "FAIL: line $LINENO: $BASH_COMMAND"; sudo tail -n 30 /var/log/auth.log 2>/dev/null || sudo journalctl -u ssh -n 30 --no-pager 2>/dev/null || true' ERR
[ "${CI:-}" = true ] || { echo "refusing to run outside CI"; exit 1; }

ME=$(id -un)
W=$(mktemp -d)
export METERM_CONFIG=$W/hosts.yaml METERM_HISTORY=$W/history.json METERM_ACTIVITY_DIR=$W/activity METERM_NO_AUTOSYNC=1 METERM_TEAM_DIR=$W/team
M=$W/meterm
go build -o "$M" ./cmd/meterm

if ! command -v sshd >/dev/null && [ ! -x /usr/sbin/sshd ]; then
  sudo apt-get update -qq && sudo apt-get install -y -qq openssh-server
fi
[ -f ~/.ssh/id_ed25519 ] || ssh-keygen -q -t ed25519 -N '' -f ~/.ssh/id_ed25519
mkdir -p ~/.ssh && chmod 700 ~/.ssh
cat ~/.ssh/id_ed25519.pub >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys
chmod go-w ~
printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\nAllowUsers %s\n' "$ME" | sudo tee /etc/ssh/sshd_config.d/05-it.conf >/dev/null
printf 'DenyUsers bsdeny\n' | sudo tee /etc/ssh/sshd_config.d/04-it-deny.conf >/dev/null
sudo systemctl enable --now ssh 2>/dev/null || true
sudo systemctl restart ssh 2>/dev/null || sudo service ssh restart
for i in 1 2 3 4 5 6 7 8 9 10; do ssh-keyscan -q 127.0.0.1 >/dev/null 2>&1 && break; sleep 1; done
echo "sshd: $(sudo sshd -T | grep -E '^(allowusers|denyusers|passwordauthentication) ' | tr '\n' ' ')"
printf 'Host 127.0.0.1\n  StrictHostKeyChecking accept-new\n' >> ~/.ssh/config && chmod 600 ~/.ssh/config
ssh -o BatchMode=yes "$ME"@127.0.0.1 true || { echo "the via account can't log in to the test sshd"; exit 1; }
for u in bsit bsx bsdeny; do sudo useradd -m -s /bin/bash "$u"; done
"$M" add LOCAL --address 127.0.0.1 --user "$ME" >/dev/null

pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; exit 1; }
KEYB=$(cut -d' ' -f2 ~/.ssh/id_ed25519.pub)

# Plain copy-id on a server without password logins points at --via.
out=$("$M" copy-id bsit@LOCAL </dev/null 2>&1 || true)
grep -q -- '--via' <<<"$out" && pass "plain copy-id suggests --via" || fail "no --via hint: $out"

# No terminal and no --yes: the key goes in, AllowUsers is left alone, and it says so.
if out=$("$M" copy-id bsit@LOCAL --via "$ME" </dev/null 2>&1); then fail "succeeded though bsit can't log in: $out"; fi
grep -q "AllowUsers" <<<"$out" && pass "reports AllowUsers in the way" || fail "$out"
grep -qx "AllowUsers $ME" /etc/ssh/sshd_config.d/05-it.conf && pass "AllowUsers untouched without --yes" || fail "AllowUsers changed"
sudo grep -q "$KEYB" ~bsit/.ssh/authorized_keys && pass "key installed for bsit" || fail "key missing"
[ "$(sudo stat -c %U ~bsit/.ssh/authorized_keys)" = bsit ] && pass "authorized_keys owned by bsit" || fail "wrong owner"

# --yes: AllowUsers gets bsit, sshd reloads, the login works, the host lists bsit.
out=$("$M" copy-id bsit@LOCAL --via "$ME" --yes </dev/null 2>&1) || fail "$out"
grep -qx "AllowUsers $ME bsit" /etc/ssh/sshd_config.d/05-it.conf && pass "bsit added to AllowUsers" || fail "$(cat /etc/ssh/sshd_config.d/05-it.conf)"
ls /etc/ssh/sshd_config.d/05-it.conf.meterm-bak-* >/dev/null && pass "backup kept" || fail "no backup"
[ "$(ssh -o BatchMode=yes bsit@127.0.0.1 whoami)" = bsit ] && pass "bsit logs in with the key" || fail "login failed"
grep -q "bsit" "$METERM_CONFIG" && pass "host's users has bsit" || fail "users not updated"

# Again: nothing doubles.
"$M" copy-id bsit@LOCAL --via "$ME" --yes </dev/null >/dev/null 2>&1 || fail "rerun failed"
[ "$(sudo grep -c "$KEYB" ~bsit/.ssh/authorized_keys)" = 1 ] && pass "key not duplicated" || fail "key duplicated"
[ "$(grep -o bsit /etc/ssh/sshd_config.d/05-it.conf | wc -l)" = 1 ] && pass "AllowUsers not duplicated" || fail "AllowUsers duplicated"

# A symlinked ~/.ssh can't make root write elsewhere: the write is as the account.
sudo rm -rf ~bsx/.ssh && sudo -u bsx ln -s /root/.ssh ~bsx/.ssh
sudo mkdir -p /root/.ssh && sudo touch /root/.ssh/authorized_keys
if "$M" copy-id bsx@LOCAL --via "$ME" --yes </dev/null >/dev/null 2>&1; then fail "symlinked .ssh accepted"; fi
sudo grep -q "$KEYB" /root/.ssh/authorized_keys && fail "wrote root's authorized_keys through a symlink" || pass "symlink can't redirect the write"

# DenyUsers is reported, never edited.
out=$("$M" copy-id bsdeny@LOCAL --via "$ME" --yes </dev/null 2>&1) && fail "DenyUsers account accepted"
grep -q DenyUsers <<<"$out" && pass "DenyUsers reported" || fail "$out"
grep -qx "DenyUsers bsdeny" /etc/ssh/sshd_config.d/04-it-deny.conf && pass "DenyUsers untouched" || fail "DenyUsers changed"

# Refused before anything runs.
for bad in nosuchuser root "$ME"; do
  "$M" copy-id "$bad@LOCAL" --via "$ME" --yes </dev/null >/dev/null 2>&1 && fail "accepted $bad"
done
pass "refuses missing account, root, and the via account itself"
"$M" copy-id '-oProxyCommand=touch /tmp/PWNED@LOCAL' --via "$ME" --yes </dev/null >/dev/null 2>&1 || true
[ ! -e /tmp/PWNED ] && pass "option-shaped account name refused" || fail "PWNED"

# sshd still fine for everyone at the end.
[ "$(ssh -o BatchMode=yes "$ME"@127.0.0.1 whoami)" = "$ME" ] && pass "via account still logs in" || fail "locked out"
echo "all copy-id --via checks passed"
