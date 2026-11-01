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
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/activity"
	"meterm/internal/config"
	"meterm/internal/logins"
	"meterm/internal/sshrun"
)

func fakeSSHExit(t *testing.T, code int) error {
	t.Helper()
	return exitErrorOf(t, code)
}

func neverAsked(t *testing.T) agentQuery {
	return func(context.Context, config.Resolved) ([]byte, string, bool, error) {
		t.Error("ssh ran")
		return nil, "", false, errors.New("no")
	}
}

const hostileText = "\x1b]52;c;aGk=\x07 \x1b[2J pwned ‮"

func assertNoEscapes(t *testing.T, what, s string) {
	t.Helper()
	for _, bad := range []string{"\x1b", "\x07", "‮", "\u009b", `\u001b`, `\u0007`, `‮`, `\u009b`} {
		if strings.Contains(s, bad) {
			t.Errorf("%s has %q: %q", what, bad, s)
		}
	}
}

// What real ssh prints up to the point the host is proven, from OpenSSH 10.3.
const (
	authStart  = "debug1: Authenticating to 192.0.2.1:22 as 'a'\n"
	authProven = "debug1: SSH2_MSG_KEXINIT received\n" +
		"debug1: kex: algorithm: mlkem768x25519-sha256\n" +
		"debug1: Server host key: ssh-ed25519 SHA256:abc\n" +
		"debug1: Host '192.0.2.1' is known and matches the ED25519 host key.\n" +
		"debug1: Found key in /x/known_hosts:1\n" +
		"debug1: SSH2_MSG_NEWKEYS received\n" +
		"debug1: SSH2_MSG_SERVICE_ACCEPT received\n"
	authMark = authStart + authProven
)

func TestParseAuthMethods(t *testing.T) {
	for _, c := range []struct {
		name, in string
		want     []string
		found    bool
		dropped  int
	}{
		{"real", authMark + "debug1: Authentications that can continue: publickey,password\na@1.2.3.4: Permission denied (publickey,password).\n", []string{"publickey", "password"}, true, 0},
		{"keys only", authMark + "debug1: Authentications that can continue: publickey\r\n", []string{"publickey"}, true, 0},
		{"denied alone may be a jump host's", authMark + "x@1.2.3.4: Permission denied (publickey).\n", nil, false, 0},
		{"denied with no marker is a jump host's", "x@hop: Permission denied (publickey).\n", nil, false, 0},
		{"the last one wins", authMark + "debug1: Authentications that can continue: password\ndebug1: Authentications that can continue: publickey\n", []string{"publickey"}, true, 0},
		{"before the marker doesn't count", "debug1: Authentications that can continue: publickey\n" + authMark, nil, false, 0},
		{"a host name another case", "debug1: Authenticating to 192.0.2.1:22 as 'a'\n" + authProven + "debug1: Authentications that can continue: publickey\n", []string{"publickey"}, true, 0},
		{"one start, another name, as with an alias", "debug1: Authenticating to real.example:2222 as 'a'\n" + authProven + "debug1: Authentications that can continue: publickey\n", []string{"publickey"}, true, 0},
		{"two starts, and the last isn't this host", authMark + "debug1: Authenticating to 192.0.2.9:22 as 'a'\n" + authProven + "debug1: Authentications that can continue: publickey\n", nil, false, 0},
		{"an answer with no key check", authStart + "debug1: Authentications that can continue: publickey\n", nil, false, 0},
		{"new keys with no host key match", authStart + "debug1: SSH2_MSG_NEWKEYS received\ndebug1: Authentications that can continue: publickey\n", nil, false, 0},
		{"a key match with no new keys", authStart + "debug1: Host 'x' is known and matches the ED25519 host key.\ndebug1: Authentications that can continue: publickey\n", nil, false, 0},
		{"a key match, more lines, no new keys", authStart + "debug1: Host 'x' is known and matches the ED25519 host key.\ndebug1: Found key in /x:1\ndebug1: Authentications that can continue: publickey\n", nil, false, 0},
		{"everything but a start", authProven + "debug1: Authentications that can continue: publickey\n", nil, false, 0},
		// All of it forged, in one message from the other end, before any key is checked.
		{"forged in a disconnect", authStart + "Received disconnect from 192.0.2.1 port 22:2: x\n" + authProven + "debug1: Authentications that can continue: publickey\n", nil, false, 0},
		{"forged in a debug message", authStart + "debug1: Remote: x\n" + authProven + "debug1: Authentications that can continue: publickey\n", nil, false, 0},
		{"forged in a kex offer", authStart + "Unable to negotiate with 192.0.2.1 port 22: no matching key exchange method found. Their offer: x\n" + authProven + "debug1: Authentications that can continue: publickey\n", nil, false, 0},
		{"forged after a hop's words", "a@hop: Permission denied (publickey).\n" + authMark + "debug1: Authentications that can continue: publickey\n", nil, false, 0},
		{"a verified host that then disconnects", authMark + "debug1: Authentications that can continue: publickey\nReceived disconnect from 192.0.2.1 port 22:2: bye\n", nil, false, 0},
		{"nothing to go on", "debug1: Connecting to host\n", nil, false, 0},
		{"not at a line's start", authMark + "banner debug1: Authentications that can continue: password\n", nil, false, 0},
		{"hostile names dropped", authMark + "debug1: Authentications that can continue: publickey,\x1b[2J,$(touch x),A,a b,password\n", []string{"publickey", "password"}, true, 4},
		{"hostile only", authMark + "debug1: Authentications that can continue: \x1b]0;x\x07\n", []string{}, true, 1},
		{"long name dropped", authMark + "debug1: Authentications that can continue: " + strings.Repeat("a", 65) + "\n", []string{}, true, 1},
		{"many cut", authMark + "debug1: Authentications that can continue: " + strings.Repeat("a,", 20) + "\n", repeat("a", authProbeMaxMethods), true, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := parseAuthMethods(c.in, "192.0.2.1", 22)
			if got.found != c.found || got.dropped != c.dropped || strings.Join(got.methods, "|") != strings.Join(c.want, "|") {
				t.Errorf("got %+v", got)
			}
		})
	}
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func yesNo(b *bool) string {
	switch {
	case b == nil:
		return "unknown"
	case *b:
		return "yes"
	}
	return "no"
}

