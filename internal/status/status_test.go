package status

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func tempDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "run")
	t.Setenv("METERM_RUNTIME_DIR", dir)
	return dir
}

func TestRegistryShowsAndForgets(t *testing.T) {
	dir := tempDir(t)
	r := Open()
	if r == nil {
		t.Fatal("no registry")
	}
	s := r.Add(KindSession, "box", "api")
	tun := r.Add(KindTunnel, "box", "db,web")
	s.SetRoute("office")
	s.Set(Connected, "")
	tun.Set(Failing, "the host refused every key")

	rows, skipped, err := List()
	if err != nil || skipped != 0 || len(rows) != 2 {
		t.Fatalf("List = %v, %d, %v", rows, skipped, err)
	}
	if got := rows[0]; got.Kind != KindSession || got.State != Connected || got.Route != "office" || got.PID != os.Getpid() {
		t.Errorf("session row = %+v", got)
	}
	if got := rows[1]; got.State != Failing || got.Detail == "" {
		t.Errorf("tunnel row = %+v", got)
	}
	r.Close()
	r.Close()
	if rows, _, _ := List(); len(rows) != 0 {
		t.Errorf("after Close: %v", rows)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestNilRegistryDoesNothing(t *testing.T) {
	var r *Registry
	h := r.Add(KindSession, "box", "api")
	h.Set(Connected, "")
	h.SetRoute("x")
	r.Close()
}

func TestNowhereToWriteStillWorks(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METERM_RUNTIME_DIR", filepath.Join(blocker, "run"))
	r := Open()
	if r != nil {
		t.Fatal("expected no registry")
	}
	r.Add(KindSession, "box", "api").Set(Connected, "")
	r.Close()
}

func TestSinceOnlyMovesWithTheState(t *testing.T) {
	tempDir(t)
	r := Open()
	defer r.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	h := r.Add(KindSession, "box", "api")
	now = now.Add(time.Minute)
	h.Set(Reconnecting, "woke from sleep")
	first := h.e.Since
	now = now.Add(time.Minute)
	h.Set(Reconnecting, "the network changed")
	if !h.e.Since.Equal(first) || h.e.Detail != "the network changed" {
		t.Errorf("since = %v, detail = %q", h.e.Since, h.e.Detail)
	}
	h.Set(Connected, "")
	if !h.e.Since.Equal(now) {
		t.Errorf("since = %v, want %v", h.e.Since, now)
	}
}

// A process killed outright leaves its file; the next look removes it.
func TestDeadProcessIsDropped(t *testing.T) {
	dir := tempDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	dead := cmd.Process.Pid
	b, _ := json.Marshal(file{Schema: SchemaVersion, PID: dead, Entries: []*Entry{{Kind: KindSession, Host: "gone", State: Connected}}})
	path := filepath.Join(dir, strconv.Itoa(dead)+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	rows, _, err := List()
	if err != nil || len(rows) != 0 {
		t.Fatalf("List = %v, %v", rows, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still there: %v", err)
	}
}

// The same PID given to a later process isn't the one that wrote the file.
func TestReusedPIDIsDropped(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("start times are only read on Linux")
	}
	dir := tempDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	b, _ := json.Marshal(file{Schema: SchemaVersion, PID: pid, ProcStart: "1", Entries: []*Entry{{Kind: KindSession, Host: "old"}}})
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if rows, _, _ := List(); len(rows) != 0 {
		t.Errorf("rows = %v", rows)
	}
}

func TestNewerSchemaIsCountedNotShown(t *testing.T) {
	dir := tempDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	b, _ := json.Marshal(file{Schema: SchemaVersion + 1, PID: pid, ProcStart: procStart(pid), Entries: []*Entry{{Kind: KindSession, Host: "future"}}})
	path := filepath.Join(dir, strconv.Itoa(pid)+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	rows, skipped, err := List()
	if err != nil || len(rows) != 0 || skipped != 1 {
		t.Fatalf("List = %v, %d, %v", rows, skipped, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a newer process's file was removed: %v", err)
	}
}

func TestStrayFilesAreLeftOrCleaned(t *testing.T) {
	dir := tempDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, tempPrefix+"old")
	fresh := filepath.Join(dir, tempPrefix+"fresh")
	broken := filepath.Join(dir, "12345.json")
	other := filepath.Join(dir, "notes.txt")
	for _, p := range []string{old, fresh, other} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(broken, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, long, long); err != nil {
		t.Fatal(err)
	}
	if rows, _, err := List(); err != nil || len(rows) != 0 {
		t.Fatalf("List = %v, %v", rows, err)
	}
	for p, want := range map[string]bool{old: false, fresh: true, broken: false, other: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s: exists = %v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}

func TestDirElsewhereMissingIsEmpty(t *testing.T) {
	tempDir(t)
	if rows, skipped, err := List(); err != nil || rows != nil || skipped != 0 {
		t.Errorf("List = %v, %d, %v", rows, skipped, err)
	}
}

func TestRefusesASymlinkedDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	base := t.TempDir()
	real := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "run")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METERM_RUNTIME_DIR", link)
	if r := Open(); r != nil {
		t.Error("opened a registry through a symlink")
	}
	if _, _, err := List(); err == nil || !strings.Contains(err.Error(), "isn't a directory") {
		t.Errorf("List err = %v", err)
	}
}

func TestDirNarrowsWidePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no such modes on Windows")
	}
	dir := tempDir(t)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r := Open()
	defer r.Close()
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	r.Add(KindSession, "box", "api")
	if st, _ := os.Stat(r.path()); st.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v", st.Mode().Perm())
	}
}

