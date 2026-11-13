package main

import (
	"testing"

	"meterm/internal/route"
)

// route check's "← used" is the route connect takes: the one that worked
// last while it answers, else the first that answers.
func TestUsedRouteIsConnects(t *testing.T) {
	both := []route.Probe{{Route: "default"}, {Route: "vpn"}}
	if got := usedRoute(both, "vpn"); got != "vpn" {
		t.Errorf("last vpn, both answer: %q", got)
	}
	if got := usedRoute(both, ""); got != "default" {
		t.Errorf("nothing last: %q", got)
	}
	if got := usedRoute([]route.Probe{{Route: "default"}, {Route: "vpn", Err: "refused"}}, "vpn"); got != "default" {
		t.Errorf("last vpn, now down: %q", got)
	}
	if got := usedRoute([]route.Probe{{Route: "default", Err: "x"}}, ""); got != "" {
		t.Errorf("none: %q", got)
	}
}
