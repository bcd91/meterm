package tui

import (
	"strings"
	"testing"
	"time"
)

// A question that guards production or a delete is answered only by y, and
// only once it has been up long enough to be read: a double Enter, or a y
// typed ahead of the box, must never run or delete anything.
func TestConfirmTakesOnlyASettledY(t *testing.T) {
	m := prodFleet(t)

	// Enter Enter in one go on a production host: the second Enter lands on
	// the question at once and must not run ssh.
	m.reselect("pay1")
	m, ran := step(t, m, "enter")
	if m, ran = step(t, m, "enter"); ran || m.mode != modeConfirmRun {
		t.Fatalf("enter enter: ran %v, mode %v", ran, m.mode)
	}
	// A y typed ahead of the box doesn't count either.
	if m, ran = step(t, m, "y"); ran || m.mode != modeConfirmRun {
		t.Fatalf("early y: ran %v, mode %v", ran, m.mode)
	}
	v := stripANSI(m.View())
	if !strings.Contains(v, "y = go") || strings.Contains(v, "enter = go") {
		t.Errorf("the question offers enter, or not y:\n%s", v)
	}
	// Once settled, Enter cancels; it never confirms.
	m = waited(m)
	if m, ran = step(t, m, "enter"); ran || m.mode != modeList {
		t.Fatalf("settled enter: ran %v, mode %v", ran, m.mode)
	}
	m.reselect("pay1")
	m, _ = step(t, m, "enter")
	if m, ran = step(t, waited(m), "y"); !ran {
		t.Error("a settled y didn't connect")
	}

	// The palette: exec --all over production hosts, Enter Enter.
	m.reselect("dev1")
	p := press(m, ":")
	p.palette.SetValue("exec --all -- uptime")
	p, _ = step(t, p, "enter")
	if p, ran = step(t, p, "enter"); ran || p.mode != modeConfirmRun {
		t.Errorf("palette enter enter: ran %v, mode %v", ran, p.mode)
	}

	// Delete: d then y 50ms apart deletes nothing.
	m.reselect("dev1")
	m, _ = step(t, m, "d")
	if m, _ = step(t, m, "y"); m.mode != modeConfirmDelete || !has(savedNames(t, m.cfgPath), "dev1") {
		t.Fatalf("early y on delete: mode %v, hosts %v", m.mode, savedNames(t, m.cfgPath))
	}
	if m, _ = step(t, waited(m), "y"); has(savedNames(t, m.cfgPath), "dev1") {
		t.Error("a settled y didn't delete")
	}
}

// waited is m once the question on screen has been up long enough to read.
func waited(m model) model {
	m.askedAt = m.askedAt.Add(-time.Second)
	return m
}
