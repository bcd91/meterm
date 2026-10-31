package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"meterm/internal/config"
	"meterm/internal/route"
	"meterm/internal/sshrun"
	"meterm/internal/status"
)

func tunnelConfig() *config.Config {
	return &config.Config{Hosts: []config.Host{
		{Name: "bastion", Address: "203.0.113.1", User: "ops"},
		{Name: "db", Address: "10.0.0.5", ProxyJump: "bastion", Forwards: []config.Forward{
			{Type: "L", LocalPort: 9000, RemoteHost: "localhost", RemotePort: 9000}, // opens on connect
			{Name: "pg", Type: "L", LocalPort: 5432, RemoteHost: "localhost", RemotePort: 5432},
			{Name: "redis", Type: "L", LocalPort: 6379, RemoteHost: "localhost", RemotePort: 6379},
		}},
	}}
}

func TestTunnelFromFlags(t *testing.T) {
	if f, err := tunnelFromFlags("pg", "5432:db.internal:5432", "", 0); err != nil || f.Type != "L" || f.RemoteHost != "db.internal" {
		t.Errorf("-L = %+v, %v", f, err)
	}
	if f, err := tunnelFromFlags("back", "", "8080:localhost:3000", 0); err != nil || f.Type != "R" || f.LocalPort != 8080 {
		t.Errorf("-R = %+v, %v", f, err)
	}
	if f, err := tunnelFromFlags("socks", "", "", 1080); err != nil || f.Type != "D" {
		t.Errorf("-D = %+v, %v", f, err)
	}
	for _, bad := range []struct {
		name, l, r string
		d          int
	}{
		{"pg", "", "", 0},          // none
		{"pg", "1:a:2", "", 1080},  // two
		{"pg", "5432:db", "", 0},   // short
		{"pg", "x:db:5432", "", 0}, // not a port
		{"a/b", "1:a:2", "", 0},    // slash in name
	} {
		if _, err := tunnelFromFlags(bad.name, bad.l, bad.r, bad.d); err == nil {
			t.Errorf("%+v: want an error", bad)
		}
	}
}

