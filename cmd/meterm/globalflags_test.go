package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/config"
)

// --config (and --debug) before or right after connect, which parses its
// own arguments, are meterm's, not a host name.
func TestGlobalFlagsBeforeConnect(t *testing.T) {
	for _, args := range [][]string{
		{"--config", "OTHER", "connect", "web9"},
		{"--config=OTHER", "connect", "web9"},
		{"connect", "--config", "OTHER", "web9"},
		{"--debug", "--config", "OTHER", "c", "web9"},
		{"connect", "--tmux", "--config", "OTHER", "web9"},
		{"connect", "--log-args", "--config=OTHER", "--tmux", "web9"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := cliSandbox(t)
			installFakeSSH(t, t.TempDir())
			t.Setenv(fakeSSHExitEnv, "0")
			sshLog := filepath.Join(dir, "ssh.log")
			t.Setenv("METERM_TEST_SSH_LOG", sshLog)
			other := filepath.Join(dir, "other.yaml")
			cfg, err := config.Load(other)
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.Update(func(c *config.Config) error {
				c.Upsert(config.Host{Name: "web9", Address: "192.0.2.99"})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for i, a := range args {
				args[i] = strings.ReplaceAll(a, "OTHER", other)
			}
			if err := runRoot(t, args...); err != nil {
				t.Fatalf("%v", err)
			}
			if b, _ := os.ReadFile(sshLog); !strings.Contains(string(b), "192.0.2.99") {
				t.Errorf("ssh ran %q", b)
			}
		})
	}
}

// After --, --config is ssh's, not meterm's.
func TestGlobalFlagsAfterDoubleDashGoToSSH(t *testing.T) {
	dir := cliSandbox(t)
	installFakeSSH(t, t.TempDir())
	t.Setenv(fakeSSHExitEnv, "0")
	sshLog := filepath.Join(dir, "ssh.log")
	t.Setenv("METERM_TEST_SSH_LOG", sshLog)
	saveHosts(t, config.Host{Name: "web9", Address: "192.0.2.99"})
	if err := runRoot(t, "connect", "web9", "--", "--config", "elsewhere"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(sshLog); !strings.Contains(string(b), "192.0.2.99") || !strings.Contains(string(b), "--config elsewhere") {
		t.Errorf("ssh ran %q", b)
	}
}
