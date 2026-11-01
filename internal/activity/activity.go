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
//
// Two hashes, because of who else reads this file. Hash covers the fields the
// first version had, in the first version's way, and no more: a released meterm
// recomputes it to verify, here and in the team's copy, so it can't change. Fields
// added since are covered by ExtHash, with its own back-pointer PrevExt. A new reader
// checks both; an old one checks Hash and ignores the rest.
package activity

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"meterm/internal/config"
	"meterm/internal/lockfile"
	"meterm/internal/safefile"
	"meterm/internal/termsafe"
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

	// RefSeq is the event this one reports the outcome of (ActionOutcome only).
	RefSeq int64 `json:"ref_seq,omitempty"`
	// ExitCode and DurationMS say how it ended. They're pointers because 0 is an
	// answer: no exit_code means it isn't known, not that it was fine.
	ExitCode   *int   `json:"exit_code,omitempty"`
	DurationMS *int64 `json:"duration_ms,omitempty"`
	// Who, as far as the process's environment says: see DetectActor.
	ActorType         string `json:"actor_type,omitempty"`
	AgentKind         string `json:"agent_kind,omitempty"`
	AgentSessionID    string `json:"agent_session_id,omitempty"`
	AttributionSource string `json:"attribution_source,omitempty"`
	PrevExt           string `json:"prev_ext,omitempty"`
	ExtHash           string `json:"ext_hash,omitempty"`
}

// ActionOutcome is the Action of an event saying how an earlier one ended.
const ActionOutcome = "outcome"

// ActionRepair is the Action of an event saying damaged bytes were moved out of the log.
const ActionRepair = "log-repair"

// Limits on what a line can hold. A line the reader can't take would stop the log
// being read at all.
const (
	maxField  = 256  // runes in User, Action, Host, Target
	maxDetail = 4096 // runes in Detail
)

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

// Clean returns e with every string fit to show: the log is a file anyone with this
// account can edit, and a team's copy comes from other people. The hashes stay as
// stored, so they won't match an event that had anything to clean. That's for Valid to
// be asked before this.
func (e Event) Clean() Event {
	for _, f := range []*string{&e.Device, &e.User, &e.Action, &e.Host, &e.Target, &e.PrevHash, &e.Hash,
		&e.ActorType, &e.AgentKind, &e.AgentSessionID, &e.AttributionSource, &e.PrevExt, &e.ExtHash} {
		*f = cleanField(*f)
	}
	e.Detail = termsafe.Clip(termsafe.Text(e.Detail), maxDetail)
	return e
}

