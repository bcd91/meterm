// Package audit checks a host against a baseline for machines reachable from
// the internet, and turns what it can't fix itself into a hand-off list for
// whoever can. It only ever reads: Script is piped to `sh -s` on the target
// and changes nothing there.
package audit

import (
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Script gathers every fact the checks need in one SSH round trip, as
// "### name" sections of plain text (see Parse). It is fed to `sh -s` on
// stdin rather than passed as an argument so it needs no shell quoting —
// which is also why every command that could read stdin (and so swallow the
// rest of the script) is wrapped in q, which redirects it from /dev/null.
//
// It ends with an explicit exit 0: the last probe (systemctl is-active) exits
// non-zero for an inactive unit, which would otherwise make ssh report the
// whole audit as failed.
//
// It uses sudo only as `sudo -n` (never prompts) and treats "can't tell" as
// a first-class answer, because a check that silently reports "fine" when it
// couldn't look is worse than no check.
const Script = scriptBase + workloadScript + opsScript + "exit 0\n"

const scriptBase = `s() { printf "### %s\n" "$1"; }
q() { "$@" </dev/null 2>/dev/null; }
s os
( . /etc/os-release 2>/dev/null; echo "ID=$ID"; echo "VERSION_ID=$VERSION_ID"; echo "PRETTY=$PRETTY_NAME" )
s uptime_days
awk '{print int($1/86400)}' /proc/uptime 2>/dev/null
s kernel
uname -r
s reboot_required
if [ -e /var/run/reboot-required ]; then echo yes; else echo no; fi
s pending
if command -v apt >/dev/null 2>&1; then q apt list --upgradable | tail -n +2 | wc -l; else echo n/a; fi
s sudo
if q sudo -n true; then echo yes; else echo no; fi
s sshd_readable
if [ -r /etc/ssh/sshd_config ]; then echo yes; else echo no; fi
s sshd
cat /etc/ssh/sshd_config.d/*.conf /etc/ssh/sshd_config 2>/dev/null | grep -iE '^[[:space:]]*(PasswordAuthentication|PermitRootLogin|Port)[[:space:]]'
s bruteforce
for u in fail2ban crowdsec; do echo "$u=$(q systemctl is-active $u)"; done
s autoupdate
echo "$(q systemctl is-enabled unattended-upgrades)"
s listening
q ss -tln | tail -n +2
s firewall
if command -v ufw >/dev/null 2>&1; then o=$(q sudo -n ufw status | head -1); if [ -n "$o" ]; then echo "$o"; else echo unknown; fi; else echo no_ufw; fi
echo "firewalld=$(q systemctl is-active firewalld)"
s addrs
ip -o -4 addr show 2>/dev/null | awk '{print $2, $4}'
s docker
if command -v docker >/dev/null 2>&1; then
  if o=$(q docker ps --format '{{.Names}} {{.Image}} {{.Ports}}'); then echo ok; echo "$o";
  elif o=$(q sudo -n docker ps --format '{{.Names}} {{.Image}} {{.Ports}}'); then echo ok; echo "$o";
  else echo noaccess; fi
else echo none; fi
s nginx
echo "active=$(q systemctl is-active nginx)"
if command -v nginx >/dev/null 2>&1; then echo "version=$(nginx -v 2>&1 </dev/null | head -1)"; fi
s nginx_conf_readable
if [ -r /etc/nginx/nginx.conf ]; then echo yes; else echo no; fi
s nginx_tokens
grep -rhE '^[[:space:]]*server_tokens' /etc/nginx 2>/dev/null | head -3
s nginx_ratelimit
grep -rlE '^[[:space:]]*limit_req_zone' /etc/nginx 2>/dev/null | head -1
s nginx_logformat
grep -rhE '^[^#]*request_time' /etc/nginx 2>/dev/null | head -1
s nginx_logs
for f in /var/log/nginx/access.log /var/log/nginx/*.access.log /var/log/nginx/*_access.log; do if [ -e "$f" ]; then if [ -r "$f" ]; then echo "readable $f"; else echo "unreadable $f"; fi; fi; done | head -8
s nginx_timing
res=unknown
for f in /var/log/nginx/access.log /var/log/nginx/*.access.log /var/log/nginx/*_access.log; do
  [ -f "$f" ] && [ -r "$f" ] && [ -s "$f" ] || continue
  if tail -n 1 "$f" 2>/dev/null </dev/null | grep -Eq ' [0-9]+(\.[0-9]+)? ([0-9]+(\.[0-9]+)?|-)$'; then res=yes; else res=no; fi
  break
done
echo $res
s log_retention
grep -hE '^[[:space:]]*(daily|weekly|monthly|rotate)[[:space:]]*' /etc/logrotate.d/nginx 2>/dev/null
s certbot
q systemctl is-active certbot.timer
`

// Listener is one TCP listening socket.
type Listener struct {
	Addr     string
	Port     int
	Wildcard bool // bound to every interface, not one address
}

// Published is a Docker port mapping: a container port made reachable on the
// host. It matters because the host port number alone (say 32771) says
// nothing about what's behind it.
type Published struct {
	Container     string
	Image         string
	HostPort      int
	ContainerPort int
}

// Addr is one IPv4 address configured on the host.
type Addr struct {
	Iface string
	IP    string
}

var cgnat = func() *net.IPNet { _, n, _ := net.ParseCIDR("100.64.0.0/10"); return n }()

// Public reports whether a is a publicly routable address: not private
// (RFC 1918), loopback, link-local, or CGNAT space — which is also where
// Tailscale addresses live. Only a public address can be reached from the
// internet without a router forwarding to it.
func (a Addr) Public() bool {
	ip := net.ParseIP(a.IP)
	return ip != nil && ip.To4() != nil && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !cgnat.Contains(ip)
}

// PublicAddrs lists the host's publicly routable addresses.
func (f Facts) PublicAddrs() []string {
	var out []string
	for _, a := range f.Addrs {
		if a.Public() {
			out = append(out, a.IP)
		}
	}
	return out
}

// Container is one running Docker container. What it is comes from its image,
// since a container's name says only what someone called it.
type Container struct {
	Name, Image string
}

// Facts is what Script found, in typed form. Fields that couldn't be read
// are left at a value the checks treat as "unknown" (-1, "", false with a
// matching *Readable/State field) rather than as a pass.
type Facts struct {
	OSID, OSVersion, OSPretty string

	UptimeDays     int // -1 unknown
	Kernel         string
	RebootRequired bool
	Pending        int // pending package updates; -1 unknown / not apt
	HaveSudo       bool

	SSHDReadable bool
	PasswordAuth string // effective PasswordAuthentication, lowercased; "" = not set (sshd defaults to yes)
	PermitRoot   string // effective PermitRootLogin, lowercased; "" = not set
	SSHPorts     []int

	BruteForce  []string // active brute-force protection tools (fail2ban, crowdsec)
	AutoUpdates bool

	Addrs     []Addr
	Listeners []Listener
	Firewall  string // ufw-active, firewalld-active, ufw-inactive, unknown, none

	DockerState string // none, ok, noaccess
	Containers  []Container
	Published   []Published

	NginxActive       bool
	NginxVersion      string
	NginxConfReadable bool
	ServerTokens      string // "" = directive absent (nginx defaults to on)
	RateLimit         bool   // some limit_req_zone is configured
	TimingInLog       bool   // request time is logged: read off the newest log line when there is one, else from a log_format in nginx's config
	LogState          string // readable, unreadable, none
	LogRetentionDays  int    // -1 unknown
	CertbotTimer      bool

	Workload WorkloadFacts // containers' insides and egress (workloads.go)
	Ops      OpsFacts      // disk, clock, failed services, certificates (ops.go)
}

// Parse turns Script's output into Facts. It never fails: a section that's
// missing or unreadable just leaves its facts at "unknown".
func Parse(raw string) Facts {
	sec := splitSections(raw)
	f := Facts{UptimeDays: -1, Pending: -1, LogRetentionDays: -1}

	for _, line := range lines(sec["os"]) {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "ID":
			f.OSID = strings.ToLower(strings.TrimSpace(v))
		case "VERSION_ID":
			f.OSVersion = strings.TrimSpace(v)
		case "PRETTY":
			f.OSPretty = strings.TrimSpace(v)
		}
	}
	if n, err := strconv.Atoi(strings.TrimSpace(sec["uptime_days"])); err == nil {
		f.UptimeDays = n
	}
	f.Kernel = strings.TrimSpace(sec["kernel"])
	f.RebootRequired = strings.TrimSpace(sec["reboot_required"]) == "yes"
	if n, err := strconv.Atoi(strings.TrimSpace(sec["pending"])); err == nil {
		f.Pending = n
	}
	f.HaveSudo = strings.TrimSpace(sec["sudo"]) == "yes"

	f.SSHDReadable = strings.TrimSpace(sec["sshd_readable"]) == "yes"
	for _, line := range lines(sec["sshd"]) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		val := strings.ToLower(fields[1])
		switch strings.ToLower(fields[0]) {
		case "passwordauthentication":
			if f.PasswordAuth == "" { // sshd honors the first occurrence
				f.PasswordAuth = val
			}
		case "permitrootlogin":
			if f.PermitRoot == "" {
				f.PermitRoot = val
			}
		case "port":
			if n, err := strconv.Atoi(val); err == nil {
				f.SSHPorts = append(f.SSHPorts, n)
			}
		}
	}

	for _, line := range lines(sec["bruteforce"]) {
		name, state, _ := strings.Cut(line, "=")
		if strings.TrimSpace(state) == "active" {
			f.BruteForce = append(f.BruteForce, name)
		}
	}
	f.AutoUpdates = strings.TrimSpace(sec["autoupdate"]) == "enabled"

	for _, line := range lines(sec["addrs"]) {
		iface, cidr, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if ip, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err == nil {
			f.Addrs = append(f.Addrs, Addr{Iface: iface, IP: ip.String()})
		}
	}
	f.Listeners = parseListeners(sec["listening"])
	f.Firewall = parseFirewall(sec["firewall"])
	f.DockerState, f.Containers, f.Published = parseDocker(sec["docker"])

	for _, line := range lines(sec["nginx"]) {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "active":
			f.NginxActive = strings.TrimSpace(v) == "active"
		case "version":
			f.NginxVersion = strings.TrimSpace(strings.TrimPrefix(v, "nginx version:"))
		}
	}
	f.NginxConfReadable = strings.TrimSpace(sec["nginx_conf_readable"]) == "yes"
	if l := lines(sec["nginx_tokens"]); len(l) > 0 {
		if fields := strings.Fields(strings.TrimSuffix(l[0], ";")); len(fields) >= 2 {
			f.ServerTokens = strings.ToLower(strings.TrimSuffix(fields[1], ";"))
		}
	}
	f.RateLimit = strings.TrimSpace(sec["nginx_ratelimit"]) != ""
	f.TimingInLog = strings.TrimSpace(sec["nginx_logformat"]) != ""
	// The log itself is better evidence than the config: a format that
	// mentions $request_time may not be the one in use, and a proxy in a
	// container has no /etc/nginx here at all.
	switch strings.TrimSpace(sec["nginx_timing"]) {
	case "yes":
		f.TimingInLog = true
	case "no":
		f.TimingInLog = false
	}
	f.LogState = "none"
	for _, line := range lines(sec["nginx_logs"]) {
		switch {
		case strings.HasPrefix(line, "readable"):
			f.LogState = "readable"
		case strings.HasPrefix(line, "unreadable") && f.LogState != "readable":
			f.LogState = "unreadable"
		}
	}
	f.LogRetentionDays = parseRetention(sec["log_retention"])
	f.CertbotTimer = strings.TrimSpace(sec["certbot"]) == "active"
	parseWorkloads(&f, sec)
	parseOps(&f, sec)
	return f
}

