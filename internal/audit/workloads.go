package audit

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// workloadScript looks inside what the host runs, for the failure modes of a
// compromised web app rather than a misconfigured server: containers with
// more power than they need, an app framework with a known RCE, nothing
// stopping a container from calling out.
//
// Still read-only. Two things reach further than the base script and are
// worth knowing about: `docker exec` runs `sh` inside containers to read
// node_modules/next/package.json, and the host makes one HTTP request to
// connectivitycheck.gstatic.com to learn whether it can reach the internet.
// Environment variables are read to compare database user names, and only
// user names are printed — never passwords or full URLs.
const workloadScript = `s workloads
D=""
if command -v docker >/dev/null 2>&1; then
  if q docker ps -q >/dev/null; then D="docker"; elif q sudo -n docker ps -q >/dev/null; then D="sudo -n docker"; fi
fi
if [ -n "$D" ]; then
  echo access
  for c in $($D ps --format '{{.Names}}' </dev/null 2>/dev/null); do
    $D inspect -f "ctr $c user={{.Config.User}} priv={{.HostConfig.Privileged}} sock={{range .Mounts}}{{if eq .Source \"/var/run/docker.sock\"}}yes{{end}}{{end}}" "$c" </dev/null 2>/dev/null
    $D inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$c" </dev/null 2>/dev/null | sed -n -e "s/^POSTGRES_USER=\(.*\)/pguser $c \1/p" -e "s#^[A-Za-z_]*=postgres\(ql\)\{0,1\}://\([^:@/]*\)[:@].*#dburl $c \2#p"
    v=$($D exec "$c" sh -c 'for p in /app /usr/src/app /home/node/app /opt/app /srv/app /var/www/app; do f=$p/node_modules/next/package.json; if [ -r "$f" ]; then sed -n "s/.*\"version\": *\"\([^\"]*\)\".*/\1/p" "$f" | head -1; exit 0; fi; done' </dev/null 2>/dev/null)
    [ -n "$v" ] && echo "next $c $v"
  done
else
  echo noaccess
fi
s egress
if command -v curl >/dev/null 2>&1; then echo "host=$(q curl -s -m 5 -o /dev/null -w '%{http_code}' http://connectivitycheck.gstatic.com/generate_204)"
elif command -v wget >/dev/null 2>&1; then if q wget -q -T 5 -O /dev/null http://connectivitycheck.gstatic.com/generate_204; then echo host=204; else echo host=000; fi
else echo host=unknown; fi
if q sudo -n iptables -S DOCKER-USER >/dev/null; then q sudo -n iptables -S DOCKER-USER | sed 's/^/rule /'; else echo rules=unknown; fi
`

// WorkloadFacts is what workloadScript found.
type WorkloadFacts struct {
	DockerAccess bool // could list and inspect containers
	Containers   []WorkloadContainer
	PGUsers      map[string]string // postgres container -> POSTGRES_USER (its superuser)
	DBURLUsers   map[string]string // container -> user in its postgres:// URL

	HostEgress      string   // open, blocked, unknown
	DockerUserRules []string // iptables -S DOCKER-USER, when readable
	RulesKnown      bool
}

// WorkloadContainer is one running container as seen from inside.
type WorkloadContainer struct {
	Name        string
	User        string // Config.User; "" means root
	Privileged  bool
	DockerSock  bool // /var/run/docker.sock mounted in
	NextVersion string
}

func parseWorkloads(f *Facts, sec map[string]string) {
	w := WorkloadFacts{PGUsers: map[string]string{}, DBURLUsers: map[string]string{}, HostEgress: "unknown"}
	byName := map[string]*WorkloadContainer{}
	get := func(name string) *WorkloadContainer {
		if c, ok := byName[name]; ok {
			return c
		}
		w.Containers = append(w.Containers, WorkloadContainer{Name: name})
		c := &w.Containers[len(w.Containers)-1]
		byName[name] = c
		return c
	}
	// Collect first, then build, so pointers into w.Containers stay valid.
	var rows [][]string
	for _, line := range lines(sec["workloads"]) {
		if line == "access" {
			w.DockerAccess = true
			continue
		}
		rows = append(rows, strings.Fields(line))
	}
	names := map[string]bool{}
	for _, r := range rows {
		if len(r) >= 2 && (r[0] == "ctr" || r[0] == "next") {
			names[r[1]] = true
		}
	}
	w.Containers = make([]WorkloadContainer, 0, len(names))
	for _, r := range rows {
		if len(r) < 2 {
			continue
		}
		switch r[0] {
		case "ctr":
			c := get(r[1])
			for _, kv := range r[2:] {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "user":
					c.User = v
				case "priv":
					c.Privileged = v == "true"
				case "sock":
					c.DockerSock = v == "yes"
				}
			}
		case "next":
			if len(r) >= 3 {
				get(r[1]).NextVersion = r[2]
			}
		case "pguser":
			if len(r) >= 3 {
				w.PGUsers[r[1]] = r[2]
			}
		case "dburl":
			if len(r) >= 3 {
				w.DBURLUsers[r[1]] = r[2]
			}
		}
	}

	for _, line := range lines(sec["egress"]) {
		switch {
		case strings.HasPrefix(line, "host="):
			switch code := strings.TrimPrefix(line, "host="); {
			case code == "unknown" || code == "":
				w.HostEgress = "unknown"
			case code == "000":
				w.HostEgress = "blocked"
			default: // any HTTP answer at all means packets got out
				w.HostEgress = "open"
			}
		case strings.HasPrefix(line, "rule "):
			w.RulesKnown = true
			w.DockerUserRules = append(w.DockerUserRules, strings.TrimPrefix(line, "rule "))
		}
	}
	f.Workload = w
}

