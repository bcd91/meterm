package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"

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

// lbl is outside text (a host name, a group, a note, a teammate's email)
// made one safe line for the terminal: no escape sequence, control
// character or bidi override. Output from a host goes through termsafe's
// Writer instead, which keeps its colours.
func lbl(s string) string { return termsafe.Label(s) }

// exactName is a host name for a message that must say which host it was:
// as it is when it shows as itself, Go-quoted ("zw\u200bsp") when it holds
// a character that wouldn't show, so it can't pass for another host.
func exactName(s string) string {
	if termsafe.Label(s) == s {
		return s
	}
	return strconv.Quote(s)
}

// lblLines is lbl for each line of a multi-line text, keeping the lines.
func lblLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = termsafe.Label(l)
	}
	return strings.Join(lines, "\n")
}
