package audit

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Exposure says which doors of a host face the internet. The baseline is
// the same for every host; exposure only decides which controls apply.
type Exposure struct {
	SSH      bool
	Web      bool
	Assumed  bool // no exposure tag was set, so both are assumed (the safe default)
	Internal bool // tagged "internal": not internet-facing
}

// ParseExposure reads exposure from host tags: "exposes:ssh",
// "exposes:web", or "internal". With none of them it assumes the worst,
// because a host nobody has classified is more likely to be forgotten than
// to be safe.
func ParseExposure(tags []string) Exposure {
	var e Exposure
	for _, t := range tags {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "exposes:ssh":
			e.SSH = true
		case "exposes:web":
			e.Web = true
		case "internal":
			e.Internal = true
		}
	}
	if !e.SSH && !e.Web && !e.Internal {
		e.SSH, e.Web, e.Assumed = true, true, true
	}
	return e
}

// Exposed reports whether the host faces the internet at all.
func (e Exposure) Exposed() bool { return e.SSH || e.Web }

func (e Exposure) String() string {
	var parts []string
	if e.SSH {
		parts = append(parts, "ssh")
	}
	if e.Web {
		parts = append(parts, "web")
	}
	if len(parts) == 0 {
		return "internal"
	}
	s := strings.Join(parts, "+")
	if e.Assumed {
		s += " (assumed: no exposes:* tag)"
	}
	return s
}

// Status of one control on one host.
type Status string

const (
	Pass    Status = "pass"
	Fail    Status = "fail"
	Unknown Status = "unknown" // couldn't look — never treated as a pass
	NA      Status = "n/a"     // control doesn't apply to this host's exposure
)

// Severity of a failed control.
type Severity int

const (
	Info Severity = iota
	Low
	Medium
	High
	Critical
)

func (s Severity) String() string {
	return [...]string{"info", "low", "medium", "high", "critical"}[s]
}

// MarshalText makes severities readable in --format json.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText reads a severity back (audit watch keeps the last run as
// JSON). An unknown name is an error rather than a silent "info".
func (s *Severity) UnmarshalText(b []byte) error {
	for i, name := range [...]string{"info", "low", "medium", "high", "critical"} {
		if string(b) == name {
			*s = Severity(i)
			return nil
		}
	}
	return fmt.Errorf("unknown severity %q", b)
}

// Owner is who has to act on a finding. The audit can't fix anything
// itself, so every failure names the role that can.
type Owner string

const (
	OwnerSysadmin Owner = "sysadmin (root on the host)"
	OwnerNetwork  Owner = "network / cloud firewall"
	OwnerApp      Owner = "application owner"
)

// Result is the outcome of one control on one host.
type Result struct {
	Control  string   `json:"control"`
	Title    string   `json:"title"`
	Status   Status   `json:"status"`
	Severity Severity `json:"severity"`
	Detail   string   `json:"detail,omitempty"`
	Fix      string   `json:"fix,omitempty"`
	Owner    Owner    `json:"owner,omitempty"`
}

// dbPorts are services that should essentially never face the internet.
var dbPorts = map[int]string{
	3306: "MySQL", 5432: "PostgreSQL", 6379: "Redis", 27017: "MongoDB",
	9200: "Elasticsearch", 11211: "Memcached", 1433: "SQL Server", 5984: "CouchDB",
}

// consolePorts are admin/development consoles recognisable by their default
// port alone. They give whoever reaches them control of the service — Node-RED's
// editor runs arbitrary code — so they must never face the internet.
var consolePorts = map[int]string{
	1880: "Node-RED editor port", 5601: "Kibana", 15672: "RabbitMQ management",
}

// consoleImages are admin consoles recognisable by image, with the container
// port each serves on. This is what tells nginx-proxy-manager's admin UI
// (port 81, which controls every proxied site and certificate) from any
// other service that happens to use 81.
var consoleImages = []struct {
	image string
	port  int
	name  string
}{
	{"nginx-proxy-manager", 81, "nginx-proxy-manager admin UI"},
	{"portainer", 9000, "Portainer"},
	{"portainer", 9443, "Portainer"},
	{"traefik", 8080, "Traefik dashboard"},
	{"adminer", 8080, "Adminer"},
	{"phpmyadmin", 80, "phpMyAdmin"},
	{"pgadmin", 80, "pgAdmin"},
	{"grafana", 3000, "Grafana"},
	{"jenkins", 8080, "Jenkins"},
}

