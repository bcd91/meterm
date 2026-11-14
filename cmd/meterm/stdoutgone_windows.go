//go:build windows

package main

import "os"

// stdoutGone is false on Windows, where there's no poll for it; `agents` doesn't run on
// Windows anyway, and a closed pipe there is noticed at the next write.
func stdoutGone(*os.File) bool { return false }

// stdoutUnusable is "" on Windows; `agents` doesn't run there.
func stdoutUnusable(*os.File) string { return "" }
