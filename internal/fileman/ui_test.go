package fileman

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

// Busy screens redraw on a timer; in tests the timer just needs to fire.
func init() { tickEvery = time.Millisecond }

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
	clock  time.Time // what the model sees as now; tests move it on
}

func newHarness(t *testing.T, left, right string) *harness {
	h := &harness{t: t, clock: time.Now()}
	remote := newRemote(t)
	model := New(Options{Left: Local{}, Right: remote, LeftDir: left, RightDir: right,
		OnEvent: func(e Event) {
			kind, detail, isRemote := e.Kind, e.Line(), e.Remote
			h.events = append(h.events, event{kind, detail, isRemote})
		}})
	model.now = func() time.Time { return h.clock }
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

// keys types ks a second apart, as a person would.
func (h *harness) keys(ks ...string) {
	for _, k := range ks {
		h.clock = h.clock.Add(time.Second)
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
	if len(h.events) != 2 || h.events[1].kind != "upload" || !h.events[1].remote || !strings.HasPrefix(h.events[1].detail, "ok: upload a.txt, c.txt") {
		t.Errorf("events = %+v", h.events)
	}
	if !strings.Contains(h.model().status, "copied 2 file(s)") {
		t.Errorf("status = %q", h.model().status)
	}

	// Pull a.txt back into a new local directory.
	os.Remove(filepath.Join(left, "a.txt"))
	h.keys("r", "tab", "c")
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
	h.keys("R")
	h.send(tea.KeyMsg{Type: tea.KeyCtrlU}) // clear the prefilled name
	h.keys("old-releases", "enter")
	if _, err := os.Stat(filepath.Join(right, "old-releases")); err != nil {
		t.Errorf("rename: %v (status %q)", err, h.model().status)
	}
	h.keys("R")
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
	h.clock = h.clock.Add(time.Second)                          // the prompt has been on screen a while
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
	for _, height := range []int{10, 12, 15, 24, 40} {
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

// gatedFS holds every read until open is closed, so a test can act while a
// copy is under way.
type gatedFS struct {
	Local
	open chan struct{}
}

func (g gatedFS) Open(p string) (io.ReadCloser, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return &gatedReader{File: f, open: g.open}, nil
}

type gatedReader struct {
	*os.File
	open chan struct{}
}

func (g *gatedReader) Read(p []byte) (int, error) {
	<-g.open
	if len(p) > 4096 {
		p = p[:4096]
	}
	return g.File.Read(p)
}

func key(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func update(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}

// untilIdle feeds the running transfer's messages to the model until it's
// done.
func untilIdle(t *testing.T, m Model) Model {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for m.mode == modeBusy {
		select {
		case msg := <-m.events:
			m, _ = update(t, m, msg)
		case <-deadline:
			t.Fatal("transfer never finished")
		}
	}
	return m
}

// Starts uploading big.bin over an existing one, through a gate the test
// opens when it likes.
func startGatedOverwrite(t *testing.T) (m Model, gate chan struct{}, right string, events *[]event) {
	t.Helper()
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "big.bin"), strings.Repeat("n", 1<<20), 0o644)
	write(t, filepath.Join(right, "big.bin"), "original", 0o644)
	gate = make(chan struct{})
	events = &[]event{}
	m = New(Options{Left: gatedFS{open: gate}, Right: newRemote(t), LeftDir: left, RightDir: right,
		OnEvent: func(e Event) {
			kind, detail, remote := e.Kind, e.Line(), e.Remote
			*events = append(*events, event{kind, detail, remote})
		}})
	clock := time.Now()
	m.now = func() time.Time { return clock }
	for i := 0; i < 2; i++ {
		msg := m.load(i, m.panes[i].dir)()
		m, _ = update(t, m, msg)
	}
	m, cmd := update(t, m, key("c"))
	m, _ = update(t, m, planOnly(cmd)()) // the plan
	if m.mode != modeConfirm {
		t.Fatalf("mode %v, want the overwrite question", m.mode)
	}
	clock = clock.Add(time.Second) // read it
	m, _ = update(t, m, key("o"))
	if m.mode != modeBusy {
		t.Fatalf("mode %v after o, want busy", m.mode)
	}
	// Wait until the temporary file is open and the copy is waiting on the gate.
	for m.progress == nil || m.progress.Temp == "" {
		select {
		case msg := <-m.events:
			m, _ = update(t, m, msg)
		case <-time.After(5 * time.Second):
			t.Fatal("copy never started")
		}
	}
	return m, gate, right, events
}

func TestEscCancelsACopyAndKeepsTheOriginal(t *testing.T) {
	m, gate, right, _ := startGatedOverwrite(t)
	m, _ = update(t, m, key("esc"))
	close(gate)
	m = untilIdle(t, m)
	if !strings.Contains(m.status, "cancelled") || strings.Contains(m.status, "failed") {
		t.Errorf("status = %q, want it to say cancelled", m.status)
	}
	if got, _ := os.ReadFile(filepath.Join(right, "big.bin")); string(got) != "original" {
		t.Errorf("original after cancel = %d bytes", len(got))
	}
}

func viewText(m Model) string { return stripANSI(m.View()) }

// A host that stops answering mid-copy: the read never returns and ignores
// the cancel, the way pkg/sftp waits on a silent peer. Esc must show that a
// cancel is under way, the line must say no data is arriving rather than an
// old average, and a second Esc or Ctrl-C must get the person out.
func TestStalledCopyCanBeLeft(t *testing.T) {
	for _, second := range []string{"esc", "ctrl+c"} {
		t.Run(second, func(t *testing.T) {
			m, gate, right, events := startGatedOverwrite(t)
			t.Cleanup(func() { close(gate) })
			aborted := 0
			m.opts.Abort = func() { aborted++ }
			start := m.clock

			m, next := update(t, m, tickMsg{id: m.tickID, at: start.Add(20 * time.Second)})
			if next == nil {
				t.Fatal("the redraw ticker stopped while busy: a stall would never show")
			}
			if tm, ok := next().(tickMsg); !ok || tm.id != m.tickID {
				t.Fatalf("ticker re-armed with %#v", tm)
			}
			v := viewText(m)
			if !strings.Contains(v, "no data for 20s") {
				t.Errorf("after 20s without a byte the view doesn't say so:\n%s", v)
			}
			if !strings.Contains(v, " 0 B/s") {
				t.Errorf("rate should have fallen to 0 B/s:\n%s", v)
			}

			m, cmd := update(t, m, key("ctrl+c"))
			if cmd != nil {
				if _, quit := cmd().(tea.QuitMsg); quit {
					t.Fatal("the first ctrl+c quit without trying to cancel")
				}
			}
			if v := viewText(m); !strings.Contains(v, "cancelling") {
				t.Errorf("cancelling… isn't visible:\n%s", v)
			}
			if m.mode != modeBusy || aborted != 0 {
				t.Fatalf("one press shouldn't drop the connection (mode %v, aborted %d)", m.mode, aborted)
			}

			m, cmd = update(t, m, key(second))
			if aborted != 1 {
				t.Errorf("second %s: Abort called %d times, want 1", second, aborted)
			}
			if cmd == nil {
				t.Fatalf("second %s did nothing", second)
			}
			if _, quit := cmd().(tea.QuitMsg); !quit {
				t.Errorf("second %s didn't quit", second)
			}
			if m.exitErr == nil || !strings.Contains(m.exitErr.Error(), "connection") {
				t.Errorf("exit error = %v, want it to say the connection was dropped", m.exitErr)
			}
			if got, _ := os.ReadFile(filepath.Join(right, "big.bin")); string(got) != "original" {
				t.Errorf("original after abort = %d bytes", len(got))
			}
			if len(*events) == 0 || !strings.Contains((*events)[len(*events)-1].detail, "abort") {
				t.Errorf("no activity log entry for the abort: %+v", *events)
			}
		})
	}
}

// The rate is over the last few seconds, not since the start: a copy that
// ran fast and then stalled must not keep showing the old speed.
func TestRateIsRecent(t *testing.T) {
	m := New(Options{Left: Local{}, Right: Local{}})
	t0 := time.Unix(1000, 0)
	m.mode, m.busy = modeBusy, busyCopy
	m.progress = &Progress{Total: 100 << 20, Started: t0, At: t0}
	m.clock, m.lastChange = t0, t0
	for i := 1; i <= 10; i++ { // 10 MB/s for 10 s
		m, _ = update(t, m, progressMsg(Progress{Done: int64(i) * 10 << 20, Total: 100 << 20, Started: t0, At: t0.Add(time.Duration(i) * time.Second)}))
	}
	if v := viewText(m); !strings.Contains(v, "10.0 MB/s") {
		t.Errorf("steady 10 MB/s not shown:\n%s", v)
	}
	m, _ = update(t, m, tickMsg{id: m.tickID, at: t0.Add(18 * time.Second)})
	if v := viewText(m); strings.Contains(v, "MB/s") || !strings.Contains(v, "no data for 8s") {
		t.Errorf("8 s after the last byte the line still shows a speed or no stall:\n%s", v)
	}
}

// blockingFS holds Lstat and Remove until open is closed, ignoring any
// cancel, like a slow or silent host.
type blockingFS struct {
	Local
	open    chan struct{}
	removed *[]string
}

func (b blockingFS) Lstat(p string) (Entry, error) {
	<-b.open
	return b.Local.Lstat(p)
}

func (b blockingFS) Remove(p string) error {
	<-b.open
	*b.removed = append(*b.removed, p)
	return b.Local.Remove(p)
}

// runAsync starts cmd the way Bubble Tea does, off the UI loop.
func runAsync(cmd tea.Cmd) chan tea.Msg {
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	return ch
}

func TestWorkingOutACopyCanBeCancelled(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "a", 0o644)
	gate := make(chan struct{})
	m := New(Options{Left: blockingFS{open: gate, removed: &[]string{}}, Right: Local{}, LeftDir: left, RightDir: right})
	for i := 0; i < 2; i++ {
		m, _ = update(t, m, m.load(i, m.panes[i].dir)())
	}
	m, cmd := update(t, m, key("c"))
	done := runAsync(planOnly(cmd))
	m, _ = update(t, m, key("esc"))
	close(gate)
	m, cmd = update(t, m, <-done)
	if cmd != nil {
		if msgs := drainCmd(cmd); len(msgs) > 0 {
			for _, msg := range msgs {
				m, _ = update(t, m, msg)
			}
		}
	}
	if m.mode != modeNormal || !strings.Contains(m.status, "cancelled") {
		t.Errorf("mode %v status %q, want the copy cancelled while it was being worked out", m.mode, m.status)
	}
	if _, err := os.Stat(filepath.Join(right, "a.txt")); err == nil {
		t.Error("copied after cancelling")
	}
}

func TestDeleteCanBeCancelled(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "a", 0o644)
	write(t, filepath.Join(left, "b.txt"), "b", 0o644)
	gate := make(chan struct{})
	var removed []string
	var events []event
	m := New(Options{Left: blockingFS{open: gate, removed: &removed}, Right: Local{}, LeftDir: left, RightDir: right,
		OnEvent: func(e Event) {
			kind, detail, remote := e.Kind, e.Line(), e.Remote
			events = append(events, event{kind, detail, remote})
		}})
	clock := time.Unix(1000, 0)
	m.now = func() time.Time { return clock }
	for i := 0; i < 2; i++ {
		m, _ = update(t, m, m.load(i, m.panes[i].dir)())
	}
	m, _ = update(t, m, key("*"))
	m, _ = update(t, m, key("d"))
	clock = clock.Add(time.Second)
	m, cmd := update(t, m, key("y"))
	if m.mode != modeBusy {
		t.Fatalf("mode %v after y, want busy deleting", m.mode)
	}
	done := runAsync(planOnly(cmd))
	m, _ = update(t, m, key("esc"))
	close(gate)
	m, _ = update(t, m, <-done)
	if m.mode != modeNormal || !strings.Contains(m.status, "cancelled") {
		t.Errorf("mode %v status %q, want the delete cancelled", m.mode, m.status)
	}
	if len(removed) > 1 {
		t.Errorf("removed %v after the cancel", removed)
	}
	if _, err := os.Stat(filepath.Join(left, "b.txt")); err != nil {
		t.Error("b.txt deleted after the cancel")
	}
}

// planOnly picks the long-running piece out of what Update returned (the
// plan or delete), leaving the redraw ticker out.
func planOnly(cmd tea.Cmd) tea.Cmd {
	return func() tea.Msg {
		for _, msg := range drainCmd(cmd) {
			if _, tick := msg.(tickMsg); !tick {
				return msg
			}
		}
		return nil
	}
}

// drainCmd runs cmd and any batch inside it, collecting the messages.
func drainCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	switch msg := cmd().(type) {
	case nil:
		return nil
	case tea.BatchMsg:
		var out []tea.Msg
		for _, c := range msg {
			out = append(out, drainCmd(c)...)
		}
		return out
	default:
		return []tea.Msg{msg}
	}
}

