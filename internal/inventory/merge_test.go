package inventory

import (
	"testing"

	"meterm/internal/config"
)

func TestMergeSkipsTakenNamesAndSavedAddresses(t *testing.T) {
	cfg := &config.Config{Hosts: []config.Host{{Name: "web1", Address: "10.0.0.1"}}}
	in := []config.Host{
		{Name: "web1", Address: "10.0.0.9"},   // name taken
		{Name: "ts-web", Address: "10.0.0.1"}, // same machine, saved by hand
		{Name: "db1", Address: "10.0.0.2"},
		{Name: "db1-again", Address: "10.0.0.2"}, // duplicate within the import
	}
	out := Merge(cfg, in, nil)
	want := []string{"exists", "duplicate", "added", "duplicate"}
	for i, o := range out {
		if o.Action != want[i] {
			t.Errorf("%s: %s, want %s", o.Host.Name, o.Action, want[i])
		}
	}
	if out[1].Other != "web1" || out[3].Other != "db1" {
		t.Errorf("others = %q %q", out[1].Other, out[3].Other)
	}
	if Added(out) != 1 || len(cfg.Hosts) != 2 {
		t.Errorf("added %d, hosts %d", Added(out), len(cfg.Hosts))
	}
}

func TestMergeAsks(t *testing.T) {
	cfg := &config.Config{}
	in := []config.Host{{Name: "a", Address: "1"}, {Name: "b", Address: "2"}, {Name: "c", Address: "3"}, {Name: "d", Address: "4"}}
	answers := map[string][2]bool{"a": {true, false}, "b": {false, false}, "c": {false, true}}
	out := Merge(cfg, in, func(h config.Host) (bool, bool) {
		a := answers[h.Name]
		return a[0], a[1]
	})
	if len(out) != 3 || out[0].Action != "added" || out[1].Action != "declined" || !out[2].Stopped {
		t.Fatalf("out = %+v", out)
	}
	if len(cfg.Hosts) != 1 || cfg.Hosts[0].Name != "a" {
		t.Errorf("hosts = %+v", cfg.Hosts)
	}
}