func TestAskAuthMethods(t *testing.T) {
	denied := fakeSSHExit(t, 255)
	failed := fakeSSHExit(t, 1)
	direct := agentJobFor("h")
	viaHop := agentJob{label: "h", resolve: func(context.Context) config.Resolved {
		return config.Resolved{Name: "h", Address: "192.0.2.1", ProxyJumpRaw: "hop"}
	}}
	const (
		cont    = "debug1: Authentications that can continue: "
		noneNew = "Authenticated to 192.0.2.1 ([192.0.2.1]:22) using \"none\".\n"
		noneOld = "debug1: Authentication succeeded (none).\n"
	)
	for _, c := range []struct {
		name         string
		job          agentJob
		stderr       string
		err          error
		reachable    bool
		password, kb string
		none         string
		wantErr      string
	}{
		{name: "keys only", job: direct, stderr: authMark + cont + "publickey\n", err: denied, reachable: true, password: "no", kb: "no", none: "no"},
		{name: "password", job: direct, stderr: authMark + cont + "publickey,password\n", err: denied, reachable: true, password: "yes", kb: "no", none: "no"},
		{name: "keyboard", job: direct, stderr: authMark + cont + "keyboard-interactive\n", err: denied, reachable: true, password: "no", kb: "yes", none: "no"},
		{name: "a name that can't be read means no false no", job: direct, stderr: authMark + cont + "publickey,Bad Name\n", err: denied, reachable: true, password: "unknown", kb: "unknown", none: "no"},
		{name: "anyone gets in", job: direct, reachable: true, password: "unknown", kb: "unknown", none: "yes"},
		{name: "anyone gets in, and the command fails", job: direct, stderr: authMark + noneNew, err: failed, reachable: true, password: "unknown", kb: "unknown", none: "yes"},
		{name: "anyone gets in, older ssh", job: direct, stderr: authMark + noneOld, err: failed, reachable: true, password: "unknown", kb: "unknown", none: "yes"},
		{name: "a banner can't claim it, as the real answer follows", job: direct, stderr: authMark + noneNew + cont + "publickey\n", err: denied, reachable: true, password: "no", kb: "no", none: "no"},
		{name: "a banner's list loses to the real one", job: direct, stderr: authMark + cont + "password\n" + noneNew, err: failed, reachable: true, password: "unknown", kb: "unknown", none: "yes"},
		{name: "forged before any key was checked", job: direct, stderr: authStart + "Received disconnect from 192.0.2.1 port 22:2: x\n" + authProven + cont + "publickey\n", err: denied, password: "unknown", kb: "unknown", none: "unknown", wantErr: "unknown"},
		{name: "an alias that never matched isn't a refused key", job: direct, stderr: "a@real: Permission denied (publickey).\n", err: denied, password: "unknown", kb: "unknown", none: "unknown", wantErr: "unknown"},
		{name: "refused", job: direct, stderr: "ssh: connect to host x port 22: Connection refused\n", err: denied, password: "unknown", kb: "unknown", none: "unknown", wantErr: "connection refused"},
		{name: "host's words stay out", job: direct, stderr: hostileText, err: denied, password: "unknown", kb: "unknown", none: "unknown", wantErr: "couldn't connect"},
		{name: "ssh won't start", job: direct, err: errors.New("exec: not found"), password: "unknown", kb: "unknown", none: "unknown", wantErr: "couldn't run ssh"},
		// The worst one: never the hop's answer as the target's.
		{name: "a jump host refuses", job: viaHop, stderr: "a@hop: Permission denied (publickey).\n", err: denied, password: "unknown", kb: "unknown", none: "unknown", wantErr: "jump host"},
		{name: "a jump host's list", job: viaHop, stderr: "a@hop: Permission denied (publickey).\ndebug1: Authenticating to 192.0.2.1:22 as 'a'\n" + cont + "publickey\n", err: denied, password: "unknown", kb: "unknown", none: "unknown", wantErr: "jump host"},
		{name: "through a jump host, from the target", job: viaHop, stderr: "debug1: noise\n" + authMark + cont + "password\n", err: denied, reachable: true, password: "yes", kb: "no", none: "no"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := askAuthMethods(context.Background(), c.job, func(context.Context, config.Resolved) ([]byte, string, bool, error) {
				return nil, c.stderr, false, c.err
			}, time.Second)
			if got.Reachable != c.reachable || yesNo(got.PasswordLogin) != c.password || yesNo(got.KeyboardInteractive) != c.kb || yesNo(got.NoneAccepted) != c.none {
				t.Errorf("%+v: password %s kb %s none %s", got, yesNo(got.PasswordLogin), yesNo(got.KeyboardInteractive), yesNo(got.NoneAccepted))
			}
			if c.wantErr != "" {
				if !strings.Contains(got.Error, c.wantErr) || got.Methods != nil {
					t.Errorf("%+v", got)
				}
			} else if got.Error != "" || got.Methods == nil {
				t.Errorf("%+v", got)
			}
			for _, format := range []string{"text", "json"} {
				var out bytes.Buffer
				if err := writeAuthMethods(&out, authMethodsReport{Schema: 1, Hosts: []authMethodsHost{got}}, format); err != nil {
					t.Fatal(err)
				}
				assertNoEscapes(t, format, out.String())
				if strings.Contains(out.String(), "pwned") {
					t.Errorf("the host's words got through: %q", out.String())
				}
			}
		})
	}
}

