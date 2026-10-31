package inventory

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"meterm/internal/config"
)

const tsJSON = `{"Self":{"HostName":"me"},"Peer":{
 "a":{"HostName":"devserver","DNSName":"devserver.tail1.ts.net.","TailscaleIPs":["100.64.0.2","fd7a::1"],"OS":"linux","Online":true,"Tags":["tag:server"]},
 "b":{"HostName":"WIN-X","DNSName":"win-db.tail1.ts.net.","TailscaleIPs":["100.64.0.3"],"OS":"windows","Online":true},
 "c":{"HostName":"localhost","DNSName":"ipad.tail1.ts.net.","TailscaleIPs":["100.64.0.4"],"OS":"iOS","Online":false},
 "d":{"HostName":"old","DNSName":"old.tail1.ts.net.","TailscaleIPs":["100.64.0.7"],"OS":"linux","Online":false}
}}`

func byName(hs []config.Host) map[string]config.Host {
	m := map[string]config.Host{}
	for _, h := range hs {
		m[h.Name] = h
	}
	return m
}

func TestFromTailscale(t *testing.T) {
	hs, err := FromTailscale([]byte(tsJSON), TailscaleOptions{User: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	got := byName(hs)
	if len(got) != 3 || got["ipad"].Name != "" {
		t.Fatalf("got %v, want devserver, win-db, old (no iPad)", hs)
	}
	d := got["devserver"]
	if d.Address != "100.64.0.2" || d.User != "dev" || strings.Join(d.Tags, ",") != "tailscale,server,os:linux" {
		t.Errorf("devserver = %+v", d)
	}
	if got["win-db"].Address != "100.64.0.3" {
		t.Errorf("named after the machine name, not the OS hostname: %+v", got["win-db"])
	}
	hs, _ = FromTailscale([]byte(tsJSON), TailscaleOptions{Tags: []string{"tag:server"}})
	if len(hs) != 1 || hs[0].Name != "devserver" {
		t.Errorf("--tag server = %v", hs)
	}
	hs, _ = FromTailscale([]byte(tsJSON), TailscaleOptions{OnlineOnly: true})
	if len(hs) != 2 {
		t.Errorf("--online = %v", hs)
	}
}

const iniInv = `
loose ansible_host=10.0.0.9

[web]
web1 ansible_host=10.0.0.1
web2 ansible_host=10.0.0.2 ansible_user=root ansible_port=2222
web[03:10]

[db]
db1 ansible_host=10.0.1.1 ansible_ssh_common_args='-o StrictHostKeyChecking=no -J ops@bastion.example'

[prod:children]
web
db

[prod:vars]
ansible_user=deploy

[web:vars]
ansible_user=www

[all:vars]
ansible_user=nobody
ansible_ssh_private_key_file=~/.ssh/prod
`

func TestFromAnsibleINI(t *testing.T) {
	hs, skipped, err := FromAnsible([]byte(iniInv))
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != "web[03:10]" {
		t.Errorf("skipped = %v", skipped)
	}
	got := byName(hs)
	for name, want := range map[string]struct {
		addr, user string
		port       int
		tags, jump string
	}{
		"loose": {"10.0.0.9", "nobody", 0, "", ""},
		"web1":  {"10.0.0.1", "www", 0, "prod,web", ""}, // web:vars beats prod:vars beats all:vars
		"web2":  {"10.0.0.2", "root", 2222, "prod,web", ""},
		"db1":   {"10.0.1.1", "deploy", 0, "db,prod", "ops@bastion.example"},
	} {
		h := got[name]
		if h.Address != want.addr || h.User != want.user || h.Port != want.port || strings.Join(h.Tags, ",") != want.tags || h.ProxyJump != want.jump {
			t.Errorf("%s = %+v, want %+v", name, h, want)
		}
		if h.IdentityFile != "~/.ssh/prod" {
			t.Errorf("%s identity = %q", name, h.IdentityFile)
		}
	}
}

const yamlInv = `
all:
  vars:
    ansible_user: nobody
  hosts:
    jump:
      ansible_host: 203.0.113.1
  children:
    prod:
      vars:
        ansible_user: deploy
      children:
        web:
          hosts:
            web1:
              ansible_host: 10.0.0.1
              ansible_port: 2200
            web2:
`

func TestFromAnsibleYAML(t *testing.T) {
	hs, _, err := FromAnsible([]byte(yamlInv))
	if err != nil {
		t.Fatal(err)
	}
	got := byName(hs)
	if h := got["web1"]; h.Address != "10.0.0.1" || h.Port != 2200 || h.User != "deploy" || strings.Join(h.Tags, ",") != "prod,web" {
		t.Errorf("web1 = %+v", h)
	}
	if h := got["web2"]; h.Address != "web2" || h.User != "deploy" {
		t.Errorf("web2 = %+v (no ansible_host: the name is the address)", h)
	}
	if h := got["jump"]; h.User != "nobody" || len(h.Tags) != 0 {
		t.Errorf("jump = %+v", h)
	}
}

func TestToAnsibleRoundTrips(t *testing.T) {
	cfg := &config.Config{
		Groups: map[string]config.Group{"prod": {User: "deploy", ProxyJump: "bastion"}},
		Hosts: []config.Host{
			{Name: "bastion", Address: "203.0.113.1", User: "ops"},
			{Name: "web1", Address: "10.0.0.1", Group: "prod", Port: 2200, Tags: []string{"exposes:web"}},
		},
	}
	out, err := ToAnsible(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("not YAML: %v\n%s", err, out)
	}
	hs, _, err := FromAnsible(out)
	if err != nil {
		t.Fatal(err)
	}
	w := byName(hs)["web1"]
	if w.Address != "10.0.0.1" || w.User != "deploy" || w.Port != 2200 || w.ProxyJump != "ops@203.0.113.1" {
		t.Errorf("web1 after round trip = %+v\n%s", w, out)
	}
	if strings.Join(w.Tags, ",") != "exposes_web,prod" {
		t.Errorf("web1 tags = %v", w.Tags)
	}
}
