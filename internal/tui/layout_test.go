package tui

import (
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"meterm/internal/config"
)

var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func maxLineWidth(s string) int {
	max := 0
	for _, l := range strings.Split(s, "\n") {
		if w := len([]rune(l)); w > max {
			max = w
		}
	}
	return max
}

func testModel(width, height int) model {
	m := New("/tmp/hosts.yaml", "/usr/local/bin/meterm")
	m.cfg = &config.Config{
		Hosts: []config.Host{
			{Name: "bastion", Address: "203.0.113.9", User: "jump"},
			{Name: "db1", Address: "10.0.1.20", User: "deploy", ProxyJump: "bastion", Tags: []string{"db"}},
			{Name: "web1", Address: "10.0.1.10", User: "deploy", Group: "prod", Tags: []string{"web", "prod"}},
		},
	}
	m.list.SetItems(m.buildItems())
	m.retitle()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return updated.(model)
}

// Regression test: bubbles' help.ShortHelpView gives up truncating (and
// dumps every remaining binding unbounded) once an overflowing item leaves
// no room even for its "…" ellipsis. With enough AdditionalShortHelpKeys
// registered this silently blew the whole framed box past the terminal
// width at width=100 while working fine at width=60 — never overflowing
// visibly in a quick narrow-terminal check, only a wider one.
func TestViewNeverExceedsTerminalWidth(t *testing.T) {
	for _, width := range []int{60, 80, 100, 120, 200} {
		m := testModel(width, 30)
		got := maxLineWidth(stripANSI(m.View()))
		if got > width {
			t.Errorf("width=%d: rendered a line %d columns wide (overflow)", width, got)
		}
	}
}

func TestFullHelpViewNeverExceedsTerminalWidth(t *testing.T) {
	for _, width := range []int{60, 100, 200} {
		m := testModel(width, 30)
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})
		got := maxLineWidth(stripANSI(updated.(model).View()))
		if got > width {
			t.Errorf("width=%d (full help): rendered a line %d columns wide (overflow)", width, got)
		}
	}
}

func TestPaletteViewNeverExceedsTerminalWidth(t *testing.T) {
	for _, width := range []int{60, 100, 200} {
		m := testModel(width, 30)
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(":")})
		got := maxLineWidth(stripANSI(updated.(model).View()))
		if got > width {
			t.Errorf("width=%d (palette): rendered a line %d columns wide (overflow)", width, got)
		}
	}
}

func TestDeleteConfirmViewNeverExceedsTerminalWidth(t *testing.T) {
	for _, width := range []int{60, 100, 200} {
		m := testModel(width, 30)
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
		got := maxLineWidth(stripANSI(updated.(model).View()))
		if got > width {
			t.Errorf("width=%d (delete confirm): rendered a line %d columns wide (overflow)", width, got)
		}
	}
}

func TestTitleShowsVersion(t *testing.T) {
	old := Version
	defer func() { Version = old }()
	for v, want := range map[string]string{"0.15.0": "meterm v0.15.0  ·", "v0.15.0": "meterm v0.15.0  ·", "dev": "meterm dev  ·", "": "meterm dev  ·"} {
		Version = v
		m := testModel(120, 40)
		m.retitle()
		if !strings.HasPrefix(m.list.Title, want) {
			t.Errorf("Version %q: title %q", v, m.list.Title)
		}
	}
}
