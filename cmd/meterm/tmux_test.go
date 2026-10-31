package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestParseTmuxList(t *testing.T) {
	out := "0\t1\t0\t1773316154\nwork\t3\t1\t1790000000\nbad;name\t1\t0\t1\n$(reboot)\t1\t0\t1\n" + tmuxMarker + "yes\n"
	s, installed, ok := parseTmuxList(out)
	if !ok || !installed || len(s) != 2 || s[0].Name != "0" || s[1].Name != "work" || s[1].Attached != 1 || s[1].Windows != 3 {
		t.Fatalf("got %+v %v %v", s, installed, ok)
	}
	if _, installed, ok := parseTmuxList(tmuxMarker + "no\n"); !ok || installed {
		t.Error("no tmux not reported")
	}
	if _, _, ok := parseTmuxList("bash: garbage\n"); ok {
		t.Error("output without the marker accepted")
	}
}

func TestChooseTmux(t *testing.T) {
	ss := []tmuxSession{{Name: "old", Activity: time.Unix(100, 0)}, {Name: "recent", Activity: time.Unix(200, 0)}}
	var out bytes.Buffer
	for in, want := range map[string]string{"\n": "recent", "1\n": "old", "n\n": "", "9\n": "recent"} {
		if got := chooseTmux("API", ss, strings.NewReader(in), &out, true); got != want {
			t.Errorf("answer %q → %q, want %q", in, got, want)
		}
	}
	if got := chooseTmux("API", ss, strings.NewReader(""), &out, false); got != "recent" {
		t.Errorf("non-interactive → %q", got)
	}
	if got := newTmuxName([]tmuxSession{{Name: "meterm"}, {Name: "meterm-2"}}); got != "meterm-3" {
		t.Errorf("newTmuxName = %q", got)
	}
}

func TestTakeTmuxFlag(t *testing.T) {
	for in, want := range map[string]string{
		"API --tmux":             "API|true",
		"--tmux API -- -L 1:x:2": "API -- -L 1:x:2|true",
		"API -- --tmux":          "API -- --tmux|false", // after --: ssh's, not ours
		"API":                    "API|false",
	} {
		got, found := takeFlagTmux(strings.Fields(in))
		if g := strings.Join(got, " ") + "|" + map[bool]string{true: "true", false: "false"}[found]; g != want {
			t.Errorf("%q → %q, want %q", in, g, want)
		}
	}
}

func takeFlagTmux(args []string) ([]string, bool) { return takeFlag(args, "--tmux") }
