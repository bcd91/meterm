package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// stdoutGone says whether stdout is a pipe or socket whose reader has gone. macOS's
// poll doesn't say so for a pipe; kqueue does: an EVFILT_WRITE event on it carries
// EV_EOF once the reader has closed its end (kqueue(2): "the filter will set EV_EOF
// when the reader disconnects"). Asked with no wait, once per call, nothing written.
// A terminal or a file never says so.
func stdoutGone(f *os.File) bool {
	kq, err := unix.Kqueue()
	if err != nil {
		return false
	}
	defer unix.Close(kq)
	change := []unix.Kevent_t{{}}
	unix.SetKevent(&change[0], int(f.Fd()), unix.EVFILT_WRITE, unix.EV_ADD|unix.EV_ONESHOT)
	events := make([]unix.Kevent_t, 1)
	n, err := unix.Kevent(kq, change, events, &unix.Timespec{})
	if err != nil || n < 1 {
		return false
	}
	return events[0].Flags&unix.EV_EOF != 0
}
