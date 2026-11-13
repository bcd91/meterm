package prodask

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setEnv puts what Issue returned into this process's environment, the way
// a child started with it would see it.
func setEnv(t *testing.T, env []string) {
	t.Helper()
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		t.Setenv(k, v)
	}
}

// privDir is a directory only this user can write to, as os.MkdirTemp makes.
func privDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp(t.TempDir(), "p")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func clearEnv(t *testing.T) {
	t.Helper()
	t.Setenv(PathEnv, "")
	t.Setenv(ValueEnv, "")
}

func TestIssueWritesAPrivateFile(t *testing.T) {
	clearEnv(t)
	dir := privDir(t)
	env, err := Issue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 2 || !strings.HasPrefix(env[0], PathEnv+"=") || !strings.HasPrefix(env[1], ValueEnv+"=") {
		t.Fatalf("env %q", env)
	}
	path := strings.TrimPrefix(env[0], PathEnv+"=")
	value := strings.TrimPrefix(env[1], ValueEnv+"=")
	if filepath.Dir(path) != dir {
		t.Errorf("token %s isn't in %s", path, dir)
	}
	if len(value) != 32 {
		t.Errorf("value %q isn't 128 bits of hex", value)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != value {
		t.Errorf("file holds %q (%v), want %q", b, err, value)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	env2, _ := Issue(dir)
	if env2[1] == env[1] || env2[0] == env[0] {
		t.Errorf("two tokens alike: %q %q", env, env2)
	}
}

// The token answers once: the file goes, and the same env doesn't answer again.
func TestConsumeOnce(t *testing.T) {
	clearEnv(t)
	env, err := Issue(privDir(t))
	if err != nil {
		t.Fatal(err)
	}
	setEnv(t, env)
	path := strings.TrimPrefix(env[0], PathEnv+"=")
	if asked, note := Consume(); !asked || note != "picker asked" {
		t.Fatalf("first use: %v %q", asked, note)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("token still there after use: %v", err)
	}
	// What a grandchild would inherit is gone too.
	if os.Getenv(PathEnv) != "" || os.Getenv(ValueEnv) != "" {
		t.Errorf("env still set: %q %q", os.Getenv(PathEnv), os.Getenv(ValueEnv))
	}
	setEnv(t, env)
	if asked, note := Consume(); asked || !strings.Contains(note, "missing") {
		t.Errorf("second use: %v %q", asked, note)
	}
}

func TestConsumeRefuses(t *testing.T) {
	cases := map[string]func(t *testing.T, path, value string) []string{
		"no env": func(t *testing.T, path, value string) []string { return nil },
		"value only": func(t *testing.T, path, value string) []string {
			return []string{ValueEnv + "=" + value}
		},
		"wrong value": func(t *testing.T, path, value string) []string {
			return []string{PathEnv + "=" + path, ValueEnv + "=" + strings.Repeat("0", 32)}
		},
		"empty value": func(t *testing.T, path, value string) []string {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return []string{PathEnv + "=" + path, ValueEnv + "="}
		},
		"value with a newline": func(t *testing.T, path, value string) []string {
			if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return []string{PathEnv + "=" + path, ValueEnv + "=" + value}
		},
		"a directory": func(t *testing.T, path, value string) []string {
			d := filepath.Join(filepath.Dir(path), "d")
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			return []string{PathEnv + "=" + d, ValueEnv + "=" + value}
		},
		"a relative path": func(t *testing.T, path, value string) []string {
			t.Chdir(filepath.Dir(path)) // so it names the real file
			return []string{PathEnv + "=" + filepath.Base(path), ValueEnv + "=" + value}
		},
	}
	if runtime.GOOS != "windows" {
		cases["mode 0644"] = func(t *testing.T, path, value string) []string {
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
			return []string{PathEnv + "=" + path, ValueEnv + "=" + value}
		}
		cases["mode 0400"] = func(t *testing.T, path, value string) []string {
			if err := os.Chmod(path, 0o400); err != nil {
				t.Fatal(err)
			}
			return []string{PathEnv + "=" + path, ValueEnv + "=" + value}
		}
		cases["a symlink"] = func(t *testing.T, path, value string) []string {
			l := path + ".link"
			if err := os.Symlink(path, l); err != nil {
				t.Fatal(err)
			}
			return []string{PathEnv + "=" + l, ValueEnv + "=" + value}
		}
		cases["someone else's"] = func(t *testing.T, path, value string) []string {
			old := ownedByUs
			ownedByUs = func(os.FileInfo) bool { return false }
			t.Cleanup(func() { ownedByUs = old })
			return []string{PathEnv + "=" + path, ValueEnv + "=" + value}
		}
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			env, err := Issue(privDir(t))
			if err != nil {
				t.Fatal(err)
			}
			path := strings.TrimPrefix(env[0], PathEnv+"=")
			value := strings.TrimPrefix(env[1], ValueEnv+"=")
			setEnv(t, setup(t, path, value))
			asked, note := Consume()
			if asked || !strings.HasPrefix(note, "picker token ") {
				t.Errorf("asked %v, note %q", asked, note)
			}
		})
	}
}

// Issue refuses a directory others can write to: someone could swap the file.
func TestIssueNeedsAPrivateDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Issue(dir); err == nil {
		t.Error("Issue in a world-writable dir")
	}
}

// A token file swapped for another between its checks and its reading (a
// racing process replacing it with one holding the same value) doesn't
// count: what's read must be the file that was checked.
func TestConsumeRefusesAFileSwappedWhileRead(t *testing.T) {
	clearEnv(t)
	dir := privDir(t)
	env, err := Issue(dir)
	if err != nil {
		t.Fatal(err)
	}
	setEnv(t, env)
	value := strings.TrimPrefix(env[1], ValueEnv+"=")
	old := betweenStatAndOpen
	betweenStatAndOpen = func(p string) {
		other := filepath.Join(dir, "other")
		if err := os.WriteFile(other, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(other, p); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { betweenStatAndOpen = old })
	if asked, note := Consume(); asked || !strings.Contains(note, "changed while being read") {
		t.Errorf("swapped file: %v %q", asked, note)
	}
}
