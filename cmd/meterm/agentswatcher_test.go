package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"meterm/internal/sshrun"

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
	if !strings.Contains(got.Error, "ProxyCommand") || got.Agents != nil || lookUnreachable(got.Error) || strings.Contains(got.Error, "watcher look:") {
		t.Errorf("%+v", got)
	}
	// agents key gives no line for it either: no look could use it.
	withAgentKeys(t)
	if _, _, err := watcherKeyLine(context.Background(), job.resolve(context.Background()), "", 5*time.Second); err == nil || !strings.Contains(err.Error(), "ProxyCommand") || strings.Contains(err.Error(), "watcher look:") {
		t.Errorf("agents key: %v", err)
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
			if msg, code := checkWatcherKey(context.Background(), job, time.Second, nil); code != keyCheckUnsure || !strings.Contains(msg, tc.want) {
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
		"killed after much output": `head -c 70000 /dev/zero | tr '\0' x; kill -9 $$`,
		"killed":                   `kill -9 $$`,
	} {
		t.Run(name, func(t *testing.T) {
			fakeSSHScript(t, script)
			cannedSSHG(t, "user agent\n")
			if msg, code := checkWatcherKey(context.Background(), job, 5*time.Second, nil); code != keyCheckUnsure {
				t.Errorf("%d %s", code, msg)
			}
		})
	}
}

// An agent that's there but can't be asked isn't taken for one with no keys: it's named
// as unchecked. No agent at all, or one with no keys, is fine.
func TestOtherLoginKeysNameAnAgentThatCantBeAsked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket")
	}
	old := agentKeys
	agentKeys = realAgentKeys
	t.Cleanup(func() { agentKeys = old })
	dir := t.TempDir()
	dead := filepath.Join(dir, "dead.sock")
	if err := os.WriteFile(dead, nil, 0o600); err != nil { // there, and not a socket
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.sock")
	l, err := net.Listen("unix", empty)
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	ring := agent.NewKeyring()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _ = agent.ServeAgent(ring, c); c.Close() }()
		}
	}()
	for name, tc := range map[string]struct {
		env, identityAgent, want string
	}{
		"no agent":               {"", "", ""},
		"an empty agent":         {empty, "", ""},
		"a dead SSH_AUTH_SOCK":   {dead, "", "ssh-agent"},
		"a dead IdentityAgent":   {"", dead, "the agent at"},
		"an empty IdentityAgent": {"", empty, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SSH_AUTH_SOCK", tc.env)
			set := sshSettings{}
			if tc.identityAgent != "" {
				set["identityagent"] = []string{tc.identityAgent}
			}
			_, unchecked := otherLoginKeys(context.Background(), set)
			got := strings.Join(unchecked, "|")
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("unchecked %q, want %q", got, tc.want)
			}
		})
	}
}

// A single agents says what the watch says: a refused watcher key, or a host that
// wasn't asked, is an error; only ssh not getting in is unreachable.
func TestAgentsTextMatchesTheWatch(t *testing.T) {
	for msg, want := range map[string]string{
		agentsWatcherRefused:                   "error",
		"not asked: this machine can't use it": "error",
		"unreachable: connection refused":      "unreachable",
		"timed out after 8s":                   "unreachable",
	} {
		var b bytes.Buffer
		if err := writeAgents(&b, agents.Report{Schema: 1, Hosts: []agents.Host{{Host: "h", Error: msg}}}, "text"); err != nil {
			t.Fatal(err)
		}
		row := strings.Split(b.String(), "\n")[1]
		if f := strings.Fields(row); len(f) < 2 || f[1] != want {
			t.Errorf("%q: %q", msg, row)
		}
	}
}

