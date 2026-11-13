package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/config"
)

// Every question answered no ends the command with errCancelled, which
// exits 1 with "meterm: cancelled: …": a script must see it didn't happen.
func TestAnsweringNoIsCancelled(t *testing.T) {
	run := func(t *testing.T, stdin string, fn func() error) (out string, err error) {
		t.Helper()
		withStdin(t, stdin, func() { out = stdout(t, func() { err = fn() }) })
		return
	}
	t.Run("rm", func(t *testing.T) {
		cliSandbox(t)
		saveHosts(t, config.Host{Name: "web1", Address: "192.0.2.51"})
		_, err := run(t, "n\n", func() error { c := newRmCmd(); c.SetArgs([]string{"web1"}); return c.Execute() })
		if !errors.Is(err, errCancelled) {
			t.Errorf("err = %v", err)
		}
		if cfg, _ := loadConfig(); cfg == nil || func() bool { _, i := cfg.Find("web1"); return i < 0 }() {
			t.Error("removed anyway")
		}
	})
	t.Run("group rm", func(t *testing.T) {
		cliSandbox(t)
		saveGroup(t, "pay", config.Group{Env: config.EnvProduction})
		saveHosts(t, config.Host{Name: "pay1", Address: "192.0.2.60", Group: "pay"})
		asTerminal(t, true)
		_, _, err := groupCmd(t, "n\n", "rm", "pay")
		if !errors.Is(err, errCancelled) || !groupExists(t, "pay") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("exec", func(t *testing.T) {
		dir := cliSandbox(t)
		installFakeSSH(t, t.TempDir())
		t.Setenv(fakeSSHExitEnv, "0")
		t.Setenv("METERM_TEST_SSH_LOG", filepath.Join(dir, "ssh.log"))
		saveHosts(t, config.Host{Name: "pay1", Address: "192.0.2.50", Env: config.EnvProduction, Tags: []string{"web"}})
		asTerminal(t, true)
		_, err := run(t, "n\n", func() error {
			c := newExecCmd()
			c.SetArgs([]string{"--tag", "web", "--", "uptime"})
			return c.Execute()
		})
		if !errors.Is(err, errCancelled) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("cp --sync --delete", func(t *testing.T) {
		dir := cliSandbox(t)
		installFake(t, t.TempDir(), "rsync")
		t.Setenv(fakeSSHExitEnv, "0")
		t.Setenv("METERM_TEST_SSH_LOG", filepath.Join(dir, "rsync.log"))
		saveHosts(t, config.Host{Name: "pay1", Address: "192.0.2.50", Env: config.EnvProduction})
		local := filepath.Join(dir, "src") + "/"
		_ = os.Mkdir(local, 0o700)
		asTerminal(t, true)
		_, err := run(t, "n\n", func() error { return runCopy(local, "pay1:/srv/", false, true, []string{"-a", "--delete"}, false) })
		if !errors.Is(err, errCancelled) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("tunnel enable", func(t *testing.T) {
		calls, _ := serviceEnv(t)
		host := "version: 1\nhosts:\n- name: box\n  address: 192.0.2.1\n  env: production\n  forwards:\n" +
			"  - {name: web, type: L, local_port: 8080, remote_host: localhost, remote_port: 80}\n"
		if err := os.WriteFile(os.Getenv("METERM_CONFIG"), []byte(host), 0o600); err != nil {
			t.Fatal(err)
		}
		asTerminal(t, true)
		_, err := run(t, "n\n", func() error { c := newTunnelEnableCmd(); c.SetArgs([]string{"box/web"}); return c.Execute() })
		if !errors.Is(err, errCancelled) || len(*calls) != 0 {
			t.Errorf("err = %v, systemctl %q", err, *calls)
		}
	})
	if !strings.HasPrefix(errCancelled.Error(), "cancelled") {
		t.Errorf("errCancelled says %q", errCancelled)
	}
}
