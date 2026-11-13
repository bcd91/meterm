//go:build !windows

package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A picker killed, or whose terminal closed, leaves its outcome directory
// behind; the next picker removes it once the process that made it is gone,
// and leaves everything else alone.
func TestLeftoverOutcomeDirsAreSwept(t *testing.T) {
	tmp := t.TempDir()
	// A process that has ended: its PID names no one (here) any more.
	done := exec.Command(os.Args[0], "-test.run=^$")
	if err := done.Run(); err != nil {
		t.Fatal(err)
	}
	dead := done.Process.Pid
	// One that is running, and stopped again below.
	live := exec.Command("sleep", "30")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Process.Kill(); _ = live.Wait() })

	mk := func(name string, files ...string) string {
		p := filepath.Join(tmp, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(p, f), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	gone := []string{
		mk(fmt.Sprintf("%s%d-a1", outcomePrefix, dead)),
		mk(fmt.Sprintf("%s%d-b2", outcomePrefix, dead), "session", "prod-0123456789abcdef"),
	}
	kept := []string{
		mk(fmt.Sprintf("%s%d-c3", outcomePrefix, dead), "session", "notes.txt"), // something not a picker's
		mk(fmt.Sprintf("%s%d-d4", outcomePrefix, os.Getpid())),
		mk(fmt.Sprintf("%s%d-e5", outcomePrefix, live.Process.Pid)),
		mk(outcomePrefix + "123abc"), // the old name, with no PID in it
		mk(fmt.Sprintf("other-%d-f6", dead)),
	}
	link := filepath.Join(tmp, fmt.Sprintf("%s%d-g7", outcomePrefix, dead))
	target := mk("target")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	kept = append(kept, link, target)

	sweepOutcomeDirs(tmp)
	for _, p := range gone {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s left: %v", filepath.Base(p), err)
		}
	}
	for _, p := range kept {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s removed: %v", filepath.Base(p), err)
		}
	}
	if es, _ := os.ReadDir(kept[0]); len(es) != 2 {
		t.Errorf("emptied a directory it then couldn't remove: %v", es)
	}

	d, err := newOutcomeDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Base(d), fmt.Sprintf("%s%d-", outcomePrefix, os.Getpid())) {
		t.Errorf("new outcome directory %s doesn't carry this PID", d)
	}
}
