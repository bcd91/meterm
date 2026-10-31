package config

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// What a host may contain before meterm hands it to ssh, scp, sftp or
// rsync. Host entries arrive from teammates (team sync), so a field that
// ssh would read as an option — an address starting with "-", an
// extra_args "-oProxyCommand=…" — would run a command on every member's
// machine. These checks run when a team host is pulled and again whenever a
// host is resolved to connect.

var (
	addrRE = regexp.MustCompile(`^[A-Za-z0-9_:][A-Za-z0-9._:%-]*$|^\[[0-9A-Fa-f:.%A-Za-z]+\]$`)
	userRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	nameRE = regexp.MustCompile(`^[^\s-][^\s]*$`)
	portRE = regexp.MustCompile(`^[0-9]{1,5}$`)
)

// CheckAddress rejects anything but a hostname or an IP address.
func CheckAddress(s string) error {
	if s == "" || len(s) > 253 || !addrRE.MatchString(s) {
		return fmt.Errorf("address %q isn't a hostname or IP address", s)
	}
	return nil
}

// CheckUser rejects anything but a plain login name.
func CheckUser(s string) error {
	if s != "" && (len(s) > 64 || !userRE.MatchString(s)) {
		return fmt.Errorf("user %q isn't a plain login name", s)
	}
	return nil
}

// CheckJump accepts a comma-separated jump path: saved host names, or
// [user@]host[:port] specs.
func CheckJump(spec string) error {
	if spec == "" {
		return nil
	}
	for _, hop := range strings.Split(spec, ",") {
		hop = strings.TrimSpace(hop)
		if hop == "" || strings.HasPrefix(hop, "-") || hasControl(hop) || strings.ContainsAny(hop, " \t'\"`$\\;&|<>()*?") {
			return fmt.Errorf("jump host %q isn't a host name or [user@]host[:port]", hop)
		}
		host := hop
		if i := strings.LastIndex(host, "@"); i >= 0 {
			if err := CheckUser(host[:i]); err != nil {
				return fmt.Errorf("jump host %q: %w", hop, err)
			}
			host = host[i+1:]
		}
		if strings.HasPrefix(host, "[") {
			if j := strings.Index(host, "]"); j > 0 && (j == len(host)-1 || (host[j+1] == ':' && portRE.MatchString(host[j+2:]))) {
				host = host[:j+1]
			}
		} else if i := strings.LastIndex(host, ":"); i > 0 && strings.Count(host, ":") == 1 && portRE.MatchString(host[i+1:]) {
			host = host[:i]
		}
		if CheckAddress(host) != nil && !nameRE.MatchString(host) {
			return fmt.Errorf("jump host %q isn't a host name or [user@]host[:port]", hop)
		}
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || r == ' ' || r == ' ' || (r >= '‪' && r <= '‮') || (r >= '⁦' && r <= '⁩') {
			return true
		}
	}
	return false
}

// CheckText rejects control characters — terminal escapes, line breaks,
// bidi overrides — in text that gets printed: names, notes, tags, groups.
// A team-shared note with an escape sequence could otherwise rewrite the
// terminal or set the clipboard.
func CheckText(field, s string) error {
	if hasControl(s) {
		return fmt.Errorf("%s contains control characters", field)
	}
	return nil
}

// safeOptions are the ssh -o options a team may set on a shared host:
// connection tuning only. Anything that runs a program (ProxyCommand,
// LocalCommand, KnownHostsCommand), loads a library (PKCS11Provider,
// SecurityKeyProvider), weakens host checking (StrictHostKeyChecking,
// UserKnownHostsFile) or forwards credentials (ForwardAgent) is refused.
var safeOptions = map[string]bool{
	"serveraliveinterval": true, "serveralivecountmax": true, "connecttimeout": true,
	"connectionattempts": true, "compression": true, "tcpkeepalive": true, "ipqos": true,
	"addressfamily": true, "rekeylimit": true, "kexalgorithms": true, "ciphers": true,
	"macs": true, "hostkeyalgorithms": true, "pubkeyacceptedalgorithms": true,
	"pubkeyacceptedkeytypes": true, "loglevel": true, "requesttty": true, "escapechar": true,
	"preferredauthentications": true, "numberofpasswordprompts": true, "batchmode": true,
	"checkhostip": true, "visualhostkey": true, "updatehostkeys": true,
}

