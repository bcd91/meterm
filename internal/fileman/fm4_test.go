package fileman

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// slowListFS is this machine's disk as a host called web1 whose listing of
// slow takes until gate is closed: a directory change that lands late.
type slowListFS struct {
	Local
	slow string
	gate chan struct{}
}

func (slowListFS) Label() string { return "web1" }

func (s slowListFS) ReadDir(dir string) ([]Entry, error) {
	if dir == s.slow {
		<-s.gate
	}
	return s.Local.ReadDir(dir)
}

// slowModel has this machine on the left at left and a web1 on the right
// at start, whose listing of slow waits for the gate it returns. The right
// pane is active.
func slowModel(t *testing.T, left, start, slow string) (Model, chan struct{}) {
	t.Helper()
	gate := make(chan struct{})
	m := New(Options{Left: Local{}, Right: slowListFS{slow: slow, gate: gate}, LeftDir: left, RightDir: start})
	clock := time.Unix(1000, 0)
	m.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	for i := 0; i < 2; i++ {
		m, _ = update(t, m, m.load(i, m.panes[i].dir)())
	}
	m, _ = update(t, m, tea.WindowSizeMsg{Width: 300, Height: 30})
	m.active = 1
	return m, gate
}

// A delete question names what was under the cursor when it opened; a
// listing of another directory landing while it's open must not turn the
// answer into a delete of the same name somewhere else.
func TestDeleteIsOfWhatTheQuestionNamed(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	a := filepath.Join(right, "A")
	write(t, filepath.Join(a, "config"), "inner", 0o644)
	write(t, filepath.Join(right, "config"), "outer", 0o644)
	m, gate := slowModel(t, left, a, right)
	if e, _ := m.panes[1].current(); e.Name != "config" {
		t.Fatalf("cursor on %q", e.Name)
	}
	m, up := update(t, m, key("h")) // the listing of right is slow
	landed := runAsync(up)
	m, _ = update(t, m, key("d"))
	if m.mode != modeConfirm {
		t.Fatalf("mode %v, want the delete question", m.mode)
	}
	if !strings.Contains(m.confirmText, "the file config in web1:"+a+"?") {
		t.Errorf("question %q, want it to name config in web1:%s", m.confirmText, a)
	}
	close(gate)
	m, _ = update(t, m, <-landed)
	if m.mode == modeConfirm || !strings.Contains(m.status, "cancelled: the directory changed while asking") {
		t.Errorf("mode %v, status %q: want the question cancelled once the directory changed", m.mode, m.status)
	}
	for _, p := range []string{filepath.Join(a, "config"), filepath.Join(right, "config")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// The same for the rename box: the directory it was opened in changing
// cancels it.
func TestRenameIsOfWhatTheBoxNamed(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	a := filepath.Join(right, "A")
	sub := filepath.Join(a, "sub")
	write(t, filepath.Join(sub, "inside"), "x", 0o644)
	write(t, filepath.Join(a, "config"), "c", 0o644)
	m, gate := slowModel(t, left, a, sub)
	m, in := update(t, m, key("l")) // into sub, slowly
	landed := runAsync(in)
	m, _ = update(t, m, key("R"))
	if m.mode != modeInput {
		t.Fatalf("mode %v, want the rename box", m.mode)
	}
	if !strings.Contains(m.input.Prompt, "web1:"+a) {
		t.Errorf("mode %v prompt %q, want the rename box naming web1:%s", m.mode, m.input.Prompt, a)
	}
	close(gate)
	m, _ = update(t, m, <-landed)
	if m.mode == modeInput || !strings.Contains(m.status, "cancelled: the directory changed while asking") {
		t.Errorf("mode %v, status %q: want the box cancelled once the directory changed", m.mode, m.status)
	}
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyCtrlU})
	m, _ = update(t, m, key("x"))
	m, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	for _, msg := range drainCmd(cmd) {
		m, _ = update(t, m, msg)
	}
	if _, err := os.Stat(sub); err != nil {
		t.Errorf("sub: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sub, "inside")); err != nil {
		t.Errorf("sub/inside: %v", err)
	}
}

// A listing of the same directory moves the cursor when an entry appears
// above it: the rename still renames what the box was opened on, and the
// delete deletes what the question named.
func TestReloadUnderAQuestionKeepsItsTarget(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(right, "config"), "c", 0o644)
	m, _ := slowModel(t, left, right, "")
	m, _ = update(t, m, key("R"))
	write(t, filepath.Join(right, "a0"), "a", 0o644) // appears above config
	m, _ = update(t, m, m.load(1, right)())
	if e, _ := m.panes[1].current(); e.Name != "a0" {
		t.Fatalf("cursor on %q after the reload; the test needs it moved", e.Name)
	}
	if m.mode != modeInput {
		t.Fatalf("mode %v: a reload of the same directory shouldn't cancel the box", m.mode)
	}
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyCtrlU})
	m, _ = update(t, m, key("renamed"))
	m, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	for _, msg := range drainCmd(cmd) {
		m, _ = update(t, m, msg)
	}
	if _, err := os.Stat(filepath.Join(right, "a0")); err != nil {
		t.Errorf("a0 was renamed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(right, "renamed")); err != nil {
		t.Errorf("config wasn't renamed: %v", err)
	}
}

