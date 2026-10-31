package config

import "testing"

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
