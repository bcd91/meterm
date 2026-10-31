package tui

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"meterm/internal/config"
)

func TestWithSelectedHost(t *testing.T) {
	hosts := map[string]bool{"web1": true, "GW1": true}
	cases := []struct {
		in   string
		want string
	}{
		{"logs", "logs GW1"},
		{"logs -f", "logs GW1 -f"},
		{"logs /var/log/syslog -f", "logs GW1 /var/log/syslog -f"},
		{"logs web1 /var/log/syslog", "logs web1 /var/log/syslog"},
		{"logs -f web1", "logs -f web1"},
		{"logs list", "logs list GW1"},
		{"tunnel up", "tunnel up GW1"},
		{"tunnel up web1/api", "tunnel up web1/api"},
		{"connect", "connect GW1"},
		{"connect -- uptime", "connect GW1 -- uptime"},
		{"connect web1 -- uptime", "connect web1 -- uptime"},
		{"exec -- uptime", "exec GW1 -- uptime"},
		{"exec --tag web -- uptime", "exec --tag web -- uptime"},
		{"exec --all -- uptime", "exec --all -- uptime"},
		{"exec web1 -- uptime", "exec web1 -- uptime"},
		// The host is optional here: without one they cover every host, and
		// the palette must not quietly narrow that.
		{"audit", "audit"},
		{"keys list", "keys list"},
		{"route check", "route check"},
		{"tunnel list", "tunnel list"},
		// Destructive: never aimed at the selected host by default.
		{"rm", "rm"},
		{"keys revoke --comment x", "keys revoke --comment x"},
		{"team status", "team status"},
	}
	for _, c := range cases {
		got := strings.Join(withSelectedHost(strings.Fields(c.in), "GW1", hosts), " ")
		if got != c.want {
			t.Errorf("%q → %q, want %q", c.in, got, c.want)
		}
	}
	// A host whose name looks like a flag is never inserted.
	if got := withSelectedHost([]string{"logs"}, "-oProxyCommand=x", hosts); !reflect.DeepEqual(got, []string{"logs"}) {
		t.Errorf("flag-like host inserted: %v", got)
	}
	if got := withSelectedHost([]string{"logs"}, "", hosts); !reflect.DeepEqual(got, []string{"logs"}) {
		t.Errorf("no selection: %v", got)
	}
}

func TestComplete(t *testing.T) {
	old := Commands
	defer func() { Commands = old }()
	Commands = map[string][]string{
		"tunnel": {"add", "list", "rm", "up"}, "team": {"status", "sync"},
		"logs": {"list"}, "list": nil, "connect": nil, "cp": nil, "exec": nil,
	}
	cfg := &config.Config{Hosts: []config.Host{
		{Name: "DEVBOX-1", Forwards: []config.Forward{{Name: "db", Type: "L"}}},
		{Name: "DEV-API"},
		{Name: "GW1"},
	}}
	cases := []struct {
		in, want string
		cands    int
	}{
		{"tun", "tunnel ", 0},
		{"l", "l", 2}, // list, logs
		{"tunnel u", "tunnel up ", 0},
		{"team s", "team s", 2},
		{"connect g", "connect GW1 ", 0},
		{"connect dev", "connect DEV", 2},
		{"connect devb", "connect DEVBOX-1 ", 0},
		{"tunnel up DEVBOX-1/", "tunnel up DEVBOX-1/db ", 0},
		{"cp G", "cp GW1:", 0},
		{"cp GW1:/et", "cp GW1:/et", 0},
		{"exec GW1 -- up", "exec GW1 -- up", 0},
		{"connect --x", "connect --x", 0},
		{"connect 'g", "connect 'g", 0},
		{"connect zz", "connect zz", 0},
	}
	for _, c := range cases {
		got, cands := complete(c.in, cfg)
		if got != c.want || len(cands) != c.cands {
			t.Errorf("complete(%q) = %q %v, want %q with %d candidates", c.in, got, cands, c.want, c.cands)
		}
	}
}

