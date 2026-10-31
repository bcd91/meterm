package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeEnv(t *testing.T) {
	for in, want := range map[string]string{"": "", "production": "production", "Production": "production", "STAGING": "staging", "dev": "dev", "test": "test"} {
		if got, err := NormalizeEnv(in); err != nil || got != want {
			t.Errorf("NormalizeEnv(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"prod", "live", " production", "production\n", "dev\x1b]52;c;ZXZpbA==\x07"} {
		_, err := NormalizeEnv(in)
		if err == nil {
			t.Errorf("NormalizeEnv(%q) accepted", in)
			continue
		}
		if !strings.Contains(err.Error(), EnvList) {
			t.Errorf("NormalizeEnv(%q): error doesn't list the values: %v", in, err)
		}
		if strings.ContainsAny(err.Error(), "\x1b\n") {
			t.Errorf("NormalizeEnv(%q): error carries control characters: %q", in, err)
		}
	}
}

func TestEnvHostOverridesGroup(t *testing.T) {
	c := &Config{Groups: map[string]Group{"live": {Env: "production"}, "plain": {}}}
	for _, tc := range []struct {
		host Host
		want string
		prod bool
	}{
		{Host{Name: "none"}, "", false},
		{Host{Name: "plain", Group: "plain"}, "", false},
		{Host{Name: "own", Env: "production"}, "production", true},
		{Host{Name: "inherits", Group: "live"}, "production", true},
		{Host{Name: "overrides", Group: "live", Env: "staging"}, "staging", false},
		{Host{Name: "cased", Env: "Production"}, "production", true},
		{Host{Name: "typo", Env: "prod"}, "production", true},
		{Host{Name: "named prod-web", Tags: []string{"prod"}}, "", false},
	} {
		if got := c.Env(tc.host); got != tc.want {
			t.Errorf("%s: Env = %q, want %q", tc.host.Name, got, tc.want)
		}
		if got := c.IsProduction(tc.host); got != tc.prod {
			t.Errorf("%s: IsProduction = %v, want %v", tc.host.Name, got, tc.prod)
		}
		h := tc.host
		h.Address = "192.0.2.1"
		if got := c.Resolve(h).IsProduction(); got != tc.prod {
			t.Errorf("%s: Resolved.IsProduction = %v, want %v", tc.host.Name, got, tc.prod)
		}
	}
}

func TestTypoInGroupEnvCountsAsProduction(t *testing.T) {
	c := &Config{Groups: map[string]Group{"g": {Env: "prdo"}}}
	if !c.IsProduction(Host{Group: "g"}) {
		t.Error("an unknown group env should count as production")
	}
}

func TestMentionsProduction(t *testing.T) {
	for _, f := range []Forward{{Name: "api", Note: "Example API (prod)"}, {Name: "x", Note: "example.prod.service"}, {Name: "db", Note: "Production DB"}, {Name: "prod-db"}} {
		if !f.MentionsProduction() {
			t.Errorf("%+v not spotted", f)
		}
	}
	for _, f := range []Forward{{Name: "product-catalog", Note: "dev"}, {Name: "reproduce", Note: "bug"}, {Name: "x", Note: "pos.api.dev"}} {
		if f.MentionsProduction() {
			t.Errorf("%+v wrongly spotted", f)
		}
	}
}

func TestSharedHostEnvIsChecked(t *testing.T) {
	h := Host{Name: "a", Address: "192.0.2.1", Env: "production"}
	if err := h.CheckShared(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"prod", "dev\x1b]52;c;ZXZpbA==\x07", "production\n"} {
		h.Env = bad
		if err := h.CheckShared(); err == nil {
			t.Errorf("env %q accepted from a team", bad)
		}
	}
}

// A file with no env stays exactly as it was through a load and save, and a
// file with one keeps it.
func TestEnvYAMLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts.yaml")
	c := &Config{Version: 1, Groups: map[string]Group{"g": {User: "u"}}, Hosts: []Host{{Name: "a", Address: "192.0.2.1", Group: "g"}}, path: path}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "env") {
		t.Fatalf("an unset env was written:\n%s", before)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.Save(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Errorf("round trip changed the file:\n%s\n---\n%s", before, after)
	}

	err = loaded.Update(func(c *Config) error {
		c.Hosts[0].Env = EnvStaging
		g := c.Groups["g"]
		g.Env = EnvProduction
		c.Groups["g"] = g
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Hosts[0].Env != EnvStaging || again.Groups["g"].Env != EnvProduction {
		t.Errorf("env lost: host %q, group %q", again.Hosts[0].Env, again.Groups["g"].Env)
	}
}