// Unknown is null in the JSON, not false.
func TestAuthMethodsUnknownIsNull(t *testing.T) {
	var out bytes.Buffer
	if err := writeAuthMethods(&out, authMethodsReport{Schema: 1, Hosts: []authMethodsHost{{Host: "h", Error: "unknown"}}}, "json"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{`"password_login":null`, `"keyboard_interactive":null`, `"none_accepted":null`, `"methods":null`} {
		if !strings.Contains(out.String(), f) {
			t.Errorf("no %s in %s", f, out.String())
		}
	}
}

// All it can ever run is the constant, and that constant does nothing.
func TestAuthProbeRunsOnlyTrue(t *testing.T) {
	if authProbeCommand != "true" {
		t.Fatalf("the probe runs %q", authProbeCommand)
	}
	if runtime.GOOS == "windows" {
		t.Skip("refused on Windows")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >>\""+log+"\"; done\nexit 255\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r := config.Resolved{Address: "192.0.2.1"}
	if _, _, _, err := sshAuthProbe("", time.Second)(context.Background(), r); err == nil {
		t.Fatal("the fake ssh exits 255")
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if lines[len(lines)-1] != "true" || lines[len(lines)-2] != "192.0.2.1" {
		t.Errorf("args: %q", lines)
	}
}

// What ssh itself makes of it: no key or password offered, whatever extra_args say.
func TestAuthProbeArgsAsSSHReadsThem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("refused on Windows")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh")
	}
	sshConfig, remove, err := writeAgentSSHConfig()
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	for name, extra := range map[string][]string{
		"none":      nil,
		"theirs":    {"-o", "PreferredAuthentications=password", "-o", "LogLevel=QUIET", "-o", "BatchMode=no"},
		"verbosity": {"-q", "-vvv"},
	} {
		if err := checkAgentExtraArgs(extra); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		r := config.Resolved{Address: "192.0.2.1", ExtraArgs: extra}
		args := authProbeArgs(r, sshConfig, 8*time.Second)
		for _, a := range args {
			if a == "-q" || strings.HasPrefix(a, "-v") {
				t.Errorf("%s: %s kept", name, a)
			}
		}
		out, err := exec.Command("ssh", append([]string{"-G"}, sshrun.BuildArgs(config.Resolved{Address: "192.0.2.1", ExtraArgs: isolatedSSHArgs(t, args)})...)...).Output()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(line, " "); ok {
				got[k] = v
			}
		}
		for k, v := range map[string]string{
			"preferredauthentications": "none", "loglevel": "DEBUG", "batchmode": "yes",
			"stricthostkeychecking": "true", "controlmaster": "false", "forwardagent": "no", "requesttty": "false",
		} {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", name, k, got[k], v)
			}
		}
	}
}

