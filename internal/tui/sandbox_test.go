package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/history"
)

// Tests must never read the person's own files: New loads history (and
// through the hooks, team state) from the default locations.
func TestTestModelNeverReadsRealFiles(t *testing.T) {
	m := testModel(t, 80, 24)
	tmp := os.TempDir()
	p, err := history.Path()
	if err != nil {
		t.Fatal(err)
	}
	for what, path := range map[string]string{"history": p, "config": m.cfgPath, "HOME": os.Getenv("HOME")} {
		if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(tmp)) {
			t.Errorf("%s is %s, outside the temp dir %s", what, path, tmp)
		}
	}
}

// sandbox points HOME and the XDG directories at a fresh temp dir, so
// nothing a test does can read or write the person's own files.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("TMPDIR", dir) // the picker's own temp files go with the test
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("METERM_HISTORY", "")
	t.Setenv("METERM_CONFIG", "")
	return dir
}

// newModel is New over a host file in a sandbox.
func newModel(t *testing.T) model {
	t.Helper()
	dir := sandbox(t)
	return New(filepath.Join(dir, "hosts.yaml"), "/usr/local/bin/meterm")
}

// A wide character takes two columns; counting runes would let a row of
// CJK host names overflow the frame unnoticed.
func TestMaxLineWidthCountsCells(t *testing.T) {
	if got := maxLineWidth("東京-web"); got != 8 {
		t.Errorf("maxLineWidth = %d, want 8", got)
	}
}
