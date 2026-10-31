// Package inventory brings hosts in from, and out to, the inventories a team
// already keeps: a Tailscale tailnet and Ansible.
package inventory

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"

	"meterm/internal/config"
)

// TailscaleOptions narrows which tailnet devices become hosts.
type TailscaleOptions struct {
	User       string   // login user for every imported host
	Tags       []string // keep only devices with one of these ACL tags ("tag:server" or "server")
	OnlineOnly bool
	AllOS      bool // include phones and tablets, which don't run sshd
}

type tsStatus struct {
	Self *tsPeer
	Peer map[string]*tsPeer
}

type tsPeer struct {
	HostName     string
	DNSName      string
	TailscaleIPs []string
	OS           string
	Online       bool
	Tags         []string
}

// TailscaleStatus runs `tailscale status --json` with the local CLI, so it
// sees what this machine's tailnet login sees and needs no API key.
func TailscaleStatus() ([]byte, error) {
	bin, err := exec.LookPath("tailscale")
	if err != nil && runtime.GOOS == "darwin" {
		const app = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
		if _, statErr := os.Stat(app); statErr == nil {
			bin, err = app, nil
		}
	}
	if err != nil {
		return nil, errors.New("the tailscale CLI isn't installed or isn't on PATH")
	}
	out, err := exec.Command(bin, "status", "--json").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, errors.New("tailscale status: " + strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

// FromTailscale turns `tailscale status --json` into hosts: named after the
// device's machine name in the tailnet, addressed by its stable 100.x IP,
// tagged "tailscale" plus its ACL tags and OS.
func FromTailscale(statusJSON []byte, o TailscaleOptions) ([]config.Host, error) {
	var st tsStatus
	if err := json.Unmarshal(statusJSON, &st); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, t := range o.Tags {
		want[strings.TrimPrefix(t, "tag:")] = true
	}
	var out []config.Host
	for _, p := range st.Peer {
		if p == nil || (o.OnlineOnly && !p.Online) {
			continue
		}
		osName := strings.ToLower(p.OS)
		if !o.AllOS && (osName == "ios" || osName == "android") {
			continue
		}
		var tags []string
		match := len(want) == 0
		for _, t := range p.Tags {
			t = strings.TrimPrefix(t, "tag:")
			tags = append(tags, t)
			match = match || want[t]
		}
		if !match {
			continue
		}
		addr := ""
		for _, ip := range p.TailscaleIPs {
			if !strings.Contains(ip, ":") {
				addr = ip
				break
			}
		}
		name := strings.SplitN(strings.TrimSuffix(p.DNSName, "."), ".", 2)[0]
		if name == "" {
			name = p.HostName
		}
		if addr == "" || name == "" {
			continue
		}
		h := config.Host{Name: name, Address: addr, User: o.User, Tags: append([]string{"tailscale"}, tags...)}
		if osName != "" {
			h.Tags = append(h.Tags, "os:"+osName)
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
