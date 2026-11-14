package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
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
	// agentsWatchGrace is added to stale_after on top of the worst case it works out,
	// for a slow machine.
	agentsWatchGrace = 15 * time.Second
	// agentsLookSlack is how far past --timeout a look can still run: ssh's WaitDelay
	// after it's killed, and ssh -G for a watcher look.
	agentsLookSlack = 2 * time.Second
	// agentsWatchOutputGone is the exit code when stdout was closed: the reader went
	// away, so the watch stopped.
	agentsWatchOutputGone = 3
	// agentsRefusedMinInterval is the least wait after a refused watcher key that had
	// got in before.
	agentsRefusedMinInterval = time.Minute
	// agentsRefusedStopAfter and agentsRefusedStopSpan: a watcher key that has never
	// got in during the run is given up on after this many refusals in a row, at least
	// this far apart from first to last. One refusal can be a passing thing (a home on
	// NFS, an LDAP hiccup, authorized_keys being rewritten).
	agentsRefusedStopAfter = 3
	agentsRefusedStopSpan  = 5 * time.Minute
)

// A stopping watch gives stdout until it has taken no line for agentsWatchStall, and
// no more than agentsWatchFlushCap in all, to take its last lines. Variables, for tests.
var (
	agentsWatchStall    = 2 * time.Second
	agentsWatchFlushCap = 10 * time.Second
)

// errWatchOutput is a write to stdout failing: the reader is gone.
var errWatchOutput = errors.New("stdout was closed")

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
	// grace and slack go into stale_after: agentsWatchGrace and agentsLookSlack,
	// except in tests that check the bound itself.
	grace, slack time.Duration
	// outputGone, if set, says the reader of stdout has gone away (stdoutGone).
	outputGone func() bool
}

// staleMargin is how long after a look is due its line can still come: its own run,
// and before that, the wait for a slot behind -j others. Waiters get a slot in turn,
// so with n hosts and j slots a look waits for at most ceil((n-j)/j) others' runs.
func (o watchOptions) staleMargin(watched int) time.Duration {
	run := o.timeout + o.slack
	j := max(o.parallel, 1)
	waves := 0
	if watched > j {
		waves = (watched - j + j - 1) / j
	}
	return run*time.Duration(1+waves) + o.grace
}

