package fileman

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

// rjoin is a path on newRemote's side, joined as Remote joins it: with "/",
// as every SFTP server takes. On Windows the in-process server serves C:\…,
// so a path built there reads C:\…\dir/name; a real server's never does,
// and this machine's own paths still use filepath.Join.
func rjoin(elem ...string) string { return Remote{}.Join(elem...) }

// modeOf is p's permissions as this OS keeps them: on Unix what write was
// given; on Windows only 0666, or 0444 for a read-only file.
func modeOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
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
	if err := RemoveAll(context.Background(), remote, filepath.Join(dir, "x"), nil); err != nil {
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

// An overwrite that doesn't finish must leave the file it was replacing as it
// was: the new bytes go to a temporary file next to it, renamed over the
// target only once complete.
func TestFailedOverwriteKeepsTheOriginal(t *testing.T) {
	for _, side := range []string{"local", "remote"} {
		t.Run(side, func(t *testing.T) {
			var dstFS FS = Local{}
			if side == "remote" {
				dstFS = newRemote(t)
			}
			src, dst := t.TempDir(), t.TempDir()
			write(t, filepath.Join(src, "a.txt"), string(bytes.Repeat([]byte("n"), 1<<20)), 0o644)
			write(t, filepath.Join(dst, "a.txt"), "original", 0o600)
			kept := modeOf(t, filepath.Join(dst, "a.txt")) // 0600; Windows has no such mode

			// Cancelled before a byte moves.
			ctx, cancel := context.WithCancel(context.Background())
			plan, err := MakePlan(ctx, Local{}, []string{filepath.Join(src, "a.txt")}, dstFS, dst)
			if err != nil {
				t.Fatal(err)
			}
			cancel()
			if _, err := Run(ctx, plan, true, nil); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want cancelled", err)
			}
			// Cancelled part way through.
			ctx, cancel = context.WithCancel(context.Background())
			plan.Src = cancellingFS{cancel: cancel}
			if _, err := Run(ctx, plan, true, nil); !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want cancelled", err)
			}
			if got, err := os.ReadFile(filepath.Join(dst, "a.txt")); err != nil || string(got) != "original" {
				t.Fatalf("original after cancelled overwrite = %q, %v", got, err)
			}
			if names := dirNames(t, dst); len(names) != 1 {
				t.Errorf("left behind: %v, want only a.txt", names)
			}

			// A finished overwrite replaces the content and keeps the
			// replaced file's permissions.
			plan.Src = Local{}
			if _, err := Run(context.Background(), plan, true, nil); err != nil {
				t.Fatal(err)
			}
			st, _ := os.Stat(filepath.Join(dst, "a.txt"))
			if st.Size() != 1<<20 || st.Mode().Perm() != kept {
				t.Errorf("after overwrite: size %d mode %v", st.Size(), st.Mode())
			}
			if names := dirNames(t, dst); len(names) != 1 {
				t.Errorf("left behind: %v, want only a.txt", names)
			}
		})
	}
}

// A file that appears at the target after the plan was made (so nobody was
// asked about it) is not replaced.
func TestNewFileDoesNotClobberOneThatAppeared(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "a.txt"), "new", 0o644)
	plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "a.txt")}, Local{}, dst)
	write(t, filepath.Join(dst, "a.txt"), "appeared", 0o644)
	if _, err := Run(context.Background(), plan, false, nil); err == nil {
		t.Error("copy over a file that appeared after planning succeeded")
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(got) != "appeared" {
		t.Errorf("a.txt = %q", got)
	}
	if names := dirNames(t, dst); len(names) != 1 {
		t.Errorf("left behind: %v", names)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, de := range des {
		out = append(out, de.Name())
	}
	return out
}

// newSftpServerRemote talks to OpenSSH's own sftp-server, the program real
// hosts run, over pipes. Skips where it isn't installed.
func newSftpServerRemote(t *testing.T, args ...string) Remote {
	t.Helper()
	bin := ""
	for _, p := range []string{"/usr/lib/openssh/sftp-server", "/usr/libexec/openssh/sftp-server", "/usr/libexec/sftp-server", "/usr/lib/ssh/sftp-server"} {
		if _, err := os.Stat(p); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		t.Skip("OpenSSH sftp-server not installed")
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = t.TempDir()
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c, err := sftp.NewClientPipe(out, in)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); cmd.Process.Kill(); cmd.Wait() })
	return Remote{Host: "openssh", Client: c}
}

