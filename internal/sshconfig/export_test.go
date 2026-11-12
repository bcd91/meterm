package sshconfig

import (
	"strings"
	"testing"

	"meterm/internal/config"
)

// A host field that ssh_config would read as more than one value or line
// (a hand-edited hosts.yaml, a team-shared host) must never reach the file.
func TestRenderLeavesOutHostsThatWouldInjectConfig(t *testing.T) {
	inject := "evil\n    ProxyCommand touch /tmp/x\nHost x"
	cfg := &config.Config{Hosts: []config.Host{
		{Name: "good", Address: "192.0.2.10", User: "deploy"},
		{Name: inject, Address: "192.0.2.11"},
		{Name: "addr", Address: "192.0.2.12\n    ProxyCommand touch /tmp/x"},
		{Name: "user", Address: "192.0.2.13", User: "root ProxyCommand=touch"},
		{Name: "key", Address: "192.0.2.14", IdentityFile: "~/.ssh/id\"x"},
		{Name: "jumpraw", Address: "192.0.2.15", ProxyJump: "gw.example.com\tProxyCommand"},
		{Name: "rtl", Address: "192.0.2.16\u202e"},
		{Name: "zw", Address: "192.0.2.17", User: "de\u200bploy"},
		{Name: "web*", Address: "192.0.2.18"},
		{Name: "hash", Address: "192.0.2.19#x"},
		{Name: "eq", Address: "192.0.2.20", User: "a=b"},
		{Name: "bs", Address: "192.0.2.21", User: `dom\user`},
		{Name: "c1", Address: "192.0.2.22\u009b"},
		{Name: "port", Address: "192.0.2.23", Port: 70000},
		{Name: "via-bad", Address: "192.0.2.24", ProxyJump: "addr"},
		{Name: "via-via-bad", Address: "192.0.2.25", ProxyJump: "via-bad"},
		{Name: "via-good", Address: "192.0.2.26", ProxyJump: "good"},
	}}
	out := Render(cfg)
	if strings.Contains(out, "ProxyCommand") || strings.Contains(out, "\nHost x") {
		t.Errorf("a host field became config:\n%s", out)
	}
	for _, kept := range []string{"Host good\n", "Host via-good\n", "ProxyJump good\n"} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q missing from:\n%s", kept, out)
		}
	}
	left := map[string]string{}
	for _, l := range LeftOut(cfg) {
		left[l.Name] = l.Why
	}
	for _, h := range cfg.Hosts {
		_, gone := left[h.Name]
		if want := !strings.HasPrefix(h.Name, "good") && h.Name != "via-good"; gone != want {
			t.Errorf("host %q left out = %v, want %v (%q)", h.Name, gone, want, left[h.Name])
		}
		if gone && strings.Contains(out, "Host "+h.Name+"\n") {
			t.Errorf("host %q is both left out and written", h.Name)
		}
	}
	for name, field := range map[string]string{inject: "name", "addr": "address", "user": "user", "key": "identity file", "jumpraw": "jump", "port": "port", "via-via-bad": "via-bad"} {
		if !strings.Contains(left[name], field) {
			t.Errorf("why %q was left out = %q, want it to name %q", name, left[name], field)
		}
	}
}

func TestRenderKeepsOrdinaryHosts(t *testing.T) {
	cfg := &config.Config{Hosts: []config.Host{
		{Name: "bastion", Address: "gw.example.com", User: "ops", Port: 2222, IdentityFile: "~/.ssh/id_ed25519"},
		{Name: "db-1.prod", Address: "2001:db8::1", ProxyJump: "bastion,jump@other.example.com:2200"},
	}}
	if l := LeftOut(cfg); len(l) != 0 {
		t.Errorf("left out %+v", l)
	}
	out := Render(cfg)
	for _, want := range []string{"Host bastion\n", "    Port 2222\n", "Host db-1.prod\n", "    HostName 2001:db8::1\n", "    ProxyJump bastion,jump@other.example.com:2200\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing from:\n%s", want, out)
		}
	}
}

