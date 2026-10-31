package fileman

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

type event struct {
	kind, detail string
	remote       bool
}

// harness drives the model like Bubble Tea does: run each command, feed its
// message back in, until nothing is left to do.
type harness struct {
	t      *testing.T
	m      tea.Model
	events []event
}

func newHarness(t *testing.T, left, right string) *harness {
	h := &harness{t: t}
	remote := newRemote(t)
	model := New(Options{Left: Local{}, Right: remote, LeftDir: left, RightDir: right,
		OnEvent: func(kind, detail string, isRemote bool) { h.events = append(h.events, event{kind, detail, isRemote}) }})
	h.m = model
	h.run(model.Init())
	h.send(tea.WindowSizeMsg{Width: 120, Height: 30})
	return h
}

func (h *harness) run(cmd tea.Cmd) {
	queue := []tea.Cmd{cmd}
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 1000 {
			h.t.Fatal("model never settled")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case nil, tea.QuitMsg:
		case tea.BatchMsg:
			queue = append(queue, msg...)
		default:
			var next tea.Cmd
			h.m, next = h.m.Update(msg)
			queue = append(queue, next)
		}
	}
}

func (h *harness) send(msgs ...tea.Msg) {
	for _, msg := range msgs {
		var cmd tea.Cmd
		h.m, cmd = h.m.Update(msg)
		h.run(cmd)
	}
}

func (h *harness) keys(ks ...string) {
	for _, k := range ks {
		switch k {
		case "enter":
			h.send(tea.KeyMsg{Type: tea.KeyEnter})
		case "esc":
			h.send(tea.KeyMsg{Type: tea.KeyEsc})
		case "tab":
			h.send(tea.KeyMsg{Type: tea.KeyTab})
		case "space":
			h.send(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
		default:
			h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		}
	}
}

func (h *harness) model() Model { return h.m.(Model) }

func (h *harness) cursorName(pane int) string {
	e, _ := h.model().panes[pane].current()
	return e.Name
}

func TestNavigateIntoAndBackOut(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "a", 0o644)
	write(t, filepath.Join(left, "sub", "inner.txt"), "i", 0o644)
	h := newHarness(t, left, right)

	if h.cursorName(0) != "sub" {
		t.Fatalf("cursor on %q, want the directory first", h.cursorName(0))
	}
	h.keys("l")
	if got := h.model().panes[0].dir; got != filepath.Join(left, "sub") || h.cursorName(0) != "inner.txt" {
		t.Fatalf("after l: dir %s, cursor %s", got, h.cursorName(0))
	}
	h.keys("h")
	if h.model().panes[0].dir != left || h.cursorName(0) != "sub" {
		t.Errorf("after h: dir %s, cursor %s (want back on sub)", h.model().panes[0].dir, h.cursorName(0))
	}
}

func TestUploadSelectionAndDownloadBack(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "alpha", 0o644)
	write(t, filepath.Join(left, "b.txt"), "bravo", 0o644)
	write(t, filepath.Join(left, "c.txt"), "charlie", 0o644)
	h := newHarness(t, left, right)

	h.keys("space", "j", "space", "c") // select a.txt and c.txt (cursor moved past b), copy
	for _, n := range []string{"a.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(right, n)); err != nil {
			t.Errorf("%s not uploaded: %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(right, "b.txt")); err == nil {
		t.Error("b.txt was not selected but got uploaded")
	}
	if len(h.events) != 1 || h.events[0].kind != "upload" || !h.events[0].remote || !strings.Contains(h.events[0].detail, "a.txt, c.txt") {
		t.Errorf("events = %+v", h.events)
	}
	if !strings.Contains(h.model().status, "copied 2 file(s)") {
		t.Errorf("status = %q", h.model().status)
	}

	// Pull a.txt back into a new local directory.
	os.Remove(filepath.Join(left, "a.txt"))
	h.keys("R", "tab", "c")
	if got, _ := os.ReadFile(filepath.Join(left, "a.txt")); string(got) != "alpha" {
		t.Errorf("downloaded a.txt = %q", got)
	}
	if h.events[len(h.events)-1].kind != "download" {
		t.Errorf("last event = %+v, want download", h.events[len(h.events)-1])
	}
}

func TestOverwriteIsAskedFirst(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "new", 0o644)
	write(t, filepath.Join(right, "a.txt"), "old", 0o644)
	h := newHarness(t, left, right)

	h.keys("c")
	if h.model().mode != modeConfirm || !strings.Contains(h.model().confirmText, "already exist") {
		t.Fatalf("mode %v, confirm %q: expected an overwrite question", h.model().mode, h.model().confirmText)
	}
	h.keys("s")
	if got, _ := os.ReadFile(filepath.Join(right, "a.txt")); string(got) != "old" {
		t.Errorf("skip overwrote: %q", got)
	}
	h.keys("c", "o")
	if got, _ := os.ReadFile(filepath.Join(right, "a.txt")); string(got) != "new" {
		t.Errorf("overwrite didn't: %q", got)
	}
}