func TestTunnelChoices(t *testing.T) {
	h := config.Host{Name: "API", Forwards: []config.Forward{
		{Type: "L", LocalPort: 2222, RemoteHost: "x", RemotePort: 22}, // unnamed: opens with connect, not a tunnel
		{Name: "api", Type: "L", LocalPort: 8080, RemoteHost: "localhost", RemotePort: 6304, Note: "Billing API (prod)"},
		{Name: "socks", Type: "D", LocalPort: 1080, Note: "evil\x1b[2Jnote"},
	}}
	cs := tunnelChoices(h, false)
	if len(cs) != 3 {
		t.Fatalf("got %d choices, want 2 tunnels + all", len(cs))
	}
	if !reflect.DeepEqual(cs[0].args, []string{"tunnel", "up", "API/api"}) || !cs[0].warn {
		t.Errorf("api choice = %+v", cs[0])
	}
	if cs[1].warn || strings.ContainsRune(cs[1].label, 0x1b) {
		t.Errorf("socks choice = %+v (control characters must be dropped)", cs[1])
	}
	if !reflect.DeepEqual(cs[2].args, []string{"tunnel", "up", "API"}) || !cs[2].warn {
		t.Errorf("all choice = %+v", cs[2])
	}
	if one := tunnelChoices(config.Host{Name: "B", Forwards: h.Forwards[1:2]}, false); len(one) != 1 {
		t.Errorf("one tunnel should not offer 'all': %+v", one)
	}
}

func TestExecPrefill(t *testing.T) {
	if got := execPrefill(nil, "GW1"); got != "exec GW1 -- " {
		t.Errorf("got %q", got)
	}
	if got := execPrefill([]string{"a", "b"}, "GW1"); got != "exec a b -- " {
		t.Errorf("got %q", got)
	}
	if got := execPrefill(nil, ""); got != "exec " {
		t.Errorf("got %q", got)
	}
}

func TestBadges(t *testing.T) {
	it := item{host: config.Host{Name: "GW1",
		Routes:   []config.Route{{Name: "a"}, {Name: "b"}},
		Forwards: []config.Forward{{Name: "db", Note: "prod db"}}},
		attention: "conflict"}
	b := stripANSI(it.badges())
	for _, want := range []string{"⇄2", "⇶1", "✗"} {
		if !strings.Contains(b, want) {
			t.Errorf("badges %q missing %q", b, want)
		}
	}
	if got := stripANSI(item{host: config.Host{Name: "x"}}.badges()); got != "" {
		t.Errorf("plain host has badges %q", got)
	}
	if got := stripANSI(item{host: config.Host{Name: "x"}, attention: "not pushed"}.badges()); got != " ↑" {
		t.Errorf("unpushed badge = %q", got)
	}
}

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func press(m model, keys ...string) model {
	for _, k := range keys {
		next, _ := m.Update(keyMsg(k))
		m = next.(model)
	}
	return m
}

func TestMarkAndExec(t *testing.T) {
	m := testModel(100, 30)
	first := m.selectedName()
	m = press(m, " ")
	second := m.selectedName()
	m = press(m, " ")
	if got := m.markedNames(); len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("marked = %v, want [%s %s]", got, first, second)
	}
	if !strings.Contains(stripANSI(m.View()), "✓") {
		t.Errorf("marks not shown")
	}
	m = press(m, "x")
	if m.mode != modePalette || m.palette.Value() != "exec "+first+" "+second+" -- " {
		t.Fatalf("x → mode %v value %q", m.mode, m.palette.Value())
	}
	m = press(m, "esc", "esc")
	if len(m.marked) != 0 || m.mode != modeList {
		t.Errorf("esc should close the palette, then drop the marks: %v %v", m.mode, m.marked)
	}
}

