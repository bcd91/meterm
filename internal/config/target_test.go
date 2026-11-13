package config

import (
	"strings"
	"testing"
)

func TestTarget(t *testing.T) {
	c := &Config{Hosts: []Host{{Name: "API", Address: "10.0.0.1", User: "svc", Users: []string{"deploy"}}, {Name: "odd@name", Address: "10.0.0.2"}}}
	h, i, err := c.Target("deploy@API")
	if err != nil || i != 0 || h.User != "deploy" || h.Address != "10.0.0.1" {
		t.Fatalf("deploy@API = %+v %d %v", h, i, err)
	}
	if c.Hosts[0].User != "svc" {
		t.Error("Target changed the saved host")
	}
	if h, _, _ := c.Target("odd@name"); h == nil || h.Name != "odd@name" || h.User != "" {
		t.Errorf("a saved name with @ should win: %+v", h)
	}
	if _, i, err := c.Target("-oProxyCommand=x@API"); err == nil || i >= 0 {
		t.Errorf("an option as the user was accepted (%d)", i)
	}
	if _, i, _ := c.Target("deploy@nope"); i >= 0 {
		t.Error("unknown host matched")
	}
	if _, i, _ := c.Target("@API"); i >= 0 {
		t.Error("empty user accepted")
	}
}

func TestUsersAreChecked(t *testing.T) {
	for _, u := range []string{"-oProxyCommand=x", "a b", "x;y", ""} {
		h := Host{Name: "x", Address: "10.0.0.1", Users: []string{u}}
		if err := h.CheckConnection(); err == nil {
			t.Errorf("user %q accepted", u)
		}
		if err := h.CheckShared(); err == nil {
			t.Errorf("shared user %q accepted", u)
		}
	}
	if err := (Host{Name: "x", Address: "10.0.0.1", Users: []string{"deploy", "root"}}).CheckShared(); err != nil {
		t.Error(err)
	}
}

// "zwsp" names the host called that, and shows as the one called
// "zw\u200bsp" too: an exact match doesn't settle it, both are named.
func TestLookupRefusesANameTwoHostsShowAs(t *testing.T) {
	c := &Config{Hosts: []Host{{Name: "zwsp", Address: "192.0.2.1", Env: "production"}, {Name: "zw\u200bsp", Address: "192.0.2.2", Env: "dev"}, {Name: "web1", Address: "192.0.2.3"}}}
	_, i, err := c.Lookup("zwsp")
	if err == nil || i >= 0 {
		t.Fatalf("Lookup(zwsp) = %d, %v; want an ambiguity error", i, err)
	}
	for _, want := range []string{`"zwsp"`, `"zw\u200bsp"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't name %s", err, want)
		}
	}
	if h, i, err := c.Lookup("zw\u200bsp"); err != nil || i != 1 || h.Address != "192.0.2.2" {
		t.Errorf("the exact raw name = %d, %v", i, err)
	}
	if _, i, err := c.Lookup("web1"); err != nil || i != 2 {
		t.Errorf("web1 = %d, %v", i, err)
	}
	if _, i, err := c.Target("deploy@zwsp"); err == nil || i >= 0 {
		t.Errorf("deploy@zwsp = %d, %v; want the ambiguity error", i, err)
	}
	d := &Config{Hosts: []Host{{Name: "a\u200bb"}, {Name: "a\u00adb"}}}
	if _, i, err := d.Lookup("ab"); err == nil || i >= 0 || !strings.Contains(err.Error(), `"a\u00adb"`) {
		t.Errorf("two that show alike, neither exact = %d, %v", i, err)
	}
}

// JumpedBy is every host whose way in goes through name: its own jump path,
// a route's, or its group's.
func TestJumpedBy(t *testing.T) {
	c := &Config{
		Groups: map[string]Group{"inner": {ProxyJump: "bastion"}},
		Hosts: []Host{
			{Name: "bastion", Address: "192.0.2.1"},
			{Name: "web11", Address: "10.0.0.11", ProxyJump: "bastion"},
			{Name: "web12", Address: "10.0.0.12", ProxyJump: "other, bastion"},
			{Name: "web13", Address: "10.0.0.13", Routes: []Route{{Name: "r", ProxyJump: "bastion"}}},
			{Name: "web14", Address: "10.0.0.14", Group: "inner"},
			{Name: "web15", Address: "10.0.0.15", Group: "inner", ProxyJump: "other"},
			{Name: "web16", Address: "10.0.0.16", ProxyJump: "bastion2"},
		}}
	got := strings.Join(c.JumpedBy("bastion"), ",")
	if got != "web11,web12,web13,web14" {
		t.Errorf("JumpedBy(bastion) = %s", got)
	}
	if got := c.JumpedBy("web16"); len(got) != 0 {
		t.Errorf("JumpedBy(web16) = %v", got)
	}
}
