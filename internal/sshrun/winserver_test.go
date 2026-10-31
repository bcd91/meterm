package sshrun

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"unicode/utf16"

	"meterm/internal/config"
)

func decodePS(t *testing.T, b64 string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(b)%2 != 0 {
		t.Fatalf("not base64 UTF-16: %v", err)
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

func TestCopyIDCommandWindows(t *testing.T) {
	r := config.Resolved{Name: "DESK", Address: "100.64.0.6", User: "dev", Port: 2222}
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample dev@NB"
	c, err := CopyIDCommandWindows(r, key+"\n")
	if err != nil {
		t.Fatal(err)
	}
	n := len(c.Args)
	if c.Args[n-3] != "--" || c.Args[n-2] != "dev@100.64.0.6" {
		t.Fatalf("args = %q", c.Args)
	}
	remote := c.Args[n-1]
	const prefix = "powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand "
	if !strings.HasPrefix(remote, prefix) {
		t.Fatalf("remote command = %q", remote)
	}
	enc := strings.TrimPrefix(remote, prefix)
	if strings.ContainsAny(enc, " \"'&|<>^%") {
		t.Errorf("encoded command has characters cmd.exe would interpret: %q", enc)
	}
	script := decodePS(t, enc)
	if strings.Contains(script, key) {
		t.Error("the key is in the script as text; it must only be there base64-encoded")
	}
	if !strings.Contains(script, base64.StdEncoding.EncodeToString([]byte(key))) {
		t.Error("key not embedded")
	}
	for _, want := range []string{"administrators_authorized_keys", "S-1-5-32-544", "/inheritance:r", "UTF8Encoding $false", "`r`n", "-contains $k"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if len(remote) > 8000 {
		t.Errorf("command is %d chars; cmd.exe's limit is 8191", len(remote))
	}
	if os.Getenv("SHOW_PS") != "" {
		t.Log(script)
	}
}

func TestCopyIDCommandWindowsRefusesNonKeys(t *testing.T) {
	r := config.Resolved{Address: "h"}
	for _, bad := range []string{
		"", "not a key",
		"ssh-ed25519 AAAA\nssh-rsa BBBB",
		"ssh-ed25519 AAAA'); Remove-Item C:\\ -Recurse; ('",
		"ssh-ed25519 AAAA comment\x1b[2J",
	} {
		if _, err := CopyIDCommandWindows(r, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestIsWindowsServer(t *testing.T) {
	if !IsWindowsServer("OpenSSH_for_Windows_10.0") || IsWindowsServer("OpenSSH_9.6p1") || IsWindowsServer("") {
		t.Error("wrong")
	}
}