// workloadChecks are evaluated alongside the base checks.
func workloadChecks(f Facts) []Result {
	return []Result{
		checkFramework(f),
		checkContainerPrivileges(f),
		checkDBSuperuser(f),
		checkEgress(f),
	}
}

// ---- Next.js versions -------------------------------------------------------

// nextFixes lists, per minor line, the first release with the fix for the
// RSC remote-code-execution flaw (CVE-2025-55182 / CVE-2025-66478) and the
// first with the December 2025 follow-ups (CVE-2025-55183 source exposure,
// CVE-2025-55184 / CVE-2025-67779 denial of service). It is a floor, not an
// advisory feed: `npm audit` in the app's repo gives the full picture.
var nextFixes = map[[2]int]struct{ rce, all [3]int }{
	{15, 0}: {[3]int{15, 0, 5}, [3]int{15, 0, 7}},
	{15, 1}: {[3]int{15, 1, 9}, [3]int{15, 1, 11}},
	{15, 2}: {[3]int{15, 2, 6}, [3]int{15, 2, 8}},
	{15, 3}: {[3]int{15, 3, 6}, [3]int{15, 3, 8}},
	{15, 4}: {[3]int{15, 4, 8}, [3]int{15, 4, 10}},
	{15, 5}: {[3]int{15, 5, 7}, [3]int{15, 5, 9}},
	{16, 0}: {[3]int{16, 0, 7}, [3]int{16, 0, 10}},
}

// nextVerdict judges one Next.js version: sev is Info when it is fine.
func nextVerdict(v string) (sev Severity, why, upgradeTo string) {
	if strings.Contains(v, "canary") {
		return High, "a canary (pre-release) build, which doesn't get security releases", "a stable release"
	}
	p, ok := semver(v)
	if !ok {
		return Info, "", ""
	}
	less := func(a, b [3]int) bool {
		for i := range a {
			if a[i] != b[i] {
				return a[i] < b[i]
			}
		}
		return false
	}
	ver := func(x [3]int) string { return fmt.Sprintf("%d.%d.%d", x[0], x[1], x[2]) }
	switch {
	case p[0] <= 13:
		return High, "a major version that no longer receives security fixes", "15.5.9 or later"
	case p[0] == 14:
		if less(p, [3]int{14, 2, 25}) {
			return Critical, "vulnerable to middleware authorization bypass (CVE-2025-29927)", "14.2.35"
		}
		if less(p, [3]int{14, 2, 35}) {
			return High, "vulnerable to denial of service via React Server Components (CVE-2025-55184)", "14.2.35"
		}
		return Info, "", ""
	}
	fix, known := nextFixes[[2]int{p[0], p[1]}]
	if !known {
		return Info, "", "" // newer than this table knows about
	}
	if less(p, fix.rce) {
		return Critical, "vulnerable to unauthenticated remote code execution through React Server Components (CVE-2025-55182)", ver(fix.all)
	}
	if less(p, fix.all) {
		return High, "vulnerable to source-code exposure and denial of service (CVE-2025-55183, CVE-2025-55184)", ver(fix.all)
	}
	return Info, "", ""
}

