package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"meterm/internal/termsafe"
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

// hasControl says s holds a character termsafe.Label wouldn't show as it
// is: a control character, a bidi control, or an invisible one (zero-width
// space or joiner, soft hyphen, BOM...). Refusing them keeps two names
// that show alike from being two hosts.
func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || termsafe.Drops(r) {
			return true
		}
	}
	return false
}

// CheckText rejects control characters — terminal escapes, line breaks,
// bidi overrides, invisible characters — in text that gets printed: names, notes, tags, groups.
// A team-shared note with an escape sequence could otherwise rewrite the
// terminal or set the clipboard.
func CheckText(field, s string) error {
	if hasControl(s) {
		return fmt.Errorf("%s contains control or invisible characters", field)
	}
	return nil
}

// ErrMixedScript is a name that mixes Latin, Cyrillic or Greek letters:
// "pаy1" with a Cyrillic а shows as "pay1" and is another host.
var ErrMixedScript = errors.New("mixes letters from different alphabets, so it can look like another name")

// MixedScripts names the alphabets s mixes ("Latin and Cyrillic"), or ""
// when it doesn't mix Latin, Cyrillic and Greek. Other scripts (Thai, CJK)
// alongside Latin are fine: they can't pass for Latin letters.
func MixedScripts(s string) string {
	var found []string
	for _, sc := range []struct {
		name string
		t    *unicode.RangeTable
	}{{"Latin", unicode.Latin}, {"Cyrillic", unicode.Cyrillic}, {"Greek", unicode.Greek}} {
		for _, r := range s {
			if unicode.IsLetter(r) && unicode.Is(sc.t, r) {
				found = append(found, sc.name)
				break
			}
		}
	}
	if len(found) < 2 {
		return ""
	}
	return strings.Join(found, " and ")
}

// CheckName is CheckText for a name, which also mustn't mix alphabets that
// share look-alike letters (MixedScripts).
func CheckName(field, s string) error {
	if err := CheckText(field, s); err != nil {
		return err
	}
	if m := MixedScripts(s); m != "" {
		return fmt.Errorf("%s %q %w (%s)", field, s, ErrMixedScript, m)
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
	for _, f := range []struct{ name, val string }{{"name", h.Name}, {"group", h.Group}} {
		if err := CheckName(f.name, f.val); err != nil {
			return err
		}
	}
	if err := CheckText("notes", h.Notes); err != nil {
		return err
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
		if err := CheckName("a route name", rt.Name); err != nil {
			return err
		}
	}
	for _, f := range h.Forwards {
		if err := CheckName("a tunnel name", f.Name); err != nil {
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

// CheckPrintable rejects control characters in every field of h that gets
// printed, naming the field. add, edit, rename and import run it on what
// they're given, so an escape sequence never gets into the host file from
// this machine. Hosts already saved with one still load: everything that
// shows them cleans them up for display.
func (h Host) CheckPrintable() error {
	fields := []struct{ name, val string }{
		{"name", h.Name}, {"address", h.Address}, {"user", h.User}, {"group", h.Group},
		{"notes", h.Notes}, {"identity_file", h.IdentityFile}, {"proxy_jump", h.ProxyJump},
		{"env", h.Env},
	}
	for _, u := range h.Users {
		fields = append(fields, struct{ name, val string }{"users", u})
	}
	for _, t := range h.Tags {
		fields = append(fields, struct{ name, val string }{"a tag", t})
	}
	for _, rt := range h.Routes {
		fields = append(fields, struct{ name, val string }{"a route name", rt.Name},
			struct{ name, val string }{"a route address", rt.Address},
			struct{ name, val string }{"a route proxy_jump", rt.ProxyJump})
	}
	for _, f := range h.Forwards {
		fields = append(fields, struct{ name, val string }{"a tunnel name", f.Name},
			struct{ name, val string }{"a tunnel note", f.Note},
			struct{ name, val string }{"a tunnel remote_host", f.RemoteHost})
	}
	for _, f := range fields {
		if err := CheckText(f.name, f.val); err != nil {
			return err
		}
	}
	for _, f := range []struct{ name, val string }{{"name", h.Name}, {"group", h.Group}} {
		if err := CheckName(f.name, f.val); err != nil {
			return err
		}
	}
	for _, f := range h.Forwards {
		if err := CheckName("a tunnel name", f.Name); err != nil {
			return err
		}
	}
	for _, rt := range h.Routes {
		if err := CheckName("a route name", rt.Name); err != nil {
			return err
		}
	}
	return nil
}