// computeExtHash covers everything computeHash leaves out, and through Hash
// everything it covers. A missing number and a zero hash differently.
func (e Event) computeExtHash() string {
	h := sha256.New()
	exit, dur, ref := "", "", ""
	if e.ExitCode != nil {
		exit = strconv.Itoa(*e.ExitCode)
	}
	if e.DurationMS != nil {
		dur = strconv.FormatInt(*e.DurationMS, 10)
	}
	if e.RefSeq != 0 {
		ref = strconv.FormatInt(e.RefSeq, 10)
	}
	for _, f := range []string{
		"meterm-ext-1", e.Hash, e.PrevExt, ref, exit, dur,
		e.ActorType, e.AgentKind, e.AgentSessionID, e.AttributionSource,
	} {
		fmt.Fprintf(h, "%d:%s|", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hasExtFields says whether e claims anything only ExtHash can vouch for.
func (e Event) hasExtFields() bool {
	return e.RefSeq != 0 || e.ExitCode != nil || e.DurationMS != nil || e.ActorType != "" ||
		e.AgentKind != "" || e.AgentSessionID != "" || e.AttributionSource != "" || e.PrevExt != ""
}

// extValid is true of an event from before ExtHash that claims nothing it would
// cover, and of one whose ExtHash matches.
func (e Event) extValid() bool {
	if e.ExtHash == "" {
		return !e.hasExtFields()
	}
	return e.ExtHash == e.computeExtHash()
}

// Valid reports whether e's hashes match its contents — i.e. it hasn't been
// edited since it was recorded. It says nothing about events around it; use
// Verify for the chain.
func (e Event) Valid() bool { return e.Hash == e.computeHash() && e.extValid() }

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

// Reader is Open for looking: it creates nothing, and a log that isn't there reads as
// empty. Only reading is meant to be done with it.
func Reader(dir string) *Log { return &Log{dir: dir} }

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
func (l *Log) headPath() string { return filepath.Join(l.dir, "head") }

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

// Device is this log's chain ID, or "" if it hasn't one yet. Unlike DeviceID it never
// makes one.
func (l *Log) Device() string {
	b, err := os.ReadFile(l.devicePath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Append records a new event at the end of the chain. Action, Host, Target, User,
// Detail, and the outcome and actor fields come from e, cleaned; the rest is filled
// in here.
//
// It goes on from the highest sequence this log is known to have reached, not from
// the last line in the file. So a log cut short stays visibly broken once it's
// written to again, and what comes next still counts as new for an upload.
func (l *Log) Append(e Event) (Event, error) {
	unlock, err := l.lock()
	if err != nil {
		return Event{}, err
	}
	defer unlock()
	return l.appendLocked(e)
}

func (l *Log) appendLocked(e Event) (Event, error) {
	device, err := l.DeviceID()
	if err != nil {
		return Event{}, err
	}
	file, err := l.read()
	if err != nil {
		return Event{}, err
	}
	// A torn last line was never a whole event, so it's moved aside now, while it can
	// still be told from an edit: written past, it would be damage in the middle of the log
	// for good, and every older meterm would stop reading the file at that line.
	if file.tornLine != 0 {
		name, size, err := l.moveTornAside(file)
		switch {
		case err == nil:
			note := Event{Action: ActionRepair, Detail: fmt.Sprintf("moved a torn last line aside: %d bytes to %s", size, name),
				ActorType: e.ActorType, AgentKind: e.AgentKind, AgentSessionID: e.AgentSessionID, AttributionSource: e.AttributionSource}
			if _, err := l.appendLocked(note); err != nil {
				return Event{}, err
			}
			if file, err = l.read(); err != nil {
				return Event{}, err
			}
		case noSpace(err):
			return Event{}, err // nothing can be written, and trying would only add damage
		}
	}
	// Writing this event will put a witness back, and with it the only sign one was gone.
	if _, ok := l.readHead(); !ok && witnessExpected(file.events) {
		l.writeHead(file.events[len(file.events)-1])
		note := Event{Action: ActionRepair, Detail: "the witness of how far this log got was missing: lines removed before this can't be seen",
			ActorType: e.ActorType, AgentKind: e.AgentKind, AgentSessionID: e.AgentSessionID, AttributionSource: e.AttributionSource}
		if _, err := l.appendLocked(note); err != nil {
			return Event{}, err
		}
		if file, err = l.read(); err != nil {
			return Event{}, err
		}
	}
	e.Device = cleanField(device) // from a file, like the rest: it reaches other people
	e.Seq, e.PrevHash, e.PrevExt = 1, "", ""
	if n := len(file.events); n > 0 {
		e.Seq = file.events[n-1].Seq + 1
		e.PrevHash = file.events[n-1].Hash
		e.PrevExt = lastExt(file.events)
	}
	if h, ok := l.readHead(); ok {
		switch {
		case h.seq >= e.Seq, h.seq == e.Seq-1 && h.hash != e.PrevHash:
			e.Seq, e.PrevHash, e.PrevExt = h.seq+1, h.hash, h.ext
		case h.seq == e.Seq-1 && h.ext != "":
			// Point at what was written, not what's there now, so that fields taken off
			// the last line stay noticed once there's a line after it.
			e.PrevExt = h.ext
		}
	}
	if sent := l.highestUploaded(); sent >= e.Seq {
		e.Seq = sent + 1
	}
	// Microseconds: the precision the old cloud backend kept. Unchanged so
	// the hash chains of existing logs still check out.
	e.Time = time.Now().UTC().Truncate(time.Microsecond)
	if e.User == "" {
		e.User = localUser()
	}
	e.User = cleanField(e.User)
	e.Action, e.Host, e.Target = cleanField(e.Action), cleanField(e.Host), cleanField(e.Target)
	e.Detail = termsafe.Clip(termsafe.Text(Redact(e.Detail)), maxDetail)
	e.Hash = e.computeHash()
	e.ExtHash = e.computeExtHash()

	line, err := json.Marshal(e)
	if err != nil {
		return Event{}, err
	}
	f, err := os.OpenFile(l.eventsPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return Event{}, err
	}
	defer f.Close()
	if file.unterminated {
		// Half a line that couldn't be moved aside, or a whole event missing its newline.
		line = append([]byte{'\n'}, line...)
	}
	var before int64 = -1
	if fi, err := f.Stat(); err == nil {
		before = fi.Size()
	}
	if _, err := writeLine(f, append(line, '\n')); err != nil {
		// A full disk can take part of a line. Take that back, or it's left for the next
		// reader to find, and only a power cut should be able to leave one of those.
		if before >= 0 {
			_ = os.Truncate(l.eventsPath(), before)
		}
		return Event{}, err
	}
	if err := f.Sync(); err != nil {
		return e, err
	}
	l.writeHead(e) // best-effort: what it guards against isn't worth losing the event for
	return e, nil
}

// writeLine is f.Write; tests make it fail partway, as a full disk does.
var writeLine = func(f *os.File, b []byte) (int, error) { return f.Write(b) }

// witnessExpected says whether events show that head should be there: this version wrote more
// than one of them, and writes head after each. One could be a crash right after the first
// event, and none is a log from before head existed, or a new one.
func witnessExpected(events []Event) bool {
	n := 0
	for _, e := range events {
		if e.ExtHash != "" {
			n++
		}
	}
	return n > 1
}

func noSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EFBIG)
}

// damagedName is a new file beside the log for bytes taken out of it.
func (l *Log) damagedPath() (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	for i := 0; i < 1000; i++ {
		name := "damaged-" + stamp + ".txt"
		if i > 0 {
			name = fmt.Sprintf("damaged-%s-%d.txt", stamp, i)
		}
		p := filepath.Join(l.dir, name)
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			return p, nil
		}
	}
	return "", errors.New("too many damaged-* files")
}

// moveTornAside saves the torn bytes, and only then takes them off the log.
func (l *Log) moveTornAside(file logFile) (string, int, error) {
	f, err := os.Open(l.eventsPath())
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	if _, err := f.Seek(file.tornOffset, io.SeekStart); err != nil {
		return "", 0, err
	}
	torn, err := io.ReadAll(f)
	if err != nil {
		return "", 0, err
	}
	p, err := l.damagedPath()
	if err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(p, torn, 0o600); err != nil {
		_ = os.Remove(p)
		return "", 0, err
	}
	if err := os.Truncate(l.eventsPath(), file.tornOffset); err != nil {
		return "", 0, err
	}
	return filepath.Base(p), len(torn), nil
}

// RepairResult says what Repair did.
type RepairResult struct {
	Lines    []int  // the lines moved out
	SavedTo  string // the file beside the log that holds them
	EventSeq int64  // the event saying so
}

// Repair moves lines that aren't events out of the log, into a file beside it, and records
// that as an event in the chain. It keeps what it takes, and it can't make a log read as sound
// that isn't: only lines that aren't events go, so an edited event stays broken, and a real
// event replaced by rubbish leaves a hole the chain still shows.
func (l *Log) Repair() (RepairResult, error) {
	unlock, err := l.lock()
	if err != nil {
		return RepairResult{}, err
	}
	defer unlock()
	data, err := os.ReadFile(l.eventsPath())
	if err != nil && !os.IsNotExist(err) {
		return RepairResult{}, err
	}
	var res RepairResult
	var kept, moved bytes.Buffer
	for n, rest := 1, data; len(rest) > 0; n++ {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i+1], rest[i+1:]
		} else {
			rest = nil
		}
		if _, ok := parseLine(line); ok || len(bytes.TrimSpace(line)) == 0 {
			kept.Write(line)
			continue
		}
		res.Lines = append(res.Lines, n)
		fmt.Fprintf(&moved, "# line %d, %d bytes\n", n, len(line))
		moved.Write(bytes.TrimRight(line, "\n"))
		moved.WriteByte('\n')
	}
	if len(res.Lines) == 0 {
		return res, nil
	}
	p, err := l.damagedPath()
	if err != nil {
		return RepairResult{}, err
	}
	if err := os.WriteFile(p, moved.Bytes(), 0o600); err != nil {
		_ = os.Remove(p)
		return RepairResult{}, err
	}
	if err := safefile.WriteFile(l.eventsPath(), kept.Bytes()); err != nil {
		return RepairResult{}, err
	}
	res.SavedTo = filepath.Base(p)
	sum := sha256.Sum256(moved.Bytes())
	a := DetectActor(os.Getenv)
	e, err := l.appendLocked(Event{Action: ActionRepair,
		Detail:    fmt.Sprintf("repair: moved %d damaged line(s) to %s, sha256 %s", len(res.Lines), res.SavedTo, hex.EncodeToString(sum[:8])),
		ActorType: a.Type, AgentKind: a.Kind, AgentSessionID: a.SessionID, AttributionSource: a.Source})
	if err != nil {
		return res, err
	}
	res.EventSeq = e.Seq
	return res, nil
}

