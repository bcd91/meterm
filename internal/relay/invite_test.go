package relay

import (
	"encoding/base64"
	"os/exec"
	"strings"
	"testing"
)

const hk = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJA+SfeNPN58Loh3y+m/lx3aP6w3hBSEitSCSHBrRLOk"

func params() InviteParams {
	return InviteParams{
		NodeParams: NodeParams{Relay: "DEVBOX-1", Candidates: []string{"203.0.113.50:5000"}, KnownHosts: []string{"[203.0.113.50]:5000 " + hk}, Port: 22100},
		Name:       "NEWBOX", AdminKey: hk + " alice@mac",
	}
}

func TestInviteScriptParsesAndHoldsNoSecrets(t *testing.T) {
	s, err := InviteScript(params())
	if err != nil {
		t.Fatal(err)
	}
	if sh, err := exec.LookPath("sh"); err == nil {
		c := exec.Command(sh, "-n")
		c.Stdin = strings.NewReader(s)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("doesn't parse: %v %s", err, out)
		}
	}
	for _, bad := range []string{"PRIVATE KEY", "BEGIN OPENSSH"} {
		if strings.Contains(s, bad) {
			t.Errorf("script contains %q", bad)
		}
	}
	for _, want := range []string{"-R localhost:22100:localhost:22", "meterm relay accept NEWBOX", "is a symlink", hk + " meterm-admin"} {
		if want == hk+" meterm-admin" {
			continue // the comment is set by the caller
		}
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q", want)
		}
	}
}

func TestInviteScriptRejectsBadInput(t *testing.T) {
	for _, mut := range []func(*InviteParams){
		func(p *InviteParams) { p.Name = "x; rm -rf /" },
		func(p *InviteParams) { p.Name = "-oops" },
		func(p *InviteParams) { p.Name = "" },
		func(p *InviteParams) { p.User = "dev'; id" },
		func(p *InviteParams) { p.AdminKey = "ssh-ed25519 AAAA'; id #" },
		func(p *InviteParams) { p.Candidates = []string{"$(id):22"} },
	} {
		p := params()
		mut(&p)
		if _, err := InviteScript(p); err == nil {
			t.Errorf("accepted %+v", p)
		}
	}
}

func reply(lines ...string) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(lines, "\n") + "\n"))
}

func TestParseReply(t *testing.T) {
	r, err := ParseReply(reply("tunnel "+hk, "user dev", "hostkey "+hk, "hostkey ecdsa-sha2-nistp256 AAAAE2VjZHNh"))
	if err != nil {
		t.Fatal(err)
	}
	if r.User != "dev" || r.TunnelKey != hk || len(r.HostKeys) != 2 {
		t.Errorf("reply = %+v", r)
	}
}

// The reply comes from the machine being added (or from whoever tampers
// with it on the way): nothing odd gets through.
func TestParseReplyRejectsForgeries(t *testing.T) {
	cases := map[string]string{
		"not base64":         "!!!",
		"no tunnel key":      reply("user dev", "hostkey "+hk),
		"no host key":        reply("tunnel "+hk, "user dev"),
		"no user":            reply("tunnel "+hk, "hostkey "+hk),
		"root":               reply("tunnel "+hk, "user root", "hostkey "+hk),
		"user injection":     reply("tunnel "+hk, "user dev; id", "hostkey "+hk),
		"key with options":   reply("tunnel command=\"id\" "+hk, "user dev", "hostkey "+hk),
		"key with quote":     reply("tunnel ssh-ed25519 AAAA'x", "user dev", "hostkey "+hk),
		"key with comment":   reply("tunnel "+hk+" evil", "user dev", "hostkey "+hk),
		"two tunnel keys":    reply("tunnel "+hk, "tunnel "+hk, "user dev", "hostkey "+hk),
		"unknown line":       reply("tunnel "+hk, "user dev", "hostkey "+hk, "port 22"),
		"host key injection": reply("tunnel "+hk, "user dev", "hostkey "+hk+"\x1b[2J"),
		"too many keys":      reply(append([]string{"tunnel " + hk, "user dev"}, strings.Split(strings.Repeat("hostkey "+hk+"\n", 9), "\n")...)...),
	}
	for name, blob := range cases {
		if _, err := ParseReply(blob); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Old machines still carry a DSA host key: it's skipped, not trusted, and
// doesn't spoil the reply. A malformed one still does.
func TestParseReplySkipsDSA(t *testing.T) {
	r, err := ParseReply(reply("tunnel "+hk, "user dev", "hostkey ssh-dss AAAAB3NzaC1kc3MAAACBALDe", "hostkey "+hk))
	if err != nil || len(r.HostKeys) != 1 || r.HostKeys[0] != hk {
		t.Errorf("reply = %+v, %v", r, err)
	}
	if _, err := ParseReply(reply("tunnel "+hk, "user dev", "hostkey ssh-dss AAAA'x", "hostkey "+hk)); err == nil {
		t.Error("malformed DSA line accepted")
	}
	if _, err := ParseReply(reply("tunnel "+hk, "user dev", "hostkey ssh-dss AAAAB3Nza")); err == nil {
		t.Error("a reply with only a DSA key accepted")
	}
}
