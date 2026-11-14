package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"meterm/internal/agents"
	"meterm/internal/config"
)

func agentsFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "agents", "testdata", "claude-2.1.323", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeClock moves only when the watch sleeps. It stops the watch after max sleeps,
// counted over every host, or, if until is set, once until says so: with several
// hosts, one host's goroutine can take every sleep before another's has started,
// so a test that needs each host looked at waits for that instead of a count.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	slept  []time.Duration
	max    int
	until  func() bool
	r      float64
	cancel context.CancelFunc
}

func (c *fakeClock) clock() watchClock {
	return watchClock{
		now: func() time.Time {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.t
		},
		sleep: func(ctx context.Context, d time.Duration) bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.slept = append(c.slept, d)
			c.t = c.t.Add(d)
			if c.until != nil && c.until() || c.until == nil && len(c.slept) > c.max || len(c.slept) > 100000 {
				c.cancel()
				return false
			}
			return ctx.Err() == nil
		},
		rand: func() float64 { return c.r },
	}
}

type reply struct {
	stdout string
	stderr string
	code   int // ssh's exit code; 0 is a good run
}

// scripted answers one host's looks in order, and repeats the last one.
func scripted(t *testing.T, replies ...reply) (agentQuery, func() int) {
	var mu sync.Mutex
	n := 0
	q := func(ctx context.Context, r config.Resolved) ([]byte, string, bool, error) {
		mu.Lock()
		rep := replies[min(n, len(replies)-1)]
		n++
		mu.Unlock()
		if rep.code != 0 {
			return nil, rep.stderr, false, exitErr(t, rep.code)
		}
		return []byte(rep.stdout), "", false, nil
	}
	return q, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

func watchLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not one JSON object per line: %q", l)
		}
		if m["schema"] != float64(agents.WatchSchema) {
			t.Fatalf("no schema: %q", l)
		}
		lines = append(lines, m)
	}
	return lines
}

func ofType(lines []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if l["type"] == typ {
			out = append(out, l)
		}
	}
	return out
}

func runWatchTest(t *testing.T, jobs []agentJob, q agentQuery, looks int, format string) (string, *fakeClock) {
	t.Helper()
	return runWatchUntil(t, jobs, q, looks, nil, format)
}

func runWatchUntil(t *testing.T, jobs []agentJob, q agentQuery, looks int, until func() bool, format string) (string, *fakeClock) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fc := &fakeClock{t: time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC), max: looks, until: until, r: 0.5, cancel: cancel}
	var out bytes.Buffer
	err := runAgentsWatch(ctx, &out, jobs, q, watchOptions{interval: time.Minute, timeout: 8 * time.Second, parallel: 4, format: format, clock: fc.clock(), grace: agentsWatchGrace})
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), fc
}

// One line per look, a change when a session starts or stops waiting, and nothing
// else: the stream as a seat reads it.
func TestWatchStreamRaisesAndClears(t *testing.T) {
	q, _ := scripted(t,
		reply{stdout: string(agentsFixture(t, "empty"))},
		reply{stdout: string(agentsFixture(t, "bg-and-interactive-waiting"))},
		reply{stdout: string(agentsFixture(t, "interactive-approved-busy"))},
		reply{stdout: string(agentsFixture(t, "bg-worker-killed"))},
		reply{stdout: string(agentsFixture(t, "bg-daemon-killed"))},
	)
	out, _ := runWatchTest(t, []agentJob{agentJobFor("devbox")}, q, 5, "json")
	lines := watchLines(t, out)
	if lines[0]["type"] != "start" || lines[len(lines)-1]["type"] != "stopped" {
		t.Errorf("start and stop: %s", out)
	}
	looks := ofType(lines, "look")
	if len(looks) != 5 {
		t.Fatalf("%d looks: %s", len(looks), out)
	}
	for i, l := range looks {
		if l["host"] != "devbox" || l["result"] != "ok" || l["looks"] != float64(i+1) || l["failures"] != float64(0) {
			t.Errorf("look %d: %v", i+1, l)
		}
	}
	var got []string
	for _, c := range ofType(lines, "change") {
		s := c["session"].(map[string]any)
		got = append(got, c["change"].(string)+" "+c["why"].(string)+" "+s["name"].(string)+" "+s["kind"].(string))
	}
	want := []string{
		"needs_input_started pid fix-login background",
		"needs_input_started pid tidy-css interactive",
		"needs_input_ended changed tidy-css interactive",
		"needs_input_ended gone fix-login background",
	}
	if !slices.Equal(got, want) {
		t.Errorf("changes:\n%q\nwant\n%q", got, want)
	}
	// The interactive session has no id, and none is made up: it's found by what it has.
	for _, c := range ofType(lines, "change") {
		s := c["session"].(map[string]any)
		if s["kind"] == "interactive" && (s["id"] != nil || s["sessionId"] == nil || s["cwd"] == nil || s["pid"] == nil) {
			t.Errorf("interactive session: %v", s)
		}
	}
	// A change comes before the look it was found in.
	if lines[2]["type"] != "change" || lines[4]["type"] != "look" {
		t.Errorf("order: %s", out)
	}
}