func splitSections(raw string) map[string]string {
	out := map[string]string{}
	var name string
	var b strings.Builder
	flush := func() {
		if name != "" {
			out[name] = strings.TrimRight(b.String(), "\n")
		}
		b.Reset()
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "### ") {
			flush()
			name = strings.TrimPrefix(line, "### ")
			continue
		}
		if name != "" {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	flush()
	return out
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// parseListeners reads `ss -tln` rows (State Recv-Q Send-Q Local Peer),
// collapsing the IPv4 and IPv6 sockets of one port into a single entry.
func parseListeners(s string) []Listener {
	seen := map[int]int{} // port -> index in out
	var out []Listener
	for _, line := range lines(s) {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		local := fields[3]
		i := strings.LastIndex(local, ":")
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(local[i+1:])
		if err != nil {
			continue
		}
		addr := local[:i]
		wild := addr == "0.0.0.0" || addr == "*" || addr == "[::]" || addr == "::"
		if idx, ok := seen[port]; ok {
			out[idx].Wildcard = out[idx].Wildcard || wild
			continue
		}
		seen[port] = len(out)
		out = append(out, Listener{Addr: addr, Port: port, Wildcard: wild})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

func parseFirewall(s string) string {
	ufw, firewalld := "", ""
	for _, line := range lines(s) {
		switch {
		case strings.HasPrefix(line, "Status: active"):
			ufw = "active"
		case strings.HasPrefix(line, "Status: inactive"):
			ufw = "inactive"
		case line == "unknown":
			ufw = "unknown"
		case strings.HasPrefix(line, "firewalld="):
			firewalld = strings.TrimPrefix(line, "firewalld=")
		}
	}
	switch {
	case ufw == "active":
		return "ufw-active"
	case firewalld == "active":
		return "firewalld-active"
	case ufw == "inactive":
		return "ufw-inactive"
	case ufw == "unknown":
		return "unknown"
	default:
		return "none"
	}
}

// dockerPort matches one published mapping, including the range form Docker
// prints when several consecutive ports are published together
// ("0.0.0.0:80-81->80-81/tcp").
var dockerPort = regexp.MustCompile(`(?:0\.0\.0\.0|\[::\]|::):(\d+)(?:-(\d+))?->(\d+)(?:-(\d+))?/tcp`)

// parseDocker reads the "docker" section: a state line, then one
// "<container> <image> <ports>" row per running container.
func parseDocker(s string) (string, []Container, []Published) {
	ls := lines(s)
	if len(ls) == 0 {
		return "none", nil, nil
	}
	state := ls[0]
	if state != "ok" {
		return state, nil, nil
	}
	seen := map[[2]any]bool{}
	var cs []Container
	var pubs []Published
	for _, line := range ls[1:] {
		name, rest, _ := strings.Cut(line, " ")
		image, ports, _ := strings.Cut(rest, " ")
		cs = append(cs, Container{Name: name, Image: image})
		for _, m := range dockerPort.FindAllStringSubmatch(ports, -1) {
			hp, _ := strconv.Atoi(m[1])
			cp, _ := strconv.Atoi(m[3])
			// A range maps host port hp+i to container port cp+i.
			n := 1
			if m[2] != "" {
				if hi, err := strconv.Atoi(m[2]); err == nil && hi >= hp && hi-hp < 1000 {
					n = hi - hp + 1
				}
			}
			for i := 0; i < n; i++ {
				key := [2]any{name, hp + i}
				if seen[key] {
					continue
				}
				seen[key] = true
				pubs = append(pubs, Published{Container: name, Image: image, HostPort: hp + i, ContainerPort: cp + i})
			}
		}
	}
	return "ok", cs, pubs
}

// parseRetention turns logrotate's frequency + "rotate N" into days.
func parseRetention(s string) int {
	mult, rotate := 0, -1
	for _, line := range lines(s) {
		fields := strings.Fields(line)
		switch fields[0] {
		case "daily":
			if mult == 0 {
				mult = 1
			}
		case "weekly":
			if mult == 0 {
				mult = 7
			}
		case "monthly":
			if mult == 0 {
				mult = 30
			}
		case "rotate":
			if len(fields) > 1 {
				if n, err := strconv.Atoi(fields[1]); err == nil {
					rotate = n
				}
			}
		}
	}
	if mult == 0 || rotate < 0 {
		return -1
	}
	return mult * rotate
}
