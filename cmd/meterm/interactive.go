package main

import (
	"os"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"meterm/internal/tui"
)

func runInteractive(cmd *cobra.Command, args []string) error {
	// A script, CI step, or AI agent running bare `meterm` with no TTY
	// attached would otherwise block on (or error out of) the picker with a
	// confusing terminal error. Fall back to --help instead.
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return cmd.Help()
	}
	path, err := resolveCfgPath()
	if err != nil {
		return err
	}
	kickAutosync(true, false)
	tui.Version = version
	tui.Commands = commandTree(cmd.Root())
	tui.HostAttention = hostAttention
	tui.TeamNames = teamNames
	return tui.Run(path, teamNotice())
}

// commandTree is the visible commands and their subcommands, for the
// palette's tab completion.
func commandTree(root *cobra.Command) map[string][]string {
	out := map[string][]string{}
	for _, c := range root.Commands() {
		if c.Hidden || c.Deprecated != "" || c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		var subs []string
		for _, s := range c.Commands() {
			if !s.Hidden && s.Deprecated == "" && s.Name() != "help" {
				subs = append(subs, s.Name())
			}
		}
		out[c.Name()] = subs
	}
	return out
}
