package health

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"meterm/internal/config"
)

// fakeNet answers for the addresses in up, refuses the rest, and records
// what was dialled.
type fakeNet struct {
	up    map[string]bool
	fail  map[string]error
	tried []string
}

func (f *fakeNet) dial(_ context.Context, addr string) error {
	f.tried = append(f.tried, addr)
	if f.up[addr] {
		return nil
	}
	if err := f.fail[addr]; err != nil {
		return err
	}
	return &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

var at = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

func check(cfg *config.Config, name, prefer string, f *fakeNet) Result {
	h, _ := cfg.Find(name)
	return CheckHost(cfg, *h, prefer, f.dial, time.Second, func() time.Time { return at })
}

// The way connect would go: the host's own address down but a route up is
// up, via that route.
func TestCheckHostFollowsRoutes(t *testing.T) {
	cfg := &config.Config{Hosts: []config.Host{
		{Name: "box", Address: "192.0.2.10", Routes: []config.Route{{Name: "vpn", Address: "198.51.100.10"}}},
	}}
	f := &fakeNet{up: map[string]bool{"198.51.100.10:22": true}}
	r := check(cfg, "box", "", f)
	if r.State != Up || r.Route != "vpn" || !r.At.Equal(at) {
		t.Errorf("own address down, vpn up: %+v", r)
	}
	if len(f.tried) != 2 || f.tried[0] != "192.0.2.10:22" {
		t.Errorf("tried %v", f.tried)
	}
	// The route that worked last is tried first, as connect does.
	f = &fakeNet{up: map[string]bool{"198.51.100.10:22": true, "192.0.2.10:22": true}}
	if r = check(cfg, "box", "vpn", f); r.Route != "vpn" || len(f.tried) != 1 {
		t.Errorf("prefer vpn: %+v, tried %v", r, f.tried)
	}
	f = &fakeNet{up: map[string]bool{"192.0.2.10:22": true}}
	if r = check(cfg, "box", "", f); r.State != Up || r.Route != "" || r.Reason != "" {
		t.Errorf("own address up: %+v", r)
	}
	// Nothing answers: down, with why its own address didn't.
	f = &fakeNet{}
	if r = check(cfg, "box", "", f); r.State != Down || r.Reason != "connection refused" {
		t.Errorf("all down: %+v", r)
	}
}

// A host behind a jump host is checked the way connect goes: its first hop,
// the jump host's own address, is dialled, and the answer says which jump
// host it was. web00 has a jump host and a vpn route: connect tries its own
// way first, through bastion, so that's what's checked and named, not vpn.
func TestCheckHostDialsTheJumpHost(t *testing.T) {
	cfg := &config.Config{Hosts: []config.Host{
		{Name: "bastion", Address: "192.0.2.1"},
		{Name: "inner", Address: "10.0.0.5", ProxyJump: "bastion"},
		{Name: "web00", Address: "10.0.0.7", ProxyJump: "bastion", Routes: []config.Route{{Name: "vpn", Address: "198.51.100.10"}}},
		{Name: "raw", Address: "10.0.0.8", ProxyJump: "ops@203.0.113.5:2200"},
		{Name: "mixed", Address: "192.0.2.20", Routes: []config.Route{{Name: "via-b", Address: "10.0.0.6", ProxyJump: "bastion"}}},
	}}
	f := &fakeNet{up: map[string]bool{"192.0.2.1:22": true}}
	if r := check(cfg, "inner", "", f); r.State != Up || r.Via != "bastion" || r.Route != "" || len(f.tried) != 1 || f.tried[0] != "192.0.2.1:22" {
		t.Errorf("inner, bastion up: %+v, tried %v", r, f.tried)
	}
	f = &fakeNet{}
	if r := check(cfg, "inner", "", f); r.State != Down || r.Via != "bastion" || r.Reason != "connection refused" {
		t.Errorf("inner, bastion down: %+v", r)
	}
	f = &fakeNet{up: map[string]bool{"192.0.2.1:22": true, "198.51.100.10:22": true}}
	if r := check(cfg, "web00", "", f); r.State != Up || r.Route != "" || r.Via != "bastion" {
		t.Errorf("web00, both up: %+v (connect goes through bastion)", r)
	}
	f = &fakeNet{up: map[string]bool{"198.51.100.10:22": true}}
	if r := check(cfg, "web00", "", f); r.State != Up || r.Route != "vpn" || r.Via != "" {
		t.Errorf("web00, bastion down: %+v (connect falls back to vpn)", r)
	}
	f = &fakeNet{up: map[string]bool{"203.0.113.5:2200": true}}
	if r := check(cfg, "raw", "", f); r.State != Up || r.Via != "ops@203.0.113.5:2200" || f.tried[0] != "203.0.113.5:2200" {
		t.Errorf("raw jump: %+v, tried %v", r, f.tried)
	}
	// Nothing answers: connect would go its own way, so that's the reason given.
	f = &fakeNet{fail: map[string]error{"192.0.2.1:22": context.DeadlineExceeded}}
	if r := check(cfg, "mixed", "via-b", f); r.State != Down || r.Route != "" || r.Via != "" || r.Reason != "connection refused" {
		t.Errorf("mixed, all down: %+v", r)
	}
	// A jump host with routes of its own is reached the way connect reaches
	// it: box's address is down, its vpn route answers, so inner is up.
	cfg.Hosts = append(cfg.Hosts, config.Host{Name: "box", Address: "192.0.2.10", Routes: []config.Route{{Name: "vpn", Address: "198.51.100.10"}}},
		config.Host{Name: "behind-box", Address: "10.0.0.9", ProxyJump: "box"})
	f = &fakeNet{up: map[string]bool{"198.51.100.10:22": true}}
	if r := check(cfg, "behind-box", "", f); r.State != Up || r.Via != "box" {
		t.Errorf("behind box, box up via vpn: %+v, tried %v", r, f.tried)
	}
	f = &fakeNet{}
	if r := check(cfg, "behind-box", "", f); r.State != Down || r.Via != "box" || r.Reason != "connection refused" {
		t.Errorf("behind box, box down: %+v", r)
	}
	// A host connect won't reach at all isn't checked, and says why.
	bad := &config.Config{Hosts: []config.Host{{Name: "odd", Address: "-oProxyCommand=x"}}}
	f = &fakeNet{}
	if r := check(bad, "odd", "", f); r.State != NotChecked || r.Reason == "" || len(f.tried) != 0 {
		t.Errorf("unsafe host: %+v, tried %v", r, f.tried)
	}
}

// Why a dial failed is kept, in words.
func TestReasons(t *testing.T) {
	for err, want := range map[error]string{
		&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}: "connection refused",
		&net.DNSError{Err: "no such host", Name: "x.invalid", IsNotFound: true}:            "no DNS",
		context.DeadlineExceeded: "no answer",
		&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}: "unreachable",
		ErrNotSSH:                     "not ssh",
		errors.New("something: else"): "else",
	} {
		if got := Why(err); got != want {
			t.Errorf("Why(%v) = %q, want %q", err, got, want)
		}
	}
}

