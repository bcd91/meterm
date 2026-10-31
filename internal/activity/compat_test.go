package activity

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// v022Event and the functions after it are what a released meterm (v0.22.0) compiles
// to, copied so they can't drift with the code here. Those binaries are out there,
// reading the same files: a team's copy, or this machine's log on an upgrade or a
// downgrade.
type v022Event struct {
	Device   string    `json:"device"`
	Seq      int64     `json:"seq"`
	Time     time.Time `json:"time"`
	User     string    `json:"user"`
	Action   string    `json:"action"`
	Host     string    `json:"host"`
	Target   string    `json:"target"`
	Detail   string    `json:"detail,omitempty"`
	PrevHash string    `json:"prev_hash"`
	Hash     string    `json:"hash"`
}

func (e v022Event) computeHash() string {
	h := sha256.New()
	for _, f := range []string{
		e.Device, strconv.FormatInt(e.Seq, 10), e.Time.UTC().Format(time.RFC3339Nano),
		e.User, e.Action, e.Host, e.Target, e.Detail, e.PrevHash,
	} {
		fmt.Fprintf(h, "%d:%s|", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func v022Verify(events []v022Event) error {
	prev := ""
	var prevSeq int64
	for i, e := range events {
		switch {
		case e.Hash != e.computeHash():
			return fmt.Errorf("line %d: modified", i+1)
		case e.PrevHash != prev:
			return fmt.Errorf("line %d: does not follow", i+1)
		case e.Seq != prevSeq+1:
			return fmt.Errorf("line %d: jumps", i+1)
		}
		prev, prevSeq = e.Hash, e.Seq
	}
	return nil
}

func v022Read(t *testing.T, path string) []v022Event {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []v022Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e v022Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("an old reader can't parse %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	return out
}

func writeV022(t *testing.T, l *Log, e v022Event) {
	t.Helper()
	b, _ := json.Marshal(e)
	f, err := os.OpenFile(l.eventsPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

// The hash of an event from before this code, as a golden value: if it stops
// matching, every existing log stops verifying.
func TestLegacyHashIsFrozen(t *testing.T) {
	e := Event{Device: "d", Seq: 1, Time: time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC),
		User: "u@h", Action: "exec", Host: "web", Target: "a@1", Detail: "uptime"}
	want := v022Event{Device: "d", Seq: 1, Time: e.Time, User: "u@h", Action: "exec", Host: "web", Target: "a@1", Detail: "uptime"}.computeHash()
	if got := e.computeHash(); got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
	const golden = "af45830f22dc82ab"
	if got := e.computeHash()[:16]; got != golden {
		t.Fatalf("hash changed: %s, want %s…", got, golden)
	}
	// New fields stay out of it, or old readers would call new events edited.
	code := 3
	e.ExitCode, e.ActorType, e.RefSeq, e.ExtHash = &code, ActorAgent, 9, "x"
	if e.computeHash() != want {
		t.Fatal("a new field reached the legacy hash")
	}
}

func TestOldReaderAcceptsNewEvents(t *testing.T) {
	l := newLog(t)
	code, ms := 0, int64(12)
	for _, e := range []Event{
		{Action: "exec", Host: "web", Target: "a@1", Detail: "uptime", ActorType: ActorAgent, AgentKind: KindClaudeCode, AgentSessionID: "s-1", AttributionSource: SourceDeclared},
		{Action: ActionOutcome, Host: "web", Target: "a@1", RefSeq: 1, ExitCode: &code, DurationMS: &ms},
	} {
		if _, err := l.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := v022Verify(v022Read(t, l.eventsPath())); err != nil {
		t.Fatalf("v0.22.0 calls a new log broken: %v", err)
	}
}

// Old lines, then new ones, then an old version writing again, then new again, as on
// a machine or a team with two versions.
func TestMixedVersionsKeepBothChains(t *testing.T) {
	l := newLog(t)
	device, _ := l.DeviceID()
	old := func(seq int64, prev string) v022Event {
		e := v022Event{Device: device, Seq: seq, Time: time.Date(2026, 1, 1, 0, 0, int(seq), 0, time.UTC), User: "u", Action: "exec", Host: "web", PrevHash: prev}
		e.Hash = e.computeHash()
		writeV022(t, l, e)
		return e
	}
	a := old(1, "")
	b := old(2, a.Hash)
	n1, err := l.Append(Event{Action: "exec", Host: "web", ActorType: ActorUnknown})
	if err != nil {
		t.Fatal(err)
	}
	if n1.PrevHash != b.Hash || n1.Seq != 3 || n1.PrevExt != "" {
		t.Fatalf("first new event: %+v", n1)
	}
	c := old(4, n1.Hash)
	n2, err := l.Append(Event{Action: "exec", Host: "web", ActorType: ActorUnknown})
	if err != nil {
		t.Fatal(err)
	}
	if n2.PrevHash != c.Hash || n2.PrevExt != n1.ExtHash {
		t.Fatalf("new event after an old one: %+v", n2)
	}
	all, _ := l.All()
	if broken := Verify(all); broken != nil {
		t.Fatalf("new reader: %v", broken)
	}
	if broken := VerifyShared(all); broken != nil {
		t.Fatalf("new reader, shared: %v", broken)
	}
	if err := v022Verify(v022Read(t, l.eventsPath())); err != nil {
		t.Fatalf("old reader: %v", err)
	}
}

// Tampering with what only ExtHash covers, however it's done, is caught by Verify and
// VerifyShared, and leaves the legacy chain alone: old readers can't see this.
func TestVerifyCatchesEditsToNewFields(t *testing.T) {
	code, ms := 1, int64(5)
	build := func(t *testing.T) []Event {
		l := newLog(t)
		for _, e := range []Event{
			{Action: "exec", Host: "h", ActorType: ActorAgent, AgentKind: KindClaudeCode, AgentSessionID: "s", AttributionSource: SourceDeclared},
			{Action: ActionOutcome, Host: "h", RefSeq: 1, ExitCode: &code, DurationMS: &ms, ActorType: ActorAgent, AttributionSource: SourceDeclared},
			{Action: "exec", Host: "h", ActorType: ActorAgent, AttributionSource: SourceDeclared},
		} {
			if _, err := l.Append(e); err != nil {
				t.Fatal(err)
			}
		}
		all, err := l.All()
		if err != nil {
			t.Fatal(err)
		}
		return all
	}
	zero := 0
	longer := int64(1)
	for _, c := range []struct {
		name string
		edit func(es []Event)
	}{
		{"exit code changed", func(es []Event) { es[1].ExitCode = &zero }},
		{"exit code removed", func(es []Event) { es[1].ExitCode = nil }},
		{"duration changed", func(es []Event) { es[1].DurationMS = &longer }},
		{"refers elsewhere", func(es []Event) { es[1].RefSeq = 3 }},
		{"actor changed", func(es []Event) { es[0].ActorType = ActorHuman }},
		{"kind changed", func(es []Event) { es[0].AgentKind = "other" }},
		{"session changed", func(es []Event) { es[0].AgentSessionID = "z" }},
		{"source upgraded", func(es []Event) { es[0].AttributionSource = "verified" }},
		{"prev_ext changed", func(es []Event) { es[1].PrevExt = "nope" }},
		{"fields stripped, hash kept", func(es []Event) {
			es[0].ActorType, es[0].AgentKind, es[0].AgentSessionID, es[0].AttributionSource = "", "", "", ""
		}},
		{"hash stripped, field kept", func(es []Event) { es[0].ExtHash = "" }},
		{"all of one event stripped", func(es []Event) {
			es[0].ActorType, es[0].AgentKind, es[0].AgentSessionID, es[0].AttributionSource, es[0].ExtHash = "", "", "", "", ""
		}},
		{"last event's hash stripped, claim kept", func(es []Event) { es[2].ExtHash = "" }},
		{"edited and rehashed, next not", func(es []Event) { es[0].ActorType = ActorHuman; es[0].ExtHash = es[0].computeExtHash() }},
	} {
		t.Run(c.name, func(t *testing.T) {
			es := build(t)
			c.edit(es)
			if b := Verify(es); b == nil {
				t.Error("Verify missed it")
			}
			// In a team's copy, an event with its fields gone in one piece is what an older
			// meterm uploads, so that's a gap there. Everything else is a problem.
			gap := c.name == "all of one event stripped"
			if b := VerifyShared(es); (b == nil) != gap {
				t.Errorf("VerifyShared = %v, gap %v", b, gap)
			}
		})
	}
}

func TestAppendCleansAndBoundsWhatItKeeps(t *testing.T) {
	l := newLog(t)
	hostile := "a\x1b]52;c;aGk=\x07b‮c\u009b2Jd\nnext\xff"
	e, err := l.Append(Event{Action: hostile, Host: hostile, Target: hostile, User: hostile, Detail: hostile + strings.Repeat("x", 3*maxDetail)})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{e.Action, e.Host, e.Target, e.User, e.Detail} {
		if strings.ContainsAny(s, "\x1b\x07‮\u009b\n") || !strings.Contains(s, "�") {
			t.Errorf("not cleaned: %q", s)
		}
	}
	if n := len([]rune(e.Detail)); n > maxDetail {
		t.Errorf("detail kept %d runes", n)
	}
	if n := len([]rune(e.Host)); n > maxField {
		t.Errorf("host kept %d runes", n)
	}
	if !e.Valid() {
		t.Error("hash doesn't match what was written")
	}
	// A huge argument can't make a line the next read gives up on.
	if _, err := l.Append(Event{Action: "exec", Host: "h", Detail: strings.Repeat("y", 4<<20)}); err != nil {
		t.Fatal(err)
	}
	if n, broken, err := l.VerifyFile(); err != nil || broken != nil || n != 2 {
		t.Fatalf("VerifyFile: %d, %v, %v", n, broken, err)
	}
}

// Redaction still happens, and before cutting: a cut can't leave half a secret.
func TestAppendRedactsBeforeItClips(t *testing.T) {
	l := newLog(t)
	e, err := l.Append(Event{Action: "exec", Host: "h", Detail: "mysql --password hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.Detail, "hunter2") {
		t.Fatalf("secret kept: %q", e.Detail)
	}
}

func TestRecordOutcome(t *testing.T) {
	t.Setenv("METERM_ACTIVITY_DIR", t.TempDir())
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "abc-1")
	start, err := Record("exec", "web", "a@1", "uptime")
	if err != nil {
		t.Fatal(err)
	}
	if start.ActorType != ActorAgent || start.AgentKind != KindClaudeCode || start.AgentSessionID != "abc-1" || start.AttributionSource != SourceDeclared {
		t.Errorf("start: %+v", start)
	}
	if start.ExitCode != nil || start.DurationMS != nil || start.RefSeq != 0 {
		t.Errorf("a start claims an end: %+v", start)
	}
	code := 0
	end, err := RecordOutcome(start, Outcome{ExitCode: &code, Took: 1500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if end.Action != ActionOutcome || end.RefSeq != start.Seq || end.Host != "web" || end.Target != "a@1" || end.ExitCode == nil || *end.ExitCode != 0 || *end.DurationMS != 1500 {
		t.Errorf("end: %+v", end)
	}
	if end.Detail != "seq 1: exit 0, 1.5s" {
		t.Errorf("detail: %q", end.Detail)
	}
	timeout, err := RecordOutcome(start, Outcome{Note: "timed out", Took: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if timeout.ExitCode != nil || timeout.Detail != "seq 1: timed out, 1s" {
		t.Errorf("no exit code: %+v", timeout)
	}
	// 0 and missing come out differently.
	b0, _ := json.Marshal(end)
	b1, _ := json.Marshal(timeout)
	if !strings.Contains(string(b0), `"exit_code":0`) || strings.Contains(string(b1), "exit_code") {
		t.Errorf("exit_code in JSON: %s / %s", b0, b1)
	}
}

func TestDetectActor(t *testing.T) {
	for _, c := range []struct {
		name          string
		env           map[string]string
		typ, kind, id string
	}{
		{"a person's shell", nil, ActorUnknown, "", ""},
		{"claude code", map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "11111111-2222", "AI_AGENT": "claude-code_2-1-316_agent"}, ActorAgent, KindClaudeCode, "11111111-2222"},
		{"claudecode alone", map[string]string{"CLAUDECODE": "1"}, ActorAgent, KindClaudeCode, ""},
		{"marker alone", map[string]string{"AI_AGENT": "claude-code_2-1-316_agent"}, ActorAgent, KindClaudeCode, ""},
		{"marker with slashes", map[string]string{"AI_AGENT": "claude-code/2.1/agent"}, ActorAgent, KindClaudeCode, ""},
		{"claudecode not 1", map[string]string{"CLAUDECODE": "0"}, ActorUnknown, "", ""},
		{"session without a marker isn't an agent", map[string]string{"CLAUDE_CODE_SESSION_ID": "abc"}, ActorUnknown, "", ""},
		{"another agent", map[string]string{"AI_AGENT": "cursor_1.2_agent"}, ActorAgent, "cursor", ""},
		{"hostile kind", map[string]string{"AI_AGENT": "\x1b]0;x\x07_1_agent"}, ActorAgent, "other", ""},
		{"long kind", map[string]string{"AI_AGENT": strings.Repeat("a", 500)}, ActorAgent, "other", ""},
		{"kind with shell", map[string]string{"AI_AGENT": "$(touch x)_1"}, ActorAgent, "other", ""},
		{"uppercase kind", map[string]string{"AI_AGENT": "EVIL"}, ActorAgent, "other", ""},
		{"a token before the separator", map[string]string{"AI_AGENT": "sk-ant-api03-abcdef_x"}, ActorAgent, "other", ""},
		{"a token after the first word", map[string]string{"AI_AGENT": "ghp_0123456789abcdefghijABCDEFGHIJ012345"}, ActorAgent, "other", ""},
		{"digits in a kind", map[string]string{"AI_AGENT": "abc123"}, ActorAgent, "other", ""},
		{"formula", map[string]string{"AI_AGENT": "=1+1_x"}, ActorAgent, "other", ""},
		{"double hyphen", map[string]string{"AI_AGENT": "a--b"}, ActorAgent, "other", ""},
		{"long kind in form", map[string]string{"AI_AGENT": strings.Repeat("a", maxKind+1) + "_1"}, ActorAgent, "other", ""},
		{"session turned off", map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "abc", NoSessionEnv: "1"}, ActorAgent, KindClaudeCode, ""},
		{"hostile session", map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "a\x1b[2Jb"}, ActorAgent, KindClaudeCode, ""},
		{"session with spaces", map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "a; rm -rf /"}, ActorAgent, KindClaudeCode, ""},
		{"long session", map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": strings.Repeat("s", 129)}, ActorAgent, KindClaudeCode, ""},
		{"session that's a token", map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "ghp_0123456789abcdefghijABCDEFGHIJ012345"}, ActorAgent, KindClaudeCode, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := DetectActor(func(k string) string { return c.env[k] })
			if a.Type != c.typ || a.Kind != c.kind || a.SessionID != c.id {
				t.Errorf("got %+v, want %s %q %q", a, c.typ, c.kind, c.id)
			}
			if a.Source != SourceDeclared {
				t.Errorf("source = %q; only declared is honest", a.Source)
			}
		})
	}
}

// Whatever else is in the environment stays out of it, tokens above all.
func TestDetectActorReadsOnlyItsOwnVariables(t *testing.T) {
	var asked []string
	DetectActor(func(k string) string { asked = append(asked, k); return "1" })
	for _, k := range asked {
		if k != "CLAUDECODE" && k != "AI_AGENT" && k != "CLAUDE_CODE_SESSION_ID" && k != NoSessionEnv {
			t.Errorf("read %s", k)
		}
	}
}

func TestBadLinesDontStopTheLog(t *testing.T) {
	for name, junk := range map[string]string{
		"garbage":           "garbage\n",
		"very long garbage": strings.Repeat("g", 2<<20) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			l := newLog(t)
			appendN(t, l, 2)
			addToLog(t, l, junk)
			got, err := l.Append(Event{Action: "exec", Host: "web1"})
			if err != nil {
				t.Fatalf("a bad line stopped recording: %v", err)
			}
			all, err := l.All()
			if err != nil || len(all) != 3 || all[2].Seq != got.Seq {
				t.Fatalf("after a bad line: %d events, %v", len(all), err)
			}
			if p, err := l.Pending("t"); err != nil || len(p) != 3 {
				t.Fatalf("a bad line stopped uploads: %d, %v", len(p), err)
			}
			n, broken, err := l.VerifyFile()
			if err != nil || broken == nil || broken.Line != 3 || n != 3 {
				t.Fatalf("VerifyFile = %d, %v, %v; want line 3 named", n, broken, err)
			}
		})
	}
}

func addToLog(t *testing.T, l *Log, s string) {
	t.Helper()
	f, err := os.OpenFile(l.eventsPath(), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// What a crash leaves: half a line with no newline. Nothing whole is missing, so it isn't a
// failure, and an older meterm must still be able to read the log once the next event is in.
func TestTornLastLine(t *testing.T) {
	l := newLog(t)
	appendN(t, l, 2)
	const torn = `{"device":"d","seq":`
	addToLog(t, l, torn)
	if _, broken, err := l.VerifyFile(); broken != nil || err != nil {
		t.Fatalf("a torn last line reads as tampering: %v, %v", broken, err)
	}
	notes, err := l.Notes()
	if err != nil || notes.TornLine != 3 || notes.TornBytes != len(torn) {
		t.Fatalf("Notes = %+v, %v", notes, err)
	}
	e, err := l.Append(Event{Action: "exec", Host: "web1"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Seq != 4 {
		t.Errorf("seq %d: the repair should be 3 and this 4", e.Seq)
	}
	if _, broken, err := l.VerifyFile(); broken != nil || err != nil {
		t.Fatalf("after the next event: %v, %v", broken, err)
	}
	if err := v022Verify(v022Read(t, l.eventsPath())); err != nil {
		t.Fatalf("v0.22.0 is blocked: %v", err)
	}
	notes, _ = l.Notes()
	if len(notes.Repairs) != 1 || notes.Repairs[0] != 3 || notes.TornLine != 0 {
		t.Fatalf("not visible afterwards: %+v", notes)
	}
	saved, _ := filepath.Glob(filepath.Join(l.dir, "damaged-*"))
	if len(saved) != 1 {
		t.Fatalf("saved %v", saved)
	}
	if b, _ := os.ReadFile(saved[0]); string(b) != torn {
		t.Errorf("kept %q", b)
	}
}

// Tearing a line that was once whole is removing it: the witness knows it got that far.
func TestTearingAWholeLineIsStillARemoval(t *testing.T) {
	l := newLog(t)
	appendN(t, l, 2)
	data, _ := os.ReadFile(l.eventsPath())
	if err := os.WriteFile(l.eventsPath(), data[:len(data)-20], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, broken, _ := l.VerifyFile(); broken == nil || !strings.Contains(broken.Reason, "removed") {
		t.Fatalf("%v", broken)
	}
	if _, err := l.Append(Event{Action: "exec", Host: "web1"}); err != nil {
		t.Fatal(err)
	}
	if _, broken, _ := l.VerifyFile(); broken == nil {
		t.Fatal("the next event washed it out")
	}
}

func TestRepair(t *testing.T) {
	t.Run("damage in the middle fails until a person repairs it, and shows after", func(t *testing.T) {
		l := newLog(t)
		appendN(t, l, 2)
		addToLog(t, l, "junk one\n")
		appendN(t, l, 1)
		addToLog(t, l, "junk two\n")
		appendN(t, l, 1)
		if _, broken, _ := l.VerifyFile(); broken == nil || broken.Line != 3 {
			t.Fatalf("%v", broken)
		}
		res, err := l.Repair()
		if err != nil || len(res.Lines) != 2 || res.Lines[0] != 3 || res.Lines[1] != 5 || res.EventSeq != 5 {
			t.Fatalf("%+v, %v", res, err)
		}
		if _, broken, _ := l.VerifyFile(); broken != nil {
			t.Fatalf("after repair: %v", broken)
		}
		if err := v022Verify(v022Read(t, l.eventsPath())); err != nil {
			t.Fatalf("v0.22.0 after repair: %v", err)
		}
		all, _ := l.All()
		last := all[len(all)-1]
		if last.Action != ActionRepair || !strings.Contains(last.Detail, res.SavedTo) || !strings.Contains(last.Detail, "2 damaged") {
			t.Errorf("%+v", last)
		}
		b, _ := os.ReadFile(filepath.Join(l.dir, res.SavedTo))
		if !strings.Contains(string(b), "junk one") || !strings.Contains(string(b), "junk two") {
			t.Errorf("kept %q", b)
		}
		if notes, _ := l.Notes(); len(notes.Repairs) != 1 {
			t.Errorf("%+v", notes)
		}
	})
	t.Run("nothing to do", func(t *testing.T) {
		l := newLog(t)
		appendN(t, l, 2)
		before, _ := os.ReadFile(l.eventsPath())
		if res, err := l.Repair(); err != nil || len(res.Lines) != 0 {
			t.Fatalf("%+v, %v", res, err)
		}
		if after, _ := os.ReadFile(l.eventsPath()); string(after) != string(before) {
			t.Error("it wrote something")
		}
	})
	// The point of it all: repair can't launder anything.
	t.Run("an edit stays broken", func(t *testing.T) {
		l := newLog(t)
		appendN(t, l, 3)
		rewrite(t, l, func(ls []string) []string {
			ls[1] = strings.Replace(ls[1], `"web1"`, `"web2"`, 1)
			return ls
		})
		if res, _ := l.Repair(); len(res.Lines) != 0 {
			t.Fatalf("it took a parseable line: %+v", res)
		}
		if _, broken, _ := l.VerifyFile(); broken == nil {
			t.Fatal("an edit got through")
		}
	})
	t.Run("a real event replaced by rubbish leaves a hole", func(t *testing.T) {
		l := newLog(t)
		appendN(t, l, 3)
		rewrite(t, l, func(ls []string) []string {
			ls[1] = "rubbish"
			return ls
		})
		if res, err := l.Repair(); err != nil || len(res.Lines) != 1 {
			t.Fatalf("%+v, %v", res, err)
		}
		if _, broken, _ := l.VerifyFile(); broken == nil {
			t.Fatal("repair hid a removed event")
		}
	})
	t.Run("the last line replaced by rubbish is still a removal", func(t *testing.T) {
		l := newLog(t)
		appendN(t, l, 3)
		rewrite(t, l, func(ls []string) []string {
			ls[2] = "rubbish"
			return ls
		})
		if _, err := l.Repair(); err != nil {
			t.Fatal(err)
		}
		if _, broken, _ := l.VerifyFile(); broken == nil {
			t.Fatal("repair hid a removed event")
		}
	})
}

// The witness holds what was written at its seq, and fields gone from that line show.
func TestVerifyFileComparesTheWitness(t *testing.T) {
	code := 7
	build := func(t *testing.T) *Log {
		l := newLog(t)
		for _, e := range []Event{
			{Action: "exec", Host: "h", ActorType: ActorAgent, AttributionSource: SourceDeclared},
			{Action: ActionOutcome, Host: "h", RefSeq: 1, ExitCode: &code, ActorType: ActorAgent, AttributionSource: SourceDeclared},
		} {
			if _, err := l.Append(e); err != nil {
				t.Fatal(err)
			}
		}
		return l
	}
	edit := func(t *testing.T, l *Log, fn func(all []Event)) {
		t.Helper()
		all, _ := l.All()
		fn(all)
		var out strings.Builder
		for _, e := range all {
			b, _ := json.Marshal(e)
			out.Write(append(b, '\n'))
		}
		if err := os.WriteFile(l.eventsPath(), []byte(out.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	strip := func(all []Event) {
		e := &all[len(all)-1]
		e.ExitCode, e.DurationMS, e.RefSeq, e.ActorType, e.AgentKind, e.AgentSessionID, e.AttributionSource, e.PrevExt, e.ExtHash = nil, nil, 0, "", "", "", "", "", ""
	}
	rebuildExt := func(all []Event) {
		zero := 0
		prev := ""
		for i := range all {
			if all[i].ExitCode != nil {
				all[i].ExitCode = &zero
			}
			all[i].PrevExt = prev
			all[i].ExtHash = all[i].computeExtHash()
			prev = all[i].ExtHash
		}
	}
	for name, fn := range map[string]func([]Event){"fields stripped from the last line": strip, "ext hashes rebuilt": rebuildExt} {
		t.Run(name, func(t *testing.T) {
			l := build(t)
			edit(t, l, fn)
			if _, broken, _ := l.VerifyFile(); broken == nil {
				t.Fatal("passed")
			}
			if _, err := l.Append(Event{Action: "exec", Host: "h"}); err != nil {
				t.Fatal(err)
			}
			if _, broken, _ := l.VerifyFile(); broken == nil {
				t.Fatal("one more event and it passes")
			}
			// All of it is also a difference from a copy, for what that's worth, and the
			// legacy chain doesn't see it: that's what older readers get.
		})
	}
	t.Run("with the witness rewritten too it passes, and the README says so", func(t *testing.T) {
		l := build(t)
		var last Event
		edit(t, l, func(all []Event) {
			rebuildExt(all)
			last = all[len(all)-1]
		})
		l.writeHead(last)
		if _, broken, _ := l.VerifyFile(); broken != nil {
			t.Fatal(broken)
		}
	})
	t.Run("an older meterm appending after the witness stays honest", func(t *testing.T) {
		l := build(t)
		all, _ := l.All()
		last := all[len(all)-1]
		old := v022Event{Device: last.Device, Seq: last.Seq + 1, Time: last.Time.Add(time.Second), User: "u", Action: "exec", Host: "h", PrevHash: last.Hash}
		old.Hash = old.computeHash()
		writeV022(t, l, old)
		if _, broken, err := l.VerifyFile(); broken != nil || err != nil {
			t.Fatalf("%v, %v", broken, err)
		}
		if _, err := l.Append(Event{Action: "exec", Host: "h", ActorType: ActorUnknown}); err != nil {
			t.Fatal(err)
		}
		if _, broken, err := l.VerifyFile(); broken != nil || err != nil {
			t.Fatalf("%v, %v", broken, err)
		}
	})
}

// An older meterm uploading new events drops what it doesn't know. The next event's prev_ext
// then points at a hash the team's copy hasn't got, which is honest use, and not tampering.
func TestCheckSharedTreatsAMissingExtAsAGap(t *testing.T) {
	l := newLog(t)
	all := appendN(t, l, 4)
	asOldUploaded := func(e Event) Event {
		e.RefSeq, e.ExitCode, e.DurationMS, e.ActorType, e.AgentKind, e.AgentSessionID, e.AttributionSource, e.PrevExt, e.ExtHash = 0, nil, nil, "", "", "", "", "", ""
		return e
	}
	for name, c := range map[string]struct {
		events []Event
		gaps   int
	}{
		"nothing odd":           {all, 0},
		"one in the middle":     {[]Event{all[0], asOldUploaded(all[1]), all[2], all[3]}, 1},
		"from the first":        {[]Event{asOldUploaded(all[0]), all[1], all[2], all[3]}, 1},
		"two in a row":          {[]Event{all[0], asOldUploaded(all[1]), asOldUploaded(all[2]), all[3]}, 1},
		"a real gap in the seq": {[]Event{all[0], all[2], all[3]}, 0},
		"all of them":           {[]Event{asOldUploaded(all[0]), asOldUploaded(all[1])}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			check := CheckShared(c.events)
			if check.First != nil || len(check.Problems) != 0 {
				t.Fatalf("honest use reads as tampering: %v", check.First)
			}
			if check.ExtGaps != c.gaps {
				t.Errorf("gaps = %d, want %d", check.ExtGaps, c.gaps)
			}
		})
	}
	// Where both sides have it, it's still compared.
	forged := append([]Event{}, all...)
	forged[2].PrevExt = "other"
	forged[2].ExtHash = forged[2].computeExtHash()
	if check := CheckShared(forged); check.First == nil || check.Problems[2] == "" {
		t.Errorf("a prev_ext that's wrong: %+v", check)
	}
}

func rewrite(t *testing.T, l *Log, edit func(lines []string) []string) {
	t.Helper()
	data, err := os.ReadFile(l.eventsPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := edit(strings.Split(strings.TrimSpace(string(data)), "\n"))
	out := ""
	for _, line := range lines {
		out += line + "\n"
	}
	if err := os.WriteFile(l.eventsPath(), []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyFileNoticesRemovedLines(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(t *testing.T, l *Log)
		want string
	}{
		{"last line removed", func(t *testing.T, l *Log) {
			rewrite(t, l, func(ls []string) []string { return ls[:len(ls)-1] })
		}, "had reached seq 3"},
		{"emptied", func(t *testing.T, l *Log) { rewrite(t, l, func([]string) []string { return nil }) }, "had reached seq 3"},
		{"deleted", func(t *testing.T, l *Log) {
			if err := os.Remove(l.eventsPath()); err != nil {
				t.Fatal(err)
			}
		}, "had reached seq 3"},
		{"cut, and head removed, but it was uploaded", func(t *testing.T, l *Log) {
			if err := l.MarkUploaded("team", 3); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(l.headPath()); err != nil {
				t.Fatal(err)
			}
			rewrite(t, l, func(ls []string) []string { return ls[:1] })
		}, "already uploaded"},
		{"chain rebuilt, head forgotten", func(t *testing.T, l *Log) { rebuildChain(t, l) }, "rebuilt"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := newLog(t)
			appendN(t, l, 3)
			c.edit(t, l)
			_, broken, err := l.VerifyFile()
			if err != nil || broken == nil || !strings.Contains(broken.Reason, c.want) {
				t.Fatalf("VerifyFile = %v, %v; want %q", broken, err, c.want)
			}
			// And writing again doesn't wash it out, or lose the new event to the cursor.
			e, err := l.Append(Event{Action: "exec", Host: "web1"})
			if err != nil {
				t.Fatal(err)
			}
			if e.Seq != 4 {
				t.Errorf("seq after tampering = %d, want it to carry on at 4", e.Seq)
			}
			if _, broken, _ := l.VerifyFile(); broken == nil {
				t.Error("one more event and it reads as intact")
			}
			if p, _ := l.Pending("team"); len(p) == 0 || p[len(p)-1].Seq != 4 {
				t.Errorf("the new event isn't waiting to upload: %+v", p)
			}
		})
	}
}

// rebuildChain does what a careful editor would: every hash recomputed, one detail
// changed. It's the one thing a file with no secret can't resist.
func rebuildChain(t *testing.T, l *Log) []Event {
	t.Helper()
	all, err := l.All()
	if err != nil {
		t.Fatal(err)
	}
	prev, ext := "", ""
	var out strings.Builder
	for i := range all {
		e := &all[i]
		if i == 0 {
			e.Detail = "rewritten"
		}
		e.PrevHash, e.PrevExt = prev, ext
		e.Hash = e.computeHash()
		e.ExtHash = e.computeExtHash()
		prev, ext = e.Hash, e.ExtHash
		b, _ := json.Marshal(e)
		out.Write(append(b, '\n'))
	}
	if err := os.WriteFile(l.eventsPath(), []byte(out.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return all
}

func TestWhatACarefulRebuildGetsPast(t *testing.T) {
	l := newLog(t)
	orig := appendN(t, l, 3)
	copied, _ := l.All()
	rebuilt := rebuildChain(t, l)
	last := rebuilt[len(rebuilt)-1]
	if err := os.WriteFile(l.headPath(), []byte(fmt.Sprintf("%d %s %s\n", last.Seq, last.Hash, last.ExtHash)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, broken, _ := l.VerifyFile(); broken != nil {
		t.Fatalf("with head rewritten too, a local check has nothing to go on, and shouldn't pretend: %v", broken)
	}
	// A copy elsewhere is what catches it.
	diff := CompareCopy(rebuilt, copied, orig[0].Device)
	if len(diff) != 3 || diff[0].Seq != 1 {
		t.Fatalf("CompareCopy = %+v", diff)
	}
}

func TestCompareCopy(t *testing.T) {
	l := newLog(t)
	all := appendN(t, l, 4)
	dev := all[0].Device
	other := Event{Device: "someone-else", Seq: 1, Hash: "x"}

	if d := CompareCopy(all, append([]Event{other}, all...), dev); len(d) != 0 {
		t.Errorf("same log: %+v", d)
	}
	if d := CompareCopy(all, all[:2], dev); len(d) != 0 {
		t.Errorf("a copy that's behind isn't a difference: %+v", d)
	}
	if d := CompareCopy(all[:3], all, dev); len(d) != 1 || d[0].Seq != 4 {
		t.Errorf("a removed event: %+v", d)
	}
	changed := append([]Event{}, all...)
	changed[1].Hash = "edited"
	if d := CompareCopy(changed, all, dev); len(d) != 1 || d[0].Seq != 2 {
		t.Errorf("a changed event: %+v", d)
	}
	edited := append([]Event{}, all...)
	edited[0].Detail = "edited, stored hash left as it was"
	if d := CompareCopy(edited, all, dev); len(d) != 1 || d[0].Seq != 1 {
		t.Errorf("an edit that kept the stored hash: %+v", d)
	}
	actor := append([]Event{}, all...)
	actor[3].ActorType = ActorHuman
	if d := CompareCopy(actor, all, dev); len(d) != 1 || d[0].Seq != 4 {
		t.Errorf("an edit to the actor that kept the stored hash: %+v", d)
	}
	ext := append([]Event{}, all...)
	ext[2].ExtHash = "edited"
	if d := CompareCopy(ext, all, dev); len(d) != 1 || d[0].Seq != 3 {
		t.Errorf("a changed extension: %+v", d)
	}
}

// Writing past a line with no newline must not glue the new event to it.
func TestAppendAfterAnUnterminatedLine(t *testing.T) {
	l := newLog(t)
	appendN(t, l, 1)
	data, _ := os.ReadFile(l.eventsPath())
	if err := os.WriteFile(l.eventsPath(), append(data, []byte(`{"cut`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(Event{Action: "exec", Host: "h"}); err != nil {
		t.Fatal(err)
	}
	all, _ := l.All()
	if len(all) != 3 || all[2].Action != "exec" {
		t.Fatalf("got %+v: the new one was lost to the cut line", all)
	}
}

func TestAppendCleansTheDeviceID(t *testing.T) {
	l := newLog(t)
	if err := os.WriteFile(l.devicePath(), []byte("d\x1b]0;x\x07\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := l.Append(Event{Action: "exec", Host: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(e.Device, "\x1b\x07") {
		t.Fatalf("%q", e.Device)
	}
}

// A full disk: part of the line goes in, then ENOSPC. The event is lost and says so, and the log
// is left as it was, not with half a line in it.
func TestFailedWriteLeavesNoPartOfALine(t *testing.T) {
	l := newLog(t)
	appendN(t, l, 2)
	before, _ := os.ReadFile(l.eventsPath())
	old := writeLine
	defer func() { writeLine = old }()
	writeLine = func(f *os.File, b []byte) (int, error) {
		n, _ := f.Write(b[:len(b)/2])
		return n, syscall.ENOSPC
	}
	if _, err := l.Append(Event{Action: "exec", Host: "web1"}); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("err = %v", err)
	}
	if after, _ := os.ReadFile(l.eventsPath()); string(after) != string(before) {
		t.Fatalf("the log kept %d more bytes", len(after)-len(before))
	}
	writeLine = old
	if _, broken, err := l.VerifyFile(); broken != nil || err != nil {
		t.Fatalf("%v, %v", broken, err)
	}
	if e, err := l.Append(Event{Action: "exec", Host: "web1"}); err != nil || e.Seq != 3 {
		t.Fatalf("%+v, %v", e, err)
	}
}

// Taking head away to hide a cut: it fails while it's gone, and leaves a mark once it's back.
func TestMissingWitness(t *testing.T) {
	l := newLog(t)
	appendN(t, l, 3)
	rewrite(t, l, func(ls []string) []string { return ls[:2] })
	if err := os.Remove(l.headPath()); err != nil {
		t.Fatal(err)
	}
	if _, broken, _ := l.VerifyFile(); broken == nil || !strings.Contains(broken.Reason, "witness") {
		t.Fatalf("%v", broken)
	}
	if _, err := l.Append(Event{Action: "exec", Host: "h"}); err != nil {
		t.Fatal(err)
	}
	if notes, _ := l.Notes(); len(notes.Repairs) != 1 {
		t.Fatalf("no mark left: %+v", notes)
	}
	all, _ := l.All()
	if !strings.Contains(all[len(all)-2].Detail, "witness") {
		t.Errorf("%+v", all[len(all)-2])
	}
	if _, broken, err := l.VerifyFile(); broken != nil || err != nil {
		t.Fatalf("%v, %v", broken, err)
	}
}

// No false alarm where no witness is to be expected.
func TestNoWitnessNoAlarm(t *testing.T) {
	t.Run("a log from before head", func(t *testing.T) {
		l := newLog(t)
		device, _ := l.DeviceID()
		prev := ""
		for seq := int64(1); seq <= 3; seq++ {
			e := v022Event{Device: device, Seq: seq, Time: time.Date(2026, 1, 1, 0, 0, int(seq), 0, time.UTC), User: "u", Action: "exec", Host: "h", PrevHash: prev}
			e.Hash = e.computeHash()
			writeV022(t, l, e)
			prev = e.Hash
		}
		if _, broken, err := l.VerifyFile(); broken != nil || err != nil {
			t.Fatalf("%v, %v", broken, err)
		}
		if _, err := l.Append(Event{Action: "exec", Host: "h"}); err != nil {
			t.Fatal(err)
		}
		if notes, _ := l.Notes(); len(notes.Repairs) != 0 {
			t.Fatalf("%+v", notes)
		}
	})
	t.Run("a crash right after the first event", func(t *testing.T) {
		l := newLog(t)
		appendN(t, l, 1)
		if err := os.Remove(l.headPath()); err != nil {
			t.Fatal(err)
		}
		if _, broken, err := l.VerifyFile(); broken != nil || err != nil {
			t.Fatalf("%v, %v", broken, err)
		}
	})
	t.Run("an empty log", func(t *testing.T) {
		l := newLog(t)
		if _, broken, err := l.VerifyFile(); broken != nil || err != nil {
			t.Fatalf("%v, %v", broken, err)
		}
	})
}