func cleanField(s string) string { return termsafe.Clip(termsafe.Text(s), maxField) }

// lastExt is the ExtHash of the latest event that has one. An older meterm can write
// events between newer ones, and those have none.
func lastExt(events []Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].ExtHash != "" {
			return events[i].ExtHash
		}
	}
	return ""
}

// head is what the log last reached, kept beside it. Anyone who can edit the log can
// edit this too, so it doesn't stop someone careful. It's what makes deleting lines,
// or rebuilding the chain and forgetting this file, show.
type head struct {
	seq       int64
	hash, ext string
}

func (l *Log) writeHead(e Event) {
	_ = safefile.WriteFile(l.headPath(), []byte(fmt.Sprintf("%d %s %s\n", e.Seq, e.Hash, e.ExtHash)))
}

func (l *Log) readHead() (head, bool) {
	b, err := safefile.ReadFile(l.headPath())
	if err != nil {
		return head{}, false
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return head{}, false
	}
	seq, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || seq < 1 {
		return head{}, false
	}
	h := head{seq: seq, hash: f[1]}
	if len(f) > 2 {
		h.ext = f[2]
	}
	return h, true
}

// highestUploaded is the furthest any team's upload got: another witness to how far
// the log has been.
func (l *Log) highestUploaded() int64 {
	var top int64
	paths, _ := filepath.Glob(filepath.Join(l.dir, "uploaded-seq.*"))
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && n > top {
				top = n
			}
		}
	}
	return top
}

