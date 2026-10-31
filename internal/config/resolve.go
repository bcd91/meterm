package config

import (
	"fmt"
	"strings"
)

// Resolved is the effective, fully-inherited connection settings for a host:
// host fields win, falling back to the host's group, falling back to defaults.
type Resolved struct {
	Name         string
	Address      string
	Port         int
	User         string
	IdentityFile string
	ProxyJump    []Resolved // resolved jump chain, first hop first
	ProxyJumpRaw string     // literal jump spec used when a name isn't a known host
	Forwards     []Forward
	ExtraArgs    []string
	Tags         []string
	Env          string // see Config.Env
	Route        string // the route used, when not the host's own address
	// Err is set when the host, or a jump host on the way, has a field ssh
	// would misread (an address starting with "-", a team host's unsafe
	// extra args, …); sshrun refuses to run anything for it.
	Err error
}

// Resolve computes the effective settings for host h, inheriting from its
// group and the global defaults, and recursively resolving any proxy-jump
// chain (comma-separated names, each either a known host or a raw user@host spec).
func (c *Config) Resolve(h Host) Resolved {
	return c.resolve(h, nil, map[string]bool{})
}

// ResolveVia resolves h over a given route instead of letting the route
// picker choose; nil means the host's own address. Jump hosts along the way
// still go through the picker.
func (c *Config) ResolveVia(h Host, rt *Route) Resolved {
	return c.resolve(h, rt, map[string]bool{})
}

// Candidates is every way to reach h, its own address first.
func (h Host) Candidates() []Route {
	return append([]Route{{Name: "default", Address: h.Address, Port: h.Port, ProxyJump: h.ProxyJump}}, h.Routes...)
}

func (c *Config) resolve(h Host, rt *Route, seen map[string]bool) Resolved {
	g := c.Groups[h.Group]
	if rt == nil && c.picker != nil && len(h.Routes) > 0 {
		if picked, ok := c.picker(h); ok {
			rt = &picked
		}
	}
	routeName, direct := "", false
	if rt != nil && rt.Name != "default" {
		routeName = rt.Name
		if rt.Address != "" {
			h.Address = rt.Address
		}
		if rt.Port != 0 {
			h.Port = rt.Port
		}
		h.ProxyJump = rt.ProxyJump
		direct = rt.ProxyJump == ""
	}

	r := Resolved{
		Name:         h.Name,
		Address:      h.Address,
		Port:         firstNonZero(h.Port, g.Port, c.Defaults.Port, 22),
		User:         firstNonEmpty(h.User, g.User, c.Defaults.User),
		IdentityFile: firstNonEmpty(h.IdentityFile, g.IdentityFile),
		Forwards:     h.Forwards,
		ExtraArgs:    h.ExtraArgs,
		Env:          c.Env(h),
		Route:        routeName,
	}
	if err := h.CheckConnection(); err != nil {
		r.Err = fmt.Errorf("%s: %w (fix it with `meterm edit %s`)", h.Name, err, h.Name)
	} else if err := CheckJump(g.ProxyJump); err != nil {
		r.Err = fmt.Errorf("%s: group %s: %w", h.Name, h.Group, err)
	}
	if r.IdentityFile == "" {
		r.IdentityFile = c.Defaults.IdentityFile
	}

	tagSet := map[string]bool{}
	var tags []string
	for _, t := range append(append([]string{}, g.Tags...), h.Tags...) {
		if !tagSet[t] {
			tagSet[t] = true
			tags = append(tags, t)
		}
	}
	r.Tags = tags

	jump := h.ProxyJump
	if jump == "" && !direct {
		jump = g.ProxyJump
	}
	if jump != "" && !seen[h.Name] {
		seen[h.Name] = true
		for _, hop := range strings.Split(jump, ",") {
			hop = strings.TrimSpace(hop)
			if hop == "" {
				continue
			}
			if jh, _ := c.Find(hop); jh != nil && !seen[hop] {
				jr := c.resolve(*jh, nil, seen)
				if jr.Err != nil && r.Err == nil {
					r.Err = fmt.Errorf("via %w", jr.Err)
				}
				r.ProxyJump = append(r.ProxyJump, jr)
			} else {
				r.ProxyJumpRaw = appendComma(r.ProxyJumpRaw, hop)
			}
		}
	}

	return r
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func appendComma(base, s string) string {
	if base == "" {
		return s
	}
	return base + "," + s
}
