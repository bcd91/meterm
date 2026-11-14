package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
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

// newWatcherKey writes a made-up ed25519 key pair's public half, and an empty
// stand-in for the private one, and returns the private path.
func newWatcherKey(t *testing.T, dir, name string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("not a real private key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+".pub", bytes.TrimSpace(ssh.MarshalAuthorizedKey(pk)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
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
	line, err := watcherKeyLine(config.Resolved{Name: "devbox", AgentsIdentity: key}, "")
	if err != nil {
		t.Fatal(err)
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
	if l2, err := watcherKeyLine(config.Resolved{Name: "devbox"}, key+".pub"); err != nil || l2 != line {
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
	for name, tc := range map[string]struct {
		r        config.Resolved
		identity string
		want     string
	}{
		"none set":          {config.Resolved{Name: "h"}, "", "no agents_identity"},
		"no such key":       {config.Resolved{Name: "h", AgentsIdentity: filepath.Join(dir, "nope")}, "", "can't read"},
		"its main key":      {config.Resolved{Name: "h", AgentsIdentity: key, IdentityFile: main}, "", "identity_file"},
		"options in it":     {config.Resolved{Name: "h"}, withOpts, "isn't one public key"},
		"two keys":          {config.Resolved{Name: "h"}, two, "isn't one public key"},
		"a private key":     {config.Resolved{Name: "h"}, key, "isn't one public key"},
		"not a key at all":  {config.Resolved{Name: "h"}, filepath.Join(dir, "watch.pub.pub"), "can't read"},
		"hostile path text": {config.Resolved{Name: "h"}, filepath.Join(dir, "x\x1b[2J"), "can't read"},
	} {
		if name == "a private key" {
			// key itself, read as if it were the .pub
			if err := os.WriteFile(filepath.Join(dir, "priv.pub"), mustRead(t, key), 0o600); err != nil {
				t.Fatal(err)
			}
			tc.identity = filepath.Join(dir, "priv.pub")
		}
		_, err := watcherKeyLine(tc.r, tc.identity)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "\x1b") {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
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
	line, err := watcherKeyLine(config.Resolved{Name: "h", AgentsIdentity: key}, "")
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
	if runtime.GOOS == "windows" {
		t.Skip("agents refuses to run on Windows")
	}
	realSSHForG(t)
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "watch")
	userCfg := filepath.Join(dir, "user_config")
	if err := os.WriteFile(userCfg, []byte("Host *\n  IdentityFile /nowhere/from-config\n  IdentitiesOnly no\n  IdentityAgent /nowhere/agent.sock\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := agentSSHIncludes
	agentSSHIncludes = []string{userCfg}
	defer func() { agentSSHIncludes = old }()

	r := config.Resolved{Address: "192.0.2.1", User: "agent", IdentityFile: "/nowhere/main", AgentsIdentity: key,
		ExtraArgs: []string{"-i", "/nowhere/extra", "-o", "IdentityFile=/nowhere/extra2", "-oIdentitiesOnly=no", "-p", "2222"}}
	wr, sshConfig, remove, err := watcherLook(r)
	if err != nil {
		t.Fatal(err)
	}
	defer remove()
	args := sshrun.BuildArgs(config.Resolved{Address: wr.Address, User: wr.User, IdentityFile: wr.IdentityFile,
		ExtraArgs: agentSSHArgs(wr, sshConfig, 5*time.Second)})
	out, err := exec.Command("ssh", append([]string{"-G"}, args...)...).Output()
	if err != nil {
		t.Fatalf("ssh -G %q: %v", args, err)
	}
	var ids []string
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(l, "identityfile "); ok {
			ids = append(ids, v)
		}
	}
	if len(ids) != 1 || ids[0] != key {
		t.Errorf("keys offered to the target: %q", ids)
	}
	if !strings.Contains(string(out), "\nidentitiesonly yes\n") || !strings.Contains(string(out), "\nport 2222\n") {
		t.Errorf("ssh -G:\n%s", out)
	}
	if strings.Contains(string(out), "/nowhere/agent.sock") {
		t.Errorf("the config's agent socket reached the target:\n%s", out)
	}
	// A jump hop gets the same -F, and with it the person's config and keys.
	hop, err := exec.Command("ssh", "-G", "-F", sshConfig, "192.0.2.99").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hop), "\nidentityfile /nowhere/from-config\n") || !strings.Contains(string(hop), "\nbatchmode yes\n") {
		t.Errorf("hop:\n%s", hop)
	}
}

// Through a look: the argv has the watcher key and no other, its config is gone
// afterwards, and a refused watcher key is said to be that, once, with no second try.
func TestWatcherKeyRefusedIsNotAFallback(t *testing.T) {
	dir := t.TempDir()
	key := newWatcherKey(t, dir, "watch")
	argvFile := fakeSSHScript(t, `echo 'agent@192.0.2.1: Permission denied (publickey).' >&2; exit 255`)
	job := agentJob{label: "x", resolve: func(context.Context) config.Resolved {
		return config.Resolved{Address: "192.0.2.1", User: "agent", IdentityFile: "/nowhere/main", AgentsIdentity: key,
			ExtraArgs: []string{"-i", "/nowhere/extra"}}
	}}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	got := askAgents(context.Background(), job, sshAgentQuery("", 5*time.Second), 5*time.Second)
	if got.Reachable || got.Agents != nil || !strings.Contains(got.Error, "watcher key") || lookUnreachable(got.Error) {
		t.Errorf("%+v", got)
	}
	b, _ := os.ReadFile(argvFile)
	if strings.Contains(string(b), "/nowhere/") || strings.Count(string(b), agents.RemoteCommand) != 1 {
		t.Errorf("argv:\n%s", b)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	// A jump host refusing its usual key isn't the watcher key's fault.
	fakeSSHScript(t, `echo 'bcd@192.0.2.50: Permission denied (publickey).' >&2; exit 255`)
	got = askAgents(context.Background(), job, sshAgentQuery("", 5*time.Second), 5*time.Second)
	if strings.Contains(got.Error, "watcher key") || !lookUnreachable(got.Error) {
		t.Errorf("hop refusal: %+v", got)
	}
}
