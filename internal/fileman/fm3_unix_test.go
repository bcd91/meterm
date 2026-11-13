//go:build unix

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
	"syscall"
	"testing"
)

// otherGroup is a group this user is in that isn't their own: a file can
// be given it, and a new file doesn't get it.
func otherGroup(t *testing.T) int {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs Unix owners")
	}
	gs, _ := os.Getgroups()
	for _, g := range gs {
		if g != os.Getgid() {
			return g
		}
	}
	t.Skip("this user is in no group but their own")
	return 0
}

func statT(t *testing.T, p string) *syscall.Stat_t {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t)
}

// Overwriting a file whose group isn't the one a new file gets keeps its
// group (and mode), when the group is one we can give.
func TestOverwriteKeepsTheGroup(t *testing.T) {
	for name, mk := range transferSides() {
		t.Run(name, func(t *testing.T) {
			dstFS := mk(t)
			g := otherGroup(t)
			src, dst := t.TempDir(), t.TempDir()
			write(t, filepath.Join(src, "a.txt"), "new", 0o644)
			write(t, filepath.Join(dst, "a.txt"), "old", 0o640)
			os.Chmod(filepath.Join(dst, "a.txt"), 0o640)
			if err := os.Chown(filepath.Join(dst, "a.txt"), -1, g); err != nil {
				t.Fatal(err)
			}
			plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "a.txt")}, dstFS, dst)
			res, err := Run(context.Background(), plan, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			st := statT(t, filepath.Join(dst, "a.txt"))
			if int(st.Gid) != g || st.Mode&0o777 != 0o640 {
				t.Errorf("after overwrite: gid %d mode %o; want gid %d mode 640 kept", st.Gid, st.Mode&0o777, g)
			}
			if got, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(got) != "new" || res.Copied != 1 {
				t.Errorf("a.txt = %q, result %+v", got, res)
			}
		})
	}
}

// A file with more than one hard link is not split without asking: the
// copy stops with *ReplaceLoses, and i, r and s each do what they say.
func TestOverwriteAsksBeforeSplittingHardLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs Unix hard link counts")
	}
	setup := func(t *testing.T) (Plan, string, string) {
		src, dst := t.TempDir(), t.TempDir()
		write(t, filepath.Join(src, "a.txt"), "new", 0o644)
		write(t, filepath.Join(dst, "a.txt"), "old", 0o644)
		if err := os.Link(filepath.Join(dst, "a.txt"), filepath.Join(dst, "hardlink-a")); err != nil {
			t.Fatal(err)
		}
		plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "a.txt")}, Local{}, dst)
		return plan, filepath.Join(dst, "a.txt"), filepath.Join(dst, "hardlink-a")
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }

	plan, a, hl := setup(t)
	ino := statT(t, a).Ino
	_, err := RunWith(context.Background(), plan, RunOptions{Overwrite: true}, nil)
	var loses *ReplaceLoses
	if !errors.As(err, &loses) {
		t.Fatalf("err = %v, want *ReplaceLoses", err)
	}
	if loses.Links != 2 || loses.Target != a || !loses.InPlaceOK || loses.Index != 0 {
		t.Errorf("ReplaceLoses %+v", loses)
	}
	if !strings.Contains(err.Error(), "2 hard links") {
		t.Errorf("error %q doesn't say the links", err)
	}
	if read(a) != "old" || len(dirNames(t, filepath.Dir(a))) != 2 {
		t.Fatalf("before an answer: a.txt %q, dir %v", read(a), dirNames(t, filepath.Dir(a)))
	}

	res, err := RunWith(context.Background(), plan, RunOptions{Overwrite: true, OnLoss: LossInPlace}, nil)
	if err != nil || read(a) != "new" || read(hl) != "new" || statT(t, a).Ino != ino || len(res.InPlace) != 1 {
		t.Errorf("in place: err %v, a %q, link %q, same inode %v, result %+v", err, read(a), read(hl), statT(t, a).Ino == ino, res)
	}
	if n := len(dirNames(t, filepath.Dir(a))); n != 2 {
		t.Errorf("in place left %v", dirNames(t, filepath.Dir(a)))
	}

	plan, a, hl = setup(t)
	res, err = RunWith(context.Background(), plan, RunOptions{Overwrite: true, OnLoss: LossReplace}, nil)
	if err != nil || read(a) != "new" || read(hl) != "old" || len(res.Lost) != 1 || !strings.Contains(res.Lost[0], "hard link") {
		t.Errorf("replace: err %v, a %q, link %q, result %+v", err, read(a), read(hl), res)
	}

	plan, a, hl = setup(t)
	res, err = RunWith(context.Background(), plan, RunOptions{Overwrite: true, OnLoss: LossSkip}, nil)
	if err != nil || read(a) != "old" || res.Skipped != 1 || res.Copied != 0 || len(dirNames(t, filepath.Dir(a))) != 2 {
		t.Errorf("skip: err %v, a %q, result %+v, dir %v", err, read(a), res, dirNames(t, filepath.Dir(a)))
	}
}

// noChownFS hands out temporary files whose owner can't be changed, as for
// anyone but root when the group isn't theirs.
type noChownFS struct{ Local }

type refuseChown struct{ *os.File }

func (refuseChown) Chown(int, int) error { return fs.ErrPermission }

func (n noChownFS) CreateTemp(target string, mode fs.FileMode) (io.WriteCloser, string, error) {
	f, tmp, err := n.Local.CreateTemp(target, mode)
	if err != nil {
		return nil, "", err
	}
	return refuseChown{f.(*os.File)}, tmp, nil
}

// When the group can't be kept, the copy asks rather than change it.
func TestOverwriteAsksWhenTheGroupCantBeKept(t *testing.T) {
	g := otherGroup(t)
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "a.txt"), "new", 0o644)
	write(t, filepath.Join(dst, "a.txt"), "old", 0o640)
	os.Chown(filepath.Join(dst, "a.txt"), -1, g)
	plan, _ := MakePlan(context.Background(), Local{}, []string{filepath.Join(src, "a.txt")}, noChownFS{}, dst)
	_, err := RunWith(context.Background(), plan, RunOptions{Overwrite: true}, nil)
	var loses *ReplaceLoses
	if !errors.As(err, &loses) {
		t.Fatalf("err = %v, want *ReplaceLoses", err)
	}
	want := fmt.Sprintf("group %d", g)
	if !strings.Contains(err.Error(), want) || loses.Links > 1 {
		t.Errorf("error %q doesn't say %q (links %d)", err, want, loses.Links)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(got) != "old" || len(dirNames(t, dst)) != 1 {
		t.Errorf("a.txt %q, dir %v", got, dirNames(t, dst))
	}
}
