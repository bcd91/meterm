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
