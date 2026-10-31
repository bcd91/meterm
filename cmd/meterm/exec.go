package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
)

// fromTUIEnv is set by the TUI on every command it dispatches, so the child
// process knows to pause for a keypress before the TUI redraws over its
// output instead of it flashing by unread.
const fromTUIEnv = "METERM_FROM_TUI"

// finish ends the process with code, pausing first if we were launched by
// the TUI's command dispatcher.
func finish(code int) {
	if os.Getenv(fromTUIEnv) != "" {
		fmt.Print("\n[meterm] press enter to return... ")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	os.Exit(code)
}

// runInherited runs c (stdio already wired to the parent) and translates a
// nonzero remote/tool exit into this process's own exit code, so `meterm`
// behaves like the underlying ssh/scp/sftp/rsync command when scripted.
func runInherited(c *exec.Cmd) error {
	if err := c.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			finish(exitErr.ExitCode())
		}
		return err
	}
	return nil
}
