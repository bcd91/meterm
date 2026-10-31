package relay

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"meterm/internal/config"
)

const pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJA+SfeNPN58Loh3y+m/lx3aP6w3hBSEitSCSHBrRLOk meterm-relay@gw1"

// Every script must at least parse.
func TestScriptsParse(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	add, err := AddKeyScript(pub, "GW1", 22100)
	if err != nil {
		t.Fatal(err)
	}
	node, err := NodeScript(NodeParams{Relay: "DEVBOX-1", Candidates: []string{"100.64.0.1:22", "203.0.113.50:5000", "[2001:db8::1]:22"},
		KnownHosts: []string{"[203.0.113.50]:5000 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJA+SfeNPN58Loh3y+m/lx3aP6w3hBSEitSCSHBrRLOk"}, Port: 22100})
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]string{"init": InitScript(), "add": add, "remove": RemoveKeyScript("GW1"),
		"node": node, "node-remove": NodeRemoveScript("DEVBOX-1"), "listening": ListeningScript} {
		c := exec.Command(sh, "-n")
		c.Stdin = strings.NewReader(s)
		if out, err := c.CombinedOutput(); err != nil {
			t.Errorf("%s doesn't parse: %v\n%s\n%s", name, err, out, s)
		}
	}
	for _, want := range []string{"-R localhost:22100:localhost:22", "StrictHostKeyChecking=yes", "meterm-relay-devbox-1.service", "Restart=always"} {
		if !strings.Contains(node, want) {
			t.Errorf("node script lacks %q", want)
		}
	}
}

func TestAuthorizedLineOnlyAllowsItsOwnPort(t *testing.T) {
	got := AuthorizedLine(pub, "GW1", 22100)
	want := `restrict,port-forwarding,permitlisten="localhost:22100" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJA+SfeNPN58Loh3y+m/lx3aP6w3hBSEitSCSHBrRLOk meterm-relay:GW1`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestRejectsAnythingThatCouldEscapeTheScripts(t *testing.T) {
	for _, bad := range []string{"", "not a key", "ssh-ed25519 AAAA'; rm -rf /", "ssh-ed25519 AAAA x$(id)"} {
		if _, err := AddKeyScript(bad, "n", 1); err == nil {
			t.Errorf("accepted key %q", bad)
		}
	}
	ok := []string{"[a]:22 ssh-ed25519 AAAA"}
	for _, bad := range []string{"host:22;id", "$(id):22", "host"} {
		if _, err := NodeScript(NodeParams{Relay: "r", Candidates: []string{bad}, KnownHosts: ok, Port: 1}); err == nil {
			t.Errorf("accepted address %q", bad)
		}
	}
	if _, err := NodeScript(NodeParams{Relay: "r", Candidates: []string{"h:22"}, KnownHosts: []string{"h ssh-ed25519 AAAA'x"}, Port: 1}); err == nil {
		t.Error("accepted a quote in a host key")
	}
}

func TestPortsAndEnrolled(t *testing.T) {
	cfg := &config.Config{Hosts: []config.Host{
		{Name: "gw1", Routes: []config.Route{{Name: RouteName("DEVBOX-1"), Address: "127.0.0.1", Port: 22100, ProxyJump: "DEVBOX-1"}}},
		{Name: "pc2", Routes: []config.Route{{Name: RouteName("DEVBOX-1"), Address: "127.0.0.1", Port: 22101, ProxyJump: "DEVBOX-1"}}},
		{Name: "other", Routes: []config.Route{{Name: "public", Address: "198.51.100.1"}}},
	}}
	used := UsedPorts(cfg)
	if !used[22100] || !used[22101] || len(used) != 2 {
		t.Errorf("used = %v", used)
	}
	if p := FreePort(used, ParsePorts("22\n22102\n5432\n")); p != 22103 {
		t.Errorf("FreePort = %d", p)
	}
	e := Enrolled(cfg)["DEVBOX-1"]
	if len(e) != 2 || e[0].Host != "gw1" || e[1].Port != 22101 {
		t.Errorf("Enrolled = %+v", e)
	}
}

func TestMarkers(t *testing.T) {
	out := "noise\r\nMETERM-PUBKEY: " + pub + "\r\nMETERM-RELAY-ADDR: 203.0.113.50:5000\r\nMETERM-OK\r\n"
	if Marker(out, "PUBKEY") != pub || Marker(out, "RELAY-ADDR") != "203.0.113.50:5000" || !OK(out) || Failure(out) != "" {
		t.Errorf("markers wrong in %q", out)
	}
	if Failure("METERM-ERR: needs systemd\n") != "needs systemd" {
		t.Error("Failure")
	}
}

// The relay's key scripts touch exactly one machine's line.
func TestKeyScriptsTouchOnlyTheirOwnLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "getent"), []byte("#!/bin/sh\necho \"meterm-relay:x:1:1::"+home+":/usr/sbin/nologin\"\n"), 0o755)
	ak := filepath.Join(home, ".ssh", "authorized_keys")
	other := AuthorizedLine("ssh-ed25519 AAAAother", "GW1X", 22101)
	os.WriteFile(ak, []byte("ssh-ed25519 AAAAadmin admin@laptop\n"+other+"\n"+AuthorizedLine("ssh-ed25519 AAAAold", "GW1", 22100)+"\n"), 0o600)
	run := func(script string) {
		c := exec.Command("sh", "-s")
		c.Stdin = strings.NewReader(script)
		c.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		if out, err := c.CombinedOutput(); err != nil || !OK(string(out)) {
			t.Fatalf("%v: %s", err, out)
		}
	}
	add, _ := AddKeyScript(pub, "GW1", 22100)
	run(add)
	got, _ := os.ReadFile(ak)
	want := "ssh-ed25519 AAAAadmin admin@laptop\n" + other + "\n" + AuthorizedLine(pub, "GW1", 22100) + "\n"
	if string(got) != want {
		t.Errorf("after add:\n%s\nwant\n%s", got, want)
	}
	run(RemoveKeyScript("GW1"))
	got, _ = os.ReadFile(ak)
	if string(got) != "ssh-ed25519 AAAAadmin admin@laptop\n"+other+"\n" {
		t.Errorf("after remove (GW1X must stay):\n%s", got)
	}
}
