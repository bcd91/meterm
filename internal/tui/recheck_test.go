package tui

import (
	"context"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"meterm/internal/config"
	"meterm/internal/health"
)

var refused = &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}

// answering makes the health check's dial answer for the addresses in up
// and refuse the rest, at once.
func answering(t *testing.T, up ...string) {
	t.Helper()
	ok := map[string]bool{}
	for _, a := range up {
		ok[a] = true
	}
	old := dialHealth
	dialHealth = func(_ context.Context, addr string) error {
		if ok[addr] {
			return nil
		}
		return refused
	}
	t.Cleanup(func() { dialHealth = old })
}

// clock gives m a clock the test moves.
func clock(m *model) *time.Time {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	return &now
}

func detailOf(m model, name string) string {
	m.reselect(name)
	lines := strings.Split(stripANSI(m.View()), "\n")
	for k, l := range lines {
		if strings.HasPrefix(l, "╰") && k > 0 {
			return lines[k-1]
		}
	}
	return ""
}

func itemOf(m model, name string) item {
	for _, it := range m.list.Items() {
		if iv := it.(item); iv.host.Name == name {
			return iv
		}
	}
	return item{}
}

// The check goes the way connect would: a host whose own address is down
// but whose vpn route answers is up, via vpn, and the detail line says so
// and when; one that's down says why.
func TestHealthFollowsRoutesAndSaysWhyAndWhen(t *testing.T) {
	m := fleet(t)
	now := clock(&m)
	answering(t, "198.51.100.10:22")
	writeHosts(t, m.cfgPath,
		config.Host{Name: "box", Address: "192.0.2.10", Routes: []config.Route{{Name: "vpn", Address: "198.51.100.10"}}},
		config.Host{Name: "dead", Address: "192.0.2.11"},
		config.Host{Name: "inner", Address: "10.0.0.5", ProxyJump: "box"})
	m = drive(t, m, nil, "r")
	if st := itemOf(m, "box").status; st != healthUp {
		t.Errorf("box: %v, want up", st)
	}
	*now = now.Add(2 * time.Minute)
	if d := detailOf(m, "box"); !strings.Contains(d, "up via vpn, 2m ago") {
		t.Errorf("box detail: %q", d)
	}
	*now = now.Add(-90 * time.Second)
	if d := detailOf(m, "dead"); !strings.Contains(d, "down: connection refused, 30s ago") {
		t.Errorf("dead detail: %q", d)
	}
	// inner goes through box, which connect reaches over its vpn route.
	if d := detailOf(m, "inner"); !strings.Contains(d, "via box: box up, the host itself not checked, 30s ago") {
		t.Errorf("inner detail: %q", d)
	}
}

// A result from an earlier round, or for an address the host no longer
// has, never lands: after r or an edit the host shows checking until its
// own answer comes.
func TestStaleHealthResultsAreDropped(t *testing.T) {
	m := fleet(t)
	clock(&m)
	answering(t, "192.0.2.3:22")
	cmds := m.healthCmds()
	var old []healthResultMsg
	for _, c := range cmds {
		old = append(old, c().(healthResultMsg))
	}
	slowDial(t)
	m = drive(t, m, nil, "r") // a new round; its answers are slow
	for _, msg := range old {
		m = feed(t, m, msg, nil, 0)
	}
	if st := itemOf(m, "web3").status; st != healthUnknown {
		t.Errorf("an old round's answer landed: %v", st)
	}
	// Same round, but the address changed since it was asked.
	answering(t, "192.0.2.3:22")
	cur := m.healthCmds()
	var msg healthResultMsg
	for _, c := range cur {
		if r := c().(healthResultMsg); r.name == "web3" {
			msg = r
		}
	}
	var hosts []config.Host
	for _, h := range m.cfg.Hosts {
		if h.Name == "web3" {
			h.Address = "192.0.2.99"
		}
		hosts = append(hosts, h)
	}
	m.cfg.Hosts = hosts
	m = feed(t, m, msg, nil, 0)
	if st := itemOf(m, "web3").status; st != healthUnknown {
		t.Errorf("an answer for the old address landed: %v", st)
	}
}

// Every few minutes the hosts are checked again in the background: the old
// marks stay (dimmed once old) until the new ones come.
func TestHealthRechecksInTheBackground(t *testing.T) {
	m := fleet(t)
	now := clock(&m)
	answering(t, "192.0.2.3:22")
	m = drive(t, m, nil, "r")
	if itemOf(m, "web3").status != healthUp {
		t.Fatal("setup: web3 not up")
	}
	m = feed(t, m, tickMsg{}, nil, 0)
	if itemOf(m, "web3").check.At != *now {
		t.Error("checked again before it was due")
	}
	*now = now.Add(recheckEvery + time.Second)
	if !itemOf(m, "web3").staleAt(*now) {
		t.Error("an old mark isn't stale")
	}
	slowDial(t)
	m = feed(t, m, tickMsg{}, nil, 0)
	if st := itemOf(m, "web3").status; st != healthUp {
		t.Errorf("the background check wiped the old mark: %v", st)
	}
	answering(t)
	m = feed(t, m, tickMsg{}, nil, 0) // not due again yet: nothing
	*now = now.Add(recheckEvery + time.Second)
	m = feed(t, m, tickMsg{}, nil, 0)
	if it := itemOf(m, "web3"); it.status != healthDown || it.check.At != *now || it.staleAt(*now) {
		t.Errorf("after the recheck: %v at %v", it.status, it.check.At)
	}
	// Coming back to the picker after a while checks again too.
	*now = now.Add(time.Minute)
	answering(t, "192.0.2.3:22")
	m = feed(t, m, focusMsg(), nil, 0)
	if it := itemOf(m, "web3"); it.status != healthUp {
		t.Errorf("focus didn't check again: %v", it.status)
	}
	_ = health.Up
}

func focusMsg() tea.Msg { return tea.FocusMsg{} }
