package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"meterm/internal/agents"
	"meterm/internal/config"
	"meterm/internal/sshrun"
)

// exitErr is a real *exec.ExitError with this code.
func exitErr(t *testing.T, code int) error {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
	var e *exec.ExitError
	if !errors.As(err, &e) || e.ExitCode() != code {
		t.Fatalf("no exit error for %d: %v", code, err)
	}
	return err
}

func agentJobFor(name string) agentJob {
	return agentJob{label: name, resolve: func(context.Context) config.Resolved {
		return config.Resolved{Name: name, Address: "192.0.2.1"}
	}}
}

// Nothing that goes wrong on a host reads as that host having no agents.
func TestAskAgentsNeverReportsFailureAsZero(t *testing.T) {
	const hostile = "\x1b]52;c;aGk=\x07 \x1b[2J pwned"
	for _, tc := range []struct {
		name          string
		stdout        string
		stderr        string
		overflow      bool
		err           error
		reachable     bool
		wantErrSubstr string
	}{
		{name: "ssh refused", err: exitErr(t, 255), stderr: "ssh: connect to host x port 22: Connection refused", wantErrSubstr: "connection refused"},
		{name: "bad key", err: exitErr(t, 255), stderr: "bcd@x: Permission denied (publickey).", wantErrSubstr: "refused the key"},
		{name: "changed key", err: exitErr(t, 255), stderr: "REMOTE HOST IDENTIFICATION HAS CHANGED", wantErrSubstr: "host key"},
		{name: "hostile banner", err: exitErr(t, 255), stderr: hostile, wantErrSubstr: "couldn't connect"},
		{name: "no claude", err: exitErr(t, 127), stderr: hostile, reachable: true, wantErrSubstr: "claude not found"},
		{name: "claude failed", err: exitErr(t, 1), stdout: "[]", stderr: hostile, reachable: true, wantErrSubstr: "claude agents failed"},
		{name: "ssh start", err: errors.New("exec: \"ssh\": not found\x1b[2J"), wantErrSubstr: "couldn't run ssh"},
		{name: "bad json", stdout: hostile, reachable: true, wantErrSubstr: "unexpected output"},
		{name: "empty", stdout: "", reachable: true, wantErrSubstr: "unexpected output"},
		{name: "null", stdout: "null", reachable: true, wantErrSubstr: "unexpected output"},
		{name: "wrong type", stdout: `[{"pid":"x"}]`, reachable: true, wantErrSubstr: "pid"},
		{name: "too much", overflow: true, err: exitErr(t, 1), reachable: true, wantErrSubstr: "more than meterm reads"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := askAgents(context.Background(), agentJobFor("h"), func(context.Context, config.Resolved) ([]byte, string, bool, error) {
				return []byte(tc.stdout), tc.stderr, tc.overflow, tc.err
			}, time.Second)
			if got.Error == "" || !strings.Contains(got.Error, tc.wantErrSubstr) {
				t.Errorf("error = %q, want %q", got.Error, tc.wantErrSubstr)
			}
			if got.Agents != nil {
				t.Errorf("agents = %v", got.Agents)
			}
			if got.Reachable != tc.reachable {
				t.Errorf("reachable = %v", got.Reachable)
			}
			if strings.ContainsAny(got.Error, "\x1b\x07") || strings.Contains(got.Error, "pwned") {
				t.Errorf("remote text in the error: %q", got.Error)
			}
			var rep bytes.Buffer
			report := agents.Report{Schema: 1, Hosts: []agents.Host{got}}
			if err := writeAgents(&rep, report, "json"); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(rep.String(), `"agents":null`) {
				t.Errorf("json: %s", rep.String())
			}
			rep.Reset()
			if err := writeAgents(&rep, report, "text"); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(rep.String(), "no agents") {
				t.Errorf("text says none: %s", rep.String())
			}
		})
	}
}

func TestAskAgentsTimeout(t *testing.T) {
	got := askAgents(context.Background(), agentJobFor("h"), func(ctx context.Context, _ config.Resolved) ([]byte, string, bool, error) {
		<-ctx.Done()
		return nil, "", false, ctx.Err()
	}, 10*time.Millisecond)
	if got.Reachable || !strings.Contains(got.Error, "timed out") || got.Agents != nil {
		t.Errorf("got %+v", got)
	}
}