// Rename must never replace what's at the target, on either side, files
// and directories alike (rename(2) quietly replaces an empty directory).
func TestRenameRefusesAnExistingTarget(t *testing.T) {
	sides := map[string]func(t *testing.T) FS{
		"local":            func(t *testing.T) FS { return Local{} },
		"pkg-sftp-server":  func(t *testing.T) FS { return newRemote(t) }, // its rename replaces: our check must catch it
		"openssh-sftp-srv": func(t *testing.T) FS { return newSftpServerRemote(t) },
	}
	for name, mk := range sides {
		t.Run(name, func(t *testing.T) {
			fsys := mk(t)
			dir := t.TempDir()
			write(t, filepath.Join(dir, "a.txt"), "A", 0o644)
			write(t, filepath.Join(dir, "b.txt"), "B", 0o644)
			if err := fsys.Rename(filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")); err == nil {
				t.Error("renamed a file over an existing one")
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(got) != "B" {
				t.Errorf("b.txt = %q after a refused rename", got)
			}
			os.Mkdir(filepath.Join(dir, "d1"), 0o755)
			os.Mkdir(filepath.Join(dir, "d2"), 0o755)
			if err := fsys.Rename(filepath.Join(dir, "d1"), filepath.Join(dir, "d2")); err == nil {
				t.Error("renamed a directory over an existing empty one")
			}
			if err := fsys.Rename(filepath.Join(dir, "a.txt"), filepath.Join(dir, "c.txt")); err != nil {
				t.Errorf("plain rename: %v", err)
			}
		})
	}
}

// The protocol's rename itself, without our look-first, must refuse on
// OpenSSH: that's what makes the remote rename atomic there.
func TestOpenSSHRenameRefusesWithoutTheCheck(t *testing.T) {
	r := newSftpServerRemote(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), "A", 0o644)
	write(t, filepath.Join(dir, "b.txt"), "B", 0o644)
	if err := r.renameNoReplace(filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")); err == nil {
		t.Error("OpenSSH's rename replaced the target")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(got) != "B" {
		t.Errorf("b.txt = %q", got)
	}
}

// The commit step of a copy must replace atomically on OpenSSH.
func TestOpenSSHReplace(t *testing.T) {
	r := newSftpServerRemote(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), "A", 0o644)
	write(t, filepath.Join(dir, "b.txt"), "B", 0o644)
	if err := r.Replace(filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "b.txt")); string(got) != "A" {
		t.Errorf("b.txt = %q", got)
	}
}

// Overwriting a symlink (sites-enabled/x -> ../sites-available/x) writes
// the file it points at and leaves the link as it was.
func TestOverwriteThroughASymlinkKeepsTheLink(t *testing.T) {
	sides := map[string]func(t *testing.T) FS{
		"local":            func(t *testing.T) FS { return Local{} },
		"openssh-sftp-srv": func(t *testing.T) FS { return newSftpServerRemote(t) },
	}
	for name, mk := range sides {
		t.Run(name, func(t *testing.T) {
			dstFS := mk(t)
			src, dir := t.TempDir(), t.TempDir()
			avail, enabled := filepath.Join(dir, "avail"), filepath.Join(dir, "enabled")
			write(t, filepath.Join(avail, "x.conf"), "old x", 0o640)
			write(t, filepath.Join(avail, "abs.conf"), "old abs", 0o600)
			os.MkdirAll(enabled, 0o755)
			links := map[string]string{
				"x.conf":   "../avail/x.conf",                // relative
				"abs.conf": filepath.Join(avail, "abs.conf"), // absolute
				"y.conf":   "x.conf",                         // a link to a link
			}
			for l, target := range links {
				if err := os.Symlink(target, filepath.Join(enabled, l)); err != nil {
					t.Skip("no symlinks here")
				}
			}
			for _, n := range []string{"x.conf", "abs.conf", "y.conf"} {
				write(t, filepath.Join(src, n), "new "+n, 0o644)
			}
			kept := map[string]os.FileMode{"x.conf": modeOf(t, filepath.Join(avail, "x.conf")), "abs.conf": modeOf(t, filepath.Join(avail, "abs.conf"))}
			for _, n := range []string{"x.conf", "abs.conf", "y.conf"} {
				plan, err := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, n)}, dstFS, enabled)
				if err != nil || plan.Conflicts() != 1 {
					t.Fatalf("%s: plan %+v, %v", n, plan, err)
				}
				if _, err := Run(context.Background(), plan, true, nil); err != nil {
					t.Fatalf("%s: %v", n, err)
				}
			}
			for l, target := range links {
				got, err := os.Readlink(filepath.Join(enabled, l))
				if err != nil || got != filepath.FromSlash(target) { // Windows stores ..\avail\x.conf
					t.Errorf("%s: link now %q (%v), want it still -> %q", l, got, err, target)
				}
			}
			// y.conf -> x.conf -> ../avail/x.conf: the last write through y wins.
			for f, want := range map[string]string{"x.conf": "new y.conf", "abs.conf": "new abs.conf"} {
				p := filepath.Join(avail, f)
				got, _ := os.ReadFile(p)
				st, _ := os.Stat(p)
				if string(got) != want {
					t.Errorf("%s = %q, want %q", f, got, want)
				}
				if st.Mode().Perm() != kept[f] {
					t.Errorf("%s mode %v: the linked file's permissions weren't kept", f, st.Mode())
				}
			}
			for _, d := range []string{avail, enabled} {
				if names := dirNames(t, d); len(names) != map[string]int{avail: 2, enabled: 3}[d] {
					t.Errorf("%s holds %v", d, names)
				}
			}
		})
	}
}

