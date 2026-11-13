package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/config"
)

// oscName is a host name with an OSC 52 (set the clipboard) in it, the way
// a hand-edited or old host file can hold one; it shows as "evilname".
const oscName = "evil\x1b]52;c;SGVsbG8=\x07name"

func dirtyHosts(t *testing.T, hosts ...config.Host) {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Hosts = append(cfg.Hosts, hosts...)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

func runCmd(c interface {
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

// A host whose name holds control characters can be fixed from meterm: the
// name as shown (cleaned) finds it, for rename, rm, connect and exec.
func TestCleanedNameFindsTheHost(t *testing.T) {
	dir := cliSandbox(t)
	installFakeSSH(t, t.TempDir())
	t.Setenv("METERM_TEST_SSH_LOG", filepath.Join(dir, "ssh.log"))
	t.Setenv(fakeSSHExitEnv, "0")
	dirtyHosts(t, config.Host{Name: oscName, Address: "192.0.2.70"})

	if _, err := connect([]string{"evilname"}); err != nil {
		t.Errorf("connect evilname: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ssh.log")); !strings.Contains(string(b), "192.0.2.70") {
		t.Errorf("connect didn't reach the host: %q", b)
	}
	_ = stdout(t, func() {
		if err := runCmd(newRenameCmd(), "evilname", "fixed"); err != nil {
			t.Errorf("rename evilname: %v", err)
		}
	})
	cfg, _ := config.Load(cfgPath)
	if _, i := cfg.Find("fixed"); i < 0 || len(cfg.Hosts) != 1 {
		t.Errorf("after rename: %+v", cfg.Hosts)
	}
}

// Two hosts that clean to the same name are an error, not a guess.
func TestCleanedNameMustBeUnambiguous(t *testing.T) {
	cliSandbox(t)
	dirtyHosts(t, config.Host{Name: oscName, Address: "192.0.2.71"},
		config.Host{Name: "evil\x1b[5mname", Address: "192.0.2.72"})
	err := runCmd(newRmCmd(), "evilname", "--yes")
	if err == nil || !strings.Contains(err.Error(), "2 hosts") {
		t.Errorf("rm of an ambiguous cleaned name: %v", err)
	}
	cfg, _ := config.Load(cfgPath)
	if len(cfg.Hosts) != 2 {
		t.Errorf("a host went: %+v", cfg.Hosts)
	}
	// The exact name still works.
	_ = stderrOf(t, func() {
		if err := runCmd(newRmCmd(), oscName, "--yes"); err != nil {
			t.Errorf("rm by the exact name: %v", err)
		}
	})
}

// edit on such a host: Enter at every prompt keeps the cleaned values and
// says so, so the edit saves and the host comes out clean, tabs in the
// notes included.
func TestEditCleansADirtyHost(t *testing.T) {
	cliSandbox(t)
	dirtyHosts(t, config.Host{Name: oscName, Address: "192.0.2.73", Group: "g\x1b[5m",
		Tags: []string{"a\u202eb"}, Notes: "line one\tcol\nline two"},
		config.Host{Name: "inner", Address: "192.0.2.74", ProxyJump: oscName})
	cfg, _ := config.Load(cfgPath)
	h, _, err := cfg.Lookup("evilname")
	if err != nil {
		t.Fatal(err)
	}
	var werr error
	out := stdout(t, func() { werr = addWizard(cfg, h, strings.NewReader(strings.Repeat("\n", 11))) })
	if werr != nil {
		t.Fatalf("edit: %v\n%s", werr, out)
	}
	if !strings.Contains(out, "cleaned") {
		t.Errorf("the wizard didn't say it cleaned anything:\n%s", out)
	}
	cfg, _ = config.Load(cfgPath)
	got, i := cfg.Find("evilname")
	if i < 0 || len(cfg.Hosts) != 2 {
		t.Fatalf("after edit: %+v", cfg.Hosts)
	}
	if got.Group != "g" || got.Tags[0] != "ab" || got.Notes != "line one col line two" || got.CheckPrintable() != nil {
		t.Errorf("not clean: %+v", *got)
	}
	in, _ := cfg.Find("inner")
	if in.ProxyJump != "evilname" {
		t.Errorf("the jump that named it: %q", in.ProxyJump)
	}
}
