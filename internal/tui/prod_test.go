package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"meterm/internal/config"
)

// step presses one key and says whether it started a subprocess.
func step(t *testing.T, m model, k string) (model, bool) {
	t.Helper()
	next, cmd := m.Update(keyMsg(k))
	ran := false
	for _, msg := range run(cmd) {
		ran = ran || fmt.Sprintf("%T", msg) == "tea.execMsg"
	}
	return next.(model), ran
}

func prodFleet(t *testing.T) model {
	t.Helper()
	m := fleet(t)
	slowDial(t)
	writeHosts(t, m.cfgPath,
		config.Host{Name: "pay1", Address: "192.0.2.30", Env: config.EnvProduction, Tags: []string{"db"},
			Forwards: []config.Forward{{Name: "ui", Type: "L", LocalPort: 8080, RemoteHost: "localhost", RemotePort: 80}}},
		config.Host{Name: "pay2", Address: "192.0.2.31", Env: config.EnvProduction, User: "app", Users: []string{"root"}},
		config.Host{Name: "stage1", Address: "192.0.2.32", Env: config.EnvStaging},
		config.Host{Name: "dev1", Address: "192.0.2.33", Tags: []string{"web"}, Forwards: []config.Forward{
			{Name: "db", Type: "L", LocalPort: 5432, RemoteHost: "localhost", RemotePort: 5432, Note: "prod db"},
			{Name: "web", Type: "L", LocalPort: 8081, RemoteHost: "localhost", RemotePort: 80}}})
	return drive(t, m, nil, "r")
}

// Enter on a production host asks first, naming it; only y goes.
func TestConnectToProductionAsks(t *testing.T) {
	m := prodFleet(t)
	m.reselect("pay1")
	m, ran := step(t, m, "enter")
	if ran || m.mode != modeConfirmRun {
		t.Fatalf("enter on production: ran %v, mode %v", ran, m.mode)
	}
	v := stripANSI(m.View())
	for _, want := range []string{"PRODUCTION", "pay1", "meterm connect pay1"} {
		if !strings.Contains(v, want) {
			t.Errorf("confirm lacks %q:\n%s", want, v)
		}
	}
	for _, w := range []int{40, 60, 120} {
		next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		if got := maxLineWidth(stripANSI(next.(model).View())); got > w {
			t.Errorf("width %d: the question is %d wide", w, got)
		}
	}
	m.mode = modeList
	for _, k := range []string{"n", "esc", "q", " ", "enter"} {
		m.reselect("pay1")
		m, _ = step(t, m, "enter")
		var ran bool
		if m, ran = step(t, waited(m), k); ran || m.mode != modeList {
			t.Errorf("%q: ran %v, mode %v", k, ran, m.mode)
		}
	}
	for _, k := range []string{"y", "Y"} {
		m.reselect("pay1")
		m, _ = step(t, m, "enter")
		if m, ran = step(t, waited(m), k); !ran {
			t.Errorf("%q didn't connect", k)
		}
	}
	// A plain host connects at once.
	m.reselect("dev1")
	if _, ran := step(t, m, "enter"); !ran {
		t.Error("enter on a dev host didn't connect")
	}
}

// Every login of a production host is marked, and choosing one asks.
func TestLoginPickerOnProductionAsks(t *testing.T) {
	m := prodFleet(t)
	m.reselect("pay2")
	m, _ = step(t, m, "enter")
	if m.mode != modePick {
		t.Fatalf("mode %v", m.mode)
	}
	if v := stripANSI(m.View()); strings.Count(v, "PRODUCTION") != len(m.picks) {
		t.Errorf("not every login is marked:\n%s", v)
	}
	m, ran := step(t, m, "down")
	if m, ran = step(t, m, "enter"); ran || m.mode != modeConfirmRun || !strings.Contains(stripANSI(m.View()), "root@pay2") {
		t.Fatalf("picking a production login: ran %v mode %v", ran, m.mode)
	}
	if _, ran = step(t, waited(m), "y"); !ran {
		t.Error("y didn't connect")
	}
}

