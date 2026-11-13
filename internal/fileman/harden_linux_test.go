package fileman

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// inNamespace runs the named test again in a new user and mount namespace
// (unshare -Urm), where it may mount things under its own temporary
// directories. It skips where unprivileged namespaces aren't allowed. In
// the namespace it returns true and the test goes on.
func inNamespace(t *testing.T) bool {
	t.Helper()
	test := strings.SplitN(t.Name(), "/", 2)[0]
	if os.Getenv("METERM_TEST_IN_NS") == test {
		return true
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("no unshare")
	}
	if err := exec.Command("unshare", "-Urm", "true").Run(); err != nil {
		t.Skipf("no user namespaces here: %v", err)
	}
	cmd := exec.Command("unshare", "-Urm", os.Args[0], "-test.run=^"+test+"$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), "METERM_TEST_IN_NS="+test)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: "+test) {
		t.Errorf("in a namespace: %v\n%s", err, out)
	}
	return false
}

// bindFile mounts the file from over the file at onto.
func bindFile(t *testing.T, from, onto string) {
	t.Helper()
	if err := syscall.Mount(from, onto, "", syscall.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Unmount(onto, syscall.MNT_DETACH) })
}

// 3, for real: a bind-mounted file (a container's /etc/hosts, docker -v
// file:file) can't be renamed over (EBUSY; over SFTP plain "failure"). It's
// written in place, as v0.23.1 did, and the person is told.
func TestBindMountedFileIsWrittenInPlace(t *testing.T) {
	if !inNamespace(t) {
		return
	}
	for side, mk := range overwriteSides() {
		t.Run(side, func(t *testing.T) {
			src, dst, outside := t.TempDir(), t.TempDir(), t.TempDir()
			write(t, filepath.Join(src, "hosts"), "new", 0o644)
			target := filepath.Join(dst, "hosts")
			write(t, target, "placeholder", 0o644)
			write(t, filepath.Join(outside, "hosts"), "old", 0o644)
			bindFile(t, filepath.Join(outside, "hosts"), target)
			res, err := overwrite(t, Local{}, mk(t), src, dst, "hosts", RunOptions{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(outside, "hosts")); got != "new" {
				t.Errorf("the mounted file = %q, want new", got)
			}
			if w := strings.Join(res.Warnings, "\n"); !strings.Contains(w, "in one step") || !strings.Contains(w, "wrote it in place") {
				t.Errorf("warnings = %q", res.Warnings)
			}
			if l := partFiles(t, dst); len(l) > 0 {
				t.Errorf("left behind: %v", l)
			}
		})
	}
}

// 2, for real: with no inode free, no temporary file can be made, and the
// original stays as it was, never truncated.
func TestNoFreeInodeLeavesTheOriginal(t *testing.T) {
	if !inNamespace(t) {
		return
	}
	for side, mk := range overwriteSides() {
		t.Run(side, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			write(t, filepath.Join(src, "f"), "new", 0o644)
			if err := syscall.Mount("none", dst, "tmpfs", 0, "nr_inodes=2,size=1m"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { syscall.Unmount(dst, syscall.MNT_DETACH) })
			target := filepath.Join(dst, "f") // the root and this use both inodes
			write(t, target, "old", 0o644)
			_, err := overwrite(t, Local{}, mk(t), src, dst, "f", RunOptions{InPlace: map[string]bool{target: true}}, nil)
			var need *InPlaceNeeded
			if err == nil || errors.As(err, &need) || !strings.Contains(err.Error(), "left as it was") {
				t.Errorf("err = %v, want a stop saying the file was left as it was", err)
			}
			if got := readFile(t, target); got != "old" {
				t.Errorf("target = %q, want the original", got)
			}
		})
	}
}

// 2, for real: a file bind-mounted read-write into a read-only filesystem
// (docker --read-only -v file:file, a Kubernetes subPath under
// readOnlyRootFilesystem). Here, EROFS asks about writing in place, and
// once agreed the file is written. Over SFTP the server only says
// "failure", which a full disk says too: that stops.
func TestReadOnlyFilesystemWithAWritableFile(t *testing.T) {
	if !inNamespace(t) {
		return
	}
	for side, mk := range overwriteSides() {
		t.Run(side, func(t *testing.T) {
			src, dst, outside := t.TempDir(), t.TempDir(), t.TempDir()
			write(t, filepath.Join(src, "app.conf"), "new", 0o644)
			if err := syscall.Mount("none", dst, "tmpfs", 0, "size=1m"); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { syscall.Unmount(dst, syscall.MNT_DETACH) })
			target := filepath.Join(dst, "app.conf")
			write(t, target, "placeholder", 0o644)
			write(t, filepath.Join(outside, "app.conf"), "old", 0o644)
			bindFile(t, filepath.Join(outside, "app.conf"), target)
			if err := syscall.Mount("", dst, "", syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
				t.Fatal(err)
			}
			dstFS := mk(t)
			_, err := overwrite(t, Local{}, dstFS, src, dst, "app.conf", RunOptions{}, nil)
			var need *InPlaceNeeded
			if side != "local" {
				if err == nil || errors.As(err, &need) || !strings.Contains(err.Error(), "left as it was") {
					t.Errorf("err = %v, want a stop", err)
				}
				if side == "openssh-sftp-srv" && !strings.Contains(err.Error(), "doesn't say why") {
					t.Errorf("err = %v, want it to say the server gave no reason", err)
				}
				if got := readFile(t, filepath.Join(outside, "app.conf")); got != "old" {
					t.Errorf("file = %q", got)
				}
				return
			}
			if !errors.As(err, &need) {
				t.Fatalf("err = %v, want the in-place question", err)
			}
			if _, err := overwrite(t, Local{}, dstFS, src, dst, "app.conf", RunOptions{InPlace: map[string]bool{need.Target: true}}, nil); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(outside, "app.conf")); got != "new" {
				t.Errorf("file = %q, want new", got)
			}
		})
	}
}
