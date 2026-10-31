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

	"meterm/internal/config"
)

const (
	beginMarker = "# >>> meterm managed hosts (do not edit by hand; run `meterm export` to refresh) >>>"
	endMarker   = "# <<< meterm managed hosts <<<"
)

// Render generates an ssh_config-syntax block covering every host in cfg,
// using each host's fully resolved settings.
func Render(cfg *config.Config) string {
	var b strings.Builder
	fmt.Fprintln(&b, beginMarker)
	for _, h := range cfg.SortedHosts() {
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
			fmt.Fprintf(&b, "    IdentityFile %s\n", r.IdentityFile)
		}
		if spec := proxyJumpSpec(r); spec != "" {
			fmt.Fprintf(&b, "    ProxyJump %s\n", spec)
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintln(&b, endMarker)
	return b.String()
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