// failFS fails Remove and Open for any path containing bad.
type failFS struct {
	Local
	bad string
}

func (f failFS) Remove(p string) error {
	if strings.Contains(p, f.bad) {
		return fmt.Errorf("permission denied (test)")
	}
	return f.Local.Remove(p)
}

func (f failFS) Open(p string) (io.ReadCloser, error) {
	if strings.Contains(p, f.bad) {
		return nil, fmt.Errorf("permission denied (test)")
	}
	return f.Local.Open(p)
}

func newFailHarness(t *testing.T, left, right, bad string) *harness {
	h := &harness{t: t, clock: time.Now()}
	model := New(Options{Left: failFS{bad: bad}, Right: Local{}, LeftDir: left, RightDir: right,
		OnEvent: func(e Event) {
			kind, detail, isRemote := e.Kind, e.Line(), e.Remote
			h.events = append(h.events, event{kind, detail, isRemote})
		}})
	model.now = func() time.Time { return h.clock }
	h.m = model
	h.run(model.Init())
	h.send(tea.WindowSizeMsg{Width: 120, Height: 30})
	return h
}

// A delete that stops part way must log what it did delete, what it
// failed on and what it never reached, and say what it's about to do first.
func TestPartialDeleteIsLogged(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a-ok", "x.txt"), "a", 0o644)
	write(t, filepath.Join(left, "b", "1.txt"), "1", 0o644)
	write(t, filepath.Join(left, "b", "2.txt"), "2", 0o644)
	write(t, filepath.Join(left, "b", "zz-bad.txt"), "x", 0o644)
	write(t, filepath.Join(left, "c.txt"), "c", 0o644)
	h := newFailHarness(t, left, right, "zz-bad")
	h.keys("*", "d")
	h.clock = h.clock.Add(time.Second)
	h.keys("y")
	if len(h.events) < 2 {
		t.Fatalf("events = %+v, want the intent and the outcome", h.events)
	}
	intent, out := h.events[0], h.events[len(h.events)-1]
	if intent.kind != "delete" || !strings.HasPrefix(intent.detail, "started: delete") || !strings.Contains(intent.detail, "c.txt") {
		t.Errorf("intent = %+v", intent)
	}
	for _, want := range []string{"failed", "zz-bad", "deleted: " + filepath.Join(left, "a-ok"), "2 entries inside", "not reached: " + filepath.Join(left, "c.txt")} {
		if !strings.Contains(out.detail, want) {
			t.Errorf("outcome %q doesn't mention %q", out.detail, want)
		}
	}
	if _, err := os.Stat(filepath.Join(left, "a-ok")); err == nil {
		t.Error("a-ok should have gone before the failure")
	}
}

