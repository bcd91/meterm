//go:build windows

package main

import "os"

// keyFileMode is 0 on Windows: the bits don't say who can read a file there, and
// `agents` doesn't run on Windows anyway.
func keyFileMode(string) os.FileMode { return 0 }
