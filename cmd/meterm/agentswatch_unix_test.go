//go:build !windows

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
)

// A reader that goes away mid-run, with looks at several hosts in flight: the watch
// stops them all, removes their files, says why on stderr and exits 3. Not SIGPIPE.
func TestAgentsWatchProcessStopsWhenStdoutCloses(t *testing.T) {
	dir := t.TempDir()
	pids := filepath.Join(dir, "pids")
	fakeSSHScript(t, `for a; do last2=$prev; prev=$a; done
if [ "$last2" = 192.0.2.1 ]; then sleep 2; echo '[]'; exit 0; fi
echo $$ >>'`+pids+`'
exec sleep 30`)
	cfgFile := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(cfgFile, []byte("version: 1\nhosts:\n- {name: fast, address: 192.0.2.1}\n- {name: slow1, address: 192.0.2.2}\n- {name: slow2, address: 192.0.2.3}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), runAgentsWatchEnv+"=fast\x1fslow1\x1fslow2\x1f--watch\x1f--interval\x1f15s\x1f--timeout\x1f14s\x1f--format\x1fjson",
		"METERM_CONFIG="+cfgFile, "TMPDIR="+tmp)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 1)
	if _, err := stdout.Read(first); err != nil { // the start line has begun
		t.Fatal(err)
	}
	// Both slow looks under way, then the reader goes.
	for i := 0; ; i++ {
		b, _ := os.ReadFile(pids)
		if len(strings.Fields(string(b))) == 2 {
			break
		}
		if i > 100 {
			t.Fatal("the slow looks didn't start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	stdout.Close()
	closed := time.Now()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var werr error
	select {
	case werr = <-done:
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("didn't stop")
	}
	// The fast host's look comes after about 2s; the slow ones would run to their
	// 14s timeout if nothing stopped them.
	if d := time.Since(closed); d > 8*time.Second {
		t.Errorf("took %s to stop", d)
	}
	var exit *exec.ExitError
	if !errors.As(werr, &exit) || exit.ExitCode() != agentsWatchOutputGone {
		t.Errorf("exit: %v (stderr %q)", werr, stderr.String())
	}
	if !strings.Contains(stderr.String(), "stdout was closed") {
		t.Errorf("stderr: %q", stderr.String())
	}
	b, _ := os.ReadFile(pids)
	started := strings.Fields(string(b))
	if len(started) != 2 {
		t.Errorf("%d slow looks were in flight, want 2", len(started))
	}
	time.Sleep(200 * time.Millisecond)
	for _, p := range started {
		if n, _ := strconv.Atoi(p); n > 0 && syscall.Kill(n, 0) == nil {
			t.Errorf("ssh %s still running", p)
			_ = syscall.Kill(n, syscall.SIGKILL)
		}
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("left in the temp directory: %v", left)
	}
}

// As `meterm agents --watch | head -1` would: the reader takes one line and goes. In
// text form nothing more would be written while nothing changes, and the watch still
// stops within a few seconds, exit 3, without logging in again and again.
func TestAgentsWatchProcessHeadOne(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			runs := filepath.Join(dir, "runs")
			fakeSSHScript(t, `echo x >>'`+runs+`'; echo '[]'`)
			cfgFile := filepath.Join(dir, "hosts.yaml")
			if err := os.WriteFile(cfgFile, []byte("version: 1\nhosts:\n- {name: a, address: 192.0.2.1}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), runAgentsWatchEnv+"=a\x1f--watch\x1f--interval\x1f15s\x1f--format\x1f"+format,
				"METERM_CONFIG="+cfgFile, "TMPDIR="+t.TempDir())
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// One line, then wait for the first look to have happened, then go.
			buf := make([]byte, 1)
			for {
				if _, err := stdout.Read(buf); err != nil || buf[0] == '\n' {
					break
				}
			}
			for i := 0; i < 100; i++ {
				if b, _ := os.ReadFile(runs); len(b) > 0 {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			stdout.Close()
			closed := time.Now()
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			var werr error
			select {
			case werr = <-done:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("still running 10s after its reader went")
			}
			var exit *exec.ExitError
			if !errors.As(werr, &exit) || exit.ExitCode() != agentsWatchOutputGone || !strings.Contains(stderr.String(), "stdout was closed") {
				t.Errorf("exit %v, stderr %q", werr, stderr.String())
			}
			if d := time.Since(closed); d > 4*time.Second {
				t.Errorf("took %s", d)
			}
			if b, _ := os.ReadFile(runs); strings.Count(string(b), "\n") > 1 {
				t.Errorf("%d logins", strings.Count(string(b), "\n"))
			}
		})
	}
}

// Started with nowhere to write: stdout closed (>&-), /dev/null, or read-only. It
// refuses at once, exit 3, without logging in to anything.
func TestAgentsWatchRefusesStdoutNowhere(t *testing.T) {
	dir := t.TempDir()
	argvFile := fakeSSHScript(t, `echo '[]'`)
	cfgFile := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(cfgFile, []byte("version: 1\nhosts:\n- {name: a, address: 192.0.2.1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := runAgentsWatchEnv + "=a\x1f--watch\x1f--interval\x1f15s"
	ro := filepath.Join(dir, "ro")
	if err := os.WriteFile(ro, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, redirect := range map[string]string{"closed": ">&-", "dev null": ">/dev/null", "read-only": "1<" + ro} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", `exec "$0" `+redirect, os.Args[0])
			cmd.Env = append(os.Environ(), args, "METERM_CONFIG="+cfgFile, "TMPDIR="+t.TempDir())
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			done := make(chan error, 1)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			go func() { done <- cmd.Wait() }()
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatal("it started watching")
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != agentsWatchOutputGone || !strings.Contains(stderr.String(), "didn't start") {
				t.Errorf("exit %v, stderr %q", err, stderr.String())
			}
		})
	}
	if _, err := os.Stat(argvFile); !os.IsNotExist(err) {
		t.Error("ssh ran")
	}
}

// The real command with a reader that holds the pipe and never reads, as a seat that
// hung would: 300 hosts fill it, and SIGTERM still ends the watch in seconds.
func TestAgentsWatchProcessStopsWithAReaderThatNeverReads(t *testing.T) {
	dir := t.TempDir()
	fakeSSHScript(t, `echo '[]'`)
	var b strings.Builder
	b.WriteString("version: 1\nhosts:\n")
	for i := 0; i < 300; i++ {
		n := "h" + strconv.Itoa(i)
		b.WriteString("- {name: " + n + ", address: " + n + ".invalid, tags: [many]}\n")
	}
	cfgFile := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(cfgFile, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close() // held open, never read
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), runAgentsWatchEnv+"=--tag\x1fmany\x1f--watch\x1f--interval\x1f15s\x1f-j\x1f32\x1f--format\x1fjson",
		"METERM_CONFIG="+cfgFile, "TMPDIR="+t.TempDir())
	cmd.Stdout = pw
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	time.Sleep(4 * time.Second) // every host looked at once: far more than the pipe holds
	start := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != agentsWatchOutputGone || !strings.Contains(stderr.String(), "wasn't being read") {
			t.Errorf("exit %v, stderr %q", err, stderr.String())
		}
		if d := time.Since(start); d > 6*time.Second {
			t.Errorf("took %s", d)
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("SIGTERM didn't stop it")
	}
}
