package config

import (
	"fmt"
	"net"
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
// a production host. A hop can name a saved host or give the address of one, in any of
// the forms ssh reads; a host can also have a production host's address under another
// name. A port never tells two apart: another port on the same address may be the same
// machine. It reads only what's saved, resolving no names, and what it can't read counts
// as production, as ssh or the resolver might read it as just that.
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
		if c.productionAddress(address) {
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
	if saved := c.findFold(hop); saved != nil {
		return c.touchesProduction(*saved, seen)
	}
	host, ok := hopHost(hop)
	if !ok {
		return true
	}
	if saved := c.findFold(host); saved != nil {
		return c.touchesProduction(*saved, seen)
	}
	return c.productionAddress(host)
}

// findFold finds a saved host by name, as ssh's host names go: in any case, and with or
// without a trailing dot. An exact match comes first, as names can differ only in case.
func (c *Config) findFold(name string) *Host {
	if h, i := c.Find(name); i >= 0 {
		return h
	}
	name = strings.TrimSuffix(name, ".")
	for i := range c.Hosts {
		if strings.EqualFold(c.Hosts[i].Name, name) {
			return &c.Hosts[i]
		}
	}
	return nil
}

// hopHostRE is a host name that means one thing to everyone. Anything else, such as a
// %-encoded name in a URI, isn't guessed at.
var hopHostRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// hopHost is the host in a jump hop: [user@]host[:port], the same with [brackets] around
// an address, or ssh://[user@]host[:port]. ok is false for anything else, a path after
// a URI or another scheme, SSH:// among them, included.
func hopHost(hop string) (host string, ok bool) {
	rest := hop
	if strings.HasPrefix(hop, "ssh://") {
		rest = strings.TrimSuffix(hop[len("ssh://"):], "/")
		if strings.ContainsAny(rest, "/?#%") {
			return "", false
		}
	} else if strings.Contains(rest, "://") {
		return "", false
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	validPort := func(p string) bool {
		n, err := strconv.Atoi(p)
		return err == nil && n >= 1 && n <= 65535 && portRE.MatchString(p)
	}
	switch {
	case strings.HasPrefix(rest, "["):
		j := strings.Index(rest, "]")
		if j < 0 {
			return "", false
		}
		host = rest[1:j]
		if tail := rest[j+1:]; tail != "" && !(strings.HasPrefix(tail, ":") && validPort(tail[1:])) {
			return "", false
		}
		if net.ParseIP(host) == nil {
			return "", false
		}
		return host, true
	case strings.Count(rest, ":") > 1:
		if net.ParseIP(rest) == nil { // a bare IPv6 address, or nothing readable
			return "", false
		}
		return rest, true
	case strings.Contains(rest, ":"):
		h, p, _ := strings.Cut(rest, ":")
		if !validPort(p) {
			return "", false
		}
		rest = h
	}
	if rest == "" || !hopHostRE.MatchString(rest) {
		return "", false
	}
	return rest, true
}

// sloppyNumberRE is what could be an IPv4 address to some resolver without being one to
// net.ParseIP: 127.1, 010.0.0.1 (which inet_aton takes as octal), 0x7f.0.0.1, 2130706433.
var sloppyNumberRE = regexp.MustCompile(`^(0[xX][0-9a-fA-F]*|[0-9]+)(\.(0[xX][0-9a-fA-F]*|[0-9]+))*\.?$`)

// addressKey is addr in the one spelling that compares: an IP in its canonical form,
// whatever brackets or IPv6 spelling it came in, and a name in lower case without a
// trailing dot. ok is false for what can't be compared.
func addressKey(addr string) (key string, ok bool) {
	addr = strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]")
	if ip := net.ParseIP(addr); ip != nil {
		return ip.String(), true
	}
	name := strings.TrimSuffix(addr, ".")
	if name == "" || sloppyNumberRE.MatchString(addr) || !hopHostRE.MatchString(name) {
		return "", false
	}
	return strings.ToLower(name), true
}

// productionAddress says whether a production host, by any of its routes, is at addr, or
// whether that can't be told.
func (c *Config) productionAddress(addr string) bool {
	key, ok := addressKey(addr)
	if !ok {
		return true
	}
	for _, h := range c.Hosts {
		if !c.IsProduction(h) {
			continue
		}
		for _, cand := range h.Candidates() {
			if cand.Address == "" {
				continue
			}
			prod, ok := addressKey(cand.Address)
			if !ok || prod == key {
				return true
			}
		}
	}
	return false
}
