package tui

import (
	"strings"
	"testing"

	"meterm/internal/config"
)

// Typed quotes are the palette's own, as before the picker learnt to quote
// names: a "..." that happens to be a valid Go literal is not decoded. A
// Windows path keeps its backslashes, and nothing turns \t into a tab.
func TestTypedQuotesAreNotGoLiterals(t *testing.T) {
	refs := map[string]string{}
	for in, want := range map[string][]string{
		`cp ./x "stage:C:\temp\new.txt"`: {"cp", "./x", `stage:C:\temp\new.txt`},
		`exec "zw\u200bsp" -- uptime`:    {"exec", `zw\u200bsp`, "--", "uptime"},
		`rename "a b" c`:                 {"rename", "a b", "c"},
		`rename "it's" c`:                {"rename", "it's", "c"},
		`cp 'a\u200bb' x`:                {"cp", `a\u200bb`, "x"},
		`exec "\x1b[2J" -- x`:            {"exec", `\x1b[2J`, "--", "x"},
		`cp ‹nope› x`:                    {"cp", "‹nope›", "x"}, // not one the picker wrote
	} {
		if got := splitArgs(in, refs); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("splitArgs(%q) = %q, want %q", in, got, want)
		}
	}
}

// The picker's own prefills name a host it can't show as it is with a
// reference only it writes; that reference, and nothing else, reads back as
// the exact name.
func TestPickerReferencesReadBackExactly(t *testing.T) {
	refs := map[string]string{}
	for _, name := range []string{"zw\u200bsp", "two words", "it's", `back\slash`, "evil\x1b]52;c;SGVsbG8=\x07name", "‹x›", `q"uote`} {
		tok := typedName(name, refs)
		if tok == name {
			t.Errorf("%q went in as it is", name)
		}
		got := splitArgs("exec "+tok+" -- uptime", refs)
		if len(got) != 4 || got[1] != name {
			t.Errorf("%q: %q reads back as %q", name, tok, got)
		}
		if got := splitArgs("cp x "+tok+":/tmp", refs); len(got) != 3 || got[2] != name+":/tmp" {
			t.Errorf("%q: host: path reads back as %q", name, got)
		}
		if shellish("exec "+tok+" -- uptime", refs) {
			t.Errorf("%q: the reference reads as shell syntax", name)
		}
	}
	if typedName("web1", refs) != "web1" {
		t.Error("a plain name was replaced")
	}
	// A reference typed inside quotes is the quotes' text, not the host.
	tok := typedName("zw\u200bsp", refs)
	if got := splitArgs(`cp '`+tok+`' x`, refs); got[1] != tok {
		t.Errorf("quoted reference: %q", got)
	}
}

// L's follow-up palette names the host by reference too, not by its raw
// name (which could carry an escape onto the screen).
func TestLogsFollowUpNamesTheHostByReference(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	writeHosts(t, m.cfgPath, config.Host{Name: "lo\x1b[2Jgs", Address: "192.0.2.95"})
	m = drive(t, m, nil, "r")
	m.reselect("lo\x1b[2Jgs")
	m, _ = step(t, m, "L")
	if strings.Contains(m.afterExec, "\x1b") || !strings.Contains(m.afterExec, "‹") {
		t.Fatalf("afterExec = %q", m.afterExec)
	}
	if got := splitArgs(m.afterExec, m.refs); len(got) < 2 || got[1] != "lo\x1b[2Jgs" {
		t.Errorf("reads back as %q", got)
	}
}
