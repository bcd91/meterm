package team

import (
	"bytes"
	"testing"
)

func TestDebugTrace(t *testing.T) {
	var buf bytes.Buffer
	SetDebug(&buf)
	defer SetDebug(nil)
	debugf("hello %d", 1)
	if buf.String() != "team: hello 1\n" {
		t.Errorf("trace = %q", buf.String())
	}
}