// A host that can't be reached is said to be unreachable, never empty; it's asked
// less often, and as often as before once it's back.
func TestWatchUnreachableBacksOffAndRecovers(t *testing.T) {
	down := reply{code: sshConnectionError, stderr: "ssh: connect to host 192.0.2.1 port 22: Connection timed out"}
	q, asked := scripted(t,
		reply{stdout: string(agentsFixture(t, "bg-waiting"))},
		down, down, down, down, down, down, down,
		reply{stdout: string(agentsFixture(t, "bg-waiting"))},
		reply{stdout: string(agentsFixture(t, "empty"))},
	)
	out, fc := runWatchTest(t, []agentJob{agentJobFor("devbox")}, q, 10, "json")
	lines := watchLines(t, out)
	looks := ofType(lines, "look")
	if len(looks) != 10 || asked() != 10 {
		t.Fatalf("%d looks, %d asked", len(looks), asked())
	}
	for i := 1; i <= 7; i++ {
		l := looks[i]
		if l["result"] != "unreachable" || l["sessions"] != nil || !strings.Contains(l["error"].(string), "timed out") {
			t.Errorf("look %d: %v", i+1, l)
		}
		if l["failing_since"] != looks[1]["looked_at"] || l["last_ok_at"] != looks[0]["looked_at"] || l["last_sessions"] == nil {
			t.Errorf("look %d: since %v, last ok %v", i+1, l["failing_since"], l["last_ok_at"])
		}
		if l["failures"] != float64(i) {
			t.Errorf("look %d: failures %v", i+1, l["failures"])
		}
	}
	if len(ofType(lines, "change")) != 2 {
		t.Errorf("an outage changed something: %v", ofType(lines, "change"))
	}
	// Sleeps: the first spread, then one per look. r is 0.5, so 5% over each.
	want := []time.Duration{3 * time.Second}
	for _, base := range []time.Duration{1, 1, 2, 4, 8, 16, 32, 64, 1, 1} {
		d := min(base*time.Minute*21/20, agents.MaxBackoff-agents.MaxBackoff/20)
		want = append(want, d)
	}
	if !slices.Equal(fc.slept[:len(want)], want) {
		t.Errorf("sleeps %v\nwant   %v", fc.slept, want)
	}
	back := looks[8]
	if back["result"] != "ok" || back["failing_since"] != nil || back["age_s"] != float64(0) {
		t.Errorf("recovery: %v", back)
	}
}

// Every look says when it goes stale: after the next look is due, plus the timeout
// and a grace for waiting behind other hosts.
func TestWatchStaleAfter(t *testing.T) {
	q, _ := scripted(t, reply{stdout: "[]"})
	out, _ := runWatchTest(t, []agentJob{agentJobFor("devbox")}, q, 3, "json")
	for _, l := range ofType(watchLines(t, out), "look") {
		at, _ := time.Parse(time.RFC3339, l["looked_at"].(string))
		next, _ := time.Parse(time.RFC3339, l["next_look_at"].(string))
		stale, _ := time.Parse(time.RFC3339, l["stale_after"].(string))
		if next.Sub(at) != 63*time.Second || stale.Sub(next) != 8*time.Second+agentsWatchGrace {
			t.Errorf("at %s next %s stale %s", at, next, stale)
		}
	}
}

