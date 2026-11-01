package activity

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"meterm/internal/lockfile"
	"meterm/internal/safefile"
	"meterm/internal/termsafe"
)

// A sink is one more copy of the log, written as each event is. It's for people with no
// team repository. It holds up as evidence only as far as the place it writes to is out
// of this account's reach, and it's named in a config file this account can edit: it
// records an honest agent's work where a careless one can't lose it, and that's all.
//
// It must never get in the way: sending happens after the log is written and its lock is
// let go, a sink gets one second, and one that fails is noted for `activity --verify`.

const (
	SinkKindFile   = "file"
	SinkKindSyslog = "syslog"

	sinkTimeout    = time.Second
	sinkBatch      = 1000 // events per send; the rest go next time
	sinkMaxCompare = 64 << 20
	sinkLineDetail = 512 // runes of detail in a syslog line, which daemons cut short anyway
)

// ParseSink reads activity.sink: "syslog", or an absolute path.
func ParseSink(s string) (kind, target string, err error) {
	switch {
	case s == "syslog":
		return SinkKindSyslog, "", nil
	case termsafe.Unsafe(s):
		return "", "", errors.New("activity.sink has control characters in it")
	case filepath.IsAbs(s):
		return SinkKindFile, s, nil
	}
	return "", "", errors.New("activity.sink must be syslog or an absolute path")
}

// sinkState is how sending has gone so far, kept beside the log.
type sinkState struct {
	Sink         string    `json:"sink"`
	SentSeq      int64     `json:"sent_seq"`
	LastOK       time.Time `json:"last_ok,omitzero"`
	FailingSince time.Time `json:"failing_since,omitzero"`
	LastError    string    `json:"last_error,omitempty"`
}

func (l *Log) sinkStatePath() string { return filepath.Join(l.dir, "sink-state.json") }
func (l *Log) sinkLockPath() string  { return filepath.Join(l.dir, "sink.lock") }

func (l *Log) readSinkState(sink string) sinkState {
	var st sinkState
	if b, err := safefile.ReadFile(l.sinkStatePath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Sink != sink { // somewhere new starts from the beginning
		return sinkState{Sink: sink}
	}
	return st
}

func (l *Log) writeSinkState(st sinkState) {
	if b, err := json.Marshal(st); err == nil {
		_ = safefile.WriteFile(l.sinkStatePath(), b)
	}
}

func (l *Log) noteSinkFailure(sink string, cause error) {
	st := l.readSinkState(sink)
	if st.FailingSince.IsZero() {
		st.FailingSince = time.Now().UTC().Truncate(time.Second)
	}
	st.LastError = termsafe.Clip(termsafe.Text(cause.Error()), 300)
	l.writeSinkState(st)
}

// sinkGaveUp stops a process waiting on a stuck sink for each of many hosts.
var sinkGaveUp atomic.Bool

// SendToSink copies what the sink hasn't got yet, including what it missed while it was
// failing. It returns within about a second whatever the sink does, and nothing it does
// comes back to the caller.
func SendToSink(sink string) {
	if sink == "" || sinkGaveUp.Load() {
		return
	}
	dir, err := DefaultDir()
	if err != nil {
		return
	}
	l := Reader(dir)
	done := make(chan error, 1)
	go func() { done <- l.sendToSink(sink) }()
	select {
	case err := <-done:
		if err != nil {
			l.noteSinkFailure(sink, err)
		}
	case <-time.After(sinkTimeout):
		sinkGaveUp.Store(true)
		l.noteSinkFailure(sink, errors.New("no answer within a second"))
	}
}

func (l *Log) sendToSink(sink string) error {
	kind, target, err := ParseSink(sink)
	if err != nil {
		return err
	}
	unlock, err := lockfile.Acquire(l.sinkLockPath(), sinkTimeout/2, 10*time.Millisecond)
	if errors.Is(err, lockfile.ErrBusy) {
		return nil // someone's sending now; whatever this misses goes next time
	}
	if err != nil {
		return err
	}
	defer unlock()
	events, err := l.All()
	if err != nil {
		return err
	}
	st := l.readSinkState(sink)
	var send []Event
	for _, e := range events {
		if e.Seq > st.SentSeq {
			send = append(send, e)
			if len(send) == sinkBatch {
				break
			}
		}
	}
	if len(send) == 0 {
		return nil
	}
	switch kind {
	case SinkKindFile:
		if l.insideLogDir(target) {
			return errors.New("activity.sink can't be inside the activity directory")
		}
		err = appendSinkFile(target, send)
	case SinkKindSyslog:
		err = sendSyslog(send)
	}
	if err != nil {
		return err
	}
	st.SentSeq = send[len(send)-1].Seq
	st.LastOK = time.Now().UTC().Truncate(time.Second)
	st.FailingSince, st.LastError = time.Time{}, ""
	l.writeSinkState(st)
	return nil
}

// insideLogDir says whether path is, or would be created, in the log's directory, by whatever
// way it's written: a symlink on the way, a link to the log itself, another case of the same
// name on a file system that ignores case. So it compares what the names lead to, not the names.
func (l *Log) insideLogDir(path string) bool {
	dir, err := os.Stat(l.dir)
	if err != nil {
		return false
	}
	if log, err := os.Stat(l.eventsPath()); err == nil {
		if fi, err := os.Stat(path); err == nil && os.SameFile(fi, log) {
			return true
		}
	}
	// A symlink's own directory isn't where it writes, so start from where it leads.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	for p := filepath.Dir(path); ; p = filepath.Dir(p) {
		if fi, err := os.Stat(p); err == nil && os.SameFile(fi, dir) {
			return true
		}
		if p == filepath.Dir(p) {
			return false
		}
	}
}

