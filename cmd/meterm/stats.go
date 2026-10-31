package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"meterm/internal/sshrun"
)

func newStatsCmd() *cobra.Command {
	var raw bool
	cmd := &cobra.Command{
		Use:   "stats <name>",
		Short: "Show CPU, memory, disk, and network info from a saved host",
		Long: "SSHes in and runs a one-shot diagnostic dump — uptime/load, CPU core count,\n" +
			"memory, disk usage, and network interfaces — aware of both Linux and\n" +
			"macOS/BSD remotes, and Windows (OpenSSH) ones. Non-interactive like exec: needs working key-based auth\n" +
			"already, and fails fast instead of hanging on a password prompt.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			r := cfg.Resolve(*h)
			fmt.Printf("Fetching stats from %s (%s)...\n\n", args[0], r.Address)
			c := sshrun.StatsCommand(r)
			if r.Err == nil && isWindowsServer(r) {
				c = sshrun.WinStatsCommand(r)
			}
			guardOutput(c, raw)
			return runInherited(c)
		},
	}
	cmd.Flags().BoolVar(&raw, "raw", false, "show the host's output as it is, escape sequences included")
	return cmd
}
