package sshrun

import (
	"strings"
	"testing"

	"meterm/internal/config"
)

func TestStatsCommandIsNonInteractive(t *testing.T) {
	r := config.Resolved{Address: "10.0.0.1", User: "deploy"}
	c := StatsCommand(r)
	if c.Stdin != nil {
		t.Error("StatsCommand should not attach stdin — it must fail fast on a password prompt, like exec")
	}
	joined := strings.Join(c.Args, " ")
	for _, want := range []string{"uptime", "== cpu ==", "== memory ==", "== disk ==", "== network =="} {
		if !strings.Contains(joined, want) {
			t.Errorf("script missing %q section: %q", want, joined)
		}
	}
	targetIdx := strings.Index(joined, "deploy@10.0.0.1")
	scriptIdx := strings.Index(joined, "sh -c")
	if targetIdx < 0 || scriptIdx < 0 || targetIdx > scriptIdx {
		t.Errorf("expected the target to appear before the remote script in argv: %q", joined)
	}
}
