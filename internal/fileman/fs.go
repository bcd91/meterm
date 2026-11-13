// Package fileman is meterm's two-pane file manager: this machine on one
// side, a saved host on the other, files copied between them.
//
// The remote side speaks SFTP over the system `ssh` binary (`ssh -s host
// sftp`), so jump hosts, keys, the agent and known_hosts work exactly as they
// do for `meterm connect`; this package only sees the resulting pipe.
package fileman

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pkg/sftp"

	"meterm/internal/termsafe"
)

// Entry is one item in a directory listing.
type Entry struct {
	Name    string
	Size    int64
	Mode    fs.FileMode
	ModTime time.Time
	IsDir   bool // true for directories and for symlinks pointing at one
	Link    bool // the entry itself is a symlink

	// Owner and group, when the side says (a local Unix file, or SFTP).
	Uid, Gid uint32
	HasOwner bool
	// Nlink is how many hard links the file has; 0 when it isn't known,
	// as over SFTP, which doesn't say.
	Nlink uint64
}

// FS is one side of the file manager.
type FS interface {
	Label() string // "local" or the host name
	Home() (string, error)
	ReadDir(dir string) ([]Entry, error)
	Stat(p string) (Entry, error)  // follows symlinks
	Lstat(p string) (Entry, error) // doesn't
	MkdirAll(p string) error
	Mkdir(p string) error  // one directory, failing if anything is at p
	Remove(p string) error // a file, or an empty directory
	// Rename moves from to to and refuses (fs.ErrExist) to replace
	// something already at to, as atomically as the filesystem allows.
	Rename(from, to string) error
	// Replace moves from to to, replacing a file at to in one step: a
	// reader sees the old file or the new one, never neither.
	Replace(from, to string) error
	Open(p string) (io.ReadCloser, error)
	// CreateTemp creates a new, empty file next to target, under a name
	// nothing else uses, with permissions mode as a new file gets them, and
	// says what it called it. A *PermError means the file was created and
	// is usable but its permissions couldn't be set.
	CreateTemp(target string, mode fs.FileMode) (io.WriteCloser, string, error)
	// OpenWrite empties the existing file p and opens it for writing. It
	// is never used just to ask whether p could be written: an open for
	// writing is itself an event to whatever watches the file.
	OpenWrite(p string) (io.WriteCloser, error)
	// Resolve follows symlinks from p to the file they lead to; an error
	// if they lead nowhere.
	Resolve(p string) (string, error)
	Join(elem ...string) string
	Dir(p string) string
	Base(p string) string
}

func entryOf(name string, fi fs.FileInfo) Entry {
	e := Entry{Name: name, Size: fi.Size(), Mode: fi.Mode(), ModTime: fi.ModTime(), IsDir: fi.IsDir(), Link: fi.Mode()&fs.ModeSymlink != 0}
	if st, ok := fi.Sys().(*sftp.FileStat); ok {
		e.Uid, e.Gid, e.HasOwner = st.UID, st.GID, true
	} else if uid, gid, n, ok := sysOwner(fi); ok {
		e.Uid, e.Gid, e.HasOwner, e.Nlink = uid, gid, true, n
	}
	return e
}

// identity is who a side acts as: its uid and the groups it's in.
// groupsKnown says gids is all of them; otherwise it's only those seen.
// rootAll says uid 0 really is root, who may write anything: true of this
// machine, never of a host, where a server that sends no owners makes
// every file, ours included, look like root's.
type identity struct {
	uid         uint32
	gids        []uint32
	groupsKnown bool
	rootAll     bool
}

func (id identity) inGroup(g uint32) bool {
	for _, x := range id.gids {
		if x == g {
			return true
		}
	}
	return false
}

// identityOf is who fsys acts as, where it can say: this machine knows,
// an SFTP server doesn't tell.
func identityOf(fsys FS) (identity, bool) {
	if w, ok := fsys.(interface{ identity() (identity, bool) }); ok {
		return w.identity()
	}
	return identity{}, false
}

