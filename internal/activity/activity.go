// Package activity keeps a local, append-only record of what meterm was used
// to do on saved hosts (connect, exec, cp, sync, sftp) — the raw material for
// a team's audit log.
//
// Each event carries the hash of the one before it, so the log is a chain:
// editing or deleting an earlier line breaks every hash after it, and Verify
// finds the first broken link. That makes tampering evident, not impossible —
// someone with the file can still rewrite the whole chain, which is why the
// team backend keeps its own append-only copy.
//
// It only sees what goes through meterm: a plain `ssh` bypasses it entirely.
package activity

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"meterm/internal/config"
	"meterm/internal/lockfile"
)

// Event is one recorded action against a saved host.
type Event struct {
	Device   string    `json:"device"`           // random per-machine ID; (Device, Seq) is globally unique
	Seq      int64     `json:"seq"`              // 1, 2, 3… within this device's chain
	Time     time.Time `json:"time"`             // UTC
	User     string    `json:"user"`             // local OS account, e.g. "alice@alices-laptop"
	Action   string    `json:"action"`           // connect, exec, cp, sync, sftp
	Host     string    `json:"host"`             // saved host name
	Target   string    `json:"target"`           // user@address it resolved to
	Detail   string    `json:"detail,omitempty"` // command for exec, paths for cp/sync; credentials masked
	PrevHash string    `json:"prev_hash"`
	Hash     string    `json:"hash"`
}

// computeHash covers every field except Hash itself, in a fixed order, so the
// value doesn't depend on JSON key ordering or encoder details.
func (e Event) computeHash() string {
	h := sha256.New()
	for _, f := range []string{
		e.Device, strconv.FormatInt(e.Seq, 10), e.Time.UTC().Format(time.RFC3339Nano),
		e.User, e.Action, e.Host, e.Target, e.Detail, e.PrevHash,
	} {
		// Length-prefix each field so "ab"+"c" can't collide with "a"+"bc".
		fmt.Fprintf(h, "%d:%s|", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Valid reports whether e's hash matches its contents — i.e. it hasn't been
// edited since it was recorded. It says nothing about events around it; use
// Verify for the chain.
func (e Event) Valid() bool { return e.Hash == e.computeHash() }

// Log is the on-disk chain plus the bookkeeping beside it.
type Log struct {
	dir string
}

// DefaultDir is where the log lives: $METERM_ACTIVITY_DIR if set (tests,
// alternate profiles), otherwise next to the host inventory.
func DefaultDir() (string, error) {
	if d := os.Getenv("METERM_ACTIVITY_DIR"); d != "" {
		return d, nil
	}
	cfgPath, err := config.DefaultPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfgPath), "activity"), nil
}

// Open returns the log in dir, creating the directory if needed.
func Open(dir string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Log{dir: dir}, nil
}

func (l *Log) eventsPath() string { return filepath.Join(l.dir, "events.jsonl") }
func (l *Log) devicePath() string { return filepath.Join(l.dir, "device-id") }
func (l *Log) sentPath(key string) string {
	return filepath.Join(l.dir, "uploaded-seq."+fileKey(key))
}

// fileKey makes a key (a team ID) safe as part of a file name. Cloud team
// IDs are plain hex and stay as they are; a Git team's ID is a URL, so it's
// hashed.
func fileKey(key string) string {
	for _, r := range key {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			sum := sha256.Sum256([]byte(key))
			return "h" + hex.EncodeToString(sum[:12])
		}
	}
	return key
}
func (l *Log) lockPath() string { return filepath.Join(l.dir, "lock") }

// DeviceID returns this machine's chain ID, generating it on first use.
func (l *Log) DeviceID() (string, error) {
	if b, err := os.ReadFile(l.devicePath()); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)
	return id, os.WriteFile(l.devicePath(), []byte(id+"\n"), 0o600)
}

// Append records a new event at the end of the chain. Only Action, Host,
// Target and Detail are taken from e; the rest is filled in here.
func (l *Log) Append(e Event) (Event, error) {
	unlock, err := l.lock()
	if err != nil {
		return Event{}, err
	}
	defer unlock()

	device, err := l.DeviceID()
	if err != nil {
		return Event{}, err
	}
	events, err := l.readAll()
	if err != nil {
		return Event{}, err
	}
	e.Device = device
	e.Seq = 1
	e.PrevHash = ""
	if n := len(events); n > 0 {
		e.Seq = events[n-1].Seq + 1
		e.PrevHash = events[n-1].Hash
	}
	// Microseconds: the precision the old cloud backend kept. Unchanged so
	// the hash chains of existing logs still check out.
	e.Time = time.Now().UTC().Truncate(time.Microsecond)
	if e.User == "" {
		e.User = localUser()
	}
	e.Detail = Redact(e.Detail)
	e.Hash = e.computeHash()

	line, err := json.Marshal(e)
	if err != nil {
		return Event{}, err
	}
	f, err := os.OpenFile(l.eventsPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return Event{}, err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return Event{}, err
	}
	return e, f.Sync()
}

