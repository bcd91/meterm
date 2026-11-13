package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"meterm/internal/activity"
	"meterm/internal/audit"
	"meterm/internal/config"
	"meterm/internal/history"
	"meterm/internal/status"
)

// Hostile text as it could arrive from an inventory or a teammate.
const (
	evilName = "evil\x1b]0;pwned\x07x"
	evilText = "t\x1b[5m‮ext\x1b]52;c;aGk=\x07"
)

// raw reports the control characters in s that came from hostile text.
func raw(t *testing.T, what, s string) {
	t.Helper()
	if strings.ContainsAny(s, "\x1b\x07‮") {
		t.Errorf("%s printed control characters: %q", what, s)
	}
}

func run(t *testing.T, c interface {
	SetArgs([]string)
	Execute() error
}, args ...string) string {
	t.Helper()
	return stdout(t, func() {
		c.SetArgs(args)
		if err := c.Execute(); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	})
}

func evilInventory(t *testing.T) {
	t.Helper()
	cfg, _ := config.Load(cfgPath)
	if err := cfg.Update(func(c *config.Config) error {
		c.Hosts = append(c.Hosts, config.Host{Name: evilName, Address: "192.0.2.60", Group: evilText,
			Routes:   []config.Route{{Name: evilText, Address: "192.0.2.61"}},
			Forwards: []config.Forward{{Name: evilText, Type: "L", LocalPort: 8080, RemoteHost: "localhost", RemotePort: 80, Note: evilText}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStatusShowsHostTextSafely(t *testing.T) {
	var b bytes.Buffer
	rep := statusReport{Entries: []status.Row{{Entry: status.Entry{Kind: status.KindTunnel, Host: evilName, Name: evilText,
		State: status.Connected, Route: evilText, Detail: evilText, Since: time.Now()}}}}
	if err := writeStatus(&b, rep, "text"); err != nil {
		t.Fatal(err)
	}
	raw(t, "status", b.String())
}

func TestHistoryShowsHostTextSafely(t *testing.T) {
	cliSandbox(t)
	h := history.History{}
	h.Record(evilName)
	if err := h.Save(os.Getenv("METERM_HISTORY")); err != nil {
		t.Fatal(err)
	}
	out := run(t, newHistoryCmd())
	raw(t, "history", out)
	if !strings.Contains(out, "evilx") {
		t.Errorf("history: %q", out)
	}
}

func TestActivityShowsHostTextSafely(t *testing.T) {
	cliSandbox(t)
	l, err := activity.Open(os.Getenv("METERM_ACTIVITY_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(activity.Event{Action: evilText, Host: evilName, User: evilText, Detail: evilText}); err != nil {
		t.Fatal(err)
	}
	raw(t, "activity", run(t, newActivityCmd()))
}

func TestInventoryTablesShowHostTextSafely(t *testing.T) {
	cliSandbox(t)
	evilInventory(t)
	raw(t, "route list", run(t, newRouteListCmd()))
	raw(t, "tunnel list", run(t, newTunnelListCmd()))
}

func TestExecLabelsAndSummaryAreSafe(t *testing.T) {
	run := func(_ context.Context, _ config.Host, _ []string, stdout, _ io.Writer) error {
		_, err := io.WriteString(stdout, "ok \x1b[31mred\x1b[0m\n")
		return err
	}
	var b bytes.Buffer
	if err := runExec([]config.Host{{Name: evilName}}, []string{"x"}, run, execOptions{format: "text", out: &b, safeOut: true}); err != nil {
		t.Fatal(err)
	}
	out := strings.ReplaceAll(b.String(), "\x1b[31mred\x1b[0m", "red") // the host's own colour stays
	raw(t, "exec", out)
	if !strings.Contains(b.String(), "\x1b[31mred") {
		t.Errorf("remote colour was stripped: %q", b.String())
	}
}

func TestAuditTextIsSafe(t *testing.T) {
	rep := audit.Report{Hosts: []audit.HostReport{{Host: evilName, Exposure: "internet", OS: evilText, Error: evilText, Notes: []string{evilText}}}}
	out := lblLines(audit.RenderText(rep))
	raw(t, "audit", out)
	if strings.Count(out, "\n") < 3 {
		t.Errorf("lines were joined: %q", out)
	}
	raw(t, "audit watch", lblLines(audit.FormatChanges("t", []audit.WatchChange{{Host: evilName, Kind: audit.Unreachable, Detail: evilText}})))
}

func TestOneLineIsSafe(t *testing.T) {
	raw(t, "oneLine", oneLine(evilText+strings.Repeat("x", 80), 20))
}

// JSON's \\u escape is UTF-16: a character past U+FFFF that's escaped (a
// tag character) is written as a surrogate pair, and decodes as itself.
func TestEncodeJSONEscapesAstralAsAPair(t *testing.T) {
	in := map[string]string{"name": "tag\U000e0041x"}
	var b bytes.Buffer
	if err := encodeJSON(&b, in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `\udb40\udc41`) {
		t.Errorf("not a surrogate pair: %s", b.String())
	}
	var out map[string]string
	if err := json.Unmarshal(b.Bytes(), &out); err != nil || out["name"] != in["name"] {
		t.Errorf("decodes as %q (%v), want %q", out["name"], err, in["name"])
	}
}
