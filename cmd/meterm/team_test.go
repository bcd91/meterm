package main

import (
	"strings"
	"testing"

	"meterm/internal/config"
	"meterm/internal/team"
)

func TestPickTeam(t *testing.T) {
	teams := []team.Team{
		{ID: "git:x", Name: "Globex"},
		{ID: "git:c2", Name: "Initech"},
		{ID: "git:c3", Name: "initech"},
	}
	if got, err := pickTeam(teams, "globex"); err != nil || got.ID != "git:x" {
		t.Errorf("globex = %+v, %v", got, err)
	}
	if got, err := pickTeam(teams, "git:c2"); err != nil || got.ID != "git:c2" {
		t.Errorf("by ID = %+v, %v", got, err)
	}
	if _, err := pickTeam(teams, "Initech"); err == nil || !strings.Contains(err.Error(), "git:c2, git:c3") {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := pickTeam(teams, "nope"); err == nil {
		t.Error("unknown team found")
	}
}

func TestRouteNote(t *testing.T) {
	r := config.Resolved{Name: "lan1", ProxyJump: []config.Resolved{{Name: "devbox", Route: "public"}}}
	if got := routeNote(r); got != `via route "public" of devbox` {
		t.Errorf("routeNote = %q", got)
	}
	if got := routeNote(config.Resolved{Name: "x"}); got != "" {
		t.Errorf("own address noted: %q", got)
	}
}

func TestConnectArgs(t *testing.T) {
	cases := map[string][]string{
		"-t sudo ls":    {"--", "-t", "sudo ls"}, // ssh options, then the command
		"uptime":        {"--", "uptime"},
		"-L 1:x:2":      {"-L", "1:x:2"}, // no "--" at all
		"-- -weird-cmd": {"--", "--", "-weird-cmd"},
		"":              {},
	}
	for want, in := range cases {
		if got := strings.Join(connectArgs(in), " "); got != want {
			t.Errorf("connectArgs(%q) = %q, want %q", in, got, want)
		}
	}
}
