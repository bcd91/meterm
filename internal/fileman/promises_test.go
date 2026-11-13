package fileman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Tests carried over from v0.23.1, where they guarded the same promises:
// a delete or rename acts on what it asked about, and esc never leaves a
// file being overwritten partly written unless asked twice.

// hold sends a key but keeps back what it started (a directory listing),
// to deliver later: the slow-server case where the answer to a question
// comes before the listing that was already on its way.
func (h *harness) hold(k string) tea.Cmd {
	h.clock = h.clock.Add(time.Second)
	var cmd tea.Cmd
	h.m, cmd = h.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	return cmd
}

func TestDeleteActsOnTheDirectoryItAskedAbout(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(right, "config"), "top", 0o644)
	write(t, filepath.Join(right, "A", "config"), "inner", 0o644)
	h := newHarness(t, left, right)
	h.keys("tab", "l") // into A
	if h.cursorName(1) != "config" {
		t.Fatalf("cursor on %q", h.cursorName(1))
	}
	inA := h.model().panes[1].dir
	listing := h.hold("h") // back to right/, listing still on its way
	h.keys("d")
	if a := h.model().ask; a == nil || a.dir != inA || !strings.Contains(h.model().confirmText, "/A?") {
		t.Errorf("question %q isn't about %s", h.model().confirmText, inA)
	}
	h.run(listing) // lands while the question is open
	if st := h.model().status; !strings.Contains(st, "cancelled: the directory changed while asking") {
		t.Errorf("status = %q, want the question cancelled", st)
	}
	h.keys("y") // typed as the answer: must not act, not even as "copy"
	for _, p := range []string{filepath.Join(right, "config"), filepath.Join(right, "A", "config")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s deleted: %v", p, err)
		}
	}
	if des, _ := os.ReadDir(left); len(des) != 0 {
		t.Errorf("the y acted as a copy: left holds %d entries", len(des))
	}
	h.keys("enter") // dismisses the notice; keys work again after it
	h.keys("j")
	if h.model().mode != modeNormal {
		t.Errorf("mode = %v after dismissing", h.model().mode)
	}
}

func TestDeleteUsesWhatItAskedAboutWhenTheDirectoryStays(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(right, "config"), "top", 0o644)
	write(t, filepath.Join(right, "A", "config"), "inner", 0o644)
	h := newHarness(t, left, right)
	h.keys("tab", "l", "d")
	h.run(h.model().load(1, h.model().panes[1].dir)) // a fresh listing of the same directory doesn't cancel
	h.keys("y")
	if _, err := os.Stat(filepath.Join(right, "A", "config")); !os.IsNotExist(err) {
		t.Errorf("A/config not deleted (status %q)", h.model().status)
	}
	if _, err := os.Stat(filepath.Join(right, "config")); err != nil {
		t.Errorf("the wrong config was deleted: %v", err)
	}
}

func TestRenameActsOnTheDirectoryItAskedAbout(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(right, "A", "config"), "inner", 0o644)
	write(t, filepath.Join(right, "other"), "o", 0o644)
	h := newHarness(t, left, right)
	h.keys("tab", "l")
	listing := h.hold("h")
	h.keys("R") // rename (r reloads)
	h.run(listing)
	if st := h.model().status; !strings.Contains(st, "cancelled: the directory changed while asking") {
		t.Errorf("status = %q, want the rename cancelled", st)
	}
	h.keys("x", "enter")
	for _, p := range []string{filepath.Join(right, "A", "config"), filepath.Join(right, "other")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s renamed away: %v", p, err)
		}
	}
	des, _ := os.ReadDir(right)
	if len(des) != 2 {
		t.Errorf("right holds %d entries, want A and other untouched", len(des))
	}
}

// A fresh listing of the same directory can move what's under the cursor;
// the rename still renames what the box was opened for.
func TestRenameRenamesWhatItWasOpenedFor(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(right, "b"), "b", 0o644)
	h := newHarness(t, left, right)
	h.keys("tab", "R")
	write(t, filepath.Join(right, "a"), "a", 0o644) // sorts first: the cursor row now shows a
	h.run(h.model().load(1, right))
	h.send(tea.KeyMsg{Type: tea.KeyCtrlU})
	h.keys("c", "enter")
	for name, want := range map[string]string{"a": "a", "c": "b"} {
		if got, err := os.ReadFile(filepath.Join(right, name)); string(got) != want {
			t.Errorf("%s = %q (%v), want %q (status %q)", name, got, err, want, h.model().status)
		}
	}
}

// After a question was cancelled because the directory changed, the next
// key must be enter or esc: a stray q (typed into what was a rename box)
// doesn't quit either. ctrl+c still does.
func TestOnlyCtrlCQuitsAfterACancelledQuestion(t *testing.T) {
	for _, tc := range []struct {
		key  tea.KeyMsg
		quit bool
	}{{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}, false}, {tea.KeyMsg{Type: tea.KeyCtrlC}, true}} {
		left, right := t.TempDir(), t.TempDir()
		write(t, filepath.Join(right, "A", "config"), "inner", 0o644)
		h := newHarness(t, left, right)
		h.keys("tab", "l")
		listing := h.hold("h")
		h.keys("R")
		h.run(listing)
		if !(h.model().mode == modeNotice) {
			t.Fatal("question wasn't cancelled")
		}
		_, cmd := h.m.Update(tc.key)
		quit := false
		if cmd != nil {
			_, quit = cmd().(tea.QuitMsg)
		}
		if quit != tc.quit {
			t.Errorf("%s: quit = %v, want %v", tc.key, quit, tc.quit)
		}
	}
}

// busyCopying puts the model in the middle of a copy with spies for both

// busyCopying puts the model in the middle of a copy, with spies for both
// ways to stop. inPlace says the person agreed to a write in place.
func busyCopying(h *harness, inPlace bool) (stoppedAfter, stoppedNow *bool) {
	var after, now bool
	m := h.model()
	m.mode, m.busy = modeBusy, busyCopy
	m.cancel = func() { now = true }
	m.stopAfter = func() { after = true }
	m.progress = &Progress{File: "/src/a.bin", FileSize: 100, Total: 100, FilesTotal: 2, Started: h.clock}
	if inPlace {
		m.runOpts = RunOptions{Overwrite: true, InPlace: map[string]bool{"/dst/a.bin": true}}
	}
	h.m = m
	return &after, &now
}

func TestFirstEscLetsAFileWrittenInPlaceFinish(t *testing.T) {
	h := newHarness(t, t.TempDir(), t.TempDir())
	after, now := busyCopying(h, true)
	h.keys("esc")
	if !*after || *now {
		t.Fatalf("first esc: stopAfter %v, cancel %v; want only the gentle stop", *after, *now)
	}
	if v := stripANSI(h.model().View()); !strings.Contains(v, "esc again: stop now") {
		t.Errorf("view doesn't say what esc does next:\n%s", v)
	}
	h.keys("esc")
	if !*now {
		t.Error("second esc didn't stop at once")
	}
}

func TestFirstEscStopsAtOnceWithNothingInPlace(t *testing.T) {
	h := newHarness(t, t.TempDir(), t.TempDir())
	_, now := busyCopying(h, false)
	h.keys("esc")
	if !*now {
		t.Error("first esc didn't stop a copy to temporary files at once")
	}
}
