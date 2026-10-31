package termsafe

import (
	"strings"
	"testing"
)

func TestText(t *testing.T) {
	const x = "�"
	for in, want := range map[string]string{
		"plain/path ü 日本語 🙂":   "plain/path ü 日本語 🙂",
		"":                     "",
		"a\x1b]52;c;aGk=\x07b": "a" + x + "]52;c;aGk=" + x + "b",
		"a\x1b[2Jb":            "a" + x + "[2Jb",
		"a\nb\rc\td":           "a" + x + "b" + x + "c" + x + "d",
		"a\u009b31mb":          "a" + x + "31mb", // C1 CSI, as UTF-8
		"a\x9b31mb":            "a" + x + "31mb", // C1 CSI, as a raw byte
		"a\xffb":               "a" + x + "b",
		"evil‮gnp.exe":         "evil" + x + "gnp.exe",
		"a⁦b⁩":                 "a" + x + "b" + x,
		"a​b‍b\xef\xbb\xbfc":   "a" + x + "b" + x + "b" + x + "c",
		"a­b":                  "a" + x + "b",
		"a b c":                "a" + x + "b" + x + "c",
		"a\U000E0041b":         "a" + x + "b", // tag character
		"a￾b￿c":                "a" + x + "b" + x + "c",
		"a\x7fb":               "a" + x + "b",
		"a\x00b":               "a" + x + "b",
	} {
		if got := Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
		if got := Text(Text(in)); got != want {
			t.Errorf("Text isn't stable on %q: %q", in, got)
		}
	}
}

func TestLinesKeepsOnlyNewlinesAndTabs(t *testing.T) {
	for in, want := range map[string]string{
		"a\nb\x1b[0m\r\n": "a\nb\uFFFD[0m\n",
		"a\tb":            "a\tb",
		"a\rb":            "a\uFFFDb",
		"a\r\r\nb":        "a\uFFFD\nb",
	} {
		if got := Lines(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestTextDefaultIgnorableAndFillers(t *testing.T) {
	for _, r := range []rune{0x034F, 0x115F, 0x1160, 0x17B4, 0x17B5, 0x180B, 0x180E, 0x2065, 0x2800, 0x3164,
		0xFE00, 0xFE0F, 0xFFA0, 0xFFF0, 0xE0000, 0xE0100, 0xE0FFF} {
		if got := Text("a" + string(r) + "b"); got != "a\uFFFDb" {
			t.Errorf("U+%04X: %q", r, got)
		}
	}
	if got := Text("日本語 ñ é 한글 ّ"); got != "日本語 ñ é 한글 ّ" {
		t.Errorf("real text changed: %q", got)
	}
}

func TestTextUnsafe(t *testing.T) {
	if Unsafe("web1/db ü") || !Unsafe("a\x1b") || !Unsafe("a‮") {
		t.Error("Unsafe disagrees with Text")
	}
}

func TestTextClip(t *testing.T) {
	if got := Clip("abc", 3); got != "abc" {
		t.Errorf("got %q", got)
	}
	if got := Clip(strings.Repeat("日", 10), 4); got != "日日日…" {
		t.Errorf("got %q", got)
	}
}
