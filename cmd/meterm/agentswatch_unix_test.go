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