// Shortcuts that run something on the host ask too; ones that stay on this
// machine don't.
func TestShortcutsOnProductionAsk(t *testing.T) {
	m := prodFleet(t)
	for _, k := range []string{"M", "f", "S", "i", "A", "L", "w", "t"} {
		m.reselect("pay1")
		next, ran := step(t, m, k)
		if ran || next.mode != modeConfirmRun {
			t.Errorf("%s on production: ran %v, mode %v", k, ran, next.mode)
		}
	}
	for _, k := range []string{"e", "C"} {
		m.reselect("pay1")
		if next, ran := step(t, m, k); !ran || next.mode != modeList {
			t.Errorf("%s (local) on production: ran %v, mode %v", k, ran, next.mode)
		}
	}
	for _, k := range []string{"M", "f", "S", "A"} {
		m.reselect("dev1")
		if next, ran := step(t, m, k); !ran || next.mode == modeConfirmRun {
			t.Errorf("%s on a dev host asked", k)
		}
	}
}

// The palette asks when what it runs reaches a production host, by name,
// by user@name, or through --all, --tag or --group.
func TestPaletteOnProductionAsks(t *testing.T) {
	m := prodFleet(t)
	m.reselect("dev1")
	for line, asks := range map[string]bool{
		"exec pay1 -- uptime":     true,
		"connect root@pay2":       true,
		"connect root@dev1":       false,
		"cp pay1:/etc/hosts /tmp": true,
		"tunnel up pay1/ui":       true,
		"exec --all -- uptime":    true,
		"exec --tag db -- uptime": true,
		"exec --tag web -- w":     false,
		"exec dev1 -- uptime":     false,
		"logs":                    false, // fills in the selected host, dev1
		"list":                    false,
		"edit pay1":               false,
		"audit":                   true, // every host
		"team status":             false,
		"tunnel enable pay1/ui":   true, // open from every login
		"tunnel enable dev1/db":   true, // its note says production
		"tunnel up dev1/db":       true,
		"tunnel up dev1/web":      false,
		"tunnel list":             false,
	} {
		p := press(m, ":")
		p.palette.SetValue(line)
		p, ran := step(t, p, "enter")
		if asks && (ran || p.mode != modeConfirmRun) {
			t.Errorf("%q: ran %v, mode %v; want a question", line, ran, p.mode)
		}
		if !asks && (!ran || p.mode == modeConfirmRun) {
			t.Errorf("%q: ran %v, mode %v; want it run", line, ran, p.mode)
		}
		if v := stripANSI(p.View()); asks && !strings.Contains(v, "pay") && !strings.Contains(v, "leads to production") {
			t.Errorf("%q: the question doesn't name the host:\n%s", line, stripANSI(p.View()))
		}
	}
}

// Staging is marked; a host without an env isn't given one.
func TestEnvMarkers(t *testing.T) {
	m := prodFleet(t)
	rows := map[string]string{}
	lay := layoutFor(m.list.Items(), 116)
	for _, it := range m.list.Items() {
		rows[it.(item).host.Name] = stripANSI(it.(item).row(lay, nil, false))
	}
	if !strings.Contains(rows["stage1"], "staging") {
		t.Errorf("staging row: %q", rows["stage1"])
	}
	if !strings.Contains(rows["pay1"], "prod") {
		t.Errorf("production row: %q", rows["pay1"])
	}
	if strings.Contains(rows["dev1"], "prod") || strings.Contains(rows["dev1"], "staging") || strings.Contains(rows["dev1"], "dev ") {
		t.Errorf("unknown env marked: %q", rows["dev1"])
	}
	// Typing the env finds the hosts.
	m = drive(t, m, nil, "/", "s", "t", "a", "g", "i", "n", "g", "enter")
	if v := m.list.VisibleItems(); len(v) == 0 || v[0].(item).host.Name != "stage1" {
		t.Errorf("filter staging: %d rows", len(v))
	}
}

// dev has a badge of its own, unset has none and the detail line says so,
// and a typo is production with the value flagged.
func TestEnvBadgesAndDetail(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	writeHosts(t, m.cfgPath,
		config.Host{Name: "devbox", Address: "192.0.2.40", Env: config.EnvDev},
		config.Host{Name: "bare", Address: "192.0.2.41"},
		config.Host{Name: "typo", Address: "192.0.2.42", Env: "prodution"})
	m = drive(t, m, nil, "r")
	lay := layoutFor(m.list.Items(), 116)
	row, detail := map[string]string{}, map[string]string{}
	for _, it := range m.list.Items() {
		iv := it.(item)
		row[iv.host.Name] = stripANSI(iv.row(lay, nil, false))
		detail[iv.host.Name] = iv.detail(lay)
	}
	if !strings.Contains(row["devbox"], " dev ") {
		t.Errorf("dev row: %q", row["devbox"])
	}
	if strings.Contains(row["bare"], "dev") || !strings.Contains(detail["bare"], "env not set") {
		t.Errorf("unset: row %q detail %q", row["bare"], detail["bare"])
	}
	if !strings.Contains(row["typo"], "prod") || !strings.Contains(detail["typo"], `"prodution"`) || !strings.Contains(detail["typo"], "not recognised") {
		t.Errorf("typo: row %q detail %q", row["typo"], detail["typo"])
	}
}

