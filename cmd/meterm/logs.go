package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"meterm/internal/logfiles"
	"meterm/internal/sshrun"
)

func newLogsCmd() *cobra.Command {
	var follow bool
	var lines int
	var raw bool
	cmd := &cobra.Command{
		Use:   "logs <name> [path...]",
		Short: "Tail log files on a saved host",
		Long: "Runs `tail` on a saved host — no need to re-type user/port/jump-host to\n" +
			"look at a log. Without a path it lists what's in /var/log instead.\n\n" +
			"Multiple paths (or a shell glob, e.g. \"api_*.access.log\") work too — they're\n" +
			"expanded and concatenated on the remote side, same as `tail file1 file2` would\n" +
			"be if you ran it there yourself. Handy on a box that logs each vhost to its\n" +
			"own file; see `meterm logs list` to find the names. Only * ? [ ] and a leading\n" +
			"~ stay live: nothing else in a path means anything to the remote shell.\n\n" +
			"On a Windows server (recognised from its SSH version) a path is a file\n" +
			"(C:\\app\\log.txt, D:/logs/*.log) shown with Get-Content, and a plain name is an\n" +
			"event log (System, Application, Microsoft-Windows-PowerShell/Operational; or\n" +
			"event:<name>), one line per event. Without a path it lists the event logs.\n\n" +
			"On a terminal, escape sequences other than colours are dropped from what the host\n" +
			"sends, so a line in a log can't act on your terminal. --raw shows them as they are.",
		Args: cobra.MinimumNArgs(1),
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
			paths := args[1:]
			r := cfg.Resolve(*h)
			if err := r.Err; err != nil {
				return err
			}
			windows := isWindowsServer(r)
			if len(paths) == 0 {
				recordActivity(cfg, *h, "logs", "list")
				kickAutosync(false, true)
				if windows {
					list := sshrun.WinListLogsCommand(r, "")
					guardOutput(list, raw)
					if err := runInherited(list); err != nil {
						return err
					}
					fmt.Printf("\nPick one: meterm logs %s System [-f]   (an event log above, or a file: meterm logs %s 'C:\\path\\app.log')\n", h.Name, h.Name)
					return nil
				}
				if err := listLogs(cfg, *h, logfiles.DefaultDir, false, raw); err != nil {
					return err
				}
				fmt.Printf("\nPick one: meterm logs %s %s/<file> [-f]\n", h.Name, logfiles.DefaultDir)
				return nil
			}
			recordActivity(cfg, *h, "logs", strings.Join(paths, " "))
			kickAutosync(false, true)
			if windows {
				c, err := sshrun.WinLogsCommand(r, paths, lines, follow)
				if err != nil {
					return err
				}
				guardOutput(c, raw)
				return runInherited(c)
			}

			c := sshrun.SSH(r, logsTailScript(paths, lines, follow))
			c.Stdin = nil
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			guardOutput(c, raw)
			return runInherited(c)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new lines as they're written, like tail -f (Ctrl-C to stop)")
	cmd.Flags().IntVarP(&lines, "lines", "n", 200, "number of lines to show")
	cmd.PersistentFlags().BoolVar(&raw, "raw", false, "show the host's output as it is, escape sequences included")
	cmd.AddCommand(newLogsListCmd(&raw))
	return cmd
}

// logsTailScript is the remote command line for tailing paths. A path can come from
// anywhere, a log line an agent was reading included, so it's quoted: looking at a
// file mustn't run anything. A glob still has to expand on the host.
func logsTailScript(paths []string, lines int, follow bool) string {
	var b strings.Builder
	b.WriteString("tail")
	if follow {
		b.WriteString(" -f")
	}
	fmt.Fprintf(&b, " -n %d --", lines)
	for _, p := range paths {
		b.WriteString(" " + globQuote(p))
	}
	return b.String()
}

// globQuote is shQuote for a path, leaving only the glob characters and a leading
// ~ for the remote shell to act on.
func globQuote(p string) string {
	var b strings.Builder
	switch {
	case p == "~":
		return `"$HOME"`
	case strings.HasPrefix(p, "~/"):
		b.WriteString(`"$HOME"/`)
		p = p[2:]
	}
	if p == "" && b.Len() == 0 {
		return "''"
	}
	for p != "" {
		if i := strings.IndexAny(p, "*?[]"); i != 0 {
			if i < 0 {
				i = len(p)
			}
			b.WriteString(shQuote(p[:i]))
			p = p[i:]
			continue
		}
		b.WriteByte(p[0])
		p = p[1:]
	}
	return b.String()
}
