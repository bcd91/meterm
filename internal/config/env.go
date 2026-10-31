package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The environments a host can be in.
const (
	EnvProduction = "production"
	EnvStaging    = "staging"
	EnvDev        = "dev"
	EnvTest       = "test"

	// EnvList is the allowed values, for help text and errors.
	EnvList = EnvProduction + ", " + EnvStaging + ", " + EnvDev + ", " + EnvTest
)

// NormalizeEnv checks an env someone gave and returns it in lower case; ""
// means none. Team-shared hosts go through this too, so nothing but these
// words is ever printed from another member's env.
func NormalizeEnv(s string) (string, error) {
	switch v := strings.ToLower(s); v {
	case "", EnvProduction, EnvStaging, EnvDev, EnvTest:
		return v, nil
	}
	return "", fmt.Errorf("env %q isn't one of: %s", s, EnvList)
}

// Env is h's environment: its own, or else its group's, or "" when neither
// says. A value meterm doesn't know, which can only come from editing the file
// by hand, counts as production: a typo mustn't turn a warning off.
func (c *Config) Env(h Host) string {
	raw := h.Env
	if raw == "" {
		raw = c.Groups[h.Group].Env
	}
	env, err := NormalizeEnv(raw)
	if err != nil {
		return EnvProduction
	}
	return env
}

// IsProduction is the one answer to whether h is a production host. It reads
// only what's saved and never touches the network.
func (c *Config) IsProduction(h Host) bool { return c.Env(h) == EnvProduction }

// IsProduction says the same of a host that's already resolved.
func (r Resolved) IsProduction() bool { return r.Env == EnvProduction }

var prodRE = regexp.MustCompile(`(?i)(^|[^a-z])(prod|production)([^a-z]|$)`)

// MentionsProduction spots a tunnel into production by its name or note, for
// tunnels that lead there from a host that isn't one itself.
func (f Forward) MentionsProduction() bool { return prodRE.MatchString(f.Name + " " + f.Note) }

// TouchesProduction says whether reaching h, by any of its routes, goes to or through
// a production host. A hop can name a saved host, as user@name or name:port as well,
// or give the address of one; a host can also have a production host's address under
// another name. It reads only what's saved.
func (c *Config) TouchesProduction(h Host) bool {
	return c.touchesProduction(h, map[string]bool{})
}

func (c *Config) touchesProduction(h Host, seen map[string]bool) bool {
	if seen[h.Name] {
		return false
	}
	seen[h.Name] = true
	if c.IsProduction(h) {
		return true
	}
	for _, cand := range h.Candidates() {
		address := cand.Address
		if address == "" {
			address = h.Address
		}
		if c.productionAddress(address, cand.Port) {
			return true
		}
		jump := cand.ProxyJump
		if jump == "" && cand.Name == "default" {
			jump = c.Groups[h.Group].ProxyJump
		}
		for _, hop := range strings.Split(jump, ",") {
			if hop = strings.TrimSpace(hop); hop != "" && c.hopTouchesProduction(hop, seen) {
				return true
			}
		}
	}
	return false
}

func (c *Config) hopTouchesProduction(hop string, seen map[string]bool) bool {
	if saved, i := c.Find(hop); i >= 0 {
		return c.touchesProduction(*saved, seen)
	}
	host := hop
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	if saved, i := c.Find(host); i >= 0 {
		return c.touchesProduction(*saved, seen)
	}
	host, port := splitHopPort(host)
	if saved, i := c.Find(host); i >= 0 {
		return c.touchesProduction(*saved, seen)
	}
	return c.productionAddress(host, port)
}

// splitHopPort takes the port off host:port and [addr]:port, and the brackets off.
func splitHopPort(s string) (string, int) {
	host, port := s, ""
	if strings.HasPrefix(s, "[") {
		if j := strings.Index(s, "]"); j > 0 {
			host = s[1:j]
			port = strings.TrimPrefix(s[j+1:], ":")
		}
	} else if strings.Count(s, ":") == 1 {
		host, port, _ = strings.Cut(s, ":")
	}
	n, _ := strconv.Atoi(port)
	return host, n
}

// productionAddress says whether a production host, by any of its routes, is at
// addr. Ports only tell two apart when both are given: with one missing it could be
// the same machine, and this errs on the side of it being so.
func (c *Config) productionAddress(addr string, port int) bool {
	addr = strings.Trim(addr, "[]")
	for _, h := range c.Hosts {
		if !c.IsProduction(h) {
			continue
		}
		for _, cand := range h.Candidates() {
			if cand.Address != "" && strings.EqualFold(strings.Trim(cand.Address, "[]"), addr) &&
				(port == 0 || cand.Port == 0 || port == cand.Port) {
				return true
			}
		}
	}
	return false
}
