package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"meterm/internal/cellwidth"
	"meterm/internal/config"
)

// At 40 columns the ? screen wraps its lines rather than cut them: every
// word of the help is on it, and no line ends in "…".
func TestHelpWrapsOnANarrowTerminal(t *testing.T) {
	m := testModel(t, 40, 20)
	m = press(m, "?")
	lines := m.helpLines()
	var words []string
	for _, l := range lines {
		if cellwidth.Width(l) > 40-4 {
			t.Errorf("line %d wide: %q", cellwidth.Width(l), l)
		}
		if strings.HasSuffix(l, "…") {
			t.Errorf("cut: %q", l)
		}
		words = append(words, strings.Fields(l)...)
	}
	all := strings.Join(words, " ")
	for _, want := range []string{"connect (asks which login when the host has several)",
		"filter: name, group, tags, env, notes, user, team", "exec on the marked hosts"} {
		if !strings.Contains(all, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	// And the screen shows them as they are.
	if v := stripANSI(m.View()); strings.Contains(v, "connect (asks which…") {
		t.Errorf("help still cut:\n%s", v)
	}
}

// The help starts with what changed: keys people knew have moved.
func TestHelpSaysWhichKeysChanged(t *testing.T) {
	m := testModel(t, 100, 30)
	top := strings.Join(press(m, "?").helpLines()[:12], " ")
	for _, want := range []string{"v0.24", "pgup", "M", "C", "R", "esc", "150ms"} {
		if !strings.Contains(top, want) {
			t.Errorf("the top of ? doesn't mention %q:\n%s", want, top)
		}
	}
}

// Paging matches the file manager: pgdn and pgup a whole page, ctrl+d and
// ctrl+u half of one.
func TestPaging(t *testing.T) {
	noDial(t)
	m := testModel(t, 100, 24)
	var hosts []config.Host
	for i := 1; i <= 60; i++ {
		hosts = append(hosts, config.Host{Name: fmt.Sprintf("h%02d", i), Address: fmt.Sprintf("192.0.2.%d", i)})
	}
	writeHosts(t, m.cfgPath, hosts...)
	m = drive(t, m, nil, "r", "s") // by name
	rows := m.list.Height()
	if rows < 6 {
		t.Fatalf("only %d rows", rows)
	}
	idx := func(m model) int { return m.list.Index() }
	m = press(m, "ctrl+d")
	if got := idx(m); got != rows/2 {
		t.Errorf("ctrl+d: at %d, want %d (half of %d rows)", got, rows/2, rows)
	}
	m = press(m, "ctrl+u")
	if got := idx(m); got != 0 {
		t.Errorf("ctrl+u: at %d, want 0", got)
	}
	m = press(m, "pgdown")
	if got := idx(m); got != rows {
		t.Errorf("pgdn: at %d, want %d", got, rows)
	}
	m = press(m, "pgup")
	if got := idx(m); got != 0 {
		t.Errorf("pgup: at %d, want 0", got)
	}
	// In ?, the same.
	h := press(testModel(t, 40, 12), "?")
	page := h.height - 2
	h = press(h, "ctrl+d")
	if h.helpTop != page/2 {
		t.Errorf("? ctrl+d: top %d, want %d", h.helpTop, page/2)
	}
	h = press(h, "ctrl+u", "pgdown")
	if h.helpTop != page {
		t.Errorf("? pgdn: top %d, want %d", h.helpTop, page)
	}
	_ = tea.KeyMsg{}
}
