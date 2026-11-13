package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"meterm/internal/cellwidth"
	"meterm/internal/config"
	"meterm/internal/history"
)

// bigFleet is 48 hosts with Thai, CJK, emoji and long names, production
// and staging groups, at width × height.
func bigFleet(t *testing.T, width, height int) model {
	t.Helper()
	noDial(t)
	m := newModel(t)
	names := []string{"เซิร์ฟเวอร์หลัก", "ฐานข้อมูลสำรอง", "🚀rocket", "日本サーバー",
		"very-long-hostname-for-the-reporting-cluster-node-07"}
	for len(names) < 48 {
		names = append(names, fmt.Sprintf("web%d", len(names)))
	}
	groups := []string{"live", "stage", "", "office"}
	var hosts []config.Host
	for i, n := range names {
		hosts = append(hosts, config.Host{Name: n, Address: fmt.Sprintf("192.0.2.%d", i+1), User: "deploy",
			Group: groups[i%len(groups)], Tags: []string{"web"}, Notes: "note for " + n})
	}
	m.cfg = &config.Config{
		Groups: map[string]config.Group{"live": {Env: config.EnvProduction}, "stage": {Env: config.EnvStaging}},
		Hosts:  hosts,
	}
	m.refreshItems()
	m.retitle()
	next, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return next.(model)
}

// lineCells is each line's width in terminal cells, styling stripped.
func lineCells(s string) []int {
	var out []int
	for _, l := range strings.Split(s, "\n") {
		out = append(out, cellwidth.Width(stripANSI(l)))
	}
	return out
}

// checkScreen fails unless the view is exactly the terminal's size: every
// line of the frame w cells (counted as the terminal counts Thai), no line
// wider, no more lines than the terminal has, and the title on the first.
func checkScreen(t *testing.T, what string, m model) {
	t.Helper()
	v := m.View()
	lines := strings.Split(v, "\n")
	if len(lines) > m.height {
		t.Errorf("%s: %d lines on a %d-line terminal", what, len(lines), m.height)
	}
	for k, w := range lineCells(v) {
		plain := stripANSI(lines[k])
		framed := strings.HasPrefix(plain, "│") || strings.HasPrefix(plain, "╭") || strings.HasPrefix(plain, "╰")
		if w > m.width || (framed && w != m.width) {
			t.Errorf("%s: line %d is %d cells on a %d-cell terminal: %q", what, k, w, m.width, plain)
		}
	}
	if !strings.Contains(stripANSI(lines[0]), "meterm") {
		t.Errorf("%s: the title isn't on the first line: %q", what, stripANSI(lines[0]))
	}
}

// One line per host: at 120×35, 28 of 48 hosts show (it was 8), and the
// screen is exactly the terminal at every size down to 40×10.
func TestTableFitsTheTerminal(t *testing.T) {
	for _, sz := range [][2]int{{120, 35}, {80, 24}, {60, 20}, {40, 10}, {200, 60}} {
		m := bigFleet(t, sz[0], sz[1])
		what := fmt.Sprintf("%dx%d", sz[0], sz[1])
		checkScreen(t, what, m)
		// The top border, the header, the detail line, the bottom border
		// and one line of keys: the rest is hosts.
		if got, want := m.list.Height(), sz[1]-5; got != want {
			t.Errorf("%s: %d rows of hosts, want %d", what, got, want)
		}
		if strings.Count(stripANSI(m.View()), "deploy@")+strings.Count(stripANSI(m.View()), "⚑") == 0 {
			t.Errorf("%s: no address or env on screen", what)
		}
		// Scrolling: the frame doesn't change shape.
		for _, k := range []string{"pgdown", "pgdown", "down", "G"} {
			m = press(m, k)
			checkScreen(t, what+" after "+k, m)
		}
		// Help, a filter, a long message: the title stays on top.
		checkScreen(t, what+" help", press(m, "?"))
		checkScreen(t, what+" filtering", press(m, "/", "w", "e", "b"))
		f := m
		f.flash = strings.Repeat("a long message that goes on ", 20)
		next, _ := f.Update(healthResultMsg{name: "nobody"})
		checkScreen(t, what+" long message", next.(model))
	}
}

