// Package health checks whether saved machines are up before you try to
// connect: a TCP dial (Check), or the way connect would go (CheckHost): over
// the host's routes in connect's order, dialling each one's first hop — the
// host itself, or the first jump host on the way — and waiting for an SSH
// server's greeting. That's what connect's route choice checks too
// (internal/route); past a jump host nothing can be checked without
// logging in.
package health

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"time"

	"meterm/internal/config"
	"meterm/internal/route"
)

// DefaultTimeout is used when callers don't need a specific budget.
const DefaultTimeout = 2 * time.Second

// Check reports whether address:port accepts a TCP connection within
// timeout. It only proves the port is listening — not that SSH auth would
// succeed.
func Check(address string, port int, timeout time.Duration) bool {
	if port == 0 {
		port = 22
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", address, port), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// State is a host's answer.
type State int

const (
	Unknown State = iota // not checked yet
	Up
	Down
	NotChecked // connect wouldn't try it (an unsafe host): Reason says why
	// Unsure: the port took the connection but no SSH greeting came in
	// time. A multiplexer such as sslh waits for the client to speak first,
	// and a slow server may just be slow: it's neither up nor down.
	Unsure
)

// Result is one host's check: the state; the route connect would use ("" for
// the host's own way), the one that answered or, when none did, the one
// connect falls back to; the jump host that route goes through ("" for
// none), whose answer it is; why it didn't answer; and when it was checked.
type Result struct {
	State  State
	Route  string
	Via    string
	Reason string
	At     time.Time
}

// Dialer connects to addr (host:port) and says whether an SSH server is there.
type Dialer func(ctx context.Context, addr string) error

// CheckHost checks h the way connect would reach it: its routes in order,
// the one that worked last (prefer) first, stopping at the first whose first
// hop greets as an SSH server.
func CheckHost(cfg *config.Config, h config.Host, prefer string, dial Dialer, timeout time.Duration, now func() time.Time) Result {
	return PlanHost(cfg, h, map[string]string{h.Name: prefer}).Run(dial, timeout, now)
}

// Plan is what checking a host takes, worked out from the config up front,
// so the dialling can run on another goroutine without reading a config
// that may change meanwhile.
type Plan struct {
	Hops []Hop
	// Fallback is the hop connect uses when none answers: the host's own way.
	Fallback int
	// Unchecked says why nothing is dialled at all ("" when it is).
	Unchecked string
}

// Hop is one way to reach a host: the route's name ("" for its own way),
// the address its first hop is at, and the jump host that is, if any. When
// the jump host is a saved host with routes of its own, connect picks one of
// those for it as well, so the hop is that host's own plan (Jump).
type Hop struct {
	Route string
	Addr  string
	Via   string
	Jump  *Plan
}

// PlanHost lists h's routes in the order connect tries them (internal/route
// Picker.Check): the one that worked last (last, by host name) first, then
// the host's own way and the rest.
func PlanHost(cfg *config.Config, h config.Host, last map[string]string) Plan {
	return planHost(cfg, h, last, map[string]bool{})
}

func planHost(cfg *config.Config, h config.Host, last map[string]string, seen map[string]bool) Plan {
	seen[h.Name] = true
	cands := h.Candidates()
	for i, rt := range cands {
		if rt.Name == last[h.Name] && i > 0 {
			cands = append([]config.Route{rt}, append(cands[:i:i], cands[i+1:]...)...)
			break
		}
	}
	var p Plan
	for _, rt := range cands {
		rt := rt
		r := cfg.ResolveVia(h, &rt) // never the route picker: that would dial
		if r.Err != nil {
			return Plan{Unchecked: r.Err.Error()}
		}
		hop := Hop{Addr: route.FirstHop(r)}
		if rt.Name != "default" {
			hop.Route = rt.Name
		} else {
			p.Fallback = len(p.Hops)
		}
		switch {
		case len(r.ProxyJump) > 0:
			hop.Via = r.ProxyJump[0].Name
			if jh, i := cfg.Find(hop.Via); i >= 0 && len(jh.Routes) > 0 && !seen[jh.Name] {
				sub := planHost(cfg, *jh, last, seen)
				hop.Jump = &sub
			}
		case r.ProxyJumpRaw != "":
			hop.Via = strings.TrimSpace(strings.Split(r.ProxyJumpRaw, ",")[0])
		}
		p.Hops = append(p.Hops, hop)
	}
	return p
}

// Run dials the plan's hops in order, stopping at the first SSH server.
// When none answers, the result is the fallback hop's: that's where connect
// goes, and its error is the one a person will see.
func (p Plan) Run(dial Dialer, timeout time.Duration, now func() time.Time) Result {
	if p.Unchecked != "" || len(p.Hops) == 0 {
		return Result{State: NotChecked, Reason: p.Unchecked, At: now()}
	}
	why := make([]string, len(p.Hops))
	state := make([]State, len(p.Hops))
	for i, hop := range p.Hops {
		if hop.Jump != nil {
			r := hop.Jump.Run(dial, timeout, now)
			if r.State == Up {
				return Result{State: Up, Route: hop.Route, Via: hop.Via, At: now()}
			}
			why[i], state[i] = r.Reason, r.State
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := dial(ctx, hop.Addr)
		cancel()
		if err == nil {
			return Result{State: Up, Route: hop.Route, Via: hop.Via, At: now()}
		}
		why[i], state[i] = Why(err), Down
		if errors.Is(err, ErrNoGreeting) {
			why[i], state[i] = fmt.Sprintf("%s in %s", why[i], timeout), Unsure
		}
	}
	fb := p.Hops[p.Fallback]
	st := Down
	if state[p.Fallback] == Unsure {
		st = Unsure
	}
	return Result{State: st, Route: fb.Route, Via: fb.Via, Reason: why[p.Fallback], At: now()}
}

// ErrNoGreeting is a port that took the connection and then said nothing
// before the deadline.
var ErrNoGreeting = errors.New("port open, no SSH greeting")

// ErrNotSSH is a port that answers with something other than an SSH server.
var ErrNotSSH = errors.New("not an ssh server")

// Dial connects to addr and waits for an SSH server's greeting: an open port
// alone says little, on someone else's network it can be anything. A server
// may send a few lines before it.
func Dial(ctx context.Context, addr string) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if t, ok := ctx.Deadline(); ok {
		_ = c.SetReadDeadline(t)
	}
	r := bufio.NewReaderSize(c, 512)
	for n := range 8 {
		line, err := r.ReadSlice('\n')
		if strings.HasPrefix(string(line), "SSH-") {
			return nil
		}
		if err != nil {
			if isTimeout(err) && n == 0 && len(line) == 0 {
				return ErrNoGreeting // connected, then silence
			}
			return ErrNotSSH // it said something, and not SSH
		}
	}
	return ErrNotSSH
}

// Why says in a few words why a dial failed: connection refused, no answer,
// no DNS, unreachable, not ssh.
func Why(err error) string {
	var dnsErr *net.DNSError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNotSSH):
		return "not ssh"
	case errors.Is(err, ErrNoGreeting):
		return ErrNoGreeting.Error()
	case errors.As(err, &dnsErr):
		return "no DNS"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "unreachable"
	case isTimeout(err):
		return "no answer"
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	return msg
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "i/o timeout")
}
