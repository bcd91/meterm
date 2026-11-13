//go:build !linux && !darwin

package fileman

import "os"

// renameNoReplace looks before renaming: os.Rename replaces an existing
// target here, and there's no portable way to ask it not to.
func renameNoReplace(a, b string) error {
	return checkThenRename(func(p string) error { _, err := os.Lstat(p); return err }, os.Rename, a, b)
}
