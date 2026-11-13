package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A jump that names a saved host whose name holds a character v0.23.1
// accepted (a zero-width space) still connects, as it did in v0.23.1: the
// name never reaches ssh, the saved host's address does. A raw
// user@host:port jump with one is still refused: that text is ssh's.
func TestJumpNamingASavedHostWithHiddenCharacterConnects(t *testing.T) {
	dir := cliSandbox(t)
	installFakeSSH(t, t.TempDir())
	t.Setenv(fakeSSHExitEnv, "0")
	sshLog := filepath.Join(dir, "ssh.log")
	t.Setenv("METERM_TEST_SSH_LOG", sshLog)
	// As v0.23.1 saved it, or a team handed it over.
	hosts := "groups:\n  behind:\n    proxy_jump: \"bas\u200btion\"\n" +
		"hosts:\n" +
		"  - name: \"bas\u200btion\"\n    address: 192.0.2.10\n" +
		"  - name: inner\n    address: 192.0.2.11\n    proxy_jump: \"bas\u200btion\"\n" +
		"  - name: viagroup\n    address: 192.0.2.12\n    group: behind\n" +
		"  - name: viaroute\n    address: 192.0.2.13\n    routes:\n      - name: vpn\n        proxy_jump: \"bas\u200btion\"\n" +
		"  - name: raw\n    address: 192.0.2.14\n    proxy_jump: \"ops@bas\u200btion.example.com:22\"\n"
	if err := os.WriteFile(cfgPath, []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"connect", "inner"}, {"connect", "viagroup"}} {
		_ = os.Remove(sshLog)
		if err := runRoot(t, args...); err != nil {
			t.Errorf("%v: %v", args, err)
			continue
		}
		if b, _ := os.ReadFile(sshLog); !strings.Contains(string(b), "-J") || !strings.Contains(string(b), "192.0.2.10") {
			t.Errorf("%v: ssh ran %q, want a -J through 192.0.2.10", args, b)
		}
	}
	// A route through it doesn't stop the host's own address working.
	if err := runRoot(t, "connect", "viaroute"); err != nil {
		t.Errorf("viaroute: %v", err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	h, _ := cfg.Find("viaroute")
	if r := cfg.ResolveVia(*h, &h.Routes[0]); r.Err != nil || len(r.ProxyJump) != 1 || r.ProxyJump[0].Address != "192.0.2.10" {
		t.Errorf("route vpn: err %v, jumps %+v", r.Err, r.ProxyJump)
	}
	_ = os.Remove(sshLog)
	if err := runRoot(t, "connect", "raw"); err == nil {
		t.Error("a raw jump with a zero-width space connected")
	}
	if b, _ := os.ReadFile(sshLog); len(b) > 0 {
		t.Errorf("ssh ran for a raw jump with a zero-width space: %q", b)
	}
}
