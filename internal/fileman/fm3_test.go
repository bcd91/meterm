package fileman

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"meterm/internal/termsafe"
)

// fakeRenames is a server's rename and remove over a map of files, with a
// rename that refuses an existing target (as OpenSSH's does) and fails
// for the paths in fail.
type fakeRenames struct {
	files map[string]string
	fail  map[string]bool // rename from these fails
}

func (f *fakeRenames) rename(a, b string) error {
	if f.fail[a] {
		return errors.New("rename: failure (test)")
	}
	if _, ok := f.files[a]; !ok {
		return fs.ErrNotExist
	}
	if _, ok := f.files[b]; ok {
		return fs.ErrExist
	}
	f.files[b] = f.files[a]
	delete(f.files, a)
	return nil
}

func (f *fakeRenames) remove(p string) error {
	if _, ok := f.files[p]; !ok {
		return fs.ErrNotExist
	}
	delete(f.files, p)
	return nil
}

// Without posix-rename, a replace that fails part way must never leave
// neither file: the old one is put back, or, if even that fails, both
// copies are kept and the error says where.
func TestReplaceWithoutPosixRenameNeverLosesBoth(t *testing.T) {
	f := &fakeRenames{files: map[string]string{"/d/a.txt": "old", "/d/.a.txt.part": "new"}, fail: map[string]bool{"/d/.a.txt.part": true}}
	if err := replaceByRenames(f.rename, f.remove, nil, "/d/.a.txt.part", "/d/a.txt"); err == nil {
		t.Fatal("a failed rename reported success")
	}
	if f.files["/d/a.txt"] != "old" {
		t.Fatalf("after a failed replace: %v; want the old a.txt back", f.files)
	}
	if len(f.files) != 2 {
		t.Errorf("files %v: want a.txt and the temporary file, nothing else", f.files)
	}

	// The temporary file and the restore both fail: the old file is under
	// its backup name and the error says so, and names the new copy.
	f = &fakeRenames{files: map[string]string{"/d/a.txt": "old", "/d/.a.txt.part": "new"}, fail: map[string]bool{"/d/.a.txt.part": true}}
	rename := func(a, b string) error {
		if b == "/d/a.txt" && strings.Contains(a, ".old") {
			return errors.New("restore: failure (test)")
		}
		return f.rename(a, b)
	}
	err := replaceByRenames(rename, f.remove, nil, "/d/.a.txt.part", "/d/a.txt")
	var lost *ReplaceStuck
	if !errors.As(err, &lost) {
		t.Fatalf("err = %v, want a *ReplaceStuck", err)
	}
	if f.files[lost.Backup] != "old" || f.files["/d/.a.txt.part"] != "new" {
		t.Fatalf("files %v, backup %q: want the old one under the backup and the new one kept", f.files, lost.Backup)
	}
	for _, want := range []string{lost.Backup, "/d/.a.txt.part", "/d/a.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't name %s", err, want)
		}
	}

	// It worked: the new file is in place and the backup is gone.
	f = &fakeRenames{files: map[string]string{"/d/a.txt": "old", "/d/.a.txt.part": "new"}}
	if err := replaceByRenames(f.rename, f.remove, nil, "/d/.a.txt.part", "/d/a.txt"); err != nil {
		t.Fatal(err)
	}
	if f.files["/d/a.txt"] != "new" || len(f.files) != 1 {
		t.Errorf("files %v: want only the new a.txt", f.files)
	}
	// Nothing there yet: a plain rename.
	f = &fakeRenames{files: map[string]string{"/d/.b.part": "new"}}
	if err := replaceByRenames(f.rename, f.remove, nil, "/d/.b.part", "/d/b"); err != nil || f.files["/d/b"] != "new" {
		t.Errorf("err %v files %v", err, f.files)
	}
}

// stuckFS is a host whose replace failed half way, as ReplaceStuck says.
type stuckFS struct{ Local }

func (stuckFS) Replace(a, b string) error {
	return &ReplaceStuck{Target: b, Backup: b + ".bak", Temp: a, Err: errors.New("failure (test)"), RestoreErr: errors.New("failure (test)")}
}

