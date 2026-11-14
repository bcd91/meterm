package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"meterm/internal/agents"
	"meterm/internal/termsafe"
)

// `meterm agents --watch`: the same look as `meterm agents`, taken again and again,
// one host at a time on its own schedule. Each look is a fresh login, as a single
// `agents` is: no connection is held or shared between looks, so each one is in the
// host's sshd log, and a key revoked there stops the next look. Nothing is written
// anywhere but stdout: no activity entry (2,880 looks a day per host would bury the
// log), no history, no state file. A seat runs it as its own child and reads the
// lines; when it's stopped, it's gone.

const (
	agentsWatchDefaultInterval = time.Minute
	agentsWatchMinInterval     = 15 * time.Second
	// agentsWatchGrace is added to a look's next time and timeout for stale_after,
	// for a look that waited its turn behind -j others.
	agentsWatchGrace = 15 * time.Second
)

// watchClock is what the watch takes from the world, so tests can run it without
// waiting: now, a sleep that says false when ctx ended first, and a random number
// in [0, 1).
type watchClock struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) bool
	rand  func() float64
}

var realWatchClock = watchClock{
	now: time.Now,
	sleep: func(ctx context.Context, d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	},
	rand: rand.Float64,
}

type watchOptions struct {
	interval, timeout time.Duration
	parallel          int
	format            string
	clock             watchClock
}

// runAgentsWatch looks at each host until ctx ends, then writes the last line and
// returns nil. It returns early with an error only if w can't be written to.
func runAgentsWatch(ctx context.Context, w io.Writer, jobs []agentJob, query agentQuery, o watchOptions) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &watchWriter{w: w, format: o.format, last: map[string]string{}}
	labels := make([]string, len(jobs))
	for i, j := range jobs {
		labels[i] = lookLabel(j)
	}
	out.write(agents.Start{Schema: agents.WatchSchema, Type: agents.LineStart, At: o.clock.now().UTC(),
		IntervalS: int64(o.interval / time.Second), TimeoutS: int64(o.timeout / time.Second), Hosts: labels})

	sem := make(chan struct{}, max(o.parallel, 1))
	var wg sync.WaitGroup
	for i, job := range jobs {
		if why := watchSkip(job); why != "" {
			// Said once, at the start: nothing about it changes while this runs.
			out.write(agents.Look{Schema: agents.WatchSchema, Type: agents.LineLook, Host: labels[i],
				Result: why, Error: watchSkipText(job, why)})
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			watchHost(ctx, job, labels[i], query, o, sem, out)
		}()
	}
	wg.Wait()
	if err := out.error(); err != nil {
		return err
	}
	out.write(agents.Stopped{Schema: agents.WatchSchema, Type: agents.LineStopped, At: o.clock.now().UTC()})
	return out.error()
}

