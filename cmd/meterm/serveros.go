package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"meterm/internal/config"
	"meterm/internal/sshrun"
)

// Which hosts run Windows' OpenSSH, remembered so commands that send a
// shell script (stats, logs) can send PowerShell instead without checking
// every time. The check reads the version the server announces before
// login (sshrun.RemoteSoftware).

const serverOSTTL = 30 * 24 * time.Hour

type serverOSEntry struct {
	Target   string    `json:"target"` // what was checked: a changed address re-checks
	Software string    `json:"software"`
	Checked  time.Time `json:"checked"`
}

func serverOSPath() string {
	p, err := resolveCfgPath()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(p), "server-os.json")
}

func checkTarget(r config.Resolved) string {
	var hops []string
	for _, j := range r.ProxyJump {
		hops = append(hops, j.Address)
	}
	return fmt.Sprintf("%s@%s:%d via %s %s", r.User, r.Address, r.Port, strings.Join(hops, ","), r.ProxyJumpRaw)
}

// isWindowsServer reports whether r is a Windows OpenSSH server, from the
// cache or by checking (and caching what it finds).
func isWindowsServer(r config.Resolved) bool {
	path := serverOSPath()
	cache := map[string]serverOSEntry{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &cache)
	}
	target := checkTarget(r)
	if e, ok := cache[r.Name]; ok && e.Target == target && time.Since(e.Checked) < serverOSTTL {
		return sshrun.IsWindowsServer(e.Software)
	}
	sw := sshrun.RemoteSoftware(r)
	if sw == "" {
		return false // couldn't tell: the POSIX path, as before
	}
	cache[r.Name] = serverOSEntry{Target: target, Software: sw, Checked: time.Now()}
	if b, err := json.MarshalIndent(cache, "", "  "); err == nil && path != "" {
		_ = os.WriteFile(path, b, 0o600)
	}
	return sshrun.IsWindowsServer(sw)
}
