package main

import (
	"os"
	"os/exec"

	"github.com/mattn/go-isatty"

	"meterm/internal/termsafe"
)

// isTerminal says whether f is a terminal, or something a terminal could be behind
// on Windows.
func isTerminal(f *os.File) bool {
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// guardOutput keeps escape sequences in what c prints from reaching this terminal:
// a log line shouldn't be able to set the clipboard or move the cursor. Pipes get the
// bytes as they are, and so does anyone who asked with raw. An interactive session
// can't go through this: full-screen programs are made of those sequences.
func guardOutput(c *exec.Cmd, raw bool) {
	if raw {
		return
	}
	if c.Stdout == os.Stdout && isTerminal(os.Stdout) {
		c.Stdout = termsafe.New(os.Stdout)
	}
	if c.Stderr == os.Stderr && isTerminal(os.Stderr) {
		c.Stderr = termsafe.New(os.Stderr)
	}
}

// safeText is s as it may be shown on f.
func safeText(s string, f *os.File, raw bool) string {
	if raw || !isTerminal(f) {
		return s
	}
	return termsafe.String(s)
}