func watchHost(ctx context.Context, job agentJob, label string, query agentQuery, o watchOptions, sem chan struct{}, out *watchWriter) {
	tr := agents.NewTracker()
	delay := agents.FirstDelay(o.interval, o.clock.rand())
	for {
		if !o.clock.sleep(ctx, delay) {
			return
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		start := o.clock.now()
		h := askAgents(ctx, job, query, o.timeout)
		<-sem
		if ctx.Err() != nil {
			return // stopped mid-look: that's not something the host said
		}
		h.Host = label
		at := o.clock.now()
		look, changes := tr.Observe(h, at.UTC(), at.Sub(start))
		if h.Error != "" && !lookUnreachable(h.Error) {
			// Not asked, or ssh couldn't start here: nothing to do with the host's
			// network, so not "unreachable" either.
			look.Result = agents.ResultError
		}
		delay = agents.NextDelay(o.interval, tr.Failing(), o.clock.rand())
		next := at.Add(delay).UTC()
		stale := next.Add(o.timeout + agentsWatchGrace)
		look.NextLookAt, look.StaleAfter = &next, &stale
		for _, c := range changes {
			out.write(c)
		}
		out.write(look)
		if out.error() != nil {
			return
		}
	}
}

// lookUnreachable says whether askAgents' error means ssh didn't get in: the host
// may be down, asleep, off the network, or refusing the key. Anything else is an
// error: the host was reached and the answer was no good, or it wasn't asked.
func lookUnreachable(msg string) bool {
	for _, p := range []string{"unreachable: ", "timed out after ", "ssh was killed"} {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	return false
}

// watchSkip is why a host isn't watched at all, or "".
func watchSkip(job agentJob) string {
	switch {
	case job.refused != "":
		return agents.ResultRefused
	case job.watchOff:
		return agents.ResultOptedOut
	}
	return ""
}

func watchSkipText(job agentJob, why string) string {
	if why == agents.ResultRefused {
		return job.refused
	}
	return "not watched: agents_watch is false for this host"
}

// lookLabel is the host as every line names it, cleaned as askAgents cleans it.
func lookLabel(j agentJob) string { return termsafe.Text(j.label) }

// watchWriter writes lines from every host's goroutine, one whole line at a time.
// The first write that fails is kept, and nothing more is written after it.
type watchWriter struct {
	mu      sync.Mutex
	w       io.Writer
	format  string
	err     error
	last    map[string]string // text: what was last said of each host
	started time.Time
}

func (o *watchWriter) error() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

func (o *watchWriter) write(v any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return
	}
	var b []byte
	if o.format == "json" {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(v); err != nil {
			o.err = err
			return
		}
		b = buf.Bytes()
	} else {
		b = []byte(o.text(v))
	}
	if len(b) == 0 {
		return
	}
	_, o.err = o.w.Write(b)
}

// text is the readable form: every change, and a host's line only when what it says
// is different from last time.
func (o *watchWriter) text(v any) string {
	clock := func(t time.Time) string { return t.Local().Format("15:04:05") }
	switch l := v.(type) {
	case agents.Start:
		o.started = l.At
		return fmt.Sprintf("%s  watching %d host(s), each about every %s; Ctrl-C to stop\n", clock(l.At), len(l.Hosts), time.Duration(l.IntervalS)*time.Second)
	case agents.Stopped:
		return fmt.Sprintf("%s  stopped\n", clock(l.At))
	case agents.Change:
		word := "NEEDS INPUT"
		if l.Change == agents.ChangeNeedsInputEnded {
			word = "no longer waiting"
			if l.Why == agents.WhyGone {
				word = "gone, was waiting"
			}
		}
		line := fmt.Sprintf("%s  %s  %s: %s", clock(l.At), l.Host, word, sessionText(l.Session.Agent))
		if l.Why == agents.WhySeenTwice {
			line += " (no process seen: low confidence)"
		}
		if l.WaitedS != nil {
			line += fmt.Sprintf(", raised for %s", time.Duration(*l.WaitedS)*time.Second)
		}
		return line + "\n"
	case agents.Look:
		var s string
		switch l.Result {
		case agents.ResultOK:
			s = lookSummary(l.Sessions)
		case agents.ResultRefused, agents.ResultOptedOut:
			s = l.Error
		default:
			s = l.Result + " since " + clock(*l.FailingSince) + ": " + strings.TrimPrefix(l.Error, "unreachable: ")
			if l.LastOKAt != nil {
				s += "; last answer " + clock(*l.LastOKAt)
			}
		}
		if o.last[l.Host] == s {
			return ""
		}
		o.last[l.Host] = s
		when := o.started
		if l.LookedAt != nil {
			when = *l.LookedAt
		}
		line := fmt.Sprintf("%s  %s  %s", clock(when), l.Host, s)
		if l.Result != agents.ResultOK && l.NextLookAt != nil {
			line += "; next look " + clock(*l.NextLookAt)
		}
		return line + "\n"
	}
	return ""
}

func lookSummary(ss []agents.WatchSession) string {
	if len(ss) == 0 {
		return "no agents"
	}
	waiting, pending := 0, 0
	for _, s := range ss {
		switch {
		case s.NeedsInputSince != nil:
			waiting++
		case s.Pending:
			pending++
		}
	}
	s := fmt.Sprintf("%d session(s)", len(ss))
	if waiting > 0 {
		s += fmt.Sprintf(", %d waiting for you", waiting)
	}
	if pending > 0 {
		s += fmt.Sprintf(", %d maybe waiting (no process seen yet)", pending)
	}
	return s
}

// sessionText is enough to find a session: its kind, its name or id, where it runs,
// and what it waits for. All of it was cleaned when it was read.
func sessionText(a agents.Agent) string {
	parts := []string{a.Kind}
	if n := agentName(a); n != "" {
		parts = append(parts, n)
	}
	if a.Cwd != "" {
		parts = append(parts, "in "+a.Cwd)
	}
	if a.PID != nil {
		parts = append(parts, "pid "+agentPID(a))
	}
	s := strings.Join(parts, " ")
	if a.WaitingFor != "" {
		s += " (" + a.WaitingFor + ")"
	}
	return s
}
