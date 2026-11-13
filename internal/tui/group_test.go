package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"meterm/internal/config"
)

// A host whose group was removed inherits nothing from it; the detail line
// says so, at any width (the group column can't).
func TestDetailFlagsAMissingGroup(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	cfg, err := config.Load(m.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Groups = map[string]config.Group{"web": {}}
		c.Hosts = []config.Host{{Name: "pay1", Address: "192.0.2.70", Group: "pay"}, {Name: "web1", Address: "192.0.2.71", Group: "web"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m = drive(t, m, nil, "r")
	for _, w := range []int{60, 160} {
		m = resized(t, m, w, 30)
		m.reselect("pay1")
		if v := stripANSI(m.View()); !strings.Contains(v, "group pay doesn't exist") {
			t.Errorf("width %d: no flag:\n%s", w, v)
		}
		m.reselect("web1")
		if v := stripANSI(m.View()); strings.Contains(v, "doesn't exist") {
			t.Errorf("width %d: a group that exists is flagged", w)
		}
	}
}

func resized(t *testing.T, m model, w, h int) model {
	t.Helper()
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return next.(model)
}
