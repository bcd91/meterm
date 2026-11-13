package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
)

// fromTUIEnv is set by the TUI on every command it dispatches, so the child
// process knows to pause for a keypress before the TUI redraws over its
// output instead of it flashing by unread. It never skips a production
// question: that takes the picker's one-use token (internal/prodask).
const fromTUIEnv = "METERM_FROM_TUI"

// quietOnSuccess is set by commands that hand the terminal to something
// you use and then leave (an ssh session, the sftp prompt, the file
// manager, the add and edit wizards): when one ends cleanly there's nothing
// left to read, so the picker comes straight back. Every other command
// prints something to read (audit, logs, route check, exec), and pauses
// either way.
var quietOnSuccess bool

// pausePrompt is what finish says before waiting for enter, or "" when it
// shouldn't wait: not started from the picker, or a quiet command that
// ended cleanly. A failure always waits, and says how it ended.
func pausePrompt(code int, fromTUI, quiet bool) string {
	switch {
	case !fromTUI:
		return ""
	case code != 0:
		return fmt.Sprintf("\n[meterm] exit status %d: press enter to return... ", code)
	case quiet:
		return ""
	}
	return "\n[meterm] press enter to return... "
}

// finish ends the process with code, pausing first if we were launched by
// the TUI's command dispatcher and there's something to read.
func finish(code int) {
	p := pausePrompt(code, os.Getenv(fromTUIEnv) != "", quietOnSuccess)
	if p != "" && sessionEnded && code != 0 {
		p = sessionPausePrompt(code)
	}
	if p != "" {
		fmt.Print(p)
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
	osExit(code)
}

// osExit is os.Exit; tests replace it.
var osExit = os.Exit

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

// sessionEnded is set by connect: the process ends with the code of a
// session that ran (or ssh's own 255), so the pause words it that way.
var sessionEnded bool

// sessionPausePrompt is the pause after a session started from the picker.
// A non-zero code other than 255 is the remote shell's last command: the
// session ran and ended, nothing failed. 255 is ssh's own: the connection
// was lost or never made.
func sessionPausePrompt(code int) string {
	if code == 255 {
		return "\n[meterm] ssh exit 255 (connection lost, or never made): press enter to return... "
	}
	return fmt.Sprintf("\n[meterm] session ended, exit %d: press enter to return... ", code)
}
