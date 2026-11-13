package fileman

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The sides a transfer test runs against: this machine, and OpenSSH's own
// sftp-server (what real hosts run).
func transferSides() map[string]func(t *testing.T) FS {
	return map[string]func(t *testing.T) FS{
		"local":            func(t *testing.T) FS { return Local{} },
		"openssh-sftp-srv": func(t *testing.T) FS { return newSftpServerRemote(t) },
	}
}

// readOnlyDir makes dir/ro holding the given files, then takes write
// permission off ro itself.
func readOnlyDir(t *testing.T, files map[string]os.FileMode) string {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix directory permissions and a non-root user")
	}
	ro := filepath.Join(t.TempDir(), "ro")
	for name, mode := range files {
		write(t, filepath.Join(ro, name), "old "+name, mode)
		os.Chmod(filepath.Join(ro, name), mode) // past the umask
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ro, 0o755) })
	return ro
}

// A writable file in a directory that can't be written: the temporary file
// can't be made, so the copy says so (naming that directory, not the
// source) and asks; once agreed, the file is written in place.
func TestOverwriteInAReadOnlyDirectory(t *testing.T) {
	for name, mk := range transferSides() {
		t.Run(name, func(t *testing.T) {
			dstFS := mk(t)
			ro := readOnlyDir(t, map[string]os.FileMode{"index.html": 0o666})
			src := t.TempDir()
			write(t, filepath.Join(src, "index.html"), "new index", 0o644)
			plan, err := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "index.html")}, dstFS, ro)
			if err != nil || plan.Conflicts() != 1 {
				t.Fatalf("plan %+v, %v", plan, err)
			}
			_, err = RunWith(context.Background(), plan, RunOptions{Overwrite: true}, nil)
			var need *InPlaceNeeded
			if !errors.As(err, &need) {
				t.Fatalf("err = %v, want it to ask about writing in place", err)
			}
			msg := err.Error()
			for _, want := range []string{"can't write a temporary file in", ro, "the file itself is writable"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q doesn't say %q", msg, want)
				}
			}
			if strings.Contains(msg, src) {
				t.Errorf("error %q names the source, which was readable", msg)
			}
			if got, _ := os.ReadFile(filepath.Join(ro, "index.html")); string(got) != "old index.html" {
				t.Fatalf("before agreeing, index.html = %q", got)
			}

			res, err := RunWith(context.Background(), plan, RunOptions{Overwrite: true, InPlace: map[string]bool{need.Target: true}, From: need.Index}, nil)
			if err != nil {
				t.Fatalf("in place: %v", err)
			}
			if got, _ := os.ReadFile(filepath.Join(ro, "index.html")); string(got) != "new index" {
				t.Errorf("after writing in place, index.html = %q", got)
			}
			if st, _ := os.Stat(filepath.Join(ro, "index.html")); st.Mode().Perm() != 0o666 {
				t.Errorf("mode %v, want 0666 kept", st.Mode())
			}
			if len(res.InPlace) != 1 || res.Replaced != 1 || len(res.ReplacedPaths) != 1 {
				t.Errorf("result %+v, want one file replaced in place", res)
			}
			if names := dirNames(t, ro); len(names) != 1 {
				t.Errorf("ro holds %v", names)
			}
		})
	}
}

// Where writing in place can't help, nobody is asked: a new file in a
// directory that can't be written, or a file that can't be written either.
// Both errors name the destination. Over SFTP, who we are isn't known, so
// whether a file can be written isn't either: that's asked, and agreeing
// fails cleanly, with the file as it was.
func TestReadOnlyDirectoryWithNothingToWriteInPlace(t *testing.T) {
	for name, mk := range transferSides() {
		t.Run(name, func(t *testing.T) {
			dstFS := mk(t)
			ro := readOnlyDir(t, map[string]os.FileMode{"locked.txt": 0o444})
			src := t.TempDir()
			write(t, filepath.Join(src, "new.txt"), "n", 0o644)
			write(t, filepath.Join(src, "locked.txt"), "n", 0o644)
			for _, f := range []string{"new.txt", "locked.txt"} {
				plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, f)}, dstFS, ro)
				_, err := RunWith(context.Background(), plan, RunOptions{Overwrite: true}, nil)
				var need *InPlaceNeeded
				if f == "locked.txt" && name != "local" {
					if !errors.As(err, &need) || !strings.Contains(err.Error(), "may be writable") {
						t.Fatalf("%s: err = %v, want the in-place question, saying it may be writable", f, err)
					}
					_, err = RunWith(context.Background(), plan, RunOptions{Overwrite: true, InPlace: map[string]bool{need.Target: true}}, nil)
					if err == nil || !strings.Contains(err.Error(), "left as it was") || strings.Contains(err.Error(), "partly") {
						t.Errorf("%s: err = %v, want a clean failure", f, err)
					}
					if got, _ := os.ReadFile(filepath.Join(ro, f)); string(got) != "old locked.txt" {
						t.Errorf("%s = %q", f, got)
					}
					continue
				}
				if err == nil || errors.As(err, &need) {
					t.Fatalf("%s: err = %v, want a plain failure", f, err)
				}
				if !strings.Contains(err.Error(), ro) || strings.Contains(err.Error(), src) {
					t.Errorf("%s: error %q should name %s and not the source", f, err, ro)
				}
				if f == "locked.txt" && !strings.Contains(err.Error(), "isn't writable") {
					t.Errorf("%s: error %q doesn't say the file can't be written", f, err)
				}
			}
		})
	}
}

