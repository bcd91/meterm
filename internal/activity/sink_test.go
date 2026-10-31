package activity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sinkLog(t *testing.T) (*Log, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("METERM_ACTIVITY_DIR", dir)
	sinkGaveUp.Store(false)
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return l, filepath.Join(t.TempDir(), "copy.jsonl")
}

func TestParseSink(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "x")
	for _, c := range []struct {
		in, kind string
		bad      bool
	}{
		{"syslog", SinkKindSyslog, false},
		{abs, SinkKindFile, false},
		{"relative/path", "", true},
		{"~/x", "", true},
		{"SYSLOG", "", true},
		{"", "", true},
		{abs + "\x1b[2J", "", true},
		{abs + "\n", "", true},
		{"|rm -rf /", "", true},
		{"$(touch x)", "", true},
	} {
		kind, _, err := ParseSink(c.in)
		if (err != nil) != c.bad || kind != c.kind {
			t.Errorf("ParseSink(%q) = %q, %v", c.in, kind, err)
		}
	}
}

func TestFileSinkKeepsACopyAndCatchesUp(t *testing.T) {
	l, path := sinkLog(t)
	all := appendN(t, l, 2)
	SendToSink(path)
	if rep := l.CheckSink(path); !rep.Healthy() || rep.SentSeq != 2 || rep.Copy != "compared" {
		t.Fatalf("after sending: %+v", rep)
	}
	copied, err := readLogFile(path)
	if err != nil || len(copied.events) != 2 || copied.events[1].Hash != all[1].Hash {
		t.Fatalf("copy: %+v, %v", copied, err)
	}
	if broken := Verify(copied.events); broken != nil {
		t.Fatalf("the copy doesn't verify by itself: %v", broken)
	}
	// Sending again adds nothing.
	SendToSink(path)
	if c, _ := readLogFile(path); len(c.events) != 2 {
		t.Fatalf("resent: %d", len(c.events))
	}
	// It fails, events go on being written, and then it catches up.
	blocked := filepath.Join(t.TempDir(), "missing-dir", "copy.jsonl")
	appendN(t, l, 1)
	SendToSink(blocked)
	if rep := l.CheckSink(blocked); rep.State != "failing" || rep.LastError == "" || rep.FailingSince == nil {
		t.Fatalf("failing: %+v", rep)
	}
	if err := os.MkdirAll(filepath.Dir(blocked), 0o700); err != nil {
		t.Fatal(err)
	}
	SendToSink(blocked)
	rep := l.CheckSink(blocked)
	if !rep.Healthy() || rep.FailingSince != nil || rep.LastError != "" || rep.SentSeq != 3 {
		t.Fatalf("recovered: %+v", rep)
	}
}

// A sink that can't work costs nothing but a line in --verify, and the log goes on.
func TestFailingSinksNeverBlock(t *testing.T) {
	notRegular := t.TempDir() // a directory
	for name, sink := range map[string]string{
		"a directory": notRegular,
		"nonsense":    "not a sink",
		"hostile":     "/x\x1b]0;evil\x07",
	} {
		t.Run(name, func(t *testing.T) {
			l, _ := sinkLog(t)
			appendN(t, l, 1)
			began := time.Now()
			SendToSink(sink)
			if d := time.Since(began); d > 3*sinkTimeout {
				t.Errorf("took %s", d)
			}
			if rep := l.CheckSink(sink); rep.Healthy() {
				t.Errorf("reads as healthy: %+v", rep)
			}
			appendN(t, l, 1)
			if n, broken, err := l.VerifyFile(); n != 2 || broken != nil || err != nil {
				t.Errorf("the log suffered: %d, %v, %v", n, broken, err)
			}
			if b, err := os.ReadFile(l.sinkStatePath()); err == nil && strings.ContainsAny(string(b), "\x1b\x07") {
				t.Errorf("state holds raw escapes: %q", b)
			}
		})
	}
}

// Someone else sending isn't a failure, and doesn't hold this up for long.
func TestSinkLockHeldElsewhere(t *testing.T) {
	l, path := sinkLog(t)
	appendN(t, l, 1)
	release := holdSinkLock(t, l)
	defer release()
	began := time.Now()
	SendToSink(path)
	if d := time.Since(began); d > 2*sinkTimeout {
		t.Errorf("first send took %s", d)
	}
	if st := l.readSinkState(path); !st.FailingSince.IsZero() {
		t.Errorf("someone else sending isn't a failure: %+v", st)
	}
}

