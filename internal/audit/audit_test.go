package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, time.September, 19, 0, 0, 0, 0, time.UTC)

// legacyWeb is an end-of-life web host: old Ubuntu, password
// SSH, no brute-force protection, an app database published by Docker.
const legacyWeb = `### os
ID=ubuntu
VERSION_ID=18.04
PRETTY=Ubuntu 18.04.6 LTS
### uptime_days
412
### kernel
4.15.0-213-generic
### reboot_required
yes
### pending
68
### sudo
no
### sshd_readable
yes
### sshd
PasswordAuthentication yes
PermitRootLogin yes
PasswordAuthentication no
### bruteforce
fail2ban=inactive
crowdsec=inactive
### autoupdate

### listening
LISTEN 0 128 0.0.0.0:22 0.0.0.0:*
LISTEN 0 511 0.0.0.0:80 0.0.0.0:*
LISTEN 0 511 [::]:80 [::]:*
LISTEN 0 244 0.0.0.0:32771 0.0.0.0:*
LISTEN 0 128 127.0.0.1:5432 0.0.0.0:*
### firewall
Status: inactive
firewalld=inactive
### docker
ok
db_1 postgres:16 0.0.0.0:32771->5432/tcp, :::32771->5432/tcp
### nginx
active=active
version=nginx version: nginx/1.14.0
### nginx_conf_readable
yes
### nginx_tokens

### nginx_ratelimit

### nginx_logformat

### nginx_logs
readable /var/log/nginx/access.log
### log_retention
weekly
rotate 2
### certbot
inactive
`

const hardened = `### os
ID=ubuntu
VERSION_ID=24.04
PRETTY=Ubuntu 24.04.1 LTS
### uptime_days
12
### reboot_required
no
### pending
0
### sudo
yes
### sshd_readable
yes
### sshd
PasswordAuthentication no
PermitRootLogin prohibit-password
### bruteforce
fail2ban=active
crowdsec=inactive
### autoupdate
enabled
### listening
LISTEN 0 128 0.0.0.0:22 0.0.0.0:*
LISTEN 0 511 0.0.0.0:443 0.0.0.0:*
### firewall
Status: active
firewalld=inactive
### docker
ok
app myapp:1 127.0.0.1:3000->3000/tcp
db postgres:17 
### nginx
active=active
version=nginx version: nginx/1.24.0
### nginx_conf_readable
yes
### nginx_tokens
server_tokens off;
### nginx_ratelimit
/etc/nginx/nginx.conf
### nginx_logformat
log_format timed '$remote_addr $request_time';
### nginx_logs
readable /var/log/nginx/access.log
### log_retention
daily
rotate 90
### certbot
active
### workloads
access
ctr app user=node priv=false sock=
ctr db user= priv=false sock=
pguser db postgres_admin
dburl app app_rw
next app 15.5.9
### egress
host=204
rule -N DOCKER-USER
rule -A DOCKER-USER -s 172.16.0.0/12 -o eth0 -j DROP
rule -A DOCKER-USER -j RETURN
### disk
41% /
12% /boot
### inodes
9% /
- /boot/efi
### timesync
yes
### failed_units
ok
### certs
cert example.com Dec  1 10:00:00 2026 GMT
`

func find(t *testing.T, rs []Result, control string) Result {
	t.Helper()
	for _, r := range rs {
		if r.Control == control {
			return r
		}
	}
	t.Fatalf("no result for %q", control)
	return Result{}
}

func TestParseLegacyHost(t *testing.T) {
	f := Parse(legacyWeb)
	if f.OSID != "ubuntu" || f.OSVersion != "18.04" {
		t.Errorf("OS = %q %q", f.OSID, f.OSVersion)
	}
	if f.UptimeDays != 412 || f.Pending != 68 || !f.RebootRequired || f.HaveSudo {
		t.Errorf("uptime/pending/reboot/sudo = %d/%d/%v/%v", f.UptimeDays, f.Pending, f.RebootRequired, f.HaveSudo)
	}
	// sshd honors the first occurrence: the later "no" must not override it.
	if f.PasswordAuth != "yes" {
		t.Errorf("PasswordAuth = %q, want yes (first occurrence wins)", f.PasswordAuth)
	}
	if len(f.BruteForce) != 0 {
		t.Errorf("BruteForce = %v, want none (both inactive)", f.BruteForce)
	}
	if f.AutoUpdates {
		t.Error("AutoUpdates = true, want false")
	}
	if f.LogRetentionDays != 14 {
		t.Errorf("LogRetentionDays = %d, want 14 (weekly x 2)", f.LogRetentionDays)
	}
	// IPv4 and IPv6 sockets of port 80 collapse into one listener.
	ports := 0
	for _, l := range f.Listeners {
		if l.Port == 80 {
			ports++
		}
	}
	if ports != 1 {
		t.Errorf("port 80 listed %d times, want 1", ports)
	}
	if f.DockerState != "ok" || len(f.Published) != 1 || f.Published[0].HostPort != 32771 || f.Published[0].Container != "db_1" {
		t.Errorf("docker = %s %+v", f.DockerState, f.Published)
	}
	if f.Firewall != "ufw-inactive" {
		t.Errorf("Firewall = %q, want ufw-inactive", f.Firewall)
	}
}

