//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"meterm/internal/agents"
	"meterm/internal/config"
)

// These run a real sshd, on 127.0.0.1 as this user, with a throwaway host key and an
// authorized_keys of its own, so the watcher key is tried against what sshd really
// does. Opt in with METERM_TEST_SSHD=1: it needs sshd and ssh-keygen, and a port.
// Nothing here reads the person's ssh config, keys or agent: ssh gets -F with only
// this test's config, and the session's HOME is the test's.

type localSSHD struct {
	log           *syncBuffer
	userConfig    string
	dir, port, me string
	authorized    string
	elsewhere     string
	watch, main   string // private keys
}

func startLocalSSHD(t *testing.T, extraConfig ...string) *localSSHD {
	t.Helper()
	if os.Getenv("METERM_TEST_SSHD") != "1" {
		t.Skip("set METERM_TEST_SSHD=1 to run against a local sshd")
	}
	sshd := "/usr/sbin/sshd"
	if _, err := os.Stat(sshd); err != nil {
		t.Skip("no sshd")
	}
	// The real ssh, first on PATH, for this test only: it connects to nothing but
	// this sshd, and reads only the -F config a look gives it.
	realSSH, lookErr := exec.LookPath("/usr/bin/ssh")
	if lookErr != nil {
		t.Skip("no /usr/bin/ssh")
	}
	sshDir := t.TempDir()
	if err := os.Symlink(realSSH, filepath.Join(sshDir, "ssh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SSH_AUTH_SOCK", "")
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// PerSourcePenalties off: the test's own failed logins on purpose would otherwise
	// have sshd turn 127.0.0.1 away for a while (OpenSSH 9.8 and later).
	// authorized_keys where an ordinary login finds it, as --check reads it: under the
	// session's HOME, the test's. elsewhere is another AuthorizedKeysFile, which it can't.
	s := &localSSHD{dir: dir, me: me.Username, authorized: filepath.Join(dir, "home", ".ssh", "authorized_keys"),
		elsewhere: filepath.Join(dir, "elsewhere_keys")}
	if err := os.MkdirAll(filepath.Dir(s.authorized), 0o700); err != nil {
		t.Fatal(err)
	}
	keygen := func(name string) string {
		p := filepath.Join(dir, name)
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", p).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v %s", err, out)
		}
		return p
	}
	hostKey := keygen("hostkey")
	s.watch, s.main = keygen("watch"), keygen("main")

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.port = strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()

	home := filepath.Join(dir, "home")
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	claude := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>'" + filepath.Join(dir, "claude-ran") + "'\necho '[]'\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(claude), 0o755); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "sshd_config")
	if err := os.WriteFile(conf, []byte(fmt.Sprintf(`Port %s
ListenAddress 127.0.0.1
HostKey %s
AuthorizedKeysFile %s %s
PidFile %s
UsePAM no
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
AllowUsers %s
SetEnv HOME=%s PATH=/usr/bin:/bin
LogLevel VERBOSE
PerSourcePenalties no
%s`, s.port, hostKey, s.authorized, s.elsewhere, filepath.Join(dir, "sshd.pid"), s.me, home, strings.Join(append(extraConfig, ""), "\n"))), 0o600); err != nil {
		t.Fatal(err)
	}
	s.setAuthorized(t)
	log := &syncBuffer{}
	s.log = log
	cmd := exec.Command(sshd, "-D", "-e", "-f", conf)
	cmd.Stderr = log
	dieWithTest(cmd) // a test killed by its timeout mustn't leave sshd running
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("sshd:\n%s", log.String())
		}
	})
	for i := 0; ; i++ {
		c, err := net.Dial("tcp", "127.0.0.1:"+s.port)
		if err == nil {
			c.Close()
			break
		}
		if i > 100 {
			t.Fatalf("sshd didn't listen: %s", log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	hostPub, _ := os.ReadFile(hostKey + ".pub")
	kh := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(kh, []byte("[127.0.0.1]:"+s.port+" "+string(hostPub)), 0o600); err != nil {
		t.Fatal(err)
	}
	// The person's ssh config: where the host is and which host keys count, and their
	// own key for every host, which a watcher look mustn't use.
	s.userConfig = "Host 127.0.0.1\n  Port " + s.port + "\n  User " + s.me + "\n  UserKnownHostsFile " + kh + "\n" +
		"  IdentityAgent none\nHost *\n  IdentityFile " + s.main + "\n  IdentityAgent none\n"
	writeUserSSHConfig(t, s.userConfig)
	return s
}

// setAuthorized writes authorized_keys: each entry is "watch", "watch-limited" or
// "main", in that order.
func (s *localSSHD) setAuthorized(t *testing.T, entries ...string) {
	t.Helper()
	opts, err := agents.KeyOptions()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		switch e {
		case "watch":
			b.Write(mustRead(t, s.watch+".pub"))
		case "watch-limited":
			b.WriteString(opts + " ")
			b.Write(mustRead(t, s.watch+".pub"))
		case "watch-cmdonly": // a forced command and nothing else
			b.WriteString(`command="` + strings.ReplaceAll(agents.RemoteCommand, `"`, `\"`) + `" `)
			b.Write(mustRead(t, s.watch+".pub"))
		case "watch-permitopen": // a forced command, and forwarding only to one place
			b.WriteString(`command="` + strings.ReplaceAll(agents.RemoteCommand, `"`, `\"`) + `",permitopen="127.0.0.1:1" `)
			b.Write(mustRead(t, s.watch+".pub"))
		case "watch-shell": // restrict, with a shell as its command
			b.WriteString(`restrict,command="/bin/sh" `)
			b.Write(mustRead(t, s.watch+".pub"))
		case "watch-pf": // restrict, then forwarding let back in
			b.WriteString(`restrict,port-forwarding,command="` + strings.ReplaceAll(agents.RemoteCommand, `"`, `\"`) + `" `)
			b.Write(mustRead(t, s.watch+".pub"))
		case "main":
			b.Write(mustRead(t, s.main+".pub"))
		default:
			if o, ok := strings.CutPrefix(e, "opts:"); ok { // these options, the watcher key
				b.WriteString(o + " ")
				b.Write(mustRead(t, s.watch+".pub"))
			}
		}
		b.WriteString("\n")
	}
	if err := os.WriteFile(s.authorized, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.elsewhere, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (s *localSSHD) job() agentJob {
	return agentJob{label: "local", resolve: func(context.Context) config.Resolved {
		return config.Resolved{Name: "local", Address: "127.0.0.1", AgentsIdentity: s.watch}
	}}
}

func (s *localSSHD) claudeRuns() int {
	b, _ := os.ReadFile(filepath.Join(s.dir, "claude-ran"))
	return strings.Count(string(b), "\n")
}

func TestLocalSSHDWatcherKey(t *testing.T) {
	s := startLocalSSHD(t)
	ctx := context.Background()

	// The line in place, the person's own key beside it: a look gets in with the
	// watcher key, and the forced command runs claude.
	s.setAuthorized(t, "watch-limited", "main")
	got := askAgents(ctx, s.job(), sshAgentQuery("", 10*time.Second), 10*time.Second)
	if got.Error != "" || got.Agents == nil || s.claudeRuns() != 1 {
		t.Fatalf("look: %+v, claude ran %d", got, s.claudeRuns())
	}
	if msg, code := checkWatcherKey(ctx, s.job(), 10*time.Second); code != keyCheckLimited {
		t.Errorf("limited line: %d %s", code, msg)
	}

	// The same key also on a plain line before it: sshd takes that one, and --check
	// says so, loudly.
	s.setAuthorized(t, "watch", "watch-limited", "main")
	if msg, code := checkWatcherKey(ctx, s.job(), 10*time.Second); code != keyCheckUnlimited || !strings.Contains(msg, "NOT LIMITED") {
		t.Errorf("plain line first: %d %s", code, msg)
	}
	// A forced command alone, or restrict with port-forwarding let back in: the key
	// runs nothing else, but it forwards, so it isn't limited.
	for _, entry := range []string{"watch-cmdonly", "watch-pf"} {
		s.setAuthorized(t, entry, "main")
		if msg, code := checkWatcherKey(ctx, s.job(), 10*time.Second); code != keyCheckUnlimited || !strings.Contains(msg, "port forwarding is allowed") {
			t.Errorf("%s: %d %s", entry, code, msg)
		}
	}
	// Forwarding only to one place still lets the key listen on the host (-R); a shell
	// as the forced command runs what comes on stdin.
	for entry, want := range map[string]string{"watch-permitopen": "remote (-R)", "watch-shell": "NOT LIMITED"} {
		s.setAuthorized(t, entry, "main")
		if msg, code := checkWatcherKey(ctx, s.job(), 10*time.Second); code != keyCheckUnlimited || !strings.Contains(msg, want) {
			t.Errorf("%s: %d %s", entry, code, msg)
		}
	}
	// The line as printed: limited, forwarding refused.
	s.setAuthorized(t, "watch-limited", "main")
	if msg, code := checkWatcherKey(ctx, s.job(), 10*time.Second); code != keyCheckLimited || !strings.Contains(msg, "exactly meterm's line") {
		t.Errorf("printed line: %d %s", code, msg)
	}

	// What no probe can see: forced commands that take stdin and say nothing, wrappers
	// that run what's sent and then answer as meterm's would, and forwarding let back
	// in only to other ports. The line on the host says what they are.
	esc := strings.ReplaceAll(agents.RemoteCommand, `"`, `\"`)
	for name, o := range map[string]string{
		"python":          `restrict,command="python3"`,
		"cat to a file":   `restrict,command="sh -c 'cat > ` + filepath.Join(s.dir, "taken") + `; exit 1'"`,
		"a shell, quiet":  `restrict,command="sh -c 'sh >/dev/null 2>&1; echo []'"`,
		"eval, then ours": `restrict,command="eval \"$SSH_ORIGINAL_COMMAND\" >/dev/null; ` + esc + `"`,
		"other ports":     `restrict,port-forwarding,permitlisten="127.0.0.1:18080",permitopen="127.0.0.1:5432",command="` + esc + `"`,
	} {
		s.setAuthorized(t, "opts:"+o, "main")
		if msg, code := checkWatcherKey(ctx, s.job(), 10*time.Second); code != keyCheckUnlimited || !strings.Contains(msg, "options differ") {
			t.Errorf("%s: %d %s", name, code, msg)
		}
	}
	// meterm's line, but in a file sshd reads that an ordinary login doesn't look in:
	// can't tell.
	s.setAuthorized(t, "main")
	opts, _ := agents.KeyOptions()
	if err := os.WriteFile(s.elsewhere, []byte(opts+" "+string(mustRead(t, s.watch+".pub"))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg, code := checkWatcherKey(ctx, s.job(), 10*time.Second); code != keyCheckUnsure || !strings.Contains(msg, "AuthorizedKeysFile") {
		t.Errorf("elsewhere: %d %s", code, msg)
	}
	// After it, sshd uses the limited line; but another line for the key is there, and
	// any change to the file can bring it first: not limited.
	s.setAuthorized(t, "watch-limited", "watch", "main")
	if msg, code := checkWatcherKey(ctx, s.job(), 10*time.Second); code != keyCheckUnlimited || !strings.Contains(msg, "options differ") {
		t.Errorf("plain line second: %d %s", code, msg)
	}

	// No line for the watcher key, though the person's own key would get in: the look
	// is refused and says so, with no second try with that key, and no user given by
	// meterm (ssh names the one from the config).
	s.setAuthorized(t, "main")
	before := s.claudeRuns()
	got = askAgents(ctx, s.job(), sshAgentQuery("", 10*time.Second), 10*time.Second)
	if got.Error != agentsWatcherRefused || s.claudeRuns() != before {
		t.Errorf("refused: %+v", got)
	}
	if msg, code := checkWatcherKey(ctx, s.job(), 10*time.Second); code != keyCheckUnsure || !strings.Contains(msg, "refused the watcher key") {
		t.Errorf("check with no line: %d %s", code, msg)
	}
	// With -q in the host's extra_args too: still read as a refused watcher key.
	quiet := agentJob{label: "local", resolve: func(context.Context) config.Resolved {
		return config.Resolved{Name: "local", Address: "127.0.0.1", AgentsIdentity: s.watch, ExtraArgs: []string{"-q"}}
	}}
	if got := askAgents(ctx, quiet, sshAgentQuery("", 10*time.Second), 10*time.Second); got.Error != agentsWatcherRefused {
		t.Errorf("-q: %+v", got)
	}
	// And an ordinary look that no key gets into: refused, not unreachable.
	s.setAuthorized(t)
	ordinary := agentJob{label: "local", resolve: func(context.Context) config.Resolved {
		return config.Resolved{Name: "local", Address: "127.0.0.1", ExtraArgs: []string{"-q"}}
	}}
	// An ordinary look gets its -F from the run, as `meterm agents` gives it: this
	// test's config only.
	cfgFile, removeCfg, err := writeAgentSSHConfig()
	if err != nil {
		t.Fatal(err)
	}
	defer removeCfg()
	if got := askAgents(ctx, ordinary, sshAgentQuery(cfgFile, 10*time.Second), 10*time.Second); got.Error != agentsKeyRefused {
		t.Errorf("ordinary: %+v", got)
	}

	// The watch, on a clock that moves only when it sleeps: real refused logins at 0, 1,
	// 3 and 7 minutes of that clock, then it stops asking.
	tries := 0
	q := sshAgentQuery("", 10*time.Second)
	counted := func(ctx context.Context, r config.Resolved) ([]byte, string, bool, error) {
		tries++
		return q(ctx, r)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	fc := &fakeClock{t: time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC), max: 1 << 30, cancel: func() {}}
	var buf bytes.Buffer
	if err := runAgentsWatch(wctx, &buf, []agentJob{s.job()}, counted, watchOptions{interval: 15 * time.Second,
		timeout: 5 * time.Second, parallel: 1, format: "json", clock: fc.clock()}); err != nil {
		t.Fatal(err)
	}
	looks := ofType(watchLines(t, buf.String()), "look")
	if tries != 4 || len(looks) != 4 || looks[3]["stopped_asking"] != true || looks[2]["stopped_asking"] != nil {
		t.Errorf("tries %d, looks %v", tries, looks)
	}

	// A ProxyCommand in the person's config for the host: the look fails here, and
	// doesn't go straight to the host instead. sshd sees no connection at all.
	s.setAuthorized(t, "watch-limited")
	writeUserSSHConfig(t, strings.Replace(s.userConfig, "  IdentityAgent none\nHost *", "  ProxyCommand nc %h %p\n  IdentityAgent none\nHost *", 1))
	conns := strings.Count(s.log.String(), "Connection from")
	got = askAgents(ctx, s.job(), sshAgentQuery("", 10*time.Second), 10*time.Second)
	time.Sleep(200 * time.Millisecond)
	if !strings.Contains(got.Error, "ProxyCommand") || strings.Count(s.log.String(), "Connection from") != conns {
		t.Errorf("ProxyCommand: %+v; sshd saw %d new connections", got, strings.Count(s.log.String(), "Connection from")-conns)
	}
	writeUserSSHConfig(t, s.userConfig)

	// And `agents key` won't print a line for the person's own key.
	r := config.Resolved{Name: "local", Address: "127.0.0.1", AgentsIdentity: s.main}
	if _, _, err := watcherKeyLine(ctx, r, "", 10*time.Second); err == nil || !strings.Contains(err.Error(), "can log in to") {
		t.Errorf("own key: %v", err)
	}
	if _, account, err := watcherKeyLine(ctx, config.Resolved{Name: "local", Address: "127.0.0.1", AgentsIdentity: s.watch}, "", 10*time.Second); err != nil || account != s.me {
		t.Errorf("watcher key: %q %v", account, err)
	}
}

// writeUserSSHConfig is useSSHConfig without changing which ssh runs.
func writeUserSSHConfig(t *testing.T, content string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "user_ssh_config")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	old := agentSSHIncludes
	agentSSHIncludes = []string{p}
	t.Cleanup(func() { agentSSHIncludes = old })
}

// An sshd that allows only remote forwarding: a forced command alone, refused -W, still
// listens for -R. --check says it isn't limited.
func TestLocalSSHDRemoteForwardingOnly(t *testing.T) {
	s := startLocalSSHD(t, "AllowTcpForwarding remote")
	s.setAuthorized(t, "watch-cmdonly", "main")
	if msg, code := checkWatcherKey(context.Background(), s.job(), 10*time.Second); code != keyCheckUnlimited || !strings.Contains(msg, "remote (-R)") {
		t.Errorf("%d %s", code, msg)
	}
	s.setAuthorized(t, "watch-limited", "main")
	if msg, code := checkWatcherKey(context.Background(), s.job(), 10*time.Second); code != keyCheckLimited {
		t.Errorf("printed line: %d %s", code, msg)
	}
}