// A reader that keeps the pipe open and never reads: writes block, and a stop still
// ends the watch within the flush grace, with errWatchBlocked.
func TestWatchStopsWithAReaderThatNeverReads(t *testing.T) {
	w := &blockingWriter{release: make(chan struct{})}
	t.Cleanup(func() { close(w.release) })
	var jobs []agentJob
	for i := 0; i < 5; i++ {
		jobs = append(jobs, agentJobFor(strings.Repeat("h", i+1)))
	}
	q := func(context.Context, config.Resolved) ([]byte, string, bool, error) {
		return []byte("[]"), "", false, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runAgentsWatch(ctx, w, jobs, q, watchOptions{interval: 5 * time.Millisecond, timeout: time.Second,
			parallel: 2, format: "json", clock: realWatchClock})
	}()
	for !w.blocked() {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // and the queue fills behind it
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, errWatchBlocked) {
			t.Errorf("err %v", err)
		}
		if d := time.Since(start); d > agentsWatchStall+2*time.Second {
			t.Errorf("took %s", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stop didn't end the watch")
	}
}

// blockingWriter takes nothing until release is closed.
type blockingWriter struct {
	mu      sync.Mutex
	waiting bool
	release chan struct{}
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.waiting = true
	b.mu.Unlock()
	<-b.release
	return 0, errors.New("released")
}

func (b *blockingWriter) blocked() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.waiting }

