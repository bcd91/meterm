package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// When this variable is set, the test binary is one writer process: it adds
// one host through Update and exits. See TestMain.
const writerEnv = "METERM_TEST_WRITER"

func TestMain(m *testing.M) {
	if spec := os.Getenv(writerEnv); spec != "" {
		path, name, _ := strings.Cut(spec, "|")
		if err := addHost(path, name); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func addHost(path, name string) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	return cfg.Update(func(c *Config) error {
		c.Upsert(Host{Name: name, Address: "192.0.2.1"})
		return nil
	})
}

// addInProcesses runs one real meterm-like process per name, all at once.
func addInProcesses(t *testing.T, path string, names ...string) {
	t.Helper()
	cmds := make([]*exec.Cmd, len(names))
	stderr := make([]bytes.Buffer, len(names))
	for i, name := range names {
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), writerEnv+"="+path+"|"+name)
		c.Stderr = &stderr[i]
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = c
	}
	for i, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Errorf("writer %s: %v: %s", names[i], err, stderr[i].String())
		}
	}
}

func hostNames(t *testing.T, path string) map[string]bool {
	t.Helper()
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, h := range cfg.Hosts {
		out[h.Name] = true
	}
	return out
}

func TestTwoProcessesNeverLoseAHost(t *testing.T) {
	dir := t.TempDir()
	for round := range 40 {
		path := filepath.Join(dir, fmt.Sprintf("hosts%d.yaml", round))
		addInProcesses(t, path, "a", "b")
		if got := hostNames(t, path); !got["a"] || !got["b"] {
			t.Fatalf("round %d: saved %v, want a and b", round, got)
		}
	}
}

func TestThirtyProcessesAllSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	var names []string
	for i := range 30 {
		names = append(names, fmt.Sprintf("n%d", i))
	}
	addInProcesses(t, path, names...)
	if got := hostNames(t, path); len(got) != 30 {
		t.Fatalf("saved %d of 30 hosts: %v", len(got), got)
	}
	assertNoLeftovers(t, path)
}

func TestConcurrentUpdatesInOneProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	var wg sync.WaitGroup
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := addHost(path, fmt.Sprintf("n%d", i)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := hostNames(t, path); len(got) != 30 {
		t.Fatalf("saved %d of 30 hosts", len(got))
	}
}

func assertNoLeftovers(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		// The lock file stays on purpose; see package lockfile.
		if e.Name() != filepath.Base(path) && e.Name() != filepath.Base(path)+".lock" {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}

func TestUpdateWorksFromTheFileNotTheStaleCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	stale, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := addHost(path, "theirs"); err != nil {
		t.Fatal(err)
	}
	if err := stale.Update(func(c *Config) error {
		if _, i := c.Find("theirs"); i < 0 {
			t.Error("fn was given a config without the other writer's host")
		}
		c.Upsert(Host{Name: "mine", Address: "192.0.2.2"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := hostNames(t, path); !got["theirs"] || !got["mine"] {
		t.Fatalf("saved %v", got)
	}
	if _, i := stale.Find("theirs"); i < 0 {
		t.Error("the caller's config wasn't refreshed")
	}
}

func TestUpdateKeepsThePicker(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "hosts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	called := false
	cfg.SetRoutePicker(func(Host) (Route, bool) { called = true; return Route{}, false })
	if err := cfg.Update(func(c *Config) error {
		c.Upsert(Host{Name: "h", Address: "192.0.2.1", Routes: []Route{{Name: "r"}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h, _ := cfg.Find("h")
	cfg.Resolve(*h)
	if !called {
		t.Error("Update dropped the route picker")
	}
}

func TestUpdateErrorWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := addHost(path, "keep"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	err = cfg.Update(func(c *Config) error {
		c.Remove("keep")
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("a failed update changed the file")
	}
	if _, i := cfg.Find("keep"); i < 0 {
		t.Error("a failed update changed the caller's config")
	}
	assertNoLeftovers(t, path)
}

func TestUpdateRefusesAnUnreadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := os.WriteFile(path, []byte("hosts: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{path: path}
	if err := cfg.Update(func(*Config) error { return nil }); err == nil {
		t.Fatal("no error for a config that doesn't parse")
	}
	if b, _ := os.ReadFile(path); string(b) != "hosts: [unclosed\n" {
		t.Errorf("file overwritten: %q", b)
	}
}

func TestSaveRefusesWhenTheFileChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := addHost(path, "first"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Upsert(Host{Name: "mine", Address: "192.0.2.2"})
	if err := addHost(path, "theirs"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); !errors.Is(err, ErrChanged) {
		t.Fatalf("Save over someone's edit: %v, want ErrChanged", err)
	}
	if got := hostNames(t, path); !got["theirs"] || got["mine"] {
		t.Fatalf("file should be untouched, has %v", got)
	}
}

func TestSaveRefusesWhenTheFileAppeared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := addHost(path, "theirs"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); !errors.Is(err, ErrChanged) {
		t.Fatalf("got %v, want ErrChanged", err)
	}
}

func TestSaveTwiceAndAfterUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Upsert(Host{Name: "a", Address: "192.0.2.1"})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	cfg.Upsert(Host{Name: "b", Address: "192.0.2.2"})
	if err := cfg.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	if err := cfg.Update(func(c *Config) error {
		c.Upsert(Host{Name: "c", Address: "192.0.2.3"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg.Upsert(Host{Name: "d", Address: "192.0.2.4"})
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save after Update: %v", err)
	}
	if got := hostNames(t, path); len(got) != 4 {
		t.Fatalf("saved %v", got)
	}
}

func TestSavedFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.yaml")
	if err := addHost(path, "a"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.PathSeparator == '/' && st.Mode().Perm() != 0o600 {
		t.Errorf("mode %o, want 600", st.Mode().Perm())
	}
}

func TestUpdateWithoutAPath(t *testing.T) {
	if err := (&Config{}).Update(func(*Config) error { return nil }); err == nil {
		t.Fatal("no error for a config with no path")
	}
}
