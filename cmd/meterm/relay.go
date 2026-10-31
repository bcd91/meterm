package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"meterm/internal/config"
	"meterm/internal/relay"
	"meterm/internal/route"
	"meterm/internal/sshrun"
)

func newRelayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "relay",
		Short: "Reach machines nobody can connect to directly, through a relay host",
		Long: "A machine behind NAT or a closed firewall keeps one outbound SSH connection to a\n" +
			"relay — a saved host everyone can reach — and the rest of the team gets to it\n" +
			"through that, as an extra route of the machine. Only OpenSSH and systemd are\n" +
			"used; nothing is installed.\n\n" +
			"  meterm relay init DEVBOX-1              # once per relay (needs sudo there)\n" +
			"  meterm relay enroll GW1 --via DEVBOX-1  # a machine you can reach now (sudo on both)\n" +
			"  meterm relay invite NEWBOX --via DEVBOX-1   # one you can't: a script to run there\n" +
			"  meterm relay accept NEWBOX <reply>       # ...with the line that script prints\n" +
			"  meterm relay status\n" +
			"  meterm relay remove GW1\n\n" +
			"On the relay, tunnels log in as a separate account, \"" + relay.User + "\", with no shell:\n" +
			"each machine's key may only listen on its own port on the relay's localhost.\n" +
			"The machine trusts the relay's host key as you already do (from your\n" +
			"known_hosts), never blindly. sudo may ask for your password on each host.",
	}
	cmd.AddCommand(newRelayInitCmd(), newRelayEnrollCmd(), newRelayInviteCmd(), newRelayAcceptCmd(), newRelayStatusCmd(), newRelayRemoveCmd())
	return cmd
}

func newRelayInitCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "init <relay>",
		Short: "Prepare a host to act as a relay (once; needs sudo there)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			h, idx := cfg.Find(args[0])
			if idx < 0 {
				return fmt.Errorf("no such host: %s", args[0])
			}
			fmt.Printf("On %s this will, as root:\n", h.Name)
			fmt.Printf("  - create the account %q (no password, no shell)\n", relay.User)
			fmt.Printf("  - add a \"Match User %s\" block to the end of /etc/ssh/sshd_config allowing\n    reverse port forwards on localhost only, and add it to any AllowUsers line\n", relay.User)
			fmt.Println("  - check the config with `sshd -t` (putting the old one back if it fails), then restart sshd")
			fmt.Println("Open SSH sessions stay up across the restart.")
			if !yes && !confirm("Go ahead?") {
				fmt.Println("Nothing changed.")
				return nil
			}
			out, err := runAsRoot(cfg, *h, relay.InitScript())
			if err := scriptResult(h.Name, out, err); err != nil {
				return err
			}
			recordActivity(cfg, *h, "relay-init", relay.User)
			fmt.Printf("%s is ready to be a relay. Next: meterm relay enroll <machine> --via %s\n", h.Name, h.Name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask")
	return cmd
}

