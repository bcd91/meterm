package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/activity"
	"meterm/internal/config"
)

func savedNames(t *testing.T, path string) []string {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, h := range cfg.Hosts {
		out = append(out, h.Name)
	}
	return out
}

func has(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// d asks; only y deletes, and only the selected host.
func TestDeleteOnlyOnYes(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	m.reselect("web2")
	for _, k := range []string{"n", "esc", "enter", "d", "Y "} {
		m = drive(t, m, nil, "d")
		if m.mode != modeConfirmDelete || m.target != "web2" {
			t.Fatalf("d: mode %v target %q", m.mode, m.target)
		}
		m = drive(t, m, nil, "wait", k)
		if m.mode != modeList {
			t.Errorf("%q left the confirm box open", k)
		}
		if !has(savedNames(t, m.cfgPath), "web2") {
			t.Fatalf("%q deleted the host", k)
		}
		m.reselect("web2")
	}
	m = drive(t, m, nil, "d", "wait", "y")
	names := savedNames(t, m.cfgPath)
	if has(names, "web2") || len(names) != 7 {
		t.Errorf("after y: %v", names)
	}
	for _, it := range m.list.Items() {
		if it.(item).host.Name == "web2" {
			t.Error("web2 still listed")
		}
	}
}

// A delete that can't be saved says so and keeps the host.
func TestDeleteFailureIsShown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only directory")
	}
	m := fleet(t)
	slowDial(t)
	m.reselect("web2")
	dir := filepath.Dir(m.cfgPath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	m = drive(t, m, nil, "d", "wait", "y")
	if v := stripANSI(m.View()); !strings.Contains(v, "couldn't delete web2: permission denied") {
		t.Errorf("no error shown:\n%s", stripANSI(m.View()))
	}
	found := false
	for _, it := range m.list.Items() {
		found = found || it.(item).host.Name == "web2"
	}
	if !found {
		t.Error("web2 dropped from the list though it wasn't deleted")
	}
}

// The confirm box says what deleting really does: a team host goes for the
// whole team at the next push, and a production host is named as such.
func TestDeleteConfirmWarnsAboutTeamAndProduction(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	writeHosts(t, m.cfgPath,
		config.Host{Name: "shared", Address: "192.0.2.20", Team: "git:x", Env: config.EnvProduction},
		config.Host{Name: "mine", Address: "192.0.2.21"})
	m = drive(t, m, nil, "r")
	m.reselect("shared")
	v := stripANSI(drive(t, m, nil, "d").View())
	for _, want := range []string{"team push", "everyone", "PRODUCTION"} {
		if !strings.Contains(v, want) {
			t.Errorf("confirm box lacks %q:\n%s", want, v)
		}
	}
	m.reselect("mine")
	v = stripANSI(drive(t, m, nil, "d").View())
	if strings.Contains(v, "team push") || strings.Contains(v, "PRODUCTION") {
		t.Errorf("plain host warned:\n%s", v)
	}
}

// A delete from the picker is in the activity log, as `meterm rm` is.
func TestDeleteIsLogged(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	m.reselect("web2")
	m = drive(t, m, nil, "d", "wait", "y")
	dir, err := activity.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	l, err := activity.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	events, _ := l.All()
	if len(events) != 1 || events[0].Action != "rm" || events[0].Host != "web2" || events[0].Target != "192.0.2.2" {
		t.Errorf("events after delete: %+v", events)
	}
}

// Deleting a jump host says which hosts go through it: their jump would then
// name a host meterm no longer has, and ssh looks that up by itself.
func TestDeleteJumpHostNamesWhoUsesIt(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	writeHosts(t, m.cfgPath,
		config.Host{Name: "bastion", Address: "192.0.2.1"},
		config.Host{Name: "web11", Address: "10.0.0.11", ProxyJump: "bastion"},
		config.Host{Name: "web12", Address: "10.0.0.12"})
	m = drive(t, m, nil, "r")
	m.reselect("bastion")
	v := stripANSI(press(m, "d").View())
	for _, want := range []string{"web11", "jump"} {
		if !strings.Contains(v, want) {
			t.Errorf("delete question lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "web12") {
		t.Errorf("names a host that doesn't jump through it:\n%s", v)
	}
	m.reselect("web12")
	if v := stripANSI(press(m, "d").View()); strings.Contains(v, "jump") {
		t.Errorf("a host nothing jumps through:\n%s", v)
	}
}