// mayWrite says whether e could be opened for writing by id, from its mode
// bits and owner, without opening it. known is false when that can't be
// worked out (no owner, no identity, or a group we can't tell we're in):
// then only trying says, which is the person's call. ACLs aren't read.
func mayWrite(e Entry, id identity, idOK bool) (ok, known bool) {
	perm := e.Mode.Perm()
	if perm&0o002 != 0 {
		return true, true // anyone may
	}
	if !idOK || !e.HasOwner {
		return true, false
	}
	switch {
	case id.uid == 0 && id.rootAll:
		return true, true
	case id.uid == e.Uid:
		return perm&0o200 != 0, true
	case id.inGroup(e.Gid):
		return perm&0o020 != 0, true
	case !id.groupsKnown && perm&0o020 != 0:
		return true, false // in a group we didn't see, perhaps
	}
	return false, true
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

// where names p together with its side, for messages: "local /etc/x" or
// "web1:/etc/x".
func where(fsys FS, p string) string {
	if l := fsys.Label(); l != "local" {
		return l + ":" + shown(p)
	}
	return "local " + shown(p)
}

// shown is a name or path from a host for a message, a row or the log: as
// it is when every character in it shows as itself, otherwise quoted with
// each hidden one spelled out (termsafe.Quote), as the picker shows such a
// host name. Cleaned up instead, "ev\x1b]0;x\a il\u202etxt.exe" would show
// as "ev ilexe.txt", pass for another file, and the log would keep U+FFFD
// where the escape was.
func shown(s string) string {
	if termsafe.Label(s) == s {
		return s
	}
	return termsafe.Quote(s)
}

// shownAll is shown for each of ps.
func shownAll(ps []string) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = shown(p)
	}
	return out
}

// cause is err without the path an *fs.PathError adds, for a message that
// names the path (and its side) itself.
func cause(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// ---- local -----------------------------------------------------------------

// Local is this machine's filesystem.
type Local struct{}

func (Local) Label() string            { return "local" }
func (Local) Home() (string, error)    { return os.UserHomeDir() }
func (Local) Join(e ...string) string  { return filepath.Join(e...) }
func (Local) Dir(p string) string      { return filepath.Dir(p) }
func (Local) Base(p string) string     { return filepath.Base(p) }
func (Local) MkdirAll(p string) error  { return os.MkdirAll(p, 0o755) }
func (Local) Mkdir(p string) error     { return os.Mkdir(p, 0o755) }
func (Local) Remove(p string) error    { return os.Remove(p) }
func (Local) Rename(a, b string) error { return renameNoReplace(a, b) }
func (Local) Replace(a, b string) error {
	return os.Rename(a, b)
}
func (Local) Open(p string) (io.ReadCloser, error) { return os.Open(p) }
func (Local) Resolve(p string) (string, error)     { return filepath.EvalSymlinks(p) }
func (Local) OpenWrite(p string) (io.WriteCloser, error) {
	return os.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)
}
func (Local) identity() (identity, bool) { return localIdentity() }
func (Local) thisMachine()               {}
func (Local) CreateTemp(target string, mode fs.FileMode) (io.WriteCloser, string, error) {
	tmp := filepath.Join(filepath.Dir(target), tempName(filepath.Base(target)))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm()) // the umask applies, as to any new file
	if err != nil {
		return nil, "", err
	}
	return f, tmp, nil
}

// checkThenRename is the fallback where the filesystem has no rename that
// refuses an existing target: there is a moment between the check and the
// rename in which something else could appear at to.
func checkThenRename(lstat func(string) error, rename func(a, b string) error, a, b string) error {
	if err := lstat(b); err == nil {
		return &os.LinkError{Op: "rename", Old: a, New: b, Err: fs.ErrExist}
	}
	return rename(a, b)
}

// tempName is the name a file being written goes under until it's complete:
// hidden, recognisably meterm's, and short enough for any filesystem: the
// name it's for is cut to 64 bytes, so the whole is at most 90, inside
// eCryptfs's 143 however long the name is.
func tempName(base string) string { return hiddenName(base, "part") }

// backupName is where a file being replaced waits, on a server that can't
// replace in one step, until its replacement is in place.
func backupName(base string) string { return hiddenName(base, "old") }

