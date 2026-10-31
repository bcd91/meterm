package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"meterm/internal/config"
	"meterm/internal/route"
)

func TestMarkerFilterHidesMarkersButNotPrompts(t *testing.T) {
	var out bytes.Buffer
	f := &markerFilter{w: &out}
	// Arrives in odd pieces, like a pty does; the prompt has no newline.
	for _, chunk := range []string{"[sudo] pass", "word for dev on GW1: ", "\r\n", "METERM-PUB", "KEY: ssh-ed25519 AAAA\r\n", "hello\r\nMETERM-OK\r\n"} {
		f.Write([]byte(chunk))
	}
	if got := out.String(); got != "[sudo] password for dev on GW1: \r\nhello\r\n" {
		t.Errorf("shown: %q", got)
	}
}

func TestRelayEndpointsPreferRoutesAndReuseTrust(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	kh := filepath.Join(t.TempDir(), "known_hosts")
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJA+SfeNPN58Loh3y+m/lx3aP6w3hBSEitSCSHBrRLOk"
	if err := os.WriteFile(kh, []byte("100.64.0.1 "+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := route.KnownHosts
	route.KnownHosts = func() string { return kh }
	defer func() { route.KnownHosts = old }()

	cfg := &config.Config{Hosts: []config.Host{{Name: "devbox", Address: "100.64.0.1", Routes: []config.Route{
		{Name: "public", Address: "203.0.113.50", Port: 5000},
		{Name: "via-x", ProxyJump: "x"}, // needs a jump: no use to a tunnel
	}}}}
	addrs, known, err := relayEndpoints(cfg, cfg.Hosts[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(addrs, " ") != "203.0.113.50:5000 100.64.0.1:22" {
		t.Errorf("addrs = %v", addrs)
	}
	if strings.Join(known, "\n") != "[203.0.113.50]:5000 "+key+"\n100.64.0.1 "+key {
		t.Errorf("known = %v", known)
	}

	// Never connected to the relay: refuse rather than trust blindly.
	if err := os.WriteFile(kh, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := relayEndpoints(cfg, cfg.Hosts[0]); err == nil || !strings.Contains(err.Error(), "host key") {
		t.Errorf("err = %v", err)
	}
}

func TestSetAndRemoveRoute(t *testing.T) {
	h := &config.Host{Routes: []config.Route{{Name: "public"}}}
	setRoute(h, config.Route{Name: "relay-x", Port: 1})
	setRoute(h, config.Route{Name: "relay-x", Port: 2})
	if len(h.Routes) != 2 || h.Routes[1].Port != 2 {
		t.Fatalf("routes = %+v", h.Routes)
	}
	removeRoute(h, "relay-x")
	if len(h.Routes) != 1 || h.Routes[0].Name != "public" {
		t.Errorf("routes = %+v", h.Routes)
	}
}

func TestInviteHelpers(t *testing.T) {
	if !subsetOf(nil, []string{"a"}) || !subsetOf([]string{"a"}, []string{"a", "b"}) || subsetOf([]string{"c"}, []string{"a"}) {
		t.Error("subsetOf")
	}
	if got := missing([]string{"a", "b"}, []string{"a"}); len(got) != 1 || got[0] != "b" {
		t.Errorf("missing = %v", got)
	}
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "hosts.yaml")
	defer func() { cfgPath = "" }()
	m := map[string]pendingInvite{"NEWBOX": {Relay: "R", Port: 22100}}
	if err := saveInvites(m); err != nil {
		t.Fatal(err)
	}
	got, err := loadInvites()
	if err != nil || got["NEWBOX"].Port != 22100 {
		t.Errorf("invites = %+v, %v", got, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "relay-invites.json")); st.Mode().Perm() != 0o600 && runtime.GOOS != "windows" {
		t.Errorf("mode %v", st.Mode().Perm())
	}
}

func TestMergeHostKeys(t *testing.T) {
	ed, ec, rsa := "ssh-ed25519 AAAAed", "ecdsa-sha2-nistp256 AAAAec", "ssh-rsa AAAArsa"
	// One address knows three types, another only one (UpdateHostKeys): fine.
	got, err := mergeHostKeys([]string{ed, ec, rsa}, []string{ed})
	if err != nil || len(got) != 3 {
		t.Errorf("superset: %v, %v", got, err)
	}
	got, err = mergeHostKeys([]string{ed}, []string{ed, ec})
	if err != nil || len(got) != 2 {
		t.Errorf("growing: %v, %v", got, err)
	}
	// Same type, different key: two machines.
	if _, err := mergeHostKeys([]string{ed}, []string{"ssh-ed25519 AAAAother"}); err == nil {
		t.Error("conflicting ed25519 keys accepted")
	}
}
