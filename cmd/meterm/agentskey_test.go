package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"meterm/internal/agents"
	"meterm/internal/config"
	"meterm/internal/sshrun"
)

// newWatcherKey writes a made-up ed25519 key pair, the private half unencrypted with
// mode 0600 as a watcher key has to be, and returns the private path.
func newWatcherKey(t *testing.T, dir, name string) string {
	t.Helper()
	return writeKeyPair(t, dir, name, "", 0o600)
}

// writeKeyPair is newWatcherKey with a passphrase and a mode of its own.
func writeKeyPair(t *testing.T, dir, name, passphrase string, mode os.FileMode) string {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, name)
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, name, []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(block), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+".pub", bytes.TrimSpace(ssh.MarshalAuthorizedKey(pk)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// cannedSSHG makes ssh -G answer this, for a test whose ssh is a fake script.
func cannedSSHG(t *testing.T, out string) {
	t.Helper()
	old := sshG
	sshG = func(context.Context, []string) ([]byte, error) { return []byte(out), nil }
	t.Cleanup(func() { sshG = old })
}

// useSSHConfig makes a look's config include this as the person's ssh config, and
// puts the real ssh first for -G. It returns the file.
func useSSHConfig(t *testing.T, content string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("agents refuses to run on Windows")
	}
	realSSHForG(t)
	p := filepath.Join(t.TempDir(), "user_ssh_config")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	old := agentSSHIncludes
	agentSSHIncludes = []string{p}
	t.Cleanup(func() { agentSSHIncludes = old })
	return p
}

// withAgentKeys makes ssh-agent hold these keys, as far as meterm asks.
func withAgentKeys(t *testing.T, keys ...string) {
	t.Helper()
	old := agentKeys
	var pks []ssh.PublicKey
	for _, k := range keys {
		pk, _, _, _, err := ssh.ParseAuthorizedKey(mustRead(t, k+".pub"))
		if err != nil {
			t.Fatal(err)
		}
		pks = append(pks, pk)
	}
	agentKeys = func(context.Context, string) ([]ssh.PublicKey, string) { return pks, "" }
	t.Cleanup(func() { agentKeys = old })
}

func TestAgentsIdentityInherits(t *testing.T) {
	cfg := hostsForAgents(t, `version: 1
defaults: {agents_identity: ~/.ssh/def}
groups:
  g: {agents_identity: ~/.ssh/grp}
hosts:
- {name: a, address: 192.0.2.1}
- {name: b, address: 192.0.2.2, group: g}
- {name: c, address: 192.0.2.3, group: g, agents_identity: ~/.ssh/own, identity_file: ~/.ssh/main}
`)
	for name, want := range map[string]string{"a": "~/.ssh/def", "b": "~/.ssh/grp", "c": "~/.ssh/own"} {
		h, _ := cfg.Find(name)
		r := cfg.Resolve(*h)
		if r.AgentsIdentity != want {
			t.Errorf("%s: %q", name, r.AgentsIdentity)
		}
		if name == "c" && r.IdentityFile != "~/.ssh/main" {
			t.Errorf("identity_file changed: %q", r.IdentityFile)
		}
	}
}

// With a watcher key, a look offers it with IdentitiesOnly, and the host's own say on
// keys in extra_args is dropped.
func TestAgentsLookUsesTheWatcherKey(t *testing.T) {
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "watch")
	argvFile := fakeSSHScript(t, `echo '[]'`)
	cannedSSHG(t, "user agent\nhostname 192.0.2.1\nport 22\n")
	job := agentJob{label: "x", resolve: func(context.Context) config.Resolved {
		return config.Resolved{Address: "192.0.2.1", IdentityFile: "/nowhere/main", AgentsIdentity: key, ExtraArgs: []string{"-o", "IdentitiesOnly=no"}}
	}}
	got := askAgents(context.Background(), job, sshAgentQuery("", 5*time.Second), 5*time.Second)
	if got.Error != "" {
		t.Fatalf("%+v", got)
	}
	b, _ := os.ReadFile(argvFile)
	argv := strings.Split(strings.TrimSpace(string(b)), "\n")
	i := slices.Index(argv, "-i")
	if i < 0 || argv[i+1] != key || slices.Contains(argv, "/nowhere/main") {
		t.Errorf("argv: %q", argv)
	}
	if !slices.Contains(argv, "IdentitiesOnly=yes") || slices.Contains(argv, "IdentitiesOnly=no") {
		t.Errorf("IdentitiesOnly: %q", argv)
	}
}