// Dial wants an SSH greeting, not just an open port.
func TestDialWantsAGreeting(t *testing.T) {
	serve := func(say string) string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				fmt.Fprint(c, say)
				bufio.NewReader(c).ReadString('\n')
				c.Close()
			}
		}()
		return l.Addr().String()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Dial(ctx, serve("SSH-2.0-OpenSSH_9.6\r\n")); err != nil {
		t.Errorf("ssh server: %v", err)
	}
	if err := Dial(ctx, serve("HTTP/1.1 400 Bad\r\n\r\n")); !errors.Is(err, ErrNotSSH) {
		t.Errorf("http server: %v", err)
	}
}

// A port that takes the connection and then says nothing (a multiplexer
// like sslh waiting for the client, a slow banner) isn't "no answer": the
// connection was made. It's unsure, not down.
func TestSilentPortIsUnsureNotDown(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { bufio.NewReader(c).ReadString('\n'); c.Close() }() // never speaks
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := Dial(ctx, l.Addr().String()); !errors.Is(err, ErrNoGreeting) {
		t.Errorf("silent port: %v", err)
	}

	cfg := &config.Config{Hosts: []config.Host{{Name: "mux", Address: "192.0.2.40"}, {Name: "far", Address: "10.0.0.1", ProxyJump: "mux"}}}
	f := &fakeNet{fail: map[string]error{"192.0.2.40:22": ErrNoGreeting}}
	h, _ := cfg.Find("mux")
	r := CheckHost(cfg, *h, "", f.dial, 2*time.Second, func() time.Time { return at })
	if r.State != Unsure || r.Reason != "port open, no SSH greeting in 2s" {
		t.Errorf("silent host: %+v", r)
	}
	h, _ = cfg.Find("far")
	if r := CheckHost(cfg, *h, "", f.dial, 2*time.Second, func() time.Time { return at }); r.State != Unsure || r.Via != "mux" {
		t.Errorf("behind a silent jump host: %+v", r)
	}
	// Refused is still down.
	f = &fakeNet{}
	h, _ = cfg.Find("mux")
	if r := CheckHost(cfg, *h, "", f.dial, 2*time.Second, func() time.Time { return at }); r.State != Down || r.Reason != "connection refused" {
		t.Errorf("refused: %+v", r)
	}
}