func TestEvaluateLegacyHostFailsTheRightThings(t *testing.T) {
	exp := ParseExposure([]string{"exposes:ssh", "exposes:web"})
	rs := Evaluate(exp, Parse(legacyWeb), nil, now)

	for _, tc := range []struct {
		control string
		want    Severity
	}{
		{"os-support", Critical},
		{"ssh-auth", High},
		{"brute-force", High},
		{"exposed-ports", High}, // Postgres published by Docker
	} {
		r := find(t, rs, tc.control)
		if r.Status != Fail || r.Severity != tc.want {
			t.Errorf("%s = %s/%s, want fail/%s (%s)", tc.control, r.Status, r.Severity, tc.want, r.Detail)
		}
	}
	if r := find(t, rs, "exposed-ports"); !strings.Contains(r.Detail, "PostgreSQL") || !strings.Contains(r.Detail, "db_1") {
		t.Errorf("exposed-ports detail should name the service and container: %s", r.Detail)
	}
	if r := find(t, rs, "exposed-ports"); r.Owner != OwnerNetwork {
		t.Errorf("exposed-ports owner = %q, want network", r.Owner)
	}
	// The worst finding sorts first.
	if rs[0].Control != "os-support" {
		t.Errorf("first result = %s, want os-support (critical)", rs[0].Control)
	}
}

func TestSSHAuthExplainsEachProblem(t *testing.T) {
	exp := ParseExposure([]string{"exposes:ssh"})
	rootOnly := Facts{SSHDReadable: true, PasswordAuth: "no", PermitRoot: "yes", BruteForce: []string{"fail2ban"}}
	r := checkSSHAuth(exp, rootOnly)
	if r.Status != Fail || r.Severity != High {
		t.Fatalf("root login = %s/%s, want fail/high", r.Status, r.Severity)
	}
	if strings.Contains(r.Detail, "guessing passwords") {
		t.Errorf("key-only host shouldn't be told about password guessing: %s", r.Detail)
	}
	pwOnly := Facts{SSHDReadable: true, PasswordAuth: "yes", PermitRoot: "no", BruteForce: []string{"fail2ban"}}
	if r := checkSSHAuth(exp, pwOnly); r.Severity != Medium {
		t.Errorf("password auth behind fail2ban = %s, want medium", r.Severity)
	}
}

func TestEvaluateHardenedHostPasses(t *testing.T) {
	rs := Evaluate(ParseExposure([]string{"exposes:ssh", "exposes:web"}), Parse(hardened), nil, now)
	for _, r := range rs {
		if r.Status != Pass {
			t.Errorf("%s = %s (%s), want pass", r.Control, r.Status, r.Detail)
		}
	}
}

func TestProbeConfirmsOrClearsExposedPorts(t *testing.T) {
	f := Parse(legacyWeb)
	exp := ParseExposure([]string{"exposes:web"})

	r := find(t, Evaluate(exp, f, map[int]bool{32771: true}, now), "exposed-ports")
	if r.Severity != Critical || !strings.Contains(r.Detail, "REACHABLE") {
		t.Errorf("reachable database = %s %q, want critical/REACHABLE", r.Severity, r.Detail)
	}
	r = find(t, Evaluate(exp, f, map[int]bool{32771: false}, now), "exposed-ports")
	if r.Status != Pass {
		t.Errorf("probed-closed port = %s (%s), want pass", r.Status, r.Detail)
	}
}

func TestUnknownIsNeverAPass(t *testing.T) {
	// Nothing readable: no sshd_config, no nginx, no os-release.
	rs := Evaluate(ParseExposure(nil), Parse(""), nil, now)
	for _, c := range []string{"os-support", "ssh-auth", "exposed-ports", "web-hardening"} {
		if r := find(t, rs, c); r.Status != Unknown {
			t.Errorf("%s = %s, want unknown when nothing could be read", c, r.Status)
		}
	}
}

