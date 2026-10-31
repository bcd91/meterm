package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeSSHEnv makes the test binary act as an ssh that fails three times, then
// stays connected, counting its runs in the file this names. A program rather
// than a script, so it runs on Windows too.
const fakeSSHEnv = "METERM_TEST_FAKE_SSH"

// runTunnelUpEnv makes the test binary act as `meterm tunnel up`, with the real exit.
const runTunnelUpEnv = "METERM_TEST_TUNNEL_UP"

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
	os.Exit(m.Run())
}

// installFakeSSH puts a copy of the test binary first on PATH, as ssh.
func installFakeSSH(t *testing.T, dir string) {
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
	name := "ssh"
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