// reverseProxies are images that are a web tier of their own: nginx running
// inside a container is invisible to the host-level nginx checks.
var reverseProxies = []struct{ image, name string }{
	{"nginx-proxy-manager", "nginx-proxy-manager"},
	{"traefik", "Traefik"},
	{"caddy", "Caddy"},
	{"haproxy", "HAProxy"},
}

// ProxyContainer returns the reverse-proxy container running on the host, if
// any, and what it is.
func (f Facts) ProxyContainer() (Container, string, bool) {
	for _, c := range f.Containers {
		img := strings.ToLower(c.Image)
		for _, rp := range reverseProxies {
			if strings.Contains(img, rp.image) {
				return c, rp.name, true
			}
		}
	}
	return Container{}, "", false
}

// Evaluate runs the baseline against facts collected from one host. probe,
// when non-nil, maps port -> reachable-from-the-auditor for ports that were
// actually tried; it turns "bound to all interfaces" from a maybe into a
// yes or no.
func Evaluate(exp Exposure, f Facts, probe map[int]bool, now time.Time) []Result {
	rs := []Result{
		checkOS(f, now),
		checkPatching(f),
		checkAutoUpdates(f),
		checkSSHAuth(exp, f),
		checkBruteForce(exp, f),
		checkFirewall(f),
		checkExposedPorts(exp, f, probe),
		checkWebHardening(exp, f),
		checkObservability(exp, f),
		checkTLSRenewal(exp, f),
	}
	rs = append(rs, workloadChecks(f)...)
	rs = append(rs, opsChecks(f, now)...)
	sort.SliceStable(rs, func(i, j int) bool { return rank(rs[i]) > rank(rs[j]) })
	return rs
}

// rank orders results: failures by severity, then unknowns, then the rest.
func rank(r Result) int {
	switch r.Status {
	case Fail:
		return 100 + int(r.Severity)
	case Unknown:
		return 50
	case Pass:
		return 10
	}
	return 0
}

func pass(control, title, detail string) Result {
	return Result{Control: control, Title: title, Status: Pass, Detail: detail}
}

func na(control, title, why string) Result {
	return Result{Control: control, Title: title, Status: NA, Detail: why}
}

func unknown(control, title, why string) Result {
	return Result{Control: control, Title: title, Status: Unknown, Severity: Low, Detail: why, Owner: OwnerSysadmin,
		Fix: "Give the audit account read access (or passwordless sudo for read-only commands) and re-run, or check this by hand."}
}

func fail(control, title string, sev Severity, owner Owner, detail, fix string) Result {
	return Result{Control: control, Title: title, Status: Fail, Severity: sev, Owner: owner, Detail: detail, Fix: fix}
}

func checkOS(f Facts, now time.Time) Result {
	const c, title = "os-support", "OS still receives security updates"
	name := f.OSPretty
	if name == "" {
		name = f.OSID + " " + f.OSVersion
	}
	if f.OSID == "" {
		return unknown(c, title, "could not read /etc/os-release")
	}
	end, ok := SupportEnd(f.OSID, f.OSVersion)
	if !ok {
		return unknown(c, title, fmt.Sprintf("%s is not in meterm's support-date table", strings.TrimSpace(name)))
	}
	if now.After(end) {
		months := int(now.Sub(end).Hours() / 24 / 30)
		return fail(c, title, Critical, OwnerSysadmin,
			fmt.Sprintf("%s reached end of standard support on %s (%d months ago): new vulnerabilities in the OS and its packages are no longer patched.",
				name, end.Format("2006-01-02"), months),
			"Plan an upgrade or rebuild onto a supported release; until then, keep the host off the public internet or behind a reverse proxy.")
	}
	if left := end.Sub(now); left < 180*24*time.Hour {
		return fail(c, title, Low, OwnerSysadmin,
			fmt.Sprintf("%s support ends %s (in %d days).", name, end.Format("2006-01-02"), int(left.Hours()/24)),
			"Schedule the upgrade before support ends.")
	}
	return pass(c, title, fmt.Sprintf("%s, supported until %s", name, end.Format("2006-01-02")))
}

