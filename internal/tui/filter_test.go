package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/lipgloss"

	"meterm/internal/config"
	"meterm/internal/health"
)

var (
	csiRE       = regexp.MustCompile("\x1b\\[[0-9;:]*[@-~]")
	underlineRE = regexp.MustCompile("\x1b\\[(?:[0-9;]*;)?4(?:;[0-9;]*)?m")
)

// colour turns styling on for the test, as on a real terminal.
func colour(t *testing.T) {
	t.Helper()
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(1) // termenv.ANSI256
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
}

// Filtering used to style matches by rune position in the already styled
// title, splitting its escape sequences: rows read "[38;5;203m● web3".
func TestFilteredRowsHaveNoEscapeFragments(t *testing.T) {
	colour(t)
	m := fleet(t)
	slowDial(t)
	m.checks["web3"] = health.Result{State: healthDown}
	m.checks["web2"] = health.Result{State: healthUp}
	m.refreshItems()
	check := func(when string, m model) {
		t.Helper()
		v := m.View()
		rest := csiRE.ReplaceAllString(v, "")
		if strings.Contains(rest, "\x1b") || strings.Contains(rest, "[38;") || strings.Contains(rest, "[0m") {
			t.Errorf("%s: escape fragments in the view:\n%q", when, rest)
		}
		if !strings.Contains(rest, "web3") {
			t.Errorf("%s: the matching row doesn't read web3:\n%s", when, rest)
		}
	}
	m = drive(t, m, nil, "/", "w", "e", "b", "3")
	check("while filtering", m)
	m = drive(t, m, nil, "enter")
	check("filter applied", m)

	// Matches are still marked: the matched letters carry the underline.
	if !underlineRE.MatchString(m.View()) {
		t.Errorf("no highlighted match in:\n%q", m.View())
	}
}

// What the filter matches against starts with the name, holds every field
// it searches, and ends with the address.
func TestFilterValueIsPlainAndSearchesEnv(t *testing.T) {
	cfg := &config.Config{Groups: map[string]config.Group{"live": {Env: config.EnvProduction}}}
	h := config.Host{Name: "api", Address: "192.0.2.5", User: "deploy", Group: "live", Tags: []string{"web"}, Notes: "billing"}
	it := item{host: h, res: cfg.Resolve(h), team: "ACME"}
	fv := it.FilterValue()
	for _, want := range []string{"api", "192.0.2.5", "live", "web", "ACME", "production", "billing", "deploy"} {
		if !strings.Contains(fv, want) {
			t.Errorf("FilterValue %q lacks %q", fv, want)
		}
	}
	if !strings.HasPrefix(fv, "api"+fieldSep) || !strings.HasSuffix(fv, fieldSep+"deploy@192.0.2.5") {
		t.Errorf("FilterValue %q should start with the name and end with the address", fv)
	}
}

// The filter matches each field on its own, fuzzily, but the address only
// as typed: /web1 is the host called web1, not every 192.0.2.1x.
func TestFilterMatchesTheAddressOnlyAsTyped(t *testing.T) {
	var items []item
	for i := 1; i <= 20; i++ {
		items = append(items, item{host: config.Host{Name: fmt.Sprintf("app%d", i)}, res: config.Resolved{Address: fmt.Sprintf("192.0.2.%d", i)}})
	}
	items = append(items,
		item{host: config.Host{Name: "web1", Group: "edge"}, res: config.Resolved{Address: "198.51.100.7"}},
		item{host: config.Host{Name: "box", Notes: "runs the payroll export"}, res: config.Resolved{Address: "203.0.113.4"}},
		item{host: config.Host{Name: "w", Group: "e", Tags: []string{"b"}, Notes: "1"}, res: config.Resolved{Address: "203.0.113.5"}},
	)
	targets := make([]string, len(items))
	for k, it := range items {
		targets[k] = it.FilterValue()
	}
	names := func(term string) []string {
		var out []string
		for _, r := range hostFilter(term, targets) {
			out = append(out, items[r.Index].host.Name)
		}
		return out
	}
	if got := names("web1"); len(got) != 1 || got[0] != "web1" {
		t.Errorf("/web1 matched %v, want only web1 (not addresses, not letters spread over fields)", got)
	}
	if got := names("192.0.2.1"); len(got) != 11 { // .1 and .10 to .19
		t.Errorf("/192.0.2.1 matched %d hosts: %v", len(got), got)
	}
	if got := names("192.0.2.17"); len(got) != 1 || got[0] != "app17" {
		t.Errorf("/192.0.2.17 matched %v", got)
	}
	if got := names("payroll"); len(got) != 1 || got[0] != "box" {
		t.Errorf("/payroll (a note) matched %v", got)
	}
	if got := names("1922"); len(got) != 0 {
		t.Errorf("/1922 matched %v: the address isn't fuzzy", got)
	}
	if got := names(""); len(got) != len(items) {
		t.Errorf("an empty filter shows %d of %d", len(got), len(items))
	}
}

// The fuzzy filter reports byte offsets; a name with wide letters before
// the match must still get the right letters marked, a match in a tag lands
// on that tag, and an address match on the address.
func TestMatchesMapOntoTheRightLetters(t *testing.T) {
	colour(t)
	it := item{host: config.Host{Name: "東京web", Tags: []string{"db", "ops"}}, res: config.Resolved{Address: "192.0.2.1"}}
	lay := layoutFor([]list.Item{it}, 100)
	rank := hostFilter("web", []string{it.FilterValue()})
	if len(rank) != 1 {
		t.Fatalf("no match: %+v", rank)
	}
	row := it.row(lay, rank[0].MatchedIndexes, false)
	plain := csiRE.ReplaceAllString(row, "")
	if !strings.Contains(plain, "東京web") {
		t.Fatalf("row reads %q", plain)
	}
	// The underlined run is exactly "web".
	if got := underlined(row); got != "web" {
		t.Errorf("underlined %q in %q, want web", got, row)
	}
	rank = hostFilter("ops", []string{it.FilterValue()})
	if got := underlined(it.row(lay, rank[0].MatchedIndexes, false)); got != "ops" {
		t.Errorf("underlined %q in the tags, want ops", got)
	}
	rank = hostFilter("2.1", []string{it.FilterValue()})
	if got := underlined(it.row(lay, rank[0].MatchedIndexes, false)); got != "2.1" {
		t.Errorf("underlined %q in the address, want 2.1", got)
	}
}

// underlined is the text drawn with underline on, in order.
func underlined(s string) string {
	var out strings.Builder
	on := false
	for len(s) > 0 {
		if loc := csiRE.FindStringIndex(s); loc != nil && loc[0] == 0 {
			seq := s[:loc[1]]
			if strings.HasSuffix(seq, "m") {
				on = underlineRE.MatchString(seq)
			}
			s = s[loc[1]:]
			continue
		}
		r, n := utf8.DecodeRuneInString(s)
		if on {
			out.WriteRune(r)
		}
		s = s[n:]
	}
	return out.String()
}