// All returns every event that can be read, oldest first. A line that isn't an
// event is left out; VerifyFile is what reports it.
func (l *Log) All() ([]Event, error) {
	file, err := l.read()
	return file.events, err
}

// Pending returns events not yet considered for upload under key (one key
// per team: each team only receives events on hosts it shares, so each keeps
// its own cursor).
func (l *Log) Pending(key string) ([]Event, error) {
	events, err := l.All()
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

// UploadedSeq is how far the upload for key has got.
func (l *Log) UploadedSeq(key string) (int64, error) { return l.uploadedSeq(key) }

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
	if b.Seq == 0 {
		return "activity log broken: " + b.Reason
	}
	return fmt.Sprintf("activity log broken at line %d (seq %d): %s", b.Line, b.Seq, b.Reason)
}

// Verify walks the chain and returns the first broken link, or nil when
// every event's hash and back-pointer check out.
func Verify(events []Event) *BrokenLink {
	prev, ext := "", ""
	var prevSeq int64
	for i, e := range events {
		switch {
		case e.Hash != e.computeHash():
			return &BrokenLink{Line: i + 1, Seq: e.Seq, Reason: "event was modified (hash mismatch)"}
		case e.PrevHash != prev:
			return &BrokenLink{Line: i + 1, Seq: e.Seq, Reason: "does not follow the previous event (an event before it was changed, removed or inserted)"}
		case e.Seq != prevSeq+1:
			return &BrokenLink{Line: i + 1, Seq: e.Seq, Reason: fmt.Sprintf("sequence jumps from %d to %d", prevSeq, e.Seq)}
		case !e.extValid():
			return &BrokenLink{Line: i + 1, Seq: e.Seq, Reason: "exit code, duration or actor was modified (extension hash mismatch)"}
		case e.ExtHash != "" && e.PrevExt != ext:
			return &BrokenLink{Line: i + 1, Seq: e.Seq, Reason: "does not follow the previous event's extension hash (one was changed, or had its fields removed)"}
		}
		prev, prevSeq = e.Hash, e.Seq
		if e.ExtHash != "" {
			ext = e.ExtHash
		}
	}
	return nil
}

