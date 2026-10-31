package sshrun

import (
	"io"
	"strings"
	"testing"

	"meterm/internal/config"
)

func TestCopyIDCommandPipesKeyOnStdinNotArgv(t *testing.T) {
	r := config.Resolved{Address: "10.0.0.1", User: "deploy", Port: 2222}
	c := CopyIDCommand(r, "ssh-ed25519 AAAAtest comment")

	joined := strings.Join(c.Args[1:], " ")
	if strings.Contains(joined, "AAAAtest") {
		t.Errorf("the public key must not appear in argv (it should be piped via stdin): %q", joined)
	}
	if !strings.Contains(joined, "-p 2222") {
		t.Errorf("missing -p 2222: %q", joined)
	}
	if !strings.HasPrefix(joined, "-p 2222 deploy@10.0.0.1") && !strings.Contains(joined, "deploy@10.0.0.1") {
		t.Errorf("missing target deploy@10.0.0.1: %q", joined)
	}

	got, err := io.ReadAll(c.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "ssh-ed25519 AAAAtest comment" {
		t.Errorf("stdin = %q, want the public key line", got)
	}
}

func TestCopyIDCommandScriptIsIdempotent(t *testing.T) {
	r := config.Resolved{Address: "10.0.0.1"}
	c := CopyIDCommand(r, "ssh-ed25519 AAAAtest")
	joined := strings.Join(c.Args, " ")
	if !strings.Contains(joined, "grep -qxF") {
		t.Errorf("expected the remote script to dedupe via grep before appending, got: %q", joined)
	}
}