func newRelayEnrollCmd() *cobra.Command {
	var via string
	var yes bool
	cmd := &cobra.Command{
		Use:   "enroll <machine> --via <relay>",
		Short: "Give a machine a tunnel to a relay, and a route through it (needs sudo on both)",
		Long: "On the machine (as root): a dedicated key in /etc/meterm-relay, the relay's host\n" +
			"keys, and a systemd unit keeping `ssh -N -R` open to the relay. The machine tries\n" +
			"the relay's routes first, then its own address, and uses the first it can reach.\n" +
			"On the relay: the machine's key, allowed to listen on one localhost port only.\n" +
			"Here: a route \"relay-<relay>\" on the machine, shared with the team.\n\n" +
			"Run it while the machine can still be reached some other way.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if via == "" {
				return fmt.Errorf("say which relay: --via <host>")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			node, ni := cfg.Find(args[0])
			if ni < 0 {
				return fmt.Errorf("no such host: %s", args[0])
			}
			rh, ri := cfg.Find(via)
			if ri < 0 {
				return fmt.Errorf("no such host: %s", via)
			}
			if node.Name == rh.Name {
				return fmt.Errorf("a machine can't relay to itself")
			}
			cands, known, err := relayEndpoints(cfg, *rh)
			if err != nil {
				return err
			}

			// Its port: the one it had through this relay, or a free one.
			routeName := relay.RouteName(rh.Name)
			port := 0
			for _, rt := range node.Routes {
				if rt.Name == routeName {
					port = rt.Port
				}
			}
			if ready, err := runScriptTimeout(cfg, *rh, "id -u "+relay.User+" >/dev/null 2>&1 && echo ready || echo missing"); err == nil && !strings.Contains(ready, "ready") {
				return fmt.Errorf("%s isn't set up as a relay yet: run `meterm relay init %s` first", rh.Name, rh.Name)
			}
			if port == 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				out, err := runScript(ctx, cfg.Resolve(*rh), relay.ListeningScript)
				cancel()
				if err != nil {
					return fmt.Errorf("checking ports on %s: %w", rh.Name, err)
				}
				port = relay.FreePort(relay.UsedPorts(cfg), relay.ParsePorts(out))
			}
			nodePort := cfg.Resolve(*node).Port
			script, err := relay.NodeScript(relay.NodeParams{Relay: rh.Name, Candidates: cands, KnownHosts: known, Port: port, LocalPort: nodePort})
			if err != nil {
				return err
			}

			fmt.Printf("On %s (as root): a key in /etc/meterm-relay and the unit %s,\n", node.Name, relay.UnitName(rh.Name))
			fmt.Printf("  keeping a tunnel to the first of %s it can reach.\n", strings.Join(cands, ", "))
			fmt.Printf("On %s (as root): allow that key to listen on localhost:%d, nothing else.\n", rh.Name, port)
			fmt.Printf("Here: route %q on %s (127.0.0.1:%d via %s).\n", routeName, node.Name, port, rh.Name)
			if !yes && !confirm("Go ahead?") {
				fmt.Println("Nothing changed.")
				return nil
			}

			fmt.Printf("\n[1/3] %s\n", node.Name)
			out, err := runAsRoot(cfg, *node, script)
			if err := scriptResult(node.Name, out, err); err != nil {
				return err
			}
			pub, addr := relay.Marker(out, "PUBKEY"), relay.Marker(out, "RELAY-ADDR")
			fmt.Printf("%s reaches %s at %s.\n", node.Name, rh.Name, addr)

			fmt.Printf("[2/3] %s\n", rh.Name)
			add, err := relay.AddKeyScript(pub, node.Name, port)
			if err != nil {
				return err
			}
			out, err = runAsRoot(cfg, *rh, add)
			if err := scriptResult(rh.Name, out, err); err != nil {
				return fmt.Errorf("%w\n(%s's unit keeps retrying; run enroll again once this is fixed)", err, node.Name)
			}

			fmt.Printf("[3/3] waiting for the tunnel")
			up := waitListening(cfg, *rh, port, 40*time.Second)
			fmt.Println()
			nodeName := node.Name
			err = cfg.Update(func(c *config.Config) error {
				h, i := c.Find(nodeName)
				if i < 0 {
					return fmt.Errorf("%s is no longer a saved host", nodeName)
				}
				setRoute(h, config.Route{Name: routeName, Address: "127.0.0.1", Port: port, ProxyJump: rh.Name})
				node = h
				return nil
			})
			if err != nil {
				return err
			}
			recordActivity(cfg, *node, "relay-enroll", fmt.Sprintf("via %s, port %d", rh.Name, port))
			if !up {
				fmt.Printf("The route is saved, but the tunnel isn't listening on %s yet. On %s, see:\n  systemctl status %s\n", rh.Name, node.Name, relay.UnitName(rh.Name))
				return nil
			}
			fmt.Printf("Tunnel up: %s is reachable through %s (route %q), used whenever its own address doesn't answer.\n", node.Name, rh.Name, routeName)
			fmt.Printf("Check it with: meterm route check %s\n", node.Name)
			teamEditHint(*node)
			return nil
		},
	}
	cmd.Flags().StringVar(&via, "via", "", "the relay: a saved host everyone can reach")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask")
	return cmd
}

func newRelayStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show enrolled machines and whether their tunnels are up",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			enrolled := relay.Enrolled(cfg)
			if invites, err := loadInvites(); err == nil && len(invites) > 0 {
				for n, inv := range invites {
					fmt.Printf("invited, waiting for the reply: %s via %s (port %d, since %s)\n", n, inv.Relay, inv.Port, inv.Created.Format("2006-01-02"))
				}
				fmt.Println()
			}
			if len(enrolled) == 0 {
				fmt.Println("No machine is enrolled with a relay yet (meterm relay enroll <machine> --via <relay>).")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "MACHINE\tRELAY\tPORT\tTUNNEL")
			for name, nodes := range enrolled {
				state := map[int]string{}
				rh, idx := cfg.Find(name)
				listening := map[int]bool{}
				var checkErr error
				if idx < 0 {
					checkErr = fmt.Errorf("relay %s isn't a saved host", name)
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					out, err := runScript(ctx, cfg.Resolve(*rh), relay.ListeningScript)
					cancel()
					checkErr = err
					listening = relay.ParsePorts(out)
				}
				for _, n := range nodes {
					switch {
					case checkErr != nil:
						state[n.Port] = "unknown (" + checkErr.Error() + ")"
					case listening[n.Port]:
						state[n.Port] = "up"
					default:
						state[n.Port] = "down"
					}
					fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", n.Host, name, n.Port, state[n.Port])
				}
			}
			return w.Flush()
		},
	}
}

