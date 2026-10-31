package sshrun

import (
	"os"
	"strings"
)

// Newer terminals set TERM to a name of their own (xterm-ghostty,
// xterm-kitty, …) whose terminfo most servers don't have yet, so
// full-screen programs there fail: "Error opening terminal: xterm-ghostty".
// The terminals' own fixes (Ghostty's ssh-env, kitty's ssh kitten) wrap the
// ssh typed in a shell, which meterm doesn't go through, so an interactive
// session sends xterm-256color instead — understood everywhere, and what
// those terminals emulate anyway. METERM_TERM=keep sends TERM unchanged;
// METERM_TERM=<name> sends that name.

var uncommonTerms = []string{"xterm-ghostty", "ghostty", "xterm-kitty", "wezterm", "alacritty", "foot", "foot-extra", "rio", "contour", "xterm-direct"}

// RemoteTerm is the TERM to send for an interactive session when the local
// one is term, and whether it differs.
func RemoteTerm(term string) (string, bool) {
	switch v := os.Getenv("METERM_TERM"); v {
	case "keep":
		return term, false
	case "":
	default:
		return v, v != term
	}
	for _, t := range uncommonTerms {
		if strings.EqualFold(term, t) {
			return "xterm-256color", true
		}
	}
	return term, false
}

// withRemoteTerm sets the session's TERM per RemoteTerm, leaving the rest
// of the environment as it is.
func withRemoteTerm(env []string) []string {
	cur := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "TERM=") {
			cur = kv[len("TERM="):]
		}
	}
	want, change := RemoteTerm(cur)
	if !change {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "TERM=") {
			out = append(out, kv)
		}
	}
	return append(out, "TERM="+want)
}
