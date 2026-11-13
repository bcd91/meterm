package tui

import (
	"reflect"
	"testing"

	"meterm/internal/config"
)

// productionTargets falls back to every host when it can't tell which a
// command reaches, so a test that only checks "some production host was
// found" passes with --group or --tag ignored. These check the exact set.
func TestProductionTargetsExactSets(t *testing.T) {
	cfg := &config.Config{Hosts: []config.Host{
		{Name: "pay1", Address: "192.0.2.1", Env: config.EnvProduction, Group: "pay", Tags: []string{"db"}},
		{Name: "pay2", Address: "192.0.2.2", Env: config.EnvProduction, Group: "pay"},
		{Name: "web1", Address: "192.0.2.3", Env: config.EnvProduction, Group: "web", Tags: []string{"db", "edge"}},
		{Name: "dev1", Address: "192.0.2.4", Group: "pay", Tags: []string{"db"}},
	}}
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"exec", "--group", "pay", "--", "uptime"}, []string{"pay1", "pay2"}},
		{[]string{"exec", "--group=pay", "--", "uptime"}, []string{"pay1", "pay2"}},
		{[]string{"exec", "--group", "web", "--", "uptime"}, []string{"web1"}},
		{[]string{"exec", "--tag", "edge", "--", "uptime"}, []string{"web1"}},
		{[]string{"exec", "--tag=db", "--", "uptime"}, []string{"pay1", "web1"}},
		{[]string{"exec", "--tag", "db", "--group", "web", "--", "uptime"}, []string{"pay1", "web1"}},
		{[]string{"exec", "--group", "none", "--", "uptime"}, nil},
		{[]string{"exec", "dev1", "--", "uptime"}, nil},
		{[]string{"exec", "pay2", "--", "rm", "pay1"}, []string{"pay2"}},
		{[]string{"connect", "pay2"}, []string{"pay2"}},
		{[]string{"exec", "--all", "--", "uptime"}, []string{"pay1", "pay2", "web1"}},
		// Nothing it can read as hosts: every production host, to be safe.
		{[]string{"exec", "--", "uptime"}, []string{"pay1", "pay2", "web1"}},
		{[]string{"exec", "nosuchhost", "--", "uptime"}, []string{"pay1", "pay2", "web1"}},
		{[]string{"list"}, nil},
	} {
		if got := productionTargets(c.args, cfg); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: %q, want %q", c.args, got, c.want)
		}
	}
}
