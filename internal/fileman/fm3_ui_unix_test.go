//go:build unix

package fileman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostFS is this machine's disk standing in for a host, under a name of
// its own, so the panes can tell the sides apart.
type hostFS struct{ Local }

func (hostFS) Label() string { return "web1" }

// linkedRight is a right-hand directory with a.txt hard-linked to
// hardlink-a, and a left one with a new a.txt.
func linkedRight(t *testing.T) (left, right string) {
	left, right = t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "new", 0o644)
	write(t, filepath.Join(right, "a.txt"), "old", 0o644)
	if err := os.Link(filepath.Join(right, "a.txt"), filepath.Join(right, "hardlink-a")); err != nil {
		t.Fatal(err)
	}
	return left, right
}

// Replacing a file would split its hard links: that's asked, once, with
// what each answer keeps, on a production pane saying PRODUCTION, and not
// in the question's first moment. The answer is logged before a byte is
// written.
func TestLossIsAskedAndLogged(t *testing.T) {
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	for _, answer := range []string{"i", "r", "s", "x"} {
		t.Run(answer, func(t *testing.T) {
			left, right := linkedRight(t)
			h := newHarnessWith(t, left, right, func(o *Options) { o.Right, o.Env = hostFS{}, "production" })
			h.keys("c", "o")
			m := h.model()
			if m.mode != modeConfirm || m.confirmKind != confirmLoss {
				t.Fatalf("mode %v kind %v status %q: want the question about what replacing loses", m.mode, m.confirmKind, m.status)
			}
			for _, want := range []string{"PRODUCTION", "2 hard links", "i = write in place (keeps owner, group and hard links; not atomic)",
				"r = replace (", "hard links split)", "s = skip", "any other key = stop", "rest of this copy"} {
				if !strings.Contains(m.confirmText, want) {
					t.Errorf("question %q doesn't say %q", m.confirmText, want)
				}
			}
			h.send(key(answer)) // straight away: too soon for i or r
			if (answer == "i" || answer == "r") && h.model().mode != modeConfirm {
				t.Fatalf("%s typed before the question could be read was taken", answer)
			}
			if answer == "i" || answer == "r" {
				h.keys(answer)
			}
			a, hl := filepath.Join(right, "a.txt"), filepath.Join(right, "hardlink-a")
			log := h.allEvents()
			switch answer {
			case "i":
				if read(a) != "new" || read(hl) != "new" {
					t.Errorf("in place: a %q, link %q", read(a), read(hl))
				}
				if !strings.Contains(log, "chose to write in place") || !strings.Contains(h.lastEvent().detail, "written in place") {
					t.Errorf("log %q", log)
				}
			case "r":
				if read(a) != "new" || read(hl) != "old" {
					t.Errorf("replace: a %q, link %q", read(a), read(hl))
				}
				if !strings.Contains(log, "chose to replace") || !strings.Contains(h.lastEvent().detail, "hard links would split") {
					t.Errorf("log %q", log)
				}
			case "s":
				if read(a) != "old" || !strings.Contains(h.model().status, "kept 1 existing") {
					t.Errorf("skip: a %q, status %q", read(a), h.model().status)
				}
				if !strings.Contains(log, "chose to skip") {
					t.Errorf("log %q", log)
				}
			case "x":
				if read(a) != "old" || !strings.Contains(h.model().status, "not copied") {
					t.Errorf("stop: a %q, status %q", read(a), h.model().status)
				}
			}
			if i := strings.Index(log, "chose to"); (answer == "i" || answer == "r") && (i < 0 || i > strings.LastIndex(log, "upload")) {
				t.Errorf("log %q: the choice should come before the copy's outcome", log)
			}
			if n := len(dirNames(t, right)); n != 2 {
				t.Errorf("right holds %v", dirNames(t, right))
			}
		})
	}
}