// appendSinkFile writes the lines the log itself has, so the copy can be read the same
// way. Something at that path that isn't a plain file is an error: opening a FIFO would
// wait for a reader.
func appendSinkFile(path string, events []Event) error {
	// A link to nothing would be followed when the file is created, to wherever it says, and
	// there's nothing yet there to compare with the log.
	if li, err := os.Lstat(path); err == nil && li.Mode()&os.ModeSymlink != 0 {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%s is a symlink to nothing", path)
		}
	}
	if fi, err := os.Stat(path); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("%s isn't a regular file", path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, e := range events {
		line, err := json.Marshal(e)
		if err != nil {
			f.Close()
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syslogLine(e Event) (string, error) {
	e.Detail = termsafe.Clip(e.Detail, sinkLineDetail)
	b, err := json.Marshal(e)
	return string(b), err
}

// SinkReport is how a sink stands, for `activity --verify`.
type SinkReport struct {
	Kind   string `json:"kind"`   // file, syslog, or invalid
	Target string `json:"target"` // the path; empty for syslog
	// State is ok, behind (events haven't reached it yet), failing, or differs (a copy
	// that can be read disagrees with this log, or has lost what it was sent).
	State        string       `json:"state"`
	SentSeq      int64        `json:"sent_seq"`
	LastOK       *time.Time   `json:"last_ok"`
	FailingSince *time.Time   `json:"failing_since"`
	LastError    string       `json:"last_error"`
	Copy         string       `json:"copy"` // compared, not comparable, or unreadable
	Differences  []Difference `json:"differences"`
}

// Healthy says nothing's wrong. Behind isn't wrong: a send may be under way, and what's
// missing goes with the next one. It's a note, and one that stays is for a person to read.
func (r SinkReport) Healthy() bool { return r.State == "ok" || r.State == "behind" }

// CheckSink looks at sink as of now, reading and writing nothing but its copy's bytes.
func (l *Log) CheckSink(sink string) SinkReport {
	rep := SinkReport{Differences: []Difference{}}
	kind, target, err := ParseSink(sink)
	if err != nil {
		rep.Kind, rep.State, rep.LastError, rep.Copy = "invalid", "failing", err.Error(), "not comparable"
		return rep
	}
	rep.Kind, rep.Target = kind, target
	// In the order things are written: the log, then the copy, then the state. Read the other
	// way round, a send landing in between can't look like something lost.
	st := l.readSinkState(sink)
	rep.SentSeq, rep.LastError = st.SentSeq, st.LastError
	if !st.LastOK.IsZero() {
		rep.LastOK = &st.LastOK
	}
	if !st.FailingSince.IsZero() {
		rep.FailingSince = &st.FailingSince
	}
	var copied logFile
	var copyErr error
	var fi os.FileInfo
	var statErr error
	if kind == SinkKindFile {
		if fi, statErr = os.Stat(target); statErr == nil && fi.Mode().IsRegular() && fi.Size() <= sinkMaxCompare {
			copied, copyErr = readLogFile(target)
		}
	}
	local, _ := l.All()
	rep.State = "ok"
	if n := len(local); n > 0 && local[n-1].Seq > st.SentSeq {
		rep.State = "behind"
	}
	if !st.FailingSince.IsZero() {
		rep.State = "failing"
	}
	rep.Copy = "not comparable"
	if kind != SinkKindFile {
		return rep
	}
	switch {
	case os.IsNotExist(statErr) && st.SentSeq == 0:
		return rep // nothing sent, nothing to compare
	case statErr != nil && !os.IsNotExist(statErr), statErr == nil && (!fi.Mode().IsRegular() || fi.Size() > sinkMaxCompare), copyErr != nil:
		rep.Copy = "unreadable"
		return rep
	}
	rep.Copy = "compared"
	device := l.Device()
	if d := CompareCopy(local, copied.events, device); d != nil {
		rep.Differences = d
	}
	have := make(map[int64]bool, len(copied.events))
	for _, c := range copied.events {
		if c.Device == device {
			have[c.Seq] = true
		} else {
			rep.Differences = append(rep.Differences, Difference{c.Seq, "in the copy from another device: this log's directory was replaced, or the copy is shared"})
		}
	}
	for _, e := range local {
		if e.Seq <= st.SentSeq && !have[e.Seq] {
			rep.Differences = append(rep.Differences, Difference{e.Seq, "sent, and gone from the copy"})
		}
	}
	if len(rep.Differences) > 0 && rep.State != "failing" {
		rep.State = "differs"
	}
	return rep
}
