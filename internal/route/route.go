// Package route picks, at connect time, which of a host's routes to use:
// its own address, or one of the alternatives saved with `meterm route add`
// (a public port forward next to a VPN address, another jump host, …).
//
// It checks each candidate's first hop — the host itself, or the first jump
// host on the way — for an SSH server's greeting, in order, and uses the first
// that gives one. Nothing past the first hop can be checked from here without
// logging in; a route whose first hop answers but whose target doesn't
// still fails, as ssh would. The route that worked last is tried first next
// time (remembered in a small file), so the usual case costs one connect.
package route

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"meterm/internal/config"
	"meterm/internal/safefile"
)

// DefaultTimeout is how long one candidate gets to connect and greet.
const DefaultTimeout = 2 * time.Second

// Picker chooses routes for one config. Use Install to hook it in.
type Picker struct {
	cfg       *config.Config
	Timeout   time.Duration
	Dial      func(ctx context.Context, addr string) error // tests replace it
	statePath string                                       // "" = don't remember
	Avoid     *Avoid                                       // routes not to use; nil = none

	mu      sync.Mutex
	chosen  map[string]Choice // this process's decisions
	last    map[string]string // route that worked last time, by host
	carried map[string]bool   // host keys already carried, by host and route
}

// Choice is the decision for one host.
type Choice struct {
	Route string  // name; "default" is the host's own address
	Tried []Probe // in the order tried
}

// Probe is one candidate's check.
type Probe struct {
	Route string `json:"route"`
	Hop   string `json:"hop"` // the address dialled
	Err   string `json:"error,omitempty"`
}

// Install hooks a Picker into cfg, remembering choices in statePath ("" to
// keep them in memory only), and returns it.
func Install(cfg *config.Config, statePath string) *Picker {
	p := &Picker{cfg: cfg, Timeout: DefaultTimeout, Dial: dialSSH, statePath: statePath,
		chosen: map[string]Choice{}, carried: map[string]bool{}}
	p.last = loadLast(statePath)
	cfg.SetRoutePicker(p.pick)
	return p
}

// NewLook is a Picker for looking without leaving a mark: it reads which route worked
// last, and through Look it writes neither that file nor known_hosts. A route whose
// host key isn't trusted yet stays that way, and ssh says so. It isn't hooked into
// cfg: each caller gets a picker from Look under its own deadline.
func NewLook(cfg *config.Config, statePath string) *Picker {
	return &Picker{cfg: cfg, Timeout: DefaultTimeout, Dial: dialSSH, last: loadLast(statePath)}
}

// Look is a picker that goes no further than ctx, for Config.WithRoutePicker. Several
// can run at once, since it only reads p.
func (p *Picker) Look(ctx context.Context) config.RoutePicker {
	return func(h config.Host) (config.Route, bool) {
		c := p.check(ctx, h, p.last[h.Name])
		for _, rt := range h.Routes {
			if rt.Name == c.Route {
				return rt, true
			}
		}
		return config.Route{}, false
	}
}

// errNotSSH is a port that answers with something other than an SSH server.
var errNotSSH = errors.New("not an ssh server")

// dialSSH connects and waits for the server's greeting. An open port alone says
// little: on someone else's network it can be anything. A server may send other
// lines before the greeting, so a few are allowed for.
func dialSSH(ctx context.Context, addr string) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if t, ok := ctx.Deadline(); ok {
		_ = c.SetReadDeadline(t)
	}
	stop := context.AfterFunc(ctx, func() { c.Close() }) // a read doesn't watch ctx
	defer stop()
	r := bufio.NewReaderSize(c, 512)
	for range 8 {
		line, err := r.ReadSlice('\n')
		if strings.HasPrefix(string(line), "SSH-") {
			return nil
		}
		if err != nil {
			if isTimeout(err) || ctx.Err() != nil {
				return err
			}
			return errNotSSH
		}
	}
	return errNotSSH
}

// avoidFor is how long a route stays off the list after the wrong host key. A
// network that lied once may be gone by then, and a process can run for months.
const avoidFor = 5 * time.Minute

// Avoid is routes not to use for a while, by host, because ssh found the wrong
// host key at the end of them. Pickers share one, since a new one is made on
// every reload.
type Avoid struct {
	mu     sync.Mutex
	routes map[string]map[string]time.Time // until when
	now    func() time.Time                // tests replace it
}

// NewAvoid is an empty Avoid.
func NewAvoid() *Avoid {
	return &Avoid{routes: map[string]map[string]time.Time{}, now: time.Now}
}

// Mark adds h's route to the list and reports whether h has another one left
// to try. When it has none, nothing is added: that was the last way in.
func (a *Avoid) Mark(h config.Host, route string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	left := 0
	for _, rt := range h.Candidates() {
		if rt.Name != route && !a.avoiding(h.Name, rt.Name) {
			left++
		}
	}
	if left == 0 {
		return false
	}
	if a.routes[h.Name] == nil {
		a.routes[h.Name] = map[string]time.Time{}
	}
	a.routes[h.Name][route] = a.now().Add(avoidFor)
	return true
}

// Clear forgets the list: the machine woke up or the network changed, so what
// was learned about the old one no longer applies.
func (a *Avoid) Clear() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.routes = map[string]map[string]time.Time{}
}

func (a *Avoid) has(host, route string) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.avoiding(host, route)
}

func (a *Avoid) avoiding(host, route string) bool {
	until, ok := a.routes[host][route]
	return ok && a.now().Before(until)
}