func TestPickTunnels(t *testing.T) {
	cfg := tunnelConfig()
	sets, err := pickTunnels(cfg, []string{"db/pg", "db"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || len(sets[0].forward) != 2 {
		t.Fatalf("db/pg + db = %+v, want one set with pg and redis once each", sets)
	}
	if _, err := pickTunnels(cfg, []string{"db/nope"}); err == nil {
		t.Error("unknown tunnel: want an error")
	}
	if _, err := pickTunnels(cfg, []string{"bastion"}); err == nil {
		t.Error("host without tunnels: want an error")
	}
}

func TestTunnelArgsOpenOnlyTheTunnel(t *testing.T) {
	cfg := tunnelConfig()
	sets, _ := pickTunnels(cfg, []string{"db/pg"})
	args := strings.Join(tunnelArgs(cfg, sets[0]), " ")
	for _, want := range []string{"-L 5432:localhost:5432", "-N", "ExitOnForwardFailure=yes", "-J ops@203.0.113.1"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	for _, not := range []string{"9000", "6379"} {
		if strings.Contains(args, not) {
			t.Errorf("args %q should not open %s", args, not)
		}
	}
	// A plain connect opens the unnamed forward but none of the tunnels.
	conn := strings.Join(sshrun.BuildArgs(cfg.Resolve(cfg.Hosts[1])), " ")
	if !strings.Contains(conn, "9000") || strings.Contains(conn, "5432") {
		t.Errorf("connect args %q: want the 9000 forward only", conn)
	}
}

// A team host's tunnel needs a note; a personal host's doesn't.
func TestTunnelNotesOnTeamHosts(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "hosts.yaml")
	defer func() { cfgPath = "" }()
	t.Setenv("METERM_ACTIVITY_DIR", dir)
	t.Setenv("METERM_NO_AUTOSYNC", "1")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// Port 1 on this machine: refused at once, so the probe fails fast.
	cfg.Upsert(config.Host{Name: "shared", Address: "127.0.0.1", Port: 1, Team: "t1"})
	cfg.Upsert(config.Host{Name: "mine", Address: "127.0.0.1", Port: 1})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) error {
		c := newTunnelAddCmd()
		c.SetArgs(args)
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		return c.Execute()
	}
	if err := run("shared", "api", "-L", "8080:localhost:80"); err == nil || !strings.Contains(err.Error(), "--note") {
		t.Errorf("team tunnel without a note: %v", err)
	}
	if err := run("shared", "api", "-L", "8080:localhost:80", "--note", "Admin UI\x1b[2J"); err == nil {
		t.Error("note with an escape sequence accepted")
	}
	if err := run("shared", "api", "-L", "8080:localhost:80", "--note", "Admin UI (prod)"); err != nil {
		t.Fatal(err)
	}
	if err := run("mine", "api", "-L", "8080:localhost:80"); err != nil {
		t.Fatalf("personal tunnel without a note: %v", err)
	}
	// Re-adding without --note keeps the note.
	if err := run("shared", "api", "-L", "8081:localhost:80"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = config.Load(cfgPath)
	h, _ := cfg.Find("shared")
	if len(h.Forwards) != 1 || h.Forwards[0].Note != "Admin UI (prod)" || h.Forwards[0].LocalPort != 8081 {
		t.Errorf("forwards = %+v", h.Forwards)
	}
}

// fakeSSH puts a script called ssh first on PATH: it notes each run in the file
// it returns, says what's in $FAKE_SSH_SAY on stderr, and exits 255.
func fakeSSH(t *testing.T, say string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script on PATH")
	}
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	script := "#!/bin/sh\necho run >> " + shQuote(runs) + "\necho " + shQuote(say) + " >&2\nexit 255\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return runs
}

func TestHoldTunnelStopsOnAFailureThatWontPass(t *testing.T) {
	runs := fakeSSH(t, "u@h: Permission denied (publickey).")
	args := func() ([]string, error) { return []string{"h"}, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := holdTunnel(ctx, "h", args, tunnelHold{keep: true, stopOnFailure: true}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "refused the key") {
		t.Errorf("err = %v", err)
	}
	if b, _ := os.ReadFile(runs); strings.Count(string(b), "run") != 1 {
		t.Errorf("ran %q, want once", b)
	}
}

func TestHoldTunnelTellsOnceWhenItKeepsFailing(t *testing.T) {
	fakeSSH(t, "u@h: Permission denied (publickey).")
	args := func() ([]string, error) { return []string{"h"}, nil }
	var notices []string
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hold := tunnelHold{keep: true, notice: func(s string) {
		notices = append(notices, s)
		cancel() // the third failure is enough to see
	}}
	if err := holdTunnel(ctx, "h", args, hold, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "refused the key") {
		t.Errorf("notices = %q", notices)
	}
}

func TestHoldTunnelStopsWhenArgsRefuse(t *testing.T) {
	runs := fakeSSH(t, "")
	want := errors.New("unsafe now")
	err := holdTunnel(context.Background(), "h", func() ([]string, error) { return nil, want }, tunnelHold{keep: true}, io.Discard)
	if !errors.Is(err, want) {
		t.Errorf("err = %v", err)
	}
	if _, serr := os.Stat(runs); serr == nil {
		t.Error("ran ssh for a host that was refused")
	}
}

func TestFreshTunnelArgsGoesByTheSavedHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	t.Setenv("METERM_CONFIG", path)
	write := func(addr string) {
		data := "version: 1\ndefaults: {port: 22}\nhosts:\n- name: db\n  address: " + addr + "\n  forwards:\n  - {name: pg, type: L, local_port: 5432, remote_host: localhost, remote_port: 5432}\n"
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("192.0.2.1")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	sets, err := pickTunnels(cfg, []string{"db/pg"})
	if err != nil {
		t.Fatal(err)
	}
	write("192.0.2.2")
	args, err := freshTunnelArgs(cfg, sets[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); !strings.Contains(got, "192.0.2.2") || strings.Contains(got, "192.0.2.1") {
		t.Errorf("args %q: want the saved address", got)
	}
	write("-oProxyCommand=evil")
	if _, err := freshTunnelArgs(cfg, sets[0]); err == nil {
		t.Error("a host that turned unsafe was accepted")
	}
}

func TestHoldTunnelReportsFailingToStatus(t *testing.T) {
	t.Setenv("METERM_RUNTIME_DIR", filepath.Join(t.TempDir(), "run"))
	fakeSSH(t, "u@h: Permission denied (publickey).")
	reg := status.Open()
	defer reg.Close()
	s := tunnelSet{host: config.Host{Name: "h"}, forward: []config.Forward{{Name: "web"}, {Name: "db"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hold := tunnelHold{keep: true, notice: func(string) { cancel() }}
	stop := hold.track(reg, &s)
	defer stop()
	args := func() ([]string, error) { return append(hold.marker.options(), "h"), nil }
	if err := holdTunnel(ctx, "h", args, hold, io.Discard); err != nil {
		t.Fatal(err)
	}
	rows, _, err := status.List()
	if err != nil || len(rows) != 1 {
		t.Fatalf("List = %v, %v", rows, err)
	}
	r := rows[0]
	if r.Kind != status.KindTunnel || r.Name != "web,db" || r.State != status.Failing || !strings.Contains(r.Detail, "refused the key") {
		t.Errorf("row = %+v", r)
	}
}

func TestFreshTunnelArgsFollowsTheSavedHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	t.Setenv("METERM_CONFIG", path)
	fw := config.Forward{Name: "db", Type: "L", LocalPort: 15432, RemoteHost: "localhost", RemotePort: 5432}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	save := func(fn func(c *config.Config)) {
		t.Helper()
		if err := cfg.Update(func(c *config.Config) error { fn(c); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	save(func(c *config.Config) {
		c.Upsert(config.Host{Name: "web", Address: "192.0.2.1", Forwards: []config.Forward{fw, {Name: "ui", Type: "L", LocalPort: 18080, RemoteHost: "localhost", RemotePort: 8080}}})
	})
	h, _ := cfg.Find("web")
	s := tunnelSet{host: *h, forward: []config.Forward{fw}}

	save(func(c *config.Config) { w, _ := c.Find("web"); w.Forwards[0].LocalPort = 15433 })
	args, err := freshTunnelArgs(cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "15433:localhost:5432") {
		t.Errorf("didn't take the changed forward: %v", args)
	}

	save(func(c *config.Config) { w, _ := c.Find("web"); w.Forwards = w.Forwards[1:] })
	if _, err := freshTunnelArgs(cfg, s); err == nil || !strings.Contains(err.Error(), "no such tunnel") {
		t.Errorf("removed tunnel: %v", err)
	}

	var verdict tunnelVerdict
	if _, err := freshTunnelArgs(cfg, s); !errors.As(err, &verdict) {
		t.Errorf("a removed tunnel is a decision: %v", err)
	}

	save(func(c *config.Config) { c.Remove("web"); c.Upsert(config.Host{Name: "other", Address: "192.0.2.2"}) })
	_, err = freshTunnelArgs(cfg, s)
	if err == nil || !strings.Contains(err.Error(), "isn't a saved host") {
		t.Errorf("removed host: %v", err)
	}
	if !errors.As(err, &verdict) {
		t.Errorf("a removed host is a decision: %v", err)
	}

	// Nothing in the file at all says nothing about this host, so it isn't for good.
	save(func(c *config.Config) { c.Remove("other") })
	_, err = freshTunnelArgs(cfg, s)
	if err == nil || errors.As(err, &verdict) {
		t.Errorf("an empty file: %v", err)
	}
}

func TestUnitTextCantSwallowTheNextLine(t *testing.T) {
	unit := renderTunnelUnit("/usr/bin/meterm", "", `web/x\`)
	if strings.Contains(unit, "\\\n") {
		t.Errorf("a line ends in a backslash:\n%s", unit)
	}
	if !strings.Contains(unit, fmt.Sprintf("RestartPreventExitStatus=%d\n", tunnelGaveUp)) {
		t.Errorf("no RestartPreventExitStatus:\n%s", unit)
	}
}

// It says so while the connection is up, not at the next drop.
func TestHoldTunnelSaysWhenItsBack(t *testing.T) {
	saved := tunnelSettled
	defer func() { tunnelSettled = saved }()
	// Longer than a failed attempt takes on a slow machine: one that lasts this long
	// counts as having worked, and starts the count of failures over.
	tunnelSettled = 5 * time.Second
	dir := t.TempDir()
	installFakeSSH(t, dir)
	t.Setenv(fakeSSHEnv, filepath.Join(dir, "runs"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	notices := make(chan string, 4)
	hold := tunnelHold{keep: true, notice: func(s string) {
		notices <- s
		if strings.Contains(s, "is back") {
			cancel()
		}
	}}
	if err := holdTunnel(ctx, "h", func() ([]string, error) { return []string{"h"}, nil }, hold, io.Discard); err != nil {
		t.Fatal(err)
	}
	close(notices)
	var got []string
	for s := range notices {
		got = append(got, s)
	}
	if len(got) != 2 || !strings.Contains(got[0], "keeps failing") || !strings.Contains(got[1], "is back") {
		t.Errorf("notices = %q", got)
	}
}

// It's the route the attempt went by that gets dropped, whatever answers now.
func TestTunnelNextRouteDropsTheRouteItUsed(t *testing.T) {
	saved := avoidedRoutes
	defer func() { avoidedRoutes = saved }()
	avoidedRoutes = route.NewAvoid()
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	t.Setenv("METERM_CONFIG", path)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h := config.Host{Name: "web", Address: "192.0.2.1", Routes: []config.Route{{Name: "vpn", Address: "192.0.2.2"}}}
	if err := cfg.Update(func(c *config.Config) error { c.Upsert(h); return nil }); err != nil {
		t.Fatal(err)
	}
	s := tunnelSet{host: h}
	next := tunnelNextRoute(&s)
	s.used.Route = "vpn"
	if got, ok := next(); !ok || got != `route "vpn" of web` {
		t.Errorf("%q, %v", got, ok)
	}
}
