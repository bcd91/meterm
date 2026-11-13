package team

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"meterm/internal/lockfile"
)

// repoLockWait is how long git work on a team's clone waits for another
// meterm's (a background autosync, a team command in another terminal)
// before giving up with ErrRepoBusy.
var repoLockWait = 10 * time.Second

// ErrRepoBusy is git work that couldn't start because another meterm kept
// the team's clone busy past repoLockWait.
var ErrRepoBusy = errors.New("team sync busy: another meterm is using the team's repository; retry in a moment")

// lockRepo takes the clone for git work: this process's other goroutines
// (g.mu) and other meterm processes (a file lock next to the clone, held by
// the kernel, so a crash gives it up). Git's own locks can't do this: two
// gits on one clone fail on each other's index.lock or ref locks instead of
// waiting. Holding it, any git lock file in the clone is a leftover from a
// git that was killed (a cancelled sync), and is cleared on the way in and
// on the way out.
func (g *GitStore) lockRepo() (unlock func(), err error) {
	g.mu.Lock()
	release, err := lockfile.Acquire(g.Dir+".lock", repoLockWait, 50*time.Millisecond)
	if err != nil {
		g.mu.Unlock()
		if errors.Is(err, lockfile.ErrBusy) {
			return nil, ErrRepoBusy
		}
		return nil, err
	}
	clearGitLocks(g.Dir)
	return func() {
		clearGitLocks(g.Dir)
		release()
		g.mu.Unlock()
	}, nil
}

// clearGitLocks removes the lock files a git leaves in dir's .git when it
// dies mid-command: the index's, HEAD's and other top-level ones, and the
// refs'. Only call it holding lockRepo.
func clearGitLocks(dir string) {
	gitDir := filepath.Join(dir, ".git")
	if top, err := filepath.Glob(filepath.Join(gitDir, "*.lock")); err == nil {
		for _, p := range top {
			_ = os.Remove(p)
		}
	}
	_ = filepath.WalkDir(filepath.Join(gitDir, "refs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".lock") {
			_ = os.Remove(p)
		}
		return nil
	})
}

// lockClone serialises the first clone into dir, which has no store yet.
func lockClone(dir string) (unlock func(), err error) {
	release, err := lockfile.Acquire(dir+".lock", repoLockWait, 50*time.Millisecond)
	if errors.Is(err, lockfile.ErrBusy) {
		return nil, ErrRepoBusy
	}
	return release, err
}

// SetRepoLockWait sets how long git work waits for a busy clone, and
// returns a func that puts it back: for tests elsewhere, which can't wait
// the real ten seconds per command.
func SetRepoLockWait(d time.Duration) (restore func()) {
	old := repoLockWait
	repoLockWait = d
	return func() { repoLockWait = old }
}