// A link that leads nowhere is refused, not replaced by a file.
func TestDanglingSymlinkIsNotReplaced(t *testing.T) {
	sides := map[string]func(t *testing.T) FS{
		"local":            func(t *testing.T) FS { return Local{} },
		"openssh-sftp-srv": func(t *testing.T) FS { return newSftpServerRemote(t) },
	}
	for name, mk := range sides {
		t.Run(name, func(t *testing.T) {
			dstFS := mk(t)
			src, dir := t.TempDir(), t.TempDir()
			write(t, filepath.Join(src, "gone.conf"), "new", 0o644)
			if err := os.Symlink("../avail/missing.conf", filepath.Join(dir, "gone.conf")); err != nil {
				t.Skip("no symlinks here")
			}
			plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "gone.conf")}, dstFS, dir)
			_, err := Run(context.Background(), plan, true, nil)
			if err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Errorf("err = %v, want a refusal naming the symbolic link", err)
			}
			if got, err := os.Readlink(filepath.Join(dir, "gone.conf")); err != nil || got != filepath.FromSlash("../avail/missing.conf") {
				t.Errorf("link now %q (%v)", got, err)
			}
			if names := dirNames(t, dir); len(names) != 1 {
				t.Errorf("left behind: %v", names)
			}
		})
	}
}

// chmodFailFS is a host whose server won't set permissions (some SFTP
// servers reject setstat): the file is created, its mode can't be set.
// With createOK, the file is created with its mode and only a later chmod
// fails.
type chmodFailFS struct {
	Local
	createOK bool
}

func (c chmodFailFS) CreateTemp(target string, mode fs.FileMode) (io.WriteCloser, string, error) {
	w, tmp, err := c.Local.CreateTemp(target, mode)
	if err != nil {
		return nil, "", err
	}
	if c.createOK {
		return noChmod{w.(*os.File)}, tmp, nil
	}
	return noChmod{w.(*os.File)}, tmp, &PermError{Err: errors.New("setstat: operation unsupported (test)")}
}

type noChmod struct{ *os.File }

func (noChmod) Chmod(fs.FileMode) error { return errors.New("setstat: operation unsupported (test)") }

func checkPermWarning(t *testing.T, res Result, err error, n int) {
	t.Helper()
	if err != nil {
		t.Fatalf("copy failed: %v (permissions are best effort)", err)
	}
	if len(res.Warnings) != n {
		t.Fatalf("warnings = %q, want %d", res.Warnings, n)
	}
	for _, w := range res.Warnings {
		if !strings.Contains(w, "permissions not set") {
			t.Errorf("warning %q doesn't say permissions weren't set", w)
		}
	}
}