func TestAskLogins(t *testing.T) {
	const line = "bcd  pts/0  1.2.3.4  Sat Oct 31 20:11:03 2026   still logged in\nwtmp begins Thu Oct  1 00:00:01 2026\n"
	job := agentJobFor("h")
	job.host = config.Host{Name: "h"}
	events := []activity.Event{{Device: "d", Seq: 9, Action: "connect", Host: "h", Target: "bcd@1.2.3.4", Time: time.Date(2026, 10, 31, 20, 11, 0, 0, time.UTC)}}
	got := askLogins(context.Background(), job, func(_ context.Context, _ config.Resolved) ([]byte, string, bool, error) {
		return []byte(line), "", false, nil
	}, events, time.Second)
	if got.Error != "" || !got.Reachable || len(got.Logins) != 1 || !got.Logins[0].Matched || got.Logins[0].Event.Seq != 9 || got.Since == nil {
		t.Fatalf("%+v", got)
	}

	exit1, exit127, exit255 := fakeSSHExit(t, 1), fakeSSHExit(t, logins.ExitNoLast), fakeSSHExit(t, 255)
	for _, c := range []struct {
		name      string
		stdout    string
		stderr    string
		overflow  bool
		err       error
		reachable bool
		want      string
	}{
		{"no last", "", "", false, exit127, true, "last not found"},
		{"last fails", "", hostileText, false, exit1, true, "last failed"},
		{"no connection", "", hostileText, false, exit255, false, "unreachable"},
		{"too much", "", "", true, nil, true, "more than meterm reads"},
		{"won't run", "", "", false, errors.New("no ssh"), false, "couldn't run ssh"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := askLogins(context.Background(), job, func(_ context.Context, _ config.Resolved) ([]byte, string, bool, error) {
				return []byte(c.stdout), c.stderr, c.overflow, c.err
			}, nil, time.Second)
			if !strings.Contains(got.Error, c.want) || got.Logins != nil || got.Reachable != c.reachable {
				t.Errorf("%+v", got)
			}
			if strings.Contains(got.Error, "pwned") {
				t.Errorf("the host's words got through: %q", got.Error)
			}
		})
	}

	// A host with nothing to say about logins isn't a host that can't say.
	empty := askLogins(context.Background(), job, func(_ context.Context, _ config.Resolved) ([]byte, string, bool, error) {
		return nil, "", false, nil
	}, nil, time.Second)
	if empty.Error != "" || empty.Logins == nil || len(empty.Logins) != 0 {
		t.Errorf("%+v", empty)
	}
	var js bytes.Buffer
	if err := writeLogins(&js, logins.Report{Schema: 1, Hosts: []logins.Host{empty}}, "json"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), `"logins":[]`) {
		t.Errorf("%s", js.String())
	}
}

