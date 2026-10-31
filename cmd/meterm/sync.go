package main

import "github.com/spf13/cobra"

func newSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "sync <src> <dst> [-- rsync-flags...]",
		Short:              "Sync a file or directory to/from a saved host with rsync (resumable, incremental)",
		Args:               cobra.MinimumNArgs(2),
		DisableFlagParsing: true,
		Hidden:             true,
		Deprecated:         "use `meterm cp --sync <src> <dst> [-- rsync-flags]`",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
				return cmd.Help()
			}
			flags := args[2:]
			if len(flags) > 0 && flags[0] == "--" {
				flags = flags[1:]
			}
			return runCopy(args[0], args[1], false, true, flags)
		},
	}
	return cmd
}