// runAgentsWatch looks at each host until ctx ends, then writes the last line and
// returns nil. It returns early with an error only if w can't be written to.
func runAgentsWatch(ctx context.Context, w io.Writer, jobs []agentJob, query agentQuery, o watchOptions) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A write that fails stops every look: the reader is gone.
	out := newWatchWriter(ctx, w, o.format, cancel)
	labels := make([]string, len(jobs))
	for i, j := range jobs {
		labels[i] = lookLabel(j)
	}
	out.write(agents.Start{Schema: agents.WatchSchema, Type: agents.LineStart, At: o.clock.now().UTC(),
		IntervalS: int64(o.interval / time.Second), TimeoutS: int64(o.timeout / time.Second), Hosts: labels})

	watched := 0
	for _, job := range jobs {
		if watchSkip(job) == "" {
			watched++
		}
	}
	margin := o.staleMargin(watched)
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
			watchHost(ctx, job, labels[i], query, o, margin, sem, out)
		}()
	}
	if o.outputGone != nil {
		// In text form nothing may be written for a long time, so a reader that went
		// away wouldn't be noticed by a write; look for it instead.
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if o.outputGone() {
						out.fail(errWatchOutput)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	return out.close(agents.Stopped{Schema: agents.WatchSchema, Type: agents.LineStopped, At: o.clock.now().UTC()}, agentsWatchStall, agentsWatchFlushCap)
}

func watchHost(ctx context.Context, job agentJob, label string, query agentQuery, o watchOptions, margin time.Duration, sem chan struct{}, out *watchWriter) {
	tr := agents.NewTracker()
	delay := agents.FirstDelay(o.interval, o.clock.rand())
	refusedRun, refusedSince := 0, time.Time{} // watcher-key refusals in a row
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
		if h.Error == agentsWatcherRefused {
			if refusedRun == 0 {
				refusedSince = at
			}
			refusedRun++
		} else {
			refusedRun = 0
		}
		if h.Error == agentsWatcherRefused && look.LastOKAt == nil &&
			refusedRun >= agentsRefusedStopAfter && at.Sub(refusedSince) >= agentsRefusedStopSpan {
			// The key has never got in during this run, and has been refused again and
			// again for minutes: it isn't set up there, and asking again would only add
			// failed logins, enough of which get an address banned (fail2ban). It takes
			// someone fixing the key.
			look.StoppedAsking = true
			for _, c := range changes {
				out.write(c)
			}
			out.write(look)
			// And the watch keeps running until it's stopped, even with nothing left to
			// ask: a seat that starts it again whenever it ends would otherwise bring
			// the failed logins straight back.
			<-ctx.Done()
			return
		}
		base := o.interval
		if h.Error == agentsWatcherRefused || h.Error == agentsKeyRefused {
			// A refused key, watcher or ordinary, that isn't given up on: this may pass (a
			// host restarting, a file being edited), and it's asked again; but no faster
			// than every minute, doubling, so a refusing host sees at most 4 tries in
			// any 10 minutes (see the doc).
			base = max(base, agentsRefusedMinInterval)
		}
		delay = agents.NextDelay(base, tr.Failing(), o.clock.rand())
		next := at.Add(delay).UTC()
		stale := next.Add(margin)
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

// watchWriter writes lines from every host's goroutine, one whole line at a time, in
// the order they're given. The writing itself is done by a goroutine of its own, so
// that a reader that keeps the pipe open and never reads can't hold up a stop: a
// line waiting to go out gives way to the watch ending (stop). The first write that
// fails is kept, stops the watch, and nothing more is written after it.
type watchWriter struct {
	mu      sync.Mutex
	w       io.Writer
	format  string
	err     error
	last    map[string]string // text: what was last said of each host
	started time.Time
	cancel  context.CancelFunc
	stop    <-chan struct{}
	lines   chan []byte
	done    chan struct{}
	taken   atomic.Int64 // when stdout last took a line, Unix nanoseconds
}

func newWatchWriter(ctx context.Context, w io.Writer, format string, cancel context.CancelFunc) *watchWriter {
	o := &watchWriter{w: w, format: format, last: map[string]string{}, cancel: cancel, stop: ctx.Done(),
		lines: make(chan []byte, 64), done: make(chan struct{})}
	go func() {
		defer close(o.done)
		for b := range o.lines {
			if o.error() != nil {
				continue
			}
			if _, err := o.w.Write(b); err != nil {
				o.fail(err)
			}
			o.taken.Store(time.Now().UnixNano())
		}
	}()
	return o
}

// fail stops the watch as a failed write would.
func (o *watchWriter) fail(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err == nil {
		o.err = err
	}
	if o.cancel != nil {
		o.cancel()
	}
}

func (o *watchWriter) error() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// line is v as it's written, or nil for nothing to write.
func (o *watchWriter) line(v any) []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return nil
	}
	if o.format == "json" {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(v); err != nil {
			o.err = err
			return nil
		}
		return buf.Bytes()
	}
	return []byte(o.text(v))
}

// write queues v, unless the watch is ending first.
func (o *watchWriter) write(v any) {
	b := o.line(v)
	if len(b) == 0 {
		return
	}
	// Into the queue if there's room, stopping or not; only a full one gives way.
	select {
	case o.lines <- b:
		return
	default:
	}
	select {
	case o.lines <- b:
	case <-o.stop:
	}
}

// errWatchBlocked is stdout not taking the last lines within the grace a stop allows.
var errWatchBlocked = fmt.Errorf("%w: stdout isn't being read", errWatchOutput)

// close writes the last line, and waits for everything to be written: as long as
// stdout keeps taking lines, up to a stall of stall with none taken, and cap in all.
// Called once, after every other write.
func (o *watchWriter) close(last any, stall, capAll time.Duration) error {
	start := time.Now()
	o.taken.Store(start.UnixNano())
	stuck := func() bool {
		return time.Since(time.Unix(0, o.taken.Load())) > stall || time.Since(start) > capAll
	}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	if b := o.line(last); len(b) > 0 {
	send:
		for {
			select {
			case o.lines <- b:
				break send
			case <-tick.C:
				if stuck() {
					return errWatchBlocked
				}
			}
		}
	}
	close(o.lines)
	for {
		select {
		case <-o.done:
			if err := o.error(); err != nil {
				return fmt.Errorf("%w: %v", errWatchOutput, err)
			}
			return nil
		case <-tick.C:
			if stuck() {
				return errWatchBlocked
			}
		}
	}
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
		if l.StoppedAsking {
			s += "; not asking it again until the watch is started again"
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
		if l.Result != agents.ResultOK && l.NextLookAt != nil && !l.StoppedAsking {
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
