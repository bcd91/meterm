package termsafe

import "testing"

func TestLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"web1", "web1"},
		{"เว็บ-หลัก 🚀", "เว็บ-หลัก 🚀"},
		{"evil\x1b]0;pwned\x07title", "eviltitle"},
		{"evil\x1b]52;c;ZXZpbA==\x1b\\x", "evilx"},
		{"grp\x1b[5m", "grp"},
		{"\x1b[38;5;203mred\x1b[0m", "red"},
		{"a\x1b(Bb", "ab"},
		{"a\u009b31mb", "ab"},
		{"a\u009d0;t\u009cb", "ab"},
		{"two\nlines\r\tx", "two lines  x"},
		{"bidi\u202egnp.exe", "bidignp.exe"},
		{"iso\u2066x\u2069", "isox"},
		{"bell\x07del\x7f", "belldel"},
		{"cut\x1b]0;never ends", "cut"},
		{"trail\x1b", "trail"},
		{"csi\x1b[12", "csi"},
	}
	for _, c := range cases {
		if got := Label(c.in); got != c.want {
			t.Errorf("Label(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLabelLeavesNoControl(t *testing.T) {
	in := "\x1b\x1b[\x1b]\x9b\x9d\x00\x1f\u202e\u2066\x1bP\x07z"
	for _, r := range Label(in) {
		if dropRune(r) || r == 0x1b {
			t.Fatalf("Label(%q) kept %U", in, r)
		}
	}
}

// Invisible characters go too: a zero-width space or a soft hyphen in a
// name makes two names that look the same differ. So does a zero-width
// joiner, even inside an emoji sequence: terminals disagree on how wide a
// joined picture is (tmux 3.5a draws a family of four as 4 cells, wcwidth says 8, a
// grapheme-aware terminal 2), so the emoji are drawn one by one.
func TestLabelDropsInvisibles(t *testing.T) {
	family := "\U0001F468\u200d\U0001F469\u200d\U0001F467"
	for in, want := range map[string]string{
		"web\u200b1":            "web1",
		"web\u200c1":            "web1",
		"web\u200d1":            "web1",
		"web\u20601":            "web1",
		"web\ufeff1":            "web1",
		"web\u00ad1":            "web1",
		"web\u200e1":            "web1",
		"web\u180e1":            "web1",
		"web\U000E00011":        "web1", // a language tag
		"web\u034f1":            "web1", // combining grapheme joiner
		"web\u31641":            "web1", // Hangul filler, drawn blank
		family:                  "\U0001F468\U0001F469\U0001F467",
		"a" + family + "\u200d": "a\U0001F468\U0001F469\U0001F467",
		"\u200d\U0001F469":      "\U0001F469",
		"\U0001F44D\U0001F3FD":  "\U0001F44D\U0001F3FD", // a skin tone isn't a joiner
		"\u2764\ufe0f":          "\u2764\ufe0f",         // nor is VS16
	} {
		if got := Label(in); got != want {
			t.Errorf("Label(%q) = %q, want %q", in, got, want)
		}
		for _, r := range Label(in) {
			if Drops(r) {
				t.Errorf("Label(%q) kept %U, which Drops says goes", in, r)
			}
		}
	}
	for _, r := range []rune{0x200b, 0x200d, 0xad, 0xfeff, 0x200e, 0x202e, 0x2066, 0x9b, 0x1b, 0x7f, 0x2028, '\n'} {
		if !Drops(r) {
			t.Errorf("Drops(%U) = false", r)
		}
	}
	for _, r := range []rune{'a', ' ', 0x0e33, 0xfe0f, 0x1f3fd, 0x1f468} {
		if Drops(r) {
			t.Errorf("Drops(%U) = true", r)
		}
	}
}

// Quote spells every invisible character, not only those strconv.Quote
// doesn't print: U+3164 and U+2028 alike. Thai stays as it is.
func TestQuoteSpellsEveryInvisible(t *testing.T) {
	for in, want := range map[string]string{
		"web1":                   `"web1"`,
		"web\u31641":             `"web\u31641"`,
		"web\u20281":             `"web\u20281"`,
		"a\u200bb":               `"a\u200bb"`,
		"a\x1b[31mb":             `"a\x1b[31mb"`,
		"tab\there":              `"tab\there"`,
		`q"b\s`:                  `"q\"b\\s"`,
		"\u0e19\u0e49\u0e33-web": "\"\u0e19\u0e49\u0e33-web\"",
		"bad\xffutf8":            `"bad\xffutf8"`,
		"tag\U000e0041x":         `"tag\U000e0041x"`,
		"it's":                   `"it's"`,
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}
