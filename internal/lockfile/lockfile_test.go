package lockfile

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// When holderEnv is set, the test binary is one process using the lock. See
// TestMain.
const holderEnv = "METERM_TEST_LOCK_HOLDER"

func TestMain(m *testing.M) {
	if spec := os.Getenv(holderEnv); spec != "" {
		mode, rest, _ := strings.Cut(spec, "|")
		if err := runHolder(mode, rest); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runHolder(mode, rest string) error {
	switch mode {
	case "hold": // take the lock, say so, and wait to be killed
		unlock, err := Acquire(rest, 10*time.Second, time.Millisecond)
		if err != nil {
			return err
		}
		defer unlock()
		fmt.Println("held")
		time.Sleep(time.Minute)
	case "log": // rounds of taking the lock and writing enter/leave to a log
		path, logPath, _ := strings.Cut(rest, "|")
		log, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer log.Close()
		for range 50 {
			unlock, err := Acquire(path, 30*time.Second, time.Millisecond)
			if err != nil {
				return err
			}
			fmt.Fprintf(log, "enter %d\n", os.Getpid())
			fmt.Fprintf(log, "leave %d\n", os.Getpid())
			unlock()
		}
	}
	return nil
}

func TestExcludesOtherHolders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "x.lock")
	var wg sync.WaitGroup
	var inside, total atomic.Int32 // atomic: the race detector can't see a lock file
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := Acquire(path, 10*time.Second, time.Millisecond)
			if err != nil {
				t.Error(err)
				return
			}
			defer unlock()
			if n := inside.Add(1); n != 1 {
				t.Errorf("%d holders at once", n)
			}
			time.Sleep(time.Millisecond)
			total.Add(1)
			inside.Add(-1)
		}()
	}
	wg.Wait()
	if n := total.Load(); n != 20 {
		t.Fatalf("got %d acquisitions, want 20", n)
	}
}

func TestBusyWithoutWaiting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := Acquire(path, 0, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path, 0, time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Acquire: %v, want ErrBusy", err)
	}
	unlock()
	again, err := Acquire(path, 0, time.Millisecond)
	if err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	again()
}

func TestWaitLimitIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := Acquire(path, 0, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	started := time.Now()
	if _, err := Acquire(path, 100*time.Millisecond, 5*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
	if took := time.Since(started); took < 100*time.Millisecond || took > 3*time.Second {
		t.Fatalf("waited %s for a 100ms limit", took)
	}
}

// A file with an old date, which is what a crash used to leave, means nothing:
// many at once find it, and they still take turns.
func TestOldFileBlocksNobodyAndStillExcludes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	for round := range 300 {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var inside atomic.Int32
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				unlock, err := Acquire(path, 5*time.Second, 100*time.Microsecond)
				if err != nil {
					t.Error(err)
					return
				}
				if n := inside.Add(1); n != 1 {
					t.Errorf("round %d: %d holders at once", round, n)
				}
				inside.Add(-1)
				unlock()
			}()
		}
		wg.Wait()
		unlock, err := Acquire(path, 0, time.Millisecond)
		if err != nil {
			t.Fatalf("round %d: free lock not free afterwards: %v", round, err)
		}
		unlock()
	}
}

func TestExcludesAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	path, logPath := filepath.Join(dir, "x.lock"), filepath.Join(dir, "log")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	const procs = 8
	cmds := make([]*exec.Cmd, procs)
	for i := range cmds {
		cmds[i] = exec.Command(os.Args[0])
		cmds[i].Env = append(os.Environ(), holderEnv+"=log|"+path+"|"+logPath)
		if err := cmds[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2*procs*50 {
		t.Fatalf("%d lines, want %d", len(lines), 2*procs*50)
	}
	for i := 0; i < len(lines); i += 2 {
		enter, _ := strings.CutPrefix(lines[i], "enter ")
		leave, _ := strings.CutPrefix(lines[i+1], "leave ")
		if enter != leave || strings.HasPrefix(enter, "leave") {
			t.Fatalf("line %d: %q then %q: two holders at once", i, lines[i], lines[i+1])
		}
	}
}

// A holder that's killed gives the lock up with it.
func TestKilledHolderReleasesTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	c := exec.Command(os.Args[0])
	c.Env = append(os.Environ(), holderEnv+"=hold|"+path)
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Process.Kill()
	if line, _ := bufio.NewReader(out).ReadString('\n'); strings.TrimSpace(line) != "held" {
		t.Fatalf("holder said %q", line)
	}
	if _, err := Acquire(path, 0, time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("while held: %v, want ErrBusy", err)
	}
	if err := c.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = c.Wait()
	unlock, err := Acquire(path, 5*time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("after the kill: %v", err)
	}
	unlock()
}

func TestUnlockTwiceIsHarmless(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	first, err := Acquire(path, 0, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	first()
	second, err := Acquire(path, 0, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	first()
	if _, err := Acquire(path, 0, time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second unlock freed the next holder's lock: %v", err)
	}
}

// A lock that can't be opened is an error at once, not a wait or a spin.
func TestUnusableLockFailsAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := Acquire(path, time.Minute, time.Millisecond); err == nil || errors.Is(err, ErrBusy) {
		t.Fatalf("got %v, want the open error", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("took %s", took)
	}
}
