package sshrun

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"meterm/internal/config"
)

// copy-id --via: installing a key for an account nobody can log in to yet,
// through one that can (and has sudo). That's the normal state of a
// hardened server — password logins off — where plain copy-id has no way in.
//
// The script runs as the via account and:
//  1. writes the key as the target account itself (sudo -u), so nothing in
//     that account's home — a symlinked .ssh, say — can make root write
//     somewhere else;
//  2. checks sshd's effective AllowUsers / AllowGroups / DenyUsers /
//     DenyGroups (sshd -T) and says plainly when the account would still be
//     refused, which is otherwise only "Permission denied (publickey)";
//  3. if AllowUsers is what's in the way, and the person agrees (or passed
//     --yes), adds the account to it: every file backed up first, sshd -t
//     before reloading, and the backups put back if the new config fails.
//
// Nothing the server says is trusted to choose what runs: the account name
// is checked here and the key travels base64-encoded.

var viaKeyRE = regexp.MustCompile(`^(ssh-[a-z0-9-]+|ecdsa-sha2-nistp(256|384|521)|sk-[a-z0-9@.-]+) [A-Za-z0-9+/]+={0,3}( [^\x00-\x1f\x7f]*)?$`)

// ViaOutcome markers the script prints, for the caller to read back.
const (
	ViaMarkKey     = "METERM-VIA-KEY:"    // installed | present
	ViaMarkAccess  = "METERM-VIA-ACCESS:" // ok | added | declined | denied:<why> | groups:<why> | unknown:<why>
	ViaMarkFailure = "METERM-VIA-ERR:"    // <message>
)

