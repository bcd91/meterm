package tui

import (
	"testing"
)

// A question open when the terminal shrinks below the minimum is a no, and
// leaves nothing behind: not the command it would have run, nor the palette
// text a logs listing would have handed back after it.
func TestTooSmallCancelsTheOpenQuestionAndForgetsIt(t *testing.T) {
	m := prodFleet(t)
	m.reselect("pay1")
	m, ran := step(t, m, "L") // logs on production: asks, and would reopen the palette after
	if ran || m.mode != modeConfirmRun || m.afterExec == "" || len(m.pending) == 0 {
		t.Fatalf("setup: ran %v mode %v afterExec %q pending %q", ran, m.mode, m.afterExec, m.pending)
	}
	m = resized(t, m, 30, 8)
	m, ran = step(t, waited(m), "y")
	if ran || m.mode != modeList {
		t.Fatalf("too small, y: ran %v mode %v", ran, m.mode)
	}
	if m.pending != nil || m.pendingProd != nil || m.afterExec != "" || m.target != "" {
		t.Errorf("left behind: pending %q prod %q afterExec %q target %q", m.pending, m.pendingProd, m.afterExec, m.target)
	}
	if m.flash == "" {
		t.Error("nothing says it was cancelled")
	}
	// Back to a usable size, the next command's end doesn't reopen the
	// palette with the cancelled one's text.
	m = resized(t, m, 120, 30)
	m.reselect("dev1")
	m, ran = step(t, m, "enter")
	if !ran {
		t.Fatal("enter on dev1 didn't connect")
	}
	next, _ := m.Update(execDoneMsg{})
	if p := next.(model); p.mode == modePalette {
		t.Errorf("the cancelled question's palette came back: %q", p.palette.Value())
	}

	// The same for a delete question: its host is forgotten.
	m.reselect("dev1")
	m, _ = step(t, m, "d")
	if m.mode != modeConfirmDelete || m.target == "" {
		t.Fatalf("setup: delete: mode %v target %q", m.mode, m.target)
	}
	m = resized(t, m, 30, 8)
	m, _ = step(t, waited(m), "y")
	if m.mode != modeList || m.target != "" {
		t.Errorf("delete question: mode %v target %q", m.mode, m.target)
	}
}
