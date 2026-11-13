package main

import (
	"strings"
	"testing"

	"meterm/internal/config"
	"meterm/internal/team"
)

// Pull and push result lines, the push preview and rename spell a name
// with a hidden character as list and the picker do, so "remove pay1, add
// pay2" can't hide that one of them is pay<ZWSP>1.
func TestTeamAndRenameMessagesSpellNames(t *testing.T) {
	const zw = "pay\u200b1"
	const want = `"pay\u200b1"`
	if l := changeLine(team.Change{Host: zw, Outcome: team.Pulled}); !strings.Contains(l, want) {
		t.Errorf("result line %+q", l)
	}
	cfg := &config.Config{Hosts: []config.Host{{Name: zw, Address: "192.0.2.40"}, {Name: "pay2", Address: "192.0.2.41"}}}
	out := stdout(t, func() {
		printPushPreview([]team.Change{{Host: zw, Outcome: team.Pushed}, {Host: "gone\u200b", Outcome: team.PushedRemoval}}, cfg,
			[]team.SharedHost{{Name: "gone\u200b", Address: "192.0.2.42"}})
	})
	if !strings.Contains(out, "add "+want) || !strings.Contains(out, `remove "gone\u200b"`) {
		t.Errorf("preview %+q", out)
	}

	cliSandbox(t)
	saveHosts(t, config.Host{Name: zw, Address: "192.0.2.40", Team: "t1"})
	var stderr string
	out = stdout(t, func() {
		stderr = stderrOf(t, func() {
			if err := runRoot(t, "rename", zw, "pay2"); err != nil {
				t.Error(err)
			}
		})
	})
	if !strings.Contains(stderr, "remove "+want+", add pay2") {
		t.Errorf("rename said %+q, %+q", out, stderr)
	}
}
