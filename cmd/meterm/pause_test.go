package main

import (
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/config"
)

// From the picker, a failure always waits for enter and says how it ended;
// a clean end waits only when there's output to read.
func TestPausePrompt(t *testing.T) {
	for _, c := range []struct {
		code           int
		fromTUI, quiet bool
		want           string // "" = no pause
	}{
		{0, false, false, ""},
		{1, false, false, ""},
		{0, true, true, ""},
		{0, true, false, "press enter"},
		{255, true, true, "exit status 255: press enter"},
		{1, true, false, "exit status 1: press enter"},
	} {
		got := pausePrompt(c.code, c.fromTUI, c.quiet)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("pausePrompt(%d, %v, %v) = %q, want %q", c.code, c.fromTUI, c.quiet, got, c.want)
		}
	}
}

// A clean ssh logout goes straight back to the picker.
func TestConnectIsQuietOnSuccess(t *testing.T) {
	dir := cliSandbox(t)
	installFakeSSH(t, t.TempDir())
	t.Setenv("METERM_TEST_SSH_LOG", filepath.Join(dir, "ssh.log"))
	t.Setenv(fakeSSHExitEnv, "0")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Upsert(config.Host{Name: "web1", Address: "192.0.2.40"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	old := quietOnSuccess
	quietOnSuccess = false
	t.Cleanup(func() { quietOnSuccess = old })
	c := newConnectCmd()
	if err := c.RunE(c, []string{"web1"}); err != nil {
		t.Fatal(err)
	}
	if !quietOnSuccess {
		t.Error("connect doesn't mark itself quiet: a clean logout would wait for enter")
	}
}
