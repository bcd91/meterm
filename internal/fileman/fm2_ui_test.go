package fileman

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"meterm/internal/cellwidth"
)

// newHarnessWith is newHarness with options changed by set first.
func newHarnessWith(t *testing.T, left, right string, set func(*Options)) *harness {
	h := &harness{t: t, clock: time.Now()}
	o := Options{Left: Local{}, Right: newRemote(t), LeftDir: left, RightDir: right,
		OnEvent: func(e Event) {
			kind, detail, isRemote := e.Kind, e.Line(), e.Remote
			h.events = append(h.events, event{kind, detail, isRemote})
		}}
	if set != nil {
		set(&o)
	}
	model := New(o)
	model.now = func() time.Time { return h.clock }
	h.m = model
	h.run(model.Init())
	h.send(tea.WindowSizeMsg{Width: 120, Height: 30})
	return h
}

func (h *harness) lastEvent() event {
	h.t.Helper()
	if len(h.events) == 0 {
		h.t.Fatal("nothing was logged")
	}
	return h.events[len(h.events)-1]
}

func (h *harness) allEvents() string {
	var b strings.Builder
	for _, e := range h.events {
		fmt.Fprintf(&b, "%s %s\n", e.kind, e.detail)
	}
	return b.String()
}

// Overwriting a writable file in a directory that can't be written asks a
// second, separate question; no is the default, and only i writes in place.
func TestInPlaceIsAskedAndLogged(t *testing.T) {
	ro := readOnlyDir(t, map[string]os.FileMode{"index.html": 0o666})
	left := t.TempDir()
	write(t, filepath.Join(left, "index.html"), "new index", 0o644)
	h := newHarnessWith(t, left, ro, func(o *Options) { o.Env = "production" })
	h.keys("c", "o")
	m := h.model()
	if m.mode != modeConfirm || m.confirmKind != confirmInPlace {
		t.Fatalf("mode %v kind %v status %q: want the write-in-place question", m.mode, m.confirmKind, m.status)
	}
	for _, want := range []string{"can't write a temporary file in", ro, "the file itself is writable", "i = write in place (not atomic: a failure leaves a partial file)", "PRODUCTION"} {
		if !strings.Contains(m.confirmText, want) {
			t.Errorf("question %q doesn't say %q", m.confirmText, want)
		}
	}
	if strings.Contains(m.confirmText, left) {
		t.Errorf("question %q names the local source", m.confirmText)
	}
	h.keys("o") // not i: no
	if got, _ := os.ReadFile(filepath.Join(ro, "index.html")); string(got) != "old index.html" {
		t.Fatalf("after declining, index.html = %q", got)
	}
	if st := h.model().status; !strings.Contains(st, "not copied") || strings.Contains(st, left) {
		t.Errorf("status after declining = %q", st)
	}
	if e := h.lastEvent(); !strings.Contains(e.detail, "can't write a temporary file") || strings.Contains(e.detail, left+"/index.html:") {
		t.Errorf("log after declining = %+v", e)
	}

	h.keys("esc", "c", "o")
	h.send(key("i")) // straight after the question: too soon to count
	if h.model().mode != modeConfirm {
		t.Fatal("an i typed before the question could be read was taken")
	}
	h.keys("i")
	if got, _ := os.ReadFile(filepath.Join(ro, "index.html")); string(got) != "new index" {
		t.Fatalf("after i, index.html = %q (status %q)", got, h.model().status)
	}
	if st := h.model().status; !strings.Contains(st, "1 written in place") {
		t.Errorf("status after writing in place = %q", st)
	}
	log := h.allEvents()
	for _, want := range []string{"in place", filepath.Join(ro, "index.html")} {
		if !strings.Contains(h.lastEvent().detail, want) {
			t.Errorf("log %q doesn't say %q", log, want)
		}
	}
	if !strings.Contains(log, "agreed to write in place") {
		t.Errorf("log %q doesn't record the decision before writing", log)
	}
}

