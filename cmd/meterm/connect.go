package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"meterm/internal/history"
	"meterm/internal/sshrun"
)

func newConnectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "connect <name|user@name> [--tmux] [-- [ssh-options] [command]]",
		Aliases: []string{"c", "ssh"},
		Short:   "Connect to a saved host",
		Long: "Connects to a saved host by name; user@name logs in as another account (see\n" +
			"`users` in `meterm edit`).\n\n" +
			"--tmux works inside tmux on the host, so a dropped connection loses nothing:\n" +
			"with sessions already there it lists them to pick one up again (or start a new\n" +
			"one), otherwise it starts a session called meterm. Without tmux on the host it\n" +
			"connects as usual. Takes ssh options after --, not a command.\n\n" +
			"The activity log says how many arguments followed, and --log-args puts them\n" +
			"there too, redacted.",
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
				return cmd.Help()
			}
			args, useTmux := takeFlag(args, "--tmux")
			args, logArgs := takeFlag(args, "--log-args")
			if len(args) == 0 {
				return cmd.Help()
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			name := args[0]
			extra := connectArgs(args[1:])
			h, idx, err := cfg.Target(name)
			if err != nil {
				return err
			}
			if idx < 0 {
				return fmt.Errorf("no such host: %s (use `meterm list` to see saved hosts)", name)
			}
			_ = history.RecordAndSave(h.Name) // best-effort; a broken history file shouldn't block connecting
			run := beginActivity(cfg, *h, "connect", connectDetail(extra, logArgs))
			kickAutosync(false, true)
			r := cfg.Resolve(*h)
			if via := routeNote(r); via != "" {
				fmt.Fprintf(os.Stderr, "meterm: %s\n", via)
			}
			if useTmux && r.Err == nil {
				if remote, ok := tmuxCommand(name, r); ok {
					extra = append(append(extra, "-t"), remote)
				}
			}
			return run.runInherited(sshrun.Command(r, extra...))
		},
	}
	return cmd
}

// connectArgs is what follows the host name, handed to ssh. The "--" that
// separates it from meterm's own arguments is meterm's, not ssh's: passed
// on, it would end ssh's options there, so `connect web1 -- -t "sudo …"`
// would run "-t sudo …" on the host instead of getting a terminal.
func connectArgs(rest []string) []string {
	if len(rest) > 0 && rest[0] == "--" {
		return rest[1:]
	}
	return rest
}

// connectDetail is what the log says about what followed the host name, which
// can be a remote command.
func connectDetail(extra []string, logArgs bool) string {
	switch {
	case len(extra) == 0:
		return ""
	case logArgs:
		return strings.Join(extra, " ")
	}
	return fmt.Sprintf("(+%d args)", len(extra))
}

// takeFlag pulls a flag out of connect's own arguments (those before
// "--"); connect parses its flags by hand so the rest can go to ssh as is.
func takeFlag(args []string, flag string) ([]string, bool) {
	var out []string
	found := false
	for i, a := range args {
		if a == "--" {
			return append(out, args[i:]...), found
		}
		if a == flag {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}
