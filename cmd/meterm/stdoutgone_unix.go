//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// stdoutGone says whether stdout is a pipe or socket whose reader has gone: poll
// reports POLLERR or POLLHUP on it without anything being written. A terminal or a
// file never says so.
func stdoutGone() bool {
	fds := []unix.PollFd{{Fd: int32(os.Stdout.Fd())}}
	n, err := unix.Poll(fds, 0)
	return err == nil && n > 0 && fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0
}
