//go:build unix

package fileman

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The sides an overwrite is checked against: this machine, pkg/sftp's own
// server in-process, and OpenSSH's sftp-server (what real hosts run).
func overwriteSides() map[string]func(t *testing.T) FS {
	return map[string]func(t *testing.T) FS{
		"local":            func(t *testing.T) FS { return Local{} },
		"pkg-sftp-server":  func(t *testing.T) FS { return newRemote(t) },
		"openssh-sftp-srv": func(t *testing.T) FS { return newSftpServerRemote(t) },
	}
}

// overwrite copies src/name over dst/name and returns what the run said.
func overwrite(t *testing.T, srcFS, dstFS FS, src, dst, name string, o RunOptions, report func(Progress)) (Result, error) {
	t.Helper()
	plan, err := MakePlan(context.Background(), srcFS, []string{filepath.Join(src, name)}, dstFS, dst)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	o.Overwrite = true
	return RunWith(context.Background(), plan, o, report)
}

// partFiles is what a copy left behind in dir under meterm's temporary names.
func partFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, n := range dirNames(t, dir) {
		if strings.Contains(n, ".meterm-") {
			out = append(out, n)
		}
	}
	return out
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// 1. A name near the 255-byte limit (a 78-character Thai name is 234 bytes)
// must still be overwritable: the temporary name can't be the name plus a
// suffix. eCryptfs allows only 143 bytes, so the temporary name fits that.
func TestLongNamesCanBeOverwritten(t *testing.T) {
	names := map[string]string{
		"thai-78":   strings.Repeat("ก", 78),
		"ascii-255": strings.Repeat("a", 251) + ".txt",
	}
	for side, mk := range overwriteSides() {
		for what, name := range names {
			t.Run(side+"/"+what, func(t *testing.T) {
				dstFS := mk(t)
				src, dst := t.TempDir(), t.TempDir()
				write(t, filepath.Join(src, name), "new", 0o644)
				write(t, filepath.Join(dst, name), "old", 0o644)
				if _, err := overwrite(t, Local{}, dstFS, src, dst, name, RunOptions{}, nil); err != nil {
					t.Fatalf("overwrite a %d-byte name: %v", len(name), err)
				}
				if got := readFile(t, filepath.Join(dst, name)); got != "new" {
					t.Errorf("content = %q", got)
				}
				if l := partFiles(t, dst); len(l) > 0 {
					t.Errorf("left behind: %v", l)
				}
			})
		}
	}
	for _, base := range []string{strings.Repeat("a", 255), strings.Repeat("ก", 85), strings.Repeat("ก", 39)} {
		for _, n := range []string{tempName(base), backupName(base)} {
			if len(n) > 143 {
				t.Errorf("temporary name %d bytes for a %d-byte name, over eCryptfs's 143", len(n), len(base))
			}
		}
	}
}

// createFails can't make a temporary file, for the reason it's given.
type createFails struct {
	FS
	err error
}

func (c createFails) CreateTemp(target string, mode fs.FileMode) (io.WriteCloser, string, error) {
	return nil, "", &fs.PathError{Op: "open", Path: target + ".part", Err: c.err}
}

// 2. Only a refusal to let a file be made (permission, or locally a
// read-only filesystem the file itself is mounted writable into) is worth
// writing in place for. Anything else (no space, no inodes, a quota, a name
// taken) stops with the original untouched: never truncated.
func TestTempCreateErrorsNeverTruncate(t *testing.T) {
	for name, cause := range map[string]error{"no space": syscall.ENOSPC, "quota": syscall.EDQUOT, "exists": syscall.EEXIST, "io": syscall.EIO} {
		for side, mk := range map[string]func(t *testing.T) FS{"local": func(*testing.T) FS { return Local{} }, "remote": func(t *testing.T) FS { return newSftpServerRemote(t) }} {
			t.Run(name+"/"+side, func(t *testing.T) {
				src, dst := t.TempDir(), t.TempDir()
				write(t, filepath.Join(src, "f"), "new", 0o644)
				write(t, filepath.Join(dst, "f"), "old", 0o644)
				_, err := overwrite(t, Local{}, createFails{mk(t), cause}, src, dst, "f", RunOptions{InPlace: map[string]bool{filepath.Join(dst, "f"): true}}, nil)
				var need *InPlaceNeeded
				if err == nil || errors.As(err, &need) || !errors.Is(err, cause) {
					t.Fatalf("err = %v, want a stop for %v", err, cause)
				}
				if !strings.Contains(err.Error(), "left as it was") {
					t.Errorf("err = %q doesn't say the file was left as it was", err)
				}
				if got := readFile(t, filepath.Join(dst, "f")); got != "old" {
					t.Errorf("target = %q, want the original", got)
				}
			})
		}
	}
	// A read-only filesystem here asks about writing in place, as a
	// directory that can't be written does.
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "f"), "new", 0o644)
	write(t, filepath.Join(dst, "f"), "old", 0o644)
	_, err := overwrite(t, Local{}, localCreateFails{err: syscall.EROFS}, src, dst, "f", RunOptions{}, nil)
	var need *InPlaceNeeded
	if !errors.As(err, &need) {
		t.Fatalf("EROFS: err = %v, want the in-place question", err)
	}
	// Over SFTP a read-only filesystem is only "failure": not a reason.
	_, err = overwrite(t, Local{}, createFails{newSftpServerRemote(t), errors.New("sftp: \"Failure\" (SSH_FX_FAILURE)")}, src, dst, "f", RunOptions{}, nil)
	if errors.As(err, &need) || err == nil {
		t.Fatalf("SFTP failure: err = %v, want a stop", err)
	}
}