func TestPaletteHintShowsFilledHost(t *testing.T) {
	m := testModel(100, 30)
	sel := m.selectedName()
	m = press(m, ":", "l", "o", "g", "s")
	if hint := stripANSI(m.paletteHint()); !strings.Contains(hint, "meterm logs "+sel) {
		t.Errorf("hint %q should show the filled-in host %s", hint, sel)
	}
	if v := stripANSI(m.View()); maxLineWidth(v) > 100 {
		t.Errorf("palette view is %d wide", maxLineWidth(v))
	}
}

func TestTunnelKey(t *testing.T) {
	m := testModel(80, 30)
	m.cfg.Hosts = []config.Host{{Name: "API", Address: "10.0.0.1", Forwards: []config.Forward{
		{Name: "api", Type: "L", LocalPort: 8080, RemoteHost: "localhost", RemotePort: 6304, Note: "Billing API (prod)"},
		{Name: "web", Type: "L", LocalPort: 8081, RemoteHost: "localhost", RemotePort: 80},
	}}, {Name: "BARE", Address: "10.0.0.2"}}
	m.list.SetItems(m.buildItems())
	m.reselect("API")
	m = press(m, "t")
	if m.mode != modePick || len(m.picks) != 3 {
		t.Fatalf("t → mode %v picks %d", m.mode, len(m.picks))
	}
	v := stripANSI(m.View())
	if !strings.Contains(v, "PRODUCTION") || maxLineWidth(v) > 80 {
		t.Errorf("picker view (width %d):\n%s", maxLineWidth(v), v)
	}
	m = press(m, "esc")
	m.reselect("BARE")
	m = press(m, "t")
	if m.mode != modeList || !strings.Contains(stripANSI(m.View()), "no tunnels on BARE") {
		t.Errorf("a host without tunnels should say so")
	}
}

func TestKeyActions(t *testing.T) {
	want := map[string][]string{
		"A": {"audit", "GW1"}, "R": {"route", "check", "GW1"},
		"w": {"keys", "list", "GW1"}, "L": {"logs", "GW1"},
	}
	for k, w := range want {
		if got := keyAction(k, "GW1"); !reflect.DeepEqual(got, w) {
			t.Errorf("%s → %v, want %v", k, got, w)
		}
	}
}

// Every shortcut must be its own key: one that collided with another (or
// with the list's own movement keys) would silently never fire.
func TestShortcutKeysDontCollide(t *testing.T) {
	seen := map[string]string{}
	for _, b := range []struct {
		name string
		keys []string
	}{
		{"connect", keys.Connect.Keys()}, {"add", keys.Add.Keys()}, {"edit", keys.Edit.Keys()},
		{"files", keys.Files.Keys()}, {"stats", keys.Stats.Keys()}, {"copyid", keys.CopyID.Keys()},
		{"keygen", keys.Keygen.Keys()}, {"delete", keys.Delete.Keys()}, {"palette", keys.Palette.Keys()},
		{"refresh", keys.Refresh.Keys()}, {"quit", keys.Quit.Keys()},
		{"tunnel", keys.Tunnel.Keys()}, {"tmux", keys.Tmux.Keys()}, {"exec", keys.Exec.Keys()}, {"mark", keys.Mark.Keys()},
		{"audit", keys.Audit.Keys()}, {"logs", keys.Logs.Keys()}, {"routes", keys.Routes.Keys()},
		{"access", keys.Access.Keys()}, {"team", keys.Team.Keys()},
		{"list-nav", []string{"j", "k", "h", "l", "g", "G", "b", "u", "/", "?"}},
	} {
		for _, k := range b.keys {
			if prev, ok := seen[k]; ok {
				t.Errorf("key %q is bound to both %s and %s", k, prev, b.name)
			}
			seen[k] = b.name
		}
	}
}

