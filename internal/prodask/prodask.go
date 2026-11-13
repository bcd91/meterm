// Package prodask lets the picker tell a meterm it starts that a person
// already answered its production question, without that word being
// something any child of the picker could repeat.
//
// The picker asks, then Issue writes a random value to a new 0600 file in a
// directory of its own and hands the child its path and the value in the
// environment. The child's Consume checks the file and deletes it: the
// answer counts once, for that command. An environment variable alone
// (METERM_FROM_TUI, or the token's env copied into a later shell) never
// skips a question.
//
// This is not a guard. Any process running as the same user can read the
// picker's directory or write a file of its own and forge the token. What it
// stops is accidental reuse: a script that sets METERM_FROM_TUI, a shell
// started from the picker that inherits its environment, a command run
// twice with the same env.
package prodask

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

const (
	// PathEnv names the token file.
	PathEnv = "METERM_PROD_TOKEN"
	// ValueEnv holds what the token file must contain.
	ValueEnv = "METERM_PROD_TOKEN_VALUE"
)

const tokenBytes = 16 // 128 bits

// ownedByUs says fi belongs to this process's uid (always true on Windows,
// where it isn't checked); tests replace it.
var ownedByUs = fileOwnedByUs

// betweenStatAndOpen runs after the token file's checks and before it's
// opened; tests swap the file there, as a racing process could.
var betweenStatAndOpen = func(string) {}

// Issue writes a fresh token to a new file in dir and returns the two
// environment entries a child needs to Consume it. dir must be the caller's
// own: on unix it is refused when group or others can write to it.
func Issue(dir string) ([]string, error) {
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(dir)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("%s isn't a directory", dir)
		}
		if fi.Mode().Perm()&0o022 != 0 || !ownedByUs(fi) {
			return nil, fmt.Errorf("%s isn't private to this user", dir)
		}
	}
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	value := hex.EncodeToString(raw)
	name := make([]byte, 8)
	if _, err := rand.Read(name); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(abs, "prod-"+hex.EncodeToString(name))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(value); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	return []string{PathEnv + "=" + path, ValueEnv + "=" + value}, nil
}

// Consume says whether the picker that started this process asked about
// production for it: its token file is there, a regular file of ours with
// mode 0600 (owner and mode aren't checked on Windows), holding exactly the
// value. It deletes the file and clears both variables, so the answer counts
// once and isn't passed on. note says why, for the log or a warning: "picker
// asked", or "picker token missing" / "picker token invalid: …".
func Consume() (asked bool, note string) {
	path, value := os.Getenv(PathEnv), os.Getenv(ValueEnv)
	os.Unsetenv(PathEnv)
	os.Unsetenv(ValueEnv)
	if path == "" {
		return false, "picker token missing"
	}
	if err := check(path, value); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "picker token missing"
		}
		return false, "picker token invalid: " + err.Error()
	}
	if err := os.Remove(path); err != nil {
		return false, "picker token invalid: can't remove it, so it could be used again"
	}
	return true, "picker asked"
}

func check(path, value string) error {
	if !filepath.IsAbs(path) {
		return errors.New("not an absolute path")
	}
	if len(value) != 2*tokenBytes {
		return errors.New("wrong length")
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	// Pin which file this is now. On Windows an Lstat result only notes the
	// path, and SameFile looks the file up by it when first asked: after a
	// swap, that's the new file, and it would match itself.
	_ = os.SameFile(fi, fi)
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if runtime.GOOS != "windows" {
		if fi.Mode().Perm() != 0o600 {
			return fmt.Errorf("mode %#o, not 0600", fi.Mode().Perm())
		}
		if !ownedByUs(fi) {
			return errors.New("not owned by this user")
		}
	}
	betweenStatAndOpen(path)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(fi, opened) {
		return errors.New("changed while being read")
	}
	b, err := io.ReadAll(io.LimitReader(f, 2*tokenBytes+1))
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(b, []byte(value)) != 1 {
		return errors.New("wrong value")
	}
	return nil
}
