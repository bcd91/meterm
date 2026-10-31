package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"time"

	"meterm/internal/config"
	"meterm/internal/route"
)

const (
	// wakeTick is how often the clock is compared, and wakeGap how far apart
	// two looks have to be to mean the machine was asleep. A sleeping machine's
	// monotonic clock stands still, so only the wall clock shows the gap.
	wakeTick = 5 * time.Second
	wakeGap  = 20 * time.Second
)

const (
	kickWoke    = "woke from sleep"
	kickNetwork = "the network changed"
)

// watchKicks says when a connection should be redone instead of waited out: an
// ssh that survived a sleep or a change of network only notices after its
// keepalives run out, some 45 seconds of a frozen terminal. Whoever hears a kick
// decides whether the connection can afford it. What was learned about routes on
// the old network is dropped along the way.
func watchKicks(ctx context.Context) <-chan string {
	ticker := time.NewTicker(wakeTick)
	context.AfterFunc(ctx, ticker.Stop)
	sig := make(chan os.Signal, 1)
	if s := kickSignals(); len(s) > 0 {
		signal.Notify(sig, s...)
		context.AfterFunc(ctx, func() { signal.Stop(sig) })
	}
	return kicksFrom(ctx, ticker.C, sig, time.Now)
}

// kicksFrom is watchKicks given its clock and signals, for tests.
func kicksFrom(ctx context.Context, tick <-chan time.Time, sig <-chan os.Signal, now func() time.Time) <-chan string {
	out := make(chan string, 1)
	send := func(why string) {
		avoidedRoutes.Clear()
		select {
		case out <- why:
		default: // one waiting is enough
		}
	}
	go func() {
		last := now().Round(0)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				send(kickNetwork)
			case <-tick:
				t := now().Round(0) // Round(0) drops the monotonic reading
				if t.Sub(last) > wakeGap {
					send(kickWoke)
				}
				last = t
			}
		}
	}()
	return out
}

// kickable gives one attempt a context that a kick ends, when allow says this
// is a good moment. stop ends it and says why it was kicked, if it was.
func kickable(ctx context.Context, kicks <-chan string, allow func() bool) (attempt context.Context, stop func() string) {
	attempt, cancel := context.WithCancel(ctx)
	done := make(chan string, 1)
	go func() {
		for {
			select {
			case <-attempt.Done():
				done <- ""
				return
			case why := <-kicks:
				if allow == nil || allow() {
					cancel()
					done <- why
					return
				}
			}
		}
	}()
	return attempt, func() string {
		cancel()
		return <-done
	}
}

// avoidedRoutes are the routes found to lead to a host with the wrong key. They
// come back after a while, or at once when the network changes: see route.Avoid.
var avoidedRoutes = route.NewAvoid()

// avoidRoute stops using the route that just failed ssh's host key check, and
// says whether there's another way to try. ssh's check stays what it was: the
// next route gets the same verification, and a changed key is never accepted.
// Which hop failed isn't known from here, so the host goes first and then its
// jump hosts; a route that gets dropped needlessly costs only one more attempt.
func avoidRoute(cfg *config.Config, h config.Host, r config.Resolved) (string, bool) {
	if avoidedRoutes.Mark(h, routeOrDefault(r.Route)) {
		return fmt.Sprintf("route %q of %s", routeOrDefault(r.Route), h.Name), true
	}
	for _, j := range r.ProxyJump {
		if jh, i := cfg.Find(j.Name); i >= 0 && avoidedRoutes.Mark(*jh, routeOrDefault(j.Route)) {
			return fmt.Sprintf("route %q of %s", routeOrDefault(j.Route), j.Name), true
		}
	}
	return "", false
}

func routeOrDefault(name string) string {
	if name == "" {
		return "default"
	}
	return name
}

// noPrompts stops c, and the ssh that -J starts for each hop, from asking anyone
// anything. BatchMode can't do that: -J's ssh gets none of the options given to
// its parent. Without a controlling terminal there's nowhere to ask, and the
// variable rules out a graphical prompt.
func noPrompts(c *exec.Cmd) {
	detach(c)
	c.Env = append(os.Environ(), "SSH_ASKPASS_REQUIRE=never")
}
