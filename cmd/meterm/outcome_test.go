package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"meterm/internal/activity"
	"meterm/internal/config"
	"meterm/internal/team"
)

func exitErrorOf(t *testing.T, code int) error {
	t.Helper()
	c := exec.Command(os.Args[0], "-test.run=^TestHelperExit$")
	c.Env = append(os.Environ(), "METERM_TEST_EXIT="+strings.Repeat("x", code))
	err := c.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != code {
		t.Fatalf("helper exit: %v", err)
	}
	return err
}

// TestHelperExit isn't a test: it's a process that ends with a chosen exit code.
func TestHelperExit(t *testing.T) {
	if s := os.Getenv("METERM_TEST_EXIT"); s != "" {
		os.Exit(len(s))
	}
}

func TestOutcomeOf(t *testing.T) {
	ok := outcomeOf(nil, time.Second)
	if ok.ExitCode == nil || *ok.ExitCode != 0 || ok.Took != time.Second {
		t.Errorf("success: %+v", ok)
	}
	failed := outcomeOf(exitErrorOf(t, 3), time.Second)
	if failed.ExitCode == nil || *failed.ExitCode != 3 {
		t.Errorf("exit 3: %+v", failed)
	}
	hostile := outcomeOf(errors.New("\x1b]0;evil\x07 host said run this"), time.Second)
	if hostile.ExitCode != nil || hostile.Note != "failed" {
		t.Errorf("an error's text must stay out: %+v", hostile)
	}
}

func TestExecOutcome(t *testing.T) {
	if o := execOutcome(execResult{ExitCode: 0, Seconds: 1.5}, false); o.ExitCode == nil || *o.ExitCode != 0 || o.Took != 1500*time.Millisecond {
		t.Errorf("ok: %+v", o)
	}
	if o := execOutcome(execResult{ExitCode: 255}, false); o.ExitCode == nil || *o.ExitCode != 255 {
		t.Errorf("ssh's failure stays what it was: %+v", o)
	}
	if o := execOutcome(execResult{ExitCode: -1, Error: "\x1b[2J from the host"}, true); o.ExitCode != nil || o.Note != "timed out" {
		t.Errorf("timeout: %+v", o)
	}
	if o := execOutcome(execResult{ExitCode: -1, Error: "\x1b[2J from the host"}, false); o.ExitCode != nil || o.Note != "failed to finish" {
		t.Errorf("never ran: %+v", o)
	}
}

// Each host's outcome is told as it finishes, with its own exit code.
func TestRunExecReportsEachHost(t *testing.T) {
	got := map[int]int{}
	ch := make(chan [2]int, 2)
	hosts := []config.Host{{Name: "a"}, {Name: "b"}}
	err := runExec(hosts, []string{"x"}, func(_ context.Context, h config.Host, _ []string, _, _ io.Writer) error {
		if h.Name == "b" {
			return exitErrorOf(t, 7)
		}
		return nil
	}, execOptions{parallel: 2, format: "json", out: io.Discard, done: func(i int, r execResult, _ bool) { ch <- [2]int{i, r.ExitCode} }})
	if err == nil {
		t.Error("a failed host must still fail the command")
	}
	for range hosts {
		v := <-ch
		got[v[0]] = v[1]
	}
	if got[0] != 0 || got[1] != 7 {
		t.Errorf("got %v", got)
	}
}

func TestActivityRowsFoldOutcomes(t *testing.T) {
	code, ms := 2, int64(1500)
	ev := func(seq int64, action, host string, ref int64) activity.Event {
		return activity.Event{Seq: seq, Action: action, Host: host, RefSeq: ref}
	}
	done := ev(3, activity.ActionOutcome, "a", 1)
	done.ExitCode, done.DurationMS = &code, &ms
	orphan := ev(5, activity.ActionOutcome, "a", 99)
	orphan.ExitCode = &code
	rows := activityRows([]activity.Event{ev(1, "exec", "a", 0), ev(2, "connect", "b", 0), done, ev(4, "cp", "a", 0), orphan}, "")
	if len(rows) != 4 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].exit != "2" || rows[0].took != "1.5s" {
		t.Errorf("the outcome didn't reach its start: %+v", rows[0])
	}
	if rows[1].exit != "-" || rows[1].took != "-" {
		t.Errorf("no outcome reads as nothing known: %+v", rows[1])
	}
	if rows[3].Action != activity.ActionOutcome || rows[3].exit != "2" {
		t.Errorf("an outcome with nothing to attach to is still shown: %+v", rows[3])
	}
	// An outcome pointing at an event from another host isn't folded into it when a host is asked for.
	if rows := activityRows([]activity.Event{ev(1, "exec", "a", 0), done}, "b"); len(rows) != 0 {
		t.Errorf("host filter: %+v", rows)
	}
	// A forged ref to a later or unrelated line does nothing odd.
	self := ev(1, activity.ActionOutcome, "a", 1)
	if rows := activityRows([]activity.Event{self}, ""); len(rows) != 1 {
		t.Errorf("self reference: %+v", rows)
	}
}

