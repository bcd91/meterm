package main

import (
	"fmt"

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
			return runSftpShell(args)
		},
	}
	return cmd
}

// runSftpShell opens OpenSSH's interactive sftp prompt on a saved host.
func runSftpShell(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	h, idx, err := cfg.Target(args[0])
	if err != nil {
		return err
	}
	if idx < 0 {
		return fmt.Errorf("no such host: %s (use `meterm list` to see saved hosts)", args[0])
	}
	startDir := ""
	if len(args) == 2 {
		startDir = args[1]
	}
	_ = history.RecordAndSave(args[0])
	recordActivity(cfg, *h, "sftp", startDir)
	kickAutosync(false, true)
	return runInherited(sshrun.SftpCommand(cfg.Resolve(*h), startDir))
}
