package main

import (
	"os"
	"strings"
	"testing"

	"meterm/internal/config"
	"meterm/internal/prodask"
)

// gate runs productionGate on one production host with stdin, and returns
// its note, whether it went on, and what it printed.
func gate(t *testing.T, stdin string, yes bool) (note string, ok bool, out, errOut string) {
	t.Helper()
	withStdin(t, stdin, func() {
		out = stdout(t, func() {
			errOut = stderrOf(t, func() {
				note, ok = productionGate([]config.Host{{Name: "pay1"}}, "exec uptime", yes)
			})
		})
	})
	return
}

// METERM_FROM_TUI says the picker started us; it doesn't say anyone was
// asked. Alone it never skips the question, and never logs "asked".
func TestFromTUIAloneDoesntSkipTheQuestion(t *testing.T) {
	cliSandbox(t)
	t.Setenv(fromTUIEnv, "1")
	t.Setenv(prodask.PathEnv, "")
	t.Setenv(prodask.ValueEnv, "")

	asTerminal(t, false)
	note, ok, _, e := gate(t, "", false)
	if !ok || note != "production, no prompt: not a terminal" || !strings.Contains(e, "not asking") {
		t.Errorf("not a terminal: note %q ok %v stderr %q", note, ok, e)
	}
	if !strings.Contains(e, "picker token missing") {
		t.Errorf("stderr doesn't say why the picker's answer didn't count: %q", e)
	}

	asTerminal(t, true)
	note, ok, o, _ := gate(t, "n\n", false)
	if ok || note != "" || !strings.Contains(o, "Go on?") {
		t.Errorf("terminal, n: note %q ok %v out %q", note, ok, o)
	}
	note, ok, _, _ = gate(t, "y\n", false)
	if !ok || note != "production, asked" {
		t.Errorf("terminal, y: note %q ok %v", note, ok)
	}

	note, ok, _, _ = gate(t, "", true)
	if !ok || note != "production, --yes" {
		t.Errorf("--yes: note %q ok %v", note, ok)
	}
}

// The picker's token skips the question once, and the log says the picker
// asked, not that this command did.
func TestPickerTokenSkipsOnce(t *testing.T) {
	cliSandbox(t)
	t.Setenv(fromTUIEnv, "1")
	dir, err := os.MkdirTemp(t.TempDir(), "picker")
	if err != nil {
		t.Fatal(err)
	}
	env, err := prodask.Issue(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	asTerminal(t, true)
	note, ok, o, _ := gate(t, "", false)
	if !ok || note != "production, picker asked" || strings.Contains(o, "Go on?") {
		t.Errorf("with a token: note %q ok %v out %q", note, ok, o)
	}
	// The same env again (a shell the picker started, a rerun): asked again.
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	note, ok, o, _ = gate(t, "n\n", false)
	if ok || note != "" || !strings.Contains(o, "Go on?") {
		t.Errorf("token reused: note %q ok %v out %q", note, ok, o)
	}
}
