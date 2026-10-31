package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"meterm/internal/config"
	"meterm/internal/relay"
	"meterm/internal/sshrun"
	"meterm/internal/status"
	"meterm/internal/termsafe"
)

func newTunnelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Save port-forwards as named tunnels and open them on demand",
		Long: "A tunnel is a port-forward saved on a host under a name — a database, an\n" +
			"admin UI, a SOCKS proxy — opened when you need it instead of on every\n" +
			"connect. It goes through the host's jump hosts like everything else, and\n" +
			"team hosts share their tunnels with the team.\n\n" +
			"Examples:\n" +
			"  meterm tunnel add prod-db pg -L 5432:localhost:5432\n" +
			"  meterm tunnel add bastion socks -D 1080\n" +
			"  meterm tunnel up prod-db/pg            # Ctrl-C to close\n" +
			"  meterm tunnel up prod-db web1 --keep   # every tunnel on both, reconnecting\n" +
			"  meterm tunnel enable prod-db/pg        # in the background from now on (Linux, macOS)",
	}
	cmd.AddCommand(newTunnelAddCmd(), newTunnelListCmd(), newTunnelRmCmd(), newTunnelUpCmd(),
		newTunnelEnableCmd(), newTunnelDisableCmd())
	return cmd
}

func newTunnelAddCmd() *cobra.Command {
	var local, remote, note string
	var dynamic int
	cmd := &cobra.Command{
		Use:   "add <host> <name> (-L port:host:port | -R port:host:port | -D port)",
		Short: "Save a tunnel on a host",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			fw, err := tunnelFromFlags(args[1], local, remote, dynamic)
			if err != nil {
				return err
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			h, idx := cfg.Find(args[0])
			if idx < 0 {
				return fmt.Errorf("no such host: %s (use `meterm list` to see saved hosts)", args[0])
			}
			fw.Note = strings.TrimSpace(note)
			if err := config.CheckText("--note", fw.Note); err != nil || len(fw.Note) > 200 {
				return fmt.Errorf("--note: plain text, at most 200 characters")
			}
			for _, f := range h.Forwards {
				if f.Name == fw.Name {
					if fw.Note == "" {
						fw.Note = f.Note // re-adding without --note keeps the old one
					}
				}
			}
			// What's at the other end, so a typo'd port shows now, and so
			// there's something to go by when choosing a note.
			found := probeTunnelTarget(cfg, *h, fw)
			if found.warning != "" {
				fmt.Fprintln(os.Stderr, "meterm: "+found.warning)
			}
			if fw.Note == "" {
				if h.Team != "" {
					hint := "e.g. --note \"Billing API (prod)\""
					if found.what != "" {
						hint = fmt.Sprintf("it reaches %s; e.g. --note \"%s\"", found.what, found.what)
					}
					return fmt.Errorf("%s is a team host, so its tunnels are shared: say what this one is with --note (%s)", h.Name, hint)
				}
				fw.Note = found.what // a personal tunnel: what we found is note enough
			}
			return saveTunnel(cfg, h.Name, fw)
		},
	}
	cmd.Flags().StringVarP(&local, "local", "L", "", "local forward LOCALPORT:HOST:PORT (HOST as seen from the server)")
	cmd.Flags().StringVarP(&remote, "remote", "R", "", "remote forward REMOTEPORT:HOST:PORT (HOST as seen from this machine)")
	cmd.Flags().IntVarP(&dynamic, "dynamic", "D", 0, "SOCKS proxy on this local port")
	cmd.Flags().StringVar(&note, "note", "", "what the tunnel reaches, e.g. \"Billing API (prod)\" (required on team hosts)")
	return cmd
}

// tunnelTarget is what probeTunnelTarget found at a tunnel's far end.
type tunnelTarget struct {
	what    string // e.g. "app.billing.prod.service", "container nginx_proxy"
	warning string // nothing listening, unreachable, couldn't check
}