// The overwrite question is about the directories it was asked in: one of
// them changing cancels it.
func TestOverwriteQuestionCancelledWhenADirectoryChanges(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "new", 0o644)
	write(t, filepath.Join(right, "a.txt"), "old", 0o644)
	other := filepath.Join(right, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	m, gate := slowModel(t, left, right, other)
	m.active = 0
	m.panes[0].cursor = 0
	m, cmd := update(t, m, key("c"))
	m, _ = update(t, m, planOnly(cmd)())
	if m.mode != modeConfirm || m.confirmKind != confirmOverwrite {
		t.Fatalf("mode %v, want the overwrite question", m.mode)
	}
	if !strings.Contains(m.confirmText, "web1:"+right) {
		t.Errorf("mode %v, question %q: want an overwrite question naming web1:%s", m.mode, m.confirmText, right)
	}
	close(gate)
	m, _ = update(t, m, dirLoadedMsg{pane: 1, dir: other})
	if m.mode == modeConfirm || !strings.Contains(m.status, "cancelled: the directory changed while asking") {
		t.Errorf("mode %v status %q", m.mode, m.status)
	}
	if got, _ := os.ReadFile(filepath.Join(right, "a.txt")); string(got) != "old" {
		t.Errorf("a.txt = %q", got)
	}
}

// kindSides are the sides a copy is checked against: this machine, OpenSSH's
// sftp-server, and that server with posix-rename refused, as an older or
// other server would be.
func kindSides() map[string]func(t *testing.T) FS {
	return map[string]func(t *testing.T) FS{
		"local":   func(t *testing.T) FS { return Local{} },
		"openssh": func(t *testing.T) FS { return newSftpServerRemote(t) },
		"openssh-no-posix-rename": func(t *testing.T) FS {
			r := newSftpServerRemote(t, "-P", "posix-rename")
			r.noPosixRename = true
			return r
		},
	}
}

