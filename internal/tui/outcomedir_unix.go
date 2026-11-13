//go:build !windows

package tui

import (
	"errors"
	"os"
	"syscall"
)

func ownedByUs(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// processRunning says whether pid names a process: signal 0 checks, and
// sends nothing. EPERM is a process of someone else's, still running.
func processRunning(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
