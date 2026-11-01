package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zalando/go-keyring"

	"meterm/internal/config"
	"meterm/internal/lockfile"
	"meterm/internal/safefile"
)

// State is the non-secret local team state, keyed by team ID where per-team.
type State struct {
	Current string          `json:"current,omitempty"` // team commands act on
	Bases   map[string]Base `json:"bases,omitempty"`   // last-synced host lists
	// Ignored hosts are ones this person chose not to receive (`team ignore`).
	Ignored map[string][]string `json:"ignored,omitempty"`
	// Autosync is nil (default on) or an explicit choice.
	Autosync *bool `json:"autosync,omitempty"`
	// What the last background pull found, for the notice in list/picker.
	LastPull  map[string]time.Time `json:"last_pull,omitempty"`
	Attention map[string][]Change  `json:"attention,omitempty"`
	TeamNames map[string]string    `json:"team_names,omitempty"`
	LastError string               `json:"last_error,omitempty"`
	// GitTeams are teams kept in a Git repository, by team ID.
	GitTeams map[string]GitTeam `json:"git_teams,omitempty"`
}

// AutosyncOn reports whether background sync is enabled.
func (s State) AutosyncOn() bool { return s.Autosync == nil || *s.Autosync }

// IgnoredSet returns teamID's ignored hosts as a set.
func (s State) IgnoredSet(teamID string) map[string]bool {
	m := map[string]bool{}
	for _, n := range s.Ignored[teamID] {
		m[n] = true
	}
	return m
}

// Lock takes the team directory's sync lock, so a background sync and a
// foreground team command never read-modify-write state.json or hosts.yaml
// at the same time. It waits up to wait (0 = try once); ok is false if the
// lock is busy. It can be held for as long as a question takes to answer: a
// background sync just skips its turn, and a crash gives it up.
func Lock(dir string, wait time.Duration) (unlock func(), ok bool, err error) {
	unlock, err = lockfile.Acquire(filepath.Join(dir, "sync.lock"), wait, 50*time.Millisecond)
	if errors.Is(err, lockfile.ErrBusy) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return unlock, true, nil
}

// WaiterLock is held by the one process waiting for Lock. It never waits: ok is false when
// another process is already waiting, and it will do what this one came for.
func WaiterLock(dir string) (unlock func(), ok bool, err error) {
	unlock, err = lockfile.Acquire(filepath.Join(dir, "sync-waiter.lock"), 0, 0)
	if errors.Is(err, lockfile.ErrBusy) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return unlock, true, nil
}

// Dir is where team files live: next to the host inventory, or
// $METERM_TEAM_DIR.
func Dir() (string, error) {
	if d := os.Getenv("METERM_TEAM_DIR"); d != "" {
		return d, nil
	}
	p, err := config.DefaultPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "team"), nil
}

func readJSON(path string, v any) (bool, error) {
	b, err := safefile.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return safefile.WriteFile(path, b)
}

// LoadState reads state.json from dir.
func LoadState(dir string) (State, error) {
	var s State
	_, err := readJSON(filepath.Join(dir, "state.json"), &s)
	if s.Bases == nil {
		s.Bases = map[string]Base{}
	}
	if s.Ignored == nil {
		s.Ignored = map[string][]string{}
	}
	if s.LastPull == nil {
		s.LastPull = map[string]time.Time{}
	}
	if s.Attention == nil {
		s.Attention = map[string][]Change{}
	}
	if s.TeamNames == nil {
		s.TeamNames = map[string]string{}
	}
	return s, err
}

// SaveState writes state.json to dir.
func SaveState(dir string, s State) error { return writeJSON(filepath.Join(dir, "state.json"), s) }

// meterm used to have a hosted (cloud) team service. Its sign-in left a
// refresh token — whoever holds it acts as you there — in the OS keychain
// (or session.json), and a device key for encrypted teams beside it.
// ForgetCloud removes all of that; it runs before any team command, so an
// upgrade leaves no live credential behind.
const keyringService = "meterm"

func dirKey(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return hex.EncodeToString(sum[:8])
}

// ForgetCloud deletes the old cloud sign-in and device keys from dir and
// the keychain. It reports whether there was anything to delete.
func ForgetCloud(dir string) bool {
	var sess struct {
		UID string `json:"uid"`
	}
	found, _ := readJSON(filepath.Join(dir, "session.json"), &sess)
	devices, _ := filepath.Glob(filepath.Join(dir, "device-*.json"))
	if !found && len(devices) == 0 {
		return false
	}
	_ = keyring.Delete(keyringService, "team-session:"+dirKey(dir))
	for _, d := range devices {
		uid := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(d), "device-"), ".json")
		_ = keyring.Delete(keyringService, "team-device:"+dirKey(dir)+":"+uid)
		_ = os.Remove(d)
	}
	if sess.UID != "" {
		_ = keyring.Delete(keyringService, "team-device:"+dirKey(dir)+":"+sess.UID)
	}
	_ = os.Remove(filepath.Join(dir, "session.json"))
	return true
}
