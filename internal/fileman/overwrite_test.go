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

// A copy over an existing file that's stopped at once (the second esc) or
// fails leaves the file where it is, and says it may be partly written.
func TestStoppedOverwriteKeepsTheFile(t *testing.T) {
	for name, dstFS := range destinations(t) {
		for _, how := range []string{"stopped", "failed"} {
			t.Run(name+"/"+how, func(t *testing.T) {
				src, dst := t.TempDir(), t.TempDir()
				write(t, filepath.Join(src, "big.bin"), string(bytes.Repeat([]byte("n"), big)), 0o644)
				target := filepath.Join(dst, "big.bin")
				write(t, target, "the original", 0o644)
				now, stop := context.WithCancel(context.Background())
				var from FS = cancellingFS{cancel: stop}
				if how == "failed" {
					from = failingFS{}
				}
				plan, _ := MakePlan(context.Background(), from, []string{filepath.Join(src, "big.bin")}, dstFS, dst)
				_, err := runPlan(now, now, plan, true, nil)
				var pw *PartlyWrittenError
				if !errors.As(err, &pw) || pw.Path != target {
					t.Errorf("err = %v, want a PartlyWrittenError for %s", err, target)
				}
				if how == "stopped" && !errors.Is(err, context.Canceled) {
					t.Errorf("err = %v, want it to be a cancel", err)
				}
				if _, err := os.Stat(target); err != nil {
					t.Errorf("the file was removed: %v", err)
				}
			})
		}
	}
}

// The first esc during an overwrite lets that file finish, then stops.
func TestFirstStopFinishesTheFileBeingOverwritten(t *testing.T) {
	for name, dstFS := range destinations(t) {
		t.Run(name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			content := string(bytes.Repeat([]byte("n"), big))
			write(t, filepath.Join(src, "a.bin"), content, 0o644)
			write(t, filepath.Join(src, "b.bin"), "b", 0o644)
			write(t, filepath.Join(dst, "a.bin"), "the original", 0o644)
			now, cancelNow := context.WithCancel(context.Background())
			defer cancelNow()
			after, stop := context.WithCancel(now)
			plan, _ := MakePlan(context.Background(), cancellingFS{cancel: stop}, []string{filepath.Join(src, "a.bin"), filepath.Join(src, "b.bin")}, dstFS, dst)
			var seen Progress
			res, err := runPlan(now, after, plan, true, func(p Progress) {
				if p.File != "" {
					seen = p
				}
			})
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want a cancel", err)
			}
			if got, _ := os.ReadFile(filepath.Join(dst, "a.bin")); string(got) != content {
				t.Errorf("a.bin holds %d bytes, want it finished (%d)", len(got), len(content))
			}
			if _, err := os.Stat(filepath.Join(dst, "b.bin")); err == nil {
				t.Error("b.bin was copied after the stop")
			}
			if len(res.Files) != 1 || res.Files[0] != filepath.Join(dst, "a.bin") {
				t.Errorf("Files = %q, want just a.bin", res.Files)
			}
			if res.Copied != 1 || !seen.Overwriting || seen.Dst != filepath.Join(dst, "a.bin") {
				t.Errorf("res = %+v, progress = %+v", res, seen)
			}
		})
	}
}

// A new file stops at once on the first esc, and the partial file (ours)
// is removed and named.
func TestFirstStopRemovesAPartialNewFile(t *testing.T) {
	for name, dstFS := range destinations(t) {
		t.Run(name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			write(t, filepath.Join(src, "new.bin"), string(bytes.Repeat([]byte("n"), big)), 0o644)
			now, cancelNow := context.WithCancel(context.Background())
			defer cancelNow()
			after, stop := context.WithCancel(now)
			plan, _ := MakePlan(context.Background(), cancellingFS{cancel: stop}, []string{filepath.Join(src, "new.bin")}, dstFS, dst)
			res, err := runPlan(now, after, plan, true, nil)
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want a cancel", err)
			}
			if _, err := os.Stat(filepath.Join(dst, "new.bin")); !os.IsNotExist(err) {
				t.Errorf("partial new file left behind (%v)", err)
			}
			if res.Partial != filepath.Join(dst, "new.bin") || res.PartialErr != nil {
				t.Errorf("Partial = %q, PartialErr = %v", res.Partial, res.PartialErr)
			}
		})
	}
}

// cantRemove has lost the connection by the time the partial file is to go.
type cantRemove struct{ Local }

func (cantRemove) Remove(string) error { return errors.New("connection lost") }

func TestAPartialNewFileThatCantBeRemovedIsNamed(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "new.bin"), string(bytes.Repeat([]byte("n"), big)), 0o644)
	now, cancelNow := context.WithCancel(context.Background())
	defer cancelNow()
	plan, _ := MakePlan(context.Background(), cancellingFS{cancel: cancelNow}, []string{filepath.Join(src, "new.bin")}, cantRemove{}, dst)
	res, _ := runPlan(now, now, plan, true, nil)
	if res.Partial != filepath.Join(dst, "new.bin") || res.PartialErr == nil {
		t.Errorf("Partial = %q, PartialErr = %v; want the file and why it's still there", res.Partial, res.PartialErr)
	}
}
