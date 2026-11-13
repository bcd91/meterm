package tui

import (
	"testing"

	"earlyterm"
)

// $COLORFGBG decides without asking the terminal anything.
func TestDarkBackgroundFromColorFGBG(t *testing.T) {
	for v, dark := range map[string]bool{"15;0": true, "0;15": false, "0;7": false, "7;8": true, "default;default": true} {
		t.Setenv("COLORFGBG", v)
		if got := darkBackground(); got != dark {
			t.Errorf("COLORFGBG=%q: dark %v, want %v", v, got, dark)
		}
	}
}

// earlyterm.Dark, which decides the background for every command: light
// backgrounds are 7 and 9-15; anything else, or nothing, is dark. (earlyterm
// is a module of its own, so its test lives here, where go test ./... runs it.)
func TestEarlytermDark(t *testing.T) {
	for v, dark := range map[string]bool{
		"": true, "15;0": true, "7;8": true, "default;default": true, "0;16": true, "0;1": true, "0;8": true,
		"0;7": false, "0;9": false, "0;10": false, "0;11": false, "0;12": false, "0;13": false, "0;14": false, "0;15": false,
		"15": false, "0;default;15": false, "0;15;0": true,
	} {
		if got := earlyterm.Dark(v); got != dark {
			t.Errorf("Dark(%q) = %v, want %v", v, got, dark)
		}
	}
}
