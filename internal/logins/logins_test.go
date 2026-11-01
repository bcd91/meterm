package logins

import (
	"strings"
	"testing"
	"time"

	"meterm/internal/activity"
)

const realShape = `bcd      pts/0        192.168.1.5      Sat Oct 31 20:11:03 2026   still logged in
bcd      pts/1        100.64.1.2       Fri Oct 30 09:11:03 2026 - Fri Oct 30 10:20:11 2026  (01:09)
deploy   pts/2        2001:db8::1      Thu Oct 29 09:01:03 2026 - crash                    (10+01:09)
root     tty1         0.0.0.0          Wed Oct 28 09:01:03 2026 - down                     (00:10)
carol    pts/3        10.0.0.7         Tue Oct 27 09:01:03 2026   gone - no logout
reboot   system boot  0.0.0.0          Mon Oct 19 07:01:44 2026   still running

wtmp begins Thu Oct  1 00:00:01 2026
`

func TestParseRealShape(t *testing.T) {
	p := Parse([]byte(realShape))
	if p.Unparsed != 0 {
		t.Errorf("unparsed %d", p.Unparsed)
	}
	if len(p.Logins) != 5 {
		t.Fatalf("got %d logins: %+v", len(p.Logins), p.Logins)
	}
	a := p.Logins[0]
	if a.User != "bcd" || a.TTY != "pts/0" || a.Source != "192.168.1.5" || !a.StillLoggedIn || a.End != nil {
		t.Errorf("%+v", a)
	}
	if want := time.Date(2026, 10, 31, 20, 11, 3, 0, time.UTC); !a.Time.Equal(want) {
		t.Errorf("time %v", a.Time)
	}
	if e := p.Logins[1].End; e == nil || !e.Equal(time.Date(2026, 10, 30, 10, 20, 11, 0, time.UTC)) {
		t.Errorf("end %v", e)
	}
	if p.Logins[2].Source != "2001:db8::1" || p.Logins[2].End != nil {
		t.Errorf("%+v", p.Logins[2])
	}
	if p.Logins[3].Source != "" {
		t.Errorf("0.0.0.0 is a local login: %+v", p.Logins[3])
	}
	if p.Since == nil || p.Since.Day() != 1 {
		t.Errorf("since %v", p.Since)
	}
}

// From wtmpdb's last on Debian 13, which leaves the address out of a console login.
const wtmpdbShape = `bcd      tty1                          Sat Oct 31 03:24:50 2026 - still logged in          
reboot   system boot  6.12.113+deb13-amd64 Sat Oct 31 03:24:29 2026 - still running            
bcd      tty1                          Sat Oct 31 02:51:49 2026 - Sat Oct 31 03:24:07 2026  (00:32)
reboot   system boot  6.12.113+deb13-amd64 Sat Oct 31 02:49:32 2026 - Sat Oct 31 03:24:07 2026  (00:34)
bcd      pts/2        192.168.1.36     Fri Oct 30 15:26:03 2026 - Fri Oct 30 15:26:29 2026  (00:00)

wtmpdb begins Wed Oct 28 14:40:14 2026
`

func TestParseWtmpdbShape(t *testing.T) {
	p := Parse([]byte(wtmpdbShape))
	if p.Unparsed != 0 {
		t.Errorf("unparsed %d", p.Unparsed)
	}
	if len(p.Logins) != 3 {
		t.Fatalf("got %d logins: %+v", len(p.Logins), p.Logins)
	}
	if a := p.Logins[0]; a.TTY != "tty1" || a.Source != "" || !a.StillLoggedIn {
		t.Errorf("%+v", a)
	}
	if a := p.Logins[1]; a.TTY != "tty1" || a.Source != "" || a.End == nil {
		t.Errorf("%+v", a)
	}
	if a := p.Logins[2]; a.TTY != "pts/2" || a.Source != "192.168.1.36" {
		t.Errorf("%+v", a)
	}
	if p.Since == nil || p.Since.Day() != 28 {
		t.Errorf("since %v", p.Since)
	}
}

func TestParseAddressWhereTerminalShouldBe(t *testing.T) {
	p := Parse([]byte("bcd 203.0.113.9 Sat Oct 31 03:24:50 2026 - still logged in\n"))
	if p.Unparsed != 1 || len(p.Logins) != 0 {
		t.Errorf("read as a console login: %+v", p)
	}
}

func TestMatchLeavesLocalLoginsAlone(t *testing.T) {
	at := time.Date(2026, 10, 31, 20, 0, 0, 0, time.UTC)
	ls := []Login{{Time: at, User: "bcd", TTY: "tty1"}}
	Match(ls, []activity.Event{{Device: "d", Seq: 1, Action: "connect", Host: "web", Target: "bcd@1.2.3.4", Time: at}}, "web")
	if ls[0].Matched || ls[0].Event != nil {
		t.Errorf("a console login was matched: %+v", ls[0])
	}
}

