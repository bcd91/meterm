package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"meterm/internal/testenv"
)

// fakeSSHEnv makes the test binary act as an ssh that fails three times, then
// stays connected, counting its runs in the file this names. A program rather
// than a script, so it runs on Windows too.
const fakeSSHEnv = "METERM_TEST_FAKE_SSH"

// runTunnelUpEnv makes the test binary act as `meterm tunnel up`, with the real exit.
const runTunnelUpEnv = "METERM_TEST_TUNNEL_UP"

// fakeSSHExitEnv makes the test binary act as an ssh that logs its
// arguments to $METERM_TEST_SSH_LOG and exits with the code this names.
const fakeSSHExitEnv = "METERM_TEST_SSH_EXIT"

func TestMain(m *testing.M) {
	if args := os.Getenv(runTunnelUpEnv); args != "" {
		c := newTunnelUpCmd()
		c.SetArgs(strings.Split(args, "\x1f"))
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if code := os.Getenv(fakeSSHExitEnv); code != "" {
		if log := os.Getenv("METERM_TEST_SSH_LOG"); log != "" {
			f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err == nil {
				fmt.Fprintln(f, strings.Join(os.Args[1:], " "))
				f.Close()
			}
		}
		// What the activity log held while "ssh" ran: the evidence a crash
		// mid-session would leave.
		if seen := os.Getenv("METERM_TEST_SSH_SEES"); seen != "" {
			b, _ := os.ReadFile(filepath.Join(os.Getenv("METERM_ACTIVITY_DIR"), "events.jsonl"))
			_ = os.WriteFile(seen, b, 0o600)
		}
		n, _ := strconv.Atoi(code)
		os.Exit(n)
	}
	if runs := os.Getenv(fakeSSHEnv); runs != "" {
		f, err := os.OpenFile(runs, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(1)
		}
		fmt.Fprintln(f, "run")
		f.Close()
		b, _ := os.ReadFile(runs)
		if strings.Count(string(b), "\n") > 3 {
			time.Sleep(time.Minute)
			os.Exit(0)
		}
		os.Exit(255)
	}
	done := testenv.Setup()
	code := guardSSH(m)
	done()
	os.Exit(code)
}

// realSSHDir is where ssh was found before guardSSH put its fake first.
var realSSHDir string

// realSSHForG puts the ssh found before the guard back first on PATH, for a
// test that asks it how it reads arguments with `ssh -G -F <file>`: -G
// connects to nothing, and -F keeps it from reading ~/.ssh/config.
func realSSHForG(t *testing.T) {
	t.Helper()
	if realSSHDir == "" {
		t.Skip("no ssh")
	}
	t.Setenv("PATH", realSSHDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// guardSSH runs the tests with ssh, scp, sftp and rsync on PATH replaced by
// a script that only notes it ran, and fails the package if any test reached
// it: such a test would have run the real ssh, which reads the person's own
// ~/.ssh/config whatever HOME says. A test that means to run ssh puts its
// own fake first (installFakeSSH, fakeSSH).
func guardSSH(m *testing.M) int {
	// A test that writes the activity log or history without a sandbox of its
	// own (cliSandbox) writes here, never to the person's own.
	state, err := os.MkdirTemp("", "meterm-test-state")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(state)
	os.Setenv("METERM_ACTIVITY_DIR", filepath.Join(state, "activity"))
	os.Setenv("METERM_HISTORY", filepath.Join(state, "history.json"))
	if runtime.GOOS == "windows" {
		return m.Run()
	}
	dir, err := os.MkdirTemp("", "meterm-ssh-guard")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	if real, err := exec.LookPath("ssh"); err == nil {
		realSSHDir = filepath.Dir(real)
	}
	log := filepath.Join(dir, "ran")
	script := "#!/bin/sh\necho \"$(basename \"$0\") $*\" >> '" + log + "'\nexit 255\n"
	for _, name := range []string{"ssh", "scp", "sftp", "rsync"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	if b, _ := os.ReadFile(log); len(b) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: a test ran ssh without a fake of its own (the real one would read ~/.ssh):\n%s", b)
		return 1
	}
	return code
}

// installFakeSSH puts a copy of the test binary first on PATH, as ssh.
func installFakeSSH(t *testing.T, dir string) {
	t.Helper()
	installFake(t, dir, "ssh")
}

// installFake puts a copy of the test binary first on PATH, as name.
func installFake(t *testing.T, dir, name string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
