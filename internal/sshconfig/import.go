package sshconfig

import (
	"bufio"
	"os"
	"strconv"
	"strings"

	"meterm/internal/config"
)

// ParseFile reads a standard ssh_config file and extracts single-pattern
// Host blocks (skipping wildcards like "*" or multi-pattern lines, which
// aren't concrete machines) as meterm hosts.
func ParseFile(path string) ([]config.Host, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var hosts []config.Host
	var cur *config.Host
	skip := false

	flush := func() {
		if cur != nil && !skip && cur.Address != "" {
			hosts = append(hosts, *cur)
		}
		cur = nil
		skip = false
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := splitDirective(line)
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "host":
			flush()
			fields := strings.Fields(val)
			if len(fields) != 1 || strings.ContainsAny(fields[0], "*?") {
				skip = true
				continue
			}
			cur = &config.Host{Name: fields[0], Address: fields[0]}
		case "hostname":
			if cur != nil {
				cur.Address = val
			}
		case "user":
			if cur != nil {
				cur.User = val
			}
		case "port":
			if cur != nil {
				if p, err := strconv.Atoi(val); err == nil {
					cur.Port = p
				}
			}
		case "identityfile":
			if cur != nil {
				cur.IdentityFile = val
			}
		case "proxyjump":
			if cur != nil {
				cur.ProxyJump = val
			}
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return hosts, nil
}

func splitDirective(line string) (key, val string, ok bool) {
	// ssh_config allows "Key value" or "Key=value" or "Key = value".
	normalized := strings.Map(func(r rune) rune {
		if r == '=' {
			return ' '
		}
		return r
	}, line)
	fields := strings.Fields(normalized)
	if len(fields) < 2 {
		return "", "", false
	}
	val = strings.Trim(strings.Join(fields[1:], " "), "\"")
	return fields[0], val, true
}
