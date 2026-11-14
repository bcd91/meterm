package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"meterm/internal/agents"
	"meterm/internal/config"
)

// A host the person reaches through a ProxyCommand isn't looked at with the watcher
// key: going straight to its HostName would be another route than theirs.
func TestWatcherLookRefusesProxyCommand(t *testing.T) {
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "watch")
	useSSHConfig(t, "Host 192.0.2.1\n  ProxyCommand nc -X 5 -x 192.0.2.80:1080 %h %p\n")
	_, _, remove, err := watcherLook(context.Background(), config.Resolved{Address: "192.0.2.1", AgentsIdentity: key}, 5*time.Second)
	if err == nil {
		remove()
	}
	if err == nil || !strings.Contains(err.Error(), "ProxyCommand") {
		t.Errorf("%v", err)
	}
	// Through a look: an error in meterm's words, and ssh doesn't run.
	argvFile := fakeSSHScript(t, `echo '[]'`)
	cannedSSHG(t, "user agent\nhostname 192.0.2.1\nproxycommand cloudflared access ssh --hostname %h\n")
	job := agentJob{label: "x", resolve: func(context.Context) config.Resolved {
		return config.Resolved{Address: "192.0.2.1", AgentsIdentity: key}
	}}
	got := askAgents(context.Background(), job, sshAgentQuery("", 5*time.Second), 5*time.Second)
	if !strings.Contains(got.Error, "ProxyCommand") || got.Agents != nil || lookUnreachable(got.Error) {
		t.Errorf("%+v", got)
	}
	if _, err := os.Stat(argvFile); !os.IsNotExist(err) {
		t.Error("ssh ran")
	}
	// "none" is no ProxyCommand.
	cannedSSHG(t, "user agent\nhostname 192.0.2.1\nproxycommand none\n")
	if got := askAgents(context.Background(), job, sshAgentQuery("", 5*time.Second), 5*time.Second); got.Error != "" {
		t.Errorf("none: %+v", got)
	}
}

// What ssh does on this machine with the key comes first: a key it would pass over
// (a passphrase, a mode others can read) is said to be this machine's problem, before
// the host is asked, and never makes the watch stop asking the host.
func TestWatcherKeyThisMachineCantUse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("agents refuses to run on Windows")
	}
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		key  string
		want string
	}{
		"passphrase": {writeKeyPair(t, dir, "locked", "secret", 0o600), "passphrase"},
		"mode 0644":  {writeKeyPair(t, dir, "open", "", 0o644), "0644"},
		"mode 0640":  {writeKeyPair(t, dir, "group", "", 0o640), "0640"},
		"not a key":  {writeNotAKey(t, dir), "isn't a private key"},
	} {
		t.Run(name, func(t *testing.T) {
			job := agentJob{label: "devbox", resolve: func(context.Context) config.Resolved {
				return config.Resolved{Name: "devbox", Address: "192.0.2.1", AgentsIdentity: tc.key}
			}}
			got := askAgents(context.Background(), job, func(context.Context, config.Resolved) ([]byte, string, bool, error) {
				t.Error("the host was asked")
				return nil, "", false, nil
			}, time.Second)
			if !strings.Contains(got.Error, "this machine can't use the watcher key") || !strings.Contains(got.Error, tc.want) || got.Error == agentsWatcherRefused {
				t.Errorf("%+v", got)
			}
			// agents key won't print a line for it, and --check doesn't log in.
			useSSHConfig(t, "")
			if _, _, err := watcherKeyLine(context.Background(), job.resolve(context.Background()), "", time.Second); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("agents key: %v", err)
			}
			if msg, code := checkWatcherKey(context.Background(), job, time.Second); code != keyCheckUnsure || !strings.Contains(msg, tc.want) {
				t.Errorf("--check: %d %s", code, msg)
			}
			// The watch keeps asking (it may be fixed here), and doesn't say it stopped.
			out, _ := runWatchTest(t, []agentJob{job}, func(context.Context, config.Resolved) ([]byte, string, bool, error) {
				return nil, "", false, nil
			}, 3, "json")
			looks := ofType(watchLines(t, out), "look")
			if len(looks) != 3 || looks[0]["stopped_asking"] != nil || looks[0]["next_look_at"] == nil || looks[0]["result"] != "error" {
				t.Errorf("watch: %v", looks)
			}
		})
	}
}