func TestAskAgentsEmptyIsAnAnswer(t *testing.T) {
	got := askAgents(context.Background(), agentJobFor("h"), func(context.Context, config.Resolved) ([]byte, string, bool, error) {
		return []byte("[]\n"), "", false, nil
	}, time.Second)
	if !got.Reachable || got.Error != "" || got.Agents == nil || len(got.Agents) != 0 {
		t.Errorf("got %+v", got)
	}
	var rep bytes.Buffer
	if err := writeAgents(&rep, agents.Report{Schema: 1, Hosts: []agents.Host{got}}, "json"); err != nil {
		t.Fatal(err)
	}
	if want := `{"schema":1,"hosts":[{"host":"h","reachable":true,"error":"","agents":[]}]}` + "\n"; rep.String() != want {
		t.Errorf("got %s want %s", rep.String(), want)
	}
}

func TestAskAgentsDoesntRunForAnUnsafeHost(t *testing.T) {
	job := agentJob{label: "bad", resolve: func(context.Context) config.Resolved {
		return config.Resolved{Err: errors.New("bad\x1b[2J: address \"-oProxyCommand=x\"")}
	}}
	got := askAgents(context.Background(), job, func(context.Context, config.Resolved) ([]byte, string, bool, error) {
		t.Error("it ran")
		return nil, "", false, nil
	}, time.Second)
	if got.Reachable || got.Error == "" || strings.Contains(got.Error, "\x1b") {
		t.Errorf("got %+v", got)
	}
}

// Hostile text from a host or the config gets out of neither format as it went in.
func TestWriteAgentsHostileText(t *testing.T) {
	const bad = "x\x1b]52;c;aGk=\x07\x1b[2J\u009b1m‮⁦​\ny"
	enc, _ := json.Marshal(bad)
	stdout := `[{"cwd":` + string(enc) + `,"name":` + string(enc) + `,"status":` + string(enc) + `,"waitingFor":` + string(enc) + `,"kind":` + string(enc) + `,"id":` + string(enc) + `}]`
	job := agentJobFor(bad)
	rep := runAgents(context.Background(), []agentJob{job}, func(context.Context, config.Resolved) ([]byte, string, bool, error) {
		return []byte(stdout), "", false, nil
	}, time.Second, 1)
	for _, format := range []string{"text", "json"} {
		var out bytes.Buffer
		if err := writeAgents(&out, rep, format); err != nil {
			t.Fatal(err)
		}
		s := out.String()
		for _, c := range []string{"\x1b", "\x07", "\u009b", "‮", "⁦", "​", `\u001b`, `\u0007`, `‮`, ` `} {
			if strings.Contains(s, c) {
				t.Errorf("%s: %q in %q", format, c, s)
			}
		}
		if format == "text" && strings.Count(s, "\n") != 2 {
			t.Errorf("a row was broken up: %q", s)
		}
	}
}

