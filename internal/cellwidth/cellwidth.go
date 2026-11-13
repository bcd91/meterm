// Package cellwidth measures and cuts plain text by terminal columns, the way
// wcwidth-based terminals count them. Both TUIs (the host picker and the
// file manager) use it, so a Thai or emoji name lines up the same in each.
package cellwidth

import (
	"strings"
	"unicode"

	"github.com/mattn/go-runewidth"
)

// Width is how many columns a terminal gives plain text (no escapes),
// counted rune by rune the way wcwidth-based terminals (tmux, xterm, foot,
// the Linux console) do. Grapheme-based measures, lipgloss's included,
// count Thai SARA AM (ำ) as part of the consonant before it and come out a
// column short, which pushes a pane's right border off the line.
func Width(s string) int {
	n := 0
	var sc scanner
	for _, r := range s {
		n += sc.cells(r)
	}
	return n
}

// An emoji and what modifies it (a skin tone, VS16) are drawn as one picture:
// 👍🏽 is two columns, not four. A zero-width joiner joins nothing here:
// terminals disagree on how wide 👨‍👩‍👧 is, so termsafe.Label drops the
// joiner and each emoji is drawn, and counted, on its own (a raw joiner
// counts 0).
const (
	zwj  = 0x200d
	vs16 = 0xfe0f // emoji presentation: ❤ is one column, ❤️ two
)

func pictographic(r rune) bool {
	return (r >= 0x1f000 && r <= 0x1faff) || (r >= 0x2600 && r <= 0x27bf) || (r >= 0x2b00 && r <= 0x2bff) ||
		(r >= 0x2190 && r <= 0x21ff) || (r >= 0x2300 && r <= 0x23ff)
}

func skinTone(r rune) bool { return r >= 0x1f3fb && r <= 0x1f3ff }

// scanner counts cells rune by rune, knowing whether an emoji is open: then
// a skin tone and VS16 add nothing (VS16 makes a one-column emoji two).
type scanner struct {
	inEmoji bool // the last rune was part of an emoji
	lastOne bool // the emoji's picture has counted one column so far
}

func (s *scanner) cells(r rune) int {
	switch {
	case s.inEmoji && skinTone(r):
		return 0
	case s.inEmoji && r == vs16:
		if s.lastOne {
			s.lastOne = false
			return 1
		}
		return 0
	}
	c := RuneCells(r)
	s.inEmoji, s.lastOne = pictographic(r) || skinTone(r), c == 1
	return c
}

func RuneCells(r rune) int {
	switch {
	case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r):
		return 0 // combining marks (Thai vowel and tone marks, accents), zero-width format
	case r >= 0x1160 && r <= 0x11ff:
		return 0 // Hangul medial vowels and final consonants join the syllable
	}
	return runewidth.RuneWidth(r)
}

// Fit cuts plain text to at most w columns, ending in "…" when cut.
func Fit(s string, w int) string {
	if Width(s) <= w {
		return s
	}
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	var sc scanner
	for _, r := range s {
		c := sc.cells(r)
		if used+c > w-1 {
			break
		}
		b.WriteRune(r)
		used += c
	}
	return trimJoin(b.String()) + "…"
}

// FitPad is Fit, padded with spaces to exactly w columns.
func FitPad(s string, w int) string {
	s = Fit(s, w)
	return s + strings.Repeat(" ", max(0, w-Width(s)))
}

// trimJoin drops a joiner left dangling at the end of a cut emoji sequence,
// which would join the next thing drawn onto it.
func trimJoin(s string) string {
	return strings.TrimRight(s, "\u200d")
}

// TruncLeft keeps the end of a long path, which is the part that matters.
func TruncLeft(s string, w int) string {
	if Width(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && Width(string(r))+1 > w {
		r = r[1:]
	}
	return "…" + string(r)
}

// Wrap breaks plain text into lines of at most w columns, at spaces where it
// can, and keeps at most maxLines of them: when the text needs more, the
// last line kept ends in "…". It never returns an empty slice for
// non-empty text.
func Wrap(s string, w, maxLines int) []string {
	if w <= 0 || maxLines <= 0 {
		return nil
	}
	var lines []string
	line := ""
	flush := func() {
		lines = append(lines, line)
		line = ""
	}
	for _, word := range strings.Fields(s) {
		for Width(word) > w { // a word too long for a line is broken where it reaches the edge
			if line != "" {
				flush()
			}
			head := cut(word, w)
			lines = append(lines, head)
			word = word[len(head):]
		}
		switch {
		case line == "":
			line = word
		case Width(line)+1+Width(word) <= w:
			line += " " + word
		default:
			flush()
			line = word
		}
	}
	if line != "" || len(lines) == 0 {
		flush()
	}
	if len(lines) > maxLines {
		rest := strings.Join(lines[maxLines-1:], " ")
		lines = append(lines[:maxLines-1], Fit(rest, w))
	}
	return lines
}

// cut is the longest prefix of s at most w columns wide (at least one rune,
// so a loop over it always moves on).
func cut(s string, w int) string {
	used := 0
	var sc scanner
	for i, r := range s {
		c := sc.cells(r)
		if used+c > w && i > 0 {
			return s[:i]
		}
		used += c
	}
	return s
}
