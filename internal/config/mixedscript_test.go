package config

import (
	"errors"
	"testing"
)

func TestMixedScriptNamesAreRefused(t *testing.T) {
	for name, bad := range map[string]bool{
		"pay1":         false,
		"web-ไทย":      false, // Thai beside Latin can't pass for Latin
		"сервер":       false, // all Cyrillic
		"p\u0430y1":    true,  // Cyrillic а in Latin
		"web\u03bf1":   true,  // Greek ο
		"\u0430\u03b1": true,  // Cyrillic and Greek
	} {
		err := CheckName("name", name)
		if bad != (err != nil) || bad && !errors.Is(err, ErrMixedScript) {
			t.Errorf("%q: %v", name, err)
		}
		h := Host{Name: name, Address: "192.0.2.1"}
		if bad != (h.CheckPrintable() != nil) {
			t.Errorf("%q: CheckPrintable %v", name, h.CheckPrintable())
		}
		if bad != (h.CheckShared() != nil) {
			t.Errorf("%q: CheckShared %v", name, h.CheckShared())
		}
	}
	if (Host{Name: "web1", Address: "192.0.2.1", Group: "p\u0430y"}).CheckShared() == nil {
		t.Error("a look-alike group from a team was taken")
	}
	if (Host{Name: "web1", Address: "192.0.2.1", Notes: "Сервер for prod"}).CheckPrintable() != nil {
		t.Error("notes mixing alphabets refused: prose is fine")
	}
}

// A whole word of Cyrillic or Greek letters that all look Latin passes the
// per-word rule but shows as Latin beside a Latin word: "web-арр" reads
// "web-app". In a name with Latin letters such a word is refused too.
func TestLookalikeWordBesideLatinIsRefused(t *testing.T) {
	for name, bad := range map[string]bool{
		"web-\u0430\u0440\u0440": true, // Cyrillic
		"web1\u0430":             true, // Cyrillic a
		"web-\u03a1":             true, // Greek capital Rho
		"p\u0430ypal":            true, // mixed in one word, as before
		"\u0410\u0420\u0420-web": true, // Cyrillic capitals
		"db-\u043c\u043e\u0441\u043a\u0432\u0430": false, // Moscow: lowercase m, k, v are small-capital shapes
		"api-\u03bc": false, // mu
		"web.\u0441\u0435\u0440\u0432\u0435\u0440_02":             false, // "server": holds v
		"\u0430\u0440\u0440":                                      false, // all Cyrillic, no Latin word to pass for
		"\u0430\u0440\u0440-\u0441\u0435\u0440\u0432\u0435\u0440": false,
		"web-\u0e44\u0e17\u0e22":                                  false, // Thai
		"web-\u0641\u0627\u0631\u0633\u06cc":                      false, // Persian
	} {
		err := CheckName("name", name)
		if bad != (err != nil) || bad && !errors.Is(err, ErrMixedScript) {
			t.Errorf("%+q: %v", name, err)
		}
		h := Host{Name: name, Address: "192.0.2.1"}
		if bad != (h.CheckShared() != nil) {
			t.Errorf("%+q: CheckShared %v", name, h.CheckShared())
		}
		warn, err := h.CheckFromTeam()
		if err != nil || bad != (warn != nil) {
			t.Errorf("%+q: CheckFromTeam warn %v err %v (a team host is warned, not refused)", name, warn, err)
		}
	}
}
