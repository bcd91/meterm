package main

import (
	"testing"

	"meterm/internal/config"
)

func TestRenameJumpsRewritesChains(t *testing.T) {
	cfg := &config.Config{
		Hosts: []config.Host{
			{Name: "a", ProxyJump: "bastion"},
			{Name: "b", ProxyJump: "edge,bastion"},
			{Name: "c", ProxyJump: "bastion-2"}, // a different host whose name merely starts the same
		},
		Groups: map[string]config.Group{"prod": {ProxyJump: "bastion"}},
	}
	if n := renameJumps(cfg, "bastion", "jump1"); n != 3 {
		t.Errorf("changed %d references, want 3", n)
	}
	want := map[string]string{"a": "jump1", "b": "edge,jump1", "c": "bastion-2"}
	for _, h := range cfg.Hosts {
		if h.ProxyJump != want[h.Name] {
			t.Errorf("%s proxy_jump = %q, want %q", h.Name, h.ProxyJump, want[h.Name])
		}
	}
	if cfg.Groups["prod"].ProxyJump != "jump1" {
		t.Errorf("group proxy_jump not renamed")
	}
}

// A route's jump path is rewritten too.
func TestRenameJumpsRewritesRoutes(t *testing.T) {
	cfg := &config.Config{Hosts: []config.Host{{Name: "web13", Routes: []config.Route{{Name: "r", ProxyJump: "bastion"}, {Name: "s"}}}}}
	if n := renameJumps(cfg, "bastion", "jump1"); n != 1 || cfg.Hosts[0].Routes[0].ProxyJump != "jump1" || cfg.Hosts[0].Routes[1].ProxyJump != "" {
		t.Errorf("changed %d, routes %+v", n, cfg.Hosts[0].Routes)
	}
}
