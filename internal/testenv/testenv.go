// Package testenv keeps a package's tests away from the machine they run on: no real home, no
// real config or log, no git identity, and no agent variables from whatever started them.
package testenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Main runs m with a temporary home, for TestMain.
func Main(m *testing.M) {
	done := Setup()
	code := m.Run()
	done()
	os.Exit(code)
}

// Setup is Main for a TestMain with more to do. It returns what to call at the end.
func Setup() func() {
	dir, err := os.MkdirTemp("", "meterm-test-home-")
	if err != nil {
		panic(err)
	}
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "METERM_") && !isChild(name) {
			os.Unsetenv(name)
		}
	}
	for _, name := range []string{"CLAUDECODE", "AI_AGENT", "CLAUDE_CODE_SESSION_ID", "SSH_AUTH_SOCK", "XDG_RUNTIME_DIR", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		os.Unsetenv(name)
	}
	for _, name := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "APPDATA", "LOCALAPPDATA"} {
		os.Setenv(name, filepath.Join(dir, strings.ToLower(name)))
		_ = os.MkdirAll(os.Getenv(name), 0o700)
	}
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return func() { _ = os.RemoveAll(dir) }
}

// isChild is a variable that says this process is a test's own helper, which has to stay.
func isChild(name string) bool { return strings.HasPrefix(name, "METERM_TEST_") }