func hiddenName(base, ext string) string {
	r := []rune(base)
	for len(string(r)) > 64 {
		r = r[:len(r)-1]
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf(".%s.meterm-%s.%s", string(r), hex.EncodeToString(b[:]), ext)
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

	noPosixRename bool // tests: behave as a server without posix-rename
}

func (r Remote) Label() string           { return r.Host }
func (r Remote) Home() (string, error)   { return r.Client.Getwd() } // the server starts in the login's home
func (Remote) Join(e ...string) string   { return path.Join(e...) }
func (Remote) Dir(p string) string       { return path.Dir(p) }
func (Remote) Base(p string) string      { return path.Base(p) }
func (r Remote) MkdirAll(p string) error { return r.Client.MkdirAll(p) }
func (r Remote) Mkdir(p string) error    { return r.Client.Mkdir(p) }
func (r Remote) Remove(p string) error   { return r.Client.Remove(p) }

// Rename uses the protocol's own rename, which OpenSSH's server carries out
// as link+unlink and so refuses an existing target atomically. Not every
// server does (some just call rename(2)), so look first as well.
func (r Remote) Rename(a, b string) error {
	return checkThenRename(func(p string) error { _, err := r.Client.Lstat(p); return err }, r.renameNoReplace, a, b)
}

// renameNoReplace is the plain SFTP rename, without the look first.
func (r Remote) renameNoReplace(a, b string) error { return r.Client.Rename(a, b) }

// Replace uses OpenSSH's posix-rename extension, which replaces the target
// in one step. A server without it gets replaceByRenames.
func (r Remote) Replace(a, b string) error {
	if _, ok := r.Client.HasExtension("posix-rename@openssh.com"); ok && !r.noPosixRename {
		return r.Client.PosixRename(a, b)
	}
	return replaceByRenames(r.Client.Rename, r.Client.Remove, r.replaceable, a, b)
}

// replaceable refuses p unless it's a regular file, or a link to one, or
// nothing at all: moving a directory aside would hide it, not replace it.
func (r Remote) replaceable(p string) error {
	fi, err := r.Client.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if _, lerr := r.Client.Lstat(p); lerr != nil {
				return nil // nothing there
			}
		}
		return fmt.Errorf("%s isn't a file that can be replaced: %w", p, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s isn't a regular file (%v); not replacing it", p, fi.Mode().Type())
	}
	return nil
}

// replaceByRenames replaces b with a on a server whose rename refuses an
// existing target: b is moved aside first, a moved into its place, and only
// then is the old b removed. Whatever fails, one copy or the other stays:
// a failed move puts b back, and if even that fails, *ReplaceStuck says
// where both are. For a moment nothing is at b. check, when not nil, is
// asked first whether b may be replaced at all.
func replaceByRenames(rename func(a, b string) error, remove func(p string) error, check func(b string) error, a, b string) error {
	if check != nil {
		if err := check(b); err != nil {
			return err
		}
	}
	backup := path.Join(path.Dir(b), backupName(path.Base(b)))
	if err := rename(b, backup); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return rename(a, b) // nothing to replace
		}
		return err
	}
	if err := rename(a, b); err != nil {
		if rerr := rename(backup, b); rerr != nil {
			return &ReplaceStuck{Target: b, Backup: backup, Temp: a, Err: err, RestoreErr: rerr}
		}
		return err
	}
	if err := remove(backup); err != nil {
		return &BackupLeft{Backup: backup, Err: err}
	}
	return nil
}

// BackupLeft is a replace that worked but couldn't remove the old file
// from where it was moved aside: a warning, not a failure.
type BackupLeft struct {
	Backup string
	Err    error
}

func (e *BackupLeft) Error() string {
	return fmt.Sprintf("the replaced file is left at %s (%v)", e.Backup, e.Err)
}

func (e *BackupLeft) Unwrap() error { return e.Err }

func (r Remote) Open(p string) (io.ReadCloser, error) { return r.Client.Open(p) }
func (r Remote) OpenWrite(p string) (io.WriteCloser, error) {
	return r.Client.OpenFile(p, os.O_WRONLY|os.O_TRUNC)
}