func checkPatching(f Facts) Result {
	const c, title = "patching", "Updates applied, no reboot pending"
	if f.Pending < 0 && !f.RebootRequired {
		return unknown(c, title, "could not count pending updates (no apt?)")
	}
	var problems []string
	sev := Low
	if f.RebootRequired {
		problems = append(problems, "a reboot is pending (installed updates, e.g. kernel, are not active yet)")
		sev = Medium
	}
	if f.Pending > 0 {
		problems = append(problems, fmt.Sprintf("%d package updates waiting", f.Pending))
		if f.Pending >= 30 {
			sev = Medium
		}
	}
	if f.UptimeDays >= 180 {
		problems = append(problems, fmt.Sprintf("up %d days without a reboot", f.UptimeDays))
	}
	if len(problems) == 0 {
		return pass(c, title, "no pending updates or reboot")
	}
	return fail(c, title, sev, OwnerSysadmin, strings.Join(problems, "; ")+".",
		"Apply updates in a maintenance window and reboot if required.")
}

func checkAutoUpdates(f Facts) Result {
	const c, title = "auto-updates", "Security updates install automatically"
	if f.AutoUpdates {
		return pass(c, title, "unattended-upgrades enabled")
	}
	return fail(c, title, Low, OwnerSysadmin,
		"unattended-upgrades is not enabled, so fixes wait for someone to remember.",
		"apt install unattended-upgrades && dpkg-reconfigure -plow unattended-upgrades")
}

func checkSSHAuth(exp Exposure, f Facts) Result {
	const c, title = "ssh-auth", "SSH: keys only, no root password login"
	if !exp.SSH {
		return na(c, title, "host does not expose SSH")
	}
	if !f.SSHDReadable {
		return unknown(c, title, "could not read /etc/ssh/sshd_config")
	}
	var problems []string
	pw := f.PasswordAuth
	if pw == "" || pw == "yes" {
		if pw == "" {
			problems = append(problems, "PasswordAuthentication is not set (sshd defaults to yes), so anyone on the internet can keep guessing passwords")
		} else {
			problems = append(problems, "PasswordAuthentication yes, so anyone on the internet can keep guessing passwords")
		}
	}
	if f.PermitRoot == "yes" {
		problems = append(problems, "PermitRootLogin yes: one leaked key or weak password is full control, with no per-person audit trail")
	}
	if len(problems) == 0 {
		return pass(c, title, "PasswordAuthentication "+f.PasswordAuth)
	}
	sev := High
	// Password logins are survivable when brute force is throttled; still a
	// standing risk, so never below Medium.
	if len(f.BruteForce) > 0 && f.PermitRoot != "yes" {
		sev = Medium
	}
	return fail(c, title, sev, OwnerSysadmin, strings.Join(problems, "; ")+".",
		"Confirm every user has a working key, then set PasswordAuthentication no and PermitRootLogin no (or prohibit-password) and reload sshd.")
}

func checkBruteForce(exp Exposure, f Facts) Result {
	const c, title = "brute-force", "Automatic banning of repeated failed logins"
	if !exp.SSH {
		return na(c, title, "host does not expose SSH")
	}
	if len(f.BruteForce) > 0 {
		return pass(c, title, strings.Join(f.BruteForce, ", ")+" active")
	}
	return fail(c, title, High, OwnerSysadmin,
		"neither fail2ban nor crowdsec is running; SSH guessing is never throttled or banned.",
		"Install fail2ban with an sshd jail, and add your own office/VPN addresses to ignoreip so you can't lock yourself out.")
}

func checkFirewall(f Facts) Result {
	const c, title = "firewall", "Host firewall active"
	switch f.Firewall {
	case "ufw-active", "firewalld-active":
		return pass(c, title, f.Firewall)
	case "unknown":
		return unknown(c, title, "ufw is installed but its status needs sudo")
	case "ufw-inactive":
		return fail(c, title, Medium, OwnerSysadmin, "ufw is installed but disabled.",
			"Allow SSH/HTTP/HTTPS (and your admin addresses) first, then enable ufw — or confirm a cloud/network firewall provides the same.")
	}
	return fail(c, title, Medium, OwnerSysadmin,
		"no host firewall found (ufw/firewalld). Fine only if a cloud or network firewall covers this host.",
		"Enable a host firewall, or confirm and document the upstream firewall that protects this host.")
}

