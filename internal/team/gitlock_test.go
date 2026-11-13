package team

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"meterm/internal/lockfile"
)

// twoProcesses is two stores on one clone, as a background autosync and a
// foreground team command on the same machine have: each its own mutex,
// one directory.
func twoProcesses(t *testing.T) (a, b *GitStore) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
	alice, _ := gitTeam(t)
	other := &GitStore{ID: alice.ID, Name: alice.Name, Remote: alice.Remote, Branch: alice.Branch, Dir: alice.Dir}
	other.identityFn = alice.identityFn
	return alice, other
}

// Quick commands in a row ran a background fetch/reset into a foreground
// change on the same clone: "cannot lock ref", "index.lock: File exists".
// Git work on a clone is one at a time across processes.
func TestGitWorkOnOneCloneIsSerialised(t *testing.T) {
	a, b := twoProcesses(t)
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for w, g := range []*GitStore{a, b} {
		wg.Add(1)
		go func(w int, g *GitStore) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				if err := g.change(ctx, fmt.Sprintf("w%d %d", w, i), func() error {
					return g.writeFile(fmt.Sprintf("x/%d-%d", w, i), []byte("x"))
				}); err != nil {
					errs <- err
				}
				if done, err := g.read(ctx); err != nil {
					errs <- err
				} else {
					done()
				}
			}
		}(w, g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(a.Dir, ".git", "index.lock")); !os.IsNotExist(err) {
		t.Errorf("index.lock left behind: %v", err)
	}
}

// When another meterm keeps the clone past the wait, the error says what's
// going on and what to do, rather than git's own.
func TestBusyCloneSaysRetry(t *testing.T) {
	a, _ := twoProcesses(t)
	old := repoLockWait
	repoLockWait = 100 * time.Millisecond
	t.Cleanup(func() { repoLockWait = old })
	unlock, err := lockfile.Acquire(a.Dir+".lock", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = a.read(ctx)
	if !errors.Is(err, ErrRepoBusy) || !strings.Contains(err.Error(), "team sync busy") || !strings.Contains(err.Error(), "retry") {
		t.Errorf("read while busy: %v", err)
	}
}

// A git killed mid-command (a cancelled sync) leaves its index.lock; the
// next meterm to take the clone clears it rather than failing on it.
func TestStaleIndexLockIsCleared(t *testing.T) {
	a, _ := twoProcesses(t)
	for _, f := range []string{"index.lock", "HEAD.lock", filepath.Join("refs", "remotes", "origin", "main.lock")} {
		p := filepath.Join(a.Dir, ".git", f)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.change(ctx, "after a crash", func() error { return a.writeFile("y", []byte("y")) }); err != nil {
		t.Fatalf("change with stale locks: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.Dir, ".git", "index.lock")); !os.IsNotExist(err) {
		t.Errorf("index.lock still there: %v", err)
	}
}

// A busy clone isn't a team you're not in: Router.Teams still lists it,
// saying it's busy, and the other teams as usual.
func TestRouterTeamsKeepsABusyTeam(t *testing.T) {
	a, _ := twoProcesses(t)
	defer SetRepoLockWait(100 * time.Millisecond)()
	r := &Router{Git: map[string]*GitStore{a.ID: a}}
	unlock, err := lockfile.Acquire(a.Dir+".lock", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	teams, err := r.Teams(ctx)
	unlock()
	if err != nil || len(teams) != 1 || teams[0].ID != a.ID || teams[0].Name != "ACME" || !errors.Is(teams[0].Err, ErrRepoBusy) {
		t.Fatalf("busy: teams %+v, err %v; want ACME, busy", teams, err)
	}
	teams, err = r.Teams(ctx)
	if err != nil || len(teams) != 1 || teams[0].Err != nil || teams[0].Role != RoleOwner {
		t.Errorf("after: teams %+v, err %v", teams, err)
	}
}