// A file never replaces a directory, nor a directory a file: refused when
// the copy is worked out, never asked.
func TestCopyRefusesToChangeAKind(t *testing.T) {
	for name, side := range kindSides() {
		t.Run(name, func(t *testing.T) {
			dst := side(t)
			src, dir := t.TempDir(), t.TempDir()
			write(t, filepath.Join(src, "conf"), "file", 0o644)
			write(t, filepath.Join(dir, "conf", "inside"), "keep", 0o644)
			_, err := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "conf")}, dst, dir)
			if err == nil || !strings.Contains(err.Error(), "is a directory; not replacing a directory with a file") {
				t.Errorf("file over a directory: %v", err)
			}
			src2, dir2 := t.TempDir(), t.TempDir()
			write(t, filepath.Join(src2, "conf", "x"), "x", 0o644)
			write(t, filepath.Join(dir2, "conf"), "file", 0o644)
			_, err = MakePlan(context.Background(), Local{}, []string{filepath.Join(src2, "conf")}, dst, dir2)
			if err == nil || !strings.Contains(err.Error(), "is a file; not replacing a file with a directory") {
				t.Errorf("directory over a file: %v", err)
			}
			// What a plan made before the directory appeared would do.
			tmp := filepath.Join(dir, ".conf.part")
			write(t, tmp, "new", 0o644)
			if err := dst.Replace(tmp, filepath.Join(dir, "conf")); err == nil {
				t.Error("Replace put a file in place of a directory")
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "conf", "inside")); string(got) != "keep" {
				t.Errorf("conf/inside = %q", got)
			}
			if names := dirNames(t, dir); len(names) != 2 {
				t.Errorf("left %v, want conf and the temporary file only", names)
			}
		})
	}
}

// In the file manager the refusal is the answer: no question.
func TestFileOverDirectoryIsNotAsked(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "conf"), "file", 0o644)
	write(t, filepath.Join(right, "conf", "inside"), "keep", 0o644)
	h := newHarness(t, left, right)
	h.keys("c")
	if m := h.model(); m.mode == modeConfirm || !m.statusErr || !strings.Contains(m.status, "not replacing a directory with a file") {
		t.Errorf("mode %v status %q, want a refusal and no question", m.mode, m.status)
	}
}

// Overwriting through a symlink is said before and after: the file it
// leads to is what changes.
func TestOverwriteSaysALinkIsFollowed(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "viaimg"), "new", 0o644)
	write(t, filepath.Join(right, "linktarget"), "old", 0o644)
	if err := os.Symlink("linktarget", filepath.Join(right, "viaimg")); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, left, right)
	h.keys("c")
	want := "viaimg (-> linktarget, the file it points to will be overwritten)"
	if m := h.model(); m.mode != modeConfirm || !strings.Contains(m.confirmText, want) {
		t.Fatalf("question %q, want %q", m.confirmText, want)
	}
	h.keys("o")
	if got, _ := os.ReadFile(filepath.Join(right, "linktarget")); string(got) != "new" {
		t.Fatalf("linktarget = %q", got)
	}
	if st := h.model().status; !strings.Contains(st, "viaimg -> linktarget") || !strings.Contains(st, "points to") {
		t.Errorf("status %q, want it to say the link was followed", st)
	}
}

// outcomes are the leading words of what was logged, in order.
func outcomes(es []event) []string {
	var out []string
	for _, e := range es {
		w, _, _ := strings.Cut(e.detail, ": ")
		out = append(out, w)
	}
	return out
}

// Every logged line starts with how it went, then what it was: a terminal
// that cuts a long detail keeps its start. Every copy logs that it started,
// so one killed part way still shows.
func TestLogLeadsWithTheOutcome(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "a", 0o644)
	h := newHarness(t, left, right)
	h.keys("c")
	want := []string{"started", "ok"}
	if got := outcomes(h.events); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("a new upload logged %v (%s), want %v", got, h.allEvents(), want)
	}
	for _, e := range h.events {
		if !strings.Contains(e.detail, ": upload a.txt -> "+right) {
			t.Errorf("logged %q, want the outcome, then upload a.txt -> %s", e.detail, right)
		}
	}
	h.events = nil
	h.keys("tab", "R")
	h.send(tea.KeyMsg{Type: tea.KeyCtrlU})
	h.keys("b.txt", "enter")
	h.keys("m", "d1", "enter")
	h.keys("G", "d", "y")
	for i, w := range []string{"ok: rename " + filepath.Join(right, "a.txt") + " -> " + filepath.Join(right, "b.txt"),
		"ok: mkdir " + filepath.Join(right, "d1"),
		"started: delete " + filepath.Join(right, "b.txt"),
		"ok: delete " + filepath.Join(right, "b.txt")} {
		if i >= len(h.events) || h.events[i].detail != w {
			t.Errorf("event %d: want %q, log is\n%s", i, w, h.allEvents())
		}
	}
}

