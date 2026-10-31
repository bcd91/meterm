package audit

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// opsScript gathers the day-to-day health a DevOps team checks every
// morning: full disks, a clock that drifted, services that died, and
// certificates about to expire. Read-only like the rest; certificates are
// read with `sudo -n` only when /etc/letsencrypt/live isn't readable.
const opsScript = `s disk
(df -P -x tmpfs -x devtmpfs -x overlay -x squashfs 2>/dev/null || df -P 2>/dev/null) </dev/null | tail -n +2 | awk '{print $5, $6}'
s inodes
(df -Pi -x tmpfs -x devtmpfs -x overlay -x squashfs 2>/dev/null || df -Pi 2>/dev/null) </dev/null | tail -n +2 | awk '{print $5, $6}'
s timesync
if command -v timedatectl >/dev/null 2>&1; then q timedatectl show -p NTPSynchronized --value || echo unknown; else echo unknown; fi
s failed_units
if command -v systemctl >/dev/null 2>&1; then echo ok; q systemctl list-units --state=failed --no-legend --plain | awk '{print $1}' | head -10; else echo none; fi
s certs
L=/etc/letsencrypt/live
if [ -d $L ]; then
  if [ -r $L ] && [ -x $L ]; then P=""; elif q sudo -n true; then P="sudo -n"; else P=skip; echo unreadable; fi
  if [ "$P" != skip ]; then
    for d in $($P ls $L 2>/dev/null </dev/null); do
      [ "$d" = README ] && continue
      e=$($P openssl x509 -enddate -noout -in $L/$d/cert.pem 2>/dev/null </dev/null | cut -d= -f2)
      [ -n "$e" ] && echo "cert $d $e"
    done
  fi
else echo none; fi
`

// OpsFacts is what opsScript found.
type OpsFacts struct {
	Disk, Inodes []Usage
	TimeSync     string // yes, no, unknown
	UnitsKnown   bool   // systemd is there to ask
	FailedUnits  []string
	CertState    string // ok, none, unreadable
	Certs        []Cert
}

// Usage is how full one mounted filesystem is.
type Usage struct {
	Mount   string
	Percent int
}

// Cert is one Let's Encrypt certificate on the host.
type Cert struct {
	Name     string
	NotAfter time.Time
}

func parseOps(f *Facts, sec map[string]string) {
	o := OpsFacts{TimeSync: "unknown", CertState: "ok"}
	o.Disk = parseUsage(sec["disk"])
	o.Inodes = parseUsage(sec["inodes"])
	switch strings.TrimSpace(sec["timesync"]) {
	case "yes":
		o.TimeSync = "yes"
	case "no":
		o.TimeSync = "no"
	}
	for i, line := range lines(sec["failed_units"]) {
		if i == 0 {
			o.UnitsKnown = line == "ok"
			continue
		}
		o.FailedUnits = append(o.FailedUnits, line)
	}
	for _, line := range lines(sec["certs"]) {
		switch {
		case line == "none" || line == "unreadable":
			o.CertState = line
		case strings.HasPrefix(line, "cert "):
			fs := strings.SplitN(line, " ", 3)
			if len(fs) < 3 {
				continue
			}
			// openssl: "Mar  5 12:00:00 2027 GMT"
			t, err := time.Parse("Jan _2 15:04:05 2006 MST", strings.Join(strings.Fields(fs[2]), " "))
			if err != nil {
				t, err = time.Parse("Jan 2 15:04:05 2006 MST", strings.Join(strings.Fields(fs[2]), " "))
			}
			if err == nil {
				o.Certs = append(o.Certs, Cert{Name: fs[1], NotAfter: t})
			}
		}
	}
	f.Ops = o
}

func parseUsage(s string) []Usage {
	var out []Usage
	for _, line := range lines(s) {
		fs := strings.Fields(line)
		if len(fs) < 2 {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(fs[0], "%"))
		if err != nil {
			continue // "-" on filesystems without inodes
		}
		out = append(out, Usage{Mount: fs[1], Percent: n})
	}
	return out
}