// CreateTemp makes the temporary file inside a directory of its own next
// to target (.meterm-<random>), never loose in target's directory: SFTP's
// open can't say what mode to create a file with, so the server makes it
// 0666 less its umask and only then is it chmod'ed, and in that moment
// anyone who may read target's directory could open it and read what's
// written. The directory is made 0700 and looked at again (not followed)
// before the file is made in it, so no one else can reach the file at
// all. Where it can't be made private (a server that refuses setstat), it
// is still used if a new file there starts no more open to others than
// mode, which the directory's own mode shows (both come from the server's
// umask), or if the file's own mode can't be set either: then the file
// keeps the server's mode for good, as before, and says so (*PermError).
// Otherwise it's ErrNotPrivate, handled as a temporary file that couldn't
// be made. The file is moved out of it into place (a rename within one
// filesystem), and the directory removed after (removeTempDir).
func (r Remote) CreateTemp(target string, mode fs.FileMode) (io.WriteCloser, string, error) {
	dir := path.Join(path.Dir(target), tempDirName())
	if err := r.Client.Mkdir(dir); err != nil {
		return nil, "", err
	}
	fail := func(err error) (io.WriteCloser, string, error) {
		_ = r.Client.RemoveDirectory(dir)
		return nil, "", err
	}
	chErr := chmodTempDir(r.Client, dir)
	// Seen by its name, not followed: a link or anything else in its place
	// isn't the directory made here.
	fi, err := r.Client.Lstat(dir)
	switch {
	case err != nil:
		return fail(err)
	case fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir():
		return fail(fmt.Errorf("the temporary directory %s was replaced (%v); not writing there", dir, fi.Mode().Type()))
	}
	perm := fi.Mode().Perm()
	private := perm&0o077 == 0 && perm&0o700 == 0o700
	// What a new file there starts with, to others, beyond what mode gives them.
	exposed := !private && 0o666&perm&^mode.Perm()&0o077 != 0
	tmp := path.Join(dir, tempName(path.Base(target)))
	f, err := r.Client.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return fail(err)
	}
	// The file is ours, so its owner is who we are there: a directory
	// owned by anyone else isn't the one made here.
	if st, ok := fi.Sys().(*sftp.FileStat); ok {
		if ft, err := f.Stat(); err == nil {
			if fst, ok := ft.Sys().(*sftp.FileStat); ok && fst.UID != st.UID {
				f.Close()
				_ = r.Client.Remove(tmp)
				return fail(fmt.Errorf("the temporary directory %s is owned by %d, not by us (%d); not writing there", dir, st.UID, fst.UID))
			}
		}
	}
	// Best effort: some servers (Windows OpenSSH among them) refuse or
	// half-support setstat, and the copy is worth more than its mode.
	if err := f.Chmod(mode.Perm()); err != nil {
		return f, tmp, &PermError{Err: err}
	}
	if exposed {
		// Its mode was set, but only after others could have opened it.
		f.Close()
		_ = r.Client.Remove(tmp)
		why := "stayed"
		if chErr != nil {
			why = "couldn't be chmod'ed (" + chErr.Error() + ") and is"
		}
		return fail(&NotPrivateError{Dir: dir, Mode: perm, Want: mode.Perm(), Why: why})
	}
	return f, tmp, nil
}

// ErrNotPrivate is a temporary file not made because no one else could have
// been kept from reading it while it was written: its directory couldn't
// be made private, and a new file there starts more open than the one it's
// for.
var ErrNotPrivate = errors.New("not private")

// NotPrivateError says which directory, and what mode it had.
type NotPrivateError struct {
	Dir        string
	Mode, Want fs.FileMode
	Why        string
}

func (e *NotPrivateError) Error() string {
	return fmt.Sprintf("can't keep the copy private while it's written: the temporary directory %s %s mode %04o, so a new file there starts more open than %04o",
		e.Dir, e.Why, e.Mode, e.Want)
}

func (e *NotPrivateError) Unwrap() error { return ErrNotPrivate }

// chmodTempDir makes dir 0700; tests replace it to see what a server that
// doesn't would mean.
var chmodTempDir = func(c *sftp.Client, dir string) error { return c.Chmod(dir, 0o700) }

// removeTempDir removes the private directory CreateTemp made for tmp, once
// tmp has been moved out of it or removed. Nothing when tmp isn't in one
// (this machine's temporary files aren't): only an empty directory with
// that name is ever removed.
func removeTempDir(dst FS, tmp string) (string, error) {
	dir := dst.Dir(tmp)
	if !isTempDirName(dst.Base(dir)) {
		return "", nil
	}
	if err := dst.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return dir, err
	}
	return "", nil
}

// tempDirName is a new private directory's name: .meterm-<12 hex>.
func tempDirName() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return ".meterm-" + hex.EncodeToString(b[:])
}

func isTempDirName(name string) bool {
	h, ok := strings.CutPrefix(name, ".meterm-")
	if !ok || len(h) != 12 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

// Resolve follows the link chain by hand: a relative target is relative to
// the link's own directory.
func (r Remote) Resolve(p string) (string, error) {
	for hops := 0; hops < 40; hops++ {
		fi, err := r.Client.Lstat(p)
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			return p, nil
		}
		t, err := r.Client.ReadLink(p)
		if err != nil {
			return "", err
		}
		if !path.IsAbs(t) {
			t = path.Join(path.Dir(p), t)
		}
		p = t
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", p)
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

// ReplaceStuck is a replace without posix-rename that failed after the old
// file was moved aside and couldn't be moved back: no file is at Target,
// the old one is at Backup and the new one at Temp. Neither is removed.
type ReplaceStuck struct {
	Target, Backup, Temp string
	Err, RestoreErr      error
}

func (e *ReplaceStuck) Error() string {
	return fmt.Sprintf("couldn't move the copy to %s (%v) nor the old file back (%v): the old file is at %s, the new one at %s",
		e.Target, e.Err, e.RestoreErr, e.Backup, e.Temp)
}

func (e *ReplaceStuck) Unwrap() error { return e.Err }
