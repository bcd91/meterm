package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"meterm/internal/activity"
	"meterm/internal/cellwidth"
)

func longEvents() []activity.Event {
	at := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	return []activity.Event{
		{Time: at, User: "me@box", Action: "sftp", Host: "web1",
			Detail: "copy /srv/app/releases/2026-11-11/build/artifacts/bundle.tar.gz -> /home/me/Downloads/bundle.tar.gz: failed: connection reset by peer"},
		{Time: at, User: "me@box", Action: "connect", Host: "น้ำ-web", Detail: "session ended, exit 0 after 3s"},
	}
}

// Not on a terminal, nothing is cut: the end of a detail is often how it
// ended. On one, the detail gets the rest of the line, not 60 runes.
func TestActivityTextWidth(t *testing.T) {
	ev := longEvents()
	var b bytes.Buffer
	printEvents(&b, ev, 0, false)
	if !strings.Contains(b.String(), "connection reset by peer") {
		t.Errorf("piped output cut the detail:\n%s", b.String())
	}
	b.Reset()
	printEvents(&b, ev, 250, false)
	if !strings.Contains(b.String(), "connection reset by peer") {
		t.Errorf("250 columns cut the detail:\n%s", b.String())
	}
	for _, w := range []int{100, 60} {
		b.Reset()
		printEvents(&b, ev, w, false)
		for _, l := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
			if cellwidth.Width(l) > w {
				t.Errorf("width %d: line is %d wide: %q", w, cellwidth.Width(l), l)
			}
		}
	}
	// Rows line up by cells, Thai included: the detail column starts at the
	// same cell on every row.
	b.Reset()
	printEvents(&b, ev, 0, false)
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	col := func(l, sub string) int { return cellwidth.Width(l[:strings.Index(l, sub)]) }
	if a, c := col(lines[1], "copy"), col(lines[2], "session"); a != c {
		t.Errorf("detail starts at %d and %d:\n%s", a, c, b.String())
	}
}

// --full wraps instead of cutting: every word is there, no line too wide.
func TestActivityFull(t *testing.T) {
	var b bytes.Buffer
	printEvents(&b, longEvents(), 80, true)
	out := b.String()
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if cellwidth.Width(l) > 80 {
			t.Errorf("line is %d wide: %q", cellwidth.Width(l), l)
		}
	}
	if !strings.Contains(strings.Join(strings.Fields(out), " "), "failed: connection reset by peer") {
		t.Errorf("--full lost words:\n%s", out)
	}
}

// One long host name (up to 128 characters) used to widen the HOST column
// for every row, and at 80 columns a delete lost its file name. The column
// is capped; a detail too long for the line loses its middle, keeping how it
// starts and the last part of its path.
func TestActivityLongHostAndMiddleCut(t *testing.T) {
	at := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	long := strings.Repeat("very-long-host-name-", 6)
	ev := []activity.Event{
		{Time: at, User: "me@box", Action: "files", Host: long, Target: "deploy@192.0.2.9", Detail: "delete /tmp/meterm-test-1234/remote/some/deep/directory/evidence-2026.txt"},
		{Time: at, User: "me@box", Action: "files", Host: "web1", Target: "192.0.2.10", Detail: "delete /tmp/meterm-test-1234/remote/some/deep/directory/evidence-2026.txt"},
	}
	var b bytes.Buffer
	printEvents(&b, ev, 80, false)
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	for _, l := range lines {
		if cellwidth.Width(l) > 80 {
			t.Errorf("line is %d wide: %q", cellwidth.Width(l), l)
		}
	}
	for _, l := range lines[1:] {
		if !strings.Contains(l, "evidence-2026.txt") || !strings.Contains(l, "delete /t") || !strings.Contains(l, "…") {
			t.Errorf("the detail lost its start or its file name: %q", l)
		}
	}
	if !strings.Contains(lines[1], "very-long-host-…") {
		t.Errorf("the long host isn't cut to the column: %q", lines[1])
	}
	// Wide enough, or --full: a TARGET column tells apart two hosts that show
	// the same.
	for _, c := range []struct {
		w    int
		full bool
	}{{160, false}, {80, true}} {
		b.Reset()
		printEvents(&b, ev, c.w, c.full)
		if out := b.String(); !strings.Contains(out, "TARGET") || !strings.Contains(out, "deploy@192.0.2.9") {
			t.Errorf("width %d full %v: no TARGET:\n%s", c.w, c.full, out)
		}
	}
	b.Reset()
	printEvents(&b, ev, 80, false)
	if strings.Contains(b.String(), "TARGET") {
		t.Errorf("80 columns has room for no TARGET:\n%s", b.String())
	}
	// A name with a hidden character is shown so it can't pass for another:
	// the log's cleaning (Event.Clean) puts � where it was.
	b.Reset()
	printEvents(&b, []activity.Event{{Time: at, Action: "exec", Host: "zw\u200bsp"}}, 0, false)
	if !strings.Contains(b.String(), "zw�sp") {
		t.Errorf("hidden character not spelled out:\n%q", b.String())
	}
}

