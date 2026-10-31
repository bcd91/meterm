package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLogsScriptQuotesPaths(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		want  string
	}{
		{"plain", []string{"/var/log/syslog"}, `tail -n 5 -- '/var/log/syslog'`},
		{"glob", []string{"/var/log/api_*.log"}, `tail -n 5 -- '/var/log/api_'*'.log'`},
		{"class", []string{"a[12]?.log"}, `tail -n 5 -- 'a'['12']?'.log'`},
		{"home", []string{"~/x y.log"}, `tail -n 5 -- "$HOME"/'x y.log'`},
		{"substitution", []string{"/x/$(touch p)"}, `tail -n 5 -- '/x/$(touch p)'`},
		{"backticks", []string{"`id`"}, "tail -n 5 -- '`id`'"},
		{"separator", []string{"/x; id"}, `tail -n 5 -- '/x; id'`},
		{"quote", []string{"it's"}, `tail -n 5 -- 'it'\''s'`},
		{"option", []string{"--follow=name"}, `tail -n 5 -- '--follow=name'`},
		{"newline", []string{"a\nid"}, "tail -n 5 -- 'a\nid'"},
		{"empty", []string{""}, `tail -n 5 -- ''`},
		{"two", []string{"a", "b"}, `tail -n 5 -- 'a' 'b'`},
	}
	for _, c := range cases {
		if got := logsTailScript(c.paths, 5, false); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
	if got := logsTailScript([]string{"a"}, 1, true); got != `tail -f -n 1 -- 'a'` {
		t.Errorf("follow: %s", got)
	}
}

// What matters is what a real shell does with it, so ask one.
func TestLogsScriptRunsNothingInAShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app-1.log"), []byte("from the log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(dir, "canary")
	for _, p := range []string{
		"$(touch " + canary + ")", "`touch " + canary + "`", "x; touch " + canary,
		"x\ntouch " + canary, "x' ; touch " + canary + " ; '", "[$(touch " + canary + ")]*",
	} {
		c := exec.Command("sh", "-c", logsTailScript([]string{p}, 1, false))
		c.Dir = dir
		_ = c.Run() // tail not finding the file is fine
		if _, err := os.Stat(canary); err == nil {
			t.Fatalf("%q ran on the host", p)
		}
	}
	c := exec.Command("sh", "-c", logsTailScript([]string{"app-*.log"}, 1, false))
	c.Dir = dir
	out, err := c.Output()
	if err != nil || !strings.Contains(string(out), "from the log") {
		t.Fatalf("glob didn't expand: %q, %v", out, err)
	}
}