func TestLoginChoices(t *testing.T) {
	cs := loginChoices(config.Host{Name: "API", User: "svc", Users: []string{"deploy", "svc", "root"}}, []string{"--tmux"})
	var got []string
	for _, c := range cs {
		got = append(got, strings.Join(c.args, " "))
	}
	want := []string{"connect API --tmux", "connect deploy@API --tmux", "connect root@API --tmux"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if cs := loginChoices(config.Host{Name: "X", Users: []string{"root"}}, nil); len(cs) != 2 || !strings.Contains(cs[0].label, "default user") {
		t.Errorf("inherited user: %+v", cs)
	}
	if cs := loginChoices(config.Host{Name: "X", User: "a"}, nil); len(cs) != 1 {
		t.Errorf("one login gives a picker: %+v", cs)
	}
}

func TestTeamBadgeAndFilter(t *testing.T) {
	it := item{host: config.Host{Name: "API", Team: "git:x"}, team: "ACME"}
	if !strings.Contains(stripANSI(it.badges()), "⧉ACME") {
		t.Errorf("badges = %q", stripANSI(it.badges()))
	}
	if !strings.Contains(it.FilterValue(), "ACME") {
		t.Error("team name not filterable")
	}
	if strings.Contains(stripANSI(item{host: config.Host{Name: "mine"}}.badges()), "⧉") {
		t.Error("personal host shows a team")
	}
}

func TestPaletteWarnsAboutShellSyntax(t *testing.T) {
	for s, want := range map[string]bool{
		"connect API -- whoami": false, "exec API -- 'df -h | grep sda'": false,
		"connect API -- \"echo '$(cat ~/.ssh/id_ed25519.pub)'\"": true, `connect API -- -t "sudo cp \$F x"`: true,
		"cp ~/x API:/tmp": true, "exec API -- echo $HOME": true,
	} {
		if got := shellish(s); got != want {
			t.Errorf("shellish(%q) = %v", s, got)
		}
	}
	m := testModel(100, 30)
	m = press(m, ":")
	m.palette.SetValue("connect web1 -- echo $(whoami)")
	m = press(m, "enter")
	if m.mode != modePalette || !strings.Contains(stripANSI(m.paletteHint()), "not a shell") {
		t.Fatalf("first enter should only warn: mode %v hint %q", m.mode, stripANSI(m.paletteHint()))
	}
	m = press(m, "enter")
	if m.mode != modeList {
		t.Error("second enter didn't run it")
	}
}

func TestProductionHostBadgeAndTunnelWarning(t *testing.T) {
	cfg := &config.Config{Groups: map[string]config.Group{"live": {Env: config.EnvProduction}}}
	h := config.Host{Name: "web", Address: "192.0.2.1", Group: "live",
		Forwards: []config.Forward{{Name: "ui", Type: "L", LocalPort: 8080, RemoteHost: "localhost", RemotePort: 80}}}
	it := item{host: h, res: cfg.Resolve(h)}
	if !strings.Contains(stripANSI(it.badges()), "⚑prod") {
		t.Errorf("no production badge: %q", stripANSI(it.badges()))
	}
	if cs := tunnelChoices(h, it.res.IsProduction()); !cs[0].warn {
		t.Error("a tunnel on a production host should warn")
	}
	if cs := tunnelChoices(h, false); cs[0].warn {
		t.Error("a plain tunnel on a plain host shouldn't warn")
	}
}

func TestExecKeepsTheTypedLine(t *testing.T) {
	line := `exec --tag web -- df -h | grep '/srv x'`
	got := execKeepsLine(line, splitArgs(line))
	want := []string{"exec", "--tag", "web", "--", `df -h | grep '/srv x'`}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("got %q", got)
	}
	for _, line := range []string{"exec -- uptime", "exec web1", "logs web1 -- a b"} {
		if got := execKeepsLine(line, splitArgs(line)); strings.Join(got, "\x00") != strings.Join(splitArgs(line), "\x00") {
			t.Errorf("%q changed: %q", line, got)
		}
	}
}
