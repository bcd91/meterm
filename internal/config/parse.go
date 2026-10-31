package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseLocalForward reads LOCAL_PORT:REMOTE_HOST:REMOTE_PORT, as for ssh -L.
func ParseLocalForward(spec string) (Forward, error) {
	parts := strings.SplitN(spec, ":", 3)
	bad := fmt.Errorf("forward %q: want LOCAL_PORT:REMOTE_HOST:REMOTE_PORT, e.g. 5432:localhost:5432", spec)
	if len(parts) != 3 || parts[1] == "" {
		return Forward{}, bad
	}
	lp, err1 := strconv.Atoi(parts[0])
	rp, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || lp < 1 || lp > 65535 || rp < 1 || rp > 65535 {
		return Forward{}, bad
	}
	return Forward{Type: "L", LocalPort: lp, RemoteHost: parts[1], RemotePort: rp}, nil
}

// SplitTags reads a comma-separated tag list, dropping blanks.
func SplitTags(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// AddForward adds f to h unless an identical forward is already there.
func (h *Host) AddForward(f Forward) {
	for _, have := range h.Forwards {
		if have == f {
			return
		}
	}
	h.Forwards = append(h.Forwards, f)
}
