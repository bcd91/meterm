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
	return agentJob{label: name, resolve: func() config.Resolved {
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
	job := agentJob{label: "bad", resolve: func() config.Resolved {
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
hosts:
- {name: dev1, address: 192.0.2.1, tags: [web]}
- {name: dev2, address: 192.0.2.2, group: g}
- {name: live, address: 192.0.2.3, env: production, tags: [web]}
- {name: live2, address: 192.0.2.4, env: production, group: g}
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

func TestAgentSSHArgs(t *testing.T) {
	r := config.Resolved{Address: "192.0.2.1", ExtraArgs: []string{"-o", "BatchMode=no", "-t", "-o", "ControlPath=/tmp/theirs"}}
	args := agentSSHArgs(r, 8*time.Second, "/tmp/meterm-cm-501/abc")
	at := func(s string) int { return slices.Index(args, s) }
	for _, want := range []string{"BatchMode=yes", "ConnectTimeout=8", "RemoteCommand=none", "ClearAllForwardings=yes", "ControlMaster=auto", "ControlPath=/tmp/meterm-cm-501/abc", "ControlPersist=60"} {
		if at(want) < 0 {
			t.Errorf("no %q in %v", want, args)
		}
	}
	// ssh keeps the first value of an option, and the last of -t and -T.
	if at("BatchMode=yes") > at("BatchMode=no") || at("ControlPath=/tmp/meterm-cm-501/abc") > at("ControlPath=/tmp/theirs") {
		t.Errorf("the host's options come first: %v", args)
	}
	if args[len(args)-1] != "-T" {
		t.Errorf("-T isn't last: %v", args)
	}
	args = agentSSHArgs(config.Resolved{}, 1500*time.Millisecond, "")
	if slices.Contains(args, "ControlMaster=auto") || !slices.Contains(args, "ControlPath=none") || !slices.Contains(args, "ControlMaster=no") || !slices.Contains(args, "ConnectTimeout=2") {
		t.Errorf("no master: %v", args)
	}
}

func TestAgentControlPathNeverForProduction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no masters on Windows")
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", t.TempDir())
	for _, tc := range []struct {
		name string
		r    config.Resolved
		none bool
	}{
		{"plain", config.Resolved{Address: "a", Env: "dev"}, false},
		{"unlabelled", config.Resolved{Address: "a"}, false},
		{"production", config.Resolved{Address: "a", Env: config.EnvProduction}, true},
		{"through production", config.Resolved{Address: "a", ProxyJump: []config.Resolved{{Address: "j", Env: config.EnvProduction}}}, true},
		{"two hops in", config.Resolved{Address: "a", ProxyJump: []config.Resolved{{Address: "j", ProxyJump: []config.Resolved{{Address: "k", Env: config.EnvProduction}}}}}, true},
	} {
		if got := agentControlPath(tc.r); (got == "") != tc.none {
			t.Errorf("%s: %q", tc.name, got)
		}
	}
}

func TestControlPathInFitsAndIsPlain(t *testing.T) {
	ok := func(string) error { return nil }
	r := config.Resolved{Address: "192.0.2.1", User: "u"}
	long := "/var/folders/1s/" + strings.Repeat("n", 90) + "/T"
	for _, tc := range []struct {
		name  string
		bases []string
		want  string // the prefix it must have; "" for none
	}{
		{"short", []string{"/tmp"}, "/tmp/"},
		{"macOS TMPDIR", []string{"/var/folders/1s/n4lg_d252s5_wtsdqyww7p2h0000gn/T/"}, "/var/folders/1s/"},
		{"too long falls through", []string{long, "/tmp"}, "/tmp/"},
		{"too long and nothing else", []string{long}, ""},
		{"a space", []string{"/tmp/a b", "/tmp"}, "/tmp/meterm-cm"},
		{"a token", []string{"/tmp/%h", "/tmp"}, "/tmp/meterm-cm"},
		{"a quote", []string{"/tmp/a'b", "/tmp"}, "/tmp/meterm-cm"},
		{"a newline", []string{"/tmp/a\nb", "/tmp"}, "/tmp/meterm-cm"},
		{"relative", []string{"tmp"}, ""},
		{"empty", []string{"", "/tmp"}, "/tmp/"},
	} {
		got := controlPathIn(r, tc.bases, ok)
		if tc.want == "" {
			if got != "" {
				t.Errorf("%s: %q", tc.name, got)
			}
			continue
		}
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: %q", tc.name, got)
		}
		if len(got) > agentControlPathMax {
			t.Errorf("%s: %d bytes", tc.name, len(got))
		}
		// What ssh binds at first: the path, a dot, 16 random characters and a NUL.
		if len(got)+18+1 > 104 {
			t.Errorf("%s: won't fit in sun_path: %d", tc.name, len(got))
		}
	}
	if got := controlPathIn(r, []string{"/nope", "/tmp"}, func(d string) error {
		if strings.HasPrefix(d, "/nope") {
			return errors.New("someone else's")
		}
		return nil
	}); !strings.HasPrefix(got, "/tmp/") {
		t.Errorf("a directory that isn't safe was used: %q", got)
	}
	if got := controlPathIn(r, []string{"/nope"}, func(string) error { return errors.New("no") }); got != "" {
		t.Errorf("got %q", got)
	}
	other := r
	other.Address = "192.0.2.2"
	if controlPathIn(r, []string{"/tmp"}, ok) == controlPathIn(other, []string{"/tmp"}, ok) {
		t.Error("two destinations share a master")
	}
	routed := r
	routed.ProxyJumpRaw = "bastion"
	if controlPathIn(r, []string{"/tmp"}, ok) == controlPathIn(routed, []string{"/tmp"}, ok) {
		t.Error("two routes share a master")
	}
}

// `dev` stays off every master, whatever `agents` does.
func TestDevStaysOffMasters(t *testing.T) {
	if !slices.Contains(devSSHOptions, "ControlPath=none") {
		t.Errorf("dev: %v", devSSHOptions)
	}
	for _, o := range devSSHOptions {
		if strings.Contains(o, "ControlPersist") || strings.Contains(o, "meterm-cm") {
			t.Errorf("dev: %v", devSSHOptions)
		}
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
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", "")
	job := agentJob{label: "x", resolve: func() config.Resolved {
		return config.Resolved{Name: "$(touch /tmp/x)", Address: "192.0.2.1", Forwards: []config.Forward{{Type: "L", LocalPort: 1, RemoteHost: "a", RemotePort: 2}}}
	}}
	got := askAgents(context.Background(), job, sshAgentQuery(30*time.Second), 30*time.Second)
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
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", "")
	start := time.Now()
	got := askAgents(context.Background(), agentJobFor("x"), sshAgentQuery(20*time.Second), 20*time.Second)
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
	t.Setenv("TMPDIR", dir)
	t.Setenv("XDG_RUNTIME_DIR", "")
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
