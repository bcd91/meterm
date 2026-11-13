//go:build unix

package fileman

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// checkOps says whether every operation logged has one start first and
// one end last, everything between them an answer, all on the same side.
func checkOps(t *testing.T, es []Event) {
	t.Helper()
	type op struct {
		started, ended bool
		remote         bool
	}
	ops := map[int64]*op{}
	for _, e := range es {
		if e.Op == 0 {
			t.Errorf("no operation: %+v", e)
			continue
		}
		o := ops[e.Op]
		switch {
		case e.Outcome == "started":
			if o != nil {
				t.Errorf("op %d started twice: %q", e.Op, e.Line())
			}
			ops[e.Op] = &op{started: true, remote: e.Remote}
		case o == nil:
			t.Errorf("op %d logged before its start: %q", e.Op, e.Line())
		case o.ended:
			t.Errorf("op %d logged after its end: %q", e.Op, e.Line())
		case o.remote != e.Remote:
			t.Errorf("op %d changed sides: %q", e.Op, e.Line())
		case e.Ends():
			o.ended = true
		}
	}
	for id, o := range ops {
		if !o.ended {
			t.Errorf("op %d never ended", id)
		}
	}
}

// 10. Every copy, delete, rename and mkdir is a start, then an end with
// the same operation: a question part way is an answer within it.
func TestEveryOperationStartsAndEnds(t *testing.T) {
	ro := readOnlyDir(t, map[string]os.FileMode{"index.html": 0o666})
	left := t.TempDir()
	write(t, filepath.Join(left, "index.html"), "new index", 0o644)
	var raw []Event
	h := newHarnessWith(t, left, ro, func(o *Options) {
		o.OnEvent = func(e Event) { raw = append(raw, e) }
	})
	h.keys("c", "o", "i") // asks about writing in place; yes
	h.keys("esc", "c", "o", "s")
	h.keys("esc", "c", "o", "n") // no: stopped
	if len(raw) < 6 {
		t.Fatalf("logged %d events", len(raw))
	}
	checkOps(t, raw)
	if t.Failed() {
		for _, e := range raw {
			t.Logf("%d %s", e.Op, e.Line())
		}
	}
	var asked int
	for _, e := range raw {
		if e.Outcome == "asked" {
			asked++
		}
	}
	if asked != 1 {
		t.Errorf("%d answers logged, want the one yes", asked)
	}

	right := t.TempDir()
	write(t, filepath.Join(right, "a.txt"), "a", 0o644)
	raw = nil
	h = newHarnessWith(t, left, right, func(o *Options) {
		o.OnEvent = func(e Event) { raw = append(raw, e) }
	})
	h.keys("tab", "R")
	h.send(tea.KeyMsg{Type: tea.KeyCtrlU})
	h.keys("b.txt", "enter", "m", "d1", "enter", "m", "b.txt", "enter", "esc", "G", "d", "y")
	h.keys("tab", "c")
	checkOps(t, raw)
	kinds := map[string]bool{}
	for _, e := range raw {
		kinds[e.Kind+" "+e.Outcome] = true
	}
	for _, want := range []string{"rename ok", "mkdir ok", "mkdir failed", "delete ok", "upload ok"} {
		if !kinds[want] {
			t.Errorf("no %q among %v", want, kinds)
		}
	}
}

// 7. A cancel says, of the file it was writing, that it's unchanged, with
// its side, on screen and in the log.
func TestCancelSaysWhichFileIsUnchanged(t *testing.T) {
	m, gate, right, events := startGatedOverwrite(t)
	m, _ = update(t, m, key("esc"))
	close(gate)
	m = untilIdle(t, m)
	if want := "cancelled: big.bin unchanged"; !strings.HasPrefix(m.status, want) {
		t.Errorf("status = %q, want %q first", m.status, want)
	}
	want := "; test:" + filepath.Join(right, "big.bin") + " unchanged"
	if last := (*events)[len(*events)-1]; !strings.HasPrefix(last.detail, "cancelled: upload") || !strings.Contains(last.detail, want) {
		t.Errorf("log = %q, want %q", last.detail, want)
	}
	// At 60 columns the words that matter are on screen.
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 60, Height: 20})
	checkFits(t, "cancel at 60", m)
	if v := viewText(m); !strings.Contains(v, "big.bin unchanged") {
		t.Errorf("at 60 columns the cancel doesn't say big.bin is unchanged:\n%s", v)
	}
}

// 8. A batch that stops part way says which files it had already copied:
// on screen the first few, in the log all of them.
func TestAStoppedBatchNamesWhatItCopied(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		write(t, filepath.Join(left, n), n, 0o644)
	}
	h := newHarnessWith(t, left, right, func(o *Options) { o.Right = noSpaceFor{o.Right, "c"} })
	h.keys("*", "c")
	st := h.model().status
	for _, want := range []string{"copy failed after 2 of 3 file(s)", "no space left on device", "already copied: a, b"} {
		if !strings.Contains(st, want) {
			t.Errorf("status %q doesn't say %q", st, want)
		}
	}
	last := h.lastEvent().detail
	if !strings.HasPrefix(last, "failed: upload") || !strings.Contains(last, "copied before it stopped: "+filepath.Join(right, "a")+", "+filepath.Join(right, "b")) {
		t.Errorf("log %q doesn't list what was copied", last)
	}
}

// 7. A listing that fails after a copy failed (the connection went with
// it) doesn't hide what the copy said: which file is unchanged, and the
// temporary file left behind.
func TestAFailedListingKeepsTheCopysWords(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	h := newHarness(t, left, right)
	m := h.model()
	m.setStatus("copy failed after 0 of 1 file(s): web1:/srv/a unchanged; the unfinished copy is still at web1:/srv/.a.meterm-1.part", true)
	h.m = m
	h.send(dirLoadedMsg{pane: 1, dir: right, err: errors.New("connection lost")})
	st := h.model().status
	if !strings.Contains(st, "unfinished copy is still at web1:/srv/.a.meterm-1.part") || !strings.Contains(st, "connection lost") {
		t.Errorf("status = %q, want the copy's words and the listing's", st)
	}
	h.send(dirLoadedMsg{pane: 1, dir: right, err: errors.New("connection lost")})
	if got := h.model().status; strings.Count(got, "connection lost") != 1 {
		t.Errorf("a second failed listing repeated itself: %q", got)
	}
}

// 7, on screen: a failed copy's status says first which file is unchanged
// and why, then the leftover's name, short enough for 60 columns.
func TestAFailedCopySaysWhatItLeftFirst(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "new", 0o644)
	write(t, filepath.Join(right, "a.txt"), "old", 0o644)
	h := newHarnessWith(t, left, right, func(o *Options) { o.Right = removeFails{failWrites{Local{}}} })
	h.send(tea.WindowSizeMsg{Width: 60, Height: 20})
	h.keys("c", "o")
	st := h.model().status
	if !strings.HasPrefix(st, "copy failed after 0 of 1 file(s): a.txt unchanged (input/output error (test)); unfinished copy left: .a.txt.meterm-") {
		t.Errorf("status = %q", st)
	}
	checkFits(t, "failed copy at 60", h.model())
	if v := viewText(h.model()); !strings.Contains(v, "a.txt unchanged") || !strings.Contains(v, "unfinished copy left") {
		t.Errorf("at 60 columns:\n%s", v)
	}
	for _, n := range partFiles(t, right) {
		os.Remove(filepath.Join(right, n))
	}
}
