package agents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestParseRealShape(t *testing.T) {
	got, err := Parse([]byte(`[{"pid":25728,"cwd":"/home/a/src","kind":"interactive","startedAt":1793311202058,
		"sessionId":"abc","name":"fix bug","status":"idle"},
		{"id":"x","state":"working","waitingFor":"permission","future":{"a":[1]}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || *got[0].PID != 25728 || *got[0].StartedAt != 1793311202058 || got[0].Name != "fix bug" || got[1].WaitingFor != "permission" {
		t.Errorf("got %+v", got)
	}
	b, _ := json.Marshal(got[1])
	if string(b) != `{"id":"x","state":"working","waitingFor":"permission","event":"needs_input","agent_kind":"claude-code","source":"claude agents --json","confidence":"high"}` {
		t.Errorf("unknown fields or empty ones got through: %s", b)
	}
}

func TestParseEmptyIsNotNil(t *testing.T) {
	got, err := Parse([]byte(" [ ]\n"))
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

// Whatever comes back, it's an error rather than a list of nothing, and the error
// holds nothing the host sent.
func TestParseRefuses(t *testing.T) {
	const hostile = "\x1b]52;c;aGk=\x07"
	big := "[" + strings.Repeat(`{},`, MaxAgents) + `{}]`
	for name, in := range map[string]string{
		"empty":        "",
		"null":         "null",
		"object":       `{"agents":[]}`,
		"number":       "1",
		"string":       `"` + `\u001b]0;x` + `"`,
		"text":         "Unknown command: agents " + hostile,
		"motd first":   "Welcome\n[]",
		"trailing":     "[] junk",
		"null item":    "[null]",
		"array item":   "[[]]",
		"string item":  `["a"]`,
		"cwd number":   `[{"cwd":1}]`,
		"name object":  `[{"name":{"a":1}}]`,
		"waiting bool": `[{"waitingFor":true}]`,
		"pid string":   `[{"pid":"1; rm -rf /"}]`,
		"pid float":    `[{"pid":1.5}]`,
		"pid negative": `[{"pid":-1}]`,
		"pid huge":     `[{"pid":123456789012345678901234567890}]`,
		"started str":  `[{"startedAt":"2026"}]`,
		"cut off":      `[{"cwd":"/a"`,
		"too many":     big,
		"too large":    "[" + strings.Repeat(" ", MaxOutput) + "]",
		"too deep":     strings.Repeat("[", 20000),
	} {
		got, err := Parse([]byte(in))
		if err == nil {
			t.Errorf("%s: accepted, %v", name, got)
			continue
		}
		if got != nil {
			t.Errorf("%s: a list came with the error", name)
		}
		if strings.ContainsAny(err.Error(), "\x1b\x07") || strings.Contains(err.Error(), "rm -rf") {
			t.Errorf("%s: the error repeats the host: %q", name, err)
		}
	}
}

func TestParseCleansEveryString(t *testing.T) {
	const bad = "x\x1b[2J\x1b]52;c;aGk=\x07\u009b1m‮⁦​\ny"
	enc, _ := json.Marshal(bad)
	var fields []string
	for _, f := range []string{"cwd", "kind", "id", "state", "status", "waitingFor", "sessionId", "name"} {
		fields = append(fields, fmt.Sprintf("%q:%s", f, enc))
	}
	got, err := Parse([]byte("[{" + strings.Join(fields, ",") + "}]"))
	if err != nil {
		t.Fatal(err)
	}
	a := got[0]
	for _, s := range []string{a.Cwd, a.Kind, a.ID, a.State, a.Status, a.WaitingFor, a.SessionID, a.Name} {
		if strings.ContainsAny(s, "\x1b\x07\u009b‮⁦​\n") {
			t.Errorf("got through: %q", s)
		}
		if !strings.HasPrefix(s, "x") || !strings.HasSuffix(s, "y") {
			t.Errorf("the text itself was lost: %q", s)
		}
	}
	out, _ := json.Marshal(a)
	if bytes.Contains(out, []byte(`\u001b`)) {
		t.Errorf("in the JSON: %s", out)
	}
}

func TestParseKeepsExactKeysOnly(t *testing.T) {
	got, err := Parse([]byte(`[{"PID":"nope","CWD":5,"Name":{}}]`))
	if err != nil || got[0].PID != nil || got[0].Cwd != "" {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestParseClipsLongStrings(t *testing.T) {
	got, err := Parse([]byte(`[{"cwd":"` + strings.Repeat("a", 5000) + `"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(got[0].Cwd)); n != maxText || !strings.HasSuffix(got[0].Cwd, "…") {
		t.Errorf("%d runes", n)
	}
}

func TestParseNullMeansAbsent(t *testing.T) {
	got, err := Parse([]byte(`[{"pid":null,"cwd":null,"waitingFor":null}]`))
	if err != nil || got[0].PID != nil || got[0].Cwd != "" {
		t.Errorf("got %+v, %v", got, err)
	}
}

// The command is what's promised: one line, one pair of quotes, and no way for it
// to be anything else, since it's a constant.
func TestRemoteCommand(t *testing.T) {
	if strings.Count(RemoteCommand, "'") != 2 || !strings.HasPrefix(RemoteCommand, "sh -c '") || !strings.HasSuffix(RemoteCommand, "'") {
		t.Errorf("quoting: %s", RemoteCommand)
	}
	for _, want := range []string{"$HOME/.local/bin", "claude agents --json", "exit 127", "|| exit 1"} {
		if !strings.Contains(RemoteCommand, want) {
			t.Errorf("no %q", want)
		}
	}
	if strings.ContainsAny(RemoteCommand, "\n\r") {
		t.Error("more than a line")
	}
}
