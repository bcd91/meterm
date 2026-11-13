package team

import (
	"path/filepath"
	"strings"
	"testing"

	"meterm/internal/config"
)

// A host the team doesn't have yet is held to this version's rules: a new
// share or a new proposal with a look-alike or invisible name is refused,
// not sent to everyone with a warning. A host the team already has is
// still sent, flagged (TestPullKeepsHostsOlderVersionsAccepted).
func TestNewShareWithLookalikeNameIsRefused(t *testing.T) {
	s := NewMemoryStore()
	teamID, a, _ := teamOf(t, s, RoleMember)
	cfg := emptyCfg(t)
	for _, n := range []string{"web-\u0430\u0440\u0440", "pay\u200b1", "p\u0430ypal"} {
		cfg.Upsert(config.Host{Name: n, Address: "192.0.2.20", Team: teamID})
	}
	cfg.Upsert(config.Host{Name: "plain", Address: "192.0.2.21", Team: teamID})
	res, err := Sync(ctx, a, teamID, RoleOwner, cfg, Base{}, Options{Push: true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Change{}
	for _, c := range res.Changes {
		got[c.Host] = c
	}
	for _, n := range []string{"web-\u0430\u0440\u0440", "pay\u200b1", "p\u0430ypal"} {
		if c := got[n]; c.Outcome != Rejected || !strings.Contains(c.Detail, "not shared") {
			t.Errorf("%+q: %+v", n, c)
		}
	}
	if got["plain"].Outcome != Pushed {
		t.Errorf("plain: %+v", got["plain"])
	}
	hosts, _ := a.ListHosts(ctx, teamID)
	if len(hosts) != 1 || hosts[0].Name != "plain" {
		t.Errorf("the team holds %+v, want only plain", hosts)
	}
}

func TestNewProposalWithLookalikeNameIsRefusedAndAnEditIsFlagged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
	alice, bob := gitTeam(t)
	if err := alice.Invite(ctx, alice.ID, "bob@example.com", RoleMember); err != nil {
		t.Fatal(err)
	}
	// An older version shared this one.
	old := SharedHost{Name: "p\u0430ypal", Address: "192.0.2.30"}
	stored, err := alice.PutHost(ctx, alice.ID, old, 0)
	if err != nil {
		t.Fatal(err)
	}
	bCfg := emptyCfg(t)
	bCfg.Upsert(config.Host{Name: "pay\u200b1", Address: "192.0.2.31", Team: bob.ID})
	bCfg.Upsert(config.Host{Name: "p\u0430ypal", Address: "192.0.2.30", Port: 2222, Team: bob.ID})
	res, err := Sync(ctx, bob, bob.ID, RoleMember, bCfg, Base{old.Name: stored}, Options{Push: true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Change{}
	for _, c := range res.Changes {
		got[c.Host] = c
	}
	if c := got["pay\u200b1"]; c.Outcome != Rejected || !strings.Contains(c.Detail, "not proposed") {
		t.Errorf("new proposal: %+v", c)
	}
	if c := got[old.Name]; c.Outcome != Proposed || !strings.Contains(c.Detail, "warning") {
		t.Errorf("edit of a flagged team host: %+v (want proposed, with the warning shown)", c)
	}
	ps, err := alice.Proposals(ctx, alice.ID)
	if err != nil || len(ps) != 1 || ps[0].Host.Name != old.Name {
		t.Errorf("proposals = %+v, %v", ps, err)
	}
}
