package tui

import (
	"os"

	"github.com/muesli/termenv"

	"earlyterm"
)

// darkBackground says whether the terminal's background is dark, for the
// picker's adaptive colours. $COLORFGBG answers without asking. Otherwise
// the terminal is asked (OSC 11) the way Bubble Tea's init used to ask for
// every command: a terminal that never answers costs the picker termenv's
// fixed five seconds, as before. A shorter wait of our own would let a late
// answer arrive as keys in the picker ("rgb:…" has an f, which opens files).
func darkBackground() bool {
	if v := os.Getenv("COLORFGBG"); v != "" {
		return earlyterm.Dark(v)
	}
	return termenv.NewOutput(os.Stdout).HasDarkBackground()
}