// A watcher key that isn't there stops the look before ssh: ssh would go on with
// another key, and the look wouldn't be the limited one.
func TestAgentsMissingWatcherKeyIsNotAsked(t *testing.T) {
	job := agentJob{label: "x", resolve: func(context.Context) config.Resolved {
		return config.Resolved{Address: "192.0.2.1", AgentsIdentity: filepath.Join(t.TempDir(), "gone\x1b[2J")}
	}}
	got := askAgents(context.Background(), job, func(context.Context, config.Resolved) ([]byte, string, bool, error) {
		t.Error("ssh ran")
		return nil, "", false, nil
	}, time.Second)
	if got.Reachable || got.Agents != nil || !strings.Contains(got.Error, "agents_identity") || strings.Contains(got.Error, "\x1b") {
		t.Errorf("%+v", got)
	}
	if lookUnreachable(got.Error) {
		t.Errorf("reads as unreachable: %q", got.Error)
	}
}

// openSSHCommand takes command="…" out of an authorized_keys option the way sshd
// does: \" is a quote, and that's the only escape.
func openSSHCommand(t *testing.T, opts []string) string {
	t.Helper()
	for _, o := range opts {
		if v, ok := strings.CutPrefix(o, `command="`); ok {
			if !strings.HasSuffix(v, `"`) {
				t.Fatalf("unterminated: %q", o)
			}
			return strings.ReplaceAll(strings.TrimSuffix(v, `"`), `\"`, `"`)
		}
	}
	t.Fatalf("no command in %q", opts)
	return ""
}

