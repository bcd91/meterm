package audit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rep(hosts ...HostReport) Report { return Report{Hosts: hosts} }

func TestDiffReportsWhatMoved(t *testing.T) {
	prev := rep(HostReport{Host: "web1", Results: []Result{
		{Control: "patching", Status: Fail, Severity: Medium, Detail: "12 pending"},
		{Control: "ssh-auth", Status: Fail, Severity: High, Detail: "root login"},
		{Control: "firewall", Status: Pass},
		{Control: "exposed-ports", Status: Fail, Severity: High, Detail: "5432 open"},
		{Control: "container-egress", Status: Unknown},
	}}, HostReport{Host: "db1", Results: []Result{{Control: "firewall", Status: Pass}}})
	cur := rep(HostReport{Host: "web1", Results: []Result{
		{Control: "patching", Status: Fail, Severity: High, Detail: "40 pending"},           // worse
		{Control: "ssh-auth", Status: Pass},                                                 // resolved
		{Control: "firewall", Status: Fail, Severity: Medium},                               // new
		{Control: "exposed-ports", Status: Fail, Severity: High, Detail: "5432, 6379 open"}, // changed
		{Control: "container-egress", Status: Pass},                                         // unknown -> pass: not news
	}}, HostReport{Host: "db1", Error: "ssh: connect timed out"}, // unreachable
		HostReport{Host: "brand-new", Results: []Result{{Control: "firewall", Status: Fail}}}) // not in prev: ignored

	got := map[string]ChangeKind{}
	for _, c := range Diff(prev, cur) {
		got[c.Host+"/"+c.Control] = c.Kind
	}
	want := map[string]ChangeKind{
		"web1/patching": Worse, "web1/ssh-auth": Resolved, "web1/firewall": NewFailure,
		"web1/exposed-ports": Changed, "db1/": Unreachable,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %v, want exactly %v", got, want)
	}
}

func TestDiffNoChangesIsEmpty(t *testing.T) {
	r := rep(HostReport{Host: "a", Results: []Result{{Control: "x", Status: Fail, Severity: Low, Detail: "d"}}})
	if cs := Diff(r, r); len(cs) != 0 {
		t.Errorf("identical audits produced %+v", cs)
	}
}

func TestFormatChangesPutsAlarmsFirst(t *testing.T) {
	cs := Diff(
		rep(HostReport{Host: "a", Results: []Result{{Control: "x", Title: "X", Status: Fail, Severity: Low}}}),
		rep(HostReport{Host: "a", Results: []Result{{Control: "x", Status: Pass}, {Control: "y", Title: "Y", Status: Fail, Severity: Critical}}}),
	)
	msg := FormatChanges("title", cs)
	if strings.Index(msg, "Y") > strings.Index(msg, "fixed") {
		t.Errorf("new critical should come before the fix:\n%s", msg)
	}
}

func TestReportSurvivesAJSONRoundTrip(t *testing.T) {
	r := rep(HostReport{Host: "a", Results: []Result{{Control: "x", Status: Fail, Severity: High, Detail: "d"}}})
	var buf bytes.Buffer
	if err := WriteJSON(&buf, r); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("can't read our own JSON back: %v", err)
	}
	if cs := Diff(r, back); len(cs) != 0 {
		t.Errorf("round trip changed the report: %+v", cs)
	}
}

func TestFirstRunListsCurrentFailuresAsNew(t *testing.T) {
	r := Report{Hosts: []HostReport{{Host: "web1", Results: []Result{{Control: "ssh-password", Status: Fail, Severity: High}}}}}
	cs := FirstRun(r)
	if len(cs) != 1 || cs[0].Kind != NewFailure || cs[0].Host != "web1" {
		t.Errorf("FirstRun = %+v", cs)
	}
	if cs := FirstRun(Report{Hosts: []HostReport{{Host: "ok"}}}); len(cs) != 0 {
		t.Errorf("clean host: %+v", cs)
	}
}

func TestWatchReportRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w", "default.json")
	if _, found, err := LoadReport(path); found || err != nil {
		t.Fatalf("missing file: found=%v err=%v", found, err)
	}
	want := Report{Hosts: []HostReport{{Host: "web1"}}}
	if err := SaveReport(path, want); err != nil {
		t.Fatal(err)
	}
	got, found, err := LoadReport(path)
	if err != nil || !found || len(got.Hosts) != 1 || got.Hosts[0].Host != "web1" {
		t.Fatalf("got %+v, %v, %v", got, found, err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadReport(path); err == nil || !strings.Contains(err.Error(), "delete it") {
		t.Errorf("corrupt file: %v", err)
	}
}

func TestValidWatchName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "c:d"} {
		if ValidWatchName(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := ValidWatchName("client-a_2"); err != nil {
		t.Error(err)
	}
}
