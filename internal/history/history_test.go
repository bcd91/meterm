package history

import (
	"path/filepath"
	"testing"
	"time"

	"meterm/internal/config"
)

func TestRecordThenSaveThenLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")

	h, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 0 {
		t.Fatalf("expected empty history for a missing file, got %v", h)
	}

	h.Record("web1")
	h.Record("web1")
	if err := h.Save(path); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := reloaded["web1"]
	if !ok {
		t.Fatalf("expected web1 in reloaded history, got %v", reloaded)
	}
	if e.Count != 2 {
		t.Errorf("Count = %d, want 2", e.Count)
	}
	if time.Since(e.Last) > time.Minute {
		t.Errorf("Last = %v, expected recent", e.Last)
	}
}

func TestSortByRecency(t *testing.T) {
	hosts := []config.Host{
		{Name: "alpha"},
		{Name: "beta"},
		{Name: "gamma"},
	}
	h := History{
		"gamma": {Last: time.Now().Add(-1 * time.Hour)},
		"beta":  {Last: time.Now()},
	}
	sorted := h.SortByRecency(hosts)
	got := []string{sorted[0].Name, sorted[1].Name, sorted[2].Name}
	want := []string{"beta", "gamma", "alpha"} // most recent first, unvisited last
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SortByRecency = %v, want %v", got, want)
			break
		}
	}
}
