package config

import (
	"strings"
	"testing"
)

func TestTeamHostsCantInjectOptions(t *testing.T) {
	bad := []Host{
		{Name: "a", Address: "-oProxyCommand=touch /tmp/x"},
		{Name: "b", Address: "10.0.0.1", User: "-oProxyCommand=x"},
		{Name: "c", Address: "10.0.0.1", ProxyJump: "-oProxyCommand=x"},
		{Name: "d", Address: "10.0.0.1", ProxyJump: "j; rm -rf /"},
		{Name: "e", Address: "10.0.0.1", ExtraArgs: []string{"-oProxyCommand=touch /tmp/x"}},
		{Name: "f", Address: "10.0.0.1", ExtraArgs: []string{"-o", "LocalCommand=id", "-o", "PermitLocalCommand=yes"}},
		{Name: "g", Address: "10.0.0.1", ExtraArgs: []string{"-o", "StrictHostKeyChecking=no"}},
		{Name: "h", Address: "10.0.0.1", ExtraArgs: []string{"-oPKCS11Provider=/tmp/evil.so"}},
		{Name: "i", Address: "10.0.0.1", ExtraArgs: []string{"-F", "/tmp/evil_config"}},
		{Name: "j", Address: "10.0.0.1", ExtraArgs: []string{"-A"}},
		{Name: "k", Address: "10.0.0.1", Routes: []Route{{Name: "r", Address: "-oProxyCommand=x"}}},
		{Name: "l", Address: "10.0.0.1", Forwards: []Forward{{Type: "L", LocalPort: 1, RemoteHost: "-x", RemotePort: 2}}},
		{Name: "m", Address: "10.0.0.1", Notes: "hi\x1b]52;c;ZXZpbA==\x07"},
		{Name: "n\x1b[2J", Address: "10.0.0.1"},
		{Name: "o", Address: "10.0.0.1", ExtraArgs: []string{"-o", "ServerAliveInterval=%d$(id)"}},
		{Name: "-p", Address: "10.0.0.1"},
	}
	for _, h := range bad {
		if err := h.CheckShared(); err == nil {
			t.Errorf("accepted %+v", h)
		}
	}
	good := []Host{
		{Name: "web1", Address: "10.0.0.1", User: "deploy", Port: 2222, ProxyJump: "bastion,ops@j.example.com:2200"},
		{Name: "v6", Address: "2001:db8::1", ProxyJump: "u@[2001:db8::2]:22"},
		{Name: "tuned", Address: "db.internal", ExtraArgs: []string{"-o", "ServerAliveInterval=30", "-oConnectTimeout=5", "-C", "-4"}},
		{Name: "routes", Address: "100.64.0.1", Routes: []Route{{Name: "public", Address: "203.0.113.50", Port: 5000}, {Name: "relay-x", Address: "127.0.0.1", Port: 22100, ProxyJump: "DEVBOX-1"}}},
		{Name: "สวัสดี", Address: "10.0.0.2", Notes: "ภาษาไทยได้", Tags: []string{"prod"}},
	}
	for _, h := range good {
		if err := h.CheckShared(); err != nil {
			t.Errorf("rejected %s: %v", h.Name, err)
		}
	}
}

// A personal host keeps whatever extra args its owner chose; a team host
// is held to the allowlist even if it's already in the local file.
func TestResolveFlagsUnsafeHostsAndTheirJumps(t *testing.T) {
	cfg := &Config{Hosts: []Host{
		{Name: "mine", Address: "10.0.0.1", ExtraArgs: []string{"-oProxyCommand=nc %h %p"}},
		{Name: "theirs", Address: "10.0.0.2", Team: "t1", ExtraArgs: []string{"-oProxyCommand=evil"}},
		{Name: "behind", Address: "10.0.0.3", ProxyJump: "theirs"},
	}}
	if r := cfg.Resolve(cfg.Hosts[0]); r.Err != nil {
		t.Errorf("personal host flagged: %v", r.Err)
	}
	if r := cfg.Resolve(cfg.Hosts[1]); r.Err == nil || !strings.Contains(r.Err.Error(), "team host") {
		t.Errorf("team host not flagged: %v", r.Err)
	}
	if r := cfg.Resolve(cfg.Hosts[2]); r.Err == nil {
		t.Error("host behind an unsafe jump not flagged")
	}
}
