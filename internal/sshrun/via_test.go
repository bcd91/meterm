package sshrun

import (
	"encoding/base64"
	"strings"
	"testing"

	"meterm/internal/config"
)

func TestCopyIDViaCommand(t *testing.T) {
	r := config.Resolved{Name: "DEVBOX", Address: "10.0.0.1", User: "dev"}
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample alice@mac"
	c, err := CopyIDViaCommand(r, "bs", key, false, true)
	if err != nil {
		t.Fatal(err)
	}
	remote := c.Args[len(c.Args)-1]
	if c.Args[len(c.Args)-2] != "-t" {
		t.Errorf("no -t for the sudo prompt: %q", c.Args)
	}
	const pre, post = "sh -c 'printf %s ", " | base64 -d | sh'"
	if !strings.HasPrefix(remote, pre) || !strings.HasSuffix(remote, post) {
		t.Fatalf("remote = %q", remote)
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(remote, pre), post))
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	for _, want := range []string{"T='bs'", "YES='ask'", base64.StdEncoding.EncodeToString([]byte(key)), `sudo -H -u "$T"`, "sshd -t", "meterm-bak-", "</dev/tty"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if strings.Contains(script, "alice@mac") || strings.Contains(script, "%!") {
		t.Error("key in clear, or a format error, in the script")
	}
	if c, _ := CopyIDViaCommand(r, "bs", key, true, false); strings.Contains(strings.Join(c.Args, " "), " -t ") {
		t.Error("-t without a terminal")
	}
}

func TestCopyIDViaRefuses(t *testing.T) {
	r := config.Resolved{Address: "h", User: "dev"}
	key := "ssh-ed25519 AAAAC3Nz x"
	for _, target := range []string{"", "root", "dev", "-oProxyCommand=x", "a'b", "a b", "$(id)"} {
		if _, err := CopyIDViaCommand(r, target, key, false, false); err == nil {
			t.Errorf("target %q accepted", target)
		}
	}
	for _, k := range []string{"", "nope", "ssh-ed25519 AAAA\nssh-rsa BBBB", "ssh-ed25519 AAAA' ; reboot ; '"} {
		if _, err := CopyIDViaCommand(r, "bs", k, false, false); err == nil {
			t.Errorf("key %q accepted", k)
		}
	}
}
