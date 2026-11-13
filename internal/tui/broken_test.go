package tui

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

const brokenTail = "hosts:\n  - name: [broken\n"

// A host file that doesn't parse must not look like an empty inventory, and
// the picker mustn't offer to add hosts over it.
func TestBrokenHostFileIsShownNotEmptied(t *testing.T) {
	noDial(t)
	dir := sandbox(t)
	path := dir + "/hosts.yaml"
	if err := os.WriteFile(path, []byte(brokenTail), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(path, "/usr/local/bin/meterm")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = next.(model)
	v := stripANSI(m.View())
	// What's wrong comes before the path: a long path (a long TMPDIR here)
	// used to push it off the banner. The path is in full under ?.
	if !strings.Contains(strings.Join(strings.Fields(v), " "), "yaml: line 1") {
		t.Errorf("view doesn't show what's wrong:\n%s", v)
	}
	if !strings.Contains(m.bannerText(), "(in "+path+")") {
		t.Errorf("banner text lacks the path: %q", m.bannerText())
	}
	if maxLineWidth(v) > 100 || strings.Count(v, "\n") >= 30 {
		t.Errorf("view overflows (%d wide, %d lines):\n%s", maxLineWidth(v), strings.Count(v, "\n")+1, v)
	}
	for _, k := range []string{"a", "e", "d"} {
		next, cmd := m.Update(keyMsg(k))
		if cmd != nil || next.(model).mode != modeList {
			t.Errorf("%s on an unreadable host file: mode %v, cmd %v", k, next.(model).mode, cmd != nil)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != brokenTail {
		t.Errorf("host file changed: %q", b)
	}
}

// Breaking the file while the picker is open keeps the last list it read,
// and says why it isn't updating; fixing it clears that.
func TestReloadKeepsTheLastGoodList(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	m = drive(t, m, nil, "r")
	if n := len(m.list.Items()); n != 8 {
		t.Fatalf("setup: %d rows", n)
	}
	good, _ := os.ReadFile(m.cfgPath)
	if err := os.WriteFile(m.cfgPath, append(append([]byte{}, good...), "\n"+brokenTail...), 0o600); err != nil {
		t.Fatal(err)
	}
	m = drive(t, m, nil, "r")
	if n := len(m.list.Items()); n != 8 {
		t.Errorf("after the file broke: %d rows, want the 8 from before", n)
	}
	if v := stripANSI(m.View()); !strings.Contains(strings.Join(strings.Fields(v), " "), m.cfgPath) {
		t.Errorf("no error shown:\n%s", v)
	}
	next, cmd := m.Update(keyMsg("d"))
	if cmd != nil || next.(model).mode != modeList {
		t.Error("delete offered over an unreadable host file")
	}
	if err := os.WriteFile(m.cfgPath, good, 0o600); err != nil {
		t.Fatal(err)
	}
	m = drive(t, m, nil, "r")
	if m.loadErr != nil || strings.Contains(stripANSI(m.View()), "can't read") {
		t.Errorf("error still shown after the fix: %v", m.loadErr)
	}
}
