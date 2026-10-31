package fileman

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pkg/sftp"
)

// newRemote serves this machine's filesystem over SFTP in-process, so the
// Remote side is exercised through a real SFTP client and server.
func newRemote(t *testing.T) Remote {
	t.Helper()
	a, b := net.Pipe()
	srv, err := sftp.NewServer(a)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	c, err := sftp.NewClientPipe(b, b, sftp.UseConcurrentWrites(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); srv.Close() })
	return Remote{Host: "test", Client: c}
}

func write(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestUploadAndDownloadTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The in-process SFTP server serves this machine's disk, so on Windows
		// its "remote" paths are C:\… rather than the POSIX paths of a real
		// server, and there's no executable bit to carry over.
		t.Skip("needs a POSIX filesystem behind the fake SFTP server")
	}
	ctx := context.Background()
	remote := newRemote(t)
	src, dst, back := t.TempDir(), t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "site", "index.html"), "<h1>hi</h1>", 0o644)
	write(t, filepath.Join(src, "site", "assets", "app.js"), "console.log(1)", 0o644)
	write(t, filepath.Join(src, "deploy.sh"), "#!/bin/sh\necho ok\n", 0o755)

	// local -> remote
	plan, err := MakePlan(ctx, Local{}, []string{filepath.Join(src, "site"), filepath.Join(src, "deploy.sh")}, remote, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != 3 || plan.Conflicts() != 0 || plan.TotalBytes == 0 {
		t.Fatalf("plan = %+v", plan)
	}
	var last Progress
	res, err := Run(ctx, plan, false, func(p Progress) { last = p })
	if err != nil || res.Copied != 3 {
		t.Fatalf("upload = %+v, %v", res, err)
	}
	if last.Done != plan.TotalBytes || last.Files != 3 {
		t.Errorf("final progress = %+v, want all %d bytes / 3 files", last, plan.TotalBytes)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "site", "assets", "app.js")); string(got) != "console.log(1)" {
		t.Errorf("uploaded content = %q", got)
	}
	if st, _ := os.Stat(filepath.Join(dst, "deploy.sh")); st.Mode().Perm() != 0o755 {
		t.Errorf("executable bit lost: %v", st.Mode())
	}

	// remote -> local
	plan, err = MakePlan(ctx, remote, []string{filepath.Join(dst, "site")}, Local{}, back)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(ctx, plan, false, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(back, "site", "index.html")); string(got) != "<h1>hi</h1>" {
		t.Errorf("downloaded content = %q", got)
	}
}

func TestExistingFilesAreSkippedUnlessOverwriting(t *testing.T) {
	ctx := context.Background()
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "a.txt"), "new", 0o644)
	write(t, filepath.Join(dst, "a.txt"), "old", 0o644)

	plan, _ := MakePlan(ctx, Local{}, []string{filepath.Join(src, "a.txt")}, Local{}, dst)
	if plan.Conflicts() != 1 {
		t.Fatalf("conflicts = %d, want 1", plan.Conflicts())
	}
	res, _ := Run(ctx, plan, false, nil)
	if got, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(got) != "old" || res.Skipped != 1 {
		t.Errorf("without overwrite: %q, %+v", got, res)
	}
	Run(ctx, plan, true, nil)
	if got, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(got) != "new" {
		t.Errorf("with overwrite: %q", got)
	}
}

// cancellingFS cancels the copy as soon as the first bytes are read, the
// way a person pressing Esc mid-transfer does.
type cancellingFS struct {
	Local
	cancel context.CancelFunc
}

func (c cancellingFS) Open(p string) (io.ReadCloser, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return &cancelOnRead{File: f, cancel: c.cancel}, nil
}

type cancelOnRead struct {
	*os.File
	cancel context.CancelFunc
}

func (c *cancelOnRead) Read(p []byte) (int, error) {
	n, err := c.File.Read(p)
	c.cancel()
	return n, err
}

func TestCancelRemovesThePartialFile(t *testing.T) {
	remote := newRemote(t)
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "big.bin"), string(bytes.Repeat([]byte("x"), 8<<20)), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	from := cancellingFS{cancel: cancel}
	plan, _ := MakePlan(ctx, from, []string{filepath.Join(src, "big.bin")}, remote, dst)
	_, err := Run(ctx, plan, false, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "big.bin")); !os.IsNotExist(err) {
		t.Errorf("partial file left behind (stat err %v)", err)
	}
}

func TestSymlinkedDirectoriesAreSkipped(t *testing.T) {
	ctx := context.Background()
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "real", "f.txt"), "x", 0o644)
	if err := os.Symlink(filepath.Join(src, "real"), filepath.Join(src, "loop")); err != nil {
		t.Skip("no symlinks here")
	}
	plan, err := MakePlan(ctx, Local{}, []string{filepath.Join(src, "loop"), filepath.Join(src, "real")}, Local{}, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Skipped) != 1 || len(plan.Files) != 1 {
		t.Errorf("plan = %+v, want the symlinked dir skipped and real/f.txt copied", plan)
	}
}

func TestRemoveAllRemote(t *testing.T) {
	remote := newRemote(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "x", "y", "z.txt"), "z", 0o644)
	if err := RemoveAll(context.Background(), remote, filepath.Join(dir, "x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); !os.IsNotExist(err) {
		t.Errorf("x still exists")
	}
}

func TestReadDirPutsDirectoriesFirst(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "b.txt"), "", 0o644)
	write(t, filepath.Join(dir, "A.txt"), "", 0o644)
	os.Mkdir(filepath.Join(dir, "zdir"), 0o755)
	for _, fsys := range []FS{Local{}, newRemote(t)} {
		es, err := fsys.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(es) != 3 || es[0].Name != "zdir" || es[1].Name != "A.txt" || es[2].Name != "b.txt" {
			t.Errorf("%s order = %+v", fsys.Label(), es)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KB", 5 << 20: "5.0 MB"} {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %s, want %s", in, got, want)
		}
	}
}
