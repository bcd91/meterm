package tui

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func shellExit(t *testing.T, code string) error {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	err = exec.Command(sh, "-c", "exit "+code).Run()
	if _, ok := err.(*exec.ExitError); !ok {
		t.Fatalf("exit %s: %v", code, err)
	}
	return err
}

// A session whose last command exited non-zero ended normally: the picker
// says so plainly. Only ssh's own 255 reads as a failure.
func TestSessionEndingNonZeroIsNotAFailure(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	m.reselect("web3")
	// meterm itself failing (no such host, a bad host file) exits 1 as well,
	// and leaves no word that a session ran.
	m = drive(t, m, shellExit(t, "1"), "enter")
	if v := stripANSI(m.View()); !strings.Contains(v, "connect web3 failed: exit status 1") || m.flashNeutral {
		t.Errorf("exit 1 with no session:\n%s", v)
	}
	if err := os.WriteFile(m.outcomeFile(), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m = drive(t, m, shellExit(t, "1"), "enter")
	v := stripANSI(m.View())
	if strings.Contains(v, "failed") || !strings.Contains(v, "session on web3 ended, exit 1") {
		t.Errorf("exit 1 after a session:\n%s", v)
	}
	if !m.flashNeutral {
		t.Error("exit 1 after a session is styled as a failure")
	}
	m = drive(t, m, shellExit(t, "255"), "enter")
	v = stripANSI(m.View())
	if !strings.Contains(v, "connect web3") || !strings.Contains(v, "ssh exit 255") || m.flashNeutral {
		t.Errorf("exit 255:\n%s", v)
	}
	// Anything else that exits non-zero is still a failure.
	m = drive(t, m, shellExit(t, "1"), "S")
	if v = stripANSI(m.View()); !strings.Contains(v, "stats web3 failed: exit status 1") || m.flashNeutral {
		t.Errorf("stats exit 1:\n%s", v)
	}
}