// All returns every event, oldest first.
func (l *Log) All() ([]Event, error) {
	return l.readAll()
}

// Pending returns events not yet considered for upload under key (one key
// per team: each team only receives events on hosts it shares, so each keeps
// its own cursor).
func (l *Log) Pending(key string) ([]Event, error) {
	events, err := l.readAll()
	if err != nil {
		return nil, err
	}
	sent, err := l.uploadedSeq(key)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, e := range events {
		if e.Seq > sent {
			out = append(out, e)
		}
	}
	return out, nil
}

// MarkUploaded records that every event up to and including seq has been
// dealt with for key (uploaded, or deliberately not shared). It never moves
// backwards.
func (l *Log) MarkUploaded(key string, seq int64) error {
	cur, err := l.uploadedSeq(key)
	if err != nil {
		return err
	}
	if seq <= cur {
		return nil
	}
	return os.WriteFile(l.sentPath(key), []byte(strconv.FormatInt(seq, 10)+"\n"), 0o600)
}

func (l *Log) uploadedSeq(key string) (int64, error) {
	b, err := os.ReadFile(l.sentPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// BrokenLink describes where Verify found the chain doesn't hold.
type BrokenLink struct {
	Line   int // 1-based line in events.jsonl
	Seq    int64
	Reason string
}

func (b BrokenLink) Error() string {
	return fmt.Sprintf("activity log broken at line %d (seq %d): %s", b.Line, b.Seq, b.Reason)
}

// Verify walks the chain and returns the first broken link, or nil when
// every event's hash and back-pointer check out.
func Verify(events []Event) *BrokenLink {
	prev := ""
	var prevSeq int64
	for i, e := range events {
		switch {
		case e.Hash != e.computeHash():
			return &BrokenLink{Line: i + 1, Seq: e.Seq, Reason: "event was modified (hash mismatch)"}
		case e.PrevHash != prev:
			return &BrokenLink{Line: i + 1, Seq: e.Seq, Reason: "does not follow the previous event (an event before it was changed, removed or inserted)"}
		case e.Seq != prevSeq+1:
			return &BrokenLink{Line: i + 1, Seq: e.Seq, Reason: fmt.Sprintf("sequence jumps from %d to %d", prevSeq, e.Seq)}
		}
		prev, prevSeq = e.Hash, e.Seq
	}
	return nil
}

// VerifyShared checks events as a team sees them: a mix of devices, each
// contributing only the events on hosts that team shares, so a device's chain
// has gaps. Every event must still match its own hash, and wherever a
// device's sequence numbers are consecutive the back-pointer must hold.
// Events may be in any order.
func VerifyShared(events []Event) *BrokenLink {
	byDevice := map[string]map[int64]Event{}
	for i, e := range events {
		if !e.Valid() {
			return &BrokenLink{Line: i + 1, Seq: e.Seq, Reason: fmt.Sprintf("event from device %s was modified (hash mismatch)", e.Device)}
		}
		if byDevice[e.Device] == nil {
			byDevice[e.Device] = map[int64]Event{}
		}
		byDevice[e.Device][e.Seq] = e
	}
	for i, e := range events {
		if prev, ok := byDevice[e.Device][e.Seq-1]; ok && e.PrevHash != prev.Hash {
			return &BrokenLink{Line: i + 1, Seq: e.Seq, Reason: fmt.Sprintf("does not follow seq %d from the same device", e.Seq-1)}
		}
	}
	return nil
}

func (l *Log) readAll() ([]Event, error) {
	f, err := os.Open(l.eventsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("activity log line %d: %w", n, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// lock serializes writers across processes (the TUI re-execs meterm, and two
// terminals can run commands at once).
func (l *Log) lock() (func(), error) {
	unlock, err := lockfile.Acquire(l.lockPath(), 3*time.Second, 20*time.Millisecond)
	if errors.Is(err, lockfile.ErrBusy) {
		return nil, fmt.Errorf("activity log is locked by another meterm (%s)", l.lockPath())
	}
	return unlock, err
}

func localUser() string {
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return name + "@" + host
	}
	return name
}

// Record is the one-call convenience used by commands: append to the default
// log and swallow errors, because a broken activity log must never stop
// someone from reaching a host. The error is still returned for callers that
// want to warn about it.
func Record(action, host, target, detail string) error {
	dir, err := DefaultDir()
	if err != nil {
		return err
	}
	l, err := Open(dir)
	if err != nil {
		return err
	}
	_, err = l.Append(Event{Action: action, Host: host, Target: target, Detail: detail})
	return err
}
