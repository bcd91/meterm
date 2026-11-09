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

// A host asleep behind a jump host answers nothing, and without a connect timeout
// ssh waits on the jump host's TCP retries for minutes. The host's own extra
// arguments must survive, and the caller's Resolved must not be changed.
func TestStatsCommandsTimeOutConnecting(t *testing.T) {
	extra := make([]string, 1, 4)
	extra[0] = "-v"
	r := config.Resolved{Address: "10.0.0.1", User: "deploy", ExtraArgs: extra}
	for name, c := range map[string][]string{"stats": StatsCommand(r).Args, "win-stats": WinStatsCommand(r).Args} {
		joined := strings.Join(c, " ")
		if !strings.Contains(joined, "-o ConnectTimeout=10") {
			t.Errorf("%s: no ConnectTimeout: %q", name, joined)
		}
		if !strings.Contains(joined, " -v ") {
			t.Errorf("%s: lost the host's extra args: %q", name, joined)
		}
	}
	if len(r.ExtraArgs) != 1 || extra[:2][1] != "" {
		t.Errorf("caller's ExtraArgs were changed: %q", extra[:cap(extra)])
	}
}
