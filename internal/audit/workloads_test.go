package audit

import (
	"testing"
)

// compromised is what a broken-into web host tends to look like: an old
// Next.js in a container that could call out freely and the app on the
// database superuser.
const compromised = `### docker
ok
nextjs-app webapp-nextjs 0.0.0.0:4202->3000/tcp
postgres postgres:17.4 0.0.0.0:5431->5432/tcp
### workloads
access
ctr nextjs-app user=nextjs priv=false sock=
ctr postgres user= priv=false sock=
pguser postgres myuser
dburl nextjs-app myuser
next nextjs-app 15.3.0
### egress
host=204
rule -N DOCKER-USER
rule -A DOCKER-USER -j RETURN
`

func TestWorkloadChecksCatchACompromisedHost(t *testing.T) {
	f := Parse(compromised)
	got := map[string]Result{}
	for _, r := range workloadChecks(f) {
		got[r.Control] = r
	}
	want := map[string]Severity{
		"framework-cves":   Critical,
		"db-superuser":     Medium,
		"container-egress": Medium,
	}
	for control, sev := range want {
		r := got[control]
		if r.Status != Fail || r.Severity != sev {
			t.Errorf("%s = %s/%s (%s), want fail/%s", control, r.Status, r.Severity, r.Detail, sev)
		}
	}
	if got["container-privileges"].Status != Pass {
		t.Errorf("container-privileges = %s, want pass (nothing privileged here)", got["container-privileges"].Status)
	}
}

func TestNextVerdict(t *testing.T) {
	cases := []struct {
		v    string
		want Severity
	}{
		{"15.3.0", Critical}, // before the fix
		{"15.5.6", Critical},
		{"15.5.7", High}, // RCE fixed, follow-ups not
		{"15.5.9", Info},
		{"15.5.27", Info}, // after the fix
		{"16.0.6", Critical},
		{"16.0.10", Info},
		{"16.1.0", Info},
		{"14.2.20", Critical}, // middleware bypass
		{"14.2.30", High},
		{"14.2.35", Info},
		{"13.5.6", High}, // unsupported major
		{"15.6.0-canary.3", High},
		{"garbage", Info},
	}
	for _, c := range cases {
		if got, _, _ := nextVerdict(c.v); got != c.want {
			t.Errorf("nextVerdict(%s) = %s, want %s", c.v, got, c.want)
		}
	}
}

func TestEgressVerdicts(t *testing.T) {
	base := Facts{DockerState: "ok", Workload: WorkloadFacts{DockerAccess: true, Containers: []WorkloadContainer{{Name: "app"}}}}

	blocked := base
	blocked.Workload.HostEgress = "blocked"
	if r := checkEgress(blocked); r.Status != Pass {
		t.Errorf("blocked upstream = %s, want pass", r.Status)
	}
	noSudo := base
	noSudo.Workload.HostEgress = "open"
	if r := checkEgress(noSudo); r.Status != Fail {
		t.Errorf("open, rules unreadable = %s, want fail", r.Status)
	}
	filtered := noSudo
	filtered.Workload.RulesKnown = true
	filtered.Workload.DockerUserRules = []string{"-A DOCKER-USER -s 172.19.0.0/16 -j REJECT"}
	if r := checkEgress(filtered); r.Status != Pass {
		t.Errorf("DOCKER-USER rejects = %s, want pass", r.Status)
	}
}

func TestNoDockerAccessIsUnknownNotPass(t *testing.T) {
	f := Facts{DockerState: "noaccess"}
	for _, r := range []Result{checkFramework(f), checkContainerPrivileges(f), checkDBSuperuser(f)} {
		if r.Status != Unknown {
			t.Errorf("%s without docker access = %s, want unknown", r.Control, r.Status)
		}
	}
}