// What's in the log is whatever got into a file this account can write, or a teammate's.
func TestActivityOutputIsCleanInBothFormats(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("METERM_CONFIG", t.TempDir()+"/hosts.yaml")
	t.Setenv("METERM_ACTIVITY_DIR", dir)
	hostile := "a\x1b]52;c;aGk=\x07b‮c\u009b2Jd"
	raw, _ := json.Marshal(activity.Event{Device: hostile, Seq: 1, User: hostile, Action: hostile, Host: hostile, Target: hostile, Detail: hostile,
		ActorType: hostile, AgentKind: hostile, AgentSessionID: hostile, AttributionSource: hostile, Hash: hostile, ExtHash: hostile})
	if err := os.WriteFile(dir+"/events.jsonl", append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"text", "json"} {
		out := captureStdout(t, func() {
			cmd := newActivityCmd()
			cmd.SetArgs([]string{"--format", format})
			if err := cmd.Execute(); err != nil {
				t.Error(err)
			}
		})
		for _, bad := range []string{"\x1b", "\x07", "‮", "\u009b", `\u001b`, `\u0007`, `‮`, `\u009b`} {
			if strings.Contains(out, bad) {
				t.Errorf("%s output has %q: %q", format, bad, out)
			}
		}
	}
}

func TestReadingTheLogCreatesNothing(t *testing.T) {
	dir := t.TempDir() + "/not-yet"
	t.Setenv("METERM_ACTIVITY_DIR", dir)
	t.Setenv("METERM_CONFIG", t.TempDir()+"/hosts.yaml")
	for _, args := range [][]string{{}, {"--format", "json"}, {"--verify"}} {
		captureStdout(t, func() {
			cmd := newActivityCmd()
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Error(err)
			}
		})
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("activity %v created %s", args, dir)
		}
	}
}

func TestAccessRowsDontCountOutcomes(t *testing.T) {
	rows := accessRows([]team.TeamEvent{{Event: activity.Event{Action: "exec", Host: "a"}}, {Event: activity.Event{Action: activity.ActionOutcome, Host: "a"}}})
	if len(rows) != 1 || rows[0].Actions != 1 || strings.Contains(rows[0].Kinds, activity.ActionOutcome) {
		t.Fatalf("got %+v", rows)
	}
}

func TestCSVCell(t *testing.T) {
	for in, want := range map[string]string{
		"=cmd()": "'=cmd()", "+1": "'+1", "-1": "'-1", "@x": "'@x", "|cmd": "'|cmd",
		"web1": "web1", "": "", "a=b": "a=b", "'=x": "'=x",
		" =cmd":         "' =cmd",
		"\u00a0=cmd":    "'\u00a0=cmd",
		"\u3000=cmd":    "'\u3000=cmd",
		"\t=cmd":        "'�=cmd",
		"\r=cmd":        "'�=cmd",
		"\n@cmd":        "'�@cmd",
		"＝1+1":          "'＝1+1",
		"＋1":            "'＋1",
		"－1":            "'－1",
		"＠1":            "'＠1",
		"\u200b=cmd":    "'�=cmd",
		"\u202e=cmd":    "'�=cmd",
		"\ufeff=cmd":    "'�=cmd",
		"\x1b]0;x\x07=": "�]0;x�=",
	} {
		if got := csvCell(in); got != want {
			t.Errorf("csvCell(%q) = %q, want %q", in, got, want)
		}
	}
	if n := len([]rune(csvCell(strings.Repeat("x", 9000)))); n > 4096 {
		t.Errorf("%d runes", n)
	}
}