func TestFailedCopyIsLogged(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "a", 0o644)
	write(t, filepath.Join(left, "zz-bad.txt"), "x", 0o644)
	h := newFailHarness(t, left, right, "zz-bad")
	h.keys("*", "c")
	if !h.model().statusErr {
		t.Fatalf("status %q, want a failure", h.model().status)
	}
	if len(h.events) == 0 {
		t.Fatal("a failed copy left no activity log entry")
	}
	last := h.events[len(h.events)-1]
	for _, want := range []string{"failed", "zz-bad", "1 of 2"} {
		if !strings.Contains(last.detail, want) {
			t.Errorf("logged %q, want it to mention %q", last.detail, want)
		}
	}
}

func TestCancelledOverwriteIsLogged(t *testing.T) {
	m, gate, _, events := startGatedOverwrite(t)
	if len(*events) != 1 || (*events)[0].kind != "upload" || !(*events)[0].remote || !strings.HasPrefix((*events)[0].detail, "started: upload") || !strings.Contains((*events)[0].detail, "replacing 1") {
		t.Errorf("before an overwrite starts, events = %+v; want the intent logged", *events)
	}
	m, _ = update(t, m, key("esc"))
	close(gate)
	untilIdle(t, m)
	if last := (*events)[len(*events)-1]; !strings.Contains(last.detail, "cancelled") || !strings.Contains(last.detail, "0 of 1") {
		t.Errorf("after the cancel, last event = %+v", last)
	}
}

