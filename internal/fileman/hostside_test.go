package fileman

import (
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/pkg/sftp"
)

// hostSide is the right pane's host, with what a test needs to set it up
// and look at it behind the file manager's back.
type hostSide struct {
	fs    Remote
	dir   string
	write func(t *testing.T, name string)
	gone  func(name string) bool
	names func(t *testing.T) []string
}

// hostSides are the hosts a test of names runs on: this machine's disk
// over SFTP, and a server holding its files in memory. Names with control
// characters can't be made on a Windows disk, though a host the file
// manager talks to may have them: there only memory has them.
func hostSides() map[string]func(t *testing.T) hostSide {
	sides := map[string]func(t *testing.T) hostSide{"memory": memSide}
	if runtime.GOOS != "windows" {
		sides["disk"] = diskSide
	}
	return sides
}

func diskSide(t *testing.T) hostSide {
	dir := t.TempDir()
	return hostSide{
		fs:    newRemote(t),
		dir:   dir,
		write: func(t *testing.T, name string) { write(t, filepath.Join(dir, name), "x", 0o644) },
		gone: func(name string) bool {
			_, err := os.Lstat(filepath.Join(dir, name))
			return os.IsNotExist(err)
		},
		names: func(t *testing.T) []string { return dirNames(t, dir) },
	}
}

// memSide is a host whose SFTP server keeps its files in memory, with
// POSIX paths as a real one has, whatever this machine is.
func memSide(t *testing.T) hostSide {
	a, b := net.Pipe()
	srv := sftp.NewRequestServer(a, sftp.InMemHandler())
	go srv.Serve()
	c, err := sftp.NewClientPipe(b, b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); srv.Close() })
	const dir = "/srv"
	if err := c.Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	return hostSide{
		fs:  Remote{Host: "test", Client: c},
		dir: dir,
		write: func(t *testing.T, name string) {
			f, err := c.Create(path.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			f.Close()
		},
		gone: func(name string) bool {
			_, err := c.Lstat(path.Join(dir, name))
			return os.IsNotExist(err)
		},
		names: func(t *testing.T) []string {
			fis, err := c.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, fi := range fis {
				out = append(out, fi.Name())
			}
			sort.Strings(out)
			return out
		},
	}
}

// newHarnessOn is newHarness with side as the right pane.
func newHarnessOn(t *testing.T, left string, side hostSide) *harness {
	return newHarnessWith(t, left, side.dir, func(o *Options) { o.Right = side.fs })
}
