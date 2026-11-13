package tui

import (
	"testing"

	"meterm/internal/config"
	"meterm/internal/health"
)

// A host behind a jump host is checked the way connect goes, through its
// first hop: the jump host. It never shows down from a direct dial of its own
// address, which from here would fail even when it's reachable (reported as
// "jump-host ไม่สามารถดึงสถานะได้หรอครับ พอดีเข้าได้แต่เห็นแดง"), and a jump
// host that's down makes it down, not "not checked".
func TestHealthCmdsCheckTheJumpHost(t *testing.T) {
	m := newModel(t)
	answering(t, "203.0.113.9:22")
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
	results := map[string]health.Result{}
	for _, cmd := range cmds {
		msg := cmd().(healthResultMsg)
		results[msg.name] = msg.res
	}
	if r := results["inner"]; r.State != healthUp || r.Via != "bastion" {
		t.Errorf("inner (bastion answers) = %+v", r)
	}
	if r := results["raw-jump"]; r.State != healthDown || r.Via != "user@somewhere.example" {
		t.Errorf("raw-jump (its jump host doesn't answer) = %+v", r)
	}
	if r := results["bastion"]; r.State != healthUp {
		t.Errorf("bastion = %+v", r)
	}
}

func TestHealthResultUpdatesItemStatusInPlace(t *testing.T) {
	m := newModel(t)
	m.cfg = &config.Config{Hosts: []config.Host{{Name: "web1", Address: "10.0.0.1", ProxyJump: "bastion"}}}
	m.list.SetItems(m.buildItems())

	updated, _ := m.Update(resultFor(m, "web1", healthSkipped))
	um := updated.(model)

	it, ok := um.list.Items()[0].(item)
	if !ok {
		t.Fatal("expected list item 0 to be an item")
	}
	if it.status != healthSkipped {
		t.Errorf("item status = %v, want healthSkipped", it.status)
	}
}