func TestRenameOntoAnExistingNameIsRefused(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(right, "a.txt"), "A", 0o644)
	write(t, filepath.Join(right, "b.txt"), "B", 0o644)
	h := newHarness(t, left, right)
	h.keys("tab", "R")
	h.send(tea.KeyMsg{Type: tea.KeyCtrlU})
	h.keys("b.txt", "enter")
	if !h.model().statusErr || !strings.Contains(h.model().status, "already exists") {
		t.Errorf("status %q, want a refusal", h.model().status)
	}
	if got, _ := os.ReadFile(filepath.Join(right, "b.txt")); string(got) != "B" {
		t.Errorf("b.txt = %q", got)
	}
}

var hostileNames = []string{
	"esc\x1b]0;fmpwn\x07name.txt",  // OSC: sets the terminal title
	"clear\x1b[2J\x1b[Hscreen.txt", // CSI: clears the screen
	"new\nline.txt",                // pushes the borders down a row
	"bell\x07and\rcr.txt",
	"rtl\u202etxt.exe",          // reads as "rtlexe.txt"
	"c1\u009b31mred.txt",        // 8-bit CSI
	"dir\x1b]52;c;cGFzdGU=\x07", // OSC 52: writes the clipboard
}

// assertSafe fails if view carries anything a terminal would act on other
// than the colour codes the UI itself draws with, or has more lines than
// the screen.
func assertSafe(t *testing.T, where, view string, height int) {
	t.Helper()
	rs := []rune(view)
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; {
		case r == 0x1b:
			j := i + 1
			if j >= len(rs) || rs[j] != '[' {
				t.Fatalf("%s: non-CSI escape at %d: %q", where, i, string(rs[i:min(i+20, len(rs))]))
			}
			for j++; j < len(rs) && (rs[j] >= '0' && rs[j] <= '9' || rs[j] == ';' || rs[j] == ':'); j++ {
			}
			if j >= len(rs) || rs[j] != 'm' {
				t.Fatalf("%s: escape other than colour at %d: %q", where, i, string(rs[i:min(i+20, len(rs))]))
			}
			i = j
		case r == '\n':
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069):
			t.Fatalf("%s: control character %U in view: %q", where, r, string(rs[max(0, i-20):min(i+20, len(rs))]))
		}
	}
	if n := strings.Count(view, "\n") + 1; n > height {
		t.Fatalf("%s: view is %d lines on a %d-line screen", where, n, height)
	}
}

