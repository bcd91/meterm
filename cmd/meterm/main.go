// Command meterm is a small manager for SSH connections: it remembers each
// machine's address, user, key, group, and jump host so you never have to.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	_ "earlyterm" // settles the background before Bubble Tea's init asks the terminal: see the package

	"meterm/internal/config"
	"meterm/internal/route"
	"meterm/internal/team"
	"meterm/internal/termsafe"
	"meterm/internal/tui"
)

var cfgPath string

// version is set at build time via -ldflags "-X main.version=..." (goreleaser
// fills this in from the git tag); it stays "dev" for a plain `go build`.
var version = "dev"

// resolveCfgPath returns the effective config path: --config if given,
// otherwise the default location. Callers that need to hand this path to a
// re-exec'd child (the TUI's command dispatch) use this instead of relying
// on the child re-deriving the default itself.
func resolveCfgPath() (string, error) {
	if cfgPath != "" {
		return cfgPath, nil
	}
	return config.DefaultPath()
}

// loadConfig loads the host file with route picking on: a host with saved
// routes is reached by whichever answers (see internal/route). Commands
// that only display or export hosts turn it off with SetRoutePicker(nil).
func loadConfig() (*config.Config, error) {
	path, err := resolveCfgPath()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	route.Install(cfg, filepath.Join(filepath.Dir(path), "routes.json")).Avoid = avoidedRoutes
	return cfg, nil
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		afterRun(err)
		// Errors can quote what was typed, or a saved name: not for the terminal to obey.
		fmt.Fprintln(os.Stderr, "meterm:", termsafe.Lines(err.Error()))
		finish(1)
	}
	finish(0)
}

// errCancelled is a command the person said n to: not done, but not failed.
// It still exits 1, with "meterm: cancelled: …" on stderr.
var errCancelled = errors.New("cancelled")

// cancelled is errCancelled saying what didn't happen: "nothing removed".
func cancelled(what string) error { return fmt.Errorf("%w: %s", errCancelled, what) }

// tellPicker says to the picker that started this command, through the file
// it named (tui.SessionOutcomeEnv), how it ended when that wasn't a failure
// though it exits non-zero: "cancelled", "stopped". Nothing when no picker
// asked.
func tellPicker(word string) {
	if p := os.Getenv(tui.SessionOutcomeEnv); p != "" {
		_ = os.WriteFile(p, []byte(word+"\n"), 0o600)
	}
}

// afterRun tells the picker, if one started this, that a command which
// ends with err was cancelled rather than failed. It still exits 1: a
// script must see it didn't happen.
func afterRun(err error) {
	if errors.Is(err, errCancelled) {
		tellPicker("cancelled")
	}
}

// takeGlobalFlags takes meterm's own flags (--config, --debug) off the
// front of args, for a command that parses its own arguments (connect,
// sync): cobra hands such a command every argument, the flags given before
// its name included, so "meterm --config x connect web1" would otherwise
// look for a host called "--config".
func takeGlobalFlags(args []string) ([]string, error) {
	for len(args) > 0 {
		switch a := args[0]; {
		case a == "--config":
			if len(args) < 2 {
				return nil, errors.New("--config needs the path to a host file")
			}
			cfgPath, args = args[1], args[2:]
		case strings.HasPrefix(a, "--config="):
			cfgPath, args = strings.TrimPrefix(a, "--config="), args[1:]
		case a == "--debug":
			team.SetDebug(os.Stderr)
			os.Setenv("METERM_DEBUG", "1")
			args = args[1:]
		default:
			return args, nil
		}
	}
	return args, nil
}

// newRootCmd is meterm with all its commands, as main runs it: tests drive
// it too, so what runs before every command (the picker's token) runs there.
// --config binds to cfgPath; a value already set (a test's) stays.
func newRootCmd() *cobra.Command {
	keep := cfgPath
	root := &cobra.Command{
		Use:     "meterm",
		Short:   "Remember your SSH hosts so you don't have to",
		Version: version,
		Long: "meterm keeps an inventory of your SSH machines (address, user, key, group, jump host)\n" +
			"and gets you onto any of them by name. Run with no arguments for an interactive picker.",
		RunE: runInteractive,
	}
	// main prints errors itself ("meterm: …"); cobra would print it a second
	// time plus the whole usage text, which buries the actual message.
	root.SilenceUsage = true
	root.SilenceErrors = true
	root.PersistentFlags().StringVar(&cfgPath, "config", keep, "path to meterm's host file (default: OS config dir)/meterm/hosts.yaml")
	var debug bool
	root.PersistentFlags().BoolVar(&debug, "debug", false, "trace the git commands team commands run, on stderr; same as METERM_DEBUG=1")
	root.PersistentPreRun = func(*cobra.Command, []string) {
		takePickerAnswer() // before anything else: see pickerAnswer
		if debug {
			team.SetDebug(os.Stderr)
			os.Setenv("METERM_DEBUG", "1") // and in background syncs started from here
		}
	}

	root.AddCommand(
		newConnectCmd(),
		newDevCmd(),
		newListCmd(),
		newStatusCmd(),
		newAgentsCmd(),
		newAddCmd(),
		newEditCmd(),
		newRmCmd(),
		newRenameCmd(),
		newGroupCmd(),
		newImportCmd(),
		newExportCmd(),
		newCpCmd(),
		newSftpCmd(),
		newFilesCmd(),
		newSyncCmd(),
		newExecCmd(),
		newTunnelCmd(),
		newRouteCmd(),
		newRelayCmd(),
		newHistoryCmd(),
		newCopyIDCmd(),
		newKeysCmd(),
		newKeygenCmd(),
		newStatsCmd(),
		newLogsCmd(),
		newAuditCmd(),
		newReportCmd(),
		newActivityCmd(),
		newFactsCmd(),
		newAuthMethodsCmd(),
		newLoginsCmd(),
		newTeamCmd(),
	)

	return root
}
