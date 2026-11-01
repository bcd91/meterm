package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/audit"
	"meterm/internal/config"
	"meterm/internal/notify"
	"meterm/internal/team"
)

func newAuditWatchCmd() *cobra.Command {
	var (
		tags, groups, probeAddrs, hooks []string
		all, probe, quiet, noTeam       bool
		parallel                        int
		name                            string
	)
	cmd := &cobra.Command{
		Use:   "watch [host...]",
		Short: "Run the audit, compare with the last run, and alert on what changed",
		Long: "Meant for cron or a scheduled task. Each run audits the selected hosts,\n" +
			"compares the result with the previous run of the same watch, prints what\n" +
			"changed, and posts it to the --notify webhooks: a new or worse failure, new\n" +
			"evidence on an existing one (another open port, another dropped file), a host\n" +
			"that can't be audited any more — and fixes, so you see those too. Nothing is\n" +
			"sent when nothing changed. The first run sends the current failures.\n\n" +
			"--notify takes a Slack or Discord incoming-webhook URL, or any other URL\n" +
			"(it gets {\"text\": …, \"changes\": […]} as JSON; n8n/Zapier/Make can forward\n" +
			"that to email or LINE).\n\n" +
			"If you're signed in to a team, the latest result for every shared host is\n" +
			"also stored in the team, where `meterm team audits` shows it to everyone.\n\n" +
			"Example (cron, every morning at 07:00):\n" +
			"  0 7 * * * meterm audit watch --tag acme --notify https://hooks.slack.com/services/…",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			hosts, err := selectHosts(cfg, args, tags, groups, all)
			if err != nil {
				return err
			}
			if len(hosts) == 0 {
				return fmt.Errorf("no hosts matched (use names, --tag, --group, or --all)")
			}
			addrs, err := parseProbeAddrs(probeAddrs)
			if err != nil {
				return err
			}
			statePath, err := watchStatePath(name)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			cur := runAudit(ctx, cfg, hosts, addrs, probe, parallel)

			prev, hadPrev, err := audit.LoadReport(statePath)
			if err != nil {
				return err
			}
			var changes []audit.WatchChange
			title := fmt.Sprintf("meterm audit watch %q", name)
			if hadPrev {
				changes = audit.Diff(prev, cur)
				title += fmt.Sprintf(": %d change(s) since %s", len(changes), prev.Generated.Format("2006-01-02 15:04"))
			} else {
				changes = audit.FirstRun(cur)
				title += fmt.Sprintf(": first run, %d finding(s) to fix", len(changes))
			}

			if len(changes) == 0 {
				switch {
				case quiet:
				case hadPrev:
					fmt.Printf("No changes since %s (%d host(s)).\n", prev.Generated.Format("2006-01-02 15:04"), len(cur.Hosts))
				default:
					fmt.Printf("First run: nothing failing on %d host(s).\n", len(cur.Hosts))
				}
			} else {
				fmt.Print(audit.FormatChanges(title, changes))
			}

			var notifyErr error
			if len(changes) > 0 {
				for _, h := range hooks {
					if err := notify.Send(ctx, h, audit.FormatChanges(title, changes), map[string]any{"changes": changes}); err != nil {
						notifyErr = errors.Join(notifyErr, fmt.Errorf("notify %s: %w", notify.Redact(h), err))
					}
				}
			}
			if err := audit.SaveReport(statePath, cur); err != nil {
				return err
			}
			if !noTeam {
				if err := uploadAudits(ctx, hosts, cur); err != nil {
					fmt.Fprintf(os.Stderr, "meterm: warning: couldn't store results in the team: %v\n", err)
				}
			}
			return notifyErr
		},
	}
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "select hosts with this tag (repeatable)")
	cmd.Flags().StringArrayVar(&groups, "group", nil, "select hosts in this group (repeatable)")
	cmd.Flags().BoolVar(&all, "all", false, "select every saved host")
	cmd.Flags().IntVarP(&parallel, "parallel", "j", 5, "maximum concurrent connections")
	cmd.Flags().BoolVar(&probe, "probe", false, "also test reachability of exposed ports from this machine (needs --probe-addr)")
	cmd.Flags().StringArrayVar(&probeAddrs, "probe-addr", nil, "public address to probe, as NAME=ADDR (repeatable)")
	cmd.Flags().StringArrayVar(&hooks, "notify", nil, "webhook to post changes to: Slack, Discord, or any URL taking JSON (repeatable)")
	cmd.Flags().StringVar(&name, "name", "default", "name of this watch, so several (e.g. one per client) keep separate history")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print nothing when nothing changed (for cron mail)")
	cmd.Flags().BoolVar(&noTeam, "no-team", false, "don't store results in the team")
	return cmd
}

