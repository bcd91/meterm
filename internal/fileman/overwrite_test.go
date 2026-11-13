//go:build !windows

package fileman

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkg/sftp"
)

// opensshRemote serves the local disk through OpenSSH's own sftp-server,
// the program a real host runs, when it's installed here.
func opensshRemote(t *testing.T) Remote {
	t.Helper()
	bin := "/usr/lib/openssh/sftp-server"
	if _, err := os.Stat(bin); err != nil {
		t.Skip("no OpenSSH sftp-server here")
	}
	cmd := exec.Command(bin)
	w, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	r, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c, err := sftp.NewClientPipe(r, w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(); cmd.Wait() })
	return Remote{Host: "openssh", Client: c}
}

func destinations(t *testing.T) map[string]FS {
	return map[string]FS{"local": Local{}, "pkg-sftp": newRemote(t), "openssh": opensshRemote(t)}
}

// failingFS hands out a source that breaks after its first chunk, the way a
// dropped connection or a read error does.
type failingFS struct{ Local }

func (failingFS) Open(p string) (io.ReadCloser, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	return &failAfterFirstRead{File: f}, nil
}

type failAfterFirstRead struct {
	*os.File
	n int
}

func (f *failAfterFirstRead) Read(p []byte) (int, error) {
	if f.n > 0 {
		return 0, errors.New("connection lost")
	}
	f.n++
	return f.File.Read(p)
}

const big = 8 << 20

// v0.23.1's promises, kept with atomic replace: an existing file being
// overwritten is never removed. A copy stopped at once or failing part way
// leaves it exactly as it was (the new bytes went to a temporary file).
func TestStoppedOverwriteKeepsTheFile(t *testing.T) {
	for name, dstFS := range destinations(t) {
		for _, how := range []string{"stopped", "failed"} {
			t.Run(name+"/"+how, func(t *testing.T) {
				src, dst := t.TempDir(), t.TempDir()
				write(t, filepath.Join(src, "big.bin"), string(bytes.Repeat([]byte("n"), big)), 0o644)
				target := filepath.Join(dst, "big.bin")
				write(t, target, "the original", 0o644)
				now, stop := context.WithCancel(context.Background())
				defer stop()
				var from FS = cancellingFS{cancel: stop}
				if how == "failed" {
					from = failingFS{}
				}
				plan, _ := MakePlan(context.Background(), from, []string{filepath.Join(src, "big.bin")}, dstFS, dst)
				_, err := RunWith(now, plan, RunOptions{Overwrite: true}, nil)
				if err == nil {
					t.Fatal("no error")
				}
				if how == "stopped" && !errors.Is(err, context.Canceled) {
					t.Errorf("err = %v, want it to be a cancel", err)
				}
				if got, err := os.ReadFile(target); err != nil || string(got) != "the original" {
					t.Errorf("the file holds %d bytes (%v), want the original", len(got), err)
				}
				if names := dirNames(t, dst); len(names) != 1 {
					t.Errorf("dst holds %v, want only big.bin", names)
				}
			})
		}
	}
}

// The first esc (After) stops a file going to a temporary name at once:
// nothing new appears, and nothing is left behind.
func TestFirstStopDiscardsANewFileAtOnce(t *testing.T) {
	for name, dstFS := range destinations(t) {
		t.Run(name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			write(t, filepath.Join(src, "new.bin"), string(bytes.Repeat([]byte("n"), big)), 0o644)
			now, cancelNow := context.WithCancel(context.Background())
			defer cancelNow()
			after, stop := context.WithCancel(now)
			plan, _ := MakePlan(context.Background(), cancellingFS{cancel: stop}, []string{filepath.Join(src, "new.bin")}, dstFS, dst)
			_, err := RunWith(now, plan, RunOptions{Overwrite: true, After: after}, nil)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want a cancel", err)
			}
			if names := dirNames(t, dst); len(names) != 0 {
				t.Errorf("dst holds %v, want nothing", names)
			}
		})
	}
}

// A file being written in place (agreed to, as its directory can't take a
// temporary file) is let finish by the first esc, and the copy stops
// before the next; the second esc (now) stops it at once, and it is left
// in place, said to be partly written.
func TestFirstStopFinishesAFileWrittenInPlace(t *testing.T) {
	for name, mk := range transferSides() {
		t.Run(name, func(t *testing.T) {
			dstFS := mk(t)
			ro := readOnlyDir(t, map[string]os.FileMode{"a.bin": 0o666, "b.bin": 0o666})
			src := t.TempDir()
			content := string(bytes.Repeat([]byte("n"), big))
			write(t, filepath.Join(src, "a.bin"), content, 0o644)
			write(t, filepath.Join(src, "b.bin"), "new b", 0o644)
			now, cancelNow := context.WithCancel(context.Background())
			defer cancelNow()
			after, stop := context.WithCancel(now)
			plan, err := MakePlan(context.Background(), cancellingFS{cancel: stop}, []string{filepath.Join(src, "a.bin"), filepath.Join(src, "b.bin")}, dstFS, ro)
			if err != nil {
				t.Fatal(err)
			}
			in := map[string]bool{filepath.Join(ro, "a.bin"): true, filepath.Join(ro, "b.bin"): true}
			res, err := RunWith(now, plan, RunOptions{Overwrite: true, InPlace: in, After: after}, nil)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want a cancel", err)
			}
			if got, _ := os.ReadFile(filepath.Join(ro, "a.bin")); string(got) != content {
				t.Errorf("a.bin holds %d bytes, want it finished (%d)", len(got), len(content))
			}
			if got, _ := os.ReadFile(filepath.Join(ro, "b.bin")); string(got) != "old b.bin" {
				t.Errorf("b.bin = %q, want it untouched after the stop", got)
			}
			if res.Copied != 1 || len(res.InPlace) != 1 {
				t.Errorf("res = %+v, want a.bin alone, in place", res)
			}
		})
	}
}

func TestSecondStopLeavesAFileWrittenInPlacePartlyWritten(t *testing.T) {
	for name, mk := range transferSides() {
		t.Run(name, func(t *testing.T) {
			dstFS := mk(t)
			ro := readOnlyDir(t, map[string]os.FileMode{"a.bin": 0o666})
			src := t.TempDir()
			write(t, filepath.Join(src, "a.bin"), string(bytes.Repeat([]byte("n"), big)), 0o644)
			now, cancelNow := context.WithCancel(context.Background())
			defer cancelNow()
			plan, err := MakePlan(context.Background(), cancellingFS{cancel: cancelNow}, []string{filepath.Join(src, "a.bin")}, dstFS, ro)
			if err != nil {
				t.Fatal(err)
			}
			_, err = RunWith(now, plan, RunOptions{Overwrite: true, InPlace: map[string]bool{filepath.Join(ro, "a.bin"): true}}, nil)
			var pw *PartialWrite
			if !errors.As(err, &pw) || !strings.Contains(err.Error(), "partly written") {
				t.Errorf("err = %v, want it to say a.bin may be partly written", err)
			}
			if _, err := os.Stat(filepath.Join(ro, "a.bin")); err != nil {
				t.Errorf("the file was removed: %v", err)
			}
		})
	}
}