func (p *Picker) pick(h config.Host) (config.Route, bool) {
	c := p.Choose(h)
	for _, rt := range h.Routes {
		if rt.Name == c.Route {
			p.carryOnce(h, rt)
			return rt, true
		}
	}
	return config.Route{}, false
}

func (p *Picker) carryOnce(h config.Host, rt config.Route) {
	p.mu.Lock()
	key := h.Name + "\x00" + rt.Name
	done := p.carried[key]
	p.carried[key] = true
	p.mu.Unlock()
	if done {
		return
	}
	if msg, err := p.carryHostKey(h, rt); err != nil {
		fmt.Fprintf(os.Stderr, "meterm: %s via route %q: couldn't reuse its host key: %v\n", h.Name, rt.Name, err)
	} else if msg != "" && os.Getenv("METERM_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "route: %s\n", msg)
	}
}

// Choose decides (once per process) which route reaches h.
func (p *Picker) Choose(h config.Host) Choice {
	p.mu.Lock()
	if c, ok := p.chosen[h.Name]; ok {
		p.mu.Unlock()
		return c
	}
	last := p.last[h.Name]
	p.mu.Unlock()

	c := p.Check(h, last)
	p.mu.Lock()
	p.chosen[h.Name] = c
	if p.last[h.Name] != c.Route {
		p.last[h.Name] = c.Route
		saveLast(p.statePath, p.last)
	}
	p.mu.Unlock()
	return c
}

// Check probes h's routes — first the one named prefer, then the rest in
// saved order, leaving out those to avoid — stopping at the first that answers.
// When none does, the choice is the host's own address unless that's to be
// avoided, so errors read as they always have.
func (p *Picker) Check(h config.Host, prefer string) Choice {
	return p.check(context.Background(), h, prefer)
}

// check is Check, stopping when ctx ends.
func (p *Picker) check(parent context.Context, h config.Host, prefer string) Choice {
	cands := h.Candidates()
	for i, rt := range cands {
		if rt.Name == prefer && i > 0 {
			cands = append([]config.Route{rt}, append(cands[:i:i], cands[i+1:]...)...)
			break
		}
	}
	var c Choice
	for _, rt := range cands {
		rt := rt
		if p.Avoid.has(h.Name, rt.Name) {
			continue
		}
		if parent.Err() != nil {
			break
		}
		hop := FirstHop(p.cfg.ResolveVia(h, &rt))
		pr := Probe{Route: rt.Name, Hop: hop}
		ctx, cancel := context.WithTimeout(parent, p.Timeout)
		err := p.Dial(ctx, hop)
		cancel()
		if err != nil {
			pr.Err = shortErr(err)
		}
		c.Tried = append(c.Tried, pr)
		if err == nil {
			c.Route = rt.Name
			return c
		}
	}
	for _, rt := range h.Candidates() {
		if !p.Avoid.has(h.Name, rt.Name) {
			c.Route = rt.Name
			break
		}
	}
	return c
}

// CheckAll probes every route of h, without stopping at the first that
// answers — for `meterm route check`.
func (p *Picker) CheckAll(h config.Host) []Probe {
	var out []Probe
	for _, rt := range h.Candidates() {
		rt := rt
		hop := FirstHop(p.cfg.ResolveVia(h, &rt))
		pr := Probe{Route: rt.Name, Hop: hop}
		ctx, cancel := context.WithTimeout(context.Background(), p.Timeout)
		if err := p.Dial(ctx, hop); err != nil {
			pr.Err = shortErr(err)
		}
		cancel()
		out = append(out, pr)
	}
	return out
}

// FirstHop is the address ssh connects to first for r: its first jump
// host, or r itself.
func FirstHop(r config.Resolved) string {
	if len(r.ProxyJump) > 0 {
		j := r.ProxyJump[0]
		return net.JoinHostPort(j.Address, strconv.Itoa(port(j.Port)))
	}
	if r.ProxyJumpRaw != "" {
		return rawHop(strings.Split(r.ProxyJumpRaw, ",")[0])
	}
	return net.JoinHostPort(r.Address, strconv.Itoa(port(r.Port)))
}

// rawHop reads a literal jump spec, [user@]host[:port] (IPv6 in brackets).
func rawHop(spec string) string {
	spec = strings.TrimSpace(spec)
	if i := strings.LastIndex(spec, "@"); i >= 0 {
		spec = spec[i+1:]
	}
	if h, p, err := net.SplitHostPort(spec); err == nil {
		return net.JoinHostPort(h, p)
	}
	return net.JoinHostPort(strings.Trim(spec, "[]"), "22")
}

func port(p int) int {
	if p == 0 {
		return 22
	}
	return p
}

func shortErr(err error) string {
	switch {
	case err == nil:
		return ""
	case isTimeout(err):
		return "no answer"
	case errors.Is(err, errNotSSH):
		return "not ssh"
	case strings.Contains(err.Error(), "refused"):
		return "refused"
	case strings.Contains(err.Error(), "no route"), strings.Contains(err.Error(), "unreachable"):
		return "unreachable"
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	return msg
}

func isTimeout(err error) bool {
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "deadline exceeded") || strings.Contains(err.Error(), "i/o timeout")
}

func loadLast(path string) map[string]string {
	m := map[string]string{}
	if path == "" {
		return m
	}
	if b, err := safefile.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func saveLast(path string, m map[string]string) {
	if path == "" {
		return
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = safefile.WriteFile(path, b)
}