// When a replace is stuck, the copy must not remove its temporary file:
// it may be the only copy of the new bytes, next to a target that's gone.
func TestStuckReplaceKeepsTheTemporaryFile(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "a.txt"), "new", 0o644)
	write(t, filepath.Join(dst, "a.txt"), "old", 0o644)
	plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "a.txt")}, stuckFS{}, dst)
	_, err := Run(context.Background(), plan, true, nil)
	var lost *ReplaceStuck
	if !errors.As(err, &lost) {
		t.Fatalf("err = %v, want *ReplaceStuck", err)
	}
	if got, rerr := os.ReadFile(lost.Temp); rerr != nil || string(got) != "new" {
		t.Errorf("temporary file %s: %q, %v; want it kept", lost.Temp, got, rerr)
	}
}

// OpenSSH's sftp-server, made to replace the way a server without
// posix-rename does: the copy lands and nothing is left behind.
func TestReplaceByRenamesOnOpenSSH(t *testing.T) {
	r := newSftpServerRemote(t)
	r.noPosixRename = true
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "a.txt"), "new", 0o644)
	write(t, filepath.Join(dst, "a.txt"), "old", 0o644)
	plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "a.txt")}, r, dst)
	if _, err := Run(context.Background(), plan, true, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(got) != "new" {
		t.Errorf("a.txt = %q", got)
	}
	if names := dirNames(t, dst); len(names) != 1 {
		t.Errorf("left behind: %v", names)
	}
}

// writeWatchFS records every open of a file for writing.
type writeWatchFS struct {
	partialWriteFS
	opened *[]string
}

func (w writeWatchFS) OpenWrite(p string) (io.WriteCloser, error) {
	*w.opened = append(*w.opened, p)
	return w.partialWriteFS.OpenWrite(p)
}

// Working out whether a file in a read-only directory can be written in
// place must not open it for writing: that alone wakes whatever watches it
// (inotify IN_CLOSE_WRITE: nginx and systemd path units reload).
func TestNoWriteOpenBeforeAgreeing(t *testing.T) {
	var opened []string
	ro := t.TempDir()
	write(t, filepath.Join(ro, "index.html"), "old", 0o644)
	src := t.TempDir()
	write(t, filepath.Join(src, "index.html"), "new", 0o644)
	dstFS := writeWatchFS{opened: &opened}
	plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "index.html")}, dstFS, ro)
	_, err := RunWith(context.Background(), plan, RunOptions{Overwrite: true}, nil)
	var need *InPlaceNeeded
	if !errors.As(err, &need) {
		t.Fatalf("err = %v, want the in-place question", err)
	}
	if len(opened) != 0 {
		t.Errorf("opened for writing before anyone agreed: %v", opened)
	}
}

// fixedModeFS creates temporary files 0644 whatever it's asked, and
// refuses every chmod, as a server with no setstat does.
type fixedModeFS struct{ Local }

func (fixedModeFS) CreateTemp(target string, mode fs.FileMode) (io.WriteCloser, string, error) {
	f, tmp, err := Local{}.CreateTemp(target, 0o644)
	if err != nil {
		return nil, "", err
	}
	os.Chmod(tmp, 0o644)
	return noChmod{f.(*os.File)}, tmp, &PermError{Err: errors.New("setstat: operation unsupported (test)")}
}

// A replacement that can't be given the old file's stricter mode doesn't
// replace it: a 0600 secret mustn't become 0644.
func TestStricterModeIsNeverLoosened(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "secret"), "new", 0o644)
	write(t, filepath.Join(dst, "secret"), "old", 0o600)
	plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "secret")}, fixedModeFS{}, dst)
	res, err := Run(context.Background(), plan, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "secret")); string(got) != "old" {
		t.Errorf("secret = %q, want the original kept", got)
	}
	if st, _ := os.Stat(filepath.Join(dst, "secret")); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	if res.Copied != 0 || res.Skipped != 1 || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "couldn't keep permissions 0600; left the original") {
		t.Errorf("result %+v", res)
	}
	if names := dirNames(t, dst); len(names) != 1 {
		t.Errorf("left behind: %v", names)
	}
}