func TestLoginsHostileOutputInBothFormats(t *testing.T) {
	job := agentJobFor("h\x1b[2J")
	got := askLogins(context.Background(), job, func(_ context.Context, _ config.Resolved) ([]byte, string, bool, error) {
		return []byte("a\x1b]0;pwned\x07  pts‮/0  1.2.3.4  Sat Oct 31 20:11:03 2026   still logged in\n" + hostileText + "\n"), "", false, nil
	}, nil, time.Second)
	if got.Unparsed != 1 || len(got.Logins) != 1 {
		t.Fatalf("%+v", got)
	}
	for _, format := range []string{"text", "json"} {
		var out bytes.Buffer
		if err := writeLogins(&out, logins.Report{Schema: 1, Hosts: []logins.Host{got}}, format); err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"\x1b", "\x07", "‮", `\u001b`, `\u0007`, `‮`} {
			if strings.Contains(out.String(), bad) {
				t.Errorf("%s has %q: %q", format, bad, out.String())
			}
		}
	}
}

// Every look goes through the same gate. Not one runs ssh for a host that fails it.
func TestLooksShareTheirRefusals(t *testing.T) {
	for _, c := range []struct {
		name string
		job  agentJob
	}{
		{"production by tag", agentJob{label: "h", refused: agentsRefusedProduction}},
		{"hostile extra_args", agentJob{label: "h", resolve: func(context.Context) config.Resolved {
			return config.Resolved{Address: "192.0.2.1", ExtraArgs: []string{"-o", "ProxyCommand=touch /tmp/x"}}
		}}},
		{"a bare word", agentJob{label: "h", resolve: func(context.Context) config.Resolved {
			return config.Resolved{Address: "192.0.2.1", ExtraArgs: []string{"elsewhere"}}
		}}},
		{"an unsafe host", agentJob{label: "h", resolve: func(context.Context) config.Resolved {
			return config.Resolved{Err: errors.New("-oProxyCommand isn't an address")}
		}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := askLogins(context.Background(), c.job, neverAsked(t), nil, time.Second)
			if l.Error == "" || l.Logins != nil || l.Reachable {
				t.Errorf("logins: %+v", l)
			}
			a := askAuthMethods(context.Background(), c.job, neverAsked(t), time.Second)
			if a.Error == "" || a.Methods != nil || a.Reachable {
				t.Errorf("auth-methods: %+v", a)
			}
		})
	}
}

func TestLooksRefuseWindowsAndNoSelection(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(cfgFile, []byte(agentsHosts), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METERM_CONFIG", cfgFile)
	// Windows is refused before hosts are looked at, so pin the OS: on a real
	// Windows runner the no-hosts check would otherwise never be reached.
	realGOOS := agentsGOOS
	t.Cleanup(func() { agentsGOOS = realGOOS })
	for name, mk := range map[string]func() *cobra.Command{
		"logins":       func() *cobra.Command { return newLoginsCmd() },
		"auth-methods": func() *cobra.Command { return newAuthMethodsCmd() },
	} {
		agentsGOOS = "linux"
		c := mk()
		c.SetArgs([]string{})
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "say which hosts") {
			t.Errorf("%s with no hosts: %v", name, err)
		}
		c = mk()
		c.SetArgs([]string{"dev1", "--format", "xml"})
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err == nil {
			t.Errorf("%s with a bad format ran", name)
		}
		agentsGOOS = "windows"
		c = mk()
		c.SetArgs([]string{"dev1"})
		c.SilenceUsage, c.SilenceErrors = true, true
		err := c.Execute()
		if err == nil || !strings.Contains(err.Error(), "Windows") || !strings.Contains(err.Error(), name) {
			t.Errorf("%s on Windows: %v", name, err)
		}
	}
}

func factsFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "hosts.yaml")
	t.Setenv("METERM_CONFIG", cfgFile)
	t.Setenv("METERM_HISTORY", filepath.Join(dir, "history.json"))
	t.Setenv("METERM_ACTIVITY_DIR", filepath.Join(dir, "activity"))
	t.Setenv("METERM_TEAM_DIR", filepath.Join(dir, "team"))
	return dir, cfgFile
}