func TestExposureDecidesWhichControlsApply(t *testing.T) {
	f := Parse(legacyWeb)

	web := Evaluate(ParseExposure([]string{"exposes:web"}), f, nil, now)
	if r := find(t, web, "ssh-auth"); r.Status != NA {
		t.Errorf("web-only host: ssh-auth = %s, want n/a", r.Status)
	}
	if r := find(t, web, "brute-force"); r.Status != NA {
		t.Errorf("web-only host: brute-force = %s, want n/a", r.Status)
	}

	internal := Evaluate(ParseExposure([]string{"internal"}), f, nil, now)
	if r := find(t, internal, "exposed-ports"); r.Status != NA {
		t.Errorf("internal host: exposed-ports = %s, want n/a", r.Status)
	}

	e := ParseExposure([]string{"prod"})
	if !e.SSH || !e.Web || !e.Assumed {
		t.Errorf("untagged host = %+v, want both assumed", e)
	}
}

func TestOSSupportWindow(t *testing.T) {
	f := Facts{OSID: "ubuntu", OSVersion: "22.04", OSPretty: "Ubuntu 22.04"}
	if r := checkOS(f, now); r.Status != Pass {
		t.Errorf("22.04 in 2026 = %s, want pass", r.Status)
	}
	if r := checkOS(f, time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)); r.Status != Fail || r.Severity != Low {
		t.Errorf("22.04 three months before EOL = %s/%s, want fail/low", r.Status, r.Severity)
	}
	if r := checkOS(Facts{OSID: "ubuntu", OSVersion: "99.04"}, now); r.Status != Unknown {
		t.Errorf("unlisted release = %s, want unknown", r.Status)
	}
}

type fakeDialer struct{ open map[string]bool }

func (d fakeDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	if d.open[address] {
		c, _ := net.Pipe()
		return c, nil
	}
	return nil, errors.New("refused")
}

func TestProbeReportsReachability(t *testing.T) {
	d := fakeDialer{open: map[string]bool{"203.0.113.1:22": true, "203.0.113.1:32771": true}}
	got := Probe(context.Background(), d, "203.0.113.1", []int{32771, 5432}, time.Second)
	want := map[int]bool{22: true, 80: false, 443: false, 32771: true, 5432: false}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("port %d = %v, want %v", p, got[p], w)
		}
	}
}

func TestRenderTextGroupsActionsByOwner(t *testing.T) {
	exp := ParseExposure([]string{"exposes:ssh", "exposes:web"})
	rep := Report{Generated: now, Hosts: []HostReport{
		{Host: "legacy", Exposure: exp.String(), Results: Evaluate(exp, Parse(legacyWeb), nil, now)},
		{Host: "down", Exposure: exp.String(), Error: "Connection timed out"},
	}}
	out := RenderText(rep)
	for _, want := range []string{
		"Read-only", "== legacy", "could not audit: Connection timed out",
		"ACTIONS BY OWNER", string(OwnerSysadmin), string(OwnerNetwork), "[CRITICAL] legacy: OS still receives security updates",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text report missing %q:\n%s", want, out)
		}
	}
}

func TestWriteJSON(t *testing.T) {
	exp := ParseExposure([]string{"exposes:web"})
	rep := Report{Generated: now, Hosts: []HostReport{{Host: "h", Results: Evaluate(exp, Parse(hardened), nil, now)}}}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, rep); err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if !strings.Contains(buf.String(), `"status": "pass"`) {
		t.Errorf("JSON should carry statuses as strings:\n%s", buf.String())
	}
}

// mixedDockerHost runs its databases, services and
// nginx-proxy-manager side by side in Docker, with several containers
// published straight onto the host.
const mixedDockerHost = `### os
ID=ubuntu
VERSION_ID=24.04
PRETTY=Ubuntu 24.04 LTS
### sudo
yes
### listening
LISTEN 0 4096 0.0.0.0:22 0.0.0.0:*
LISTEN 0 4096 0.0.0.0:80 0.0.0.0:*
LISTEN 0 4096 0.0.0.0:81 0.0.0.0:*
LISTEN 0 4096 0.0.0.0:443 0.0.0.0:*
LISTEN 0 4096 0.0.0.0:32771 0.0.0.0:*
LISTEN 0 4096 0.0.0.0:32772 0.0.0.0:*
LISTEN 0 4096 0.0.0.0:32774 0.0.0.0:*
### firewall
Status: active
### docker
ok
postgres postgres:16.3 0.0.0.0:32771->5432/tcp, :::32771->5432/tcp
nodered nodered:3 0.0.0.0:32772->1880/tcp, :::32772->1880/tcp
api_prod api_prod:7 0.0.0.0:32774->1880/tcp
nginx_proxy jc21/nginx-proxy-manager:latest 0.0.0.0:80-81->80-81/tcp, :::80-81->80-81/tcp, 0.0.0.0:443->443/tcp
### nginx
active=inactive
`

