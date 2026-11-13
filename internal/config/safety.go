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
func CheckJump(spec string) error { return checkJump(spec, refused) }

func checkJump(spec string, ctl func(rune) bool) error {
	return checkJumpBy(spec, func(string) func(rune) bool { return ctl })
}

// checkJumpBy is checkJump with the characters refused chosen per hop.
func checkJumpBy(spec string, pick func(hop string) func(rune) bool) error {
	if spec == "" {
		return nil
	}
	for _, hop := range strings.Split(spec, ",") {
		hop = strings.TrimSpace(hop)
		if hop == "" || strings.HasPrefix(hop, "-") || has(hop, pick(hop)) || strings.ContainsAny(hop, " \t'\"`$\\;&|<>()*?") {
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

// refused says r is a character termsafe.Label wouldn't show as it is: a
// control character, a bidi control, or an invisible one (zero-width space
// or joiner, soft hyphen, BOM...). Refusing them keeps two names that show
// alike from being two hosts.
func refused(r rune) bool { return unicode.IsControl(r) || termsafe.Drops(r) }

// refusedInNote is refused less the zero-width joiner and non-joiner, which
// prose needs: emoji sequences hold a joiner (👨\u200d💻, 🏳️\u200d🌈) and Persian
// writes with the non-joiner. Display drops both, and a note doesn't tell
// one host from another, so they can't make two hosts pass for one.
func refusedInNote(r rune) bool { return r != 0x200c && r != 0x200d && refused(r) }

// refusedBefore is what v0.23.1 refused in text from a team: control
// characters (C0, C1, DEL — escapes and line breaks among them), the line
// and paragraph separators, and the bidi embeddings, overrides and
// isolates. A team host holding anything else that refused takes was
// accepted then, so it is kept now, with a warning (CheckFromTeam).
func refusedBefore(r rune) bool {
	return unicode.IsControl(r) || r == 0x2028 || r == 0x2029 || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

func has(s string, ctl func(rune) bool) bool {
	for _, r := range s {
		if ctl(r) {
			return true
		}
	}
	return false
}

func hasControl(s string) bool { return has(s, refused) }

// CheckText rejects control characters — terminal escapes, line breaks,
// bidi overrides, invisible characters — in text that gets printed: names, notes, tags, groups.
// A team-shared note with an escape sequence could otherwise rewrite the
// terminal or set the clipboard.
func CheckText(field, s string) error { return checkText(field, s, refused) }

// CheckNote is CheckText for free text — notes, tunnel notes — which may
// also hold the zero-width joiner and non-joiner (refusedInNote).
func CheckNote(field, s string) error { return checkText(field, s, refusedInNote) }

func checkText(field, s string, ctl func(rune) bool) error {
	if has(s, ctl) {
		return fmt.Errorf("%s contains control or invisible characters", field)
	}
	return nil
}

// ErrMixedScript is a name that mixes Latin, Cyrillic or Greek letters:
// "pаy1" with a Cyrillic а shows as "pay1" and is another host.
var ErrMixedScript = errors.New("mixes letters from different alphabets, so it can look like another name")

// MixedScripts names the alphabets s mixes ("Latin and Cyrillic"), or ""
// when it mixes none of Latin, Cyrillic and Greek in a way that can look
// like another name. A word is a run of letters (with their combining
// marks): "-", "_", ".", digits and anything else that isn't a letter split
// words. Two things count as mixing:
//
//   - one word holding letters of two of the three alphabets: "pаypal"
//     with a Cyrillic а;
//   - in a name that has Latin letters, a Cyrillic or Greek word made only
//     of letters that look Latin (lookalikeLatin): "web-арр" in Cyrillic
//     shows as "web-app", "web1а" as "web1a".
//
// So "db-москва" and "api-μ" are fine, and so are other scripts (Thai,
// Persian, CJK) alongside Latin: they can't pass for Latin letters.
func MixedScripts(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsMark(r) })
	latin := false
	for _, w := range words {
		var found []string
		for _, sc := range scripts {
			for _, r := range w {
				if unicode.IsLetter(r) && unicode.Is(sc.t, r) {
					found = append(found, sc.name)
					break
				}
			}
		}
		if len(found) >= 2 {
			return strings.Join(found, " and ")
		}
		latin = latin || len(found) == 1 && found[0] == "Latin"
	}
	if !latin {
		return ""
	}
	for _, w := range words {
		for _, sc := range scripts[1:] {
			if allLookalike(w, sc.t) {
				return "Latin and " + sc.name
			}
		}
	}
	return ""
}

var scripts = []struct {
	name string
	t    *unicode.RangeTable
}{{"Latin", unicode.Latin}, {"Cyrillic", unicode.Cyrillic}, {"Greek", unicode.Greek}}

// allLookalike says every letter of w is in script t and in lookalikeLatin.
func allLookalike(w string, t *unicode.RangeTable) bool {
	n := 0
	for _, r := range w {
		if unicode.IsMark(r) {
			continue
		}
		if !unicode.Is(t, r) || !lookalikeLatin[r] {
			return false
		}
		n++
	}
	return n > 0
}

// lookalikeLatin are the Cyrillic and Greek letters that show as a Latin
// letter in common fonts. Picked by hand from the Latin look-alikes in
// Unicode's confusables data (UTS #39, confusables.txt), keeping only
// letters that read as a plain Latin letter. Lowercase letters shaped like
// small capitals (Cyrillic в к м н т, Greek κ τ) are left out on purpose:
// they don't pass for Latin in running text, and with them ordinary words
// such as "москва" would be refused.
var lookalikeLatin = func() map[rune]bool {
	m := map[rune]bool{}
	for _, r := range "" +
		"\u0430\u0435\u043e\u0440\u0441\u0443\u0445\u0456\u0458\u0455\u0501\u051b\u051d\u04af\u04bb\u04cf" + // Cyrillic lowercase: a e o p c y x i j s d q w y h l
		"\u0410\u0412\u0415\u041a\u041c\u041d\u041e\u0420\u0421\u0422\u0425\u0405\u0406\u0408\u04ae" + // Cyrillic capitals: A B E K M H O P C T X S I J Y
		"\u0391\u0392\u0395\u0396\u0397\u0399\u039a\u039c\u039d\u039f\u03a1\u03a4\u03a5\u03a7" + // Greek capitals: A B E Z H I K M N O P T Y X
		"\u03bf\u03bd\u03c1\u03c5\u03c7\u03b9" { // Greek lowercase: o v p u x i
		m[r] = true
	}
	return m
}()

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
func CheckSharedExtraArgs(args []string) error { return checkSharedExtraArgs(args, refused) }

func checkSharedExtraArgs(args []string, ctl func(rune) bool) error {
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
		if !ok || !safeOptions[key] || has(val, ctl) || strings.ContainsAny(val, "%$`") {
			return fmt.Errorf("ssh option %q isn't allowed on a team host (only connection tuning is)", opt)
		}
	}
	return nil
}

// CheckConnection checks the fields of h that reach ssh's command line:
// address, user, jump path, routes and forwards; and extra args when the
// host is shared with a team.
func (h Host) CheckConnection() error { return h.checkConnection(refused) }

// checkConnection checks what reaches ssh's command line; jump paths are
// held to the characters ctl refuses.
func (h Host) checkConnection(ctl func(rune) bool) error {
	return h.checkConnectionJump(ctl, func(spec string) error { return checkJump(spec, ctl) })
}

// checkConnection is CheckConnection as connect runs it, against this file.
// A hop naming a saved host is held only to what v0.23.1 refused
// (refusedBefore): the name never reaches ssh, the saved host's address
// does, and a host v0.23.1 saved or a team handed over with a zero-width
// space in its name is still reachable through. A raw [user@]host[:port]
// hop goes to ssh as it is and keeps the strict rule.
func (c *Config) checkConnection(h Host) error {
	return h.checkConnectionJump(refused, c.checkJump)
}

// checkJump is CheckJump with the saved-host leniency of checkConnection.
func (c *Config) checkJump(spec string) error {
	return checkJumpBy(spec, func(hop string) func(rune) bool {
		if jh, _ := c.Find(hop); jh != nil {
			return refusedBefore
		}
		return refused
	})
}

func (h Host) checkConnectionJump(ctl func(rune) bool, checkJump func(string) error) error {
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
	if err := checkJump(h.ProxyJump); err != nil {
		return err
	}
	for _, rt := range h.Routes {
		if rt.Address != "" {
			if err := CheckAddress(rt.Address); err != nil {
				return fmt.Errorf("route %q: %w", rt.Name, err)
			}
		}
		if err := checkJump(rt.ProxyJump); err != nil {
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
		if err := checkSharedExtraArgs(h.ExtraArgs, ctl); err != nil {
			return err
		}
	}
	return nil
}

// CheckShared checks everything about a host that came from a team: the
// connection fields, the extra args against the team allowlist, and the
// text fields for control and invisible characters and look-alike names.
func (h Host) CheckShared() error { return h.checkShared(true) }

// CheckFromTeam is CheckShared for a host a team already holds — pulled,
// taken with `team resolve --theirs`, or pushed again after an edit. err is
// what v0.23.1 refused too (escape sequences, line breaks, bidi controls,
// unsafe ssh arguments): such a host isn't used. warn is what only this
// version refuses (invisible characters, a word mixing alphabets): the
// team already had that host, so it is kept and flagged rather than
// dropped, as a local host saved with one still loads.
func (h Host) CheckFromTeam() (warn, err error) {
	if err := h.checkShared(false); err != nil {
		return nil, err
	}
	return h.checkShared(true), nil
}

// checkShared is CheckShared; with strict false it applies only the rules
// v0.23.1 did (refusedBefore, no alphabet check).
func (h Host) checkShared(strict bool) error {
	ctl, note := refusedBefore, refusedBefore
	if strict {
		ctl, note = refused, refusedInNote
	}
	name := func(field, s string) error {
		if !strict {
			return checkText(field, s, ctl)
		}
		return CheckName(field, s)
	}
	h.Team = "shared" // apply the extra-args allowlist
	if err := h.checkConnection(ctl); err != nil {
		return err
	}
	if h.Name == "" || strings.HasPrefix(h.Name, "-") || len(h.Name) > 128 {
		return fmt.Errorf("host name %q isn't usable", h.Name)
	}
	for _, f := range []struct{ name, val string }{{"name", h.Name}, {"group", h.Group}} {
		if err := name(f.name, f.val); err != nil {
			return err
		}
	}
	if err := checkText("notes", h.Notes, note); err != nil {
		return err
	}
	if _, err := NormalizeEnv(h.Env); err != nil {
		return err
	}
	for _, t := range h.Tags {
		if err := checkText("a tag", t, ctl); err != nil {
			return err
		}
	}
	for _, rt := range h.Routes {
		if err := name("a route name", rt.Name); err != nil {
			return err
		}
	}
	for _, f := range h.Forwards {
		if err := name("a tunnel name", f.Name); err != nil {
			return err
		}
		if err := checkText("a tunnel note", f.Note, note); err != nil {
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
		{"identity_file", h.IdentityFile}, {"proxy_jump", h.ProxyJump},
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
			struct{ name, val string }{"a tunnel remote_host", f.RemoteHost})
	}
	for _, f := range fields {
		if err := CheckText(f.name, f.val); err != nil {
			return err
		}
	}
	notes := []string{h.Notes}
	for _, f := range h.Forwards {
		notes = append(notes, f.Note)
	}
	for i, n := range notes {
		field := "notes"
		if i > 0 {
			field = "a tunnel note"
		}
		if err := CheckNote(field, n); err != nil {
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