// probeTunnelTarget looks, read-only, at what a -L tunnel reaches: the
// service listening on the port when it's the server's own, or just
// whether it answers when it's another host as seen from the server.
func probeTunnelTarget(cfg *config.Config, h config.Host, fw config.Forward) tunnelTarget {
	if fw.Type != "L" {
		return tunnelTarget{}
	}
	if config.CheckAddress(fw.RemoteHost) != nil {
		return tunnelTarget{warning: fmt.Sprintf("%s isn't a plain host name; not checking it", fw.RemoteHost)}
	}
	p := strconv.Itoa(fw.RemotePort)
	var script string
	switch fw.RemoteHost {
	case "localhost", "127.0.0.1", "::1":
		script = `p=` + p + `
l=$( (sudo -n ss -Hltnp "sport = :$p" 2>/dev/null || ss -Hltnp "sport = :$p" 2>/dev/null) | head -1)
[ -n "$l" ] || { echo "METERM-LISTEN: none"; exit 0; }
echo "METERM-LISTEN: yes"
pid=$(echo "$l" | grep -o 'pid=[0-9]*' | head -1 | cut -d= -f2)
[ -n "$pid" ] || exit 0
c=$(cat /proc/$pid/comm 2>/dev/null)
if [ "$c" = docker-proxy ]; then
  n=$( (docker ps --filter "publish=$p" --format '{{.Names}}' 2>/dev/null || sudo -n docker ps --filter "publish=$p" --format '{{.Names}}' 2>/dev/null) | head -1)
  [ -n "$n" ] && { echo "METERM-WHAT: container $n"; exit 0; }
fi
u=$(sed -n 's#.*/\([^/]*\.service\).*#\1#p' /proc/$pid/cgroup 2>/dev/null | head -1)
case "$u" in ''|user@*|session-*) echo "METERM-WHAT: $c" ;; *) echo "METERM-WHAT: $u" ;; esac
`
	default:
		script = `if timeout 4 bash -c "</dev/tcp/` + fw.RemoteHost + `/` + p + `" 2>/dev/null; then echo "METERM-REACH: yes"; else echo "METERM-REACH: no"; fi
`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := runScript(ctx, cfg.Resolve(h), script)
	if err != nil {
		return tunnelTarget{warning: fmt.Sprintf("couldn't check what %s:%d is on %s (%v); saved anyway", fw.RemoteHost, fw.RemotePort, h.Name, err)}
	}
	if relay.Marker(out, "LISTEN") == "none" {
		return tunnelTarget{warning: fmt.Sprintf("nothing on %s listens on port %d right now: is that the right port? (saved anyway)", h.Name, fw.RemotePort)}
	}
	if relay.Marker(out, "REACH") == "no" {
		return tunnelTarget{warning: fmt.Sprintf("%s can't reach %s:%d right now: is that right? (saved anyway)", h.Name, fw.RemoteHost, fw.RemotePort)}
	}
	what := relay.Marker(out, "WHAT")
	if config.CheckText("", what) != nil || len(what) > 100 {
		what = ""
	}
	return tunnelTarget{what: what}
}

func saveTunnel(cfg *config.Config, name string, fw config.Forward) error {
	var h *config.Host
	verb := "added"
	err := cfg.Update(func(c *config.Config) error {
		var idx int
		h, idx = c.Find(name)
		if idx < 0 {
			return fmt.Errorf("no such host: %s", name)
		}
		for i, f := range h.Forwards {
			if f.Name == fw.Name {
				h.Forwards[i] = fw // re-adding a name replaces it
				verb = "updated"
				return nil
			}
		}
		h.Forwards = append(h.Forwards, fw)
		return nil
	})
	if err != nil {
		return err
	}
	flag, spec := sshrun.ForwardSpec(fw)
	fmt.Printf("Tunnel %s/%s %s (%s %s).\n", h.Name, fw.Name, verb, flag, spec)
	if fw.Note != "" {
		fmt.Printf("  %s\n", fw.Note)
	}
	if h.Team != "" {
		fmt.Println("This is a team host: `meterm team push` shares the tunnel with the team.")
	}
	return nil
}

// tunnelFromFlags builds a named forward from exactly one of -L, -R, -D.
func tunnelFromFlags(name, local, remote string, dynamic int) (config.Forward, error) {
	if name == "" || strings.ContainsAny(name, "/ \t") {
		return config.Forward{}, fmt.Errorf("tunnel name %q: use letters, digits, - or _", name)
	}
	set := 0
	for _, on := range []bool{local != "", remote != "", dynamic != 0} {
		if on {
			set++
		}
	}
	if set != 1 {
		return config.Forward{}, fmt.Errorf("give exactly one of -L, -R or -D")
	}
	if dynamic != 0 {
		if dynamic < 1 || dynamic > 65535 {
			return config.Forward{}, fmt.Errorf("-D %d: not a port", dynamic)
		}
		return config.Forward{Name: name, Type: "D", LocalPort: dynamic}, nil
	}
	typ, spec := "L", local
	if remote != "" {
		typ, spec = "R", remote
	}
	parts := strings.Split(spec, ":")
	if len(parts) != 3 {
		return config.Forward{}, fmt.Errorf("-%s %s: want PORT:HOST:PORT", typ, spec)
	}
	p1, err1 := strconv.Atoi(parts[0])
	p2, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || p1 < 1 || p1 > 65535 || p2 < 1 || p2 > 65535 || parts[1] == "" {
		return config.Forward{}, fmt.Errorf("-%s %s: want PORT:HOST:PORT", typ, spec)
	}
	return config.Forward{Name: name, Type: typ, LocalPort: p1, RemoteHost: parts[1], RemotePort: p2}, nil
}

func newTunnelListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list [host]",
		Aliases: []string{"ls"},
		Short:   "List saved tunnels",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			n := 0
			for _, h := range cfg.SortedHosts() {
				if len(args) == 1 && h.Name != args[0] {
					continue
				}
				for _, f := range h.Forwards {
					if f.Name == "" {
						continue
					}
					if n == 0 {
						fmt.Fprintln(tw, "TUNNEL\tFORWARD\tNOTE")
					}
					note := f.Note
					if note == "" {
						note = "(no note)"
					}
					if cfg.IsProduction(h) || f.MentionsProduction() {
						note = "⚠ " + note
					}
					fmt.Fprintf(tw, "%s/%s\t%s\t%s\n", h.Name, f.Name, describeTunnel(f), note)
					n++
				}
			}
			tw.Flush()
			if n == 0 {
				fmt.Println("No tunnels saved. Add one with `meterm tunnel add <host> <name> -L 5432:localhost:5432`.")
			}
			return nil
		},
	}
}

