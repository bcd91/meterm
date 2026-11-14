package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testdata/claude-2.1.323 is what `claude agents --json` printed on Claude Code
// 2.1.323, one file per state seen, in the order of a real run: a background session
// waiting for a permission, an interactive one waiting beside it and then approved,
// the background worker killed (its row keeps state blocked with no pid for 6 to 10
// seconds), started again by the supervisor, and the supervisor killed as well (the
// row is gone). Layout and field order are Claude's; ids, names, paths and pids are
// made up.
func fixture(t *testing.T, name string) Host {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "claude-2.1.323", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	list, err := Parse(b)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return Host{Host: "devbox", Reachable: true, Agents: list}
}

var t0 = time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)

func at(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }

type step struct {
	host Host
	want []string // "change why name", in order
}

func runSteps(t *testing.T, steps []step) *Tracker {
	t.Helper()
	tr := NewTracker()
	for i, s := range steps {
		_, changes := tr.Observe(s.host, at(i), time.Second)
		var got []string
		for _, c := range changes {
			got = append(got, c.Change+" "+c.Why+" "+c.Session.Name)
		}
		if strings.Join(got, "|") != strings.Join(s.want, "|") {
			t.Errorf("look %d: changes %q, want %q", i+1, got, s.want)
		}
	}
	return tr
}

func TestParseFixtures(t *testing.T) {
	for name, want := range map[string]string{
		"empty":                      "",
		"bg-waiting":                 "needs_input/high",
		"bg-and-interactive-waiting": "needs_input/high needs_input/high",
		"interactive-approved-busy":  "needs_input/high working/high",
		"interactive-idle":           "needs_input/high idle/high",
		"bg-worker-killed":           "needs_input/low idle/high",
		"bg-respawned-working":       "idle/high working/high",
		"bg-respawned-waiting":       "idle/high needs_input/high",
		"bg-daemon-killed":           "idle/high",
		"bg-working":                 "working/high",
		"bg-done":                    "done/high",
	} {
		var got []string
		for _, a := range fixture(t, name).Agents {
			got = append(got, a.Event+"/"+a.Confidence)
		}
		if strings.Join(got, " ") != want {
			t.Errorf("%s: %v, want %s", name, got, want)
		}
	}
}

// The real run, look by look: each wait raises once and ends once.
func TestWatchRealSequence(t *testing.T) {
	runSteps(t, []step{
		{fixture(t, "empty"), nil},
		{fixture(t, "bg-waiting"), []string{"needs_input_started pid fix-login"}},
		{fixture(t, "bg-and-interactive-waiting"), []string{"needs_input_started pid tidy-css"}},
		{fixture(t, "interactive-approved-busy"), []string{"needs_input_ended changed tidy-css"}},
		{fixture(t, "interactive-idle"), nil},
		// The worker is killed: no pid, still blocked. Not an end, and not a new start.
		{fixture(t, "bg-worker-killed"), nil},
		// Started again, and working on it: the wait is over for now.
		{fixture(t, "bg-respawned-working"), []string{"needs_input_ended changed fix-login"}},
		{fixture(t, "bg-respawned-waiting"), []string{"needs_input_started pid fix-login"}},
		{fixture(t, "bg-daemon-killed"), []string{"needs_input_ended gone fix-login"}},
		{fixture(t, "bg-daemon-killed"), nil},
	})
}

// A row with no pid that says blocked is what a dead worker leaves for a few seconds.
// It's raised only when a second look still says so.
func TestWatchPidlessBlockedNeedsTwoLooks(t *testing.T) {
	tr := runSteps(t, []step{
		{fixture(t, "interactive-idle"), []string{"needs_input_started pid fix-login"}},
		{fixture(t, "bg-daemon-killed"), []string{"needs_input_ended gone fix-login"}},
		{fixture(t, "bg-worker-killed"), nil}, // first sighting, no pid: pending
		{fixture(t, "bg-worker-killed"), []string{"needs_input_started seen_twice fix-login"}},
		{fixture(t, "bg-worker-killed"), nil},
		{fixture(t, "bg-worker-killed"), nil}, // still raised, not raised again
		{fixture(t, "bg-respawned-waiting"), nil},
	})
	for i := 0; i < 3; i++ {
		look, ch := tr.Observe(fixture(t, "bg-worker-killed"), at(10+i), 0)
		if len(ch) != 0 || look.Sessions[0].Pending || look.Sessions[0].NeedsInputSince == nil {
			t.Errorf("a raised session that lost its process: %+v %+v", ch, look.Sessions[0])
		}
	}

	// Pending is in the look, and cleared when the row comes back with something else.
	tr = NewTracker()
	look, _ := tr.Observe(fixture(t, "bg-worker-killed"), at(0), 0)
	if !look.Sessions[0].Pending || look.Sessions[0].NeedsInputSince != nil {
		t.Errorf("first pid-less look: %+v", look.Sessions[0])
	}
	if _, ch := tr.Observe(fixture(t, "bg-respawned-working"), at(1), 0); len(ch) != 0 {
		t.Errorf("a pending one was ended: %+v", ch)
	}
	// The count starts again: one more pid-less look isn't two.
	look, ch := tr.Observe(fixture(t, "bg-worker-killed"), at(2), 0)
	if len(ch) != 0 || !look.Sessions[0].Pending {
		t.Errorf("count didn't start again: %+v %+v", ch, look.Sessions[0])
	}
	// A blocked row with a pid raises at once, pending or not.
	look, ch = tr.Observe(fixture(t, "bg-respawned-waiting"), at(3), 0)
	if len(ch) != 1 || ch[0].Why != WhyPID || look.Sessions[1].Pending || look.Sessions[1].NeedsInputSince == nil {
		t.Errorf("with a pid: %+v %+v", ch, look.Sessions)
	}
}

