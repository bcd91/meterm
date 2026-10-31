package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/status"
	"meterm/internal/termsafe"
)

// statusReport is the --format json shape: stable field names for status bars
// and scripts, with entries always an array, empty when nothing is held open.
type statusReport struct {
	Schema  int          `json:"schema"`
	Entries []status.Row `json:"entries"`
	// Skipped counts processes of a newer meterm whose state couldn't be read.
	Skipped int `json:"skipped,omitempty"`
}

func newStatusCmd() *cobra.Command {
	var format string
	var watch bool
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the sessions and tunnels meterm is holding open right now",
		Long: "Lists what running `meterm dev` and `meterm tunnel up` processes hold open:\n" +
			"which host, which route, and whether it's connected, connecting, reconnecting\n" +
			"or failing. On macOS it also shows a tunnel from `tunnel enable` that has stopped.\n" +
			"It reads local files only and never touches the network.\n\n" +
			"--format json is for status bars and scripts. With --watch it prints one line\n" +
			"of JSON at the start and one for every change.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				return fmt.Errorf("--format must be text or json")
			}
			if !watch {
				rep, err := readStatus()
				if err != nil {
					return err
				}
				return writeStatus(os.Stdout, rep, format)
			}
			if interval <= 0 {
				return fmt.Errorf("--interval must be above zero")
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			ticks := time.NewTicker(interval)
			defer ticks.Stop()
			return watchStatus(ctx, ticks.C, readStatus, func(rep statusReport) error {
				return writeStatus(os.Stdout, rep, format)
			})
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "output format: text or json")
	cmd.Flags().BoolVar(&watch, "watch", false, "keep running, and print again whenever something changes")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "how often --watch looks")
	return cmd
}

func readStatus() (statusReport, error) {
	rows, skipped, err := status.List()
	if err != nil {
		return statusReport{}, err
	}
	rows = serviceRows(rows)
	if rows == nil {
		rows = []status.Row{}
	}
	return statusReport{Schema: status.SchemaVersion, Entries: rows, Skipped: skipped}, nil
}

// watchStatus shows the report at the start and again each time it's different.
// A read that fails is shown as an error only at the start; later ones may just
// be the moment a process ended, and the next look settles it.
func watchStatus(ctx context.Context, tick <-chan time.Time, read func() (statusReport, error), show func(statusReport) error) error {
	rep, err := read()
	if err != nil {
		return err
	}
	if err := show(rep); err != nil {
		return err
	}
	last, _ := json.Marshal(rep)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
		}
		rep, err := read()
		if err != nil {
			continue
		}
		now, _ := json.Marshal(rep)
		if string(now) == string(last) {
			continue
		}
		last = now
		if err := show(rep); err != nil {
			return err
		}
	}
}

func writeStatus(w io.Writer, rep statusReport, format string) error {
	if format == "json" {
		return json.NewEncoder(w).Encode(rep)
	}
	if rep.Skipped > 0 {
		fmt.Fprintf(w, "(%d process(es) of a newer meterm not shown)\n", rep.Skipped)
	}
	if len(rep.Entries) == 0 {
		_, err := fmt.Fprintln(w, "Nothing held open.")
		return err
	}
	// All at once: tabwriter writes cell by cell, and a reader that has seen what it
	// wanted, such as grep -q, would end this with SIGPIPE halfway through.
	var buf bytes.Buffer
	t := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	fmt.Fprintln(t, "HOST\tKIND\tNAME\tSTATE\tROUTE\tFOR\tDETAIL")
	for _, r := range rep.Entries {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", termsafe.Text(r.Host), termsafe.Text(r.Kind), termsafe.Text(r.Name),
			termsafe.Text(r.State), termsafe.Text(r.Route), time.Since(r.Since).Round(time.Second), termsafe.Text(r.Detail))
	}
	if err := t.Flush(); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}
