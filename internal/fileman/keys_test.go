package fileman

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"meterm/internal/cellwidth"
)

// The keys mean what they mean in the host picker: r reloads, R renames,
// * selects all (a is the picker's add, and does nothing here).
func TestKeysMatchThePicker(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "a", 0o644)
	write(t, filepath.Join(left, "b.txt"), "b", 0o644)
	h := newHarness(t, left, right)

	write(t, filepath.Join(left, "new.txt"), "n", 0o644)
	h.keys("r")
	if !strings.Contains(viewText(h.model()), "new.txt") {
		t.Error("r didn't reload")
	}
	h.keys("a")
	if n := len(h.model().panes[0].sel); n != 0 {
		t.Errorf("a selected %d entries", n)
	}
	h.keys("*")
	if n := len(h.model().panes[0].sel); n != 3 {
		t.Errorf("* selected %d of 3", n)
	}
	h.keys("*")
	if n := len(h.model().panes[0].sel); n != 0 {
		t.Errorf("second * left %d selected", n)
	}
	h.keys("R")
	if h.model().mode != modeInput || h.model().inputKind != inputRename {
		t.Errorf("R: mode %v kind %v, want the rename box", h.model().mode, h.model().inputKind)
	}
}

// Esc clears one thing at a time: a message, then the selection.
func TestEscClearsMessageThenSelection(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "a", 0o644)
	h := newHarness(t, left, right)
	h.keys("space")
	m := h.model()
	m.setStatus("something failed", true)
	h.m = m
	h.keys("esc")
	if h.model().status != "" || len(h.model().panes[0].sel) != 1 {
		t.Errorf("first esc: status %q, %d selected", h.model().status, len(h.model().panes[0].sel))
	}
	h.keys("esc")
	if len(h.model().panes[0].sel) != 0 {
		t.Error("second esc kept the selection")
	}
}

// Esc and the next key read together (alt+key) are Esc, then the key.
func TestEscThenKeyIsNotAlt(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "a", 0o644)
	write(t, filepath.Join(left, "b.txt"), "b", 0o644)
	h := newHarness(t, left, right)
	h.keys("space") // selects a.txt, moves to b.txt
	h.keys("k")
	h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j"), Alt: true})
	if len(h.model().panes[0].sel) != 0 || h.cursorName(0) != "b.txt" {
		t.Errorf("alt+j: %d selected, cursor on %s; want esc (clear) then j (down)", len(h.model().panes[0].sel), h.cursorName(0))
	}
}

// A long message or question takes two lines, not one cut at the edge: the
// choices at the end of the overwrite question stay on screen.
func TestLongStatusWrapsToTwoLines(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "a", 0o644)
	write(t, filepath.Join(right, "a.txt"), "old", 0o644)
	h := newHarness(t, left, right)
	h.send(tea.WindowSizeMsg{Width: 60, Height: 20})
	h.keys("c")
	if h.model().mode != modeConfirm {
		t.Fatalf("mode %v, want the overwrite question", h.model().mode)
	}
	checkFits(t, "overwrite question", h.model())
	if v := viewText(h.model()); !strings.Contains(v, "esc = cancel") {
		t.Errorf("the question's choices are cut off:\n%s", v)
	}
	h.keys("esc")
	m := h.model()
	m.setStatus(strings.Repeat("copy failed because the host said no ", 10), true)
	h.m = m
	checkFits(t, "long error", h.model())
	lines := strings.Split(viewText(h.model()), "\n")
	if !strings.HasSuffix(strings.TrimSpace(lines[len(lines)-2]), "…") {
		t.Errorf("a cut message doesn't end in …: %q", lines[len(lines)-2])
	}
}

func TestTooSmallSaysSo(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "a", 0o644)
	h := newHarness(t, left, right)
	for _, sz := range [][2]int{{39, 10}, {40, 9}, {20, 5}} {
		h.send(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		v := viewText(h.model())
		if !strings.Contains(v, "too small") || strings.Contains(v, "a.txt") {
			t.Errorf("%dx%d: %q", sz[0], sz[1], v)
		}
		h.keys("d")
		if h.model().mode != modeNormal {
			t.Errorf("%dx%d: d asked a question nobody can see", sz[0], sz[1])
		}
	}
	h.send(tea.WindowSizeMsg{Width: 40, Height: 10})
	checkFits(t, "40x10", h.model())
	h.keys("?")
	checkFits(t, "40x10 help", h.model())
	if first := strings.Split(viewText(h.model()), "\n")[0]; !strings.Contains(first, "keys") {
		t.Errorf("help title isn't on top: %q", first)
	}
	h.keys("G")
	if first := strings.Split(viewText(h.model()), "\n")[0]; !strings.Contains(first, "keys") {
		t.Errorf("scrolled help lost its title: %q", first)
	}
}

// checkFits fails unless the view fits the terminal: no more lines, no
// line wider, counted in cells.
func checkFits(t *testing.T, what string, m Model) {
	t.Helper()
	lines := strings.Split(viewText(m), "\n")
	if len(lines) > m.h {
		t.Errorf("%s: %d lines on a %d-line terminal", what, len(lines), m.h)
	}
	for k, l := range lines {
		if w := cellwidth.Width(l); w > m.w {
			t.Errorf("%s: line %d is %d wide on %d: %q", what, k, w, m.w, l)
		}
	}
}
