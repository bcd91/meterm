package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"meterm/internal/config"
	"meterm/internal/health"
)

// drive presses keys like press does, but also runs the commands Update
// returns and feeds their messages back, the way bubbletea would: a
// command whose message is dropped is a bug these tests are after. A
// dispatched subprocess (tea.ExecProcess) isn't run; it comes back as
// execDoneMsg{err: execErr}. Commands that don't answer at once (cursor
// blink ticks) are left alone.
func drive(t *testing.T, m model, execErr error, keys ...string) model {
	t.Helper()
	for _, k := range keys {
		if k == "wait" { // let the question on screen settle (see ConfirmSettle)
			m = waited(m)
			continue
		}
		m = feed(t, m, keyMsg(k), execErr, 0)
	}
	return m
}

func feed(t *testing.T, m model, msg tea.Msg, execErr error, depth int) model {
	t.Helper()
	if depth > 20 {
		t.Fatalf("message loop: %T", msg)
	}
	next, cmd := m.Update(msg)
	m = next.(model)
	for _, out := range run(cmd) {
		if fmt.Sprintf("%T", out) == "tea.execMsg" {
			out = execDoneMsg{err: execErr}
		}
		m = feed(t, m, out, execErr, depth+1)
	}
	return m
}

// run executes cmd and returns the messages it produces, flattening batches
// and sequences, and skipping anything that takes longer than a moment.
func run(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(50 * time.Millisecond):
		return nil
	}
	switch msg := msg.(type) {
	case nil:
		return nil
	case tea.BatchMsg:
		var out []tea.Msg
		for _, c := range msg {
			out = append(out, run(c)...)
		}
		return out
	}
	if fmt.Sprintf("%T", msg) == "tea.sequenceMsg" {
		return nil // not used by the picker
	}
	return []tea.Msg{msg}
}

// noDial makes health checks answer at once, down, without the network.
func noDial(t *testing.T) {
	t.Helper()
	old := dialHealth
	dialHealth = func(context.Context, string) error { return refused }
	t.Cleanup(func() { dialHealth = old })
}

func fleet(t *testing.T) model {
	t.Helper()
	noDial(t)
	m := testModel(t, 120, 40)
	var hosts []config.Host
	for i := 1; i <= 8; i++ {
		hosts = append(hosts, config.Host{Name: fmt.Sprintf("web%d", i), Address: fmt.Sprintf("192.0.2.%d", i)})
	}
	writeHosts(t, m.cfgPath, hosts...)
	m.reload()
	return m
}

// slowDial makes health checks outlast drive's patience, so their results
// never arrive and can't stand in for a refilter that was dropped.
func slowDial(t *testing.T) {
	t.Helper()
	dialHealth = func(context.Context, string) error { time.Sleep(time.Second); return refused }
}

// After a command run from a filtered list, the list must still show what
// matches the filter, not "0 items".
func TestFilterSurvivesARunCommand(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	m = drive(t, m, nil, "/", "w", "e", "b", "3", "enter")
	if n := len(m.list.VisibleItems()); n != 1 {
		t.Fatalf("filter web3 shows %d rows", n)
	}
	m = drive(t, m, nil, "enter") // connect, and come back
	if n := len(m.list.VisibleItems()); n != 1 || m.selectedName() != "web3" {
		t.Errorf("after the command: %d rows, selected %q", n, m.selectedName())
	}
	// A health result arriving while filtered shows on the filtered row.
	m = feed(t, m, resultFor(m, "web3", healthUp), nil, 0)
	if v := m.list.VisibleItems(); len(v) != 1 || v[0].(item).status != healthUp {
		t.Errorf("after a health result: %d rows, status %v", len(v), v[0].(item).status)
	}
	// Marking rebuilds the rows too.
	m = drive(t, m, nil, " ")
	if v := m.list.VisibleItems(); len(v) != 1 || !v[0].(item).marked {
		t.Errorf("after marking: %d rows", len(v))
	}
}

// writeHosts saves hosts to the host file at path.
func writeHosts(t *testing.T, path string, hosts ...config.Host) {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error { c.Hosts = hosts; return nil }); err != nil {
		t.Fatal(err)
	}
}

// When the command changed the rows (here: removed one above the cursor),
// the cursor stays on the same host, filter or not.
func TestSelectionFollowsTheHostAfterACommand(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	m = drive(t, m, nil, "/", "w", "e", "b", "enter", "down", "down", "down")
	want := m.selectedName()
	first := m.list.VisibleItems()[0].(item).host.Name
	if want == "" || want == first {
		t.Fatalf("setup: selected %q, first %q", want, first)
	}
	var keep []config.Host
	for _, h := range m.cfg.Hosts {
		if h.Name != first {
			keep = append(keep, h)
		}
	}
	writeHosts(t, m.cfgPath, keep...)
	m = feed(t, m, execDoneMsg{}, nil, 0)
	if got := m.selectedName(); got != want {
		t.Errorf("selected %q after the command, want %q", got, want)
	}
}

// A command that fails says so once the picker is back.
func TestFailedCommandIsShown(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	m.reselect("web3")
	m = drive(t, m, errors.New("exit status 255"), "enter")
	v := stripANSI(m.View())
	if !strings.Contains(v, "connect web3") || !strings.Contains(v, "255") {
		t.Errorf("no failure shown:\n%s", v)
	}
	// One that worked leaves nothing behind.
	m = drive(t, m, nil, "enter")
	if v := stripANSI(m.View()); strings.Contains(v, "255") {
		t.Errorf("old failure still shown:\n%s", v)
	}
	// After a failed L, the palette doesn't open for a log that wasn't listed.
	m = drive(t, m, errors.New("exit status 1"), "L")
	if m.mode == modePalette {
		t.Error("palette opened after a failed logs list")
	}
}

// Marking moves to the next row, under a filter too.
func TestMarkAdvancesUnderAFilter(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	m = drive(t, m, nil, "/", "w", "e", "b", "enter")
	first := m.selectedName()
	m = drive(t, m, nil, " ")
	if got := m.selectedName(); got == first || got == "" {
		t.Errorf("after marking %s the cursor is on %q", first, got)
	}
	if !m.marked[first] {
		t.Errorf("%s not marked", first)
	}
}

// resultFor is the health answer state for the host called name, in m's
// current round, as if its check had just come back.
func resultFor(m model, name string, state healthState) healthResultMsg {
	h, _ := m.cfg.Find(name)
	key := ""
	if h != nil {
		key = hostKey(m.cfg, *h)
	}
	return healthResultMsg{name: name, key: key, round: m.round, res: health.Result{State: state, At: m.now()}}
}
