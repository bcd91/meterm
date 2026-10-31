package sshrun

import (
	"strings"
	"testing"

	"meterm/internal/config"
)

func TestScpCommandDownload(t *testing.T) {
	r := config.Resolved{Address: "10.0.0.1", User: "deploy", Port: 2222, IdentityFile: "~/.ssh/key"}
	c := ScpCommand(r, "/etc/nginx.conf", "./nginx.conf", false, false)
	joined := strings.Join(c.Args[1:], " ")
	if !strings.Contains(joined, "-P 2222") {
		t.Errorf("missing -P 2222: %q", joined)
	}
	if !strings.Contains(joined, "deploy@10.0.0.1:/etc/nginx.conf ./nginx.conf") {
		t.Errorf("expected remote source before local dest, got %q", joined)
	}
}

func TestScpCommandUploadRecursive(t *testing.T) {
	r := config.Resolved{Address: "10.0.0.1"}
	c := ScpCommand(r, "/tmp/dst", "./localdir", true, true)
	joined := strings.Join(c.Args[1:], " ")
	if !strings.Contains(joined, "-r") {
		t.Errorf("expected -r flag, got %q", joined)
	}
	if !strings.HasSuffix(joined, "./localdir 10.0.0.1:/tmp/dst") {
		t.Errorf("expected local source before remote dest, got %q", joined)
	}
}

func TestSftpCommandWithStartDir(t *testing.T) {
	r := config.Resolved{Address: "10.0.0.1", User: "u", Port: 22}
	c := SftpCommand(r, "/var/www")
	joined := strings.Join(c.Args[1:], " ")
	if !strings.HasSuffix(joined, "u@10.0.0.1:/var/www") {
		t.Errorf("expected target with start dir, got %q", joined)
	}
}

func TestRsyncCommandQuotesIdentityPath(t *testing.T) {
	r := config.Resolved{Address: "10.0.0.1", User: "u", IdentityFile: "/path with spaces/key"}
	c := RsyncCommand(r, "/remote/dir/", "./local/", false, []string{"-avz"})
	joined := strings.Join(c.Args[1:], " ")
	if !strings.Contains(joined, `-i '/path with spaces/key'`) {
		t.Errorf("expected quoted identity path in -e string, got %q", joined)
	}
	if !strings.Contains(joined, "u@10.0.0.1:/remote/dir/ ./local/") {
		t.Errorf("expected remote source before local dest, got %q", joined)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":     "plain",
		"has space": "'has space'",
		"":          "''",
		"o'clock":   `'o'\''clock'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
