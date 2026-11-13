package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/prodask"
)

// tokenOf is the token file a dispatched command was given, "" for none.
func tokenOf(m model) (path, value string) {
	if m.lastDispatch == nil {
		return "", ""
	}
	for _, e := range m.lastDispatch.Env {
		if v, ok := strings.CutPrefix(e, prodask.PathEnv+"="); ok {
			path = v
		}
		if v, ok := strings.CutPrefix(e, prodask.ValueEnv+"="); ok {
			value = v
		}
	}
	return path, value
}

// A y to the picker's production question hands the command a one-use
// token in the picker's own dir; a command nobody was asked about gets none,
// and the picker removes a token the command didn't use.
func TestPickerHandsATokenOnlyAfterItsQuestion(t *testing.T) {
	t.Setenv(prodask.PathEnv, "/elsewhere/stale")
	t.Setenv(prodask.ValueEnv, "stale")
	m := prodFleet(t)
	t.Cleanup(func() { os.RemoveAll(m.outcomeDir) })

	m.reselect("dev1")
	m, ran := step(t, m, "enter")
	if p, v := tokenOf(m); !ran || p != "" || v != "" {
		t.Fatalf("dev host: ran %v, token %q %q", ran, p, v)
	}
	for _, e := range m.lastDispatch.Env {
		if strings.Contains(e, "stale") {
			t.Errorf("the picker passed on its own token env: %q", e)
		}
	}

	m.reselect("pay1")
	m, _ = step(t, m, "enter")
	m, ran = step(t, waited(m), "y")
	path, value := tokenOf(m)
	if !ran || path == "" || len(value) != 32 {
		t.Fatalf("after y: ran %v, token %q %q", ran, path, value)
	}
	if filepath.Dir(path) != m.outcomeDir {
		t.Errorf("token %s isn't in the picker's dir %s", path, m.outcomeDir)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != value {
		t.Errorf("token file %q %v", b, err)
	}
	next, _ := m.Update(execDoneMsg{})
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("an unused token outlived its command: %v", err)
	}
	m = next.(model)

	// Cancelled: nothing is handed out.
	m.reselect("pay1")
	m, _ = step(t, m, "enter")
	m.lastDispatch = nil
	if m, ran = step(t, waited(m), "n"); ran || m.lastDispatch != nil {
		t.Errorf("cancelled, yet dispatched %v", m.lastDispatch)
	}
}