func TestPermissionsAreBestEffort(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "new.txt"), "new", 0o644)
	write(t, filepath.Join(src, "old.txt"), "replacement", 0o644)
	write(t, filepath.Join(dst, "old.txt"), "old", 0o600)
	plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "new.txt"), filepath.Join(src, "old.txt")}, chmodFailFS{}, dst)
	res, err := Run(context.Background(), plan, true, nil)
	checkPermWarning(t, res, err, 2)
	for f, want := range map[string]string{"new.txt": "new", "old.txt": "replacement"} {
		if got, _ := os.ReadFile(filepath.Join(dst, f)); string(got) != want {
			t.Errorf("%s = %q", f, got)
		}
	}
	// Created fine, but keeping the replaced file's mode fails.
	write(t, filepath.Join(src, "old.txt"), "again", 0o644)
	plan, _ = MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "old.txt")}, chmodFailFS{createOK: true}, dst)
	res, err = Run(context.Background(), plan, true, nil)
	checkPermWarning(t, res, err, 1)
}

// failSetstat is an SFTP server that refuses every setstat, as some do.
type failSetstat struct{ sftp.FileCmder }

func (f failSetstat) Filecmd(r *sftp.Request) error {
	if r.Method == "Setstat" {
		return sftp.ErrSSHFxOpUnsupported
	}
	return f.FileCmder.Filecmd(r)
}

func (f failSetstat) PosixRename(r *sftp.Request) error {
	return f.FileCmder.(sftp.PosixRenameFileCmder).PosixRename(r)
}

func TestRemotePermissionsAreBestEffort(t *testing.T) {
	h := sftp.InMemHandler()
	h.FileCmd = failSetstat{h.FileCmd}
	a, b := net.Pipe()
	srv := sftp.NewRequestServer(a, h)
	go srv.Serve()
	c, err := sftp.NewClientPipe(b, b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); srv.Close() })
	remote := Remote{Host: "nosetstat", Client: c}
	if f, err := c.Create("/old.txt"); err == nil {
		f.Write([]byte("old"))
		f.Close()
	}
	src := t.TempDir()
	write(t, filepath.Join(src, "new.txt"), "new", 0o755)
	write(t, filepath.Join(src, "old.txt"), "replacement", 0o644)
	plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "new.txt"), filepath.Join(src, "old.txt")}, remote, "/")
	res, err := Run(context.Background(), plan, true, nil)
	checkPermWarning(t, res, err, 2)
	for f, want := range map[string]string{"/new.txt": "new", "/old.txt": "replacement"} {
		r, err := c.Open(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		got, _ := io.ReadAll(r)
		r.Close()
		if string(got) != want {
			t.Errorf("%s = %q", f, got)
		}
	}
}

// OpenSSH's own server with setstat turned off (-P), the way a locked-down
// or partial server behaves: a new file is copied without its mode, but a
// 0600 file isn't replaced by one the server made more open.
func TestOpenSSHWithoutSetstatStillCopies(t *testing.T) {
	r := newSftpServerRemote(t, "-P", "setstat,fsetstat")
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "run.sh"), "#!/bin/sh\n", 0o755)
	write(t, filepath.Join(src, "old.txt"), "replacement", 0o644)
	write(t, filepath.Join(dst, "old.txt"), "old", 0o600)
	plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "run.sh"), filepath.Join(src, "old.txt")}, r, dst)
	res, err := Run(context.Background(), plan, true, nil)
	if err != nil || len(res.Warnings) != 2 || res.Copied != 1 || res.Skipped != 1 {
		t.Fatalf("err %v, result %+v", err, res)
	}
	if !strings.Contains(res.Warnings[0], "permissions not set") || !strings.Contains(res.Warnings[1], "couldn't keep permissions 0600; left the original") {
		t.Errorf("warnings %q", res.Warnings)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "old.txt")); string(got) != "old" {
		t.Errorf("old.txt = %q, want the original kept", got)
	}
	if st, _ := os.Stat(filepath.Join(dst, "old.txt")); st.Mode().Perm() != 0o600 {
		t.Errorf("old.txt mode %v", st.Mode())
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "run.sh")); string(got) != "#!/bin/sh\n" {
		t.Errorf("run.sh = %q", got)
	}
	if names := dirNames(t, dst); len(names) != 2 {
		t.Errorf("left behind: %v", names)
	}
}