// A production host picked by a tag is said once, as refused, and never asked, look
// after look. Named, it's watched.
func TestWatchNeverAsksProductionUnlessNamed(t *testing.T) {
	cfg := hostsForAgents(t, agentsHosts)
	for _, tc := range []struct {
		names, tags []string
		live        bool
	}{
		{nil, []string{"web"}, false},
		{[]string{"dev1"}, []string{"j"}, false},
		{[]string{"live"}, []string{"web"}, true},
	} {
		jobs, err := selectAgentHosts(cfg, tc.names, tc.tags, nil)
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		asked := map[string]int{}
		q := func(_ context.Context, r config.Resolved) ([]byte, string, bool, error) {
			mu.Lock()
			asked[r.Name]++
			mu.Unlock()
			return []byte("[]"), "", false, nil
		}
		// Until every host that may be asked has been, three times: then a refused one
		// had its chances too.
		until := func() bool {
			mu.Lock()
			defer mu.Unlock()
			for _, j := range jobs {
				if j.refused == "" && asked[j.host.Name] < 3 {
					return false
				}
			}
			return true
		}
		out, _ := runWatchUntil(t, jobs, q, 0, until, "json")
		if !tc.live && asked["live"] > 0 {
			t.Errorf("%v %v: production was asked %d times", tc.names, tc.tags, asked["live"])
		}
		for _, j := range jobs {
			if j.refused != "" && asked[j.host.Name] > 0 {
				t.Errorf("%v %v: %s was asked", tc.names, tc.tags, j.label)
			}
		}
		if tc.live && asked["live"] == 0 {
			t.Errorf("named production wasn't watched")
		}
		refused := 0
		for _, l := range ofType(watchLines(t, out), "look") {
			if l["result"] == "refused" {
				refused++
				if l["looks"] != float64(0) || l["sessions"] != nil || l["looked_at"] != nil || l["stale_after"] != nil {
					t.Errorf("refused: %v", l)
				}
			}
		}
		if !tc.live && refused == 0 {
			t.Errorf("%v %v: no refused line", tc.names, tc.tags)
		}
	}
}

// agents_watch: false keeps --watch away from the host; a single look still asks it.
func TestWatchOptOut(t *testing.T) {
	cfg := hostsForAgents(t, `version: 1
groups:
  sleepy: {agents_watch: false}
hosts:
- {name: a, address: 192.0.2.1, tags: [t]}
- {name: b, address: 192.0.2.2, tags: [t], agents_watch: false}
- {name: c, address: 192.0.2.3, tags: [t], group: sleepy}
- {name: d, address: 192.0.2.4, tags: [t], group: sleepy, agents_watch: true}
`)
	jobs, err := selectAgentHosts(cfg, nil, []string{"t"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	asked := map[string]int{}
	q := func(_ context.Context, r config.Resolved) ([]byte, string, bool, error) {
		mu.Lock()
		asked[r.Name]++
		mu.Unlock()
		return []byte("[]"), "", false, nil
	}
	// Until a and d have each been asked three times: b and c had as long.
	until := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return asked["a"] >= 3 && asked["d"] >= 3
	}
	out, _ := runWatchUntil(t, jobs, q, 0, until, "json")
	mu.Lock()
	if asked["a"] < 3 || asked["b"] != 0 || asked["c"] != 0 || asked["d"] < 3 {
		t.Errorf("asked %v", asked)
	}
	mu.Unlock()
	opted := 0
	for _, l := range ofType(watchLines(t, out), "look") {
		if l["result"] == "opted_out" {
			opted++
		}
	}
	if opted != 2 {
		t.Errorf("%d opted_out lines: %s", opted, out)
	}
	rep := runAgents(context.Background(), jobs, q, time.Second, 2)
	if agentsFailed(rep) != 0 || asked["b"] != 1 {
		t.Errorf("a single look skipped an opted-out host: %+v", rep)
	}
}

// Stopping mid-look ends at once, says so, and prints no half-made look.
func TestWatchStopsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 1)
	q := func(ctx context.Context, _ config.Resolved) ([]byte, string, bool, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, "", false, ctx.Err()
	}
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runAgentsWatch(ctx, &out, []agentJob{agentJobFor("a"), agentJobFor("b")}, q,
			watchOptions{interval: time.Minute, timeout: 8 * time.Second, parallel: 1, format: "json", clock: realWatchClock})
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("didn't stop")
	}
	lines := watchLines(t, out.String())
	if len(ofType(lines, "look")) != 0 || lines[len(lines)-1]["type"] != "stopped" {
		t.Errorf("%s", out.String())
	}
}

