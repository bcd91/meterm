package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/logfiles"
	"meterm/internal/sshrun"
)

func newLogsListCmd(raw *bool) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list <name> [dir]",
		Short: "List log files on a saved host (default dir: /var/log)",
		Long: "Runs `ls -la` on a log directory so you can find the right file to pass to\n" +
			"`meterm logs`, e.g. `meterm logs list web1 /var/log/nginx` on a box that logs\n" +
			"each vhost to its own file.\n\n" +
			"By default this hides rotated/compressed history (access.log.1,\n" +
			"access.log.2.gz, ...) so the currently-active files are easy to spot;\n" +
			"pass --all to see everything.",
		Args: cobra.RangeArgs(1, 2),
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
			if r := cfg.Resolve(*h); r.Err == nil && isWindowsServer(r) {
				dir := "" // the event logs
				if len(args) == 2 {
					dir = args[1]
				}
				run := beginActivity(cfg, *h, "logs", strings.TrimSpace("list "+dir))
				kickAutosync(false, true)
				c := sshrun.WinListLogsCommand(r, dir)
				guardOutput(c, *raw)
				return run.runInherited(c)
			}
			dir := logfiles.DefaultDir
			if len(args) == 2 {
				dir = args[1]
			}
			run := beginActivity(cfg, *h, "logs", "list "+dir)
			kickAutosync(false, true)
			err = listLogs(cfg, *h, dir, all, *raw)
			run.end(err)
			return err
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "also show rotated and .gz-compressed history")
	return cmd
}

// listLogs prints the log files in dir on h, hiding rotated history unless all.
func listLogs(cfg *config.Config, h config.Host, dir string, all, raw bool) error {
	c := sshrun.SSH(cfg.Resolve(h), "ls -la -- "+globQuote(dir))
	c.Stdin = nil
	out, err := c.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return fmt.Errorf("listing %s: %s", dir, safeText(strings.TrimSpace(string(ee.Stderr)), os.Stderr, raw))
		}
		return fmt.Errorf("listing %s: %w", dir, err)
	}
	entries := logfiles.Parse(string(out), all)
	if len(entries) == 0 {
		if all {
			fmt.Printf("No files in %s.\n", dir)
		} else {
			fmt.Printf("No current (non-rotated) files in %s — pass --all to see rotated/compressed history too.\n", dir)
		}
		return nil
	}
	for _, e := range entries {
		fmt.Println(safeText(e.Raw, os.Stdout, raw))
	}
	return nil
}