// partialWriteFS can't make temporary files, as in a read-only directory
// (but its directory can be written, so a Remove would work), and its
// in-place writer fails after a few bytes.
type partialWriteFS struct{ Local }

func (partialWriteFS) CreateTemp(target string, mode fs.FileMode) (io.WriteCloser, string, error) {
	return nil, "", &fs.PathError{Op: "open", Path: target + ".part", Err: fs.ErrPermission}
}

func (partialWriteFS) OpenWrite(p string) (io.WriteCloser, error) {
	f, err := Local{}.OpenWrite(p)
	if err != nil {
		return f, err
	}
	return &failAfter{w: f, left: 3}, nil
}

type failAfter struct {
	w    io.WriteCloser
	left int
}

func (f *failAfter) Write(p []byte) (int, error) {
	if len(p) > f.left {
		n, _ := f.w.Write(p[:f.left])
		f.left = 0
		return n, errors.New("no space left on device (test)")
	}
	f.left -= len(p)
	return f.w.Write(p)
}
func (f *failAfter) Close() error { return f.w.Close() }

// A failed write in place can't put the old file back. It must never
// remove what's left, and must say it may be partly written.
func TestFailedWriteInPlaceKeepsWhatIsThere(t *testing.T) {
	ro := t.TempDir()
	write(t, filepath.Join(ro, "index.html"), "old index.html", 0o666)
	src := t.TempDir()
	write(t, filepath.Join(src, "index.html"), "new index", 0o644)
	plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "index.html")}, partialWriteFS{}, ro)
	_, err := RunWith(context.Background(), plan, RunOptions{Overwrite: true, InPlace: map[string]bool{filepath.Join(ro, "index.html"): true}}, nil)
	if err == nil || !strings.Contains(err.Error(), "may be partly written") || !strings.Contains(err.Error(), filepath.Join(ro, "index.html")) {
		t.Fatalf("err = %v, want it to say %s may be partly written", err, filepath.Join(ro, "index.html"))
	}
	if _, serr := os.Stat(filepath.Join(ro, "index.html")); serr != nil {
		t.Errorf("the target was removed after a failed write in place: %v", serr)
	}
}

// writeFailFS hands out temporary files that refuse every write.
type writeFailFS struct{ Local }

type refuseWrites struct{ *os.File }

func (refuseWrites) Write([]byte) (int, error) { return 0, errors.New("input/output error (test)") }

func (w writeFailFS) CreateTemp(target string, mode fs.FileMode) (io.WriteCloser, string, error) {
	f, tmp, err := w.Local.CreateTemp(target, mode)
	if err != nil {
		return nil, "", err
	}
	return refuseWrites{f.(*os.File)}, tmp, nil
}

// A copy that fails names the side and path that failed: reading the
// source, or writing the destination.
func TestCopyErrorsNameTheRightSide(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "zz-bad.txt"), "x", 0o644)
	plan, _ := MakePlan(context.Background(), failFS{bad: "zz-bad"}, []string{filepath.Join(src, "zz-bad.txt")}, Local{}, dst)
	_, err := Run(context.Background(), plan, false, nil)
	if err == nil || !strings.Contains(err.Error(), "read local "+filepath.Join(src, "zz-bad.txt")) {
		t.Errorf("source failure: %v, want it to name the local source", err)
	}

	write(t, filepath.Join(src, "a.txt"), "a", 0o644)
	plan, _ = MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "a.txt")}, writeFailFS{}, dst)
	_, err = Run(context.Background(), plan, false, nil)
	if err == nil || !strings.Contains(err.Error(), "write local "+filepath.Join(dst, "a.txt")) || strings.Contains(err.Error(), src) {
		t.Errorf("destination failure: %v, want it to name %s and not the source", err, filepath.Join(dst, "a.txt"))
	}
}

// Deleting a symlink to a directory removes the link, never what's in the
// directory it points at.
func TestDeletingADirectorySymlinkKeepsTheTarget(t *testing.T) {
	for name, mk := range transferSides() {
		t.Run(name, func(t *testing.T) {
			fsys := mk(t)
			dir := t.TempDir()
			write(t, filepath.Join(dir, "real", "keep.txt"), "k", 0o644)
			write(t, filepath.Join(dir, "real", "sub", "deep.txt"), "d", 0o644)
			if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link")); err != nil {
				t.Skip("no symlinks here")
			}
			var removed []string
			if err := RemoveAll(context.Background(), fsys, filepath.Join(dir, "link"), func(p string) { removed = append(removed, p) }); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(dir, "link")); !os.IsNotExist(err) {
				t.Errorf("the link is still there (%v)", err)
			}
			for _, f := range []string{"keep.txt", "sub/deep.txt"} {
				if _, err := os.Stat(filepath.Join(dir, "real", f)); err != nil {
					t.Errorf("real/%s went with the link: %v", f, err)
				}
			}
			if fmt.Sprint(removed) != fmt.Sprint([]string{filepath.Join(dir, "link")}) {
				t.Errorf("removed %v, want only the link", removed)
			}
		})
	}
}