func TestTooSmallSaysSo(t *testing.T) {
	for _, sz := range [][2]int{{39, 10}, {40, 9}, {30, 8}, {10, 3}} {
		m := bigFleet(t, sz[0], sz[1])
		v := stripANSI(m.View())
		if !strings.Contains(v, "too small") || strings.Contains(v, "web1") {
			t.Errorf("%dx%d: %q", sz[0], sz[1], v)
		}
		for _, w := range lineCells(v) {
			if w > sz[0] {
				t.Errorf("%dx%d: a line %d wide", sz[0], sz[1], w)
			}
		}
		// Keys can't act on rows nobody can see.
		if _, cmd := m.Update(keyMsg("enter")); cmd != nil {
			t.Errorf("%dx%d: enter did something", sz[0], sz[1])
		}
	}
}

// A narrow terminal drops whole columns from the lowest priority, and keeps
// one line per host.
func TestNarrowDropsColumnsNotLines(t *testing.T) {
	wide := stripANSI(strings.Split(bigFleet(t, 160, 20).View(), "\n")[1])
	for _, col := range []string{"NAME", "ENV", "ADDRESS", "GROUP", "TAGS"} {
		if !strings.Contains(wide, col) {
			t.Errorf("160 wide: header %q lacks %s", wide, col)
		}
	}
	narrow := stripANSI(strings.Split(bigFleet(t, 40, 10).View(), "\n")[1])
	if !strings.Contains(narrow, "NAME") || !strings.Contains(narrow, "ENV") || strings.Contains(narrow, "TAGS") {
		t.Errorf("40 wide: header %q", narrow)
	}
	// The address it no longer has room for is on the detail line.
	m := bigFleet(t, 40, 10)
	if v := stripANSI(m.View()); !strings.Contains(v, "│ deploy@192.0.2.") {
		t.Errorf("40 wide: the selected host's address isn't shown:\n%s", v)
	}
}

// Notes show for the selected host only.
func TestNotesOnlyForTheSelectedHost(t *testing.T) {
	m := bigFleet(t, 120, 35)
	v := stripANSI(m.View())
	if n := strings.Count(v, "note for "); n != 1 {
		t.Errorf("%d notes on screen, want 1 (the selected host's)", n)
	}
}

func isQuit(cmd tea.Cmd) bool {
	for _, msg := range run(cmd) {
		if _, ok := msg.(tea.QuitMsg); ok {
			return true
		}
	}
	return false
}

// Esc clears the filter, then the marks, then does nothing; it never quits.
func TestEscNeverQuits(t *testing.T) {
	m := bigFleet(t, 100, 30)
	m = drive(t, m, nil, " ", "/", "w", "e", "b", "1", "enter")
	if len(m.marked) != 1 || len(m.list.VisibleItems()) == 48 {
		t.Fatalf("setup: marks %v, %d rows", m.marked, len(m.list.VisibleItems()))
	}
	m = drive(t, m, nil, "esc")
	if len(m.list.VisibleItems()) != 48 || len(m.marked) != 1 {
		t.Errorf("first esc: %d rows, marks %v; want the filter cleared, marks kept", len(m.list.VisibleItems()), m.marked)
	}
	m = drive(t, m, nil, "esc")
	if len(m.marked) != 0 {
		t.Errorf("second esc kept the marks")
	}
	for i := 0; i < 3; i++ {
		next, cmd := m.Update(keyMsg("esc"))
		if isQuit(cmd) {
			t.Fatal("esc quit the picker")
		}
		m = next.(model)
	}
	if _, cmd := m.Update(keyMsg("q")); !isQuit(cmd) {
		t.Error("q doesn't quit")
	}
	if _, cmd := m.Update(keyMsg("ctrl+c")); !isQuit(cmd) {
		t.Error("ctrl+c doesn't quit")
	}
	f := press(m, "/", "w")
	if _, cmd := f.Update(keyMsg("ctrl+c")); !isQuit(cmd) {
		t.Error("ctrl+c doesn't quit while typing a filter")
	}
}

