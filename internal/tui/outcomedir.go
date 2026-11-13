package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// outcomePrefix starts the name of a picker's outcome directory (see
// outcomeFile): meterm-tui-<pid>-<random>, so a later picker can tell when
// the one that made it is gone.
const outcomePrefix = "meterm-tui-"

// newOutcomeDir makes this picker's outcome directory in tmp ("": the
// system's), private to this user, its name carrying this process's PID.
func newOutcomeDir(tmp string) (string, error) {
	return os.MkdirTemp(tmp, fmt.Sprintf("%s%d-", outcomePrefix, os.Getpid()))
}

// sweepOutcomeDirs removes, from tmp, the outcome directories of pickers
// that ended without cleaning up (killed, or their terminal closed): a
// directory, not a link, owned by this user, named for a PID that isn't
// running, and holding nothing but what a picker leaves there (the session
// file, unused production tokens). Anything else is left as it is: another
// user's, a running picker's, or one with a file meterm didn't put there.
// A directory named the old way, with no PID, is left too.
func sweepOutcomeDirs(tmp string) {
	if tmp == "" {
		tmp = os.TempDir()
	}
	es, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	for _, e := range es {
		rest, ok := strings.CutPrefix(e.Name(), outcomePrefix)
		if !ok {
			continue
		}
		pidText, _, ok := strings.Cut(rest, "-")
		pid, err := strconv.Atoi(pidText)
		if !ok || err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		dir := filepath.Join(tmp, e.Name())
		fi, err := os.Lstat(dir)
		if err != nil || !fi.IsDir() || !ownedByUs(fi) || processRunning(pid) {
			continue
		}
		inside, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		ours := true
		for _, f := range inside {
			if !f.Type().IsRegular() || (f.Name() != "session" && !strings.HasPrefix(f.Name(), "prod-")) {
				ours = false
			}
		}
		if !ours {
			continue
		}
		for _, f := range inside {
			_ = os.Remove(filepath.Join(dir, f.Name()))
		}
		_ = os.Remove(dir) // not RemoveAll: something that appeared meanwhile stays
	}
}
