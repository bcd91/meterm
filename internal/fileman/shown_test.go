package fileman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/termsafe"
)

// A remote name with hidden characters is shown spelled out and quoted, as
// the picker shows such a host name: in its row, in the delete question, in
// the status line and in the log. Cleaned up it would pass for another
// file ("evil name.exe" for one ending \u202etxt.exe) and the question would
// name a file that isn't the one deleted.
func TestHiddenNamesAreSpelledNotCleaned(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	name := "ev\x1b]0;PWN\x07 il\n name\u202etxt.exe"
	write(t, filepath.Join(right, name), "x", 0o644)
	write(t, filepath.Join(right, "plain.txt"), "x", 0o644)
	h := newHarness(t, left, right)
	q := termsafe.Quote(name)
	h.keys("tab")
	for h.cursorName(1) != name {
		h.keys("j")
	}
	v := viewText(h.model())
	if !strings.Contains(v, q[:20]) { // the row is cut to the pane
		t.Errorf("row doesn't spell the name %s:\n%s", q, v)
	}
	if !strings.Contains(v, " plain.txt") || strings.Contains(v, `"plain.txt"`) {
		t.Errorf("a plain name is quoted too:\n%s", v)
	}
	h.keys("d")
	if !strings.Contains(h.model().confirmText, q) {
		t.Errorf("question %q doesn't spell the name %s", h.model().confirmText, q)
	}
	if !strings.Contains(viewText(h.model()), q) {
		t.Errorf("question as shown doesn't spell the name:\n%s", viewText(h.model()))
	}
	h.keys("y")
	if _, err := os.Lstat(filepath.Join(right, name)); !os.IsNotExist(err) {
		t.Fatalf("not deleted: %v", err)
	}
	full := termsafe.Quote(filepath.Join(right, name))
	var logged []string
	for _, e := range h.events {
		if e.kind == "delete" {
			logged = append(logged, e.detail)
		}
	}
	if len(logged) != 2 {
		t.Fatalf("delete events: %q", logged)
	}
	for _, d := range logged {
		if !strings.Contains(d, full) || termsafe.Text(d) != d {
			t.Errorf("logged %q, want the exact path spelled: %s", d, full)
		}
	}
}