// A failed write in place is reported as a file that may be partly
// written, on screen and in the log, and the file is left there.
func TestFailedInPlaceSaysPartlyWritten(t *testing.T) {
	ro := readOnlyDir(t, map[string]os.FileMode{"index.html": 0o666})
	left := t.TempDir()
	write(t, filepath.Join(left, "index.html"), "new index", 0o644)
	h := newHarnessWith(t, left, ro, func(o *Options) { o.Right = partialWriteFS{} })
	h.keys("c", "o", "i")
	if st := h.model().status; !strings.Contains(st, "may be partly written") {
		t.Errorf("status = %q", st)
	}
	if e := h.lastEvent(); !strings.Contains(e.detail, "may be partly written") {
		t.Errorf("log = %+v", e)
	}
	if _, err := os.Stat(filepath.Join(ro, "index.html")); err != nil {
		t.Errorf("index.html removed: %v", err)
	}
}

// A production pane says so: its header always shows the host and a
// badge, whatever the width, and every destructive question says
// PRODUCTION. The local pane's questions don't.
func TestProductionIsSaidEverywhere(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	deep := filepath.Join(right, "a-rather-long-directory-name", "and-another-one")
	write(t, filepath.Join(deep, "a.txt"), "old", 0o644)
	write(t, filepath.Join(left, "a.txt"), "new", 0o644)
	h := newHarnessWith(t, left, deep, func(o *Options) { o.Env = "production" })
	for _, w := range []int{40, 60, 120} {
		h.send(tea.WindowSizeMsg{Width: w, Height: 20})
		lines := strings.Split(viewText(h.model()), "\n")
		header := lines[1]
		if !strings.Contains(header, "test:") || !strings.Contains(header, "⚑PROD") {
			t.Errorf("at %d columns the header is %q: want the host and ⚑PROD", w, header)
		}
		if cellwidth.Width(lines[1]) != w {
			t.Errorf("at %d columns the header is %d wide", w, cellwidth.Width(lines[1]))
		}
	}
	h.send(tea.WindowSizeMsg{Width: 120, Height: 30})

	h.keys("c")
	if m := h.model(); m.confirmKind != confirmOverwrite || !strings.Contains(m.confirmText, "PRODUCTION") {
		t.Errorf("overwrite question %q doesn't say PRODUCTION", m.confirmText)
	}
	h.keys("esc", "d")
	if m := h.model(); strings.Contains(m.confirmText, "PRODUCTION") {
		t.Errorf("deleting on this machine says %q", m.confirmText)
	}
	h.keys("n", "tab", "d")
	if m := h.model(); m.confirmKind != confirmDelete || !strings.Contains(m.confirmText, "PRODUCTION") {
		t.Errorf("delete question %q doesn't say PRODUCTION", m.confirmText)
	}
	h.keys("n")

	s := newHarnessWith(t, left, right, func(o *Options) { o.Env = "staging" })
	if header := strings.Split(viewText(s.model()), "\n")[1]; !strings.Contains(header, "⚑staging") || strings.Contains(header, "PROD") {
		t.Errorf("staging header %q", header)
	}
	s.keys("tab", "d")
	if strings.Contains(s.model().confirmText, "PRODUCTION") {
		t.Errorf("staging delete says %q", s.model().confirmText)
	}
}

// A delete that stops part way says on screen what it had removed before
// it stopped, and the log lists every path removed.
func TestPartialDeleteSaysWhatWentFirst(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "sub", "deep", "a"), "a", 0o644)
	write(t, filepath.Join(left, "sub", "zz-bad", "keep"), "k", 0o644)
	write(t, filepath.Join(left, "sub", "aaa"), "x", 0o644)
	h := newFailHarness(t, left, right, "zz-bad")
	h.keys("d", "y")
	st := h.model().status
	for _, want := range []string{"removed 2", "sub/deep/a", "see ? or the log for all", "permission denied"} {
		if !strings.Contains(st, want) {
			t.Errorf("status %q doesn't say %q", st, want)
		}
	}
	e := h.lastEvent()
	for _, p := range []string{filepath.Join(left, "sub", "deep", "a"), filepath.Join(left, "sub", "deep")} {
		if !strings.Contains(e.detail, p) {
			t.Errorf("log %q doesn't list %s", e.detail, p)
		}
	}
	h.keys("?")
	help := viewText(h.model())
	if !strings.Contains(help, "sub/deep/a") {
		h.send(tea.KeyMsg{Type: tea.KeyEnd})
		help = viewText(h.model())
	}
	if !strings.Contains(help, filepath.Join(left, "sub", "deep", "a")) {
		t.Errorf("? doesn't list what was removed:\n%s", help)
	}
}

