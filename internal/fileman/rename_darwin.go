package fileman

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace asks the kernel to refuse an existing target
// (RENAME_EXCL), so nothing can slip in between a check and the rename.
// Filesystems without the flag get the check-then-rename fallback.
func renameNoReplace(a, b string) error {
	err := unix.RenameatxNp(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCL)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
		return checkThenRename(func(p string) error { _, err := os.Lstat(p); return err }, os.Rename, a, b)
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: a, New: b, Err: err}
	}
	return nil
}
