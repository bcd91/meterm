package route

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"meterm/internal/config"
)

// A route reaches the same machine at another address, so ssh would ask to
// trust "a new host" — and refuse outright in batch mode (exec, audit, and
// every jump hop). When the host's own address is already trusted, its keys
// are added to known_hosts under the route's address too: no new trust,
// just the same keys under another name. If a route really led somewhere
// else, ssh would then report a changed host key and stop.

// KnownHosts is the file host keys are carried into; tests change it.
var KnownHosts = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh", "known_hosts")
}

// knownName is how ssh names host:port in known_hosts.
func knownName(addr string, port int) string {
	if port == 0 || port == 22 {
		return addr
	}
	return "[" + addr + "]:" + strconv.Itoa(port)
}

// carryHostKey adds h's trusted keys under route rt's address, if they
// aren't there yet. It returns what it did, for --debug-style reporting.
func (p *Picker) carryHostKey(h config.Host, rt config.Route) (string, error) {
	file := KnownHosts()
	if file == "" {
		return "", nil
	}
	own := h.Candidates()[0]
	from := p.cfg.ResolveVia(h, &own)
	to := p.cfg.ResolveVia(h, &rt)
	fromName, toName := knownName(from.Address, from.Port), knownName(to.Address, to.Port)
	if fromName == toName {
		return "", nil
	}
	if have, _ := lookupKeys(file, toName); len(have) > 0 {
		return "", nil
	}
	keys, err := lookupKeys(file, fromName)
	if err != nil || len(keys) == 0 {
		return "", err
	}
	if err := appendKnown(file, to.Address, toName, keys); err != nil {
		return "", err
	}
	return fmt.Sprintf("trusted %s's host key for %s too", fromName, toName), nil
}

// AddKnownHosts trusts keys ("type base64") for addr:port in the user's
// known_hosts, hashed if their ssh config hashes host names.
func AddKnownHosts(addr string, port int, keys []string) error {
	file := KnownHosts()
	if file == "" {
		return fmt.Errorf("no home directory for known_hosts")
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return appendKnown(file, addr, knownName(addr, port), keys)
}

func appendKnown(file, addr, name string, keys []string) error {
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s %s\n", name, k)
	}
	lines := b.String()
	if hashesKnownHosts(addr) {
		var err error
		if lines, err = hashLines(lines); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(lines)
	return err
}

// lookupKeys returns "type base64" for each key known_hosts has for name,
// hashed entries included (via ssh-keygen -F). Revoked and CA lines are
// skipped.
func lookupKeys(file, name string) ([]string, error) {
	if _, err := os.Stat(file); err != nil {
		return nil, nil
	}
	out, err := exec.Command("ssh-keygen", "-F", name, "-f", file).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil // not found
		}
		return nil, err
	}
	var keys []string
	for _, line := range strings.Split(string(bytes.TrimSpace(out)), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || strings.HasPrefix(f[0], "#") || strings.HasPrefix(f[0], "@") {
			continue
		}
		keys = append(keys, f[1]+" "+f[2])
	}
	return keys, nil
}

// TrustedKeys is "type base64" for each key known_hosts trusts for a host
// at addr:port.
func TrustedKeys(addr string, port int) ([]string, error) {
	return lookupKeys(KnownHosts(), knownName(addr, port))
}

// KnownName is how ssh names addr:port in known_hosts.
func KnownName(addr string, port int) string { return knownName(addr, port) }

// hashesKnownHosts reports whether the user's ssh config hashes host names
// in known_hosts (HashKnownHosts yes), so added lines should be hashed too
// rather than leave the route's address readable there.
func hashesKnownHosts(addr string) bool {
	out, err := exec.Command("ssh", "-G", "--", addr).Output()
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(out), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "hashknownhosts" {
			return f[1] == "yes"
		}
	}
	return false
}

// hashLines hashes the host names of known_hosts lines, as ssh-keygen -H.
func hashLines(lines string) (string, error) {
	dir, err := os.MkdirTemp("", "meterm-kh")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(f, []byte(lines), 0o600); err != nil {
		return "", err
	}
	if out, err := exec.Command("ssh-keygen", "-H", "-f", f).CombinedOutput(); err != nil {
		return "", fmt.Errorf("ssh-keygen -H: %v: %s", err, out)
	}
	b, err := os.ReadFile(f)
	return string(b), err
}