// A copy that fails or is cancelled says so first.
func TestFailedAndCancelledCopiesLeadWithIt(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "zz-bad.txt"), "x", 0o644)
	h := newFailHarness(t, left, right, "zz-bad")
	h.keys("c")
	if last := h.lastEvent(); !strings.HasPrefix(last.detail, "failed: upload zz-bad.txt -> "+right) {
		t.Errorf("failed copy logged %q", last.detail)
	}
	m, gate, _, events := startGatedOverwrite(t)
	m, _ = update(t, m, key("esc"))
	close(gate)
	untilIdle(t, m)
	if got := outcomes(*events); strings.Join(got, ",") != "started,cancelled" {
		t.Errorf("cancelled overwrite logged %v", *events)
	}
}

// Deleting something already gone says so, rather than "deleted".
func TestDeleteOfSomethingGoneSaysSo(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(right, "a.txt"), "a", 0o644)
	write(t, filepath.Join(right, "b.txt"), "b", 0o644)
	h := newHarness(t, left, right)
	h.keys("tab", "*", "d")
	if err := os.Remove(filepath.Join(right, "b.txt")); err != nil {
		t.Fatal(err)
	}
	h.keys("y")
	if st := h.model().status; !strings.Contains(st, "deleted 1 item(s)") || !strings.Contains(st, "1 already gone") {
		t.Errorf("status %q", st)
	}
	if last := h.lastEvent(); last.detail != "ok: delete "+filepath.Join(right, "a.txt")+"; already gone: "+filepath.Join(right, "b.txt") {
		t.Errorf("logged %q", last.detail)
	}
	write(t, filepath.Join(right, "c.txt"), "c", 0o644)
	h.keys("r", "*", "d")
	_ = os.Remove(filepath.Join(right, "c.txt"))
	h.keys("y")
	if last := h.lastEvent(); last.detail != "already gone: delete "+filepath.Join(right, "c.txt") || !strings.Contains(h.model().status, "already gone") {
		t.Errorf("logged %q, status %q", last.detail, h.model().status)
	}
}

// Enter on a rename box nobody changed renames nothing; a name the box
// can't show starts the box empty, saying why, rather than with a cleaned
// copy that enter would rename it to.
func TestRenameWithoutAnEditDoesNothing(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	for _, n := range []string{" sp", "tab\tx", "plain"} {
		write(t, filepath.Join(right, n), "x", 0o644)
	}
	h := newHarness(t, left, right)
	h.keys("tab")
	for i, n := range []string{" sp", "plain", "tab\tx"} {
		h.keys("g")
		for k := 0; k < i; k++ {
			h.keys("j")
		}
		if h.cursorName(1) != n {
			t.Fatalf("cursor on %q, want %q", h.cursorName(1), n)
		}
		h.keys("R")
		m := h.model()
		hidden := n != "plain"
		if hidden && (m.input.Value() != "" || !strings.Contains(m.input.Prompt, "this name has hidden characters; type the new name")) {
			t.Errorf("%q: box %q with prompt %q, want it empty, saying why", n, m.input.Value(), m.input.Prompt)
		}
		h.keys("enter")
		if st := h.model().status; !strings.Contains(st, "unchanged") {
			t.Errorf("%q: status %q, want unchanged", n, st)
		}
	}
	if got := dirNames(t, right); strings.Join(got, "|") != " sp|plain|tab\tx" {
		t.Errorf("names now %q", got)
	}
	if len(h.events) != 0 {
		t.Errorf("logged %v", h.events)
	}
	h.keys("g", "R", "sp", "enter")
	if _, err := os.Stat(filepath.Join(right, "sp")); err != nil {
		t.Errorf("typed name not used: %v", err)
	}
}

