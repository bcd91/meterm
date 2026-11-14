package agents

import (
	"maps"
	"slices"
	"strconv"
	"time"
)

// What `meterm agents --watch` keeps between looks at one host, and the lines it
// prints. docs/agents-watch.md is the contract; this file is the one place that
// decides when a session starts or stops needing someone.

// WatchSchema is the version of the --watch stream. It goes up when a field changes
// meaning; new fields and new line types don't need it to.
const WatchSchema = 1

// Line types in the stream.
const (
	LineStart   = "start"
	LineLook    = "look"
	LineChange  = "change"
	LineStopped = "stopped"
)

// Results of one look.
const (
	ResultOK          = "ok"          // the host answered; sessions is the list, empty or not
	ResultUnreachable = "unreachable" // ssh couldn't get in, or gave up: nothing is known
	ResultError       = "error"       // it was reached, or not asked, and there's no list
	ResultRefused     = "refused"     // production or through production, and not named
	ResultOptedOut    = "opted_out"   // agents_watch: false
)

// Changes.
const (
	ChangeNeedsInputStarted = "needs_input_started"
	ChangeNeedsInputEnded   = "needs_input_ended"
)

// Why a change happened.
const (
	WhyPID       = "pid"        // a row with a process said so
	WhySeenTwice = "seen_twice" // a row with no process said so on two looks
	WhyChanged   = "changed"    // the session is still there and says something else
	WhyGone      = "gone"       // the session isn't listed any more
)

// MaxBackoff is the longest wait between looks at a host that keeps failing, unless
// the interval itself is longer.
const MaxBackoff = 10 * time.Minute

// WatchSession is a session as the watch reports it: what the host said, plus what
// the watch has made of it over time.
type WatchSession struct {
	Agent
	// NeedsInputSince is when the watch raised needs_input for this session. It's
	// when it was first seen, not when it started waiting, which may be up to an
	// interval earlier.
	NeedsInputSince *time.Time `json:"needs_input_since,omitempty"`
	// Pending: it says needs_input but has no process, and has been seen so once.
	// It's raised if the next look says the same.
	Pending bool `json:"pending,omitempty"`
}

// Start is the stream's first line, and Stopped its last when it ends cleanly.
type Start struct {
	Schema    int       `json:"schema"`
	Type      string    `json:"type"`
	At        time.Time `json:"at"`
	IntervalS int64     `json:"interval_s"`
	TimeoutS  int64     `json:"timeout_s"`
	Hosts     []string  `json:"hosts"`
}

type Stopped struct {
	Schema int       `json:"schema"`
	Type   string    `json:"type"`
	At     time.Time `json:"at"`
}

// Look is what one look at one host found.
type Look struct {
	Schema int    `json:"schema"`
	Type   string `json:"type"`
	Host   string `json:"host"`
	// LookedAt is when the answer came, or null for a host that isn't looked at.
	LookedAt *time.Time `json:"looked_at"`
	TookMS   int64      `json:"took_ms"`
	Result   string     `json:"result"`
	Error    string     `json:"error"`
	// Sessions is the list when Result is ok, and null otherwise: a host that
	// couldn't say isn't a host with nothing running.
	Sessions []WatchSession `json:"sessions"`
	// LastOKAt is when the host last answered, null if it never has in this run.
	// On a failed look, LastSessions is what it said then and AgeS how long ago.
	LastOKAt     *time.Time     `json:"last_ok_at"`
	LastSessions []WatchSession `json:"last_sessions,omitempty"`
	AgeS         *int64         `json:"age_s"`
	// FailingSince is the first failed look of the failures running now.
	FailingSince *time.Time `json:"failing_since"`
	NextLookAt   *time.Time `json:"next_look_at"`
	// StaleAfter: with no newer line for this host by then, the watch has stopped
	// or is stuck, and none of this is current.
	StaleAfter *time.Time `json:"stale_after"`
	// Looks and Failures count this run's looks at the host: each one is an sshd
	// login there.
	Looks    int `json:"looks"`
	Failures int `json:"failures"`
	// StoppedAsking: the host won't be asked again in this run (it refused the
	// watcher key). next_look_at and stale_after are null then.
	StoppedAsking bool `json:"stopped_asking,omitempty"`
}