func TestCutMiddle(t *testing.T) {
	for _, c := range []struct {
		in        string
		w         int
		head, end string
	}{
		{"delete /tmp/a/b/c/d/e/f/g/h/i/j/k/file.txt", 30, "delete /tmp", "file.txt"},
		{"exit 1 after 0.2s; systemctl restart a-very-long-service-name-here please", 40, "exit 1 after", "please"},
		{"short", 40, "short", "short"},
	} {
		got := cutMiddle(c.in, c.w)
		if cellwidth.Width(got) > c.w || !strings.HasPrefix(got, c.head) || !strings.HasSuffix(got, c.end) {
			t.Errorf("cutMiddle(%q, %d) = %q", c.in, c.w, got)
		}
	}
}

// --host takes the name as shown; two hosts that show the same are an error
// naming both.
func TestActivityHostFilter(t *testing.T) {
	ev := []activity.Event{{Host: "web1"}, {Host: "evil\x1b[2Jname"}, {Host: "zwsp"}, {Host: "zw\u200bsp"}}
	for arg, want := range map[string]int{"web1": 1, "evilname": 1, "evil\x1b[2Jname": 1, "zw\u200bsp": 1, "nope": 0} {
		got, err := filterHost(ev, arg)
		if err != nil || len(got) != want {
			t.Errorf("--host %q: %d, %v", arg, len(got), err)
		}
	}
	if _, err := filterHost(ev, "zwsp"); err == nil || !strings.Contains(err.Error(), `"zw\u200bsp"`) {
		t.Errorf("--host zwsp: %v", err)
	}
}

// A cut keeps the outcome: everything up to the first ':' or ';' (outside
// brackets) is there whole, then the end.
func TestCutMiddleKeepsTheOutcome(t *testing.T) {
	for _, c := range []struct {
		in        string
		w         int
		head, end string
	}{
		{"exit 1 after 0.2s; systemctl restart a-very-long-service-name-here please", 24, "exit 1 after 0.2s;", "ase"},
		{"failed: copy /srv/app/releases/2026-11-11/build/bundle.tar.gz -> /home/me/b.tgz: connection reset", 30, "failed:", "reset"},
		{"interrupted (maybe partly done): delete /srv/a/b/c/d/e/f/g/evidence.txt", 40, "interrupted (maybe partly done):", "txt"},
		{"failed (3 of 9: see above): delete /srv/a/b/c/d/e/f/g/h/i/j/evidence.txt", 34, "failed (3 of 9: see above):", "txt"},
	} {
		got := cutMiddle(c.in, c.w)
		if cellwidth.Width(got) > c.w || !strings.HasPrefix(got, c.head) || !strings.HasSuffix(got, c.end) {
			t.Errorf("cutMiddle(%q, %d) = %q", c.in, c.w, got)
		}
	}
}

// At 60 columns the columns before DETAIL leave it 10 to 20 cells, too few
// for "exit 1 after 0.2s;" or a production note. Under 24 cells the detail
// goes on a line of its own below its row; cut, its production note is
// written short as ⚑ and how it went ahead, so a row that reached
// production never reads like one that didn't.
func TestActivityKeepsTheProductionNoteWhenCut(t *testing.T) {
	at := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	cmd := "; systemctl restart a-long-service-name-that-goes-on-and-on-and-on"
	notes := map[string]string{
		"production, asked":                     "⚑asked;",
		"production, picker asked":              "⚑picker;",
		"production, --yes":                     "⚑--yes;",
		"production, no prompt: not a terminal": "⚑no prompt;",
		"production":                            "⚑;",
	}
	for note, want := range notes {
		for _, action := range []string{"exec", "tunnel-disable"} {
			ev := []activity.Event{
				{Time: at, User: "me@box", Action: action, Host: "pay1", Detail: "started (" + note + ")" + cmd},
				{Time: at, User: "me@box", Action: action, Host: "web1", Detail: "exit 1 after 0.2s" + cmd},
			}
			var b bytes.Buffer
			printEvents(&b, ev, 60, false)
			out := b.String()
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			for _, l := range lines {
				if cellwidth.Width(l) > 60 {
					t.Errorf("line is %d wide: %q", cellwidth.Width(l), l)
				}
			}
			if len(lines) != 6 || !strings.Contains(lines[2], "pay1") || !strings.Contains(lines[4], "web1") {
				t.Fatalf("%s: not a row and a detail line each:\n%s", action, out)
			}
			if !strings.HasPrefix(lines[3], "  started "+want) || !strings.HasSuffix(lines[3], "on-and-on") {
				t.Errorf("%s, %q: the outcome or the note lost: %q", action, note, lines[3])
			}
			if !strings.HasPrefix(lines[5], "  exit 1 after 0.2s;") || strings.Contains(lines[5], "⚑") {
				t.Errorf("%s: dev host's detail: %q", action, lines[5])
			}
		}
	}
	// With room on the row, the detail stays on it.
	var b bytes.Buffer
	printEvents(&b, []activity.Event{{Time: at, User: "me@box", Action: "exec", Host: "pay1", Detail: "started (production, picker asked)" + cmd}}, 100, false)
	if lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n"); len(lines) != 2 || !strings.Contains(lines[1], "pay1  started ⚑picker;") {
		t.Errorf("100 columns:\n%s", b.String())
	}
	// --full on a narrow terminal wraps it below, note as written.
	b.Reset()
	printEvents(&b, []activity.Event{{Time: at, User: "me@box", Action: "exec", Host: "pay1", Detail: "started (production, picker asked)" + cmd}}, 60, true)
	if !strings.Contains(strings.Join(strings.Fields(b.String()), " "), "started (production, picker asked); systemctl") {
		t.Errorf("--full at 60:\n%s", b.String())
	}
	for _, l := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		if cellwidth.Width(l) > 60 {
			t.Errorf("--full: line is %d wide: %q", cellwidth.Width(l), l)
		}
	}
	// Uncut, the note is as written.
	b.Reset()
	printEvents(&b, []activity.Event{{Time: at, Action: "exec", Host: "pay1", Detail: "started (production, picker asked); uptime"}}, 0, false)
	if !strings.Contains(b.String(), "started (production, picker asked); uptime") {
		t.Errorf("piped output changed the note:\n%s", b.String())
	}
}