// safeFlags are the bare ssh flags a team may set.
var safeFlags = map[string]bool{"-4": true, "-6": true, "-C": true, "-q": true, "-v": true, "-vv": true, "-vvv": true, "-T": true, "-t": true}

// CheckSharedExtraArgs accepts only the extra ssh arguments it's safe for
// a teammate to set: a few bare flags and connection-tuning -o options.
func CheckSharedExtraArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if safeFlags[a] {
			continue
		}
		opt := ""
		switch {
		case a == "-o" && i+1 < len(args):
			i++
			opt = args[i]
		case strings.HasPrefix(a, "-o") && len(a) > 2:
			opt = a[2:]
		default:
			return fmt.Errorf("extra ssh argument %q isn't allowed on a team host", a)
		}
		key, val, ok := strings.Cut(opt, "=")
		if !ok {
			key, val, ok = strings.Cut(strings.TrimSpace(opt), " ")
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if !ok || !safeOptions[key] || hasControl(val) || strings.ContainsAny(val, "%$`") {
			return fmt.Errorf("ssh option %q isn't allowed on a team host (only connection tuning is)", opt)
		}
	}
	return nil
}

// CheckConnection checks the fields of h that reach ssh's command line:
// address, user, jump path, routes and forwards; and extra args when the
// host is shared with a team.
func (h Host) CheckConnection() error {
	if err := CheckAddress(h.Address); err != nil {
		return err
	}
	if err := CheckUser(h.User); err != nil {
		return err
	}
	for _, u := range h.Users {
		if u == "" {
			return fmt.Errorf("an empty name in users")
		}
		if err := CheckUser(u); err != nil {
			return err
		}
	}
	if err := CheckJump(h.ProxyJump); err != nil {
		return err
	}
	for _, rt := range h.Routes {
		if rt.Address != "" {
			if err := CheckAddress(rt.Address); err != nil {
				return fmt.Errorf("route %q: %w", rt.Name, err)
			}
		}
		if err := CheckJump(rt.ProxyJump); err != nil {
			return fmt.Errorf("route %q: %w", rt.Name, err)
		}
		if rt.Port < 0 || rt.Port > 65535 {
			return fmt.Errorf("route %q: port %d is out of range", rt.Name, rt.Port)
		}
	}
	for _, f := range h.Forwards {
		if f.RemoteHost != "" {
			if err := CheckAddress(f.RemoteHost); err != nil {
				return fmt.Errorf("forward: %w", err)
			}
		}
	}
	if h.Port < 0 || h.Port > 65535 {
		return fmt.Errorf("port %d is out of range", h.Port)
	}
	if h.Team != "" {
		if err := CheckSharedExtraArgs(h.ExtraArgs); err != nil {
			return err
		}
	}
	return nil
}

// CheckShared checks everything about a host that came from a team: the
// connection fields, the extra args against the team allowlist, and the
// text fields for control characters.
func (h Host) CheckShared() error {
	h.Team = "shared" // apply the extra-args allowlist
	if err := h.CheckConnection(); err != nil {
		return err
	}
	if h.Name == "" || strings.HasPrefix(h.Name, "-") || len(h.Name) > 128 {
		return fmt.Errorf("host name %q isn't usable", h.Name)
	}
	for _, f := range []struct{ name, val string }{{"name", h.Name}, {"notes", h.Notes}, {"group", h.Group}} {
		if err := CheckText(f.name, f.val); err != nil {
			return err
		}
	}
	if _, err := NormalizeEnv(h.Env); err != nil {
		return err
	}
	for _, t := range h.Tags {
		if err := CheckText("a tag", t); err != nil {
			return err
		}
	}
	for _, rt := range h.Routes {
		if err := CheckText("a route name", rt.Name); err != nil {
			return err
		}
	}
	for _, f := range h.Forwards {
		if err := CheckText("a tunnel name", f.Name); err != nil {
			return err
		}
		if err := CheckText("a tunnel note", f.Note); err != nil {
			return err
		}
		if len(f.Note) > 200 {
			return fmt.Errorf("tunnel %q: note is longer than 200 characters", f.Name)
		}
	}
	return nil
}
