package main

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"meterm/internal/config"
	"meterm/internal/route"
)

func expectKick(t *testing.T, kicks <-chan string, want string) {
	t.Helper()
	select {
	case got := <-kicks:
		if got != want {
			t.Errorf("kick %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("no kick, want %q", want)
	}
}

func TestKicksSeeASleepAndASignal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var clock atomic.Int64
	base := time.Unix(1_700_000_000, 0)
	tick, sig := make(chan time.Time), make(chan os.Signal, 1)
	kicks := kicksFrom(ctx, tick, sig, func() time.Time { return base.Add(time.Duration(clock.Load())) })

	clock.Add(int64(wakeTick))
	tick <- time.Time{}
	select {
	case got := <-kicks:
		t.Fatalf("a kick (%q) with the clock running normally", got)
	case <-time.After(50 * time.Millisecond):
	}

	clock.Add(int64(time.Hour))
	tick <- time.Time{}
	expectKick(t, kicks, kickWoke)

	sig <- os.Interrupt
	expectKick(t, kicks, kickNetwork)
}

func TestKickableEndsTheAttemptOnlyWhenAllowed(t *testing.T) {
	kicks := make(chan string, 1)
	var allowed atomic.Bool
	attempt, stop := kickable(context.Background(), kicks, allowed.Load)
	kicks <- "first"
	select {
	case <-attempt.Done():
		t.Fatal("ended although not allowed")
	case <-time.After(50 * time.Millisecond):
	}
	allowed.Store(true)
	kicks <- "second"
	select {
	case <-attempt.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("not ended by an allowed kick")
	}
	if got := stop(); got != "second" {
		t.Errorf("stop = %q", got)
	}

	_, stop = kickable(context.Background(), make(chan string), nil)
	if got := stop(); got != "" {
		t.Errorf("stop without a kick = %q", got)
	}
}

func TestAvoidRouteMovesOnThenRunsOut(t *testing.T) {
	saved := avoidedRoutes
	defer func() { avoidedRoutes = saved }()
	avoidedRoutes = route.NewAvoid()
	h := config.Host{Name: "web", Address: "192.0.2.1", Routes: []config.Route{{Name: "vpn", Address: "192.0.2.2"}}}
	cfg := &config.Config{Hosts: []config.Host{h}}
	r := config.Resolved{Name: "web"}

	got, ok := avoidRoute(cfg, h, r)
	if !ok || got != `route "default" of web` {
		t.Errorf("first: %q, %v", got, ok)
	}
	r.Route = "vpn"
	if got, ok := avoidRoute(cfg, h, r); ok {
		t.Errorf("no route left, but got %q", got)
	}
}

func TestAvoidRouteGoesOnToTheJumpHost(t *testing.T) {
	saved := avoidedRoutes
	defer func() { avoidedRoutes = saved }()
	avoidedRoutes = route.NewAvoid()
	jump := config.Host{Name: "bastion", Address: "192.0.2.1", Routes: []config.Route{{Name: "vpn", Address: "192.0.2.2"}}}
	h := config.Host{Name: "web", Address: "10.0.0.1", ProxyJump: "bastion"}
	cfg := &config.Config{Hosts: []config.Host{jump, h}}
	r := config.Resolved{Name: "web", ProxyJump: []config.Resolved{{Name: "bastion"}}}
	got, ok := avoidRoute(cfg, h, r)
	if !ok || got != `route "default" of bastion` {
		t.Errorf("%q, %v", got, ok)
	}
}