// Change is a session starting or stopping needing someone.
type Change struct {
	Schema  int          `json:"schema"`
	Type    string       `json:"type"`
	Host    string       `json:"host"`
	At      time.Time    `json:"at"`
	Change  string       `json:"change"`
	Why     string       `json:"why"`
	Session WatchSession `json:"session"`
	// WaitedS is, for an end, how long it was raised: a lower bound on the wait,
	// to within an interval either side.
	WaitedS *int64 `json:"waited_s,omitempty"`
}

// Tracker is one host's memory across looks. It isn't safe for use by more than
// one goroutine; the watch gives each host its own.
type Tracker struct {
	looks, failures, failingRun int
	lastOK                      *time.Time
	lastSessions                []WatchSession
	failingSince                *time.Time
	sessions                    map[string]*track
}

type track struct {
	needsSince *time.Time
	pending    int // answers in a row with needs_input and no pid, while not raised; failed looks don't count
	last       WatchSession
}

func NewTracker() *Tracker { return &Tracker{sessions: map[string]*track{}} }

// Failing is how many looks in a row have failed, for the backoff.
func (t *Tracker) Failing() int { return t.failingRun }

// Observe takes one look's answer, at the time it came, and says what to print.
// NextLookAt and StaleAfter are the caller's, as it knows the schedule.
func (t *Tracker) Observe(h Host, at time.Time, took time.Duration) (Look, []Change) {
	t.looks++
	look := Look{Schema: WatchSchema, Type: LineLook, Host: h.Host, LookedAt: ptr(at), TookMS: took.Milliseconds(), Error: h.Error}
	if h.Error != "" {
		t.failures++
		t.failingRun++
		if t.failingSince == nil {
			t.failingSince = ptr(at)
		}
		look.Result = ResultError
		if !h.Reachable {
			look.Result = ResultUnreachable
		}
		look.LastSessions = t.lastSessions
		if t.lastOK != nil {
			look.AgeS = ptr(int64(at.Sub(*t.lastOK) / time.Second))
		}
		t.fill(&look)
		// What's known of each session stays as it was: a host that can't be
		// reached says nothing about whether anyone answered.
		return look, nil
	}
	t.failingRun, t.failingSince = 0, nil
	t.lastOK = ptr(at)
	look.Result = ResultOK
	look.AgeS = ptr(int64(0))

	var changes []Change
	seen := map[string]bool{}
	look.Sessions = make([]WatchSession, 0, len(h.Agents))
	for _, a := range h.Agents {
		key := sessionKey(a, seen)
		seen[key] = true
		tr := t.sessions[key]
		if tr == nil {
			tr = &track{}
			t.sessions[key] = tr
		}
		s := WatchSession{Agent: a}
		switch {
		case a.Event == EventNeedsInput && tr.needsSince != nil:
			// Still waiting. A row that lost its process doesn't end it either: that's
			// the few seconds before the supervisor starts the worker again.
			tr.pending = 0
		case a.Event == EventNeedsInput && a.PID != nil:
			tr.needsSince, tr.pending = ptr(at), 0
			s.NeedsInputSince = tr.needsSince
			changes = append(changes, t.change(h.Host, at, ChangeNeedsInputStarted, WhyPID, s, nil))
		case a.Event == EventNeedsInput:
			tr.pending++
			if tr.pending < 2 {
				s.Pending = true
				break
			}
			tr.needsSince, tr.pending = ptr(at), 0
			s.NeedsInputSince = tr.needsSince
			changes = append(changes, t.change(h.Host, at, ChangeNeedsInputStarted, WhySeenTwice, s, nil))
		case a.Event == EventUnknown:
			// Can't say: a raised wait stays raised, as ending it would read as the
			// prompt answered, which nothing here knows. A pending one starts its count
			// again: two answers in a row means two that said so.
			tr.pending = 0
		default:
			tr.pending = 0
			if tr.needsSince != nil {
				changes = append(changes, t.change(h.Host, at, ChangeNeedsInputEnded, WhyChanged, s, tr.needsSince))
				tr.needsSince = nil
			}
		}
		s.NeedsInputSince = tr.needsSince
		tr.last = s
		look.Sessions = append(look.Sessions, s)
	}
	for _, key := range slices.Sorted(maps.Keys(t.sessions)) {
		tr := t.sessions[key]
		if seen[key] {
			continue
		}
		if tr.needsSince != nil {
			last := tr.last
			last.NeedsInputSince = nil
			changes = append(changes, t.change(h.Host, at, ChangeNeedsInputEnded, WhyGone, last, tr.needsSince))
		}
		delete(t.sessions, key)
	}
	sortChanges(changes)
	t.lastSessions = look.Sessions
	t.fill(&look)
	return look, changes
}