func hostsForAgents(t *testing.T, yaml string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

const agentsHosts = `version: 1
groups:
  live-group: {env: production}
  via-live: {proxy_jump: live}
hosts:
- {name: dev1, address: 192.0.2.1, tags: [web]}
- {name: dev2, address: 192.0.2.2, group: g}
- {name: live, address: 192.0.2.3, env: production, tags: [web]}
- {name: live2, address: 192.0.2.4, env: production, group: g}
- {name: inherit, address: 192.0.2.5, group: live-group, tags: [inh]}
- {name: typo, address: 192.0.2.6, env: prodution, tags: [inh]}
- {name: j-name, address: 192.0.2.10, proxy_jump: live, tags: [j]}
- {name: j-user, address: 192.0.2.11, proxy_jump: "deploy@live", tags: [j]}
- {name: j-port, address: 192.0.2.12, proxy_jump: "live:2222", tags: [j]}
- {name: j-addr, address: 192.0.2.13, proxy_jump: "192.0.2.3", tags: [j]}
- {name: j-useraddr, address: 192.0.2.14, proxy_jump: "root@192.0.2.3:22", tags: [j]}
- {name: j-chain, address: 192.0.2.15, proxy_jump: "dev3", tags: [j]}
- {name: j-second, address: 192.0.2.16, proxy_jump: "dev1,live", tags: [j]}
- {name: j-group, address: 192.0.2.17, group: via-live, tags: [j]}
- {name: j-route, address: 192.0.2.18, tags: [j], routes: [{name: r, proxy_jump: live}]}
- {name: j-routeaddr, address: 192.0.2.19, tags: [j], routes: [{name: r, address: 192.0.2.3}]}
- {name: j-alias, address: 192.0.2.3, tags: [j]}
- {name: j-liveroute, address: 192.0.2.30, tags: [j], proxy_jump: "198.51.100.7"}
- {name: live3, address: 192.0.2.31, env: production, routes: [{name: pub, address: 198.51.100.7}]}
- {name: dev3, address: 192.0.2.20, proxy_jump: live}
- {name: ok-jump, address: 192.0.2.21, proxy_jump: dev1, tags: [ok]}
- {name: same-address, address: 192.0.2.3, port: 2222, tags: [never]}
`

func TestSelectAgentHostsProduction(t *testing.T) {
	cfg := hostsForAgents(t, agentsHosts)
	for _, tc := range []struct {
		name         string
		names        []string
		tags, groups []string
		want         map[string]bool // label → refused
	}{
		{"by tag", nil, []string{"web"}, nil, map[string]bool{"dev1": false, "live": true}},
		{"by group", nil, nil, []string{"g"}, map[string]bool{"dev2": false, "live2": true}},
		{"named", []string{"live"}, nil, nil, map[string]bool{"live": false}},
		{"named and by tag", []string{"live"}, []string{"web"}, nil, map[string]bool{"dev1": false, "live": false}},
		{"named with a login", []string{"root@live"}, nil, nil, map[string]bool{"root@live": false}},
		{"env from the group, or a typo", nil, []string{"inh"}, nil, map[string]bool{"inherit": true, "typo": true}},
		{"a jump through a good host", nil, []string{"ok"}, nil, map[string]bool{"ok-jump": false}},
		{"named, through production", []string{"j-name"}, nil, nil, map[string]bool{"j-name": false}},
		{"every way through production", nil, []string{"j"}, nil, map[string]bool{
			"j-name": true, "j-user": true, "j-port": true, "j-addr": true, "j-useraddr": true, "j-chain": true, "j-second": true,
			"j-group": true, "j-route": true, "j-routeaddr": true, "j-alias": true, "j-liveroute": true}},
	} {
		jobs, err := selectAgentHosts(cfg, tc.names, tc.tags, tc.groups)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		got := map[string]bool{}
		for _, j := range jobs {
			got[j.label] = j.refused != ""
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %v", tc.name, got)
		}
		for k, v := range tc.want {
			if r, ok := got[k]; !ok || r != v {
				t.Errorf("%s: %s refused = %v, %v; got %v", tc.name, k, r, ok, got)
			}
		}
	}
}

// A refused host is listed, makes the run incomplete, and isn't contacted.
func TestRefusedProductionIsReportedNotAsked(t *testing.T) {
	cfg := hostsForAgents(t, agentsHosts)
	jobs, err := selectAgentHosts(cfg, nil, []string{"web"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rep := runAgents(context.Background(), jobs, func(_ context.Context, r config.Resolved) ([]byte, string, bool, error) {
		if r.Name == "live" {
			t.Error("the production host was asked")
		}
		return []byte("[]"), "", false, nil
	}, time.Second, 2)
	if agentsFailed(rep) != 1 || rep.Hosts[1].Host != "live" || rep.Hosts[1].Reachable || rep.Hosts[1].Agents != nil {
		t.Errorf("got %+v", rep)
	}
}

func TestSelectAgentHostsRefusals(t *testing.T) {
	cfg := hostsForAgents(t, agentsHosts)
	for _, tc := range []struct {
		names, tags []string
	}{
		{[]string{"nope"}, nil},
		{[]string{"dev1", "nope"}, nil},
		{[]string{"-oProxyCommand=touch /tmp/x"}, nil},
		{[]string{"$(touch /tmp/x)"}, nil},
		{nil, []string{"no-such-tag"}},
	} {
		if _, err := selectAgentHosts(cfg, tc.names, tc.tags, nil); err == nil {
			t.Errorf("%v %v: accepted", tc.names, tc.tags)
		}
	}
}

// A login given with the host can't become a flag: Resolve refuses it first.
func TestHostileLoginAndAddressDontReachSSH(t *testing.T) {
	cfg := hostsForAgents(t, agentsHosts+`- {name: evil, address: "-oProxyCommand=touch /tmp/x"}
- {name: "dollar", address: "$(touch /tmp/x)"}
- {name: "semi", address: "a;b"}
- {name: "jump", address: 192.0.2.9, proxy_jump: "-oProxyCommand=x"}
`)
	for _, arg := range []string{"-oProxyCommand=x@dev1", "a b@dev1", "$(x)@dev1", "evil", "dollar", "semi", "jump"} {
		jobs, err := selectAgentHosts(cfg, []string{arg}, nil, nil)
		if err != nil {
			continue
		}
		got := askAgents(context.Background(), jobs[0], func(context.Context, config.Resolved) ([]byte, string, bool, error) {
			t.Errorf("%s: ssh would have run", arg)
			return nil, "", false, nil
		}, time.Second)
		if got.Error == "" {
			t.Errorf("%s: no error", arg)
		}
	}
}

// What real ssh makes of the arguments: the options that matter hold against everything
// the allowlist lets through, and against what's in the generated config.
func TestAgentSSHArgsAsSSHReadsThem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("agents refuses to run on Windows")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh")
	}
	sshConfig, remove, err := writeAgentSSHConfig()
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	want := map[string]string{
		"batchmode": "yes", "stricthostkeychecking": "true", "updatehostkeys": "false",
		"controlmaster": "false", "controlpersist": "no",
		"requesttty": "false", "clearallforwardings": "yes", "forwardagent": "no",
		"forwardx11": "no", "permitlocalcommand": "no", "connecttimeout": "8",
	}
	for name, extra := range map[string][]string{
		"none":             nil,
		"their BatchMode":  {"-o", "BatchMode=no"},
		"their tty":        {"-t"},
		"their tty, twice": {"-tt"},
		"glued":            {"-oBatchMode=no", "-p2222", "-iid"},
	} {
		if err := checkAgentExtraArgs(extra); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		r := config.Resolved{Address: "192.0.2.1", ExtraArgs: extra}
		args := append([]string{"-G"}, sshrun.BuildArgs(config.Resolved{Address: "192.0.2.1", ExtraArgs: isolatedSSHArgs(t, agentSSHArgs(r, sshConfig, 8*time.Second))})...)
		out, err := exec.Command("ssh", args...).Output()
		if err != nil {
			t.Fatalf("%s: ssh -G %v: %v", name, args, err)
		}
		got := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(line, " "); ok {
				got[k] = v
			}
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", name, k, got[k], v)
			}
		}
		if got["hostname"] != "192.0.2.1" {
			t.Errorf("%s: goes to %s", name, got["hostname"])
		}
		if p, there := got["controlpath"]; there { // ssh -G leaves "none" out
			t.Errorf("%s: controlpath %s", name, p)
		}
	}
}