func opsChecks(f Facts, now time.Time) []Result {
	return []Result{
		checkDisk(f),
		checkTimeSync(f),
		checkFailedUnits(f),
		checkCertExpiry(f, now),
	}
}

func checkDisk(f Facts) Result {
	const c, title = "disk-space", "Filesystems have room left"
	if len(f.Ops.Disk) == 0 {
		return unknown(c, title, "couldn't read df")
	}
	sev := Info
	var bad []string
	note := func(kind string, us []Usage) {
		for _, u := range us {
			switch {
			case u.Percent >= 90:
				sev = max(sev, High)
			case u.Percent >= 80:
				sev = max(sev, Medium)
			default:
				continue
			}
			bad = append(bad, fmt.Sprintf("%s %d%% %s", u.Mount, u.Percent, kind))
		}
	}
	note("full", f.Ops.Disk)
	note("of inodes used", f.Ops.Inodes)
	if len(bad) > 0 {
		return fail(c, title, sev, OwnerSysadmin, strings.Join(bad, ", "),
			"Free space before it hits 100% (old logs, docker images: `docker system df`, package caches) or grow the volume; a full disk stops databases and logging first.")
	}
	return pass(c, title, "every filesystem under 80% (space and inodes)")
}

func checkTimeSync(f Facts) Result {
	const c, title = "time-sync", "Clock is synchronised"
	switch f.Ops.TimeSync {
	case "yes":
		return pass(c, title, "NTP synchronised")
	case "no":
		return fail(c, title, Medium, OwnerSysadmin, "the clock is not NTP-synchronised: TLS, tokens, cron and log timelines all drift with it",
			"Enable time sync: `timedatectl set-ntp true` (systemd-timesyncd) or run chrony.")
	}
	return unknown(c, title, "couldn't ask timedatectl")
}

func checkFailedUnits(f Facts) Result {
	const c, title = "failed-units", "No systemd services have failed"
	if !f.Ops.UnitsKnown {
		return na(c, title, "no systemd on this host")
	}
	if len(f.Ops.FailedUnits) > 0 {
		return fail(c, title, Medium, OwnerSysadmin, "failed: "+strings.Join(f.Ops.FailedUnits, ", "),
			"See why with `systemctl status <unit>` and `journalctl -u <unit>`; fix and restart it, or disable it if it's no longer needed.")
	}
	return pass(c, title, "no failed units")
}

func checkCertExpiry(f Facts, now time.Time) Result {
	const c, title = "cert-expiry", "TLS certificates aren't about to expire"
	switch f.Ops.CertState {
	case "none":
		return na(c, title, "no Let's Encrypt certificates on the host (/etc/letsencrypt/live)")
	case "unreadable":
		return unknown(c, title, "/etc/letsencrypt/live isn't readable without sudo")
	}
	if len(f.Ops.Certs) == 0 {
		return na(c, title, "no certificates in /etc/letsencrypt/live")
	}
	certs := append([]Cert(nil), f.Ops.Certs...)
	sort.Slice(certs, func(i, j int) bool { return certs[i].NotAfter.Before(certs[j].NotAfter) })
	sev := Info
	var bad []string
	for _, ct := range certs {
		days := int(ct.NotAfter.Sub(now).Hours() / 24)
		switch {
		case !ct.NotAfter.After(now):
			sev = max(sev, Critical)
			bad = append(bad, fmt.Sprintf("%s expired %s", ct.Name, ct.NotAfter.Format("2006-01-02")))
		case days < 14:
			sev = max(sev, High)
			bad = append(bad, fmt.Sprintf("%s expires in %d days", ct.Name, days))
		case days < 30:
			sev = max(sev, Medium)
			bad = append(bad, fmt.Sprintf("%s expires in %d days", ct.Name, days))
		}
	}
	if len(bad) > 0 {
		return fail(c, title, sev, OwnerSysadmin, strings.Join(bad, ", "),
			"Renewal isn't happening: run `certbot renew --dry-run` to see why (DNS, port 80 blocked, a stale vhost), fix it, then `certbot renew`.")
	}
	return pass(c, title, fmt.Sprintf("%d certificate(s), soonest expiry %s", len(certs), certs[0].NotAfter.Format("2006-01-02")))
}