// A copy that stopped to ask about writing in place stops there when a
// pane's directory changes under the question: nothing is written.
func TestInPlaceQuestionCancelledWhenADirectoryChanges(t *testing.T) {
	ro := readOnlyDir(t, map[string]os.FileMode{"index.html": 0o666})
	left := t.TempDir()
	write(t, filepath.Join(left, "index.html"), "new index", 0o644)
	h := newHarness(t, left, ro)
	h.keys("c", "o")
	if m := h.model(); m.mode != modeConfirm || m.confirmKind != confirmInPlace {
		t.Fatalf("mode %v kind %v status %q: want the write-in-place question", h.model().mode, h.model().confirmKind, h.model().status)
	}
	h.send(dirLoadedMsg{pane: 0, dir: t.TempDir()})
	if m := h.model(); m.mode == modeConfirm || !strings.Contains(m.status, dirChanged) || !strings.Contains(m.status, "not copied") {
		t.Errorf("mode %v status %q", m.mode, m.status)
	}
	h.keys("i")
	if got, _ := os.ReadFile(filepath.Join(ro, "index.html")); string(got) != "old index.html" {
		t.Errorf("index.html = %q", got)
	}
	if last := h.lastEvent(); !strings.HasPrefix(last.detail, "stopped: upload index.html") {
		t.Errorf("logged %q", last.detail)
	}
}

// The answer typed to a question that was just cancelled mustn't become a
// command: y is also copy, and a name typed into a cancelled box is a
// string of commands. Keys wait until the cancel is seen: enter or esc.
func TestKeysAfterACancelledQuestionWait(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	a := filepath.Join(right, "A")
	write(t, filepath.Join(a, "config"), "inner", 0o644)
	m, gate := slowModel(t, left, a, right)
	m, up := update(t, m, key("h"))
	landed := runAsync(up)
	m, _ = update(t, m, key("d"))
	close(gate)
	m, _ = update(t, m, <-landed)
	for _, k := range []string{"y", "d", "m", "c"} {
		m, _ = update(t, m, key(k))
		if m.mode == modeBusy || m.mode == modeConfirm || m.mode == modeInput {
			t.Fatalf("%s after the cancel: mode %v; want it waiting for enter or esc", k, m.mode)
		}
	}
	if v := viewText(m); !strings.Contains(v, dirChanged) || !strings.Contains(v, "enter or esc") {
		t.Errorf("screen doesn't say what happened and how to go on:\n%s", v)
	}
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeNormal {
		t.Errorf("mode %v after enter", m.mode)
	}
	m, _ = update(t, m, key("j"))
	if m.mode != modeNormal {
		t.Errorf("mode %v", m.mode)
	}
}

// A question whose names fill its three lines still shows its keys: the
// key line names them.
func TestLongOverwriteQuestionKeepsItsKeys(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	for _, n := range []string{"a-long-file-name-number-one", "a-long-file-name-number-two", "a-long-file-name-number-three"} {
		write(t, filepath.Join(left, n), "new", 0o644)
		write(t, filepath.Join(right, "target-of-"+n), "old", 0o644)
		if err := os.Symlink("target-of-"+n, filepath.Join(right, n)); err != nil {
			t.Fatal(err)
		}
	}
	h := newHarness(t, left, right)
	h.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	h.keys("*", "c")
	if h.model().mode != modeConfirm {
		t.Fatalf("mode %v", h.model().mode)
	}
	lines := strings.Split(viewText(h.model()), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "o overwrite") || !strings.Contains(last, "esc cancel") {
		t.Errorf("key line %q, want the question's keys", last)
	}
	h.keys("esc", "R", "esc", "/")
	if w := h.model().input.Width; w != 0 {
		t.Errorf("find box width %d, want it unlimited as before", w)
	}
}