// A host that can't be reached isn't a host where nothing waits, and doesn't end a
// wait: nothing is known until it answers again.
func TestWatchUnreachableIsNotIdle(t *testing.T) {
	tr := NewTracker()
	tr.Observe(fixture(t, "bg-waiting"), at(0), 0)
	down := Host{Host: "devbox", Error: "unreachable: connection refused"}
	for i := 1; i <= 3; i++ {
		look, ch := tr.Observe(down, at(i), 0)
		if len(ch) != 0 {
			t.Errorf("look %d: changes %+v", i, ch)
		}
		if look.Result != ResultUnreachable || look.Sessions != nil || look.Error == "" {
			t.Errorf("look %d: %+v", i, look)
		}
		if look.FailingSince == nil || !look.FailingSince.Equal(at(1)) {
			t.Errorf("failing since %v", look.FailingSince)
		}
		if look.AgeS == nil || *look.AgeS != int64(i*60) || len(look.LastSessions) != 1 || look.LastOKAt == nil || !look.LastOKAt.Equal(at(0)) {
			t.Errorf("look %d: last known %+v", i, look)
		}
		if tr.Failing() != i || look.Failures != i || look.Looks != i+1 {
			t.Errorf("look %d: failing %d, failures %d, looks %d", i, tr.Failing(), look.Failures, look.Looks)
		}
		b, _ := json.Marshal(look)
		if !strings.Contains(string(b), `"sessions":null`) {
			t.Errorf("json: %s", b)
		}
	}
	// Reached but no list: an error, not unreachable, and not empty either.
	look, _ := tr.Observe(Host{Host: "devbox", Reachable: true, Error: "claude not found on this host"}, at(4), 0)
	if look.Result != ResultError || look.Sessions != nil {
		t.Errorf("%+v", look)
	}
	// Back: the wait is still there, so nothing changes; the failures are over.
	look, ch := tr.Observe(fixture(t, "bg-waiting"), at(5), 0)
	if len(ch) != 0 || look.Result != ResultOK || look.FailingSince != nil || tr.Failing() != 0 || look.Failures != 4 || *look.AgeS != 0 {
		t.Errorf("recovery: %+v %+v", ch, look)
	}
	if look.Sessions[0].NeedsInputSince == nil || !look.Sessions[0].NeedsInputSince.Equal(at(0)) {
		t.Errorf("needs input since %v", look.Sessions[0].NeedsInputSince)
	}
	// And when it answers with the session gone, that ends it, with how long.
	_, ch = tr.Observe(fixture(t, "empty"), at(6), 0)
	if len(ch) != 1 || ch[0].Why != WhyGone || ch[0].WaitedS == nil || *ch[0].WaitedS != 360 {
		t.Errorf("end: %+v", ch)
	}
	b, _ := json.Marshal(ch[0])
	if !strings.Contains(string(b), `"type":"change"`) || !strings.Contains(string(b), `"host":"devbox"`) || !strings.Contains(string(b), `"schema":1`) {
		t.Errorf("change json: %s", b)
	}
}

func TestWatchEmptyIsAnAnswer(t *testing.T) {
	look, _ := NewTracker().Observe(fixture(t, "empty"), at(0), 0)
	b, _ := json.Marshal(look)
	if look.Result != ResultOK || look.Sessions == nil || !strings.Contains(string(b), `"sessions":[]`) || !strings.Contains(string(b), `"type":"look"`) {
		t.Errorf("%s", b)
	}
}

