// Package fileman is meterm's two-pane file manager: this machine on one
// side, a saved host on the other, files copied between them.
//
// The remote side speaks SFTP over the system `ssh` binary (`ssh -s host
// sftp`), so jump hosts, keys, the agent and known_hosts work exactly as they
// do for `meterm connect`; this package only sees the resulting pipe.
package fileman

import (
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pkg/sftp"
)

// Entry is one item in a directory listing.
type Entry struct {
	Name    string
	Size    int64
	Mode    fs.FileMode
	ModTime time.Time
	IsDir   bool // true for directories and for symlinks pointing at one
	Link    bool // the entry itself is a symlink
}

// FS is one side of the file manager.
type FS interface {
	Label() string // "local" or the host name
	Home() (string, error)
	ReadDir(dir string) ([]Entry, error)
	Stat(p string) (Entry, error)  // follows symlinks
	Lstat(p string) (Entry, error) // doesn't
	MkdirAll(p string) error
	Remove(p string) error // a file, or an empty directory
	Rename(from, to string) error
	Open(p string) (io.ReadCloser, error)
	Create(p string, mode fs.FileMode) (io.WriteCloser, error)
	Join(elem ...string) string
	Dir(p string) string
	Base(p string) string
}

func entryOf(name string, fi fs.FileInfo) Entry {
	return Entry{Name: name, Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime(), IsDir: fi.IsDir(), Link: fi.Mode()&fs.ModeSymlink != 0}
}

// sortEntries puts directories first, then names case-insensitively, the
// order every file manager uses.
func sortEntries(es []Entry) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].IsDir != es[j].IsDir {
			return es[i].IsDir
		}
		return strings.ToLower(es[i].Name) < strings.ToLower(es[j].Name)
	})
}

// ---- local -----------------------------------------------------------------

// Local is this machine's filesystem.
type Local struct{}

func (Local) Label() string           { return "local" }
func (Local) Home() (string, error)   { return os.UserHomeDir() }
func (Local) Join(e ...string) string { return filepath.Join(e...) }
func (Local) Dir(p string) string     { return filepath.Dir(p) }
func (Local) Base(p string) string    { return filepath.Base(p) }
func (Local) MkdirAll(p string) error { return os.MkdirAll(p, 0o755) }
func (Local) Remove(p string) error   { return os.Remove(p) }
func (Local) Rename(a, b string) error {
	return os.Rename(a, b)
}
func (Local) Open(p string) (io.ReadCloser, error) { return os.Open(p) }
func (Local) Create(p string, mode fs.FileMode) (io.WriteCloser, error) {
	return os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
}

func (Local) Stat(p string) (Entry, error) {
	fi, err := os.Stat(p)
	if err != nil {
		return Entry{}, err
	}
	return entryOf(filepath.Base(p), fi), nil
}

func (Local) Lstat(p string) (Entry, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return Entry{}, err
	}
	return entryOf(filepath.Base(p), fi), nil
}

func (l Local) ReadDir(dir string) ([]Entry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		fi, err := de.Info()
		if err != nil {
			continue // vanished between listing and stat
		}
		e := entryOf(de.Name(), fi)
		if e.Link {
			if t, err := os.Stat(filepath.Join(dir, de.Name())); err == nil && t.IsDir() {
				e.IsDir = true
			}
		}
		out = append(out, e)
	}
	sortEntries(out)
	return out, nil
}

// ---- remote ----------------------------------------------------------------

// Remote is a host's filesystem over SFTP.
type Remote struct {
	Host   string
	Client *sftp.Client
}

func (r Remote) Label() string           { return r.Host }
func (r Remote) Home() (string, error)   { return r.Client.Getwd() } // the server starts in the login's home
func (Remote) Join(e ...string) string   { return path.Join(e...) }
func (Remote) Dir(p string) string       { return path.Dir(p) }
func (Remote) Base(p string) string      { return path.Base(p) }
func (r Remote) MkdirAll(p string) error { return r.Client.MkdirAll(p) }
func (r Remote) Remove(p string) error   { return r.Client.Remove(p) }
func (r Remote) Rename(a, b string) error {
	// PosixRename replaces an existing target like a local rename does,
	// where plain SFTP rename refuses; fall back for servers without it.
	if err := r.Client.PosixRename(a, b); err == nil {
		return nil
	}
	return r.Client.Rename(a, b)
}
func (r Remote) Open(p string) (io.ReadCloser, error) { return r.Client.Open(p) }
func (r Remote) Create(p string, mode fs.FileMode) (io.WriteCloser, error) {
	f, err := r.Client.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(mode.Perm()) // best effort: keep scripts executable
	return f, nil
}

func (r Remote) Stat(p string) (Entry, error) {
	fi, err := r.Client.Stat(p)
	if err != nil {
		return Entry{}, err
	}
	return entryOf(path.Base(p), fi), nil
}

func (r Remote) Lstat(p string) (Entry, error) {
	fi, err := r.Client.Lstat(p)
	if err != nil {
		return Entry{}, err
	}
	return entryOf(path.Base(p), fi), nil
}

func (r Remote) ReadDir(dir string) ([]Entry, error) {
	fis, err := r.Client.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(fis))
	for _, fi := range fis {
		e := entryOf(fi.Name(), fi)
		if e.Link {
			if t, err := r.Client.Stat(path.Join(dir, fi.Name())); err == nil && t.IsDir() {
				e.IsDir = true
			}
		}
		out = append(out, e)
	}
	sortEntries(out)
	return out, nil
}