// localCreateFails is createFails on this machine.
type localCreateFails struct {
	Local
	err error
}

func (c localCreateFails) CreateTemp(target string, mode fs.FileMode) (io.WriteCloser, string, error) {
	return createFails{c.Local, c.err}.CreateTemp(target, mode)
}

// replaceFails can't rename over the target, as with a bind-mounted file.
type replaceFails struct {
	FS
	err error
}

func (r replaceFails) Replace(a, b string) error { return r.err }

// secondOpenFails gives its source once; a second read fails.
type secondOpenFails struct {
	Local
	n *int
}

func (s secondOpenFails) Open(p string) (io.ReadCloser, error) {
	if *s.n++; *s.n > 1 {
		return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EIO}
	}
	return s.Local.Open(p)
}

// 3. A rename that fails leaves the target untouched: the temporary file is
// removed and the file written in place, as v0.23.1 did, saying so.
func TestRenameFailureWritesInPlace(t *testing.T) {
	cases := map[string]func(t *testing.T) FS{
		"local EBUSY": func(*testing.T) FS { return replaceFails{Local{}, &os.LinkError{Op: "rename", Err: syscall.EBUSY}} },
		"openssh SSH_FX_FAILURE": func(t *testing.T) FS {
			return replaceFails{newSftpServerRemote(t), errors.New("sftp: \"Failure\" (SSH_FX_FAILURE)")}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			write(t, filepath.Join(src, "hosts"), "new", 0o644)
			target := filepath.Join(dst, "hosts")
			write(t, target, "old", 0o644)
			before := statT(t, target).Ino
			var last Progress
			res, err := overwrite(t, Local{}, mk(t), src, dst, "hosts", RunOptions{}, func(p Progress) { last = p })
			if err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, target); got != "new" || statT(t, target).Ino != before {
				t.Errorf("target = %q (same inode %v), want the new content written in place", got, statT(t, target).Ino == before)
			}
			w := strings.Join(res.Warnings, "\n")
			if !strings.Contains(w, "couldn't replace") || !strings.Contains(w, "in one step") || !strings.Contains(w, "wrote it in place") {
				t.Errorf("warnings %q don't say it was written in place after the rename failed", res.Warnings)
			}
			if len(res.InPlace) != 1 || res.Bytes != 3 || last.Done != 3 {
				t.Errorf("in place %v, bytes %d, progress %d: want one file, 3 bytes counted once", res.InPlace, res.Bytes, last.Done)
			}
			if l := partFiles(t, dst); len(l) > 0 {
				t.Errorf("left behind: %v", l)
			}
		})
	}
	t.Run("then in place fails", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		write(t, filepath.Join(src, "hosts"), "new", 0o644)
		write(t, filepath.Join(dst, "hosts"), "old", 0o644)
		n := 0
		_, err := overwrite(t, secondOpenFails{n: &n}, replaceFails{Local{}, syscall.EBUSY}, src, dst, "hosts", RunOptions{}, nil)
		if err == nil || !strings.Contains(err.Error(), "in one step") {
			t.Fatalf("err = %v, want it to say the rename failed first", err)
		}
		if readFile(t, filepath.Join(dst, "hosts")) != "old" {
			t.Errorf("the target changed although the source couldn't be read again")
		}
	})
	t.Run("then the write in place fails", func(t *testing.T) {
		src, dst := t.TempDir(), t.TempDir()
		write(t, filepath.Join(src, "hosts"), strings.Repeat("n", 100), 0o644)
		write(t, filepath.Join(dst, "hosts"), "old", 0o644)
		_, err := overwrite(t, Local{}, replaceFails{partialRenameFS{}, syscall.EBUSY}, src, dst, "hosts", RunOptions{}, nil)
		var pw *PartialWrite
		if !errors.As(err, &pw) || !strings.Contains(err.Error(), "may be partly written") || !strings.Contains(err.Error(), "in one step") {
			t.Fatalf("err = %v, want a partial write that says the rename failed first", err)
		}
	})
}

