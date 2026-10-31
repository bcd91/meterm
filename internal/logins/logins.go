// Package logins reads what a host's `last` says about who logged in, to set beside what
// meterm's log says. All of it is remote output, and treated as such: a line that isn't
// the known shape is counted and dropped, addresses have to be addresses, and text is
// cleaned and cut before anything can show it.
package logins

import (
	"bufio"
	"bytes"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"meterm/internal/activity"
	"meterm/internal/termsafe"
)

// Schema is the version of the --format json report. It goes up when a field changes
// meaning; new fields don't need it to.
const Schema = 1

// MaxLogins is how many records are asked for.
const MaxLogins = 500

// RemoteCommand is all `meterm logins` ever runs on a host. As with agents.RemoteCommand
// it's a constant, inside one pair of single quotes for whatever the login shell is. TZ
// makes the times UTC, since `last` prints no zone. -w keeps long names whole, -F full
// times with a year, -i addresses instead of names a host can make up. No sudo: wtmp is
// normally readable by anyone, and if it isn't that's this host's error.
const RemoteCommand = `sh -c 'LC_ALL=C TZ=UTC; export LC_ALL TZ; command -v last >/dev/null 2>&1 || exit 127; last -F -i -w -n 500 || exit 1'`

// ExitNoLast is RemoteCommand's exit code when `last` isn't there.
const ExitNoLast = 127

// Limits on what's believed.
const (
	MaxOutput = 1 << 20
	maxText   = 64 // runes per name or terminal
)

// Login is one interactive login as wtmp has it.
type Login struct {
	Time          time.Time  `json:"time"` // UTC
	End           *time.Time `json:"end"`  // nil if it's still on, or ended without saying when
	User          string     `json:"user"`
	TTY           string     `json:"tty"`
	Source        string     `json:"source"` // the address; empty for a local login
	StillLoggedIn bool       `json:"still_logged_in"`
	// Matched says this machine's activity log has an event on this host and account
	// within MatchWindow of this login. It's no more than that in either direction: see
	// docs/export.md.
	Matched bool      `json:"matched"`
	Event   *EventRef `json:"event"`
}

// EventRef points at the event a login was matched with.
type EventRef struct {
	Device string    `json:"device"`
	Seq    int64     `json:"seq"`
	Action string    `json:"action"`
	Time   time.Time `json:"time"`
}

// Parsed is what a host's output came to.
type Parsed struct {
	Logins []Login
	// Since is when wtmp starts, which is as far back as any of this can see.
	Since *time.Time
	// Unparsed counts lines that weren't the known shape.
	Unparsed int
	// Records counts every line `last` gave as a record, logins or not, to tell whether
	// it stopped at the number asked for.
	Records int
}

const lastTime = "Mon Jan _2 15:04:05 2006"

var (
	dateRE = `[A-Z][a-z]{2} [A-Z][a-z]{2} [ 0-9][0-9] [0-9]{2}:[0-9]{2}:[0-9]{2} [0-9]{4}`
	// user, terminal, address, start, and how it ended.
	lineRE  = regexp.MustCompile(`^(\S+)\s+(\S+|system boot)\s+(\S+)\s+(` + dateRE + `)\s+(?:(still logged in|still running|gone - no logout)|-\s+(` + dateRE + `|crash|down)\s+\([0-9+:]+\))\s*$`)
	beginRE = regexp.MustCompile(`^\S+ begins (` + dateRE + `)\s*$`)
)

// notLogins are what wtmp also keeps that nobody logged in as.
var notLogins = map[string]bool{"reboot": true, "shutdown": true, "runlevel": true}

// Parse reads `last -F -i -w`.
func Parse(raw []byte) Parsed {
	var out Parsed
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), MaxOutput)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		if line == "" {
			continue
		}
		if m := beginRE.FindStringSubmatch(line); m != nil {
			if t, err := parseTime(m[1]); err == nil {
				out.Since = &t
				continue
			}
		}
		out.Records++
		m := lineRE.FindStringSubmatch(line)
		if m == nil {
			out.Unparsed++
			continue
		}
		if notLogins[m[1]] {
			continue
		}
		start, err := parseTime(m[4])
		if err != nil {
			out.Unparsed++
			continue
		}
		addr, err := netip.ParseAddr(m[3])
		if err != nil {
			out.Unparsed++ // -i prints addresses: anything else isn't what it seems
			continue
		}
		l := Login{Time: start, User: clean(m[1]), TTY: clean(m[2]), StillLoggedIn: m[5] == "still logged in"}
		if !addr.IsUnspecified() {
			l.Source = addr.WithZone("").String()
		}
		if t, err := parseTime(m[6]); err == nil {
			l.End = &t
		}
		out.Logins = append(out.Logins, l)
	}
	return out
}

func parseTime(s string) (time.Time, error) { return time.ParseInLocation(lastTime, s, time.UTC) }

func clean(s string) string { return termsafe.Clip(termsafe.Text(s), maxText) }

// MatchWindow is how far apart a login and an event can be and still count as the same
// thing: the time between logging an event and sshd seeing the login, and two clocks
// that may not agree.
const MatchWindow = 2 * time.Minute

// Match fills in Matched and Event from events, for logins on host as account user ("" if
// the event doesn't say). Whatever isn't about reaching the host is left out: an outcome
// is the end of something, not a login.
func Match(ls []Login, events []activity.Event, host string) {
	for i := range ls {
		var best *activity.Event
		var bestGap time.Duration
		for j := range events {
			e := &events[j]
			if e.Host != host || e.Action == activity.ActionOutcome {
				continue
			}
			if u, _, ok := strings.Cut(e.Target, "@"); ok && u != ls[i].User {
				continue
			}
			gap := e.Time.Sub(ls[i].Time)
			if gap < 0 {
				gap = -gap
			}
			if gap <= MatchWindow && (best == nil || gap < bestGap) {
				best, bestGap = e, gap
			}
		}
		if best != nil {
			c := best.Clean()
			ls[i].Matched = true
			ls[i].Event = &EventRef{Device: c.Device, Seq: c.Seq, Action: c.Action, Time: c.Time}
		}
	}
}

// Host is what one host said. Logins is null, not empty, when Error is set: a host that
// couldn't say isn't a host nobody logged in to.
type Host struct {
	Host      string     `json:"host"`
	Reachable bool       `json:"reachable"`
	Error     string     `json:"error"`
	Since     *time.Time `json:"since"`
	Logins    []Login    `json:"logins"`
	Unparsed  int        `json:"unparsed"`
	// Truncated says there may be older logins than these.
	Truncated bool `json:"truncated"`
}

// Report is the --format json output.
type Report struct {
	Schema int    `json:"schema"`
	Hosts  []Host `json:"hosts"`
}
