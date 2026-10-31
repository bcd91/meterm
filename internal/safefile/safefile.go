// Package safefile reads and replaces small files that other meterm processes
// use at the same time.
//
// On Unix a rename over an open file just works. On Windows a file that's open
// can't be replaced or removed, and one being replaced can't be opened, so the
// loser gets a sharing violation or "access denied" for the moment the other
// holds it. These functions try again for those errors, for a limited time. A
// real permission problem looks the same on Windows, so it still comes back as
// the error it is, only later.
package safefile

import (
	"os"
	"path/filepath"
	"time"
)

// patience is how long a Windows sharing error is given to pass.
const patience = 3 * time.Second

// retry runs op until it succeeds, fails for another reason, or patience runs out.
func retry(op func() error) error {
	deadline := time.Now().Add(patience)
	wait := time.Millisecond
	for {
		err := op()
		if err == nil || !transient(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(wait)
		wait = min(wait*2, 50*time.Millisecond)
	}
}

// ReadFile is os.ReadFile, waiting out a writer on Windows.
func ReadFile(path string) ([]byte, error) {
	var data []byte
	err := retry(func() error {
		var err error
		data, err = os.ReadFile(path)
		return err
	})
	return data, err
}

// Rename is os.Rename, waiting out readers on Windows.
func Rename(from, to string) error {
	return retry(func() error { return os.Rename(from, to) })
}

// Remove is os.Remove, waiting out readers on Windows.
func Remove(path string) error {
	return retry(func() error { return os.Remove(path) })
}

// WriteFile replaces path in one step, private to this user. A temp file of its
// own means writers can't trip over each other's.
func WriteFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