// The log of a long partial delete is capped, and says how many more.
func TestRemovedListIsCapped(t *testing.T) {
	var ps []string
	for i := 0; i < 250; i++ {
		ps = append(ps, fmt.Sprintf("/x/%03d", i))
	}
	got := capList(ps, logCap)
	if !strings.Contains(got, "/x/199") || strings.Contains(got, "/x/200") || !strings.HasSuffix(got, "+50 more") {
		t.Errorf("capped list ends %q", got[len(got)-40:])
	}
}

// The overwrite question names the files it would replace; the log names
// those it did, and for a symlink, the file the link led to.
func TestOverwriteNamesTheFiles(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	for _, n := range []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"} {
		write(t, filepath.Join(left, n), "new", 0o644)
		write(t, filepath.Join(right, n), "old", 0o644)
	}
	h := newHarness(t, left, right)
	h.keys("*", "c")
	q := h.model().confirmText
	for _, want := range []string{"a.txt", "b.txt", "c.txt", "+2 more"} {
		if !strings.Contains(q, want) {
			t.Errorf("question %q doesn't say %q", q, want)
		}
	}
	h.keys("o")
	if e := h.lastEvent(); !strings.Contains(e.detail, filepath.Join(right, "e.txt")) || !strings.Contains(e.detail, "replaced 5") {
		t.Errorf("log %q doesn't name what it replaced", e.detail)
	}

	l2, r2 := t.TempDir(), t.TempDir()
	write(t, filepath.Join(r2, "hello.txt"), "old", 0o644)
	if err := os.Symlink("hello.txt", filepath.Join(r2, "linkfile")); err != nil {
		t.Skip("no symlinks here")
	}
	write(t, filepath.Join(l2, "linkfile"), "new", 0o644)
	s := newHarness(t, l2, r2)
	s.keys("c", "o")
	want := filepath.Join(r2, "linkfile") + " (-> " + filepath.Join(r2, "hello.txt") + ")"
	if e := s.lastEvent(); !strings.Contains(e.detail, want) {
		t.Errorf("log %q, want it to say %q", e.detail, want)
	}
}

// pgup and pgdn move a full page, ctrl+u and ctrl+d half a page, and ?
// says so.
func TestPageKeys(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	for i := 0; i < 100; i++ {
		write(t, filepath.Join(left, fmt.Sprintf("f%03d", i)), "", 0o644)
	}
	h := newHarness(t, left, right)
	rows := h.model().rows()
	h.send(tea.KeyMsg{Type: tea.KeyPgDown})
	if c := h.model().panes[0].cursor; c != rows {
		t.Errorf("pgdn: cursor %d, want %d (a page)", c, rows)
	}
	h.send(tea.KeyMsg{Type: tea.KeyCtrlD})
	if c := h.model().panes[0].cursor; c != rows+rows/2 {
		t.Errorf("ctrl+d: cursor %d, want %d", c, rows+rows/2)
	}
	h.send(tea.KeyMsg{Type: tea.KeyCtrlU}, tea.KeyMsg{Type: tea.KeyPgUp})
	if c := h.model().panes[0].cursor; c != 0 {
		t.Errorf("ctrl+u, pgup: cursor %d, want 0", c)
	}
	help := strings.Join(fullHelp, "\n")
	if !strings.Contains(help, "pgup pgdn") || !strings.Contains(help, "a page up, down") || !strings.Contains(help, "half a page") {
		t.Errorf("help doesn't explain the page keys:\n%s", help)
	}
}