// Names (and everything that quotes them) come from the host: a name must
// not be able to set the title, clear the screen, write the clipboard or
// add a row, wherever it's shown.
func TestHostileNamesAreShownSafely(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	for _, n := range hostileNames {
		write(t, filepath.Join(right, n), "x", 0o644)
		write(t, filepath.Join(left, n), "x", 0o644)
	}
	hostileDir := filepath.Join(right, "sub\x1b]0;pwn\x07dir")
	write(t, filepath.Join(hostileDir, "f.txt"), "x", 0o644)
	h := newHarness(t, left, right)
	const height = 30
	check := func(where string) { t.Helper(); assertSafe(t, where, h.m.View(), height) }

	check("listing")
	h.keys("tab")
	for i := 0; i < len(hostileNames)+1; i++ {
		check(fmt.Sprintf("cursor on %q", h.cursorName(1)))
		h.keys("d")
		check(fmt.Sprintf("delete prompt for %q", h.cursorName(1)))
		h.keys("n", "R")
		check(fmt.Sprintf("rename box for %q", h.cursorName(1)))
		h.keys("esc", "j")
	}
	// The directory itself in the title.
	h.keys("g", "l")
	if !strings.Contains(h.model().panes[1].dir, "pwn") {
		t.Fatalf("didn't enter the hostile directory: %s", h.model().panes[1].dir)
	}
	check("title")
	// An error that quotes a name: copy over a directory of the same name.
	os.Mkdir(filepath.Join(hostileDir, "clash\x1b]0;err\x07"), 0o755)
	write(t, filepath.Join(left, "clash\x1b]0;err\x07"), "x", 0o644)
	h.keys("r", "tab", "/", "clash", "enter", "c")
	check("overwrite prompt")
	h.keys("o")
	check("copy error")
	if !h.model().statusErr {
		t.Errorf("expected an error copying over a directory, status %q", h.model().status)
	}
	// A directory that can't be read: the error quotes its path, in the
	// pane's header.
	locked := filepath.Join(hostileDir, "locked\x1b]0;pwn\x07")
	os.Mkdir(locked, 0o000)
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	h.send(h.model().load(1, locked)())
	if h.model().panes[1].err == "" {
		t.Skip("can read a mode-000 directory (running as root?)")
	}
	check("unreadable directory")
	h.send(dirLoadedMsg{pane: 1, dir: "/x", err: fmt.Errorf("open /x\x1b]0;pwn\x07: permission denied")}) // short enough not to be cut
	check("unreadable directory, short path")
	// And the progress line names the file being copied.
	m := h.model()
	m.mode, m.busy = modeBusy, busyCopy
	m.progress = &Progress{File: filepath.Join(left, hostileNames[0]), Total: 10}
	assertSafe(t, "progress", m.View(), height)
}