func TestParseDockerPortRangesAndImages(t *testing.T) {
	f := Parse(mixedDockerHost)
	if len(f.Containers) != 4 {
		t.Fatalf("Containers = %+v, want 4", f.Containers)
	}
	var got []int
	for _, p := range f.Published {
		if p.Container == "nginx_proxy" {
			got = append(got, p.HostPort)
		}
	}
	if fmt.Sprint(got) != "[80 81 443]" {
		t.Errorf("nginx_proxy published %v, want [80 81 443] (the 80-81 range must expand)", got)
	}
	if _, kind, ok := f.ProxyContainer(); !ok || kind != "nginx-proxy-manager" {
		t.Errorf("ProxyContainer = %q, %v; want nginx-proxy-manager", kind, ok)
	}
}

func TestAdminConsolesAreNamedAndEscalate(t *testing.T) {
	f := Parse(mixedDockerHost)
	exp := ParseExposure([]string{"exposes:ssh", "exposes:web"})

	r := find(t, Evaluate(exp, f, nil, now), "exposed-ports")
	if r.Status != Fail || r.Severity != High {
		t.Fatalf("exposed-ports = %s/%s, want fail/high: %s", r.Status, r.Severity, r.Detail)
	}
	for _, want := range []string{
		"81 (nginx-proxy-manager admin UI)", "32771 (PostgreSQL)", "32772 (Node-RED editor port)",
		"32774 (Node-RED editor port)", "skip its TLS and access lists", "Admin consoles",
	} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail missing %q:\n%s", want, r.Detail)
		}
	}

	// Confirmed reachable: an admin console on the internet is critical.
	r = find(t, Evaluate(exp, f, map[int]bool{81: true, 32772: true}, now), "exposed-ports")
	if r.Severity != Critical || !strings.Contains(r.Detail, "REACHABLE") {
		t.Errorf("reachable console = %s (%s), want critical", r.Severity, r.Detail)
	}
}

func TestWebTierInDockerIsExplainedNotJustUnknown(t *testing.T) {
	r := find(t, Evaluate(ParseExposure([]string{"exposes:web"}), Parse(mixedDockerHost), nil, now), "web-hardening")
	if r.Status != Unknown {
		t.Fatalf("web-hardening = %s, want unknown (can't read a container's nginx from the host)", r.Status)
	}
	for _, want := range []string{"nginx-proxy-manager in container nginx_proxy", "Advanced tab", "unpinned (:latest)"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail missing %q:\n%s", want, r.Detail)
		}
	}
}

func TestProxyManagerHostExplainsLogsAndCertificates(t *testing.T) {
	rs := Evaluate(ParseExposure([]string{"exposes:web"}), Parse(mixedDockerHost), nil, now)
	if r := find(t, rs, "observability"); r.Status != Unknown || !strings.Contains(r.Detail, "nginx-proxy-manager") {
		t.Errorf("observability = %s: %s", r.Status, r.Detail)
	}
	if r := find(t, rs, "tls-renewal"); r.Status != Unknown || !strings.Contains(r.Detail, "Certificates page") {
		t.Errorf("tls-renewal = %s: %s", r.Status, r.Detail)
	}
}

func TestPublicAddressesExcludePrivateVPCAndTailscale(t *testing.T) {
	// Shaped after a cloud droplet that is also on a VPC and a Tailscale net.
	f := Parse(`### addrs
lo 127.0.0.1/8
eth0 203.0.113.9/20
eth0 10.0.50.5/16
eth1 192.168.4.2/20
docker0 172.17.0.1/16
tailscale0 100.64.0.5/32
`)
	if got := f.PublicAddrs(); fmt.Sprint(got) != "[203.0.113.9]" {
		t.Errorf("PublicAddrs = %v, want only the routable address (not RFC1918, CGNAT/Tailscale, loopback)", got)
	}
	nat := Parse("### addrs\neth0 192.168.1.100/24\n")
	if len(nat.PublicAddrs()) != 0 {
		t.Errorf("a host behind NAT has no public address, got %v", nat.PublicAddrs())
	}
}