// VerifyFile checks the log as it is on disk: lines that aren't events, the chain, and
// whether it still reaches as far as it's known to have. It returns how many events
// there are. It writes nothing. A torn last line isn't counted against it, as nothing that
// was ever whole is missing: Notes says so.
func (l *Log) VerifyFile() (int, *BrokenLink, error) {
	// The witnesses first: they're written after the log, so read in this order a command
	// running right now can't look like lines going missing.
	h, haveHead := l.readHead()
	sent := l.highestUploaded()
	file, err := l.read()
	if err != nil {
		return 0, nil, err
	}
	broken := Verify(file.events)
	if broken != nil && broken.Line <= len(file.lines) {
		broken.Line = file.lines[broken.Line-1] // Verify counts events; people count lines
	}
	if file.badLine != 0 && (broken == nil || file.badLine < broken.Line) {
		broken = &BrokenLink{Line: file.badLine, Reason: "isn't an event: `meterm activity --repair` moves it aside, and records that"}
	}
	if broken != nil {
		return len(file.events), broken, nil
	}
	var lastSeq int64
	if n := len(file.events); n > 0 {
		lastSeq = file.events[n-1].Seq
	}
	last := len(file.lines)
	if haveHead {
		if h.seq > lastSeq {
			return len(file.events), &BrokenLink{Line: last, Seq: lastSeq, Reason: fmt.Sprintf("the log ends here, but it had reached seq %d: lines were removed", h.seq)}, nil
		}
		// By seq, not the last line: an older meterm appends without moving the witness.
		for i, e := range file.events {
			if e.Seq != h.seq {
				continue
			}
			switch {
			case e.Hash != h.hash:
				return len(file.events), &BrokenLink{Line: file.lines[i], Seq: e.Seq, Reason: "this isn't the event last written at this seq: the chain was rebuilt"}, nil
			case h.ext != "" && e.ExtHash != h.ext:
				return len(file.events), &BrokenLink{Line: file.lines[i], Seq: e.Seq, Reason: "exit code, duration or actor isn't what was written at this seq: they were changed or removed"}, nil
			}
		}
	}
	if sent > lastSeq {
		return len(file.events), &BrokenLink{Line: last, Seq: lastSeq, Reason: fmt.Sprintf("the log ends here, but seq %d was already uploaded: lines were removed", sent)}, nil
	}
	if !haveHead && witnessExpected(file.events) {
		return len(file.events), &BrokenLink{Line: last, Seq: lastSeq, Reason: "the witness of how far this log got is missing, so nothing can say whether lines were removed"}, nil
	}
	return len(file.events), nil, nil
}

