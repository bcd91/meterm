package tui

import (
	"strings"
	"testing"

	"meterm/internal/config"
)

// Two hosts that differ only by a hidden character must not look the same
// in their rows: the dirty one is shown spelled out and quoted, as `meterm
// list` does. A name mixing Latin and Cyrillic is flagged under the table.
func TestRowsTellLookalikesApart(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	writeHosts(t, m.cfgPath,
		config.Host{Name: "zwsp", Address: "192.0.2.90"},
		config.Host{Name: "zw\u200bsp", Address: "192.0.2.91"},
		config.Host{Name: "p\u0430y1", Address: "192.0.2.92"}) // Cyrillic а
	m = drive(t, m, nil, "r")
	m = resized(t, m, 100, 20)
	v := stripANSI(m.View())
	if !strings.Contains(v, `"zw\u200bsp"`) {
		t.Errorf("the dirty name isn't spelled out in its row:\n%s", v)
	}
	rows := 0
	for _, l := range strings.Split(v, "\n") {
		if strings.Contains(l, " zwsp ") {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("%d rows read zwsp:\n%s", rows, v)
	}
	m.reselect("p\u0430y1")
	if v := stripANSI(m.View()); !strings.Contains(v, "name mixes Latin and Cyrillic letters") {
		t.Errorf("look-alike not flagged:\n%s", v)
	}
	m.reselect("zwsp")
	if v := stripANSI(m.View()); strings.Contains(v, "mixes") {
		t.Errorf("a plain name flagged:\n%s", v)
	}
}
