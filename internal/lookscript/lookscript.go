// Package lookscript is what the looks that send a script to a host share: how the
// command starts, and how to tell from what came back whether the script ran at all.
package lookscript

import (
	"bytes"
	"strings"
)

// Start is what a look's command begins with, before its script and a closing `'`.
// The login shell echoes Marker first, which every login shell does, cmd's and
// PowerShell 7's included. Then sh echoes ShMarker before anything else. A look needs
// both, because a login shell that isn't sh still reads the rest of the line: cmd runs
// `|| exit 127` itself when it can't find sh, and that exit would read as the script's.
//
// It's one line with no `!`, so that csh and fish read it the same way.
const Start = `echo ` + Marker + ` && sh -c 'echo ` + ShMarker + `; `

// Marker is what the login shell prints. cmd's echo keeps the space before &&.
const Marker = "meterm-look"

// ShMarker is what sh prints first, once it's running the script.
const ShMarker = "meterm-sh"

// Output is what a look's stdout says about how far the command got.
type Output struct {
	Reached bool   // the login shell ran: Marker came, on a line of its own
	Ran     bool   // sh ran the script: ShMarker came
	Body    []byte // what the script printed after ShMarker; nil when it didn't run
}

// Read splits a look's stdout. What a startup file printed before the markers is
// dropped. A Marker line counts with a startup file's text before it, since that text
// can end with no newline.
func Read(stdout []byte) Output {
	var out Output
	rest := stdout
	for len(rest) > 0 {
		line := rest
		next := []byte(nil)
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, next = rest[:i], rest[i+1:]
		}
		rest = next
		text := strings.TrimSpace(string(line))
		switch {
		case text == ShMarker:
			out.Ran = true
			out.Body = rest
			if out.Body == nil {
				out.Body = []byte{}
			}
			return out
		case strings.HasSuffix(text, Marker):
			out.Reached = true
		}
	}
	return out
}
