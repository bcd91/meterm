package sshrun

import (
	"os"
	"os/exec"
	"strings"

	"meterm/internal/config"
)

// ScpCommand builds a copy between localPath and remotePath on r. When
// upload is true, localPath is the source and remotePath the destination.
func ScpCommand(r config.Resolved, remotePath, localPath string, upload, recursive bool) *exec.Cmd {
	args := commonArgs(r, "-P")
	if recursive {
		args = append(args, "-r")
	}
	remote := remoteSpec(r, remotePath)
	args = append(args, "--")
	if upload {
		args = append(args, localPath, remote)
	} else {
		args = append(args, remote, localPath)
	}
	return inherited(guarded(r, "scp", args...))
}

// SftpCommand opens an interactive sftp session against r, optionally
// starting in startDir on the remote side.
func SftpCommand(r config.Resolved, startDir string) *exec.Cmd {
	args := commonArgs(r, "-P")
	target := bracketed(r.Address)
	if r.User != "" {
		target = r.User + "@" + target
	}
	if startDir != "" {
		target += ":" + startDir
	}
	args = append(args, "--", target)
	c := inherited(guarded(r, "sftp", args...))
	c.Env = withRemoteTerm(os.Environ())
	return c
}

// RsyncCommand builds an rsync-over-ssh transfer between localPath and
// remotePath on r, using rsyncFlags (e.g. "-avz", "--progress").
func RsyncCommand(r config.Resolved, remotePath, localPath string, upload bool, rsyncFlags []string) *exec.Cmd {
	sshParts := append([]string{"ssh"}, commonArgs(r, "-p")...)
	quoted := make([]string, len(sshParts))
	for i, p := range sshParts {
		quoted[i] = shellQuote(p)
	}

	args := append([]string{"-e", strings.Join(quoted, " ")}, rsyncFlags...)
	remote := remoteSpec(r, remotePath)
	args = append(args, "--")
	if upload {
		args = append(args, localPath, remote)
	} else {
		args = append(args, remote, localPath)
	}
	return inherited(guarded(r, "rsync", args...))
}

func inherited(cmd *exec.Cmd) *exec.Cmd {
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

// shellQuote makes s safe to embed in the single command string rsync's -e
// flag expects, so identity paths or jump specs with special characters
// survive being re-parsed by a shell.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// SubsystemCommand starts an ssh subsystem (e.g. "sftp") on r with stdio left
// for the caller to wire up as pipes. Port forwards are left out on purpose:
// a file session shouldn't fail because a forwarded port is already taken.
func SubsystemCommand(r config.Resolved, subsystem string) *exec.Cmd {
	args := commonArgs(r, "-p")
	args = append(args, r.ExtraArgs...)
	target := r.Address
	if r.User != "" {
		target = r.User + "@" + target
	}
	args = append(args, "-s", "--", target, subsystem)
	return guarded(r, "ssh", args...)
}
