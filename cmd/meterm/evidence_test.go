package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/activity"
	"meterm/internal/config"
)

// events is what the activity log holds now, as "action host: detail" lines,
// with an outcome folded onto the line it's the outcome of, as
// "... => exit 1" or "... => failed": its seq and duration vary run to run.
func events(t *testing.T) []string {
	t.Helper()
	l, err := activity.Open(os.Getenv("METERM_ACTIVITY_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	all, err := l.All()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	line := map[int64]int{}
	for _, e := range all {
		if i, ok := line[e.RefSeq]; ok && e.Action == activity.ActionOutcome {
			end := strings.TrimPrefix(e.Detail, fmt.Sprintf("seq %d: ", e.RefSeq))
			if j := strings.LastIndex(end, ", "); j >= 0 {
				end = end[:j]
			}
			out[i] += " => " + end
			continue
		}
		line[e.Seq] = len(out)
		out = append(out, e.Action+" "+e.Host+": "+e.Detail)
	}
	return out
}

func saveHosts(t *testing.T, hosts ...config.Host) {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		for _, h := range hosts {
			c.Upsert(h)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// exitCalls stands in for os.Exit, so a failing scp doesn't end the test.
func exitCalls(t *testing.T) *[]int {
	t.Helper()
	var codes []int
	old := osExit
	osExit = func(code int) { codes = append(codes, code); panic(exitPanic{}) }
	t.Cleanup(func() { osExit = old })
	return &codes
}

type exitPanic struct{}

func catchExit(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(exitPanic); !ok {
				panic(r)
			}
		}
	}()
	fn()
}

func has(lines []string, parts ...string) bool {
	for _, l := range lines {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(l, p)
		}
		if ok {
			return true
		}
	}
	return false
}

// exec leaves a line when it starts on each host and one when it ends there,
// with the exit code: a failing command doesn't read as a success.
func TestExecLogsEachHostsOutcome(t *testing.T) {
	cliSandbox(t)
	installFakeSSH(t, t.TempDir())
	t.Setenv(fakeSSHExitEnv, "1")
	saveHosts(t, config.Host{Name: "web03", Address: "192.0.2.73"})
	c := newExecCmd()
	c.SetArgs([]string{"web03", "--", "false"})
	_ = stdout(t, func() {
		_ = stderrOf(t, func() {
			if err := c.Execute(); err == nil {
				t.Error("exec of false succeeded")
			}
		})
	})
	ev := events(t)
	if !has(ev, "exec web03: false => exit 1") {
		t.Errorf("events: %q", ev)
	}
	t.Setenv(fakeSSHExitEnv, "255")
	c = newExecCmd()
	c.SetArgs([]string{"web03", "--", "true"})
	_ = stdout(t, func() { _ = stderrOf(t, func() { _ = c.Execute() }) })
	if ev := events(t); !has(ev, "exec web03: true => exit 255") {
		t.Errorf("events after 255: %q", ev)
	}
}

// cp says how it ended too.
func TestCopyLogsItsOutcome(t *testing.T) {
	dir := cliSandbox(t)
	installFake(t, t.TempDir(), "scp")
	t.Setenv(fakeSSHExitEnv, "1")
	codes := exitCalls(t)
	saveHosts(t, config.Host{Name: "web03", Address: "192.0.2.73"})
	local := filepath.Join(dir, "f.txt")
	_ = os.WriteFile(local, []byte("x"), 0o600)
	catchExit(func() { _ = runCopy(local, "web03:/tmp/", false, false, nil, false) })
	if len(*codes) != 1 || (*codes)[0] != 1 {
		t.Errorf("exit codes %v", *codes)
	}
	ev := events(t)
	if !has(ev, "cp web03: upload", "=> exit 1") {
		t.Errorf("events: %q", ev)
	}
	t.Setenv(fakeSSHExitEnv, "0")
	if err := runCopy("web03:/etc/hosts", local, false, false, nil, false); err != nil {
		t.Fatal(err)
	}
	if ev := events(t); !has(ev, "cp web03: download", "=> exit 0") {
		t.Errorf("events: %q", ev)
	}
}

// edit says which fields it changed; env from production to dev turns the
// warnings off, so it's spelled out.
func TestEditAndRenameAreLogged(t *testing.T) {
	cliSandbox(t)
	saveHosts(t, config.Host{Name: "pay1", Address: "192.0.2.50", Env: config.EnvProduction, Notes: "old"},
		config.Host{Name: "web1", Address: "192.0.2.51", ProxyJump: "pay1"})
	c := newEditCmd()
	c.SetArgs([]string{"pay1", "--env", "dev", "--address", "192.0.2.52", "--notes", "new"})
	_ = stdout(t, func() {
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	ev := events(t)
	if !has(ev, "edit pay1: ", "env production→dev", "address 192.0.2.50→192.0.2.52", "notes changed") {
		t.Errorf("events after edit: %q", ev)
	}
	r := newRenameCmd()
	r.SetArgs([]string{"pay1", "pay-one"})
	_ = stdout(t, func() {
		if err := r.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	if ev := events(t); !has(ev, "rename pay-one: name pay1→pay-one") {
		t.Errorf("events after rename: %q", ev)
	}
}

var _ = errors.New

// The edit prompts are logged the same way.
func TestEditWizardIsLogged(t *testing.T) {
	cliSandbox(t)
	saveHosts(t, config.Host{Name: "pay2", Address: "192.0.2.53", Env: config.EnvProduction})
	cfg, _ := config.Load(cfgPath)
	h, _ := cfg.Find("pay2")
	in := strings.NewReader("\n\n\n\n\n\n\n\n\n\ndev\n")
	_ = stdout(t, func() {
		if err := addWizard(cfg, h, in); err != nil {
			t.Fatal(err)
		}
	})
	if ev := events(t); !has(ev, "edit pay2: env production→dev") {
		t.Errorf("events: %q", ev)
	}
}

func asTerminal(t *testing.T, yes bool) {
	t.Helper()
	old := stdinTerminal
	stdinTerminal = func() bool { return yes }
	t.Cleanup(func() { stdinTerminal = old })
}

// exec --tag that sweeps in production hosts lists them and asks, y only; not
// on a terminal it goes ahead, warns, and the log says no one was asked.
func TestExecAsksBeforeProductionItSweepsIn(t *testing.T) {
	dir := cliSandbox(t)
	installFakeSSH(t, t.TempDir())
	t.Setenv(fakeSSHExitEnv, "0")
	sshLog := filepath.Join(dir, "ssh.log")
	t.Setenv("METERM_TEST_SSH_LOG", sshLog)
	saveHosts(t, config.Host{Name: "pay1", Address: "192.0.2.50", Env: config.EnvProduction, Tags: []string{"web"}},
		config.Host{Name: "web1", Address: "192.0.2.51", Tags: []string{"web"}})
	execute := func(stdin string, args ...string) (string, string) {
		var o, e string
		withStdin(t, stdin, func() {
			o = stdout(t, func() {
				e = stderrOf(t, func() {
					c := newExecCmd()
					c.SetArgs(args)
					_ = c.Execute()
				})
			})
		})
		return o, e
	}
	ran := func() string { b, _ := os.ReadFile(sshLog); os.Remove(sshLog); return string(b) }

	asTerminal(t, true)
	o, _ := execute("n\n", "--tag", "web", "--", "uptime")
	if !strings.Contains(o, "PRODUCTION") || !strings.Contains(o, "pay1") || !strings.Contains(o, "Cancelled") || ran() != "" {
		t.Errorf("answered n: %q", o)
	}
	o, _ = execute("yes\n", "--tag", "web", "--", "uptime")
	if !strings.Contains(o, "Cancelled") || ran() != "" {
		t.Errorf("answered yes (only y goes): %q", o)
	}
	_, _ = execute("y\n", "--tag", "web", "--", "uptime")
	if r := ran(); strings.Count(r, "\n") != 2 {
		t.Errorf("answered y, ssh ran %q", r)
	}
	if ev := events(t); !has(ev, "exec pay1: (production, asked) uptime => exit 0") || has(ev, "exec web1: (production") {
		t.Errorf("events: %q", ev)
	}
	o, _ = execute("", "web1", "pay1", "--", "uptime") // named: no question
	if strings.Contains(o, "Go on?") || strings.Count(ran(), "\n") != 2 {
		t.Errorf("named hosts asked: %q", o)
	}
	o, e := execute("", "--all", "--yes", "--", "uptime")
	if strings.Contains(o, "Go on?") || !strings.Contains(e, "PRODUCTION") || strings.Count(ran(), "\n") != 2 {
		t.Errorf("--yes: out %q err %q", o, e)
	}

	asTerminal(t, false)
	o, e = execute("", "--group", "", "--tag", "web", "--", "uptime")
	if strings.Contains(o, "Go on?") || !strings.Contains(e, "not asking") || !strings.Contains(e, "pay1") || strings.Count(ran(), "\n") != 2 {
		t.Errorf("not a terminal: out %q err %q", o, e)
	}
	if ev := events(t); !has(ev, "exec pay1: (production, no prompt: not a terminal) uptime") {
		t.Errorf("events: %q", ev)
	}
}

// cp --sync --delete on a production host asks the same way.
func TestSyncDeleteAsksOnProduction(t *testing.T) {
	dir := cliSandbox(t)
	installFake(t, t.TempDir(), "rsync")
	t.Setenv(fakeSSHExitEnv, "0")
	rsLog := filepath.Join(dir, "rsync.log")
	t.Setenv("METERM_TEST_SSH_LOG", rsLog)
	saveHosts(t, config.Host{Name: "pay1", Address: "192.0.2.50", Env: config.EnvProduction},
		config.Host{Name: "web1", Address: "192.0.2.51"})
	local := filepath.Join(dir, "src") + "/"
	_ = os.Mkdir(local, 0o700)
	ran := func() string { b, _ := os.ReadFile(rsLog); os.Remove(rsLog); return string(b) }
	asTerminal(t, true)
	var o string
	withStdin(t, "n\n", func() {
		o = stdout(t, func() { _ = runCopy(local, "pay1:/srv/", false, true, []string{"-a", "--delete"}, false) })
	})
	if !strings.Contains(o, "PRODUCTION") || !strings.Contains(o, "Cancelled") || ran() != "" {
		t.Errorf("answered n: %q", o)
	}
	withStdin(t, "", func() {
		o = stdout(t, func() { _ = runCopy(local, "web1:/srv/", false, true, []string{"-a", "--delete"}, false) })
	})
	if strings.Contains(o, "Go on?") || ran() == "" {
		t.Errorf("a dev host asked: %q", o)
	}
	withStdin(t, "", func() { o = stdout(t, func() { _ = runCopy(local, "pay1:/srv/", false, true, []string{"-a"}, false) }) })
	if strings.Contains(o, "Go on?") || ran() == "" {
		t.Errorf("no --delete asked: %q", o)
	}
	asTerminal(t, false)
	e := stderrOf(t, func() {
		_ = stdout(t, func() { _ = runCopy(local, "pay1:/srv/", false, true, []string{"-a", "--delete"}, false) })
	})
	if !strings.Contains(e, "not asking") || ran() == "" {
		t.Errorf("not a terminal: %q", e)
	}
	if ev := events(t); !has(ev, "sync pay1: (production, no prompt: not a terminal)", "--delete", "=> exit 0") {
		t.Errorf("events: %q", ev)
	}
}
