package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/route"
)

func newRouteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "Other ways to reach a host, used when its own address doesn't answer",
		Long: "A host can have routes besides its own address: a public port forward next to a\n" +
			"VPN address, a different jump host, the LAN address for when you're in the\n" +
			"office. Whenever meterm connects, it checks the first hop of each in turn — the\n" +
			"host's own address first, then its routes in order — and uses the first that\n" +
			"answers. Hosts that jump through it follow automatically, so a whole office can\n" +
			"move from a VPN to a port forward by giving one jump host a second route.\n\n" +
			"  meterm route add devbox public --address 203.0.113.7 --port 5000\n" +
			"  meterm route add db1 office-lan --address 192.168.1.20 --direct\n" +
			"  meterm route check\n\n" +
			"Routes are shared with the team along with the rest of the host.",
	}
	cmd.AddCommand(newRouteAddCmd(), newRouteListCmd(), newRouteRmCmd(), newRouteCheckCmd())
	return cmd
}

func newRouteAddCmd() *cobra.Command {
	var address, via string
	var port int
	var direct bool
	cmd := &cobra.Command{
		Use:   "add <host> <route-name>",
		Short: "Add a route to a host (tried after its own address)",
		Long: "A route replaces the address, the port and the jump path. What you leave out\n" +
			"keeps the host's own address or port; the path is direct unless --via names a\n" +
			"jump host (a saved host, or user@host[:port]).",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[1])
			if name == "" || name == "default" || strings.ContainsAny(name, " \t,") {
				return fmt.Errorf("route name %q: pick a short word other than \"default\"", args[1])
			}
			if direct && via != "" {
				return fmt.Errorf("--direct and --via contradict each other")
			}
			if address == "" && port == 0 && via == "" && !direct {
				return fmt.Errorf("say how this route differs: --address, --port, --via or --direct")
			}
			if port < 0 || port > 65535 {
				return fmt.Errorf("--port %d is out of range", port)
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			var h *config.Host
			err = cfg.Update(func(c *config.Config) error {
				var idx int
				h, idx = c.Find(args[0])
				if idx < 0 {
					return fmt.Errorf("no such host: %s (use `meterm list` to see saved hosts)", args[0])
				}
				for _, r := range h.Routes {
					if strings.EqualFold(r.Name, name) {
						return fmt.Errorf("%s already has a route called %q (remove it first)", h.Name, r.Name)
					}
				}
				if via != "" {
					for _, hop := range strings.Split(via, ",") {
						if strings.EqualFold(strings.TrimSpace(hop), h.Name) {
							return fmt.Errorf("a host can't jump through itself")
						}
					}
				}
				h.Routes = append(h.Routes, config.Route{Name: name, Address: address, Port: port, ProxyJump: via})
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Printf("Added route %q to %s (tried after its own address).\n", name, h.Name)
			teamEditHint(*h)
			return nil
		},
	}
	cmd.Flags().StringVar(&address, "address", "", "address for this route (default: the host's own)")
	cmd.Flags().IntVar(&port, "port", 0, "SSH port for this route (default: the host's own)")
	cmd.Flags().StringVar(&via, "via", "", "jump host(s) for this route: saved names or user@host[:port], comma-separated")
	cmd.Flags().BoolVar(&direct, "direct", false, "connect straight to the address, even if the host or its group normally jumps (the default without --via)")
	return cmd
}

func newRouteListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list [host...]",
		Aliases: []string{"ls"},
		Short:   "Show hosts' routes, in the order they're tried",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			hosts, err := routeHosts(cfg, args)
			if err != nil {
				return err
			}
			if len(hosts) == 0 {
				fmt.Println("No host has routes yet. Add one with `meterm route add <host> <name> --address …`.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "HOST\tROUTE\tADDRESS\tPORT\tPATH")
			for _, h := range hosts {
				for _, rt := range h.Candidates() {
					addr, port, path := rt.Address, "", "direct"
					if addr == "" {
						addr = h.Address
					}
					if rt.Port != 0 {
						port = fmt.Sprint(rt.Port)
					} else if h.Port != 0 {
						port = fmt.Sprint(h.Port)
					}
					if rt.ProxyJump != "" {
						path = "via " + rt.ProxyJump
					} else if rt.Name == "default" && cfg.Groups[h.Group].ProxyJump != "" {
						path = "via " + cfg.Groups[h.Group].ProxyJump + " (group)"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", h.Name, rt.Name, addr, port, path)
				}
			}
			return w.Flush()
		},
	}
}

func newRouteRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <host> <route-name>",
		Short: "Remove a route from a host",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			var h *config.Host
			var removed string
			err = cfg.Update(func(c *config.Config) error {
				var idx int
				h, idx = c.Find(args[0])
				if idx < 0 {
					return fmt.Errorf("no such host: %s", args[0])
				}
				for i, r := range h.Routes {
					if strings.EqualFold(r.Name, args[1]) {
						h.Routes = append(h.Routes[:i], h.Routes[i+1:]...)
						removed = r.Name
						return nil
					}
				}
				return fmt.Errorf("%s has no route called %q", h.Name, args[1])
			})
			if err != nil {
				return err
			}
			fmt.Printf("Removed route %q from %s.\n", removed, h.Name)
			teamEditHint(*h)
			return nil
		},
	}
}

func newRouteCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check [host...]",
		Short: "Check every route's first hop, and show which one meterm would use",
		Long: "Dials the first hop of each route (the host, or the first jump host on the\n" +
			"way) and reports which answer. Without names: every host that has routes.\n" +
			"Only the first hop can be checked without logging in.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			hosts, err := routeHosts(cfg, args)
			if err != nil {
				return err
			}
			if len(hosts) == 0 {
				fmt.Println("No host has routes yet.")
				return nil
			}
			p := route.Install(cfg, "")
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "HOST\tROUTE\tFIRST HOP\tRESULT\t")
			for _, h := range hosts {
				probes := p.CheckAll(h)
				chosen := ""
				for _, pr := range probes {
					if pr.Err == "" && chosen == "" {
						chosen = pr.Route
					}
				}
				for _, pr := range probes {
					res := "answers"
					if pr.Err != "" {
						res = pr.Err
					}
					if pr.Route == chosen {
						res += "  ← used"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\n", h.Name, pr.Route, pr.Hop, res)
				}
			}
			return w.Flush()
		},
	}
}

// routeHosts is the named hosts, or every host that has routes.
func routeHosts(cfg *config.Config, names []string) ([]config.Host, error) {
	var out []config.Host
	if len(names) == 0 {
		for _, h := range cfg.SortedHosts() {
			if len(h.Routes) > 0 {
				out = append(out, h)
			}
		}
		return out, nil
	}
	for _, n := range names {
		h, idx := cfg.Find(n)
		if idx < 0 {
			return nil, fmt.Errorf("no such host: %s", n)
		}
		out = append(out, *h)
	}
	return out, nil
}

// routeNote says which routes a connection takes, when any hop isn't using
// its own address: "via route public of DEVBOX-1".
func routeNote(r config.Resolved) string {
	var parts []string
	for _, j := range r.ProxyJump {
		if j.Route != "" {
			parts = append(parts, fmt.Sprintf("route %q of %s", j.Route, j.Name))
		}
	}
	if r.Route != "" {
		parts = append(parts, fmt.Sprintf("route %q of %s", r.Route, r.Name))
	}
	if len(parts) == 0 {
		return ""
	}
	return "via " + strings.Join(parts, ", ")
}
