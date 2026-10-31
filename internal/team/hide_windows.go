//go:build windows

package team

import (
	"os/exec"
	"syscall"
)

// createNoWindow runs a console program without a console window of its own.
const createNoWindow = 0x08000000

// quiet keeps git from opening a console window: autosync runs detached,
// with no console, and Windows gives every console program it starts a new
// window — a black box flashing up on each background pull.
func quiet(c *exec.Cmd) *exec.Cmd {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow, HideWindow: true}
	return c
}