// A Windows identity file is an ordinary path with backslashes and a drive
// letter; one with a space is written quoted. What ssh_config would still
// misread (a quote, a newline, an escape-like backslash) leaves the host out.
func TestRenderIdentityFilePaths(t *testing.T) {
	cfg := &config.Config{Hosts: []config.Host{
		{Name: "win", Address: "192.0.2.30", IdentityFile: `C:\Users\me\.ssh\id_ed25519`},
		{Name: "winspace", Address: "192.0.2.31", IdentityFile: `C:\Users\Jane Doe\.ssh\id_ed25519`},
		{Name: "unixspace", Address: "192.0.2.32", IdentityFile: "~/keys/my key"},
		{Name: "hashkey", Address: "192.0.2.33", IdentityFile: "~/keys/a#b"},
		{Name: "quote", Address: "192.0.2.34", IdentityFile: `C:\Users\me\"x`},
		{Name: "newline", Address: "192.0.2.35", IdentityFile: "C:\\Users\\me\n    ProxyCommand touch /tmp/x"},
		{Name: "unc", Address: "192.0.2.36", IdentityFile: `\\server\share\id`},
		{Name: "trailing", Address: "192.0.2.37", IdentityFile: `C:\keys\`},
		{Name: "tab", Address: "192.0.2.38", IdentityFile: "C:\\keys\\a\tb"},
		{Name: "bsaddr", Address: `192.0.2.39\x`},
	}}
	out := Render(cfg)
	for _, want := range []string{
		"Host win\n    HostName 192.0.2.30\n    Port 22\n    IdentityFile C:\\Users\\me\\.ssh\\id_ed25519\n",
		"    IdentityFile \"C:\\Users\\Jane Doe\\.ssh\\id_ed25519\"\n",
		"    IdentityFile \"~/keys/my key\"\n",
		"    IdentityFile \"~/keys/a#b\"\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing from:\n%s", want, out)
		}
	}
	left := map[string]string{}
	for _, l := range LeftOut(cfg) {
		left[l.Name] = l.Why
	}
	for _, name := range []string{"quote", "newline", "unc", "trailing", "tab", "bsaddr"} {
		if _, ok := left[name]; !ok {
			t.Errorf("%s exported:\n%s", name, out)
		}
	}
	for _, name := range []string{"win", "winspace", "unixspace", "hashkey"} {
		if w, ok := left[name]; ok {
			t.Errorf("%s left out: %s", name, w)
		}
	}
	if strings.Contains(out, "ProxyCommand") {
		t.Errorf("an identity file became config:\n%s", out)
	}
}

// What meterm itself refuses to connect to (Resolve's Err) is left out too:
// ssh runs a ProxyJump with shell characters through /bin/sh, and an
// address starting with '-' reads as an option.
func TestRenderLeavesOutWhatMeTermRefuses(t *testing.T) {
	cfg := &config.Config{
		Groups: map[string]config.Group{"g": {ProxyJump: "x;y"}},
		Hosts: []config.Host{
			{Name: "pipe", Address: "192.0.2.40", ProxyJump: "a|date>pwned"},
			{Name: "semi", Address: "192.0.2.41", ProxyJump: "x;y"},
			{Name: "ingroup", Address: "192.0.2.42", Group: "g"},
			{Name: "dash", Address: "-E/tmp/x"},
			{Name: "web#2", Address: "192.0.2.43"},
		},
	}
	out := Render(cfg)
	for _, bad := range []string{"ProxyJump a|", "ProxyJump x;y", "-E/tmp/x"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q exported:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "Host web#2\n") {
		t.Errorf("a '#' in a name is fine for ssh, want it kept:\n%s", out)
	}
	left := map[string]bool{}
	for _, l := range LeftOut(cfg) {
		left[l.Name] = true
		if strings.ContainsAny(l.Why, "\n\x1b") {
			t.Errorf("why %q isn't safe to print", l.Why)
		}
	}
	for _, n := range []string{"pipe", "semi", "ingroup", "dash"} {
		if !left[n] {
			t.Errorf("%s not left out", n)
		}
	}
}

// "Host #web" is a comment line to ssh. '%' is left as it was in v0.23.0:
// ssh expands it, and IdentityFile ~/.ssh/id_%r is real use.
func TestRenderLeavesOutCommentNames(t *testing.T) {
	cfg := &config.Config{Hosts: []config.Host{
		{Name: "#web", Address: "192.0.2.50"},
		{Name: "web#2", Address: "192.0.2.51"},
		{Name: "key", Address: "192.0.2.52", IdentityFile: "~/.ssh/id_%r"},
		{Name: "ll", Address: "fe80::1%eth0"},
	}}
	out := Render(cfg)
	left := map[string]bool{}
	for _, l := range LeftOut(cfg) {
		left[l.Name] = true
	}
	if !left["#web"] || left["web#2"] || left["key"] || left["ll"] || strings.Contains(out, "Host #web") || !strings.Contains(out, "IdentityFile ~/.ssh/id_%r\n") {
		t.Errorf("left out %v; got:\n%s", left, out)
	}
}

// OpenSSH reads a ProxyJump hop "h#1" as h, "ops@b" as user ops on b and
// "app:1" as port 1 on app, so the hosts that jump through such a host are
// left out. The host itself is fine ("ssh app:1" works).
func TestRenderLeavesOutJumpsThroughNamesSSHWouldMisread(t *testing.T) {
	cfg := &config.Config{Hosts: []config.Host{
		{Name: "h#1", Address: "192.0.2.60"},
		{Name: "ops@b", Address: "192.0.2.61"},
		{Name: "app:1", Address: "192.0.2.62"},
		{Name: "via-hash", Address: "192.0.2.64", ProxyJump: "h#1"},
		{Name: "via-at", Address: "192.0.2.65", ProxyJump: "ops@b"},
		{Name: "via-colon", Address: "192.0.2.66", ProxyJump: "app:1"},
		{Name: "via-via", Address: "192.0.2.67", ProxyJump: "via-at"},
	}}
	out := Render(cfg)
	left := map[string]string{}
	for _, l := range LeftOut(cfg) {
		left[l.Name] = l.Why
	}
	for _, n := range []string{"via-hash", "via-at", "via-colon", "via-via"} {
		if _, ok := left[n]; !ok {
			t.Errorf("%q not left out", n)
		}
	}
	for _, n := range []string{"h#1", "ops@b", "app:1"} {
		if w, ok := left[n]; ok || !strings.Contains(out, "Host "+n+"\n") {
			t.Errorf("%q left out (%q), want it kept", n, w)
		}
	}
	if strings.Contains(out, "ProxyJump") {
		t.Errorf("got:\n%s", out)
	}
}
