package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"meterm/internal/config"
)

// shRunner runs command locally with sh instead of over ssh; the host name
// picks the script, so one run can mix successes, failures and hangs.
func shRunner(scripts map[string]string) execRunner {
	return func(ctx context.Context, h config.Host, _ []string, stdout, stderr io.Writer) error {
		c := exec.CommandContext(ctx, "sh", "-c", scripts[h.Name])
		c.Stdout, c.Stderr = stdout, stderr
		return c.Run()
	}
}

func TestExecJSONReportsEachHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	hosts := []config.Host{{Name: "ok"}, {Name: "bad"}, {Name: "slow"}}
	run := shRunner(map[string]string{"ok": "echo hello", "bad": "echo oops >&2; exit 3", "slow": "sleep 5"})
	var out bytes.Buffer
	err := runExec(hosts, []string{"x"}, run, execOptions{parallel: 3, timeout: 300 * time.Millisecond, format: "json", out: &out})
	if err == nil || !strings.Contains(err.Error(), "2 of 3") {
		t.Fatalf("err = %v, want 2 of 3 failed", err)
	}
	var got []execResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	by := map[string]execResult{}
	for _, r := range got {
		by[r.Host] = r
	}
	if r := by["ok"]; r.ExitCode != 0 || r.Stdout != "hello\n" || r.Error != "" {
		t.Errorf("ok = %+v", r)
	}
	if r := by["bad"]; r.ExitCode != 3 || r.Stderr != "oops\n" {
		t.Errorf("bad = %+v", r)
	}
	if r := by["slow"]; r.ExitCode != -1 || !strings.Contains(r.Error, "timed out") {
		t.Errorf("slow = %+v", r)
	}
}

func TestExecTextSummary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	hosts := []config.Host{{Name: "a"}, {Name: "b"}}
	var out bytes.Buffer
	err := runExec(hosts, []string{"x"}, shRunner(map[string]string{"a": "echo hi", "b": "exit 0"}), execOptions{out: &out})
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"hi", "2/2 succeeded", "HOST", "EXIT"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
}