// Which pane is active is unmistakable, with or without colour: its
// cursor row has the › marker (and reverse video), its border is heavy.
// The other pane's cursor is underlined and dim, without the marker, on
// every kind of entry.
func TestActivePaneIsUnmistakable(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "l.txt"), "x", 0o644)
	write(t, filepath.Join(right, "adir", "x"), "x", 0o644)
	write(t, filepath.Join(right, "file.txt"), "x", 0o644)
	if err := os.Symlink("file.txt", filepath.Join(right, "link")); err != nil {
		t.Skip("no symlinks here")
	}
	h := newHarness(t, left, right)
	paneOf := func(line string, i int) string {
		r := []rune(stripANSI(line))
		if len(r) < 120 {
			return "" // the status and key lines
		}
		if i == 0 {
			return string(r[:60])
		}
		return string(r[60:])
	}
	underlined := regexp.MustCompile(`\x1b\[([0-9;]*;)?4(;[0-9;]*)?m`)
	for _, name := range []string{"adir/", "file.txt", "link@"} {
		found := false
		for _, l := range strings.Split(h.model().View(), "\n") {
			if strings.Contains(paneOf(l, 0), "l.txt") && !strings.Contains(paneOf(l, 0), "›") {
				t.Errorf("active pane's cursor row %q has no ›", paneOf(l, 0))
			}
			r := paneOf(l, 1)
			if !strings.Contains(r, name) {
				continue
			}
			found = true
			if strings.Contains(r, "›") {
				t.Errorf("inactive pane's row %q has the active marker", r)
			}
			onCursor := h.cursorName(1)+suffixOf(name) == name
			if onCursor != underlined.MatchString(l) {
				t.Errorf("row %q: underlined %v with the cursor on %q", r, !onCursor, h.cursorName(1))
			}
		}
		if !found {
			t.Fatalf("%s not drawn", name)
		}
		h.keys("tab", "j", "tab")
	}
	top := stripANSI(strings.Split(h.model().View(), "\n")[0])
	if !strings.HasPrefix(top, "┏") || !strings.Contains(top, "╭") {
		t.Errorf("top border %q: want the active pane heavy (┏), the other light (╭)", top)
	}
	h.keys("tab")
	top = stripANSI(strings.Split(h.model().View(), "\n")[0])
	if !strings.HasPrefix(top, "╭") || !strings.Contains(top, "┏") {
		t.Errorf("after tab, top border %q: want the right pane heavy", top)
	}
}

func suffixOf(drawn string) string {
	if strings.HasSuffix(drawn, "/") || strings.HasSuffix(drawn, "@") {
		return drawn[len(drawn)-1:]
	}
	return ""
}

// Hiding dotfiles takes them out of the selection: what the title counts
// is what d acts on.
func TestHidingDotfilesDropsThemFromTheSelection(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	for _, n := range []string{".a", ".b", "c.txt"} {
		write(t, filepath.Join(left, n), "x", 0o644)
	}
	h := newHarness(t, left, right)
	h.keys(".", "*")
	if n := len(h.model().panes[0].sel); n != 3 {
		t.Fatalf("selected %d, want 3", n)
	}
	h.keys(".")
	if n := len(h.model().panes[0].sel); n != 1 {
		t.Errorf("after hiding, %d still selected: hidden ones would come back selected", n)
	}
	if !strings.Contains(viewText(h.model()), "[1 selected]") {
		t.Errorf("title doesn't say 1 selected:\n%s", viewText(h.model()))
	}
	h.keys("d")
	if q := h.model().confirmText; !strings.Contains(q, "c.txt") {
		t.Errorf("delete asks %q, want it to be about c.txt alone", q)
	}
	h.keys("y")
	for _, n := range []string{".a", ".b"} {
		if _, err := os.Stat(filepath.Join(left, n)); err != nil {
			t.Errorf("%s, hidden, was deleted", n)
		}
	}
}