func (t *Tracker) fill(l *Look) {
	l.Looks, l.Failures = t.looks, t.failures
	l.LastOKAt, l.FailingSince = t.lastOK, t.failingSince
}

func (t *Tracker) change(host string, at time.Time, what, why string, s WatchSession, since *time.Time) Change {
	c := Change{Schema: WatchSchema, Type: LineChange, Host: host, At: at, Change: what, Why: why, Session: s}
	if since != nil {
		c.WaitedS = ptr(int64(at.Sub(*since) / time.Second))
	}
	return c
}

// sortChanges puts ends before starts, and keeps the order found otherwise.
func sortChanges(cs []Change) {
	slices.SortStableFunc(cs, func(a, b Change) int {
		return rank(a) - rank(b)
	})
}

func rank(c Change) int {
	if c.Change == ChangeNeedsInputEnded {
		return 0
	}
	return 1
}

// sessionKey is how the watch knows a session from one look to the next. It's
// never printed: an interactive session has no id, and none is made up for it.
// A background session keeps its id when its worker is started again, with a new
// pid and even a new startedAt, so the id comes first.
func sessionKey(a Agent, taken map[string]bool) string {
	var key string
	switch {
	case a.ID != "":
		key = "id:" + a.ID
	case a.SessionID != "":
		key = "session:" + a.SessionID
	case a.PID != nil:
		key = "pid:" + strconv.FormatInt(*a.PID, 10)
	default:
		key = "row:" + a.Kind + "\x00" + a.Cwd + "\x00" + a.Name
	}
	if !taken[key] {
		return key
	}
	for n := 2; ; n++ {
		k := key + "#" + strconv.Itoa(n)
		if !taken[k] {
			return k
		}
	}
}

// NextDelay is how long to wait before the next look at a host, given how many
// looks in a row have failed and r, a random number in [0, 1). While the host
// answers it's the interval plus up to a tenth, so hosts watched together drift
// apart rather than being asked at the same moment. Each failure after the first
// doubles it, up to MaxBackoff, and there the jitter comes off the top instead, so
// it's never past the cap. It's never under the interval, and an interval at or
// past the cap isn't backed off at all.
func NextDelay(interval time.Duration, failing int, r float64) time.Duration {
	jitter := func(d time.Duration) time.Duration { return time.Duration(r * float64(d) / 10) }
	if interval >= MaxBackoff {
		return interval + jitter(interval)
	}
	base := interval
	for i := 1; i < failing && base < MaxBackoff; i++ {
		base *= 2
	}
	base = min(base, MaxBackoff)
	d := base + jitter(base)
	if d > MaxBackoff {
		d = MaxBackoff - jitter(MaxBackoff)
	}
	return max(d, interval)
}

// FirstDelay is when to take the first look at a host: soon, spread over a tenth
// of the interval but no more than 10 seconds, so a watch over many hosts doesn't
// start with all of them at once.
func FirstDelay(interval time.Duration, r float64) time.Duration {
	return time.Duration(r * float64(min(interval/10, 10*time.Second)))
}

func ptr[T any](v T) *T { return &v }
