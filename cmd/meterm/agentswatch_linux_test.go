package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openPty is a pseudo-terminal: the master, and the path of its slave.
func openPty(t *testing.T) (*os.File, string) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip(err)
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		m.Close()
		t.Skip(err)
	}
	n, err := unix.IoctlGetInt(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		m.Close()
		t.Skip(err)
	}
	return m, "/dev/pts/" + strconv.Itoa(n)
}

// The watch in a terminal that's closed: the terminal goes (the master side shuts),
// SIGHUP comes, and writing to the terminal fails. That's a hangup, the usual way a
// watch in a terminal ends, and it exits 0, as SIGHUP is documented to; not 3.
func TestAgentsWatchTerminalClosed(t *testing.T) {
	dir := t.TempDir()
	fakeSSHScript(t, `echo '[]'`)
	cfgFile := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(cfgFile, []byte("version: 1\nhosts:\n- {name: a, address: 192.0.2.1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			master, slavePath := openPty(t)
			slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
			if err != nil {
				master.Close()
				t.Skip(err)
			}
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), runAgentsWatchEnv+"=a\x1f--watch\x1f--interval\x1f15s\x1f--format\x1f"+format,
				"METERM_CONFIG="+cfgFile, "TMPDIR="+t.TempDir())
			cmd.Stdout = slave
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			slave.Close()
			buf := make([]byte, 256)
			if _, err := master.Read(buf); err != nil { // it's writing to the terminal
				t.Fatal(err)
			}
			time.Sleep(1500 * time.Millisecond) // its first look done
			master.Close()
			_ = cmd.Process.Signal(syscall.SIGHUP)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				var exit *exec.ExitError
				if err != nil && errors.As(err, &exit) {
					t.Errorf("exit %d, stderr %q", exit.ExitCode(), stderr.String())
				} else if err != nil {
					t.Errorf("%v", err)
				}
				if strings.Contains(stderr.String(), "stdout") {
					t.Errorf("stderr: %q", stderr.String())
				}
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				<-done
				t.Fatal("didn't stop")
			}
		})
	}
}
