package termsafe

import (
	"bytes"
	"strings"
	"testing"
)

var cases = []struct{ name, in, want string }{
	{"text", "hello\tworld\n", "hello\tworld\n"},
	{"utf-8", "สวัสดี é 日本語\n", "สวัสดี é 日本語\n"},
	{"sgr kept", "\x1b[1;31mred\x1b[0m\n", "\x1b[1;31mred\x1b[0m\n"},
	{"sgr colons", "\x1b[38:2:1:2:3mx\x1b[m", "\x1b[38:2:1:2:3mx\x1b[m"},
	{"osc 52 bel", "a\x1b]52;c;ZXZpbA==\x07b", "ab"},
	{"osc 52 st", "a\x1b]52;c;ZXZpbA==\x1b\\b", "ab"},
	{"osc title", "\x1b]0;owned\x07ok", "ok"},
	{"osc 176", "\x1b]176;x\x07ok", "ok"},
	{"dcs", "\x1bPq#0;2;0;0;0\x1b\\ok", "ok"},
	{"apc", "\x1b_Gf=100;x\x1b\\ok", "ok"},
	{"pm", "\x1b^x\x1b\\ok", "ok"},
	{"sos", "\x1bXx\x1b\\ok", "ok"},
	{"cursor", "\x1b[2J\x1b[H\x1b[10;10Hok", "ok"},
	{"erase line", "good\x1b[2K\rbad", "goodbad"},
	{"private mode", "\x1b[?1049h\x1b[?25lok", "ok"},
	{"query answer", "\x1b[6n\x1b[cok", "ok"},
	{"sgr with private marker", "\x1b[?1mok", "ok"},
	{"sgr with intermediate", "\x1b[1 mok", "ok"},
	{"two byte", "\x1bcok\x1b7\x1b8", "ok"},
	{"charset", "\x1b(0ok", "ok"},
	{"controls", "a\x07\x08\x0b\x0c\r\x0e\x0f\x7fb", "ab"},
	{"crlf", "a\r\nb\r\n", "a\nb\n"},
	{"raw c1 csi", "\x9b2Jok", "ok"},
	{"raw c1 osc", "\x9d52;c;x\x9cok", "ok"},
	{"raw c1 dcs", "\x90x\x9cok", "ok"},
	{"raw c1 apc", "\x9fx\x9cok", "ok"},
	{"utf-8 c1 csi", "\xc2\x9b2Jok", "ok"},
	{"utf-8 c1 osc", "\xc2\x9d52;c;x\xc2\x9cok", "ok"},
	{"utf-8 c1 sgr", "\xc2\x9b31mok", "\x1b[31mok"},
	{"not utf-8 then c1", "\xe0\x9b2Jok", "\xe0ok"},
	{"legacy bytes stay", "\xa1\xb2\xfb\n", "\xa1\xb2\xfb\n"},
	{"stray osc doesn't hide later lines", "\x1b]oops\nnext line\n", "\nnext line\n"},
	{"stray csi doesn't hide later lines", "\x1b[12\nnext\n", "\nnext\n"},
	{"cancelled", "\x1b]0;x\x18ok", "ok"},
	{"esc starts over in a string", "\x1b]0;x\x1b[31mok", "\x1b[31mok"},
	{"long csi", "\x1b[" + strings.Repeat("1;", 100) + "mok", "ok"},
	{"nested", "\x1b\x1b]52;c;x\x07ok", "ok"},
}

func TestFilter(t *testing.T) {
	for _, c := range cases {
		if got := String(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// However it's cut up on the way, the answer is the same.
func TestFilterSplitAnywhere(t *testing.T) {
	for _, c := range cases {
		var one bytes.Buffer
		f := New(&one)
		for i := 0; i < len(c.in); i++ {
			if _, err := f.Write([]byte{c.in[i]}); err != nil {
				t.Fatal(err)
			}
		}
		if one.String() != c.want {
			t.Errorf("%s, a byte at a time: got %q, want %q", c.name, one.String(), c.want)
		}
		for cut := 1; cut < len(c.in); cut++ {
			var two bytes.Buffer
			f := New(&two)
			_, _ = f.Write([]byte(c.in[:cut]))
			_, _ = f.Write([]byte(c.in[cut:]))
			if two.String() != c.want {
				t.Errorf("%s, cut at %d: got %q, want %q", c.name, cut, two.String(), c.want)
			}
		}
	}
}

// A line reaches the terminal as soon as it's written, not when more comes.
func TestWritesThrough(t *testing.T) {
	var b bytes.Buffer
	f := New(&b)
	_, _ = f.Write([]byte("line\n"))
	if b.String() != "line\n" {
		t.Errorf("held back: %q", b.String())
	}
	_, _ = f.Write([]byte("partial"))
	if b.String() != "line\npartial" {
		t.Errorf("held back: %q", b.String())
	}
}

func TestWriteReportsAllConsumed(t *testing.T) {
	n, err := New(&bytes.Buffer{}).Write([]byte("\x1b]0;gone\x07"))
	if n != 9 || err != nil {
		t.Errorf("n=%d err=%v", n, err)
	}
}

func TestWriterDropsBidiControlsOnly(t *testing.T) {
	for in, want := range map[string]string{
		"a\u202eb\u202ac":    "abc",
		"a\u2066b\u2069c":    "abc",
		"a\u2067b\u2068c":    "abc",
		"क\u200dष \u200cx":   "क\u200dष \u200cx", // joiners are real text
		"\x1b[31mred\x1b[0m": "\x1b[31mred\x1b[0m",
	} {
		if got := String(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	// One byte at a time, as a stream can arrive.
	var b strings.Builder
	w := New(&b)
	for _, c := range []byte("a\u202eb") {
		if _, err := w.Write([]byte{c}); err != nil {
			t.Fatal(err)
		}
	}
	if b.String() != "ab" {
		t.Errorf("split across writes: %q", b.String())
	}
}