func describeTunnel(f config.Forward) string {
	switch f.Type {
	case "L":
		return fmt.Sprintf("localhost:%d → %s:%d", f.LocalPort, f.RemoteHost, f.RemotePort)
	case "R":
		return fmt.Sprintf("server port %d → %s:%d here", f.LocalPort, f.RemoteHost, f.RemotePort)
	case "D":
		return fmt.Sprintf("SOCKS proxy on localhost:%d", f.LocalPort)
	}
	return ""
}

func newTunnelRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <host> <name>",
		Short: "Delete a saved tunnel",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			err = cfg.Update(func(c *config.Config) error {
				h, idx := c.Find(args[0])
				if idx < 0 {
					return fmt.Errorf("no such host: %s", args[0])
				}
				kept := h.Forwards[:0:0]
				for _, f := range h.Forwards {
					if f.Name != args[1] {
						kept = append(kept, f)
					}
				}
				if len(kept) == len(h.Forwards) {
					return fmt.Errorf("%s has no tunnel named %s", args[0], args[1])
				}
				h.Forwards = kept
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Printf("Tunnel %s/%s deleted.\n", args[0], args[1])
			return nil
		},
	}
}

// tunnelSet is the tunnels to open on one host, in one ssh process.
type tunnelSet struct {
	host    config.Host
	forward []config.Forward
	// onRoute hears which route each attempt is about to go by.
	onRoute func(string)
	// used is how the last attempt went, for tunnelNextRoute.
	used *config.Resolved
}

