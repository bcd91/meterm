package main

import (
	"errors"
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"

	"meterm/internal/history"
	"meterm/internal/sshrun"
)

func newSftpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:        "sftp <name> [remote-dir]",
		Short:      "Open an interactive SFTP session (browse, get, put, mget, mput)",
		Args:       cobra.RangeArgs(1, 2),
		Hidden:     true,
		Deprecated: "use `meterm files <name>` (or `meterm files --shell <name>` for the sftp prompt)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSftpShell(args, "")
		},
	}
	return cmd
}

// runSftpShell opens OpenSSH's interactive sftp prompt on a saved host.
// note is how a production question before it went (files --shell); ""
// leaves it to namedNote.
func runSftpShell(args []string, note string) error {
	quietOnSuccess = true
	code, err := sftpShellNoted(args, note)
	if err != nil {
		return err
	}
	if code != 0 {
		finish(code)
	}
	return nil
}

// sftpShell runs the sftp prompt and returns its exit code. The activity
// log gets a start before sftp runs and an outcome after; last-connected
// is written only when sftp exits 0, since it reports a connection it
// couldn't make as a plain failure.
func sftpShell(args []string) (int, error) { return sftpShellNoted(args, "") }

func sftpShellNoted(args []string, note string) (int, error) {
	cfg, err := loadConfig()
	if err != nil {
		return 0, err
	}
	h, idx, err := cfg.Target(args[0])
	if err != nil {
		return 0, err
	}
	if idx < 0 {
		return 0, fmt.Errorf("no such host: %s (use `meterm list` to see saved hosts)", args[0])
	}
	startDir := ""
	if len(args) == 2 {
		startDir = args[1]
	}
	if note == "" {
		note = namedNote(cfg, *h, false)
	}
	run := beginActivity(cfg, *h, "sftp", noteDetail(note, startDir))
	kickAutosync(false, true)
	started, runErr := runOutliving(sshrun.SftpCommand(cfg.Resolve(*h), startDir))
	if !started {
		run.end(runErr)
		return 0, runErr
	}
	run.end(runErr)
	code := 0
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		_ = history.RecordAndSave(h.Name)
	case errors.As(runErr, &exitErr):
		if code = exitErr.ExitCode(); code < 0 {
			code = 1
		}
	default:
		return 0, runErr
	}
	return code, nil
}
