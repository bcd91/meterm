package team

import (
	"fmt"
	"strings"

	"meterm/internal/config"
)

// Describe is a host in one line: user@address:port via jump.
func (h SharedHost) Describe() string {
	s := h.Address
	if h.User != "" {
		s = h.User + "@" + s
	}
	if h.Port != 0 && h.Port != 22 {
		s += fmt.Sprintf(":%d", h.Port)
	}
	if h.ProxyJump != "" {
		s += " via " + h.ProxyJump
	}
	return s
}

// Diff lists, field by field, what changes from a to b — what a push would
// do to the team's copy. Bookkeeping (rev, who, when) is left out.
func Diff(a, b SharedHost) []string {
	var out []string
	f := func(name, x, y string) {
		if x != y {
			out = append(out, fmt.Sprintf("%s: %q → %q", name, x, y))
		}
	}
	f("address", a.Address, b.Address)
	f("user", a.User, b.User)
	f("users", strings.Join(a.Users, ","), strings.Join(b.Users, ","))
	f("port", fmt.Sprint(a.Port), fmt.Sprint(b.Port))
	f("group", a.Group, b.Group)
	f("tags", strings.Join(a.Tags, ","), strings.Join(b.Tags, ","))
	f("env", a.Env, b.Env)
	f("proxy_jump", a.ProxyJump, b.ProxyJump)
	f("forwards", describeForwards(a.Forwards), describeForwards(b.Forwards))
	f("extra_args", strings.Join(a.ExtraArgs, " "), strings.Join(b.ExtraArgs, " "))
	f("notes", a.Notes, b.Notes)
	f("routes", describeRoutes(a.Routes), describeRoutes(b.Routes))
	return out
}

// Problems counts the changes a person must sort out. A change that's only
// pending because this run didn't push isn't one.
func Problems(cs []Change) int {
	n := 0
	for _, c := range cs {
		if c.Outcome == Conflict || c.Outcome == NameTaken || c.Outcome == Forbidden {
			n++
		}
	}
	return n
}

// NeedingAttention is the changes whose outcome needs a look.
func NeedingAttention(cs []Change) []Change {
	var out []Change
	for _, c := range cs {
		if c.Outcome.NeedsAttention() {
			out = append(out, c)
		}
	}
	return out
}

// describeForwards is forwards for a diff line: "web -L 8080:localhost:80
// (Admin UI), -D 1080".
func describeForwards(fs []config.Forward) string {
	var parts []string
	for _, fw := range fs {
		var spec string
		switch fw.Type {
		case "D":
			spec = fmt.Sprintf("-D %d", fw.LocalPort)
		default:
			spec = fmt.Sprintf("-%s %d:%s:%d", fw.Type, fw.LocalPort, fw.RemoteHost, fw.RemotePort)
		}
		if fw.Name != "" {
			spec = fw.Name + " " + spec
		}
		if fw.Note != "" {
			spec += " (" + fw.Note + ")"
		}
		parts = append(parts, spec)
	}
	return strings.Join(parts, ", ")
}

// describeRoutes is routes for a diff line: "public 203.0.113.50:5000,
// relay-x 127.0.0.1:22100 via R".
func describeRoutes(rs []config.Route) string {
	var parts []string
	for _, rt := range rs {
		s := rt.Name + " " + rt.Address
		if rt.Port != 0 {
			s += fmt.Sprintf(":%d", rt.Port)
		}
		if rt.ProxyJump != "" {
			s += " via " + rt.ProxyJump
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}