// A reader that went away stops the watch, with the error.
func TestWatchStopsWhenStdoutFails(t *testing.T) {
	q, _ := scripted(t, reply{stdout: "[]"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fc := &fakeClock{t: time.Now(), max: 1000, cancel: cancel}
	err := runAgentsWatch(ctx, failingWriter{}, []agentJob{agentJobFor("a")}, q,
		watchOptions{interval: time.Minute, timeout: time.Second, parallel: 1, format: "json", clock: fc.clock()})
	if err == nil {
		t.Error("no error")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, syscall.EPIPE }

// The readable form: a line when something changes, not every look, with nothing a
// host sent getting out as it came.
func TestWatchTextForm(t *testing.T) {
	const bad = "x\x1b]52;c;aGk=\x07\x1b[2J\u202e\u200b\ny"
	enc, _ := json.Marshal(bad)
	hostile := `[{"pid":1,"kind":"background","id":` + string(enc) + `,"cwd":` + string(enc) + `,"name":` + string(enc) + `,"state":"blocked","status":"waiting","waitingFor":` + string(enc) + `}]`
	q, _ := scripted(t,
		reply{stdout: "[]"}, reply{stdout: "[]"},
		reply{stdout: hostile}, reply{stdout: hostile},
		reply{code: sshConnectionError, stderr: "Connection refused"}, reply{code: sshConnectionError, stderr: "Connection refused"},
		reply{stdout: "[]"},
	)
	out, _ := runWatchTest(t, []agentJob{agentJobFor(bad)}, q, 7, "text")
	for _, c := range []string{"\x1b", "\x07", "\u202e", "\u200b"} {
		if strings.Contains(out, c) {
			t.Errorf("%q got out: %q", c, out)
		}
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// start, no agents, 1 session + NEEDS INPUT, unreachable, no agents + gone, stopped
	if len(lines) != 8 {
		t.Errorf("%d lines:\n%s", len(lines), out)
	}
	for _, want := range []string{"watching 1 host", "no agents", "NEEDS INPUT: background", "1 waiting for you", "unreachable since", "next look", "gone, was waiting", "stopped"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

func TestAgentsWatchFlags(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(cfgFile, []byte(agentsHosts+"- {name: off, address: 192.0.2.50, agents_watch: false}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METERM_CONFIG", cfgFile)
	argvFile := fakeSSHScript(t, `echo '[]'`)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"dev1", "--interval", "30s"}, "goes with --watch"},
		{[]string{"dev1", "--watch", "--interval", "14s"}, "15s or more"},
		{[]string{"dev1", "--watch", "--interval", "15s", "--timeout", "15s"}, "shorter than --interval"},
		{[]string{"--watch", "--tag", "web", "--group", "live-group"}, ""}, // has dev1: would run
		{[]string{"--watch"}, "say which hosts"},
		{[]string{"off", "--watch"}, "nothing to watch"},
		{[]string{"--watch", "--tag", "inh"}, "nothing to watch"},
	} {
		if tc.want == "" {
			continue
		}
		c := newAgentsCmd()
		c.SetArgs(tc.args)
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.args, err, tc.want)
		}
	}
	if _, err := os.Stat(argvFile); !os.IsNotExist(err) {
		t.Error("ssh ran")
	}
}

// The real command, as a seat runs it: its own process, ssh and all, stopped by
// SIGTERM. It exits 0, says it stopped, and leaves nothing in the temp directory.
func TestAgentsWatchProcessStopsOnSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("agents refuses to run on Windows")
	}
	fix := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(fix, agentsFixture(t, "bg-waiting"), 0o600); err != nil {
		t.Fatal(err)
	}
	argvFile := fakeSSHScript(t, `cat '`+fix+`'`)
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "hosts.yaml")
	if err := os.WriteFile(cfgFile, []byte(agentsHosts), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), runAgentsWatchEnv+"=dev1\x1fdev2\x1f--watch\x1f--interval\x1f15s\x1f--format\x1fjson",
		"METERM_CONFIG="+cfgFile, "TMPDIR="+tmp)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stdout)
	var lines []string
	looks := 0
	for looks < 2 && sc.Scan() {
		lines = append(lines, sc.Text())
		if strings.Contains(sc.Text(), `"type":"look"`) {
			looks++
		}
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	err = cmd.Wait()
	if err != nil {
		t.Errorf("exit: %v\n%s", err, strings.Join(lines, "\n"))
	}
	if len(lines) == 0 || !strings.Contains(lines[len(lines)-1], `"type":"stopped"`) {
		t.Errorf("last line: %q", lines)
	}
	if n := strings.Count(strings.Join(lines, "\n"), `"change":"needs_input_started"`); n != 2 {
		t.Errorf("%d raised, want one per host:\n%s", n, strings.Join(lines, "\n"))
	}
	left, _ := os.ReadDir(tmp)
	if len(left) != 0 {
		t.Errorf("left in the temp directory: %v", left)
	}
	b, _ := os.ReadFile(argvFile)
	if !strings.Contains(string(b), "ControlMaster=no") || strings.Count(string(b), agents.RemoteCommand) != 2 {
		t.Errorf("ssh argv: %s", b)
	}
}