func TestReachableFindingWarnsAboutSingleVantagePoint(t *testing.T) {
	r := find(t, Evaluate(ParseExposure([]string{"exposes:web"}), Parse(mixedDockerHost), map[int]bool{32771: true}, now), "exposed-ports")
	if !strings.Contains(r.Fix, "repeat from a second network") {
		t.Errorf("a reachable port from one vantage point must say so:\n%s", r.Fix)
	}
}

func TestRenderShowsWhatWasReachable(t *testing.T) {
	rep := Report{Generated: now, Hosts: []HostReport{{
		Host: "h", Exposure: "ssh+web", HaveSudo: true,
		Probed:  map[int]bool{22: true, 80: true, 443: true, 81: false, 32771: true},
		Results: []Result{pass("x", "y", "z")},
	}}}
	out := RenderText(rep)
	if !strings.Contains(out, "reachable from the auditing machine: 22, 80, 443, 32771; not reachable: 81") {
		t.Errorf("text report should list probe results:\n%s", out)
	}
}

func TestTimingIsReadFromTheLogNotJustTheConfig(t *testing.T) {
	// A proxy in a container has no /etc/nginx on the host, so the config says
	// nothing; the log's own last line does.
	exp := ParseExposure([]string{"exposes:web"})
	base := "### nginx\nactive=active\n### nginx_conf_readable\nyes\n### nginx_tokens\nserver_tokens off;\n### nginx_ratelimit\n/x\n### nginx_logs\nreadable /var/log/nginx/site_a_access.log\n"

	timed := Parse(base + "### nginx_timing\nyes\n")
	if !timed.TimingInLog || timed.LogState != "readable" {
		t.Errorf("timed log: TimingInLog=%v LogState=%s", timed.TimingInLog, timed.LogState)
	}
	if r := find(t, Evaluate(exp, timed, nil, now), "observability"); r.Status != Pass {
		t.Errorf("observability with a timed, readable *_access.log = %s: %s", r.Status, r.Detail)
	}

	// The log says no timing even though the config mentions $request_time.
	untimed := Parse(base + "### nginx_logformat\nlog_format x '$request_time';\n### nginx_timing\nno\n")
	if untimed.TimingInLog {
		t.Error("the log is the ground truth: a config mention must not override a log with no timing")
	}
	if r := find(t, Evaluate(exp, untimed, nil, now), "observability"); r.Status != Fail || !strings.Contains(r.Detail, "$request_time") {
		t.Errorf("observability = %s: %s", r.Status, r.Detail)
	}

	// No readable log to sample: fall back to the config.
	if !Parse(base + "### nginx_logformat\nlog_format x '$request_time';\n### nginx_timing\nunknown\n").TimingInLog {
		t.Error("with nothing to sample, the config's log_format should still count")
	}
}

func TestOpsChecksFlagTrouble(t *testing.T) {
	f := Parse(`### disk
93% /
81% /var/lib/docker
### inodes
50% /
### timesync
no
### failed_units
ok
backup.service
nginx.service
### certs
cert old.example Sep 10 00:00:00 2026 GMT
cert soon.example Sep 29 00:00:00 2026 GMT
cert fine.example Dec  1 00:00:00 2026 GMT
`)
	got := map[string]Result{}
	for _, r := range opsChecks(f, now) {
		got[r.Control] = r
	}
	for control, sev := range map[string]Severity{"disk-space": High, "time-sync": Medium, "failed-units": Medium, "cert-expiry": Critical} {
		if r := got[control]; r.Status != Fail || r.Severity != sev {
			t.Errorf("%s = %s/%s (%s), want fail/%s", control, r.Status, r.Severity, r.Detail, sev)
		}
	}
	for control, want := range map[string]string{
		"disk-space":   "/var/lib/docker 81%",
		"failed-units": "backup.service, nginx.service",
		"cert-expiry":  "soon.example expires in 10 days",
	} {
		if !strings.Contains(got[control].Detail, want) {
			t.Errorf("%s detail %q missing %q", control, got[control].Detail, want)
		}
	}
}

func TestOpsUnknownsAreNotPasses(t *testing.T) {
	f := Parse("### certs\nunreadable\n### failed_units\nnone\n")
	got := map[string]Status{}
	for _, r := range opsChecks(f, now) {
		got[r.Control] = r.Status
	}
	want := map[string]Status{"disk-space": Unknown, "time-sync": Unknown, "failed-units": NA, "cert-expiry": Unknown}
	for c, st := range want {
		if got[c] != st {
			t.Errorf("%s = %s, want %s", c, got[c], st)
		}
	}
}
