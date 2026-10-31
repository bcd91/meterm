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

func TestLinesKeepsOnlyNewlines(t *testing.T) {
	if got, want := Lines("a\nb\x1b[0m\r\n"), "a\nb�[0m�\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
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