// printEvents is printActivity for events with no outcomes to fold in.
func printEvents(out io.Writer, events []activity.Event, width int, full bool) {
	printActivity(out, activityRows(events, ""), width, full)
}

// At 60 columns an agent's rows (ACTOR agent:claude-code, tunnel-disable,
// EXIT and TOOK) were 67 to 71 cells and wrapped: the row fits, ACTOR
// keeping its kind and ACTION still telling enable from disable.
func TestActivityFitsSixtyColumnsWithAnActor(t *testing.T) {
	at := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	ev := []activity.Event{
		{Time: at, User: "someone@workstation", ActorType: "agent", AgentKind: "claude-code", Action: "tunnel-enable", Host: "pay1", Detail: "(production, --yes) ui"},
		{Time: at, User: "someone@workstation", ActorType: "agent", AgentKind: "claude-code", Action: "tunnel-disable", Host: "pay1", Detail: "(production) ui"},
	}
	for _, w := range []int{60, 50} {
		var b bytes.Buffer
		printEvents(&b, ev, w, false)
		out := b.String()
		for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			if cellwidth.Width(l) > w {
				t.Errorf("width %d: line is %d wide: %q", w, cellwidth.Width(l), l)
			}
		}
		if !strings.Contains(out, "agent ") || !strings.Contains(out, "tunnel-en") || !strings.Contains(out, "tunnel-di") {
			t.Errorf("width %d lost who or what:\n%s", w, out)
		}
	}
}

// At 80 columns a file manager failure lost the word that says what became
// of the file ("…unchanged"): the cut keeps the outcome, the file and its
// status, and how it ended.
func TestCutMiddleKeepsTheStatusWord(t *testing.T) {
	for _, c := range []struct {
		in   string
		keep []string
	}{
		{"failed: upload report.pdf -> /srv/app/shared/uploads/2026 (1 file(s), 2.1 MB): web1:/srv/app/shared/uploads/2026/report.pdf unchanged: connection lost",
			[]string{"failed:", "report.pdf unchanged", "connection lost"}},
		{"cancelled: upload big.bin -> /srv/app/shared/uploads/2026 (1 file(s), 381.5 MB); web1:/srv/app/shared/uploads/2026/big.bin not created, the unfinished copy discarded",
			[]string{"cancelled:", "discarded"}},
		{"failed: download dump.sql -> /home/me/Downloads/archive/2026 (1 file(s), 9 GB): local:/home/me/Downloads/archive/2026/dump.sql not created: no space left on device",
			[]string{"failed:", "dump.sql not created", "no space left on device"}},
	} {
		for _, w := range []int{40, 48, 60} {
			got := cutMiddle(c.in, w)
			if cellwidth.Width(got) > w {
				t.Errorf("%d wide at %d: %q", cellwidth.Width(got), w, got)
			}
			keep := c.keep
			if w < 60 { // the outcome, the status word and the very end
				last := c.keep[len(c.keep)-1]
				keep = []string{c.keep[0], statusOf(c.in), last[strings.LastIndex(last, " ")+1:]}
			}
			for _, k := range keep {
				if !strings.Contains(got, k) {
					t.Errorf("cutMiddle(…, %d) = %q: lost %q", w, got, k)
				}
			}
		}
	}
}

func statusOf(s string) string {
	for _, w := range statusWords {
		if strings.Contains(s, w) {
			return w
		}
	}
	return ""
}