// m on a name that's already a directory says so and logs nothing: it
// didn't create anything. A mkdir or rename that fails is logged as failed.
func TestMkdirAndRenameLogWhatHappened(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	os.Mkdir(filepath.Join(right, "deep"), 0o755)
	write(t, filepath.Join(right, "a.txt"), "A", 0o644)
	write(t, filepath.Join(right, "b.txt"), "B", 0o644)
	h := newHarness(t, left, right)
	h.keys("tab", "m", "deep", "enter")
	if st := h.model().status; !strings.Contains(st, "already exists") || strings.HasPrefix(st, "created") {
		t.Errorf("status %q, want it to say deep already exists", st)
	}
	if len(h.events) != 0 {
		t.Errorf("logged %q for a directory that was already there", h.allEvents())
	}

	h.keys("esc", "m", "a.txt", "enter") // a file has the name
	if !h.model().statusErr || len(h.events) != 1 || h.events[0].kind != "mkdir" || !strings.Contains(h.events[0].detail, "failed") ||
		!strings.Contains(h.events[0].detail, filepath.Join(right, "a.txt")) {
		t.Errorf("status %q, log %q: want a failed mkdir, logged", h.model().status, h.allEvents())
	}

	h.events = nil
	h.keys("esc", "g", "j", "R") // the cursor on a.txt
	h.send(tea.KeyMsg{Type: tea.KeyCtrlU})
	h.keys("b.txt", "enter")
	if len(h.events) != 1 || h.events[0].kind != "rename" || !strings.Contains(h.events[0].detail, "failed") ||
		!strings.Contains(h.events[0].detail, filepath.Join(right, "a.txt")+" -> "+filepath.Join(right, "b.txt")) {
		t.Errorf("log %q: want the refused rename, logged as failed", h.allEvents())
	}
}

// A delete on a host that stops answering can be left with esc twice: the
// connection is dropped, and the log says the delete may be partly done,
// naming what it was deleting.
func TestAbortedDeleteIsLogged(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(right, "a.txt"), "a", 0o644)
	gate := make(chan struct{})
	defer close(gate)
	var mu sync.Mutex // the delete logs from its own goroutine
	var events []event
	aborted := false
	m := New(Options{Left: Local{}, Right: blockingFS{open: gate, removed: &[]string{}}, LeftDir: left, RightDir: right,
		Abort: func() { aborted = true },
		OnEvent: func(kind, detail string, remote bool) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, event{kind, detail, remote})
		}})
	clock := time.Unix(1000, 0)
	m.now = func() time.Time { return clock }
	m, _ = update(t, m, m.load(0, m.panes[0].dir)())
	m, _ = update(t, m, dirLoadedMsg{pane: 1, dir: right, entries: []Entry{{Name: "a.txt", Size: 1}}})
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyTab})
	m, _ = update(t, m, key("d"))
	clock = clock.Add(time.Second)
	m, cmd := update(t, m, key("y"))
	if m.mode != modeBusy {
		t.Fatalf("mode %v after y, want busy deleting", m.mode)
	}
	_ = runAsync(planOnly(cmd)) // stuck on the silent host
	m, _ = update(t, m, key("esc"))
	_, cmd = update(t, m, key("esc"))
	if !aborted || cmd == nil {
		t.Fatalf("second esc: aborted %v, cmd %v; want the connection dropped and a quit", aborted, cmd)
	}
	mu.Lock()
	defer mu.Unlock()
	var got *event
	for i := range events {
		if events[i].kind == "delete" && strings.HasPrefix(events[i].detail, "aborted") {
			got = &events[i]
		}
	}
	if got == nil || !got.remote || !strings.HasPrefix(got.detail, "aborted: delete "+filepath.Join(right, "a.txt")+": ") || !strings.Contains(got.detail, "may be partly deleted") {
		t.Errorf("events %+v: want the aborted delete logged on the host, naming a.txt", events)
	}
}

