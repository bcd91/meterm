package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/activity"
	"meterm/internal/config"
	"meterm/internal/history"
)

// A connection ssh couldn't make (exit 255) isn't \"last connected just
// now\", and the activity log says how each attempt ended.
func TestConnectRecordsTheOutcome(t *testing.T) {
	dir := cliSandbox(t)
	installFakeSSH(t, t.TempDir())
	t.Setenv("METERM_TEST_SSH_LOG", filepath.Join(dir, "ssh.log"))
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Upsert(config.Host{Name: "web1", Address: "192.0.2.40"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	connected := func() bool {
		h, err := history.Load(os.Getenv("METERM_HISTORY"))
		if err != nil {
			t.Fatal(err)
		}
		_, ok := h["web1"]
		return ok
	}
	events := func() []activity.Event {
		l, err := activity.Open(os.Getenv("METERM_ACTIVITY_DIR"))
		if err != nil {
			t.Fatal(err)
		}
		ev, err := l.All()
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	lastEvent := func() activity.Event {
		l, err := activity.Open(os.Getenv("METERM_ACTIVITY_DIR"))
		if err != nil {
			t.Fatal(err)
		}
		ev, err := l.All()
		if err != nil || len(ev) == 0 {
			t.Fatalf("no activity: %v", err)
		}
		return ev[len(ev)-1]
	}

	seen := filepath.Join(dir, "seen.jsonl")
	t.Setenv("METERM_TEST_SSH_SEES", seen)
	t.Setenv(fakeSSHExitEnv, "255")
	code, err := connect([]string{"web1"})
	if err != nil || code != 255 {
		t.Fatalf("exit 255: code %d err %v", code, err)
	}
	if connected() {
		t.Error("a failed connection was recorded as last connected")
	}
	if ev := lastEvent(); ev.Action != activity.ActionOutcome || ev.ExitCode == nil || *ev.ExitCode != 255 || !strings.Contains(ev.Detail, "never made") {
		t.Errorf("failed attempt logged as %q %q", ev.Action, ev.Detail)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "ssh.log")); !strings.Contains(string(b), "192.0.2.40") {
		t.Fatalf("fake ssh not run: %q", b)
	}
	// While ssh ran, the log already said a session had started: a crash or
	// SIGKILL mid-session still leaves that.
	if b, _ := os.ReadFile(seen); !strings.Contains(string(b), `"action":"connect"`) || strings.Contains(string(b), `"action":"outcome"`) {
		t.Errorf("nothing recorded while ssh ran: %q", b)
	}
	// Each attempt is two entries: its start, then its outcome.
	if ev := events(); len(ev) != 2 || ev[0].Action != "connect" || ev[1].Action != activity.ActionOutcome || ev[1].RefSeq != ev[0].Seq {
		t.Errorf("entries: %+v", ev)
	}

	// The remote shell's own status: ssh got there.
	t.Setenv(fakeSSHExitEnv, "3")
	if code, err := connect([]string{"web1"}); err != nil || code != 3 {
		t.Fatalf("exit 3: code %d err %v", code, err)
	}
	if !connected() {
		t.Error("a session that ran wasn't recorded")
	}
	if ev := lastEvent(); strings.Contains(ev.Detail, "failed") || !strings.Contains(ev.Detail, "exit 3") {
		t.Errorf("exit 3 logged as %q", ev.Detail)
	}

	t.Setenv(fakeSSHExitEnv, "0")
	if code, err := connect([]string{"web1", "--", "uptime"}); err != nil || code != 0 {
		t.Fatalf("exit 0: code %d err %v", code, err)
	}
	if ev := lastEvent(); !strings.Contains(ev.Detail, "exit 0") {
		t.Errorf("exit 0 logged as %q", ev.Detail)
	}
	if ev := events(); len(ev) != 6 || ev[4].Detail != "(+1 args)" {
		t.Errorf("entries after three attempts: %d, start detail %q", len(ev), ev[len(ev)-2].Detail)
	}
}

// dev gets the same treatment as connect: "started" before ssh runs, the
// outcome after, and last-connected only when it got there.
func TestDevRecordsStartAndOutcome(t *testing.T) {
	dir := cliSandbox(t)
	installFakeSSH(t, t.TempDir())
	seen := filepath.Join(dir, "seen.jsonl")
	t.Setenv("METERM_TEST_SSH_SEES", seen)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.Upsert(config.Host{Name: "box", Address: "192.0.2.41"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	events := func() []activity.Event {
		l, _ := activity.Open(os.Getenv("METERM_ACTIVITY_DIR"))
		ev, err := l.All()
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	lastConnected := func() bool {
		h, _ := history.Load(os.Getenv("METERM_HISTORY"))
		_, ok := h["box"]
		return ok
	}

	t.Setenv(fakeSSHExitEnv, "255")
	code, err := dev("box:/srv", nil, "", nil, false, false)
	if err != nil || code != 255 {
		t.Fatalf("exit 255: code %d err %v", code, err)
	}
	if b, _ := os.ReadFile(seen); !strings.Contains(string(b), `"action":"connect"`) {
		t.Errorf("nothing recorded while ssh ran: %q", b)
	}
	ev := events()
	if len(ev) != 2 || ev[0].Detail != "dev srv" || ev[1].Action != activity.ActionOutcome || !strings.Contains(ev[1].Detail, "never connected") {
		t.Errorf("entries: %+v", ev)
	}
	if lastConnected() {
		t.Error("a dev session that never connected was recorded as last connected")
	}

	t.Setenv(fakeSSHExitEnv, "0")
	if code, err := dev("box:/srv", nil, "", nil, false, false); err != nil || code != 0 {
		t.Fatalf("exit 0: code %d err %v", code, err)
	}
	if ev := events(); len(ev) != 4 || !strings.Contains(ev[3].Detail, "exit 0") {
		t.Errorf("entries: %+v", ev)
	}
	if !lastConnected() {
		t.Error("a dev session that ran wasn't recorded")
	}
}

// The sftp prompt: "started" first, the outcome after, last-connected only
// when sftp exits 0 (it reports a failed connection as 1 or 255).
func TestSftpRecordsStartAndOutcome(t *testing.T) {
	dir := cliSandbox(t)
	installFake(t, t.TempDir(), "sftp")
	seen := filepath.Join(dir, "seen.jsonl")
	t.Setenv("METERM_TEST_SSH_SEES", seen)
	cfg, _ := config.Load(cfgPath)
	if err := cfg.Update(func(c *config.Config) error {
		c.Upsert(config.Host{Name: "box", Address: "192.0.2.42"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lastConnected := func() bool {
		h, _ := history.Load(os.Getenv("METERM_HISTORY"))
		_, ok := h["box"]
		return ok
	}
	t.Setenv(fakeSSHExitEnv, "1")
	if code, err := sftpShell([]string{"box"}); err != nil || code != 1 {
		t.Fatalf("exit 1: code %d err %v", code, err)
	}
	if b, _ := os.ReadFile(seen); !strings.Contains(string(b), `"action":"sftp"`) {
		t.Errorf("nothing recorded while sftp ran: %q", b)
	}
	if lastConnected() {
		t.Error("a failed sftp was recorded as last connected")
	}
	t.Setenv(fakeSSHExitEnv, "0")
	if code, err := sftpShell([]string{"box"}); err != nil || code != 0 {
		t.Fatalf("exit 0: code %d err %v", code, err)
	}
	if !lastConnected() {
		t.Error("a working sftp wasn't recorded")
	}
	l, _ := activity.Open(os.Getenv("METERM_ACTIVITY_DIR"))
	ev, _ := l.All()
	if len(ev) != 4 || ev[0].Action != "sftp" || ev[1].Action != activity.ActionOutcome ||
		!strings.Contains(ev[1].Detail, "exit 1") || !strings.Contains(ev[3].Detail, "exit 0") {
		t.Errorf("entries: %+v", ev)
	}
}
