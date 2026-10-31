package audit

import (
	"strings"
	"testing"
	"time"
)

func TestRenderHTML(t *testing.T) {
	r := Report{
		Generated: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
		Hosts: []HostReport{
			{
				Host:     "WEB",
				Exposure: "ssh+web",
				OS:       "Ubuntu 22.04",
				HaveSudo: true,
				Results: []Result{
					{Title: "Password auth enabled", Status: Fail, Severity: 2, Detail: "sshd allows password login", Fix: "set PasswordAuthentication no", Owner: "sysadmin"},
					{Title: "fail2ban active", Status: Pass, Detail: "jail sshd active"},
				},
			},
			{Host: "DOWN-HOST", Error: "connection refused"},
		},
	}

	var buf strings.Builder
	if err := RenderHTML(&buf, r, "Acme Corp"); err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"<title>Acme Corp</title>",
		"<h1>Acme Corp</h1>",
		"WEB",
		"Password auth enabled",
		"set PasswordAuthentication no",
		"DOWN-HOST",
		"could not audit: connection refused",
		"sysadmin",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
	// A host that errored out never got HaveSudo info — the "no
	// passwordless sudo" note is a real finding, not a default-value
	// artifact, so it must not appear for DOWN-HOST.
	if i := strings.Index(out, "DOWN-HOST"); i >= 0 && strings.Contains(out[i:], "no passwordless sudo") {
		t.Error("errored host incorrectly reported \"no passwordless sudo\"")
	}
	if strings.Contains(out, "<no value>") {
		t.Error("template left a <no value> artifact")
	}

	if err := RenderHTML(&buf, Report{}, ""); err != nil {
		t.Fatalf("RenderHTML with empty title/report: %v", err)
	}
}