func semver(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, s := range parts {
		n, err := strconv.Atoi(s)
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

func checkFramework(f Facts) Result {
	const c, title = "framework-cves", "Web frameworks have no known critical vulnerabilities"
	w := f.Workload
	if f.DockerState == "none" {
		return na(c, title, "no Docker on this host (apps outside containers aren't inspected)")
	}
	if !w.DockerAccess {
		return unknown(c, title, "can't inspect containers (no Docker access for the audit account)")
	}
	worst := Info
	var bad, fine []string
	upgrade := ""
	for _, ct := range w.Containers {
		if ct.NextVersion == "" {
			continue
		}
		sev, why, to := nextVerdict(ct.NextVersion)
		if sev == Info {
			fine = append(fine, fmt.Sprintf("%s: Next.js %s", ct.Name, ct.NextVersion))
			continue
		}
		bad = append(bad, fmt.Sprintf("%s runs Next.js %s, %s", ct.Name, ct.NextVersion, why))
		if sev > worst {
			worst, upgrade = sev, to
		}
	}
	if len(bad) > 0 {
		return fail(c, title, worst, OwnerApp, strings.Join(bad, "; "),
			fmt.Sprintf("Upgrade Next.js (and react/react-dom to the matching patched release) to %s or later, rebuild and redeploy the container. Assume the old container was compromised if it faced the internet: rotate secrets it could read.", upgrade))
	}
	if len(fine) == 0 {
		return na(c, title, "no Next.js apps found in containers")
	}
	return pass(c, title, strings.Join(fine, "; "))
}

// ---- container privileges --------------------------------------------------

func checkContainerPrivileges(f Facts) Result {
	const c, title = "container-privileges", "Containers can't take over the host"
	w := f.Workload
	if f.DockerState == "none" {
		return na(c, title, "no Docker on this host")
	}
	if !w.DockerAccess {
		return unknown(c, title, "can't inspect containers (no Docker access for the audit account)")
	}
	var bad []string
	for _, ct := range w.Containers {
		switch {
		case ct.Privileged:
			bad = append(bad, ct.Name+" runs --privileged")
		case ct.DockerSock:
			bad = append(bad, ct.Name+" has /var/run/docker.sock mounted")
		}
	}
	if len(bad) > 0 {
		return fail(c, title, High, OwnerSysadmin, strings.Join(bad, "; ")+" — whoever compromises it controls the whole host",
			"Drop --privileged; mount the Docker socket only into tools that truly need it (and keep those off the internet), or use a socket proxy that allows read-only calls.")
	}
	if len(w.Containers) == 0 {
		return na(c, title, "no running containers")
	}
	return pass(c, title, fmt.Sprintf("%d container(s), none privileged or holding the Docker socket", len(w.Containers)))
}

// ---- database superuser ----------------------------------------------------

func checkDBSuperuser(f Facts) Result {
	const c, title = "db-superuser", "Apps don't use the database superuser"
	w := f.Workload
	if !w.DockerAccess {
		if f.DockerState == "none" {
			return na(c, title, "no Docker on this host")
		}
		return unknown(c, title, "can't inspect containers (no Docker access for the audit account)")
	}
	if len(w.DBURLUsers) == 0 {
		return na(c, title, "no container has a postgres:// connection string")
	}
	supers := map[string]bool{"postgres": true}
	for _, u := range w.PGUsers {
		supers[u] = true // the official image makes POSTGRES_USER a superuser
	}
	var bad []string
	apps := make([]string, 0, len(w.DBURLUsers))
	for app := range w.DBURLUsers {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	for _, app := range apps {
		if u := w.DBURLUsers[app]; supers[u] {
			bad = append(bad, fmt.Sprintf("%s connects as %s", app, u))
		}
	}
	if len(bad) > 0 {
		return fail(c, title, Medium, OwnerApp, strings.Join(bad, "; ")+" — a database superuser, so a bug or break-in in the app can read every database and run commands in the database container (COPY … FROM PROGRAM)",
			"Create a role with only the rights the app needs (CONNECT, and SELECT/INSERT/UPDATE/DELETE on its tables), point the app's connection string at it, and keep the superuser for administration.")
	}
	return pass(c, title, "apps connect with their own database roles")
}

// ---- egress ----------------------------------------------------------------

func checkEgress(f Facts) Result {
	const c, title = "container-egress", "Containers can't call out to the internet freely"
	w := f.Workload
	if f.DockerState == "none" || (w.DockerAccess && len(w.Containers) == 0) {
		return na(c, title, "no running containers")
	}
	switch w.HostEgress {
	case "blocked":
		return pass(c, title, "this host can't reach the internet directly (blocked upstream)")
	case "unknown":
		return unknown(c, title, "couldn't test outbound access (no curl or wget on the host)")
	}
	if !w.RulesKnown {
		return fail(c, title, Medium, OwnerNetwork, "this host reaches the internet, and the DOCKER-USER firewall chain couldn't be read (no sudo), so containers most likely can too",
			"Limit outbound traffic for this server on the perimeter firewall (allow only DNS, NTP, package and image registries, and the APIs the apps call), or add DROP rules for container subnets in the DOCKER-USER chain.")
	}
	for _, r := range w.DockerUserRules {
		if strings.Contains(r, "-j DROP") || strings.Contains(r, "-j REJECT") {
			return pass(c, title, "the DOCKER-USER chain filters container traffic")
		}
	}
	return fail(c, title, Medium, OwnerNetwork, "this host reaches the internet and nothing filters container traffic (DOCKER-USER has no DROP/REJECT rules): a compromised container can download tools and send data out",
		"Limit outbound traffic for this server on the perimeter firewall (allow only DNS, NTP, package and image registries, and the APIs the apps call), or add DROP rules for container subnets in the DOCKER-USER chain.")
}
