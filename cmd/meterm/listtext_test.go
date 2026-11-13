package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"meterm/internal/cellwidth"
	"meterm/internal/config"
)

// list lines up by terminal cells: a Thai name with marks above the line,
// or an emoji, doesn't push its row's columns out of line.
func TestListAlignsByCells(t *testing.T) {
	rows := []listRow{
		{Name: "น้ำ-ประปา", Target: "192.0.2.1", lastHuman: "never"},
		{Name: "🚀-web", Target: "192.0.2.2", lastHuman: "never"},
		{Name: "plain", Target: "192.0.2.3", lastHuman: "never"},
	}
	var b bytes.Buffer
	if err := writeListText(&b, rows, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	col := -1
	for _, l := range lines[1:] {
		at := cellwidth.Width(l[:strings.Index(l, "192.0.2.")])
		if col >= 0 && at != col {
			t.Errorf("target starts at %d, not %d:\n%s", at, col, b.String())
		}
		col = at
	}
}

// An env meterm doesn't know is listed as production, flagged, and named
// in JSON so a script can find it.
func TestListFlagsAnUnrecognisedEnv(t *testing.T) {
	cliSandbox(t)
	cfg, _ := config.Load(cfgPath)
	cfg.Upsert(config.Host{Name: "pay1", Address: "192.0.2.60", Env: "prodution"})
	cfg.Upsert(config.Host{Name: "web1", Address: "192.0.2.61", Env: "dev"})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	out := stdout(t, func() {
		c := newListCmd()
		c.SetArgs(nil)
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "prodution?") {
		t.Errorf("text doesn't flag the typo:\n%s", out)
	}
	js := stdout(t, func() {
		c := newListCmd()
		c.SetArgs([]string{"--format", "json"})
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	var rows []map[string]any
	if err := json.Unmarshal([]byte(js), &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r["name"] == "pay1" && (r["env"] != "production" || r["env_unrecognised"] != "prodution") {
			t.Errorf("json row: %v", r)
		}
	}
}

// JSON keeps bidi controls as \u escapes: still valid, still equal when
// decoded, but nothing a terminal showing the raw text will reorder.
func TestListJSONEscapesBidi(t *testing.T) {
	cliSandbox(t)
	b, _ := os.ReadFile(cfgPath)
	_ = b
	cfg, _ := config.Load(cfgPath)
	cfg.Upsert(config.Host{Name: "web1", Address: "192.0.2.62", Notes: "x", Group: "g\u202eabc"})
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	js := stdout(t, func() {
		c := newListCmd()
		c.SetArgs([]string{"--format", "json"})
		if err := c.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	if strings.ContainsRune(js, 0x202e) || !strings.Contains(js, `\u202e`) {
		t.Errorf("bidi passed through: %q", js)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(js), &rows); err != nil || rows[0]["group"] != "g\u202eabc" {
		t.Errorf("decoded %v, %v", rows, err)
	}
}

// Two hosts whose names differ by a hidden character must not list as two
// identical rows: the one with it is spelled out, as activity does.
func TestListSpellsOutHiddenCharacters(t *testing.T) {
	rows := []listRow{
		{Name: "web1", Target: "192.0.2.1", lastHuman: "never"},
		{Name: "web1\u200b", Target: "192.0.2.2", Jump: "bast\u200bion", lastHuman: "never"},
	}
	var b bytes.Buffer
	if err := writeListText(&b, rows, false); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, `"web1\u200b"`) || !strings.Contains(out, `"bast\u200bion"`) {
		t.Errorf("hidden characters not spelled out:\n%s", out)
	}
	if strings.Count(out, "web1 ") != 1 || strings.ContainsRune(out, '\u200b') {
		t.Errorf("rows read alike:\n%q", out)
	}
}
