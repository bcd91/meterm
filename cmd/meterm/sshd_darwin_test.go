package main

import "os/exec"

// dieWithTest can't ask macOS to kill c with the test; t.Cleanup still does on a
// normal end.
func dieWithTest(*exec.Cmd) {}