// checkExposedPorts flags services bound to every interface beyond the
// expected 22/80/443. Whether they are reachable from the internet depends
// on firewalls this host can't see, so without a probe the finding says so;
// with a probe it says yes or no.
func checkExposedPorts(exp Exposure, f Facts, probe map[int]bool) Result {
	const c, title = "exposed-ports", "Only intended ports reachable"
	if !exp.Exposed() {
		return na(c, title, "host is internal")
	}
	if len(f.Listeners) == 0 {
		return unknown(c, title, "could not list listening sockets (ss)")
	}
	allowed := map[int]bool{22: true, 80: true, 443: true}
	for _, p := range f.SSHPorts {
		allowed[p] = true
	}
	// Docker publishes on arbitrary host ports (5432 often becomes 32771), so
	// what a port is has to come from the container side, not the number.
	container := map[int]string{}
	service := map[int]string{} // ports that are databases or admin consoles, by name
	console := map[int]bool{}
	for _, p := range f.Published {
		container[p.HostPort] = p.Container
		if name := dbPorts[p.ContainerPort]; name != "" {
			service[p.HostPort] = name
		}
		if name := consolePorts[p.ContainerPort]; name != "" {
			service[p.HostPort], console[p.HostPort] = name, true
		}
		img := strings.ToLower(p.Image)
		for _, ci := range consoleImages {
			if p.ContainerPort == ci.port && strings.Contains(img, ci.image) {
				service[p.HostPort], console[p.HostPort] = ci.name, true
			}
		}
	}
	for _, l := range f.Listeners {
		if name := dbPorts[l.Port]; name != "" {
			service[l.Port] = name
		}
	}

	var reachable, maybe, blocked []string
	sev := Info
	for _, l := range f.Listeners {
		if !l.Wildcard || allowed[l.Port] {
			continue
		}
		label := fmt.Sprint(l.Port)
		if name := service[l.Port]; name != "" {
			label += " (" + name + ")"
		}
		if ct := container[l.Port]; ct != "" {
			label += " [container " + ct + "]"
		}
		isDB := service[l.Port] != ""
		s := Medium
		if isDB {
			s = High
		}
		switch reach, probed := probe[l.Port]; {
		case probed && reach:
			reachable = append(reachable, label)
			if isDB {
				s = Critical
			} else if s < High {
				s = High
			}
			if s > sev {
				sev = s
			}
		case probed:
			blocked = append(blocked, label)
		default:
			maybe = append(maybe, label)
			if s > sev {
				sev = s
			}
		}
	}

	if len(reachable) == 0 && len(maybe) == 0 {
		detail := "only 22/80/443 are bound to all interfaces"
		if len(blocked) > 0 {
			detail = "extra ports are bound to all interfaces but not reachable from the auditing machine: " + strings.Join(blocked, ", ")
		}
		return pass(c, title, detail)
	}

	var parts []string
	if len(reachable) > 0 {
		parts = append(parts, "REACHABLE from the auditing machine: "+strings.Join(reachable, ", "))
	}
	if len(maybe) > 0 {
		parts = append(parts, "bound to all interfaces (reachable unless a firewall upstream blocks them; run with --probe-addr to check): "+strings.Join(maybe, ", "))
	}
	if len(blocked) > 0 {
		parts = append(parts, "not reachable from the auditing machine: "+strings.Join(blocked, ", "))
	}
	detail := strings.Join(parts, ". ") + "."
	if len(container) > 0 && f.Firewall == "ufw-active" {
		detail += " Docker-published ports bypass ufw, so ufw alone does not protect them."
	}
	if pc, kind, ok := f.ProxyContainer(); ok && len(container) > 1 {
		detail += fmt.Sprintf(" %s (container %s) is the intended front door, but these containers are published directly, so requests to them skip its TLS and access lists.", kind, pc.Name)
	}
	if len(console) > 0 {
		detail += " Admin consoles (" + consoleNames(console, service) + ") give whoever reaches them control of the service."
	}
	owner := OwnerNetwork
	fix := "Close these at the cloud/router firewall, or bind the service to 127.0.0.1 / a private address (for Docker: publish as 127.0.0.1:PORT:PORT)."
	if len(reachable) > 0 {
		fix = "Close these now (cloud/router firewall or bind to 127.0.0.1); databases and admin ports should never be reachable from the internet. " +
			"The probe ran from one network: if the firewall only allows specific source addresses (an office IP), the exposure is narrower than the whole internet — repeat from a second network (e.g. a phone hotspot) to tell which."
	}
	return fail(c, title, sev, owner, detail, fix)
}

