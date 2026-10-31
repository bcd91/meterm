package relay

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

// Adding a machine nobody can reach yet: someone at the machine runs an
// invite script once, and the admin accepts the reply.
//
//   - `relay invite` writes a script holding nothing secret: the relay's
//     addresses and host keys (as the admin already trusts them), the
//     tunnel port, and the admin's public key to log in with.
//   - Run with sudo on the machine, it installs that public key for the
//     login user, creates the tunnel key there (the private half never
//     leaves the machine), starts the tunnel unit (which retries until the
//     relay accepts the key) and prints one reply line.
//   - `relay accept` takes the reply — untrusted input, checked field by
//     field — authorizes the tunnel key on the relay, records the
//     machine's host keys in known_hosts (so the first connection isn't a
//     blind leap) and saves the machine as a host reached through the relay.

// InviteParams is what an invite script needs.
type InviteParams struct {
	NodeParams
	Name     string // what the machine will be called in meterm
	AdminKey string // public key to install for the login user
	User     string // login user to install it for; "" = whoever runs sudo
}

var loginRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)

// InviteScript is the script for the person at the new machine to run
// with sudo.
func InviteScript(p InviteParams) (string, error) {
	if !pubkeyRE.MatchString(strings.TrimSpace(p.AdminKey)) {
		return "", fmt.Errorf("not a public key: %q", p.AdminKey)
	}
	if p.Name == "" || p.Name != safe(p.Name) || strings.HasPrefix(p.Name, "-") {
		return "", fmt.Errorf("machine name %q: use letters, digits, '.', '_' and '-'", p.Name)
	}
	if p.User != "" && !loginRE.MatchString(p.User) {
		return "", fmt.Errorf("user %q isn't a plain login name", p.User)
	}
	node, err := NodeScript(p.NodeParams)
	if err != nil {
		return "", err
	}
	adminKey := strings.Join(strings.Fields(p.AdminKey), " ")
	return `#!/bin/sh
# meterm relay invite: run this once on the new machine, with sudo:
#     sudo sh ` + "<this file>" + `
# It installs the admin's public key for the login user, creates a key for
# the tunnel (the private half stays on this machine), and starts a systemd
# unit that keeps an outbound SSH tunnel to the relay ` + safe(p.Relay) + `.
# It changes nothing else, and contains no secrets.
set -e
[ "$(id -u)" = 0 ] || { echo "Run it with sudo: sudo sh $0"; exit 1; }
U='` + p.User + `'
[ -n "$U" ] || U="${SUDO_USER:-}"
case "$U" in ''|root|*[!A-Za-z0-9._-]*|-*) echo "METERM-ERR: run with sudo as the user the admin will log in as (or ask for an invite naming the user)"; exit 1 ;; esac
getent passwd "$U" >/dev/null || { echo "METERM-ERR: no user $U here"; exit 1; }
H=$(getent passwd "$U" | cut -d: -f6)
[ -n "$H" ] && [ -d "$H" ] || { echo "METERM-ERR: $U has no home directory"; exit 1; }
case "$H" in /*) ;; *) echo "METERM-ERR: odd home directory for $U"; exit 1 ;; esac
case "$H" in *[!A-Za-z0-9/._-]*) echo "METERM-ERR: odd home directory for $U"; exit 1 ;; esac
# Refuse symlinks: root must not be tricked into writing somewhere else.
for p in "$H/.ssh" "$H/.ssh/authorized_keys"; do [ -L "$p" ] && { echo "METERM-ERR: $p is a symlink; not touching it"; exit 1; }; done
# Written as $U, not root: nothing in $U's home can redirect the write.
su -s /bin/sh "$U" -c "umask 077; mkdir -p '$H/.ssh'; touch '$H/.ssh/authorized_keys'; chmod 700 '$H/.ssh'; chmod 600 '$H/.ssh/authorized_keys'; grep -qxF '` + adminKey + `' '$H/.ssh/authorized_keys' || printf '%s\n' '` + adminKey + `' >> '$H/.ssh/authorized_keys'"
` + strings.TrimPrefix(node, "set -e\n") + `
HOSTKEYS=$(cat /etc/ssh/ssh_host_*_key.pub 2>/dev/null | awk '$1 != "ssh-dss" {print "hostkey " $1 " " $2}')
[ -n "$HOSTKEYS" ] || { echo "METERM-ERR: no SSH host keys in /etc/ssh (is sshd installed?)"; exit 1; }
REPLY=$(printf 'tunnel %s\nuser %s\n%s\n' "$(cut -d' ' -f1,2 $D/id_ed25519.pub)" "$U" "$HOSTKEYS" | base64 | tr -d '\n')
echo
echo "Done. The tunnel will connect as soon as the admin accepts it."
echo "Send this line to your meterm admin (it holds no secrets):"
echo
echo "  meterm relay accept ` + p.Name + ` $REPLY"
echo
`, nil
}

// Reply is what an invited machine sends back.
type Reply struct {
	TunnelKey string   // "type base64"
	User      string   // login user the admin's key went to
	HostKeys  []string // "type base64", each of the machine's host keys
}

var hostKeyRE = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/]+={0,2}$`)

var dsaKeyRE = regexp.MustCompile(`^ssh-dss [A-Za-z0-9+/]+={0,2}$`)

// ParseReply reads and checks a reply. It comes from the machine being
// added, so every field is held to a strict shape: nothing in it can reach
// a shell or a config file as anything but a key or a login name.
func ParseReply(blob string) (Reply, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(blob))
	if err != nil {
		return Reply{}, fmt.Errorf("the reply isn't valid (base64: %v)", err)
	}
	if len(raw) > 16<<10 {
		return Reply{}, fmt.Errorf("the reply is too long")
	}
	var r Reply
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		kind, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch kind {
		case "tunnel":
			if r.TunnelKey != "" || !hostKeyRE.MatchString(rest) {
				return Reply{}, fmt.Errorf("the reply's tunnel key isn't valid")
			}
			r.TunnelKey = rest
		case "user":
			if r.User != "" || !loginRE.MatchString(rest) || rest == "root" {
				return Reply{}, fmt.Errorf("the reply's user isn't valid")
			}
			r.User = rest
		case "hostkey":
			if dsaKeyRE.MatchString(rest) {
				continue // DSA: disabled in OpenSSH since 7.0; never trusted, just skipped
			}
			if !hostKeyRE.MatchString(rest) {
				return Reply{}, fmt.Errorf("a host key in the reply isn't valid")
			}
			r.HostKeys = append(r.HostKeys, rest)
		case "":
		default:
			return Reply{}, fmt.Errorf("the reply has an unexpected line %q", kind)
		}
	}
	if r.TunnelKey == "" || r.User == "" || len(r.HostKeys) == 0 {
		return Reply{}, fmt.Errorf("the reply is incomplete")
	}
	if len(r.HostKeys) > 8 {
		return Reply{}, fmt.Errorf("the reply has too many host keys")
	}
	return r, nil
}
