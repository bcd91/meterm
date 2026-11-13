package main

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// Starting meterm must not ask the terminal anything: Bubble Tea's init
// asks for the background colour (OSC 11) and, on a terminal that never
// answers, waited five seconds before every command, `meterm list`
// included. Under script(1) with nothing answering, the test binary (which
// links the same packages, inits and all) must start at once.
func TestStartupAsksTheTerminalNothing(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses util-linux script(1)")
	}
	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("no script(1)")
	}
	c := exec.Command(script, "-qec", os.Args[0]+" -test.run=^$", "/dev/null")
	c.Env = append(os.Environ(), "TERM=xterm-256color", "COLORFGBG=")
	start := time.Now()
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("starting took %s: something asked the terminal and waited", took.Round(time.Millisecond))
	}
}