func consoleNames(console map[int]bool, service map[int]string) string {
	seen := map[string]bool{}
	var names []string
	for p := range console {
		if n := service[p]; !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func checkWebHardening(exp Exposure, f Facts) Result {
	const c, title = "web-hardening", "Web server: rate limiting, no version banner"
	if !exp.Web {
		return na(c, title, "host does not expose web")
	}
	if !f.NginxActive {
		if pc, kind, ok := f.ProxyContainer(); ok {
			d := fmt.Sprintf("the web tier is %s in container %s (%s), not nginx on the host, so its settings can't be read from here — check server_tokens and rate limiting inside it", kind, pc.Name, pc.Image)
			if kind == "nginx-proxy-manager" {
				d += " (NPM: each proxy host's Advanced tab takes custom nginx config, e.g. limit_req)"
			}
			if strings.HasSuffix(strings.ToLower(pc.Image), ":latest") || !strings.Contains(pc.Image, ":") {
				d += "; the image is unpinned (:latest), so its version changes silently whenever the container is recreated"
			}
			return unknown(c, title, d)
		}
		return unknown(c, title, "nginx is not running here (another proxy, or in a container?) — checked nothing")
	}
	if !f.NginxConfReadable {
		return unknown(c, title, "could not read /etc/nginx")
	}
	var problems []string
	sev := Low
	if f.ServerTokens != "off" {
		problems = append(problems, "server_tokens is not off (nginx version is advertised)")
	}
	if !f.RateLimit {
		problems = append(problems, "no limit_req_zone: nothing slows a client hammering one endpoint")
		sev = Medium
	}
	if len(problems) == 0 {
		return pass(c, title, "server_tokens off, limit_req_zone configured")
	}
	return fail(c, title, sev, OwnerSysadmin, strings.Join(problems, "; ")+".",
		"Set server_tokens off; and add limit_req_zone + limit_req on the expensive/abusable endpoints (see README, 'Rate limiting').")
}

func checkObservability(exp Exposure, f Facts) Result {
	const c, title = "observability", "Logs readable, timed, and kept long enough"
	if !exp.Web {
		return na(c, title, "host does not expose web")
	}
	if f.LogState == "none" {
		if pc, kind, ok := f.ProxyContainer(); ok {
			return unknown(c, title, fmt.Sprintf("no nginx access log on the host: %s (container %s) keeps its own logs inside its data volume, so this check can't see whether it logs request times or how long it keeps them", kind, pc.Name))
		}
		return unknown(c, title, "no nginx access log at the standard paths")
	}
	if f.LogState == "unreadable" {
		return fail(c, title, Medium, OwnerSysadmin,
			"nginx access logs exist but the audit account cannot read them, so bots and abuse are invisible.",
			"Add the account to the adm group (or grant read on /var/log/nginx) — read-only access is enough.")
	}
	var problems []string
	if !f.TimingInLog {
		problems = append(problems, "log format has no $request_time (slow endpoints can't be found)")
	}
	if f.LogRetentionDays >= 0 && f.LogRetentionDays < 30 {
		problems = append(problems, fmt.Sprintf("only ~%d days of logs are kept (an incident is often noticed later than that)", f.LogRetentionDays))
	}
	if len(problems) == 0 {
		return pass(c, title, "readable, timed")
	}
	return fail(c, title, Low, OwnerSysadmin, strings.Join(problems, "; ")+".",
		"Use a log_format with $request_time $upstream_response_time, and raise logrotate 'rotate' (or ship logs off-host).")
}

func checkTLSRenewal(exp Exposure, f Facts) Result {
	const c, title = "tls-renewal", "TLS certificates renew automatically"
	if !exp.Web {
		return na(c, title, "host does not expose web")
	}
	if f.CertbotTimer {
		return pass(c, title, "certbot.timer active")
	}
	if pc, kind, ok := f.ProxyContainer(); ok && kind == "nginx-proxy-manager" {
		return unknown(c, title, fmt.Sprintf("certificates are presumably managed by %s (container %s), which renews them itself; this can't be confirmed from the host — check the Certificates page for anything near expiry", kind, pc.Name))
	}
	return unknown(c, title, "certbot.timer is not active — fine if certificates are managed elsewhere (proxy manager, Cloudflare, a container); confirm who renews them")
}
