//go:build unix

package fileman

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// spySource is this machine as a source whose files, on their first read
// (when the copy's temporary file is open and nothing is written yet),
// call look.
type spySource struct {
	Local
	look func()
}

func (s spySource) Open(p string) (io.ReadCloser, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return &spyReader{File: f, look: s.look}, nil
}

type spyReader struct {
	*os.File
	look   func()
	looked bool
}

func (r *spyReader) Read(b []byte) (int, error) {
	if !r.looked {
		r.looked = true
		r.look()
	}
	return r.File.Read(b)
}

// What's in dir while a copy is written: every entry, and its mode.
func modesIn(t *testing.T, dir string) map[string]fs.FileMode {
	t.Helper()
	out := map[string]fs.FileMode{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[rel] = fi.Mode()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Overwriting a 0600 file on a host through OpenSSH's sftp-server (umask
// 022, so a new file there starts 0644): while the copy is written, the
// only new thing in the directory is a 0700 directory of meterm's, with the
// temporary file inside it; nothing readable by others is loose next to the
// target. After, the directory is gone and the file has its old mode.
func TestRemoteTempFileIsNeverReadableByOthers(t *testing.T) {
	r := newSftpServerRemote(t, "-u", "022")
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "secret"), "new secret", 0o644)
	write(t, filepath.Join(dst, "secret"), "old secret", 0o600)
	seen := map[string]fs.FileMode{}
	spy := spySource{look: func() { seen = modesIn(t, dst) }}
	plan, err := MakePlan(context.Background(), spy, []string{filepath.Join(src, "secret")}, r, dst)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), plan, true, nil)
	if err != nil || res.Copied != 1 {
		t.Fatalf("err %v, result %+v", err, res)
	}
	if len(seen) != 3 {
		t.Fatalf("while writing, %s held %v; want the target, one directory and the file in it", dst, seen)
	}
	for name, mode := range seen {
		switch {
		case name == "secret":
		case !strings.Contains(name, "/"):
			if !isTempDirName(name) || !mode.IsDir() || mode.Perm() != 0o700 {
				t.Errorf("while writing: %s is %v; want a .meterm-<hex> directory, mode 0700", name, mode)
			}
		default:
			if !strings.HasSuffix(name, ".part") || !mode.IsRegular() {
				t.Errorf("while writing: %s is %v; want the temporary file", name, mode)
			}
		}
	}
	if names := dirNames(t, dst); len(names) != 1 {
		t.Errorf("left behind: %v", names)
	}
	st, _ := os.Stat(filepath.Join(dst, "secret"))
	got, _ := os.ReadFile(filepath.Join(dst, "secret"))
	if string(got) != "new secret" || st.Mode().Perm() != 0o600 {
		t.Errorf("secret = %q, mode %v", got, st.Mode())
	}
}

// A server that won't chmod a directory but will chmod an open file
// (sftp-server -P setstat): the directory can't be made private, so a 0600
// file isn't overwritten through a file others could have opened; it asks
// to write in place instead, leaving nothing behind. A new file whose mode
// is no tighter than what the server gives is still copied.
func TestRemoteTempDirNotPrivate(t *testing.T) {
	r := newSftpServerRemote(t, "-u", "022", "-P", "setstat")
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "secret"), "new secret", 0o644)
	write(t, filepath.Join(dst, "secret"), "old secret", 0o600)
	plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "secret")}, r, dst)
	_, err := Run(context.Background(), plan, true, nil)
	var need *InPlaceNeeded
	if !errors.As(err, &need) || !errors.Is(need.Err, ErrNotPrivate) {
		t.Fatalf("err %v; want the question to write in place, because the directory isn't private", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "secret")); string(got) != "old secret" {
		t.Errorf("secret = %q, want it left as it was", got)
	}
	if names := dirNames(t, dst); len(names) != 1 {
		t.Errorf("left behind: %v", names)
	}

	write(t, filepath.Join(src, "notes"), "notes", 0o644)
	plan, _ = MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "notes")}, r, dst)
	if res, err := Run(context.Background(), plan, false, nil); err != nil || res.Copied != 1 {
		t.Fatalf("a 0644 file: err %v, result %+v", err, res)
	}
	if names := dirNames(t, dst); len(names) != 2 {
		t.Errorf("left behind: %v", names)
	}
}

// A temporary directory that can't be removed after the copy (the server
// refuses rmdir) is named, not hidden.
func TestRemoteTempDirLeftIsNamed(t *testing.T) {
	r := newSftpServerRemote(t, "-u", "022", "-P", "rmdir")
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "a"), "a", 0o644)
	plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "a")}, r, dst)
	res, err := Run(context.Background(), plan, false, nil)
	if err != nil || res.Copied != 1 {
		t.Fatalf("err %v, result %+v", err, res)
	}
	if len(res.Leftovers) != 1 || !strings.Contains(res.Leftovers[0], "/.meterm-") || !strings.Contains(res.Leftovers[0], "empty temporary directory") {
		t.Errorf("leftovers %q; want the directory named", res.Leftovers)
	}
}
