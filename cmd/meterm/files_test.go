package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"meterm/internal/config"
)

// fakeHostileSSH puts an ssh (and an sftp) first on PATH that notes each run in the file
// it returns and fails with stderr that tries to retitle and clear the
// terminal.
func fakeHostileSSH(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as ssh")
	}
	bin := t.TempDir()
	runs := filepath.Join(bin, "runs")
	script := "#!/bin/sh\necho run >> '" + runs + "'\nprintf '\\033]0;PWNED\\007\\033[2J\\033[31mPermission denied (publickey).\\n' >&2\nexit 255\n"
	// sftp too: the real one runs /usr/bin/ssh by its full path, past PATH.
	for _, name := range []string{"ssh", "sftp"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return runs
}

func ranTimes(runs string) int {
	b, _ := os.ReadFile(runs)
	return strings.Count(string(b), "run")
}

// `meterm files` on a production host asks once, y only; --yes skips the
// question (the picker's token: TestFilesProductionToken). When
// SFTP can't be opened, ssh's text is shown safely and the failure logged.
func TestFilesOnProduction(t *testing.T) {
	dir := cliSandbox(t)
	runs := fakeHostileSSH(t)
	cfg, _ := config.Load(cfgPath)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upsert(config.Host{Name: "web1", Address: "192.0.2.60", Env: config.EnvProduction})
		c.Upsert(config.Host{Name: "dev1", Address: "192.0.2.61"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	files := func(args ...string) error {
		c := newFilesCmd()
		c.SetArgs(args)
		c.SilenceUsage, c.SilenceErrors = true, true
		return c.Execute()
	}

	asTerminal(t, true)
	var out string
	var err error
	for _, answer := range []string{"n\n", "\n", ""} {
		withStdin(t, answer, func() { out = stdout(t, func() { err = files("web1") }) })
		if !strings.Contains(out, "PRODUCTION") || !strings.Contains(out, "Open the file manager on it?") || ranTimes(runs) != 0 {
			t.Fatalf("answer %q: err %v, out %q, ssh ran %d times; want a question and no connection", answer, err, out, ranTimes(runs))
		}
		// A no is a run that didn't do what was asked: not exit status 0.
		if err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Errorf("answer %q: err %v, want a cancel, so meterm exits non-zero", answer, err)
		}
	}

	withStdin(t, "n\n", func() { out = stdout(t, func() { err = files("--shell", "web1") }) })
	if !strings.Contains(out, "PRODUCTION") || ranTimes(runs) != 0 || err == nil {
		t.Fatalf("--shell: out %q, err %v, ssh ran %d times; want a question, no connection and a cancel", out, err, ranTimes(runs))
	}
	if !strings.Contains(out, "Open the sftp prompt on it?") || strings.Contains(out, "file manager") {
		t.Errorf("--shell asked %q, want it to name the sftp prompt", out)
	}

	withStdin(t, "y\n", func() { out = stdout(t, func() { err = files("web1", "/srv/www") }) })
	if ranTimes(runs) != 1 || err == nil {
		t.Fatalf("after y: ssh ran %d times, err %v", ranTimes(runs), err)
	}
	if msg := err.Error(); strings.ContainsAny(msg, "\x1b\x07") || strings.Contains(msg, "]0;") || !strings.Contains(msg, "Permission denied (publickey).") {
		t.Errorf("error %q: want ssh's words without its escapes", msg)
	}
	events, _ := os.ReadFile(filepath.Join(dir, "activity", "events.jsonl"))
	if !strings.Contains(string(events), "failed: files /srv/www (production, asked): ") || !strings.Contains(string(events), "Permission denied (publickey).") || strings.Contains(string(events), `\u001b`) {
		t.Errorf("activity log = %s; want the failure, safely", events)
	}

	withStdin(t, "", func() { _ = stdout(t, func() { err = files("--yes", "web1") }) })
	if ranTimes(runs) != 2 {
		t.Errorf("--yes: ssh ran %d times in all, want 2", ranTimes(runs))
	}
	withStdin(t, "", func() { out = stdout(t, func() { err = files("dev1") }) })
	if ranTimes(runs) != 3 || strings.Contains(out, "PRODUCTION") {
		t.Errorf("not production: ssh ran %d times in all (want 3), out %q", ranTimes(runs), out)
	}
}