// -q in a host's extra_args would silence "Permission denied", and a refused key would
// read as a host that isn't there. A look leaves it out, and real ssh keeps ERROR.
func TestLooksLeaveOutDashQ(t *testing.T) {
	args := agentSSHArgs(config.Resolved{ExtraArgs: []string{"-q", "-p", "2222"}}, "", time.Second)
	for _, a := range args {
		if a == "-q" {
			t.Fatalf("-q kept: %q", args)
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	realSSHForG(t)
	r := config.Resolved{Address: "192.0.2.1", ExtraArgs: isolatedSSHArgs(t, agentSSHArgs(config.Resolved{ExtraArgs: []string{"-q"}}, "", time.Second))}
	out, err := exec.Command("ssh", append([]string{"-G"}, sshrun.BuildArgs(r)...)...).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "\nloglevel ERROR\n") {
		t.Errorf("loglevel:\n%s", out)
	}
}

// An ordinary look whose key the host refuses (no agents_identity) is an error, not
// unreachable, and is asked again no faster than once a minute, whatever --interval
// is; it never stops being asked. A jump host refusing is still unreachable.
func TestOrdinaryKeyRefused(t *testing.T) {
	fakeSSHScript(t, `echo 'bcd@192.0.2.1: Permission denied (publickey).' >&2; exit 255`)
	got := askAgents(context.Background(), agentJobFor("h"), sshAgentQuery("", 5*time.Second), 5*time.Second)
	if got.Error != agentsKeyRefused || lookUnreachable(got.Error) {
		t.Errorf("%+v", got)
	}
	fakeSSHScript(t, `echo 'bcd@192.0.2.50: Permission denied (publickey).' >&2; exit 255`)
	if got := askAgents(context.Background(), agentJobFor("h"), sshAgentQuery("", 5*time.Second), 5*time.Second); !lookUnreachable(got.Error) {
		t.Errorf("hop: %+v", got)
	}
	// In the watch.
	q := func(context.Context, config.Resolved) ([]byte, string, bool, error) {
		return nil, "bcd@192.0.2.1: Permission denied (publickey).", false, errKeyRefused
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fc := &fakeClock{t: time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC), max: 5, r: 0, cancel: cancel}
	var out bytes.Buffer
	if err := runAgentsWatch(ctx, &out, []agentJob{agentJobFor("devbox")}, q,
		watchOptions{interval: 15 * time.Second, timeout: 8 * time.Second, parallel: 1, format: "json", clock: fc.clock()}); err != nil {
		t.Fatal(err)
	}
	looks := ofType(watchLines(t, out.String()), "look")
	if len(looks) != 5 {
		t.Fatalf("%d looks", len(looks))
	}
	for _, l := range looks {
		if l["result"] != "error" || l["stopped_asking"] != nil || l["next_look_at"] == nil {
			t.Errorf("%v", l)
		}
	}
	want := []time.Duration{0, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
	for i, w := range want {
		if fc.slept[i] != w {
			t.Errorf("sleeps %v, want %v", fc.slept[:len(want)], want)
			break
		}
	}
}

// The same with the queue far from full: the writer is stuck on the first line, and
// the stop still doesn't wait for it past the grace.
func TestWatchStopsWithAStuckWriteAndRoomInTheQueue(t *testing.T) {
	w := &blockingWriter{release: make(chan struct{})}
	t.Cleanup(func() { close(w.release) })
	q := func(context.Context, config.Resolved) ([]byte, string, bool, error) {
		return []byte("[]"), "", false, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runAgentsWatch(ctx, w, []agentJob{agentJobFor("a")}, q, watchOptions{interval: time.Hour, timeout: time.Second,
			parallel: 1, format: "json", clock: realWatchClock})
	}()
	for !w.blocked() {
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, errWatchBlocked) {
			t.Errorf("err %v", err)
		}
		if d := time.Since(start); d > agentsWatchStall+2*time.Second {
			t.Errorf("took %s", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stop didn't end the watch")
	}
}

// A watcher key that never got in is given up on only after 3 refusals in a row
// spanning 5 minutes: one refusal can be a passing thing. Refused at 0, 1, 3 and 7
// minutes, it stops at the fourth. Broken by anything else, the count starts again.
func TestWatchGivesUpOnlyAfterRepeatedRefusals(t *testing.T) {
	run := func(answers []error) []map[string]any {
		t.Helper()
		n := 0
		q := func(context.Context, config.Resolved) ([]byte, string, bool, error) {
			err := answers[min(n, len(answers)-1)]
			n++
			if err == nil {
				return []byte("[]"), "", false, nil
			}
			return nil, "agent@192.0.2.1: Permission denied (publickey).", false, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		fc := &fakeClock{t: time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC), max: 12, r: 0, cancel: cancel}
		var out bytes.Buffer
		if err := runAgentsWatch(ctx, &out, []agentJob{agentJobFor("devbox")}, q,
			watchOptions{interval: 15 * time.Second, timeout: time.Second, parallel: 1, format: "json", clock: fc.clock()}); err != nil {
			t.Fatal(err)
		}
		return ofType(watchLines(t, out.String()), "look")
	}
	r := errWatcherKeyRefused
	looks := run([]error{r})
	if len(looks) != 4 || looks[3]["stopped_asking"] != true || looks[2]["stopped_asking"] != nil {
		t.Errorf("steady refusal: %d looks, last %v", len(looks), looks[len(looks)-1])
	}
	// A look that doesn't refuse (here, a host that isn't there) starts the count again.
	looks = run([]error{r, r, errors.New("not ssh"), r, r, r, r})
	stopped := -1
	for i, l := range looks {
		if l["stopped_asking"] == true {
			stopped = i
		}
	}
	// Refusals 4, 5 and 6 are the three in a row (the backoff from earlier failures
	// already spaces them past 5 minutes), so it stops at the sixth, not the third.
	if stopped != 5 {
		t.Errorf("stopped at look %d of %d", stopped+1, len(looks))
	}
	// A login that worked but found no claude got in too: never given up on.
	noClaude := exitErr(t, agents.ExitNoClaude)
	looks = run([]error{noClaude, r})
	for _, l := range looks {
		if l["stopped_asking"] != nil {
			t.Errorf("gave up on a key that got in to no claude: %v", l)
		}
	}
	// Once it got in, it's never given up on.
	looks = run([]error{nil, r})
	for _, l := range looks {
		if l["stopped_asking"] != nil {
			t.Errorf("gave up on a key that got in: %v", l)
		}
	}
	if len(looks) < 10 {
		t.Errorf("%d looks", len(looks))
	}
}

// slowWriter takes each line after a pause.
type slowWriter struct {
	mu    sync.Mutex
	pause time.Duration
	b     bytes.Buffer
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.pause)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *slowWriter) String() string { w.mu.Lock(); defer w.mu.Unlock(); return w.b.String() }

// A reader that's slow but keeps reading gets every queued line and the last one: a
// stop gives up only when nothing has been taken for a while, not after a fixed time.
// One that keeps taking lines, slowly enough, is cut off at the overall cap.
func TestWatchStopWaitsForASlowReader(t *testing.T) {
	var jobs []agentJob
	for i := 0; i < 80; i++ {
		jobs = append(jobs, agentJobFor("h"+strings.Repeat("x", i)))
	}
	q := func(context.Context, config.Resolved) ([]byte, string, bool, error) {
		return []byte("[]"), "", false, nil
	}
	w := &slowWriter{pause: 40 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	err := runAgentsWatch(ctx, w, jobs, q, watchOptions{interval: 10 * time.Millisecond, timeout: time.Second,
		parallel: 80, format: "json", clock: realWatchClock})
	if err != nil {
		t.Fatalf("a slow reader lost the end: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(w.String()), "\n")
	if !strings.Contains(lines[len(lines)-1], `"type":"stopped"`) || len(lines) < 64 {
		t.Errorf("%d lines, last %q", len(lines), lines[len(lines)-1])
	}
	// The looks that finished as it stopped, with the queue full, are counted.
	var stopped map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &stopped); err != nil {
		t.Fatal(err)
	}
	if d, ok := stopped["dropped"].(float64); !ok || d < 1 {
		t.Errorf("dropped: %v", stopped)
	}

	old := agentsWatchFlushCap
	agentsWatchFlushCap = 2 * time.Second
	t.Cleanup(func() { agentsWatchFlushCap = old })
	w = &slowWriter{pause: time.Second} // a line a second: never 2s without one
	ctx2, cancel2 := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel2()
	start := time.Now()
	err = runAgentsWatch(ctx2, w, jobs, q, watchOptions{interval: 10 * time.Millisecond, timeout: time.Second,
		parallel: 80, format: "json", clock: realWatchClock})
	if !errors.Is(err, errWatchBlocked) {
		t.Errorf("err %v", err)
	}
	if d := time.Since(start); d > 1500*time.Millisecond+agentsWatchFlushCap+2*time.Second {
		t.Errorf("took %s", d)
	}
}

// The host a refusal names is matched as ssh names it: in any case, and by the name the
// person's ssh config gives it (ssh -G), for ordinary looks as for watcher looks.
func TestOrdinaryRefusalNamesTheHostAsSSHDoes(t *testing.T) {
	for name, tc := range map[string]struct{ address, g, stderr string }{
		"other case": {"LocalHost", "hostname LocalHost\n", "bcd@localhost: Permission denied (publickey)."},
		"HostName":   {"192.0.2.1", "hostname box.example.com\n", "bcd@box.example.com: Permission denied (publickey)."},
	} {
		t.Run(name, func(t *testing.T) {
			fakeSSHScript(t, `echo '`+tc.stderr+`' >&2; exit 255`)
			cannedSSHG(t, tc.g)
			job := agentJob{label: "h", resolve: func(context.Context) config.Resolved { return config.Resolved{Address: tc.address} }}
			if got := askAgents(context.Background(), job, sshAgentQuery("", 5*time.Second), 5*time.Second); got.Error != agentsKeyRefused {
				t.Errorf("%+v", got)
			}
		})
	}
	// A watcher look too, in another case.
	if !(watcherResolved{Resolved: config.Resolved{Address: "LOCALHOST"}}).refused("a@localhost: Permission denied (publickey).") {
		t.Error("watcher look, other case")
	}
}
