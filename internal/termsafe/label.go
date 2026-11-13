package termsafe

import (
	"strings"
)

// Label makes outside text (a host name, a group, a tag, a remote file name)
// safe to show as one line of a TUI. Unlike String, it keeps no escape
// sequence at all, colour included: a label is drawn inside the TUI's own
// styling, and a stray SGR would bleed into it. Whole sequences are dropped,
// not just their ESC byte, so "\x1b]0;title\x07" leaves nothing behind.
// Line breaks and tabs become spaces, so a label can't add rows. Other
// control characters, bidi overrides and isolates (which can make
// "gnp.exe" read as "exe.png") are dropped, and so are invisible ones (a
// zero-width space, a soft hyphen, every other format character), which
// make two names that look alike differ. That includes the zero-width
// joiner inside an emoji sequence (👨‍👩‍👧): terminals disagree on how wide
// the joined picture is (tmux 3.5a gives a family of four 4 cells, wcwidth
// 8, a grapheme-aware terminal 2), so a row with one ran past its border.
// Without the joiner each emoji is drawn, and counted, on its own.
//
// Drops says which characters go; config refuses them in names and notes.
func Label(s string) string {
	if isPlain(s) {
		return s
	}
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == 0x1b || r == 0x9b || r == 0x9d || r == 0x90 || r == 0x98 || r == 0x9e || r == 0x9f:
			i = skipSequence(rs, i)
		case r == '\n' || r == '\r' || r == '\t' || r == '\v' || r == '\f' || r == 0x2028 || r == 0x2029:
			b.WriteByte(' ')
		case dropRune(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isPlain(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

// Drops says Label leaves r out of what it shows, or shows it as a space:
// control characters (C0, DEL, C1), line and paragraph separators, format
// characters (bidi controls, zero-width space, non-joiner and joiner, word
// joiner, soft hyphen, BOM, tags…) and the rest of what Invisible says draws
// as nothing.
func Drops(r rune) bool {
	return r < 0x20 || r == 0x2028 || r == 0x2029 || dropRune(r)
}

func dropRune(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || Invisible(r)
}

// skipSequence returns the index of the last rune of the escape sequence
// that starts at i. An unterminated sequence runs to the end of the text.
func skipSequence(rs []rune, i int) int {
	r := rs[i]
	kind := r
	if r == 0x1b {
		if i+1 >= len(rs) {
			return i
		}
		switch n := rs[i+1]; n {
		case '[':
			kind, i = 0x9b, i+1
		case ']', 'P', 'X', '^', '_':
			kind, i = 0x9d, i+1
		default:
			// ESC with intermediates (0x20-0x2f), then one final byte.
			j := i + 1
			for j < len(rs) && rs[j] >= 0x20 && rs[j] <= 0x2f {
				j++
			}
			if j >= len(rs) {
				return len(rs) - 1
			}
			return j
		}
	}
	if kind == 0x9b { // CSI: parameters and intermediates, then a final byte 0x40-0x7e
		for j := i + 1; j < len(rs); j++ {
			if rs[j] >= 0x40 && rs[j] <= 0x7e {
				return j
			}
			if rs[j] < 0x20 {
				return j - 1
			}
		}
		return len(rs) - 1
	}
	// A string (OSC, DCS, SOS, PM, APC): up to BEL, ST (ESC \) or C1 ST.
	for j := i + 1; j < len(rs); j++ {
		switch {
		case rs[j] == 0x07 || rs[j] == 0x9c:
			return j
		case rs[j] == 0x1b && j+1 < len(rs) && rs[j+1] == '\\':
			return j + 1
		}
	}
	return len(rs) - 1
}