// Esc and the key after it can arrive together, as alt+key: it's taken as
// Esc, then the key.
func TestEscThenKeyIsNotAlt(t *testing.T) {
	m := bigFleet(t, 100, 30)
	m = drive(t, m, nil, "/", "w", "e", "b", "enter")
	m = drive(t, m, nil, "alt+j")
	if n := len(m.list.VisibleItems()); n != 48 {
		t.Errorf("alt+j after a filter: %d rows, want the filter cleared", n)
	}
	if m.list.Index() != 1 {
		t.Errorf("alt+j: cursor on row %d, want 1 (the j moved it)", m.list.Index())
	}
	if _, cmd := m.Update(keyMsg("alt+q")); !isQuit(cmd) {
		t.Error("alt+q (esc, q) doesn't quit")
	}
}

// s cycles recent → name → group → env, and says so in the title and
// header.
func TestSortCycles(t *testing.T) {
	m := bigFleet(t, 140, 30)
	first := func(m model) item { return m.list.VisibleItems()[0].(item) }
	m.hist = history.History{} // nothing connected: recent falls back to names
	for _, want := range []sortMode{sortName, sortGroup, sortEnv, sortRecent} {
		m = drive(t, m, nil, "s")
		if m.sort != want {
			t.Fatalf("sort %v, want %v", m.sort, want)
		}
		if !strings.Contains(stripANSI(m.View()), "sort: "+want.String()) {
			t.Errorf("%v: not shown", want)
		}
		switch want {
		case sortName:
			v := m.list.VisibleItems()
			for k := 1; k < len(v); k++ {
				if a, b := strings.ToLower(v[k-1].(item).host.Name), strings.ToLower(v[k].(item).host.Name); a > b {
					t.Errorf("name: %q before %q", a, b)
				}
			}
		case sortGroup:
			if first(m).host.Group != "live" {
				t.Errorf("group: first is in %q", first(m).host.Group)
			}
		case sortEnv:
			if !first(m).res.IsProduction() {
				t.Errorf("env: first is %q", first(m).host.Name)
			}
			last := m.list.VisibleItems()[47].(item)
			if last.envLabel() != "" {
				t.Errorf("env: last is %q (%s)", last.host.Name, last.envLabel())
			}
		}
	}
}

// A message longer than the line wraps to two lines and ends in "…"; the
// whole of it is in ? until the next one.
func TestLongMessageWrapsAndStaysInHelp(t *testing.T) {
	m := bigFleet(t, 60, 20)
	msg := "meterm connect web9 failed: exit status 255 because the host closed the connection before it said anything at all, " +
		"which it has now done twice in a row, so look at its sshd log"
	m.flash = msg
	next, _ := m.Update(healthResultMsg{name: "nobody"})
	m = next.(model)
	lines := strings.Split(stripANSI(m.View()), "\n")
	tail := strings.Join(lines[len(lines)-2:], " ")
	if !strings.Contains(tail, "exit status 255") || !strings.HasSuffix(strings.TrimSpace(tail), "…") {
		t.Errorf("footer: %q", tail)
	}
	if got := m.list.Height(); got != 20-6 {
		t.Errorf("rows %d with a two-line message, want %d", got, 20-6)
	}
	h := press(m, "?")
	all := strings.Join(h.helpLines(), " ")
	if !strings.Contains(strings.Join(strings.Fields(all), " "), "so look at its sshd log") {
		t.Errorf("? lacks the whole message:\n%s", all)
	}
}

