package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/audit"
)

// newReportCmd wraps `audit` for the common MSP/consulting shape: "run the
// baseline against one client's hosts and hand them something presentable",
// instead of a terminal dump meant for the person who ran it.
//
// There's no new data model here on purpose — a "client" is just a tag by
// convention (client:<name>), so this needs zero schema change and existing
// commands (list --tag, audit --tag) already understand it. If that
// convention earns its keep, a first-class Client field can follow later;
// until then this is the cheapest way to find out.
func newReportCmd() *cobra.Command {
	var client, title, out, format string
	var extraTags, groups, probeAddrs []string
	var all, probe bool
	var parallel, accessDays int
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Run the security baseline for one client and produce a report to hand them",
		Long: "A thin wrapper around `audit`, shaped for handing the result to someone\n" +
			"else — a client, in an MSP/consulting setup — instead of reading it\n" +
			"yourself in a terminal.\n\n" +
			"Hosts belong to a client by convention, not a separate field: tag each\n" +
			"of that client's hosts \"client:<name>\" (e.g. \"client:acme\") the same way\n" +
			"you'd tag exposes:web or internal, then run:\n\n" +
			"  meterm add web1 --address 10.0.1.10 --tags client:acme,exposes:web\n" +
			"  meterm report --client acme\n\n" +
			"Defaults to a self-contained HTML file (no external CSS/JS — safe to\n" +
			"email or hand over as-is) written next to where you run it; --format\n" +
			"text/json behave like `audit` and print to stdout instead.",
		Example: "  meterm report --client acme\n" +
			"  meterm report --client acme --title \"Acme Corp — Q4 2026\" --out ~/reports/acme-q4.html\n" +
			"  meterm report --client acme --format json | jq .",
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "html" && format != "text" && format != "json" {
				return fmt.Errorf("--format must be html, text, or json, got %q", format)
			}
			if client == "" && len(extraTags) == 0 && len(groups) == 0 && !all {
				return fmt.Errorf("need --client <name> (or --tag/--group/--all) to know which hosts to report on")
			}

			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			tags := extraTags
			if client != "" {
				tags = append(tags, "client:"+client)
			}
			hosts, err := selectHosts(cfg, nil, tags, groups, all)
			if err != nil {
				return err
			}
			if len(hosts) == 0 {
				hint := "no hosts matched"
				if client != "" {
					hint = fmt.Sprintf("no hosts tagged \"client:%s\" found — tag some with `meterm edit <name> --tags client:%s`", client, client)
				}
				return fmt.Errorf("%s", hint)
			}

			addrs, err := parseProbeAddrs(probeAddrs)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			rep := runAudit(ctx, cfg, hosts, addrs, probe, parallel)
			if accessDays > 0 {
				rep.Access = teamAccess(ctx, hosts, accessDays)
			}

			if title == "" {
				if client != "" {
					title = client
				} else {
					title = "Security baseline audit"
				}
			}

			switch format {
			case "json":
				return writeReportOut(out, "", func(w *os.File) error { return audit.WriteJSON(w, rep) })
			case "text":
				return writeReportOut(out, "", func(w *os.File) error { _, err := w.WriteString(audit.RenderText(rep)); return err })
			default: // html
				defaultName := fmt.Sprintf("meterm-report-%s-%s.html", safeFilenamePart(title), time.Now().Format("2006-01-02"))
				return writeReportOut(out, defaultName, func(w *os.File) error { return audit.RenderHTML(w, rep, title) })
			}
		},
	}
	cmd.Flags().StringVar(&client, "client", "", "report on hosts tagged client:<name>")
	cmd.Flags().StringVar(&title, "title", "", "report heading (default: the client name)")
	cmd.Flags().StringVar(&out, "out", "", "write to this file; for --format html, defaults to a generated filename instead of stdout")
	cmd.Flags().StringVar(&format, "format", "html", "output format: html, text, or json")
	cmd.Flags().StringArrayVar(&extraTags, "tag", nil, "also select hosts with this tag (repeatable; combine with --client)")
	cmd.Flags().StringArrayVar(&groups, "group", nil, "also select hosts in this group (repeatable)")
	cmd.Flags().BoolVar(&all, "all", false, "select every saved host (ignores --client)")
	cmd.Flags().IntVarP(&parallel, "parallel", "j", 5, "maximum concurrent connections")
	cmd.Flags().IntVar(&accessDays, "access-days", 30, "include who accessed these hosts over this many days, from the team activity log (0 = leave out)")
	cmd.Flags().BoolVar(&probe, "probe", false, "also test reachability of exposed ports from this machine (needs --probe-addr)")
	cmd.Flags().StringArrayVar(&probeAddrs, "probe-addr", nil, "public address to probe, as NAME=ADDR (repeatable)")
	return cmd
}

// writeReportOut runs render against out (if set) or defaultPath (if out is
// empty and defaultPath isn't) or stdout (if both are empty), and prints
// where it landed when it wasn't stdout.
func writeReportOut(out, defaultPath string, render func(*os.File) error) error {
	path := out
	if path == "" {
		path = defaultPath
	}
	if path == "" {
		return render(os.Stdout)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if err := render(f); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	return nil
}

// safeFilenamePart turns a free-text title into something sane to embed in
// a default filename — lowercase, spaces/slashes to hyphens, nothing exotic.
func safeFilenamePart(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastHyphen := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "report"
	}
	return out
}
