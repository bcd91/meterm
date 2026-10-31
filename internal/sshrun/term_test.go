package sshrun

import (
	"strings"
	"testing"
)

func TestRemoteTerm(t *testing.T) {
	t.Setenv("METERM_TERM", "")
	for in, want := range map[string]string{
		"xterm-ghostty": "xterm-256color", "xterm-kitty": "xterm-256color", "wezterm": "xterm-256color",
		"xterm-256color": "xterm-256color", "screen-256color": "screen-256color", "tmux-256color": "tmux-256color", "": "",
	} {
		if got, _ := RemoteTerm(in); got != want {
			t.Errorf("RemoteTerm(%q) = %q, want %q", in, got, want)
		}
	}
	t.Setenv("METERM_TERM", "keep")
	if got, change := RemoteTerm("xterm-ghostty"); got != "xterm-ghostty" || change {
		t.Errorf("keep: %q %v", got, change)
	}
	t.Setenv("METERM_TERM", "vt100")
	if got, _ := RemoteTerm("xterm-256color"); got != "vt100" {
		t.Errorf("forced: %q", got)
	}
}

func TestWithRemoteTermOnlyTouchesTERM(t *testing.T) {
	t.Setenv("METERM_TERM", "")
	env := withRemoteTerm([]string{"HOME=/h", "TERM=xterm-ghostty", "LANG=th_TH.UTF-8"})
	got := strings.Join(env, " ")
	if got != "HOME=/h LANG=th_TH.UTF-8 TERM=xterm-256color" {
		t.Errorf("env = %q", got)
	}
	plain := []string{"HOME=/h", "TERM=xterm-256color"}
	if got := withRemoteTerm(plain); strings.Join(got, " ") != strings.Join(plain, " ") {
		t.Errorf("changed a common TERM: %q", got)
	}
}
