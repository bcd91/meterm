package team

import (
	"reflect"
	"strings"
	"testing"

	"meterm/internal/config"
)

func TestDescribe(t *testing.T) {
	cases := map[string]SharedHost{
		"10.0.0.1":                         {Address: "10.0.0.1", Port: 22},
		"deploy@10.0.0.1:2222 via bastion": {Address: "10.0.0.1", User: "deploy", Port: 2222, ProxyJump: "bastion"},
	}
	for want, h := range cases {
		if got := h.Describe(); got != want {
			t.Errorf("Describe = %q, want %q", got, want)
		}
	}
}

func TestDiffIgnoresBookkeeping(t *testing.T) {
	a := SharedHost{Name: "web1", Address: "10.0.0.1", Tags: []string{"prod"}, Rev: 3, UpdatedBy: "a@x"}
	b := a
	b.Rev, b.UpdatedBy = 4, "b@x"
	if d := Diff(a, b); d != nil {
		t.Errorf("bookkeeping showed up: %q", d)
	}
	b.Address, b.Tags = "10.0.0.2", []string{"prod", "web"}
	b.Forwards = []config.Forward{{Type: "L", LocalPort: 1, RemoteHost: "x", RemotePort: 2}}
	want := []string{`address: "10.0.0.1" → "10.0.0.2"`, `tags: "prod" → "prod,web"`, `forwards: "" → "-L 1:x:2"`}
	if d := Diff(a, b); !reflect.DeepEqual(d, want) {
		t.Errorf("Diff = %q", d)
	}
}

func TestProblemsAndAttention(t *testing.T) {
	cs := []Change{{Host: "a", Outcome: Conflict}, {Host: "b", Outcome: Forbidden}, {Host: "c", Outcome: PushedRemoval}}
	if n := Problems(cs); n != 2 {
		t.Errorf("Problems = %d", n)
	}
	for _, c := range NeedingAttention(cs) {
		if !c.Outcome.NeedsAttention() {
			t.Errorf("%s doesn't need attention", c.Host)
		}
	}
}

func TestRoutesDiff(t *testing.T) {
	h := SharedHost{Name: "devbox", Address: "100.64.0.1", Routes: []config.Route{
		{Name: "public", Address: "203.0.113.50", Port: 5000},
		{Name: "via-b", ProxyJump: "bastion"},
	}}
	b := h
	b.Routes = nil
	if d := Diff(h, b); len(d) != 1 || d[0][:7] != "routes:" {
		t.Errorf("Diff = %q", d)
	}
}

func TestEnvIsShared(t *testing.T) {
	h := config.Host{Name: "a", Address: "192.0.2.1", Env: config.EnvProduction}
	s := SharedFromHost(h)
	if s.Env != config.EnvProduction {
		t.Fatalf("not taken from the host: %+v", s)
	}
	if got := s.ApplyTo(config.Host{}, "t"); got.Env != config.EnvProduction {
		t.Errorf("not applied to the host: %+v", got)
	}
	o := s
	o.Env = config.EnvStaging
	if s.SameContent(o) {
		t.Error("a changed env should count as a change")
	}
	if d := Diff(s, o); len(d) != 1 || !strings.Contains(d[0], "env") {
		t.Errorf("Diff = %v", d)
	}
}