func TestWatcherKeyLine(t *testing.T) {
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "watch")
	other := newWatcherKey(t, dir, "other")
	useSSHConfig(t, "Host 192.0.2.1\n  User cfguser\nHost *\n  IdentityFile "+other+"\n")
	withAgentKeys(t, other)
	line, account, err := watcherKeyLine(context.Background(), config.Resolved{Name: "devbox", Address: "192.0.2.1", AgentsIdentity: key}, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// The account named is the one the look logs in to, as ssh works it out.
	if account != "cfguser" {
		t.Errorf("account %q", account)
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("more than a line: %q", line)
	}
	pk, comment, opts, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(rest) != 0 {
		t.Fatalf("%q: %v", line, err)
	}
	want, _ := os.ReadFile(key + ".pub")
	if strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))) != string(want) || comment != agents.KeyComment {
		t.Errorf("key %q comment %q", ssh.MarshalAuthorizedKey(pk), comment)
	}
	for _, o := range []string{"restrict", "no-pty", "no-port-forwarding", "no-agent-forwarding", "no-X11-forwarding", "no-user-rc"} {
		if !slices.Contains(opts, o) {
			t.Errorf("no %s in %q", o, opts)
		}
	}
	if got := openSSHCommand(t, opts); got != agents.RemoteCommand {
		t.Errorf("forced command:\n%s\nwant what a look sends:\n%s", got, agents.RemoteCommand)
	}
	// The same through --identity, given as the .pub.
	if l2, _, err := watcherKeyLine(context.Background(), config.Resolved{Name: "devbox", Address: "192.0.2.1"}, key+".pub", 5*time.Second); err != nil || l2 != line {
		t.Errorf("%q %v", l2, err)
	}
	// ssh-keygen, which reads authorized_keys as sshd does, takes it as one key.
	if kg, err := exec.LookPath("ssh-keygen"); err == nil {
		f := filepath.Join(dir, "authorized_keys")
		if err := os.WriteFile(f, []byte(line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(kg, "-l", "-f", f).CombinedOutput()
		if err != nil || strings.Count(strings.TrimSpace(string(out)), "\n") != 0 || !strings.Contains(string(out), agents.KeyComment) {
			t.Errorf("ssh-keygen -l: %v %s", err, out)
		}
	}
}

func TestWatcherKeyLineRefuses(t *testing.T) {
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "watch")
	main := filepath.Join(dir, "main")
	if err := os.WriteFile(main+".pub", mustRead(t, key+".pub"), 0o600); err != nil {
		t.Fatal(err)
	}
	withOpts := filepath.Join(dir, "opts.pub")
	if err := os.WriteFile(withOpts, append([]byte("command=\"sh\" "), mustRead(t, key+".pub")...), 0o600); err != nil {
		t.Fatal(err)
	}
	two := filepath.Join(dir, "two.pub")
	if err := os.WriteFile(two, append(append(mustRead(t, key+".pub"), '\n'), mustRead(t, key+".pub")...), 0o600); err != nil {
		t.Fatal(err)
	}
	// A key with no .pub beside it, unencrypted: its public half is read from it.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(dir, "bare")
	if err := os.WriteFile(bare, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		t.Fatal(err)
	}
	pub, _ := ssh.NewPublicKey(priv.Public())
	bareWatcher := filepath.Join(dir, "barewatch")
	if err := os.WriteFile(bareWatcher+".pub", ssh.MarshalAuthorizedKey(pub), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bareWatcher, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgKey := filepath.Join(dir, "cfgkey")
	if err := os.WriteFile(cfgKey+".pub", mustRead(t, key+".pub"), 0o600); err != nil {
		t.Fatal(err)
	}
	unrelated := newWatcherKey(t, dir, "unrelated")
	r := func(identity, idFile string) config.Resolved {
		return config.Resolved{Name: "h", Address: "192.0.2.1", AgentsIdentity: identity, IdentityFile: idFile}
	}
	for name, tc := range map[string]struct {
		cfg      string // the person's ssh config
		agent    []string
		r        config.Resolved
		identity string
		want     string
	}{
		"none set":            {"", nil, r("", ""), "", "no agents_identity"},
		"no such key":         {"", nil, r(filepath.Join(dir, "nope"), ""), "", "can't read"},
		"its identity_file":   {"", nil, r(key, main), "", "can log in to"},
		"an IdentityFile":     {"Host *\n  IdentityFile " + cfgKey + "\n", nil, r(key, unrelated), "", "can log in to"},
		"only for that host":  {"Host 192.0.2.1\n  IdentityFile " + cfgKey + "\n", nil, r(key, ""), "", "can log in to"},
		"a private key alone": {"Host *\n  IdentityFile " + bare + "\n", nil, r(bareWatcher, ""), "", "can log in to"},
		"in ssh-agent":        {"", []string{key}, r(key, unrelated), "", "ssh-agent"},
		"options in it":       {"", nil, r("", ""), withOpts, "isn't one public key"},
		"two keys":            {"", nil, r("", ""), two, "isn't one public key"},
		"a private key":       {"", nil, r("", ""), filepath.Join(dir, "priv.pub"), "isn't one public key"},
		"not a key at all":    {"", nil, r("", ""), filepath.Join(dir, "watch.pub.pub"), "can't read"},
		"hostile path text":   {"", nil, r("", ""), filepath.Join(dir, "x\x1b[2J"), "don't show"},
	} {
		if name == "a private key" {
			if err := os.WriteFile(filepath.Join(dir, "priv.pub"), mustRead(t, key), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Run(name, func(t *testing.T) {
			useSSHConfig(t, tc.cfg+"Host *\n  IdentityFile "+unrelated+"\n")
			withAgentKeys(t, tc.agent...)
			_, _, err := watcherKeyLine(context.Background(), tc.r, tc.identity, 5*time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "\x1b") {
				t.Errorf("%v, want %q", err, tc.want)
			}
		})
	}
	// And a key of its own is fine, with the same config and agent.
	useSSHConfig(t, "Host *\n  IdentityFile "+cfgKey+"\n")
	withAgentKeys(t, unrelated)
	if _, _, err := watcherKeyLine(context.Background(), r(newWatcherKey(t, dir, "fresh"), main+"x"), "", 5*time.Second); err != nil {
		t.Errorf("a key of its own: %v", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(b)
}

// What sshd does with the line: run the forced command through the account's shell,
// whatever the client asked for, with that in SSH_ORIGINAL_COMMAND. Whatever a client
// sends, only `claude agents --json` runs.
func TestWatcherForcedCommandIgnoresTheClient(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "watch")
	useSSHConfig(t, "")
	withAgentKeys(t)
	line, _, err := watcherKeyLine(context.Background(), config.Resolved{Name: "h", Address: "192.0.2.1", AgentsIdentity: key}, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _, opts, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	forced := openSSHCommand(t, opts)

	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	ran := filepath.Join(home, "ran")
	pwned := filepath.Join(home, "pwned")
	claude := "#!/bin/sh\nprintf '%s|' \"$@\" >>'" + ran + "'\nprintf '\\n' >>'" + ran + "'\necho '[]'\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(claude), 0o755); err != nil {
		t.Fatal(err)
	}
	attempts := []string{
		agents.RemoteCommand,
		"touch " + pwned,
		"claude agents --json; touch " + pwned,
		"$(touch " + pwned + ")",
		"`touch " + pwned + "`",
		"'; touch " + pwned + "; '",
		"\"; touch " + pwned + "; \"",
		"claude rm --all",
		"sh -c 'touch " + pwned + "'",
		"x\ntouch " + pwned,
		"--help",
	}
	shells := []string{"sh"}
	if _, err := exec.LookPath("bash"); err == nil {
		shells = append(shells, "bash")
	}
	for _, shell := range shells {
		for _, a := range attempts {
			c := exec.Command(shell, "-c", forced)
			// The PATH a non-login ssh session has, without ~/.local/bin: the
			// command has to find claude there itself.
			c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "SSH_ORIGINAL_COMMAND=" + a}
			out, err := c.CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != "[]" {
				t.Errorf("%s, %q: %v %s", shell, a, err, out)
			}
		}
	}
	if _, err := os.Stat(pwned); err == nil {
		t.Error("the client's command ran")
	}
	b, _ := os.ReadFile(ran)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != len(attempts)*len(shells) {
		t.Errorf("claude ran %d times: %q", len(lines), lines)
	}
	for _, l := range lines {
		if l != "agents|--json|" {
			t.Errorf("claude was run as %q", l)
		}
	}
}

// `agents key` prints and contacts nothing; a host called key is still reachable
// with --, and as a plain name the word is the subcommand.
func TestAgentsKeyCommand(t *testing.T) {
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "watch")
	cfgFile := filepath.Join(dir, "hosts.yaml")
	yaml := "version: 1\nhosts:\n- {name: devbox, address: 192.0.2.1, agents_identity: " + key + "}\n- {name: key, address: 192.0.2.2}\n"
	if err := os.WriteFile(cfgFile, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METERM_CONFIG", cfgFile)
	argvFile := fakeSSHScript(t, `echo '[]'`)
	cannedSSHG(t, "user agent\nhostname 192.0.2.1\nidentityfile /nowhere/x\n")
	withAgentKeys(t)
	out := captureStdout(t, func() {
		c := newAgentsCmd()
		c.SetArgs([]string{"key", "devbox"})
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err != nil {
			t.Error(err)
		}
	})
	if !strings.HasPrefix(out, "restrict,") || strings.Count(out, "\n") != 1 {
		t.Errorf("stdout: %q", out)
	}
	if _, err := os.Stat(argvFile); !os.IsNotExist(err) {
		t.Error("ssh ran")
	}
	out = captureStdout(t, func() {
		c := newAgentsCmd()
		c.SetArgs([]string{"--format", "json", "--", "key"})
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(out, `"host":"key"`) {
		t.Errorf("host called key: %q", out)
	}
}

// With a watcher key, ssh is offered that key and no other, as real ssh reads the
// options: not the host's identity_file, not a key from extra_args, not one from the
// person's ssh config. Jump hops still read that config, so they log in as always.
func TestWatcherLookOffersOnlyTheWatcherKey(t *testing.T) {
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "watch")
	useSSHConfig(t, "Host *\n  IdentityFile /nowhere/from-config\n  IdentitiesOnly no\n  IdentityAgent /nowhere/agent.sock\n  CertificateFile /nowhere/cert\n")

	r := config.Resolved{Address: "192.0.2.1", User: "agent", IdentityFile: "/nowhere/main", AgentsIdentity: key,
		ExtraArgs: []string{"-i", "/nowhere/extra", "-o", "IdentityFile=/nowhere/extra2", "-oIdentitiesOnly=no", "-p", "2222"}}
	got := watcherG(t, r)
	if ids := got["identityfile"]; len(ids) != 1 || ids[0] != key {
		t.Errorf("keys offered to the target: %q", ids)
	}
	if got.one("identitiesonly") != "yes" || got.one("port") != "2222" || got.one("user") != "agent" {
		t.Errorf("ssh -G: %v", got)
	}
	if got.one("identityagent") == "/nowhere/agent.sock" || len(got["certificatefile"]) != 0 {
		t.Errorf("the config's agent or certificate reached the target: %v", got)
	}
	// A jump hop gets the same -F, and with it the person's config and keys.
	_, sshConfig, remove, err := watcherLook(context.Background(), r, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	hop, err := exec.Command("ssh", "-G", "-F", sshConfig, "192.0.2.99").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hop), "\nidentityfile /nowhere/from-config\n") || !strings.Contains(string(hop), "\nbatchmode yes\n") {
		t.Errorf("hop:\n%s", hop)
	}
}

// watcherG is what real ssh makes of a watcher look at r.
func watcherG(t *testing.T, r config.Resolved) sshSettings {
	t.Helper()
	wr, sshConfig, remove, err := watcherLook(context.Background(), r, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	args := sshrun.BuildArgs(config.Resolved{Address: wr.Address, User: wr.User, Port: wr.Port, IdentityFile: wr.IdentityFile,
		ExtraArgs: agentSSHArgs(wr.Resolved, sshConfig, 5*time.Second)})
	out, err := exec.Command("ssh", append([]string{"-G"}, args...)...).Output()
	if err != nil {
		t.Fatalf("ssh -G %q: %v", args, err)
	}
	set := sshSettings{}
	for _, l := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(l, " "); ok {
			set[k] = append(set[k], v)
		}
	}
	return set
}

// What the person's ssh config says about where the host is, as whom and which host
// keys count still holds for a watcher look; only its keys don't.
func TestWatcherLookKeepsTheRestOfTheConfig(t *testing.T) {
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "watch")
	kh := filepath.Join(dir, "known_hosts")
	useSSHConfig(t, "Host 192.0.2.1\n  HostName 198.51.100.5\n  User cfguser\n  Port 2200\n  ProxyJump jumpy\n"+
		"  UserKnownHostsFile "+kh+"\n  GlobalKnownHostsFile "+kh+"2\n  HostKeyAlias alias1\n  IdentityFile /nowhere/cfg\n"+
		"  AddressFamily inet\n  BindAddress 192.0.2.200\n  BindInterface lo\n"+
		"Host *\n  IdentityFile /nowhere/all\n")
	got := watcherG(t, config.Resolved{Address: "192.0.2.1", AgentsIdentity: key})
	for k, want := range map[string]string{
		"hostname": "198.51.100.5", "user": "cfguser", "port": "2200", "proxyjump": "jumpy",
		"userknownhostsfile": kh, "globalknownhostsfile": kh + "2", "hostkeyalias": "alias1",
		"stricthostkeychecking": "true", "identitiesonly": "yes",
		"addressfamily": "inet", "bindaddress": "192.0.2.200", "bindinterface": "lo",
	} {
		if got.one(k) != want {
			t.Errorf("%s = %q, want %q", k, got.one(k), want)
		}
	}
	if ids := got["identityfile"]; len(ids) != 1 || ids[0] != key {
		t.Errorf("keys: %q", ids)
	}
	// What meterm says itself still comes first.
	got = watcherG(t, config.Resolved{Address: "192.0.2.1", User: "agent", Port: 2222, AgentsIdentity: key})
	if got.one("user") != "agent" || got.one("port") != "2222" {
		t.Errorf("meterm's own: %v", got)
	}
	// A value that can't go in a config line as one word isn't carried: the look isn't made.
	useSSHConfig(t, "Host 192.0.2.1\n  HostKeyAlias a$b\n")
	_, _, remove, err := watcherLook(context.Background(), config.Resolved{Address: "192.0.2.1", AgentsIdentity: key}, 5*time.Second)
	if err == nil {
		remove()
	}
	if err == nil || !strings.Contains(err.Error(), "HostKeyAlias") {
		t.Errorf("%v", err)
	}
}

// Through a look: the argv has the watcher key and no other, its config is gone
// afterwards, and a refused watcher key is said to be that, once, with no second try,
// whoever chose the user ssh named.
func TestWatcherKeyRefusedIsNotAFallback(t *testing.T) {
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "watch")
	for _, tc := range []struct {
		name, user, gUser, stderr string
		watcher                   bool
	}{
		{"user from meterm", "agent", "agent", "agent@192.0.2.1: Permission denied (publickey).", true},
		{"no user: ssh's own", "", "bcd", "bcd@192.0.2.1: Permission denied (publickey).", true},
		{"user from the config", "", "cfguser", "cfguser@192.0.2.1: Permission denied (publickey).", true},
		{"named by its HostName", "", "cfguser", "cfguser@198.51.100.5: Permission denied (publickey).", true},
		{"a jump host", "agent", "agent", "bcd@192.0.2.50: Permission denied (publickey).", false},
		{"a jump host, no user", "", "bcd", "bcd@192.0.2.50: Permission denied (publickey).", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argvFile := fakeSSHScript(t, `echo '`+tc.stderr+`' >&2; exit 255`)
			cannedSSHG(t, "user "+tc.gUser+"\nhostname 198.51.100.5\nport 22\n")
			job := agentJob{label: "x", resolve: func(context.Context) config.Resolved {
				return config.Resolved{Address: "192.0.2.1", User: tc.user, IdentityFile: "/nowhere/main", AgentsIdentity: key,
					ExtraArgs: []string{"-i", "/nowhere/extra"}}
			}}
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			got := askAgents(context.Background(), job, sshAgentQuery("", 5*time.Second), 5*time.Second)
			if tc.watcher != (got.Error == agentsWatcherRefused) || got.Reachable || got.Agents != nil {
				t.Errorf("%+v", got)
			}
			if !tc.watcher && !lookUnreachable(got.Error) {
				t.Errorf("hop: %q", got.Error)
			}
			b, _ := os.ReadFile(argvFile)
			if strings.Contains(string(b), "/nowhere/") || strings.Count(string(b), agents.RemoteCommand) != 1 {
				t.Errorf("argv:\n%s", b)
			}
			if left, _ := os.ReadDir(tmp); len(left) != 0 {
				t.Errorf("left behind: %v", left)
			}
		})
	}
}