// partialRenameFS makes temporary files normally, but its writes in place
// fail part way.
type partialRenameFS struct{ Local }

func (partialRenameFS) OpenWrite(p string) (io.WriteCloser, error) {
	return partialWriteFS{}.OpenWrite(p)
}

// swapsTemp creates its temporary file, then moves it aside and puts a
// symlink to victim at its name: what another user could do in a
// world-writable directory without the sticky bit.
type swapsTemp struct {
	FS
	victim string
	moved  *string
}

func (s swapsTemp) CreateTemp(target string, mode fs.FileMode) (io.WriteCloser, string, error) {
	w, tmp, err := s.FS.CreateTemp(target, mode)
	if err != nil {
		return w, tmp, err
	}
	*s.moved = tmp + ".moved"
	if err := os.Rename(tmp, *s.moved); err != nil {
		panic(err)
	}
	if err := os.Symlink(s.victim, tmp); err != nil {
		panic(err)
	}
	return w, tmp, err
}

// 4. The temporary file is changed through the handle that made it, never
// by its name, and isn't moved into place if its name now leads elsewhere.
func TestASwappedTempIsNeverFollowed(t *testing.T) {
	for side, mk := range overwriteSides() {
		t.Run(side, func(t *testing.T) {
			src, dst, other := t.TempDir(), t.TempDir(), t.TempDir()
			write(t, filepath.Join(src, "f"), "new", 0o644)
			target := filepath.Join(dst, "f")
			write(t, target, "old", 0o640)
			os.Chmod(target, 0o640)
			victim := filepath.Join(other, "victim")
			write(t, victim, "secret", 0o600)
			os.Chmod(victim, 0o600)
			moved := ""
			_, err := overwrite(t, Local{}, swapsTemp{mk(t), victim, &moved}, src, dst, "f", RunOptions{}, nil)
			if err == nil {
				t.Errorf("a temporary file swapped for a symlink was moved into place")
			}
			if fi, _ := os.Lstat(target); fi == nil || fi.Mode()&fs.ModeSymlink != 0 || readFile(t, target) != "old" {
				t.Errorf("target is now %v, %q; want the original file", fi.Mode(), readFile(t, target))
			}
			if fi, _ := os.Stat(victim); fi.Mode().Perm() != 0o600 || readFile(t, victim) != "secret" {
				t.Errorf("the victim was changed: %v %q", fi.Mode(), readFile(t, victim))
			}
			os.Remove(moved)
		})
	}
}

