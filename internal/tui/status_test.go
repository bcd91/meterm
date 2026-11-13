package tui

import (
	"strings"
	"testing"
	"time"

	"meterm/internal/config"
	"meterm/internal/health"
)

// Up and down must differ in shape, not only colour: red and green look
// alike to many people, and a no-colour terminal shows neither.
func TestStatusMarksDifferInShape(t *testing.T) {
	marks := map[healthState]string{}
	for _, st := range []healthState{healthUnknown, healthUp, healthDown, healthSkipped} {
		it := item{host: config.Host{Name: "h"}, status: st}
		marks[st] = stripANSI(it.statusMark())
	}
	seen := map[string]healthState{}
	for st, m := range marks {
		if prev, dup := seen[m]; dup {
			t.Errorf("states %v and %v both show %q", prev, st, m)
		}
		seen[m] = st
	}
	legend := strings.Join(pickerHelp, "\n")
	for st, word := range map[healthState]string{healthUp: "up", healthDown: "down", healthUnknown: "checking", healthSkipped: "not checked"} {
		if !strings.Contains(legend, marks[st]+" "+word) {
			t.Errorf("legend %q lacks %q", legend, marks[st]+" "+word)
		}
	}
}

// r checks again from scratch: old results mustn't pass for new ones.
func TestRefreshForgetsOldResults(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	m = feed(t, m, resultFor(m, "web1", healthUp), nil, 0)
	m = drive(t, m, nil, "r")
	for _, it := range m.list.Items() {
		if st := it.(item).status; st != healthUnknown {
			t.Errorf("%s still shows %v after r", it.(item).host.Name, st)
		}
	}
}

// The keys that changed meaning come first on the ? screen: d used to turn
// the page and now deletes, f now opens files, and h l b u turn nothing.
func TestHelpPutsThePagingKeysFirst(t *testing.T) {
	var keys []string
	for _, l := range keysChanged[1:] {
		if k, _, ok := strings.Cut(l, "\t"); ok {
			keys = append(keys, strings.TrimSpace(k))
		}
	}
	if len(keys) < 4 || keys[0] != "d" || keys[1] != "f" || !strings.Contains(keys[2], "h l b u") || !strings.Contains(keys[3], "pgup") {
		t.Errorf("first changed keys: %q", keys)
	}
	for _, k := range []string{"h", "l", "b", "u", "left", "right"} {
		if km := listKeys(); km.PrevPage.Keys()[0] == k || km.NextPage.Keys()[0] == k {
			t.Errorf("%s still pages", k)
		}
	}
}

// While the filter is being typed, ? and q are letters of it: the footer says
// what does work then, not "? help  q quit".
func TestFilterFooterShowsTheFilterKeys(t *testing.T) {
	m := fleet(t)
	slowDial(t)
	f := press(m, "/")
	foot := stripANSI(strings.Join(f.footerLines(), " "))
	if strings.Contains(foot, "? help") || strings.Contains(foot, "q quit") || !strings.Contains(foot, "esc") || !strings.Contains(foot, "ctrl+c quit") {
		t.Errorf("filtering footer: %q", foot)
	}
	if foot := stripANSI(strings.Join(m.footerLines(), " ")); !strings.Contains(foot, "? help") {
		t.Errorf("list footer: %q", foot)
	}
}

// When the LAST column has no room, the detail line says what the age is:
// "checking…  ·  5m ago" read as the check's age.
func TestDetailSaysLastConnected(t *testing.T) {
	it := item{host: config.Host{Name: "h"}, last: "5m ago"}
	d := it.detail(layout{})
	if !strings.Contains(d, "last connected 5m ago") {
		t.Errorf("detail: %q", d)
	}
}

// A port that took the connection and said nothing is shown as unsure (?),
// with why, never as down (✕).
func TestSilentPortShowsUnsure(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	res := health.Result{State: health.Unsure, Reason: "port open, no SSH greeting in 2s", At: now}
	it := item{host: config.Host{Name: "mux"}, status: res.State, check: res, now: now}
	if m := stripANSI(it.statusMark()); m != "?" {
		t.Errorf("mark = %q", m)
	}
	if h := it.healthText(); h != "unsure: port open, no SSH greeting in 2s, 0s ago" {
		t.Errorf("text = %q", h)
	}
	res.Via = "bastion"
	it.check = res
	if h := it.healthText(); h != "via bastion: unsure: port open, no SSH greeting in 2s, 0s ago" {
		t.Errorf("text = %q", h)
	}
}

// Behind a jump host only the jump host is checked: its answer up isn't the
// host's, so the mark isn't ●, and the line says what was checked.
func TestJumpHostUpIsNotTheHostUp(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	res := health.Result{State: health.Up, Via: "bastion", At: now}
	it := item{host: config.Host{Name: "inner"}, status: res.State, check: res, now: now}
	if m := stripANSI(it.statusMark()); m != "◐" {
		t.Errorf("mark = %q", m)
	}
	if h := it.healthText(); h != "via bastion: bastion up, the host itself not checked, 0s ago" {
		t.Errorf("text = %q", h)
	}
	res.Via = ""
	it.check = res
	if m := stripANSI(it.statusMark()); m != "●" {
		t.Errorf("direct: mark = %q", m)
	}
	if !strings.Contains(strings.Join(pickerHelp, "\n"), "◐ its jump host up") {
		t.Error("? doesn't explain ◐")
	}
}
