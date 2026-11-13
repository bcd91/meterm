package termsafe

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Invisible is the one answer, for Text and for Label alike, to whether r is a
// character that draws as nothing, or changes how the text around it reads,
// without showing itself: format characters (bidi controls, zero-width space,
// non-joiner and joiner, word joiner, soft hyphen, BOM, tag characters), line
// and paragraph separators, the rest of Default_Ignorable_Code_Point (the
// combining grapheme joiner, the Hangul fillers, the Khmer inherent vowels),
// the Mongolian vowel separator (Cf in some Unicode versions, not others), the
// blank braille pattern, and noncharacters. Two names that differ only by one
// of these look alike.
//
// Variation selectors are not in it: they change how the character before
// them is drawn, and an emoji in a name or note holds one ("⚠️"). Text, which
// is for tables and JSON, replaces them as well; Label, whose Drops decides
// what config refuses, keeps them.
func Invisible(r rune) bool {
	switch r {
	case 0x180e, 0x034f, 0x115f, 0x1160, 0x3164, 0xffa0, 0x17b4, 0x17b5, 0x2800:
		return true
	}
	return unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Other_Default_Ignorable_Code_Point) ||
		(r >= 0xFDD0 && r <= 0xFDEF) || r&0xFFFE == 0xFFFE
}

// Quote is s in double quotes, as strconv.Quote writes it, except that every
// Invisible character is spelled as a \u escape too: strconv.Quote spells
// U+2028 but prints U+3164 (a Hangul filler) as it is, which shows as
// nothing, so two names quoted that way could still look alike.
func Quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i, r := range s {
		switch {
		case r == utf8.RuneError:
			if _, n := utf8.DecodeRuneInString(s[i:]); n == 1 {
				fmt.Fprintf(&b, `\x%02x`, s[i])
				continue
			}
			b.WriteString(`�`)
		case Invisible(r) || !unicode.IsPrint(r) || r == '"' || r == '\\':
			if r < 0x80 {
				q := strconv.Quote(string(r)) // \n, \x01, \", \\
				b.WriteString(q[1 : len(q)-1])
			} else if r < 0x10000 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
