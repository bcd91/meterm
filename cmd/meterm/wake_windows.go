//go:build windows

package main

import "os"

// kickSignals is empty: Windows has no signal to send a running meterm.
func kickSignals() []os.Signal { return nil }
