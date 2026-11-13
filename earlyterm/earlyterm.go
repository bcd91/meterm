// Package earlyterm decides the terminal's background before Bubble Tea is
// initialised. Bubble Tea's init asks the terminal (OSC 11) and waits up to
// five seconds for an answer, in every program that links it: a terminal that
// never answers made even `meterm list` take five seconds to start. Settling
// the answer first, from $COLORFGBG, means nothing is asked.
//
// It's a module of its own only for its import path: Go initialises packages
// in import-path order once their imports are ready, and "earlyterm" sorts
// before "github.com/charmbracelet/bubbletea", so this init runs first. Inside
// the meterm module ("meterm/...") it would run after. Only cmd/meterm
// imports it. The picker (internal/tui) still asks the terminal when
// $COLORFGBG doesn't say, with termenv's own five-second wait: a terminal
// that never answers OSC 11 makes the picker, and only the picker, start
// five seconds late. A shorter wait would let a late answer ("rgb:…") reach
// the picker as keys, and f in it opens files.
package earlyterm

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func init() {
	lipgloss.SetHasDarkBackground(Dark(os.Getenv("COLORFGBG")))
}

// Dark reads $COLORFGBG ("fg;bg", set by rxvt, Konsole and others).
// Background colours 7 and 9-15 are light; anything else, or nothing, is
// taken as dark.
func Dark(colorfgbg string) bool {
	parts := strings.Split(colorfgbg, ";")
	switch parts[len(parts)-1] {
	case "7", "9", "10", "11", "12", "13", "14", "15":
		return false
	}
	return true
}
