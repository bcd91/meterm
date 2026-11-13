package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/config"
	"meterm/internal/fileman"
)

// The picker's one-time token is the only thing that skips the production
// question: METERM_FROM_TUI alone doesn't, a token is good once, and one
// that isn't ours, isn't private or doesn't match is ignored. What the log
// says about the question is what happened.
func TestFilesProductionToken(t *testing.T) {
	dir := cliSandbox(t)
	runs := fakeHostileSSH(t)
	cfg, _ := config.Load(cfgPath)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upsert(config.Host{Name: "web1", Address: "192.0.2.60", Env: config.EnvProduction})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	files := func(args ...string) error { return runRoot(t, append([]string{"files"}, args...)...) }
	lastLine := func() string {
		b, _ := os.ReadFile(filepath.Join(dir, "activity", "events.jsonl"))
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		return lines[len(lines)-1]
	}
	const goodTok = "0123456789abcdef0123456789abcdef" // the length prodask.Issue writes
	token := func(mode os.FileMode, content string) string {
		p := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		t.Setenv("METERM_PROD_TOKEN", p)
		t.Setenv("METERM_PROD_TOKEN_VALUE", goodTok)
		return p
	}
	asTerminal(t, true)
	var out, errOut string

	// Typed by hand: asked, and nothing said about a picker.
	withStdin(t, "n\n", func() { errOut = stderrOf(t, func() { out = stdout(t, func() { _ = files("web1") }) }) })
	if strings.Count(out, "[y/N]") != 1 || ranTimes(runs) != 0 || strings.Contains(errOut, "picker") {
		t.Fatalf("by hand: out %q, stderr %q, ssh ran %d times; want one question, no picker note", out, errOut, ranTimes(runs))
	}

	t.Setenv(fromTUIEnv, "1")
	withStdin(t, "n\n", func() { errOut = stderrOf(t, func() { out = stdout(t, func() { _ = files("web1") }) }) })
	if !strings.Contains(out, "Open the file manager on it?") || ranTimes(runs) != 0 {
		t.Fatalf("METERM_FROM_TUI alone: out %q, ssh ran %d times; want the question", out, ranTimes(runs))
	}
	if !strings.Contains(errOut, "picker token missing; asking here.") {
		t.Errorf("METERM_FROM_TUI alone: stderr %q; want the note that the picker's token is missing", errOut)
	}

	// Launched by the picker after its y: no second question, no note.
	p := token(0o600, goodTok)
	withStdin(t, "", func() { errOut = stderrOf(t, func() { out = stdout(t, func() { _ = files("web1", "/srv") }) }) })
	if strings.Contains(out, "[y/N]") || ranTimes(runs) != 1 || strings.Contains(errOut, "picker token") {
		t.Fatalf("a good token: out %q, stderr %q, ssh ran %d times; want no question and no note", out, errOut, ranTimes(runs))
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("the token is still there after use: %v", err)
	}
	if l := lastLine(); !strings.Contains(l, "failed: files /srv (production, picker asked)") {
		t.Errorf("log %s", l)
	}
	withStdin(t, "n\n", func() { out = stdout(t, func() { _ = files("web1") }) })
	if !strings.Contains(out, "[y/N]") || ranTimes(runs) != 1 {
		t.Errorf("a used token: out %q, ssh ran %d times; want the question again", out, ranTimes(runs))
	}
	for _, c := range []struct {
		name    string
		mode    os.FileMode
		content string
	}{{"readable by others", 0o644, goodTok}, {"another value", 0o600, "fedcba9876543210fedcba9876543210"}} {
		p := token(c.mode, c.content)
		withStdin(t, "n\n", func() { out = stdout(t, func() { _ = files("web1") }) })
		if !strings.Contains(out, "[y/N]") || ranTimes(runs) != 1 {
			t.Errorf("%s: out %q, ssh ran %d times; want the question", c.name, out, ranTimes(runs))
		}
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: a token not taken was removed: %v", c.name, err)
		}
	}
	t.Setenv("METERM_PROD_TOKEN", "")
	t.Setenv("METERM_PROD_TOKEN_VALUE", "")

	withStdin(t, "y\n", func() { out = stdout(t, func() { _ = files("web1", "/a") }) })
	if l := lastLine(); ranTimes(runs) != 2 || !strings.Contains(l, "failed: files /a (production, asked)") {
		t.Errorf("after y: ssh ran %d times, log %s", ranTimes(runs), l)
	}
	asTerminal(t, false)
	withStdin(t, "", func() { out = stdout(t, func() { _ = files("web1", "/b") }) })
	if l := lastLine(); ranTimes(runs) != 3 || strings.Contains(out, "[y/N]") || !strings.Contains(l, "failed: files /b (production, no prompt: not a terminal)") {
		t.Errorf("not a terminal: out %q, ssh ran %d times, log %s", out, ranTimes(runs), l)
	}
}

