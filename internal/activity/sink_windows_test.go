package activity

import (
	"testing"
	"time"

	"meterm/internal/lockfile"
)

func holdSinkLock(t *testing.T, l *Log) func() {
	t.Helper()
	unlock, err := lockfile.Acquire(l.sinkLockPath(), time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return unlock
}