const prohibited = `echo 'channel 0: open failed: administratively prohibited: open failed' >&2; echo 'stdio forwarding failed' >&2; exit 255`

// fwd is a fake ssh's answer to -W, ahead of what it does otherwise.
func fwd(onW string) string {
	return `for a; do [ "$a" = -W ] && { ` + onW + `; }; done; `
}

// --check: the key ran what was sent, so it isn't limited (2); the host ran something
// else, so it is (0); refused or no answer, can't tell (1).
func TestCheckWatcherKey(t *testing.T) {
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "watch")
	job := agentJob{label: "devbox", resolve: func(context.Context) config.Resolved {
		return config.Resolved{Name: "devbox", Address: "192.0.2.1", AgentsIdentity: key}
	}}
	for _, tc := range []struct {
		name, script string
		code         int
		want         string
	}{
		{"unlimited", `for a; do last=$a; done; eval "$last"`, keyCheckUnlimited, "NOT LIMITED"},
		{"forced command", fwd(prohibited) + `echo '[]'`, keyCheckLimited, "refuses port forwarding"},
		{"forced, no claude", fwd(prohibited) + `exit 127`, keyCheckLimited, "refuses port forwarding"},
		{"forced, but forwards", fwd(`echo SSH-2.0-OpenSSH_10.0; exec sleep 30`) + `echo '[]'`, keyCheckUnlimited, "port forwarding is allowed"},
		{"forced, forward tried", fwd(`echo 'channel 0: open failed: connect failed: Connection refused' >&2; echo 'stdio forwarding failed' >&2; exit 255`) + `echo '[]'`, keyCheckUnlimited, "port forwarding is allowed"},
		{"forced, forward unclear", fwd(`echo 'stdio forwarding failed' >&2; exit 255`) + `echo '[]'`, keyCheckUnsure, "couldn't be told"},
		{"refused", `echo 'agent@192.0.2.1: Permission denied (publickey).' >&2; exit 255`, keyCheckUnsure, "refused the watcher key"},
		{"down", `echo 'ssh: connect to host 192.0.2.1 port 22: Connection refused' >&2; exit 255`, keyCheckUnsure, "couldn't log in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argvFile := fakeSSHScript(t, tc.script)
			cannedSSHG(t, "user agent\nhostname 192.0.2.1\n")
			msg, code := checkWatcherKey(context.Background(), job, 5*time.Second)
			if code != tc.code || !strings.Contains(msg, tc.want) {
				t.Errorf("%d %q", code, msg)
			}
			b, _ := os.ReadFile(argvFile)
			argv := strings.Split(strings.TrimSpace(string(b)), "\n")
			hasEcho := slices.ContainsFunc(argv, func(a string) bool { return strings.HasPrefix(a, "echo meterm-watcher-check-") })
			if !hasEcho || !slices.Contains(argv, key) {
				t.Errorf("argv %q", argv)
			}
			// The forward goes to the host's own sshd, with the watcher key only.
			if i := slices.Index(argv, "-W"); i >= 0 && argv[i+1] != "127.0.0.1:22" {
				t.Errorf("-W %q", argv[i+1])
			}
		})
	}
	// Two checks don't send the same word.
	argvFile := fakeSSHScript(t, `echo '[]'`)
	cannedSSHG(t, "user agent\n")
	checkWatcherKey(context.Background(), job, 5*time.Second)
	checkWatcherKey(context.Background(), job, 5*time.Second)
	b, _ := os.ReadFile(argvFile)
	var words []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "echo ") {
			words = append(words, l)
		}
	}
	if len(words) != 2 || words[0] == words[1] {
		t.Errorf("%q", words)
	}
}

// An agents_identity from a group or defaults isn't checked when it's written: a path
// that has characters that don't show is refused when it's used, file or no file.
func TestAgentsIdentityWithHiddenCharacters(t *testing.T) {
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "wa\u200btch")
	job := agentJob{label: "x", resolve: func(context.Context) config.Resolved {
		return config.Resolved{Address: "192.0.2.1", AgentsIdentity: key}
	}}
	got := askAgents(context.Background(), job, func(context.Context, config.Resolved) ([]byte, string, bool, error) {
		t.Error("ssh ran")
		return nil, "", false, nil
	}, time.Second)
	if !strings.Contains(got.Error, "don't show") {
		t.Errorf("%+v", got)
	}
	cfg := hostsForAgents(t, "version: 1\ndefaults: {agents_identity: \""+strings.ReplaceAll(key, "\u200b", "\\u200b")+"\"}\nhosts:\n- {name: a, address: 192.0.2.1}\n")
	h, _ := cfg.Find("a")
	if r := cfg.Resolve(*h); checkAgentsIdentity(r) == "" {
		t.Errorf("from defaults: %q", r.AgentsIdentity)
	}
}
