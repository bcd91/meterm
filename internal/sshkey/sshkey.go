// Package sshkey finds and generates local SSH key pairs, backing
// `meterm keygen` and the default identity `meterm copy-id` installs.
package sshkey

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// candidateNames lists default private key basenames, in the order ssh
// itself tries them.
var candidateNames = []string{"id_ed25519", "id_ecdsa", "id_rsa"}

// ResolvePublicKey finds which public key to install for copy-id:
// identityFlag (an explicit --identity, naming a private key; ".pub" is
// appended) wins, then the host's already-configured identity, then the
// first of ~/.ssh/id_ed25519, id_ecdsa, id_rsa that has a matching .pub
// file on disk.
func ResolvePublicKey(identityFlag, configuredIdentity string) (string, error) {
	if identityFlag != "" {
		return pubPathFor(identityFlag)
	}
	if configuredIdentity != "" {
		return pubPathFor(configuredIdentity)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	var tried []string
	for _, name := range candidateNames {
		pub := filepath.Join(home, ".ssh", name+".pub")
		if _, err := os.Stat(pub); err == nil {
			return pub, nil
		}
		tried = append(tried, pub)
	}
	return "", fmt.Errorf("no public key found (looked for %s) — run `meterm keygen` first, or pass --identity", strings.Join(tried, ", "))
}

func pubPathFor(privateKeyPath string) (string, error) {
	p := privateKeyPath
	if !strings.HasSuffix(p, ".pub") {
		p += ".pub"
	}
	p = LocalPath(p)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("public key not found at %s: %w", p, err)
	}
	return p, nil
}

// ExpandHome resolves a leading "~/" (or "~\" on Windows) without relying
// on shell expansion — needed because meterm's TUI dispatches commands via
// exec.Command directly (no shell in between), so a path typed in the
// command palette never gets tilde-expanded for us.
func ExpandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

// LocalPath resolves a key path from the hosts file for this machine. On top
// of ExpandHome it copes with a hosts file copied from another OS: an
// absolute path like /Users/alice/.ssh/id_ed25519 that doesn't exist here is
// looked up under this machine's own ~/.ssh instead (C:\Users\alice\.ssh on
// Windows). If neither exists, the expanded original comes back so errors
// still name the configured path.
func LocalPath(path string) string {
	p := ExpandHome(path)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	norm := strings.ReplaceAll(p, `\`, "/")
	i := strings.LastIndex(norm, "/.ssh/")
	if i < 0 {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	alt := filepath.Join(home, ".ssh", filepath.FromSlash(norm[i+len("/.ssh/"):]))
	if _, err := os.Stat(alt); err == nil {
		return alt
	}
	return p
}

// ContractHome rewrites a path under the home directory as "~/...", so a
// hosts file saved on one machine still points at the right key on another
// (/Users/alice on a Mac is C:\Users\alice on Windows). Paths outside home and
// relative paths are returned unchanged.
func ContractHome(path string) string {
	if path == "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(home, ExpandHome(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return "~/" + filepath.ToSlash(rel)
}

// GenerateCommand builds `ssh-keygen -t ed25519 -f path [-C comment]`,
// wired to the current terminal so the passphrase prompt works normally.
func GenerateCommand(path, comment string) *exec.Cmd {
	args := []string{"-t", "ed25519", "-f", path}
	if comment != "" {
		args = append(args, "-C", comment)
	}
	cmd := exec.Command("ssh-keygen", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}