// An error stays on the status line until it's dismissed (esc) or replaced,
// not wiped by the next cursor move.
func TestErrorStaysUntilDismissed(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "a", 0o644)
	write(t, filepath.Join(left, "zz-bad.txt"), "x", 0o644)
	h := newFailHarness(t, left, right, "zz-bad")
	h.keys("G", "c")
	if !h.model().statusErr {
		t.Fatalf("status %q, want an error", h.model().status)
	}
	h.keys("k", "j", "tab", "tab")
	if !h.model().statusErr || !strings.Contains(viewText(h.model()), "zz-bad") {
		t.Errorf("error gone after moving: status %q", h.model().status)
	}
	h.keys("esc")
	if h.model().status != "" {
		t.Errorf("esc didn't dismiss the error: %q", h.model().status)
	}
}

// "dy" typed as one burst, or y within a moment of the prompt appearing,
// is a slip, not a decision: the prompt stays and nothing is deleted.
func TestDeleteNeedsThePromptSeenFirst(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "x.txt"), "x", 0o644)
	h := newHarness(t, left, right)

	h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("dy")})
	if _, err := os.Stat(filepath.Join(left, "x.txt")); err != nil {
		t.Fatal("\"dy\" in one burst deleted")
	}
	if h.model().mode != modeConfirm {
		t.Fatalf("mode %v: the prompt should still be waiting", h.model().mode)
	}
	h.clock = h.clock.Add(50 * time.Millisecond)
	h.send(key("y"))
	if _, err := os.Stat(filepath.Join(left, "x.txt")); err != nil {
		t.Fatal("y 50ms after the prompt deleted")
	}
	h.clock = h.clock.Add(time.Second)
	h.send(key("y"))
	if _, err := os.Stat(filepath.Join(left, "x.txt")); !os.IsNotExist(err) {
		t.Error("y a second after the prompt didn't delete")
	}
}

