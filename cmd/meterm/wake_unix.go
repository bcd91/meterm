//go:build !windows

package main

import (
	"os"
	"syscall"
)

// kickSignals are what a network-change hook can send to have connections redone
// now. A meterm that isn't listening for it, such as `meterm list`, ignores it.
func kickSignals() []os.Signal { return []os.Signal{syscall.SIGUSR1} }