// Two hosts show as zwsp: one is production and called that, the other a dev
// host with a zero-width space in its name. x and R on the dev row act on
// the dev host, and say so in a form that can't be mistaken for the other.
func TestPrefillActsOnTheSelectedHostItself(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	dirty := "zw\u200bsp"
	writeHosts(t, m.cfgPath,
		config.Host{Name: "zwsp", Address: "192.0.2.40", Env: config.EnvProduction},
		config.Host{Name: dirty, Address: "192.0.2.41", Env: config.EnvDev},
		config.Host{Name: "two words", Address: "192.0.2.42"},
		config.Host{Name: "it's", Address: "192.0.2.43"})
	m = drive(t, m, nil, "r")
	for _, c := range []struct {
		host, key, typed string
		want             []string
	}{
		{dirty, "x", "uptime", []string{"exec", dirty, "--", "uptime"}},
		{dirty, "R", "zwsp-dev", []string{"rename", dirty, "zwsp-dev"}},
		{"two words", "x", "uptime", []string{"exec", "two words", "--", "uptime"}},
		{"it's", "R", "its", []string{"rename", "it's", "its"}},
	} {
		m.reselect(c.host)
		p := press(m, c.key)
		if p.mode != modePalette {
			t.Fatalf("%s: mode %v", c.key, p.mode)
		}
		if v := p.palette.Value(); strings.ContainsRune(v, 0x200b) {
			t.Errorf("%s on %q: the palette holds an invisible character: %q", c.key, c.host, v)
		}
		p.palette.SetValue(p.palette.Value() + c.typed)
		if hint := stripANSI(p.paletteHint()); strings.ContainsRune(hint, 0x200b) || (c.host == dirty && !strings.Contains(hint, `\u200b`)) {
			t.Errorf("%s on %q: hint %q", c.key, c.host, hint)
		}
		p, ran := step(t, p, "enter")
		if !ran || p.mode == modeConfirmRun {
			t.Errorf("%s on %q: ran %v, mode %v (it isn't production)", c.key, c.host, ran, p.mode)
			continue
		}
		if strings.Join(p.running, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("%s on %q ran %q, want %q", c.key, c.host, p.running, c.want)
		}
	}
	// The production row's x still asks.
	m.reselect("zwsp")
	p := press(m, "x")
	p.palette.SetValue(p.palette.Value() + "uptime")
	if p, ran := step(t, p, "enter"); ran || p.mode != modeConfirmRun {
		t.Errorf("x on production: ran %v, mode %v", ran, p.mode)
	}
}

// The question shows the command it will run without the ↵ the palette
// uses: here Enter cancels. A host reached through production asks too,
// says why, and y hands the command the picker's one-use answer.
func TestProductionQuestionShowsNoEnterGlyph(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	writeHosts(t, m.cfgPath,
		config.Host{Name: "bastion", Address: "192.0.2.40", Env: config.EnvProduction},
		config.Host{Name: "inner", Address: "192.0.2.41", ProxyJump: "bastion"})
	m = drive(t, m, nil, "r")
	m.reselect("inner")
	m, ran := step(t, m, "enter")
	if ran || m.mode != modeConfirmRun {
		t.Fatalf("enter on a host behind production: ran %v, mode %v", ran, m.mode)
	}
	v := stripANSI(m.View())
	if strings.Contains(v, "↵") {
		t.Errorf("the question shows ↵ while enter cancels:\n%s", v)
	}
	if !strings.Contains(v, "meterm connect inner") || !strings.Contains(v, "reached through production") {
		t.Errorf("question:\n%s", v)
	}
	m, ran = step(t, waited(m), "y")
	if !ran || m.lastDispatch == nil {
		t.Fatal("y didn't run it")
	}
	if !strings.Contains(strings.Join(m.lastDispatch.Env, "\n"), "METERM_PROD_TOKEN=") {
		t.Error("y didn't hand the command the picker's answer")
	}
}