// The questions fit too: bubbletea drops the top of a view taller than the
// terminal, which would take "⚠ PRODUCTION" with it.
func TestQuestionsFitSmallTerminals(t *testing.T) {
	for _, sz := range [][2]int{{40, 10}, {60, 20}, {80, 24}} {
		m := bigFleet(t, sz[0], sz[1])
		m.cfg.Hosts[0].Users = []string{"root", "admin", "backup", "ops"}
		m.cfg.Hosts[0].Team = "git:x"
		m.cfg.Groups["office"] = config.Group{Env: config.EnvProduction}
		m.refreshItems()
		m.reselect(m.cfg.Hosts[0].Name)
		what := fmt.Sprintf("%dx%d", sz[0], sz[1])
		run := m
		run.mode, run.pending, run.pendingProd = modeConfirmRun, []string{"exec", "--all", "--", "systemctl restart nginx and a long tail"}, []string{"a", "b", "c", "d", "e", "f", "g"}
		del := m
		del.mode, del.target = modeConfirmDelete, m.cfg.Hosts[0].Name
		pick := press(m, "enter")
		if pick.mode != modePick {
			t.Fatalf("%s: enter on a host with logins: mode %v", what, pick.mode)
		}
		for name, mm := range map[string]model{"production question": run, "delete question": del, "login picker": pick} {
			v := stripANSI(mm.View())
			lines := strings.Split(v, "\n")
			if len(lines) > sz[1] {
				t.Errorf("%s %s: %d lines on a %d-line terminal:\n%s", what, name, len(lines), sz[1], v)
			}
			for k, w := range lineCells(v) {
				if w > sz[0] {
					t.Errorf("%s %s: line %d is %d wide: %q", what, name, k, w, lines[k])
				}
			}
			if name != "login picker" && !strings.Contains(v, "PRODUCTION") && name == "production question" {
				t.Errorf("%s %s: PRODUCTION isn't on screen:\n%s", what, name, v)
			}
		}
	}
}

// A box around a Thai name with ำ (which lipgloss counts a column short)
// keeps its right border on the line: every line of the box is as wide as
// its top.
func TestBoxesAroundThaiNamesLineUp(t *testing.T) {
	m := bigFleet(t, 60, 20)
	m.reselect("ฐานข้อมูลสำรอง")
	m.cfg.Hosts[1].Users = []string{"root", "ผู้ดูแลระบบสำรอง"}
	m.refreshItems()
	m.reselect("ฐานข้อมูลสำรอง")
	for name, mm := range map[string]model{
		"delete":  press(m, "d"),
		"login":   press(m, "enter"),
		"palette": press(m, "R"),
	} {
		v := stripANSI(mm.View())
		box := 0
		for _, l := range strings.Split(v, "\n") {
			l = strings.TrimLeft(l, " ")
			w := cellwidth.Width(l)
			if strings.HasPrefix(l, "╭") {
				box = w
			}
			if strings.HasPrefix(l, "│") && box > 0 && w != box && !strings.Contains(l, "NAME") {
				t.Errorf("%s: a box line is %d wide, its top %d: %q", name, w, box, l)
			}
			if w > 60 {
				t.Errorf("%s: a line is %d wide on 60", name, w)
			}
		}
		if !strings.Contains(v, "ฐานข้อมูลสำรอง") {
			t.Errorf("%s: the name isn't shown:\n%s", name, v)
		}
	}
}

// The host-file error keeps what to do at the front, where a narrow screen
// still shows it, and ? has the whole of it.
func TestBannerSaysWhatToDoFirst(t *testing.T) {
	m := bigFleet(t, 40, 10)
	m.loadErr = fmt.Errorf("parse config /a/very/long/path/to/hosts.yaml: yaml: line 2: did not find expected node content")
	next, _ := m.Update(healthResultMsg{name: "nobody"})
	m = next.(model)
	checkScreenNoTitle := strings.Split(stripANSI(m.View()), "\n")
	if len(checkScreenNoTitle) > 10 {
		t.Errorf("%d lines on a 10-line terminal", len(checkScreenNoTitle))
	}
	if top := strings.Join(checkScreenNoTitle[:3], " "); !strings.Contains(top, "press r") {
		t.Errorf("the banner doesn't say what to do: %q", top)
	}
	if help := strings.Join(press(m, "?").helpLines(), " "); !strings.Contains(help, "expected node content") {
		t.Errorf("? lacks the error: %q", help)
	}
}
