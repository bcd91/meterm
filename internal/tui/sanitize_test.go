package tui

import (
	"strings"
	"testing"

	"meterm/internal/config"
)

// evilHost has an escape sequence or a bidi override in every text field
// that reaches the screen.
func evilHost() config.Host {
	return config.Host{
		Name:      "evil\x1b]0;pwned\x07x",
		Address:   "192.0.2.7\x1b[2J",
		User:      "ro\x1b[5mot",
		Users:     []string{"adm\x1b]52;c;aGk=\x07in"},
		Group:     "g\x1b[5m",
		Tags:      []string{"t‮ag", "w\x1b[31meb"},
		ProxyJump: "jump\x1b[1;1H.example",
		Notes:     "n\x1b]8;;http://x\x07ote",
		Team:      "git:x",
	}
}

// injected reports what in s came from the hostile host text rather than
// from the picker's own styling.
func injected(s string) []string {
	var found []string
	for _, bad := range []string{"\x1b]", "\x07", "\x1b[5m", "\x1b[2J", "\x1b[31m", "\x1b[1;1H", "‮"} {
		if strings.Contains(s, bad) {
			found = append(found, strings.ReplaceAll(bad, "\x1b", "ESC"))
		}
	}
	return found
}

func TestHostTextNeverReachesTheTerminalRaw(t *testing.T) {
	m := testModel(t, 120, 40)
	h := evilHost()
	m.cfg.Hosts = []config.Host{h}
	m.teams = map[string]string{"git:x": "te\x1b]0;am\x07"}
	m.attention = map[string]string{h.Name: "conf\x1b[5mlict"}
	m.refreshItems()
	m.list.Select(0)
	it := m.list.SelectedItem().(item)

	check := func(what, s string) {
		t.Helper()
		if bad := injected(s); len(bad) > 0 {
			t.Errorf("%s shows %v raw: %q", what, bad, s)
		}
	}
	lay := layoutFor(m.list.Items(), 116)
	check("row", it.row(lay, nil, true))
	check("detail", it.detail(layout{}))
	check("FilterValue", it.FilterValue())
	check("list view", m.View())

	check("delete confirm", press(m, "d").View())

	login := press(m, "enter") // two logins: the picker
	if login.mode != modePick {
		t.Fatalf("enter on a host with two logins: mode %v", login.mode)
	}
	check("login picker", login.View())

	pal := press(m, ":", "l", "o", "g", "s")
	check("palette hint", pal.paletteHint())
	pal = press(m, ":", "c", "o", "n", "n", "e", "c", "t", " ", "e", "tab")
	check("palette view after tab", pal.View())

	check("tunnel flash", press(m, "t").View())
}

// R and x prefill the palette with a reference to the host's own name, so no
// escape reaches the screen and what runs is that host, not one whose name
// shows the same (see TestPrefillActsOnTheSelectedHostItself).
func TestPrefillQuotesAControlName(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	writeHosts(t, m.cfgPath, config.Host{Name: "evil\x1b]52;c;SGVsbG8=\x07name", Address: "192.0.2.80"})
	m = drive(t, m, nil, "r")
	p := press(m, "R")
	if p.palette.Value() != `rename ‹evil\x1b]52;c;SGVsbG8=\aname› ` {
		t.Errorf("R: %q", p.palette.Value())
	}
	p = press(m, "x")
	if p.palette.Value() != `exec ‹evil\x1b]52;c;SGVsbG8=\aname› -- ` {
		t.Errorf("x: %q", p.palette.Value())
	}
	if got := splitArgs(p.palette.Value()+"uptime", p.refs); len(got) != 4 || got[1] != "evil\x1b]52;c;SGVsbG8=\x07name" {
		t.Errorf("the prefill reads back as %q", got)
	}
}

// A production host named as shown still asks first.
func TestCleanedNameOfAProductionHostAsks(t *testing.T) {
	cfg := &config.Config{Hosts: []config.Host{{Name: "pay\x1b[5m1", Address: "192.0.2.81", Env: config.EnvProduction},
		{Name: "web1", Address: "192.0.2.82"}}}
	if got := productionTargets([]string{"exec", "web1", "pay1", "--", "uptime"}, cfg); len(got) != 1 {
		t.Errorf("exec pay1 reaches %v", got)
	}
}
