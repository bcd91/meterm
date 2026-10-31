package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"meterm/internal/history"
)

// historyRow is the --format json shape for one host's connection history —
// a stable schema for scripts/agents, with LastConnected as RFC 3339 (not
// FormatAgo's human string, which shifts every time it's rendered).
type historyRow struct {
	Name          string `json:"name"`
	LastConnected string `json:"last_connected"`
	Count         int    `json:"count"`
}

func newHistoryCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show recently connected hosts",
		Long: "Lists every host with connection history, most recent first.\n\n" +
			"Use --format json for scripting or AI agents: it prints a JSON array of\n" +
			"objects ({name, last_connected, count}) with last_connected as RFC 3339,\n" +
			"instead of the human-oriented table.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				return fmt.Errorf("--format must be text or json, got %q", format)
			}

			path, err := history.Path()
			if err != nil {
				return err
			}
			h, err := history.Load(path)
			if err != nil {
				return err
			}

			type row struct {
				name string
				e    history.Entry
			}
			rows := make([]row, 0, len(h))
			for name, e := range h {
				rows = append(rows, row{name, e})
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].e.Last.After(rows[j].e.Last) })

			if format == "json" {
				out := make([]historyRow, 0, len(rows))
				for _, r := range rows {
					out = append(out, historyRow{
						Name:          r.name,
						LastConnected: r.e.Last.Format("2006-01-02T15:04:05Z07:00"),
						Count:         r.e.Count,
					})
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}

			if len(rows) == 0 {
				fmt.Println("No connection history yet.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tLAST CONNECTED\tCOUNT")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%d\n", r.name, history.FormatAgo(r.e.Last), r.e.Count)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "output format: text or json")
	return cmd
}