const viaScript = `set -e
T='%s'
YES='%s'
K=$(printf '%%s' '%s' | base64 -d)
say() { printf '%%s\n' "$*"; }
fail() { say "` + ViaMarkFailure + `$*"; exit 1; }
getent passwd "$T" >/dev/null 2>&1 || fail "there's no account $T on this machine (create it first, e.g. sudo adduser $T)"
command -v sudo >/dev/null 2>&1 || fail "sudo isn't installed"
say "meterm: sudo for $(id -un) on $(hostname) — needed to write as $T and to read sshd's settings"
sudo -v || fail "sudo refused"
printf '%%s\n' "$K" | sudo -H -u "$T" sh -c 'umask 077; d="$HOME/.ssh"; f="$d/authorized_keys"; mkdir -p "$d"; touch "$f"; chmod 700 "$d"; chmod 600 "$f"; k=$(cat); if grep -qxF "$k" "$f"; then echo present; else printf "%%s\n" "$k" >> "$f"; echo installed; fi' > /tmp/.meterm-via.$$ || { rm -f /tmp/.meterm-via.$$; fail "couldn't write $T's authorized_keys"; }
say "` + ViaMarkKey + `$(cat /tmp/.meterm-via.$$)"; rm -f /tmp/.meterm-via.$$

PATH="$PATH:/usr/sbin:/sbin"
EFF=$(sudo sshd -T 2>/dev/null) || { say "` + ViaMarkAccess + `unknown:sshd -T didn't run"; exit 0; }
list() { printf '%%s\n' "$EFF" | awk -v k="$1" '$1 == k { for (i = 2; i <= NF; i++) print $i }'; }
GROUPS_OF=$(id -Gn "$T")
in_words() { for w in $2; do [ "$w" = "$1" ] && return 0; done; return 1; }
for u in $(list denyusers); do [ "$u" = "$T" ] && { say "` + ViaMarkAccess + `denied:DenyUsers names $T"; exit 0; }; done
for g in $(list denygroups); do in_words "$g" "$GROUPS_OF" && { say "` + ViaMarkAccess + `denied:DenyGroups names $T's group $g"; exit 0; }; done
AG=$(list allowgroups)
if [ -n "$AG" ]; then
  ok=no; for g in $AG; do in_words "$g" "$GROUPS_OF" && ok=yes; done
  [ $ok = yes ] || { say "` + ViaMarkAccess + `groups:AllowGroups is $(echo $AG) and $T is in none of them"; exit 0; }
fi
AU=$(list allowusers)
[ -z "$AU" ] && { say "` + ViaMarkAccess + `ok"; exit 0; }
for u in $AU; do
  [ "$u" = "$T" ] && { say "` + ViaMarkAccess + `ok"; exit 0; }
  case "$u" in *'*'*|*'?'*|*@*) say "` + ViaMarkAccess + `unknown:AllowUsers has patterns ($u); check $T is covered"; exit 0 ;; esac
done
say "meterm: sshd's AllowUsers only lets these accounts in: $(echo $AU). $T isn't one of them."
if [ "$YES" = no ]; then
  say "` + ViaMarkAccess + `declined"; exit 0 # no terminal to ask on, and no --yes
fi
if [ "$YES" = ask ]; then
  printf 'Add %%s to AllowUsers on %%s? [y/N] ' "$T" "$(hostname)" >/dev/tty
  read a </dev/tty || a=
  case "$a" in y|Y|yes) ;; *) say "` + ViaMarkAccess + `declined"; exit 0 ;; esac
fi
FILES=$(sudo sh -c 'grep -lE "^[[:space:]]*AllowUsers" /etc/ssh/sshd_config /etc/ssh/sshd_config.d/*.conf 2>/dev/null' || true)
[ -n "$FILES" ] || fail "AllowUsers is set but not in /etc/ssh/sshd_config or sshd_config.d; add $T by hand"
STAMP=$(date +%%Y%%m%%d%%H%%M%%S)
for f in $FILES; do sudo cp -p "$f" "$f.meterm-bak-$STAMP"; done
for f in $FILES; do
  sudo sed -i -E "/^[[:space:]]*AllowUsers/{/[[:space:]]$T([[:space:]]|\$)/!s/\$/ $T/}" "$f"
done
if ! sudo sshd -t; then
  for f in $FILES; do sudo mv "$f.meterm-bak-$STAMP" "$f"; done
  fail "the new sshd config didn't pass sshd -t; put the old one back, nothing changed"
fi
if command -v systemctl >/dev/null 2>&1; then
  sudo systemctl reload ssh 2>/dev/null || sudo systemctl reload sshd 2>/dev/null || fail "sshd config updated but reloading sshd failed; reload it by hand"
else
  sudo kill -HUP "$(cat /run/sshd.pid 2>/dev/null || cat /var/run/sshd.pid)" || fail "sshd config updated but reloading sshd failed; reload it by hand"
fi
say "meterm: added $T to AllowUsers in $(echo $FILES) (backups: *.meterm-bak-$STAMP)"
say "` + ViaMarkAccess + `added"
`

// CopyIDViaCommand runs the --via script as r's user (who needs sudo),
// installing pubKey for target. yes skips the AllowUsers question; tty
// asks ssh for a terminal (sudo's password prompt, the question).
func CopyIDViaCommand(r config.Resolved, target, pubKey string, yes, tty bool) (*exec.Cmd, error) {
	if err := config.CheckUser(target); err != nil || target == "" {
		return nil, fmt.Errorf("account %q isn't a plain login name", target)
	}
	if target == "root" {
		return nil, fmt.Errorf("not for root: log in as an account and use sudo")
	}
	if target == r.User {
		return nil, fmt.Errorf("--via %s is the account you're installing for; use plain copy-id", target)
	}
	key := strings.TrimSpace(pubKey)
	if strings.ContainsAny(key, "\r\n") || !viaKeyRE.MatchString(key) {
		return nil, fmt.Errorf("that doesn't look like a single public key line")
	}
	y := "no" // AllowUsers in the way: leave it
	switch {
	case yes:
		y = "yes"
	case tty:
		y = "ask"
	}
	script := fmt.Sprintf(viaScript, target, y, base64.StdEncoding.EncodeToString([]byte(key)))
	remote := "sh -c 'printf %s " + base64.StdEncoding.EncodeToString([]byte(script)) + " | base64 -d | sh'"
	var extra []string
	if tty {
		extra = append(extra, "-t")
	}
	return Command(r, append(extra, remote)...), nil
}
