package safefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// However often it's replaced, a reader gets a whole file and a writer succeeds.
func TestReadersAndWritersTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := WriteFile(path, []byte("start")); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	for w := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-done:
					return
				default:
				}
				if err := WriteFile(path, fmt.Appendf(nil, "%d-%d", w, i)); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}()
	}
	for range 500 {
		b, err := ReadFile(path)
		if err != nil || len(b) == 0 {
			t.Errorf("read: %q, %v", b, err)
			break
		}
	}
	close(done)
	wg.Wait()
	names, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(names) != 1 {
		t.Errorf("left behind: %v, %v", names, err)
	}
}

func TestErrorsThatArentSharingComeBackAtOnce(t *testing.T) {
	if _, err := ReadFile(filepath.Join(t.TempDir(), "none")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("read: %v", err)
	}
	if err := Remove(filepath.Join(t.TempDir(), "none")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("remove: %v", err)
	}
}
