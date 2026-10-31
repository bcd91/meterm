package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"meterm/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Hosts: []config.Host{
			{Name: "web1", Address: "10.0.0.1", Group: "prod", Tags: []string{"web"}},
			{Name: "web2", Address: "10.0.0.2", Group: "prod", Tags: []string{"web"}},
			{Name: "db1", Address: "10.0.0.3", Group: "prod", Tags: []string{"db"}},
			{Name: "staging1", Address: "10.0.1.1", Group: "staging", Tags: []string{"web"}},
		},
	}
}

func names(hosts []config.Host) []string {
	out := make([]string, len(hosts))
	for i, h := range hosts {
		out[i] = h.Name
	}
	return out
}

func TestSelectHostsByTag(t *testing.T) {
	got, err := selectHosts(testConfig(), nil, []string{"web"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"staging1", "web1", "web2"} // sorted by name
	if !equalStrings(names(got), want) {
		t.Errorf("got %v, want %v", names(got), want)
	}
}

func TestSelectHostsByGroup(t *testing.T) {
	got, err := selectHosts(testConfig(), nil, nil, []string{"prod"}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"db1", "web1", "web2"}
	if !equalStrings(names(got), want) {
		t.Errorf("got %v, want %v", names(got), want)
	}
}

func TestSelectHostsAll(t *testing.T) {
	got, err := selectHosts(testConfig(), nil, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Errorf("got %d hosts, want 4", len(got))
	}
}

func TestSelectHostsExplicitNameDedupedWithTag(t *testing.T) {
	got, err := selectHosts(testConfig(), []string{"web1"}, []string{"web"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"staging1", "web1", "web2"}
	if !equalStrings(names(got), want) {
		t.Errorf("got %v, want %v (web1 should not be duplicated)", names(got), want)
	}
}

func TestSelectHostsUnknownName(t *testing.T) {
	if _, err := selectHosts(testConfig(), []string{"ghost"}, nil, nil, false); err == nil {
		t.Error("expected an error for an unknown host name")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPrefixWriterBuffersPartialLines(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	var mu sync.Mutex
	pw := &prefixWriter{mu: &mu, out: w, prefix: "[host]"}

	pw.Write([]byte("hello "))
	pw.Write([]byte("world\npartial"))
	pw.Flush()
	w.Close()

	var buf bytes.Buffer
	buf.ReadFrom(r)
	got := buf.String()
	want := "[host] hello world\n[host] partial\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExecRemoteLine(t *testing.T) {
	cases := []struct {
		command []string
		windows bool
		want    string
	}{
		{[]string{"df -h | grep /srv"}, false, "df -h | grep /srv"},
		{[]string{"echo", "$(id)"}, false, `'echo' '$(id)'`},
		{[]string{"sh", "-c", "echo a; echo b"}, false, `'sh' '-c' 'echo a; echo b'`},
		{[]string{"echo", "it's"}, false, `'echo' 'it'\''s'`},
		{[]string{"ipconfig", "/all"}, true, "ipconfig /all"},
	}
	for _, c := range cases {
		if got := execRemoteLine(c.command, c.windows); got != c.want {
			t.Errorf("%q: got %s, want %s", c.command, got, c.want)
		}
	}
}

func TestExecDropsEscapesForATerminal(t *testing.T) {
	run := func(_ context.Context, _ config.Host, _ []string, stdout, _ io.Writer) error {
		_, err := io.WriteString(stdout, "one\x1b]52;c;ZXZpbA==\x07 \x1b[31mred\x1b[0m\r\ntwo\x1b]0;")
		return err
	}
	hosts := []config.Host{{Name: "web"}}
	var safe bytes.Buffer
	if err := runExec(hosts, []string{"x"}, run, execOptions{format: "text", out: &safe, safeOut: true}); err != nil {
		t.Fatal(err)
	}
	if s := safe.String(); strings.Contains(s, "]52;") || strings.Contains(s, "]0;") || strings.Contains(s, "\r") ||
		!strings.Contains(s, "one \x1b[31mred\x1b[0m\n") || !strings.Contains(s, "] two\n") {
		t.Errorf("filtered: %q", s)
	}
	var plain bytes.Buffer
	if err := runExec(hosts, []string{"x"}, run, execOptions{format: "text", out: &plain}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plain.String(), "]52;") {
		t.Errorf("not a terminal, so it should be as sent: %q", plain.String())
	}
	var j bytes.Buffer
	if err := runExec(hosts, []string{"x"}, run, execOptions{format: "json", out: &j, safeOut: true}); err != nil {
		t.Fatal(err)
	}
	var results []execResult
	if err := json.Unmarshal(j.Bytes(), &results); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if !strings.Contains(results[0].Stdout, "]52;") {
		t.Errorf("json holds what was sent: %q", results[0].Stdout)
	}
}
