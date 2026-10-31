package termsafe

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// What follows is for one piece of text rather than a stream: a name or a path on its
// way into a table, a status bar or JSON. It's stricter than Writer, which has to let
// colours and line structure through for a log to read properly. Text keeps no escape
// sequence at all, and also takes out what looks fine and isn't: bidi controls and
// other format characters, and bytes that aren't UTF-8.

// Replacement stands in for what was taken out, so that it shows something was
// there: two paths that differ only by a hidden character must not look alike.
const Replacement = '�'

// unsafeRune is true of C0 and C1 controls, format characters (bidi controls,
// zero-width ones, tag characters), line and paragraph separators, and
// noncharacters.
func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) ||
		(r >= 0xFDD0 && r <= 0xFDEF) || r&0xFFFE == 0xFFFE
}

// Text returns s on one line with everything unsafe replaced. Bytes that aren't
// UTF-8 are replaced too: a raw 0x9B is a CSI to some terminals.
func Text(s string) string { return clean(s, false) }

// Lines is Text for messages of several lines: only \n survives.
func Lines(s string) string { return clean(s, true) }

func clean(s string, lines bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n <= 1:
			b.WriteRune(Replacement)
		case lines && r == '\n':
			b.WriteByte('\n')
		case unsafeRune(r):
			b.WriteRune(Replacement)
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// Unsafe says whether Text would change s.
func Unsafe(s string) bool { return Text(s) != s }

// Clip cuts s to at most max runes, ending in … when it had to.
func Clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max-1]) + "…"
}