func TestQuestionMarkShowsHelp(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	h := newHarness(t, left, right)
	for _, w := range []int{80, 120} {
		h.send(tea.WindowSizeMsg{Width: w, Height: 30})
		lines := strings.Split(viewText(h.model()), "\n")
		help := lines[len(lines)-1]
		if strings.HasSuffix(help, "…") || !strings.Contains(help, "? help") || !strings.Contains(help, "q quit") {
			t.Errorf("width %d: short help %q is cut or lacks ? and q", w, help)
		}
	}
	h.keys("?")
	v := viewText(h.model())
	for _, want := range []string{"copy", "delete", "rename", "new directory", "find", "hidden", "reload", "cancel"} {
		if !strings.Contains(v, want) {
			t.Errorf("full help lacks %q:\n%s", want, v)
		}
	}
	assertSafe(t, "help", h.m.View(), 30)
	h.keys("x") // j and k scroll it; another key closes it
	if strings.Contains(viewText(h.model()), "new directory") {
		t.Error("a key didn't close the help")
	}
	if h.model().mode != modeNormal {
		t.Errorf("mode %v after closing help", h.model().mode)
	}
}

// refWidth is the width a terminal using wcwidth (tmux, xterm, foot) gives
// the runes these tests use: combining marks 0, CJK and kana 2, the rest
// (Thai SARA AM included) 1.
func refWidth(s string) int {
	n := 0
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
		case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf, r >= 0xac00 && r <= 0xd7a3, r >= 0xff00 && r <= 0xff60:
			n += 2
		default:
			n++
		}
	}
	return n
}

// Thai with SARA AM (which grapheme-based width counts as nothing), CJK and
// combining accents must not push the right border out of line.
func TestWideAndThaiNamesKeepTheBorders(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	names := []string{"กำลังทดสอบ.txt", "น้ำ.txt", "ภาษาไทย.txt", "日本語ファイル.txt", "e\u0301cole.txt", "plain.txt",
		"น้ำ" + strings.Repeat("ยาว", 20) + ".txt", strings.Repeat("日本", 40) + ".txt"}
	for _, n := range names {
		write(t, filepath.Join(right, n), "x", 0o644)
	}
	write(t, filepath.Join(right, "ทดสอบ", "x"), "x", 0o644) // a Thai directory name, for the title
	h := newHarness(t, left, right)
	for _, w := range []int{60, 81, 120} {
		h.send(tea.WindowSizeMsg{Width: w, Height: 20})
		for _, step := range []string{"list", "into the Thai dir"} {
			if step == "into the Thai dir" {
				h.keys("tab", "g", "l")
			}
			lines := strings.Split(viewText(h.model()), "\n")
			for i, line := range lines[:len(lines)-2] { // the panes
				if got := refWidth(line); got != w {
					t.Errorf("width %d, %s: line %d is %d cells: %q", w, step, i, got, line)
				}
			}
			for _, line := range lines[len(lines)-2:] { // status and help
				if got := refWidth(line); got > w {
					t.Errorf("width %d, %s: %q is %d cells", w, step, line, got)
				}
			}
			if step == "into the Thai dir" {
				h.keys("h", "tab")
			}
		}
	}
}