func writeNotAKey(t *testing.T, dir string) string {
	t.Helper()
	p := writeKeyPair(t, dir, "garbled", "", 0o600)
	if err := os.WriteFile(p, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nnope\n-----END OPENSSH PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A certificate beside the watcher key would be offered with it, and a host trusting
// its CA would let it in without the line's limits.
func TestWatcherKeyWithACertificate(t *testing.T) {
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "watch")
	if err := os.WriteFile(key+"-cert.pub", []byte("ssh-ed25519-cert-v01@openssh.com AAAA x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	useSSHConfig(t, "")
	r := config.Resolved{Name: "devbox", Address: "192.0.2.1", AgentsIdentity: key}
	if _, _, err := watcherKeyLine(context.Background(), r, "", time.Second); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("agents key: %v", err)
	}
	if msg := checkAgentsIdentity(r); !strings.Contains(msg, "certificate") {
		t.Errorf("look: %q", msg)
	}
}

// After the watcher key has got in once in this run, a refusal may pass (a host
// restarting, a file being edited): the host is asked again, no faster than every
// minute and doubling. Before it ever got in, the watch stops asking.
func TestWatchRefusedAfterItGotIn(t *testing.T) {
	n := 0
	wrapped := func(ctx context.Context, r config.Resolved) ([]byte, string, bool, error) {
		n++
		if n >= 2 && n <= 6 {
			return nil, "agent@192.0.2.1: Permission denied (publickey).", false, errWatcherKeyRefused
		}
		return []byte("[]"), "", false, nil
	}
	// A deadline as well as the clock: a watch that stopped asking would wait for ever.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fc := &fakeClock{t: time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC), max: 7, r: 0, cancel: cancel}
	var out bytes.Buffer
	if err := runAgentsWatch(ctx, &out, []agentJob{agentJobFor("devbox")}, wrapped,
		watchOptions{interval: 15 * time.Second, timeout: 8 * time.Second, parallel: 1, format: "json", clock: fc.clock()}); err != nil {
		t.Fatal(err)
	}
	looks := ofType(watchLines(t, out.String()), "look")
	if len(looks) != 7 {
		t.Fatalf("%d looks", len(looks))
	}
	for i, l := range looks[1:6] {
		if l["stopped_asking"] != nil || l["next_look_at"] == nil || l["result"] != "error" {
			t.Errorf("refusal %d: %v", i+1, l)
		}
	}
	if looks[6]["result"] != "ok" {
		t.Errorf("back: %v", looks[6])
	}
	// The waits: first spread, the interval after the good look, then 1, 2, 4, 8 and
	// 10 minutes however short --interval is, and the interval again once it's back.
	want := []time.Duration{0, 15 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 15 * time.Second}
	if len(fc.slept) < len(want) {
		t.Fatalf("slept %v", fc.slept)
	}
	for i, w := range want {
		if fc.slept[i] != w {
			t.Errorf("sleeps %v\nwant   %v", fc.slept[:len(want)], want)
			break
		}
	}
	// In any 10 minutes, at most 4 tries while refused: under fail2ban's default of
	// 5 failures in 10 minutes.
	var tries []time.Duration
	var at time.Duration
	for i, d := range fc.slept[:len(want)-1] {
		at += d
		if i >= 1 && i <= 5 { // looks 2 to 6, the refused ones
			tries = append(tries, at)
		}
	}
	for i := range tries {
		n := 0
		for _, u := range tries[i:] {
			if u-tries[i] < 10*time.Minute {
				n++
			}
		}
		if n > 4 {
			t.Errorf("%d tries within 10 minutes from %s: %v", n, tries[i], tries)
		}
	}
}

// Text form writes only when something changes, so a reader that went away is looked
// for rather than waited for. A failing write of the last line is the same stop.
func TestWatchNoticesTheReaderGoneWithoutWriting(t *testing.T) {
	var mu sync.Mutex
	gone := false
	asks := 0
	q := func(context.Context, config.Resolved) ([]byte, string, bool, error) {
		mu.Lock()
		asks++
		if asks == 3 {
			gone = true
		}
		mu.Unlock()
		return []byte("[]"), "", false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out syncBuffer
	start := time.Now()
	err := runAgentsWatch(ctx, &out, []agentJob{agentJobFor("devbox")}, q, watchOptions{interval: 20 * time.Millisecond,
		timeout: time.Second, parallel: 1, format: "text", clock: realWatchClock,
		outputGone: func() bool { mu.Lock(); defer mu.Unlock(); return gone }})
	if !errors.Is(err, errWatchOutput) {
		t.Errorf("err %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s", d)
	}
	mu.Lock()
	n := asks
	mu.Unlock()
	if n > 3+200 { // a look every 20ms for about the poll's second after it went
		t.Errorf("%d looks", n)
	}

	// Only the last line fails to write: still the reader gone, exit 3.
	w := &failAfter{n: 1}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := runAgentsWatch(ctx2, w, []agentJob{agentJobFor("devbox")}, q,
		watchOptions{interval: time.Minute, timeout: time.Second, parallel: 1, format: "text", clock: realWatchClock}); !errors.Is(err, errWatchOutput) {
		t.Errorf("last line: %v", err)
	}
}

type failAfter struct{ n int }

func (f *failAfter) Write(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, errors.New("broken pipe")
	}
	f.n--
	return len(p), nil
}

// The keys an ordinary login could use include IdentityFile paths written with ~,
// ${VAR} and % tokens, as ssh expands them, and the keys in an agent an IdentityAgent
// names. What can't be worked out is listed as unchecked.
func TestOtherLoginKeysExpandLikeSSH(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	old := sshHome
	sshHome = func() string { return home }
	t.Cleanup(func() { sshHome = old })
	t.Setenv("METERM_TEST_KEYDIR", dir)

	set := sshSettings{"hostname": {"198.51.100.5"}, "user": {"agent"}, "port": {"2222"}}
	for name, tc := range map[string]struct{ cfg, path string }{
		"tilde":  {"~/k1", filepath.Join(home, "k1")},
		"%d":     {"%d/k2", filepath.Join(home, "k2")},
		"${VAR}": {"${METERM_TEST_KEYDIR}/k3", filepath.Join(dir, "k3")},
		"%h %r":  {dir + "/k4-%r-%h-%p", filepath.Join(dir, "k4-agent-198.51.100.5-2222")},
		"%%":     {dir + "/k5-%%", filepath.Join(dir, "k5-%")},
	} {
		got, ok := expandIdentityPath(tc.cfg, set)
		if !ok || got != tc.path {
			t.Errorf("%s: %q %v, want %q", name, got, ok, tc.path)
		}
		key := newWatcherKey(t, filepath.Dir(tc.path), filepath.Base(tc.path))
		keys, unchecked := otherLoginKeys(context.Background(), sshSettings{"identityfile": {tc.cfg}, "hostname": set["hostname"], "user": set["user"], "port": set["port"]})
		pub, _ := publicKeyOf(key)
		if len(unchecked) != 0 || len(keys) != 1 || !sameKey(keys[0], pub) {
			t.Errorf("%s: keys %d unchecked %v", name, len(keys), unchecked)
		}
	}
	for _, bad := range []string{"%Z/x", "${METERM_TEST_UNSET_VAR}/x", "~someone/x", "x%"} {
		if _, ok := expandIdentityPath(bad, set); ok {
			t.Errorf("%q: expanded", bad)
		}
		if _, unchecked := otherLoginKeys(context.Background(), sshSettings{"identityfile": {bad}}); len(unchecked) != 1 {
			t.Errorf("%q: not listed as unchecked", bad)
		}
	}
}

// An agent named by IdentityAgent (1Password, Secretive, …) is asked too: a real
// ssh-add against an agent of the test's own.
func TestOtherLoginKeysAskTheConfigsAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket")
	}
	if realAgentKeys == nil {
		t.Skip("no agentKeys")
	}
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "watch")
	b, _ := os.ReadFile(key)
	raw, err := ssh.ParseRawPrivateKey(b)
	if err != nil {
		t.Fatal(err)
	}
	ring := agent.NewKeyring()
	if err := ring.Add(agent.AddedKey{PrivateKey: raw}); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "a.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _ = agent.ServeAgent(ring, c); c.Close() }()
		}
	}()
	old := agentKeys
	agentKeys = realAgentKeys
	t.Cleanup(func() { agentKeys = old })
	t.Setenv("SSH_AUTH_SOCK", "")

	keys, _ := otherLoginKeys(context.Background(), sshSettings{"identityagent": {sock}})
	pub, _ := publicKeyOf(key)
	if len(keys) != 1 || !sameKey(keys[0], pub) {
		t.Fatalf("%d keys from the config's agent", len(keys))
	}
	// And through agents key, with real ssh -G reading that IdentityAgent.
	useSSHConfig(t, "Host *\n  IdentityAgent "+sock+"\n")
	r := config.Resolved{Name: "devbox", Address: "192.0.2.1", AgentsIdentity: key}
	if _, _, err := watcherKeyLine(context.Background(), r, "", 5*time.Second); err == nil || !strings.Contains(err.Error(), "can log in to") {
		t.Errorf("agents key: %v", err)
	}
}

// --check is "limited" only when ssh logged in and ran to its end and the word didn't
// come back. Too much output, or ssh stopped from outside, can't tell.
func TestCheckWatcherKeyCantTell(t *testing.T) {
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "watch")
	job := agentJob{label: "devbox", resolve: func(context.Context) config.Resolved {
		return config.Resolved{Name: "devbox", Address: "192.0.2.1", AgentsIdentity: key}
	}}
	for name, script := range map[string]string{
		"too much output": `head -c 70000 /dev/zero | tr '\0' x; for a; do last=$a; done; eval "$last"`,
		"killed":          `kill -9 $$`,
	} {
		t.Run(name, func(t *testing.T) {
			fakeSSHScript(t, script)
			cannedSSHG(t, "user agent\n")
			if msg, code := checkWatcherKey(context.Background(), job, 5*time.Second); code != keyCheckUnsure {
				t.Errorf("%d %s", code, msg)
			}
		})
	}
}

var _ = agents.RemoteCommand
