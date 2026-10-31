package tui

import (
	"testing"

	"meterm/internal/config"
)

// A host reachable only through a jump host must never show as "down" from
// a direct dial — that's misleading (it may be perfectly reachable through
// the jump host) and was reported as a live bug: "jump-host ไม่สามารถดึง
// สถานะได้หรอครับ พอดีเข้าได้แต่เห็นแดง".
func TestHealthCmdsSkipHostsBehindProxyJump(t *testing.T) {
	m := New("/tmp/hosts.yaml", "/usr/local/bin/meterm")
	m.cfg = &config.Config{
		Hosts: []config.Host{
			{Name: "bastion", Address: "203.0.113.9"},
			{Name: "inner", Address: "192.168.1.101", ProxyJump: "bastion"},
			{Name: "raw-jump", Address: "192.168.1.102", ProxyJump: "user@somewhere.example"},
		},
	}

	cmds := m.healthCmds()
	if len(cmds) != 3 {
		t.Fatalf("expected 3 health commands, got %d", len(cmds))
	}

	results := map[string]healthState{}
	for _, cmd := range cmds {
		msg := cmd().(healthResultMsg)
		results[msg.name] = msg.state
	}

	if results["inner"] != healthSkipped {
		t.Errorf("inner (behind a resolved proxy_jump) state = %v, want healthSkipped", results["inner"])
	}
	if results["raw-jump"] != healthSkipped {
		t.Errorf("raw-jump (behind a raw proxy_jump spec) state = %v, want healthSkipped", results["raw-jump"])
	}
	// bastion has no jump host itself, so it should actually be dialed
	// (and, dialing a bogus TEST-NET-3 address, come back down — not skipped).
	if results["bastion"] != healthDown {
		t.Errorf("bastion (no proxy_jump) state = %v, want healthDown (a real dial was attempted)", results["bastion"])
	}
}

func TestHealthResultUpdatesItemStatusInPlace(t *testing.T) {
	m := New("/tmp/hosts.yaml", "/usr/local/bin/meterm")
	m.cfg = &config.Config{Hosts: []config.Host{{Name: "web1", Address: "10.0.0.1", ProxyJump: "bastion"}}}
	m.list.SetItems(m.buildItems())

	updated, _ := m.Update(healthResultMsg{name: "web1", state: healthSkipped})
	um := updated.(model)

	it, ok := um.list.Items()[0].(item)
	if !ok {
		t.Fatal("expected list item 0 to be an item")
	}
	if it.status != healthSkipped {
		t.Errorf("item status = %v, want healthSkipped", it.status)
	}
}