// What askAgents says, as the watch sorts it: only "ssh didn't get in" is unreachable.
func TestWatchResultOfEachFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		job    agentJob
		stderr string
		err    error
		want   string
	}{
		{"refused", agentJobFor("h"), "ssh: connect to host x port 22: Connection refused", exitErr(t, sshConnectionError), agents.ResultUnreachable},
		{"key", agentJobFor("h"), "x: Permission denied (publickey).", exitErr(t, sshConnectionError), agents.ResultUnreachable},
		{"banner", agentJobFor("h"), "\x1b[2J", exitErr(t, sshConnectionError), agents.ResultUnreachable},
		{"no claude", agentJobFor("h"), "", exitErr(t, agents.ExitNoClaude), agents.ResultError},
		{"claude failed", agentJobFor("h"), "", exitErr(t, 1), agents.ResultError},
		{"no ssh", agentJobFor("h"), "", errors.New("exec: not found"), agents.ResultError},
		{"unsafe", agentJob{label: "h", resolve: func(context.Context) config.Resolved {
			return config.Resolved{Address: "192.0.2.1", ExtraArgs: []string{"-A"}}
		}}, "", nil, agents.ResultError},
	} {
		q := func(context.Context, config.Resolved) ([]byte, string, bool, error) {
			return nil, tc.stderr, false, tc.err
		}
		ctx, cancel := context.WithCancel(context.Background())
		fc := &fakeClock{t: time.Now(), max: 1, cancel: cancel}
		var out bytes.Buffer
		if err := runAgentsWatch(ctx, &out, []agentJob{tc.job}, q, watchOptions{interval: time.Minute, timeout: time.Second, parallel: 1, format: "json", clock: fc.clock()}); err != nil {
			t.Fatal(err)
		}
		cancel()
		looks := ofType(watchLines(t, out.String()), "look")
		if len(looks) != 1 || looks[0]["result"] != tc.want || looks[0]["sessions"] != nil {
			t.Errorf("%s: %v", tc.name, looks)
		}
	}
	// A timeout is the host not answering.
	q := func(ctx context.Context, _ config.Resolved) ([]byte, string, bool, error) {
		<-ctx.Done()
		return nil, "", false, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fc := &fakeClock{t: time.Now(), max: 1, cancel: cancel}
	var out bytes.Buffer
	if err := runAgentsWatch(ctx, &out, []agentJob{agentJobFor("h")}, q, watchOptions{interval: time.Minute, timeout: 10 * time.Millisecond, parallel: 1, format: "json", clock: fc.clock()}); err != nil {
		t.Fatal(err)
	}
	if l := ofType(watchLines(t, out.String()), "look"); len(l) != 1 || l[0]["result"] != agents.ResultUnreachable {
		t.Errorf("timeout: %v", l)
	}
}

