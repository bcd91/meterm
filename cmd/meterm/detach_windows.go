//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// detach makes c outlive this process and its console, without flashing a
// window.
func detach(c *exec.Cmd) {
	const detachedProcess, newProcessGroup = 0x00000008, 0x00000200
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | newProcessGroup, HideWindow: true}
}