func newRelayRemoveCmd() *cobra.Command {
	var via string
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove <machine> [--via <relay>]",
		Short: "Take a machine's tunnel down and forget its route (needs sudo on both)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			node, ni := cfg.Find(args[0])
			if ni < 0 {
				return fmt.Errorf("no such host: %s", args[0])
			}
			var rt *config.Route
			for i := range node.Routes {
				if r, ok := relay.IsRelayRoute(node.Routes[i]); ok && (via == "" || strings.EqualFold(r, via)) {
					rt = &node.Routes[i]
					break
				}
			}
			var relayName, routeName string
			if rt != nil {
				relayName, routeName = rt.ProxyJump, rt.Name
			} else if r, ok := relay.DefaultLink(*node); ok && (via == "" || strings.EqualFold(r, via)) {
				relayName, routeName = r, "default" // added by invite: the tunnel is its only way in
			} else {
				return fmt.Errorf("%s isn't enrolled with a relay", node.Name)
			}
			fmt.Printf("Stop and remove %s's tunnel through %s (%s).\n", node.Name, relayName, routeName)
			if !yes && !confirm("Go ahead?") {
				fmt.Println("Nothing changed.")
				return nil
			}
			var problems []string
			out, err := runAsRoot(cfg, *node, relay.NodeRemoveScript(relayName))
			if err := scriptResult(node.Name, out, err); err != nil {
				problems = append(problems, err.Error())
			}
			if rh, idx := cfg.Find(relayName); idx >= 0 {
				out, err := runAsRoot(cfg, *rh, relay.RemoveKeyScript(node.Name))
				if err := scriptResult(rh.Name, out, err); err != nil {
					problems = append(problems, err.Error())
				}
			}
			if routeName == "default" {
				fmt.Printf("The tunnel was %s's only way in; `meterm rm %s` once you no longer need the entry.\n", node.Name, node.Name)
			} else {
				nodeName := node.Name
				err = cfg.Update(func(c *config.Config) error {
					if h, i := c.Find(nodeName); i >= 0 {
						removeRoute(h, routeName)
						node = h
					}
					return nil
				})
				if err != nil {
					return err
				}
				fmt.Printf("Route %q removed from %s.\n", routeName, node.Name)
			}
			teamEditHint(*node)
			if len(problems) > 0 {
				return fmt.Errorf("not everything could be cleaned up:\n  %s", strings.Join(problems, "\n  "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&via, "via", "", "which relay, if it has several")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask")
	return cmd
}

// relayEndpoints is where a machine can find the relay — its routes first
// (typically the public way in), then its own address — as host:port,
// plus known_hosts lines for each, built from the keys you already trust
// for the relay. Routes that need a jump are left out: the tunnel connects
// directly.
func relayEndpoints(cfg *config.Config, rh config.Host) ([]string, []string, error) {
	cands := append(append([]config.Route{}, rh.Routes...), rh.Candidates()[0])
	var keys []string
	seen := map[string]bool{}
	type ep struct {
		addr string
		port int
	}
	var eps []ep
	for _, rt := range cands {
		rt := rt
		r := cfg.ResolveVia(rh, &rt)
		if len(r.ProxyJump) > 0 || r.ProxyJumpRaw != "" {
			continue
		}
		port := r.Port
		if port == 0 {
			port = 22
		}
		key := net.JoinHostPort(r.Address, strconv.Itoa(port))
		if seen[key] {
			continue
		}
		seen[key] = true
		eps = append(eps, ep{r.Address, port})
		ks, err := route.TrustedKeys(r.Address, port)
		if err != nil {
			return nil, nil, err
		}
		if keys, err = mergeHostKeys(keys, ks); err != nil {
			return nil, nil, fmt.Errorf("%s at %s: %w: sort that out first (it may not be the same machine)", rh.Name, key, err)
		}
	}
	if len(eps) == 0 {
		return nil, nil, fmt.Errorf("%s has no address a machine could connect to directly", rh.Name)
	}
	if len(keys) == 0 {
		return nil, nil, fmt.Errorf("you don't have %s's host key yet: connect to it once (meterm connect %s), then try again", rh.Name, rh.Name)
	}
	var addrs, known []string
	for _, e := range eps {
		addrs = append(addrs, net.JoinHostPort(e.addr, strconv.Itoa(e.port)))
		for _, k := range keys {
			known = append(known, route.KnownName(e.addr, e.port)+" "+k)
		}
	}
	return addrs, known, nil
}

// mergeHostKeys adds more of one machine's host keys ("type base64") to
// have. A machine has one key per type, so the same type with a different
// key means two different machines; one address knowing more types than
// another (ssh's UpdateHostKeys learns them over time) is normal.
func mergeHostKeys(have, more []string) ([]string, error) {
	byType := map[string]string{}
	for _, k := range have {
		t, _, _ := strings.Cut(k, " ")
		byType[t] = k
	}
	for _, k := range more {
		t, _, _ := strings.Cut(k, " ")
		if old, ok := byType[t]; ok {
			if old != k {
				return nil, fmt.Errorf("your known_hosts has a different %s host key here than at its other addresses", t)
			}
			continue
		}
		byType[t] = k
		have = append(have, k)
	}
	return have, nil
}

func setRoute(h *config.Host, rt config.Route) {
	for i := range h.Routes {
		if h.Routes[i].Name == rt.Name {
			h.Routes[i] = rt
			return
		}
	}
	h.Routes = append(h.Routes, rt)
}

func removeRoute(h *config.Host, name string) {
	for i := range h.Routes {
		if h.Routes[i].Name == name {
			h.Routes = append(h.Routes[:i], h.Routes[i+1:]...)
			return
		}
	}
}

// waitListening polls the relay until port listens, printing a dot a try.
func waitListening(cfg *config.Config, rh config.Host, port int, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		out, err := runScript(ctx, cfg.Resolve(rh), relay.ListeningScript)
		cancel()
		if err == nil && relay.ParsePorts(out)[port] {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		fmt.Print(".")
		time.Sleep(3 * time.Second)
	}
}

func runScriptTimeout(cfg *config.Config, h config.Host, script string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return runScript(ctx, cfg.Resolve(h), script)
}

// scriptResult turns a root script's run into an error naming the host.
func scriptResult(host, out string, err error) error {
	if msg := relay.Failure(out); msg != "" {
		return fmt.Errorf("%s: %s", host, msg)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", host, err)
	}
	if !relay.OK(out) {
		return fmt.Errorf("%s: the script didn't finish:\n%s", host, strings.TrimSpace(out))
	}
	return nil
}

// runAsRoot runs script as root on h. With passwordless sudo it runs
// unattended; otherwise ssh gets a terminal so sudo can ask for the
// password here. Its output is returned; with a terminal, everything but
// meterm's own markers is shown as it comes (that's where the prompt is).
func runAsRoot(cfg *config.Config, h config.Host, script string) (string, error) {
	r := cfg.Resolve(h)
	if r.Err != nil {
		return "", r.Err
	}
	inner := "printf %s " + base64.StdEncoding.EncodeToString([]byte(script)) + " | base64 -d | sh"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := runScript(ctx, r, "sudo -n true"); err == nil {
		return runScript(ctx, r, "sudo -n sh -c "+shQuote(inner))
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("needs sudo with a password: run this in a terminal")
	}
	prompt := "[sudo] password for %u on " + h.Name + ": "
	args := append([]string{"-t"}, sshrun.BuildArgs(r, "sudo -p "+shQuote(prompt)+" sh -c "+shQuote(inner))...)
	c := exec.Command("ssh", args...)
	var buf bytes.Buffer
	c.Stdin = os.Stdin
	c.Stdout = io.MultiWriter(&buf, &markerFilter{w: os.Stdout})
	c.Stderr = os.Stderr
	err := c.Run()
	return strings.ReplaceAll(buf.String(), "\r", ""), err
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// markerFilter passes output through except whole lines starting with
// "METERM-", which are for meterm, not the person.
type markerFilter struct {
	w    io.Writer
	line []byte
}

func (f *markerFilter) Write(p []byte) (int, error) {
	for _, b := range p {
		f.line = append(f.line, b)
		if b == '\n' {
			if !bytes.HasPrefix(bytes.TrimLeft(f.line, "\r"), []byte("METERM-")) {
				f.w.Write(f.line)
			}
			f.line = f.line[:0]
			continue
		}
		// A partial line that can't be a marker (a password prompt) shows now.
		trimmed := bytes.TrimLeft(f.line, "\r")
		if !bytes.HasPrefix([]byte("METERM-"), trimmed) && !bytes.HasPrefix(trimmed, []byte("METERM-")) {
			f.w.Write(f.line)
			f.line = f.line[:0]
		}
	}
	return len(p), nil
}

func confirm(q string) bool {
	fmt.Printf("%s [y/N] ", q)
	resp, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	r := strings.ToLower(strings.TrimSpace(resp))
	return r == "y" || r == "yes"
}
