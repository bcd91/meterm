//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// stdoutUnusable says why a watch can't write to stdout from the start, or "": it was
// closed (Go then puts /dev/null there), it is /dev/null, or it was opened read-only.
// A watch writing nowhere would log in to its hosts for ever for nobody.
func stdoutUnusable(f *os.File) string {
	fd := int(f.Fd())
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return "stdout is closed"
	}
	if flags&unix.O_ACCMODE == unix.O_RDONLY {
		return "stdout isn't open for writing"
	}
	var out, null unix.Stat_t
	if unix.Fstat(fd, &out) == nil && unix.Stat("/dev/null", &null) == nil && out.Dev == null.Dev && out.Ino == null.Ino {
		return "stdout is closed or /dev/null"
	}
	return ""
}
