//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach makes c outlive this process and the terminal it runs in.
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