func watchStatePath(name string) (string, error) {
	if err := audit.ValidWatchName(name); err != nil {
		return "", err
	}
	p, err := resolveCfgPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "audit-watch", name+".json"), nil
}

// uploadAudits stores each shared host's result in its team. Not signed in
// or nothing shared: nothing to do.
func uploadAudits(ctx context.Context, hosts []config.Host, rep audit.Report) error {
	teamOf := map[string]string{}
	for _, h := range hosts {
		if h.Team != "" {
			teamOf[h.Name] = h.Team
		}
	}
	if len(teamOf) == 0 {
		return nil
	}
	tc, err := openTeam()
	if err != nil || tc.be == nil {
		return err
	}
	var errs error
	for _, h := range rep.Hosts {
		t, ok := teamOf[h.Host]
		if !ok {
			continue
		}
		rec := team.AuditRecord{Host: h.Host, Time: rep.Generated, Error: h.Error}
		worst := audit.Severity(-1)
		for _, r := range h.Results {
			switch r.Status {
			case audit.Fail:
				rec.Fails++
				rec.Findings = append(rec.Findings, team.AuditFinding{Control: r.Control, Title: r.Title, Severity: r.Severity.String(), Detail: r.Detail})
				if r.Severity > worst {
					worst = r.Severity
				}
			case audit.Unknown:
				rec.Unknowns++
			}
		}
		if worst >= 0 {
			rec.Worst = worst.String()
		}
		if err := tc.be.PutAudit(ctx, t, rec); err != nil {
			errs = errors.Join(errs, fmt.Errorf("%s: %w", h.Host, err))
		}
	}
	return errs
}

func teamAuditsCmd(teamFlag *string) *cobra.Command {
	var format string
	var noFetch bool
	cmd := &cobra.Command{
		Use:   "audits",
		Short: "Latest security audit of each shared host (from `meterm audit watch`)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			open := openTeamAny
			if noFetch {
				open = openTeamLocal
				ctx = team.WithNoFetch(ctx)
			}
			tc, err := open()
			if err != nil {
				return err
			}
			t, err := tc.current(ctx, *teamFlag)
			if err != nil {
				return err
			}
			recs, err := tc.be.ListAudits(ctx, t.ID)
			if err != nil {
				return err
			}
			if format == "json" {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if recs == nil {
					recs = []team.AuditRecord{}
				}
				return enc.Encode(recs)
			}
			if len(recs) == 0 {
				fmt.Println("No audits stored yet. Run `meterm audit watch` on shared hosts (e.g. from cron).")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "HOST\tAUDITED\tBY\tTO FIX\tWORST\tTOP FINDING")
			for _, a := range recs {
				top := ""
				if a.Error != "" {
					top = "could not audit: " + a.Error
				} else if len(a.Findings) > 0 {
					top = a.Findings[0].Title
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", a.Host, humanAgo(time.Since(a.Time)), a.By, a.Fails, a.Worst, oneLine(top, 50))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&format, "format", "text", "text or json")
	cmd.Flags().BoolVar(&noFetch, "no-fetch", false, "use this machine's clone as it is: no network, and only what it last saw")
	return cmd
}
