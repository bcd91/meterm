//go:build !windows

package main

import "os"

// keyFileMode is the permission bits of a key file, as ssh checks them.
func keyFileMode(p string) os.FileMode {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Mode().Perm()
}
