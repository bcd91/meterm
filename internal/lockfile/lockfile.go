// Package lockfile serializes writers across meterm processes with a lock the
// operating system holds on an open file: flock on Unix, LockFileEx on Windows.
// The kernel releases it when the holder exits, however that happens, so a
// crash leaves nothing to clean up and nothing has to guess that a lock is
// stale. The file itself stays where it is and means nothing by being there;
// removing it would let one process lock a file another can no longer find.
package lockfile

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrBusy is returned by Acquire when another process still holds the lock
// after waiting.
var ErrBusy = errors.New("locked by another meterm")

// errHeld is what tryLock says when someone else has the lock.
var errHeld = errors.New("lock held")

// Acquire takes the lock at path, creating its directory if needed. It keeps
// trying every poll for up to wait (0 means one attempt), then returns ErrBusy.
// A lock that can't be tried at all, such as a file that can't be opened, is an
// error straight away. Calling unlock twice is harmless.
func Acquire(path string, wait, poll time.Duration) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := tryLock(f)
		if err == nil {
			var once sync.Once
			return func() {
				once.Do(func() {
					_ = releaseLock(f)
					f.Close()
				})
			}, nil
		}
		if !errors.Is(err, errHeld) {
			f.Close()
			return nil, err
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, ErrBusy
		}
		time.Sleep(poll)
	}
}