// SharedCheck is what CheckShared found.
type SharedCheck struct {
	// Problems is what doesn't check out, by index into the events given.
	Problems map[int]string
	// First is the lowest of those, or nil.
	First *BrokenLink
	// ExtGaps counts events whose prev_ext points at a hash the copy doesn't have, since
	// the event before them has no ext_hash. A version that doesn't know those fields uploads
	// them that way, honestly; so does anyone stripping them. It can't be told which, so
	// it isn't a problem, and nothing is vouched for across it.
	ExtGaps int
}

// CheckShared checks events as a team sees them: a mix of devices, each contributing only
// the events on hosts that team shares, so a device's chain has gaps. Every event must still
// match its own hash, and wherever a device's sequence numbers are consecutive the
// back-pointers must hold. Events may be in any order.
func CheckShared(events []Event) SharedCheck {
	res := SharedCheck{Problems: map[int]string{}}
	byDevice := map[string]map[int64]Event{}
	for i, e := range events {
		if !e.Valid() {
			res.Problems[i] = fmt.Sprintf("event from device %s was modified (hash mismatch)", e.Device)
		}
		if byDevice[e.Device] == nil {
			byDevice[e.Device] = map[int64]Event{}
		}
		byDevice[e.Device][e.Seq] = e
	}
	for i, e := range events {
		prev, ok := byDevice[e.Device][e.Seq-1]
		if !ok || res.Problems[i] != "" {
			continue
		}
		switch {
		case e.PrevHash != prev.Hash:
			res.Problems[i] = fmt.Sprintf("does not follow seq %d from the same device", e.Seq-1)
		case e.ExtHash == "":
		case prev.ExtHash == "":
			if e.PrevExt != "" {
				res.ExtGaps++
			}
		case e.PrevExt != prev.ExtHash:
			res.Problems[i] = fmt.Sprintf("does not follow seq %d's extension hash from the same device", e.Seq-1)
		}
	}
	for i := range events {
		if why := res.Problems[i]; why != "" {
			res.First = &BrokenLink{Line: i + 1, Seq: events[i].Seq, Reason: why}
			break
		}
	}
	return res
}

// VerifyShared returns CheckShared's first problem, or nil.
func VerifyShared(events []Event) *BrokenLink { return CheckShared(events).First }

// Difference is one way a log and a copy of it disagree.
type Difference struct {
	Seq    int64  `json:"seq"`
	Reason string `json:"reason"`
}

