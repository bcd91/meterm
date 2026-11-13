package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/config"
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
	files := func(args ...string) error {
		c := newFilesCmd()
		c.SetArgs(args)
		c.SilenceUsage, c.SilenceErrors = true, true
		return c.Execute()
	}
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
	var out string

	t.Setenv(fromTUIEnv, "1")
	withStdin(t, "n\n", func() { out = stdout(t, func() { _ = files("web1") }) })
	if !strings.Contains(out, "Open the file manager on it?") || ranTimes(runs) != 0 {
		t.Fatalf("METERM_FROM_TUI alone: out %q, ssh ran %d times; want the question", out, ranTimes(runs))
	}

	p := token(0o600, goodTok)
	withStdin(t, "", func() { out = stdout(t, func() { _ = files("web1", "/srv") }) })
	if strings.Contains(out, "[y/N]") || ranTimes(runs) != 1 {
		t.Fatalf("a good token: out %q, ssh ran %d times; want no question", out, ranTimes(runs))
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
