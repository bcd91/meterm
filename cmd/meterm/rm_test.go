package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"meterm/internal/activity"
	"meterm/internal/config"
)

// withStdin runs fn with in as standard input.
func withStdin(t *testing.T, in string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() { io.WriteString(w, in); w.Close() }()
	old := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = old }()
	fn()
}

// stderrOf runs fn and returns what it wrote to standard error.
func stderrOf(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

// rm on a production host says so, before asking and when told --yes.
func TestRmSaysProduction(t *testing.T) {
	cliSandbox(t)
	cfg, _ := config.Load(cfgPath)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upsert(config.Host{Name: "pay1", Address: "192.0.2.50", Env: config.EnvProduction})
		c.Upsert(config.Host{Name: "dev1", Address: "192.0.2.51"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rm := func(args ...string) error {
		c := newRmCmd()
		c.SetArgs(args)
		return c.Execute()
	}
	var out string
	withStdin(t, "n\n", func() { out = stdout(t, func() { _ = rm("pay1") }) })
	if !strings.Contains(out, "PRODUCTION") || !strings.Contains(out, "Cancelled") {
		t.Errorf("prompt for a production host: %q", out)
	}
	errOut := stderrOf(t, func() { _ = rm("pay1", "--yes") })
	if !strings.Contains(errOut, "PRODUCTION") {
		t.Errorf("--yes on a production host: %q", errOut)
	}
	withStdin(t, "n\n", func() { out = stdout(t, func() { _ = rm("dev1") }) })
	if strings.Contains(out, "PRODUCTION") {
		t.Errorf("plain host called production: %q", out)
	}
}

// rm leaves a line in the activity log, like the picker's delete does.
func TestRmIsLogged(t *testing.T) {
	cliSandbox(t)
	cfg, _ := config.Load(cfgPath)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upsert(config.Host{Name: "pay1", Address: "192.0.2.50", Env: config.EnvProduction})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c := newRmCmd()
	c.SetArgs([]string{"pay1", "--yes"})
	_ = stderrOf(t, func() {
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	l, err := activity.Open(os.Getenv("METERM_ACTIVITY_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	events, _ := l.All()
	if len(events) != 1 || events[0].Action != "rm" || events[0].Host != "pay1" || !strings.Contains(events[0].Detail, "production") {
		t.Errorf("events after rm: %+v", events)
	}
}

// rm says which hosts jump through the one it removes.
func TestRmNamesHostsThatJumpThroughIt(t *testing.T) {
	cliSandbox(t)
	saveHosts(t, config.Host{Name: "bastion", Address: "192.0.2.1"},
		config.Host{Name: "web11", Address: "10.0.0.11", ProxyJump: "bastion"})
	rm := func(args ...string) error {
		c := newRmCmd()
		c.SetArgs(args)
		return c.Execute()
	}
	var out string
	withStdin(t, "n\n", func() { out = stdout(t, func() { _ = rm("bastion") }) })
	if !strings.Contains(out, "web11") || !strings.Contains(out, "jump") || !strings.Contains(out, "Cancelled") {
		t.Errorf("rm bastion: %q", out)
	}
	errOut := stderrOf(t, func() { _ = stdout(t, func() { _ = rm("bastion", "--yes") }) })
	if !strings.Contains(errOut, "web11") {
		t.Errorf("rm --yes bastion: %q", errOut)
	}
}
