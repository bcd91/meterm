package termsafe

import "testing"

// A name typed in plain ASCII as `"pay\u200b1"` (quotes, a backslash) must
// not show like the quoted pay<ZWSP>1: Exact quotes it too.
func TestExactTellsQuotedLookalikeApart(t *testing.T) {
	ascii := `"pay\u200b1"`
	real := "pay\u200b1"
	if Exact(ascii) == Exact(real) {
		t.Fatalf("both show as %s", Exact(real))
	}
	for s, want := range map[string]string{
		"pay1":       "pay1",
		real:         `"pay\u200b1"`,
		ascii:        `"\"pay\\u200b1\""`,
		`a\b`:        `"a\\b"`,
		`say"hi"`:    `"say\"hi\""`,
		"a\u3164b":   `"a\u3164b"`,
		"web-\u0e44": "web-\u0e44",
	} {
		if got := Exact(s); got != want {
			t.Errorf("Exact(%+q) = %s, want %s", s, got, want)
		}
	}
}