func TestCheckSinkFindsWhatWasDone(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(t *testing.T, l *Log, path string)
		want string
	}{
		{"local line removed", func(t *testing.T, l *Log, _ string) {
			rewrite(t, l, func(ls []string) []string { return ls[:len(ls)-1] })
		}, "removed from this log"},
		{"local chain rebuilt, head too", func(t *testing.T, l *Log, _ string) { rebuildChain(t, l) }, "changed in this log"},
		{"copy emptied", func(t *testing.T, _ *Log, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "gone from the copy"},
		{"copy deleted", func(t *testing.T, _ *Log, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, "gone from the copy"},
		{"this log's directory replaced", func(t *testing.T, l *Log, _ string) {
			for _, f := range []string{l.eventsPath(), l.headPath(), l.devicePath(), l.sinkStatePath()} {
				if err := os.Remove(f); err != nil {
					t.Fatal(err)
				}
			}
		}, "another device"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, path := sinkLog(t)
			appendN(t, l, 3)
			SendToSink(path)
			c.edit(t, l, path)
			if c.name == "this log's directory replaced" {
				appendN(t, l, 1)
			}
			rep := l.CheckSink(path)
			if rep.State != "differs" {
				t.Fatalf("state %q: %+v", rep.State, rep)
			}
			found := false
			for _, d := range rep.Differences {
				found = found || strings.Contains(d.Reason, c.want)
			}
			if !found {
				t.Errorf("no %q in %+v", c.want, rep.Differences)
			}
		})
	}
}

// Behind is a note: a send may be under way, and it isn't a problem to exit on.
func TestBehindIsReportedAndHealthy(t *testing.T) {
	l, path := sinkLog(t)
	appendN(t, l, 1)
	SendToSink(path)
	appendN(t, l, 1)
	rep := l.CheckSink(path)
	if rep.State != "behind" || !rep.Healthy() {
		t.Fatalf("%+v", rep)
	}
	for _, state := range []string{"failing", "differs"} {
		rep.State = state
		if rep.Healthy() {
			t.Errorf("%s reads as healthy", state)
		}
	}
}

func TestNewSinkStartsFromTheBeginning(t *testing.T) {
	l, a := sinkLog(t)
	appendN(t, l, 2)
	SendToSink(a)
	b := filepath.Join(t.TempDir(), "other.jsonl")
	SendToSink(b)
	if c, _ := readLogFile(b); len(c.events) != 2 {
		t.Fatalf("new sink got %d", len(c.events))
	}
}

func TestSyslogLineIsBounded(t *testing.T) {
	line, err := syslogLine(Event{Action: "exec", Detail: strings.Repeat("d", 4000)})
	if err != nil {
		t.Fatal(err)
	}
	if len(line) > 1500 {
		t.Fatalf("%d bytes", len(line))
	}
}

// Copying the log onto itself would write events into the log that's being copied.
func TestSinkCantBeTheLog(t *testing.T) {
	l, _ := sinkLog(t)
	appendN(t, l, 2)
	SendToSink(l.eventsPath())
	if n, broken, err := l.VerifyFile(); n != 2 || broken != nil || err != nil {
		t.Fatalf("the log itself changed: %d, %v, %v", n, broken, err)
	}
	if rep := l.CheckSink(l.eventsPath()); rep.State != "failing" {
		t.Errorf("%+v", rep)
	}
}

// What a failure says goes to a terminal later, and an OS error can quote anything.
func TestSinkFailureTextIsCleanedAndBounded(t *testing.T) {
	l, path := sinkLog(t)
	l.noteSinkFailure(path, errors.New("a\x1b]0;x\x07b\u202e"+strings.Repeat("y", 1000)))
	rep := l.CheckSink(path)
	if strings.ContainsAny(rep.LastError, "\x1b\x07\u202e") || len([]rune(rep.LastError)) > 300 {
		t.Fatalf("%q", rep.LastError)
	}
}
