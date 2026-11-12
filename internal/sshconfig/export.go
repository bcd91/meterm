// Package sshconfig bridges meterm's inventory with the standard
// ~/.ssh/config file, so tools like scp, rsync, or git that only know plain
// ssh config still work against hosts defined in meterm.
package sshconfig

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"meterm/internal/config"
)

const (
	beginMarker = "# >>> meterm managed hosts (do not edit by hand; run `meterm export` to refresh) >>>"
	endMarker   = "# <<< meterm managed hosts <<<"
)

// Render generates an ssh_config-syntax block covering every host in cfg,
// using each host's fully resolved settings. Hosts in LeftOut are not in it.
func Render(cfg *config.Config) string {
	skip := map[string]bool{}
	for _, l := range LeftOut(cfg) {
		skip[l.Name] = true
	}
	var b strings.Builder
	fmt.Fprintln(&b, beginMarker)
	for _, h := range cfg.SortedHosts() {
		if skip[h.Name] {
			continue
		}
		r := cfg.Resolve(h)
		fmt.Fprintf(&b, "Host %s\n", h.Name)
		fmt.Fprintf(&b, "    HostName %s\n", r.Address)
		if r.User != "" {
			fmt.Fprintf(&b, "    User %s\n", r.User)
		}
		if r.Port != 0 {
			fmt.Fprintf(&b, "    Port %d\n", r.Port)
		}
		if r.IdentityFile != "" {
			fmt.Fprintf(&b, "    IdentityFile %s\n", quotePath(r.IdentityFile))
		}
		if spec := proxyJumpSpec(r); spec != "" {
			fmt.Fprintf(&b, "    ProxyJump %s\n", spec)
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintln(&b, endMarker)
	return b.String()
}

// Skipped is a host an export leaves out, and why.
type Skipped struct {
	Name, Why string
}

// LeftOut lists the hosts an export must leave out because a field would be
// read as more than one value, another line or a pattern by ssh_config (or
// by the shell-style split of an Ansible inventory): a host name or field
// from a hand-edited or team-shared hosts.yaml must never become a
// ProxyCommand. Hosts that jump through a left-out host go too.
func LeftOut(cfg *config.Config) []Skipped {
	hosts := cfg.SortedHosts()
	why := map[string]string{}
	for _, h := range hosts {
		r := cfg.Resolve(h)
		fields := []struct {
			field, value string
			name         bool
		}{
			{"name", h.Name, true},
			{"address", r.Address, false},
			{"user", r.User, false},
			{"jump", r.ProxyJumpRaw, false},
		}
		for _, f := range fields {
			if strings.Contains(f.value, "{{") || strings.Contains(f.value, "{%") || strings.Contains(f.value, "{#") {
				why[h.Name] = fmt.Sprintf("its %s %q looks like an Ansible template", f.field, f.value)
				break
			}
			if f.name && strings.HasPrefix(f.value, "#") {
				why[h.Name] = "its name starts with '#', which ssh reads as a comment"
				break
			}
			if c, bad := unsafeRune(f.value, f.name); bad {
				why[h.Name] = fmt.Sprintf("its %s %q contains %q", f.field, f.value, c)
				if f.name {
					why[h.Name] = fmt.Sprintf("its name contains %q", c)
				}
				break
			}
		}
		if _, ok := why[h.Name]; !ok {
			if p := pathProblem(r.IdentityFile); p != "" {
				why[h.Name] = fmt.Sprintf("its identity file %q %s", r.IdentityFile, p)
			}
		}
		if _, ok := why[h.Name]; !ok {
			// What meterm won't connect to either (shell characters in a
			// jump, an address starting with '-'): the host's own part of
			// Resolve's Err. A jump host's part is the loop below.
			err := h.CheckConnection()
			if err == nil {
				err = config.CheckJump(cfg.Groups[h.Group].ProxyJump)
			}
			if err != nil {
				why[h.Name] = fmt.Sprintf("meterm refuses it (%q)", err.Error())
			}
		}
		if _, ok := why[h.Name]; !ok && (r.Port < 0 || r.Port > 65535) {
			why[h.Name] = fmt.Sprintf("its port %d is out of range", r.Port)
		}
	}
	// A ProxyJump hop "h#1" is read as h, "ops@b" as user ops on b and
	// "app:1" as port 1 on app: a host that jumps through a host with those
	// in its name is left out (that host itself is fine).
	for _, h := range hosts {
		if _, ok := why[h.Name]; ok {
			continue
		}
		for _, jh := range cfg.Resolve(h).ProxyJump {
			if i := strings.IndexAny(jh.Name, "#@:"); i >= 0 {
				why[h.Name] = fmt.Sprintf("it jumps through %q, and ssh would misread the %q in that name", jh.Name, jh.Name[i])
				break
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, h := range hosts {
			if _, ok := why[h.Name]; ok {
				continue
			}
			r := cfg.Resolve(h)
			hops := strings.Split(r.ProxyJumpRaw, ",")
			for _, jh := range r.ProxyJump {
				hops = append(hops, jh.Name)
			}
			for _, hop := range hops {
				if _, bad := why[hop]; bad {
					why[h.Name] = fmt.Sprintf("it jumps through %q, which is left out", hop)
					changed = true
					break
				}
			}
		}
	}
	for _, h := range hosts {
		if _, ok := why[h.Name]; !ok {
			if err := cfg.Resolve(h).Err; err != nil { // anything else Resolve refuses
				why[h.Name] = fmt.Sprintf("meterm refuses it (%q)", err.Error())
			}
		}
	}
	var out []Skipped
	for _, h := range hosts {
		if w, ok := why[h.Name]; ok {
			out = append(out, Skipped{Name: h.Name, Why: w})
		}
	}
	return out
}

// unsafeRune finds a character ssh_config would split on, treat as a
// comment, quote, escape or "=", or that hides what's written (control and
// invisible or direction-changing format characters). In a host name the
// pattern characters * ? ! , are refused too, but not '#', which ssh reads
// as a comment only at the start of a word.
func unsafeRune(s string, name bool) (rune, bool) {
	for _, c := range s {
		switch {
		case c == utf8.RuneError, unicode.IsSpace(c), unicode.IsControl(c), // C0, DEL, C1
			unicode.Is(unicode.Cf, c), // U+00AD, U+200B-U+200F, U+202A-U+202E, U+2060-U+2064, U+2066-U+2069, U+FEFF
			c == '\u2028', c == '\u2029',
			strings.ContainsRune("\"'\\=", c), c == '#' && !name, // ssh accepts "Host web#2"
			name && strings.ContainsRune("*?!,", c):
			return c, true
		}
	}
	return 0, false
}

// pathProblem says why an identity file can't be written into ssh_config
// as it is, or "" when it can. Unlike other fields it may hold backslashes
// and a drive letter (C:\Users\me\.ssh\id_ed25519), and a space, '#', a
// single quote or '=' (it's then double-quoted). ssh_config reads a backslash
// before another backslash, a quote or a space as an escape, so those
// sequences (and one at the end, which would escape the closing quote)
// are refused rather than rewritten.
func pathProblem(s string) string {
	if strings.Contains(s, "{{") || strings.Contains(s, "{%") || strings.Contains(s, "{#") {
		return "looks like an Ansible template"
	}
	for i, c := range s {
		switch {
		case c == '"':
			return "contains a double quote"
		case c == ' ' || c == '#' || c == '\'' || c == '=':
			// fine inside the double quotes quotePath adds
		case c == '\\':
			if next := s[i+1:]; next == "" || strings.ContainsAny(next[:1], "\\\"' ") {
				return "has a backslash ssh_config would read as an escape"
			}
		default:
			if r, bad := unsafeRune(string(c), false); bad {
				return fmt.Sprintf("contains %q", r)
			}
		}
	}
	return ""
}

// quotePath double-quotes a path that pathProblem accepted when it holds a
// character ssh_config would otherwise split on or treat as a comment.
func quotePath(s string) string {
	if strings.ContainsAny(s, " #'=") {
		return `"` + s + `"`
	}
	return s
}

func proxyJumpSpec(r config.Resolved) string {
	var hops []string
	for _, jh := range r.ProxyJump {
		hops = append(hops, jh.Name)
	}
	spec := strings.Join(hops, ",")
	if r.ProxyJumpRaw != "" {
		if spec != "" {
			spec += ","
		}
		spec += r.ProxyJumpRaw
	}
	return spec
}

// GeneratedPath is where the exported, meterm-managed ssh_config fragment lives.
func GeneratedPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "meterm", "ssh_config"), nil
}

// Write renders cfg and saves it to GeneratedPath, returning that path.
func Write(cfg *config.Config) (string, error) {
	path, err := GeneratedPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(Render(cfg)), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// EnsureIncluded makes sure ~/.ssh/config has an `Include` line pointing at
// the meterm-managed fragment, appending one if it's missing. It never
// touches the rest of the file.
func EnsureIncluded(generatedPath string) (added bool, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	sshConfigPath := filepath.Join(home, ".ssh", "config")
	includeLine := "Include " + generatedPath

	if f, err := os.Open(sshConfigPath); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			if strings.TrimSpace(scanner.Text()) == includeLine {
				return false, nil
			}
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}

	if err := os.MkdirAll(filepath.Dir(sshConfigPath), 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(sshConfigPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	// Include directives only apply to what follows them, so this must be
	// the very first thing in the file to take priority over later blocks.
	existing, _ := os.ReadFile(sshConfigPath)
	if len(existing) > 0 {
		content := includeLine + "\n\n" + string(existing)
		f.Close()
		if err := os.WriteFile(sshConfigPath, []byte(content), 0o600); err != nil {
			return false, err
		}
		return true, nil
	}
	if _, err := fmt.Fprintln(f, includeLine); err != nil {
		return false, err
	}
	return true, nil
}
