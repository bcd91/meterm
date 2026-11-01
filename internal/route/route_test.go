package route

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"meterm/internal/config"
	"meterm/internal/testenv"
)

// fakeNet answers for the addresses in up and records every dial.
type fakeNet struct {
	mu    sync.Mutex
	up    map[string]bool
	dials []string
}

func (f *fakeNet) dial(_ context.Context, addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials = append(f.dials, addr)
	if f.up[addr] {
		return nil
	}
	return errors.New("dial tcp " + addr + ": connect: no route to host")
}

func office() *config.Config {
	return &config.Config{Hosts: []config.Host{
		{Name: "devbox", Address: "100.64.0.1", User: "dev",
			Routes: []config.Route{{Name: "public", Address: "203.0.113.50", Port: 5000}}},
		{Name: "lan1", Address: "192.168.1.10", User: "dev", ProxyJump: "devbox"},
	}}
}

func TestPrefersOwnAddressWhenItAnswers(t *testing.T) {
	cfg := office()
	n := &fakeNet{up: map[string]bool{"100.64.0.1:22": true, "203.0.113.50:5000": true}}
	p := Install(cfg, "")
	p.Dial = n.dial
	h, _ := cfg.Find("devbox")
	r := cfg.Resolve(*h)
	if r.Address != "100.64.0.1" || r.Port != 22 || r.Route != "" {
		t.Errorf("resolved %+v", r)
	}
}

func TestFallsBackToTheNextRouteAndJumpsFollow(t *testing.T) {
	cfg := office()
	n := &fakeNet{up: map[string]bool{"203.0.113.50:5000": true}} // VPN down
	p := Install(cfg, "")
	p.Dial = n.dial
	lan, _ := cfg.Find("lan1")
	r := cfg.Resolve(*lan)
	if len(r.ProxyJump) != 1 || r.ProxyJump[0].Address != "203.0.113.50" || r.ProxyJump[0].Port != 5000 || r.ProxyJump[0].Route != "public" {
		t.Fatalf("jump = %+v", r.ProxyJump)
	}
	// Decided once per process: resolving again doesn't dial again.
	before := len(n.dials)
	cfg.Resolve(*lan)
	if len(n.dials) != before {
		t.Errorf("dialled again: %v", n.dials[before:])
	}
}

func TestRemembersTheRouteThatWorked(t *testing.T) {
	state := filepath.Join(t.TempDir(), "routes.json")
	n := &fakeNet{up: map[string]bool{"203.0.113.50:5000": true}}
	cfg := office()
	p := Install(cfg, state)
	p.Dial = n.dial
	h, _ := cfg.Find("devbox")
	cfg.Resolve(*h)

	// Next run tries "public" first: one dial.
	n.dials = nil
	cfg2 := office()
	p2 := Install(cfg2, state)
	p2.Dial = n.dial
	h2, _ := cfg2.Find("devbox")
	if r := cfg2.Resolve(*h2); r.Route != "public" || len(n.dials) != 1 || n.dials[0] != "203.0.113.50:5000" {
		t.Errorf("route %q after dials %v", r.Route, n.dials)
	}
}

func TestNothingAnswersMeansOwnAddress(t *testing.T) {
	cfg := office()
	p := Install(cfg, "")
	p.Dial = (&fakeNet{}).dial
	h, _ := cfg.Find("devbox")
	c := p.Choose(*h)
	if c.Route != "default" || len(c.Tried) != 2 || c.Tried[1].Err != "unreachable" {
		t.Errorf("choice = %+v", c)
	}
	if r := cfg.Resolve(*h); r.Address != "100.64.0.1" {
		t.Errorf("resolved %+v", r)
	}
}

func TestDirectRouteIgnoresTheGroupsJump(t *testing.T) {
	cfg := &config.Config{
		Groups: map[string]config.Group{"office": {ProxyJump: "bastion"}},
		Hosts: []config.Host{
			{Name: "bastion", Address: "198.51.100.1"},
			{Name: "db", Address: "10.0.0.5", Group: "office", Routes: []config.Route{{Name: "lan"}}},
		},
	}
	p := Install(cfg, "")
	p.Dial = (&fakeNet{up: map[string]bool{"10.0.0.5:22": true}}).dial
	h, _ := cfg.Find("db")
	if r := cfg.Resolve(*h); len(r.ProxyJump) != 0 || r.ProxyJumpRaw != "" || r.Route != "lan" {
		t.Errorf("resolved %+v", r)
	}
}