// pickTunnels resolves "host" (all its tunnels) and "host/name" arguments.
func pickTunnels(cfg *config.Config, args []string) ([]tunnelSet, error) {
	byHost := map[string]*tunnelSet{}
	var order []string
	for _, a := range args {
		hostName, name, _ := strings.Cut(a, "/")
		h, idx, err := cfg.Target(hostName)
		if err != nil {
			return nil, err
		}
		if idx < 0 {
			return nil, fmt.Errorf("no such host: %s", hostName)
		}
		set, ok := byHost[h.Name]
		if !ok {
			set = &tunnelSet{host: *h}
			byHost[h.Name] = set
			order = append(order, h.Name)
		}
		found := false
		for _, f := range h.Forwards {
			if f.Name == "" || (name != "" && f.Name != name) {
				continue
			}
			found = true
			dup := false
			for _, have := range set.forward {
				dup = dup || have.Name == f.Name
			}
			if !dup {
				set.forward = append(set.forward, f)
			}
		}
		if !found {
			if name != "" {
				return nil, fmt.Errorf("%s has no tunnel named %s (see `meterm tunnel list %s`)", h.Name, name, h.Name)
			}
			return nil, fmt.Errorf("%s has no tunnels (add one with `meterm tunnel add %s <name> -L ...`)", h.Name, h.Name)
		}
	}
	sort.Strings(order)
	out := make([]tunnelSet, 0, len(order))
	for _, n := range order {
		out = append(out, *byHost[n])
	}
	return out, nil
}