// The reason for the allowlist, as real ssh shows it: each of these does what the
// options set before it say shouldn't happen.
func TestRefusedExtraArgsReallyAreDangerous(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("agents refuses to run on Windows")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh")
	}
	for name, tc := range map[string]struct {
		extra     []string
		key, want string
	}{
		"-A beats ForwardAgent=no": {[]string{"-A"}, "forwardagent", "yes"},
		"-X beats ForwardX11=no":   {[]string{"-X"}, "forwardx11", "yes"},
		"-M beats ControlMaster":   {[]string{"-M"}, "controlmaster", "true"},
		"-S beats ControlPath":     {[]string{"-S", "/tmp/x"}, "controlpath", "/tmp/x"},
		"a bare word is the host":  {[]string{"evil.example"}, "hostname", "evil.example"},
	} {
		if err := checkAgentExtraArgs(tc.extra); err == nil {
			t.Errorf("%s: allowed", name)
		}
		r := config.Resolved{Address: "192.0.2.1", ExtraArgs: tc.extra}
		args := append([]string{"-G"}, sshrun.BuildArgs(config.Resolved{Address: "192.0.2.1", ExtraArgs: isolatedSSHArgs(t, agentSSHArgs(r, "", time.Second))})...)
		out, err := exec.Command("ssh", args...).Output()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(string(out), "\n"+tc.key+" "+tc.want+"\n") {
			t.Errorf("%s: ssh didn't do that, so the test's reason is out of date", name)
		}
	}
}

