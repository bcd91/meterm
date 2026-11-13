package cellwidth

import (
	"strings"
	"testing"
)

// Thai vowel and tone marks take no column of their own: a terminal draws
// "ทำ" in two columns, and so must Width, or a border ends up a column off.
func TestWidthCountsLikeTheTerminal(t *testing.T) {
	for s, want := range map[string]int{
		"web1": 4,
		"เซิร์ฟเวอร์หลัก": 11,
		"ทำ": 2,
		"日本": 4,
		"🚀x": 3,
	} {
		if got := Width(s); got != want {
			t.Errorf("Width(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestFitPadIsExactlyTheWidth(t *testing.T) {
	for _, s := range []string{"web1", "เซิร์ฟเวอร์หลัก", "日本サーバー", "a-very-long-hostname-indeed"} {
		for w := 1; w < 20; w++ {
			if got := Width(FitPad(s, w)); got != w {
				t.Errorf("FitPad(%q, %d) is %d wide", s, w, got)
			}
		}
	}
}

func TestWrapKeepsToTheWidthAndLineCount(t *testing.T) {
	msg := "meterm connect web1 failed: exit status 255 (the host closed the connection before it answered)"
	for w := 5; w < 60; w++ {
		lines := Wrap(msg, w, 2)
		if len(lines) == 0 || len(lines) > 2 {
			t.Fatalf("w=%d: %d lines", w, len(lines))
		}
		for _, l := range lines {
			if Width(l) > w {
				t.Errorf("w=%d: line %q is %d wide", w, l, Width(l))
			}
		}
		if len(strings.Join(lines, " ")) < len(msg) && !strings.HasSuffix(lines[len(lines)-1], "…") {
			t.Errorf("w=%d: text cut without an ellipsis: %q", w, lines)
		}
	}
	if got := Wrap("short", 40, 2); len(got) != 1 || got[0] != "short" {
		t.Errorf("Wrap(short) = %q", got)
	}
	if got := Wrap("", 40, 2); len(got) != 1 || got[0] != "" {
		t.Errorf("Wrap(empty) = %q", got)
	}
}

// A zero-width joiner joins nothing here: terminals disagree on how wide a
// joined picture is, so termsafe.Label drops the joiner and each emoji is
// drawn, and counted, on its own. VS16 and a skin tone still belong to the
// emoji before them.
func TestEmojiSequencesAreOnePicture(t *testing.T) {
	family := "\U0001F468\u200d\U0001F469\u200d\U0001F467" // man, woman, girl, joined
	thumb := "\U0001F44D\U0001F3FD"                        // 👍🏽
	for s, want := range map[string]int{
		family:                           6,
		"\U0001F468\U0001F469\U0001F467": 6,
		family + "x":                     7,
		thumb:                            2,
		"\U0001F3FD":                     2, // a skin tone alone is drawn as a swatch
		"a\u200db":                       2,
		"\U0001F468\u200d":               2,
		"\U0001F468\U0001F469\U0001F467\U0001F466": 8,
	} {
		if got := Width(s); got != want {
			t.Errorf("Width(%q) = %d, want %d", s, got, want)
		}
	}
	family = "\U0001F468\U0001F469\U0001F467"
	// Cutting never splits a sequence: it stays whole or goes.
	if got := Fit(family+"abc", 3); got != "\U0001F468…" {
		t.Errorf("Fit = %q", got)
	}
	if got := Fit("ab"+family, 3); got != "ab…" {
		t.Errorf("Fit = %q", got)
	}
	for w := 1; w < 8; w++ {
		if got := Width(FitPad(family+"x"+thumb, w)); got != w {
			t.Errorf("FitPad(…, %d) is %d wide", w, got)
		}
	}
	if got := Wrap(family+" "+family, 6, 3); len(got) != 2 || got[0] != family {
		t.Errorf("Wrap = %q", got)
	}
}
