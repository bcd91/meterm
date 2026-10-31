package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// serviceEnv gives a command a temp config dir and a host file, and records what
// would have gone to systemctl. Nothing reaches the real user manager.
func serviceEnv(t *testing.T) (calls *[]string, unitDir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix only")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir) // where the OS's config dir is, on macOS
	t.Setenv("XDG_CONFIG_HOME", dir)
	oldOS := serviceGOOS
	serviceGOOS = "linux"
	t.Cleanup(func() { serviceGOOS = oldOS })
	t.Setenv("METERM_CONFIG", filepath.Join(dir, "hosts.yaml"))
	t.Setenv("METERM_ACTIVITY_DIR", filepath.Join(dir, "activity"))
	t.Setenv("METERM_NO_AUTOSYNC", "1")
	host := "version: 1\nhosts:\n- name: box\n  address: 192.0.2.1\n  forwards:\n" +
		"  - {name: web, type: L, local_port: 8080, remote_host: localhost, remote_port: 80}\n"
	if err := os.WriteFile(filepath.Join(dir, "hosts.yaml"), []byte(host), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	old := systemctlUser
	systemctlUser = func(args ...string) error {
		got = append(got, strings.Join(args, " "))
		return nil
	}
	t.Cleanup(func() { systemctlUser = old })
	unitDir, err := userUnitDir()
	if err != nil {
		t.Fatal(err)
	}
	return &got, unitDir
}

func TestTunnelEnableAndDisable(t *testing.T) {
	calls, unitDir := serviceEnv(t)
	unit := serviceUnitName("box/web")
	en := newTunnelEnableCmd()
	en.SetArgs([]string{"box/web"})
	if err := en.Execute(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(unitDir, unit))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"tunnel" "up" "--keep" "--service" "box/web"`, "--config", "Restart=on-failure", "WantedBy=default.target"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("no %q in:\n%s", want, b)
		}
	}
	if got := strings.Join(*calls, "; "); got != "daemon-reload; enable --now "+unit {
		t.Errorf("systemctl: %s", got)
	}

	*calls = nil
	dis := newTunnelDisableCmd()
	dis.SetArgs([]string{"box/web"})
	if err := dis.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(unitDir, unit)); !os.IsNotExist(err) {
		t.Errorf("unit still there: %v", err)
	}
	if got := strings.Join(*calls, "; "); got != "disable --now "+unit+"; daemon-reload" {
		t.Errorf("systemctl: %s", got)
	}
}

func TestTunnelEnableRefusals(t *testing.T) {
	calls, unitDir := serviceEnv(t)
	for _, arg := range []string{"box", "box/", "/web", "box/nope", "other/web"} {
		c := newTunnelEnableCmd()
		c.SetArgs([]string{arg})
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err == nil {
			t.Errorf("%q was accepted", arg)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("systemctl was called: %v", *calls)
	}
	if _, err := os.Stat(unitDir); !os.IsNotExist(err) {
		t.Errorf("a unit dir appeared: %v", err)
	}
}

func TestTunnelDisableWhatIsntThere(t *testing.T) {
	calls, _ := serviceEnv(t)
	c := newTunnelDisableCmd()
	c.SetArgs([]string{"gone/web"})
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "isn't enabled") {
		t.Errorf("err = %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("systemctl was called: %v", *calls)
	}
}

// A failure from systemctl is reported, with the unit left for another try.
func TestTunnelEnableReportsSystemctl(t *testing.T) {
	_, unitDir := serviceEnv(t)
	systemctlUser = func(args ...string) error {
		if args[0] == "enable" {
			return errors.New("no user manager")
		}
		return nil
	}
	c := newTunnelEnableCmd()
	c.SetArgs([]string{"box/web"})
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "no user manager") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(unitDir, serviceUnitName("box/web"))); err != nil {
		t.Errorf("unit: %v", err)
	}
}

func TestServiceUnitNameKeepsApart(t *testing.T) {
	a, b := serviceUnitName("a-b/c"), serviceUnitName("a/b-c")
	if a == b {
		t.Errorf("both %s", a)
	}
	if got := serviceUnitName("we b/ü$x"); strings.ContainsAny(got, " $/ü") || !strings.HasSuffix(got, ".service") {
		t.Errorf("name = %s", got)
	}
}

func TestSystemdQuote(t *testing.T) {
	for in, want := range map[string]string{
		"box/web":     `"box/web"`,
		`a b`:         `"a b"`,
		`say "hi"`:    `"say \"hi\""`,
		`back\slash`:  `"back\\slash"`,
		`$HOME %h`:    `"$$HOME %%h"`,
		"line\nbreak": `"line\nbreak"`,
	} {
		if got := systemdQuote(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}

// Nothing in a name can start another setting in the unit.
func TestRenderUnitStaysOneSettingPerLine(t *testing.T) {
	u := renderTunnelUnit("/opt/my tools/meterm", "/home/a b/hosts.yaml", "bo\nExecStartPost=/bin/evil/web%h")
	for _, l := range strings.Split(u, "\n") {
		if strings.HasPrefix(l, "ExecStartPost") {
			t.Errorf("injected: %q", l)
		}
	}
	for _, want := range []string{`"/opt/my tools/meterm"`, `"--config" "/home/a b/hosts.yaml"`, "%%h"} {
		if !strings.Contains(u, want) {
			t.Errorf("no %q in:\n%s", want, u)
		}
	}
}

// The same list for systemd, whose file format has its own ways to be escaped from.
func TestRenderUnitHostile(t *testing.T) {
	for _, spec := range hostileNames {
		u := renderTunnelUnit("/m", "", spec)
		for _, l := range strings.Split(u, "\n") {
			if strings.HasPrefix(l, "ExecStartPost") || strings.HasPrefix(l, "ExecStartPre") {
				t.Errorf("%q: %q", spec, l)
			}
		}
		if got := serviceUnitName(spec); strings.ContainsAny(got, "/ $`;\"'<>&%") {
			t.Errorf("%q: %q", spec, got)
		}
	}
}