func TestFacts(t *testing.T) {
	dir, cfgFile := factsFixture(t)
	old := cfgPath
	cfgPath = ""
	defer func() { cfgPath = old }()
	if err := os.WriteFile(cfgFile, []byte(`version: 1
hosts:
- name: web
  address: 10.0.0.1
  user: deploy
  env: production
  identity_file: /secret/path/key
  extra_args: [-o, "IdentityAgent=/secret/agent"]
  notes: secret note
  tags: [a]
  team: "git:https://user:tok3n@example.com/t"
  routes: [{name: vpn, address: 100.64.0.1, port: 2222}]
- name: "other"
  address: 10.0.0.2
  proxy_jump: web
`), 0o600); err != nil {
		t.Fatal(err)
	}
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("history.json", `{"web":{"last":"2026-10-31T01:02:03Z","count":4}}`)
	write("routes.json", `{"web":"vpn"}`)
	write("server-os.json", `{"web":{"target":"x","software":"OpenSSH_for_Windows_9 \u001b]0;pwned\u0007‮","checked":"2026-10-01T00:00:00Z"}}`)
	before := snapshot(t, dir)
	out := captureStdout(t, func() {
		c := newFactsCmd()
		c.SetArgs([]string{"--format", "json"})
		if err := c.Execute(); err != nil {
			t.Error(err)
		}
	})
	if after := snapshot(t, dir); after != before {
		t.Errorf("facts wrote something:\n%s\n---\n%s", before, after)
	}
	for _, leak := range []string{"secret", "tok3n", "IdentityAgent"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q in the export: %s", leak, out)
		}
	}
	var rep factsReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Schema != 1 || rep.Sources.History != "ok" || rep.Sources.Routes != "ok" || rep.Sources.ServerOS != "ok" || len(rep.Hosts) != 2 {
		t.Fatalf("%+v", rep)
	}
	web := rep.Hosts[0]
	if web.Name != "web" || web.Target != "deploy@10.0.0.1" || !web.Production || !web.ViaProd || !web.TeamShared || web.Connections != 4 ||
		web.LastConnected == nil || web.LastRoute != "vpn" || len(web.Routes) != 1 || web.Routes[0].Port != 2222 || web.Server == nil || !web.Server.Windows || !web.Server.Stale {
		t.Errorf("%+v", web)
	}
	assertNoEscapes(t, "software", web.Server.Software)
	if other := rep.Hosts[1]; other.Production || !other.ViaProd || other.LastConnected != nil || other.Server != nil || other.Tags == nil || other.Routes == nil {
		t.Errorf("%+v", other)
	}
	textOut := captureStdout(t, func() {
		c := newFactsCmd()
		c.SetArgs(nil)
		if err := c.Execute(); err != nil {
			t.Error(err)
		}
	})
	assertNoEscapes(t, "text", textOut)
}

func TestFactsSaysWhatItCouldntRead(t *testing.T) {
	dir, cfgFile := factsFixture(t)
	old := cfgPath
	cfgPath = ""
	defer func() { cfgPath = old }()
	if err := os.WriteFile(cfgFile, []byte("version: 1\nhosts:\n- {name: a, address: 1.2.3.4}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "routes.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	rep := buildFacts(cfg, cfgFile, time.Now())
	if rep.Sources.Routes != "unreadable" || rep.Sources.History != "missing" || rep.Sources.ServerOS != "missing" || len(rep.Hosts) != 1 {
		t.Errorf("%+v", rep)
	}
}

// snapshot is every file under dir, and what's in it.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		b.WriteString(p)
		if !fi.IsDir() {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			b.Write(data)
		}
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// isolatedSSHArgs points ssh -G at a config with the Include lines gone, or at none, so that
// these tests read what's generated here and never the real ~/.ssh/config.
func isolatedSSHArgs(t *testing.T, args []string) []string {
	t.Helper()
	out := append([]string{}, args...)
	for i, a := range out {
		if a == "-F" && i+1 < len(out) {
			b, err := os.ReadFile(out[i+1])
			if err != nil {
				t.Fatal(err)
			}
			var keep []string
			for _, line := range strings.Split(string(b), "\n") {
				if !strings.HasPrefix(line, "Include ") {
					keep = append(keep, line)
				}
			}
			p := filepath.Join(t.TempDir(), "ssh_config")
			if err := os.WriteFile(p, []byte(strings.Join(keep, "\n")), 0o600); err != nil {
				t.Fatal(err)
			}
			out[i+1] = p
			return out
		}
	}
	return append([]string{"-F", os.DevNull}, out...)
}