// / finds a name as it's shown, so a name with characters Label drops is
// found by what's on screen.
func TestFindMatchesTheShownName(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	for _, n := range []string{"aaa", "bidi\u202etxt.exe", "zzz"} {
		write(t, filepath.Join(left, n), "x", 0o644)
	}
	h := newHarness(t, left, right)
	h.keys("/", "biditxt", "enter")
	if got := h.cursorName(0); got != "bidi\u202etxt.exe" {
		t.Errorf("cursor on %q", got)
	}
}

// Cancelling a write in place can't put the old file back: the screen and
// the log say the file may be partly written, not that it was left as it
// was. The first esc lets it finish (v0.23.1's promise); the second stops it.
func TestCancelledInPlaceSaysPartlyWritten(t *testing.T) {
	m, gate, events := startInPlace(t)
	m, _ = update(t, m, key("esc"))
	m, _ = update(t, m, key("esc"))
	close(gate)
	m = untilIdle(t, m)
	if !strings.Contains(m.status, "may be partly written") || strings.Contains(m.status, "left as it was") {
		t.Errorf("status = %q", m.status)
	}
	if last := (*events)[len(*events)-1]; !strings.Contains(last.detail, "may be partly written") || strings.Contains(last.detail, "discarded") {
		t.Errorf("log = %+v", last)
	}
}

// Dropping the connection during a write in place says the same.
func TestAbortedInPlaceSaysPartlyWritten(t *testing.T) {
	m, gate, events := startInPlace(t)
	defer close(gate)
	m, _ = update(t, m, key("esc"))
	m, _ = update(t, m, key("esc"))
	m, _ = update(t, m, key("esc"))
	if err := m.ExitError(); err == nil || !strings.Contains(err.Error(), "may be partly written") {
		t.Errorf("exit error = %v", err)
	}
	if last := (*events)[len(*events)-1]; !strings.Contains(last.detail, "may be partly written") {
		t.Errorf("log = %+v", last)
	}
}

// The first esc during a write in place lets it finish, and says so.
func TestFirstEscFinishesAWriteInPlace(t *testing.T) {
	m, gate, _ := startInPlace(t)
	m, _ = update(t, m, key("esc"))
	if v := m.viewStatus(); !strings.Contains(stripANSI(v), "esc again: stop now") {
		t.Errorf("status line = %q", stripANSI(v))
	}
	close(gate)
	m = untilIdle(t, m)
	if !strings.Contains(m.status, "written in place") || strings.Contains(m.status, "partly written") {
		t.Errorf("status = %q, want the file finished", m.status)
	}
}

// startInPlace starts writing big.bin in place in a read-only directory,
// with the copy held at a gate once the file has been emptied.
func startInPlace(t *testing.T) (Model, chan struct{}, *[]event) {
	t.Helper()
	ro := readOnlyDir(t, map[string]os.FileMode{"big.bin": 0o666})
	left := t.TempDir()
	write(t, filepath.Join(left, "big.bin"), strings.Repeat("n", 1<<20), 0o644)
	gate := make(chan struct{})
	events := &[]event{}
	m := New(Options{Left: gatedFS{open: gate}, Right: newRemote(t), LeftDir: left, RightDir: ro,
		OnEvent: func(e Event) {
			kind, detail, remote := e.Kind, e.Line(), e.Remote
			*events = append(*events, event{kind, detail, remote})
		}})
	clock := time.Now()
	m.now = func() time.Time { return clock }
	for i := 0; i < 2; i++ {
		m, _ = update(t, m, m.load(i, m.panes[i].dir)())
	}
	m, cmd := update(t, m, key("c"))
	m, _ = update(t, m, planOnly(cmd)())
	clock = clock.Add(time.Second)
	m, _ = update(t, m, key("o"))
	m = untilIdle(t, m)
	if m.confirmKind != confirmInPlace {
		t.Fatalf("mode %v kind %v status %q", m.mode, m.confirmKind, m.status)
	}
	clock = clock.Add(time.Second)
	m, _ = update(t, m, key("i"))
	for m.progress == nil || m.progress.InPlace == "" {
		select {
		case msg := <-m.events:
			m, _ = update(t, m, msg)
		case <-time.After(5 * time.Second):
			t.Fatal("the write in place never started")
		}
	}
	return m, gate, events
}