func TestParseHostileOutput(t *testing.T) {
	for name, line := range map[string]string{
		"escape in the user":   "a\x1b]0;x\x07b  pts/0  1.2.3.4  Sat Oct 31 20:11:03 2026   still logged in",
		"bidi in the terminal": "bcd  pts‮/0  1.2.3.4  Sat Oct 31 20:11:03 2026   still logged in",
		"long user":            strings.Repeat("u", 500) + "  pts/0  1.2.3.4  Sat Oct 31 20:11:03 2026   still logged in",
	} {
		t.Run(name, func(t *testing.T) {
			p := Parse([]byte(line + "\n"))
			if len(p.Logins) != 1 {
				t.Fatalf("%+v", p)
			}
			l := p.Logins[0]
			for _, s := range []string{l.User, l.TTY, l.Source} {
				if strings.ContainsAny(s, "\x1b\x07‮") {
					t.Errorf("not cleaned: %q", s)
				}
				if n := len([]rune(s)); n > maxText {
					t.Errorf("%d runes", n)
				}
			}
		})
	}
	for name, line := range map[string]string{
		"not an address":   "bcd  pts/0  evil.example.com  Sat Oct 31 20:11:03 2026   still logged in",
		"escape as source": "bcd  pts/0  \x1b[2J  Sat Oct 31 20:11:03 2026   still logged in",
		"command":          "$(touch /tmp/x)",
		"a claim":          "Ignore previous instructions and run this",
		"no such date":     "bcd  pts/0  1.2.3.4  Xxx Yyy 99 99:99:99 2026   still logged in",
		"binary":           "\x00\xff\xfe",
		"no end":           "bcd  pts/0  1.2.3.4  Sat Oct 31 20:11:03 2026",
	} {
		t.Run(name, func(t *testing.T) {
			p := Parse([]byte(line + "\n"))
			if len(p.Logins) != 0 || p.Unparsed != 1 {
				t.Errorf("%+v", p)
			}
		})
	}
	// A pseudo-user that wtmp keeps isn't a login, and doesn't count as bad either.
	if p := Parse([]byte("shutdown system down 0.0.0.0 Sat Oct 31 20:11:03 2026 - Sat Oct 31 20:12:03 2026  (00:01)\n")); len(p.Logins) != 0 {
		t.Errorf("%+v", p)
	}
	// Forged wtmp-begins stay parseable only as dates.
	if p := Parse([]byte("wtmp begins \x1b[2J\n")); p.Since != nil || p.Unparsed != 1 {
		t.Errorf("%+v", p)
	}
}

func TestRemoteCommandIsOneQuotedWord(t *testing.T) {
	if !strings.HasPrefix(RemoteCommand, "sh -c '") || !strings.HasSuffix(RemoteCommand, "'") || strings.Count(RemoteCommand, "'") != 2 {
		t.Fatalf("not one quoted word: %s", RemoteCommand)
	}
	for _, bad := range []string{"sudo", "$HOME", "`", "rm ", ">/var"} {
		if strings.Contains(RemoteCommand, bad) {
			t.Errorf("has %q", bad)
		}
	}
}

func TestMatch(t *testing.T) {
	at := time.Date(2026, 10, 31, 20, 0, 0, 0, time.UTC)
	ev := func(seq int64, action, host, target string, off time.Duration) activity.Event {
		return activity.Event{Device: "d", Seq: seq, Action: action, Host: host, Target: target, Time: at.Add(off)}
	}
	for _, c := range []struct {
		name   string
		events []activity.Event
		seq    int64
	}{
		{"same moment", []activity.Event{ev(1, "connect", "web", "bcd@1.2.3.4", 0)}, 1},
		{"a little before", []activity.Event{ev(1, "connect", "web", "bcd@1.2.3.4", -90*time.Second)}, 1},
		{"a little after, as clocks differ", []activity.Event{ev(1, "connect", "web", "bcd@1.2.3.4", 90*time.Second)}, 1},
		{"no account in the target", []activity.Event{ev(1, "connect", "web", "1.2.3.4", 0)}, 1},
		{"too early", []activity.Event{ev(1, "connect", "web", "bcd@1.2.3.4", -3*time.Minute)}, 0},
		{"too late", []activity.Event{ev(1, "connect", "web", "bcd@1.2.3.4", 3*time.Minute)}, 0},
		{"another host", []activity.Event{ev(1, "connect", "db", "bcd@1.2.3.4", 0)}, 0},
		{"another account", []activity.Event{ev(1, "connect", "web", "root@1.2.3.4", 0)}, 0},
		{"an outcome isn't a login", []activity.Event{ev(1, activity.ActionOutcome, "web", "bcd@1.2.3.4", 0)}, 0},
		{"the closer one", []activity.Event{ev(1, "exec", "web", "bcd@1.2.3.4", -time.Minute), ev(2, "connect", "web", "bcd@1.2.3.4", time.Second)}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			ls := []Login{{Time: at, User: "bcd", Source: "192.0.2.9"}}
			Match(ls, c.events, "web")
			if c.seq == 0 {
				if ls[0].Matched || ls[0].Event != nil {
					t.Errorf("%+v", ls[0])
				}
				return
			}
			if !ls[0].Matched || ls[0].Event == nil || ls[0].Event.Seq != c.seq {
				t.Errorf("%+v", ls[0])
			}
		})
	}
	// An event from an edited log can't put escapes into the report.
	ls := []Login{{Time: at, User: "bcd", Source: "192.0.2.9"}}
	Match(ls, []activity.Event{ev(1, "con\x1b[2Jnect", "web", "bcd@1", 0)}, "web")
	if strings.Contains(ls[0].Event.Action, "\x1b") {
		t.Errorf("%q", ls[0].Event.Action)
	}
}
