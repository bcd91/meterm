//go:build !windows

package team

import "os/exec"

// quiet is a no-op off Windows (see hide_windows.go).
func quiet(c *exec.Cmd) *exec.Cmd { return c }
