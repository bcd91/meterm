package main

import (
	"os/exec"
	"syscall"
)

// dieWithTest has the kernel kill c when the test binary goes, however it goes.
func dieWithTest(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