func TestDeleteNeedsYes(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(right, "dir", "f.txt"), "x", 0o644)
	h := newHarness(t, left, right)
	h.keys("tab", "d", "n")
	if _, err := os.Stat(filepath.Join(right, "dir")); err != nil {
		t.Fatal("deleted without a yes")
	}
	h.keys("d", "y")
	if _, err := os.Stat(filepath.Join(right, "dir")); !os.IsNotExist(err) {
		t.Errorf("dir still there after yes")
	}
	if last := h.events[len(h.events)-1]; last.kind != "delete" || !last.remote {
		t.Errorf("event = %+v", last)
	}
}

func TestMkdirAndRename(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	h := newHarness(t, left, right)
	h.keys("tab", "m", "releases", "enter")
	if st, err := os.Stat(filepath.Join(right, "releases")); err != nil || !st.IsDir() {
		t.Fatalf("mkdir: %v", err)
	}
	if h.cursorName(1) != "releases" {
		t.Errorf("cursor on %q, want the new directory", h.cursorName(1))
	}
	h.keys("r")
	h.send(tea.KeyMsg{Type: tea.KeyCtrlU}) // clear the prefilled name
	h.keys("old-releases", "enter")
	if _, err := os.Stat(filepath.Join(right, "old-releases")); err != nil {
		t.Errorf("rename: %v (status %q)", err, h.model().status)
	}
	h.keys("r")
	h.send(tea.KeyMsg{Type: tea.KeyCtrlU})
	h.keys("../escape", "enter")
	if !h.model().statusErr {
		t.Error("a name with a slash should be refused")
	}
}

func TestViewFitsSmallTerminals(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, strings.Repeat("very-long-file-name-", 10)+".txt"), "x", 0o644)
	h := newHarness(t, left, right)
	for _, w := range []int{40, 80, 200} {
		h.send(tea.WindowSizeMsg{Width: w, Height: 12})
		for i, line := range strings.Split(h.m.View(), "\n") {
			if lw := len([]rune(stripANSI(line))); lw > w+2 {
				t.Errorf("width %d: line %d is %d wide", w, i, lw)
			}
		}
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			in = true
		case in && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'):
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestKeysTypedTogetherEachCount(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(right, "a.txt"), "a", 0o644)
	write(t, filepath.Join(right, "z.txt"), "z", 0o644)
	h := newHarness(t, left, right)
	h.keys("tab")
	h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Gc")}) // what a fast burst looks like
	if got, _ := os.ReadFile(filepath.Join(left, "z.txt")); string(got) != "z" {
		t.Errorf("burst \"Gc\" didn't copy the last file (status %q)", h.model().status)
	}
}

func TestBurstAtAPromptAnswersIt(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(right, "x.txt"), "x", 0o644)
	h := newHarness(t, left, right)
	h.keys("tab", "d")
	h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("yq")}) // "y" then "q", read together
	if _, err := os.Stat(filepath.Join(right, "x.txt")); !os.IsNotExist(err) {
		t.Errorf("burst \"yq\" at the delete prompt didn't delete")
	}
}

func TestFindJumpsToAMatch(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	for _, n := range []string{"alpha.conf", "hostname", "hosts", "nginx.conf"} {
		write(t, filepath.Join(left, n), "x", 0o644)
	}
	h := newHarness(t, left, right)
	h.keys("/", "host", "enter")
	if h.cursorName(0) != "hostname" {
		t.Fatalf("find host -> %q, want hostname (first prefix match)", h.cursorName(0))
	}
	h.keys("n")
	if h.cursorName(0) != "hosts" {
		t.Errorf("n -> %q, want hosts", h.cursorName(0))
	}
	h.keys("/", "ginx", "enter") // no prefix match: falls back to contains
	if h.cursorName(0) != "nginx.conf" {
		t.Errorf("find ginx -> %q, want nginx.conf", h.cursorName(0))
	}
}

// The cursor row must always be on screen, whatever the height, scrolling
// down past the bottom and back up past the top.
func TestCursorStaysVisibleWhileScrolling(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	for i := 0; i < 60; i++ {
		write(t, filepath.Join(left, fmt.Sprintf("file-%02d.txt", i)), "x", 0o644)
	}
	h := newHarness(t, left, right)
	for _, height := range []int{8, 12, 15, 24, 40} {
		h.send(tea.WindowSizeMsg{Width: 100, Height: height})
		h.keys("g")
		check := func(step string) {
			t.Helper()
			name := h.cursorName(0)
			view := stripANSI(h.m.View())
			if !strings.Contains(view, name) {
				t.Fatalf("height %d, %s: cursor on %s but it isn't drawn", height, step, name)
			}
			if n := len(strings.Split(h.m.View(), "\n")); n > height {
				t.Fatalf("height %d: view is %d lines, taller than the terminal", height, n)
			}
		}
		for i := 0; i < 59; i++ {
			h.keys("j")
			check(fmt.Sprintf("down %d", i+1))
		}
		for i := 0; i < 59; i++ {
			h.keys("k")
			check(fmt.Sprintf("up %d", i+1))
		}
		h.send(tea.KeyMsg{Type: tea.KeyPgDown})
		check("pgdown")
		h.keys("G")
		check("G")
	}
}