// The delete question says what's selected: a file, a directory and
// everything in it, or how many of each.
func TestDeleteQuestionSaysWhatIsSelected(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(left, "a.txt"), "a", 0o644)
	write(t, filepath.Join(left, "b.txt"), "b", 0o644)
	write(t, filepath.Join(left, "dir", "x"), "x", 0o644)
	h := newHarness(t, left, right)
	ask := func(keys ...string) string {
		h.keys(keys...)
		q := h.model().confirmText
		h.keys("n") // no
		return q
	}
	for _, c := range []struct {
		keys    []string
		want    string
		notWant string
	}{
		{[]string{"g", "d"}, "Delete the directory dir and everything inside it in local ", ""},
		{[]string{"g", "j", "d"}, "Delete the file a.txt in local ", "inside"},
		{[]string{"g", "j", "space", "space", "d"}, "Delete 2 files in local ", "inside"},
		{[]string{"esc", "*", "d"}, "Delete 2 files and 1 directory (with everything inside it) in local ", ""},
	} {
		q := ask(c.keys...)
		if !strings.Contains(q, c.want) || c.notWant != "" && strings.Contains(q, c.notWant) {
			t.Errorf("keys %v: question %q, want %q", c.keys, q, c.want)
		}
		h.keys("esc", "esc")
	}
}

// While something runs, the key line shows the keys that work then, not
// q, which doesn't.
func TestKeyLineShowsTheKeysThatWork(t *testing.T) {
	m, gate, _, _ := startGatedOverwrite(t)
	defer close(gate)
	lines := strings.Split(viewText(m), "\n")
	last := lines[len(lines)-1]
	if strings.Contains(last, "q quit") || !strings.Contains(last, "esc or ctrl+c cancel") {
		t.Errorf("key line during a copy = %q", last)
	}
}

// tmuxWidth is how tmux 3.5a draws text: every emoji two cells, joiners
// or not (it doesn't join them), everything else as refWidth.
func tmuxWidth(s string) int {
	n := 0
	for _, r := range s {
		switch {
		case r == 0x200d:
		case r >= 0x1f300 && r <= 0x1faff:
			n += 2
		default:
			n += refWidth(string(r))
		}
	}
	return n
}

// A host's file named with an emoji ZWJ sequence (the family emoji: man,
// woman, girl and boy joined by U+200D), which tmux 3.5a draws as four
// emoji, eight cells, where a joining terminal draws two, must not push
// the pane's border out of line. File rows go through termsafe.Label,
// which is to take the joiners out, so every terminal draws the emoji one
// by one at the width cellwidth counts.
func TestZWJNameKeepsTheBorders(t *testing.T) {
	family := "\U0001F468\u200d\U0001F469\u200d\U0001F467\u200d\U0001F466"
	if strings.Contains(termsafe.Label(family), "\u200d") {
		t.Skip("needs termsafe.Label to drop U+200D in emoji sequences (the TUI agent's termsafe/cellwidth change); not merged here yet")
	}
	left, right := t.TempDir(), t.TempDir()
	write(t, filepath.Join(right, family+".txt"), "x", 0o644)
	write(t, filepath.Join(right, "a"+family+"b"+strings.Repeat("x", 80)+".txt"), "x", 0o644)
	h := newHarness(t, left, right)
	h.keys("tab")
	for _, w := range []int{60, 81, 120} {
		h.send(tea.WindowSizeMsg{Width: w, Height: 20})
		lines := strings.Split(viewText(h.model()), "\n")
		drawn := false
		for i, line := range lines[:len(lines)-2] {
			if strings.Contains(line, "\U0001F468") {
				drawn = true
			}
			if strings.Contains(line, "\u200d") {
				t.Errorf("width %d: line %d still has a joiner: %q", w, i, line)
			}
			if got := tmuxWidth(line); got != w {
				t.Errorf("width %d: line %d is %d cells in tmux: %q", w, i, got, line)
			}
		}
		if !drawn {
			t.Errorf("width %d: the emoji name isn't drawn", w)
		}
	}
}
