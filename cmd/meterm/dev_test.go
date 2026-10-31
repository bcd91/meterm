package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"meterm/internal/sshrun"
)

func TestSplitDevTarget(t *testing.T) {
	saved := map[string]bool{"devbox": true, "odd:name": true}
	isHost := func(s string) bool { return saved[s] }
	for in, want := range map[string][2]string{
		"devbox":             {"devbox", ""},
		"devbox:~/src/api":   {"devbox", "~/src/api"},
		"deploy@devbox:/srv": {"deploy@devbox", "/srv"},
		"odd:name":           {"odd:name", ""},
		"devbox:/a:b":        {"devbox", "/a:b"},
	} {
		h, d := splitDevTarget(in, isHost)
		if h != want[0] || d != want[1] {
			t.Errorf("%q → %q %q, want %v", in, h, d, want)
		}
	}
}

func TestDevSessionName(t *testing.T) {
	for dir, want := range map[string]string{
		"":            "dev",
		"~":           "dev",
		"/":           "dev",
		".":           "dev",
		"~/src/api":   "api",
		"~/src/api/":  "api",
		"/srv/my.app": "my_app",
		"/srv/a b;rm": "a_b_rm",
		"/srv/-x":     "x",
		"/srv/...":    "dev",
	} {
		got, err := devSessionName("", dir)
		if err != nil || got != want {
			t.Errorf("dir %q → %q, %v; want %q", dir, got, err, want)
		}
	}
	if got, err := devSessionName("work-1", "~/src/api"); err != nil || got != "work-1" {
		t.Errorf("explicit name → %q, %v", got, err)
	}
	for _, bad := range []string{"a.b", "-x", "a b", "$(reboot)", "a:b"} {
		if _, err := devSessionName(bad, ""); err == nil {
			t.Errorf("--session %q accepted", bad)
		}
	}
}

func TestDevRemoteScript(t *testing.T) {
	s, err := devRemoteScript("~/src/it's", "api", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`cd -- "$HOME"/'src/it'\''s' || exit 3;`, "tmux new-session -A -s 'api'", `exec "${SHELL:-/bin/sh}" -l`} {
		if !strings.Contains(s, want) {
			t.Errorf("script %q missing %q", s, want)
		}
	}
	s, _ = devRemoteScript("/srv/$(reboot)", "x", false)
	if !strings.Contains(s, `cd -- '/srv/$(reboot)'`) {
		t.Errorf("directory not quoted: %q", s)
	}
	s, _ = devRemoteScript("", "dev", true)
	if strings.Contains(s, "cd --") {
		t.Errorf("cd with no directory: %q", s)
	}
	if _, err := devRemoteScript("/a\nb", "x", true); err == nil {
		t.Error("newline in directory accepted")
	}
}

func TestDevRemoteScriptKeepsDirectoriesApart(t *testing.T) {
	a, _ := devRemoteScript("~/client-a/api", "api", true)
	b, _ := devRemoteScript("~/client-b/api", "api", true)
	again, _ := devRemoteScript("~/client-a/api", "api", true)
	if a == strings.ReplaceAll(b, "client-b", "client-a") {
		t.Error("two directories share a fallback name")
	}
	if a != again {
		t.Error("the same directory got a different script")
	}
	for _, want := range []string{"tmux has-session -t '=api-", "-t '=api:' '#{session_path}'", `new-session -A -s "$s"`} {
		if !strings.Contains(a, want) {
			t.Errorf("script %q missing %q", a, want)
		}
	}
	// A name asked for by hand means exactly that session.
	s, _ := devRemoteScript("~/client-a/api", "work", false)
	if strings.Contains(s, "session_path") || !strings.Contains(s, "new-session -A -s 'work'") {
		t.Errorf("--session changed: %q", s)
	}
}

func TestDevShouldRetry(t *testing.T) {
	for _, c := range []struct {
		code               int
		connected, enabled bool
		failure            sshrun.Failure
		want               bool
	}{
		{255, true, true, sshrun.FailureNone, true},
		{255, false, true, sshrun.FailureNone, false}, // never got in: refused or unreachable, say so
		{255, true, false, sshrun.FailureNone, false}, // --no-reconnect
		{1, true, true, sshrun.FailureNone, false},    // the remote command's own exit
		{devDirMissing, true, true, sshrun.FailureNone, false},
		{255, true, true, sshrun.FailureAuth, false},    // the key was revoked
		{255, true, true, sshrun.FailureHostKey, false}, // the host key changed
		{255, true, true, sshrun.FailureConfig, false},
	} {
		if got := devShouldRetry(c.code, c.connected, c.enabled, c.failure); got != c.want {
			t.Errorf("%+v → %v", c, got)
		}
	}
}

func TestConnectMarker(t *testing.T) {
	m := newConnectMarker()
	if m == nil {
		t.Skip("no marker on this OS")
	}
	defer m.close()
	if m.seen(time.Hour) {
		t.Error("seen before ssh left a mark, however long it ran")
	}
	if err := os.WriteFile(m.path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !m.seen(0) {
		t.Error("not seen after a mark, however briefly it ran")
	}
	m.reset()
	if m.seen(0) {
		t.Error("reset kept the mark")
	}
	m.path = "/tmp/100%/it's"
	got := strings.Join(m.options(), " ")
	if !strings.Contains(got, "PermitLocalCommand=yes") || !strings.Contains(got, `/tmp/100%%/it'\''s`) {
		t.Errorf("options = %q", got)
	}
}

func TestNoMarkerFallsBackToTime(t *testing.T) {
	var m *connectMarker // what Windows gets
	if m.options() != nil {
		t.Error("options without a marker")
	}
	m.reset()
	m.close()
	if m.seen(devSettle - time.Second) {
		t.Error("counted as connected before devSettle")
	}
	if !m.seen(devSettle) {
		t.Error("not counted as connected at devSettle")
	}
	if devSettle <= 10*time.Second {
		t.Error("devSettle must be past ssh's ConnectTimeout")
	}
}

func TestDevCommandScripts(t *testing.T) {
	first, again, err := devCommandScripts("~/src/api", "api", false, []string{"echo", "$(reboot)", "it's"})
	if err != nil {
		t.Fatal(err)
	}
	line := `'echo' '$(reboot)' 'it'\''s'`
	if !strings.Contains(first, "exec "+line) {
		t.Errorf("no plain run of the command: %s", first)
	}
	if !strings.Contains(first, `tmux new-window -t "=$s:" -c "$PWD" `+shQuote(line)) {
		t.Errorf("no new window for it: %s", first)
	}
	if strings.Contains(again, "reboot") || strings.Contains(again, "new-window") {
		t.Errorf("coming back starts it again: %s", again)
	}
	if !strings.Contains(again, "exit 1") || strings.Contains(again, "SHELL") {
		t.Errorf("no tmux should stop, not open a shell: %s", again)
	}
	first, _, _ = devCommandScripts("", "dev", true, []string{"x"})
	if !strings.Contains(first, "-c '#{session_path}' ") || !strings.Contains(first, "s='dev'") {
		t.Errorf("no directory: %s", first)
	}
	if _, _, err := devCommandScripts("/a\nb", "x", true, []string{"x"}); err == nil {
		t.Error("control character in the directory accepted")
	}
}