// Interactive sessions have no id. They're told apart by sessionId, then pid, then
// what's left, and nothing made up is printed.
func TestWatchSessionsWithoutID(t *testing.T) {
	pid := func(n int64) *int64 { return &n }
	tr := NewTracker()
	two := Host{Host: "h", Reachable: true, Agents: []Agent{
		{Kind: "interactive", Cwd: "/a", PID: pid(1), Status: "waiting", Event: EventNeedsInput},
		{Kind: "interactive", Cwd: "/a", PID: pid(2), Status: "waiting", Event: EventNeedsInput},
	}}
	_, ch := tr.Observe(two, at(0), 0)
	if len(ch) != 2 {
		t.Fatalf("%+v", ch)
	}
	one := Host{Host: "h", Reachable: true, Agents: two.Agents[1:]}
	_, ch = tr.Observe(one, at(1), 0)
	if len(ch) != 1 || ch[0].Why != WhyGone || *ch[0].Session.PID != 1 {
		t.Errorf("%+v", ch)
	}
	// Two rows with nothing to tell them apart are still two.
	same := Host{Host: "h", Reachable: true, Agents: []Agent{
		{Kind: "interactive", Cwd: "/b", Status: "idle", Event: EventIdle},
		{Kind: "interactive", Cwd: "/b", Status: "idle", Event: EventIdle},
	}}
	look, _ := NewTracker().Observe(same, at(0), 0)
	if len(look.Sessions) != 2 {
		t.Errorf("%+v", look.Sessions)
	}
	b, _ := json.Marshal(look)
	for _, made := range []string{"pid:", "row:", "session:", "#2"} {
		if strings.Contains(string(b), made) {
			t.Errorf("an internal key got out: %s", b)
		}
	}
}

func TestNextDelayBounds(t *testing.T) {
	for _, interval := range []time.Duration{15 * time.Second, time.Minute, 5 * time.Minute, 9*time.Minute + 50*time.Second, MaxBackoff, 30 * time.Minute} {
		for failing := 0; failing <= 12; failing++ {
			base := interval
			for i := 1; i < failing && base < MaxBackoff; i++ {
				base = min(base*2, MaxBackoff)
			}
			base = max(base, interval)
			for _, r := range []float64{0, 0.25, 0.5, 0.999999} {
				d := NextDelay(interval, failing, r)
				switch {
				case d < interval:
					t.Errorf("%s, %d failing, r %v: %s is under the interval", interval, failing, r, d)
				case interval >= MaxBackoff && d > interval+interval/10:
					t.Errorf("%s, %d failing, r %v: %s, want at most a tenth over", interval, failing, r, d)
				case interval < MaxBackoff && d > MaxBackoff:
					t.Errorf("%s, %d failing, r %v: %s is past the cap", interval, failing, r, d)
				case base+base/10 <= MaxBackoff && (d < base || d > base+base/10):
					t.Errorf("%s, %d failing, r %v: %s, want %s plus up to a tenth", interval, failing, r, d, base)
				}
			}
		}
	}
	// It does back off, and does jitter.
	if NextDelay(time.Minute, 3, 0) != 4*time.Minute || NextDelay(time.Minute, 0, 0.5) != time.Minute+3*time.Second {
		t.Errorf("%s %s", NextDelay(time.Minute, 3, 0), NextDelay(time.Minute, 0, 0.5))
	}
	if NextDelay(time.Minute, 100, 0) != MaxBackoff {
		t.Errorf("cap: %s", NextDelay(time.Minute, 100, 0))
	}
}

func TestFirstDelayBounds(t *testing.T) {
	for _, interval := range []time.Duration{15 * time.Second, time.Minute, time.Hour} {
		for _, r := range []float64{0, 0.5, 0.999999} {
			d := FirstDelay(interval, r)
			if d < 0 || d > interval/10 || d > 10*time.Second {
				t.Errorf("%s, %v: %s", interval, r, d)
			}
		}
	}
	if FirstDelay(time.Minute, 0.5) != 3*time.Second {
		t.Errorf("no spread: %s", FirstDelay(time.Minute, 0.5))
	}
}

// A raised wait whose row says something nobody knows stays raised: unknown isn't an
// answered prompt. It ends when the row says something known.
func TestWatchUnknownDoesntEndAWait(t *testing.T) {
	pid := func(n int64) *int64 { return &n }
	row := func(state, status string) Host {
		ev, conf := Classify(state, status, "")
		return Host{Host: "h", Reachable: true, Agents: []Agent{{ID: "a", PID: pid(1), State: state, Status: status, Event: ev, Confidence: conf}}}
	}
	tr := NewTracker()
	if _, ch := tr.Observe(row("blocked", "waiting"), at(0), 0); len(ch) != 1 {
		t.Fatalf("%+v", ch)
	}
	look, ch := tr.Observe(row("frobnicating", ""), at(1), 0)
	if len(ch) != 0 || look.Sessions[0].NeedsInputSince == nil || look.Sessions[0].Event != EventUnknown {
		t.Errorf("unknown: %+v %+v", ch, look.Sessions[0])
	}
	if _, ch := tr.Observe(row("blocked", "waiting"), at(2), 0); len(ch) != 0 {
		t.Errorf("raised again: %+v", ch)
	}
	if _, ch := tr.Observe(row("working", "busy"), at(3), 0); len(ch) != 1 || ch[0].Why != WhyChanged {
		t.Errorf("known: %+v", ch)
	}
}
