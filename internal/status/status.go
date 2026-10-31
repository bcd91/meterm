// Package status says what meterm's running processes are holding open right
// now, so a status bar can show it without asking each of them.
//
// Each process that holds a connection keeps one small JSON file in a per-user
// runtime directory and removes it when it ends. There's no daemon: `meterm
// status` reads the directory, and drops the files of processes that are gone,
// which is what's left behind by a kill -9 or a crash.
//
// None of this may stand between anyone and a host, so a Registry that can't
// write just stops saying anything.
package status

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"meterm/internal/safefile"
)

// SchemaVersion is the version of the state files and of `status --format json`.
// It goes up when a field changes meaning; new fields don't need it to.
const SchemaVersion = 1

// Kinds of entry.
const (
	KindSession = "session"
	KindTunnel  = "tunnel"
)

// States an entry can be in. Connecting hasn't got in yet, reconnecting has and
// lost it, and failing is a tunnel that has kept failing.
const (
	Connecting   = "connecting"
	Connected    = "connected"
	Reconnecting = "reconnecting"
	Failing      = "failing"
)

// Entry is one connection a process holds. Detail is meterm's own short wording,
// never a command line or anything ssh said, so it's safe to show in a bar.
type Entry struct {
	Kind   string    `json:"kind"`
	Host   string    `json:"host"`
	Name   string    `json:"name"`            // session name, or the tunnels' names
	State  string    `json:"state"`           // one of the states above
	Route  string    `json:"route,omitempty"` // the route in use, once one is chosen
	Detail string    `json:"detail,omitempty"`
	Since  time.Time `json:"since"` // when it entered this state
}

// Row is an Entry with the process holding it, as `status` reports it.
type Row struct {
	Entry
	PID int `json:"pid"`
}

// file is what's on disk for one process.
type file struct {
	Schema    int      `json:"schema"`
	PID       int      `json:"pid"`
	ProcStart string   `json:"proc_start,omitempty"`
	Entries   []*Entry `json:"entries"`
}

// Dir is where the state files live: $METERM_RUNTIME_DIR if set (tests,
// alternate profiles), else $XDG_RUNTIME_DIR/meterm. Without one it's a directory
// of this user's own under the temp directory, which is per-user on Windows and
// checked by ensureDir everywhere else.
//
// macOS has its own answer, the same for every process of the user: a tunnel run by
// launchd and a `meterm status` typed in a terminal have to find the same place,
// and $TMPDIR and $XDG_RUNTIME_DIR can differ between those two (a nix shell, a
// tmux started elsewhere, a line in .zshrc).
func Dir() string {
	if d := os.Getenv("METERM_RUNTIME_DIR"); d != "" {
		return d
	}
	if runtime.GOOS == "darwin" {
		if d, err := os.UserCacheDir(); err == nil && homeIsMine() {
			return filepath.Join(d, "meterm", "run")
		}
		return filepath.Join(os.TempDir(), "meterm-"+strconv.Itoa(os.Getuid()))
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "meterm")
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(os.TempDir(), "meterm")
	}
	return filepath.Join(os.TempDir(), "meterm-"+strconv.Itoa(os.Getuid()))
}

// homeIsMine is false under sudo with HOME kept: state files of root's in that user's
// Caches would be in the way of that user's own, and they'd never be seen by status.
func homeIsMine() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	st, err := os.Stat(home)
	return err == nil && ownedByMe(st)
}

// ensureDir makes dir, and refuses one that someone else could have prepared:
// under a shared /tmp, the name alone proves nothing.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return checkDir(dir)
}

func checkDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || !ownedByMe(st) {
		return fmt.Errorf("%s isn't a directory of this user's", dir)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// Registry is this process's state file. A nil Registry is fine to use and does
// nothing, as are the Handles it gives out.
type Registry struct {
	mu     sync.Mutex
	dir    string
	file   file
	closed bool
	now    func() time.Time
}

// Open starts this process's state file, or returns nil if there's nowhere to
// keep one.
func Open() *Registry {
	dir := Dir()
	if ensureDir(dir) != nil {
		return nil
	}
	pid := os.Getpid()
	return &Registry{dir: dir, now: time.Now,
		file: file{Schema: SchemaVersion, PID: pid, ProcStart: procStart(pid)}}
}

// Close removes the file. It's safe to call more than once.
func (r *Registry) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		_ = safefile.Remove(r.path())
	}
}

