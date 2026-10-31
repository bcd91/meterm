package config

import "testing"

func TestResolveInheritance(t *testing.T) {
	cfg := &Config{
		Version:  1,
		Defaults: Defaults{User: "defaultuser", Port: 22, IdentityFile: "~/.ssh/default"},
		Groups: map[string]Group{
			"prod": {User: "deploy", IdentityFile: "~/.ssh/prod", ProxyJump: "bastion", Tags: []string{"prod"}},
		},
		Hosts: []Host{
			{Name: "bastion", Address: "198.51.100.1", User: "jump"},
			{Name: "web1", Address: "10.0.0.1", Group: "prod", Tags: []string{"web"}},
			{Name: "solo", Address: "10.0.0.9"},
		},
	}

	web1, _ := cfg.Find("web1")
	r := cfg.Resolve(*web1)
	if r.User != "deploy" {
		t.Errorf("User = %q, want deploy (inherited from group)", r.User)
	}
	if r.IdentityFile != "~/.ssh/prod" {
		t.Errorf("IdentityFile = %q, want ~/.ssh/prod", r.IdentityFile)
	}
	if len(r.ProxyJump) != 1 || r.ProxyJump[0].Name != "bastion" {
		t.Fatalf("ProxyJump = %+v, want [bastion]", r.ProxyJump)
	}
	if got, want := len(r.Tags), 2; got != want {
		t.Errorf("Tags = %v, want 2 tags (group + host)", r.Tags)
	}

	solo, _ := cfg.Find("solo")
	r2 := cfg.Resolve(*solo)
	if r2.User != "defaultuser" {
		t.Errorf("solo User = %q, want defaultuser (inherited from defaults)", r2.User)
	}
	if r2.IdentityFile != "~/.ssh/default" {
		t.Errorf("solo IdentityFile = %q, want ~/.ssh/default", r2.IdentityFile)
	}
	if len(r2.ProxyJump) != 0 {
		t.Errorf("solo ProxyJump = %+v, want none", r2.ProxyJump)
	}
}

func TestResolveProxyJumpRawFallback(t *testing.T) {
	cfg := &Config{
		Version: 1,
		Hosts: []Host{
			{Name: "web1", Address: "10.0.0.1", ProxyJump: "unknown@somewhere.example"},
		},
	}
	h, _ := cfg.Find("web1")
	r := cfg.Resolve(*h)
	if r.ProxyJumpRaw != "unknown@somewhere.example" {
		t.Errorf("ProxyJumpRaw = %q, want the raw jump spec", r.ProxyJumpRaw)
	}
	if len(r.ProxyJump) != 0 {
		t.Errorf("ProxyJump = %+v, want empty since the name isn't a known host", r.ProxyJump)
	}
}

func TestResolveProxyJumpSelfReferenceDoesNotLoop(t *testing.T) {
	cfg := &Config{
		Version: 1,
		Hosts: []Host{
			{Name: "a", Address: "10.0.0.1", ProxyJump: "b"},
			{Name: "b", Address: "10.0.0.2", ProxyJump: "a"},
		},
	}
	a, _ := cfg.Find("a")
	r := cfg.Resolve(*a) // must return, not recurse forever
	if len(r.ProxyJump) != 1 || r.ProxyJump[0].Name != "b" {
		t.Fatalf("ProxyJump = %+v, want [b]", r.ProxyJump)
	}
}
