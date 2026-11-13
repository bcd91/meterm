package fileman

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pkg/sftp"
)

// CreateTemp looks at its directory again, not following it, before making
// the file in it: something that swaps it for a link between the mkdir and
// the open would otherwise have the copy written wherever the link leads.
func TestRemoteCreateTempRefusesAReplacedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the in-process server serves this disk; symlinks need privileges on Windows")
	}
	for what, swap := range map[string]func(dir, elsewhere string) error{
		"a symlink": func(dir, elsewhere string) error {
			if err := os.Remove(dir); err != nil {
				return err
			}
			return os.Symlink(elsewhere, dir)
		},
		"a file": func(dir, _ string) error {
			if err := os.Remove(dir); err != nil {
				return err
			}
			return os.WriteFile(dir, nil, 0o600)
		},
	} {
		t.Run(what, func(t *testing.T) {
			r := newRemote(t)
			root, elsewhere := t.TempDir(), t.TempDir()
			saved := chmodTempDir
			t.Cleanup(func() { chmodTempDir = saved })
			chmodTempDir = func(c *sftp.Client, dir string) error {
				if err := swap(dir, elsewhere); err != nil {
					t.Fatal(err)
				}
				return nil
			}
			f, tmp, err := r.CreateTemp(filepath.Join(root, "target.txt"), 0o644)
			if err == nil {
				f.Close()
				t.Fatalf("wrote %s through %s", tmp, what)
			}
			if !strings.Contains(err.Error(), "was replaced") {
				t.Errorf("err = %v, want one saying the directory was replaced", err)
			}
			if es, _ := os.ReadDir(elsewhere); len(es) != 0 {
				t.Errorf("something was made where the link led: %v", es)
			}
		})
	}
}

// uidInfo reports a uid of its own for directories, as a server would for a
// directory someone else made.
type uidInfo struct {
	os.FileInfo
	uid uint32
}

func (u uidInfo) Uid() uint32 { return u.uid }
func (u uidInfo) Gid() uint32 { return u.uid }

type uidLister struct {
	l   sftp.ListerAt
	dir uint32
}

func (u uidLister) ListAt(fis []os.FileInfo, off int64) (int, error) {
	n, err := u.l.ListAt(fis, off)
	for i := 0; i < n; i++ {
		uid := uint32(1000)
		if fis[i].IsDir() {
			uid = u.dir
		}
		fis[i] = uidInfo{fis[i], uid}
	}
	return n, err
}

type uidFS struct {
	sftp.FileLister
	dir uint32
}

func (u uidFS) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	l, err := u.FileLister.Filelist(r)
	if err != nil {
		return nil, err
	}
	return uidLister{l, u.dir}, nil
}

func (u uidFS) Lstat(r *sftp.Request) (sftp.ListerAt, error) {
	if ls, ok := u.FileLister.(sftp.LstatFileLister); ok {
		l, err := ls.Lstat(r)
		if err != nil {
			return nil, err
		}
		return uidLister{l, u.dir}, nil
	}
	return u.Filelist(r)
}

func uidRemote(t *testing.T, dirUID uint32) Remote {
	t.Helper()
	a, b := net.Pipe()
	h := sftp.InMemHandler()
	h.FileList = uidFS{h.FileList, dirUID}
	srv := sftp.NewRequestServer(a, h)
	go srv.Serve()
	c, err := sftp.NewClientPipe(b, b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); srv.Close() })
	if err := c.Mkdir("/srv"); err != nil {
		t.Fatal(err)
	}
	return Remote{Host: "test", Client: c}
}

// The file CreateTemp makes is ours, so its owner is who we are there; a
// directory owned by anyone else isn't the one it just made.
func TestRemoteCreateTempRefusesADirectoryOwnedByAnother(t *testing.T) {
	r := uidRemote(t, 4242)
	f, tmp, err := r.CreateTemp("/srv/target.txt", 0o644)
	if err == nil {
		f.Close()
		t.Fatalf("wrote %s in a directory owned by another uid", tmp)
	}
	if !strings.Contains(err.Error(), "is owned by 4242, not by us (1000)") {
		t.Errorf("err = %v, want one naming the owner", err)
	}
	if es, _ := r.Client.ReadDir("/srv"); len(es) != 0 {
		var names []string
		for _, e := range es {
			names = append(names, e.Name())
		}
		t.Errorf("left behind: %v", names)
	}

	// The same server with the directory ours: made.
	r = uidRemote(t, 1000)
	f, tmp, err = r.CreateTemp("/srv/target.txt", 0o644)
	var pe *PermError
	if err != nil && !asPermError(err, &pe) {
		t.Fatalf("our own directory: %v", err)
	}
	if f != nil {
		f.Close()
	}
	if tmp == "" {
		t.Error("no temporary file made in our own directory")
	}
}

func asPermError(err error, pe **PermError) bool {
	p, ok := err.(*PermError)
	*pe = p
	return ok
}

var _ fs.FileInfo = uidInfo{}