func TestBackgroundFromColorFGBG(t *testing.T) {
	for env, dark := range map[string]bool{"": true, "15;0": true, "0;15": false, "0;7": false, "7;default;0": true, "12;8": true, "junk": true, "0;default;15": false} {
		if got := darkBackground(env); got != dark {
			t.Errorf("COLORFGBG=%q: dark %v, want %v", env, got, dark)
		}
	}
}

func TestOverwriteNeedsThePromptSeenFirst(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "new", 0o644)
	write(t, filepath.Join(right, "a.txt"), "old", 0o644)
	h := newHarness(t, left, right)
	h.keys("c")
	h.send(key("o")) // the same instant the question appeared
	if got, _ := os.ReadFile(filepath.Join(right, "a.txt")); string(got) != "old" || h.model().mode != modeConfirm {
		t.Fatalf("o at once: a.txt = %q, mode %v; want the question still up", got, h.model().mode)
	}
	h.keys("o")
	if got, _ := os.ReadFile(filepath.Join(right, "a.txt")); string(got) != "new" {
		t.Errorf("o after a second: a.txt = %q", got)
	}
}

// A host that won't set permissions still gets the copy, and the person is
// told, on screen and in the log.
func TestPermissionWarningIsShownAndLogged(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "a", 0o644)
	h := &harness{t: t, clock: time.Now()}
	model := New(Options{Left: Local{}, Right: chmodFailFS{}, LeftDir: left, RightDir: right,
		OnEvent: func(e Event) {
			kind, detail, isRemote := e.Kind, e.Line(), e.Remote
			h.events = append(h.events, event{kind, detail, isRemote})
		}})
	model.now = func() time.Time { return h.clock }
	h.m = model
	h.run(model.Init())
	h.keys("c")
	if got, _ := os.ReadFile(filepath.Join(right, "a.txt")); string(got) != "a" {
		t.Fatalf("not copied: %q (status %q)", got, h.model().status)
	}
	if st := h.model().status; !strings.Contains(st, "permissions not set") || !h.model().statusErr {
		t.Errorf("status %q (err %v), want the permission warning, kept on screen", st, h.model().statusErr)
	}
	if len(h.events) == 0 || !strings.Contains(h.events[len(h.events)-1].detail, "permissions not set") {
		t.Errorf("events = %+v, want the warning logged", h.events)
	}
}

// A refused rename must leave the cursor where it was: moving it to the
// existing file would make the next key (d, R, c) act on a different file
// than the one the person was looking at.
func TestRefusedRenameKeepsTheCursor(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(right, "a.txt"), "A", 0o644)
	write(t, filepath.Join(right, "b.txt"), "B", 0o644)
	h := newHarness(t, left, right)
	h.keys("tab", "R")
	h.send(tea.KeyMsg{Type: tea.KeyCtrlU})
	h.keys("b.txt", "enter")
	if e, ok := h.model().panes[1].current(); !ok || e.Name != "a.txt" {
		t.Fatalf("cursor on %q after a refused rename, want a.txt", e.Name)
	}
	h.keys("R")
	h.send(tea.KeyMsg{Type: tea.KeyCtrlU})
	h.keys("c.txt", "enter")
	if got, _ := os.ReadFile(filepath.Join(right, "c.txt")); string(got) != "A" {
		t.Errorf("c.txt = %q, want a.txt's content", got)
	}
	if e, ok := h.model().panes[1].current(); !ok || e.Name != "c.txt" {
		t.Errorf("cursor on %q after a rename, want c.txt", e.Name)
	}
}