func TestFirstHop(t *testing.T) {
	cases := map[string]config.Resolved{
		"10.0.0.1:22":        {Address: "10.0.0.1"},
		"[2001:db8::1]:2222": {Address: "2001:db8::1", Port: 2222},
		"198.51.100.2:22":    {Address: "x", ProxyJump: []config.Resolved{{Address: "198.51.100.2"}}},
		"j.example:2200":     {Address: "x", ProxyJumpRaw: "me@j.example:2200,other"},
		"[2001:db8::2]:22":   {Address: "x", ProxyJumpRaw: "me@[2001:db8::2]"},
	}
	for want, r := range cases {
		if got := FirstHop(r); got != want {
			t.Errorf("FirstHop(%+v) = %q, want %q", r, got, want)
		}
	}
}

func TestCarriesTheTrustedHostKeyToTheRoute(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	kh := filepath.Join(t.TempDir(), "known_hosts")
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJA+SfeNPN58Loh3y+m/lx3aP6w3hBSEitSCSHBrRLOk"
	if err := os.WriteFile(kh, []byte("100.64.0.1 "+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := KnownHosts
	KnownHosts = func() string { return kh }
	defer func() { KnownHosts = old }()

	cfg := office()
	p := Install(cfg, "")
	p.Dial = (&fakeNet{up: map[string]bool{"203.0.113.50:5000": true}}).dial
	h, _ := cfg.Find("devbox")
	cfg.Resolve(*h)
	cfg.Resolve(*h)
	// Hashed or not depends on the machine's HashKnownHosts (Ubuntu: yes).
	if ks, _ := lookupKeys(kh, "[203.0.113.50]:5000"); len(ks) != 1 || ks[0] != key {
		got, _ := os.ReadFile(kh)
		t.Errorf("route address not trusted with the same key; known_hosts =\n%s", got)
	}
	if got, _ := os.ReadFile(kh); strings.Count(string(got), "\n") != 2 {
		t.Errorf("expected exactly one line added (and only once):\n%s", got)
	}

	// Nothing trusted for the host: nothing is added.
	if err := os.WriteFile(kh, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = office()
	p = Install(cfg, "")
	p.Dial = (&fakeNet{up: map[string]bool{"203.0.113.50:5000": true}}).dial
	h, _ = cfg.Find("devbox")
	cfg.Resolve(*h)
	if got, _ := os.ReadFile(kh); len(got) != 0 {
		t.Errorf("added keys nobody trusted: %s", got)
	}
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "route-test")
	if err != nil {
		panic(err)
	}
	KnownHosts = func() string { return filepath.Join(dir, "known_hosts") } // never the real one
	done := testenv.Setup()
	code := m.Run()
	done()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestHashLines(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	got, err := hashLines("[203.0.113.50]:5000 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJA+SfeNPN58Loh3y+m/lx3aP6w3hBSEitSCSHBrRLOk\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "|1|") || strings.Contains(got, "203.0.113") || !strings.Contains(got, "ssh-ed25519 AAAAC3") {
		t.Errorf("hashed = %q", got)
	}
	// ssh-keygen still finds it by name.
	kh := filepath.Join(t.TempDir(), "kh")
	os.WriteFile(kh, []byte(got), 0o600)
	if ks, _ := lookupKeys(kh, "[203.0.113.50]:5000"); len(ks) != 1 {
		t.Errorf("hashed line not found: %v", ks)
	}
}

// sshListener answers each connection with what greet says, then closes it.
// An empty greet says nothing and stays open.
func sshListener(t *testing.T, greet string) string {
	t.Helper()
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
			if greet == "" {
				t.Cleanup(func() { c.Close() })
				continue
			}
			_, _ = c.Write([]byte(greet))
			c.Close()
		}
	}()
	return l.Addr().String()
}

func TestDialSSHWantsAGreeting(t *testing.T) {
	for _, c := range []struct {
		name, greet string
		ok          bool
	}{
		{"ssh server", "SSH-2.0-OpenSSH_10.0\r\n", true},
		{"lines before the greeting", "welcome\r\nSSH-2.0-x\r\n", true},
		{"web server", "HTTP/1.1 400 Bad Request\r\n\r\n", false},
		{"too much else", strings.Repeat("hello\n", 20) + "SSH-2.0-x\n", false},
		{"silent port", "", false},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		err := dialSSH(ctx, sshListener(t, c.greet))
		cancel()
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}

func TestShortErrSaysNotSSH(t *testing.T) {
	if got := shortErr(errNotSSH); got != "not ssh" {
		t.Errorf("shortErr = %q", got)
	}
}

func TestCheckLeavesOutAvoidedRoutes(t *testing.T) {
	h := config.Host{Name: "web", Address: "10.0.0.1", Routes: []config.Route{
		{Name: "vpn", Address: "10.0.0.2"}, {Name: "public", Address: "10.0.0.3"}}}
	cfg := &config.Config{Hosts: []config.Host{h}}
	p := Install(cfg, "")
	p.Dial = (&fakeNet{up: map[string]bool{"10.0.0.1:22": true, "10.0.0.3:22": true}}).dial
	if got := p.Check(h, "").Route; got != "default" {
		t.Fatalf("before = %q", got)
	}
	a := NewAvoid()
	p.Avoid = a
	if !a.Mark(h, "default") {
		t.Fatal("no route left, with two more")
	}
	if got := p.Check(h, "default").Route; got != "public" {
		t.Errorf("avoiding default: %q, want public", got)
	}
	// Nothing answers: fall back to a way that's still allowed, not the avoided one.
	p.Dial = (&fakeNet{}).dial
	if got := p.Check(h, "").Route; got != "vpn" {
		t.Errorf("nothing up: %q, want vpn", got)
	}
	if !a.Mark(h, "vpn") {
		t.Error("public is still left")
	}
	if a.Mark(h, "public") {
		t.Error("marked the last way in")
	}
	if got := p.Check(h, "").Route; got != "public" {
		t.Errorf("last way in: %q", got)
	}
}

func TestAvoidExpiresAndClears(t *testing.T) {
	h := config.Host{Name: "web", Address: "192.0.2.1", Routes: []config.Route{{Name: "vpn", Address: "192.0.2.2"}}}
	a := NewAvoid()
	now := time.Now()
	a.now = func() time.Time { return now }
	if !a.Mark(h, "default") {
		t.Fatal("vpn is still left")
	}
	if !a.has("web", "default") {
		t.Fatal("not avoided right after the mark")
	}
	now = now.Add(avoidFor + time.Second)
	if a.has("web", "default") {
		t.Error("still avoided after it expired")
	}
	if !a.Mark(h, "default") {
		t.Fatal("vpn is still left")
	}
	a.Clear()
	if a.has("web", "default") {
		t.Error("still avoided after Clear")
	}
}

// A look writes nothing, and goes no further than its context.
func TestLookLeavesNoState(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "routes.json")
	known := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(known, []byte("192.0.2.1 ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := KnownHosts
	KnownHosts = func() string { return known }
	defer func() { KnownHosts = old }()
	h := config.Host{Name: "a", Address: "192.0.2.1", Routes: []config.Route{{Name: "pub", Address: "198.51.100.1"}}}
	cfg := &config.Config{Hosts: []config.Host{h}}
	p := NewLook(cfg, state)
	p.Dial = func(_ context.Context, addr string) error {
		if strings.Contains(addr, "198.51.100.1") {
			return nil
		}
		return errors.New("no")
	}
	rt, ok := p.Look(context.Background())(h)
	if !ok || rt.Name != "pub" {
		t.Fatalf("got %+v, %v", rt, ok)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("routes.json: %v", err)
	}
	if b, _ := os.ReadFile(known); string(b) != "192.0.2.1 ssh-ed25519 AAAA\n" {
		t.Errorf("known_hosts: %s", b)
	}
}

func TestLookStopsWithItsContext(t *testing.T) {
	h := config.Host{Name: "a", Address: "192.0.2.1", Routes: []config.Route{{Name: "r1", Address: "192.0.2.2"}, {Name: "r2", Address: "192.0.2.3"}}}
	p := NewLook(&config.Config{Hosts: []config.Host{h}}, "")
	dials := 0
	ctx, cancel := context.WithCancel(context.Background())
	p.Dial = func(context.Context, string) error {
		dials++
		cancel()
		return errors.New("no")
	}
	p.Look(ctx)(h)
	if dials != 1 {
		t.Errorf("%d dials after the context ended", dials)
	}
}

// A server that accepts and says nothing can't hold a probe past its context.
func TestDialSSHHonoursCancel(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	if err := dialSSH(ctx, l.Addr().String()); err == nil {
		t.Error("no error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %s", d)
	}
}