func (r *Registry) path() string {
	return filepath.Join(r.dir, strconv.Itoa(r.file.PID)+".json")
}

// Add starts tracking a connection, in state connecting.
func (r *Registry) Add(kind, host, name string) *Handle {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := &Entry{Kind: kind, Host: host, Name: name, State: Connecting, Since: r.now().UTC()}
	r.file.Entries = append(r.file.Entries, e)
	r.save()
	return &Handle{r: r, e: e}
}

// save writes the file whole, through a rename so a reader never sees half of
// one. The caller holds mu.
func (r *Registry) save() {
	if r.closed {
		return
	}
	data, err := json.Marshal(r.file)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(r.dir, tempPrefix+"*")
	if errors.Is(err, os.ErrNotExist) && ensureDir(r.dir) == nil {
		// Caches get purged, and this process may run for months.
		f, err = os.CreateTemp(r.dir, tempPrefix+"*")
	}
	if err != nil {
		return
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr != nil || cerr != nil || safefile.Rename(f.Name(), r.path()) != nil {
		_ = os.Remove(f.Name())
	}
}

// Handle changes one entry.
type Handle struct {
	r *Registry
	e *Entry
}

// Set moves the entry to state, with detail saying why if there's anything to
// say. Since only changes along with the state.
func (h *Handle) Set(state, detail string) {
	if h == nil {
		return
	}
	h.r.mu.Lock()
	defer h.r.mu.Unlock()
	if h.e.State == state && h.e.Detail == detail {
		return
	}
	if h.e.State != state {
		h.e.Since = h.r.now().UTC()
	}
	h.e.State, h.e.Detail = state, detail
	h.r.save()
}

// SetRoute records the route the next attempt goes by.
func (h *Handle) SetRoute(route string) {
	if h == nil {
		return
	}
	h.r.mu.Lock()
	defer h.r.mu.Unlock()
	if h.e.Route != route {
		h.e.Route = route
		h.r.save()
	}
}

const (
	tempPrefix = ".tmp-"
	// staleTemp is when a temp file must be one a killed process left behind.
	staleTemp = time.Minute
)

// List reads the directory for every connection being held, dropping the files
// of processes that are gone. Files of a newer schema are left alone and not
// reported, and skipped says how many there were, so a mixed install can say so
// instead of showing fields it doesn't understand.
func List() (rows []Row, skipped int, err error) {
	dir := Dir()
	names, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if err := checkDir(dir); err != nil {
		return nil, 0, err
	}
	for _, de := range names {
		path := filepath.Join(dir, de.Name())
		if strings.HasPrefix(de.Name(), tempPrefix) {
			if st, serr := de.Info(); serr == nil && time.Since(st.ModTime()) > staleTemp {
				_ = os.Remove(path)
			}
			continue
		}
		pid, perr := strconv.Atoi(strings.TrimSuffix(de.Name(), ".json"))
		if perr != nil || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		data, rerr := safefile.ReadFile(path)
		if errors.Is(rerr, os.ErrNotExist) {
			continue // it ended between the listing and the read
		}
		if rerr != nil {
			return nil, 0, rerr
		}
		var f file
		if json.Unmarshal(data, &f) != nil || f.PID != pid {
			_ = os.Remove(path)
			continue
		}
		if !alive(f.PID, f.ProcStart) {
			_ = os.Remove(path)
			continue
		}
		if f.Schema > SchemaVersion {
			skipped++
			continue
		}
		for _, e := range f.Entries {
			if e != nil {
				rows = append(rows, Row{Entry: *e, PID: f.PID})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.PID < b.PID
	})
	return rows, skipped, nil
}

// alive says whether the process that wrote a file is still the one with that
// PID. A process's start time tells it from a later one given the same number,
// where the system says (Linux and macOS do, cheaply).
func alive(pid int, started string) bool {
	if !processExists(pid) {
		return false
	}
	now := procStart(pid)
	return started == "" || now == "" || started == now
}

// procStart is when the process started as the kernel counts it, or "" if that
// can't be read. comm can hold spaces and parentheses, so the fields are counted
// from the last ")".
func procStart(pid int) string {
	if runtime.GOOS == "darwin" {
		return darwinProcStart(pid)
	}
	if runtime.GOOS != "linux" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return ""
	}
	// After ")" come state (field 3) onwards; starttime is field 22.
	fields := strings.Fields(s[i+1:])
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

func fmtStart(sec, usec int64) string { return fmt.Sprintf("%d.%06d", sec, usec) }
