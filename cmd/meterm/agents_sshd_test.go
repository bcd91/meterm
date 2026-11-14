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
	watch, main   string // private keys
}

func startLocalSSHD(t *testing.T) *localSSHD {
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
	s := &localSSHD{dir: dir, me: me.Username, authorized: filepath.Join(dir, "authorized_keys")}
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
AuthorizedKeysFile %s
PidFile %s
UsePAM no
StrictModes no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
AllowUsers %s
SetEnv HOME=%s PATH=/usr/bin:/bin
LogLevel VERBOSE
`, s.port, hostKey, s.authorized, filepath.Join(dir, "sshd.pid"), s.me, home)), 0o600); err != nil {
		t.Fatal(err)
	}
	s.setAuthorized(t)
	log := &syncBuffer{}
	s.log = log
	cmd := exec.Command(sshd, "-D", "-e", "-f", conf)
	cmd.Stderr = log
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
		case "main":
			b.Write(mustRead(t, s.main+".pub"))
		}
		b.WriteString("\n")
	}
	if err := os.WriteFile(s.authorized, []byte(b.String()), 0o600); err != nil {
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
	// After it, the limited line wins.
	s.setAuthorized(t, "watch-limited", "watch", "main")
	if msg, code := checkWatcherKey(ctx, s.job(), 10*time.Second); code != keyCheckLimited {
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

	// The watch: one try, then it stops asking.
	tries := 0
	q := sshAgentQuery("", 10*time.Second)
	counted := func(ctx context.Context, r config.Resolved) ([]byte, string, bool, error) {
		tries++
		return q(ctx, r)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var buf bytes.Buffer
	if err := runAgentsWatch(wctx, &buf, []agentJob{s.job()}, counted, watchOptions{interval: 50 * time.Millisecond,
		timeout: 5 * time.Second, parallel: 1, format: "json", clock: realWatchClock}); err != nil {
		t.Fatal(err)
	}
	looks := ofType(watchLines(t, buf.String()), "look")
	if tries != 1 || len(looks) != 1 || looks[0]["stopped_asking"] != true {
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
