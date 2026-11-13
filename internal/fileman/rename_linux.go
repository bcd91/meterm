package fileman

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace asks the kernel to refuse an existing target
// (RENAME_NOREPLACE), so nothing can slip in between a check and the rename.
// Filesystems without the flag get the check-then-rename fallback.
func renameNoReplace(a, b string) error {
	err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		return checkThenRename(func(p string) error { _, err := os.Lstat(p); return err }, os.Rename, a, b)
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: a, New: b, Err: err}
	}
	return nil
}