// runRoot runs meterm with args as main does, through the root command, so
// what runs before every command (taking the picker's token) runs too. The
// answer taken is forgotten after, as a new process would.
func runRoot(t *testing.T, args ...string) error {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(args)
	defer func() { pickerAnswer, warnedPicker = nil, false }()
	return root.Execute()
}

// A host reached through a production jump host is asked about as one, and
// the log says it was via production.
func TestFilesViaProductionAsks(t *testing.T) {
	dir := cliSandbox(t)
	runs := fakeHostileSSH(t)
	saveHosts(t, config.Host{Name: "web1", Address: "192.0.2.60", Env: config.EnvProduction},
		config.Host{Name: "inner", Address: "192.0.2.61", ProxyJump: "web1"})
	lastLine := func() string {
		b, _ := os.ReadFile(filepath.Join(dir, "activity", "events.jsonl"))
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		return lines[len(lines)-1]
	}
	asTerminal(t, true)
	var out string
	withStdin(t, "n\n", func() { out = stdout(t, func() { _ = runRoot(t, "files", "inner") }) })
	if !strings.Contains(out, "reached through a PRODUCTION host") || strings.Count(out, "[y/N]") != 1 || ranTimes(runs) != 0 {
		t.Fatalf("n: out %q, ssh ran %d times; want one question and nothing run", out, ranTimes(runs))
	}
	withStdin(t, "y\n", func() { out = stdout(t, func() { _ = runRoot(t, "files", "inner", "/a") }) })
	if l := lastLine(); ranTimes(runs) != 1 || !strings.Contains(l, "failed: files /a (via production, asked)") {
		t.Errorf("y: ssh ran %d times, log %s", ranTimes(runs), l)
	}
	asTerminal(t, false)
	var errOut string
	withStdin(t, "", func() { errOut = stderrOf(t, func() { _ = runRoot(t, "files", "inner", "/b") }) })
	if l := lastLine(); !strings.Contains(l, "(via production, no prompt: not a terminal)") || !strings.Contains(errOut, "reached through a PRODUCTION host") {
		t.Errorf("not a terminal: stderr %q, log %s", errOut, l)
	}
}

// n to the files question is a cancel: exit 1, and the picker is told it
// was cancelled, not failed. A dropped stalled connection is logged as
// stopped.
func TestFilesCancelAndDropAreNotFailures(t *testing.T) {
	cliSandbox(t)
	fakeHostileSSH(t)
	saveHosts(t, config.Host{Name: "web1", Address: "192.0.2.60", Env: config.EnvProduction})
	said := filepath.Join(t.TempDir(), "session")
	t.Setenv("METERM_SESSION_OUTCOME", said)
	asTerminal(t, true)
	var err error
	withStdin(t, "n\n", func() { stdout(t, func() { err = runRoot(t, "files", "web1") }) })
	if !errors.Is(err, errCancelled) {
		t.Fatalf("n: err %v; want a cancel", err)
	}
	afterRun(err)
	if b, _ := os.ReadFile(said); strings.TrimSpace(string(b)) != "cancelled" {
		t.Errorf("the picker was told %q", b)
	}
	o := outcomeOf(fmt.Errorf("stopped waiting for web1 during a copy: %w", fileman.ErrDropped), 0)
	if o.Note != "stopped" || o.ExitCode != nil {
		t.Errorf("a dropped connection's outcome: %+v", o)
	}
	if o := outcomeOf(errors.New("couldn't open SFTP"), 0); o.Note != "failed" {
		t.Errorf("a real failure's outcome: %+v", o)
	}
}
