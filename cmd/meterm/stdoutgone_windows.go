//go:build windows

package main

// stdoutGone is false on Windows, where there's no poll for it; `agents` doesn't run on
// Windows anyway, and a closed pipe there is noticed at the next write.
func stdoutGone() bool { return false }
