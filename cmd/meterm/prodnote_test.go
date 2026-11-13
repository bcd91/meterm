package main

import (
	"os"
	"strings"
	"testing"

	"meterm/internal/config"
	"meterm/internal/prodask"
)

// pickerToken sets this process up as a command the picker started after
// asking: a fresh token, and the path of its file.
func pickerToken(t *testing.T) string {
	t.Helper()
	t.Setenv(fromTUIEnv, "1")
	dir, err := os.MkdirTemp(t.TempDir(), "picker")
	if err != nil {
		t.Fatal(err)
	}
	env, err := prodask.Issue(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
	return strings.TrimPrefix(env[0], prodask.PathEnv+"=")
}

func noPicker(t *testing.T) {
	t.Helper()
	t.Setenv(fromTUIEnv, "")
	t.Setenv(prodask.PathEnv, "")
	t.Setenv(prodask.ValueEnv, "")
	old := pickerAnswer
	pickerAnswer = nil
	warnedPicker = false
	t.Cleanup(func() { pickerAnswer = old; warnedPicker = false })
}

func runExecCmd(t *testing.T, args ...string) (string, string) {
	t.Helper()
	var o, e string
	withStdin(t, "", func() {
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

// Every start on production says how it went ahead: a host named on the
// command line says so, and the picker's y is recorded wherever it was
// given, not only for hosts --tag swept in.
func TestEveryProductionStartCarriesANote(t *testing.T) {
	cliSandbox(t)
	installFakeSSH(t, t.TempDir())
	t.Setenv(fakeSSHExitEnv, "0")
	saveHosts(t,
		config.Host{Name: "pay1", Address: "192.0.2.50", Env: config.EnvProduction},
		config.Host{Name: "bastion", Address: "192.0.2.52", Env: config.EnvProduction},
		config.Host{Name: "inner", Address: "192.0.2.53", ProxyJump: "bastion"},
		config.Host{Name: "web1", Address: "192.0.2.51"})
	asTerminal(t, false)

	noPicker(t)
	runExecCmd(t, "pay1", "web1", "inner", "--", "uptime")
	ev := events(t)
	for _, want := range []string{
		"exec pay1: (production, named on the command line) uptime",
		"exec inner: (via production, named on the command line) uptime",
	} {
		if !has(ev, want) {
			t.Errorf("missing %q in %q", want, ev)
		}
	}
	if has(ev, "exec web1: (") {
		t.Errorf("a dev host got a note: %q", ev)
	}

	// From the picker, after its y: the token is used up and the log says so.
	tok := pickerToken(t)
	pickerAnswer = nil
	takePickerAnswer()
	if _, err := os.Stat(tok); !os.IsNotExist(err) {
		t.Errorf("the token is still there after the command took it: %v", err)
	}
	if os.Getenv(prodask.PathEnv) != "" {
		t.Error("the token's path is still in the environment, for children to inherit")
	}
	runExecCmd(t, "pay1", "--", "hostname")
	if ev := events(t); !has(ev, "exec pay1: (production, picker asked) hostname") {
		t.Errorf("picker's y for a named host not recorded: %q", ev)
	}
	if _, err := connect([]string{"pay1"}); err != nil {
		t.Fatal(err)
	}
	if ev := events(t); !has(ev, "connect pay1: (production, picker asked)") {
		t.Errorf("picker's y for connect not recorded: %q", ev)
	}

	// The picker started it but didn't ask (no token): not "named".
	noPicker(t)
	t.Setenv(fromTUIEnv, "1")
	takePickerAnswer()
	var e string
	e = stderrOf(t, func() { _, _ = connect([]string{"inner"}) })
	if ev := events(t); !has(ev, "connect inner: (via production, picker didn't ask)") {
		t.Errorf("picker without a token: %q", ev)
	}
	if !strings.Contains(e, "picker token missing") {
		t.Errorf("stderr doesn't say why: %q", e)
	}

	// Plain CLI connect names the host.
	noPicker(t)
	takePickerAnswer()
	_, _ = connect([]string{"pay1"})
	if ev := events(t); !has(ev, "connect pay1: (production, named on the command line)") {
		t.Errorf("CLI connect: %q", ev)
	}
}

func TestShortProdNoteReadsEveryNote(t *testing.T) {
	for in, want := range map[string]string{
		"(production, named on the command line) uptime": "⚑named uptime",
		"(via production, picker asked) uptime":          "⚑via picker uptime",
		"(tunnel to production, picker didn't ask) db":   "⚑tunnel not asked db",
		"(production, no prompt: not a terminal) x":      "⚑no prompt x",
	} {
		if got := shortProdNote(in); got != want {
			t.Errorf("shortProdNote(%q) = %q, want %q", in, got, want)
		}
	}
}
