// Package relay sets up reverse SSH tunnels so a machine nobody can connect
// to directly (behind NAT, CGNAT, a closed cloud firewall) can still be
// reached through a relay host everyone can reach.
//
// The node keeps one outbound connection to the relay, as a dedicated
// account that can do nothing but listen on one localhost port there:
//
//	node ──ssh -R localhost:P:localhost:22──▶ relay (user meterm-relay)
//
// Everyone else jumps through the relay as usual and lands on localhost:P,
// which is the node's sshd. meterm records that as one of the node's routes
// (address 127.0.0.1, port P, via the relay), so connecting picks it when
// the node's own address doesn't answer. Only OpenSSH and systemd are used;
// nothing is installed.
//
// This package only writes the shell scripts and config text; cmd/meterm
// runs them on the hosts.
package relay

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"meterm/internal/config"
)

// User is the account tunnels log in as on a relay.
const User = "meterm-relay"

// FirstPort is where tunnel ports start on a relay's localhost.
const FirstPort = 22100

// RouteName is the route a node gets for a relay.
func RouteName(relay string) string {
	return "relay-" + strings.ToLower(safe(relay))
}

// IsRelayRoute reports whether rt is a tunnel route, and through which relay.
func IsRelayRoute(rt config.Route) (relay string, ok bool) {
	if !strings.HasPrefix(rt.Name, "relay-") || rt.Address != "127.0.0.1" || rt.ProxyJump == "" {
		return "", false
	}
	return rt.ProxyJump, true
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func safe(s string) string { return unsafeChars.ReplaceAllString(s, "_") }

// UnitName is the systemd unit on a node for its tunnel to relay.
func UnitName(relay string) string {
	return "meterm-relay-" + strings.ToLower(safe(relay)) + ".service"
}

// keyComment tags a node's line in the relay's authorized_keys.
func keyComment(node string) string { return "meterm-relay:" + safe(node) }

// UsedPorts is every tunnel port cfg already gives out, through any relay.
// Ports are unique across relays: each machine's host key is remembered as
// [127.0.0.1]:port, so two machines sharing a port number would clash.
func UsedPorts(cfg *config.Config) map[int]bool {
	used := map[int]bool{}
	for _, h := range cfg.Hosts {
		if isLoopback(h.Address) {
			used[h.Port] = true
		}
		for _, rt := range h.Routes {
			if isLoopback(rt.Address) {
				used[rt.Port] = true
			}
		}
	}
	return used
}

func isLoopback(a string) bool { return a == "127.0.0.1" || a == "localhost" || a == "::1" }

// DefaultLink reports whether h itself is reached through a relay tunnel —
// a machine added with invite/accept, which has no address of its own.
func DefaultLink(h config.Host) (relay string, ok bool) {
	if h.Address != "127.0.0.1" || h.ProxyJump == "" || strings.Contains(h.ProxyJump, ",") {
		return "", false
	}
	return h.ProxyJump, true
}

// FreePort is the first port from FirstPort that's neither given out nor
// already listening on the relay.
func FreePort(used, listening map[int]bool) int {
	for p := FirstPort; p < 65535; p++ {
		if !used[p] && !listening[p] {
			return p
		}
	}
	return 0
}

// ListeningScript prints the TCP ports listening on a host, one per line.
// No root needed.
const ListeningScript = `(ss -Hltn 2>/dev/null || netstat -ltn 2>/dev/null | tail -n +3) | awk '{print $4}' | grep -oE '[0-9]+$' | sort -un
`

// ParsePorts reads ListeningScript's output.
func ParsePorts(out string) map[int]bool {
	m := map[int]bool{}
	for _, l := range strings.Fields(out) {
		if p, err := strconv.Atoi(l); err == nil {
			m[p] = true
		}
	}
	return m
}

// InitScript prepares a relay (as root): the tunnel account, with no shell
// and no password, and an sshd Match block letting it open reverse
// forwards on localhost and nothing else. The new sshd config is checked
// before sshd restarts; if the check fails, the old config is put back and
// nothing restarts.
func InitScript() string {
	return `set -e
U=` + User + `
PATH="$PATH:/usr/sbin:/sbin"
v=$(ssh -V 2>&1 | sed -nE 's/^OpenSSH_([0-9]+)\.([0-9]+).*/\1 \2/p'); set -- $v
if [ -n "$1" ] && { [ "$1" -lt 7 ] || { [ "$1" -eq 7 ] && [ "$2" -lt 8 ]; }; }; then echo "METERM-ERR: OpenSSH $1.$2 is too old (7.8+ needed)"; exit 1; fi
NOLOGIN=$(command -v nologin || echo /usr/sbin/nologin)
if ! id "$U" >/dev/null 2>&1; then
  useradd --system --create-home --home-dir /var/lib/$U --shell "$NOLOGIN" "$U"
fi
H=$(getent passwd "$U" | cut -d: -f6)
G=$(id -gn "$U")
install -d -m 700 -o "$U" -g "$G" "$H/.ssh"
touch "$H/.ssh/authorized_keys"
chown "$U:$G" "$H/.ssh/authorized_keys"; chmod 600 "$H/.ssh/authorized_keys"
C=/etc/ssh/sshd_config
cp -p "$C" "$C.meterm-bak"
for f in /etc/ssh/sshd_config.d/*.conf; do
  [ -f "$f" ] && grep -qE '^[[:space:]]*AllowUsers' "$f" && cp -p "$f" "$f.meterm-bak"
done
for f in "$C" /etc/ssh/sshd_config.d/*.conf; do
  [ -f "$f" ] || continue
  sed -i -E "/^[[:space:]]*AllowUsers/{/[[:space:]]$U([[:space:]]|\$)/!s/\$/ $U/}" "$f"
done
if ! grep -q '^# meterm-relay' "$C"; then
  # At the very end: a Match block anywhere earlier would swallow what follows.
  printf '\n# meterm-relay: reverse tunnels only (managed by meterm; see meterm relay)\nMatch User %s\n    AllowTcpForwarding remote\n    GatewayPorts no\n    X11Forwarding no\n    AllowAgentForwarding no\n    AllowStreamLocalForwarding no\n    PermitTTY no\n    ForceCommand %s\n' "$U" "$NOLOGIN" >> "$C"
fi
if ! sshd -t; then
  for b in "$C.meterm-bak" /etc/ssh/sshd_config.d/*.conf.meterm-bak; do [ -f "$b" ] && mv "$b" "${b%.meterm-bak}"; done
  echo "METERM-ERR: sshd rejected the new config; the old one is back"; exit 1
fi
rm -f "$C.meterm-bak" /etc/ssh/sshd_config.d/*.conf.meterm-bak
systemctl restart ssh 2>/dev/null || systemctl restart sshd
echo METERM-OK
`
}

// AuthorizedLine is a node's line in the relay's authorized_keys: the key
// may only listen on localhost:port, nothing else.
func AuthorizedLine(pubkey, node string, port int) string {
	f := strings.Fields(pubkey)
	key := pubkey
	if len(f) >= 2 {
		key = f[0] + " " + f[1]
	}
	return fmt.Sprintf(`restrict,port-forwarding,permitlisten="localhost:%d" %s %s`, port, key, keyComment(node))
}

var pubkeyRE = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)) [A-Za-z0-9+/=]+( [^'"\x60$\\\n]*)?$`)

// AddKeyScript puts a node's line in the relay's authorized_keys (as
// root), replacing any older line for the same node.
func AddKeyScript(pubkey, node string, port int) (string, error) {
	pubkey = strings.TrimSpace(pubkey)
	if !pubkeyRE.MatchString(pubkey) {
		return "", fmt.Errorf("not a public key: %q", pubkey)
	}
	line := AuthorizedLine(pubkey, node, port)
	return `set -e
H=$(getent passwd ` + User + ` | cut -d: -f6)
[ -n "$H" ] || { echo "METERM-ERR: this relay isn't set up; run: meterm relay init"; exit 1; }
F="$H/.ssh/authorized_keys"
awk -v c='` + keyComment(node) + `' '$NF != c' "$F" > "$F.new"
printf '%s\n' '` + line + `' >> "$F.new"
cat "$F.new" > "$F"; rm -f "$F.new"
echo METERM-OK
`, nil
}

// RemoveKeyScript takes a node's line out of the relay's authorized_keys
// (as root).
func RemoveKeyScript(node string) string {
	return `H=$(getent passwd ` + User + ` | cut -d: -f6)
[ -n "$H" ] || { echo METERM-OK; exit 0; }
F="$H/.ssh/authorized_keys"
awk -v c='` + keyComment(node) + `' '$NF != c' "$F" > "$F.new"
cat "$F.new" > "$F"; rm -f "$F.new"
echo METERM-OK
`
}

// NodeParams is what a node needs to keep its tunnel up.
type NodeParams struct {
	Relay      string   // relay's name, for the unit
	Candidates []string // relay addresses as host:port, tried in order from the node
	KnownHosts []string // relay host key lines, "name type key", already trusted
	Port       int      // relay-side localhost port
	LocalPort  int      // node's own sshd port (0: 22)
}

var hostPortRE = regexp.MustCompile(`^(\[[0-9A-Fa-f:]+\]|[A-Za-z0-9.-]+):[0-9]+$`)
var knownLineRE = regexp.MustCompile(`^[^\s'"\x60$\\;&|]+ [a-z0-9-]+ [A-Za-z0-9+/=]+$`)

// NodeScript sets a node up (as root): a dedicated key, the relay's host
// keys, and a systemd unit that keeps the tunnel open. It picks the first
// relay address the node can actually reach, prints the public key and
// that address, and starts the unit (which retries until the relay
// accepts the key).
func NodeScript(p NodeParams) (string, error) {
	if len(p.Candidates) == 0 || len(p.KnownHosts) == 0 || p.Port == 0 {
		return "", fmt.Errorf("relay addresses, host keys and a port are all needed")
	}
	for _, c := range p.Candidates {
		if !hostPortRE.MatchString(c) {
			return "", fmt.Errorf("unexpected relay address %q", c)
		}
	}
	for _, k := range p.KnownHosts {
		if !knownLineRE.MatchString(k) {
			return "", fmt.Errorf("unexpected host key line %q", k)
		}
	}
	lp := p.LocalPort
	if lp == 0 {
		lp = 22
	}
	unit := UnitName(p.Relay)
	forward := fmt.Sprintf("localhost:%d:localhost:%d", p.Port, lp)
	return `set -e
command -v systemctl >/dev/null || { echo "METERM-ERR: needs systemd"; exit 1; }
SSH=$(command -v ssh) || { echo "METERM-ERR: no ssh client"; exit 1; }
D=/etc/meterm-relay
install -d -m 700 "$D"
[ -f "$D/id_ed25519" ] || ssh-keygen -q -t ed25519 -N '' -C "meterm-relay@$(hostname)" -f "$D/id_ed25519"
printf '%s\n' ` + quoteAll(p.KnownHosts) + ` > "$D/known_hosts"
chmod 600 "$D/known_hosts"
reach() { if command -v nc >/dev/null; then nc -z -w 4 "$1" "$2" 2>/dev/null; else timeout 4 bash -c "</dev/tcp/$1/$2" 2>/dev/null; fi; }
ADDR=
for c in ` + strings.Join(p.Candidates, " ") + `; do
  h=${c%:*}; pt=${c##*:}; h=${h#[}; h=${h%]}
  if reach "$h" "$pt"; then ADDR="$h"; PORT="$pt"; break; fi
done
[ -n "$ADDR" ] || { echo "METERM-ERR: this machine can't reach the relay at ` + strings.Join(p.Candidates, ", ") + `"; exit 1; }
case "$ADDR" in *:*) TARGET="` + User + `@[$ADDR]" ;; *) TARGET="` + User + `@$ADDR" ;; esac
printf '%s\n' \
  '[Unit]' \
  'Description=meterm relay tunnel through ` + safe(p.Relay) + `' \
  'After=network-online.target' \
  'Wants=network-online.target' \
  '' \
  '[Service]' \
  "ExecStart=$SSH -N -p $PORT -i $D/id_ed25519 -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$D/known_hosts -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -R ` + forward + ` $TARGET" \
  'Restart=always' \
  'RestartSec=10' \
  '' \
  '[Install]' \
  'WantedBy=multi-user.target' > /etc/systemd/system/` + unit + `
systemctl daemon-reload
systemctl enable ` + unit + ` >/dev/null 2>&1
systemctl restart ` + unit + `
echo "METERM-RELAY-ADDR: $ADDR:$PORT"
echo "METERM-PUBKEY: $(cat $D/id_ed25519.pub)"
echo METERM-OK
`, nil
}

func quoteAll(lines []string) string {
	q := make([]string, len(lines))
	for i, l := range lines {
		q[i] = "'" + l + "'"
	}
	return strings.Join(q, " ")
}

// NodeRemoveScript stops and removes a node's tunnel unit (as root). The
// key goes too, unless another relay's unit still uses it.
func NodeRemoveScript(relay string) string {
	unit := UnitName(relay)
	return `systemctl disable --now ` + unit + ` 2>/dev/null || true
rm -f /etc/systemd/system/` + unit + `
systemctl daemon-reload
ls /etc/systemd/system/meterm-relay-*.service >/dev/null 2>&1 || rm -rf /etc/meterm-relay
echo METERM-OK
`
}

// Marker finds "METERM-<name>: value" in a script's output.
func Marker(out, name string) string {
	prefix := "METERM-" + name + ": "
	for _, l := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if i := strings.Index(l, prefix); i >= 0 {
			return strings.TrimSpace(l[i+len(prefix):])
		}
	}
	return ""
}

// Failure is a script's own error message, if it printed one.
func Failure(out string) string { return Marker(out, "ERR") }

// OK reports whether a script got to its end.
func OK(out string) bool { return strings.Contains(out, "METERM-OK") }

// Node is one enrolled machine.
type Node struct {
	Host  string
	Port  int
	Route string
}

// Enrolled lists cfg's nodes that have a tunnel route, by relay.
func Enrolled(cfg *config.Config) map[string][]Node {
	out := map[string][]Node{}
	for _, h := range cfg.Hosts {
		if via, ok := DefaultLink(h); ok {
			out[via] = append(out[via], Node{Host: h.Name, Port: h.Port, Route: "default"})
		}
		for _, rt := range h.Routes {
			if via, ok := IsRelayRoute(rt); ok {
				out[via] = append(out[via], Node{Host: h.Name, Port: rt.Port, Route: rt.Name})
			}
		}
	}
	for _, ns := range out {
		sort.Slice(ns, func(i, j int) bool { return ns[i].Port < ns[j].Port })
	}
	return out
}
