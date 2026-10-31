// Package history tracks which saved hosts you've actually connected to
// recently, so the picker can float what you use most to the top instead of
// making you scroll or remember a name.
package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"meterm/internal/config"
	"meterm/internal/safefile"
)

// Entry is one host's connection record.
type Entry struct {
	Last         time.Time `json:"last"`
	Count        int       `json:"count"`
	KeyInstalled bool      `json:"key_installed,omitempty"`
}

// History maps host name to its record.
type History map[string]Entry

// Path returns where history is stored, honoring $METERM_HISTORY for
// overrides (tests, alternate profiles), and otherwise sitting next to the
// host inventory.
func Path() (string, error) {
	if p := os.Getenv("METERM_HISTORY"); p != "" {
		return p, nil
	}
	cfgPath, err := config.DefaultPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfgPath), "history.json"), nil
}

// Load reads history from path, returning an empty History if it doesn't
// exist yet.
func Load(path string) (History, error) {
	data, err := safefile.ReadFile(path)
	if os.IsNotExist(err) {
		return History{}, nil
	}
	if err != nil {
		return nil, err
	}
	var h History
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, err
	}
	if h == nil {
		h = History{}
	}
	return h, nil
}

// Save writes h back to path.
func (h History) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return safefile.WriteFile(path, data)
}

// Record marks name as visited right now.
func (h History) Record(name string) {
	e := h[name]
	e.Last = time.Now()
	e.Count++
	h[name] = e
}

// RecordAndSave loads history from its default location, records name, and
// saves it back — the one-call convenience used by connect/sftp.
func RecordAndSave(name string) error {
	path, err := Path()
	if err != nil {
		return err
	}
	h, err := Load(path)
	if err != nil {
		return err
	}
	h.Record(name)
	return h.Save(path)
}

// MarkKeyInstalled flags name as having had a key installed by copy-id,
// without touching its connect count/timestamp.
func (h History) MarkKeyInstalled(name string) {
	e := h[name]
	e.KeyInstalled = true
	h[name] = e
}

// MarkKeyInstalledAndSave is the one-call convenience used by copy-id.
func MarkKeyInstalledAndSave(name string) error {
	path, err := Path()
	if err != nil {
		return err
	}
	h, err := Load(path)
	if err != nil {
		return err
	}
	h.MarkKeyInstalled(name)
	return h.Save(path)
}

// FormatAgo renders t as a short relative time ("3m ago", "2d ago"), the
// shared format used by `meterm list` and `meterm history`.
func FormatAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// SortByRecency orders hosts by most-recently-connected first; hosts never
// connected to sort after all visited ones, alphabetically among themselves.
func (h History) SortByRecency(hosts []config.Host) []config.Host {
	sorted := make([]config.Host, len(hosts))
	copy(sorted, hosts)
	sort.SliceStable(sorted, func(i, j int) bool {
		ei, oki := h[sorted[i].Name]
		ej, okj := h[sorted[j].Name]
		switch {
		case oki && okj:
			return ei.Last.After(ej.Last)
		case oki != okj:
			return oki // visited hosts sort before never-visited ones
		default:
			return sorted[i].Name < sorted[j].Name
		}
	})
	return sorted
}
