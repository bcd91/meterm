package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"meterm/internal/config"
	"meterm/internal/tui"
)

// exitErr is a real *exec.ExitError with the given code, from the test
// binary acting as a fake ssh.
func sshExitErr(t *testing.T, code string) error {
	t.Helper()
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), fakeSSHExitEnv+"="+code, "METERM_TEST_SSH_LOG=")
	err := c.Run()
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("exit %s: %v", code, err)
	}
	return err
}

// ssh's 255 after a long session can be a dropped line as well as a refused
// login: the outcome says it could be either. A session whose shell exited
// non-zero ran; it isn't a failure.
func TestConnectOutcomeWording(t *testing.T) {
	code, o := connectOutcome(sshExitErr(t, "255"), 3*time.Hour)
	if code != 255 || o.ExitCode == nil || *o.ExitCode != 255 || o.Note != "connection lost, or never made" || o.Took != 3*time.Hour {
		t.Errorf("255 after 3h: %d %+v", code, o)
	}
	code, o = connectOutcome(sshExitErr(t, "1"), 5*time.Minute)
	if code != 1 || o.ExitCode == nil || *o.ExitCode != 1 || o.Note != "" {
		t.Errorf("exit 1: %d %+v", code, o)
	}
	if code, o = connectOutcome(nil, 2*time.Hour); code != 0 || o.ExitCode == nil || *o.ExitCode != 0 {
		t.Errorf("exit 0: %d %+v", code, o)
	}
}

// From the picker, the pause after a session that ended non-zero says the
// session ended, not that something failed; ssh's own 255 still says exit status.
func TestSessionPauseIsNeutral(t *testing.T) {
	if got := sessionPausePrompt(1); !strings.Contains(got, "session ended, exit 1") || !strings.Contains(got, "press enter") {
		t.Errorf("exit 1: %q", got)
	}
	if got := sessionPausePrompt(255); !strings.Contains(got, "ssh exit 255") || !strings.Contains(got, "press enter") {
		t.Errorf("exit 255: %q", got)
	}
}

// connect tells the picker that a session ran, and only then: meterm's own
// errors and ssh's 255 exit non-zero too.
func TestConnectTellsThePickerASessionRan(t *testing.T) {
	dir := cliSandbox(t)
	installFakeSSH(t, t.TempDir())
	t.Setenv("METERM_TEST_SSH_LOG", filepath.Join(dir, "ssh.log"))
	out := filepath.Join(dir, "outcome")
	t.Setenv(tui.SessionOutcomeEnv, out)
	cfg, _ := config.Load(cfgPath)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upsert(config.Host{Name: "web1", Address: "192.0.2.40"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		host, exit string
		want       string
	}{
		{"nosuch", "1", ""},
		{"web1", "255", ""},
		{"web1", "1", "1\n"},
		{"web1", "0", "0\n"},
	} {
		os.Remove(out)
		t.Setenv(fakeSSHExitEnv, c.exit)
		_, _ = connect([]string{c.host})
		b, _ := os.ReadFile(out)
		if string(b) != c.want {
			t.Errorf("%s exit %s: outcome %q, want %q", c.host, c.exit, b, c.want)
		}
	}
}
