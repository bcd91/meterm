package sshrun

import (
	"os"
	"os/exec"
	"strings"

	"meterm/internal/config"
)

// installKeyScript is run on the remote host with the public key piped in
// on stdin. It's idempotent (won't duplicate an already-installed key) and
// run via `sh -c` explicitly so it doesn't depend on the remote account's
// login shell being POSIX-ish.
const installKeyScript = `sh -c 'umask 077; mkdir -p ~/.ssh; touch ~/.ssh/authorized_keys; chmod 600 ~/.ssh/authorized_keys; key=$(cat); grep -qxF "$key" ~/.ssh/authorized_keys || echo "$key" >> ~/.ssh/authorized_keys'`

// CopyIDCommand installs pubKey on r's authorized_keys, the same way
// ssh-copy-id does (and reusing that same trick: ssh reads a password
// prompt from /dev/tty, not from stdin, so piping the key through stdin
// here doesn't stop you from being prompted for the account's password).
func CopyIDCommand(r config.Resolved, pubKey string) *exec.Cmd {
	args := commonArgs(r, "-p")
	target := r.Address
	if r.User != "" {
		target = r.User + "@" + r.Address
	}
	args = append(args, "--", target, installKeyScript)

	cmd := guarded(r, "ssh", args...)
	cmd.Stdin = strings.NewReader(strings.TrimSpace(pubKey) + "\n")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}
