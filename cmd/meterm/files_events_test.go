package main

import (
	"os"
	"strings"
	"testing"

	"meterm/internal/activity"
	"meterm/internal/config"
	"meterm/internal/fileman"
	"meterm/internal/termsafe"
)

// Each file manager operation on the host is a start and an outcome, as
// connect, exec and cp are: the start with the production note, the outcome
// with an exit code or the file manager's word, and what only the end knows
// in words just before it. Nothing on this machine is logged, and the chain
// still verifies.
func TestFileManagerOperationsStartAndEnd(t *testing.T) {
	cliSandbox(t)
	saveHosts(t, config.Host{Name: "web1", Address: "192.0.2.10"})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := cfg.Target("web1")
	if err != nil {
		t.Fatal(err)
	}
	on := fileEventLogger(cfg, *h, "production, asked")
	for _, e := range []fileman.Event{
		{Op: 1, Kind: "upload", Outcome: "started", Detail: "a.txt -> /srv (1 file(s), 3 B)", Remote: true},
		{Op: 2, Kind: "delete", Outcome: "started", Detail: "/srv/old", Remote: true},
		{Op: 3, Kind: "copy", Outcome: "started", Detail: "x -> /tmp", Remote: false},
		{Op: 1, Kind: "upload", Outcome: "ok", Detail: "a.txt -> /srv (1 file(s), 3 B)", Remote: true},
		{Op: 2, Kind: "delete", Outcome: "partial", Detail: "/srv/old: failed on /srv/old/x: permission denied; removed: /srv/old/a", More: true, Remote: true},
		{Op: 3, Kind: "copy", Outcome: "ok", Detail: "x -> /tmp", Remote: false},
		{Op: 4, Kind: "mkdir", Outcome: "started", Detail: "/srv/d", Remote: true},
		{Op: 4, Kind: "mkdir", Outcome: "made up", Detail: "/srv/d", Remote: true},
	} {
		on(e)
	}
	ev := events(t)
	want := []string{
		"sftp web1: (production, asked) upload a.txt -> /srv (1 file(s), 3 B) => exit 0",
		"sftp web1: (production, asked) delete /srv/old => partial",
		"sftp web1: partial: delete /srv/old: failed on /srv/old/x: permission denied; removed: /srv/old/a",
		"sftp web1: (production, asked) mkdir /srv/d => ended",
	}
	if strings.Join(ev, "\n") != strings.Join(want, "\n") {
		t.Errorf("logged\n  %s\nwant\n  %s", strings.Join(ev, "\n  "), strings.Join(want, "\n  "))
	}
	l, err := activity.Open(os.Getenv("METERM_ACTIVITY_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	all, _ := l.All()
	if b := activity.Verify(all); b != nil {
		t.Errorf("the chain doesn't verify: %+v", b)
	}
	for _, e := range all {
		if e.Action == activity.ActionOutcome && e.RefSeq == 0 {
			t.Errorf("an outcome that points at nothing: %+v", e)
		}
	}
}

// A name the file manager spelled out (termsafe.Quote) reaches the log as
// it is: the exact name, not U+FFFD where a hidden character was.
func TestFileManagerLogKeepsSpelledNames(t *testing.T) {
	cliSandbox(t)
	saveHosts(t, config.Host{Name: "web1", Address: "192.0.2.10"})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := cfg.Target("web1")
	if err != nil {
		t.Fatal(err)
	}
	q := termsafe.Quote("/srv/ev\x1b]0;PWN\x07 il\n name‮txt.exe")
	on := fileEventLogger(cfg, *h, "")
	on(fileman.Event{Op: 1, Kind: "delete", Outcome: "started", Detail: q, Remote: true})
	on(fileman.Event{Op: 1, Kind: "delete", Outcome: "ok", Detail: q, Remote: true})
	l, err := activity.Open(os.Getenv("METERM_ACTIVITY_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	all, _ := l.All()
	found := false
	for _, e := range all {
		if strings.Contains(e.Detail, "�") {
			t.Errorf("logged %q", e.Detail)
		}
		found = found || strings.Contains(e.Detail, q)
	}
	if !found {
		t.Errorf("no event holds %s: %+v", q, all)
	}
}