// A host that refused the watcher key isn't asked again in this run: each try is a
// failed login there, and enough of those get an address banned.
func TestWatchStopsAskingWhenTheWatcherKeyIsRefused(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		var mu sync.Mutex
		asked := map[string]int{}
		q := func(_ context.Context, r config.Resolved) ([]byte, string, bool, error) {
			mu.Lock()
			asked[r.Address]++
			mu.Unlock()
			if r.Address == "192.0.2.1" {
				return nil, "agent@192.0.2.1: Permission denied (publickey).", false, errWatcherKeyRefused
			}
			return []byte("[]"), "", false, nil
		}
		other := agentJob{label: "other", resolve: func(context.Context) config.Resolved {
			return config.Resolved{Name: "other", Address: "192.0.2.2"}
		}}
		out := &syncBuffer{}
		ctx, cancel := context.WithCancel(context.Background())
		fc := &fakeClock{t: time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC), r: 0.5, cancel: cancel}
		// Until devbox's line is out and the other host has had five looks since:
		// devbox had as long to be asked again.
		mark := -1
		fc.until = func() bool {
			mu.Lock()
			defer mu.Unlock()
			if mark < 0 && strings.Contains(out.String(), "watcher key") {
				mark = asked["192.0.2.2"]
			}
			return mark >= 0 && asked["192.0.2.2"] >= mark+5
		}
		if err := runAgentsWatch(ctx, out, []agentJob{agentJobFor("devbox"), other}, q,
			watchOptions{interval: time.Minute, timeout: time.Second, parallel: 2, format: format, clock: fc.clock()}); err != nil {
			t.Fatal(err)
		}
		cancel()
		if asked["192.0.2.1"] != 1 {
			t.Errorf("%s: devbox asked %d times", format, asked["192.0.2.1"])
		}
		if format == "text" {
			if !strings.Contains(out.String(), "not asking it again") {
				t.Errorf("text:\n%s", out.String())
			}
			continue
		}
		var mine []map[string]any
		for _, l := range ofType(watchLines(t, out.String()), "look") {
			if l["host"] == "devbox" {
				mine = append(mine, l)
			}
		}
		if len(mine) != 1 || mine[0]["stopped_asking"] != true || mine[0]["result"] != "error" ||
			mine[0]["next_look_at"] != nil || mine[0]["stale_after"] != nil || !strings.Contains(mine[0]["error"].(string), "watcher key") {
			t.Errorf("%v", mine)
		}
	}
}

// With every host stopped, the watch doesn't end by itself: a seat that starts it again
// whenever it ends would bring the failed logins back.
func TestWatchWithNothingLeftToAskKeepsRunning(t *testing.T) {
	tries := 0
	q := func(context.Context, config.Resolved) ([]byte, string, bool, error) {
		tries++
		return nil, "", false, errWatcherKeyRefused
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	var out bytes.Buffer
	if err := runAgentsWatch(ctx, &out, []agentJob{agentJobFor("devbox")}, q,
		watchOptions{interval: 10 * time.Millisecond, timeout: 100 * time.Millisecond, parallel: 1, format: "json", clock: realWatchClock}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 250*time.Millisecond || tries != 1 {
		t.Errorf("returned after %s, %d tries", d, tries)
	}
}

// syncBuffer is a bytes.Buffer a test can read while the watch writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// stale_after holds when there are more hosts than -j slots and every look is slow:
// a look's next line comes before its stale_after, queue and all.
func TestWatchStaleAfterCoversTheQueue(t *testing.T) {
	q := func(ctx context.Context, _ config.Resolved) ([]byte, string, bool, error) {
		select {
		case <-time.After(150 * time.Millisecond):
		case <-ctx.Done():
		}
		return []byte("[]"), "", false, ctx.Err()
	}
	var jobs []agentJob
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		jobs = append(jobs, agentJobFor(n))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := runAgentsWatch(ctx, &out, jobs, q, watchOptions{interval: 10 * time.Millisecond, timeout: 200 * time.Millisecond,
		parallel: 1, format: "json", clock: realWatchClock}); err != nil {
		t.Fatal(err)
	}
	last := map[string]time.Time{}
	checked := 0
	for _, l := range ofType(watchLines(t, out.String()), "look") {
		h := l["host"].(string)
		at, _ := time.Parse(time.RFC3339Nano, l["looked_at"].(string))
		if stale, ok := last[h]; ok {
			checked++
			if at.After(stale) {
				t.Errorf("%s: a line at %s, after the last one's stale_after %s", h, at, stale)
			}
		}
		if l["stale_after"] != nil {
			last[h], _ = time.Parse(time.RFC3339Nano, l["stale_after"].(string))
		}
	}
	if checked < 5 {
		t.Errorf("only %d pairs:\n%s", checked, out.String())
	}
}
