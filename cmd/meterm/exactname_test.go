package main

import "testing"

// A host named `"pay\u200b1"` in ASCII doesn't show like pay<ZWSP>1, in
// messages or in the picker.
func TestExactNameQuotesQuotesAndBackslashes(t *testing.T) {
	ascii, real := `"pay\u200b1"`, "pay\u200b1"
	if exactName(ascii) == exactName(real) {
		t.Fatalf("both show as %s", exactName(real))
	}
	if exactName("pay1") != "pay1" {
		t.Errorf("plain name quoted: %s", exactName("pay1"))
	}
}