// tunnelArgs is the ssh command line that holds set's forwards open and
// nothing else: no shell, exit at once if a port can't be bound rather than
// sit there looking connected, and no shared ControlMaster, which would take
// the forwards over and let this ssh exit. options are added to ssh's own.
func tunnelArgs(cfg *config.Config, set tunnelSet, options ...string) []string {
	r := cfg.Resolve(set.host)
	r.Forwards = nil
	for _, f := range set.forward {
		f.Name = "" // BuildArgs skips named forwards
		r.Forwards = append(r.Forwards, f)
	}
	r.ExtraArgs = append(append([]string(nil), r.ExtraArgs...),
		"-N", "-o", "ControlPath=none", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=3")
	r.ExtraArgs = append(r.ExtraArgs, options...)
	return sshrun.BuildArgs(r)
}

// tunnelGaveUp is the exit code of `tunnel up --service` when going on is pointless:
// the tunnel is gone or its host is unsafe. The unit's RestartPreventExitStatus
// names it, so systemd shows the failure instead of restarting into it forever.
const tunnelGaveUp = 78

// tunnelSignalled is the exit code of `tunnel up --service` after SIGTERM: not success,
// so that whatever killed it from outside (killall, a memory cleaner) doesn't end the
// tunnel for good. A deliberate stop isn't affected. launchd doesn't restart what
// bootout stopped, and systemd.service(5) says the same of a stop it was asked to do.
const tunnelSignalled = 128 + 15

// tunnelVerdict is the one thing that may stop a service for good: a decision read from
// a config that parsed and has hosts in it, that this tunnel is gone or its host unsafe.
// A config that can't be read, or has nothing in it yet, can be fixed; and nothing ssh
// printed counts, since that includes what the server sends before authentication,
// which mustn't be able to end a tunnel.
type tunnelVerdict struct{ error }

// errTunnelSignalled is how runTunnelUp in service mode says a signal ended it.
var errTunnelSignalled = errors.New("stopped by a signal")

// tunnelServiceExit is the exit code of `tunnel up --service` for how it ended.
func tunnelServiceExit(err error, gaveUp int) int {
	var verdict tunnelVerdict
	switch {
	case err == nil:
		return 0
	case errors.As(err, &verdict):
		return gaveUp
	case errors.Is(err, errTunnelSignalled):
		return tunnelSignalled
	}
	return 1
}

// lockedWriter lets ssh's two streams and holdTunnel's own messages share a writer
// that isn't safe for that.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func newTunnelUpCmd() *cobra.Command {
	var keep, service bool
	var gaveUpExit int
	cmd := &cobra.Command{
		Use:   "up <host>[/<tunnel>]...",
		Short: "Open saved tunnels until Ctrl-C",
		Long: "Opens the named tunnels (or every tunnel on a host given without /name),\n" +
			"one ssh connection per host, and holds them open until Ctrl-C. With --keep a\n" +
			"dropped connection is retried with backoff instead of ending the command.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			err := runTunnelUp(args, keep, service)
			if service && err != nil {
				if !errors.Is(err, errTunnelSignalled) {
					fmt.Fprintf(os.Stderr, "meterm: %s\n", termsafe.Lines(err.Error()))
				}
				finish(tunnelServiceExit(err, gaveUpExit))
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&keep, "keep", false, "reconnect when the connection drops")
	// For the units of `tunnel enable`: with nobody there to act on a refused
	// key, wait for it to be unlocked rather than exit and be restarted.
	cmd.Flags().BoolVar(&service, "service", false, "keep retrying on failures that need a person")
	_ = cmd.Flags().MarkHidden("service")
	// launchd can't be told to leave one exit code alone as systemd can, only every
	// code but 0: see renderLaunchAgent.
	cmd.Flags().IntVar(&gaveUpExit, "gave-up-exit", tunnelGaveUp, "exit code when --service gives up")
	_ = cmd.Flags().MarkHidden("gave-up-exit")
	return cmd
}

func runTunnelUp(args []string, keep, service bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	sets, err := pickTunnels(cfg, args)
	if err != nil {
		if len(cfg.Hosts) > 0 {
			return tunnelVerdict{err}
		}
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	// Before anything is recorded: a service restarted into a host that has turned
	// unsafe shouldn't add to the permanent log on every try.
	for _, s := range sets {
		if err := cfg.Resolve(s.host).Err; err != nil {
			return tunnelVerdict{err}
		}
	}
	for _, s := range sets {
		var names []string
		for _, f := range s.forward {
			names = append(names, f.Name)
			fmt.Printf("%s/%s: %s\n", termsafe.Text(s.host.Name), termsafe.Text(f.Name), termsafe.Text(describeTunnel(f)))
		}
		recordActivity(cfg, s.host, "tunnel", strings.Join(names, ","))
	}
	kickAutosync(false, true)
	for _, set := range sets {
		for _, f := range set.forward {
			if cfg.IsProduction(set.host) || f.MentionsProduction() {
				fmt.Fprintf(os.Stderr, "⚠ %s/%s reaches PRODUCTION: %s\n", termsafe.Text(set.host.Name), termsafe.Text(f.Name), termsafe.Text(f.Note))
			}
		}
	}
	fmt.Println("Holding tunnels open — Ctrl-C to close.")

	reg := status.Open()
	defer reg.Close()

	var wg sync.WaitGroup
	errs := make([]error, len(sets))
	for i, s := range sets {
		wg.Add(1)
		go func(i int, s tunnelSet) {
			defer wg.Done()
			hold := tunnelHold{keep: keep, stopOnFailure: !service, service: service, nextRoute: tunnelNextRoute(&s)}
			stop := hold.track(reg, &s)
			defer stop()
			var log io.Writer = os.Stderr
			if service {
				// It ends up in a log file someone will cat, and ssh's stderr holds
				// what the server sends before authentication.
				log = &lockedWriter{w: termsafe.New(os.Stderr)}
			}
			errs[i] = holdTunnel(ctx, s.host.Name, func() ([]string, error) { return freshTunnelArgs(cfg, s, hold.marker.options()...) }, hold, log)
		}(i, s)
	}
	wg.Wait()
	if ctx.Err() != nil {
		fmt.Println("\nTunnels closed.")
		if service {
			return errTunnelSignalled
		}
		return nil
	}
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// freshTunnelArgs is tunnelArgs from the config as it is now: another attempt
// goes by the routes that answer now, and by the host and forwards as they're
// saved now. A host or tunnel that has since been removed (a team pull, say) isn't
// connected to again, and neither is a host that has turned unsafe. A file that
// can't be read for the moment changes nothing.
func freshTunnelArgs(cfg *config.Config, s tunnelSet, options ...string) ([]string, error) {
	if fresh, err := loadConfig(); err == nil {
		if len(fresh.Hosts) == 0 {
			// Not a decision about this host: more likely a file on its way somewhere.
			return nil, fmt.Errorf("the host file has no hosts in it, so %s's tunnel is closed for now", s.host.Name)
		}
		cfg = fresh
		h, i := fresh.Find(s.host.Name)
		if i < 0 {
			return nil, tunnelVerdict{fmt.Errorf("%s isn't a saved host any more, so its tunnel is closed", s.host.Name)}
		}
		user := s.host.User
		s.host = *h
		s.host.User = user
		var kept []config.Forward
		for _, was := range s.forward {
			if f, ok := findForward(*h, was.Name); ok {
				kept = append(kept, f)
			}
		}
		if len(kept) == 0 {
			return nil, tunnelVerdict{fmt.Errorf("%s has no such tunnel any more, so it's closed", s.host.Name)}
		}
		s.forward = kept
	}
	r := cfg.Resolve(s.host)
	if r.Err != nil {
		return nil, tunnelVerdict{r.Err}
	}
	if s.used != nil {
		*s.used = r
	}
	if s.onRoute != nil {
		s.onRoute(routeOrDefault(r.Route))
	}
	return tunnelArgs(cfg, s, options...), nil
}

func findForward(h config.Host, name string) (config.Forward, bool) {
	for _, f := range h.Forwards {
		if f.Name == name {
			return f, true
		}
	}
	return config.Forward{}, false
}

// tunnelHold is how holdTunnel treats a drop.
type tunnelHold struct {
	keep bool // reconnect after a drop
	// stopOnFailure ends the hold on what won't pass by itself (a refused key,
	// a changed host key). Behind a session it's left off: the key may just be
	// locked, and the session is what the user is there for.
	stopOnFailure bool
	// service is a job with nobody there and all the time in the world. Nothing read
	// from ssh's stderr ends it: see tunnelVerdict.
	service bool
	// notice hears about tunnels that keep failing, once per run of failures,
	// and about their recovery. It's for a user who can't see log.
	notice func(string)
	// nextRoute moves off a route whose host key is wrong, and says what it left
	// and whether there's another. See avoidRoute.
	nextRoute func() (string, bool)
	// noPrompts is for an ssh running behind a session, which mustn't ask for
	// anything on its terminal. See the function of that name.
	noPrompts bool
	// state and marker tell whoever reads `meterm status` how this tunnel is
	// doing: see track. Either can be nil.
	state  *status.Handle
	marker *connectMarker
}

// track has hold report on s to reg, and s report the routes it uses. The ssh
// that holds a tunnel has no output to go by, so it leaves the same mark a session
// does; the caller adds marker's options to its ssh. stop cleans up after it.
func (hold *tunnelHold) track(reg *status.Registry, s *tunnelSet) (stop func()) {
	var names []string
	for _, f := range s.forward {
		names = append(names, f.Name)
	}
	hold.state = reg.Add(status.KindTunnel, s.host.Name, strings.Join(names, ","))
	if hold.state == nil {
		return func() {}
	}
	hold.marker = newConnectMarker()
	s.onRoute = hold.state.SetRoute
	return hold.marker.close
}

// tunnelNextRoute is tunnelHold.nextRoute for s: it moves off the route the last
// attempt went by, which looking again might not find.
func tunnelNextRoute(s *tunnelSet) func() (string, bool) {
	s.used = &config.Resolved{}
	return func() (string, bool) {
		cfg, err := loadConfig()
		if err != nil {
			return "", false
		}
		h, i := cfg.Find(s.host.Name)
		if i < 0 {
			return "", false
		}
		return avoidRoute(cfg, *h, *s.used)
	}
}

// tunnelFailNotice is how many attempts in a row have to fail before notice
// hears about it, so that one dropped connection stays in the log.
const tunnelFailNotice = 3

// tunnelSettled is how long a connection has to last to count as working again.
var tunnelSettled = time.Minute

// holdTunnel runs ssh until ctx ends; with keep it restarts ssh after a
// drop, backing off from 2s up to 30s (reset once a connection lasts a minute).
// args is asked again for each attempt; ssh's output and news of drops go to log.
func holdTunnel(ctx context.Context, host string, args func() ([]string, error), hold tunnelHold, log io.Writer) error {
	backoff := 2 * time.Second
	failures := 0
	var noticed atomic.Bool // notice was told it keeps failing, and not yet that it's back
	tail := &sshrun.Tail{Max: 16 << 10}
	var kicks <-chan string
	if hold.keep {
		kicks = watchKicks(ctx)
	}
	for {
		argv, err := args()
		if err != nil {
			return err
		}
		tail.Reset()
		started := time.Now()
		attempt, stopAttempt := kickable(ctx, kicks, nil)
		c := exec.CommandContext(attempt, "ssh", argv...)
		c.Stdout, c.Stderr = log, io.MultiWriter(log, tail)
		c.WaitDelay = 2 * time.Second
		if hold.noPrompts {
			noPrompts(c)
		}
		settled := time.AfterFunc(tunnelSettled, func() {
			if noticed.Swap(false) && hold.notice != nil {
				hold.notice(fmt.Sprintf("tunnel to %s is back", host))
			}
		})
		endWatch := func() {}
		if hold.state != nil {
			hold.marker.reset()
			endWatch = watchConnected(attempt, hold.marker, started, func() { hold.state.Set(status.Connected, "") })
		}
		err = c.Run()
		settled.Stop()
		endWatch()
		kicked := stopAttempt()
		if ctx.Err() != nil {
			return nil
		}
		if kicked != "" {
			hold.state.Set(status.Reconnecting, kicked)
			fmt.Fprintf(log, "%s: %s; reconnecting\n", host, kicked)
			backoff = 2 * time.Second
			continue
		}
		failure := sshrun.ClassifyFailure(tail.String())
		if !hold.keep {
			if err == nil {
				return nil
			}
			if failure != sshrun.FailureNone {
				return fmt.Errorf("%s: tunnel closed: %s", host, failure)
			}
			return fmt.Errorf("%s: tunnel closed: %w", host, err)
		}
		if failure == sshrun.FailureHostKey && hold.nextRoute != nil {
			if bad, ok := hold.nextRoute(); ok {
				fmt.Fprintf(log, "meterm: WARNING: %s has the wrong host key, so it may not be that machine; trying another route\n", bad)
				continue
			}
		}
		// What's refused now would be refused on every try. A locked key or a port in
		// use can pass by itself, so those wait where nobody's there to see the exit.
		if failure == sshrun.FailureConfig && !hold.service || hold.stopOnFailure && failure != sshrun.FailureNone {
			hold.state.Set(status.Failing, failure.String())
			return fmt.Errorf("%s: tunnel closed: %s", host, failure)
		}
		if time.Since(started) > tunnelSettled {
			backoff = 2 * time.Second
			failures = 0
		}
		failures++
		if failures == tunnelFailNotice && hold.notice != nil {
			hold.notice(fmt.Sprintf("tunnel to %s keeps failing: %s", host, failure))
			noticed.Store(true)
		}
		if failures >= tunnelFailNotice || failure != sshrun.FailureNone {
			hold.state.Set(status.Failing, failure.String())
		} else {
			hold.state.Set(status.Reconnecting, "connection lost")
		}
		fmt.Fprintf(log, "%s: tunnel dropped (%v); reconnecting in %s\n", host, err, backoff)
		select {
		case <-ctx.Done():
			return nil
		case why := <-kicks:
			fmt.Fprintf(log, "%s: %s; trying now\n", host, why)
			backoff = 2 * time.Second
		case <-time.After(backoff):
			backoff = min(backoff*2, 30*time.Second)
		}
	}
}
