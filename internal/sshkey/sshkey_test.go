package sshkey

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePublicKeyExplicitIdentity(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "mykey")
	writeFile(t, priv+".pub", "ssh-ed25519 AAAA test")

	got, err := ResolvePublicKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != priv+".pub" {
		t.Errorf("got %q, want %q", got, priv+".pub")
	}
}

func TestResolvePublicKeyExplicitAlreadyDotPub(t *testing.T) {
	dir := t.TempDir()
	pub := filepath.Join(dir, "mykey.pub")
	writeFile(t, pub, "ssh-ed25519 AAAA test")

	got, err := ResolvePublicKey(pub, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != pub {
		t.Errorf("got %q, want %q (should not double-append .pub)", got, pub)
	}
}

func TestResolvePublicKeyMissingExplicitErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := ResolvePublicKey(filepath.Join(dir, "nope"), ""); err == nil {
		t.Error("expected an error for a missing explicit identity")
	}
}

func TestResolvePublicKeyFallsBackToConfiguredIdentity(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "hostkey")
	writeFile(t, priv+".pub", "ssh-ed25519 AAAA test")

	got, err := ResolvePublicKey("", priv)
	if err != nil {
		t.Fatal(err)
	}
	if got != priv+".pub" {
		t.Errorf("got %q, want %q", got, priv+".pub")
	}
}

func TestResolvePublicKeyNoneFoundErrorsWithGuidance(t *testing.T) {
	setHome(t, t.TempDir()) // an empty fake home with no ~/.ssh at all
	_, err := ResolvePublicKey("", "")
	if err == nil {
		t.Fatal("expected an error when no key exists anywhere")
	}
	if got := err.Error(); !strings.Contains(got, "meterm keygen") {
		t.Errorf("error should point at `meterm keygen`, got: %s", got)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir available")
	}
	got := ExpandHome("~/.ssh/id_ed25519")
	want := filepath.Join(home, ".ssh", "id_ed25519")
	if got != want {
		t.Errorf("ExpandHome = %q, want %q", got, want)
	}
	if ExpandHome("/already/absolute") != "/already/absolute" {
		t.Error("ExpandHome should leave non-~ paths untouched")
	}
}

func TestLocalPathRemapsAnotherMachinesHome(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".ssh", "id_ed25519.pub")
	writeFile(t, want, "ssh-ed25519 AAAA test")

	// A hosts file copied from a Mac (or from Windows) names that machine's home.
	for _, foreign := range []string{"/Users/someone-else/.ssh/id_ed25519.pub", `C:\Users\alice\.ssh\id_ed25519.pub`} {
		if got := LocalPath(foreign); got != want {
			t.Errorf("LocalPath(%q) = %q, want %q", foreign, got, want)
		}
	}

	// copy-id goes through the same lookup when the host's identity is foreign.
	got, err := ResolvePublicKey("", "/Users/someone-else/.ssh/id_ed25519")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("ResolvePublicKey = %q, want %q", got, want)
	}
}

func TestLocalPathKeepsMissingPathForErrors(t *testing.T) {
	setHome(t, t.TempDir())
	const p = "/Users/someone-else/.ssh/nope"
	if got := LocalPath(p); got != p {
		t.Errorf("LocalPath(%q) = %q, want it unchanged when nothing matches", p, got)
	}
}

func TestContractHome(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	cases := map[string]string{
		filepath.Join(home, ".ssh", "id_ed25519"): "~/.ssh/id_ed25519",
		"~/.ssh/id_rsa":       "~/.ssh/id_rsa",
		"/etc/ssh/deploy_key": "/etc/ssh/deploy_key",
		"keys/relative":       "keys/relative",
		"":                    "",
	}
	for in, want := range cases {
		if got := ContractHome(in); got != want {
			t.Errorf("ContractHome(%q) = %q, want %q", in, got, want)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setHome points os.UserHomeDir at dir: $HOME on Unix, %USERPROFILE% on Windows.
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}
