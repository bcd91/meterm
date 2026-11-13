package config

import (
	"errors"
	"strings"
	"testing"
)

// Notes are prose: an emoji sequence holds a zero-width joiner (👨\u200d💻) and
// Persian needs the zero-width non-joiner. Display drops both, so they can't
// make two rows look alike; they stay refused where a host is told apart.
func TestNotesKeepJoiners(t *testing.T) {
	for _, note := range []string{"\U0001F468\u200d\U0001F4BB oncall", "\U0001F3F3️\u200d\U0001F308", "می\u200cخواهم"} {
		h := Host{Name: "plain", Address: "192.0.2.1", Notes: note, Forwards: []Forward{{Name: "f", Type: "D", LocalPort: 1080, Note: note}}}
		if err := h.CheckPrintable(); err != nil {
			t.Errorf("note %q refused by add/edit: %v", note, err)
		}
		if err := h.CheckShared(); err != nil {
			t.Errorf("note %q refused from a team: %v", note, err)
		}
	}
	for _, r := range []rune{0x200c, 0x200d} {
		s := "a" + string(r) + "b"
		for field, h := range map[string]Host{
			"name":    {Name: s, Address: "192.0.2.1"},
			"group":   {Name: "x", Address: "192.0.2.1", Group: s},
			"tag":     {Name: "x", Address: "192.0.2.1", Tags: []string{s}},
			"user":    {Name: "x", Address: "192.0.2.1", User: s},
			"address": {Name: "x", Address: "192.0.2." + string(r) + "1"},
			"jump":    {Name: "x", Address: "192.0.2.1", ProxyJump: s},
		} {
			if h.CheckPrintable() == nil {
				t.Errorf("%U in %s accepted by add/edit", r, field)
			}
			if h.CheckShared() == nil {
				t.Errorf("%U in %s accepted from a team", r, field)
			}
		}
	}
	if (Host{Name: "x", Address: "192.0.2.1", Notes: "a\u200bb"}).CheckPrintable() == nil {
		t.Error("a zero-width space in notes was accepted")
	}
}

// The mixed-alphabet rule looks at one word at a time: "db-москва" is two
// words in two alphabets, "pаypal" with a Cyrillic а is one word in two.
func TestMixedScriptsPerWord(t *testing.T) {
	for name, bad := range map[string]bool{
		"db-москва":     false,
		"api-μ":         false,
		"db1москва":     false,
		"web.сервер_02": false,
		"pаypal":        true,
		"web-pаy":       true,
		"сервер-webο":   true,
		"й-api":        false, // a combining mark stays with its letter
		"webйapi":      true,
	} {
		err := CheckName("name", name)
		if bad != (err != nil) || bad && !errors.Is(err, ErrMixedScript) {
			t.Errorf("%q: %v", name, err)
		}
		h := Host{Name: name, Address: "192.0.2.1"}
		if bad != (h.CheckPrintable() != nil) || bad != (h.CheckShared() != nil) {
			t.Errorf("%q: printable %v, shared %v", name, h.CheckPrintable(), h.CheckShared())
		}
	}
}

// CheckFromTeam splits what v0.24 refuses into what v0.23.1 refused too (an
// error: the host isn't used) and what only v0.24 refuses (a warning: a
// host a team already had stays, flagged).
func TestCheckFromTeamKeepsWhatOlderVersionsAccepted(t *testing.T) {
	for what, h := range map[string]Host{
		"zero-width space in name": {Name: "zw\u200bsp", Address: "192.0.2.1"},
		"joiner in tag":            {Name: "x", Address: "192.0.2.1", Tags: []string{"a\u200db"}},
		"look-alike name":          {Name: "pаypal", Address: "192.0.2.1"},
		"soft hyphen in group":     {Name: "x", Address: "192.0.2.1", Group: "a\u00adb"},
		"zero-width in jump":       {Name: "x", Address: "192.0.2.1", ProxyJump: "bast\u200bion"},
	} {
		warn, err := h.CheckFromTeam()
		if err != nil || warn == nil {
			t.Errorf("%s: warn %v, err %v; want a warning only", what, warn, err)
		}
	}
	for what, h := range map[string]Host{
		"escape in notes": {Name: "x", Address: "192.0.2.1", Notes: "a\x1b]0;PWN\x07"},
		"newline in name": {Name: "a\nb", Address: "192.0.2.1"},
		"bidi in name":    {Name: "a\u202eb", Address: "192.0.2.1"},
		"C1 in tag":       {Name: "x", Address: "192.0.2.1", Tags: []string{"a\u009bb"}},
		"unsafe address":  {Name: "x", Address: "-oProxyCommand=x"},
		"unsafe option":   {Name: "x", Address: "192.0.2.1", ExtraArgs: []string{"-oProxyCommand=x"}},
	} {
		if _, err := h.CheckFromTeam(); err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
	warn, err := Host{Name: "pаypal", Address: "192.0.2.1"}.CheckFromTeam()
	if err != nil || warn == nil || !strings.Contains(warn.Error(), "name") {
		t.Errorf("warning %v doesn't name the field", warn)
	}
	if warn, err := (Host{Name: "db-москва", Address: "192.0.2.1", Notes: "\U0001F468\u200d\U0001F4BB"}).CheckFromTeam(); warn != nil || err != nil {
		t.Errorf("clean host: %v %v", warn, err)
	}
}
