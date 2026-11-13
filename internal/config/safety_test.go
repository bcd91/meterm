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

func TestCheckPrintableNamesTheField(t *testing.T) {
	ok := Host{Name: "web1", Address: "192.0.2.1", User: "deploy", Group: "g", Tags: []string{"a"}, Notes: "fine — ünïcode"}
	if err := ok.CheckPrintable(); err != nil {
		t.Fatalf("plain host refused: %v", err)
	}
	for field, h := range map[string]Host{
		"name":          {Name: "x\x1b]0;own\x07y"},
		"group":         {Name: "x", Group: "g\x1b[5m"},
		"address":       {Name: "x", Address: "192.0.2.1\x1b[2J"},
		"user":          {Name: "x", User: "r\noot"},
		"users":         {Name: "x", Users: []string{"a\x1bb"}},
		"tag":           {Name: "x", Tags: []string{"t‮ag"}},
		"notes":         {Name: "x", Notes: "n\x1b]8;;u\x07"},
		"identity_file": {Name: "x", IdentityFile: "~/.ssh/k\x1b"},
		"proxy_jump":    {Name: "x", ProxyJump: "j\x07"},
		"route name":    {Name: "x", Routes: []Route{{Name: "r\x1b"}}},
		"tunnel note":   {Name: "x", Forwards: []Forward{{Name: "f", Note: "\x1b[2J"}}},
	} {
		err := h.CheckPrintable()
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v, want one naming %q", field, err, field)
		}
	}
}

// Every character termsafe.Label leaves out is refused in text a host
// file gets from here or from a team: otherwise "zw\u200bsp" and "zwsp"
// show as the same host and a command typed for one reaches the other.
func TestInvisibleCharactersAreRefused(t *testing.T) {
	for _, r := range []rune{0x200b, 0x200c, 0x200d, 0x2060, 0x00ad, 0xfeff, 0x200e, 0x200f, 0x061c, 0x180e, 0x2061, 0xe0001, 0x034f, 0x3164, 0x9b, 0x85, 0x1b, 0x2028} {
		name := "zw" + string(r) + "sp"
		h := Host{Name: name, Address: "192.0.2.1"}
		if err := h.CheckShared(); err == nil {
			t.Errorf("team host %q (%U) accepted", name, r)
		}
		if err := h.CheckPrintable(); err == nil {
			t.Errorf("host %q (%U) accepted by add/edit", name, r)
		}
		if err := CheckText("a tag", name); err == nil {
			t.Errorf("CheckText(%q) passed", name)
		}
	}
	for _, s := range []string{"สวัสดี", "web-1", "日本", "\U0001F680", "❤️", "\U0001F44D\U0001F3FD"} {
		if err := CheckText("name", s); err != nil {
			t.Errorf("CheckText(%q): %v", s, err)
		}
	}
}
