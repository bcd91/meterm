//go:build windows

package team

import (
	"os/exec"
	"testing"
)

// git runs without a console window of its own: the Windows CI job checks
// the flags, and runs the whole git store test suite through quiet.
func TestQuietGitHasNoWindow(t *testing.T) {
	c := quiet(exec.Command("git", "--version"))
	if c.SysProcAttr == nil || !c.SysProcAttr.HideWindow || c.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatalf("SysProcAttr = %+v", c.SysProcAttr)
	}
	if out, err := c.Output(); err != nil {
		t.Skipf("git not runnable here: %v %s", err, out)
	}
}
