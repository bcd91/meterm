package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/audit"
	"meterm/internal/config"
	"meterm/internal/sshrun"
)

func newAuditCmd() *cobra.Command {
	var tags, groups, probeAddrs []string
	var all, probe bool
	var parallel int
	var format string
	cmd := &cobra.Command{
		Use:   "audit [host...]",
		Short: "Check internet-facing hosts against a security baseline (read-only)",
		Long: "Runs the same baseline on every selected host — OS support, patching, SSH\n" +
			"hardening, brute-force protection, firewall, exposed ports, web hardening,\n" +
			"log visibility, TLS renewal — and lists what to fix grouped by who has to do\n" +
			"it (sysadmin, network/cloud firewall, application owner).\n\n" +
			"It only reads: one script is piped to `sh -s` on each host and changes\n" +
			"nothing. It never uses sudo interactively; checks that need root show up\n" +
			"as \"could not be checked\", never as passing.\n\n" +
			"Which controls apply depends on how the host faces the internet. Tag hosts\n" +
			"with exposes:ssh and/or exposes:web (or \"internal\"); an untagged host is\n" +
			"assumed to expose both.\n\n" +
			"--probe additionally opens a plain TCP connection from THIS machine to each\n" +
			"port a host has bound to all interfaces, using the address given by\n" +
			"--probe-addr NAME=ADDR (the public address; the inventory address is often a\n" +
			"VPN or LAN one). Run it from outside the network for the result to mean\n" +
			"\"reachable from the internet\".\n\n" +
			"Examples:\n" +
			"  meterm audit --all\n" +
			"  meterm audit WEB GW1 --probe --probe-addr WEB=203.0.113.10 --probe-addr GW1=203.0.113.20\n" +
			"  meterm audit --tag exposes:web --format json",
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
			if format != "text" && format != "json" {
				return fmt.Errorf("--format must be text or json, got %q", format)
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			rep := runAudit(ctx, cfg, hosts, addrs, probe, parallel)
			if format == "json" {
				return audit.WriteJSON(os.Stdout, rep)
			}
			fmt.Print(audit.RenderText(rep))
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "select hosts with this tag (repeatable)")
	cmd.Flags().StringArrayVar(&groups, "group", nil, "select hosts in this group (repeatable)")
	cmd.Flags().BoolVar(&all, "all", false, "select every saved host")
	cmd.Flags().IntVarP(&parallel, "parallel", "j", 5, "maximum concurrent connections")
	cmd.Flags().BoolVar(&probe, "probe", false, "also test reachability of exposed ports from this machine (needs --probe-addr)")
	cmd.Flags().StringArrayVar(&probeAddrs, "probe-addr", nil, "public address to probe, as NAME=ADDR (repeatable)")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text or json")
	cmd.AddCommand(newAuditWatchCmd())
	return cmd
}

func parseProbeAddrs(specs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, s := range specs {
		name, addr, ok := strings.Cut(s, "=")
		if !ok || name == "" || addr == "" {
			return nil, fmt.Errorf("--probe-addr wants NAME=ADDR, got %q", s)
		}
		out[name] = addr
	}
	return out, nil
}

func runAudit(ctx context.Context, cfg *config.Config, hosts []config.Host, probeAddrs map[string]string, probe bool, parallel int) audit.Report {
	if parallel < 1 {
		parallel = 1
	}
	reports := make([]audit.HostReport, len(hosts))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, h config.Host) {
			defer wg.Done()
			defer func() { <-sem }()
			reports[i] = auditHost(ctx, cfg, h, probeAddrs[h.Name], probe)
		}(i, h)
	}
	wg.Wait()
	return audit.Report{Generated: time.Now(), Hosts: reports}
}

func auditHost(ctx context.Context, cfg *config.Config, h config.Host, probeAddr string, probe bool) audit.HostReport {
	exp := audit.ParseExposure(h.Tags)
	rep := audit.HostReport{Host: h.Name, Exposure: exp.String()}

	out, err := collectFacts(ctx, cfg.Resolve(h))
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	f := audit.Parse(out)
	rep.HaveSudo = f.HaveSudo
	rep.OS = f.OSPretty

	var probed map[int]bool
	if probe {
		// The inventory address is often a VPN or LAN one (Tailscale, a jump
		// host), and probing that says nothing about the internet. If the
		// host has a public address of its own, use it; the auditing machine
		// then reaches it the same way anyone else would.
		if probeAddr == "" {
			if pub := f.PublicAddrs(); len(pub) > 0 {
				probeAddr = pub[0]
				rep.Notes = append(rep.Notes, "probing the host's own public address "+probeAddr+" (no --probe-addr given)")
			}
		}
		if probeAddr == "" {
			rep.Notes = append(rep.Notes, "--probe given but "+h.Name+" has no public address of its own (behind NAT or a proxy?), so pass --probe-addr "+h.Name+"=<the address the internet sees>; reachability was not tested")
		} else {
			probed = audit.Probe(ctx, &net.Dialer{}, probeAddr, audit.PortsToProbe(f), 3*time.Second)
			rep.Probed = probed
		}
	}
	rep.Results = audit.Evaluate(exp, f, probed, time.Now())
	return rep
}

// collectFacts pipes the read-only fact script to `sh -s` on the host. It
// runs unattended (BatchMode) and gives up on a dead host quickly rather
// than hanging the whole fan-out.
func collectFacts(ctx context.Context, r config.Resolved) (string, error) {
	r.ExtraArgs = append([]string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}, r.ExtraArgs...)
	c := sshrun.SSHContext(ctx, r, "sh", "-s")
	c.Stdin = strings.NewReader(audit.Script)
	var stderr strings.Builder
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		// Keep the last line: ssh's warnings come first, the reason last.
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:]
		}
		return "", fmt.Errorf("%s", msg)
	}
	return string(out), nil
}
