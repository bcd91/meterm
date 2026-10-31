package config

import (
	"fmt"
	"regexp"
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
