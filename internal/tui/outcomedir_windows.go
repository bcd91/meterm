//go:build windows

package tui

import "os"

// On Windows a leftover directory isn't swept: who owns it and whether its
// PID still runs aren't checked here, so it's taken for in use.
func ownedByUs(os.FileInfo) bool { return false }

func processRunning(int) bool { return true }