// 5. setuid and setgid are kept, but only set on the finished copy just
// before it's renamed: a half-written setuid file never exists.
func TestSetuidIsKeptButOnlyAtTheEnd(t *testing.T) {
	for side, mk := range overwriteSides() {
		t.Run(side, func(t *testing.T) {
			dstFS := mk(t)
			src, dst := t.TempDir(), t.TempDir()
			write(t, filepath.Join(src, "tool"), strings.Repeat("x", 64<<10), 0o755)
			target := filepath.Join(dst, "tool")
			write(t, target, "old", 0o755)
			if err := os.Chmod(target, fs.ModeSetuid|0o755); err != nil {
				t.Fatal(err)
			}
			if fi, _ := os.Stat(target); fi.Mode()&fs.ModeSetuid == 0 {
				t.Skip("this filesystem doesn't keep setuid")
			}
			sawTemp, early := false, false
			_, err := overwrite(t, slowSource{}, dstFS, src, dst, "tool", RunOptions{}, func(p Progress) {
				if p.Temp == "" || p.FileDone == 0 {
					return
				}
				if fi, err := os.Stat(p.Temp); err == nil {
					sawTemp = true
					if fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
						early = true
					}
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if !sawTemp {
				t.Fatal("never saw the temporary file while it was written")
			}
			if early {
				t.Errorf("the temporary file was setuid while it was still being written")
			}
			fi, _ := os.Stat(target)
			if fi.Mode()&fs.ModeSetuid == 0 || fi.Mode().Perm() != 0o755 {
				t.Errorf("replaced file mode = %v, want setuid 0755 kept", fi.Mode())
			}
		})
	}
}

// 6. A file that we may not write is never replaced by renaming over it:
// a read-only file stays as it is, as it did when v0.23.1 wrote in place.
func TestAReadOnlyFileIsNeverRenamedOver(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may write anything")
	}
	for side, mk := range overwriteSides() {
		t.Run(side, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			write(t, filepath.Join(src, "f"), "new", 0o644)
			target := filepath.Join(dst, "f")
			write(t, target, "old", 0o444)
			os.Chmod(target, 0o444)
			res, err := overwrite(t, Local{}, mk(t), src, dst, "f", RunOptions{}, nil)
			if got := readFile(t, target); got != "old" {
				t.Errorf("a read-only file was replaced: now %q (err %v, res %+v)", got, err, res)
			}
			if err == nil || !strings.Contains(err.Error(), "left as it was") {
				t.Errorf("err = %v, want it to say the file was left as it was", err)
			}
			if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o444 {
				t.Errorf("mode = %v", fi.Mode())
			}
			if l := partFiles(t, dst); len(l) > 0 {
				t.Errorf("left behind: %v", l)
			}
		})
	}
}

func TestMayWriteHasNoRootShortcutOnAHost(t *testing.T) {
	other := Entry{Mode: 0o644, Uid: 33, Gid: 33, HasOwner: true}
	if ok, known := mayWrite(other, identity{uid: 0, gids: []uint32{0}}, true); ok && known {
		t.Errorf("uid 0 on a host counted as root: a server that sends no owners says 0 for everyone")
	}
	if ok, known := mayWrite(other, identity{uid: 0, rootAll: true, groupsKnown: true}, true); !ok || !known {
		t.Errorf("root on this machine may write anything")
	}
	grp := Entry{Mode: 0o464, Uid: 1, Gid: 4242, HasOwner: true}
	if ok, known := mayWrite(grp, identity{uid: 1000, gids: []uint32{100, 4242}, groupsKnown: true}, true); !ok || !known {
		t.Errorf("a supplementary group's write bit wasn't counted")
	}
}

// removeFails can't remove meterm's temporary files, as when the
// connection is lost part way.
type removeFails struct{ FS }

func (r removeFails) Remove(p string) error {
	if strings.Contains(path.Base(filepath.ToSlash(p)), ".meterm-") {
		return &fs.PathError{Op: "remove", Path: p, Err: errors.New("connection lost")}
	}
	return r.FS.Remove(p)
}

// failWrites hands out temporary files that refuse every write.
type failWrites struct{ FS }

func (f failWrites) CreateTemp(target string, mode fs.FileMode) (io.WriteCloser, string, error) {
	w, tmp, err := f.FS.CreateTemp(target, mode)
	if err != nil {
		return w, tmp, err
	}
	return refuseWrites{w.(*os.File)}, tmp, nil
}

// 7 and 9. A temporary file that can't be removed is named, and a stop says
// exactly what became of the file it was for: unchanged, or not created.
func TestATempThatCantBeRemovedIsNamed(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "f"), "new", 0o644)
	write(t, filepath.Join(src, "g"), "new", 0o644)
	write(t, filepath.Join(dst, "f"), "old", 0o644)
	dstFS := removeFails{failWrites{Local{}}}
	res, err := overwrite(t, Local{}, dstFS, src, dst, "f", RunOptions{}, nil)
	left := partFiles(t, dst)
	if len(left) != 1 {
		t.Fatalf("left %v, want the one temporary file", left)
	}
	msg := err.Error()
	for _, want := range []string{"local " + filepath.Join(dst, "f") + " unchanged", "the unfinished copy is still at local " + filepath.Join(dst, left[0]), "connection lost"} {
		if !strings.Contains(msg, want) {
			t.Errorf("err %q doesn't say %q", msg, want)
		}
	}
	if len(res.Leftovers) != 1 || !strings.Contains(res.Leftovers[0], left[0]) {
		t.Errorf("leftovers = %v", res.Leftovers)
	}
	os.Remove(filepath.Join(dst, left[0]))

	// A new file: v0.23.1's words.
	_, err = overwrite(t, Local{}, dstFS, src, dst, "g", RunOptions{}, nil)
	left = partFiles(t, dst)
	if len(left) != 1 {
		t.Fatalf("left %v", left)
	}
	want := "couldn't remove the unfinished new file local " + filepath.Join(dst, left[0]) + " (connection lost)"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to say %q", err, want)
	}
	if _, serr := os.Lstat(filepath.Join(dst, "g")); serr == nil {
		t.Errorf("a failed new file appeared at its name")
	}
}

