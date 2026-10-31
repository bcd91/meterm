package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"meterm/internal/safefile"
)

// ChangeKind is how a control (or a whole host) moved between two audits.
type ChangeKind string

const (
	NewFailure  ChangeKind = "new"         // failing now, wasn't before
	Worse       ChangeKind = "worse"       // still failing, at a higher severity
	Changed     ChangeKind = "changed"     // still failing, different evidence (e.g. a new open port)
	Better      ChangeKind = "better"      // still failing, at a lower severity
	Resolved    ChangeKind = "resolved"    // was failing, isn't now
	Unreachable ChangeKind = "unreachable" // the audit couldn't run on the host
	Recovered   ChangeKind = "recovered"   // the host can be audited again
)

// WatchChange is one line of an audit-to-audit comparison.
type WatchChange struct {
	Host     string     `json:"host"`
	Kind     ChangeKind `json:"kind"`
	Control  string     `json:"control,omitempty"`
	Title    string     `json:"title,omitempty"`
	Severity Severity   `json:"severity"`
	Detail   string     `json:"detail,omitempty"`
}

// Alarming reports whether c should wake someone up: things got worse.
func (c WatchChange) Alarming() bool {
	switch c.Kind {
	case NewFailure, Worse, Changed, Unreachable:
		return true
	}
	return false
}

// Diff compares two audits of (mostly) the same hosts. Hosts present in only
// one of them are ignored: adding a host to the watch list isn't news.
// "Couldn't check" results don't count either way — a host where sudo
// briefly failed shouldn't flap between alarm and all-clear.
func Diff(prev, cur Report) []WatchChange {
	before := map[string]HostReport{}
	for _, h := range prev.Hosts {
		before[h.Host] = h
	}
	var out []WatchChange
	for _, h := range cur.Hosts {
		p, ok := before[h.Host]
		if !ok {
			continue
		}
		switch {
		case h.Error != "" && p.Error == "":
			out = append(out, WatchChange{Host: h.Host, Kind: Unreachable, Severity: High, Detail: h.Error})
			continue
		case h.Error != "":
			continue // still down; already reported
		case p.Error != "":
			out = append(out, WatchChange{Host: h.Host, Kind: Recovered})
			// and fall through to compare against nothing: report current failures as new
			p.Results = nil
		}
		was := map[string]Result{}
		for _, r := range p.Results {
			was[r.Control] = r
		}
		for _, r := range h.Results {
			o, had := was[r.Control]
			wasFail := had && o.Status == Fail
			switch {
			case r.Status == Fail && !wasFail:
				out = append(out, change(h.Host, NewFailure, r))
			case r.Status == Fail && r.Severity > o.Severity:
				out = append(out, change(h.Host, Worse, r))
			case r.Status == Fail && r.Severity < o.Severity:
				out = append(out, change(h.Host, Better, r))
			case r.Status == Fail && r.Detail != o.Detail:
				out = append(out, change(h.Host, Changed, r))
			case r.Status == Pass && wasFail:
				c := change(h.Host, Resolved, o)
				c.Detail = r.Detail
				out = append(out, c)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := out[i].Alarming(), out[j].Alarming()
		if ai != aj {
			return ai
		}
		return out[i].Severity > out[j].Severity
	})
	return out
}

func change(host string, k ChangeKind, r Result) WatchChange {
	return WatchChange{Host: host, Kind: k, Control: r.Control, Title: r.Title, Severity: r.Severity, Detail: r.Detail}
}

// FormatChanges renders changes as a short plain-text message for a chat
// notification: one line per change, the worst first.
func FormatChanges(title string, cs []WatchChange) string {
	var b strings.Builder
	b.WriteString(title)
	b.WriteString("\n")
	for _, c := range cs {
		icon := map[ChangeKind]string{
			NewFailure: "🔴", Worse: "🔴", Changed: "🟠", Unreachable: "⚫",
			Better: "🟡", Resolved: "🟢", Recovered: "🟢",
		}[c.Kind]
		switch c.Kind {
		case Unreachable:
			fmt.Fprintf(&b, "%s %s: can't be audited — %s\n", icon, c.Host, c.Detail)
		case Recovered:
			fmt.Fprintf(&b, "%s %s: reachable again\n", icon, c.Host)
		case Resolved:
			fmt.Fprintf(&b, "%s %s: fixed — %s\n", icon, c.Host, c.Title)
		default:
			fmt.Fprintf(&b, "%s %s [%s, %s]: %s — %s\n", icon, c.Host, c.Kind, c.Severity, c.Title, c.Detail)
		}
	}
	return b.String()
}

// FirstRun is a watch's first result: every current failure, as new.
func FirstRun(r Report) []WatchChange {
	empty := Report{}
	for _, h := range r.Hosts {
		empty.Hosts = append(empty.Hosts, HostReport{Host: h.Host})
	}
	return Diff(empty, r)
}

// ValidWatchName reports whether name is usable as a watch's file name.
func ValidWatchName(name string) error {
	if strings.ContainsAny(name, `/\:`) || name == "" || name == "." || name == ".." {
		return fmt.Errorf("--name %q: use letters, digits, - and _", name)
	}
	return nil
}

// LoadReport reads a watch's previous result; found is false when there's
// none yet.
func LoadReport(path string) (r Report, found bool, err error) {
	b, err := safefile.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Report{}, false, nil
	}
	if err != nil {
		return Report{}, false, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Report{}, false, fmt.Errorf("%s is unreadable (%v); delete it to start over", path, err)
	}
	return r, true, nil
}

// SaveReport writes a watch's result atomically, private to this user.
func SaveReport(path string, r Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return safefile.WriteFile(path, b)
}
