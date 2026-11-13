package fileman

import "testing"

// A file named `"pay\u200b1"` in ASCII doesn't show like pay<ZWSP>1.
func TestShownQuotesQuotesAndBackslashes(t *testing.T) {
	ascii, real := `"pay\u200b1"`, "pay\u200b1"
	if shown(ascii) == shown(real) {
		t.Fatalf("both show as %s", shown(real))
	}
	if shown(`say"hi"`) != `"say\"hi\""` || shown("plain.txt") != "plain.txt" {
		t.Errorf("shown: %s, %s", shown(`say"hi"`), shown("plain.txt"))
	}
	// A backslash alone can't pass for a quoted name: a Windows path stays.
	if shown(`C:\Users\x`) != `C:\Users\x` || shown(`pay\u200b1`) == shown(real) {
		t.Errorf("shown: %s, %s", shown(`C:\Users\x`), shown(`pay\u200b1`))
	}
}