// A cancel says, of the file it stopped in, that it's unchanged.
func TestACancelSaysTheFileIsUnchanged(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "big"), strings.Repeat("x", 64<<10), 0o644)
	write(t, filepath.Join(dst, "big"), "old", 0o644)
	plan, _ := MakePlan(context.Background(), slowSource{}, []string{filepath.Join(src, "big")}, Local{}, dst)
	ctx, cancel := context.WithCancel(context.Background())
	_, err := RunWith(ctx, plan, RunOptions{Overwrite: true}, func(p Progress) {
		if p.FileDone > 0 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	var u *Unchanged
	if !errors.As(err, &u) || !strings.Contains(err.Error(), filepath.Join(dst, "big")+" unchanged") {
		t.Errorf("err = %v, want it to say %s is unchanged", err, filepath.Join(dst, "big"))
	}
	if readFile(t, filepath.Join(dst, "big")) != "old" {
		t.Errorf("target changed")
	}
}

// 8. A batch that stops part way says which files it had already copied.
func TestAStoppedBatchSaysWhatItCopied(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		write(t, filepath.Join(src, "d", n), n, 0o644)
	}
	write(t, filepath.Join(dst, "d", "b"), "old", 0o644)
	plan, err := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "d")}, noSpaceFor{Local{}, "c"}, dst)
	if err != nil {
		t.Fatal(err)
	}
	res, err := RunWith(context.Background(), plan, RunOptions{Overwrite: true}, nil)
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("err = %v", err)
	}
	want := []string{filepath.Join(dst, "d", "a"), filepath.Join(dst, "d", "b")}
	if strings.Join(res.Files, ",") != strings.Join(want, ",") {
		t.Errorf("copied files = %v, want %v", res.Files, want)
	}
}

// noSpaceFor has no room for a file called name.
type noSpaceFor struct {
	FS
	name string
}

func (n noSpaceFor) CreateTemp(target string, mode fs.FileMode) (io.WriteCloser, string, error) {
	if filepath.Base(target) == n.name {
		return nil, "", &fs.PathError{Op: "open", Path: target, Err: syscall.ENOSPC}
	}
	return n.FS.CreateTemp(target, mode)
}

// slowSource reads its files 8 KB at a time, 40 ms a read, so a copy is
// seen part way.
type slowSource struct{ Local }

func (slowSource) Open(p string) (io.ReadCloser, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return slowReader{f}, nil
}

type slowReader struct{ *os.File }

func (s slowReader) Read(p []byte) (int, error) {
	time.Sleep(40 * time.Millisecond)
	if len(p) > 8<<10 {
		p = p[:8<<10]
	}
	return s.File.Read(p)
}

// swapsTempHere is swapsTemp on this machine, putting a regular file of the
// same size at the temporary name: only the kernel's own answer (is it the
// same file?) catches that.
type swapsTempHere struct {
	Local
	moved *string
}

func (s swapsTempHere) CreateTemp(target string, mode fs.FileMode) (io.WriteCloser, string, error) {
	w, tmp, err := s.Local.CreateTemp(target, mode)
	if err != nil {
		return w, tmp, err
	}
	*s.moved = tmp + ".moved"
	if err := os.Rename(tmp, *s.moved); err != nil {
		panic(err)
	}
	if err := os.WriteFile(tmp, []byte("bad"), 0o644); err != nil {
		panic(err)
	}
	return w, tmp, nil
}

func TestASwappedTempIsCaughtHere(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "f"), "new", 0o644)
	write(t, filepath.Join(dst, "f"), "old", 0o644)
	moved := ""
	_, err := overwrite(t, Local{}, swapsTempHere{moved: &moved}, src, dst, "f", RunOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "no longer the file that was written") {
		t.Errorf("err = %v, want the swap caught", err)
	}
	if got := readFile(t, filepath.Join(dst, "f")); got != "old" {
		t.Errorf("target = %q", got)
	}
	os.Remove(moved)
}