// A reader never sees half a file, however often it's rewritten.
func TestReadersSeeWholeFiles(t *testing.T) {
	tempDir(t)
	r := Open()
	defer r.Close()
	h := r.Add(KindSession, "box", "api")
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		states := []string{Connected, Reconnecting}
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
				h.Set(states[i%2], strconv.Itoa(i))
			}
		}
	}()
	for i := 0; i < 500; i++ {
		rows, _, err := List()
		if err != nil || len(rows) != 1 {
			close(done)
			wg.Wait()
			t.Fatalf("read %d: %v, %v", i, rows, err)
		}
	}
	close(done)
	wg.Wait()
}

func TestProcStartTellsProcessesApart(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no start time here")
	}
	me := procStart(os.Getpid())
	if me == "" || me != procStart(os.Getpid()) {
		t.Fatalf("own start time: %q", me)
	}
	if !alive(os.Getpid(), me) {
		t.Error("alive says no to this process")
	}
	if alive(os.Getpid(), "1.000000") {
		t.Error("a later process with the same PID passed as the one that wrote the file")
	}
}

// A process started by launchd and one started in a terminal have different
// surroundings, and still have to find each other.
func TestDirOnMacIgnoresWhatDiffersBetweenProcesses(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("METERM_RUNTIME_DIR", "")
	want := filepath.Join(home, "Library", "Caches", "meterm", "run")
	for _, env := range [][2]string{
		{"", ""},
		{"/var/folders/xx/T/", ""},
		{"/" + strings.Repeat("long/", 60), ""},
		{"/tmp", "/run/user/501"},
	} {
		t.Setenv("TMPDIR", env[0])
		t.Setenv("XDG_RUNTIME_DIR", env[1])
		if got := Dir(); got != want {
			t.Errorf("TMPDIR=%q XDG_RUNTIME_DIR=%q: %s", env[0], env[1], got)
		}
	}
}

func TestDirOnMacUnderSudoLeavesThatHomeAlone(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getuid() == 0 {
		t.Skip("macOS, not as root")
	}
	t.Setenv("METERM_RUNTIME_DIR", "")
	t.Setenv("HOME", "/") // owned by someone else, as a user's home is to root
	if got, want := Dir(), filepath.Join(os.TempDir(), "meterm-"+strconv.Itoa(os.Getuid())); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// Caches get purged while a tunnel runs for months.
func TestSaveRecreatesAPurgedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	t.Setenv("METERM_RUNTIME_DIR", dir)
	r := Open()
	defer r.Close()
	h := r.Add(KindTunnel, "box", "web")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	h.Set(Connected, "")
	rows, _, err := List()
	if err != nil || len(rows) != 1 || rows[0].State != Connected {
		t.Errorf("rows %+v, %v", rows, err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("%v %v", st, err)
	}
}