// CompareCopy looks for events of device in copy that this log has lost or changed.
// A copy kept where this machine can't rewrite it is the one thing a rebuilt chain
// can't get past. Events in the log that haven't reached the copy aren't differences:
// they may just not have been sent yet, or not be for that copy. They come back
// oldest first.
func CompareCopy(local, copied []Event, device string) []Difference {
	bySeq := make(map[int64]Event, len(local))
	for _, e := range local {
		if e.Device == device {
			bySeq[e.Seq] = e
		}
	}
	var out []Difference
	for _, c := range copied {
		if c.Device != device {
			continue
		}
		l, ok := bySeq[c.Seq]
		switch {
		case !ok:
			out = append(out, Difference{c.Seq, "removed from this log"})
		case l.Hash != c.Hash || l.computeHash() != c.Hash: // an edit needn't touch the stored hash
			out = append(out, Difference{c.Seq, "changed in this log"})
		case c.ExtHash != "" && (l.ExtHash != c.ExtHash || l.computeExtHash() != c.ExtHash):
			out = append(out, Difference{c.Seq, "exit code, duration or actor changed in this log"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// logFile is what's in events.jsonl.
type logFile struct {
	events       []Event
	lines        []int // lines[i] is the line in the file of events[i]
	badLine      int   // first line that isn't an event, torn or not; 0 if none
	unterminated bool  // the last line has no newline
	// tornLine is the last line when it has no newline and isn't an event: what a write cut
	// off by power loss or a full disk leaves. It's the one kind of bad line that says nothing
	// about what was once there. 0 if there isn't one, and it doesn't count as badLine.
	tornLine   int
	tornOffset int64
	tornBytes  int
}

// read reads the whole log. Only a file that can't be read is an error. A line that
// isn't an event must not stop it: that would stop the log being written to, and a
// single stray byte would then record nothing from there on.
func (l *Log) read() (logFile, error) { return readLogFile(l.eventsPath()) }

func readLogFile(path string) (logFile, error) {
	var out logFile
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var offset int64
	for n := 1; ; n++ {
		raw, err := r.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return out, err
		}
		end := err != nil
		if end && len(raw) > 0 {
			out.unterminated = true
		}
		if e, ok := parseLine(raw); ok {
			out.events = append(out.events, e)
			out.lines = append(out.lines, n)
		} else if len(bytes.TrimSpace(raw)) > 0 {
			if end {
				out.tornLine, out.tornOffset, out.tornBytes = n, offset, len(raw)
			} else if out.badLine == 0 {
				out.badLine = n
			}
		}
		offset += int64(len(raw))
		if end {
			return out, nil
		}
	}
}

// parseLine reads one line of the log.
func parseLine(raw []byte) (Event, bool) {
	line := bytes.TrimSpace(raw)
	if len(line) == 0 {
		return Event{}, false
	}
	var e Event
	if json.Unmarshal(line, &e) != nil {
		return Event{}, false
	}
	return e, true
}

// Notes is what's worth saying about a log beyond whether it holds.
type Notes struct {
	// TornLine and TornBytes are a last line cut short, if any.
	TornLine, TornBytes int
	// Repairs are the seqs of events saying bytes were moved out of the log.
	Repairs []int64
}

// Notes reads what a check should mention. It writes nothing.
func (l *Log) Notes() (Notes, error) {
	file, err := l.read()
	if err != nil {
		return Notes{}, err
	}
	n := Notes{TornLine: file.tornLine, TornBytes: file.tornBytes}
	for _, e := range file.events {
		if e.Action == ActionRepair {
			n.Repairs = append(n.Repairs, e.Seq)
		}
	}
	return n, nil
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
// log, saying who's asking, and swallow errors, because a broken activity log must
// never stop someone from reaching a host. The error is still returned for callers
// that want to warn about it, with the event as far as it got.
func Record(action, host, target, detail string) (Event, error) {
	return record(Event{Action: action, Host: host, Target: target, Detail: detail})
}

// Outcome is how a recorded action ended. A nil ExitCode is one that isn't known, and
// Note then says why, in words from the caller's own code: never remote or free text.
type Outcome struct {
	ExitCode *int
	Took     time.Duration
	Note     string
}

// RecordOutcome notes how start ended, as a new event pointing back at it. The start
// event stays as it was, since nothing in a chain can change, and it was written before
// the host was touched so that a crash or a kill still leaves it. A start with no
// outcome is an action nobody can say the end of: that's its meaning, not a gap.
func RecordOutcome(start Event, o Outcome) (Event, error) {
	ms := o.Took.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	e := Event{Action: ActionOutcome, Host: start.Host, Target: start.Target, RefSeq: start.Seq, ExitCode: o.ExitCode, DurationMS: &ms}
	// Detail repeats it in words, because Hash covers that and a version that knows
	// nothing of exit_code still shows it.
	took := o.Took.Round(100 * time.Millisecond).String()
	switch {
	case o.ExitCode != nil:
		e.Detail = fmt.Sprintf("seq %d: exit %d, %s", start.Seq, *o.ExitCode, took)
	case o.Note != "":
		e.Detail = fmt.Sprintf("seq %d: %s, %s", start.Seq, o.Note, took)
	default:
		e.Detail = fmt.Sprintf("seq %d: exit unknown, %s", start.Seq, took)
	}
	return record(e)
}

func record(e Event) (Event, error) {
	dir, err := DefaultDir()
	if err != nil {
		return Event{}, err
	}
	l, err := Open(dir)
	if err != nil {
		return Event{}, err
	}
	a := DetectActor(os.Getenv)
	e.ActorType, e.AgentKind, e.AgentSessionID, e.AttributionSource = a.Type, a.Kind, a.SessionID, a.Source
	return l.Append(e)
}
