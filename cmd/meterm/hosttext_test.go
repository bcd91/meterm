package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/config"
)

// cliSandbox points every file meterm reads or writes at a temp dir.
func cliSandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("APPDATA", filepath.Join(dir, "config")) // Windows's config dir
	t.Setenv("LOCALAPPDATA", filepath.Join(dir, "cache"))
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("METERM_HISTORY", filepath.Join(dir, "history.json"))
	t.Setenv("METERM_ACTIVITY_DIR", filepath.Join(dir, "activity"))
	t.Setenv("METERM_NO_AUTOSYNC", "1")
	t.Setenv("METERM_CONFIG", "")
	cfgPath = filepath.Join(dir, "hosts.yaml")
	t.Cleanup(func() { cfgPath = "" })
	return dir
}

func hostFile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(cfgPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func TestAddEditRenameRefuseControlCharacters(t *testing.T) {
	cliSandbox(t)
	run := func(c interface {
		SetArgs([]string)
		SetOut(io.Writer)
		SetErr(io.Writer)
		Execute() error
	}, args ...string) error {
		c.SetArgs(args)
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		return c.Execute()
	}
	if err := run(newAddCmd(), "web1", "--address", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	before := hostFile(t)
	for _, c := range []struct {
		field string
		err   error
	}{
		{"name", run(newAddCmd(), "x\x1b]0;own\x07y", "--address", "192.0.2.2")},
		{"group", run(newAddCmd(), "web2", "--address", "192.0.2.2", "--group", "g\x1b[5m")},
		{"tag", run(newAddCmd(), "web2", "--address", "192.0.2.2", "--tags", "a,b\u202ec")},
		{"notes", run(newEditCmd(), "web1", "--notes", "n\x1b]8;;http://x\x07")},
		{"group", run(newEditCmd(), "web1", "--group", "g\x1b[5m")},
		{"address", run(newEditCmd(), "web1", "--address", "192.0.2.9\x1b[2J")},
		{"new name", run(newRenameCmd(), "web1", "we\x1b[5mb")},
	} {
		if c.err == nil || !strings.Contains(c.err.Error(), c.field) {
			t.Errorf("%s: err = %v, want one naming the field", c.field, c.err)
		}
	}
	if after := hostFile(t); after != before {
		t.Errorf("host file changed:\n%s", after)
	}

	// The wizard: name, address, user, users, port, identity, group, tags, jump, notes, env.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader("w3\n192.0.2.3\n\n\n\n\ng\x1b[5m\n\n\n\n\n")
	if err := addWizard(cfg, nil, in); err == nil || !strings.Contains(err.Error(), "group") {
		t.Errorf("wizard: err = %v", err)
	}
	if after := hostFile(t); after != before {
		t.Errorf("wizard changed the host file:\n%s", after)
	}
}

func TestImportSkipsHostsWithControlCharacters(t *testing.T) {
	cliSandbox(t)
	err := importHosts([]config.Host{
		{Name: "good", Address: "192.0.2.10"},
		{Name: "bad\x1b]0;own\x07", Address: "192.0.2.11"},
		{Name: "bad2", Address: "192.0.2.12", Tags: []string{"t\x1b[5m"}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(cfgPath)
	if len(cfg.Hosts) != 1 || cfg.Hosts[0].Name != "good" {
		t.Errorf("imported %+v, want only good", cfg.Hosts)
	}
}

func TestListTextShowsHostTextSafely(t *testing.T) {
	var b bytes.Buffer
	rows := []listRow{{Name: "evil\x1b]0;pwned\x07x", Target: "r\x1b[5m@192.0.2.1", Group: "g\x1b[5m",
		Team: "t\u202eeam", Tags: []string{"a\tb", "c\x1b[2J"}, Jump: "j\x1b[1;1H", Env: "production", lastHuman: "never"}}
	if err := writeListText(&b, rows, false); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.ContainsAny(out, "\x1b\x07\u202e") {
		t.Errorf("list printed control characters: %q", out)
	}
	if !strings.Contains(out, `"evil\x1b]0;pwned\ax"`) || strings.Count(out, "\n") != 2 {
		t.Errorf("list output: %q", out)
	}
}

// stdout runs fn and returns what it printed.
func stdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestImportAnsibleMessagesAreSafe(t *testing.T) {
	dir := cliSandbox(t)
	inv := filepath.Join(dir, "inv.ini")
	if err := os.WriteFile(inv, []byte("[web]\nweb9 ansible_host=192.0.2.99\nbad\x1b]0;own\x07 ansible_host=192.0.2.98\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := stdout(t, func() {
		c := newImportAnsibleCmd()
		c.SetArgs([]string{inv, "--yes"})
		if err := c.Execute(); err != nil {
			t.Error(err)
		}
	})
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("import printed control characters: %q", out)
	}
}