func TestCheckAgentExtraArgs(t *testing.T) {
	for _, ok := range [][]string{
		nil,
		{"-p", "2222"}, {"-p2222"},
		{"-i", "~/.ssh/key"}, {"-i/home/a/key"},
		{"-l", "deploy"},
		{"-4"}, {"-6"}, {"-C"}, {"-q"}, {"-T"}, {"-t"}, {"-v"},
		{"-o", "Port=22"}, {"-oPort=22"}, {"-o", "Port 22"}, {"-o", "PORT=22"},
		{"-o", "User=deploy"}, {"-o", "IdentityFile=~/.ssh/k"}, {"-o", "IdentitiesOnly=yes"},
		{"-o", "ConnectTimeout=5"}, {"-o", "ServerAliveInterval=15"}, {"-o", "BatchMode=no"},
	} {
		if err := checkAgentExtraArgs(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range [][]string{
		{"evil.example"}, {"-p", "22", "evil.example"}, {"sudo", "rm"},
		{"--"}, {"--", "evil.example"},
		{"-A"}, {"-X"}, {"-Y"}, {"-M"}, {"-S", "/tmp/x"}, {"-f"}, {"-N"}, {"-n"}, {"-s"}, {"-G"}, {"-V"},
		{"-L", "1:a:2"}, {"-R", "1:a:2"}, {"-D", "1080"}, {"-W", "a:1"}, {"-w", "1"}, {"-J", "evil"}, {"-F", "/tmp/c"},
		{"-E", "/tmp/log"}, {"-e", "x"}, {"-b", "1.1.1.1"}, {"-c", "aes"}, {"-m", "x"}, {"-I", "x"}, {"-P", "x"}, {"-Q", "x"}, {"-O", "exit"}, {"-B", "en0"},
		{"-AX"}, {"-qA"}, {"-pfoo"}, {"-p", "0"}, {"-p", "99999"}, {"-p"}, {"-i"}, {"-o"}, {"-l"},
		{"-l", "-oProxyCommand=x"}, {"-l", "a b"},
		{"-i", "-oProxyCommand=x"}, {"-i", "a b"}, {"-i", "%d/k"}, {"-i", "$(x)"}, {"-i", "a\nb"},
		{"-o", "ProxyCommand=touch /tmp/x"}, {"-oProxyCommand=x"}, {"-o", "proxycommand x"}, {"-o", "ProxyJump=evil"},
		{"-o", "LocalCommand=x"}, {"-o", "PermitLocalCommand=yes"}, {"-o", "KnownHostsCommand=x"}, {"-o", "Match=exec x"},
		{"-o", "PKCS11Provider=x"}, {"-o", "SecurityKeyProvider=x"}, {"-o", "SendEnv=*"}, {"-o", "SetEnv=A=b"},
		{"-o", "HostName=evil"}, {"-o", "Include=/tmp/c"}, {"-o", "RemoteCommand=x"}, {"-o", "RemoteForward=1 a:2"}, {"-o", "LocalForward=1 a:2"},
		{"-o", "DynamicForward=1"}, {"-o", "ForwardAgent=yes"}, {"-o", "ForwardX11=yes"}, {"-o", "Tunnel=yes"}, {"-o", "ControlMaster=yes"}, {"-o", "ControlPath=/tmp/x"},
		{"-o", "StrictHostKeyChecking=no"}, {"-o", "UserKnownHostsFile=/dev/null"}, {"-o", "GlobalKnownHostsFile=/dev/null"}, {"-o", "UpdateHostKeys=yes"},
		{"-o", "IdentityAgent=/tmp/s"}, {"-o", "CertificateFile=/tmp/c"},
		{"-o", "Port=22 ProxyCommand=x"}, {"-o", "User=$(x)"}, {"-o", "IdentityFile=%d/x"}, {"-o", "Port"}, {"-o", "Port="}, {"-o", "User=-x"},
		{"-o", "\"ProxyCommand\"=x"},
		{"\x1b]0;x\x07"},
	} {
		err := checkAgentExtraArgs(bad)
		if err == nil {
			t.Errorf("%q: allowed", bad)
		}
	}
}

// Through askAgents: a host with one of these is a per-host error, and ssh doesn't run.
func TestAgentsDontRunWithHostileExtraArgs(t *testing.T) {
	const bad = "evil\x1b]0;x\x07"
	job := agentJob{label: "x", resolve: func(context.Context) config.Resolved {
		return config.Resolved{Address: "192.0.2.1", ExtraArgs: []string{bad}}
	}}
	got := askAgents(context.Background(), job, func(context.Context, config.Resolved) ([]byte, string, bool, error) {
		t.Error("ssh ran")
		return nil, "", false, nil
	}, time.Second)
	if got.Reachable || got.Agents != nil || !strings.Contains(got.Error, "extra_args") || strings.ContainsAny(got.Error, "\x1b\x07") {
		t.Errorf("got %+v", got)
	}
}

// A look stops at --timeout even while it's picking a route, and writes neither file.
func TestAgentsLookBoundedAndLeavesNoState(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "hosts.yaml")
	// Port 1 of a TEST-NET address never answers, so only the context ends the dial.
	yaml := "version: 1\nhosts:\n- name: a\n  address: 192.0.2.1\n  routes:\n  - {name: r1, address: 192.0.2.2}\n  - {name: r2, address: 192.0.2.3}\n  - {name: r3, address: 192.0.2.4}\n"
	if err := os.WriteFile(cfgFile, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := selectAgentHosts(cfg, []string{"a"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got := askAgents(context.Background(), jobs[0], func(ctx context.Context, _ config.Resolved) ([]byte, string, bool, error) {
		t.Error("ssh ran past the deadline")
		return nil, "", false, ctx.Err()
	}, 200*time.Millisecond)
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("took %s", d)
	}
	if !strings.Contains(got.Error, "timed out") {
		t.Errorf("got %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "routes.json")); !os.IsNotExist(err) {
		t.Errorf("routes.json: %v", err)
	}
}

func TestCappedBuffer(t *testing.T) {
	over := 0
	b := &cappedBuffer{max: 4, over: func() { over++ }}
	for _, s := range []string{"ab", "cd", "e", "fg"} {
		if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("%q: %d %v", s, n, err)
		}
	}
	if !b.overflow || b.b != nil || over != 1 {
		t.Errorf("%+v, over %d", b, over)
	}
}

// fakeSSHScript puts an ssh first on PATH that saves its argv, one per line, and then
// does what body says.
func fakeSSHScript(t *testing.T, body string) (argvFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >>'" + argvFile + "'; done\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvFile
}

// Through the real command line builder and exec: the remote command is the constant
// and it's the last argument, whatever the host is called.
func TestSSHAgentQueryRunsTheOneCommand(t *testing.T) {
	argvFile := fakeSSHScript(t, `echo '[{"pid":1,"cwd":"/a"}]'`)
	job := agentJob{label: "x", resolve: func(context.Context) config.Resolved {
		return config.Resolved{Name: "$(touch /tmp/x)", Address: "192.0.2.1", Forwards: []config.Forward{{Type: "L", LocalPort: 1, RemoteHost: "a", RemotePort: 2}}}
	}}
	got := askAgents(context.Background(), job, sshAgentQuery("", 30*time.Second), 30*time.Second)
	if got.Error != "" || !got.Reachable || len(got.Agents) != 1 {
		t.Fatalf("got %+v", got)
	}
	b, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if argv[len(argv)-1] != agents.RemoteCommand || argv[len(argv)-2] != "192.0.2.1" {
		t.Errorf("argv ends %v", argv[len(argv)-2:])
	}
	if slices.Contains(argv, "-L") || strings.Contains(string(b), "$(touch") {
		t.Errorf("argv: %v", argv)
	}
}

func TestSSHAgentQueryStopsAtTheLimit(t *testing.T) {
	fakeSSHScript(t, `head -c 3000000 /dev/zero; sleep 30`)
	start := time.Now()
	got := askAgents(context.Background(), agentJobFor("x"), sshAgentQuery("", 20*time.Second), 20*time.Second)
	if !strings.Contains(got.Error, "more than meterm reads") {
		t.Errorf("got %+v", got)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %s", d)
	}
}

// The command has no activity or history of its own to write, even on a good run.
func TestAgentsWritesNoActivityOrHistory(t *testing.T) {
	fakeSSHScript(t, `echo '[]'`)
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(cfgFile, []byte(agentsHosts), 0o600); err != nil {
		t.Fatal(err)
	}
	activity, history, team := filepath.Join(dir, "activity"), filepath.Join(dir, "history.json"), filepath.Join(dir, "team")
	t.Setenv("METERM_CONFIG", cfgFile)
	t.Setenv("METERM_ACTIVITY_DIR", activity)
	t.Setenv("METERM_HISTORY", history)
	t.Setenv("METERM_TEAM_DIR", team)
	c := newAgentsCmd()
	c.SetArgs([]string{"dev1", "--format", "json"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{activity, history, team} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s appeared: %v", p, err)
		}
	}
}

func TestAgentsCmdRefusals(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(cfgFile, []byte(agentsHosts), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METERM_CONFIG", cfgFile)
	argvFile := fakeSSHScript(t, `echo '[]'`)
	for _, args := range [][]string{
		{},
		{"--format", "json"},
		{"dev1", "--format", "yaml"},
		{"dev1", "--timeout", "0s"},
		{"dev1", "-j", "0"},
		{"dev1", "-j", "100000"},
		{"--tag", "no-such"},
		{"--", "-oProxyCommand=touch /tmp/x"},
		{"no-such"},
	} {
		c := newAgentsCmd()
		c.SetArgs(args)
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
	if _, err := os.Stat(argvFile); !os.IsNotExist(err) {
		t.Error("ssh ran")
	}
}

func TestAskAgentsStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := askAgents(ctx, agentJobFor("h"), func(ctx context.Context, _ config.Resolved) ([]byte, string, bool, error) {
		return []byte("[]"), "", false, ctx.Err()
	}, time.Second)
	if got.Reachable || got.Error == "" || got.Agents != nil {
		t.Errorf("got %+v", got)
	}
}

func TestAgentsRefusedOnWindows(t *testing.T) {
	if err := checkAgentsClient("linux"); err != nil {
		t.Error(err)
	}
	if err := checkAgentsClient("darwin"); err != nil {
		t.Error(err)
	}
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(cfgFile, []byte(agentsHosts), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METERM_CONFIG", cfgFile)
	old := agentsGOOS
	agentsGOOS = "windows"
	defer func() { agentsGOOS = old }()
	c := newAgentsCmd()
	c.SetArgs([]string{"dev1"})
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "Windows") {
		t.Errorf("err = %v", err)
	}
}

// A hop in the URI form, through the real selection.
func TestSelectAgentHostsRefusesURIHop(t *testing.T) {
	cfg := hostsForAgents(t, agentsHosts+`- {name: via-uri, address: 192.0.2.40, tags: [uri], proxy_jump: "ssh://192.0.2.3"}
- {name: via-uri2, address: 192.0.2.41, tags: [uri], proxy_jump: "ssh://live"}
- {name: via-other-port, address: 192.0.2.42, tags: [uri], proxy_jump: "192.0.2.3:2200"}
- {name: alias-port, address: 192.0.2.3, port: 22, tags: [uri]}
`)
	jobs, err := selectAgentHosts(cfg, nil, []string{"uri"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 4 {
		t.Fatalf("%d jobs", len(jobs))
	}
	for _, j := range jobs {
		if j.refused == "" {
			t.Errorf("%s wasn't refused", j.label)
		}
	}
}
