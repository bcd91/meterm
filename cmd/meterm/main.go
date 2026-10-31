// Command meterm is a small manager for SSH connections: it remembers each
// machine's address, user, key, group, and jump host so you never have to.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/route"
	"meterm/internal/team"
	"meterm/internal/termsafe"
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
	root.PersistentFlags().StringVar(&cfgPath, "config", "", "path to meterm's host file (default: OS config dir)/meterm/hosts.yaml")
	var debug bool
	root.PersistentFlags().BoolVar(&debug, "debug", false, "trace the git commands team commands run, on stderr; same as METERM_DEBUG=1")
	root.PersistentPreRun = func(*cobra.Command, []string) {
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

	if err := root.Execute(); err != nil {
		// Errors can quote what was typed, or a saved name: not for the terminal to obey.
		fmt.Fprintln(os.Stderr, "meterm:", termsafe.Lines(err.Error()))
		finish(1)
	}
	finish(0)
}