// Every column goes through the one function: nothing hostile in any of them.
func TestTeamLogCSVCleansEveryColumn(t *testing.T) {
	const bad = "=a\x1b]52;c;aGk=\x07b\u202ec\nd"
	var out bytes.Buffer
	err := writeTeamLogCSV(&out, []team.TeamEvent{{UploaderEmail: bad, UploaderUID: bad, Event: activity.Event{
		User: bad, Action: bad, Host: bad, Target: bad, Detail: bad, Device: bad, Hash: bad}}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&out).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("%v, %v", rows, err)
	}
	for i, cell := range rows[1] {
		if strings.ContainsAny(cell, "\x1b\x07\u202e\n") {
			t.Errorf("%s has %q", rows[0][i], cell)
		}
		if strings.HasPrefix(cell, "=") {
			t.Errorf("%s reads as a formula: %q", rows[0][i], cell)
		}
	}
}

// verified agrees between the formats: an event that's sound alone and doesn't follow the one
// before it isn't verified in either.
func TestTeamLogCSVVerifiedMeansWhatJSONDoes(t *testing.T) {
	l, err := activity.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, _ := l.Append(activity.Event{Action: "exec", Host: "h"})
	second, _ := l.Append(activity.Event{Action: "exec", Host: "h"})
	first.Hash = "not what the next one follows"
	events := []team.TeamEvent{{Event: first}, {Event: second}}
	var out bytes.Buffer
	if err := writeTeamLogCSV(&out, events); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&out).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var js, errOut bytes.Buffer
	if err := writeTeamLogJSON(&js, &errOut, events); err != nil {
		t.Fatal(err)
	}
	var fromJSON []struct {
		Verified bool `json:"verified"`
	}
	if err := json.Unmarshal(js.Bytes(), &fromJSON); err != nil {
		t.Fatal(err)
	}
	for i := range events {
		if got := rows[i+1][len(rows[0])-1]; got != strconv.FormatBool(fromJSON[i].Verified) {
			t.Errorf("event %d: csv %s, json %v", i, got, fromJSON[i].Verified)
		}
	}
	if fromJSON[1].Verified {
		t.Error("the second doesn't follow the first")
	}
}

// The export has to stay usable with a bad event in the log, and has to say so.
func TestTeamLogJSONMarksWhatDoesntHold(t *testing.T) {
	good := activity.Event{Device: "d", Seq: 1, Action: "exec", Host: "h"}
	l, err := activity.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if good, err = l.Append(good); err != nil {
		t.Fatal(err)
	}
	second, err := l.Append(activity.Event{Action: "exec", Host: "h"})
	if err != nil {
		t.Fatal(err)
	}
	second.Detail = "edited \x1b[2J"
	var out, errOut bytes.Buffer
	if err := writeTeamLogJSON(&out, &errOut, []team.TeamEvent{{Event: good}, {Event: second}}); err != nil {
		t.Fatalf("a bad event must not fail the export: %v", err)
	}
	var rows []struct {
		Seq      int64  `json:"seq"`
		Verified bool   `json:"verified"`
		Problem  string `json:"integrity_problem"`
		Detail   string `json:"detail"`
	}
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || !rows[0].Verified || rows[0].Problem != "" || rows[1].Verified || rows[1].Problem == "" {
		t.Errorf("%+v", rows)
	}
	if strings.Contains(rows[1].Detail, "\x1b") {
		t.Errorf("%q", rows[1].Detail)
	}
	if !strings.Contains(errOut.String(), "1 event(s)") {
		t.Errorf("no signal on stderr: %q", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if err := writeTeamLogJSON(&out, &errOut, []team.TeamEvent{}); err != nil || errOut.Len() != 0 || strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("empty: %q %q %v", out.String(), errOut.String(), err)
	}
}

// A process killed has no exit code. -1 isn't one, and neither is 0.
func TestKilledHasNoExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a kill on Windows comes back as exit 1")
	}
	c := exec.Command(os.Args[0], "-test.run=^TestHelperHang$")
	c.Env = append(os.Environ(), "METERM_TEST_HANG=1")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	_ = c.Process.Kill()
	err := c.Wait()
	o := outcomeOf(err, time.Second)
	if o.ExitCode != nil || o.Note != "killed" {
		t.Errorf("%+v, from %v", o, err)
	}
}

func TestHelperHang(t *testing.T) {
	if os.Getenv("METERM_TEST_HANG") != "" {
		time.Sleep(time.Minute)
	}
}

// captureStdout runs fn with os.Stdout going to a pipe, and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	defer func() { os.Stdout = old }()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}
