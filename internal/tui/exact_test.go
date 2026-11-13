package tui

import "testing"

func TestShownNameQuotesQuotesAndBackslashes(t *testing.T) {
	ascii, real := `"pay\u200b1"`, "pay\u200b1"
	if shownName(ascii) == shownName(real) {
		t.Fatalf("both show as %s", shownName(real))
	}
	if shownName("a\u3164b") == "a\u3164b" || shownName("pay1") != "pay1" {
		t.Errorf("shownName: %+q, %s", shownName("a\u3164b"), shownName("pay1"))
	}
}
