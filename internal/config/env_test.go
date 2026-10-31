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

func TestTouchesProduction(t *testing.T) {
	c := &Config{
		Groups: map[string]Group{"via": {ProxyJump: "live"}, "prod": {Env: EnvProduction}},
		Hosts: []Host{
			{Name: "live", Address: "10.0.0.1", Env: EnvProduction, Port: 2222, Routes: []Route{{Name: "pub", Address: "203.0.113.5"}, {Name: "v6", Address: "2001:db8::1"}, {Name: "dns", Address: "prod.example.com"}}},
			{Name: "ok", Address: "10.0.0.9"},
		},
	}
	for name, tc := range map[string]struct {
		h    Host
		want bool
	}{
		"plain":                   {Host{Name: "a", Address: "10.0.0.2"}, false},
		"through ok":              {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "ok"}, false},
		"raw elsewhere":           {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "u@198.51.100.1:22"}, false},
		"own env":                 {Host{Name: "a", Address: "10.0.0.2", Env: EnvProduction}, true},
		"group env":               {Host{Name: "a", Address: "10.0.0.2", Group: "prod"}, true},
		"group jump":              {Host{Name: "a", Address: "10.0.0.2", Group: "via"}, true},
		"name":                    {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "live"}, true},
		"user@name":               {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "root@live"}, true},
		"name:port":               {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "live:22"}, true},
		"address":                 {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "10.0.0.1"}, true},
		"same port":               {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "10.0.0.1:2222"}, true},
		"other port":              {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "10.0.0.1:22"}, true},
		"own address, other port": {Host{Name: "a", Address: "10.0.0.1", Port: 22}, true},
		"ssh://name":              {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "ssh://live"}, true},
		"ssh://name:port":         {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "ssh://live:22"}, true},
		"ssh://user@name":         {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "ssh://u@live:22"}, true},
		"ssh://addr/":             {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "ssh://10.0.0.1/"}, true},
		"SSH:// isn't read":       {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "SSH://198.51.100.1"}, true},
		"ssh://[v6]:port":         {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "ssh://[2001:DB8:0::1]:22"}, true},
		"ssh:// elsewhere":        {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "ssh://u@198.51.100.1:22"}, false},
		"name in upper":           {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "LIVE"}, true},
		"name, dot":               {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "live."}, true},
		"hostname case":           {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "PROD.Example.Com"}, true},
		"hostname dot":            {Host{Name: "a", Address: "PROD.example.com."}, true},
		"v6 spelled out":          {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "[2001:0db8:0000::0001]"}, true},
		"v6 bare":                 {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "2001:db8::1"}, true},
		"v6 own, brackets":        {Host{Name: "a", Address: "[2001:DB8::1]"}, true},
		"v4 as mapped v6":         {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "[::ffff:10.0.0.1]"}, true},
		"other v6":                {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "[2001:db8::2]"}, false},
		"short v4 hop":            {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "127.1"}, true},
		"octal v4 hop":            {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "010.0.0.1"}, true},
		"hex v4 hop":              {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "0x7f.0.0.1"}, true},
		"decimal v4 hop":          {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "2130706433"}, true},
		"short v4 own":            {Host{Name: "a", Address: "127.1"}, true},
		"zone":                    {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "[fe80::1%en0]"}, true},
		"other scheme":            {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "http://198.51.100.1"}, true},
		"uri with path":           {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "ssh://198.51.100.1/x"}, true},
		"uri %-encoded":           {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "ssh://%6c%69%76%65"}, true},
		"uri query":               {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "ssh://198.51.100.1?x"}, true},
		"bad port":                {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "198.51.100.1:x"}, true},
		"port too big":            {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "198.51.100.1:70000"}, true},
		"open bracket":            {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "[2001:db8::1"}, true},
		"bracketed name":          {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "[live]"}, true},
		"garbage after":           {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "[::1]x"}, true},
		"empty host":              {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "u@"}, true},
		"user@address":            {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "u@10.0.0.1"}, true},
		"its route's addr":        {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "203.0.113.5"}, true},
		"second hop":              {Host{Name: "a", Address: "10.0.0.2", ProxyJump: "ok, live"}, true},
		"saved route":             {Host{Name: "a", Address: "10.0.0.2", Routes: []Route{{Name: "r", ProxyJump: "live"}}}, true},
		"route's address":         {Host{Name: "a", Address: "10.0.0.2", Routes: []Route{{Name: "r", Address: "10.0.0.1"}}}, true},
		"same address":            {Host{Name: "a", Address: "10.0.0.1"}, true},
		"upper case":              {Host{Name: "a", Address: "LIVE.example", ProxyJump: "x"}, false},
	} {
		if got := c.TouchesProduction(tc.h); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
	// A loop of jumps ends.
	loop := &Config{Hosts: []Host{{Name: "a", Address: "1.1.1.1", ProxyJump: "b"}, {Name: "b", Address: "1.1.1.2", ProxyJump: "a"}}}
	if loop.TouchesProduction(loop.Hosts[0]) {
		t.Error("loop")
	}
}

// A production host at an address that can't be compared could be anyone.
func TestTouchesProductionUnreadableProductionAddress(t *testing.T) {
	c := &Config{Hosts: []Host{{Name: "live", Address: "010.0.0.1", Env: EnvProduction}}}
	if !c.TouchesProduction(Host{Name: "a", Address: "10.0.0.1"}) {
		t.Error("not refused")
	}
}

func TestHopHostAcceptsPlainForms(t *testing.T) {
	for in, want := range map[string]string{
		"web":              "web",
		"u@web":            "web",
		"web:22":           "web",
		"u@web:22":         "web",
		"ssh://web":        "web",
		"ssh://u@web:22/":  "web",
		"[::1]:22":         "::1",
		"::1":              "::1",
		"ssh://[::1]":      "::1",
		"10.0.0.1":         "10.0.0.1",
		"Web.Example.COM.": "Web.Example.COM.",
	} {
		if got, ok := hopHost(in); !ok || got != want {
			t.Errorf("%q: %q, %v", in, got, ok)
		}
	}
}
